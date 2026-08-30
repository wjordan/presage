package delta

import (
	"bytes"
	"testing"

	"github.com/wjordan/presage/delta/gobin"
)

// The Go-table module predicting a binary from itself: every section it
// writes must come back exact, and the maps it derives must be identities.
func TestGoTablesSelfPredict(t *testing.T) {
	for _, path := range corpus(t)[:min(2, len(corpus(t)))] {
		t.Run(path, func(t *testing.T) {
			b := readFile(t, path)
			bin, err := gobin.Parse(b)
			if err != nil {
				t.Skipf("not a Go binary the module understands: %v", err)
			}
			plan, err := EncodeGoTables(b, b)
			if err != nil {
				t.Fatal(err)
			}
			out := make([]byte, len(b))
			st, err := ApplyGoTables(b, plan, out, func(name string) bool { return name == ".text" })
			if err != nil {
				t.Fatal(err)
			}
			if st.Sections == 0 || st.Funcs != len(bin.Funcs) || st.Matched != len(bin.Funcs) {
				t.Errorf("stats %+v, want every one of %d functions matched", st, len(bin.Funcs))
			}
			for _, s := range bin.Order {
				if s.NoBits || s.Name == ".text" {
					continue
				}
				if !bytes.Equal(out[s.Off:s.Off+s.Size], b[s.Off:s.Off+s.Size]) {
					t.Errorf("%s: self-prediction differs", s.Name)
				}
			}
			fm, err := GoFunctionMap(b, plan)
			if err != nil {
				t.Fatal(err)
			}
			if len(fm) != len(bin.Funcs) {
				t.Fatalf("function map has %d entries, want %d", len(fm), len(bin.Funcs))
			}
			lookup, err := GoAddressLookup(b, plan)
			if err != nil {
				t.Fatal(err)
			}
			for i, f := range fm {
				if f.OldEntry != f.NewEntry || f.OldSize != f.NewSize || f.OldEntry != bin.Funcs[i].Entry {
					t.Fatalf("map %d = %+v, want the identity at %#x", i, f, bin.Funcs[i].Entry)
				}
				if tg := lookup(f.OldEntry); !tg.Known || tg.Addr != f.OldEntry {
					t.Fatalf("lookup(%#x) = %+v, want itself", f.OldEntry, tg)
				}
			}
			if s := bin.Sects[".rodata"]; s != nil {
				if tg := lookup(s.Addr + 8); !tg.Known || tg.Addr != s.Addr+8 {
					t.Errorf("lookup into .rodata = %+v, want the identity", tg)
				}
			}
		})
	}
}
