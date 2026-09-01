package elfmod

import (
	"encoding/binary"
	"errors"
	"slices"

	"github.com/wjordan/presage/delta/x86"
)

// .rodata is the largest non-.text cost at 409,800 XZ, and the shape of its
// error says what it is: of 71,494 wrong runs, 66,550 are four bytes or
// shorter and 54,849 start on a four-byte boundary. That is an array of 32-bit
// values churning like every other address column here.
//
// The values are clang's switch tables. Each entry holds its target minus the
// address of the table, so nothing relocates them -- they are self-relative --
// and the byte matcher has no way to know they are addresses at all. They are
// found by their signature instead: a run of entries that all land inside
// .text when added to the address of the run's first entry.

// A run of four is the length at which a false positive becomes unlikely by
// chance: .text is 77% of the image, so a random word lands in it about 5% of
// the time and four in a row about one time in 160,000. Shorter runs are real
// tables too -- small switches are the common case -- but they cannot be
// identified by the signature alone, so the encoder verifies them against the
// target and ships one bit each. See selectRoDataTables.
const (
	jumpTableMinRun   = 4
	jumpTableShortRun = 2
)

type roDataPlan struct {
	OldOff, OldSize  uint64
	NewOff, NewSize  uint64
	OldAddr, NewAddr uint64
	TextLo, TextHi   uint64 // old .text address range, the target test
	// Keep holds one bit per candidate, in the order roDataCandidates lists
	// them, saying the encoder checked that candidate against the target and
	// it made the prediction better. Every candidate is verified, long runs
	// included: a run that starts one word before a real table reads every
	// entry against the wrong base, and the signature cannot tell.
	Keep []byte
	// Seg lists the spans whose kept variant is placed by cursor rather than
	// by projection, as ascending index deltas -- a handful of spans out of a
	// hundred thousand, so a list and not a bitmap. Cursor holds the signed
	// corrections that placement reads: one per table, in span order, for
	// those spans alone.
	Seg    []byte
	Cursor []byte
}

func (p roDataPlan) marshal() []byte {
	var b []byte
	for _, v := range []uint64{p.OldOff, p.OldSize, p.NewOff, p.NewSize, p.OldAddr, p.NewAddr, p.TextLo, p.TextHi} {
		b = binary.AppendUvarint(b, v)
	}
	for _, s := range [][]byte{p.Keep, p.Seg, p.Cursor} {
		b = appendStream(b, s)
	}
	return b
}

func unmarshalRoDataPlan(b []byte) (roDataPlan, error) {
	r := &planReader{b: b}
	p := roDataPlan{
		OldOff: r.u(), OldSize: r.u(), NewOff: r.u(), NewSize: r.u(),
		OldAddr: r.u(), NewAddr: r.u(), TextLo: r.u(), TextHi: r.u(),
	}
	p.Keep, p.Seg, p.Cursor = r.stream().b, r.stream().b, r.stream().b
	if r.err != nil || !r.done() {
		return roDataPlan{}, errors.New("invalid rodata plan")
	}
	return p, nil
}

type roDataStats struct {
	Candidates  int
	Tables      int
	SelfRel     int
	Rebased     int
	Entries     int
	Retargeted  int
	Unresolved  int
	Unplaced    int
	Segmented   int
	Corrections int
}

// jumpTables lists the switch tables in an exact .rodata image. Scanning left
// to right and consuming each run keeps the base honest: an entry list read
// from the wrong starting word would still land in .text, because .text is
// large, so the first position that begins a long run is taken as the base.
func jumpTables(sec []byte, secAddr, textLo, textHi uint64, minRun int) [][2]int {
	n := len(sec) / 4
	var out [][2]int
	for i := 0; i < n; {
		if j := runEnd(sec, secAddr, textLo, textHi, i, n, i); j-i >= minRun {
			out = append(out, [2]int{i, j})
			i = j
			continue
		}
		i++
	}
	return out
}

// runEnd reports how far the run starting at word i reaches, reading each
// entry relative to word base and stopping at limit.
func runEnd(sec []byte, secAddr, textLo, textHi uint64, i, limit, base int) int {
	baseAddr := secAddr + uint64(base*4)
	j := i
	for j < limit {
		v := int32(binary.LittleEndian.Uint32(sec[j*4:]))
		if t := uint64(int64(baseAddr) + int64(v)); t < textLo || t >= textHi {
			break
		}
		j++
	}
	return j
}

