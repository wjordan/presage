package elfmod

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/wjordan/presage/delta/x86"
)

// The derived function map (research/pgo-churn.md §5.1c).
//
// The dense map's five columns name each mapped function by a boundary index
// plus an offset, and they are the largest single row of the plan's ledger.
// Most of what they say is already visible in the old image: a shared,
// ordered enumeration of old function starts can be *derived* from the old
// bytes alone, and the plan then only has to describe how the new map differs
// from a positional walk of it — drops, order exceptions, size deltas,
// inserts and a layout replay.
//
// Nothing is carried between patches: the enumeration is a pure deterministic
// function of the old image, computed by one function both sides call. As a
// loud guard against that ever slipping, the stream ships the enumeration's
// length and the decoder refuses a plan whose own derivation disagrees.

// Structural plan map modes, the byte after the geometry.
const (
	planDense   = 0 // the five map columns, verbatim
	planDerived = 1 // the derived enumeration plus a delta stream
)

// --- the enumeration ----------------------------------------------------

// deriveEnumeration lists candidate old function starts in a code window, as
// offsets into it. Its only inputs are the old file's bytes and where the
// window lies in them.
//
//	E1  call rel32 targets, from a linear x86 sweep of the whole window
//	E2  relocation addends landing in the window
//	E3  padding starts, the detectBoundaries rule
//
// Jump targets are deliberately absent: §5.1c measured them at 1.4–5.0 M
// spurious entries for +0.7 pp of recall. A file with no parseable relocation
// section simply contributes no E2; that cannot desynchronise the two sides,
// which run this same function over the same bytes.
func deriveEnumeration(oldFile []byte, win section) []uint64 {
	if win.Off > uint64(len(oldFile)) || win.Size > uint64(len(oldFile))-win.Off {
		return nil
	}
	code := oldFile[win.Off : win.Off+win.Size]
	// The three enumerations read the old image and nothing else, so they run
	// concurrently; unionSorted puts them back in one order regardless.
	var ct, rw, db []uint64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rw = relaWindowTargets(oldFile, win)
	}()
	go func() {
		defer wg.Done()
		db = detectBoundaries(code)
	}()
	ct = callTargets(code, win.Addr)
	wg.Wait()
	return unionSorted(ct, rw, db)
}

// callTargets is E1: every rel32 call whose target lands back in the window.
// Chunking is fixed and both sides use it, so the desynchronised decoding
// inside data islands is identical on both sides; a wrong target is only ever
// a spurious enumeration entry, never a correctness hazard.
func callTargets(code []byte, addr uint64) []uint64 {
	const chunk = 8 << 20
	n := (len(code) + chunk - 1) / chunk
	parts := make([][]uint64, n)
	end := addr + uint64(len(code))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers())
	for k := 0; k < n; k++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(k int) {
			defer wg.Done()
			defer func() { <-sem }()
			lo := k * chunk
			hi := min(lo+chunk, len(code))
			part := code[lo:hi]
			x86.WalkReferences(part, addr+uint64(lo), func(ref x86.Reference) {
				if ref.Target < addr || ref.Target >= end || ref.N != 4 ||
					ref.Start < 0 || ref.Start >= len(part) || part[ref.Start] != 0xe8 {
					return
				}
				parts[k] = append(parts[k], ref.Target-addr)
			})
		}(k)
	}
	wg.Wait()
	var out []uint64
	for _, p := range parts {
		out = append(out, p...)
	}
	return sortUniq(out)
}

// relaWindowTargets is E2: the R_X86_64_RELATIVE addends that point into the
// window, which is where the address-taken functions show up.
func relaWindowTargets(oldFile []byte, win section) []uint64 {
	im, err := loadImage(oldFile)
	if err != nil {
		return nil
	}
	sec, ok := relaSection(im.Sections)
	if !ok || sec.NoBits || sec.Off > uint64(len(oldFile)) || sec.Size > uint64(len(oldFile))-sec.Off {
		return nil
	}
	rel, _, _ := parseRela(oldFile[sec.Off : sec.Off+sec.Size])
	end := win.Addr + win.Size
	var out []uint64
	for _, e := range rel {
		if e.addend >= win.Addr && e.addend < end {
			out = append(out, e.addend-win.Addr)
		}
	}
	return sortUniq(out)
}

