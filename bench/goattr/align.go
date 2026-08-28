package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
)

// ---------------------------------------------------------------- level 7
//
// The codec copies a matched old body positionally from the function entry
// and relocates it at the new PCs. Where the new body is a resized version of
// the old one -- an instruction inserted near the top -- every byte after the
// insertion lands at the wrong PC, and level 4 reads that as "prediction
// mid-instruction": 58% of the .text residual on the minor pair.
//
// The candidate fix is a sub-function segment map: the encoder aligns the two
// bodies and ships (old offset, new offset, length) segments, the decoder
// assembles the new body out of old segments at their true positions and only
// then relocates. This level measures that scheme's ceiling before it is
// built -- how much of the new body an alignment can cover, what is left, and
// what the segment list itself costs.
//
// Both bodies are canonicalised first (opclass.go, every PC-relative field
// zeroed), so a segment's residual is what re-relocation would *not* fix: the
// operand-level change level 4b priced and chrome-elf-whole-image.md 15 found
// not worth modelling.

// seg is one aligned stretch: n bytes of the old body at oldOff line up with
// the new body at newOff.
type seg struct{ oldOff, newOff, n int }

// boundaries returns the instruction start offsets of code, walked exactly
// the way canonicalise walks it so the two agree on where a field was zeroed.
func boundaries(code []byte) []int32 {
	out := make([]int32, 0, len(code)/4+1)
	for i := 0; i < len(code); {
		out = append(out, int32(i))
		inst, ok := safeDecode(code[i:])
		if !ok {
			i++
			continue
		}
		i += inst.Len
	}
	return out
}

func winHash(b []byte, p, w int) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b[p : p+w] {
		h = (h ^ uint64(c)) * 1099511628211
	}
	return h
}

const (
	alignWin = 12 // window hashed at each instruction boundary
	alignGap = 16 // mismatching bytes a segment will bridge rather than split
	maxCand  = 8  // candidate old positions kept per window hash
)

// alignBodies anchors instruction-aligned windows of the canonicalised old
// body in the canonicalised new one, keeps the longest monotone chain of
// anchors, then grows each anchor byte-wise and merges neighbours that share
// an offset. O(n) hashing plus one O(n log n) chain per function; bodies are
// two kilobytes.
func alignBodies(oldC, newC []byte, win, gap int) []seg {
	if len(oldC) < win || len(newC) < win {
		return nil
	}
	idx := make(map[uint64][]int32, len(oldC)/8)
	for _, p := range boundaries(oldC) {
		if int(p)+win > len(oldC) {
			break
		}
		h := winHash(oldC, int(p), win)
		if len(idx[h]) < maxCand {
			idx[h] = append(idx[h], p)
		}
	}
	type anchor struct{ q, p int32 }
	var as []anchor
	for _, q := range boundaries(newC) {
		if int(q)+win > len(newC) {
			break
		}
		ps := idx[winHash(newC, int(q), win)]
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

	// paint each anchor's window with its offset, first anchor winning
	off := make([]int32, len(newC))
	has := make([]bool, len(newC))
	for _, a := range chain {
		for k := 0; k < win; k++ {
			q, p := int(a.q)+k, int(a.p)+k
			if q >= len(newC) || p >= len(oldC) || has[q] {
				continue
			}
			has[q], off[q] = true, a.p-a.q
		}
	}

	var segs []seg
	pn, po := 0, 0 // end of the previous segment, in each body
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
		a, b := i, j
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
		if n := len(segs); n > 0 && segs[n-1].oldOff-segs[n-1].newOff == d &&
			a-(segs[n-1].newOff+segs[n-1].n) <= gap {
			segs[n-1].n = b - segs[n-1].newOff
		} else {
			segs = append(segs, seg{oldOff: a + d, newOff: a, n: b - a})
		}
		pn, po = b, b+d
		i = j
		if i < b {
			i = b
		}
	}
	return segs
}

// alignStat is one bucket of functions, aligned.
type alignStat struct {
	funcs, newB, oldB int
	covered, residual int
	cov0, res0        int // the same, restricted to segments at offset 0
	segs              int
	baseCorrect       int // bytes the current positional copy already gets right
	hist              [6]int
	wrong             []int // file offsets the scheme would still leave wrong
	runs              int
	planSegs          []seg
	planCnt           []int
	planIdx           []int // the function each plan entry belongs to
	trivial           int   // functions whose map is what the codec already does
}

var segHistNames = [6]string{"1 segment", "2", "3-4", "5-8", "9-16", "17+"}

func histBucket(n int) int {
	switch {
	case n <= 1:
		return 0
	case n == 2:
		return 1
	case n <= 4:
		return 2
	case n <= 8:
		return 3
	case n <= 16:
		return 4
	}
	return 5
}

