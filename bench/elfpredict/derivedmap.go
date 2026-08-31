package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"sync"

	"github.com/wjordan/presage/delta/x86"
)

// The symbol sidecar (symsidecar.go) buys -135,868 by giving the decoder a
// carried table of the old binary's 925,590 function units. This probe asks
// whether the carried file is needed at all.
//
// The decode walk never reads a name hash: it uses the table only as a shared,
// ordered enumeration of old function boundaries -- index references, positional
// runs, and sizes for layout replay. If the decoder can DERIVE an equivalent
// enumeration from the old image alone, the file disappears and the stream is
// simply re-keyed against the derived list, with the differences shipped.
//
// Three sources of decoder-derivable evidence are measured, all from the old
// image only:
//
//	E1 direct branch targets -- a linear x86 sweep of the whole old .text,
//	   split by opcode into call / unconditional jmp / jcc. The reference walk
//	   in plan.go cannot be reused verbatim: it walks the *mapped* bodies, and
//	   here the map is what we are trying to build. A linear sweep over fixed
//	   chunks is what a decoder holding nothing but the old image can do, and
//	   both sides do it identically.
//	E2 relocation targets -- .rela.dyn R_X86_64_RELATIVE addends landing in
//	   .text: the address-taken functions (vtables, function pointers).
//	E3 padding starts -- detectBoundaries, the aligned non-padding byte after
//	   a 0xcc run.
//
// .eh_frame_hdr is already refuted: 31,794 FDEs against 925,590 units.

// --- evidence ----------------------------------------------------------

type branchTargets struct {
	call, jmp, jcc []uint64 // .text offsets, sorted and deduplicated
}

// walkOldText sweeps the whole old .text linearly and classifies every
// PC-relative operand whose target lands back in .text by the opcode that
// carries it. Chunking is fixed and both sides use it, so the desynchronised
// decoding inside data islands is identical on both sides; a wrong target is
// only ever a spurious enumeration entry, never a correctness hazard.
func walkOldText(oldText []byte, textAddr uint64) branchTargets {
	const chunk = 8 << 20
	type part struct{ call, jmp, jcc []uint64 }
	n := (len(oldText) + chunk - 1) / chunk
	parts := make([]part, n)
	end := textAddr + uint64(len(oldText))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for k := 0; k < n; k++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(k int) {
			defer wg.Done()
			defer func() { <-sem }()
			lo := k * chunk
			hi := min(lo+chunk, len(oldText))
			code := oldText[lo:hi]
			p := &parts[k]
			x86.WalkReferences(code, textAddr+uint64(lo), func(ref x86.Reference) {
				if ref.Target < textAddr || ref.Target >= end {
					return
				}
				off := ref.Target - textAddr
				s := ref.Start
				switch {
				case code[s] == 0xe8 && ref.N == 4:
					p.call = append(p.call, off)
				case code[s] == 0xe9 && ref.N == 4, code[s] == 0xeb && ref.N == 1:
					p.jmp = append(p.jmp, off)
				case code[s] == 0x0f && s+1 < len(code) && code[s+1] >= 0x80 && code[s+1] <= 0x8f:
					p.jcc = append(p.jcc, off)
				case code[s] >= 0x70 && code[s] <= 0x7f && ref.N == 1:
					p.jcc = append(p.jcc, off)
				}
			})
		}(k)
	}
	wg.Wait()
	var t branchTargets
	for _, p := range parts {
		t.call = append(t.call, p.call...)
		t.jmp = append(t.jmp, p.jmp...)
		t.jcc = append(t.jcc, p.jcc...)
	}
	t.call, t.jmp, t.jcc = sortUniq(t.call), sortUniq(t.jmp), sortUniq(t.jcc)
	return t
}

func sortUniq(v []uint64) []uint64 {
	slices.Sort(v)
	return slices.Compact(v)
}

func unionSorted(vs ...[]uint64) []uint64 {
	var out []uint64
	for _, v := range vs {
		out = append(out, v...)
	}
	return sortUniq(out)
}

