package elfmod

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"

	"github.com/wjordan/presage/delta/x86"
)

// predictionPlan is the structural plan: the function map from the old
// .text to the new one, the exact reference points, and the per-section
// shift ranges. Maps are kept in destination order.
type predictionPlan struct {
	OldAddr   uint64
	NewAddr   uint64
	TargetLen uint64
	Maps      []mapping
	Points    []addressPoint
	Ranges    []addressRange
}

// boundaryAlign is the alignment a candidate function start must have: it
// discards a third of the spurious candidates and almost no real starts.
const boundaryAlign = 8

// detectBoundaries lists where old functions begin, as the old image shows
// it: the first byte, and any aligned non-padding byte that follows
// padding. It over-reads; a wrong entry is simply never named.
func detectBoundaries(oldText []byte) []uint64 {
	out := []uint64{0}
	for i := boundaryAlign; i < len(oldText); i += boundaryAlign {
		if oldText[i] != 0xcc && oldText[i-1] == 0xcc {
			out = append(out, uint64(i))
		}
	}
	return out
}

// boundaryIndex finds the greatest detected boundary at or below addr.
func boundaryIndex(detected []uint64, addr uint64) int {
	i, ok := slices.BinarySearch(detected, addr)
	if !ok {
		i--
	}
	return max(i, 0)
}

// alignedGuess is where the next function starts if only alignment
// padding separates it from the previous one.
func alignedGuess(prevEnd uint64) uint64 { return prevEnd + (16-prevEnd%16)%16 }

// sourceExtents guesses where each old function's body ends: the byte
// after the last non-padding byte before the next distinct source start.
func sourceExtents(srcs []uint64, oldText []byte) map[uint64]uint64 {
	distinct := slices.Clone(srcs)
	slices.Sort(distinct)
	distinct = slices.Compact(distinct)
	out := make(map[uint64]uint64, len(distinct))
	for i, src := range distinct {
		if src > uint64(len(oldText)) {
			out[src] = 0
			continue
		}
		next := uint64(len(oldText))
		if i+1 < len(distinct) && distinct[i+1] < next {
			next = distinct[i+1]
		}
		end := next
		for end > src && oldText[end-1] == 0xcc {
			end--
		}
		out[src] = end - src
	}
	return out
}

// referenceTargets enumerates every address the old image's mapped
// functions branch or call to, sorted and deduplicated, with a leading
// zero so an index always exists. Both sides build it from the old image
// and the map alone.
func referenceTargets(oldText []byte, maps []mapping, oldAddr uint64) []uint64 {
	// The plan retains mappings in destination order. Sort small indices into
	// that table rather than cloning and sorting its 40-byte entries.
	bySrc := make([]int, len(maps))
	for i := range bySrc {
		bySrc[i] = i
	}
	sortByKey(bySrc, func(i int) uint64 { return maps[i].Src }, func(a, b int) int {
		return cmpU(maps[a].Src, maps[b].Src)
	})
	bySrc = slices.CompactFunc(bySrc, func(a, b int) bool { return maps[a].Src == maps[b].Src })
	bodyAt := func(k int) x86.Body {
		mi := bySrc[k]
		m := maps[mi]
		if m.Src > uint64(len(oldText)) || m.SrcSize > uint64(len(oldText))-m.Src {
			return x86.Body{}
		}
		return x86.Body{Code: oldText[m.Src : m.Src+m.SrcSize], PC: oldAddr + m.Src}
	}
	parts := x86.CollectReferences(len(bySrc), runtime.GOMAXPROCS(0), bodyAt, func(_ int, body x86.Body, ref x86.Reference) (uint64, bool) {
		disp, ok := readDisplacement(body.Code, ref)
		if !ok {
			return 0, false
		}
		return uint64(int64(body.PC+uint64(ref.Next)) + disp), true
	})
	shards := make([][]uint64, 1, len(parts)+1)
	shards[0] = []uint64{0} // the leading zero, so an index always exists
	return sortDedupShards(append(shards, parts...))
}

// The target domain is a pure function of the old text and the map, and an
// encode asks for it several times (each prediction decodes the plan), so it
// is memoised in-process. The key is the identity of the text buffer plus the
// source column of the map -- the entry retains the text, so a matching
// buffer is the same bytes (memo.go), and the source column is small enough
// to compare outright. Hashing the text itself was 0.12 s of every apply, to
// build a key for a cache an apply never hits.
type targetsKey struct {
	text    bufID
	oldAddr uint64
}

type targetsEntry struct {
	text []byte // retained, so the key's buffer identity stays meaningful
	srcs []uint64
	v    []uint64
}

