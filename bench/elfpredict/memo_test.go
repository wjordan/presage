package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestReferenceTargetMemo pins the one thing the memo must never get wrong.
// Every point in a plan is an index into this domain, so a stale or truncated
// entry would not fail loudly -- it would silently decode every point to the
// wrong address, and every measurement taken after it would be fiction.
func TestReferenceTargetMemo(t *testing.T) {
	// Its own memo directory: the entry count below is exact, and any other
	// test in the package that serializes a plan leaves entries in the shared
	// one, which would make this assertion depend on declaration order.
	prev := memoDir
	defer func() { memoDir = prev }()
	memoDir = t.TempDir()

	old := make([]byte, 64)
	// e8 rel32 at 0x10, so the walk finds one call and one target.
	old[0x10] = 0xe8
	old[0x11] = 0x08
	maps := []mapping{{Src: 0, SrcSize: 64}}
	const addr = 0x400000

	want := referenceTargets(old, maps, addr)
	if got := cachedReferenceTargets(old, maps, addr); !slices.Equal(got, want) {
		t.Fatalf("cold call returned %v, want %v", got, want)
	}
	entries, err := filepath.Glob(filepath.Join(memoDir, "reference-targets-*.bin"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("cold call left %d memo entries (%v)", len(entries), err)
	}
	// A second process would re-read the entry; drop the in-process map so the
	// disk path is the one under test.
	clear(targetsLocal)
	if got := cachedReferenceTargets(old, maps, addr); !slices.Equal(got, want) {
		t.Fatalf("warm call returned %v, want %v", got, want)
	}
	// A different old image must not read this entry.
	other := slices.Clone(old)
	other[0x11] = 0x0c
	if got := cachedReferenceTargets(other, maps, addr); slices.Equal(got, want) {
		t.Errorf("a changed old image reused the memoised domain %v", got)
	}
}

// TestMemoChecksum makes sure a corrupted entry falls through to the real work
// rather than being handed back.
func TestMemoChecksum(t *testing.T) {
	key := memoKey("unit-test", "code", "input")
	memoStore(key, []byte("payload"))
	if b, ok := memoLoad(key); !ok || string(b) != "payload" {
		t.Fatalf("round trip returned %q, %v", b, ok)
	}
	b, err := os.ReadFile(memoPath(key))
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(memoPath(key), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := memoLoad(key); ok {
		t.Error("a corrupted entry was accepted")
	}
}

// TestCodeIdentitySplit is the property the memo exists for: an edit under
// delta/ must not invalidate the symbol parse.
func TestCodeIdentitySplit(t *testing.T) {
	root := moduleDir()
	if root == "" {
		t.Skip("no checkout to hash")
	}
	if harnessCode() == codecCode() {
		t.Error("harness and codec identities coincide, so a delta/x86 edit would throw away the symbol parse")
	}
	if got := memoKey("symbols", harnessCode(), "x"); got == memoKey("symbols", codecCode(), "x") {
		t.Error("keys under the two identities coincide")
	}
}

// TestPackUnits round-trips the memoised symbol-parse value.
func TestPackUnits(t *testing.T) {
	units := []codeUnit{
		{Off: 0, Size: 16, Names: []nameID{{1}, {2}}},
		{Off: 16, Size: 4096, Names: nil},
		{Off: 1 << 20, Size: 3, Names: []nameID{{0xff}}},
	}
	st := symbolStats{FunctionSymbols: 9, AddressUnits: 3, CoveredBytes: 4115}
	got, gotStats, ok := unpackUnits(packUnits(units, st))
	if !ok || gotStats != st {
		t.Fatalf("unpack ok=%v stats=%+v", ok, gotStats)
	}
	if len(got) != len(units) {
		t.Fatalf("got %d units, want %d", len(got), len(units))
	}
	for i := range units {
		if got[i].Off != units[i].Off || got[i].Size != units[i].Size || !slices.Equal(got[i].Names, units[i].Names) {
			t.Errorf("unit %d: got %+v, want %+v", i, got[i], units[i])
		}
	}
	if _, _, ok := unpackUnits([]byte{0xff}); ok {
		t.Error("a truncated entry unpacked")
	}
}

// TestPlanArtifactsRoundTrip covers the memo entry that stands in for the
// whole of construction.
func TestPlanArtifactsRoundTrip(t *testing.T) {
	art := planArtifacts{
		Equivalence: []byte("eq"), AllMapped: []byte("map"), Derived: nil,
		Retarget: []byte("re"), Selected: []byte("sel"),
	}
	key := memoKey("plans", codecCode(), "in")
	storePlansMemo(key, art)
	got, ok := loadPlansMemo(key)
	if !ok {
		t.Fatal("plans memo missed after store")
	}
	for i, b := range got.blobs() {
		if string(b) != string(art.blobs()[i]) {
			t.Errorf("blob %d: got %q, want %q", i, b, art.blobs()[i])
		}
	}
	dir := t.TempDir()
	if err := art.write(dir); err != nil {
		t.Fatal(err)
	}
	back, err := readPlanArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range back.blobs() {
		if string(b) != string(art.blobs()[i]) {
			t.Errorf("file %d: got %q, want %q", i, b, art.blobs()[i])
		}
	}
}