// alignOne aligns one function and folds it into st.
func (c *ctx) alignOne(st *alignStat, i, j int) {
	f, g := c.ob.Funcs[i], c.nb.Funcs[j]
	lo, hi := c.funcRange(g)
	if lo < 0 || hi > len(c.new) {
		return
	}
	oldC := canonicalise(c.ob.FuncBytes(f))
	newC := canonicalise(c.new[lo:hi])
	st.funcs++
	st.newB += len(newC)
	st.oldB += len(oldC)
	for k := 0; k < min(len(oldC), len(newC)); k++ {
		if oldC[k] == newC[k] {
			st.baseCorrect++
		}
	}
	segs := alignBodies(oldC, newC, alignWin, alignGap)
	st.segs += len(segs)
	st.hist[histBucket(len(segs))]++
	// A single segment at offset 0 is what the codec already does, so it
	// costs the plan nothing: only the others need an entry.
	if len(segs) == 1 && segs[0].oldOff == 0 && segs[0].newOff == 0 {
		st.trivial++
	} else {
		st.planCnt = append(st.planCnt, len(segs))
		st.planIdx = append(st.planIdx, j)
		st.planSegs = append(st.planSegs, segs...)
	}
	covd := make([]bool, len(newC))
	for _, s := range segs {
		st.covered += s.n
		if s.oldOff == s.newOff {
			st.cov0 += s.n
		}
		for k := range s.n {
			covd[s.newOff+k] = true
			if oldC[s.oldOff+k] != newC[s.newOff+k] {
				st.residual++
				if s.oldOff == s.newOff {
					st.res0++
				}
				st.wrong = append(st.wrong, lo+s.newOff+k)
			}
		}
	}
	for k := range newC {
		if !covd[k] {
			st.wrong = append(st.wrong, lo+k)
		}
	}
}

// planCost encodes the segment list the way a plan would -- four varint
// columns, each contiguous so its own statistics compress -- and prices it
// with xz -T1 and with the codec's own compressor.
func planCost(st *alignStat) (xz, cz int, cols [5]int) {
	var ids, count, gaps, lens, offs []byte
	var buf [10]byte
	put := func(dst *[]byte, v uint64) { *dst = append(*dst, buf[:binary.PutUvarint(buf[:], v)]...) }
	puts := func(dst *[]byte, v int64) { *dst = append(*dst, buf[:binary.PutVarint(buf[:], v)]...) }
	k, prevID := 0, 0
	for e, n := range st.planCnt {
		put(&ids, uint64(st.planIdx[e]-prevID))
		prevID = st.planIdx[e]
		put(&count, uint64(n))
		end, prevD := 0, 0
		for _, s := range st.planSegs[k : k+n] {
			put(&gaps, uint64(max(0, s.newOff-end)))
			put(&lens, uint64(s.n))
			puts(&offs, int64(s.oldOff-s.newOff-prevD))
			end, prevD = s.newOff+s.n, s.oldOff-s.newOff
		}
		k += n
	}
	all := append(append(append(append(append([]byte(nil), ids...), count...), gaps...), lens...), offs...)
	s := xzSizes(ids, count, gaps, lens, offs, all)
	return s[5], czSize(all), [5]int{s[0], s[1], s[2], s[3], s[4]}
}

// countRuns is regions() over a set of file offsets, deduplicated: how many
// correction regions the codec would write for exactly these wrong bytes.
func countRuns(pos []int) (runs, bytes int) {
	if len(pos) == 0 {
		return 0, 0
	}
	sort.Ints(pos)
	runs, bytes, last := 1, 1, pos[0]
	for _, p := range pos[1:] {
		if p == last {
			continue
		}
		if p-last > mergeGap {
			runs++
		}
		bytes++
		last = p
	}
	return runs, bytes
}

