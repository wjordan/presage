package elfmod

import (
	"encoding/binary"
	"testing"
)

// derivedFixture lays a 128-byte code window holding five function units and
// the map of a plausible rebuild, chosen so that every column of the derived
// stream carries something:
//
//	old  0(16) 32(16) 64(8, padding rule says 16) 96(16) 112(16, undetectable)
//	new  0←old2  16←old0  32←old1 (resized)  64 inserted  96←old4 (fixed up)
//	                                                       old3 dropped
func derivedFixture() (oldText []byte, win section, oldUnits, newUnits []codeUnit, maps []mapping) {
	oldText = make([]byte, 128)
	for i := range oldText {
		oldText[i] = 0xcc
	}
	body := func(off, n int) {
		for i := off; i < off+n; i++ {
			oldText[i] = 0x90
		}
	}
	body(0, 16)
	body(32, 16)
	body(64, 16) // the symbol says 8: the padding rule over-reads, and pays a fixup
	body(96, 32) // two units back to back, so the second start is undetectable
	// A call from the first body to offset 64, which is evidence source E1.
	oldText[0] = 0xe8
	binary.LittleEndian.PutUint32(oldText[1:], uint32(int32(64-5)))

	win = section{Addr: 0x1000, Off: 0, Size: 128}
	oldUnits = []codeUnit{{Off: 0, Size: 16}, {Off: 32, Size: 16}, {Off: 64, Size: 8}, {Off: 96, Size: 16}, {Off: 112, Size: 16}}
	newUnits = []codeUnit{{Off: 0, Size: 16}, {Off: 16, Size: 16}, {Off: 32, Size: 24}, {Off: 64, Size: 16}, {Off: 96, Size: 16}}
	maps = []mapping{
		{Src: 64, SrcSize: 8, Dst: 0, DstSize: 16, Copy: true},
		{Src: 0, SrcSize: 16, Dst: 16, DstSize: 16},
		{Src: 32, SrcSize: 16, Dst: 32, DstSize: 24, Copy: true},
		{Src: 112, SrcSize: 16, Dst: 96, DstSize: 16},
	}
	return oldText, win, oldUnits, newUnits, maps
}

// modeOf reads the map-mode byte that follows the plan's three geometry
// varints.
func modeOf(t *testing.T, b []byte) byte {
	t.Helper()
	r := &planReader{b: b}
	r.u()
	r.u()
	r.u()
	m := r.byteAt()
	if r.err != nil {
		t.Fatal(r.err)
	}
	return m
}

func TestDeriveEnumerationIsDeterministic(t *testing.T) {
	t.Parallel()
	oldText, win, _, _, _ := derivedFixture()
	got := deriveEnumeration(oldText, win)
	want := []uint64{0, 32, 64, 96}
	if len(got) != len(want) {
		t.Fatalf("enumeration %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("enumeration %v, want %v", got, want)
		}
	}
	// Same bytes, same answer, however many times it is asked.
	for i := 0; i < 3; i++ {
		again := deriveEnumeration(append([]byte(nil), oldText...), win)
		if len(again) != len(got) {
			t.Fatalf("run %d: %v", i, again)
		}
		for k := range again {
			if again[k] != got[k] {
				t.Fatalf("run %d: %v vs %v", i, again, got)
			}
		}
	}
	// 112 is a real unit start the derivation cannot see: it follows a body
	// byte, not padding, and nothing calls it. That is a boundary exception.
	for _, off := range got {
		if off == 112 {
			t.Fatal("the enumeration found an undetectable start")
		}
	}
}

// The whole decoder path: build the derived form, serialize the structural
// plan, and reconstruct the map from the old image's bytes and the plan alone.
func TestDerivedMapPlanRoundTrip(t *testing.T) {
	t.Parallel()
	oldText, win, oldUnits, newUnits, maps := derivedFixture()
	p := predictionPlan{OldAddr: win.Addr, NewAddr: 0x9000, TargetLen: 128, Maps: maps,
		Ranges: []addressRange{{Old: 0x3000, New: 0x4000, Size: 0x100}}}

	ds, st, ok := buildDerivedForm(p, oldText, win, oldUnits, newUnits)
	if !ok {
		t.Fatal("the derived form declined a representable map")
	}
	if st.Boundary != 1 || st.Dropped != 1 || st.Inserts != 1 || st.Resizes == 0 || st.Reorders == 0 || st.Fixes != 1 {
		t.Fatalf("stats %+v: the fixture should exercise every column", st)
	}
	if len(ds.SizeFixIdx) == 0 {
		t.Fatal("no size fixup for the unit whose padding-rule size is wrong")
	}

	b, err := p.marshalMode(oldText, ds)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unmarshalPlan(b, oldText, win)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Maps) != len(maps) {
		t.Fatalf("%d mappings, want %d", len(got.Maps), len(maps))
	}
	for i := range maps {
		if got.Maps[i] != maps[i] {
			t.Fatalf("mapping %d: %+v, want %+v", i, got.Maps[i], maps[i])
		}
	}
	if len(got.Ranges) != 1 || got.Ranges[0] != p.Ranges[0] {
		t.Fatalf("ranges %+v", got.Ranges)
	}
	dense, err := p.marshal(oldText)
	if err != nil {
		t.Fatal(err)
	}
	if modeOf(t, b) != planDerived || modeOf(t, dense) != planDense {
		t.Fatalf("mode bytes %d / %d", modeOf(t, b), modeOf(t, dense))
	}
	// Truncation is refused, never misread.
	for n := 0; n < len(b); n++ {
		if _, err := unmarshalPlan(b[:n], oldText, win); err == nil {
			t.Fatalf("truncated derived plan of %d bytes accepted", n)
		}
	}
}

