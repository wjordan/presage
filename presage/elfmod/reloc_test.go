package elfmod

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/wjordan/presage/delta/x86"
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

// A flag word this build does not know -- the harness's experiment flags
// included -- must be refused rather than ignored.
func TestRelocRejectsUnknownFlag(t *testing.T) {
	p := relocPlan{OldOff: 64, OldSize: relaEntrySize, NewOff: 64, NewSize: relaEntrySize, RelCount: 1}
	for _, f := range []byte{1, 2, 8, 1 << 6} {
		if _, err := unmarshalRelocPlan(append(p.marshal(), f)); err == nil {
			t.Errorf("relocation plan flag %d was accepted", f)
		}
	}
	if _, err := unmarshalRelocPlan(p.marshal()); err != nil {
		t.Fatal(err)
	}
}
