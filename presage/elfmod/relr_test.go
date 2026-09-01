package elfmod

import (
	"encoding/binary"
	"slices"
	"testing"

	"github.com/wjordan/presage/delta/x86"
)

// packRelr is the linker's side of the encoding, here only so the parser can
// be checked against a table built from a known slot list.
func packRelr(addrs []uint64) []byte {
	var out []byte
	for i := 0; i < len(addrs); {
		base := addrs[i]
		i++
		out = binary.LittleEndian.AppendUint64(out, base)
		base += 8
		for {
			var bitmap uint64
			for i < len(addrs) && addrs[i] >= base && (addrs[i]-base)/8 < 63 {
				bitmap |= 1 << ((addrs[i] - base) / 8)
				i++
			}
			if bitmap == 0 {
				break
			}
			out = binary.LittleEndian.AppendUint64(out, bitmap<<1|1)
			base += 63 * 8
		}
	}
	return out
}

func relrSlots(table []byte) []uint64 {
	var out []uint64
	eachRelrSlot(table, func(a uint64) { out = append(out, a) })
	return out
}

func TestRelrRoundTrip(t *testing.T) {
	// A run dense enough to need two chained bitmaps, then a jump far
	// enough away to force a fresh address entry.
	want := []uint64{0x10000, 0x10008, 0x10010, 0x10040}
	for i := range 70 {
		want = append(want, 0x10100+uint64(i)*8)
	}
	want = append(want, 0x90000, 0x90008, 0xa0000)
	slices.Sort(want)

	table := packRelr(want)
	if len(table)%8 != 0 || len(table) == 0 {
		t.Fatalf("packed table is %d bytes", len(table))
	}
	// The point of the encoding: far fewer words than slots.
	if len(table)/8 >= len(want) {
		t.Errorf("packed %d slots into %d words, expected fewer", len(want), len(table)/8)
	}
	if got := relrSlots(table); !slices.Equal(got, want) {
		t.Fatalf("parsed %#x\n    want %#x", got, want)
	}
	// A truncated table stops rather than walking off the end.
	if got := relrSlots(table[:len(table)-3]); len(got) >= len(want) {
		t.Errorf("a truncated table parsed %d slots, want fewer than %d", len(got), len(want))
	}
}

func TestRelrPlanRoundTrip(t *testing.T) {
	p := relrPlan{OldOff: 0x123456, OldSize: 0x789a}
	if b := p.marshal(); len(b) > 8 {
		t.Errorf("plan is %d bytes, the layer must stay under 8", len(b))
	}
	got, err := unmarshalRelrPlan(p.marshal())
	if err != nil || got != p {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	if _, err := unmarshalRelrPlan(append(p.marshal(), 0)); err == nil {
		t.Error("trailing plan bytes must be refused")
	}
}

// The decoder path on a synthetic pair: the old image's RELR slots hold
// pointers into a section that moved, and every one of them must come out
// pointing where the oracle says its target went. Words the table does not
// name must keep the bytes the equivalence copy laid down.
func TestRelrApply(t *testing.T) {
	const dataAddr, dataOff = uint64(0x20000), uint64(0x1000)
	const dataSize = 0x100
	const tableOff = uint64(0x800)
	const newDataOff = uint64(0x1400) // the section moved in the new image

	slots := []uint64{dataAddr + 0x10, dataAddr + 0x18, dataAddr + 0x80}
	table := packRelr(slots)

	old := make([]byte, 0x2000)
	copy(old[tableOff:], table)
	for _, a := range slots {
		// Each slot points at its own address plus a marker, so a moved
		// pointer is distinguishable from a copied one.
		binary.LittleEndian.PutUint64(old[dataOff+(a-dataAddr):], a+0x1000)
	}
	binary.LittleEndian.PutUint64(old[dataOff+0x20:], 0xdeadbeef) // not a slot

	out := make([]byte, 0x2000)
	copy(out[newDataOff:newDataOff+dataSize], old[dataOff:dataOff+dataSize])

	ep := equivalencePlan{OldLen: uint64(len(old)), NewLen: uint64(len(out)),
		Eqs: []equivalence{{Src: dataOff, Dst: newDataOff, N: dataSize}}}
	secs := sectionMap{{Old: dataAddr, New: dataOff, Size: dataSize}}
	pointer := func(a uint64) x86.Target {
		if a < dataAddr {
			return x86.Target{}
		}
		return x86.Target{Addr: a + 0x40000, Known: true}
	}

	st := applyRelr(out, old, relrPlan{OldOff: tableOff, OldSize: uint64(len(table))},
		secs, newSourceEquivalenceMapper(ep), pointer)
	if st.Slots != len(slots) || st.Retargeted != len(slots) || st.Unknown != 0 || st.Unplaced != 0 {
		t.Fatalf("stats %+v, want %d slots all retargeted", st, len(slots))
	}
	for _, a := range slots {
		off := newDataOff + (a - dataAddr)
		want := a + 0x1000 + 0x40000
		if got := binary.LittleEndian.Uint64(out[off:]); got != want {
			t.Errorf("slot %#x holds %#x, want %#x", a, got, want)
		}
	}
	if got := binary.LittleEndian.Uint64(out[newDataOff+0x20:]); got != 0xdeadbeef {
		t.Errorf("a word the table does not name was rewritten to %#x", got)
	}

	// An oracle that knows nothing leaves the copied bytes alone.
	copy(out[newDataOff:newDataOff+dataSize], old[dataOff:dataOff+dataSize])
	st = applyRelr(out, old, relrPlan{OldOff: tableOff, OldSize: uint64(len(table))},
		secs, newSourceEquivalenceMapper(ep), func(uint64) x86.Target { return x86.Target{} })
	if st.Unknown != len(slots) || st.Retargeted != 0 {
		t.Fatalf("stats %+v, want every slot unknown", st)
	}
	if got := binary.LittleEndian.Uint64(out[newDataOff+0x10:]); got != dataAddr+0x10+0x1000 {
		t.Errorf("an unknown slot was rewritten to %#x", got)
	}
}