// shortCandidates finds the two- and three-word runs the long scan left over.
// It searches only the space between accepted tables, so adding it cannot
// re-segment a table the long scan already found -- a run that began one word
// early would take the whole table with it and read every entry against the
// wrong base.
func shortCandidates(sec []byte, secAddr, textLo, textHi uint64, long [][2]int) [][2]int {
	n := len(sec) / 4
	var out [][2]int
	at := 0
	for k := 0; k <= len(long); k++ {
		end := n
		if k < len(long) {
			end = long[k][0]
		}
		for i := at; i < end; {
			if j := runEnd(sec, secAddr, textLo, textHi, i, end, i); j-i >= jumpTableShortRun {
				out = append(out, [2]int{i, j})
				i = j
				continue
			}
			i++
		}
		if k < len(long) {
			at = long[k][1]
		}
	}
	return out
}

// roDataCandidate is one run the decoder will consider, with the convention
// its entries are read under. Both conventions occur -- a compiler switch
// table is relative to the table base, a generated table of relative pointers
// is usually relative to each entry's own address -- and for a short table
// they are indistinguishable by signature, because the two interpretations
// differ only by the entry's offset within the table and both land in .text.
// So the scan finds spans and the encoder chooses the convention.
type roDataCandidate struct {
	Span    [2]int
	SelfRel bool
}

// roDataBaseShifts is how many origins are offered per span. The scan starts a
// run at the first word that begins one, which can be earlier than the table
// itself: a junk word before a real table still lands in .text under its own
// base, and the table's entries then land in .text under that base too,
// because they miss their true targets by only the shift. Reading a table one
// word early gives every entry the wrong origin, which is why 7,282 of the
// 10,837 runs found by signature alone made the prediction worse.
const roDataBaseShifts = 4

// roDataSpans enumerates every run both sides can find from the old section
// alone, in a fixed order so a fixed number of bits per entry addresses them.
func roDataSpans(sec []byte, secAddr, textLo, textHi uint64) [][2]int {
	long := jumpTables(sec, secAddr, textLo, textHi, jumpTableMinRun)
	spans := append(long, shortCandidates(sec, secAddr, textLo, textHi, long)...)
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	return spans
}

// spanVariants lists what the encoder may choose from for one span: each
// origin from the run's start up to three words in, under each convention.
func spanVariants(span [2]int) []roDataCandidate {
	out := make([]roDataCandidate, 0, 2*roDataBaseShifts)
	for shift := 0; shift < roDataBaseShifts; shift++ {
		if span[0]+shift >= span[1] {
			break
		}
		for _, self := range []bool{false, true} {
			out = append(out, roDataCandidate{Span: [2]int{span[0] + shift, span[1]}, SelfRel: self})
		}
	}
	return out
}

func bitSet(b []byte, i int) bool { return i/8 < len(b) && b[i/8]&(1<<(i%8)) != 0 }

// spanBit is where the encoder's decision for variant v of span i lives. The
// stride is fixed rather than packed so that a span near the section end,
// which offers fewer origins, does not shift every later span's bits.
func spanBit(i, v int) int { return i*2*roDataBaseShifts + v }

// retargetTable computes what one candidate table should hold in the new
// image. It drives from the old section, which is exact, resolves each entry's
// target through the code lookup, and writes it back relative to wherever pl
// says the entry landed.
func retargetTable(old []byte, p roDataPlan, c roDataCandidate, pl placement, lookup func(uint64) x86.Target, st *roDataStats, visit func(off uint64, v uint32)) {
	sec := old[p.OldOff : p.OldOff+p.OldSize]
	tb := c.Span
	baseOff, ok := pl(tb[0])
	if !ok {
		st.Unplaced += tb[1] - tb[0]
		return
	}
	oldBase, newBase := p.OldAddr+uint64(tb[0])*4, p.NewAddr+(baseOff-p.NewOff)
	for k := tb[0]; k < tb[1]; k++ {
		st.Entries++
		newOff, ok := pl(k)
		if !ok {
			st.Unplaced++
			continue
		}
		// The entry is relative to the table base, or to itself.
		oldRef, newRef := oldBase, newBase
		if c.SelfRel {
			oldRef, newRef = p.OldAddr+uint64(k)*4, p.NewAddr+(newOff-p.NewOff)
		}
		v := int32(binary.LittleEndian.Uint32(sec[k*4:]))
		t := lookup(uint64(int64(oldRef) + int64(v)))
		if !t.Known {
			st.Unresolved++
			continue
		}
		st.Retargeted++
		visit(newOff, uint32(int32(int64(t.Addr)-int64(newRef))))
	}
}

