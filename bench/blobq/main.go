// blobq: blob frame codec measurement — brotli quality 10 vs 11 on 8 MiB
// frames of a whole binary, encoding 8-way parallel, decoding sequentially.
//
//	blobq <binary>
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
)

func main() {
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	const frame = 8 << 20
	var frames [][]byte
	for off := 0; off < len(src); off += frame {
		frames = append(frames, src[off:min(off+frame, len(src))])
	}
	for _, q := range []int{10, 11} {
		zs := make([][]byte, len(frames))
		sem := make(chan struct{}, 8)
		var wg sync.WaitGroup
		t := time.Now()
		for i, f := range frames {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				var buf bytes.Buffer
				w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: q, LGWin: 24})
				w.Write(f)
				w.Close()
				zs[i] = buf.Bytes()
				<-sem
			}()
		}
		wg.Wait()
		enc := time.Since(t)
		total := 0
		for _, z := range zs {
			total += len(z)
		}
		t = time.Now()
		for i, z := range zs {
			out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(z)))
			if err != nil || !bytes.Equal(out, frames[i]) {
				panic("round trip")
			}
		}
		fmt.Printf("brotli-%d: %d frames, %d B, encode %v (8-way), decode %v\n", q, len(frames), total, enc.Round(time.Millisecond), time.Since(t).Round(time.Millisecond))
	}
}
