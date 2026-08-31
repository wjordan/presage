// Package eqmatch finds equivalence runs between two byte images: the
// (source offset, destination offset, length) triples along which the
// destination can be predicted by copying the source. It is the core's
// matcher for any format (docs/general/research/matcher-spike.md).
//
// A run is not a common substring. Runs tolerate mismatching bytes inside
// them, because the residual coder fixes the differences for less than a
// second run costs: a matcher that emitted only exact runs produced
// 414,668 of them where 77 were wanted. So a run is grown by the extension
// BLAST calls X-drop and Zucchini calls ExtendEquivalence: walk outwards
// scoring +match per agreeing byte and +mismatch per disagreeing one,
// remember the length at which the running score peaked, stop once the
// score falls Drop below that peak, and cut at the peak, so a run's
// agreeing bytes always outweigh its disagreeing ones.
//
// Anchors come from a chained hash table on fixed-length seeds rather than
// a suffix array: constant expected time per query, one int32 per source
// byte. Two things beyond match length matter, because the runs are coded
// as three varint columns (Encode) and not as a count: a run that continues
// the previous run's delta costs a small source skip whatever its absolute
// offset, so near-ties break towards the delta in force; and runs that abut
// in both images at the same delta merge into one.
package eqmatch

import (
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
)

// Run is one equivalence: N bytes of the destination at Dst predicted by
// the source at Src.
type Run struct {
	Src, Dst, N uint64
}

// Params tunes the matcher. Zero values take the defaults.
type Params struct {
	// Min is the shortest run emitted, and the seed length up to maxSeed.
	// Default 32: a run shorter than the codec can use costs more than it
	// saves.
	Min int
	// Drop is how far the running score may fall below its peak before the
	// extension gives up, in units of match/mismatch below. Default 4096.
	// Zucchini's equivalent is 24: its runs feed a diff whose per-byte
	// correction is expensive, so it cuts at the first sustained
	// disagreement; here the correction is an LZ stream over the whole
	// image that absorbs a disagreeing stretch for almost nothing, while a
	// second run costs three varints. The setting is where this matcher
	// parts company with Zucchini's, and it is worth 50 % on a DWARF pair.
	Drop int
	// Expect, when set, says where a structural model (a function map)
	// predicts the source of destination offset dst. The matcher probes
	// that source as a candidate and breaks near-ties towards it, because a
	// caller with such a model codes the source column as a residual
	// against it, so a run near the expected source is nearly free.
	Expect func(dst int) (src int, ok bool)
	// Slack is the exact-prefix advantage a candidate needs to displace the
	// one nearest the expected source. Default 16.
	Slack int
	// MinFar is the shortest run emitted whose source is not near the
	// expected one: such a run pays a full source offset, and a short one
	// is usually an idiom the residual coder would have compressed anyway.
	// Default Min, which disables the distinction.
	MinFar int
}

// Defaults are the measured-best settings for data whose residual is an
// LZ stream over the whole image (DWARF sections under the Go module).
var Defaults = Params{Min: 32, Drop: 4096}

// CodeDefaults are the measured-best settings for a whole ELF image whose
// .text is matched on x86.Canonical bytes and whose source column is coded
// against a function map (Expect set): Chrome 151 .169 → .173 2,617,700 and
// libxul 154.0 → 154.0.1 3,632,264 through bench/elfpredict, against
// 2,621,664 / 4,063,404 with Zucchini's stream
// (docs/general/research/matcher-chrome.md).
var CodeDefaults = Params{Min: 12, Drop: 24, MinFar: 96, Slack: 16}

const (
	maxSeed  = 16
	match    = 2  // Zucchini's 1.0
	mismatch = -3 // Zucchini's -1.5
	// chain bounds the candidates examined per destination offset: binaries
	// have seeds (zero runs, padding, relocation shapes) that occur hundreds
	// of thousands of times.
	chain = 32
	// probe bounds the exact prefix measured while a candidate is only being
	// scored; the winner is extended, fuzzily, without a limit.
	probe = 1 << 12
	// accept is the exact prefix at which the delta in force is taken
	// without consulting the chain; inside a block that moved whole this
	// keeps the scan linear.
	accept = 1 << 9
	// slack is the prefix advantage a candidate needs before it beats one
	// that continues the delta in force.
	slack = 16
	// near is how far from the expected source the chain is searched past
	// its cap, and nearChain bounds that search. A function that gained a
	// few bytes puts the rest of its old body a little away from where the
	// map says, and the seed there is what the residual column wants.
	near      = 1 << 16
	nearChain = 1 << 12
)

