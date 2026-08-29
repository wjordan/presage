// czprobe: per-frame codec accounting for presage patches — what cz chose,
// and what each candidate (and xz, for reference) would have cost.
//
//	czprobe patch.psg ...
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"

	"path/filepath"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/wjordan/go-binsync/internal/cz"
	"github.com/wjordan/go-binsync/presage"
)

func main() {
	for _, p := range os.Args[1:] {
		b, err := os.ReadFile(p)
		check(err)
		h, err := presage.ParseHeader(b)
		check(err)
		pos := h.BodyOff
		var tz, tb, tc, tx int
		var body []byte
		fmt.Printf("%s: %d B, header %d B, %d frames\n", p, len(b), h.BodyOff, len(h.Frames))
		for i, f := range h.Frames {
			z := b[pos : pos+f.ZLen]
			pos += f.ZLen
			raw, err := cz.Decompress(f.Codec, z, int(f.Len))
			check(err)
			body = append(body, raw...)
			zs := len(cz.CompressZstd(raw))
			bs := brotliLen(raw)
			xs := xzLen(raw)
			if dump := os.Getenv("CZDUMP"); dump != "" {
				check(os.WriteFile(fmt.Sprintf("%s/%s.f%d", dump, filepathBase(p), i), raw, 0o644))
			}
			fmt.Printf("    klauspost variants: allLit %d single+allLit %d\n", zstdVar(raw, zstd.WithAllLitEntropyCompression(true)), zstdVar(raw, zstd.WithAllLitEntropyCompression(true), zstd.WithSingleSegment(true)))
			tz += zs
			tb += bs
			tc += int(f.ZLen)
			tx += xs
			fmt.Printf("  frame %d raw %d codec %d -> %d | zstd %d brotli %d xz-9e %d\n", i, f.Len, f.Codec, f.ZLen, zs, bs, xs)
		}
		fmt.Printf("  total: chosen %d | zstd-only %d | brotli-only %d | xz-9e per frame %d | xz-9e whole body %d\n", tc, tz, tb, tx, xzLen(body))
	}
}

func zstdVar(src []byte, opts ...zstd.EOption) int {
	opts = append([]zstd.EOption{zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithWindowSize(8 << 20), zstd.WithEncoderConcurrency(1)}, opts...)
	e, err := zstd.NewWriter(nil, opts...)
	check(err)
	return len(e.EncodeAll(src, nil))
}

func filepathBase(p string) string { return filepath.Base(p) }

func brotliLen(src []byte) int {
	var buf bytes.Buffer
	w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: 11, LGWin: 24})
	w.Write(src)
	w.Close()
	return buf.Len()
}

func xzLen(src []byte) int {
	cmd := exec.Command("xz", "-9e", "-c")
	cmd.Stdin = bytes.NewReader(src)
	out, err := cmd.Output()
	check(err)
	return len(out)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
