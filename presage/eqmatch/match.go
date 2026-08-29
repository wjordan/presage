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
}

// Defaults are the measured-best settings.
var Defaults = Params{Min: 32, Drop: 4096}

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
)

func (p Params) withDefaults() Params {
	if p.Min <= 0 {
		p.Min = Defaults.Min
	}
	if p.Drop <= 0 {
		p.Drop = Defaults.Drop
	}
	return p
}

// seedIndex maps a seed's hash to the source offsets carrying it, most
// recent first: head[h] is an offset, next[o] the previous offset in the
// same bucket, and -1 ends a chain.
type seedIndex struct {
	head []int32
	next []int32
	mask uint32
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

func buildSeedIndex(src []byte, k int) *seedIndex {
	bits := 16
	for 1<<bits < len(src)/4 && bits < 26 {
		bits++
	}
	idx := &seedIndex{head: make([]int32, 1<<bits), next: make([]int32, len(src)), mask: 1<<bits - 1}
	for i := range idx.head {
		idx.head[i] = -1
	}
	for i := 0; i+k <= len(src); i++ {
		h := seedHash(src, i, k) & idx.mask
		idx.next[i] = idx.head[h]
		idx.head[h] = int32(i)
	}
	return idx
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
		if bestLen < accept {
			h := seedHash(dst, pos, k) & idx.mask
			seen := 0
			for c := idx.head[h]; c >= 0 && seen < chain; c = idx.next[c] {
				seen++
				o := int(c)
				l := matchForward(src, o, dst, pos, probe)
				if l < k {
					continue
				}
				if bestSrc < 0 || betterCandidate(l, o, bestLen, bestSrc, expected) {
					bestSrc, bestLen = o, l
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
		if n < p.Min {
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
func betterCandidate(l, o, bestLen, bestSrc, expected int) bool {
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
