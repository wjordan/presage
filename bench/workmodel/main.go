// workmodel measures the two throughputs the encoder's cost model needs: how
// fast a correction shape is written over a region, and how fast the shipping
// compressors price the stream it produces. Both are per-byte of a different
// quantity, which is why one number cannot stand for the encoder's cost.
//
//	workmodel <old> <new> <label>
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/internal/cz"
	"github.com/wjordan/presage/presage/elfmod"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: workmodel old new label")
		os.Exit(2)
	}
	old, new := read(os.Args[1]), read(os.Args[2])
	_, pred, err := elfmod.Module{}.Analyse([][]byte{old}, new)
	if err != nil {
		fmt.Fprintln(os.Stderr, "workmodel:", err)
		os.Exit(1)
	}
	t0 := time.Now()
	s, err := delta.EncodeCorrection(pred, new)
	if err != nil {
		fmt.Fprintln(os.Stderr, "workmodel:", err)
		os.Exit(1)
	}
	write := time.Since(t0)
	t0 = time.Now()
	_, z := cz.Compress(s)
	comp := time.Since(t0)
	fmt.Printf("%-12s region %10d stream %10d -> %9d | write %6.2fs = %5.1f MB/s of region | price %6.2fs = %5.1f MB/s of stream\n",
		os.Args[3], len(new), len(s), len(z),
		write.Seconds(), float64(len(new))/write.Seconds()/1e6,
		comp.Seconds(), float64(len(s))/comp.Seconds()/1e6)
}

func read(p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "workmodel:", err)
		os.Exit(1)
	}
	return b
}
