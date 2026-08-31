package elfmod

import (
	"math/rand"
	"slices"
	"testing"
)

// TestPageIndexBounds checks the contract the windowed searches rely on:
// a lower-bound search restricted to bounds() lands where one over the
// whole table would.
func TestPageIndexBounds(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, n := range []int{0, 1, 255, 300, 5000} {
		type rng struct{ lo, size uint64 }
		var ranges []rng
		var at uint64 = 1000
		for i := 0; i < n; i++ {
			at += uint64(r.Intn(400))
			size := uint64(r.Intn(300))
			ranges = append(ranges, rng{at, size})
			at += size
		}
		key := func(i int) uint64 { return ranges[i].lo }
		end := func(i int) uint64 { return ranges[i].lo + ranges[i].size }
		cmp := func(x rng, addr uint64) int {
			if x.lo > addr {
				return 1
			}
			if x.lo+x.size <= addr {
				return -1
			}
			return 0
		}
		for _, points := range []bool{false, true} {
			var x *pageIndex
			if points {
				x = newPageIndex(n, key, nil)
			} else {
				x = newPageIndex(n, key, end)
			}
			for probe := 0; probe < 2000; probe++ {
				addr := uint64(r.Intn(int(at + 2000)))
				wantI, wantOK := slices.BinarySearchFunc(ranges, addr, cmp)
				lo, hi := x.bounds(addr, n)
				if points { // a points index bounds only exact keys
					_, gotOK := slices.BinarySearchFunc(ranges[lo:hi], addr, func(x rng, addr uint64) int {
						return cmpU(x.lo, addr)
					})
					_, exact := slices.BinarySearchFunc(ranges, addr, func(x rng, addr uint64) int {
						return cmpU(x.lo, addr)
					})
					if gotOK != exact {
						t.Fatalf("n=%d addr=%d: exact hit %v, want %v", n, addr, gotOK, exact)
					}
					continue
				}
				gotI, gotOK := slices.BinarySearchFunc(ranges[lo:hi], addr, cmp)
				if gotOK != wantOK || (gotOK && lo+gotI != wantI) {
					t.Fatalf("n=%d addr=%d: got (%d,%v) in [%d,%d), want (%d,%v)", n, addr, lo+gotI, gotOK, lo, hi, wantI, wantOK)
				}
			}
		}
	}
}

// TestEqCursor checks the cursor against a search over the whole run list,
// for probes that ascend as a walked body's do and for ones that do not.
func TestEqCursor(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	var eqs []equivalence
	var at uint64
	for i := 0; i < 500; i++ {
		at += uint64(r.Intn(50))
		n := uint64(1 + r.Intn(80))
		eqs = append(eqs, equivalence{Src: at * 2, Dst: at, N: n})
		at += n
	}
	want := func(dst uint64) (uint64, int, bool) {
		i, ok := slices.BinarySearchFunc(eqs, dst, func(eq equivalence, dst uint64) int {
			if eq.Dst > dst {
				return 1
			}
			if eq.Dst+eq.N <= dst {
				return -1
			}
			return 0
		})
		if !ok {
			return 0, 0, false
		}
		return eqs[i].Src + dst - eqs[i].Dst, i, true
	}
	c := newEqCursor(eqs)
	for probe := uint64(0); probe < at+20; probe++ {
		gotSrc, gotI, gotOK := c.at(probe)
		wantSrc, wantI, wantOK := want(probe)
		if gotSrc != wantSrc || gotOK != wantOK || (gotOK && gotI != wantI) {
			t.Fatalf("ascending %d: got (%d,%d,%v), want (%d,%d,%v)", probe, gotSrc, gotI, gotOK, wantSrc, wantI, wantOK)
		}
	}
	c = newEqCursor(eqs)
	for i := 0; i < 5000; i++ {
		probe := uint64(r.Intn(int(at + 20)))
		gotSrc, gotI, gotOK := c.at(probe)
		wantSrc, wantI, wantOK := want(probe)
		if gotSrc != wantSrc || gotOK != wantOK || (gotOK && gotI != wantI) {
			t.Fatalf("random %d: got (%d,%d,%v), want (%d,%d,%v)", probe, gotSrc, gotI, gotOK, wantSrc, wantI, wantOK)
		}
	}
}