var (
	targetsMu    sync.Mutex
	targetsCache = map[targetsKey][]targetsEntry{}
)

func cachedReferenceTargets(oldText []byte, maps []mapping, oldAddr uint64) []uint64 {
	key := targetsKey{text: idOf(oldText), oldAddr: oldAddr}
	srcs := make([]uint64, 0, 2*len(maps))
	for _, m := range maps {
		srcs = append(srcs, m.Src, m.SrcSize)
	}
	targetsMu.Lock()
	for _, e := range targetsCache[key] {
		if slices.Equal(e.srcs, srcs) {
			targetsMu.Unlock()
			return e.v
		}
	}
	targetsMu.Unlock()
	v := referenceTargets(oldText, maps, oldAddr)
	targetsMu.Lock()
	if len(targetsCache) > 8 {
		clear(targetsCache)
	}
	targetsCache[key] = append(targetsCache[key], targetsEntry{text: oldText, srcs: srcs, v: v})
	targetsMu.Unlock()
	return v
}

// targetIndex names addr by the last enumerated target at or below it.
func targetIndex(targets []uint64, addr uint64) int {
	i, ok := slices.BinarySearch(targets, addr)
	if !ok {
		i--
	}
	return max(i, 0)
}

// marshal writes the structural plan: geometry, then the six map columns,
// the three point columns and the ranges (elf-module.md §2).
func (p predictionPlan) marshal(oldText []byte) ([]byte, error) {
	return p.marshalMode(oldText, nil)
}