// relocTextTargets is E2: the R_X86_64_RELATIVE addends that point into .text.
func relocTextTargets(img *image) []uint64 {
	sec, ok := img.Sections[".rela.dyn"]
	if !ok {
		if sec, ok = img.Sections[".rela"]; !ok {
			return nil
		}
	}
	rel, _, _ := parseRela(img.Data[sec.Off : sec.Off+sec.Size])
	end := img.Text.Addr + img.Text.Size
	var out []uint64
	for _, e := range rel {
		if e.addend >= img.Text.Addr && e.addend < end {
			out = append(out, e.addend-img.Text.Addr)
		}
	}
	return sortUniq(out)
}

// --- phase 1: coverage --------------------------------------------------

type coverage struct {
	name                          string
	derived                       int
	recovered                     int // true unit starts present in the enumeration
	spuriousInside, spuriousOuter int
	sizeExact, sizeOver, sizeUnder int
}

// paddingExtent is the size rule both sides can apply: run to the next
// enumerated start and back off the trailing 0xcc padding. It is sourceExtents'
// rule, restated over an enumeration rather than a source column.
func paddingExtent(oldText []byte, start, next uint64) uint64 {
	end := next
	for end > start && oldText[end-1] == 0xcc {
		end--
	}
	return end - start
}

func measureCoverage(name string, derived []uint64, units []namedUnit, oldText []byte) coverage {
	c := coverage{name: name, derived: len(derived)}
	starts := make([]uint64, len(units))
	for i, u := range units {
		starts[i] = u.Off
	}
	trueIdx := make(map[uint64]int, len(units))
	for i, u := range units {
		trueIdx[u.Off] = i
	}
	for k, off := range derived {
		i, ok := trueIdx[off]
		if !ok {
			// Inside a unit body, or in padding / outside every unit.
			j := sort.Search(len(starts), func(j int) bool { return starts[j] > off }) - 1
			if j >= 0 && off < units[j].Off+units[j].Size {
				c.spuriousInside++
			} else {
				c.spuriousOuter++
			}
			continue
		}
		c.recovered++
		next := uint64(len(oldText))
		if k+1 < len(derived) {
			next = derived[k+1]
		}
		switch sz := paddingExtent(oldText, off, next); {
		case sz == units[i].Size:
			c.sizeExact++
		case sz > units[i].Size:
			c.sizeOver++
		default:
			c.sizeUnder++
		}
	}
	return c
}

// --- phase 2: the re-keyed stream ---------------------------------------

// derivedStream is the sidecar delta re-keyed against a derived enumeration,
// plus the three columns that turn "what the old image implies" into "the old
// unit list".
type derivedStream struct {
	base *sidecarDelta

	Boundary   []byte // address gaps of the true starts the enumeration missed
	Suppress   []byte // optional bitmap: which derived entries are real
	SizeFixIdx []byte // enumeration entries whose padding-rule size is wrong
	SizeFixVal []byte
}

func (s *derivedStream) columns() []struct {
	name string
	b    []byte
} {
	out := []struct {
		name string
		b    []byte
	}{
		{"boundary exceptions", s.Boundary},
		{"suppression bitmap", s.Suppress},
		{"old-size fixup index", s.SizeFixIdx},
		{"old-size fixup value", s.SizeFixVal},
	}
	for _, c := range s.base.columns() {
		if c.name == "insert name hashes" {
			continue // no carried table to roll forward
		}
		out = append(out, struct {
			name string
			b    []byte
		}{c.name, *c.b})
	}
	return out
}

func (s *derivedStream) bytes() []byte {
	var b []byte
	for _, c := range s.columns() {
		b = append(b, c.b...)
	}
	return b
}

