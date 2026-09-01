package elfmod

import (
	"runtime"
	"slices"
	"sync"
)

// sortDedupShards sorts and deduplicates the concatenation of shards, in
// parallel, and returns exactly what slices.Sort + slices.Compact over the
// concatenation would.
//
// The reference-target domain of a Chrome-sized window is 12.8 M addresses
// gathered in per-body shards, and sorting it was half a second of strictly
// serial apply time. The addresses are dense in a known range, so a
// value-range partition beats a comparison merge: each bucket holds a
// disjoint interval, so the buckets sort independently and concatenate
// already sorted, with no merge step and no serial tail. The bucket
// boundaries are derived from the data, so the result cannot depend on how
// the work was split.
func sortDedupShards(shards [][]uint64) []uint64 {
	var n int
	for _, s := range shards {
		n += len(s)
	}
	if n == 0 {
		return nil
	}
	w := workers()
	if n < 1<<16 || w == 1 {
		out := make([]uint64, 0, n)
		for _, s := range shards {
			out = append(out, s...)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}

	// The splitters come from a strided sample rather than from the value
	// range: a handful of the addresses are garbage displacements read out of
	// a data island, and one of those near 2^64 would collapse an even split
	// of [min,max] into a single bucket. Sample quantiles are indifferent to
	// them.
	nb := 4 * w
	const perBucket = 64
	sample := make([]uint64, 0, nb*perBucket)
	stride := max(n/(nb*perBucket), 1)
	for _, s := range shards {
		for i := 0; i < len(s); i += stride {
			sample = append(sample, s[i])
		}
	}
	slices.Sort(sample)
	split := make([]uint64, 0, nb-1)
	for b := 1; b < nb && len(sample) > 0; b++ {
		v := sample[b*len(sample)/nb]
		if len(split) == 0 || v > split[len(split)-1] {
			split = append(split, v)
		}
	}
	nb = len(split) + 1
	// bucketOf is the index of the first splitter greater than v, so the
	// buckets are consecutive disjoint intervals in value order.
	bucketOf := func(v uint64) int {
		lo, hi := 0, len(split)
		for lo < hi {
			m := int(uint(lo+hi) >> 1)
			if split[m] <= v {
				lo = m + 1
			} else {
				hi = m
			}
		}
		return lo
	}

	// Count, prefix-sum, scatter: every element lands at an index fixed by
	// its shard and its bucket, so the scatter is race-free without locks.
	counts := make([][]int, len(shards))
	var wg sync.WaitGroup
	each := func(f func(i int)) {
		sem := make(chan struct{}, w)
		var g sync.WaitGroup
		for i := range shards {
			g.Add(1)
			sem <- struct{}{}
			go func(i int) { defer g.Done(); defer func() { <-sem }(); f(i) }(i)
		}
		g.Wait()
	}
	each(func(i int) {
		c := make([]int, nb)
		for _, v := range shards[i] {
			c[bucketOf(v)]++
		}
		counts[i] = c
	})
	bucketAt := make([]int, nb+1)
	at := make([][]int, len(shards))
	for i := range at {
		at[i] = make([]int, nb)
	}
	pos := 0
	for b := 0; b < nb; b++ {
		bucketAt[b] = pos
		for i := range shards {
			at[i][b] = pos
			pos += counts[i][b]
		}
	}
	bucketAt[nb] = pos
	buf := make([]uint64, n)
	each(func(i int) {
		a := slices.Clone(at[i])
		for _, v := range shards[i] {
			b := bucketOf(v)
			buf[a[b]] = v
			a[b]++
		}
	})

	// Each bucket is a disjoint value interval, so sorting and compacting
	// them independently leaves the concatenation sorted and duplicate-free.
	ends := make([]int, nb)
	sem := make(chan struct{}, w)
	for b := 0; b < nb; b++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(b int) {
			defer wg.Done()
			defer func() { <-sem }()
			s := buf[bucketAt[b]:bucketAt[b+1]]
			slices.Sort(s)
			ends[b] = bucketAt[b] + len(slices.Compact(s))
		}(b)
	}
	wg.Wait()

	dst := make([]int, nb)
	total := 0
	for b := 0; b < nb; b++ {
		dst[b] = total
		total += ends[b] - bucketAt[b]
	}
	out := make([]uint64, total)
	for b := 0; b < nb; b++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(b int) {
			defer wg.Done()
			defer func() { <-sem }()
			copy(out[dst[b]:], buf[bucketAt[b]:ends[b]])
		}(b)
	}
	wg.Wait()
	return out
}

func workersFor(n int) int { return min(max(n, 1), runtime.GOMAXPROCS(0)) }