// marshalMode is marshal with an optional derived stream: when ds is
// non-nil the five map columns go out empty and the stream sits where they
// were, immediately after the copy bitmap. Everything downstream — the
// reference points, the ranges — is byte for byte what the dense form
// emits, because the decoder reconstructs the same map and so derives the
// same reference-target basis (derived.go).
func (p predictionPlan) marshalMode(oldText []byte, ds *derivedStream) ([]byte, error) {
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
	var b []byte
	b = appendU(b, p.OldAddr)
	b = appendU(b, p.NewAddr)
	b = appendU(b, p.TargetLen)
	mode := byte(planDense)
	if ds != nil {
		mode = planDerived
	}
	b = append(b, mode)
	b = appendU(b, uint64(len(maps)))
	detected := detectBoundaries(oldText)
	var srcIndexDeltas, srcOffsets, extentResiduals, sizeDeltas, startResiduals []byte
	copyBits := make([]byte, (len(maps)+7)/8)
	var prevDstEnd uint64
	var prevIdx int
	for i, m := range maps {
		if m.Dst < prevDstEnd || m.DstSize > p.TargetLen-m.Dst {
			return nil, fmt.Errorf("map %d has overlapping or invalid destination", i)
		}
		idx := boundaryIndex(detected, m.Src)
		srcIndexDeltas = appendS(srcIndexDeltas, int64(idx-prevIdx))
		srcOffsets = appendU(srcOffsets, m.Src-detected[idx])
		extentResiduals = appendS(extentResiduals, int64(extents[m.Src])-int64(m.SrcSize))
		sizeDeltas = appendS(sizeDeltas, int64(m.SrcSize)-int64(m.DstSize))
		startResiduals = appendS(startResiduals, int64(m.Dst)-int64(alignedGuess(prevDstEnd)))
		if m.Copy {
			copyBits[i/8] |= 1 << (i % 8)
		}
		prevDstEnd, prevIdx = m.Dst+m.DstSize, idx
	}
	if ds != nil {
		srcIndexDeltas, srcOffsets, extentResiduals, sizeDeltas, startResiduals = nil, nil, nil, nil, nil
	}
	b = appendStream(b, srcIndexDeltas)
	b = appendStream(b, srcOffsets)
	b = appendStream(b, extentResiduals)
	b = appendStream(b, sizeDeltas)
	b = appendStream(b, startResiduals)
	b = appendStream(b, copyBits)
	if ds != nil {
		b = append(b, ds.marshal()...)
	}
	points := slices.Clone(p.Points)
	slices.SortFunc(points, func(a, b addressPoint) int { return cmpU(a.Old, b.Old) })
	b = appendU(b, uint64(len(points)))
	var targets []uint64
	if len(points) > 0 {
		targets = cachedReferenceTargets(oldText, maps, p.OldAddr)
	}
	var pointIndexDeltas, pointOffsets, pointShiftDeltas []byte
	var prevShift int64
	prevPointIdx := 0
	for i, point := range points {
		if i > 0 && point.Old <= points[i-1].Old {
			return nil, fmt.Errorf("address point %d is not unique", i)
		}
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
	for i, ar := range ranges {
		if ar.Size == 0 || (i > 0 && ar.Old < ranges[i-1].Old+ranges[i-1].Size) {
			return nil, fmt.Errorf("address range %d is empty or overlapping", i)
		}
		b = appendU(b, ar.Old-prevOld)
		if ar.New >= prevNew {
			b = appendS(b, int64(ar.New-prevNew))
		} else {
			b = appendS(b, -int64(prevNew-ar.New))
		}
		b = appendU(b, ar.Size)
		prevOld, prevNew = ar.Old, ar.New
	}
	return b, nil
}

// unmarshalPlan decodes a structural plan for one code window of the old
// image. The whole old file is needed because the derived map mode reads the
// relocation table as one of its three evidence sources (derived.go).
func unmarshalPlan(b, oldFile []byte, win section) (predictionPlan, error) {
	if win.Off > uint64(len(oldFile)) || win.Size > uint64(len(oldFile))-win.Off {
		return predictionPlan{}, errors.New("code window lies outside the old image")
	}
	oldText := oldFile[win.Off : win.Off+win.Size]
	r := &planReader{b: b}
	p := predictionPlan{OldAddr: r.u(), NewAddr: r.u(), TargetLen: r.u()}
	mode := r.byteAt()
	if r.err != nil || (mode != planDense && mode != planDerived) {
		return predictionPlan{}, errors.New("unsupported map mode in structural plan")
	}
	// A dense mapping costs at least a byte in each of five columns; a
	// derived one can cost nothing at all, so its floor is the window that
	// supplies the sources plus whatever the stream itself carries.
	n := r.u()
	limit := uint64(len(b))
	readMaps := readDenseMaps
	if mode == planDerived {
		limit += win.Size
		readMaps = func(r *planReader, p *predictionPlan, n uint64, _ []byte) error {
			return readDerivedMaps(r, p, n, oldFile, win)
		}
	}
	if n > limit {
		return predictionPlan{}, errors.New("implausible mapping count")
	}
	if err := readMaps(r, &p, n, oldText); err != nil {
		return predictionPlan{}, err
	}
	if err := readPointsAndRanges(r, &p, len(b), oldText); err != nil {
		return predictionPlan{}, err
	}
	if !r.done() {
		return predictionPlan{}, errors.New("trailing or invalid structural plan data")
	}
	return p, nil
}

func readDenseMaps(r *planReader, p *predictionPlan, n uint64, oldText []byte) error {
	srcIndexDeltas, srcOffsets := r.stream(), r.stream()
	extentResiduals, sizeDeltas := r.stream(), r.stream()
	startResiduals, copyBits := r.stream(), r.stream()
	if r.err != nil || len(copyBits.b) != (int(n)+7)/8 {
		return errors.New("invalid mapping streams")
	}
	// Sources first: the extent guess needs the whole source column, and
	// the destination starts need the extents.
	detected := detectBoundaries(oldText)
	srcs := make([]uint64, n)
	idx := int64(0)
	for i := uint64(0); i < n; i++ {
		idx += srcIndexDeltas.s()
		off := srcOffsets.u()
		if r.err != nil || srcIndexDeltas.err != nil || srcOffsets.err != nil || idx < 0 || idx >= int64(len(detected)) {
			return errors.New("source boundary index out of range")
		}
		if off > uint64(len(oldText))-detected[idx] {
			return errors.New("source offset exceeds the old text")
		}
		srcs[i] = detected[idx] + off
	}
	if !srcIndexDeltas.done() || !srcOffsets.done() {
		return errors.New("invalid mapping stream contents")
	}
	extents := sourceExtents(srcs, oldText)
	p.Maps = make([]mapping, 0, n)
	var prevDstEnd uint64
	for i := uint64(0); i < n; i++ {
		srcSize := int64(extents[srcs[i]]) - extentResiduals.s()
		dstSize := srcSize - sizeDeltas.s()
		dst := int64(alignedGuess(prevDstEnd)) + startResiduals.s()
		if extentResiduals.err != nil || sizeDeltas.err != nil || startResiduals.err != nil ||
			srcSize < 0 || dstSize < 0 || dst < int64(prevDstEnd) {
			return errors.New("invalid mapping extent in structural plan")
		}
		if uint64(dst) > p.TargetLen || uint64(dstSize) > p.TargetLen-uint64(dst) {
			return errors.New("mapping destination exceeds prediction")
		}
		copyFlag := copyBits.b[i/8]&(1<<(i%8)) != 0
		p.Maps = append(p.Maps, mapping{
			Src: srcs[i], SrcSize: uint64(srcSize),
			Dst: uint64(dst), DstSize: uint64(dstSize), Copy: copyFlag,
		})
		prevDstEnd = uint64(dst) + uint64(dstSize)
	}
	if !extentResiduals.done() || !sizeDeltas.done() || !startResiduals.done() {
		return errors.New("invalid mapping stream contents")
	}
	return nil
}

func readPointsAndRanges(r *planReader, p *predictionPlan, size int, oldText []byte) error {
	npoints := r.u()
	if npoints > uint64(size) {
		return errors.New("implausible address point count")
	}
	pointIndexDeltas, pointOffsets, pointShiftDeltas := r.stream(), r.stream(), r.stream()
	if r.err != nil {
		return errors.New("invalid address point streams")
	}
	var targets []uint64
	if npoints > 0 {
		targets = cachedReferenceTargets(oldText, p.Maps, p.OldAddr)
	}
	var prevPointOld uint64
	var shift int64
	idx := 0
	for i := uint64(0); i < npoints; i++ {
		idx += int(pointIndexDeltas.s())
		offset := pointOffsets.u()
		shift += pointShiftDeltas.s()
		if pointIndexDeltas.err != nil || pointOffsets.err != nil || pointShiftDeltas.err != nil ||
			idx < 0 || idx >= len(targets) || offset > ^uint64(0)-targets[idx] {
			return errors.New("invalid address point")
		}
		old := targets[idx] + offset
		if i > 0 && old <= prevPointOld {
			return errors.New("address points are not increasing")
		}
		newAddr := int64(old) + shift
		if newAddr < 0 {
			return errors.New("address point underflows")
		}
		p.Points = append(p.Points, addressPoint{Old: old, New: uint64(newAddr)})
		prevPointOld = old
	}
	if !pointIndexDeltas.done() || !pointOffsets.done() || !pointShiftDeltas.done() {
		return errors.New("invalid address point stream contents")
	}
	nranges := r.u()
	if nranges > uint64(size) {
		return errors.New("implausible address range count")
	}
	var prevOld, prevNew, prevEnd uint64
	for i := uint64(0); i < nranges; i++ {
		oldDelta, newDelta, size := r.u(), r.s(), r.u()
		if r.err != nil || oldDelta > ^uint64(0)-prevOld {
			return errors.New("invalid address range")
		}
		old := prevOld + oldDelta
		var newAddr uint64
		if newDelta >= 0 {
			if uint64(newDelta) > ^uint64(0)-prevNew {
				return errors.New("address range overflows")
			}
			newAddr = prevNew + uint64(newDelta)
		} else {
			d := uint64(-(newDelta + 1)) + 1
			if d > prevNew {
				return errors.New("address range underflows")
			}
			newAddr = prevNew - d
		}
		if size == 0 || (i > 0 && old < prevEnd) || size > ^uint64(0)-old {
			return errors.New("empty or overlapping address range")
		}
		p.Ranges = append(p.Ranges, addressRange{Old: old, New: newAddr, Size: size})
		prevOld, prevNew, prevEnd = old, newAddr, old+size
	}
	return nil
}

// addressLookup answers where an old .text address landed: exact points
// first, then the function map, then the section shift ranges.
type addressLookup struct {
	p      predictionPlan
	bySrc  []mapping
	points *pageIndex
	maps   *pageIndex
}

func newAddressLookup(p predictionPlan) *addressLookup {
	bySrc := slices.Clone(p.Maps)
	sortByKey(bySrc, func(m mapping) uint64 { return m.Src }, func(a, b mapping) int {
		if a.Src != b.Src {
			return cmpU(a.Src, b.Src)
		}
		return cmpU(a.Dst, b.Dst)
	})
	pi := newPageIndex(len(p.Points), func(i int) uint64 { return p.Points[i].Old }, nil)
	mi := newPageIndex(len(bySrc), func(i int) uint64 { return bySrc[i].Src },
		func(i int) uint64 { return bySrc[i].Src + bySrc[i].SrcSize })
	return &addressLookup{p: p, bySrc: bySrc, points: pi, maps: mi}
}

func (l *addressLookup) pointTarget(addr uint64) x86.Target {
	lo, hi := l.points.bounds(addr, len(l.p.Points))
	i, ok := slices.BinarySearchFunc(l.p.Points[lo:hi], addr, func(point addressPoint, addr uint64) int {
		return cmpU(point.Old, addr)
	})
	if ok {
		return x86.Target{Addr: l.p.Points[lo+i].New, Known: true}
	}
	return x86.Target{}
}

func (l *addressLookup) mapTarget(addr uint64) x86.Target {
	p := l.p
	if addr < p.OldAddr {
		return x86.Target{}
	}
	off := addr - p.OldAddr
	lo, hi := l.maps.bounds(off, len(l.bySrc))
	i, ok := slices.BinarySearchFunc(l.bySrc[lo:hi], off, func(m mapping, off uint64) int {
		if m.Src > off {
			return 1
		}
		if m.Src+m.SrcSize <= off {
			return -1
		}
		return 0
	})
	if ok {
		m := l.bySrc[lo+i]
		delta := off - m.Src
		if delta < m.DstSize {
			return x86.Target{Addr: p.NewAddr + m.Dst + delta, Known: true}
		}
	}
	return x86.Target{}
}

// codeLookup answers address questions across every code window. The order
// is the same as one window's: an exact point first, then the function map,
// then the section ranges — but each phase asks every window before the next
// begins, so a map in one window is never overruled by a range answer that
// another window's plan happens to carry. With one window it is exactly
// addressLookup.target.
type codeLookup struct {
	win []*addressLookup
}

func newCodeLookup(structures []predictionPlan) *codeLookup {
	l := &codeLookup{win: make([]*addressLookup, len(structures))}
	for i := range structures {
		l.win[i] = newAddressLookup(structures[i])
	}
	return l
}

func (l *codeLookup) pointTarget(addr uint64) x86.Target {
	for _, w := range l.win {
		if t := w.pointTarget(addr); t.Known {
			return t
		}
	}
	return x86.Target{}
}

func (l *codeLookup) mapTarget(addr uint64) x86.Target {
	for _, w := range l.win {
		if t := w.mapTarget(addr); t.Known {
			return t
		}
	}
	return x86.Target{}
}

func (l *codeLookup) target(addr uint64) x86.Target {
	if t := l.pointTarget(addr); t.Known {
		return t
	}
	if t := l.mapTarget(addr); t.Known {
		return t
	}
	// Every window's plan carries the same ranges; ask one.
	if len(l.win) != 0 {
		return l.win[0].rangeTarget(addr)
	}
	return x86.Target{}
}

func (l *addressLookup) rangeTarget(addr uint64) x86.Target {
	p := l.p
	i, ok := slices.BinarySearchFunc(p.Ranges, addr, func(ar addressRange, addr uint64) int {
		if ar.Old > addr {
			return 1
		}
		if ar.Old+ar.Size <= addr {
			return -1
		}
		return 0
	})
	if ok {
		ar := p.Ranges[i]
		return x86.Target{Addr: ar.New + addr - ar.Old, Known: true}
	}
	return x86.Target{}
}

func (l *addressLookup) target(addr uint64) x86.Target {
	if t := l.pointTarget(addr); t.Known {
		return t
	}
	if t := l.mapTarget(addr); t.Known {
		return t
	}
	return l.rangeTarget(addr)
}

// predictDecoded is the structural .text prediction: every copied body
// relocated into place through lookupFn (the plan's own lookup when nil).
func predictDecoded(oldText []byte, p predictionPlan, lookupFn func(uint64) x86.Target) ([]byte, x86.Stats, error) {
	if p.TargetLen > uint64(int(^uint(0)>>1)) {
		return nil, x86.Stats{}, errors.New("prediction is too large")
	}
	out := make([]byte, int(p.TargetLen))
	fill(out, 0xcc)
	if lookupFn == nil {
		lookupFn = newAddressLookup(p).target
	}
	var (
		errMu sync.Mutex
		bad   error
	)
	fail := func(err error) {
		errMu.Lock()
		if bad == nil {
			bad = err
		}
		errMu.Unlock()
	}
	stats := parallelStats(len(p.Maps), workers(), func(st *x86.Stats, i int) {
		m := p.Maps[i]
		if !m.Copy {
			return
		}
		if m.Src > uint64(len(oldText)) || m.SrcSize > uint64(len(oldText))-m.Src {
			fail(fmt.Errorf("map %d source exceeds old text", i))
			return
		}
		if m.Dst > uint64(len(out)) || m.DstSize > uint64(len(out))-m.Dst {
			fail(fmt.Errorf("map %d destination exceeds target text", i))
			return
		}
		x86.Relocate(oldText[m.Src:m.Src+m.SrcSize], out[m.Dst:m.Dst+m.DstSize], p.OldAddr+m.Src, p.NewAddr+m.Dst, lookupFn, st, nil)
	})
	if bad != nil {
		return nil, x86.Stats{}, bad
	}
	return out, stats, nil
}