func (c *ctx) alignment() {
	fs := c.fields()
	mask := c.fieldMask(fs)
	var resized, same alignStat
	fieldWrong := [2]int{}
	curWrong, curRuns := [2]int{}, [2]int{}
	for j := range c.nb.Funcs {
		i := c.pr.NewToOld[j]
		if i < 0 {
			continue
		}
		k := c.causeOf(j)
		lo, hi := c.funcRange(c.nb.Funcs[j])
		if lo < 0 || hi > len(c.new) {
			continue
		}
		wrong, inField := 0, 0
		for p := lo; p < hi; p++ {
			if c.pred[p] != c.new[p] {
				wrong++
				if mask[p] {
					inField++
				}
			}
		}
		var st *alignStat
		var b int
		switch {
		case k == cvNameResized:
			st, b = &resized, 0
		case k == cvNameSame && wrong > 0:
			st, b = &same, 1
		default:
			continue
		}
		c.alignOne(st, i, j)
		fieldWrong[b] += inField
		curWrong[b] += wrong
		// A wrong byte inside a relocated field stays wrong: whether the
		// mapper picks the right target is level 3b's question, not this
		// one, so the scheme is charged for every one of them.
		for p := lo; p < hi; p++ {
			if mask[p] && c.pred[p] != c.new[p] {
				st.wrong = append(st.wrong, p)
			}
		}
	}
	tlo, thi := int(c.nb.Text.Off), int(c.nb.Text.Off+c.nb.Text.Size)
	for _, r := range c.regs {
		if r.s < tlo || r.s >= thi {
			continue
		}
		f := c.nb.FuncAt(c.nb.Text.Addr + uint64(r.s-tlo))
		if f == nil {
			continue
		}
		switch c.causeOf(f.Idx) {
		case cvNameResized:
			curRuns[0]++
		case cvNameSame:
			curRuns[1]++
		}
	}
	var leftB [2]int
	resized.runs, leftB[0] = countRuns(resized.wrong)
	same.runs, leftB[1] = countRuns(same.wrong)

	fmt.Fprintf(os.Stderr, "\n-- 7. sub-function alignment ceiling (window %d B, bridged gap %d B)\n", alignWin, alignGap)
	head := "  %-34s %8s %10s %10s %10s %10s %9s %10s\n"
	body := "  %-34s %8d %10d %10d %10d %10d %9d %10d\n"
	fmt.Fprintf(os.Stderr, head, "bucket", "funcs", "new B", "covered B", "inserted B", "residual B", "segments", "runs left")
	for _, r := range []struct {
		n string
		s *alignStat
	}{{"name-matched, resized", &resized}, {"name-matched, same length, wrong", &same}} {
		fmt.Fprintf(os.Stderr, body, r.n, r.s.funcs, r.s.newB, r.s.covered, r.s.newB-r.s.covered,
			r.s.residual, r.s.segs, r.s.runs)
	}
	fmt.Fprintf(os.Stderr, "  current positional copy gets right: resized %d of %d B, same-length %d of %d B\n",
		resized.baseCorrect, resized.newB, same.baseCorrect, same.newB)
	fmt.Fprintf(os.Stderr, "  same-length: %d B covered at offset 0 (in-place change), %d B covered at a shifted offset\n",
		same.cov0, same.covered-same.cov0)
	fmt.Fprintf(os.Stderr, "  resized:     %d B covered at offset 0, %d B covered at a shifted offset\n",
		resized.cov0, resized.covered-resized.cov0)

	fmt.Fprintf(os.Stderr, "\n-- 7b. segments per function\n")
	fmt.Fprintf(os.Stderr, "  %-34s %8s %8s\n", "segments", "resized", "same-len")
	for b, n := range segHistNames {
		fmt.Fprintf(os.Stderr, "  %-34s %8d %8d\n", n, resized.hist[b], same.hist[b])
	}

	fmt.Fprintf(os.Stderr, "\n-- 7c. priced (yardstick %.3f per run + %.3f per wrong byte)\n", fitA, fitB)
	fmt.Fprintf(os.Stderr, "  %-30s %9s %9s %9s %9s %9s %9s %9s\n",
		"bucket", "gross", "gross fit", "plan xz", "plan cz", "resid fit", "net meas", "net fit")
	for b, r := range []struct {
		n     string
		s     *alignStat
		gross int
	}{
		{"name-matched, resized", &resized, marginalResized(c)},
		{"name-matched, same length", &same, marginalSameLen(c)},
	} {
		xz, cz, cols := planCost(r.s)
		cost := int(fitA*float64(r.s.runs) + fitB*float64(leftB[b]))
		gfit := int(fitA*float64(curRuns[b]) + fitB*float64(curWrong[b]))
		fmt.Fprintf(os.Stderr, "  %-30s %9d %9d %9d %9d %9d %9d %9d\n",
			r.n, r.gross, gfit, xz, cz, cost, r.gross-xz-cost, gfit-xz-cost)
		fmt.Fprintf(os.Stderr, "    plan: %d functions carry a non-trivial map, %d do not; xz by column -- ids %d, count %d, gaps %d, lengths %d, offsets %d\n",
			len(r.s.planCnt), r.s.trivial, cols[0], cols[1], cols[2], cols[3], cols[4])
		fmt.Fprintf(os.Stderr, "    charged: %d inserted + %d residual + %d relocated-field bytes = %d B in %d runs (now %d B in %d runs)\n",
			r.s.newB-r.s.covered, r.s.residual, fieldWrong[b], leftB[b], r.s.runs, curWrong[b], curRuns[b])
	}
}

// marginalResized and marginalSameLen re-measure the two buckets level 2
// prices, so 7c is not reading a number out of a printed table.
func (c *ctx) bucketMarginal(want int) int {
	var pos []int
	tlo, thi := int(c.nb.Text.Off), int(c.nb.Text.Off+c.nb.Text.Size)
	for j := range c.nb.Funcs {
		if c.causeOf(j) != want {
			continue
		}
		lo, hi := c.funcRange(c.nb.Funcs[j])
		for p := max(lo, tlo); p < min(hi, thi); p++ {
			if c.pred[p] != c.new[p] {
				pos = append(pos, p)
			}
		}
	}
	m, _ := c.marginal(pos)
	return m
}

func marginalResized(c *ctx) int { return c.bucketMarginal(cvNameResized) }
func marginalSameLen(c *ctx) int { return c.bucketMarginal(cvNameSame) }
