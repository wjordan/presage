package main

import (
	"bytes"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// xzCalls, xzBytes and xzNanos let a stage line say how much of itself was xz.
// xzNanos is summed across concurrent calls, so it is CPU-ish time, not wall.
var xzCalls, xzBytes, xzNanos atomic.Int64

// xzJobs bounds how many xz processes run at once.
//
// Every xz call in this benchmark is single-threaded in practice: -T0 only
// splits an input into independently coded blocks above three times the
// dictionary size, which at -9 is 192 MiB, and the largest stream measured
// here is a 64 MB correction. So on a 24-core box the wall time of a
// measurement was the sum of a hundred serial one-core compressions. The bound
// is memory, not cores: -9e holds ~674 MiB per encoder.
//
// The flags stay exactly as they were. Overlapping the calls cannot move a
// reported size -- each process is independent -- whereas changing -T0 to -T1
// would, and would silently move the headline.
var xzJobs = min(runtime.GOMAXPROCS(0), 8)

var (
	xzGateOnce sync.Once
	xzGate     chan struct{}
)

func xzRun(b []byte, threads string) int {
	xzGateOnce.Do(func() {
		if xzJobs < 1 {
			xzJobs = 1
		}
		xzGate = make(chan struct{}, xzJobs)
	})
	xzGate <- struct{}{}
	defer func() { <-xzGate }()
	t0 := time.Now()
	cmd := exec.Command("xz", "-9e", threads, "-c")
	cmd.Stdin = bytes.NewReader(b)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	xzCalls.Add(1)
	xzBytes.Add(int64(len(b)))
	xzNanos.Add(int64(time.Since(t0)))
	if err != nil {
		return 0
	}
	return out.Len()
}

// xzSize reports what the incumbent's terminal compressor makes of a stream.
// Zucchini patches are published as XZ, so every headline comparison in this
// benchmark has to be made with the same coder. It returns 0 when xz is
// absent, and the JSON simply omits the field.
func xzSize(b []byte) int { return xzRun(b, "-T0") }

// xzSizeContiguous compresses as a single block, which is what makes the
// marginal cost of a suffix measurable as the difference of two calls.
// Multithreaded xz splits a large input into independently coded blocks, and
// that would hide exactly the cross-boundary matches a dictionary probe asks
// about.
func xzSizeContiguous(b []byte) int { return xzRun(b, "-T1") }

// xzSizes compresses several streams at once, bounded by xzJobs. Use it
// wherever a diagnostic prints a row of independent column sizes: the sizes
// are identical to the serial ones, and the wall time is not.
func xzSizes(bs ...[]byte) []int {
	out := make([]int, len(bs))
	var wg sync.WaitGroup
	for i, b := range bs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = xzSize(b)
		}()
	}
	wg.Wait()
	return out
}
