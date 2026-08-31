package main

// PROBE ONLY -- measurement scaffolding, not part of the shipped codec.
//
// Two questions the existing cmix rungs cannot answer:
//
//  A. The match model in cmcoder.go searches only the stream it is coding --
//     the wrong bytes, end to end, with every correct byte skipped. That is a
//     sparse, address-discontinuous stream, so its matches are accidental. But
//     the decoder holds far more than that: it holds the prediction, and it can
//     reconstruct the target image incrementally as it places each run. This
//     file gives the coder a match model whose *pointer lives in the
//     prediction* and whose *anchor is the partially reconstructed target
//     image*, in two flavours:
//
//       LOCAL  -- candidates restricted to +/- win bytes of the current
//                 address. Prices the "a byte was inserted or deleted inside a
//                 recompiled function, so pred[i] is off by a few" hypothesis.
//       GLOBAL -- candidates from a hash-chain index over the whole of the old
//                 .text. Prices the "the new bytes already exist somewhere else
//                 in the old binary" hypothesis (inlined callees, sibling
//                 template instantiations).
//
//     Every byte of every run differs from the prediction by construction, and
//     the decoder knows each run's length from the Lens column before it needs
//     the bytes -- so q == p is excluded from the candidate set for free.
//
//  B. The wrong-byte runs ship in address order within each length bucket. If
//     they shipped in a similarity order that both sides can compute from the
//     position columns plus the old image, the permutation costs nothing. Do
//     the contexts find more structure? See probeCMOrder.

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"sort"
	"time"
)

// --- reference match model ---------------------------------------------------

const (
	rmLocal = iota
	rmGlobal
	// rmLocalIdx is rmLocal's candidate set reached through an index instead of
	// a scan: the key carries the position's block number, so a chain holds only
	// candidates from one 4 KB block of the prediction and three chain walks
	// cover the whole window. It exists to separate the local hypothesis's value
	// from the probe's brute-force cost.
	rmLocalIdx
)

const localBlockBits = 12

const refIndexBits = 24

// refIndex is a hash-chain index over the reference image, keyed by the six
// bytes ending at each position. Built once, shared by every coder instance.
type refIndex struct {
	head []int32
	next []int32
}

func refHash6(b []byte) uint32 {
	var h uint32 = 0x811C9DC5
	for _, c := range b {
		h = (h ^ uint32(c)) * 16777619
	}
	return (h ^ h>>15) & (1<<refIndexBits - 1)
}

// blockKey folds the position's block number into the key so a chain holds only
// candidates from one block.
func blockKey(h uint32, q int) uint32 {
	return mix32(h*0x9E3779B1+uint32(q>>localBlockBits)) & (1<<refIndexBits - 1)
}

func buildRefIndex(ref []byte, blocked bool) *refIndex {
	ix := &refIndex{head: make([]int32, 1<<refIndexBits), next: make([]int32, len(ref)+1)}
	for i := range ix.head {
		ix.head[i] = -1
	}
	for q := 6; q <= len(ref); q++ {
		h := refHash6(ref[q-6 : q])
		if blocked {
			h = blockKey(h, q)
		}
		ix.next[q] = ix.head[h]
		ix.head[h] = int32(q)
	}
	return ix
}

type refMatch struct {
	ref []byte  // the prediction
	img []byte  // partially reconstructed target; starts as a copy of ref
	pos []int32 // stream index -> address in ref/img

	mode   int
	win    int // local half-window
	depth  int // global chain walk depth
	minLen int // shortest backward anchor accepted

	ix *refIndex

	ptr    int
	length int
	valid  bool
	expect byte
	lastP  int

	probs   [64 * 2]uint16
	cnt     [64 * 2]uint8
	idx     int
	predBit int

	searches, hits int64
}