// The stream ships the enumeration's length, so an old image whose own
// derivation disagrees refuses the plan rather than decoding a shifted map.
func TestDerivedMapRefusesDivergentEnumeration(t *testing.T) {
	t.Parallel()
	oldText, win, oldUnits, newUnits, maps := derivedFixture()
	p := predictionPlan{OldAddr: win.Addr, NewAddr: 0x9000, TargetLen: 128, Maps: maps}
	ds, _, ok := buildDerivedForm(p, oldText, win, oldUnits, newUnits)
	if !ok {
		t.Fatal("the derived form declined a representable map")
	}
	b, err := p.marshalMode(oldText, ds)
	if err != nil {
		t.Fatal(err)
	}
	// Fill the padding byte before the second unit: that start is no longer
	// detectable and the derivation returns one entry fewer.
	other := append([]byte(nil), oldText...)
	other[31] = 0x90
	if len(deriveEnumeration(other, win)) == len(deriveEnumeration(oldText, win)) {
		t.Fatal("the fixture's perturbation did not change the derivation")
	}
	if _, err := unmarshalPlan(b, other, win); err == nil {
		t.Fatal("a plan built against a different enumeration was accepted")
	}
}

// A window with no symbols cannot be expressed and falls back to the dense
// columns rather than shipping a plan the decoder would reject.
func TestDerivedMapDeclinesWithoutUnits(t *testing.T) {
	t.Parallel()
	oldText, win, _, _, maps := derivedFixture()
	p := predictionPlan{OldAddr: win.Addr, NewAddr: 0x9000, TargetLen: 128, Maps: maps}
	if _, _, ok := buildDerivedForm(p, oldText, win, nil, nil); ok {
		t.Fatal("the derived form accepted a window with no units")
	}
}

// An identity rebuild is where the derived form is cheapest: the delta
// stream is a handful of bytes for hundreds of mappings. The structural
// plan's sanity bound on the mapping count must allow for that — under the
// dense columns a mapping costs at least five bytes, and the same bound
// rejects a perfectly good derived plan (found by the corpus gate's
// self-prediction case).
func TestDerivedMapIdentityRebuild(t *testing.T) {
	t.Parallel()
	const units, stride = 256, 16
	code := make([]byte, units*stride)
	for i := range code {
		code[i] = 0xcc
	}
	var old, new []codeUnit
	var maps []mapping
	for i := 0; i < units; i++ {
		off := uint64(i * stride)
		for k := off; k < off+8; k++ {
			code[k] = 0x90
		}
		old = append(old, codeUnit{Off: off, Size: 8})
		new = append(new, codeUnit{Off: off, Size: 8})
		maps = append(maps, mapping{Src: off, SrcSize: 8, Dst: off, DstSize: 8, Copy: true})
	}
	win := section{Addr: 0x1000, Off: 0, Size: uint64(len(code))}
	p := predictionPlan{OldAddr: win.Addr, NewAddr: win.Addr, TargetLen: uint64(len(code)), Maps: maps}
	ds, _, ok := buildDerivedForm(p, code, win, old, new)
	if !ok {
		t.Fatal("the derived form declined an identity rebuild")
	}
	b, err := p.marshalMode(code, ds)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) >= units {
		t.Fatalf("plan is %d bytes for %d mappings: the fixture no longer exercises the bound", len(b), units)
	}
	got, err := unmarshalPlan(b, code, win)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Maps) != units {
		t.Fatalf("%d mappings, want %d", len(got.Maps), units)
	}
	for i := range maps {
		if got.Maps[i] != maps[i] {
			t.Fatalf("mapping %d: %+v, want %+v", i, got.Maps[i], maps[i])
		}
	}
}
