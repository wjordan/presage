package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"
)

// synthDerivedELF builds a minimal ELF64 whose .text is laid out so that every
// case the derived enumeration has to handle appears exactly once:
//
//	off   0..39   unit A, a call rel32 at 0 targeting offset 96
//	off  40..47   0xcc padding
//	off  48..71   unit B, found by the padding rule and by .rela.dyn
//	off  72..95   0xcc padding
//	off  96..127  unit C, with a 0xcc island at 104..111 that makes 112 a
//	              spurious padding start inside the body
//	off 128..159  unit D, whose start no evidence finds -- a boundary exception
//
// So the derivation must produce exactly {0, 48, 96, 112}: three true starts,
// one spurious entry, one true start missed.
const (
	synthTextAddr = 0x1000
	synthTextLen  = 160
)

func synthDerivedText() []byte {
	t := make([]byte, synthTextLen)
	for i := range t {
		t[i] = 0x90 // nop: no PC-relative operand of its own
	}
	// call rel32 at 0; next pc is 5, so 5+91 = 96.
	t[0] = 0xe8
	binary.LittleEndian.PutUint32(t[1:], 91)
	for i := 40; i < 48; i++ {
		t[i] = 0xcc
	}
	for i := 72; i < 96; i++ {
		t[i] = 0xcc
	}
	for i := 104; i < 112; i++ {
		t[i] = 0xcc
	}
	return t
}

// synthDerivedELF wraps that .text in the smallest ELF debug/elf will parse,
// with one .rela.dyn entry whose addend points at offset 48.
func synthDerivedELF() ([]byte, section) {
	text := synthDerivedText()
	const (
		ehSize  = 64
		shSize  = 64
		shCount = 4
	)
	names := []byte("\x00.text\x00.rela.dyn\x00.shstrtab\x00")
	textOff := uint64(ehSize)
	relaOff := textOff + uint64(len(text))
	rela := make([]byte, 24)
	binary.LittleEndian.PutUint64(rela[0:], 0x9000)            // r_offset
	binary.LittleEndian.PutUint64(rela[8:], 8)                 // R_X86_64_RELATIVE
	binary.LittleEndian.PutUint64(rela[16:], synthTextAddr+48) // r_addend
	strOff := relaOff + uint64(len(rela))
	shOff := strOff + uint64(len(names))

	b := make([]byte, shOff+shSize*shCount)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	binary.LittleEndian.PutUint16(b[16:], 2)  // ET_EXEC
	binary.LittleEndian.PutUint16(b[18:], 62) // EM_X86_64
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint64(b[40:], shOff)
	binary.LittleEndian.PutUint16(b[52:], ehSize)
	binary.LittleEndian.PutUint16(b[58:], shSize)
	binary.LittleEndian.PutUint16(b[60:], shCount)
	binary.LittleEndian.PutUint16(b[62:], 3) // shstrndx
	copy(b[textOff:], text)
	copy(b[relaOff:], rela)
	copy(b[strOff:], names)

	sh := func(i int, name uint32, typ uint32, flags, addr, off, size, entsize uint64) {
		h := b[shOff+uint64(i*shSize):]
		binary.LittleEndian.PutUint32(h[0:], name)
		binary.LittleEndian.PutUint32(h[4:], typ)
		binary.LittleEndian.PutUint64(h[8:], flags)
		binary.LittleEndian.PutUint64(h[16:], addr)
		binary.LittleEndian.PutUint64(h[24:], off)
		binary.LittleEndian.PutUint64(h[32:], size)
		binary.LittleEndian.PutUint64(h[48:], 1)
		binary.LittleEndian.PutUint64(h[56:], entsize)
	}
	sh(1, 1, 1 /*PROGBITS*/, 0x6 /*ALLOC|EXEC*/, synthTextAddr, textOff, uint64(len(text)), 0)
	sh(2, 7, 4 /*RELA*/, 0x2 /*ALLOC*/, 0x8000, relaOff, uint64(len(rela)), 24)
	sh(3, 17, 3 /*STRTAB*/, 0, 0, strOff, uint64(len(names)), 0)
	return b, section{Addr: synthTextAddr, Off: textOff, Size: uint64(len(text))}
}

