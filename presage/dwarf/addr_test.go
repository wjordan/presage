package dwarf

import (
	"encoding/binary"
	"slices"
	"testing"
)

// A .debug_addr contribution is longer than its header record, and the
// header's length field is the whole contribution's: on a release that did
// not change the section, the layer must leave it alone.
func TestAddrUnitLengthSurvivesTheTable(t *testing.T) {
	le := binary.LittleEndian
	addr := []byte{0, 0, 0, 0, 5, 0, 8, 0}
	for i := range 40 {
		addr = le.AppendUint64(addr, 0x1000+uint64(i)*16)
	}
	le.PutUint32(addr, uint32(len(addr)-4))
	img, secs := testImage([][]byte{testUnit(nil, "a", 0, 0x1000, []string{"x"}, 12)}, nil)
	secs[Addr] = Sec{OldOff: uint64(len(img)), OldSize: uint64(len(addr))}
	img = append(img, addr...)
	same := func(uint64) (uint64, bool) { return 0, false }
	plan, ok := Build(img, img, testSecs(secs, secs), func(int) bool { return true }, same, nil)
	if !ok || len(plan.Records[Addr]) < 2 {
		t.Fatalf("plan %v, addr records %d", ok, len(plan.Records[Addr]))
	}
	got, err := Unmarshal(plan.Marshal(), img)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(img))
	if _, err := Apply(out, img, got, func(a uint64) (uint64, bool) { return a, true }, nil); err != nil {
		t.Fatal(err)
	}
	// The unit names no addr_base, so the entries pair with nothing; the
	// header record is what the table places, length field included.
	s := got.Secs[Addr]
	if pred := out[s.NewOff : s.NewOff+8]; !slices.Equal(pred, addr[:8]) {
		t.Fatalf(".debug_addr header changed on a self pair: %x, want %x", pred, addr[:8])
	}
}
