//go:build corpus

// The ELF module's gate (docs/general/elf-module.md §7): the Chrome and
// libxul pairs encode, apply and compare byte-exactly, under a parity
// budget (the harness's own -native-equivalences number, proving the port)
// and a product budget (the comparison table), the latter behind
// PRESAGE_ELF_PRODUCT_GATE=1 until the matcher track lands. Minutes to run:
//
//	go test -tags corpus ./presage/elfmod -run 'TestPairs|TestSelfPrediction|TestNoSymbols' -timeout 30m
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
		parity:  4823576, product: 2634264,
	},
	{
		name:    "libxul-154.0-154.0.1",
		old:     home(".cache", "presage-pairs", "libxul-154.0.so"),
		new:     home(".cache", "presage-pairs", "libxul-154.0.1.so"),
		oldSyms: home(".cache", "presage-pairs", "libxul-154.0.funcs"),
		newSyms: home(".cache", "presage-pairs", "libxul-154.0.1.funcs"),
		parity:  4780572, product: 4063404,
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

func TestPairs(t *testing.T) {
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			old, new, syms := p.files(t, true)
			size, _ := roundTrip(t, old, new, syms)
			if limit := p.parity * 103 / 100; size > limit {
				t.Errorf("parity: %d B > %d (1.03 × harness %d)", size, limit, p.parity)
			}
			if os.Getenv("PRESAGE_ELF_PRODUCT_GATE") == "" {
				t.Logf("product gate skipped (PRESAGE_ELF_PRODUCT_GATE unset): %d vs %d", size, p.product)
				return
			}
			if size > p.product {
				t.Errorf("product: %d B > %d", size, p.product)
			}
		})
	}
}

func TestSelfPrediction(t *testing.T) {
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
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
	p := pairs[1]
	old, new, _ := p.files(t, false)
	size, _ := roundTrip(t, old, new, [2]symbols.Reader{})
	t.Logf("%s without symbols: %d B (recorded, not asserted)", p.name, size)
}
