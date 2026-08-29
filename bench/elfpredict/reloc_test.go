package main

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/wjordan/go-binsync/delta/x86"
)

func rela(entries ...relaEntry) []byte {
	b := make([]byte, 0, len(entries)*relaEntrySize)
	for _, e := range entries {
		var w [relaEntrySize]byte
		binary.LittleEndian.PutUint64(w[0:], e.slot)
		binary.LittleEndian.PutUint64(w[8:], e.info)
		binary.LittleEndian.PutUint64(w[16:], e.addend)
		b = append(b, w[:]...)
	}
	return b
}

// TestRelocJoinBySlot covers the case positional placement gets wrong: an
// entry whose slot moves past another lands at a different index in the
// linker-sorted new table, so its addend must be found by slot, not by
// position. The old table's second and third entries swap order in the new
// one, and a fourth entry is inserted.
func TestRelocJoinBySlot(t *testing.T) {
	const rel = relTypeRelative
	oldTable := rela(
		relaEntry{slot: 0x1000, info: rel, addend: 0x9000},
		relaEntry{slot: 0x1010, info: rel, addend: 0x9010},
		relaEntry{slot: 0x1020, info: rel, addend: 0x9020},
	)
	// The projection below moves slot 0x1010 to 0x1030, past 0x1020.
	newTable := rela(
		relaEntry{slot: 0x1000, info: rel, addend: 0x8000},
		relaEntry{slot: 0x1020, info: rel, addend: 0x8020},
		relaEntry{slot: 0x1028, info: rel, addend: 0x8028},
		relaEntry{slot: 0x1030, info: rel, addend: 0x8010},
	)
	move := map[uint64]uint64{
		0x1000: 0x1000, 0x1010: 0x1030, 0x1020: 0x1020,
		0x9000: 0x8000, 0x9010: 0x8010, 0x9020: 0x8020,
	}
	lookup := func(a uint64) x86.Target {
		if v, ok := move[a]; ok {
			return x86.Target{Addr: v, Known: true}
		}
		return x86.Target{}
	}

	old := make([]byte, 256)
	copy(old[64:], oldTable)
	target := make([]byte, 256)
	copy(target[64:], newTable)

	base := relocPlan{OldOff: 64, OldSize: uint64(len(oldTable)), NewOff: 64, NewSize: uint64(len(newTable))}
	p, err := buildRelocPlan(old, target, base, lookup)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 256)
	if _, err := applyReloc(out, old, p, lookup); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out[64:64+len(newTable)], newTable) {
		t.Fatalf("replayed table = %x, want %x", out[64:64+len(newTable)], newTable)
	}
	// The join should place all three carried addends exactly, so the
	// correction only has to supply the inserted entry.
	if len(p.AddendCorrection) > 64 {
		t.Errorf("addend correction is %d bytes; the join should have left almost nothing to fix", len(p.AddendCorrection))
	}
}

