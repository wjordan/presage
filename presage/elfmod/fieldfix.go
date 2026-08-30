package elfmod

import (
	"encoding/binary"
	"errors"
	"runtime"
	"slices"

	"github.com/wjordan/presage/delta/x86"
)

// The correction codec treats .text as bytes, but a quarter of what it has to
// fix there is not bytes -- it is addresses. Of the 12,714,476 relocated
// fields the retargeter writes, 164,998 hold the wrong displacement, and
// shipping four literal bytes for each of them ships the answer where the
// decoder only needed the difference.
//
// Both layers here name a field by its position in a list the decoder builds
// for itself: it holds the prediction, and walking the prediction's
// instructions is what produced the fields in the first place.
//
//   - The address remap works on what the field means. A field holds a
//     displacement to some address, so a wrong field means the oracle returned
//     the wrong address; where several fields chose the same wrong address,
//     one entry fixes all of them. Each candidate is scored against the target
//     and kept only if it fixes more fields than it breaks -- a wrong address
//     can be the right answer somewhere else.
//   - The field delta works on what the field holds, one field at a time, and
//     picks up whatever the remap could not group.

type fieldSite struct {
	off  int // field position within .text
	next int // position of the instruction's end, which the displacement is from
}

// fieldSites lists the four-byte displacement fields of a predicted .text, in
// the order the retargeter walked them. Instruction lengths do not depend on
// the displacement values, so this list is the same before and after either
// layer rewrites a field.
func fieldSites(text []byte, maps []mapping) []fieldSite {
	keep := func(out []fieldSite, refs []x86.Reference, base int) []fieldSite {
		for _, ref := range refs {
			if ref.N == 4 && base+ref.Off+4 <= len(text) {
				out = append(out, fieldSite{base + ref.Off, base + ref.Next})
			}
		}
		return out
	}
	if len(maps) == 0 {
		return keep(nil, x86.References(text, 0), 0)
	}
	// One body per mapping, walked concurrently. WalkBodies returns the
	// per-body references in map order, so concatenating them reproduces the
	// exact site list -- and thus the exact plan basis -- a serial walk built.
	bodies := make([]x86.Body, 0, len(maps))
	bases := make([]int, 0, len(maps))
	for _, m := range maps {
		if m.Dst > uint64(len(text)) || m.DstSize > uint64(len(text))-m.Dst {
			continue
		}
		bodies = append(bodies, x86.Body{Code: text[m.Dst : m.Dst+m.DstSize]})
		bases = append(bases, int(m.Dst))
	}
	res := x86.WalkBodies(bodies, runtime.GOMAXPROCS(0))
	var out []fieldSite
	for k, refs := range res {
		out = keep(out, refs, bases[k])
	}
	return out
}

func (s fieldSite) addr(text []byte, textAddr uint64) uint64 {
	disp := int64(int32(binary.LittleEndian.Uint32(text[s.off:])))
	return uint64(int64(textAddr) + int64(s.next) + disp)
}

func (s fieldSite) put(text []byte, textAddr, target uint64) {
	binary.LittleEndian.PutUint32(text[s.off:], uint32(int32(int64(target)-int64(textAddr)-int64(s.next))))
}

// fieldPlan is the serialized form of both layers.
type fieldPlan struct {
	RemapIndex, RemapShift []byte
	FieldIndex, FieldDelta []byte
}

func (p fieldPlan) marshal() []byte {
	var b []byte
	b = appendStream(b, p.RemapIndex)
	b = appendStream(b, p.RemapShift)
	b = appendStream(b, p.FieldIndex)
	b = appendStream(b, p.FieldDelta)
	return b
}

func unmarshalFieldPlan(b []byte) (fieldPlan, error) {
	r := &planReader{b: b}
	ri, rs, fi, fd := r.stream(), r.stream(), r.stream(), r.stream()
	if r.err != nil || len(r.b) != 0 {
		return fieldPlan{}, errors.New("invalid field plan")
	}
	return fieldPlan{RemapIndex: ri.b, RemapShift: rs.b, FieldIndex: fi.b, FieldDelta: fd.b}, nil
}

type fieldStats struct {
	Sites   int
	Remaps  int
	Remade  int
	Deltas  int
	Domain  int
	Skipped int
}