func sortUniq(v []uint64) []uint64 {
	return sortDedupShards([][]uint64{v})
}

// unionSorted is the sorted union of its arguments -- which is exactly the
// parallel sort's shard form, so it hands them straight over rather than
// concatenating first.
func unionSorted(vs ...[]uint64) []uint64 {
	return sortDedupShards(vs)
}

// The sweep costs seconds on a 130 MB window and every prediction of one
// encode wants the same answer, so it is memoised by content. The key is the
// image's content hash, not its address: a pointer would go stale the moment
// two images shared a buffer.
type enumKey struct {
	file bufID
	win  section
}

type enumEntry struct {
	file []byte // retained, so the key's buffer identity stays meaningful
	v    []uint64
}

var (
	enumMu    sync.Mutex
	enumCache = map[enumKey]enumEntry{}
)

func cachedEnumeration(oldFile []byte, win section) []uint64 {
	key := enumKey{file: idOf(oldFile), win: win}
	enumMu.Lock()
	e, ok := enumCache[key]
	enumMu.Unlock()
	if ok {
		return e.v
	}
	v := deriveEnumeration(oldFile, win)
	enumMu.Lock()
	if len(enumCache) > 8 {
		clear(enumCache)
	}
	enumCache[key] = enumEntry{file: oldFile, v: v}
	enumMu.Unlock()
	return v
}

// paddingExtent is the size rule both sides apply: run to the next enumerated
// start and back off the trailing 0xcc padding. It is sourceExtents' rule,
// restated over an enumeration rather than a source column.
func paddingExtent(code []byte, start, next uint64) uint64 {
	end := next
	for end > start && code[end-1] == 0xcc {
		end--
	}
	return end - start
}

// derivedUnit is one entry of the repaired enumeration: an old function as
// the decoder is able to see it.
type derivedUnit struct{ Off, Size uint64 }

// enumerationUnits turns a derived enumeration plus the two columns that
// repair it into the ordered unit list the delta stream is keyed against.
// Both sides run exactly this.
func enumerationUnits(derived []uint64, suppress, boundary, code []byte) ([]derivedUnit, error) {
	kept := derived
	if len(suppress) != 0 {
		if len(suppress) != (len(derived)+7)/8 {
			return nil, errors.New("derived suppression bitmap does not match the enumeration")
		}
		kept = nil
		for i, off := range derived {
			if suppress[i/8]&(1<<(i%8)) != 0 {
				kept = append(kept, off)
			}
		}
	}
	var missing []uint64
	r := &planReader{b: boundary}
	var prev uint64
	for !r.done() {
		prev += r.u()
		if r.err != nil {
			return nil, errors.New("invalid derived boundary exception")
		}
		missing = append(missing, prev)
	}
	if r.err != nil {
		return nil, errors.New("invalid derived boundary exception")
	}
	offs := unionSorted(kept, missing)
	units := make([]derivedUnit, len(offs))
	for i, off := range offs {
		if off >= uint64(len(code)) {
			return nil, errors.New("derived enumeration entry lies outside the old code window")
		}
		next := uint64(len(code))
		if i+1 < len(offs) {
			next = offs[i+1]
		}
		units[i] = derivedUnit{Off: off, Size: paddingExtent(code, off, next)}
	}
	return units, nil
}

// --- the delta stream ---------------------------------------------------

// mapDelta replaces the map's five columns. Every field is a separate byte
// column so that the plan's compressor sees like next to like.
//
// The harness's sidecar form (research/pgo-churn.md §5.1b) also carries a
// name hash per insert and a correspondence-exception layer; neither exists
// here, because there is no carried table to roll forward and the encoder
// codes the map's own correspondence directly against the positional cursor.
type mapDelta struct {
	NewUnits uint64
	Align    uint64
	Maps     uint64 // mappings the decoder must end up with

	DropRuns   []byte // alternating kept-run, dropped-run lengths over old units
	OrderIndex []byte // new-unit gaps where the walk's cursor is wrong
	OrderSrc   []byte // and how far from the cursor the truth is
	SizeIndex  []byte // new-unit gaps where the size changed
	SizeDelta  []byte
	InsertPos  []byte // new-unit gaps of the units with no old counterpart
	InsertSize []byte
	FixIndex   []byte // new-unit gaps where the aligned layout guess is wrong
	FixDelta   []byte
	Raw        []byte // mappings the unit model cannot express at all
}

