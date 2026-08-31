package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"slices"
)

// The sidecar probe prices one architectural alternative to the function map.
//
// The map ships two things: the old->new correspondence of code units, and the
// new layout (where each new function starts and how long it is). Both are
// derived, encoder-side, from two symbol tables the decoder never sees.
//
// Suppose it did. A decoder that already applied the previous patch could have
// been handed the old binary's symbol table with it, and could carry it
// forward for free. Then correspondence is a name join, which costs nothing to
// transmit, and the patch only has to ship what the join cannot answer: the
// *delta* between the two symbol tables (dropped, resized and inserted
// functions, plus order exceptions) and whatever the layout replay gets wrong.
//
// This probe builds that stream for real and compresses it, against the map
// component priced the way the current accounting prices it.

// namedUnit is a codeUnit that kept its symbol names as text. loadCodeUnits
// deliberately throws the text away -- a fingerprint is all the matcher needs
// -- but a sidecar has to transmit the name of every inserted function, so the
// probe needs the bytes to weigh.
type namedUnit struct {
	Off   uint64
	Size  uint64
	Names []string
}

// loadNamedUnits repeats loadCodeUnits' grouping exactly -- group STT_FUNC
// symbols by address, take the widest size, clamp to the next start -- so the
// units it returns line up one for one with the ones the shipped map is built
// from, and an offset can be used as a join key between the two.
func loadNamedUnits(path string, text section) ([]namedUnit, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	syms, err := f.Symbols()
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("%s: read symbols: %w", path, err)
	}
	type group struct {
		maxSize uint64
		names   []string
	}
	groups := make(map[uint64]*group)
	textEnd := text.Addr + text.Size
	for _, sym := range syms {
		if elf.ST_TYPE(sym.Info) != elf.STT_FUNC || sym.Size == 0 ||
			sym.Value < text.Addr || sym.Value >= textEnd {
			continue
		}
		g := groups[sym.Value]
		if g == nil {
			g = &group{}
			groups[sym.Value] = g
		}
		g.maxSize = max(g.maxSize, sym.Size)
		if sym.Name != "" {
			g.names = append(g.names, sym.Name)
		}
	}
	syms = nil
	runtime.GC()
	starts := make([]uint64, 0, len(groups))
	for addr := range groups {
		starts = append(starts, addr)
	}
	slices.Sort(starts)
	units := make([]namedUnit, 0, len(starts))
	for i, addr := range starts {
		limit := textEnd - addr
		if i+1 < len(starts) {
			limit = starts[i+1] - addr
		}
		sz := min(groups[addr].maxSize, limit)
		if sz == 0 {
			continue
		}
		names := groups[addr].names
		slices.Sort(names)
		names = slices.Compact(names)
		units = append(units, namedUnit{Off: addr - text.Addr, Size: sz, Names: names})
	}
	return units, nil
}

