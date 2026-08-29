package delta

import (
	"sort"

	"github.com/wjordan/go-binsync/delta/gobin"

	"github.com/wjordan/go-binsync/delta/x86"
)

// Far pieces (transform 3). A resized function's aligner finds the parts of
// the new body that are old body shifted; what it cannot find is the code
// the edit added. That code is rarely new to the binary: the compiler
// emits the same sequences for the same constructs, so an added call or
// loop is usually somewhere else in the old .text already. A far piece
// names it there: its Old is an offset from the function's old entry that
// may fall before the body or after it, and it is laid down and relocated
// at its true old PC like any other piece. The old-body monotonicity of
// transform 2 is dropped for them; the new-body monotonicity stays.
//
// The index below is the encoder's: every instruction boundary of the old
// .text, in canonical form, hashed over segWin bytes and sorted, so a
// window's candidates are one binary search away.

type textIndex struct {
	text   []byte
	canon  []byte
	bounds []int32
	keys   []uint64
	pos    []int32
}

// indexText builds the index over one .text section.
func indexText(text []byte) *textIndex {
	canon, bounds := x86.Canonical(text)
	ix := &textIndex{text: text, canon: canon, bounds: bounds}
	type entry struct {
		h uint64
		p int32
	}
	ents := make([]entry, 0, len(bounds))
	for _, p := range bounds {
		if int(p)+segWin > len(canon) {
			break
		}
		ents = append(ents, entry{winHash(canon, int(p), segWin), p})
	}
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].h != ents[j].h {
			return ents[i].h < ents[j].h
		}
		return ents[i].p < ents[j].p
	})
	ix.keys, ix.pos = make([]uint64, len(ents)), make([]int32, len(ents))
	for i, e := range ents {
		ix.keys[i], ix.pos[i] = e.h, e.p
	}
	return ix
}

// candidates returns the old positions whose window matches, up to
// farCand of them.
func (ix *textIndex) candidates(h uint64) []int32 {
	i := sort.Search(len(ix.keys), func(i int) bool { return ix.keys[i] >= h })
	j := i
	for j < len(ix.keys) && ix.keys[j] == h && j-i < farCand {
		j++
	}
	return ix.pos[i:j]
}

const (
	// farCand is how many index hits a window considers. The compiler's
	// commonest sequences have thousands; the longest extensions among
	// the first few hundred are tried.
	farCand = 256
	// farTry is how many of those, longest first, are scored by relocation.
	farTry = 4
	// farMin is the least a far piece must save the correction, by the
	// pricing above, to be worth its entry (a wide old offset, a few bytes
	// compressed) and the runs it splits at its ends.
	farMin = 20
)

// farAlign adds far pieces to one resized pair's local ones. It lays the
// body down as the decoder would with the local pieces alone, and wherever
// that fill is wrong looks the new code up in the old .text; a candidate is
// kept when, relocated at its own PC, it beats the fill by farMin bytes.
func (ix *textIndex) farAlign(old, new *gobin.Bin, f, g *gobin.Func, local []segPiece, lookup func(uint64) x86.Target) []segPiece {
	oldBody, newBody := old.FuncBytes(f), new.FuncBytes(g)
	base := f.Entry - old.Text.Addr
	fill := make([]byte, len(newBody))
	relocatePieces(old.Text.Data, oldBody, fill, base, f.Entry, g.Entry, local, lookup, &x86.Stats{})
	newC, newB := x86.Canonical(newBody)
	covered := make([]bool, len(newC))
	for _, s := range local {
		for k := s.New; k < s.New+s.N; k++ {
			covered[k] = true
		}
	}
	// a prediction is priced as the correction would price it: a wrong run
	// costs its bytes and a header
	cost := func(pred []byte, q, n int) int {
		c := 0
		for k := q; k < q+n; k++ {
			if pred[k] != newBody[k] {
				c++
				if k == q || pred[k-1] == newBody[k-1] {
					c += 2
				}
			}
		}
		return c
	}
	buf := make([]byte, len(newBody))
	gain := func(p int32, q, n int) int {
		x86.Relocate(ix.text[p:int(p)+n], buf[q:q+n], old.Text.Addr+uint64(p), g.Entry+uint64(q), lookup, &x86.Stats{}, nil)
		return cost(fill, q, n) - cost(buf, q, n)
	}
	var far []segPiece
	bi := 0
	for bi < len(newB) {
		q := int(newB[bi])
		bi++
		if q+segWin > len(newC) {
			break
		}
		if covered[q] || fillRight(fill, newBody, q, newB, bi) {
			continue
		}
		type cand struct {
			p int32
			n int
		}
		var cs []cand
		for _, p := range ix.candidates(winHash(newC, q, segWin)) {
			n := 0
			for q+n < len(newC) && !covered[q+n] && int(p)+n < len(ix.canon) && ix.canon[int(p)+n] == newC[q+n] {
				n++
			}
			if n >= segMin {
				cs = append(cs, cand{p, n})
			}
		}
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].n > cs[j].n })
		best, bestG := segPiece{}, 0
		for k, c := range cs {
			if k == farTry {
				break
			}
			s, ok := snap(segPiece{Old: c.p, New: int32(q), N: int32(c.n)}, ix.bounds)
			if !ok || s.N < segMin {
				continue
			}
			if gn := gain(s.Old, int(s.New), int(s.N)); gn > bestG {
				best, bestG = s, gn
			}
		}
		if bestG < farMin {
			continue
		}
		for k := best.New; k < best.New+best.N; k++ {
			covered[k] = true
		}
		best.Old -= int32(base)
		far = append(far, best)
		for bi < len(newB) && newB[bi] < best.New+best.N {
			bi++
		}
	}
	if len(far) == 0 {
		return local
	}
	out := append(append([]segPiece(nil), local...), far...)
	sort.Slice(out, func(i, j int) bool { return out[i].New < out[j].New })
	return out
}

// fillRight reports whether the fill already has the instruction at q, the
// bi'th boundary, right: an opcode is often right when its operand is not.
func fillRight(fill, want []byte, q int, bounds []int32, bi int) bool {
	end := len(want)
	if bi < len(bounds) {
		end = int(bounds[bi])
	}
	for k := q; k < end; k++ {
		if fill[k] != want[k] {
			return false
		}
	}
	return true
}

// isFar reports whether a piece reaches outside the old body.
func isFar(s segPiece, oldSize int64) bool {
	return s.Old < 0 || int64(s.Old)+int64(s.N) > oldSize
}

// localPieces is the subset of a function's pieces the offset map may use:
// those inside the old body and monotone in it, which is every piece a
// transform-2 map has.
func localPieces(segs []segPiece, oldSize int64) []segPiece {
	var out []segPiece
	var oldEnd int32
	for _, s := range segs {
		if isFar(s, oldSize) || s.Old < oldEnd {
			continue
		}
		out = append(out, s)
		oldEnd = s.Old + s.N
	}
	return out
}