func (d *mapDelta) columns() []*[]byte {
	return []*[]byte{
		&d.DropRuns, &d.OrderIndex, &d.OrderSrc, &d.SizeIndex, &d.SizeDelta,
		&d.InsertPos, &d.InsertSize, &d.FixIndex, &d.FixDelta, &d.Raw,
	}
}

func (d *mapDelta) marshal() []byte {
	b := appendU(nil, d.NewUnits)
	b = appendU(b, d.Align)
	b = appendU(b, d.Maps)
	for _, c := range d.columns() {
		b = appendStream(b, *c)
	}
	return b
}

func parseMapDelta(r *planReader) (*mapDelta, error) {
	d := &mapDelta{NewUnits: r.u(), Align: r.u(), Maps: r.u()}
	if r.err != nil || d.Align == 0 {
		return nil, errors.New("invalid map delta header")
	}
	for _, c := range d.columns() {
		*c = r.stream().b
	}
	if r.err != nil {
		return nil, errors.New("invalid map delta streams")
	}
	return d, nil
}

// derivedStream is the map delta re-keyed against a derived enumeration, plus
// the columns that turn "what the old image implies" into "the old unit list".
type derivedStream struct {
	base *mapDelta

	// Derived is how many entries the encoder's derivation produced. The
	// decoder derives its own and refuses the plan if the two disagree, which
	// makes "the derivation is identical on both sides" a checked property
	// rather than a hope.
	Derived uint64

	Boundary   []byte // address gaps of the true starts the enumeration missed
	Suppress   []byte // bitmap: which derived entries are real
	SizeFixIdx []byte // enumeration entries whose padding-rule size is wrong
	SizeFixVal []byte
}

func (s *derivedStream) marshal() []byte {
	b := appendU(nil, s.Derived)
	b = appendStream(b, s.Boundary)
	b = appendStream(b, s.Suppress)
	b = appendStream(b, s.SizeFixIdx)
	b = appendStream(b, s.SizeFixVal)
	return append(b, s.base.marshal()...)
}

func parseDerivedStream(r *planReader) (*derivedStream, error) {
	s := &derivedStream{Derived: r.u()}
	s.Boundary = r.stream().b
	s.Suppress = r.stream().b
	s.SizeFixIdx = r.stream().b
	s.SizeFixVal = r.stream().b
	if r.err != nil {
		return nil, errors.New("invalid derived enumeration streams")
	}
	d, err := parseMapDelta(r)
	if err != nil {
		return nil, err
	}
	s.base = d
	return s, nil
}

// derivedUnits is the decoder's whole enumeration path: derive, repair, size.
// It is a pure function of the old file's bytes and the stream.
func derivedUnits(s *derivedStream, oldFile []byte, win section) ([]derivedUnit, error) {
	if win.Off > uint64(len(oldFile)) || win.Size > uint64(len(oldFile))-win.Off {
		return nil, errors.New("the old code window lies outside the old image")
	}
	code := oldFile[win.Off : win.Off+win.Size]
	d := cachedEnumeration(oldFile, win)
	if uint64(len(d)) != s.Derived {
		return nil, fmt.Errorf("derived enumeration has %d entries, the plan was built against %d", len(d), s.Derived)
	}
	units, err := enumerationUnits(d, s.Suppress, s.Boundary, code)
	if err != nil {
		return nil, err
	}
	fixAt, err := sparseSigned(s.SizeFixIdx, s.SizeFixVal, len(units))
	if err != nil {
		return nil, err
	}
	for i, v := range fixAt {
		sz := int64(units[i].Size) + v
		if sz < 0 {
			return nil, errors.New("derived size fixup underflows")
		}
		units[i].Size = uint64(sz)
	}
	return units, nil
}

// --- replay -------------------------------------------------------------

