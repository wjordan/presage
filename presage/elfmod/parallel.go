package elfmod

import (
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/wjordan/presage/delta/x86"
)

// workers is the pool size the per-body passes use.
func workers() int { return runtime.GOMAXPROCS(0) }

// parallelStats runs do(st, k) for every k in [0,n) across w workers and
// returns the summed Stats. Correct only when the bodies are independent:
// each k writes a region no other k touches and reads nothing another k
// writes.
func parallelStats(n, w int, do func(st *x86.Stats, k int)) x86.Stats {
	if w < 1 {
		w = 1
	}
	if w > n {
		w = n
	}
	locals := make([]x86.Stats, w)
	var next atomic.Int64
	var wg sync.WaitGroup
	for id := 0; id < w; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var local x86.Stats
			for {
				k := int(next.Add(1)) - 1
				if k >= n {
					break
				}
				do(&local, k)
			}
			locals[id] = local
		}(id)
	}
	wg.Wait()
	var total x86.Stats
	for _, s := range locals {
		total.Add(s)
	}
	return total
}

// parallelFor runs do(k) for every k in [0,n) across the worker pool.
func parallelFor(n int, do func(k int)) {
	parallelStats(n, workers(), func(_ *x86.Stats, k int) { do(k) })
}
