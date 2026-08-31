package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"

	"github.com/wjordan/presage/delta/x86"
)

// EPP5 columnised the function map so that neither the extent of a function
// nor its source address is stored twice, taking the structural plan from
// 2,107,780 to 1,339,528 compressed bytes. Consecutive source addresses
// advance by the same amount as consecutive destination addresses for 96.6%
// of Chrome's functions, so the source column is delta-coded against the
// destination column rather than against itself.
//
// EPP9 applies the same reasoning to the reference points. A point's new
// address is its old one plus a shift, and that shift is piecewise constant --
// 99.200% of points keep the previous one -- so coding the shift's change
// instead of the address's takes that column from 176,096 to 4,472. The old
// column was being carried twice.
//
// EPP10 finishes the point table the same way. A point's old address is by
// construction a branch or call target in the old image, and the decoder holds
// the old image, so it can walk the mapped function bodies and enumerate every
// such target for itself. All 259,469 of Chrome's points are in that list of
// 6,093,998, so the column ships an index into it rather than an address:
// 171,984 compressed bytes become 14,404.
//
// EPP8 pushes that principle through the whole destination side of the map.
// The old image's padding says where old functions begin and end, so the
// source column names a detected boundary rather than an address, and the
// extent is guessed from the padding that follows it. With extents known
// before starts, a new function's start is just the previous one's end rounded
// up to sixteen bytes, right 90.832% of the time. Three columns that cost
// 1,004,300 compressed bytes standalone become four that cost 326,172.
// The tag is four bytes, so EPP10 is 'A' and EPP11 'B'.
var planMagic = [4]byte{'E', 'P', 'P', 'B'}

// planMode selects how the function map is encoded.
//
//   - planDense ships every function with its exact extent and source.
//   - planSparse ships only the functions the selector chose; address queries
//     and the walker fall back to Ranges, the piecewise-constant shift map.
//
// A third mode that replaced the per-function source column with an index-keyed
// shift map -- 31,171 breakpoints for 925,344 residuals -- was measured and
// removed: it saved 38,436 compressed bytes, because xz was already pricing
// that column at almost nothing next to the two it sits beside.
type planMode byte

const (
	planDense planMode = iota
	planSparse
	// planGoDerived transmits no map: the decoder derives it from the Go-table
	// plan carried beside the structural plan (see delta.GoFunctionMap).
	planGoDerived
	// planSidecar transmits the five map columns empty and, in their place, a
	// delta against a symbol table the decoder carries from the previous
	// patch (see symsidecar.go). Everything else is exactly planDense.
	planSidecar
)

type mapping struct {
	Src     uint64
	SrcSize uint64
	Dst     uint64
	DstSize uint64
	Copy    bool
}

type addressRange struct {
	Old  uint64
	New  uint64
	Size uint64
}

type addressPoint struct {
	Old uint64
	New uint64
}

type predictionPlan struct {
	OldAddr   uint64
	NewAddr   uint64
	TargetLen uint64
	// Mode selects how the function map is encoded; see planMode.
	Mode   planMode
	Maps   []mapping // canonical serialization order is destination order
	Points []addressPoint
	Ranges []addressRange
	// Prior answers before the plan's own evidence: the Go-table module's
	// address map for a planGoDerived plan. Never serialized; both sides
	// derive it.
	Prior func(uint64) x86.Target
}

// derivedMap is what a planGoDerived plan takes from the Go-table plan
// beside it: the function map and the address map.
type derivedMap struct {
	maps  []mapping
	prior func(uint64) x86.Target
}

func appendU(b []byte, v uint64) []byte {
	return binary.AppendUvarint(b, v)
}

func appendS(b []byte, v int64) []byte {
	return binary.AppendVarint(b, v)
}

func appendStream(b, stream []byte) []byte {
	b = appendU(b, uint64(len(stream)))
	return append(b, stream...)
}