// nextKeptTable[i] is the first kept old unit at or after i, so the table's
// [oi+1] is "the one after oi" and [0] is where the walk starts.
func nextKeptTable(kept []bool) []int {
	out := make([]int, len(kept)+1)
	out[len(kept)] = len(kept)
	for i := len(kept) - 1; i >= 0; i-- {
		if kept[i] {
			out[i] = i
		} else {
			out[i] = out[i+1]
		}
	}
	return out
}

func alignUp(v, a uint64) uint64 { return v + (a-v%a)%a }

// sparseSigned reads a gap-coded index column paired with a signed value
// column into a map keyed by index.
func sparseSigned(index, value []byte, n int) (map[int]int64, error) {
	out := map[int]int64{}
	ir, vr := &planReader{b: index}, &planReader{b: value}
	prev, first := 0, true
	for !ir.done() {
		gap := int(ir.u())
		if ir.err != nil || (gap == 0 && !first) {
			return nil, errors.New("invalid sparse column index")
		}
		first = false
		ni := prev + gap
		if ni >= n {
			return nil, errors.New("invalid sparse column index")
		}
		out[ni] = vr.s()
		prev = ni
	}
	if ir.err != nil || !vr.done() {
		return nil, errors.New("invalid sparse column")
	}
	return out, nil
}

func appendGap(b []byte, i, prev int) []byte { return appendU(b, uint64(i-prev)) }

// reconstructMaps is the decoder's map: the positional walk over the
// enumeration, its exceptions, the layout replay, then whatever the unit
// model could not express. It never sees a symbol name or the new image.
func reconstructMaps(d *mapDelta, units []derivedUnit) ([]mapping, error) {
	// A new unit is either joined to an old one or shipped in InsertPos, so
	// the count the plan claims is bounded by the enumeration plus that
	// column: an untrusted plan cannot ask for an arbitrary allocation.
	n := int(d.NewUnits)
	if n < 0 || uint64(n) != d.NewUnits || d.NewUnits > uint64(len(units)+len(d.InsertPos)) {
		return nil, errors.New("implausible new-unit count")
	}
	// Drops.
	kept := make([]bool, len(units))
	dr := &planReader{b: d.DropRuns}
	for i := 0; i < len(units); {
		run := int(dr.u())
		if dr.err != nil || run < 0 || i+run > len(units) {
			return nil, errors.New("invalid drop run")
		}
		for k := 0; k < run; k++ {
			kept[i+k] = true
		}
		i += run
		gap := int(dr.u())
		if dr.err != nil || gap < 0 || i+gap > len(units) {
			return nil, errors.New("invalid drop run")
		}
		i += gap
	}
	if !dr.done() {
		return nil, errors.New("trailing drop-run data")
	}
	nextKept := nextKeptTable(kept)

	inserted := make([]bool, n)
	sizes := make([]uint64, n)
	ip, is := &planReader{b: d.InsertPos}, &planReader{b: d.InsertSize}
	prev, first := 0, true
	for !ip.done() {
		gap := int(ip.u())
		if ip.err != nil || (gap == 0 && !first) {
			return nil, errors.New("invalid insert position")
		}
		first = false
		ni := prev + gap
		if ni >= n {
			return nil, errors.New("invalid insert position")
		}
		inserted[ni] = true
		sizes[ni] = is.u()
		prev = ni
	}
	if ip.err != nil || !is.done() {
		return nil, errors.New("invalid insert streams")
	}

	orderAt, err := sparseSigned(d.OrderIndex, d.OrderSrc, n)
	if err != nil {
		return nil, err
	}
	sizeAt, err := sparseSigned(d.SizeIndex, d.SizeDelta, n)
	if err != nil {
		return nil, err
	}
	fixAt, err := sparseSigned(d.FixIndex, d.FixDelta, n)
	if err != nil {
		return nil, err
	}

	src := make([]int, n)
	cursor := nextKept[0]
	for ni := 0; ni < n; ni++ {
		if inserted[ni] {
			src[ni] = -1
			continue
		}
		oi := cursor
		if v, ok := orderAt[ni]; ok {
			oi = cursor + int(v)
		}
		if oi < 0 || oi >= len(units) || !kept[oi] {
			return nil, errors.New("order exception names a missing old unit")
		}
		src[ni] = oi
		cursor = nextKept[oi+1]
		sizes[ni] = units[oi].Size
		if v, ok := sizeAt[ni]; ok {
			s := int64(sizes[ni]) + v
			if s < 0 {
				return nil, errors.New("size delta underflows")
			}
			sizes[ni] = uint64(s)
		}
	}

	// Layout replay.
	offs := make([]uint64, n)
	var prevEnd uint64
	for ni := 0; ni < n; ni++ {
		off := int64(alignUp(prevEnd, d.Align))
		if v, ok := fixAt[ni]; ok {
			off += v
		}
		if off < 0 {
			return nil, errors.New("layout replay underflows")
		}
		offs[ni] = uint64(off)
		prevEnd = uint64(off) + sizes[ni]
	}

	// d.Maps is the count the plan claims; the reconstruction can only ever
	// produce one mapping per new unit plus one per unrepresentable record,
	// so that is what the buffer is sized for.
	out := make([]mapping, 0, min(d.Maps, uint64(n+len(d.Raw))))
	for ni := 0; ni < n; ni++ {
		if src[ni] < 0 {
			continue
		}
		o := units[src[ni]]
		out = append(out, mapping{Src: o.Off, SrcSize: o.Size, Dst: offs[ni], DstSize: sizes[ni]})
	}
	rr := &planReader{b: d.Raw}
	var prevRawDst uint64
	for !rr.done() {
		dst := prevRawDst + rr.u()
		dstSize := rr.u()
		s := rr.u()
		srcSize := rr.u()
		if rr.err != nil {
			return nil, errors.New("invalid unrepresentable mapping")
		}
		out = append(out, mapping{Src: s, SrcSize: srcSize, Dst: dst, DstSize: dstSize})
		prevRawDst = dst
	}
	if rr.err != nil {
		return nil, errors.New("invalid unrepresentable mapping")
	}
	slices.SortFunc(out, func(a, b mapping) int {
		if a.Dst != b.Dst {
			return cmpU(a.Dst, b.Dst)
		}
		return cmpU(a.Src, b.Src)
	})
	return out, nil
}

