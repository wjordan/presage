package delta

import (
	"fmt"
	"sort"
	"sync"

	"github.com/wjordan/go-binsync/delta/gobin"
	"github.com/wjordan/go-binsync/delta/x86"
)

// The segment map for resized functions (docs/DESIGN.md 3.2.1).
//
// A matched function is copied positionally -- byte k of the old body lands
// at byte k of the new one -- and relocated as if it had moved by one
// constant. That is right for a function that only moved and wrong from the
// first inserted byte onwards for one that was edited: every later
// instruction is copied to the wrong offset *and* relocated at the wrong PC,
// so the residual pays for it twice.
//
// A resized matched function therefore carries a short list of pieces, and
// the decoder lays its body down piece by piece, relocating each at its own
// PC. The aligner below runs on the encoder only: a bad alignment costs
// bytes, never correctness, because the output is still verified.

const (
	// segWin is the window hashed at each old instruction boundary, segGap
	// the mismatching bytes a piece bridges rather than splitting on, and
	// segCand the candidate old positions kept per window hash.
	segWin  = 12
	segGap  = 16
	segCand = 8
	// segMin is the shortest piece worth transmitting. In the minor pair's
	// own yardstick a piece entry costs 1.65 B compressed and can split a
	// correction run at each end, so it must fix
	// (1.65 + 2*0.606)/0.244 ~ 12 bytes to pay for itself.
	segMin = 12
	// segMaxOff bounds a decoded offset, so that a corrupt list cannot
	// overflow the int32 columns before the size checks reach it.
	segMaxOff = 1 << 30
)

// segPiece is one aligned stretch: N bytes of the old body at Old line up
// with the new body at New.
type segPiece struct{ Old, New, N int32 }

// segMap is one function's pieces, in new-offset order, strictly monotone
// and non-overlapping in both bodies.
type segMap struct {
	Idx  int // index into the new function list
	Segs []segPiece
}

// winHash is the FNV-1a of a fixed-width window.
func winHash(b []byte, p, w int) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b[p : p+w] {
		h = (h ^ uint64(c)) * 1099511628211
	}
	return h
}