// applyRoData retargets the tables the encoder vouched for.
func applyRoData(out, old []byte, p roDataPlan, mapper sourceEquivalenceMapper, lookup func(uint64) x86.Target, unitAt func(uint64) (uint64, bool)) roDataStats {
	var st roDataStats
	sec := old[p.OldOff : p.OldOff+p.OldSize]
	write := func(off uint64, v uint32) { binary.LittleEndian.PutUint32(out[off:], v) }
	proj := projectedPlacement(p, mapper)
	corr := &planReader{b: p.Cursor}
	spans := roDataSpans(sec, p.OldAddr, p.TextLo, p.TextHi)
	segIdx, si := readSegList(p.Seg, len(spans)), 0
	for i, span := range spans {
		st.Candidates++
		segmented := false
		for si < len(segIdx) && segIdx[si] <= i {
			segmented = segIdx[si] == i
			si++
		}
		for v, c := range spanVariants(span) {
			if !bitSet(p.Keep, spanBit(i, v)) {
				continue
			}
			st.Tables++
			if c.SelfRel {
				st.SelfRel++
			}
			if c.Span[0] > span[0] {
				st.Rebased++
			}
			pl := proj
			if segmented {
				// The corrections are one stream in span order, so they are
				// read whether or not the span turns out to be placeable.
				bounds := segmentSpan(sec, p, c, unitAt)
				ds := make([]int64, len(bounds)-1)
				for t := range ds {
					ds[t] = corr.s()
				}
				st.Corrections += len(ds)
				if start, ok := proj(c.Span[0]); ok {
					st.Segmented++
					pl = cursorPlacement(p, bounds, placeTables(p, bounds, start, ds))
				}
			}
			retargetTable(old, p, c, pl, lookup, &st, write)
			break
		}
	}
	return st
}

// A span the byte matcher cannot match is placed by cursor instead: its
// entries are segmented into the per-function tables they were assembled from,
// and each table is predicted at the end of the one before it. Two of libxul's
// spans are 43,128 and 70,165 entries long and lie in a 453,499-byte hole --
// every entry is a target minus its own address, so the whole region churns
// when .text moves and nothing in it matches. Projecting a flat shift across
// that hole put 61 of 98,455 entries in the right slot. The cursor is right
// wherever the table sequence is unchanged, which is most of it, and one
// signed varint per table pays for the rest.
const (
	// roDataSegMin is the shortest span the encoder will try to segment:
	// below it a span is one table and the projection already places it.
	roDataSegMin = 8
	// roDataResyncMin is where the encoder starts paying for the index that
	// finds a table which moved further than the local scan reaches. Below
	// it the local scan is what a span gets, and measurement says that is
	// the whole of it: at 32 the index costs more plan than it saves.
	roDataResyncMin = 256
	// roDataLocalScan is how many words either side of the cursor the encoder
	// tries for every table.
	roDataLocalScan = 4
)

// roDataUnplaced marks a table the cursor put outside the new section.
const roDataUnplaced = ^uint64(0)

// placement says where entry k of a span lands in the new image, or that it
// cannot be placed. Staying inside the new section is its responsibility, so
// no correction the plan carries can make a write leave it.
type placement func(k int) (uint64, bool)

func (p roDataPlan) holds(off uint64) bool {
	return off >= p.NewOff && off+4 <= p.NewOff+p.NewSize
}

// projectedPlacement puts each entry wherever the byte matcher put the bytes
// it was in. That is right whenever a matched run reaches the entry, and
// extrapolates a flat shift when none does.
func projectedPlacement(p roDataPlan, mapper sourceEquivalenceMapper) placement {
	return func(k int) (uint64, bool) {
		off, ok := mapper.project(p.OldOff + uint64(k)*4)
		if !ok || !p.holds(off) {
			return 0, false
		}
		return off, true
	}
}

