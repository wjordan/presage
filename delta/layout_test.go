package delta

import (
	"testing"

	"github.com/wjordan/presage/delta/gobin"
)

// TestSectsRoundTrip covers the section ops: the unchanged run (op 3), a
// section shifted off the running delta, a reordered one and a new one.
func TestSectsRoundTrip(t *testing.T) {
	old := &gobin.Bin{Order: []*gobin.Section{
		{Name: ".a", Addr: 0x1000, Off: 0x1000, Size: 0x100},
		{Name: ".b", Addr: 0x2000, Off: 0x2000, Size: 0x200},
		{Name: ".c", Addr: 0x3000, Off: 0x3000, Size: 0x300},
		{Name: ".d", Addr: 0x4000, Off: 0x4000, Size: 0x400, NoBits: true},
	}}
	sects := []sectInfo{
		{Name: ".a", Addr: 0x1000, Off: 0x1000, Size: 0x100},
		{Name: ".b", Addr: 0x2000, Off: 0x2000, Size: 0x200},
		{Name: ".c", Addr: 0x3020, Off: 0x3020, Size: 0x320},
		{Name: ".e", Addr: 0x3f00, Off: 0x3f00, Size: 0x10},
		{Name: ".d", Addr: 0x4020, Off: 0x4020, Size: 0x400, NoBits: true},
	}
	w := &wbuf{}
	encodeSects(w, old, sects)
	got := decodeSects(&rbuf{b: w.b}, old)
	if len(got) != len(sects) {
		t.Fatalf("decoded %d sections, want %d", len(got), len(sects))
	}
	for i, s := range sects {
		if got[i] != s {
			t.Errorf("section %d: %+v, want %+v", i, got[i], s)
		}
	}
}

// TestFuncLayoutRoundTrip covers the function ops: the unchanged run (op 3),
// a resized function, a reordered one and a new one.
func TestFuncLayoutRoundTrip(t *testing.T) {
	mk := func(entries ...uint64) []*gobin.Func {
		var fs []*gobin.Func
		for i := 0; i+1 < len(entries); i++ {
			fs = append(fs, &gobin.Func{Idx: i, Name: string(rune('a' + i)), Entry: entries[i], End: entries[i+1]})
		}
		return fs
	}
	old := &gobin.Bin{Funcs: mk(0x1000, 0x1100, 0x1250, 0x1400, 0x1500, 0x1600)}
	// new: a and b unchanged, c resized +0x20, a fresh function, then e
	// (skipping d), all shifted accordingly.
	new := &gobin.Bin{Funcs: []*gobin.Func{
		{Idx: 0, Name: "a", Entry: 0x1000, End: 0x1100},
		{Idx: 1, Name: "b", Entry: 0x1100, End: 0x1250},
		{Idx: 2, Name: "c", Entry: 0x1250, End: 0x1420},
		{Idx: 3, Name: "fresh", Entry: 0x1420, End: 0x1480},
		{Idx: 4, Name: "e", Entry: 0x1480, End: 0x1580},
	}}
	m := &match{NewToOld: []int{0, 1, 2, -1, 4}, OldToNew: []int{0, 1, 2, -1, 4, -1}}
	l := &layout{
		FirstEntry: 0x1000, NFunc: len(new.Funcs),
		Funcs: encodeFuncLayout(old, new, m),
	}
	funcs, gm, err := decodeFuncLayout(old, l)
	if err != nil {
		t.Fatalf("decodeFuncLayout: %v", err)
	}
	for j, f := range new.Funcs {
		g := funcs[j]
		if g.Name != f.Name || g.Entry != f.Entry || g.End != f.End {
			t.Errorf("func %d: %+v, want %+v", j, g, f)
		}
		if gm.NewToOld[j] != m.NewToOld[j] {
			t.Errorf("func %d maps to old %d, want %d", j, gm.NewToOld[j], m.NewToOld[j])
		}
	}
}