// alignBodies anchors instruction-aligned windows of the old body in the new
// one, keeps the longest chain of anchors monotone in both, then grows each
// anchor byte-wise and merges neighbours that share a shift across up to
// segGap mismatching bytes. Both bodies are canonical, so an anchor is not
// broken by a call target that merely moved. O(n) hashing plus one O(n log n)
// chain per function; bodies average two kilobytes.
func alignBodies(oldC, newC []byte, oldB, newB []int32) []segPiece {
	if len(oldC) < segWin || len(newC) < segWin {
		return nil
	}
	idx := make(map[uint64][]int32, len(oldC)/8)
	for _, p := range oldB {
		if int(p)+segWin > len(oldC) {
			break
		}
		h := winHash(oldC, int(p), segWin)
		if len(idx[h]) < segCand {
			idx[h] = append(idx[h], p)
		}
	}
	type anchor struct{ q, p int32 }
	var as []anchor
	for _, q := range newB {
		if int(q)+segWin > len(newC) {
			break
		}
		ps := idx[winHash(newC, int(q), segWin)]
		for k := len(ps) - 1; k >= 0; k-- { // descending p: at most one per q
			as = append(as, anchor{q, ps[k]})
		}
	}
	if len(as) == 0 {
		return nil
	}
	// longest chain with strictly increasing p (q is already non-decreasing)
	tails := make([]int, 0, len(as))
	prev := make([]int, len(as))
	for i, a := range as {
		k := sort.Search(len(tails), func(k int) bool { return as[tails[k]].p >= a.p })
		prev[i] = -1
		if k > 0 {
			prev[i] = tails[k-1]
		}
		if k == len(tails) {
			tails = append(tails, i)
		} else {
			tails[k] = i
		}
	}
	chain := make([]anchor, 0, len(tails))
	for i := tails[len(tails)-1]; i >= 0; i = prev[i] {
		chain = append(chain, as[i])
	}
	for l, r := 0, len(chain)-1; l < r; l, r = l+1, r-1 {
		chain[l], chain[r] = chain[r], chain[l]
	}

	// paint each anchor's window with its shift, the first anchor winning
	off := make([]int32, len(newC))
	has := make([]bool, len(newC))
	for _, a := range chain {
		for k := 0; k < segWin; k++ {
			q, p := int(a.q)+k, int(a.p)+k
			if q >= len(newC) || p >= len(oldC) || has[q] {
				continue
			}
			has[q], off[q] = true, a.p-a.q
		}
	}

	var segs []segPiece
	pn, po := 0, 0 // end of the previous piece, in each body
	for i := 0; i < len(newC); {
		if !has[i] {
			i++
			continue
		}
		d := int(off[i])
		j := i
		for j < len(newC) && has[j] && int(off[j]) == d {
			j++
		}
		// clipped to the previous piece's end in *both* bodies: the chain is
		// monotone in each, but its shifts are not, so a piece that starts
		// further back than the last one ended would overlap it
		a, b := max(i, pn), j
		if a+d < po {
			a = po - d
		}
		if a >= b {
			i = j
			continue
		}
		for a > pn && a+d > po && oldC[a+d-1] == newC[a-1] {
			a--
		}
		for b < len(newC) && b+d < len(oldC) && oldC[b+d] == newC[b] {
			b++
		}
		if n := len(segs); n > 0 && segs[n-1].Old-segs[n-1].New == int32(d) &&
			int32(a)-(segs[n-1].New+segs[n-1].N) <= segGap {
			segs[n-1].N = int32(b) - segs[n-1].New
		} else {
			segs = append(segs, segPiece{Old: int32(a + d), New: int32(a), N: int32(b - a)})
		}
		pn, po = b, b+d
		i = max(j, b)
	}
	return segs
}

// snap moves a piece's start forward and its end back to instruction
// boundaries of the old body, so that the decoder's Relocate restarts on an
// instruction. bounds ends with the body length, which is a boundary too.
func snap(s segPiece, bounds []int32) (segPiece, bool) {
	i := sort.Search(len(bounds), func(k int) bool { return bounds[k] >= s.Old })
	if i == len(bounds) {
		return s, false
	}
	if d := bounds[i] - s.Old; d > 0 {
		s.Old, s.New, s.N = s.Old+d, s.New+d, s.N-d
	}
	j := sort.Search(len(bounds), func(k int) bool { return bounds[k] > s.Old+s.N })
	if j == 0 {
		return s, false
	}
	s.N = bounds[j-1] - s.Old
	return s, s.N > 0
}

