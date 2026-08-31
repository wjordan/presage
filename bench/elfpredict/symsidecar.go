package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"
)

// A real symbol sidecar, encoder and decoder.
//
// sidecar.go prices the hypothesis; this file implements it. The client is
// assumed to have installed, with the previous patch, a table describing the
// old binary's code units -- start, size, and 64-bit hashes of the symbol
// names that live there. The patch then ships the *delta* between that table
// and the new binary's, instead of the five columns of the function map.
//
// The decoder never joins by name: it cannot, because it has no new symbol
// table. The join is the encoder's hypothesis about how the two tables line
// up, and what crosses the wire is its residual -- which old units vanished,
// where the new order departs from the old one, which units changed size,
// which are new, and where the "next start is the previous end, aligned"
// layout guess is wrong. Everything the decoder does is replay.
//
// Correspondence exceptions are a second, independent layer: the shipped
// function map is not the identity join (renames, identical-code folds and
// the content matcher all disagree with it), so the stream also carries, per
// diverging unit, how far the map's source is from the join's.

// sidecarUnit is one row of the carried table.
type sidecarUnit struct {
	Off    uint64
	Size   uint64
	Hashes []uint64
}

// nameHash64 is FNV-1a. It is the join key the carried table stores in place
// of a mangled C++ name: deterministic, dependency-free, and identical on both
// sides of the wire.
func nameHash64(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func hashNamedUnits(units []namedUnit) []sidecarUnit {
	out := make([]sidecarUnit, len(units))
	for i, u := range units {
		hs := make([]uint64, len(u.Names))
		for j, n := range u.Names {
			hs[j] = nameHash64(n)
		}
		out[i] = sidecarUnit{Off: u.Off, Size: u.Size, Hashes: hs}
	}
	return out
}

// marshalSidecarTable serializes the carried table: per unit in address order,
// a varint address delta, a varint size, and the unit's name hashes. Most
// units carry exactly one; aliases and identical-code folds carry several, and
// dropping the extras would cost join fidelity the patch would then have to
// pay for in exceptions.
func marshalSidecarTable(units []sidecarUnit) []byte {
	b := binary.AppendUvarint(nil, uint64(len(units)))
	var prev uint64
	for _, u := range units {
		b = binary.AppendUvarint(b, u.Off-prev)
		b = binary.AppendUvarint(b, u.Size)
		b = binary.AppendUvarint(b, uint64(len(u.Hashes)))
		for _, h := range u.Hashes {
			b = binary.LittleEndian.AppendUint64(b, h)
		}
		prev = u.Off
	}
	return b
}

func unmarshalSidecarTable(b []byte) ([]sidecarUnit, error) {
	r := &planReader{b: b}
	n := r.u()
	if r.err != nil || n > uint64(len(b)) {
		return nil, errors.New("invalid sidecar table")
	}
	units := make([]sidecarUnit, 0, n)
	var prev uint64
	for i := uint64(0); i < n; i++ {
		off := prev + r.u()
		size := r.u()
		k := r.u()
		if r.err != nil || k > uint64(len(r.b)/8) {
			return nil, errors.New("invalid sidecar table entry")
		}
		hs := make([]uint64, k)
		for j := range hs {
			hs[j] = binary.LittleEndian.Uint64(r.b)
			r.b = r.b[8:]
		}
		units = append(units, sidecarUnit{Off: off, Size: size, Hashes: hs})
		prev = off
	}
	if r.err != nil || len(r.b) != 0 {
		return nil, errors.New("trailing sidecar table data")
	}
	return units, nil
}

// sidecarHashJoin is sidecar.go's name join over hashes. Duplicate keys are
// disambiguated by relative order; a unit carrying several names votes with
// each of them and the majority wins, ties to the lowest old index.
func sidecarHashJoin(oldUnits, newUnits []sidecarUnit) []int {
	oldSeq := make(map[uint64][]int, len(oldUnits))
	for i, u := range oldUnits {
		for _, h := range u.Hashes {
			oldSeq[h] = append(oldSeq[h], i)
		}
	}
	newSeq := make(map[uint64][]int, len(newUnits))
	for i, u := range newUnits {
		for _, h := range u.Hashes {
			newSeq[h] = append(newSeq[h], i)
		}
	}
	joined := make([]int, len(newUnits))
	votes := map[int]int{}
	for ni, u := range newUnits {
		clear(votes)
		for _, h := range u.Hashes {
			pos := slices.Index(newSeq[h], ni)
			if os := oldSeq[h]; pos >= 0 && pos < len(os) {
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
	return joined
}

// hashCollisions counts distinct names that share a 64-bit hash. A collision
// is not a correctness hazard here -- the encoder joins on hashes too, so both
// sides agree on whatever the collision produced, and any resulting wrong
// correspondence is caught and shipped as an exception -- but it is the
// quantity a hashed-name design has to be sure stays near zero.
func hashCollisions(units ...[]namedUnit) (colliding int) {
	byHash := map[uint64]string{}
	for _, us := range units {
		for _, u := range us {
			for _, n := range u.Names {
				if prev, ok := byHash[nameHash64(n)]; ok {
					if prev != n {
						colliding++
					}
					continue
				}
				byHash[nameHash64(n)] = n
			}
		}
	}
	return colliding
}

// --- the delta stream -------------------------------------------------

// sidecarDelta is the replacement for the map's five columns. Every field is a
// separate byte column so that the plan's compressor sees like next to like.
type sidecarDelta struct {
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
	InsertHash []byte // their name hashes, so the table can be rolled forward
	FixIndex   []byte // new-unit gaps where the aligned layout guess is wrong
	FixDelta   []byte
	ExcIndex   []byte // new-unit gaps where the map disagrees with the join
	ExcSrc     []byte // map source minus join source; -1 means "not mapped"
	Raw        []byte // mappings the unit model cannot express at all
}

func (d *sidecarDelta) columns() []struct {
	name string
	b    *[]byte
} {
	return []struct {
		name string
		b    *[]byte
	}{
		{"drop runs", &d.DropRuns},
		{"order-exception index", &d.OrderIndex},
		{"order-exception source", &d.OrderSrc},
		{"size-delta index", &d.SizeIndex},
		{"size-delta value", &d.SizeDelta},
		{"insert position", &d.InsertPos},
		{"insert size", &d.InsertSize},
		{"insert name hashes", &d.InsertHash},
		{"layout-fixup index", &d.FixIndex},
		{"layout-fixup value", &d.FixDelta},
		{"correspondence exception index", &d.ExcIndex},
		{"correspondence exception source", &d.ExcSrc},
		{"unrepresentable mappings", &d.Raw},
	}
}

func (d *sidecarDelta) marshal() []byte {
	b := binary.AppendUvarint(nil, d.NewUnits)
	b = binary.AppendUvarint(b, d.Align)
	b = binary.AppendUvarint(b, d.Maps)
	for _, c := range d.columns() {
		b = appendStream(b, *c.b)
	}
	return b
}

func parseSidecarDelta(r *planReader) (*sidecarDelta, error) {
	d := &sidecarDelta{NewUnits: r.u(), Align: r.u(), Maps: r.u()}
	if r.err != nil || d.Align == 0 {
		return nil, errors.New("invalid sidecar delta header")
	}
	for _, c := range d.columns() {
		*c.b = r.stream().b
	}
	if r.err != nil {
		return nil, errors.New("invalid sidecar delta streams")
	}
	return d, nil
}

// sidecarStats is what the measurement prints about a built stream.
type sidecarStats struct {
	OldUnits, NewUnits          int
	Dropped, Runs               int
	Reorders, Resizes, Inserts  int
	Fixes, Align                int
	Exceptions, Unrepresentable int
	Collisions                  int
	JoinAgreed, JoinDisagreed   int
	TableRawBytes, TableXZ      int
}

// buildSidecarDelta produces the stream and, as its own gate, replays the
// decoder against it and checks the reconstruction is the shipped map exactly.
func buildSidecarDelta(oldNamed, newNamed []namedUnit, maps []mapping) (*sidecarDelta, []sidecarUnit, sidecarStats, error) {
	var st sidecarStats
	oldUnits, newUnits := hashNamedUnits(oldNamed), hashNamedUnits(newNamed)
	st.OldUnits, st.NewUnits = len(oldUnits), len(newUnits)
	st.Collisions = hashCollisions(oldNamed, newNamed)
	joined := sidecarHashJoin(oldUnits, newUnits)

	oldIdx := make(map[uint64]int, len(oldUnits))
	for i, u := range oldUnits {
		oldIdx[u.Off] = i
	}
	newIdx := make(map[uint64]int, len(newUnits))
	for i, u := range newUnits {
		newIdx[u.Off] = i
	}

	// The map's own answer, per new unit, and the mappings the unit model
	// cannot express -- a destination that is not a unit start, a source that
	// is not an old unit, or an extent that is not the unit's. There are none
	// on this corpus; the column exists so that "reconstruction is exact" is a
	// property of the format rather than of the inputs.
	mapSrc := make([]int, len(newUnits))
	for i := range mapSrc {
		mapSrc[i] = -1
	}
	var raw []byte
	var prevRawDst uint64
	rawCount := 0
	sorted := slices.Clone(maps)
	slices.SortFunc(sorted, func(a, b mapping) int {
		if a.Dst != b.Dst {
			return cmpU(a.Dst, b.Dst)
		}
		return cmpU(a.Src, b.Src)
	})
	for _, m := range sorted {
		ni, okN := newIdx[m.Dst]
		oi, okO := oldIdx[m.Src]
		if okN && okO && mapSrc[ni] < 0 &&
			m.DstSize == newUnits[ni].Size && m.SrcSize == oldUnits[oi].Size {
			mapSrc[ni] = oi
			continue
		}
		raw = binary.AppendUvarint(raw, m.Dst-prevRawDst)
		raw = binary.AppendUvarint(raw, m.DstSize)
		raw = binary.AppendUvarint(raw, m.Src)
		raw = binary.AppendUvarint(raw, m.SrcSize)
		prevRawDst = m.Dst
		rawCount++
	}
	st.Unrepresentable = rawCount

	d := &sidecarDelta{NewUnits: uint64(len(newUnits)), Maps: uint64(len(sorted)), Raw: raw}

	// Drops first: the decoder needs to know which old units survive before
	// "the next old unit" means anything, and that is what leaves the kept
	// sequence implicit.
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
		d.DropRuns = binary.AppendUvarint(d.DropRuns, uint64(j-i))
		k := j
		for k < len(oldUnits) && !kept[k] {
			k++
		}
		d.DropRuns = binary.AppendUvarint(d.DropRuns, uint64(k-j))
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
			d.InsertSize = binary.AppendUvarint(d.InsertSize, u.Size)
			// One hash, not the unit's whole alias set. A hash is eight
			// incompressible bytes and identical-code folding gives an inserted
			// unit 16.8 names on average, so shipping them all costs 190,184
			// compressed bytes against 11,248 -- more than the entire rest of
			// the stream. The price is paid by the *next* patch, whose join
			// sees one key where the encoder saw seventeen.
			var h uint64
			if len(u.Hashes) > 0 {
				h = u.Hashes[0]
			}
			d.InsertHash = binary.LittleEndian.AppendUint64(d.InsertHash, h)
			prevInsert = ni
			st.Inserts++
			continue
		}
		if oi != cursor {
			d.OrderIndex = appendGap(d.OrderIndex, ni, prevOrder)
			d.OrderSrc = binary.AppendVarint(d.OrderSrc, int64(oi)-int64(cursor))
			prevOrder = ni
			st.Reorders++
		}
		cursor = nextKept[oi+1]
		if delta := int64(u.Size) - int64(oldUnits[oi].Size); delta != 0 {
			d.SizeIndex = appendGap(d.SizeIndex, ni, prevSize)
			d.SizeDelta = binary.AppendVarint(d.SizeDelta, delta)
			prevSize = ni
			st.Resizes++
		}
	}

	// Sizes are known for every new unit now, so a start is the previous end
	// rounded up. Which alignment is measured, not assumed.
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
			d.FixDelta = binary.AppendVarint(d.FixDelta, int64(u.Off)-int64(guess))
			prevFix = ni
			st.Fixes++
		}
		prevEnd = u.Off + u.Size
	}

	prevExc := 0
	for ni := range newUnits {
		s, j := mapSrc[ni], joined[ni]
		if s == j {
			if s >= 0 {
				st.JoinAgreed++
			}
			continue
		}
		if s >= 0 && j >= 0 {
			st.JoinDisagreed++
		}
		d.ExcIndex = appendGap(d.ExcIndex, ni, prevExc)
		// Both sides know the join's answer, so the exception says how far the
		// truth is from it rather than naming an old index outright. -1 on
		// either side is "no mapping", and the difference carries it.
		d.ExcSrc = binary.AppendVarint(d.ExcSrc, int64(s)-int64(j))
		prevExc = ni
		st.Exceptions++
	}

	// The gate: run the decoder's own reconstruction and require the shipped
	// map back, field for field, in order.
	got, err := reconstructMaps(d, oldUnits)
	if err != nil {
		return nil, nil, st, fmt.Errorf("sidecar reconstruction failed: %w", err)
	}
	if len(got) != len(sorted) {
		return nil, nil, st, fmt.Errorf("sidecar reconstruction produced %d mappings, want %d", len(got), len(sorted))
	}
	for i := range got {
		// Copy is carried by the plan's own bitmap, not by this stream.
		want := sorted[i]
		want.Copy = false
		if got[i] != want {
			return nil, nil, st, fmt.Errorf("sidecar reconstruction differs at mapping %d: %+v want %+v", i, got[i], want)
		}
	}
	return d, oldUnits, st, nil
}

