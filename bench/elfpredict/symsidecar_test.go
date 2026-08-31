package main

import (
	"slices"
	"testing"
)

func nu(off, size uint64, names ...string) namedUnit {
	return namedUnit{Off: off, Size: size, Names: names}
}

// synthSidecar is a handful of units covering every case the stream has to
// encode: a dropped old unit, two reorders, a resize, an insert, a layout
// fixup, a duplicated name the join can only resolve by order, and two
// correspondence exceptions -- one that renames a unit onto a dropped source
// and one that withdraws a mapping the join proposed.
func synthSidecar() (old, new []namedUnit, maps []mapping) {
	old = []namedUnit{
		nu(0, 16, "a"),
		nu(16, 16, "b"),
		nu(32, 16, "gone"),
		nu(48, 16, "d"),
		nu(64, 16, "e"),
		nu(80, 16, "dup"),
		nu(96, 16, "dup"),
	}
	new = []namedUnit{
		nu(0, 16, "a"),          // 0 -> old 0, in order
		nu(16, 32, "d"),         // 1 -> old 3, reorder + resize
		nu(48, 16, "b"),         // 2 -> old 1, reorder
		nu(64, 16, "brand new"), // 3 -> insert
		nu(80, 16, "e"),         // 4 -> old 4
		nu(100, 16, "dup"),      // 5 -> old 5 by order; layout fixup (not 96)
		nu(116, 16, "dup"),      // 6 -> old 6 by order
	}
	// The map: new 2 is really the old "gone" body renamed, new 4 is not
	// mapped at all, and the two duplicates are crossed over.
	pair := func(ni, oi int) mapping {
		return mapping{Src: old[oi].Off, SrcSize: old[oi].Size, Dst: new[ni].Off, DstSize: new[ni].Size}
	}
	maps = []mapping{pair(0, 0), pair(1, 3), pair(2, 2), pair(5, 6), pair(6, 5)}
	slices.SortFunc(maps, func(a, b mapping) int { return cmpU(a.Dst, b.Dst) })
	return old, new, maps
}

func TestSidecarTableRoundTrip(t *testing.T) {
	oldNamed, _, _ := synthSidecar()
	units := hashNamedUnits(oldNamed)
	got, err := unmarshalSidecarTable(marshalSidecarTable(units))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(units) {
		t.Fatalf("got %d units, want %d", len(got), len(units))
	}
	for i := range got {
		if got[i].Off != units[i].Off || got[i].Size != units[i].Size ||
			!slices.Equal(got[i].Hashes, units[i].Hashes) {
			t.Fatalf("unit %d round-tripped as %+v, want %+v", i, got[i], units[i])
		}
	}
}

// TestSidecarReconstruction is the gate the encoder itself applies: build the
// stream, then rebuild the map from the carried table and the stream alone.
func TestSidecarReconstruction(t *testing.T) {
	oldNamed, newNamed, maps := synthSidecar()
	d, oldUnits, st, err := buildSidecarDelta(oldNamed, newNamed, maps)
	if err != nil {
		t.Fatal(err)
	}
	if st.Inserts != 1 || st.Dropped != 1 || st.Resizes != 1 {
		t.Fatalf("expected 1 insert, 1 drop, 1 resize; got %+v", st)
	}
	if st.Reorders == 0 || st.Exceptions == 0 || st.Fixes == 0 {
		t.Fatalf("expected reorders, exceptions and layout fixups; got %+v", st)
	}
	if st.Unrepresentable != 0 {
		t.Fatalf("synthetic map should be fully representable, got %d raw", st.Unrepresentable)
	}
	// The stream must survive serialization, which is how it reaches the
	// decoder inside the plan.
	r := &planReader{b: d.marshal()}
	parsed, err := parseSidecarDelta(r)
	if err != nil {
		t.Fatal(err)
	}
	if !r.done() {
		t.Fatal("trailing delta-stream data")
	}
	got, err := reconstructMaps(parsed, oldUnits)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(maps)
	slices.SortFunc(want, func(a, b mapping) int { return cmpU(a.Dst, b.Dst) })
	if !slices.Equal(got, want) {
		t.Fatalf("reconstruction\n got %+v\nwant %+v", got, want)
	}
}