// sidecarCalibration prices the map component the way the current accounting
// prices it -- planComponents' standalone xz of the structure stream -- and
// again marginally, because §9.3's trap says the two disagree by a lot. It
// also splits the structure stream into the part a sidecar could replace
// (correspondence + destination layout) and the part it could not (the copy
// bitmap, the reference points, the section ranges).
func sidecarCalibration(planBytes []byte, structure predictionPlan, oldText []byte) (standalone, marginal, mapOnly, replaceable int) {
	cp, err := unmarshalCombinedPlan(planBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  sidecar: cannot parse combined plan: %v\n", err)
		return
	}
	without := cp
	without.Structure = nil
	full := xzSizeContiguous(planBytes)
	bare := xzSizeContiguous(without.marshal())
	standalone = xzSizeContiguous(cp.Structure)
	marginal = full - bare

	// Splitting the structure stream by re-marshalling a plan with no maps
	// does not work: the point table is indexed against reference targets
	// derived *from* the map, so dropping the map destroys that basis and the
	// point columns balloon. The map columns have to be priced marginally,
	// with everything else held exactly as it is -- which is also what the
	// sidecar would actually save, since the decoder still reconstructs the
	// map and so still derives the same target list.
	// The blanked marshal has to reproduce the shipped stream byte for byte
	// when nothing is blanked, or the two sides of the subtraction are not the
	// same encoding.
	if b, err := structure.marshalBlanked(oldText, false, false); err != nil || !bytes.Equal(b, cp.Structure) {
		fmt.Fprintf(os.Stderr, "  sidecar: blanked marshal does not reproduce the structure stream (%d vs %d, err %v); calibration unsafe\n",
			len(b), len(cp.Structure), err)
		return
	}
	if b, err := structure.marshalBlanked(oldText, true, false); err == nil {
		mapOnly = standalone - xzSizeContiguous(b)
	}
	copyBits := 0
	if b, err := structure.marshalBlanked(oldText, false, true); err == nil {
		copyBits = standalone - xzSizeContiguous(b)
	}
	fmt.Fprintf(os.Stderr, "  calibration: whole plan xz %d; structure stream standalone %d, marginal within the plan %d\n",
		full, standalone, marginal)
	fmt.Fprintf(os.Stderr, "  calibration: %d mappings, %d points, %d ranges; map columns cost %d marginally inside the structure stream, of which the copy bitmap is %d\n",
		len(structure.Maps), len(structure.Points), len(structure.Ranges), mapOnly, copyBits)
	replaceable = mapOnly - copyBits
	fmt.Fprintf(os.Stderr, "  calibration: the sidecar can replace %d of that (map columns less the copy bitmap)\n", replaceable)
	return
}

// marshalBlanked serializes the structural plan exactly as marshal does, but
// with some of the map's columns emptied. Everything downstream -- the point
// index basis, the range map -- is computed from the unmodified map, so the
// difference between this and the real stream is the marginal cost of the
// blanked columns and nothing else.
func (p predictionPlan) marshalBlanked(oldText []byte, blankMap, blankCopy bool) ([]byte, error) {
	maps := slices.Clone(p.Maps)
	slices.SortFunc(maps, func(a, b mapping) int {
		if a.Dst != b.Dst {
			return cmpU(a.Dst, b.Dst)
		}
		return cmpU(a.Src, b.Src)
	})
	srcs := make([]uint64, len(maps))
	for i, m := range maps {
		srcs[i] = m.Src
	}
	extents := sourceExtents(srcs, oldText)
	b := append([]byte(nil), planMagic[:]...)
	b = appendU(b, p.OldAddr)
	b = appendU(b, p.NewAddr)
	b = appendU(b, p.TargetLen)
	b = append(b, byte(p.Mode))
	b = appendU(b, uint64(len(maps)))
	detected := detectBoundaries(oldText)
	var srcIndexDeltas, srcOffsets, extentResiduals, sizeDeltas, startResiduals []byte
	copyBits := make([]byte, (len(maps)+7)/8)
	var prevDstEnd uint64
	var prevIdx int
	for i, m := range maps {
		idx := boundaryIndex(detected, m.Src)
		if !blankMap {
			srcIndexDeltas = appendS(srcIndexDeltas, int64(idx-prevIdx))
			srcOffsets = appendU(srcOffsets, m.Src-detected[idx])
			extentResiduals = appendS(extentResiduals, int64(extents[m.Src])-int64(m.SrcSize))
			sizeDeltas = appendS(sizeDeltas, int64(m.SrcSize)-int64(m.DstSize))
			startResiduals = appendS(startResiduals, int64(m.Dst)-int64(alignedGuess(prevDstEnd)))
		}
		if m.Copy && !blankCopy {
			copyBits[i/8] |= 1 << (i % 8)
		}
		prevDstEnd, prevIdx = m.Dst+m.DstSize, idx
	}
	if blankCopy {
		copyBits = nil
	}
	b = appendStream(b, srcIndexDeltas)
	b = appendStream(b, srcOffsets)
	b = appendStream(b, extentResiduals)
	b = appendStream(b, sizeDeltas)
	b = appendStream(b, startResiduals)
	b = appendStream(b, copyBits)
	points := slices.Clone(p.Points)
	slices.SortFunc(points, func(a, b addressPoint) int { return cmpU(a.Old, b.Old) })
	b = appendU(b, uint64(len(points)))
	targets := cachedReferenceTargets(oldText, maps, p.OldAddr)
	var pointIndexDeltas, pointOffsets, pointShiftDeltas []byte
	var prevShift int64
	prevPointIdx := 0
	for _, point := range points {
		idx := targetIndex(targets, point.Old)
		pointIndexDeltas = appendS(pointIndexDeltas, int64(idx-prevPointIdx))
		pointOffsets = appendU(pointOffsets, point.Old-targets[idx])
		shift := int64(point.New) - int64(point.Old)
		pointShiftDeltas = appendS(pointShiftDeltas, shift-prevShift)
		prevPointIdx, prevShift = idx, shift
	}
	b = appendStream(b, pointIndexDeltas)
	b = appendStream(b, pointOffsets)
	b = appendStream(b, pointShiftDeltas)
	ranges := slices.Clone(p.Ranges)
	slices.SortFunc(ranges, func(a, b addressRange) int { return cmpU(a.Old, b.Old) })
	b = appendU(b, uint64(len(ranges)))
	var prevOld, prevNew uint64
	for _, ar := range ranges {
		b = appendU(b, ar.Old-prevOld)
		b = appendS(b, int64(ar.New)-int64(prevNew))
		b = appendU(b, ar.Size)
		prevOld, prevNew = ar.Old, ar.New
	}
	return b, nil
}