// cursorPlacement puts each entry at its table's chosen position.
func cursorPlacement(p roDataPlan, bounds []int, pos []uint64) placement {
	return func(k int) (uint64, bool) {
		t, exact := slices.BinarySearch(bounds, k)
		if !exact {
			t--
		}
		if t < 0 || t >= len(pos) || pos[t] == roDataUnplaced {
			return 0, false
		}
		off := pos[t] + uint64(k-bounds[t])*4
		if !p.holds(off) {
			return 0, false
		}
		return off, true
	}
}

// placeTables walks the tables left to right: each one is predicted at the
// running cursor plus its correction, and the cursor moves past it carrying
// that correction forward. A table inserted or deleted in the new image
// therefore costs one correction, not one for every table after it. Both the
// correction and the cursor are clamped to the section, so a plan that asks
// for a wild position only loses the table.
func placeTables(p roDataPlan, bounds []int, start uint64, corr []int64) []uint64 {
	lo, hi := int64(p.NewOff), int64(p.NewOff+p.NewSize)
	size := int64(p.NewSize)
	pos := make([]uint64, len(bounds)-1)
	cursor := min(max(int64(start), lo), hi)
	for t := range pos {
		var d int64
		if t < len(corr) {
			d = min(max(corr[t], -size), size)
		}
		q := cursor + d
		n := int64(bounds[t+1]-bounds[t]) * 4
		pos[t] = roDataUnplaced
		if q >= lo && q+n <= hi {
			pos[t] = uint64(q)
		}
		cursor = min(max(q+n, lo), hi)
	}
	return pos
}

// segmentSpan splits a candidate's entries into the tables they were
// assembled from: consecutive entries whose targets lie in the same old
// function. It reads the old image alone, so both sides get the same
// boundaries, and an entry whose target names no function stays with the
// table it follows rather than starting one.
func segmentSpan(sec []byte, p roDataPlan, c roDataCandidate, unitAt func(uint64) (uint64, bool)) []int {
	bounds := []int{c.Span[0]}
	var owner uint64
	var have bool
	for k := c.Span[0]; k < c.Span[1]; k++ {
		u, ok := unitAt(roDataTarget(sec, p, c, k))
		if !ok || (have && u == owner) {
			continue
		}
		if have && k > bounds[len(bounds)-1] {
			bounds = append(bounds, k)
		}
		owner, have = u, true
	}
	return append(bounds, c.Span[1])
}

// roDataTarget is the old address entry k points at, under c's convention.
func roDataTarget(sec []byte, p roDataPlan, c roDataCandidate, k int) uint64 {
	ref := p.OldAddr + uint64(c.Span[0])*4
	if c.SelfRel {
		ref = p.OldAddr + uint64(k)*4
	}
	return uint64(int64(ref) + int64(int32(binary.LittleEndian.Uint32(sec[k*4:]))))
}