// remapDomain is the sorted list of distinct addresses the prediction points
// at. The decoder can build it because it holds the prediction.
func remapDomain(text []byte, textAddr uint64, sites []fieldSite) []uint64 {
	out := make([]uint64, len(sites))
	for i, s := range sites {
		out[i] = s.addr(text, textAddr)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// encodeFieldFix builds both layers. text is the prediction's .text and is
// left untouched; applyFieldFix reproduces the result from the same inputs.
func encodeFieldFix(text, want []byte, textAddr uint64, maps []mapping) (fieldPlan, fieldStats) {
	sites := fieldSites(text, maps)
	st := fieldStats{Sites: len(sites)}
	domain := remapDomain(text, textAddr, sites)
	st.Domain = len(domain)

	// Only addresses some wrong field chose can be worth remapping, so group
	// just those. Grouping all 12.7 million would cost more memory than the
	// image.
	wrongAddr := make(map[uint64]bool)
	for _, s := range sites {
		if !equal4(text[s.off:], want[s.off:]) {
			wrongAddr[s.addr(text, textAddr)] = true
		}
	}
	type group struct {
		sites []int
	}
	groups := make(map[uint64]*group, len(wrongAddr))
	for i, s := range sites {
		a := s.addr(text, textAddr)
		if !wrongAddr[a] {
			continue
		}
		g := groups[a]
		if g == nil {
			g = &group{}
			groups[a] = g
		}
		g.sites = append(g.sites, i)
	}

	// Score every candidate destination for each group and keep the best, if
	// it is a net gain. A field that is already right votes against changing
	// the address it chose.
	chosen := make(map[uint64]uint64, len(groups))
	votes := make(map[uint64]int)
	for from, g := range groups {
		clear(votes)
		correct := 0
		for _, i := range g.sites {
			s := sites[i]
			if equal4(text[s.off:], want[s.off:]) {
				correct++
				continue
			}
			disp := int64(int32(binary.LittleEndian.Uint32(want[s.off:])))
			votes[uint64(int64(textAddr)+int64(s.next)+disp)]++
		}
		bestTo, best := uint64(0), 0
		for to, n := range votes {
			if n > best || (n == best && to < bestTo) {
				bestTo, best = to, n
			}
		}
		if best-correct <= 0 {
			st.Skipped++
			continue
		}
		chosen[from] = bestTo
	}

	var p fieldPlan
	prevIdx, prevShift := 0, int64(0)
	for _, from := range slices.Sorted(mapKeys(chosen)) {
		i, ok := slices.BinarySearch(domain, from)
		if !ok {
			continue
		}
		shift := int64(chosen[from]) - int64(from)
		p.RemapIndex = appendS(p.RemapIndex, int64(i-prevIdx))
		p.RemapShift = appendS(p.RemapShift, shift-prevShift)
		prevIdx, prevShift = i, shift
		st.Remaps++
	}

	// Apply the remaps to a scratch copy so the per-field layer corrects what
	// is actually left, exactly as the decoder will see it.
	fixed := append([]byte(nil), text...)
	for _, s := range sites {
		if to, ok := chosen[s.addr(text, textAddr)]; ok {
			s.put(fixed, textAddr, to)
			st.Remade++
		}
	}
	prev := 0
	for i, s := range sites {
		if equal4(fixed[s.off:], want[s.off:]) {
			continue
		}
		d := int64(int32(binary.LittleEndian.Uint32(want[s.off:]))) - int64(int32(binary.LittleEndian.Uint32(fixed[s.off:])))
		p.FieldIndex = appendU(p.FieldIndex, uint64(i-prev))
		p.FieldDelta = appendS(p.FieldDelta, d)
		prev = i
		st.Deltas++
	}
	return p, st
}

// applyFieldFix replays both layers over a predicted .text.
func applyFieldFix(text []byte, textAddr uint64, maps []mapping, b []byte) (fieldStats, error) {
	p, err := unmarshalFieldPlan(b)
	if err != nil {
		return fieldStats{}, err
	}
	sites := fieldSites(text, maps)
	st := fieldStats{Sites: len(sites)}
	domain := remapDomain(text, textAddr, sites)
	st.Domain = len(domain)

	idxr, shr := &planReader{b: p.RemapIndex}, &planReader{b: p.RemapShift}
	remap := map[uint64]uint64{}
	idx, shift := 0, int64(0)
	for !idxr.done() {
		idx += int(idxr.s())
		shift += shr.s()
		if idxr.err != nil || shr.err != nil || idx < 0 || idx >= len(domain) {
			return st, errors.New("invalid address remap entry")
		}
		// Addresses wrap: this is a position-independent image, so a
		// displacement can point below zero and both sides must agree to let
		// it. Nothing here indexes memory by the result -- put truncates it to
		// the field's four bytes -- so wrapping is safe as well as necessary.
		remap[domain[idx]] = uint64(int64(domain[idx]) + shift)
		st.Remaps++
	}
	if !shr.done() {
		return st, errors.New("trailing address remap data")
	}
	for _, s := range sites {
		if to, ok := remap[s.addr(text, textAddr)]; ok {
			s.put(text, textAddr, to)
			st.Remade++
		}
	}

	fir, fdr := &planReader{b: p.FieldIndex}, &planReader{b: p.FieldDelta}
	at := 0
	first := true
	for !fir.done() {
		gap := fir.u()
		d := fdr.s()
		if fir.err != nil || fdr.err != nil || gap > uint64(len(sites)) {
			return st, errors.New("invalid field delta entry")
		}
		if !first && gap == 0 {
			return st, errors.New("field delta repeats a field")
		}
		at += int(gap)
		if at >= len(sites) {
			return st, errors.New("field delta runs past the field list")
		}
		s := sites[at]
		v := int64(int32(binary.LittleEndian.Uint32(text[s.off:]))) + d
		binary.LittleEndian.PutUint32(text[s.off:], uint32(int32(v)))
		st.Deltas++
		first = false
	}
	if !fdr.done() {
		return st, errors.New("trailing field delta data")
	}
	return st, nil
}

func equal4(a, b []byte) bool {
	return len(a) >= 4 && len(b) >= 4 && a[0] == b[0] && a[1] == b[1] && a[2] == b[2] && a[3] == b[3]
}

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