// buildDerivedStream re-keys the sidecar construction against enumeration E.
//
// Two differences from symsidecar. First, there is no hash join, so there is
// no correspondence-exception layer: the encoder codes the shipped map's own
// correspondence directly against the positional cursor, and the walk's order
// exceptions are the only thing that pays. Second, an enumeration entry that
// is not a real unit is never referenced, so it behaves exactly like a dropped
// unit and the drop-run column already carries it.
func buildDerivedStream(derived []uint64, oldNamed, newNamed []namedUnit, maps []mapping, oldText []byte, suppress bool) (*derivedStream, []sidecarUnit, sidecarStats, error) {
	var st sidecarStats
	trueIdx := make(map[uint64]int, len(oldNamed))
	for i, u := range oldNamed {
		trueIdx[u.Off] = i
	}

	// Boundary exceptions: the true starts the enumeration missed, shipped as
	// address gaps. With suppression the enumeration keeps only its correct
	// entries, so E is exactly the true start list.
	inDerived := make(map[uint64]bool, len(derived))
	for _, off := range derived {
		inDerived[off] = true
	}
	s := &derivedStream{}
	var missing []uint64
	for _, u := range oldNamed {
		if !inDerived[u.Off] {
			missing = append(missing, u.Off)
		}
	}
	var prev uint64
	for _, off := range missing {
		s.Boundary = binary.AppendUvarint(s.Boundary, off-prev)
		prev = off
	}

	var offs []uint64
	if suppress {
		bits := make([]byte, (len(derived)+7)/8)
		for i, off := range derived {
			if _, ok := trueIdx[off]; ok {
				bits[i/8] |= 1 << (i % 8)
			}
		}
		s.Suppress = bits
		for _, u := range oldNamed {
			offs = append(offs, u.Off)
		}
	} else {
		offs = unionSorted(derived, missing)
	}

	// Sizes by the padding rule, then fixups where the rule is wrong. Only
	// entries the map actually sources from need a correct size, so the fixup
	// column is built after the correspondence is known.
	E := make([]sidecarUnit, len(offs))
	for i, off := range offs {
		next := uint64(len(oldText))
		if i+1 < len(offs) {
			next = offs[i+1]
		}
		E[i] = sidecarUnit{Off: off, Size: paddingExtent(oldText, off, next)}
	}
	eIdx := make(map[uint64]int, len(E))
	for i, u := range E {
		eIdx[u.Off] = i
	}

	// The shipped map's own answer, per new unit, in enumeration indices.
	newUnits := hashNamedUnits(newNamed)
	for i := range newUnits {
		newUnits[i].Hashes = nil
	}
	newIdx := make(map[uint64]int, len(newUnits))
	for i, u := range newUnits {
		newIdx[u.Off] = i
	}
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
			m.DstSize == newUnits[ni].Size && m.SrcSize == oldNamed[ti].Size {
			joined[ni] = oi
			continue
		}
		raw = binary.AppendUvarint(raw, m.Dst-prevRawDst)
		raw = binary.AppendUvarint(raw, m.DstSize)
		raw = binary.AppendUvarint(raw, m.Src)
		raw = binary.AppendUvarint(raw, m.SrcSize)
		prevRawDst = m.Dst
		st.Unrepresentable++
	}

	// Size fixups over the referenced entries.
	referenced := make([]bool, len(E))
	for _, oi := range joined {
		if oi >= 0 {
			referenced[oi] = true
		}
	}
	prevFix := 0
	for i := range E {
		if !referenced[i] {
			continue
		}
		want := oldNamed[trueIdx[E[i].Off]].Size
		if want == E[i].Size {
			continue
		}
		s.SizeFixIdx = appendGap(s.SizeFixIdx, i, prevFix)
		s.SizeFixVal = binary.AppendVarint(s.SizeFixVal, int64(want)-int64(E[i].Size))
		E[i].Size = want
		prevFix = i
	}

	d, st2, err := deltaFromJoin(E, newUnits, joined, raw, uint64(len(sorted)))
	if err != nil {
		return nil, nil, st, err
	}
	st2.Unrepresentable = st.Unrepresentable
	s.base = d

	// The gate: replay the decoder and require the shipped map back exactly.
	// InsertHash is not part of the derived stream -- there is no carried table
	// to roll forward -- but reconstructMaps reads it, so the replay is handed a
	// zero-filled column that is never priced.
	gate := *d
	gate.InsertHash = make([]byte, 8*st2.Inserts)
	got, err := reconstructMaps(&gate, E)
	if err != nil {
		return nil, nil, st2, fmt.Errorf("derived reconstruction failed: %w", err)
	}
	if len(got) != len(sorted) {
		return nil, nil, st2, fmt.Errorf("derived reconstruction produced %d mappings, want %d", len(got), len(sorted))
	}
	for i := range got {
		want := sorted[i]
		want.Copy = false
		if got[i] != want {
			return nil, nil, st2, fmt.Errorf("derived reconstruction differs at mapping %d: %+v want %+v", i, got[i], want)
		}
	}
	return s, E, st2, nil
}

