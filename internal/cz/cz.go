// Package cz is the compression used inside patches and blobs: a one-byte
// codec tag, and an encoder that keeps the smallest of brotli-11, zstd and
// raw.
//
// Pure-Go brotli-11 matches the reference encoder byte for byte and is
// 5-13 % smaller than pure-Go zstd on every patch frame measured (klauspost's
// best level is the equivalent of zstd -11; it has no btopt/btultra), but
// zstd's framing wins by a few dozen bytes on the smallest patches, and
// trying it costs ~30 ms per 8 MiB frame. Brotli quality 10 on large frames
// was 4-5 % larger for 1.7-1.9x the speed and is no longer used.
// See docs/go-module-design.md 2.5.
package cz

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Codec tags. They are written into patch frame tables and must not be
// renumbered.
const (
	Raw    = 0
	Zstd   = 1
	Brotli = 2
)

var (
	encOnce sync.Once
	zEnc    *zstd.Encoder
	zDec    *zstd.Decoder
	decOnce sync.Once
)

func zstdEncoder() *zstd.Encoder {
	encOnce.Do(func() {
		zEnc, _ = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedBestCompression),
			zstd.WithWindowSize(8<<20),
			zstd.WithEncoderConcurrency(1))
	})
	return zEnc
}

func zstdDecoder() *zstd.Decoder {
	decOnce.Do(func() {
		zDec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(4<<30))
	})
	return zDec
}

// CompressZstd compresses with zstd only.
func CompressZstd(src []byte) []byte { return zstdEncoder().EncodeAll(src, nil) }

// Compress returns the smallest of raw, zstd and brotli-11 for src, and the
// tag that names it. It never returns something larger than src itself.
func Compress(src []byte) (codec byte, out []byte) {
	codec, out = Raw, src
	if z := CompressZstd(src); len(z) < len(out) {
		codec, out = Zstd, z
	}
	var buf bytes.Buffer
	buf.Grow(len(out))
	w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: 11, LGWin: 24})
	if _, err := w.Write(src); err == nil && w.Close() == nil && buf.Len() < len(out) {
		codec, out = Brotli, buf.Bytes()
	}
	return codec, out
}

// An encoder choosing between two shapes of the same bytes needs a number,
// not the bytes. Compress gives it the exact number and charges brotli-11 for
// it, and most of what brotli-11 does on this path is thrown away: the losing
// shape is never written. SizeProxy answers the same question with a fast
// compressor, so the trial costs a thirtieth and only the bytes that ship are
// compressed for real.
//
// A proxy's job is to *rank* two candidates the way quality 11 would, so the
// obvious choice is brotli at a low quality -- the same algorithm with a
// smaller search. Measured, it is not: on Chrome the two proxies pick exactly
// the same shapes and cost the same wall time, and on the small streams the
// correction tests use, quality 5 misprices a nine-byte margin that zstd gets
// right. zstd is what a trial already computes on its way to brotli, so it is
// the default. See docs/general/research/encode-profile.md.
const (
	proxyZstd = iota
	proxyBrotli5
	proxyExact // brotli-11: what a trial cost before, for measurement
)

// sizeProxyMode is settable by PRESAGE_SIZE_PROXY ("brotli5", "zstd",
// "exact") so the proxy can be re-measured against the real compressor
// without a rebuild. It changes encode-side choices only; no patch this
// package produces is unreadable under another setting.
var sizeProxyMode, proxyEnvForced = func() (int, bool) {
	switch os.Getenv("PRESAGE_SIZE_PROXY") {
	case "brotli5":
		return proxyBrotli5, true
	case "exact":
		return proxyExact, true
	case "zstd":
		return proxyZstd, true
	}
	return proxyZstd, false
}()

// PreferExactUnder switches trial pricing to the real compressor when the
// target is small enough that the proxy's time saving is trivial (the proxy
// exists for 100 MB+ targets; on a 94 MB pair it saved 0.5 s and cost 327
// patch bytes). The env override wins. Advisory: any mode yields valid
// patches, so a concurrent encode reading a stale mode is harmless.
func PreferExactUnder(targetLen, threshold int64) {
	if proxyEnvForced {
		return
	}
	if targetLen < threshold {
		sizeProxyMode = proxyExact
	} else {
		sizeProxyMode = proxyZstd
	}
}

// counter is the sink for a compressor whose output is only ever measured.
type counter int

func (c *counter) Write(p []byte) (int, error) { *c += counter(len(p)); return len(p), nil }

// SizeProxy estimates what Compress would return for src. It is never larger
// than src, as Compress is not.
func SizeProxy(src []byte) int {
	n := len(src)
	switch sizeProxyMode {
	case proxyExact:
		_, z := Compress(src)
		return len(z)
	case proxyBrotli5:
		var c counter
		w := brotli.NewWriterOptions(&c, brotli.WriterOptions{Quality: 5, LGWin: 24})
		if _, err := w.Write(src); err == nil && w.Close() == nil && int(c) < n {
			n = int(c)
		}
	default:
		if z := len(CompressZstd(src)); z < n {
			n = z
		}
	}
	return n
}

// readAll reads r into buf, refusing to grow past n+1 bytes so that a
// hostile stream cannot make the decoder allocate without bound.
func readAll(r io.Reader, buf []byte, n int) ([]byte, error) {
	for {
		if len(buf) == cap(buf) {
			if len(buf) > n {
				return buf, fmt.Errorf("stream longer than the declared %d bytes", n)
			}
			buf = append(buf, 0)[:len(buf)]
		}
		m, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+m]
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return buf, err
		}
	}
}

// Decompress reverses Compress. n is the expected decompressed length; it
// bounds the allocation, and a stream that does not produce exactly n bytes
// is an error.
func Decompress(codec byte, src []byte, n int) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("cz: negative output length %d", n)
	}
	var out []byte
	var err error
	switch codec {
	case Raw:
		out = src
	case Zstd:
		out, err = zstdDecoder().DecodeAll(src, make([]byte, 0, n))
	case Brotli:
		out = make([]byte, 0, n)
		out, err = readAll(brotli.NewReader(bytes.NewReader(src)), out, n)
	default:
		return nil, fmt.Errorf("cz: unknown codec %d", codec)
	}
	if err != nil {
		return nil, fmt.Errorf("cz: codec %d: %w", codec, err)
	}
	if len(out) != n {
		return nil, fmt.Errorf("cz: codec %d: decompressed %d bytes, want %d", codec, len(out), n)
	}
	return out, nil
}