// TestRelocPairByRow measures the same fixture with the positional pairing the
// slot join replaced. It must still replay byte-exactly -- the correction just
// has more to fix -- and the flag must survive the plan round trip, while a
// plan without it marshals exactly as it did before the flag existed.
func TestRelocPairByRow(t *testing.T) {
	const rel = relTypeRelative
	oldTable := rela(
		relaEntry{slot: 0x1000, info: rel, addend: 0x9000},
		relaEntry{slot: 0x1010, info: rel, addend: 0x9010},
		relaEntry{slot: 0x1020, info: rel, addend: 0x9020},
	)
	newTable := rela(
		relaEntry{slot: 0x1000, info: rel, addend: 0x8000},
		relaEntry{slot: 0x1020, info: rel, addend: 0x8020},
		relaEntry{slot: 0x1028, info: rel, addend: 0x8028},
		relaEntry{slot: 0x1030, info: rel, addend: 0x8010},
	)
	move := map[uint64]uint64{
		0x1000: 0x1000, 0x1010: 0x1030, 0x1020: 0x1020,
		0x9000: 0x8000, 0x9010: 0x8010, 0x9020: 0x8020,
	}
	lookup := func(a uint64) x86.Target {
		if v, ok := move[a]; ok {
			return x86.Target{Addr: v, Known: true}
		}
		return x86.Target{}
	}
	old := make([]byte, 256)
	copy(old[64:], oldTable)
	target := make([]byte, 256)
	copy(target[64:], newTable)

	base := relocPlan{OldOff: 64, OldSize: uint64(len(oldTable)), NewOff: 64, NewSize: uint64(len(newTable)), PairByRow: true}
	p, err := buildRelocPlan(old, target, base, lookup)
	if err != nil {
		t.Fatal(err)
	}
	round, err := unmarshalRelocPlan(p.marshal())
	if err != nil {
		t.Fatal(err)
	}
	if !round.PairByRow {
		t.Fatal("pair-by-row flag did not survive the plan round trip")
	}
	out := make([]byte, 256)
	if _, err := applyReloc(out, old, round, lookup); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out[64:64+len(newTable)], newTable) {
		t.Fatalf("replayed table = %x, want %x", out[64:64+len(newTable)], newTable)
	}
	// Row 3 of the new table takes old row 3's addend rather than the one that
	// projects to its slot, so the correction has real work to do here.
	joined := base
	joined.PairByRow = false
	jp, err := buildRelocPlan(old, target, joined, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if len(jp.AddendCorrection) >= len(p.AddendCorrection) {
		t.Errorf("by-row addend correction %d B is not worse than the join's %d B", len(p.AddendCorrection), len(jp.AddendCorrection))
	}
	// The flag is a single appended byte and changes nothing before it, so a
	// plan that does not set it is byte-identical to one from before the flag.
	unflagged := p
	unflagged.PairByRow = false
	if b, want := unflagged.marshal(), p.marshal(); !bytes.Equal(b, want[:len(want)-1]) {
		t.Errorf("clearing the flag changed more than the trailing byte: %x vs %x", b, want)
	}
}

// TestRelocNoAddends covers the upstream-Zucchini shape: the plan models the
// slot column and nothing else, so the decoder must leave each entry's addend
// bytes exactly as the equivalence copy wrote them -- not zero them -- and the
// ordinary byte correction pays for the ones that are wrong.
func TestRelocNoAddends(t *testing.T) {
	const rel = relTypeRelative
	oldTable := rela(
		relaEntry{slot: 0x1000, info: rel, addend: 0x9000},
		relaEntry{slot: 0x1010, info: rel, addend: 0x9010},
		relaEntry{slot: 0x1020, info: rel, addend: 0x9020},
	)
	newTable := rela(
		relaEntry{slot: 0x1000, info: rel, addend: 0x8000},
		relaEntry{slot: 0x1020, info: rel, addend: 0x8020},
		relaEntry{slot: 0x1028, info: rel, addend: 0x8028},
		relaEntry{slot: 0x1030, info: rel, addend: 0x8010},
	)
	move := map[uint64]uint64{
		0x1000: 0x1000, 0x1010: 0x1030, 0x1020: 0x1020,
		0x9000: 0x8000, 0x9010: 0x8010, 0x9020: 0x8020,
	}
	lookup := func(a uint64) x86.Target {
		if v, ok := move[a]; ok {
			return x86.Target{Addr: v, Known: true}
		}
		return x86.Target{}
	}
	old := make([]byte, 256)
	copy(old[64:], oldTable)
	target := make([]byte, 256)
	copy(target[64:], newTable)

	base := relocPlan{OldOff: 64, OldSize: uint64(len(oldTable)), NewOff: 64, NewSize: uint64(len(newTable)), NoAddends: true}
	p, err := buildRelocPlan(old, target, base, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.AddendCorrection) != 0 {
		t.Errorf("plan carries %d bytes of addend correction; it should carry none", len(p.AddendCorrection))
	}
	round, err := unmarshalRelocPlan(p.marshal())
	if err != nil {
		t.Fatal(err)
	}
	if !round.NoAddends || round.PairByRow {
		t.Fatalf("flags did not survive the plan round trip: NoAddends=%v PairByRow=%v", round.NoAddends, round.PairByRow)
	}

	// The decoder writes into a buffer the equivalence copy has already
	// filled. Standing in for it with the old table's bytes makes the
	// untouched addends visible.
	out := make([]byte, 256)
	copy(out[64:], oldTable)
	if _, err := applyReloc(out, old, round, lookup); err != nil {
		t.Fatal(err)
	}
	for i := 0; i*relaEntrySize < len(newTable); i++ {
		got, want := out[64+i*relaEntrySize:], newTable[i*relaEntrySize:]
		if !bytes.Equal(got[:16], want[:16]) {
			t.Errorf("entry %d slot and info = %x, want %x", i, got[:16], want[:16])
		}
		// Rows 0-2 sit where the prediction left an old entry, so they keep
		// its addend; row 3 is past the end of the copy, so it keeps the zero.
		wantAddend := uint64(0)
		if i < len(oldTable)/relaEntrySize {
			wantAddend = binary.LittleEndian.Uint64(oldTable[i*relaEntrySize+16:])
		}
		if got := binary.LittleEndian.Uint64(got[16:]); got != wantAddend {
			t.Errorf("entry %d addend = %#x, want the prediction's %#x", i, got, wantAddend)
		}
	}

	// The flag is a single appended byte and changes nothing before it, so a
	// plan that does not set it is byte-identical to one from before the flag.
	unflagged := p
	unflagged.NoAddends = false
	if b, want := unflagged.marshal(), p.marshal(); !bytes.Equal(b, want[:len(want)-1]) {
		t.Errorf("clearing the flag changed more than the trailing byte: %x vs %x", b, want)
	}
	// A flag word this build does not know must be refused rather than ignored.
	if _, err := unmarshalRelocPlan(append(unflagged.marshal(), 1<<6)); err == nil {
		t.Error("an unknown relocation plan flag was accepted")
	}
}

