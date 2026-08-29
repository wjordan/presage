package main

import (
	"encoding/binary"
	"slices"
	"testing"

	"github.com/wjordan/go-binsync/delta/x86"
)

// A DWARF 5 abbreviation table: 1 = compile_unit (name string, stmt_list
// sec_offset, low_pc addr) with children; 2 = base_type (name string);
// 3 = variable (name string, type ref_addr, location sec_offset).
var testAbbrev = []byte{
	1, 0x11, 1, 0x03, 0x08, 0x10, 0x17, 0x11, 0x01, 0, 0,
	2, 0x24, 0, 0x03, 0x08, 0, 0,
	3, 0x34, 0, 0x03, 0x08, 0x49, 0x10, 0x02, 0x17, 0, 0,
	0,
}

// testUnit builds one unit: header, unit DIE, base type, then variables
// naming the type by its section offset, with an optional extra variable.
func testUnit(info []byte, name string, stmt uint32, pc uint64, vars []string, loc uint32) []byte {
	le := binary.LittleEndian
	start := len(info)
	info = append(info, 0, 0, 0, 0, 5, 0, 1, 8, 0, 0, 0, 0) // length, version 5, unit_type, addr size 8, abbrev 0
	info = append(info, 1)
	info = append(info, name...)
	info = append(info, 0)
	info = le.AppendUint32(info, stmt)
	info = le.AppendUint64(info, pc)
	typeOff := uint32(len(info))
	info = append(info, 2)
	info = append(info, "int"...)
	info = append(info, 0)
	for i, v := range vars {
		info = append(info, 3)
		info = append(info, v...)
		info = append(info, 0)
		info = le.AppendUint32(info, typeOff)
		info = le.AppendUint32(info, loc+uint32(i)*8)
	}
	info = append(info, 0)
	le.PutUint32(info[start:], uint32(len(info)-start-4))
	return info
}

func testImage(units [][]byte, extra []byte) (*image, [dwarfSecCount]debugSec) {
	var secs [dwarfSecCount]debugSec
	data := make([]byte, 64)
	put := func(k int, b []byte) {
		secs[k] = debugSec{OldOff: uint64(len(data)), OldSize: uint64(len(b))}
		data = append(data, b...)
	}
	var info []byte
	for _, u := range units {
		info = append(info, u...)
	}
	put(dwInfo, info)
	put(dwAbbrev, testAbbrev)
	put(dwLoclists, extra)
	im := &image{Data: data, Debug: map[string]section{}}
	for k, s := range secs {
		if s.OldSize != 0 {
			im.Debug[dwarfSecNames[k]] = section{Off: s.OldOff, Size: s.OldSize}
		}
	}
	return im, secs
}

// One location list per variable: DW_LLE_offset_pair with a 1-byte
// description, then end of list; the new image inserts a list.
func testLists(ids ...byte) []byte {
	b := []byte{0, 0, 0, 0, 5, 0, 8, 0, 0, 0, 0, 0}
	for _, id := range ids {
		b = append(b, 4, 0, 8, 1, 0x50+id, 0)
	}
	binary.LittleEndian.PutUint32(b, uint32(len(b)-4))
	return b
}

func TestDwarfLayerNoEquivalences(t *testing.T) {
	old, _ := testImage([][]byte{
		testUnit(nil, "a", 0, 0x1000, []string{"x", "y"}, 12),
		testUnit(nil, "b", 100, 0x2000, []string{"z"}, 24),
	}, testLists(1, 2, 3))
	// Unit a gains a variable, so unit b and its references shift; the
	// list section gains a list before z's.
	nw, _ := testImage([][]byte{
		testUnit(nil, "a", 0, 0x1100, []string{"x", "w", "y"}, 12),
		testUnit(nil, "b", 100, 0x2100, []string{"z"}, 30),
	}, testLists(1, 9, 2, 3))
	// Positions inside unit a before the insertion are unchanged; a's
	// second variable and everything in b move by the inserted DIE.
	addrMap := func(a uint64) (uint64, bool) { return a + 0x100, true }
	plan, ok := buildDwarfPlan(old, nw, func(int) bool { return true }, addrMap)
	if !ok {
		t.Fatal("no plan")
	}
	wire := plan.marshal()
	got, err := unmarshalDwarfPlan(wire, old.Data)
	if err != nil {
		t.Fatal(err)
	}
	for k := range plan.Records {
		if !slices.Equal(plan.Records[k], got.Records[k]) {
			t.Fatalf("section %d records differ after round trip:\n%+v\n%+v", k, plan.Records[k], got.Records[k])
		}
	}
	if len(got.Records[dwInfo]) <= 2 {
		t.Fatalf("unit a should be split into DIE records, got %d records", len(got.Records[dwInfo]))
	}
	out := make([]byte, len(nw.Data))
	st, err := applyDwarf(out, old.Data, got, equivalencePlan{}, func(a uint64) x86.Target { return x86.Target{Addr: a + 0x100, Known: true} }, func(uint64) (int64, bool) { return 0, false })
	if err != nil {
		t.Fatal(err)
	}
	if st.UnitsPaired != 2 || st.Refs != 3 || st.Addrs != 2 {
		t.Fatalf("stats %+v", st)
	}
	s := got.Secs[dwInfo]
	pred, want := out[s.NewOff:s.NewOff+s.NewSize], nw.Data[s.NewOff:s.NewOff+s.NewSize]
	// Everything but the inserted variable's own bytes is predicted.
	wrong := 0
	for i := range want {
		if pred[i] != want[i] {
			wrong++
		}
	}
	if wrong > 10 {
		t.Fatalf("%d of %d .debug_info bytes wrong\n%x\n%x", wrong, len(want), pred, want)
	}
	ls := got.Secs[dwLoclists]
	if lp, lw := out[ls.NewOff:ls.NewOff+ls.NewSize], nw.Data[ls.NewOff:ls.NewOff+ls.NewSize]; !slices.Equal(lp[:18], lw[:18]) || !slices.Equal(lp[24:], lw[24:]) {
		t.Fatalf("list section not placed:\n%x\n%x", lp, lw)
	}
}

func TestPairByDiff(t *testing.T) {
	a := []uint64{1, 2, 3, 4, 5, 3, 6}
	b := []uint64{1, 9, 2, 4, 5, 3, 6}
	want := []int{0, 2, -1, 3, 4, 5, 6}
	if got := pairByDiff(a, b); !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := pairByDiff(nil, b); len(got) != 0 {
		t.Fatal("empty old should pair nothing")
	}
}

func TestListBounds(t *testing.T) {
	recs, err := listBounds(testLists(1, 2), true)
	if err != nil {
		t.Fatal(err)
	}
	want := []unitPair{{OldOff: 0, OldLen: 12, Unit: true}, {OldOff: 12, OldLen: 6}, {OldOff: 18, OldLen: 6}}
	if !slices.Equal(recs, want) {
		t.Fatalf("got %+v", recs)
	}
}