// selectRoDataTables is the encoder half: it scores every variant of every
// span against the target and keeps the best one, or none. A run that lands in
// .text by chance, and a real table read one word early, are both rejected
// here rather than paid for in the correction. A long span is then offered the
// cursor placement as well, and keeps it only if it removes more wrong bytes
// than the corrections cost.
func selectRoDataTables(pred, old, target []byte, p roDataPlan, mapper sourceEquivalenceMapper, lookup func(uint64) x86.Target, unitAt func(uint64) (uint64, bool)) (roDataPlan, roDataStats) {
	sec := old[p.OldOff : p.OldOff+p.OldSize]
	spans := roDataSpans(sec, p.OldAddr, p.TextLo, p.TextHi)
	p.Keep = make([]byte, (2*roDataBaseShifts*len(spans)+7)/8)
	p.Seg, p.Cursor = nil, nil
	prevSeg := -1
	proj := projectedPlacement(p, mapper)
	var st roDataStats
	st.Candidates = len(spans)
	for i, span := range spans {
		bestGain, bestV := 0, -1
		var best roDataCandidate
		for v, c := range spanVariants(span) {
			if g := placementGain(pred, old, target, p, c, proj, lookup); g > bestGain {
				bestGain, bestV, best = g, v, c
			}
		}
		var corr []int64
		if span[1]-span[0] >= roDataSegMin {
			// The cursor arm runs on the variant the projection liked best,
			// or, where it liked none, on the two conventions at the run's
			// own origin: a span whose entries all land in the wrong slot is
			// exactly the one whose projected gain is nothing.
			tries := []int{bestV}
			if bestV < 0 {
				tries = []int{0, 1}
			}
			var idx *resyncIndex
			for _, v := range tries {
				vars := spanVariants(span)
				if v < 0 || v >= len(vars) {
					continue
				}
				c := vars[v]
				start, ok := proj(c.Span[0])
				if !ok {
					continue
				}
				if idx == nil && c.SelfRel && span[1]-span[0] >= roDataResyncMin {
					idx = buildResyncIndex(target, p, span, start)
				}
				ds, g := chooseCursors(pred, old, target, p, c, segmentSpan(sec, p, c, unitAt), start, lookup, proj, idx)
				if g > bestGain {
					bestGain, bestV, best, corr = g, v, c, ds
				}
			}
		}
		if bestV < 0 {
			continue
		}
		bit := spanBit(i, bestV)
		p.Keep[bit/8] |= 1 << (bit % 8)
		st.Tables++
		if best.SelfRel {
			st.SelfRel++
		}
		if best.Span[0] > span[0] {
			st.Rebased++
		}
		if corr != nil {
			p.Seg = appendU(p.Seg, uint64(i-prevSeg-1))
			prevSeg = i
			st.Segmented++
			st.Corrections += len(corr)
			for _, d := range corr {
				p.Cursor = appendS(p.Cursor, d)
			}
		}
	}
	return p, st
}

// readSegList reads roDataPlan.Seg, refusing anything that does not name a
// span in ascending order: a plan that lied about which spans are segmented
// would take corrections meant for one span and spend them on another.
func readSegList(b []byte, nspans int) []int {
	r := &planReader{b: b}
	at := -1
	var out []int
	for !r.done() && r.err == nil {
		d := r.u()
		if r.err != nil || d > uint64(nspans) || at+int(d)+1 >= nspans {
			break
		}
		at += int(d) + 1
		out = append(out, at)
	}
	return out
}

// placementGain is how many wrong bytes a placement of one candidate removes.
// Only the bytes it writes can change, so summing over those is the change in
// the image's error even when two placements write to different offsets.
func placementGain(pred, old, target []byte, p roDataPlan, c roDataCandidate, pl placement, lookup func(uint64) x86.Target) int {
	gain := 0
	var ignored roDataStats
	retargetTable(old, p, c, pl, lookup, &ignored, func(off uint64, val uint32) {
		gain += wordGain(pred, target, off, val)
	})
	return gain
}

// wordGain is the same for one four-byte write.
func wordGain(pred, target []byte, off uint64, val uint32) int {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], val)
	gain := 0
	for k := range 4 {
		if pred[off+uint64(k)] != target[off+uint64(k)] {
			gain++
		}
		if buf[k] != target[off+uint64(k)] {
			gain--
		}
	}
	return gain
}