// sidecarJoin is the correspondence a name join recovers. Duplicate names are
// disambiguated by relative order: the k-th new unit carrying a name is joined
// to the k-th old unit carrying it, which is the only rule available to a
// decoder that holds two symbol tables and nothing else.
func sidecarJoin(oldUnits, newUnits []namedUnit) (joined []int, dupNames, dupUnits int) {
	oldSeq := make(map[string][]int, len(oldUnits))
	for i, u := range oldUnits {
		for _, n := range u.Names {
			oldSeq[n] = append(oldSeq[n], i)
		}
	}
	newSeq := make(map[string][]int, len(newUnits))
	for i, u := range newUnits {
		for _, n := range u.Names {
			newSeq[n] = append(newSeq[n], i)
		}
	}
	for n, s := range newSeq {
		if len(s) > 1 || len(oldSeq[n]) > 1 {
			dupNames++
			dupUnits += len(s)
		}
	}
	joined = make([]int, len(newUnits))
	for i := range joined {
		joined[i] = -1
	}
	// A unit may carry several names (aliases, or identical-code folding
	// pointing several symbols at one body). Each name proposes an old unit;
	// the majority proposal wins, ties going to the lowest old index so the
	// rule is deterministic on both sides.
	votes := map[int]int{}
	for ni, u := range newUnits {
		clear(votes)
		for _, n := range u.Names {
			pos := slices.Index(newSeq[n], ni)
			if os := oldSeq[n]; pos >= 0 && pos < len(os) {
				votes[os[pos]]++
			}
		}
		best, bestVotes := -1, 0
		for oi, v := range votes {
			if v > bestVotes || (v == bestVotes && oi < best) {
				best, bestVotes = oi, v
			}
		}
		joined[ni] = best
	}
	// One old unit may only be claimed once. Where two new units join the same
	// old unit -- possible when name sets overlap unevenly -- the earlier new
	// unit keeps it and the later becomes an insert.
	taken := make(map[int]bool, len(newUnits))
	for ni, oi := range joined {
		if oi < 0 {
			continue
		}
		if taken[oi] {
			joined[ni] = -1
			continue
		}
		taken[oi] = true
	}
	return joined, dupNames, dupUnits
}

func appendGap(b []byte, cur, prev int) []byte {
	return binary.AppendUvarint(b, uint64(cur-prev))
}

