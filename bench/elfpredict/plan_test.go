package main

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"slices"
	"testing"

	"github.com/wjordan/go-binsync/delta"
	"github.com/wjordan/go-binsync/delta/x86"
)

func TestPlanRoundTripAndCorrection(t *testing.T) {
	old := []byte{0xe8, 0, 0, 0, 0, 0xc3, 0x90, 0x90}
	p := predictionPlan{
		OldAddr: 0x1000, NewAddr: 0x2000, TargetLen: 8,
		Maps: []mapping{{Src: 0, SrcSize: 6, Dst: 0, DstSize: 6, Copy: true}},
	}
	b, err := p.marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := predict(old, b, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0xe8, 0, 0, 0, 0, 0xc3, 1, 2}
	corr, err := delta.EncodeCorrection(got, want)
	if err != nil {
		t.Fatal(err)
	}
	if err := delta.ApplyCorrection(got, corr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("replay = %x, want %x", got, want)
	}
}

func TestPlanRejectsTruncation(t *testing.T) {
	old := []byte{0x90}
	p := predictionPlan{TargetLen: 1, Maps: []mapping{{SrcSize: 1, DstSize: 1}}}
	b, err := p.marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	for n := range b {
		if _, err := unmarshalPlan(b[:n], old, nil); err == nil {
			t.Fatalf("accepted truncation to %d bytes", n)
		}
	}
}

func TestEquivalencePlanRoundTrip(t *testing.T) {
	p := equivalencePlan{
		OldLen: 8, NewLen: 8,
		OldText: section{Size: 8}, NewText: section{Size: 8},
		SrcSkip: appendS(appendS(nil, 0), 2),
		DstSkip: appendU(appendU(nil, 0), 1),
		CopyLen: appendU(appendU(nil, 3), 2),
	}
	b, err := p.marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	got, decoded, copied, err := predictEquivalences([]byte{1, 2, 3, 4, 5, 6, 7, 8}, b)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 2, 3, 0xcc, 6, 7, 0xcc, 0xcc}
	if !bytes.Equal(got, want) || len(decoded.Eqs) != 2 || copied != 5 {
		t.Fatalf("prediction = %x, equivalences = %d, copied = %d", got, len(decoded.Eqs), copied)
	}
}

func TestCombinedSelectionReplay(t *testing.T) {
	old := []byte{1, 2, 3, 4, 9, 9, 7, 8}
	ep := equivalencePlan{
		OldLen: 8, NewLen: 8,
		OldText: section{Addr: 0x1000, Size: 8}, NewText: section{Addr: 0x2000, Size: 8},
		SrcSkip: appendS(nil, 0), DstSkip: appendU(nil, 0), CopyLen: appendU(nil, 4),
	}
	eqBytes, err := ep.marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	structure := predictionPlan{
		OldAddr: 0x1000, NewAddr: 0x2000, TargetLen: 8,
		Maps: []mapping{{Src: 4, SrcSize: 2, Dst: 4, DstSize: 2, Copy: true}},
	}
	structureBytes, err := structure.marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	plan := combinedPlan{Equivalences: eqBytes, Structure: structureBytes, Choices: []byte{1}}.marshal()
	got, stats, err := predictCombined(old, plan)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 2, 3, 4, 9, 9, 0xcc, 0xcc}
	if !bytes.Equal(got, want) || stats.SelectedFunctions != 1 || stats.SelectedBytes != 2 {
		t.Fatalf("prediction = %x, stats = %+v", got, stats)
	}
}

