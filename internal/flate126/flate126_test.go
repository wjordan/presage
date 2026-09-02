package flate126

import (
	"bytes"
	"compress/zlib"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// goldens are pairs of testdata/<name>.in (input) and testdata/<name>.z
// (compress/zlib at BestSpeed, produced once by the go1.26.3 toolchain).
func goldens(t *testing.T) []string {
	t.Helper()
	zs, err := filepath.Glob("testdata/*.z")
	if err != nil {
		t.Fatal(err)
	}
	if len(zs) == 0 {
		t.Fatal("no goldens in testdata")
	}
	names := make([]string, len(zs))
	for i, z := range zs {
		names[i] = goldenName(z)
	}
	return names
}

func goldenName(zpath string) string {
	return zpath[len("testdata/") : len(zpath)-len(".z")]
}

func readPair(t *testing.T, name string) (in, z []byte) {
	t.Helper()
	in, err := os.ReadFile(filepath.Join("testdata", name+".in"))
	if err != nil {
		t.Fatal(err)
	}
	z, err = os.ReadFile(filepath.Join("testdata", name+".z"))
	if err != nil {
		t.Fatal(err)
	}
	return in, z
}

func compress(t *testing.T, in []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := NewZlibWriter(&buf)
	if _, err := w.Write(in); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestGolden is the whole point of the package: our output must be the exact
// bytes the go1.26 toolchain produced.
func TestGolden(t *testing.T) {
	for _, name := range goldens(t) {
		t.Run(name, func(t *testing.T) {
			in, want := readPair(t, name)

			got := compress(t, in)
			if !bytes.Equal(got, want) {
				n := firstDiff(got, want)
				t.Fatalf("output differs from go1.26 golden: len got=%d want=%d, first difference at byte %d",
					len(got), len(want), n)
			}

			// The golden must also be a valid zlib stream for the input.
			zr, err := zlib.NewReader(bytes.NewReader(want))
			if err != nil {
				t.Fatal(err)
			}
			back, err := io.ReadAll(zr)
			if err != nil {
				t.Fatal(err)
			}
			if err := zr.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(back, in) {
				t.Fatalf("golden inflates to %d bytes, want %d", len(back), len(in))
			}
		})
	}
}

// TestHostToolchainDiffers documents why this package exists: the host
// toolchain's compress/zlib no longer reproduces the go1.26 bytes.
func TestHostToolchainDiffers(t *testing.T) {
	const name = "mixed700k"
	in, want := readPair(t, name)

	var buf bytes.Buffer
	hw, err := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hw.Write(in); err != nil {
		t.Fatal(err)
	}
	if err := hw.Close(); err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(buf.Bytes(), want) {
		t.Skipf("host compress/zlib still matches the go1.26 golden for %s; this package is currently redundant", name)
	}
	t.Logf("host compress/zlib produced %d bytes, go1.26 golden is %d bytes (first difference at byte %d)",
		buf.Len(), len(want), firstDiff(buf.Bytes(), want))
}

func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
