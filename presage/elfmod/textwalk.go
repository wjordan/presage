package elfmod

import (
	"math"
	"sync"

	"github.com/wjordan/presage/delta/x86"
)

// textWalk is the one instruction walk of a predicted code window that both
// field layers read.
//
// Each layer used to walk the window for itself, over the same bodies with the
// same decoder, for two facts that fall out of a single pass: the four-byte
// PC-relative sites the field fix names by position, and how many operand
// fields of each class each body holds, which is where the operand layer's
// domain comes from. Neither depends on the values in those fields -- an
// instruction's length and layout are fixed by its opcode -- so one walk taken
// before the field fix still describes the window the operand layer sees after
// it.
type textWalk struct {
	// sites are the four-byte displacement fields, in the order fieldSites
	// lists them: map order, each body from its own start.
	sites []fieldSite
	// counts holds nOpClass field counts per mapping, in map order. A mapping
	// the window does not contain counts nothing.
	counts []int32
}

// walkChunk is how many sites a shard collects before starting a new slice,
// so a window with millions of them never holds one growing allocation.
const walkChunk = 16 << 10

// newTextWalk walks the mapped bodies of text. sites and counts say which of
// the two results the caller needs; the walk itself is the same either way.
func newTextWalk(text []byte, maps []mapping, sites, counts bool) *textWalk {
	w := &textWalk{}
	if counts && len(maps) != 0 && len(text) <= math.MaxInt32 {
		w.counts = make([]int32, nOpClass*len(maps))
	}
	if len(maps) == 0 {
		if sites {
			w.sites = keepSites(nil, x86.References(text, 0), 0, len(text))
		}
		return w
	}
	// Four shards per worker: mapped bodies differ in size by orders of
	// magnitude, and one shard per worker leaves the machine waiting on
	// whichever drew the big ones.
	shards := shardsOf(len(maps), 4*workers(), func(lo, hi int) [][]fieldSite {
		var parts [][]fieldSite
		var part []fieldSite
		var buf [2]opField
		for k := lo; k < hi; k++ {
			m := maps[k]
			if m.Dst > uint64(len(text)) || m.DstSize > uint64(len(text))-m.Dst {
				continue
			}
			base := int(m.Dst)
			var cnt []int32
			if w.counts != nil {
				cnt = w.counts[k*nOpClass : (k+1)*nOpClass]
			}
			x86.WalkAll(text[base:base+int(m.DstSize)], func(in x86.Insn, f x86.Fields, vouched bool) {
				if sites && in.N == 4 && base+in.Off+4 <= len(text) {
					if part == nil {
						part = make([]fieldSite, 0, walkChunk)
					}
					part = append(part, makeFieldSite(base+in.Off, base+in.Start+in.Length))
					if len(part) == cap(part) {
						parts, part = append(parts, part), nil
					}
				}
				if cnt != nil && vouched {
					for _, fl := range opFieldsOf(f, buf[:0]) {
						cnt[fl.class]++
					}
				}
			})
		}
		if len(part) != 0 {
			parts = append(parts, part)
		}
		return parts
	})
	if !sites {
		return w
	}
	// The shards are contiguous ranges of the map in order, so joining them
	// is a copy to a known offset: the offsets are the prefix sums, and the
	// copies are as parallel as the walk was.
	at := make([]int, len(shards)+1)
	for i, parts := range shards {
		n := 0
		for _, p := range parts {
			n += len(p)
		}
		at[i+1] = at[i] + n
	}
	w.sites = make([]fieldSite, at[len(shards)])
	shardsOf(len(shards), len(shards), func(lo, hi int) struct{} {
		for i := lo; i < hi; i++ {
			out := w.sites[at[i]:]
			for _, p := range shards[i] {
				out = out[copy(out, p):]
			}
		}
		return struct{}{}
	})
	return w
}

// shardsOf splits [0,n) into nsh contiguous ranges and collects what f makes
// of each, in range order.
func shardsOf[T any](n, nsh int, f func(lo, hi int) T) []T {
	nsh = min(max(nsh, 1), max(n, 1))
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

// keepSites is the unmapped arm: a window with no function map is one body.
func keepSites(out []fieldSite, refs []x86.Reference, base, n int) []fieldSite {
	for _, ref := range refs {
		if ref.N == 4 && base+ref.Off+4 <= n {
			out = append(out, makeFieldSite(base+ref.Off, base+ref.Next))
		}
	}
	return out
}