// TestDeriveEnumerationDeterministic pins what the shared derivation produces
// and that it is a pure function of the old image's bytes: same input, same
// answer, and no input but the image.
func TestDeriveEnumerationDeterministic(t *testing.T) {
	file, text := synthDerivedELF()
	want := []uint64{0, 48, 96, 112}
	for i := 0; i < 3; i++ {
		got := deriveEnumeration(file, text)
		if !slices.Equal(got, want) {
			t.Fatalf("run %d derived %v, want %v", i, got, want)
		}
	}
	// A separate copy of the same bytes must derive the same list.
	if got := deriveEnumeration(slices.Clone(file), text); !slices.Equal(got, want) {
		t.Fatalf("a copy of the image derived %v, want %v", got, want)
	}
	// Each source contributes what it is supposed to.
	if got := relaTextTargets(file, text); !slices.Equal(got, []uint64{48}) {
		t.Fatalf(".rela.dyn targets %v, want [48]", got)
	}
	if got := walkOldText(file[text.Off:text.Off+text.Size], text.Addr).call; !slices.Equal(got, []uint64{96}) {
		t.Fatalf("call targets %v, want [96]", got)
	}
	if got := detectBoundaries(file[text.Off : text.Off+text.Size]); !slices.Equal(got, []uint64{0, 48, 96, 112}) {
		t.Fatalf("padding starts %v, want [0 48 96 112]", got)
	}
	// A buffer that is not an ELF still derives, just without E2.
	if got := deriveEnumeration(file[text.Off:text.Off+text.Size], section{Size: text.Size}); len(got) == 0 {
		t.Fatal("a bare .text buffer derived nothing")
	}
}

// synthDerivedUnits is the symbol view of the same image, plus a new-side
// layout and the map between them. It is what the encoder alone sees.
func synthDerivedUnits() (old, new []namedUnit, maps []mapping) {
	old = []namedUnit{nu(0, 40, "a"), nu(48, 24, "b"), nu(96, 32, "c"), nu(128, 32, "d")}
	new = []namedUnit{nu(0, 40, "a"), nu(40, 24, "b"), nu(64, 32, "c"), nu(96, 32, "d")}
	for i := range new {
		maps = append(maps, mapping{Src: old[i].Off, SrcSize: old[i].Size, Dst: new[i].Off, DstSize: new[i].Size})
	}
	return old, new, maps
}

// TestDerivedStreamRoundTrip is the whole decoder contract in miniature: build
// the stream from symbols, throw the symbols away, and rebuild the map from
// the old image's bytes and the serialized stream alone.
func TestDerivedStreamRoundTrip(t *testing.T) {
	file, text := synthDerivedELF()
	oldText := file[text.Off : text.Off+text.Size]
	oldNamed, newNamed, maps := synthDerivedUnits()
	derived := deriveEnumeration(file, text)

	s, E, st, err := buildDerivedStream(derived, oldNamed, newNamed, maps, oldText, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Boundary) == 0 {
		t.Fatal("unit d's start is missed by every source; expected a boundary exception")
	}
	if len(s.Suppress) == 0 {
		t.Fatal("offset 112 is spurious; expected a suppression bitmap")
	}
	if st.Unrepresentable != 0 {
		t.Fatalf("synthetic map should be fully representable, got %d raw", st.Unrepresentable)
	}
	if len(E) != len(oldNamed) {
		t.Fatalf("enumeration has %d units, want the 4 real ones: %+v", len(E), E)
	}
	for i, u := range oldNamed {
		if E[i].Off != u.Off || E[i].Size != u.Size {
			t.Fatalf("enumeration unit %d is %+v, want %d+%d", i, E[i], u.Off, u.Size)
		}
	}

	// The decoder's path: serialized stream plus the old file, nothing else.
	parsed, err := parseDerivedStream(&planReader{b: s.marshal()})
	if err != nil {
		t.Fatal(err)
	}
	back, err := derivedUnits(parsed, file, text)
	if err != nil {
		t.Fatal(err)
	}
	sameUnits := func(a, b sidecarUnit) bool { return a.Off == b.Off && a.Size == b.Size }
	if !slices.EqualFunc(back, E, sameUnits) {
		t.Fatalf("decoder rebuilt %+v, want %+v", back, E)
	}
	got, err := reconstructMaps(parsed.base, back)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, maps) {
		t.Fatalf("reconstruction\n got %+v\nwant %+v", got, maps)
	}

	// A plan the decoder cannot have derived the same enumeration for is
	// refused rather than silently decoded against the wrong list.
	parsed.Derived++
	if _, err := derivedUnits(parsed, file, text); err == nil {
		t.Fatal("a mismatched enumeration length decoded anyway")
	}
}