// TestSidecarPlanRoundTrip runs the whole path the rung runs: a sidecar-form
// structural plan, decoded with nothing but the carried table.
func TestSidecarPlanRoundTrip(t *testing.T) {
	oldNamed, newNamed, maps := synthSidecar()
	for i := range maps {
		maps[i].Copy = i%2 == 0
	}
	d, oldUnits, _, err := buildSidecarDelta(oldNamed, newNamed, maps)
	if err != nil {
		t.Fatal(err)
	}
	p := predictionPlan{OldAddr: 0x1000, NewAddr: 0x2000, TargetLen: 4096, Maps: maps}
	oldText := make([]byte, 4096)
	b, err := p.marshalSidecarForm(oldText, d)
	if err != nil {
		t.Fatal(err)
	}
	prev := decoderSidecar
	defer func() { decoderSidecar = prev }()

	decoderSidecar = nil
	if _, err := unmarshalPlan(b, oldText, nil); err == nil {
		t.Fatal("a sidecar-form plan decoded without the carried table")
	}
	decoderSidecar = oldUnits
	got, err := unmarshalPlan(b, oldText, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(maps)
	slices.SortFunc(want, func(a, b mapping) int { return cmpU(a.Dst, b.Dst) })
	if !slices.Equal(got.Maps, want) {
		t.Fatalf("plan round trip\n got %+v\nwant %+v", got.Maps, want)
	}
	if got.Mode != planSidecar {
		t.Fatalf("mode %d, want planSidecar", got.Mode)
	}
}

// TestSidecarNormalPlanUnchanged guards the constraint that matters most: the
// sidecar mode is an addition, and the ordinary plan's bytes did not move.
func TestSidecarNormalPlanUnchanged(t *testing.T) {
	_, _, maps := synthSidecar()
	p := predictionPlan{OldAddr: 0x1000, NewAddr: 0x2000, TargetLen: 4096, Maps: maps}
	oldText := make([]byte, 4096)
	b, err := p.marshal(oldText)
	if err != nil {
		t.Fatal(err)
	}
	if b[3] != planMagic[3] {
		t.Fatalf("plan magic changed: %q", b[:4])
	}
	got, err := unmarshalPlan(b, oldText, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != planDense {
		t.Fatalf("mode %d, want planDense", got.Mode)
	}
	if len(got.Maps) != len(maps) {
		t.Fatalf("got %d mappings, want %d", len(got.Maps), len(maps))
	}
}

// TestSidecarRollForward covers the two-hop path: the table the decoder can
// compute after hop 1 must describe hop 1's new layout exactly, inherit the
// carried hashes for kept units, and carry the one shipped hash per insert --
// and hop 2 must then encode against it.
func TestSidecarRollForward(t *testing.T) {
	oldNamed, newNamed, maps := synthSidecar()
	d, oldUnits, _, err := buildSidecarDelta(oldNamed, newNamed, maps)
	if err != nil {
		t.Fatal(err)
	}
	rp, err := replaySidecar(d, oldUnits)
	if err != nil {
		t.Fatal(err)
	}
	rolled := rollForwardTable(rp, oldUnits)
	fresh := hashNamedUnits(newNamed)
	if len(rolled) != len(fresh) {
		t.Fatalf("rolled %d units, want %d", len(rolled), len(fresh))
	}
	for i := range rolled {
		if rolled[i].Off != fresh[i].Off || rolled[i].Size != fresh[i].Size {
			t.Fatalf("unit %d rolled to %d+%d, want %d+%d", i,
				rolled[i].Off, rolled[i].Size, fresh[i].Off, fresh[i].Size)
		}
	}
	// new 3 is the insert; new 0 keeps old 0's hashes.
	if len(rolled[3].Hashes) != 1 || rolled[3].Hashes[0] != nameHash64("brand new") {
		t.Fatalf("insert rolled with %v", rolled[3].Hashes)
	}
	if !slices.Equal(rolled[0].Hashes, oldUnits[0].Hashes) {
		t.Fatalf("kept unit did not inherit its carried hashes: %v", rolled[0].Hashes)
	}
	// Hop 2 against the rolled table, with an identity map.
	var next []mapping
	for _, u := range fresh {
		next = append(next, mapping{Src: u.Off, SrcSize: u.Size, Dst: u.Off, DstSize: u.Size})
	}
	if _, _, err := buildSidecarDeltaFrom(rolled, nil, newNamed, next); err != nil {
		t.Fatalf("hop 2 against the rolled table: %v", err)
	}
}

func TestNameHash64Stable(t *testing.T) {
	// FNV-1a of "a" and of the empty string, so a change of hash function is
	// caught rather than silently invalidating every installed sidecar.
	if h := nameHash64(""); h != 14695981039346656037 {
		t.Fatalf("empty hash %d", h)
	}
	if h := nameHash64("a"); h != 12638187200555641996 {
		t.Fatalf("hash of \"a\" is %d", h)
	}
}
