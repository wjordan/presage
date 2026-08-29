package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
)

// A native equivalence matcher, so that the whole-image equivalence stream no
// longer has to come out of an external Zucchini patch.
//
// An equivalence run is not a common substring. Zucchini's runs tolerate
// mismatching bytes inside them -- on the synthetic pair one run of 14,615,056
// bytes spans a region where thousands of bytes differ -- because the layer
// below fixes the differences and a second run costs three varint columns. A
// matcher that only emitted exact runs produced 414,668 of them where Zucchini
// produced 77, and cost 1,138,136 bytes against Zucchini's 2,652.
//
// So the run is grown by the extension BLAST calls X-drop and Zucchini calls
// ExtendEquivalence: walk outwards adding nativeEqMatch per agreeing byte and
// nativeEqMismatch per disagreeing one, remember the length at which the
// running score peaked, and stop once the score falls nativeEqDrop below that
// peak. The run is cut at the peak, so its matching bytes always outweigh its
// mismatching ones and a run is never worse than leaving the bytes uncovered.
//
// Anchors come from a chained hash table on fixed-length seeds rather than
// Zucchini's suffix array: it answers "where else do these k bytes occur" in
// constant expected time and costs one int32 per old offset, where a suffix
// array over the 181 MB prometheus image would cost four.
//
// Two things beyond match length matter, because the stream is costed as three
// varint columns (encodeColumns) and not as a run count:
//
//   - The source column is a signed skip from the previous run's source end, so
//     a run that continues the previous run's delta writes a small value there
//     whatever its absolute offset is. Ties and near-ties are therefore broken
//     towards the delta already in force rather than towards the longer match.
//   - Runs that abut in both images at the same delta are merged, so a region
//     that moved as a block costs one row however many seeds it took to find.
var (
	// nativeEq selects this matcher over -equivalence-patch.
	nativeEq bool
	// nativeEqMin is the shortest run the matcher will emit, and also its seed
	// length up to nativeEqMaxSeed. Zucchini's own floor is 12; 32 measured
	// better on both pairs, because a run this codec cannot use is a run whose
	// bytes the correction has to undo.
	nativeEqMin = 32
	// nativeEqDrop is how far the running score may fall below its peak before
	// the extension gives up, in the units below. Zucchini's is 12 matching
	// bytes' worth, which is 24 here; this is 170 times as tolerant.
	//
	// The setting is where this matcher parts company with Zucchini's, and the
	// reason is that the two feed different second stages. Zucchini's runs are
	// consumed by a diff whose per-byte correction is expensive, so it pays to
	// cut a run at the first sustained disagreement. Here the correction is an
	// xz'd LZ stream over the whole image, which absorbs a disagreeing stretch
	// for almost nothing, while a second run costs three varints in a plan that
	// is only 60 KB. Extending through disagreement is therefore close to free
	// and cutting is not.
	nativeEqDrop = 4096
)

const (
	nativeEqMaxSeed = 16
	// The extension's score per byte. The 2:-3 ratio is Zucchini's 1.0:-1.5.
	nativeEqMatch    = 2
	nativeEqMismatch = -3
	// nativeEqChain bounds the candidates examined per new offset. Binaries
	// have seeds -- runs of zeroes, padding, repeated relocation shapes -- that
	// occur hundreds of thousands of times, and an unbounded walk spends the
	// whole run on them.
	nativeEqChain = 32
	// nativeEqProbe bounds the exact prefix measured while a candidate is only
	// being scored. The winner is extended, fuzzily, without a limit.
	nativeEqProbe = 1 << 12
	// nativeEqAccept is the exact prefix at which the delta already in force is
	// taken without consulting the hash chain at all. Inside a region that
	// moved as a block this is what keeps the scan linear.
	nativeEqAccept = 1 << 9
	// nativeEqSlack is the prefix advantage a candidate needs before it beats
	// one that continues the delta already in force.
	nativeEqSlack = 16
)

// seedIndex maps a seed's hash to the old offsets carrying it, most recent
// first: head[h] is an offset, next[o] the previous offset in the same bucket,
// and -1 ends a chain.
type seedIndex struct {
	head []int32
	next []int32
	mask uint32
}

