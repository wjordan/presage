package elfmod

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// The parallel primitives must return exactly what the serial code they
// replaced returned, for every shape the apply path can hand them: the
// address domains are millions of entries wide, contain duplicates, and --
// because a displacement read out of a data island can be anything -- run to
// the extremes of the range.
func TestSortDedupShards(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, n := range []int{0, 1, 1000, 1 << 16, 300000} {
		for _, spread := range []string{"dense", "skewed", "constant"} {
			all := make([]uint64, n)
			for i := range all {
				switch spread {
				case "dense":
					all[i] = uint64(r.IntN(max(n/4, 1)))
				case "constant":
					all[i] = 7
				default:
					// Most values in a narrow band, a few at the extremes --
					// the shape that defeats an even split of [min,max].
					switch r.IntN(64) {
					case 0:
						all[i] = ^uint64(0) - uint64(r.IntN(4))
					case 1:
						all[i] = uint64(r.IntN(4))
					default:
						all[i] = 0x5000000 + uint64(r.IntN(1<<20))
					}
				}
			}
			want := slices.Compact(slices.Sorted(slices.Values(all)))
			// Uneven shards, including empty ones.
			var shards [][]uint64
			for at := 0; at < len(all); {
				k := min(r.IntN(1000), len(all)-at)
				shards = append(shards, all[at:at+k])
				at += k
			}
			shards = append(shards, nil)
			got := sortDedupShards(shards)
			if !slices.Equal(got, want) {
				t.Fatalf("n=%d %s: got %d values, want %d", n, spread, len(got), len(want))
			}
		}
	}
}

func TestSortByKey(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for _, n := range []int{0, 1, 1000, 1 << 15, 200000} {
		s := make([]mapping, n)
		for i := range s {
			// Distinct Dst, as the plan guarantees, so the order is total and
			// the parallel result must equal the serial one exactly.
			s[i] = mapping{Src: uint64(r.IntN(max(n/8, 1))), Dst: uint64(i), SrcSize: uint64(i % 13)}
		}
		want := slices.Clone(s)
		slices.SortFunc(want, func(a, b mapping) int {
			if a.Src != b.Src {
				return cmpU(a.Src, b.Src)
			}
			return cmpU(a.Dst, b.Dst)
		})
		got := slices.Clone(s)
		sortByKey(got, func(m mapping) uint64 { return m.Src }, func(a, b mapping) int {
			if a.Src != b.Src {
				return cmpU(a.Src, b.Src)
			}
			return cmpU(a.Dst, b.Dst)
		})
		if !slices.Equal(got, want) {
			t.Fatalf("n=%d: parallel sort differs from slices.SortFunc", n)
		}
	}
}

func TestFill(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 17, 4096, 100000} {
		b := make([]byte, n)
		fill(b, 0xcc)
		for i, v := range b {
			if v != 0xcc {
				t.Fatalf("n=%d: byte %d is %#x", n, i, v)
			}
		}
	}
}
