package dwarf

import (
	"encoding/binary"
	"slices"
	"testing"

	"github.com/wjordan/presage/presage/eqmatch"
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

// testImage lays out one image's sections, with their geometry in the old
// image's fields; testSecs pairs two of them into a plan's geometry.
func testImage(units [][]byte, extra []byte) ([]byte, [NSec]Sec) {
	var secs [NSec]Sec
	data := make([]byte, 64)
	put := func(k int, b []byte) {
		if len(b) == 0 {
			return
		}
		secs[k] = Sec{OldOff: uint64(len(data)), OldSize: uint64(len(b))}
		data = append(data, b...)
	}
	var info []byte
	for _, u := range units {
		info = append(info, u...)
	}
	put(Info, info)
	put(Abbrev, testAbbrev)
	put(Loclists, extra)
	return data, secs
}

func testSecs(o, n [NSec]Sec) [NSec]Sec {
	var secs [NSec]Sec
	for k := range secs {
		if o[k].OldSize != 0 && n[k].OldSize != 0 {
			secs[k] = Sec{OldOff: o[k].OldOff, OldSize: o[k].OldSize, NewOff: n[k].OldOff, NewSize: n[k].OldSize}
		}
	}
	return secs
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

func TestLayerWithoutRuns(t *testing.T) {
	old, oldSecs := testImage([][]byte{
		testUnit(nil, "a", 0, 0x1000, []string{"x", "y"}, 12),
		testUnit(nil, "b", 100, 0x2000, []string{"z"}, 24),
	}, testLists(1, 2, 3))
	// Unit a gains a variable, so unit b and its references shift; the
	// list section gains a list before z's.
	nw, newSecs := testImage([][]byte{
		testUnit(nil, "a", 0, 0x1100, []string{"x", "w", "y"}, 12),
		testUnit(nil, "b", 100, 0x2100, []string{"z"}, 30),
	}, testLists(1, 9, 2, 3))
	// Positions inside unit a before the insertion are unchanged; a's
	// second variable and everything in b move by the inserted DIE.
	addrMap := func(a uint64) (uint64, bool) { return a + 0x100, true }
	plan, ok := Build(old, nw, testSecs(oldSecs, newSecs), func(int) bool { return true }, addrMap, nil)
	if !ok {
		t.Fatal("no plan")
	}
	wire := plan.Marshal()
	got, err := Unmarshal(wire, old)
	if err != nil {
		t.Fatal(err)
	}
	for k := range plan.Records {
		if !slices.Equal(plan.Records[k], got.Records[k]) {
			t.Fatalf("section %d records differ after round trip:\n%+v\n%+v", k, plan.Records[k], got.Records[k])
		}
	}
	if len(got.Records[Info]) <= 2 {
		t.Fatalf("unit a should be split into DIE records, got %d records", len(got.Records[Info]))
	}
	out := make([]byte, len(nw))
	st, err := Apply(out, old, got, func(a uint64) (uint64, bool) { return a + 0x100, true }, func(uint64) (int64, bool) { return 0, false })
	if err != nil {
		t.Fatal(err)
	}
	if st.UnitsPaired != 2 || st.Refs != 3 || st.Addrs != 2 {
		t.Fatalf("stats %+v", st)
	}
	s := got.Secs[Info]
	pred, want := out[s.NewOff:s.NewOff+s.NewSize], nw[s.NewOff:s.NewOff+s.NewSize]
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
	ls := got.Secs[Loclists]
	if lp, lw := out[ls.NewOff:ls.NewOff+ls.NewSize], nw[ls.NewOff:ls.NewOff+ls.NewSize]; !slices.Equal(lp[:18], lw[:18]) || !slices.Equal(lp[24:], lw[24:]) {
		t.Fatalf("list section not placed:\n%x\n%x", lp, lw)
	}
}

// TestRunsWireForm round-trips the runs of the sections no record table
// places; a section with a table ships none.
func TestRunsWireForm(t *testing.T) {
	var p Plan
	p.Secs[Info] = Sec{OldOff: 0, OldSize: 100, NewOff: 0, NewSize: 120}
	p.Secs[Line] = Sec{OldOff: 100, OldSize: 50, NewOff: 120, NewSize: 50}
	p.Runs[Info] = []eqmatch.Run{{Src: 0, Dst: 0, N: 40}, {Src: 40, Dst: 60, N: 60}}
	p.Runs[Line] = []eqmatch.Run{{Src: 0, Dst: 0, N: 50}}
	p.Records[Line] = []Record{{OldOff: 0, OldLen: 50, NewOff: 0, NewLen: 50}}
	q := Plan{Secs: p.Secs}
	if err := q.UnmarshalRuns(p.MarshalRuns()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(q.Runs[Info], p.Runs[Info]) {
		t.Fatalf("runs differ after round trip: %v", q.Runs[Info])
	}
	if q.Runs[Line] != nil {
		t.Fatalf("a section with a record table should ship no runs, got %v", q.Runs[Line])
	}
	if err := (&Plan{Secs: p.Secs}).UnmarshalRuns(append(p.MarshalRuns(), 0)); err == nil {
		t.Fatal("trailing data accepted")
	}
	var bad Plan
	bad.Secs = p.Secs
	bad.Runs[Info] = []eqmatch.Run{{Src: 0, Dst: 0, N: 200}}
	if err := (&Plan{Secs: p.Secs}).UnmarshalRuns(bad.MarshalRuns()); err == nil {
		t.Fatal("a run leaving its section accepted")
	}
}

// TestApplyRunsProjectRefs places .debug_info by runs alone: the insertion
// breaks them, and every ref_addr must still land at its new position with
// its new value.
func TestApplyRunsProjectRefs(t *testing.T) {
	vars := []string{"alpha", "bravo", "charlie", "delta"}
	newVars := []string{"alpha", "bravo", "inserted", "charlie", "delta"}
	// The units are built one after the other, so the second one's type
	// references are section offsets past the insertion: both their
	// positions and their values move with it.
	old, oldSecs := testImage([][]byte{
		testUnit(testUnit(nil, "first", 0, 0x1000, vars, 12), "second", 100, 0x2000, vars, 24),
	}, nil)
	nw, newSecs := testImage([][]byte{
		testUnit(testUnit(nil, "first", 0, 0x1100, newVars, 12), "second", 100, 0x2100, vars, 24),
	}, nil)
	// No section gets a record table, so the runs place the bytes and the
	// fields project through them.
	p, ok := Build(old, nw, testSecs(oldSecs, newSecs), func(int) bool { return false }, nil, nil)
	if !ok {
		t.Fatal("no plan")
	}
	s := p.Secs[Info]
	oldInfo, newInfo := old[s.OldOff:s.OldOff+s.OldSize], nw[s.NewOff:s.NewOff+s.NewSize]
	p.Runs[Info] = eqmatch.Match(oldInfo, newInfo, eqmatch.Params{Min: 8, Drop: 8})
	if len(p.Runs[Info]) < 2 {
		t.Fatalf("the insertion should break the runs, got %v", p.Runs[Info])
	}
	q := Plan{Secs: p.Secs}
	if err := q.UnmarshalRuns(p.MarshalRuns()); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(nw))
	st, err := Apply(out, old, q, func(a uint64) (uint64, bool) { return a + 0x100, true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Refs != 2*len(vars) {
		t.Fatalf("stats %+v", st)
	}
	units, err := unitBounds(newInfo)
	if err != nil {
		t.Fatal(err)
	}
	a := p.Secs[Abbrev]
	refs := 0
	for _, u := range units {
		err := walkUnit(newInfo, nw[a.NewOff:a.NewOff+a.NewSize], u, func(pos uint64, at dwarfAttr, _ uint8, _ *lebReader) {
			if at.form != 0x10 { // ref_addr
				return
			}
			refs++
			if got, want := out[s.NewOff+pos:s.NewOff+pos+4], newInfo[pos:pos+4]; !slices.Equal(got, want) {
				t.Errorf("ref_addr at %d: got %x want %x", pos, got, want)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if refs != len(vars)+len(newVars) {
		t.Fatalf("walked %d ref_addr fields", refs)
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
	want := []Record{{OldOff: 0, OldLen: 12, Unit: true}, {OldOff: 12, OldLen: 6}, {OldOff: 18, OldLen: 6}}
	if !slices.Equal(recs, want) {
		t.Fatalf("got %+v", recs)
	}
}
