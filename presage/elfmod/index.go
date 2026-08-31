package elfmod

// The address tables an image carries — the exact points, the function map,
// the equivalence runs — are hundreds of thousands of entries long, and both
// the encoder and the decoder probe them once per reference. A binary search
// over the whole table is all cache misses, and the log n dominates. A
// pageIndex splits the address space into fixed pages and records, per page,
// the slice window a whole-table lower-bound search could land in, so the
// search that remains is over a handful of adjacent entries.
type pageIndex struct {
	base  uint64
	shift uint
	// start[q] is the first entry whose key is at or after page q's start,
	// so every entry from start[q+1] on is keyed past page q.
	start []int32
	// cover[q] is the first entry whose extent reaches past page q's start,
	// so every entry before it ends at or before any address in page q.
	// For a table of points it is start.
	cover []int32
}

// newPageIndex indexes n entries whose keys ascend. end gives the entry's
// exclusive upper bound for a table of ranges, and is nil for a table of
// points. Small tables get no index: the search is already cache-resident.
func newPageIndex(n int, key func(int) uint64, end func(int) uint64) *pageIndex {
	if n < 256 {
		return nil
	}
	base, span := key(0), key(n-1)-key(0)
	shift := uint(6)
	// One page per two entries, so the tables stay proportional to the data.
	for shift < 48 && span>>shift > uint64(n)/2 {
		shift++
	}
	pages := int(span>>shift) + 1
	x := &pageIndex{base: base, shift: shift, start: make([]int32, pages+1)}
	for q, i := 0, 0; q <= pages; q++ {
		at := base + uint64(q)<<shift
		for i < n && key(i) < at {
			i++
		}
		x.start[q] = int32(i)
	}
	if end == nil {
		x.cover = x.start
		return x
	}
	x.cover = make([]int32, pages+1)
	for q, i := 0, 0; q <= pages; q++ {
		at := base + uint64(q)<<shift
		for i < n && end(i) <= at {
			i++
		}
		x.cover[q] = int32(i)
	}
	return x
}

// bounds is the [lo, hi] window of a table of n entries that a lower-bound
// search for addr can land in: every entry below lo compares less and every
// entry at or above hi compares greater. A nil index bounds nothing.
func (x *pageIndex) bounds(addr uint64, n int) (int, int) {
	if x == nil {
		return 0, n
	}
	if addr < x.base {
		return 0, 0
	}
	q := (addr - x.base) >> x.shift
	if q+1 >= uint64(len(x.start)) {
		return int(x.cover[len(x.cover)-1]), n
	}
	return int(x.cover[q]), int(x.start[q+1])
}

// eqCursor answers sourceAt for probes that arrive in ascending order, as
// they do when a body is walked front to back: the runs are disjoint and
// sorted by destination, so the answer is the run the last probe landed in
// or one just after it. A probe that moves backwards searches afresh.
type eqCursor struct {
	eqs []equivalence
	i   int // the run the last probe fell at or after, -1 before the first
}

func newEqCursor(eqs []equivalence) eqCursor { return eqCursor{eqs: eqs, i: -1} }

func (c *eqCursor) at(dst uint64) (uint64, int, bool) {
	i := c.i
	if i < 0 || c.eqs[i].Dst > dst {
		i = eqPredecessor(c.eqs, dst)
	} else {
		for i+1 < len(c.eqs) && c.eqs[i+1].Dst <= dst {
			i++
		}
	}
	c.i = i
	if i < 0 {
		return 0, 0, false
	}
	eq := c.eqs[i]
	if dst-eq.Dst >= eq.N {
		return 0, 0, false
	}
	return eq.Src + dst - eq.Dst, i, true
}

// eqPredecessor is the last run starting at or before dst, or -1.
func eqPredecessor(eqs []equivalence, dst uint64) int {
	lo, hi := 0, len(eqs)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if eqs[mid].Dst <= dst {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo - 1
}