// probeSidecar is the whole measurement. It prints the calibration, the
// fidelity of the name join against the shipped map, and the cost of the
// symbol-table delta stream that would replace the map.
func probeSidecar(planBytes []byte, structure predictionPlan, oldImage, newImage *image) {
	if oldDebugPath == "" || newDebugPath == "" {
		fmt.Fprintln(os.Stderr, "probe sidecar: needs -old-debug and -new-debug")
		return
	}
	fmt.Fprintln(os.Stderr, "probe sidecar: symbol-table sidecar against the function map")
	oldText := oldImage.textBytes()
	standalone, marginal, mapOnly, replaceable := sidecarCalibration(planBytes, structure, oldText)
	if standalone < 150_000 || standalone > 400_000 {
		fmt.Fprintf(os.Stderr, "  sidecar: structure stream is %d, outside the expected ~250 KB region; stopping\n", standalone)
		return
	}

	t := startStage("sidecar symbols")
	oldUnits, err := loadNamedUnits(oldDebugPath, oldImage.Text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe sidecar FAILED: %v\n", err)
		return
	}
	newUnits, err := loadNamedUnits(newDebugPath, newImage.Text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe sidecar FAILED: %v\n", err)
		return
	}
	t.done("old %d units, new %d units", len(oldUnits), len(newUnits))

	oldIdx := make(map[uint64]int, len(oldUnits))
	for i, u := range oldUnits {
		oldIdx[u.Off] = i
	}
	newIdx := make(map[uint64]int, len(newUnits))
	for i, u := range newUnits {
		newIdx[u.Off] = i
	}

	joined, dupNames, dupUnits := sidecarJoin(oldUnits, newUnits)

	// --- fidelity against the shipped map -------------------------------
	structSrc := make([]int, len(newUnits))
	for i := range structSrc {
		structSrc[i] = -1
	}
	var mapUnmatchedDst, mapUnmatchedSrc int
	for _, m := range structure.Maps {
		ni, ok := newIdx[m.Dst]
		if !ok {
			mapUnmatchedDst++
			continue
		}
		oi, ok := oldIdx[m.Src]
		if !ok {
			mapUnmatchedSrc++
			continue
		}
		structSrc[ni] = oi
	}
	var agree, disagree, structOnly, joinOnly, neither int
	var agreeBytes, disagreeBytes, structOnlyBytes, joinOnlyBytes uint64
	var mappedUnits int
	var mappedBytes uint64
	for ni := range newUnits {
		s, j := structSrc[ni], joined[ni]
		sz := newUnits[ni].Size
		if s >= 0 {
			mappedUnits++
			mappedBytes += sz
		}
		switch {
		case s >= 0 && j == s:
			agree++
			agreeBytes += sz
		case s >= 0 && j >= 0:
			disagree++
			disagreeBytes += sz
		case s >= 0:
			structOnly++
			structOnlyBytes += sz
		case j >= 0:
			joinOnly++
			joinOnlyBytes += sz
		default:
			neither++
		}
	}
	pct := func(n int, d int) float64 { return 100 * float64(n) / float64(max(1, d)) }
	pctB := func(n, d uint64) float64 { return 100 * float64(n) / float64(max(1, d)) }
	fmt.Fprintf(os.Stderr, "  name join: %d duplicated names covering %d new units (%.2f%% of %d)\n",
		dupNames, dupUnits, pct(dupUnits, len(newUnits)), len(newUnits))
	fmt.Fprintf(os.Stderr, "  map coverage: %d of %d mappings landed on a new unit start (%d missed dst, %d missed src)\n",
		mappedUnits, len(structure.Maps), mapUnmatchedDst, mapUnmatchedSrc)
	fmt.Fprintf(os.Stderr, "  fidelity: agree %d units (%.3f%% of mapped) %d B (%.3f%%); disagree %d (%d B); map-only %d (%d B); join-only %d (%d B); neither %d\n",
		agree, pct(agree, mappedUnits), agreeBytes, pctB(agreeBytes, mappedBytes),
		disagree, disagreeBytes, structOnly, structOnlyBytes, joinOnly, joinOnlyBytes, neither)

	// Every unit the map matched and the join did not, or matched differently,
	// is an exception the sidecar has to ship to reach the same prediction.
	var excIndex, excSrc []byte
	prevExc := 0
	exceptions := 0
	for ni := range newUnits {
		s, j := structSrc[ni], joined[ni]
		if s < 0 || j == s {
			continue
		}
		excIndex = appendGap(excIndex, ni, prevExc)
		// Both sides know what the join answered, so the exception says how
		// far the truth is from it rather than naming an old index outright.
		excSrc = binary.AppendVarint(excSrc, int64(s)-int64(j))
		prevExc = ni
		exceptions++
	}
	excZ := xzSizeContiguous(append(slices.Clone(excIndex), excSrc...))
	fmt.Fprintf(os.Stderr, "  correspondence exceptions: %d entries, raw %d B, xz %d\n",
		exceptions, len(excIndex)+len(excSrc), excZ)

	// --- the replacement stream -----------------------------------------
	// Drops first: the decoder needs to know which old units are gone before
	// it can say what "the next old unit" is, which is what makes the kept
	// sequence mostly implicit.
	kept := make([]bool, len(oldUnits))
	for _, oi := range joined {
		if oi >= 0 {
			kept[oi] = true
		}
	}
	var dropRuns []byte
	dropped, runs := 0, 0
	for i := 0; i < len(oldUnits); {
		j := i
		for j < len(oldUnits) && kept[j] {
			j++
		}
		dropRuns = binary.AppendUvarint(dropRuns, uint64(j-i))
		k := j
		for k < len(oldUnits) && !kept[k] {
			k++
		}
		dropRuns = binary.AppendUvarint(dropRuns, uint64(k-j))
		dropped += k - j
		runs++
		i = k
	}

	// The next kept old unit after the last one used. Where the new order
	// agrees with the old order -- which is the common case -- the stream says
	// nothing at all.
	nextKept := make([]int, len(oldUnits)+1)
	nxt := len(oldUnits)
	for i := len(oldUnits) - 1; i >= 0; i-- {
		nextKept[i] = nxt
		if kept[i] {
			nxt = i
		}
	}
	nextKept[len(oldUnits)] = len(oldUnits)
	firstKept := nxt

	var orderIndex, orderSrc, sizeIndex, sizeDelta []byte
	var insertGap, insertSize, insertNames []byte
	var reorders, resizes, inserts int
	var insertBytes uint64
	prevOrder, prevSize, prevInsert := 0, 0, 0
	cursor := firstKept
	for ni, u := range newUnits {
		oi := joined[ni]
		if oi < 0 {
			insertGap = appendGap(insertGap, ni, prevInsert)
			insertSize = binary.AppendUvarint(insertSize, u.Size)
			name := ""
			if len(u.Names) > 0 {
				name = u.Names[0]
			}
			insertNames = append(append(insertNames, name...), 0)
			prevInsert = ni
			inserts++
			insertBytes += u.Size
			continue
		}
		if oi != cursor {
			orderIndex = appendGap(orderIndex, ni, prevOrder)
			// Against the cursor, not absolute: a reordered function is
			// usually a short hop from where the walk had got to.
			orderSrc = binary.AppendVarint(orderSrc, int64(oi)-int64(cursor))
			prevOrder = ni
			reorders++
		}
		cursor = nextKept[oi]
		if d := int64(u.Size) - int64(oldUnits[oi].Size); d != 0 {
			sizeIndex = appendGap(sizeIndex, ni, prevSize)
			sizeDelta = binary.AppendVarint(sizeDelta, d)
			prevSize = ni
			resizes++
		}
	}

	// --- layout replay ---------------------------------------------------
	// Sizes are now known for every new unit, so a start is the previous end
	// rounded up. Which alignment, measured rather than assumed.
	bestAlign, bestHits := uint64(16), -1
	for _, a := range []uint64{1, 2, 4, 8, 16, 32, 64} {
		hits, prevEnd := 0, uint64(0)
		for _, u := range newUnits {
			if u.Off == prevEnd+(a-prevEnd%a)%a {
				hits++
			}
			prevEnd = u.Off + u.Size
		}
		fmt.Fprintf(os.Stderr, "  layout replay at %2d-byte alignment: %d of %d starts exact (%.3f%%)\n",
			a, hits, len(newUnits), pct(hits, len(newUnits)))
		if hits > bestHits {
			bestAlign, bestHits = a, hits
		}
	}
	var fixIndex, fixDelta []byte
	fixes, prevFix := 0, 0
	prevEnd := uint64(0)
	for ni, u := range newUnits {
		guess := prevEnd + (bestAlign-prevEnd%bestAlign)%bestAlign
		if u.Off != guess {
			fixIndex = appendGap(fixIndex, ni, prevFix)
			fixDelta = binary.AppendVarint(fixDelta, int64(u.Off)-int64(guess))
			prevFix = ni
			fixes++
		}
		prevEnd = u.Off + u.Size
	}

	type col struct {
		name string
		b    []byte
	}
	cols := []col{
		{"drop runs", dropRuns},
		{"order-exception index", orderIndex},
		{"order-exception source", orderSrc},
		{"size-delta index", sizeIndex},
		{"size-delta value", sizeDelta},
		{"insert position", insertGap},
		{"insert size", insertSize},
		{"insert names", insertNames},
		{"layout-fixup index", fixIndex},
		{"layout-fixup value", fixDelta},
		{"correspondence exception index", excIndex},
		{"correspondence exception source", excSrc},
	}
	var whole []byte
	bs := make([][]byte, len(cols))
	raw := 0
	for i, c := range cols {
		bs[i] = c.b
		raw += len(c.b)
		whole = append(whole, c.b...)
	}
	z := xzSizes(bs...)
	total := xzSizeContiguous(whole)
	fmt.Fprintf(os.Stderr, "  replacement stream: %d old units, %d dropped in %d runs; %d reorders, %d resizes, %d inserts (%d B of new code), %d layout fixups at align %d\n",
		len(oldUnits), dropped, runs, reorders, resizes, inserts, insertBytes, fixes, bestAlign)
	fmt.Fprintf(os.Stderr, "  %-34s %10s %10s\n", "column", "raw", "xz alone")
	sum := 0
	for i, c := range cols {
		sum += z[i]
		fmt.Fprintf(os.Stderr, "  %-34s %10d %10d\n", c.name, len(c.b), z[i])
	}
	fmt.Fprintf(os.Stderr, "  %-34s %10d %10d  (sum of columns %d)\n", "TOTAL, one contiguous stream", raw, total, sum)

	// Names dominate the insert cost, and a real sidecar could ship them in a
	// dictionary shared with the previous patch's names; report the stream
	// without them so the floor is visible too.
	var noNames []byte
	for i, c := range cols {
		if c.name != "insert names" {
			noNames = append(noNames, cols[i].b...)
		}
	}
	fmt.Fprintf(os.Stderr, "  same stream without the inserted names: xz %d\n", xzSizeContiguous(noNames))

	// --- verdict ---------------------------------------------------------
	share := func(n int) float64 { return 100 * float64(n) / 2_621_664 }
	fmt.Fprintf(os.Stderr, "  VERDICT: sidecar stream %d xz; map columns %d marginal, of which %d is replaceable (structure stream %d standalone / %d marginal)\n",
		total, mapOnly, replaceable, standalone, marginal)
	fmt.Fprintf(os.Stderr, "  VERDICT: %+d bytes (%+.3f%% of the 2,621,664 patch) against the replaceable map columns; %+d (%+.3f%%) against the whole map columns\n",
		total-replaceable, share(total-replaceable), total-mapOnly, share(total-mapOnly))
	fmt.Fprintf(os.Stderr, "  NOT replaced by the sidecar: the copy bitmap inside the map columns, the %d reference points and %d address ranges in the same stream, the equivalence stream, the choice bitmap, the field corrections and every byte correction.\n",
		len(structure.Points), len(structure.Ranges))
}