func newRefMatch(ref []byte, pos []int32, mode, win, depth, minLen int, ix *refIndex) *refMatch {
	m := &refMatch{
		ref: ref, img: bytes.Clone(ref), pos: pos,
		mode: mode, win: win, depth: depth, minLen: minLen, ix: ix, lastP: -2,
	}
	for i := range m.probs {
		m.probs[i] = 1 << 15
	}
	return m
}

// backLen counts how far img[..p) and ref[..q) agree, walking backwards.
func (m *refMatch) backLen(p, q, limit int) int {
	n := 0
	for n < limit && p-1-n >= 0 && q-1-n >= 0 && m.img[p-1-n] == m.ref[q-1-n] {
		n++
	}
	return n
}

const refAnchorCap = 32

func (m *refMatch) search(p int) {
	m.searches++
	best, bestQ := m.minLen-1, -1
	consider := func(q int) {
		if q < 1 || q >= len(m.ref) || q == p {
			return
		}
		if n := m.backLen(p, q, refAnchorCap); n > best {
			best, bestQ = n, q
		}
	}
	switch m.mode {
	case rmLocal:
		for d := 1; d <= m.win; d++ {
			consider(p - d)
			consider(p + d)
		}
	case rmLocalIdx:
		if p < 6 {
			m.valid = false
			return
		}
		h := refHash6(m.img[p-6 : p])
		blk := p >> localBlockBits
		for b := blk - 1; b <= blk+1; b++ {
			if b < 0 {
				continue
			}
			q, n := m.ix.head[blockKey(h, b<<localBlockBits)], 0
			for q >= 0 && n < m.depth {
				if c := int(q); c > p-m.win && c < p+m.win {
					consider(c)
				}
				q, n = m.ix.next[q], n+1
			}
		}
	default:
		if p < 6 {
			m.valid = false
			return
		}
		h := refHash6(m.img[p-6 : p])
		q, n := m.ix.head[h], 0
		for q >= 0 && n < m.depth {
			consider(int(q))
			q, n = m.ix.next[q], n+1
		}
	}
	if bestQ < 0 {
		m.valid, m.length = false, 0
		return
	}
	m.hits++
	m.ptr, m.valid, m.length = bestQ, true, min(best, 63)
}

// startByte prepares the expectation for stream byte i.
func (m *refMatch) startByte(i int) {
	p := int(m.pos[i])
	// The running match only survives if the address advanced by one, so the
	// anchor is still the bytes immediately behind ptr.
	if !m.valid || p != m.lastP+1 {
		m.valid = false
	}
	if !m.valid {
		m.search(p)
	}
	if m.valid && m.ptr < len(m.ref) {
		m.expect = m.ref[m.ptr]
	} else {
		m.valid, m.length = false, 0
	}
}

func (m *refMatch) stretchIn(c0 uint32, done uint) float32 {
	m.idx = -1
	if !m.valid || m.length == 0 {
		return 0
	}
	if done > 0 && uint32(m.expect)>>(8-done) != c0&(1<<done-1) {
		m.valid, m.length = false, 0
		return 0
	}
	m.predBit = int(m.expect>>(7-done)) & 1
	m.idx = m.length*2 + m.predBit
	return stretchTab[m.probs[m.idx]>>(16-stretchBits)]
}

func (m *refMatch) update(bit int) {
	if m.idx < 0 {
		return
	}
	i := m.idx
	p := int32(m.probs[i])
	target := int32(0)
	if bit == 1 {
		target = 65535
	}
	m.probs[i] = uint16(p + ((target-p)*adaptRate[m.cnt[i]])>>16)
	if m.cnt[i] < 63 {
		m.cnt[i]++
	}
}

// endByte records the coded byte into the reconstructed image and walks the
// pointer forward when the expectation held.
func (m *refMatch) endByte(i int, b byte) {
	p := int(m.pos[i])
	m.img[p] = b
	m.lastP = p
	if m.valid && m.ptr < len(m.ref) && m.ref[m.ptr] == b {
		m.ptr++
		if m.length < 63 {
			m.length++
		}
	} else {
		m.valid, m.length = false, 0
	}
}