// --- the encoder --------------------------------------------------------

// derivedStats is what one built stream reports to the encode's notes.
type derivedStats struct {
	Enumerated, Units          int
	Dropped, Runs              int
	Reorders, Resizes, Inserts int
	Fixes, Align               int
	Boundary, Unrepresentable  int
}

// buildDerivedStream keys the map delta against enumeration units derived
// from the old window, and gates itself: the shipped map must come back
// exactly out of reconstructMaps.
func buildDerivedStream(derived []uint64, oldUnits, newUnits []codeUnit, maps []mapping, code []byte) (*derivedStream, []derivedUnit, derivedStats, error) {
	var st derivedStats
	st.Enumerated = len(derived)
	trueIdx := make(map[uint64]int, len(oldUnits))
	for i, u := range oldUnits {
		trueIdx[u.Off] = i
	}

	// Boundary exceptions: the true starts the enumeration missed, shipped as
	// address gaps. With suppression the enumeration keeps only its correct
	// entries, so the repaired list is exactly the true start list.
	inDerived := make(map[uint64]bool, len(derived))
	for _, off := range derived {
		inDerived[off] = true
	}
	s := &derivedStream{Derived: uint64(len(derived))}
	var prev uint64
	for _, u := range oldUnits {
		if inDerived[u.Off] {
			continue
		}
		s.Boundary = appendU(s.Boundary, u.Off-prev)
		prev = u.Off
		st.Boundary++
	}
	bits := make([]byte, (len(derived)+7)/8)
	for i, off := range derived {
		if _, ok := trueIdx[off]; ok {
			bits[i/8] |= 1 << (i % 8)
		}
	}
	s.Suppress = bits

	units, err := enumerationUnits(derived, s.Suppress, s.Boundary, code)
	if err != nil {
		return nil, nil, st, err
	}
	st.Units = len(units)
	eIdx := make(map[uint64]int, len(units))
	for i, u := range units {
		eIdx[u.Off] = i
	}
	newIdx := make(map[uint64]int, len(newUnits))
	for i, u := range newUnits {
		newIdx[u.Off] = i
	}

	// The shipped map's own answer, per new unit, in enumeration indices.
	joined := make([]int, len(newUnits))
	for i := range joined {
		joined[i] = -1
	}
	sorted := slices.Clone(maps)
	slices.SortFunc(sorted, func(a, b mapping) int {
		if a.Dst != b.Dst {
			return cmpU(a.Dst, b.Dst)
		}
		return cmpU(a.Src, b.Src)
	})
	var raw []byte
	var prevRawDst uint64
	for _, m := range sorted {
		ni, okN := newIdx[m.Dst]
		oi, okO := eIdx[m.Src]
		ti, okT := trueIdx[m.Src]
		if okN && okO && okT && joined[ni] < 0 &&
			m.DstSize == newUnits[ni].Size && m.SrcSize == oldUnits[ti].Size {
			joined[ni] = oi
			continue
		}
		raw = appendU(raw, m.Dst-prevRawDst)
		raw = appendU(raw, m.DstSize)
		raw = appendU(raw, m.Src)
		raw = appendU(raw, m.SrcSize)
		prevRawDst = m.Dst
		st.Unrepresentable++
	}

	// Size fixups, over the referenced entries only.
	referenced := make([]bool, len(units))
	for _, oi := range joined {
		if oi >= 0 {
			referenced[oi] = true
		}
	}
	prevFix := 0
	for i := range units {
		if !referenced[i] {
			continue
		}
		want := oldUnits[trueIdx[units[i].Off]].Size
		if want == units[i].Size {
			continue
		}
		s.SizeFixIdx = appendGap(s.SizeFixIdx, i, prevFix)
		s.SizeFixVal = appendS(s.SizeFixVal, int64(want)-int64(units[i].Size))
		units[i].Size = want
		prevFix = i
	}

	s.base = deltaFromJoin(units, newUnits, joined, raw, uint64(len(sorted)), &st)

	// The gate: replay the decoder and require the shipped map back exactly.
	got, err := reconstructMaps(s.base, units)
	if err != nil {
		return nil, nil, st, fmt.Errorf("derived reconstruction failed: %w", err)
	}
	if len(got) != len(sorted) {
		return nil, nil, st, fmt.Errorf("derived reconstruction produced %d mappings, want %d", len(got), len(sorted))
	}
	for i := range got {
		want := sorted[i]
		want.Copy = false
		if got[i] != want {
			return nil, nil, st, fmt.Errorf("derived reconstruction differs at mapping %d: %+v want %+v", i, got[i], want)
		}
	}
	return s, units, st, nil
}

