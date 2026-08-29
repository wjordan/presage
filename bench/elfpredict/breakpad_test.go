package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBreakpadCodeUnits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.sym")
	sym := "MODULE Linux x86_64 ABCDEF0 libx.so\nFILE 0 a.cc\nFUNC 1000 10 0 f\nFUNC m 1010 8 0 g(int)\nFUNC 1010 8 0 g_alias\nFUNC 20 4 0 outside\n1000 4 1 0\n"
	if err := os.WriteFile(path, []byte(sym), 0o644); err != nil {
		t.Fatal(err)
	}
	units, st, err := loadCodeUnits(path, section{Addr: 0x1000, Size: 0x100})
	if err != nil {
		t.Fatal(err)
	}
	if st.FunctionSymbols != 3 || len(units) != 2 {
		t.Fatalf("got %d symbols, %d units", st.FunctionSymbols, len(units))
	}
	if units[0].Off != 0 || units[0].Size != 0x10 || units[1].Off != 0x10 || units[1].Size != 8 || len(units[1].Names) != 2 {
		t.Fatalf("units %+v", units)
	}
}
