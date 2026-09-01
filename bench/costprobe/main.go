// costprobe measures the (bytes, time) frontier of the ways one region can
// be coded, so the encoder's forks can be chosen against a declared price
// for a second rather than against a threshold.
//
// Tiers, for a module's prediction and for the lz fallback: the stream in
// its cheapest form priced by a fast compressor, the same stream through the
// shipping compressors, and the adaptive shapes through the shipping
// compressors, which is what the encoder does today.
//
//	costprobe [-symbols OLD,NEW] <old> <new>
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/internal/cz"
	"github.com/wjordan/presage/presage/elfmod"
	"github.com/wjordan/presage/presage/symbols"
)

var fast, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))

func main() {
	symPaths := flag.String("symbols", "", "OLD,NEW function symbols")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: costprobe [-symbols OLD,NEW] old new")
		os.Exit(2)
	}
	old, new := read(flag.Arg(0)), read(flag.Arg(1))
	var syms [2]symbols.Reader
	if *symPaths != "" {
		for i, p := range strings.Split(*symPaths, ",") {
			r, err := symbols.Open(p)
			if err != nil {
				fatal(err)
			}
			syms[i] = r
		}
	}

	t0 := time.Now()
	plan, pred, err := elfmod.Module{Symbols: syms}.Analyse([][]byte{old}, new)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("analyse %s (sunk): plan %d B, %d/%d mispredicted (%.2f%%)\n\n",
		round(time.Since(t0)), len(plan), wrong(pred, new), len(new),
		100*float64(wrong(pred, new))/float64(len(new)))
	fmt.Printf("%-34s %12s %10s\n", "coding", "bytes", "seconds")

	// The module's correction, cheapest first. Plan bytes are added: they
	// ship whichever way the correction is coded.
	row("module: plain shape, zstd-1", len(plan), func() []byte {
		return must(delta.EncodeCorrection(pred, new))
	}, cheap)
	row("module: plain shape, shipping", len(plan), func() []byte {
		return must(delta.EncodeCorrection(pred, new))
	}, shipping)
	row("module: adaptive shapes, shipping", len(plan), func() []byte {
		return must(delta.EncodeCorrectionAdaptive(pred, new))
	}, shipping)

	// Sampling cannot compare two different streams -- the bias is not the
	// same for both -- but the shape choice is two codings of one stream,
	// where most of the bias is common. 1/32 of the region, in windows.
	{
		wins := windows(len(new), 64<<10, 32)
		var plainS, adaptS []byte
		t0 := time.Now()
		for _, w := range wins {
			plainS = append(plainS, must(delta.EncodeCorrection(pred[w[0]:w[1]], new[w[0]:w[1]]))...)
			adaptS = append(adaptS, must(delta.EncodeCorrectionAdaptive(pred[w[0]:w[1]], new[w[0]:w[1]]))...)
		}
		p, a := shipping(plainS), shipping(adaptS)
		fmt.Printf("%-34s %12s %10.1f   sampled adaptive/plain = %.3f\n",
			"  (sample, 1/32)", "", time.Since(t0).Seconds(), float64(a)/float64(p))
	}

	// The fallback, the same way.
	row("lz: zstd-1", 0, func() []byte { return delta.DiffLZ(old, new) }, cheap)
	row("lz: shipping", 0, func() []byte { return delta.DiffLZ(old, new) }, shipping)
}

// windows returns evenly spaced sample windows covering 1/frac of n.
func windows(n, win, frac int) [][2]int {
	count := max(n/(win*frac), 1)
	stride := n / count
	var out [][2]int
	for i := range count {
		if a := i * stride; a+win <= n {
			out = append(out, [2]int{a, a + win})
		}
	}
	return out
}

func cheap(b []byte) int    { return len(fast.EncodeAll(b, nil)) }
func shipping(b []byte) int { _, z := cz.Compress(b); return len(z) }

func row(name string, extra int, produce func() []byte, price func([]byte) int) {
	t0 := time.Now()
	n := price(produce()) + extra
	fmt.Printf("%-34s %12d %10.1f\n", name, n, time.Since(t0).Seconds())
}

func must(b []byte, err error) []byte {
	if err != nil {
		fatal(err)
	}
	return b
}

func wrong(a, b []byte) int {
	n := 0
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			n++
		}
	}
	return n + max(len(a), len(b)) - min(len(a), len(b))
}

func read(p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		fatal(err)
	}
	return b
}

func round(d time.Duration) time.Duration { return d.Round(time.Millisecond) }

func fatal(err error) { fmt.Fprintln(os.Stderr, "costprobe:", err); os.Exit(1) }