// seedHash mixes the seed's first and last eight bytes. Both loads are inside
// the seed for k >= 8, so a k of 8 hashes each byte exactly once.
func seedHash(b []byte, i, k int) uint32 {
	a := binary.LittleEndian.Uint64(b[i:])
	c := binary.LittleEndian.Uint64(b[i+k-8:])
	h := a*0x9e3779b97f4a7c15 ^ c*0xc2b2ae3d27d4eb4f
	h ^= h >> 29
	h *= 0xbf58476d1ce4e5b9
	return uint32(h >> 32)
}

func buildSeedIndex(old []byte, k int) *seedIndex {
	bits := 16
	for 1<<bits < len(old)/4 && bits < 26 {
		bits++
	}
	idx := &seedIndex{
		head: make([]int32, 1<<bits),
		next: make([]int32, len(old)),
		mask: 1<<bits - 1,
	}
	for i := range idx.head {
		idx.head[i] = -1
	}
	for i := 0; i+k <= len(old); i++ {
		h := seedHash(old, i, k) & idx.mask
		idx.next[i] = idx.head[h]
		idx.head[h] = int32(i)
	}
	return idx
}

// matchForward counts the bytes that agree from the two offsets, stopping at
// limit. It is the cheap score a candidate is chosen on; the winner is then
// extended by fuzzyForward, which does not stop at the first disagreement.
func matchForward(old []byte, o int, nw []byte, n int, limit int) int {
	end := min(len(old)-o, len(nw)-n, limit)
	i := 0
	for ; i+8 <= end; i += 8 {
		if binary.LittleEndian.Uint64(old[o+i:]) != binary.LittleEndian.Uint64(nw[n+i:]) {
			break
		}
	}
	for ; i < end; i++ {
		if old[o+i] != nw[n+i] {
			break
		}
	}
	return i
}

// fuzzyForward returns the extension length at which the running score peaks:
// the longest prefix whose agreeing bytes outweigh its disagreeing ones under
// the nativeEqMatch/nativeEqMismatch weights.
func fuzzyForward(old []byte, o int, nw []byte, n int) int {
	end := min(len(old)-o, len(nw)-n)
	score, best, bestLen, i := 0, 0, 0, 0
	for i < end {
		// While the score is at its peak the extension is inside agreeing
		// bytes, which is the whole of a long run; step eight at a time there.
		if score == best && i+8 <= end &&
			binary.LittleEndian.Uint64(old[o+i:]) == binary.LittleEndian.Uint64(nw[n+i:]) {
			i += 8
			score += 8 * nativeEqMatch
			best, bestLen = score, i
			continue
		}
		if old[o+i] == nw[n+i] {
			score += nativeEqMatch
		} else {
			score += nativeEqMismatch
		}
		i++
		if score > best {
			best, bestLen = score, i
		} else if score <= best-nativeEqDrop {
			break
		}
	}
	return bestLen
}