// deltaFromJoin is buildSidecarDelta's second half with the correspondence
// taken as given: drop runs, the positional walk with its order exceptions,
// size deltas, inserts and the layout replay.
func deltaFromJoin(oldUnits, newUnits []sidecarUnit, joined []int, raw []byte, nmaps uint64) (*sidecarDelta, sidecarStats, error) {
	var st sidecarStats
	st.OldUnits, st.NewUnits = len(oldUnits), len(newUnits)
	d := &sidecarDelta{NewUnits: uint64(len(newUnits)), Maps: nmaps, Raw: raw}

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
			d.InsertHash = binary.LittleEndian.AppendUint64(d.InsertHash, 0)
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
	return d, st, nil
}

// spliceStructure is marshalSidecarForm with an arbitrary blob in place of the
// map columns, so a re-keyed stream can be priced where the columns it replaces
// actually sit.
func spliceStructure(p predictionPlan, oldText []byte, stream []byte) ([]byte, error) {
	b, err := p.marshalBlanked(oldText, true, false)
	if err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("cannot locate the map columns in the blanked plan")
	}
	cut := len(b) - len(r.b)
	out := make([]byte, 0, len(b)+len(stream)+8)
	out = append(out, b[:modeAt]...)
	out = append(out, byte(planSidecar))
	out = append(out, b[modeAt+1:cut]...)
	out = append(out, stream...)
	out = append(out, b[cut:]...)
	return out, nil
}

func splicedPlanSize(planBytes []byte, structure predictionPlan, oldText, stream []byte) int {
	sb, err := spliceStructure(structure, oldText, stream)
	if err != nil {
		return 0
	}
	cp, err := unmarshalCombinedPlan(planBytes)
	if err != nil {
		return 0
	}
	cp.Structure = sb
	return xzSizeContiguous(cp.marshal())
}

// --- the probe ----------------------------------------------------------

