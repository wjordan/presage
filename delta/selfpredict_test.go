package delta

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wjordan/presage/delta/gobin"
)

// The self-prediction gate (docs/go-module-design.md 2.6, D14). Encoding a binary
// against itself exercises every predictor -- the function layout, the
// pclntab rebuild, the stage-1b blob emulation, the type-descriptor walk,
// the instruction relocator -- with a known answer: the prediction must
// reproduce the input byte for byte, so the correction is empty.
//
// This is what qualifies a new Go release. It needs binaries built by that
// release, which the repository does not carry, so it runs over a corpus
// directory named by BINSYNC_CORPUS (bench/out/bin127 after bench/build.sh).
func TestSelfPrediction(t *testing.T) {
	for _, path := range corpus(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			bin := readFile(t, path)
			if _, err := gobin.Parse(bin); err != nil {
				t.Skipf("not a binary the Go-aware codec takes: %v", err)
			}
			var st Stats
			patch, err := Encode(bin, bin, Options{Stats: &st})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if st.Transform < TransformGoAMD64 {
				t.Fatalf("transform %d, want the Go-aware codec", st.Transform)
			}
			if st.PredictErr != 0 {
				t.Errorf("self-prediction differs from the input in %d bytes; "+
					"the codec does not reproduce this Go release's layout", st.PredictErr)
			}
			if st.Stage2 > 64 {
				t.Errorf("stage-2 correction is %d bytes, want an empty one", st.Stage2)
			}
			mustRoundTrip(t, bin, bin, patch)
		})
	}
}

// TestCorpusRoundTrip encodes ordered pairs of corpus binaries and checks the
// patch reproduces the target exactly. It is the codec's end-to-end
// correctness test; the size numbers it prints are what the documentation
// quotes.
//
// By default it runs every pair whose two binaries share a build flavour --
// the pairings a release actually produces -- plus, for each pair of
// flavours, one representative in each direction so that mismatched inputs
// (stripped against unstripped, PIE against EXEC) still round-trip. That is
// 168 of the 600 ordered pairs on the reference corpus and 7.9% of the work:
// the 432 pairs it leaves out are repetitions of the same cross-flavour
// shapes, and they cost 103 s each because the prediction is mostly wrong.
// Set BINSYNC_CORPUS_ALL=1 for all of them (~35 min, the release gate).
func TestCorpusRoundTrip(t *testing.T) {
	for _, pair := range corpusPairs(corpus(t)) {
		a, b := pair[0], pair[1]
		t.Run(filepath.Base(a)+"->"+filepath.Base(b), func(t *testing.T) {
			t.Parallel()
			old, new := readFile(t, a), readFile(t, b)
			var st Stats
			patch, err := Encode(old, new, Options{Stats: &st})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			mustRoundTrip(t, old, new, patch)
			t.Logf("transform %d, patch %d B (%.3f%% of %d), residual %d B",
				st.Transform, len(patch), 100*float64(len(patch))/float64(len(new)), len(new), st.PredictErr)
		})
	}
}

// TestTransform1Compat holds the previous transform open. An encoder capped
// at transform 1 (docs/go-module-design.md 2.6) must produce a patch with no
// segment map that still applies.
func TestTransform1Compat(t *testing.T) {
	var old, new []byte
	for _, p := range corpus(t) {
		b := readFile(t, p)
		if _, err := gobin.Parse(b); err != nil {
			continue
		}
		if old == nil {
			old = b
		} else {
			new = b
			break
		}
	}
	if new == nil {
		t.Skip("needs two binaries the Go-aware codec takes")
	}
	var st Stats
	patch, err := Encode(old, new, Options{MaxTransform: TransformGoAMD64, Stats: &st})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if st.Transform != TransformGoAMD64 {
		t.Fatalf("transform %d, want %d", st.Transform, TransformGoAMD64)
	}
	mustRoundTrip(t, old, new, patch)
}

func mustRoundTrip(t *testing.T, old, new, patch []byte) {
	t.Helper()
	var got bytes.Buffer
	got.Grow(len(new))
	if err := Apply(old, patch, &got); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !bytes.Equal(got.Bytes(), new) {
		t.Fatalf("Apply produced %d bytes, want %d, and they differ", got.Len(), len(new))
	}
}

// corpusPairs chooses the ordered pairs TestCorpusRoundTrip encodes. A corpus
// name is "<version>-<flavour>", optionally "-rebuild"; the flavour is what
// makes a pair cheap or expensive, so the default set is every same-flavour
// pair plus, for each ordered pair of flavours, the smallest cross-flavour
// pair -- smallest because these run in parallel and one 43 MB unstripped
// target alone costs six minutes of wall clock.
func corpusPairs(files []string) [][2]string {
	if os.Getenv("BINSYNC_CORPUS_ALL") != "" {
		var all [][2]string
		for i, a := range files {
			for j, b := range files {
				if i != j {
					all = append(all, [2]string{a, b})
				}
			}
		}
		return all
	}
	flavour := func(p string) string {
		n := filepath.Base(p)
		i := strings.Index(n, "-")
		if i < 0 {
			return n
		}
		return strings.TrimSuffix(n[i+1:], "-rebuild")
	}
	size := func(p string) int64 {
		fi, err := os.Stat(p)
		if err != nil {
			return 1 << 62
		}
		return fi.Size()
	}
	var out [][2]string
	rep := map[string][2]string{}
	for i, a := range files {
		for j, b := range files {
			if i == j {
				continue
			}
			fa, fb := flavour(a), flavour(b)
			if fa == fb {
				out = append(out, [2]string{a, b})
				continue
			}
			k := fa + "->" + fb
			if cur, ok := rep[k]; !ok || size(a)+size(b) < size(cur[0])+size(cur[1]) {
				rep[k] = [2]string{a, b}
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(rep)) {
		out = append(out, rep[k])
	}
	return out
}

// corpus lists the binaries BINSYNC_CORPUS names, skipping the test when it
// is unset.
func corpus(t *testing.T) []string {
	t.Helper()
	dir := os.Getenv("BINSYNC_CORPUS")
	if dir == "" {
		t.Skip("set BINSYNC_CORPUS to a directory of Go binaries to run the codec gate")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("BINSYNC_CORPUS: %v", err)
	}
	var out []string
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		// Stat, not e.Info: a corpus is often a directory of symlinks.
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Fatalf("BINSYNC_CORPUS=%s holds no executables", dir)
	}
	return out
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
