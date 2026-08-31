package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/zeebo/blake3"
)

// The derived-map rung: the symbol sidecar's decode-side role served with no
// carried file at all.
//
// symsidecar.go hands the decoder a table of the old binary's function units,
// installed by the previous patch. derivedmap.go measured what happens if that
// table is *derived* from the old image instead. This file graduates that probe
// into a real encoder and decoder, so a plan in planDerived mode decodes from
// nothing but the old file's bytes and the patch.
//
// The whole design rests on one property: the enumeration is a pure
// deterministic function of the old image bytes, computed by a single function
// -- deriveEnumeration -- that both sides call. It reads no symbols, no map,
// and nothing the decoder does not hold. As a loud guard against that ever
// slipping, the stream ships the enumeration's length and the decoder refuses a
// plan whose own derivation disagrees.

// deriveEnumeration is the shared derivation. Its only input is the old file's
// bytes plus where .text lies inside them.
//
//	E1 call rel32 targets, from a linear x86 sweep of the whole old .text
//	E2 .rela.dyn addends landing in .text
//	E3 padding starts, the detectBoundaries rule
//
// Conditional and unconditional jump targets are deliberately absent: §5.1c
// measured them at 1.4-5.0 M spurious entries for +0.7 pp of recall.
//
// A file with no parseable ELF header or no relocation section simply
// contributes no E2. That cannot desynchronise the two sides -- they run this
// same function over the same bytes -- and it keeps the derivation total.
func deriveEnumeration(oldFile []byte, text section) []uint64 {
	if text.Off > uint64(len(oldFile)) || text.Size > uint64(len(oldFile))-text.Off {
		return nil
	}
	oldText := oldFile[text.Off : text.Off+text.Size]
	e1 := walkOldText(oldText, text.Addr).call
	e2 := relaTextTargets(oldFile, text)
	e3 := detectBoundaries(oldText)
	return unionSorted(e1, e2, e3)
}

// relaTextTargets is E2 over raw file bytes: the relocation addends that point
// into .text, which is where the address-taken functions show up.
func relaTextTargets(oldFile []byte, text section) []uint64 {
	f, err := elf.NewFile(bytes.NewReader(oldFile))
	if err != nil {
		return nil
	}
	var sec *elf.Section
	for _, name := range []string{".rela.dyn", ".rela"} {
		if s := f.Section(name); s != nil {
			sec = s
			break
		}
	}
	if sec == nil || sec.Type == elf.SHT_NOBITS ||
		sec.Offset > uint64(len(oldFile)) || sec.Size > uint64(len(oldFile))-sec.Offset {
		return nil
	}
	rel, _, _ := parseRela(oldFile[sec.Offset : sec.Offset+sec.Size])
	end := text.Addr + text.Size
	var out []uint64
	for _, e := range rel {
		if e.addend >= text.Addr && e.addend < end {
			out = append(out, e.addend-text.Addr)
		}
	}
	return sortUniq(out)
}

// The sweep costs seconds on a 130 MB .text and both the encoder and the
// decoder want the same answer, so it is computed once per distinct old image.
// The key is the image's content hash, not its address: a pointer would go
// stale the moment two images shared a buffer.
var derivedEnum struct {
	sync.Mutex
	key string
	v   []uint64
}

func cachedDerivedEnumeration(oldFile []byte, text section) []uint64 {
	sum := blake3.Sum256(oldFile)
	key := fmt.Sprintf("%x/%d/%d/%d", sum, text.Off, text.Size, text.Addr)
	derivedEnum.Lock()
	defer derivedEnum.Unlock()
	if derivedEnum.key == key {
		return derivedEnum.v
	}
	t := startStage("derived enumeration")
	v := deriveEnumeration(oldFile, text)
	t.done("%d entries from %d bytes of old .text", len(v), text.Size)
	derivedEnum.key, derivedEnum.v = key, v
	return v
}

// --- the enumeration columns -------------------------------------------