func (p Params) withDefaults() Params {
	if p.Min <= 0 {
		p.Min = Defaults.Min
	}
	if p.Drop <= 0 {
		p.Drop = Defaults.Drop
	}
	if p.Slack <= 0 {
		p.Slack = slack
	}
	if p.MinFar < p.Min {
		p.MinFar = p.Min
	}
	return p
}

// seedIndex maps a seed's hash to the source offsets carrying it. The
// offsets of one hash are packed together in ascending order, so the
// matcher both walks a bucket most-recent-first (from the back) and finds
// the offsets near where the map expects the source without touching the
// ones in between. A chained table would answer the first question just as
// well and the second only by walking, which on a binary's hot seeds —
// padding, zero runs, common relocation shapes — is thousands of dependent
// cache misses per destination offset.
type seedIndex struct {
	// end[h] is where bucket h ends in items; it starts where h-1 ends.
	end   []int32
	items []int32
	mask  uint32
}

func (x *seedIndex) bucket(h uint32) []int32 {
	var lo int32
	if h != 0 {
		lo = x.end[h-1]
	}
	return x.items[lo:x.end[h]]
}

// seedHash mixes the seed's first and last eight bytes; both loads are
// inside the seed for k >= 8.
func seedHash(b []byte, i, k int) uint32 {
	a := binary.LittleEndian.Uint64(b[i:])
	c := binary.LittleEndian.Uint64(b[i+k-8:])
	h := a*0x9e3779b97f4a7c15 ^ c*0xc2b2ae3d27d4eb4f
	h ^= h >> 29
	h *= 0xbf58476d1ce4e5b9
	return uint32(h >> 32)
}

// buildSeedIndex counts each hash's offsets, turns the counts into bucket
// bounds, then places the offsets. The counting pass is the price of the
// packed layout, and it buys back an order of magnitude in the walk.
func buildSeedIndex(src []byte, k int) *seedIndex {
	bits := 16
	for 1<<bits < len(src)/4 && bits < 26 {
		bits++
	}
	n := len(src) - k + 1
	idx := &seedIndex{end: make([]int32, 1<<bits+1), items: make([]int32, n), mask: 1<<bits - 1}
	end := idx.end
	buf := make([]uint32, min(n, 1<<22))
	eachSeedHash(src, k, n, idx.mask, buf, func(_ int, hashes []uint32) {
		for _, h := range hashes {
			end[h+1]++
		}
	})
	for h := 1; h < len(end); h++ {
		end[h] += end[h-1]
	}
	// end[h] is bucket h's start here, and its end once the bucket is full.
	eachSeedHash(src, k, n, idx.mask, buf, func(base int, hashes []uint32) {
		for i, h := range hashes {
			idx.items[end[h]] = int32(base + i)
			end[h]++
		}
	})
	return idx
}

// eachSeedHash hands do the masked hash of every seed in ascending order, a
// chunk at a time. The hashing runs across the pool — it is the one part of
// the build that parallelizes, since both passes over the counts are
// order-dependent — and the chunk buffer keeps the build from holding a
// hash per source byte, which on a whole image is another gigabyte.
func eachSeedHash(src []byte, k, n int, mask uint32, buf []uint32, do func(base int, hashes []uint32)) {
	const grain = 1 << 16
	for base := 0; base < n; base += len(buf) {
		chunk := buf[:min(len(buf), n-base)]
		shards := (len(chunk) + grain - 1) / grain
		var next atomic.Int64
		var wg sync.WaitGroup
		for id := min(runtime.GOMAXPROCS(0), shards); id > 0; id-- {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					s := int(next.Add(1)) - 1
					if s >= shards {
						return
					}
					for i := s * grain; i < min((s+1)*grain, len(chunk)); i++ {
						chunk[i] = seedHash(src, base+i, k) & mask
					}
				}
			}()
		}
		wg.Wait()
		do(base, chunk)
	}
}

// matchForward counts the bytes that agree from the two offsets, up to limit.
func matchForward(src []byte, o int, dst []byte, n int, limit int) int {
	end := min(len(src)-o, len(dst)-n, limit)
	i := 0
	for ; i+8 <= end; i += 8 {
		if binary.LittleEndian.Uint64(src[o+i:]) != binary.LittleEndian.Uint64(dst[n+i:]) {
			break
		}
	}
	for ; i < end; i++ {
		if src[o+i] != dst[n+i] {
			break
		}
	}
	return i
}

