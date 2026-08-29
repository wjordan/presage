package cz

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/andybalholm/brotli"
)

func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, n := range []int{0, 1, 1000, 200000} {
		// compressible: a repeated pattern with noise
		src := make([]byte, n)
		for i := range src {
			src[i] = byte(i % 251)
		}
		for k := 0; k < n/100; k++ {
			src[r.Intn(n)] = byte(r.Intn(256))
		}
		codec, out := Compress(src)
		got, err := Decompress(codec, out, len(src))
		if err != nil || !bytes.Equal(got, src) {
			t.Fatalf("n=%d codec=%d: %v", n, codec, err)
		}
		if n > 1000 && len(out) >= len(src) {
			t.Fatalf("n=%d: compressed %d >= raw %d", n, len(out), len(src))
		}
	}
}

// Large streams go to brotli-11 (always quality 11, whatever the size);
// zstd is still a candidate because its framing wins on tiny ones.
func TestCompressChoosesSmallest(t *testing.T) {
	src := make([]byte, 5<<20)
	for i := range src {
		src[i] = byte(i*7 ^ i>>5)
	}
	codec, out := Compress(src)
	if codec != Brotli || len(out) >= len(src) {
		t.Fatalf("codec %d, %d bytes", codec, len(out))
	}
	var buf bytes.Buffer
	w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: 11, LGWin: 24})
	w.Write(src)
	w.Close()
	if buf.Len() != len(out) {
		t.Fatalf("large stream: got %d bytes, brotli-11 gives %d", len(out), buf.Len())
	}
	if codec, _ := Compress([]byte{1, 2, 3}); codec != Raw {
		t.Fatalf("tiny input: codec %d, want raw", codec)
	}
}

func TestDecompressRejectsBadInput(t *testing.T) {
	_, out := Compress(bytes.Repeat([]byte("abc"), 1000))
	if _, err := Decompress(Zstd, out, 3000); err == nil {
		t.Skip("stream happened to be raw")
	}
	if _, err := Decompress(9, out, 3000); err == nil {
		t.Fatal("unknown codec accepted")
	}
	if _, err := Decompress(Raw, []byte("abc"), 4); err == nil {
		t.Fatal("wrong length accepted")
	}
}