// fuzzyBackward is fuzzyForward run towards lower offsets, stopping at floor in
// the new image so runs never overlap in the destination.
func fuzzyBackward(old []byte, o int, nw []byte, n int, floor int) int {
	end := min(o, n-floor)
	score, best, bestLen, i := 0, 0, 0, 0
	for i < end {
		if score == best && i+8 <= end &&
			binary.LittleEndian.Uint64(old[o-i-8:]) == binary.LittleEndian.Uint64(nw[n-i-8:]) {
			i += 8
			score += 8 * nativeEqMatch
			best, bestLen = score, i
			continue
		}
		if old[o-i-1] == nw[n-i-1] {
			score += nativeEqMatch
		} else {
			score += nativeEqMismatch
		}
		i++
		if score > best {
			best, bestLen = score, i
		} else if score <= best-nativeEqDrop {
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

// nativeEquivalences matches new against old and returns the runs in
// destination order, non-overlapping in the destination, each at least
// nativeEqMin bytes long.
func nativeEquivalences(old, nw []byte) []equivalence {
	k := min(max(nativeEqMin, 8), nativeEqMaxSeed)
	if len(old) < k || len(nw) < k || len(old) > math.MaxInt32 {
		return nil
	}
	idx := buildSeedIndex(old, k)
	var out []equivalence
	// prevSrcEnd and prevDstEnd track the last emitted run; expected is where
	// the source column would like the next run to start.
	var prevSrcEnd, prevDstEnd int
	haveDelta := false

	pos := 0
	for pos+k <= len(nw) {
		expected, hint := -1, -1
		if haveDelta {
			expected = prevSrcEnd + (pos - prevDstEnd)
			if expected >= 0 && expected+k <= len(old) {
				hint = expected
			}
		}
		bestSrc, bestLen := -1, 0
		if hint >= 0 {
			if l := matchForward(old, hint, nw, pos, nativeEqProbe); l >= k {
				bestSrc, bestLen = hint, l
			}
		}
		if bestLen < nativeEqAccept {
			h := seedHash(nw, pos, k) & idx.mask
			seen := 0
			for c := idx.head[h]; c >= 0 && seen < nativeEqChain; c = idx.next[c] {
				seen++
				o := int(c)
				l := matchForward(old, o, nw, pos, nativeEqProbe)
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
		// The winner is extended fuzzily, forwards without a limit and
		// backwards into whatever the previous run left uncovered.
		fwd := fuzzyForward(old, bestSrc, nw, pos)
		back := fuzzyBackward(old, bestSrc, nw, pos, prevDstEnd)
		src, dst, n := bestSrc-back, pos-back, fwd+back
		if n < nativeEqMin {
			pos++
			continue
		}
		if len(out) != 0 && dst == prevDstEnd && src == prevSrcEnd {
			out[len(out)-1].N += uint64(n)
		} else {
			out = append(out, equivalence{Src: uint64(src), Dst: uint64(dst), N: uint64(n)})
		}
		prevSrcEnd, prevDstEnd = src+n, dst+n
		haveDelta = true
		pos = dst + n
	}
	return out
}

// betterCandidate implements the near-tie rule: a candidate has to be
// nativeEqSlack bytes longer to displace one that starts closer to where the
// source column expects the next run, because the column pays for the
// difference and not for the run.
func betterCandidate(l, o, bestLen, bestSrc, expected int) bool {
	if l >= bestLen+nativeEqSlack {
		return true
	}
	if bestLen >= l+nativeEqSlack {
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

// nativeEquivalencePlan is parseExternalEquivalence's replacement: the same
// plan, with the runs matched here instead of read out of a Zucchini patch.
func nativeEquivalencePlan(oldImage, newImage *image) (equivalencePlan, error) {
	t := startStage("native equivalences")
	eqs := nativeEquivalences(oldImage.Data, newImage.Data)
	var covered uint64
	for _, e := range eqs {
		covered += e.N
	}
	p := equivalencePlan{
		OldLen: uint64(len(oldImage.Data)), NewLen: uint64(len(newImage.Data)),
		OldText: oldImage.Text, NewText: newImage.Text, Eqs: eqs,
	}
	p.SrcSkip, p.DstSkip, p.CopyLen = encodeColumns(eqs)
	t.done("min %d: %d runs covering %d of %d new bytes (%.3f%%)",
		nativeEqMin, len(eqs), covered, len(newImage.Data),
		100*float64(covered)/float64(max(len(newImage.Data), 1)))
	return p, nil
}

// buildEquivalencePlan supplies runCombined's equivalences from whichever
// source the flags name.
func buildEquivalencePlan(externalPath string, oldImage, newImage *image) (equivalencePlan, error) {
	var p equivalencePlan
	var err error
	if nativeEq {
		p, err = nativeEquivalencePlan(oldImage, newImage)
	} else {
		p, err = parseExternalEquivalence(externalPath, oldImage, newImage)
	}
	if err != nil {
		return equivalencePlan{}, err
	}
	textLo, textHi := p.NewText.Off, p.NewText.Off+p.NewText.Size
	var inText, textBytes, covered uint64
	for _, e := range p.Eqs {
		covered += e.N
		if e.Dst < textHi && e.Dst+e.N > textLo {
			inText++
			textBytes += min(e.Dst+e.N, textHi) - max(e.Dst, textLo)
		}
	}
	fmt.Fprintf(os.Stderr, "  equivalences: %d runs, %d bytes covered; %d runs touch .text, covering %d of its %d bytes\n",
		len(p.Eqs), covered, inText, textBytes, p.NewText.Size)
	return p, nil
}

// checkNativeEqFlags rejects the combinations that would silently measure
// something other than what was asked for.
func checkNativeEqFlags(externalPath string) error {
	if nativeEq && externalPath != "" {
		return errors.New("-native-equivalences and -equivalence-patch are alternatives")
	}
	if nativeEqMin < 4 {
		return errors.New("-native-eq-min must be at least 4")
	}
	return nil
}