// --- context set carrying reference match models -----------------------------

// refCtx is rung 4's context set (pred[i] + field class/offset + stream match
// model) plus one or more reference match models.
type refCtx struct {
	corrCtx
	models []*refMatch
}

func (c *refCtx) refModels() []*refMatch { return c.models }

// --- similarity orderings ----------------------------------------------------

// orderKey names a decoder-derivable permutation of the runs inside a bucket.
// Every key reads only the prediction and the run's position and length, all of
// which the decoder holds before it needs a single replacement byte.
type orderKey struct {
	name string
	less func(pred []byte, a, b cmRun) bool
}

func predSlice(pred []byte, lo, hi int) []byte {
	lo = max(lo, 0)
	hi = min(hi, len(pred))
	if lo >= hi {
		return nil
	}
	return pred[lo:hi]
}

var orderKeys = []orderKey{
	{"address (shipped)", nil},
	{"pred[p:p+8] lex", func(pred []byte, a, b cmRun) bool {
		if v := bytes.Compare(predSlice(pred, a.lo, a.lo+8), predSlice(pred, b.lo, b.lo+8)); v != 0 {
			return v < 0
		}
		return a.lo < b.lo
	}},
	{"pred[p-8:p+8] lex", func(pred []byte, a, b cmRun) bool {
		if v := bytes.Compare(predSlice(pred, a.lo-8, a.lo+8), predSlice(pred, b.lo-8, b.lo+8)); v != 0 {
			return v < 0
		}
		return a.lo < b.lo
	}},
}

func sortRuns(rs []cmRun, pred []byte, k orderKey) {
	sort.SliceStable(rs, func(i, j int) bool { return k.less(pred, rs[i], rs[j]) })
}

func sortBytes(b []byte) { sort.Slice(b, func(i, j int) bool { return b[i] < b[j] }) }

// --- the probe ---------------------------------------------------------------

type sideInfo struct {
	buck, pred, cls, off, roff []byte
	pos                        []int32
	stream                     []byte
}

// buildSide walks the runs in the given order and lays out the coded stream and
// every per-position context column alongside it.
func buildSide(predText, targetText []byte, byBucket [][]cmRun, coded [correctionBuckets][]byte) *sideInfo {
	// The instruction walker only moves forward, so the field class and offset
	// of every wrong byte have to be resolved in one monotonic address pass --
	// exactly the pass probeCMCoder makes -- before any reordered emit. Doing it
	// bucket by bucket instead leaves the walker stranded past the end of .text
	// for every bucket after the first, which silently poisons those columns.
	type runInfo struct {
		cls, off []byte
		base     int // this run's slot in its bucket's byte array
	}
	info := make(map[int]*runInfo)
	var addr []cmRun
	for _, rs := range byBucket {
		addr = append(addr, rs...)
	}
	sort.Slice(addr, func(i, j int) bool { return addr[i].lo < addr[j].lo })

	var bucketAt [correctionBuckets]int
	var cls, off [16]byte
	curStart, curLen := 0, 0
	for _, r := range addr {
		ri := &runInfo{base: bucketAt[r.buck]}
		bucketAt[r.buck] += r.hi - r.lo
		for p := r.lo; p < r.hi; p++ {
			for curStart+curLen <= p {
				curStart += curLen
				if curStart >= len(predText) {
					curLen = 1
					break
				}
				if inst, ok := safeDecode(predText[curStart:]); ok {
					curLen = inst.Len
					instClasses(predText[curStart:curStart+curLen], cls[:curLen], off[:curLen])
				} else {
					curLen = 1
					cls[0], off[0] = fcUndecoded, 0
				}
			}
			k := p - curStart
			if k < 0 || k >= curLen {
				k = 0
			}
			ri.cls = append(ri.cls, cls[k])
			ri.off = append(ri.off, off[k])
		}
		info[r.lo] = ri
	}

	s := &sideInfo{}
	for b := 0; b < correctionBuckets; b++ {
		for _, r := range byBucket[b] {
			ri := info[r.lo]
			for p := r.lo; p < r.hi; p++ {
				k := p - r.lo
				s.stream = append(s.stream, coded[b][ri.base+k])
				s.buck = append(s.buck, byte(b))
				s.pred = append(s.pred, predText[p])
				s.cls = append(s.cls, ri.cls[k])
				s.off = append(s.off, ri.off[k])
				s.roff = append(s.roff, byte(min(k, 15)))
				s.pos = append(s.pos, int32(p))
			}
		}
	}
	return s
}

