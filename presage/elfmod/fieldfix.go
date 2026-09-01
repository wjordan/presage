package elfmod

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"slices"

	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/internal/cz"
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

// fieldSite packs a field position and its distance to the instruction end
// into one word. x86 instructions are at most 15 bytes, so the low byte is
// ample for the distance and the remaining 56 bits cover any practical text
// section. The old two-int representation cost 16 bytes per site.
type fieldSite uint64

func makeFieldSite(off, next int) fieldSite {
	return fieldSite(uint64(off)<<8 | uint64(next-off))
}

func (s fieldSite) off() int  { return int(uint64(s) >> 8) }
func (s fieldSite) next() int { return s.off() + int(uint64(s)&0xff) }

// fieldSites lists the four-byte displacement fields of a predicted .text, in
// the order the retargeter walked them. Instruction lengths do not depend on
// the displacement values, so this list is the same before and after either
// layer rewrites a field.
func fieldSites(text []byte, maps []mapping) []fieldSite {
	keep := func(out []fieldSite, refs []x86.Reference, base int) []fieldSite {
		for _, ref := range refs {
			if ref.N == 4 && base+ref.Off+4 <= len(text) {
				out = append(out, makeFieldSite(base+ref.Off, base+ref.Next))
			}
		}
		return out
	}
	if len(maps) == 0 {
		return keep(nil, x86.References(text, 0), 0)
	}
	// Project each mapping into a body only when its worker reaches it, avoiding
	// a large duplicate body table. The shards stay in map order, so joining
	// them reproduces the exact site list -- and thus the exact plan basis -- a
	// serial walk built.
	bodyAt := func(k int) x86.Body {
		m := maps[k]
		if m.Dst > uint64(len(text)) || m.DstSize > uint64(len(text))-m.Dst {
			return x86.Body{}
		}
		return x86.Body{Code: text[m.Dst : m.Dst+m.DstSize], PC: m.Dst}
	}
	shards := x86.CollectReferences(len(maps), runtime.GOMAXPROCS(0), bodyAt, func(_ int, body x86.Body, ref x86.Reference) (fieldSite, bool) {
		base := int(body.PC)
		if ref.N != 4 || base+ref.Off+4 > len(text) {
			return 0, false
		}
		return makeFieldSite(base+ref.Off, base+ref.Next), true
	})
	n := 0
	for _, p := range shards {
		n += len(p)
	}
	out := make([]fieldSite, 0, n)
	for _, p := range shards {
		out = append(out, p...)
	}
	return out
}

func (s fieldSite) addr(text []byte, textAddr uint64) uint64 {
	disp := int64(int32(binary.LittleEndian.Uint32(text[s.off():])))
	return uint64(int64(textAddr) + int64(s.next()) + disp)
}

func (s fieldSite) put(text []byte, textAddr, target uint64) {
	binary.LittleEndian.PutUint32(text[s.off():], uint32(int32(int64(target)-int64(textAddr)-int64(s.next()))))
}

// The basis the remap layer states a new target on.
//
// remapShiftBasis says how far the address moved, in bytes. But an address the
// oracle got wrong is almost never corrected to an arbitrary place: 45 % of the
// time the right answer is an address the prediction already points at or a
// function start the map placed, and that set -- remapTargets -- is three
// orders of magnitude denser than the address space it indexes.
// remapIndexBasis names the target by its index in that set where it is in it,
// escapes to the shift where it is not, and carries one bit per entry to say
// which. Measured on the plan's own column, cz-compressed: Chrome 101,803 ->
// 88,733, libxul 100,946 -> 89,040. The same question asked of the per-field
// delta column answers no (Chrome +821, libxul -810: noise), so that column
// keeps its basis.
const (
	remapShiftBasis byte = 0
	remapIndexBasis byte = 1
)

// fieldPlan is the serialized form of both layers.
type fieldPlan struct {
	Basis                  byte
	RemapIndex, RemapShift []byte
	// Escape and Tag are the index basis's other two columns: the shifts of
	// the entries whose target is not in the set, and the bitmap saying which
	// entries those are.
	RemapEscape, RemapTag  []byte
	FieldIndex, FieldDelta []byte
}

func (p fieldPlan) marshal() []byte {
	b := []byte{p.Basis}
	b = appendStream(b, p.RemapIndex)
	b = appendStream(b, p.RemapShift)
	if p.Basis == remapIndexBasis {
		b = appendStream(b, p.RemapEscape)
		b = appendStream(b, p.RemapTag)
	}
	b = appendStream(b, p.FieldIndex)
	b = appendStream(b, p.FieldDelta)
	return b
}

func unmarshalFieldPlan(b []byte) (fieldPlan, error) {
	r := &planReader{b: b}
	p := fieldPlan{Basis: r.byteAt()}
	if r.err != nil || (p.Basis != remapShiftBasis && p.Basis != remapIndexBasis) {
		return fieldPlan{}, fmt.Errorf("unsupported remap basis %d in field plan", p.Basis)
	}
	p.RemapIndex, p.RemapShift = r.stream().b, r.stream().b
	if p.Basis == remapIndexBasis {
		p.RemapEscape, p.RemapTag = r.stream().b, r.stream().b
	}
	p.FieldIndex, p.FieldDelta = r.stream().b, r.stream().b
	if r.err != nil || len(r.b) != 0 {
		return fieldPlan{}, errors.New("invalid field plan")
	}
	return p, nil
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
	// The site table is already contiguous and each address is independent.
	// Fill one exact array concurrently, then sort it in place: the generic
	// parallel sorter needs another full-size scatter buffer, which overlaps
	// the output image and the site table at the decoder's high-water mark.
	domain := make([]uint64, len(sites))
	shardRange(len(sites), func(lo, hi int) struct{} {
		for i := lo; i < hi; i++ {
			domain[i] = sites[i].addr(text, textAddr)
		}
		return struct{}{}
	})
	sortUint64InPlace(domain)
	return slices.Compact(domain)
}

