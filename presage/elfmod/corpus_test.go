//go:build corpus

// The ELF module's gate (docs/general/elf-module.md §7): the Chrome pair
// encodes, applies and compares byte-exactly under a ratchet budget, plus
// self-prediction on every corpus pair and the no-symbols path. Subtests
// run in parallel; the whole gate is ~3 minutes wall. There is deliberately
// no full multi-pair round-trip tier — per-pair headline sizes are a CLI
// measurement (presage diff), paid only when a number is wanted:
//
//	go test -tags corpus ./presage/elfmod -run 'TestPairs|TestSelfPrediction|TestNoSymbols' -timeout 10m
package elfmod

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wjordan/presage/presage"
	"github.com/wjordan/presage/presage/gomod"
	"github.com/wjordan/presage/presage/symbols"
)

type pair struct {
	name             string
	old, new         string
	oldSyms, newSyms string
	parity, product  int
}

func home(parts ...string) string {
	h, _ := os.UserHomeDir()
	return filepath.Join(append([]string{h}, parts...)...)
}

var pairs = []pair{
	{
		name:    "chrome-151.0.7922.169-173",
		old:     home(".cache", "presage-chrome-zucchini", "chrome-151.0.7922.169"),
		new:     home(".cache", "presage-chrome-zucchini", "chrome-151.0.7922.173"),
		oldSyms: home(".cache", "presage-chrome-zucchini", "symbols-151.0.7922.169", "debug-info", "chrome.debug"),
		newSyms: home(".cache", "presage-chrome-zucchini", "symbols-151.0.7922.173", "debug-info", "chrome.debug"),
		// parity is the measured container-v6 patch with the compact CM model
		// over bit-history states, balanced terminal compression and
		// cursor-placed .rodata tables, a ratchet against regression.
		parity: 2305394, product: 2634264,
	},
	{
		name:    "libxul-154.0-154.0.1",
		old:     home(".cache", "presage-pairs", "libxul-154.0.so"),
		new:     home(".cache", "presage-pairs", "libxul-154.0.1.so"),
		oldSyms: home(".cache", "presage-pairs", "libxul-154.0.funcs"),
		newSyms: home(".cache", "presage-pairs", "libxul-154.0.1.funcs"),
		parity:  4780572, product: 4063404,
	},
	{
		// Two adjacent rustc nightlies, one day apart: the corpus's only
		// BOLT'd multi-window image (four code windows) and its only Rust
		// v0-mangled symbols. No harness number exists, so parity is the
		// measured patch, a ratchet against regression; bsdiff is
		// 15,106,624. Symbols come from the images' own .symtab.
		name:    "librustc_driver-2026-08-27-28",
		old:     home(".cache", "presage-pairs", "rd-2026-08-27.so"),
		new:     home(".cache", "presage-pairs", "rd-2026-08-28.so"),
		oldSyms: home(".cache", "presage-pairs", "rd-2026-08-27.so"),
		newSyms: home(".cache", "presage-pairs", "rd-2026-08-28.so"),
		parity:  6597439, product: 6597439,
	},
}

func (p pair) files(t *testing.T, withSyms bool) (old, new []byte, syms [2]symbols.Reader) {
	t.Helper()
	paths := []string{p.old, p.new}
	if withSyms {
		paths = append(paths, p.oldSyms, p.newSyms)
	}
	for _, f := range paths {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("%s absent", f)
		}
	}
	old, new = read(t, p.old), read(t, p.new)
	if withSyms {
		for i, f := range []string{p.oldSyms, p.newSyms} {
			r, err := symbols.Open(f)
			if err != nil {
				t.Fatal(err)
			}
			syms[i] = r
		}
	}
	return old, new, syms
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func registry(syms [2]symbols.Reader) *presage.Registry {
	r := presage.NewRegistry()
	r.Add(gomod.Module{})
	r.Add(Module{Symbols: syms})
	return r
}

// roundTrip encodes new against old, applies the patch, and checks the
// result byte for byte; it returns the patch size and the region stats.
func roundTrip(t *testing.T, old, new []byte, syms [2]symbols.Reader) (int, presage.RegionStats) {
	t.Helper()
	var st presage.Stats
	patch, err := presage.Encode([][]byte{old}, new, presage.Options{Registry: registry(syms), Stats: &st})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := presage.Apply([][]byte{old}, patch, registry([2]symbols.Reader{}), &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), new) {
		t.Fatal("Apply did not reproduce the target")
	}
	r := st.Regions[0]
	if r.Module != "elf" {
		t.Fatalf("module %s, want elf", r.Module)
	}
	for _, n := range st.Notes {
		t.Log(n)
	}
	t.Logf("patch %d B: plan %d, residual %d, %d mispredicted", len(patch), r.Plan, r.Residual, r.PredictErr)
	return len(patch), r
}

// TestPairs round-trips the Chrome pair only — the cheapest encode, and the
// one exercising every structural path (derived map, displacement columns,
// CM coder). The other pairs' sizes are CLI measurements, not tests.
func TestPairs(t *testing.T) {
	t.Parallel()
	p := pairs[0]
	old, new, syms := p.files(t, true)
	size, _ := roundTrip(t, old, new, syms)
	if limit := p.parity * 103 / 100; size > limit {
		t.Errorf("ratchet: %d B > %d (1.03 × %d)", size, limit, p.parity)
	}
	if os.Getenv("PRESAGE_ELF_PRODUCT_GATE") == "" {
		t.Logf("product gate skipped (PRESAGE_ELF_PRODUCT_GATE unset): %d vs %d", size, p.product)
		return
	}
	if size > p.product {
		t.Errorf("product: %d B > %d", size, p.product)
	}
}

func TestSelfPrediction(t *testing.T) {
	t.Parallel()
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			old, _, syms := p.files(t, true)
			// The target is the old image with its own symbols on both sides;
			// copy would claim it, so the module is named explicitly.
			var st presage.Stats
			reg := registry([2]symbols.Reader{syms[0], syms[0]})
			patch, err := presage.Encode([][]byte{old}, old, presage.Options{Registry: reg, Modules: []byte{presage.ModuleLZ, ModuleELF}, Stats: &st})
			if err != nil {
				t.Fatal(err)
			}
			r := st.Regions[0]
			if r.Module != "elf" {
				t.Fatalf("module %s, want elf", r.Module)
			}
			if r.PredictErr != 0 || r.Residual > 64 {
				t.Errorf("self-prediction differs in %d bytes, residual %d B", r.PredictErr, r.Residual)
			}
			var out bytes.Buffer
			if err := presage.Apply([][]byte{old}, patch, registry([2]symbols.Reader{}), &out); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), old) {
				t.Fatal("Apply did not reproduce the input")
			}
		})
	}
}

func TestNoSymbols(t *testing.T) {
	t.Parallel()
	p := pairs[1]
	old, new, _ := p.files(t, false)
	size, _ := roundTrip(t, old, new, [2]symbols.Reader{})
	t.Logf("%s without symbols: %d B (recorded, not asserted)", p.name, size)
}