// enumerationUnits turns a derived enumeration plus the two columns that repair
// it into the ordered unit list the delta stream is keyed against. Both sides
// run exactly this; the encoder then builds the size fixups against the result
// and the decoder replays them.
func enumerationUnits(derived []uint64, suppress, boundary, oldText []byte) ([]sidecarUnit, error) {
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
	E := make([]sidecarUnit, len(offs))
	for i, off := range offs {
		if off >= uint64(len(oldText)) {
			return nil, errors.New("derived enumeration entry lies outside old .text")
		}
		next := uint64(len(oldText))
		if i+1 < len(offs) {
			next = offs[i+1]
		}
		E[i] = sidecarUnit{Off: off, Size: paddingExtent(oldText, off, next)}
	}
	return E, nil
}

// derivedUnits is the decoder's whole enumeration path: derive, repair, size.
// It is a pure function of the old file's bytes and the stream.
func derivedUnits(s *derivedStream, oldFile []byte, text section) ([]sidecarUnit, error) {
	if text.Off > uint64(len(oldFile)) || text.Size > uint64(len(oldFile))-text.Off {
		return nil, errors.New("old .text lies outside the old image")
	}
	oldText := oldFile[text.Off : text.Off+text.Size]
	d := cachedDerivedEnumeration(oldFile, text)
	if uint64(len(d)) != s.Derived {
		return nil, fmt.Errorf("derived enumeration has %d entries, the plan was built against %d", len(d), s.Derived)
	}
	E, err := enumerationUnits(d, s.Suppress, s.Boundary, oldText)
	if err != nil {
		return nil, err
	}
	fixAt, err := sparseSigned(s.SizeFixIdx, s.SizeFixVal, len(E))
	if err != nil {
		return nil, err
	}
	for i, v := range fixAt {
		sz := int64(E[i].Size) + v
		if sz < 0 {
			return nil, errors.New("derived size fixup underflows")
		}
		E[i].Size = uint64(sz)
	}
	return E, nil
}

// --- serialization -----------------------------------------------------

func (s *derivedStream) marshal() []byte {
	b := binary.AppendUvarint(nil, s.Derived)
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
	d, err := parseSidecarDelta(r)
	if err != nil {
		return nil, err
	}
	// There is no carried table to roll forward, so no insert carries a name
	// hash and the column is absent rather than zero-filled.
	d.NoInsertHash = true
	s.base = d
	return s, nil
}

// marshalDerivedForm is marshalSidecarForm's twin: the five map columns emptied
// and the derived stream put where they were. Everything downstream -- the copy
// bitmap, the reference points, the ranges -- is byte-for-byte what the normal
// plan emits, because the decoder reconstructs the same map.
func (p predictionPlan) marshalDerivedForm(oldText []byte, s *derivedStream) ([]byte, error) {
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
		return nil, errors.New("cannot locate the map columns in the blanked plan")
	}
	cut := len(b) - len(r.b)
	out := make([]byte, 0, len(b)+64)
	out = append(out, b[:modeAt]...)
	out = append(out, byte(planDerived))
	out = append(out, b[modeAt+1:cut]...)
	out = append(out, s.marshal()...)
	out = append(out, b[cut:]...)
	return out, nil
}