func probeDerivedMap(planBytes []byte, structure predictionPlan, oldImage, newImage *image) {
	if oldDebugPath == "" || newDebugPath == "" {
		fmt.Fprintln(os.Stderr, "probe derivedmap: needs -old-debug and -new-debug")
		return
	}
	f := os.Stderr
	fmt.Fprintln(f, "probe derivedmap: can the sidecar's decode-side role be served without a carried file?")
	oldText := oldImage.textBytes()
	_, _, mapOnly, replaceable := sidecarCalibration(planBytes, structure, oldText)

	t := startStage("derivedmap symbols")
	oldNamed, err := loadNamedUnits(oldDebugPath, oldImage.Text)
	if err != nil {
		fmt.Fprintf(f, "probe derivedmap FAILED: %v\n", err)
		return
	}
	newNamed, err := loadNamedUnits(newDebugPath, newImage.Text)
	if err != nil {
		fmt.Fprintf(f, "probe derivedmap FAILED: %v\n", err)
		return
	}
	t.done("old %d units, new %d units", len(oldNamed), len(newNamed))

	t = startStage("derivedmap evidence")
	bt := walkOldText(oldText, oldImage.Text.Addr)
	e2 := relocTextTargets(oldImage)
	e3 := detectBoundaries(oldText)
	t.done("%d call, %d jmp, %d jcc, %d reloc, %d padding", len(bt.call), len(bt.jmp), len(bt.jcc), len(e2), len(e3))

	// --- PHASE 1 ---------------------------------------------------------
	e1call := bt.call
	e1cj := unionSorted(bt.call, bt.jmp)
	e1all := unionSorted(bt.call, bt.jmp, bt.jcc)
	cands := []struct {
		name string
		v    []uint64
	}{
		{"E1 call only", e1call},
		{"E1 call+jmp", e1cj},
		{"E1 call+jmp+jcc", e1all},
		{"E2 reloc targets", e2},
		{"E3 padding starts", e3},
		{"E1(call+jmp) + E2", unionSorted(e1cj, e2)},
		{"E1(call+jmp) + E2 + E3", unionSorted(e1cj, e2, e3)},
		{"E1(call) + E2 + E3", unionSorted(e1call, e2, e3)},
	}
	fmt.Fprintf(f, "  PHASE 1: coverage against %d true old units\n", len(oldNamed))
	fmt.Fprintf(f, "  %-24s %10s %10s %8s %10s %10s %10s %8s %8s\n",
		"evidence", "derived", "recovered", "recall%", "spur-in", "spur-out", "size-exact", "over", "under")
	for _, c := range cands {
		cv := measureCoverage(c.name, c.v, oldNamed, oldText)
		fmt.Fprintf(f, "  %-24s %10d %10d %7.3f%% %10d %10d %10d %8d %8d\n",
			cv.name, cv.derived, cv.recovered,
			100*float64(cv.recovered)/float64(max(1, len(oldNamed))),
			cv.spuriousInside, cv.spuriousOuter, cv.sizeExact, cv.sizeOver, cv.sizeUnder)
	}

	// Where the misses concentrate: units the shipped map sources from (the
	// live ones) against the rest.
	best := unionSorted(e1cj, e2, e3)
	inBest := make(map[uint64]bool, len(best))
	for _, off := range best {
		inBest[off] = true
	}
	used := make(map[uint64]bool, len(structure.Maps))
	for _, m := range structure.Maps {
		used[m.Src] = true
	}
	var missUsed, missUnused, totUsed, totUnused int
	for _, u := range oldNamed {
		if used[u.Off] {
			totUsed++
			if !inBest[u.Off] {
				missUsed++
			}
		} else {
			totUnused++
			if !inBest[u.Off] {
				missUnused++
			}
		}
	}
	fmt.Fprintf(f, "  misses: %d of %d map-sourced units (%.3f%%), %d of %d never-sourced units (%.3f%%)\n",
		missUsed, totUsed, 100*float64(missUsed)/float64(max(1, totUsed)),
		missUnused, totUnused, 100*float64(missUnused)/float64(max(1, totUnused)))

	// --- PHASE 2 ---------------------------------------------------------
	// Recall and precision both cost. A missing true start costs a boundary
	// exception; a spurious entry costs its place in the drop runs AND, worse,
	// truncates the padding-rule size of whatever unit contains it, so it drags
	// a size fixup with it. Phase 1 says the conditional-branch targets are all
	// cost and no recall, so the enumerations worth pricing are the three below,
	// each under both spurious-handling policies.
	fmt.Fprintln(f, "  PHASE 2: the re-keyed stream")
	enums := []struct {
		name string
		v    []uint64
	}{
		{"E1(call)+E2+E3", unionSorted(e1call, e2, e3)},
		{"E3 only", e3},
		{"E1(call+jmp)+E2+E3", best},
	}
	variants := []struct {
		name     string
		suppress bool
	}{
		{"spurious kept as dropped entries", false},
		{"spurious suppressed by bitmap", true},
	}
	type priced struct {
		name       string
		standalone int
		inPlace    int
	}
	var results []priced
	for _, en := range enums {
		for _, v := range variants {
			t = startStage("derivedmap stream")
			s, E, st, err := buildDerivedStream(en.v, oldNamed, newNamed, structure.Maps, oldText, v.suppress)
			if err != nil {
				fmt.Fprintf(f, "probe derivedmap FAILED (%s, %s): %v\n", en.name, v.name, err)
				return
			}
			name := en.name + ", " + v.name
			t.done("%s: %d enumeration entries", name, len(E))
			fmt.Fprintf(f, "  %s\n", name)
			fmt.Fprintf(f, "    %d enumeration entries, %d dropped in %d runs; %d reorders, %d resizes, %d inserts, %d layout fixups at align %d, %d unrepresentable\n",
				st.OldUnits, st.Dropped, st.Runs, st.Reorders, st.Resizes, st.Inserts, st.Fixes, st.Align, st.Unrepresentable)
			cols := s.columns()
			bs := make([][]byte, len(cols))
			raw := 0
			for i, c := range cols {
				bs[i] = c.b
				raw += len(c.b)
			}
			z := xzSizes(bs...)
			fmt.Fprintf(f, "    %-34s %10s %10s\n", "column", "raw", "xz alone")
			sum := 0
			for i, c := range cols {
				sum += z[i]
				fmt.Fprintf(f, "    %-34s %10d %10d\n", c.name, len(c.b), z[i])
			}
			stream := s.bytes()
			total := xzSizeContiguous(stream)
			inPlace := splicedPlanSize(planBytes, structure, oldText, stream)
			fmt.Fprintf(f, "    %-34s %10d %10d  (sum of columns %d)\n", "TOTAL, one contiguous stream", raw, total, sum)
			results = append(results, priced{name, total, inPlace})
		}
	}

	// The carried sidecar, built in the same run and priced the same way, so
	// the comparison is not against a remembered number.
	carriedStandalone, carriedInPlace, carriedTable := 0, 0, 0
	if d, oldUnits, cst, err := buildSidecarDelta(oldNamed, newNamed, structure.Maps); err == nil {
		var whole []byte
		for _, c := range d.columns() {
			whole = append(whole, *c.b...)
		}
		carriedStandalone = xzSizeContiguous(whole)
		carriedInPlace = splicedPlanSize(planBytes, structure, oldText, d.marshal())
		carriedTable = xzSizeContiguous(marshalSidecarTable(oldUnits))
		fmt.Fprintf(f, "  carried sidecar rebuilt here: %d exceptions, %d correspondence disagreements, carried table xz %d\n",
			cst.Exceptions, cst.JoinDisagreed, carriedTable)
	} else {
		fmt.Fprintf(f, "  carried sidecar rebuild FAILED: %v\n", err)
	}

	// --- PHASE 3 ---------------------------------------------------------
	full := xzSizeContiguous(planBytes)
	fmt.Fprintln(f, "  PHASE 3: verdict")
	fmt.Fprintf(f, "  %-46s %12s %12s %12s %12s\n", "variant", "stream xz", "plan xz", "vs baseline", "carried file")
	fmt.Fprintf(f, "  %-46s %12s %12d %12s %12s\n", "map columns (baseline)", "-", full, "-", "none")
	if carriedInPlace != 0 {
		fmt.Fprintf(f, "  %-46s %12d %12d %+12d %12d\n", "carried symbol sidecar", carriedStandalone, carriedInPlace, carriedInPlace-full, carriedTable)
	}
	for _, r := range results {
		fmt.Fprintf(f, "  %-46s %12d %12d %+12d %12s\n", r.name, r.standalone, r.inPlace, r.inPlace-full, "none")
	}
	fmt.Fprintf(f, "  map columns cost %d marginally, of which %d is replaceable\n", mapOnly, replaceable)
	fmt.Fprintln(f, "  decoder cost: one extra linear x86 sweep of the old .text plus a .rela.dyn scan, both before the map is known; per §10 the decoder already walks old .text twice, so this is a third pass over the same bytes and no new machinery.")
}
