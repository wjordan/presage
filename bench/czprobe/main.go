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

	"github.com/andybalholm/brotli"
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
			tz += zs
			tb += bs
			tc += int(f.ZLen)
			tx += xs
			fmt.Printf("  frame %d raw %d codec %d -> %d | zstd %d brotli %d xz-9e %d\n", i, f.Len, f.Codec, f.ZLen, zs, bs, xs)
		}
		fmt.Printf("  total: chosen %d | zstd-only %d | brotli-only %d | xz-9e per frame %d | xz-9e whole body %d\n", tc, tz, tb, tx, xzLen(body))
	}
}

func brotliLen(src []byte) int {
	q := 11
	if len(src) > cz.BrotliMax {
		q = 10
	}
	var buf bytes.Buffer
	w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: q, LGWin: 24})
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