// readDerivedMaps is readSidecarMaps with the enumeration derived rather than
// carried. Its inputs are the plan and the old file, and nothing else.
func readDerivedMaps(r *planReader, p *predictionPlan, n uint64, oldFile []byte, text section) error {
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
	E, err := derivedUnits(s, oldFile, text)
	if err != nil {
		return err
	}
	maps, err := reconstructMaps(s.base, E)
	if err != nil {
		return err
	}
	if uint64(len(maps)) != n {
		return fmt.Errorf("derived reconstruction produced %d mappings, plan says %d", len(maps), n)
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

// --- the rung -----------------------------------------------------------

// buildDerivedRung turns the corrected-fields plan into its derived-form twin.
// Symbols are read here for exactly one reason -- they are what the *encoder*
// built the shipped map from, as they are for every other rung -- and never
// enter the enumeration.
func buildDerivedRung(correctedPlan []byte, oldImage, newImage *image, structure predictionPlan) ([]byte, error) {
	if oldDebugPath == "" || newDebugPath == "" {
		return nil, errors.New("the derived-map rung needs -old-debug and -new-debug")
	}
	oldText := oldImage.textBytes()
	t := startStage("derived symbols")
	oldNamed, err := loadNamedUnits(oldDebugPath, oldImage.Text)
	if err != nil {
		return nil, err
	}
	newNamed, err := loadNamedUnits(newDebugPath, newImage.Text)
	if err != nil {
		return nil, err
	}
	t.done("old %d units, new %d units", len(oldNamed), len(newNamed))

	derived := cachedDerivedEnumeration(oldImage.Data, oldImage.Text)

	t = startStage("derived stream")
	s, E, st, err := buildDerivedStream(derived, oldNamed, newNamed, structure.Maps, oldText, true)
	if err != nil {
		return nil, err
	}
	t.done("%d enumeration entries, %d unrepresentable", len(E), st.Unrepresentable)

	// The determinism gate: rebuild the unit list the way the decoder will,
	// from the old image bytes and the serialized stream alone, and require it
	// to be what the encoder keyed against.
	parsed, err := parseDerivedStream(&planReader{b: s.marshal()})
	if err != nil {
		return nil, err
	}
	back, err := derivedUnits(parsed, oldImage.Data, oldImage.Text)
	if err != nil {
		return nil, err
	}
	if len(back) != len(E) {
		return nil, fmt.Errorf("decoder-side enumeration has %d units, the encoder used %d", len(back), len(E))
	}
	for i := range back {
		if back[i].Off != E[i].Off || back[i].Size != E[i].Size {
			return nil, fmt.Errorf("decoder-side enumeration differs at unit %d: %+v want %+v", i, back[i], E[i])
		}
	}

	cp, err := unmarshalCombinedPlan(correctedPlan)
	if err != nil {
		return nil, err
	}
	normalStructure := cp.Structure
	derivedStructure, err := structure.marshalDerivedForm(oldText, s)
	if err != nil {
		return nil, err
	}
	cp.Structure = derivedStructure
	out := cp.marshal()

	t = startStage("derived accounting")
	reportDerived(st, s, len(derived), normalStructure, derivedStructure, correctedPlan, out, structure, oldText)
	t.done("")
	return out, nil
}

func reportDerived(st sidecarStats, s *derivedStream, derived int, normalStructure, derivedStructure, normalPlan, derivedPlan []byte, structure predictionPlan, oldText []byte) {
	f := os.Stderr
	fmt.Fprintf(f, "derived: %d enumeration entries derived from the old image alone (no carried file)\n", derived)
	fmt.Fprintf(f, "derived: %d units after suppression and boundary exceptions, %d dropped in %d runs; %d reorders, %d resizes, %d inserts, %d layout fixups at align %d, %d unrepresentable\n",
		st.OldUnits, st.Dropped, st.Runs, st.Reorders, st.Resizes, st.Inserts, st.Fixes, st.Align, st.Unrepresentable)

	// Each column priced by emptying it inside the whole plan, which is the
	// only number the headline is made of.
	full := xzSizeContiguous(derivedPlan)
	cols := s.pointers()
	fmt.Fprintf(f, "  %-34s %10s %10s\n", "derived column", "raw", "xz in place")
	sum := 0
	for _, c := range cols {
		saved := *c.b
		*c.b = nil
		b, err := structure.marshalDerivedForm(oldText, s)
		*c.b = saved
		size := 0
		if err == nil {
			if cp, err := unmarshalCombinedPlan(derivedPlan); err == nil {
				cp.Structure = b
				size = full - xzSizeContiguous(cp.marshal())
			}
		}
		sum += size
		fmt.Fprintf(f, "  %-34s %10d %10d\n", c.name, len(*c.b), size)
	}
	fmt.Fprintf(f, "  %-34s %10s %10d\n", "sum of columns", "", sum)
	fmt.Fprintf(f, "derived: structure stream %d -> %d raw, %d -> %d xz standalone; whole plan %d -> %d xz\n",
		len(normalStructure), len(derivedStructure),
		xzSizeContiguous(normalStructure), xzSizeContiguous(derivedStructure),
		xzSizeContiguous(normalPlan), full)
}