// sortUint64InPlace partitions s into disjoint value ranges and sorts those
// ranges concurrently. Unlike sortDedupShards it needs no full-size scatter
// buffer, which matters when s itself is an output-sized derived domain.
func sortUint64InPlace(s []uint64) {
	depth := 0
	for 1<<depth < 2*workers() {
		depth++
	}
	var sortPart func([]uint64, int)
	sortPart = func(part []uint64, depth int) {
		if depth == 0 || len(part) < 1<<16 {
			slices.Sort(part)
			return
		}
		const samples = 33
		var sample [samples]uint64
		for i := range sample {
			sample[i] = part[i*(len(part)-1)/(samples-1)]
		}
		slices.Sort(sample[:])
		pivot := sample[samples/2]
		i, j := 0, len(part)
		for i < j {
			if part[i] < pivot {
				i++
				continue
			}
			j--
			part[i], part[j] = part[j], part[i]
		}
		// A heavily duplicated or unrepresentative sample cannot buy useful
		// parallelism. The standard sort handles that case efficiently.
		if i < len(part)/16 || i > 15*len(part)/16 {
			slices.Sort(part)
			return
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			sortPart(part[:i], depth-1)
		}()
		sortPart(part[i:], depth-1)
		wg.Wait()
	}
	sortPart(s, depth)
}

// sortByKey sorts s in place with cmp, in parallel, where cmp orders first on
// the ascending integer key. Elements are partitioned into buckets of disjoint
// key ranges -- so equal keys stay together -- and each bucket is handed to the
// same sort the serial path used, which leaves the concatenation sorted.
//
// The bucket boundaries come from a strided sample of s, so they are a function
// of the input alone and the result does not depend on how the work was split.
func sortByKey[T any](s []T, key func(T) uint64, cmp func(a, b T) int) {
	w := workers()
	if len(s) < 1<<15 || w == 1 {
		slices.SortFunc(s, cmp)
		return
	}
	nb := 4 * w
	const perBucket = 64
	sample := make([]uint64, 0, nb*perBucket)
	for i := 0; i < len(s); i += max(len(s)/(nb*perBucket), 1) {
		sample = append(sample, key(s[i]))
	}
	slices.Sort(sample)
	split := make([]uint64, 0, nb-1)
	for b := 1; b < nb; b++ {
		v := sample[b*len(sample)/nb]
		if len(split) == 0 || v > split[len(split)-1] {
			split = append(split, v)
		}
	}
	if len(split) == 0 {
		slices.SortFunc(s, cmp)
		return
	}
	bucketOf := func(v uint64) int {
		lo, hi := 0, len(split)
		for lo < hi {
			m := int(uint(lo+hi) >> 1)
			if split[m] <= v {
				lo = m + 1
			} else {
				hi = m
			}
		}
		return lo
	}
	nb = len(split) + 1
	// Count, prefix-sum, scatter: the destination of every element is fixed by
	// its shard and its bucket, so the scatter needs no lock.
	nsh := w
	counts := make([][]int, nsh)
	part := func(i int) (int, int) { return i * len(s) / nsh, (i + 1) * len(s) / nsh }
	var wg sync.WaitGroup
	for i := range nsh {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lo, hi := part(i)
			c := make([]int, nb)
			for _, v := range s[lo:hi] {
				c[bucketOf(key(v))]++
			}
			counts[i] = c
		}(i)
	}
	wg.Wait()
	at := make([][]int, nsh)
	for i := range at {
		at[i] = make([]int, nb)
	}
	bucketAt := make([]int, nb+1)
	pos := 0
	for b := range nb {
		bucketAt[b] = pos
		for i := range nsh {
			at[i][b] = pos
			pos += counts[i][b]
		}
	}
	bucketAt[nb] = pos
	buf := make([]T, len(s))
	for i := range nsh {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lo, hi := part(i)
			a := at[i]
			for _, v := range s[lo:hi] {
				b := bucketOf(key(v))
				buf[a[b]] = v
				a[b]++
			}
		}(i)
	}
	wg.Wait()
	copy(s, buf)
	sem := make(chan struct{}, w)
	for b := range nb {
		wg.Add(1)
		sem <- struct{}{}
		go func(b int) {
			defer wg.Done()
			defer func() { <-sem }()
			slices.SortFunc(s[bucketAt[b]:bucketAt[b+1]], cmp)
		}(b)
	}
	wg.Wait()
}

// fill sets every byte of b to v. A byte-at-a-time loop is what Go compiles a
// range assignment to for any value but zero, and .text is 226 MB of it: the
// doubling copy runs at memmove speed instead.
func fill(b []byte, v byte) {
	if len(b) == 0 {
		return
	}
	b[0] = v
	for n := 1; n < len(b); n *= 2 {
		copy(b[n:], b[:n])
	}
}

// shardRange splits [0,n) into one contiguous range per worker and collects
// what f makes of each, in range order.
func shardRange[T any](n int, f func(lo, hi int) T) []T {
	nsh := workersFor(min(n, 1<<20))
	out := make([]T, nsh)
	var wg sync.WaitGroup
	for s := range nsh {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			out[s] = f(s*n/nsh, (s+1)*n/nsh)
		}(s)
	}
	wg.Wait()
	return out
}

// eachRange splits [0,n) into one contiguous range per worker and runs f over
// each concurrently.
func eachRange(n int, f func(lo, hi int)) {
	shardRange(n, func(lo, hi int) struct{} { f(lo, hi); return struct{}{} })
}