func (s *sideInfo) ctx(rung int) corrCtx {
	return corrCtx{s.buck, s.pred, s.cls, s.off, s.roff, rung}
}

// probeCMRef prices the reference match models (A) and the similarity
// orderings (B) on top of rung 4, the best existing cmix rung.
func probeCMRef(predText, targetText []byte, runs []cmRun, coded [correctionBuckets][]byte, bucketXZ, otherXZ, rung4 int) {
	byBucket := make([][]cmRun, correctionBuckets)
	for _, r := range runs {
		byBucket[r.buck] = append(byBucket[r.buck], r)
	}
	base := buildSide(predText, targetText, byBucket, coded)

	fmt.Fprintf(os.Stderr, "  probe cmix-ref: %d bytes of prediction .text, %d stream bytes, rung-4 baseline %d\n",
		len(predText), len(base.stream), rung4)
	fmt.Fprintf(os.Stderr, "    %-38s %9s %8s %9s %9s\n", "variant", "coded", "vs xz", "enc MB/s", "dec MB/s")

	report := func(name string, stream []byte, n int, encS, decS float64, extra string) {
		fmt.Fprintf(os.Stderr, "    %-38s %9d %8d %9.2f %9.2f  (%+d vs rung4, .text piece %d)%s\n",
			name, n, n-bucketXZ,
			float64(len(stream))/1e6/max(encS, 1e-9), float64(len(stream))/1e6/max(decS, 1e-9),
			n-rung4, n+otherXZ, extra)
	}

	run := func(name string, stream []byte, mk func() cmContexts, note func(cmContexts) string) {
		set := mk()
		t := time.Now()
		out := cmEncode(stream, set)
		encS := time.Since(t).Seconds()
		extra := ""
		if note != nil {
			extra = note(set)
		}
		set = nil
		runtime.GC()
		set2 := mk()
		t = time.Now()
		back := cmDecode(out, len(stream), set2)
		decS := time.Since(t).Seconds()
		if !bytes.Equal(back, stream) {
			fmt.Fprintf(os.Stderr, "    %-38s ROUND TRIP FAILED\n", name)
			return
		}
		report(name, stream, len(out), encS, decS, extra)
	}

	// Sanity: address order must reproduce rung 4 exactly.
	run("rung 4 (rebuilt, address order)", base.stream,
		func() cmContexts { c := base.ctx(4); return &c }, nil)

	stats := func(s cmContexts) string {
		var b []byte
		for _, m := range s.(*refCtx).models {
			b = fmt.Appendf(b, " [%d/%d anchored]", m.hits, m.searches)
		}
		return string(b)
	}
	local := func(win, minLen int) func() *refMatch {
		return func() *refMatch { return newRefMatch(predText, base.pos, rmLocal, win, 0, minLen, nil) }
	}

	// --- Probe A1: local realignment ----------------------------------------
	run("A1 local realign win=512 min=4", base.stream, func() cmContexts {
		return &refCtx{base.ctx(4), []*refMatch{local(512, 4)()}}
	}, stats)
	run("A1 local realign win=4096 min=4", base.stream, func() cmContexts {
		return &refCtx{base.ctx(4), []*refMatch{local(4096, 4)()}}
	}, stats)

	// A1 again, with the same candidate window reached through a block-keyed
	// index instead of a scan, to separate the hypothesis from the probe's cost.
	t := time.Now()
	bix := buildRefIndex(predText, true)
	fmt.Fprintf(os.Stderr, "    (built block-keyed index in %.1fs)\n", time.Since(t).Seconds())
	localIdx := func(win, minLen int) func() *refMatch {
		return func() *refMatch { return newRefMatch(predText, base.pos, rmLocalIdx, win, 64, minLen, bix) }
	}
	run("A1 local indexed win=4096 min=6", base.stream, func() cmContexts {
		return &refCtx{base.ctx(4), []*refMatch{localIdx(4096, 6)()}}
	}, stats)

	// --- Probe A2: global dictionary ----------------------------------------
	t = time.Now()
	ix := buildRefIndex(predText, false)
	fmt.Fprintf(os.Stderr, "    (built hash-chain index over %d bytes in %.1fs)\n", len(predText), time.Since(t).Seconds())
	global := func(depth, minLen int) func() *refMatch {
		return func() *refMatch { return newRefMatch(predText, base.pos, rmGlobal, 0, depth, minLen, ix) }
	}
	for _, depth := range []int{64, 256} {
		name := fmt.Sprintf("A2 global dict depth=%d min=6", depth)
		mk := global(depth, 6)
		run(name, base.stream, func() cmContexts {
			return &refCtx{base.ctx(4), []*refMatch{mk()}}
		}, stats)
	}

	// --- A1 + A2 together ----------------------------------------------------
	run("A1(512,4) + A2(64,6)   [cheap]", base.stream, func() cmContexts {
		return &refCtx{base.ctx(4), []*refMatch{local(512, 4)(), global(64, 6)()}}
	}, nil)
	run("A1(4096,4) + A2(256,6) [best]", base.stream, func() cmContexts {
		return &refCtx{base.ctx(4), []*refMatch{local(4096, 4)(), global(256, 6)()}}
	}, nil)
	run("A1idx(4096,6) + A2(256,6) [fast]", base.stream, func() cmContexts {
		return &refCtx{base.ctx(4), []*refMatch{localIdx(4096, 6)(), global(256, 6)()}}
	}, nil)

	// --- Probe B: similarity-ordered residual --------------------------------
	// Priced two ways: against xz, which is what the shipped format would pay,
	// and against the CM rungs, with and without the reference models -- a
	// reordering that only feeds the stream match model may not compound with a
	// model that already reaches into the prediction.
	splitXZ := func(s *sideInfo) int {
		var bs [correctionBuckets][]byte
		for i, b := range s.buck {
			bs[b] = append(bs[b], s.stream[i])
		}
		n := 0
		for _, sz := range xzSizes(bs[:]...) {
			n += sz
		}
		return n
	}
	fmt.Fprintf(os.Stderr, "    B baseline (address order): xz 5-stream %d, xz concatenated %d\n",
		splitXZ(base), xzSize(base.stream))
	for _, k := range orderKeys[1:2] {
		ord := make([][]cmRun, correctionBuckets)
		for b := range byBucket {
			ord[b] = append([]cmRun(nil), byBucket[b]...)
			sort.SliceStable(ord[b], func(i, j int) bool { return k.less(predText, ord[b][i], ord[b][j]) })
		}
		s := buildSide(predText, targetText, ord, coded)
		fmt.Fprintf(os.Stderr, "    B order %-24s xz 5-stream %8d (%+d), xz concat %8d (%+d)\n",
			k.name, splitXZ(s), splitXZ(s)-bucketXZ, xzSize(s.stream), xzSize(s.stream)-xzSize(base.stream))
		run("B "+k.name+" + rung4", s.stream, func() cmContexts { c := s.ctx(4); return &c }, nil)
		run("B "+k.name+" + A1+A2", s.stream, func() cmContexts {
			return &refCtx{s.ctx(4), []*refMatch{
				newRefMatch(predText, s.pos, rmLocal, 4096, 0, 4, nil),
				newRefMatch(predText, s.pos, rmGlobal, 0, 256, 6, ix)}}
		}, nil)
	}
}