// boundaryAlign is the alignment a candidate function start must have. A
// spurious candidate is not free after all: the plan names a boundary by its
// index, so every false positive below a real start pushes that start's index
// up and widens the delta. Requiring eight-byte alignment discards 414,381 of
// the 1,352,889 raw candidates and only 6 of the 882,098 real starts, taking
// the source columns from 283,828 to 197,684. Sixteen is what the compiler
// actually aligns to and cuts more candidates still, but it loses 82,114 real
// starts to the offset column and comes out level.
const boundaryAlign = 8

// detectBoundaries lists where old functions begin, as the old image shows it:
// the first byte, and any aligned non-padding byte that follows padding. It
// still over-reads -- 938,508 candidates for 924,932 real starts, the excess
// being data islands and padding inside function bodies -- but a wrong entry
// is simply never named.
func detectBoundaries(oldText []byte) []uint64 {
	out := []uint64{0}
	for i := boundaryAlign; i < len(oldText); i += boundaryAlign {
		if oldText[i] != 0xcc && oldText[i-1] == 0xcc {
			out = append(out, uint64(i))
		}
	}
	return out
}

// boundaryIndex finds the greatest detected boundary at or below addr. Naming
// the boundary and an offset from it avoids a sentinel for "not a boundary",
// which would be wrong in any case: identical-code folding makes consecutive
// mappings share a source, so an index delta of zero is legitimate.
func boundaryIndex(detected []uint64, addr uint64) int {
	i, ok := slices.BinarySearch(detected, addr)
	if !ok {
		i--
	}
	return max(i, 0)
}

// alignedGuess is where the next function starts if the only thing between it
// and the previous one is alignment padding.
func alignedGuess(prevEnd uint64) uint64 { return prevEnd + (16-prevEnd%16)%16 }

// sourceExtents guesses where each old function's body ends: the byte after
// the last non-padding byte before the next distinct source start. Both sides
// compute it from the same inputs -- the source column and the old text -- so
// the plan carries only where it is wrong.
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