// chooseCursors picks each table's correction, left to right, against the
// target: the cursor itself, the byte matcher's opinion, a few words either
// side, and -- for a long self-relative span -- wherever in the new section an
// entry resolving to the table's first target actually sits. The gain it
// returns is net of what the corrections cost the plan.
func chooseCursors(pred, old, target []byte, p roDataPlan, c roDataCandidate, bounds []int, start uint64, lookup func(uint64) x86.Target, hint placement, idx *resyncIndex) ([]int64, int) {
	sec := old[p.OldOff : p.OldOff+p.OldSize]
	lo, hi := int64(p.NewOff), int64(p.NewOff+p.NewSize)
	size := int64(p.NewSize)
	corr := make([]int64, len(bounds)-1)
	cursor := min(max(int64(start), lo), hi)
	base := start // the new home of the span's base word, the base-relative reference
	gain := 0
	var addr []int64
	for t := range corr {
		k0, k1 := bounds[t], bounds[t+1]
		addr = addr[:0]
		for k := k0; k < k1; k++ {
			if tg := lookup(roDataTarget(sec, p, c, k)); tg.Known {
				addr = append(addr, int64(tg.Addr))
			} else {
				addr = append(addr, -1)
			}
		}
		n := int64(k1-k0) * 4
		score := func(q int64) (int, bool) {
			if q < lo || q+n > hi {
				return 0, false
			}
			ref := base
			if t == 0 {
				ref = uint64(q)
			}
			g := 0
			for j, a := range addr {
				if a < 0 {
					continue
				}
				off := uint64(q) + uint64(j)*4
				newRef := p.NewAddr + (ref - p.NewOff)
				if c.SelfRel {
					newRef = p.NewAddr + (off - p.NewOff)
				}
				g += wordGain(pred, target, off, uint32(int32(a-int64(newRef))))
			}
			return g, true
		}
		bestD, bestScore, haveBest := int64(0), 0, false
		try := func(q int64) {
			d := min(max(q-cursor, -size), size)
			g, ok := score(cursor + d)
			if !ok {
				return
			}
			s := g - varintLen(d)
			if !haveBest || s > bestScore || (s == bestScore && abs64(d) < abs64(bestD)) {
				bestD, bestScore, haveBest = d, s, true
			}
		}
		try(cursor)
		if h, ok := hint(k0); ok {
			try(int64(h))
		}
		for r := int64(1); r <= roDataLocalScan; r++ {
			try(cursor + r*4)
			try(cursor - r*4)
		}
		if idx != nil {
			for j, a := range addr {
				if a < 0 {
					continue
				}
				for _, q := range idx.near(uint64(a), cursor) {
					try(q - int64(j)*4)
				}
				break
			}
		}
		if haveBest {
			corr[t], gain = bestD, gain+bestScore
		} else {
			gain -= varintLen(0)
		}
		q := cursor + corr[t]
		if t == 0 && q >= lo && q+n <= hi {
			base = uint64(q)
		}
		cursor = min(max(q+n, lo), hi)
	}
	return corr, gain
}

// resyncIndex maps "the address an entry here resolves to" back to where that
// entry sits, over the new bytes one span can land in. It is how the encoder
// finds a table that moved further than the local scan reaches. Self-relative
// only: under the other convention an entry's resolved address depends on
// where its own table's base landed, which is the unknown being solved for.
type resyncIndex struct {
	ents []resyncEnt
}

type resyncEnt struct {
	key uint64
	off uint64
}

func buildResyncIndex(target []byte, p roDataPlan, span [2]int, start uint64) *resyncIndex {
	bytes := uint64(span[1]-span[0]) * 4
	slack := max(bytes/2, 4<<10)
	lo, hi := p.NewOff, p.NewOff+p.NewSize
	if start > lo+slack {
		lo = start - slack
	}
	if start+bytes+slack < hi {
		hi = start + bytes + slack
	}
	x := &resyncIndex{ents: make([]resyncEnt, 0, (hi-lo)/4)}
	for off := lo; off+4 <= hi; off += 4 {
		v := int32(binary.LittleEndian.Uint32(target[off:]))
		x.ents = append(x.ents, resyncEnt{key: uint64(int64(p.NewAddr+off-p.NewOff) + int64(v)), off: off})
	}
	slices.SortFunc(x.ents, func(a, b resyncEnt) int { return cmpU(a.key, b.key) })
	return x
}

// resyncFanout bounds how many positions one key offers, nearest first: a
// jump-table target that half the image branches to must not turn the search
// into a scan.
const resyncFanout = 4

func (x *resyncIndex) near(key uint64, want int64) []int64 {
	i, _ := slices.BinarySearchFunc(x.ents, key, func(e resyncEnt, key uint64) int { return cmpU(e.key, key) })
	var out []int64
	for ; i < len(x.ents) && x.ents[i].key == key; i++ {
		out = append(out, int64(x.ents[i].off))
	}
	if len(out) > resyncFanout {
		slices.SortFunc(out, func(a, b int64) int { return cmpU(uint64(abs64(a-want)), uint64(abs64(b-want))) })
		out = out[:resyncFanout]
	}
	return out
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// varintLen is what one correction costs the plan.
func varintLen(v int64) int {
	u := uint64(v) << 1
	if v < 0 {
		u = ^u
	}
	n := 1
	for u >= 0x80 {
		u >>= 7
		n++
	}
	return n
}