func TestEquivalenceDerivedRetargeting(t *testing.T) {
	old := make([]byte, 16)
	copy(old, []byte{0xe8, 5, 0, 0, 0})
	ep := equivalencePlan{
		OldLen: 16, NewLen: 16,
		OldText: section{Addr: 0x1000, Size: 16}, NewText: section{Addr: 0x2000, Size: 16},
		SrcSkip: appendS(appendS(nil, 0), 5),
		DstSkip: appendU(appendU(nil, 0), 7),
		CopyLen: appendU(appendU(nil, 5), 1),
	}
	eqBytes, err := ep.marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	structure := predictionPlan{OldAddr: 0x1000, NewAddr: 0x2000, TargetLen: 16}
	structureBytes, err := structure.marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	plan := combinedPlan{Equivalences: eqBytes, Structure: structureBytes}.marshal()
	got, stats, err := predictCombined(old, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 0xe8 || got[1] != 7 || stats.Relocation.Refs != 1 || stats.Relocation.Unknown != 0 {
		t.Fatalf("prediction = %x, stats = %+v", got[:5], stats.Relocation)
	}
}

// TestPlanColumnRoundTrip exercises what the single-mapping round trip cannot.
// No destination size is shipped: a source extent is guessed from the old
// image's padding and corrected, and the destination size follows from that.
// The plan therefore has to survive trailing islands, a backward source jump,
// a function whose source is a different size from its destination, and a
// source whose padding does not reach the next function.
func TestPlanColumnRoundTrip(t *testing.T) {
	want := predictionPlan{
		OldAddr: 0x1000, NewAddr: 0x2000, TargetLen: 64,
		Maps: []mapping{
			{Src: 0x40, SrcSize: 8, Dst: 0, DstSize: 8, Copy: true},
			{Src: 0x50, SrcSize: 12, Dst: 12, DstSize: 10}, // 4-byte island before it
			{Src: 0x20, SrcSize: 6, Dst: 22, DstSize: 6},   // source jumps backwards
			{Src: 0x80, SrcSize: 5, Dst: 40, DstSize: 7, Copy: true},
		},
		// Two points sharing a shift and one breaking it, since the column
		// codes the change in shift rather than the address.
		Points: []addressPoint{{Old: 0x1100, New: 0x2100}, {Old: 0x1140, New: 0x2140}, {Old: 0x1180, New: 0x2170}},
		Ranges: []addressRange{{Old: 0x1000, New: 0x2000, Size: 0x40}},
	}
	// Old text with each body followed by 0xcc padding up to the next source,
	// except 0x50's, which is followed by an unpadded island the guess will
	// overshoot and the residual has to correct.
	old := bytes.Repeat([]byte{0xcc}, 0x100)
	for _, body := range [][2]int{{0x20, 6}, {0x40, 8}, {0x50, 12}, {0x80, 5}} {
		for i := body[0]; i < body[0]+body[1]; i++ {
			old[i] = 0x90
		}
	}
	old[0x60] = 0x90 // island inside the guessed extent of 0x50
	// A call at 0x40 whose target is 0x1100 puts the first point on a walked
	// reference target, so the point column exercises a real index and the two
	// later points exercise offsets from it.
	old[0x40] = 0xe8
	binary.LittleEndian.PutUint32(old[0x41:], 0xbb)
	b, err := want.marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unmarshalPlan(b, old, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	// The three sources whose padding runs to the next function need no
	// correction at all; only the one with the island does.
	if n := len(sourceExtents([]uint64{0x20, 0x40, 0x50, 0x80}, old)); n != 4 {
		t.Fatalf("guessed %d extents, want 4", n)
	}
	if got := sourceExtents([]uint64{0x20, 0x40, 0x50, 0x80}, old)[0x50]; got != 0x11 {
		t.Errorf("extent of 0x50 guessed as %#x, want 0x11", got)
	}
	// The walked target list must contain the call's destination, or the point
	// column would be coding raw addresses against the leading zero.
	targets := referenceTargets(old, want.Maps, want.OldAddr)
	if !slices.Contains(targets, uint64(0x1100)) {
		t.Errorf("walked targets %v do not include the call destination 0x1100", targets)
	}
}

// TestSparseStructureSerializes covers what broke the first whole-image run:
// identical-code folding lets several destination functions share one source
// address, so shift breakpoints can collide and the range map has to come out
// strictly increasing with positive extents or it will not serialize.
func TestSparseStructureSerializes(t *testing.T) {
	full := predictionPlan{
		OldAddr: 0x1000, NewAddr: 0x2000, TargetLen: 128,
		Maps: []mapping{
			{Src: 0x00, SrcSize: 8, Dst: 0, DstSize: 8},
			{Src: 0x20, SrcSize: 8, Dst: 8, DstSize: 8},  // shift changes
			{Src: 0x20, SrcSize: 8, Dst: 16, DstSize: 8}, // folded: same source
			{Src: 0x40, SrcSize: 8, Dst: 24, DstSize: 8},
		},
	}
	choices := []byte{0b0000_1010} // retain maps 1 and 3
	old := bytes.Repeat([]byte{0xcc}, 0x100)
	for _, at := range []int{0x00, 0x20, 0x40} {
		for i := at; i < at+8; i++ {
			old[i] = 0x90
		}
	}
	sparse := sparseStructure(full, choices, 0x100)
	b, err := sparse.marshal(old)
	if err != nil {
		t.Fatalf("sparse plan did not serialize: %v", err)
	}
	got, err := unmarshalPlan(b, old, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != planSparse {
		t.Error("round trip lost the sparse flag")
	}
	if len(got.Maps) != 2 {
		t.Errorf("retained %d mappings, want 2", len(got.Maps))
	}
	for i, ar := range got.Ranges {
		if ar.Size == 0 {
			t.Errorf("range %d has no extent", i)
		}
		if i > 0 && ar.Old < got.Ranges[i-1].Old+got.Ranges[i-1].Size {
			t.Errorf("range %d overlaps its predecessor", i)
		}
	}
}

// TestBoundaryDetection covers the two derivations EPP8 rests on: where the
// old image says a function begins, and where the new one is expected to.
func TestBoundaryDetection(t *testing.T) {
	// Three padded functions. The one at 4 is not eight-byte aligned, so the
	// detector must skip it however plainly the padding marks it.
	old := []byte{
		0x90, 0x90, 0xcc, 0xcc, // [0,2) then padding
		0x90, 0xcc, 0xcc, 0xcc, // [4,5): unaligned, not a candidate
		0x90, 0x90, 0x90, 0xcc, // [8,11) then padding
		0xcc, 0xcc, 0xcc, 0xcc,
		0x90, 0x90, 0x90, 0x90, // [16,20)
	}
	got := detectBoundaries(old)
	want := []uint64{0, 8, 16}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("boundaries = %v, want %v", got, want)
	}
	// A source that is not a boundary is named by the one below it.
	if i := boundaryIndex(got, 10); i != 1 {
		t.Errorf("boundaryIndex(10) = %d, want 1", i)
	}
	if i := boundaryIndex(got, 4); i != 0 {
		t.Errorf("boundaryIndex(4) = %d, want 0", i)
	}
	if i := boundaryIndex(got, 0); i != 0 {
		t.Errorf("boundaryIndex(0) = %d, want 0", i)
	}
	for _, c := range [][2]uint64{{0, 0}, {1, 16}, {16, 16}, {17, 32}, {32, 32}} {
		if g := alignedGuess(c[0]); g != c[1] {
			t.Errorf("alignedGuess(%d) = %d, want %d", c[0], g, c[1])
		}
	}
}

// TestPredictedEquivalenceSources covers the split source column: an
// equivalence starting inside a mapped function is written as the difference
// from what the map implies, one outside .text keeps the skip it always had,
// and which column an entry used follows from its destination alone.
func TestPredictedEquivalenceSources(t *testing.T) {
	ep := equivalencePlan{
		OldLen: 32, NewLen: 32,
		OldText: section{Addr: 0x1000, Off: 8, Size: 16},
		NewText: section{Addr: 0x2000, Off: 8, Size: 16},
		Eqs: []equivalence{
			{Src: 0, Dst: 0, N: 4},   // before .text: no mapping can predict it
			{Src: 12, Dst: 10, N: 4}, // inside the mapped function at .text+2
		},
		// The plain column, which marshal(nil) keeps verbatim: each source as
		// the signed distance from where the previous run ended.
		SrcSkip: appendS(appendS(nil, 0), 8),
		DstSkip: appendU(appendU(nil, 0), 6),
		CopyLen: appendU(appendU(nil, 4), 4),
	}
	pred := &srcPredictor{
		maps:   []mapping{{Src: 2, SrcSize: 8, Dst: 2, DstSize: 8}},
		oldOff: 8, newOff: 8, newSize: 16,
	}
	// The map puts .text+2 at old .text+2, that is file offset 10; the run
	// actually starts at 12, so the residual is two and not the address.
	if base, ok := pred.at(10); !ok || base != 10 {
		t.Fatalf("predictor said (%d, %v) for .text+2, want (10, true)", base, ok)
	}
	if _, ok := pred.at(0); ok {
		t.Error("predictor claimed a destination outside .text")
	}
	b, err := ep.marshal(pred)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseEquivalencePlan(b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Predicted {
		t.Fatal("round trip lost the predicted flag")
	}
	if len(got.SrcResidual) == 0 || len(got.SrcSkip) == 0 {
		t.Fatalf("columns are %d skip and %d residual bytes, want both used", len(got.SrcSkip), len(got.SrcResidual))
	}
	eqs, err := decodeEquivalences(got, pred)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(eqs, ep.Eqs) {
		t.Fatalf("decoded %+v, want %+v", eqs, ep.Eqs)
	}
	// A decoder without the map must refuse rather than read the wrong column.
	if _, err := decodeEquivalences(got, nil); err == nil {
		t.Error("a plan with predicted sources decoded without the function map")
	}
	// And the reverse.
	plain, err := ep.marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseEquivalencePlan(plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeEquivalences(parsed, pred); err == nil {
		t.Error("a plan without predicted sources decoded with the function map")
	}
}

// TestPredictedSourcesNeedTheirOwnMap covers what broke the sparse rung: the
// source column is written against one function map, so a plan that ships a
// different map cannot decode it. predictImage builds the predictor out of the
// plan's own structure, which makes the two agree by construction; this pins
// the failure that follows if an encoder ever ships them mismatched.
func TestPredictedSourcesNeedTheirOwnMap(t *testing.T) {
	ep := equivalencePlan{
		OldLen: 64, NewLen: 64,
		OldText: section{Addr: 0x1000, Off: 8, Size: 32},
		NewText: section{Addr: 0x2000, Off: 8, Size: 32},
		Eqs:     []equivalence{{Src: 12, Dst: 10, N: 8}},
		DstSkip: appendU(nil, 10),
		CopyLen: appendU(nil, 8),
	}
	geom := func(maps []mapping) *srcPredictor {
		return &srcPredictor{maps: maps, oldOff: 8, newOff: 8, newSize: 32}
	}
	dense := geom([]mapping{{Src: 2, SrcSize: 8, Dst: 2, DstSize: 8}})
	// The same destination, sourced from somewhere else entirely.
	sparse := geom([]mapping{{Src: 20, SrcSize: 8, Dst: 2, DstSize: 8}})
	b, err := ep.marshal(dense)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseEquivalencePlan(b)
	if err != nil {
		t.Fatal(err)
	}
	eqs, err := decodeEquivalences(got, sparse)
	if err == nil && reflect.DeepEqual(eqs, ep.Eqs) {
		t.Fatal("the wrong function map decoded the source column correctly, so a mismatch could pass unnoticed")
	}
}

// A planGoDerived plan carries no map: the decoder takes it from the
// Go-table plan beside it, and the plan refuses to decode without one.
func TestGoDerivedPlanSerializes(t *testing.T) {
	old := bytes.Repeat([]byte{0x90}, 0x100)
	maps := []mapping{
		{Src: 0x00, SrcSize: 0x20, Dst: 0x00, DstSize: 0x20, Copy: true},
		{Src: 0x40, SrcSize: 0x20, Dst: 0x30, DstSize: 0x28, Copy: true},
	}
	p := predictionPlan{OldAddr: 0x1000, NewAddr: 0x2000, TargetLen: 0x100, Mode: planGoDerived, Maps: maps}
	b, err := p.marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	dense := p
	dense.Mode = planDense
	db, err := dense.marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) >= len(db) {
		t.Errorf("derived plan is %d bytes, dense %d: the map columns should be gone", len(b), len(db))
	}
	if _, err := unmarshalPlan(b, old, nil); err == nil {
		t.Error("derived plan decoded without a map to derive")
	}
	prior := func(uint64) x86.Target { return x86.Target{} }
	got, err := unmarshalPlan(b, old, func() (derivedMap, error) { return derivedMap{maps: maps, prior: prior}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != planGoDerived || !slices.Equal(got.Maps, maps) || got.Prior == nil {
		t.Errorf("round trip: mode %v, maps %v, prior set %v", got.Mode, got.Maps, got.Prior != nil)
	}
}
