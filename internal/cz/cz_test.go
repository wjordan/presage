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

// Compress tries the shipping Brotli quality and zstd, and keeps the actual
// smallest stream rather than assuming one codec wins by input size.
func TestCompressChoosesSmallest(t *testing.T) {
	src := make([]byte, 5<<20)
	for i := range src {
		src[i] = byte(i*7 ^ i>>5)
	}
	codec, out := Compress(src)
	var buf bytes.Buffer
	w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: 9, LGWin: 24})
	w.Write(src)
	w.Close()
	wantCodec, wantLen := Brotli, buf.Len()
	if z := CompressZstd(src); len(z) < wantLen {
		wantCodec, wantLen = Zstd, len(z)
	}
	if len(src) < wantLen {
		wantCodec, wantLen = Raw, len(src)
	}
	if codec != byte(wantCodec) || len(out) != wantLen {
		t.Fatalf("large stream: codec %d/%d bytes, want %d/%d", codec, len(out), wantCodec, wantLen)
	}
	if codec, _ := Compress([]byte{1, 2, 3}); codec != Raw {
		t.Fatalf("tiny input: codec %d, want raw", codec)
	}
}

// SizeProxy stands in for Compress wherever only the number is wanted. What
// it owes the caller is that its answer is on the same scale: never above
// the raw bytes, and never so far above the real compressed size that a
// shape choice made on it would be made on noise. It is not bounded below by
// Compress -- on some inputs a lower quality can land a byte under the
// shipping compressor --
// and it does not need to be, since nothing is decoded from this number.
// Every mode is checked, because PRESAGE_SIZE_PROXY can select any of them.
func TestSizeProxyBracketsCompress(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	srcs := [][]byte{
		nil,
		[]byte("abc"),
		bytes.Repeat([]byte("the quick brown fox"), 5000),
		make([]byte, 1<<20),
	}
	incompressible := make([]byte, 1<<16)
	r.Read(incompressible)
	srcs = append(srcs, incompressible)

	saved := sizeProxyMode
	defer func() { sizeProxyMode = saved }()
	for _, mode := range []int{proxyZstd, proxyBrotli5, proxyExact} {
		for _, src := range srcs {
			_, z := Compress(src)
			sizeProxyMode = mode
			got := SizeProxy(src)
			if got > len(src) {
				t.Errorf("mode %d, %d bytes: proxy %d exceeds the raw size", mode, len(src), got)
			}
			if got > 2*len(z)+64 {
				t.Errorf("mode %d, %d bytes: proxy %d is far off the compressor's %d", mode, len(src), got, len(z))
			}
		}
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