// alignFunc aligns one resized matched pair and keeps the pieces worth
// transmitting. A piece below segMin does not pay for its entry, and a piece
// at shift 0 is free by omission: the decoder's positional fill already
// produces exactly those bytes.
func alignFunc(oldBody, newBody []byte) []segPiece {
	oldC, oldB := x86.Canonical(oldBody)
	newC, newB := x86.Canonical(newBody)
	segs := alignBodies(oldC, newC, oldB, newB)
	out := segs[:0]
	for _, s := range segs {
		s, ok := snap(s, oldB)
		if !ok || s.N < segMin || s.Old == s.New {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildSegMaps aligns every matched pair whose size changed, which is the
// whole scope: a same-length pair is already at offset 0 nearly everywhere,
// and an unmatched function has no old body to segment. Alignment is per
// function and order-independent, so the worker count does not change the
// patch.
func buildSegMaps(old, new *gobin.Bin, m *match) []segMap {
	per := make([][]segPiece, len(new.Funcs))
	var wg sync.WaitGroup
	for w := range predictWorkers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := w; j < len(new.Funcs); j += predictWorkers {
				i := m.NewToOld[j]
				if i < 0 || old.Funcs[i].Size() == new.Funcs[j].Size() {
					continue
				}
				per[j] = alignFunc(old.FuncBytes(old.Funcs[i]), new.FuncBytes(new.Funcs[j]))
			}
		}(w)
	}
	wg.Wait()
	var maps []segMap
	for j, s := range per {
		if len(s) > 0 {
			maps = append(maps, segMap{j, s})
		}
	}
	return maps
}

// encodeSegMaps writes the sparse list as five contiguous varint columns,
// for the same reason the correction splits ctrl from lit: the five have
// statistics an order of magnitude apart and each compresses against itself.
func encodeSegMaps(w *wbuf, maps []segMap) {
	w.u(uint64(len(maps)))
	prev := 0
	for _, sm := range maps {
		w.u(uint64(sm.Idx - prev))
		prev = sm.Idx
	}
	for _, sm := range maps {
		w.u(uint64(len(sm.Segs)))
	}
	for _, sm := range maps {
		var end int32
		for _, s := range sm.Segs {
			w.u(uint64(s.New - end))
			end = s.New + s.N
		}
	}
	for _, sm := range maps {
		for _, s := range sm.Segs {
			w.u(uint64(s.N))
		}
	}
	for _, sm := range maps {
		var prevD int32
		for _, s := range sm.Segs {
			w.s(int64(s.Old - s.New - prevD))
			prevD = s.Old - s.New
		}
	}
}

// decodeSegMaps reads the five columns and checks everything that can be
// checked without the function list: indices strictly increasing and below
// nfunc, at least one piece each, and pieces monotone and non-overlapping in
// both bodies. The sizes are checked in skeleton, where the bodies are known.
func decodeSegMaps(r *rbuf, nfunc int) []segMap {
	n := r.un(uint64(min(nfunc, len(r.b)+1)), "segment map count")
	if n == 0 {
		return nil
	}
	maps := make([]segMap, n)
	prev := 0
	for i := range maps {
		g := int(r.un(uint64(nfunc), "segment map index gap"))
		if r.err != nil {
			return nil
		}
		idx := prev + g
		if idx >= nfunc || (i > 0 && g == 0) {
			r.fail("segment map index %d is out of order or out of range", idx)
			return nil
		}
		maps[i].Idx, prev = idx, idx
	}
	// a piece costs at least three varints, so the columns still to come
	// bound how many of them the stream can hold
	room := uint64(len(r.b))
	for i := range maps {
		ns := r.un(room, "piece count")
		if r.err != nil {
			return nil
		}
		if ns == 0 {
			r.fail("function %d carries an empty segment map", maps[i].Idx)
			return nil
		}
		maps[i].Segs = make([]segPiece, ns)
		room -= ns
	}
	// the gaps are relative to the previous piece's end, so they become
	// offsets only once the lengths are in; New carries the raw gap until then
	for i := range maps {
		for k := range maps[i].Segs {
			maps[i].Segs[k].New = int32(r.un(segMaxOff, "piece gap"))
		}
	}
	for i := range maps {
		var end int64
		for k := range maps[i].Segs {
			s := &maps[i].Segs[k]
			n := int64(r.un(segMaxOff, "piece length"))
			if r.err != nil {
				return nil
			}
			if n == 0 {
				r.fail("function %d has a zero-length piece", maps[i].Idx)
				return nil
			}
			// pieces are monotone in the new body by construction; only the
			// running total can leave any function
			pos := end + int64(s.New)
			if pos+n > segMaxOff {
				r.fail("a piece of function %d is outside any function", maps[i].Idx)
				return nil
			}
			s.New, s.N = int32(pos), int32(n)
			end = pos + n
		}
	}
	for i := range maps {
		var d, oldEnd int64
		for k := range maps[i].Segs {
			s := &maps[i].Segs[k]
			d += r.s()
			o := int64(s.New) + d
			if r.err != nil {
				return nil
			}
			if o < oldEnd || o+int64(s.N) > segMaxOff {
				r.fail("function %d has pieces that overlap in the old body", maps[i].Idx)
				return nil
			}
			s.Old, oldEnd = int32(o), o+int64(s.N)
		}
	}
	return maps
}

// checkSegMaps is the rest of the decoder's bounds check: every mapped
// function must be matched, and every piece must fit both bodies.
func checkSegMaps(maps []segMap, old *gobin.Bin, funcs []*gobin.Func, m *match) error {
	for _, sm := range maps {
		if sm.Idx >= len(funcs) {
			return fmt.Errorf("%w: segment map for function %d, which does not exist", errCorrupt, sm.Idx)
		}
		i := m.NewToOld[sm.Idx]
		if i < 0 {
			return fmt.Errorf("%w: segment map for unmatched function %d", errCorrupt, sm.Idx)
		}
		oldSize, newSize := int64(old.Funcs[i].Size()), int64(funcs[sm.Idx].Size())
		for _, s := range sm.Segs {
			if int64(s.New)+int64(s.N) > newSize || int64(s.Old)+int64(s.N) > oldSize {
				return fmt.Errorf("%w: a piece of function %d runs past its body", errCorrupt, sm.Idx)
			}
		}
	}
	return nil
}

// segsByIdx is the decoded list in the form the mapper asks it: the pieces
// of the 0.6 % of functions that have any, by new function index.
func segsByIdx(maps []segMap) map[int][]segPiece {
	if len(maps) == 0 {
		return nil
	}
	byIdx := make(map[int][]segPiece, len(maps))
	for _, sm := range maps {
		byIdx[sm.Idx] = sm.Segs
	}
	return byIdx
}

// mapSegOff turns an offset into a resized function's old body into an
// offset into its new one. Transmitted pieces take precedence, which makes
// this the inverse of the covering list predictText lays down; case 3 is an
// old byte that was deleted or displaced, where every answer is a guess and
// only determinism matters (docs/DESIGN.md 3.2.1).
func mapSegOff(segs []segPiece, o, newSize uint64) uint64 {
	k := sort.Search(len(segs), func(k int) bool {
		return uint64(segs[k].Old)+uint64(segs[k].N) > o
	})
	if k < len(segs) && o >= uint64(segs[k].Old) {
		return uint64(segs[k].New) + (o - uint64(segs[k].Old))
	}
	if o < newSize && !coversNew(segs, o) {
		return o
	}
	var shift int64
	if k > 0 {
		shift = int64(segs[k-1].New) - int64(segs[k-1].Old)
	}
	return uint64(min(max(int64(o)+shift, 0), int64(newSize)))
}

// coversNew reports whether a transmitted piece claims new offset o.
func coversNew(segs []segPiece, o uint64) bool {
	k := sort.Search(len(segs), func(k int) bool {
		return uint64(segs[k].New)+uint64(segs[k].N) > o
	})
	return k < len(segs) && o >= uint64(segs[k].New)
}

// relocatePieces lays a mapped body down piece by piece. The covering list
// is the transmitted pieces plus an implicit shift-0 piece over every new
// byte they do not cover, and each piece is one Relocate at its own PC --
// the call predictText makes for an unmapped function, once per piece.
func relocatePieces(code, out []byte, srcPC, dstPC uint64, segs []segPiece,
	lookup func(uint64) x86.Target, st *x86.Stats) {
	end := 0
	fill := func(lo, hi int) {
		if lo >= hi {
			return
		}
		a, b := min(lo, len(code)), min(hi, len(code))
		x86.Relocate(code[a:b], out[lo:hi], srcPC+uint64(lo), dstPC+uint64(lo), lookup, st, nil)
	}
	for _, s := range segs {
		fill(end, int(s.New))
		x86.Relocate(code[s.Old:s.Old+s.N], out[s.New:s.New+s.N],
			srcPC+uint64(s.Old), dstPC+uint64(s.New), lookup, st, nil)
		end = int(s.New + s.N)
	}
	fill(end, len(out))
}
