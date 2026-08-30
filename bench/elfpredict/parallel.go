package main

import (
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/wjordan/go-binsync/delta/x86"
)

// workers is the pool size the per-body passes use: one goroutine per core.
func workers() int { return runtime.GOMAXPROCS(0) }

// parallelStats runs do(st, k) for every k in [0,n) across a pool of workers
// and returns the summed Stats. Each worker accumulates into a goroutine-local
// Stats and publishes it once at the end, so the millions of counter bumps
// Relocate does never touch a shared cache line.
//
// It is only correct when the bodies are independent: each do(k) must write a
// region of the output no other k writes and read nothing another k is
// writing. The address-field passes satisfy this -- function bodies are laid
// out disjoint and every lookup is a read of an immutable plan -- so the
// result is identical to the serial loop it replaces, order and all.
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
		total.Insns += s.Insns
		total.Fails += s.Fails
		total.Refs += s.Refs
		total.Unknown += s.Unknown
		total.NoFit += s.NoFit
	}
	return total
}