// fuzzyForward returns the extension length at which the running score peaks.
func fuzzyForward(src []byte, o int, dst []byte, n int, drop int) int {
	end := min(len(src)-o, len(dst)-n)
	score, best, bestLen, i := 0, 0, 0, 0
	for i < end {
		if score == best && i+8 <= end &&
			binary.LittleEndian.Uint64(src[o+i:]) == binary.LittleEndian.Uint64(dst[n+i:]) {
			i += 8
			score += 8 * match
			best, bestLen = score, i
			continue
		}
		if src[o+i] == dst[n+i] {
			score += match
		} else {
			score += mismatch
		}
		i++
		if score > best {
			best, bestLen = score, i
		} else if score <= best-drop {
			break
		}
	}
	return bestLen
}

// fuzzyBackward is fuzzyForward towards lower offsets, stopping at floor in
// the destination so runs never overlap there.
func fuzzyBackward(src []byte, o int, dst []byte, n int, floor int, drop int) int {
	end := min(o, n-floor)
	score, best, bestLen, i := 0, 0, 0, 0
	for i < end {
		if score == best && i+8 <= end &&
			binary.LittleEndian.Uint64(src[o-i-8:]) == binary.LittleEndian.Uint64(dst[n-i-8:]) {
			i += 8
			score += 8 * match
			best, bestLen = score, i
			continue
		}
		if src[o-i-1] == dst[n-i-1] {
			score += match
		} else {
			score += mismatch
		}
		i++
		if score > best {
			best, bestLen = score, i
		} else if score <= best-drop {
			break
		}
	}
	return bestLen
}

// lowerBound is the first index of the ascending bucket b whose offset is
// at least v; upperBound the first past v.
func lowerBound(b []int32, v int) int {
	return sort.Search(len(b), func(i int) bool { return int(b[i]) >= v })
}