// nextKeptTable[i] is the first kept old unit at or after i, so nextKept[oi+1]
// is "the one after oi" and nextKept[0] is where the walk starts.
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

// reconstructMaps is the decoder. Its only inputs are the carried table and
// the delta stream; it never sees a symbol name or the new binary.
func reconstructMaps(d *sidecarDelta, oldUnits []sidecarUnit) ([]mapping, error) {
	n := int(d.NewUnits)
	if n < 0 || uint64(n) != d.NewUnits {
		return nil, errors.New("implausible new-unit count")
	}
	// Drops.
	kept := make([]bool, len(oldUnits))
	dr := &planReader{b: d.DropRuns}
	for i := 0; i < len(oldUnits); {
		run := int(dr.u())
		if dr.err != nil || run < 0 || i+run > len(oldUnits) {
			return nil, errors.New("invalid drop run")
		}
		for k := 0; k < run; k++ {
			kept[i+k] = true
		}
		i += run
		gap := int(dr.u())
		if dr.err != nil || gap < 0 || i+gap > len(oldUnits) {
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
	ih := &planReader{b: d.InsertHash}
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
		if len(ih.b) < 8 {
			return nil, errors.New("invalid insert name hashes")
		}
		// The hash is not used to rebuild the map; it is what lets the client
		// roll its carried table forward for the next patch.
		ih.b = ih.b[8:]
		prev = ni
	}
	if ip.err != nil || !is.done() || !ih.done() {
		return nil, errors.New("invalid insert streams")
	}

	// Order exceptions and size deltas, both read as sparse index/value pairs
	// keyed by new-unit position.
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
	excAt, err := sparseSigned(d.ExcIndex, d.ExcSrc, n)
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
		if oi < 0 || oi >= len(oldUnits) || !kept[oi] {
			return nil, errors.New("sidecar order exception names a missing old unit")
		}
		src[ni] = oi
		cursor = nextKept[oi+1]
		sizes[ni] = oldUnits[oi].Size
		if v, ok := sizeAt[ni]; ok {
			s := int64(sizes[ni]) + v
			if s < 0 {
				return nil, errors.New("sidecar size delta underflows")
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
			return nil, errors.New("sidecar layout replay underflows")
		}
		offs[ni] = uint64(off)
		prevEnd = uint64(off) + sizes[ni]
	}

	// Correspondence exceptions.
	for ni, v := range excAt {
		s := src[ni] + int(v)
		if s < -1 || s >= len(oldUnits) {
			return nil, errors.New("sidecar correspondence exception out of range")
		}
		src[ni] = s
	}

	out := make([]mapping, 0, d.Maps)
	for ni := 0; ni < n; ni++ {
		if src[ni] < 0 {
			continue
		}
		o := oldUnits[src[ni]]
		out = append(out, mapping{Src: o.Off, SrcSize: o.Size, Dst: offs[ni], DstSize: sizes[ni]})
	}
	// Anything the unit model could not express.
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

// --- the sidecar-form structural plan ---------------------------------

// decoderSidecar is the carried table, as the decoder holds it. Set from the
// sidecar file; a sidecar-form plan cannot be decoded without it, which is the
// point of the mode.
var decoderSidecar []sidecarUnit

// marshalSidecarForm is marshal with the five map columns emptied and the
// symbol-table delta put where they were. Everything else -- the copy bitmap,
// the reference points, the ranges -- is byte-for-byte what the normal plan
// emits, because the decoder reconstructs the same map and so derives the same
// reference-target basis.
func (p predictionPlan) marshalSidecarForm(oldText []byte, d *sidecarDelta) ([]byte, error) {
	b, err := p.marshalBlanked(oldText, true, false)
	if err != nil {
		return nil, err
	}
	// The blanked marshal is the normal stream with the five columns empty;
	// splice the mode byte and insert the delta after the copy bitmap.
	//
	// Rather than re-derive the offsets, re-run the header here: the layout is
	// magic, three varints, mode, count, six streams, then points and ranges.
	r := &planReader{b: b[len(planMagic):]}
	r.u()
	r.u()
	r.u()
	modeAt := len(b) - len(r.b)
	r.byteAt()
	r.u()
	for i := 0; i < 6; i++ {
		r.stream()
	}
	if r.err != nil {
		return nil, errors.New("cannot locate the map columns in the blanked plan")
	}
	cut := len(b) - len(r.b)
	out := make([]byte, 0, len(b)+64)
	out = append(out, b[:modeAt]...)
	out = append(out, byte(planSidecar))
	out = append(out, b[modeAt+1:cut]...)
	out = append(out, d.marshal()...)
	out = append(out, b[cut:]...)
	return out, nil
}

// readSidecarMaps is readDenseMaps' counterpart: the five columns are empty
// and the map comes from the carried table plus the delta stream.
func readSidecarMaps(r *planReader, p *predictionPlan, n uint64) error {
	for i := 0; i < 5; i++ {
		if s := r.stream(); r.err == nil && len(s.b) != 0 {
			return errors.New("sidecar-form plan carries a map column")
		}
	}
	copyBits := r.stream()
	if r.err != nil || len(copyBits.b) != (int(n)+7)/8 {
		return errors.New("invalid mapping streams")
	}
	d, err := parseSidecarDelta(r)
	if err != nil {
		return err
	}
	if decoderSidecar == nil {
		return errors.New("sidecar-form plan without a carried symbol table")
	}
	maps, err := reconstructMaps(d, decoderSidecar)
	if err != nil {
		return err
	}
	if uint64(len(maps)) != n {
		return fmt.Errorf("sidecar reconstruction produced %d mappings, plan says %d", len(maps), n)
	}
	var prevDstEnd uint64
	for i := range maps {
		m := &maps[i]
		if m.Dst < prevDstEnd || m.Dst > p.TargetLen || m.DstSize > p.TargetLen-m.Dst {
			return errors.New("reconstructed mapping destination exceeds prediction")
		}
		m.Copy = copyBits.b[i/8]&(1<<(i%8)) != 0
		prevDstEnd = m.Dst + m.DstSize
	}
	p.Maps = maps
	return nil
}

// --- the measurement ---------------------------------------------------

// buildSidecarRung turns the corrected-fields plan into its sidecar-form twin:
// the same plan in every stream but the structural one, whose map columns are
// replaced by the symbol-table delta. It writes the carried table to the
// artifact directory and reports what it costs, which is the client's carried
// cost and not a patch byte.
func buildSidecarRung(correctedPlan []byte, oldImage, newImage *image, structure predictionPlan, outDir string) ([]byte, error) {
	if oldDebugPath == "" || newDebugPath == "" {
		return nil, errors.New("the sidecar rung needs -old-debug and -new-debug")
	}
	oldText := oldImage.textBytes()
	t := startStage("sidecar symbols")
	oldNamed, err := loadNamedUnits(oldDebugPath, oldImage.Text)
	if err != nil {
		return nil, err
	}
	newNamed, err := loadNamedUnits(newDebugPath, newImage.Text)
	if err != nil {
		return nil, err
	}
	t.done("old %d units, new %d units", len(oldNamed), len(newNamed))

	t = startStage("sidecar delta")
	d, oldUnits, st, err := buildSidecarDelta(oldNamed, newNamed, structure.Maps)
	if err != nil {
		return nil, err
	}
	table := marshalSidecarTable(oldUnits)
	st.TableRawBytes = len(table)
	t.done("%d exceptions, %d unrepresentable", st.Exceptions, st.Unrepresentable)

	// The decoder holds it; loading it here is what makes the rung's decode
	// path real rather than a simulation.
	decoderSidecar, err = unmarshalSidecarTable(table)
	if err != nil {
		return nil, err
	}
	if err := writeFile(outDir, "symbol-sidecar.bin", table); err != nil {
		return nil, err
	}

	cp, err := unmarshalCombinedPlan(correctedPlan)
	if err != nil {
		return nil, err
	}
	normalStructure := cp.Structure
	sidecarStructure, err := structure.marshalSidecarForm(oldText, d)
	if err != nil {
		return nil, err
	}
	cp.Structure = sidecarStructure
	out := cp.marshal()

	t = startStage("sidecar accounting")
	st.TableXZ = xzSizeContiguous(table)
	reportSidecar(st, normalStructure, sidecarStructure, correctedPlan, out, d, structure, oldText)
	t.done("")
	return out, nil
}

func reportSidecar(st sidecarStats, normalStructure, sidecarStructure, normalPlan, sidecarPlan []byte, d *sidecarDelta, structure predictionPlan, oldText []byte) {
	f := os.Stderr
	fmt.Fprintf(f, "sidecar: carried table %d units, raw %d B, xz %d (installed cost, not patch bytes)\n",
		st.OldUnits, st.TableRawBytes, st.TableXZ)
	fmt.Fprintf(f, "sidecar: %d old units, %d dropped in %d runs; %d reorders, %d resizes, %d inserts, %d layout fixups at align %d\n",
		st.OldUnits, st.Dropped, st.Runs, st.Reorders, st.Resizes, st.Inserts, st.Fixes, st.Align)
	fmt.Fprintf(f, "sidecar: hash join agreed with the map on %d units, disagreed on %d; %d correspondence exceptions, %d unrepresentable mappings, %d distinct-name hash collisions\n",
		st.JoinAgreed, st.JoinDisagreed, st.Exceptions, st.Unrepresentable, st.Collisions)

	// The delta columns as compressed in place: each priced by emptying it
	// inside the whole plan, which is the only number the headline is made of.
	full := xzSizeContiguous(sidecarPlan)
	cols := d.columns()
	blanks := make([][]byte, len(cols))
	for i := range cols {
		saved := *cols[i].b
		*cols[i].b = nil
		b, err := structure.marshalSidecarForm(oldText, d)
		*cols[i].b = saved
		if err != nil {
			continue
		}
		cp, err := unmarshalCombinedPlan(sidecarPlan)
		if err != nil {
			continue
		}
		cp.Structure = b
		blanks[i] = cp.marshal()
	}
	sizes := make([]int, len(cols))
	for i, b := range blanks {
		if b != nil {
			sizes[i] = full - xzSizeContiguous(b)
		}
	}
	fmt.Fprintf(f, "  %-34s %10s %10s\n", "delta column", "raw", "xz in place")
	sum := 0
	for i, c := range cols {
		sum += sizes[i]
		fmt.Fprintf(f, "  %-34s %10d %10d\n", c.name, len(*c.b), sizes[i])
	}
	fmt.Fprintf(f, "  %-34s %10s %10d\n", "sum of columns", "", sum)
	fmt.Fprintf(f, "sidecar: structure stream %d -> %d raw, %d -> %d xz standalone; whole plan %d -> %d xz\n",
		len(normalStructure), len(sidecarStructure),
		xzSizeContiguous(normalStructure), xzSizeContiguous(sidecarStructure),
		xzSizeContiguous(normalPlan), full)
}