// remapTargets is the address set a remapped field's new target is named
// against: the addresses the prediction already points at, plus the function
// starts the map placed. Both sides build it from the prediction and the plan,
// so both name the same index.
func remapTargets(domain []uint64, maps []mapping, textAddr uint64) []uint64 {
	extra := make([]uint64, len(maps))
	for i, m := range maps {
		extra[i] = textAddr + m.Dst
	}
	slices.Sort(extra)
	out := make([]uint64, 0, len(domain)+len(extra))
	i, j := 0, 0
	for i < len(domain) && j < len(extra) {
		if domain[i] <= extra[j] {
			out, i = append(out, domain[i]), i+1
		} else {
			out, j = append(out, extra[j]), j+1
		}
	}
	out = append(out, domain[i:]...)
	out = append(out, extra[j:]...)
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
		if !equal4(text[s.off():], want[s.off():]) {
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
			if equal4(text[s.off():], want[s.off():]) {
				correct++
				continue
			}
			disp := int64(int32(binary.LittleEndian.Uint32(want[s.off():])))
			votes[uint64(int64(textAddr)+int64(s.next())+disp)]++
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
	targets := remapTargets(domain, maps, textAddr)
	var shiftCol, idxCol, escCol, tagCol []byte
	var tag byte
	prevIdx, prevShift, prevTarget := 0, int64(0), 0
	for _, from := range slices.Sorted(mapKeys(chosen)) {
		i, ok := slices.BinarySearch(domain, from)
		if !ok {
			continue
		}
		shift := int64(chosen[from]) - int64(from)
		p.RemapIndex = appendS(p.RemapIndex, int64(i-prevIdx))
		shiftCol = appendS(shiftCol, shift-prevShift)
		prevIdx, prevShift = i, shift
		// The index basis, built alongside so the two are priced on the same
		// entries and the smaller one ships.
		ti, _ := slices.BinarySearch(targets, from)
		tj, ok := slices.BinarySearch(targets, chosen[from])
		if ok {
			idxCol = appendS(idxCol, int64(tj-ti)-int64(prevTarget))
			prevTarget = tj - ti
		} else {
			escCol = appendS(escCol, shift)
			tag |= 1 << (st.Remaps % 8)
		}
		if st.Remaps%8 == 7 {
			tagCol = append(tagCol, tag)
			tag = 0
		}
		st.Remaps++
	}
	if st.Remaps%8 != 0 {
		tagCol = append(tagCol, tag)
	}
	p.Basis, p.RemapShift = remapShiftBasis, shiftCol
	indexed := append(append(slices.Clone(idxCol), escCol...), tagCol...)
	if cz.SizeProxy(indexed) < cz.SizeProxy(shiftCol) {
		p.Basis, p.RemapShift = remapIndexBasis, idxCol
		p.RemapEscape, p.RemapTag = escCol, tagCol
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
		if equal4(fixed[s.off():], want[s.off():]) {
			continue
		}
		d := int64(int32(binary.LittleEndian.Uint32(want[s.off():]))) - int64(int32(binary.LittleEndian.Uint32(fixed[s.off():])))
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
	escr := &planReader{b: p.RemapEscape}
	var targets []uint64
	if p.Basis == remapIndexBasis {
		targets = remapTargets(domain, maps, textAddr)
	}
	remap := map[uint64]uint64{}
	idx, shift, prevTarget := 0, int64(0), 0
	for !idxr.done() {
		idx += int(idxr.s())
		if idxr.err != nil || idx < 0 || idx >= len(domain) {
			return st, errors.New("invalid address remap entry")
		}
		from := domain[idx]
		var to uint64
		switch {
		case p.Basis == remapShiftBasis:
			shift += shr.s()
			// Addresses wrap: this is a position-independent image, so a
			// displacement can point below zero and both sides must agree to
			// let it. Nothing here indexes memory by the result -- put
			// truncates it to the field's four bytes -- so wrapping is safe as
			// well as necessary.
			to = uint64(int64(from) + shift)
		case st.Remaps/8 < len(p.RemapTag) && p.RemapTag[st.Remaps/8]&(1<<(st.Remaps%8)) != 0:
			to = uint64(int64(from) + escr.s())
		default:
			i, _ := slices.BinarySearch(targets, from)
			prevTarget += int(shr.s())
			j := i + prevTarget
			if shr.err != nil || j < 0 || j >= len(targets) {
				return st, errors.New("address remap target out of range")
			}
			to = targets[j]
		}
		if shr.err != nil || escr.err != nil {
			return st, errors.New("invalid address remap entry")
		}
		remap[from] = to
		st.Remaps++
	}
	if !shr.done() || !escr.done() ||
		(p.Basis == remapIndexBasis && len(p.RemapTag) != (st.Remaps+7)/8) {
		return st, errors.New("trailing address remap data")
	}
	// Every site owns four bytes no other site owns, and the remap table is
	// finished and read-only by here, so the replay runs over disjoint slices
	// of text in parallel. The count is summed per shard so it does not depend
	// on the split.
	remade := shardRange(len(sites), func(lo, hi int) int {
		n := 0
		for _, s := range sites[lo:hi] {
			if to, ok := remap[s.addr(text, textAddr)]; ok {
				s.put(text, textAddr, to)
				n++
			}
		}
		return n
	})
	for _, n := range remade {
		st.Remade += n
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
		v := int64(int32(binary.LittleEndian.Uint32(text[s.off():]))) + d
		binary.LittleEndian.PutUint32(text[s.off():], uint32(int32(v)))
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