func upperBound(b []int32, v int) int {
	return sort.Search(len(b), func(i int) bool { return int(b[i]) > v })
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Match returns the runs predicting dst from src, in destination order,
// non-overlapping in the destination, each at least p.Min bytes long.
func Match(src, dst []byte, p Params) []Run {
	p = p.withDefaults()
	k := min(max(p.Min, 8), maxSeed)
	if len(src) < k || len(dst) < k || len(src) > math.MaxInt32 {
		return nil
	}
	idx := buildSeedIndex(src, k)
	var out []Run
	var prevSrcEnd, prevDstEnd int
	haveDelta := false
	pos := 0
	for pos+k <= len(dst) {
		expected, hint := -1, -1
		if haveDelta {
			expected = prevSrcEnd + (pos - prevDstEnd)
			if expected >= 0 && expected+k <= len(src) {
				hint = expected
			}
		}
		bestSrc, bestLen := -1, 0
		if hint >= 0 {
			if l := matchForward(src, hint, dst, pos, probe); l >= k {
				bestSrc, bestLen = hint, l
			}
		}
		if p.Expect != nil {
			if e, ok := p.Expect(pos); ok && e >= 0 && e+k <= len(src) {
				expected = e
				if l := matchForward(src, e, dst, pos, probe); l >= k && (bestSrc < 0 || betterCandidate(l, e, bestLen, bestSrc, expected, p.Slack)) {
					bestSrc, bestLen = e, l
				}
			}
		}
		if bestLen < accept {
			b := idx.bucket(seedHash(dst, pos, k) & idx.mask)
			// A bucket runs from low offsets up, so the most recent seeds
			// are at its back.
			j := len(b) - 1
			for seen := 0; j >= 0 && seen < chain; j, seen = j-1, seen+1 {
				o := int(b[j])
				l := matchForward(src, o, dst, pos, probe)
				if l >= k && (bestSrc < 0 || betterCandidate(l, o, bestLen, bestSrc, expected, p.Slack)) {
					bestSrc, bestLen = o, l
				}
			}
			// Keep walking while the expected source may still be ahead,
			// looking only near it. The walk stops below expected-near or
			// after nearChain offsets in all — so at index len(b)-nearChain,
			// whatever the first loop consumed — and the offsets above
			// expected+near cost only their place in that count.
			if j >= 0 && expected >= 0 && (bestSrc < 0 || absInt(bestSrc-expected) > near) {
				floor := max(len(b)-nearChain, lowerBound(b, expected-near))
				for t := min(j, upperBound(b, expected+near)-1); t >= floor; t-- {
					o := int(b[t])
					l := matchForward(src, o, dst, pos, probe)
					if l >= k && (bestSrc < 0 || betterCandidate(l, o, bestLen, bestSrc, expected, p.Slack)) {
						bestSrc, bestLen = o, l
					}
				}
			}
		}
		if bestSrc < 0 {
			pos++
			continue
		}
		fwd := fuzzyForward(src, bestSrc, dst, pos, p.Drop)
		back := fuzzyBackward(src, bestSrc, dst, pos, prevDstEnd, p.Drop)
		s, d, n := bestSrc-back, pos-back, fwd+back
		need := p.Min
		if expected < 0 || absInt(bestSrc-expected) > near {
			need = p.MinFar
		}
		if n < need {
			pos++
			continue
		}
		if len(out) != 0 && d == prevDstEnd && s == prevSrcEnd {
			out[len(out)-1].N += uint64(n)
		} else {
			out = append(out, Run{Src: uint64(s), Dst: uint64(d), N: uint64(n)})
		}
		prevSrcEnd, prevDstEnd = s+n, d+n
		haveDelta = true
		pos = d + n
	}
	return out
}

// betterCandidate is the near-tie rule: a candidate has to be slack bytes
// longer to displace one that starts closer to where the source column
// expects the next run, because the column pays for the difference and
// not for the run.
func betterCandidate(l, o, bestLen, bestSrc, expected, slack int) bool {
	if l >= bestLen+slack {
		return true
	}
	if bestLen >= l+slack {
		return false
	}
	if expected >= 0 {
		d, best := absInt(o-expected), absInt(bestSrc-expected)
		if d != best {
			return d < best
		}
	}
	return l > bestLen
}

// Encode writes runs as three varint columns — signed source skip from the
// previous run's source end, destination gap from the previous run's
// destination end, length — each column contiguous, so the terminal coder
// sees three regular streams. The result is a self-delimiting plan.
func Encode(runs []Run) []byte {
	var src, dst, n []byte
	var tmp [binary.MaxVarintLen64]byte
	var prevSrcEnd, prevDstEnd uint64
	for _, r := range runs {
		src = append(src, tmp[:binary.PutVarint(tmp[:], int64(r.Src)-int64(prevSrcEnd))]...)
		dst = append(dst, tmp[:binary.PutUvarint(tmp[:], r.Dst-prevDstEnd)]...)
		n = append(n, tmp[:binary.PutUvarint(tmp[:], r.N)]...)
		prevSrcEnd, prevDstEnd = r.Src+r.N, r.Dst+r.N
	}
	var out []byte
	for _, col := range [][]byte{src, dst, n} {
		out = append(out, tmp[:binary.PutUvarint(tmp[:], uint64(len(col)))]...)
		out = append(out, col...)
	}
	return out
}

// Decode is Encode's inverse, checking every run against the image sizes.
func Decode(b []byte, srcLen, dstLen uint64) ([]Run, error) {
	var cols [3][]byte
	for i := range cols {
		n, k := binary.Uvarint(b)
		if k <= 0 || n > uint64(len(b)-k) {
			return nil, errors.New("eqmatch: bad column length")
		}
		cols[i], b = b[k:k+int(n)], b[k+int(n):]
	}
	if len(b) != 0 {
		return nil, errors.New("eqmatch: trailing bytes")
	}
	src, dst, cnt := cols[0], cols[1], cols[2]
	var runs []Run
	var prevSrcEnd, prevDstEnd uint64
	for len(dst) != 0 {
		gap, nd := binary.Uvarint(dst)
		n, nc := binary.Uvarint(cnt)
		skip, ns := binary.Varint(src)
		if nd <= 0 || nc <= 0 || ns <= 0 || n == 0 {
			return nil, errors.New("eqmatch: invalid run")
		}
		dst, cnt, src = dst[nd:], cnt[nc:], src[ns:]
		if gap > math.MaxUint64-prevDstEnd {
			return nil, errors.New("eqmatch: destination overflows")
		}
		d := prevDstEnd + gap
		var s uint64
		if skip >= 0 {
			if uint64(skip) > math.MaxUint64-prevSrcEnd {
				return nil, errors.New("eqmatch: source overflows")
			}
			s = prevSrcEnd + uint64(skip)
		} else {
			back := uint64(-(skip + 1)) + 1
			if back > prevSrcEnd {
				return nil, errors.New("eqmatch: source underflows")
			}
			s = prevSrcEnd - back
		}
		if s > srcLen || n > srcLen-s || d > dstLen || n > dstLen-d {
			return nil, errors.New("eqmatch: run leaves an image")
		}
		runs = append(runs, Run{Src: s, Dst: d, N: n})
		prevSrcEnd, prevDstEnd = s+n, d+n
	}
	if len(src) != 0 || len(cnt) != 0 {
		return nil, errors.New("eqmatch: columns have unequal counts")
	}
	return runs, nil
}
