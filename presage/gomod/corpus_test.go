package gomod

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wjordan/go-binsync/delta"
	"github.com/wjordan/go-binsync/delta/gobin"
	"github.com/wjordan/go-binsync/presage"
)

// The milestone-1 exit (docs/general/presage-core.md §1), over the corpus
// named by BINSYNC_CORPUS: every pair round-trips through presage; the
// self-prediction gate is green for the Go module; and no pair's patch is
// more than 2 % larger than delta.Encode's.
func corpus(t *testing.T) []string {
	dir := os.Getenv("BINSYNC_CORPUS")
	if dir == "" {
		t.Skip("BINSYNC_CORPUS not set")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("BINSYNC_CORPUS: %v", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && !strings.Contains(e.Name(), "rebuild") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files
}

func read(t *testing.T, path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSelfPrediction(t *testing.T) {
	for _, path := range corpus(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			bin := read(t, path)
			if _, err := gobin.Parse(bin); err != nil {
				t.Skipf("not a binary the Go module takes: %v", err)
			}
			// copy would claim an identical target; the gate is about the Go module.
			var st presage.Stats
			patch, err := presage.Encode([][]byte{bin}, bin, presage.Options{Registry: Registry(), Modules: []byte{presage.ModuleLZ, presage.ModuleGo}, Stats: &st})
			if err != nil {
				t.Fatal(err)
			}
			r := st.Regions[0]
			if r.Module != "go" {
				t.Fatalf("module %s, want go", r.Module)
			}
			if r.PredictErr != 0 || r.Residual > 64 {
				t.Errorf("self-prediction differs in %d bytes, residual %d B", r.PredictErr, r.Residual)
			}
			var out bytes.Buffer
			if err := presage.Apply([][]byte{bin}, patch, Registry(), &out); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), bin) {
				t.Fatal("Apply did not reproduce the input")
			}
		})
	}
}

// Same size class = same "-F" build flavour, as delta's corpus test.
func flavour(path string) string {
	base := filepath.Base(path)
	if i := strings.LastIndex(base, "-"); i >= 0 {
		return base[i:]
	}
	return ""
}

func TestCorpusAgainstDelta(t *testing.T) {
	files := corpus(t)
	for _, a := range files {
		for _, b := range files {
			if a == b || flavour(a) != flavour(b) {
				continue
			}
			t.Run(filepath.Base(a)+"->"+filepath.Base(b), func(t *testing.T) {
				t.Parallel()
				old, new := read(t, a), read(t, b)
				var st presage.Stats
				patch, err := presage.Encode([][]byte{old}, new, presage.Options{Registry: Registry(), Stats: &st})
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				if err := presage.Apply([][]byte{old}, patch, Registry(), &out); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(out.Bytes(), new) {
					t.Fatal("Apply did not reproduce the target")
				}
				ref, err := delta.Encode(old, new, delta.Options{})
				if err != nil {
					t.Fatal(err)
				}
				line := fmt.Sprintf("presage %d (%s) delta %d", len(patch), st.Regions[0].Module, len(ref))
				t.Log(line)
				if float64(len(patch)) > 1.02*float64(len(ref))+64 {
					t.Errorf("%s: more than 2 %% above delta", line)
				}
			})
		}
	}
}