// Go's linker writes its GLOB_DAT entries before the relative block; the
// plan has to carry them as a head, not shift the block over them.
func TestRelocHeadEntries(t *testing.T) {
	const rel = relTypeRelative
	const globDat = 0xc00000006
	oldTable := rela(
		relaEntry{slot: 0x2000, info: globDat, addend: 0},
		relaEntry{slot: 0x1000, info: rel, addend: 0x9000},
		relaEntry{slot: 0x1010, info: rel, addend: 0x9010},
	)
	newTable := rela(
		relaEntry{slot: 0x2008, info: globDat, addend: 0},
		relaEntry{slot: 0x1000, info: rel, addend: 0x8000},
		relaEntry{slot: 0x1010, info: rel, addend: 0x8010},
		relaEntry{slot: 0x1018, info: rel, addend: 0x8018},
	)
	move := map[uint64]uint64{0x2000: 0x2008, 0x1000: 0x1000, 0x1010: 0x1010, 0x9000: 0x8000, 0x9010: 0x8010}
	lookup := func(a uint64) x86.Target {
		if v, ok := move[a]; ok {
			return x86.Target{Addr: v, Known: true}
		}
		return x86.Target{}
	}
	old := make([]byte, 256)
	copy(old[64:], oldTable)
	target := make([]byte, 256)
	copy(target[64:], newTable)
	base := relocPlan{OldOff: 64, OldSize: uint64(len(oldTable)), NewOff: 64, NewSize: uint64(len(newTable))}
	p, err := buildRelocPlan(old, target, base, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if p.HeadCount != 1 || p.TailCount != 1 {
		t.Fatalf("head %d tail %d, want 1 and 1", p.HeadCount, p.TailCount)
	}
	rt, err := unmarshalRelocPlan(p.marshal())
	if err != nil {
		t.Fatal(err)
	}
	if rt.HeadCount != 1 {
		t.Fatalf("round trip lost the head count: %d", rt.HeadCount)
	}
	out := make([]byte, 256)
	if _, err := applyReloc(out, old, rt, lookup); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out[64:64+len(newTable)], newTable) {
		t.Fatalf("replayed table = %x, want %x", out[64:64+len(newTable)], newTable)
	}
	if len(p.TailCorrection) > 32 {
		t.Errorf("tail correction is %d bytes for one projected entry", len(p.TailCorrection))
	}
}