// TestDerivedSuppressionSavesASize is the reason suppression exists: offset 112
// lies inside unit c, so an unsuppressed enumeration cuts c's padding-rule size
// from 32 to 8 and has to ship a fixup to repair it.
func TestDerivedSuppressionSavesASize(t *testing.T) {
	file, text := synthDerivedELF()
	oldText := file[text.Off : text.Off+text.Size]
	oldNamed, newNamed, maps := synthDerivedUnits()
	derived := deriveEnumeration(file, text)

	kept, E, _, err := buildDerivedStream(derived, oldNamed, newNamed, maps, oldText, false)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(E, func(u sidecarUnit) bool { return u.Off == 96 })
	if i < 0 {
		t.Fatalf("unit c missing from the unsuppressed enumeration: %+v", E)
	}
	if len(kept.SizeFixIdx) == 0 {
		t.Fatal("the spurious start at 112 should have forced a size fixup")
	}
	// Before the fixup the rule would have said 8; the encoder repaired it.
	if raw := paddingExtent(oldText, 96, 112); raw != 8 {
		t.Fatalf("padding rule over the spurious split gives %d, want 8", raw)
	}
	if E[i].Size != 32 {
		t.Fatalf("unit c ended at size %d, want 32", E[i].Size)
	}

	sup, Esup, _, err := buildDerivedStream(derived, oldNamed, newNamed, maps, oldText, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(sup.SizeFixIdx) != 0 {
		t.Fatalf("suppression should leave no size fixups, got %d bytes", len(sup.SizeFixIdx))
	}
	if len(Esup) != 4 {
		t.Fatalf("suppressed enumeration has %d units, want 4", len(Esup))
	}
}

// TestDerivedPlanRoundTrip runs the rung's own path: a derived-form structural
// plan, decoded with nothing but the old image.
func TestDerivedPlanRoundTrip(t *testing.T) {
	file, text := synthDerivedELF()
	oldText := file[text.Off : text.Off+text.Size]
	oldNamed, newNamed, maps := synthDerivedUnits()
	for i := range maps {
		maps[i].Copy = i%2 == 0
	}
	s, _, _, err := buildDerivedStream(deriveEnumeration(file, text), oldNamed, newNamed, maps, oldText, true)
	if err != nil {
		t.Fatal(err)
	}
	p := predictionPlan{OldAddr: synthTextAddr, NewAddr: 0x2000, TargetLen: 256, Maps: maps}
	b, err := p.marshalDerivedForm(oldText, s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unmarshalPlanFile(b, file, text, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != planDerived {
		t.Fatalf("mode %d, want planDerived", got.Mode)
	}
	if !slices.Equal(got.Maps, maps) {
		t.Fatalf("plan round trip\n got %+v\nwant %+v", got.Maps, maps)
	}
}

// TestSidecarNormalPlanBytesUnchanged is the constraint the derived-map rung
// had to keep: it is an addition, and the bytes the normal and sidecar modes
// emit did not move. The hashes are of the pre-derived-map tree.
func TestSidecarNormalPlanBytesUnchanged(t *testing.T) {
	oldNamed, newNamed, maps := synthSidecar()
	for i := range maps {
		maps[i].Copy = i%2 == 0
	}
	d, _, _, err := buildSidecarDelta(oldNamed, newNamed, maps)
	if err != nil {
		t.Fatal(err)
	}
	p := predictionPlan{OldAddr: 0x1000, NewAddr: 0x2000, TargetLen: 4096, Maps: maps}
	oldText := make([]byte, 4096)
	dense, err := p.marshal(oldText)
	if err != nil {
		t.Fatal(err)
	}
	side, err := p.marshalSidecarForm(oldText, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		b    []byte
		want string
	}{
		{"dense plan", dense, "e5d6811b4ed56891dac216021c57c1b91ba3c6991c3ff683a29bb9340b28dd99"},
		{"sidecar-form plan", side, "79c7f6dac9e4955afde4e933ad62e9212eaea4e5e7fbc6377592ce4be50a00e7"},
		{"sidecar delta stream", d.marshal(), "fefeea0b17f09886c4b1c48e9583310fd38f692a1dc5647c4f00604b7c68bbaa"},
	} {
		if got := fmt.Sprintf("%x", sha256.Sum256(c.b)); got != c.want {
			t.Fatalf("%s emitted %d bytes hashing %s, want %s", c.name, len(c.b), got, c.want)
		}
	}
}