// referenceTargets enumerates every address the old image's mapped functions
// branch or call to, sorted and deduplicated. Both sides can build it: it
// depends only on the old image and on the function map, which the plan
// decodes before it reaches the point table. A leading zero keeps the list
// non-empty and below every address, so an index always exists.
func referenceTargets(oldText []byte, maps []mapping, oldAddr uint64) []uint64 {
	bySrc := slices.Clone(maps)
	slices.SortFunc(bySrc, func(a, b mapping) int { return cmpU(a.Src, b.Src) })
	// One body per distinct source, walked concurrently. The result is sorted
	// and deduplicated below, so the order bodies come back in does not matter.
	var bodies []x86.Body
	var bases []uint64
	var prevSrc uint64
	for i, m := range bySrc {
		if i > 0 && m.Src == prevSrc {
			continue // identical-code folding: one body, several destinations
		}
		prevSrc = m.Src
		if m.Src > uint64(len(oldText)) || m.SrcSize > uint64(len(oldText))-m.Src {
			continue
		}
		bodies = append(bodies, x86.Body{Code: oldText[m.Src : m.Src+m.SrcSize]})
		bases = append(bases, oldAddr+m.Src)
	}
	res := x86.WalkBodies(bodies, runtime.GOMAXPROCS(0))
	out := []uint64{0}
	for k, refs := range res {
		body := bodies[k].Code
		base := bases[k]
		for _, ref := range refs {
			disp, ok := readDisplacement(body, ref)
			if !ok {
				continue
			}
			out = append(out, uint64(int64(base+uint64(ref.Next))+disp))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// targetIndex names addr by the last enumerated target at or below it.
func targetIndex(targets []uint64, addr uint64) int {
	i, ok := slices.BinarySearch(targets, addr)
	if !ok {
		i--
	}
	return max(i, 0)
}

func (p predictionPlan) marshal(oldText []byte) ([]byte, error) {
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
	sent := maps
	if p.Mode == planGoDerived {
		sent = nil
	}
	b = appendU(b, uint64(len(sent)))
	detected := detectBoundaries(oldText)
	var srcIndexDeltas, srcOffsets, extentResiduals, sizeDeltas, startResiduals []byte
	copyBits := make([]byte, (len(sent)+7)/8)
	var prevDstEnd uint64
	var prevIdx int
	for i, m := range maps {
		if m.Dst < prevDstEnd || m.DstSize > p.TargetLen-m.Dst {
			return nil, fmt.Errorf("map %d has overlapping or invalid destination", i)
		}
		if p.Mode == planGoDerived {
			prevDstEnd = m.Dst + m.DstSize
			continue
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

type planReader struct {
	b   []byte
	err error
}

func (r *planReader) u() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = errors.New("invalid unsigned integer in plan")
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *planReader) s() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.b)
	if n <= 0 {
		r.err = errors.New("invalid signed integer in plan")
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *planReader) stream() *planReader {
	n := r.u()
	if r.err != nil || n > uint64(len(r.b)) {
		r.err = errors.New("invalid typed stream length in plan")
		return &planReader{err: r.err}
	}
	s := &planReader{b: r.b[:n]}
	r.b = r.b[n:]
	return s
}

func (r *planReader) done() bool { return r.err == nil && len(r.b) == 0 }

func (r *planReader) byteAt() byte {
	if r.err != nil || len(r.b) == 0 {
		r.err = errors.New("prediction plan truncated")
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

// unmarshalPlan decodes a structural plan. derive supplies the function map
// of a planGoDerived plan, which carries none of its own; it is called only
// for that mode.
func unmarshalPlan(b, oldText []byte, derive func() (derivedMap, error)) (predictionPlan, error) {
	if len(b) < len(planMagic) || !bytes.Equal(b[:4], planMagic[:]) {
		return predictionPlan{}, errors.New("invalid prediction plan magic")
	}
	r := &planReader{b: b[4:]}
	p := predictionPlan{OldAddr: r.u(), NewAddr: r.u(), TargetLen: r.u()}
	if flag := r.byteAt(); flag > byte(planSidecar) {
		return predictionPlan{}, errors.New("invalid map mode in prediction plan")
	} else {
		p.Mode = planMode(flag)
	}
	n := r.u()
	if n > uint64(len(b)) {
		return predictionPlan{}, errors.New("implausible mapping count")
	}
	if p.Mode == planSidecar {
		if err := readSidecarMaps(r, &p, n); err != nil {
			return predictionPlan{}, err
		}
	} else if err := readDenseMaps(r, &p, n, oldText); err != nil {
		return predictionPlan{}, err
	}
	if p.Mode == planGoDerived {
		if n != 0 {
			return predictionPlan{}, errors.New("derived-map plan carries a map")
		}
		if derive == nil {
			return predictionPlan{}, errors.New("derived-map plan without a Go-table plan to derive from")
		}
		d, err := derive()
		if err != nil {
			return predictionPlan{}, err
		}
		p.Maps, p.Prior = d.maps, d.prior
	}
	if err := readPointsAndRanges(r, &p, len(b), oldText); err != nil {
		return predictionPlan{}, err
	}
	if r.err != nil || len(r.b) != 0 {
		return predictionPlan{}, errors.New("trailing or invalid prediction plan data")
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
	// Sources first, because the extent guess needs the whole source column --
	// a function's end is bounded by the next function's start -- and the
	// destination starts need the extents.
	detected := detectBoundaries(oldText)
	srcs := make([]uint64, n)
	idx := int64(0)
	for i := uint64(0); i < n; i++ {
		idx += srcIndexDeltas.s()
		off := srcOffsets.u()
		if r.err != nil || idx < 0 || idx >= int64(len(detected)) {
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
		if r.err != nil || srcSize < 0 || dstSize < 0 || dst < int64(prevDstEnd) {
			return errors.New("invalid mapping extent in prediction plan")
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

// readPointsAndRanges decodes the two auxiliary tables shared by every mode.
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
		if r.err != nil || idx < 0 || idx >= len(targets) || offset > ^uint64(0)-targets[idx] {
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

func cmpU(a, b uint64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

type addressLookup struct {
	p     predictionPlan
	bySrc []mapping
}

func newAddressLookup(p predictionPlan) *addressLookup {
	bySrc := slices.Clone(p.Maps)
	slices.SortFunc(bySrc, func(a, b mapping) int {
		if a.Src != b.Src {
			return cmpU(a.Src, b.Src)
		}
		return cmpU(a.Dst, b.Dst)
	})
	return &addressLookup{p: p, bySrc: bySrc}
}

// pointTarget answers only from the exact correspondences, which are the most
// reliable evidence in the plan.
func (l *addressLookup) pointTarget(addr uint64) x86.Target {
	i, ok := slices.BinarySearchFunc(l.p.Points, addr, func(point addressPoint, addr uint64) int {
		return cmpU(point.Old, addr)
	})
	if ok {
		return x86.Target{Addr: l.p.Points[i].New, Known: true}
	}
	return x86.Target{}
}

// mapTarget answers from the symbol-derived function map: an address inside a
// mapped old function lands at the same displacement inside its counterpart.
func (l *addressLookup) mapTarget(addr uint64) x86.Target {
	p := l.p
	if addr < p.OldAddr {
		return x86.Target{}
	}
	off := addr - p.OldAddr
	i, ok := slices.BinarySearchFunc(l.bySrc, off, func(m mapping, off uint64) int {
		if m.Src > off {
			return 1
		}
		if m.Src+m.SrcSize <= off {
			return -1
		}
		return 0
	})
	if ok {
		m := l.bySrc[i]
		delta := off - m.Src
		if delta < m.DstSize {
			return x86.Target{Addr: p.NewAddr + m.Dst + delta, Known: true}
		}
	}
	return x86.Target{}
}

func (l *addressLookup) target(addr uint64) x86.Target {
	p := l.p
	if t := l.pointTarget(addr); t.Known {
		return t
	}
	if p.Prior != nil {
		if t := p.Prior(addr); t.Known {
			return t
		}
	}
	if t := l.mapTarget(addr); t.Known {
		return t
	}
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

// predict is decoder-faithful: its only inputs are old code, a serialized
// plan, and the choice of whether to retarget PC-relative operands.
func predict(old, encodedPlan []byte, relocate bool, derive func() (derivedMap, error)) ([]byte, x86.Stats, error) {
	return predictWith(old, encodedPlan, relocate, nil, derive)
}

// predictWith is predict with the caller's address oracle. The structural map
// describes .text and nothing else, so on its own it cannot say where a
// reference that leaves .text now points. A whole-image caller knows -- it
// holds the section geometry and the equivalence projection -- and passing
// that in costs no plan bytes, because both sides build the oracle from
// streams they already hold.
func predictWith(old, encodedPlan []byte, relocate bool, lookupFn func(uint64) x86.Target, derive func() (derivedMap, error)) ([]byte, x86.Stats, error) {
	p, err := unmarshalPlan(encodedPlan, old, derive)
	if err != nil {
		return nil, x86.Stats{}, err
	}
	return predictDecoded(old, p, relocate, lookupFn)
}

// predictDecoded is predictWith for a caller that already decoded the plan --
// predictImage holds both the plan and the sorted lookup, so re-deriving them
// from the encoded bytes would only repeat work.
func predictDecoded(old []byte, p predictionPlan, relocate bool, lookupFn func(uint64) x86.Target) ([]byte, x86.Stats, error) {
	if p.TargetLen > uint64(int(^uint(0)>>1)) {
		return nil, x86.Stats{}, errors.New("prediction is too large")
	}
	out := make([]byte, int(p.TargetLen))
	for i := range out {
		out[i] = 0xcc
	}
	if lookupFn == nil {
		lookupFn = newAddressLookup(p).target
	}

	// One function body per map, relocated concurrently: each writes only its
	// own disjoint [Dst,Dst+DstSize) span of out and lookupFn is a read of an
	// immutable plan, so the output and the summed stats match a serial loop.
	var (
		errMu sync.Mutex
		bad   error
	)
	stats := parallelStats(len(p.Maps), workers(), func(st *x86.Stats, i int) {
		m := p.Maps[i]
		if !m.Copy {
			return
		}
		if m.Src > uint64(len(old)) || m.SrcSize > uint64(len(old))-m.Src {
			errMu.Lock()
			if bad == nil {
				bad = fmt.Errorf("map %d source exceeds old text", i)
			}
			errMu.Unlock()
			return
		}
		if m.Dst > uint64(len(out)) || m.DstSize > uint64(len(out))-m.Dst {
			errMu.Lock()
			if bad == nil {
				bad = fmt.Errorf("map %d destination exceeds target text", i)
			}
			errMu.Unlock()
			return
		}
		src := old[m.Src : m.Src+m.SrcSize]
		dst := out[m.Dst : m.Dst+m.DstSize]
		if relocate {
			x86.Relocate(src, dst, p.OldAddr+m.Src, p.NewAddr+m.Dst, lookupFn, st, nil)
		} else {
			copy(dst, src)
		}
	})
	if bad != nil {
		return nil, x86.Stats{}, bad
	}
	return out, stats, nil
}

// sparseStructure re-expresses a plan for the decoder. The per-function map is
// replaced by the piecewise-constant shift map it already implies -- 925,344
// functions collapse to 31,281 ranges, because consecutive functions keep
// their relative spacing 96.6% of the time -- and only the functions the
// selector actually chose keep their exact extents, since those are the only
// ones whose bodies get used.
func sparseStructure(p predictionPlan, choices []byte, oldTextSize uint64) predictionPlan {
	maps := slices.Clone(p.Maps)
	slices.SortFunc(maps, func(a, b mapping) int {
		if a.Dst != b.Dst {
			return cmpU(a.Dst, b.Dst)
		}
		return cmpU(a.Src, b.Src)
	})
	out := predictionPlan{
		OldAddr: p.OldAddr, NewAddr: p.NewAddr, TargetLen: p.TargetLen,
		Mode: planSparse, Points: slices.Clone(p.Points),
	}
	var ranges []addressRange
	var prevShift int64
	for i, m := range maps {
		shift := int64(m.Src) - int64(m.Dst)
		if i == 0 || shift != prevShift {
			ranges = append(ranges, addressRange{Old: p.OldAddr + m.Src, New: p.NewAddr + m.Dst})
			prevShift = shift
		}
		if i/8 < len(choices) && choices[i/8]&(1<<(i%8)) != 0 {
			out.Maps = append(out.Maps, m)
		}
	}
	slices.SortFunc(ranges, func(a, b addressRange) int { return cmpU(a.Old, b.Old) })
	// Identical-code folding lets several destination functions share one
	// source, so two shift breakpoints can land on the same old address. Keep
	// the first at each: the map is source-keyed and can only answer once,
	// which is the same ambiguity the per-function map already had.
	kept := ranges[:0]
	for _, ar := range ranges {
		if len(kept) > 0 && kept[len(kept)-1].Old == ar.Old {
			continue
		}
		kept = append(kept, ar)
	}
	ranges = kept
	// Each range runs up to the next one in source order; the last covers the
	// remainder of the old section.
	for i := range ranges {
		end := p.OldAddr + oldTextSize
		if i+1 < len(ranges) {
			end = ranges[i+1].Old
		}
		if end <= ranges[i].Old {
			ranges = ranges[:i]
			break
		}
		ranges[i].Size = end - ranges[i].Old
	}
	out.Ranges = ranges
	return out
}