// deltaFromJoin writes the correspondence as drop runs, a positional walk
// with its order exceptions, size deltas, inserts and a layout replay.
func deltaFromJoin(oldUnits []derivedUnit, newUnits []codeUnit, joined []int, raw []byte, nmaps uint64, st *derivedStats) *mapDelta {
	d := &mapDelta{NewUnits: uint64(len(newUnits)), Maps: nmaps, Raw: raw}

	kept := make([]bool, len(oldUnits))
	for _, oi := range joined {
		if oi >= 0 {
			kept[oi] = true
		}
	}
	for i := 0; i < len(oldUnits); {
		j := i
		for j < len(oldUnits) && kept[j] {
			j++
		}
		d.DropRuns = appendU(d.DropRuns, uint64(j-i))
		k := j
		for k < len(oldUnits) && !kept[k] {
			k++
		}
		d.DropRuns = appendU(d.DropRuns, uint64(k-j))
		st.Dropped += k - j
		st.Runs++
		i = k
	}

	nextKept := nextKeptTable(kept)
	cursor := nextKept[0]
	prevOrder, prevSize, prevInsert := 0, 0, 0
	for ni, u := range newUnits {
		oi := joined[ni]
		if oi < 0 {
			d.InsertPos = appendGap(d.InsertPos, ni, prevInsert)
			d.InsertSize = appendU(d.InsertSize, u.Size)
			prevInsert = ni
			st.Inserts++
			continue
		}
		if oi != cursor {
			d.OrderIndex = appendGap(d.OrderIndex, ni, prevOrder)
			d.OrderSrc = appendS(d.OrderSrc, int64(oi)-int64(cursor))
			prevOrder = ni
			st.Reorders++
		}
		cursor = nextKept[oi+1]
		if delta := int64(u.Size) - int64(oldUnits[oi].Size); delta != 0 {
			d.SizeIndex = appendGap(d.SizeIndex, ni, prevSize)
			d.SizeDelta = appendS(d.SizeDelta, delta)
			prevSize = ni
			st.Resizes++
		}
	}

	best, bestHits := uint64(16), -1
	for _, a := range []uint64{1, 2, 4, 8, 16, 32, 64} {
		hits, prevEnd := 0, uint64(0)
		for _, u := range newUnits {
			if u.Off == alignUp(prevEnd, a) {
				hits++
			}
			prevEnd = u.Off + u.Size
		}
		if hits > bestHits {
			best, bestHits = a, hits
		}
	}
	d.Align, st.Align = best, int(best)
	prevFix, prevEnd := 0, uint64(0)
	for ni, u := range newUnits {
		guess := alignUp(prevEnd, best)
		if u.Off != guess {
			d.FixIndex = appendGap(d.FixIndex, ni, prevFix)
			d.FixDelta = appendS(d.FixDelta, int64(u.Off)-int64(guess))
			prevFix = ni
			st.Fixes++
		}
		prevEnd = u.Off + u.Size
	}
	return d
}

