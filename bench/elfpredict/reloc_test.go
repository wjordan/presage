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