// buildDerivedForm builds the derived-form structural plan for one window and
// checks it end to end: the stream is serialized, parsed back, and the unit
// list rebuilt from the old image alone must be the one the encoder keyed
// against. A pair the form cannot express returns ok=false and the caller
// ships the dense columns.
func buildDerivedForm(p predictionPlan, oldFile []byte, win section, oldUnits, newUnits []codeUnit) (*derivedStream, derivedStats, bool) {
	var st derivedStats
	if len(p.Maps) == 0 || len(oldUnits) == 0 || len(newUnits) == 0 {
		return nil, st, false
	}
	code := oldFile[win.Off : win.Off+win.Size]
	derived := cachedEnumeration(oldFile, win)
	if len(derived) == 0 {
		return nil, st, false
	}
	s, units, st, err := buildDerivedStream(derived, oldUnits, newUnits, p.Maps, code)
	if err != nil {
		return nil, st, false
	}
	// The determinism gate: rebuild the unit list the way the decoder will,
	// from the old image's bytes and the serialized stream alone.
	parsed, err := parseDerivedStream(&planReader{b: s.marshal()})
	if err != nil {
		return nil, st, false
	}
	back, err := derivedUnits(parsed, oldFile, win)
	if err != nil || len(back) != len(units) {
		return nil, st, false
	}
	for i := range back {
		if back[i] != units[i] {
			return nil, st, false
		}
	}
	return s, st, true
}

// readDerivedMaps is readDenseMaps with the enumeration derived rather than
// shipped. Its inputs are the plan and the old file, and nothing else.
func readDerivedMaps(r *planReader, p *predictionPlan, n uint64, oldFile []byte, win section) error {
	for i := 0; i < 5; i++ {
		if s := r.stream(); r.err == nil && len(s.b) != 0 {
			return errors.New("derived-form plan carries a map column")
		}
	}
	copyBits := r.stream()
	if r.err != nil || len(copyBits.b) != (int(n)+7)/8 {
		return errors.New("invalid mapping streams")
	}
	s, err := parseDerivedStream(r)
	if err != nil {
		return err
	}
	units, err := derivedUnits(s, oldFile, win)
	if err != nil {
		return err
	}
	maps, err := reconstructMaps(s.base, units)
	if err != nil {
		return err
	}
	if uint64(len(maps)) != n {
		return fmt.Errorf("derived reconstruction produced %d mappings, plan says %d", len(maps), n)
	}
	var prevDstEnd uint64
	for i := range maps {
		m := &maps[i]
		if m.Dst < prevDstEnd || m.Dst > p.TargetLen || m.DstSize > p.TargetLen-m.Dst ||
			m.Src > win.Size || m.SrcSize > win.Size-m.Src {
			return errors.New("reconstructed mapping lies outside its window")
		}
		m.Copy = copyBits.b[i/8]&(1<<(i%8)) != 0
		prevDstEnd = m.Dst + m.DstSize
	}
	p.Maps = maps
	return nil
}
