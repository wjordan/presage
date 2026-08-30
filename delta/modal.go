package delta

import (
	"fmt"
	"math"

	"github.com/wjordan/go-binsync/internal/cz"

	"github.com/wjordan/go-binsync/delta/internal/lz"
)

// The modal correction is the positional correction with the transform named
// per region rather than per file (docs/general/SPEC.md 6.1, research
// bsdiff6-spike.md).
//
// The shipped shapes write a differing region's bytes either as the target's
// literals or as want^pred, one transform for the whole file. But a
// mispredicted address that moved by a small amount is *numerically* close,
// not bitwise close, and how close depends on the region: a relocation table
// wants eight-byte balanced digits, a patched call site wants four, and
// recompiled code wants literals. Naming the transform per region and giving
// each transform its own sub-stream is worth roughly a tenth of the patch on
// a binary whose pointer tables no module predicts.
//
// Balanced digits are Percival's multiprecision difference (thesis 2.7): the
// difference want-pred over one machine word, re-expressed base 256 with
// digits in [-128,127] so a borrow propagates across the word instead of
// landing in the stream. A pointer that moved by -1 is one digit and a
// significance count, not eight literals.

const (
	modeLit = iota // the target's own bytes
	modeLZ         // (lit, copy, seek) triples over the prediction's bytes here
	modeXor        // want^pred
	modeSub        // want-pred, bytewise
	modeMP4        // balanced digits over 4-byte words
	modeMP8        // balanced digits over 8-byte words
	nmodes
)

// modeWord is the word width a mode's digits cover; 1 means bytewise.
var modeWord = [nmodes]int{1, 1, 1, 1, 4, 8}

// modalMerge is how many identical bytes a region absorbs. A near-miss
// residual is runs a few bytes apart, and under every transform but literals
// the swallowed bytes are correct and so code as zeros; 32 measured best and
// is what the near-miss shape already uses.
const modalMerge = 32

// modalWord is the boundary regions are widened to, so that a digit covers a
// whole field rather than straddling two.
const modalWord = 8

// mpDigits appends want-pred over each word of the region as a significance
// count followed by that many balanced base-256 digits, low digit first, high
// zero digits dropped. The count is what lets the decoder find the next word
// without the region's digit length being transmitted.
func mpDigits(pred, want []byte, word int, dst []byte) []byte {
	for i := 0; i < len(want); i += word {
		w := min(word, len(want)-i)
		var p, t uint64
		for j := w - 1; j >= 0; j-- {
			p = p<<8 | uint64(pred[i+j])
			t = t<<8 | uint64(want[i+j])
		}
		d := int64(t - p)
		if w < 8 { // sign-extend the difference to the field's width
			sh := uint(64 - 8*w)
			d = int64(uint64(d)<<sh) >> sh
		}
		var dig [8]byte
		nsig := 0
		for j := 0; j < w; j++ {
			v := d & 0xff
			if v > 127 {
				v -= 256
			}
			d = (d - v) >> 8
			dig[j] = byte(v)
			if v != 0 {
				nsig = j + 1
			}
		}
		dst = append(dst, byte(nsig))
		dst = append(dst, dig[:nsig]...)
	}
	return dst
}

// mpApply rewrites buf in place from src, and returns what is left of src.
func mpApply(buf []byte, word int, src []byte) ([]byte, bool) {
	for i := 0; i < len(buf); i += word {
		w := min(word, len(buf)-i)
		if len(src) == 0 {
			return nil, false
		}
		nsig := int(src[0])
		src = src[1:]
		if nsig > w || nsig > len(src) {
			return nil, false
		}
		// The digits are balanced: each is a *signed* byte weighted by
		// 256^j. Reading them back as an unsigned little-endian integer is
		// wrong whenever a low digit is negative and a higher one is not.
		var d int64
		for j := nsig - 1; j >= 0; j-- {
			d += int64(int8(src[j])) << (8 * j)
		}
		src = src[nsig:]
		var p uint64
		for j := w - 1; j >= 0; j-- {
			p = p<<8 | uint64(buf[i+j])
		}
		t := uint64(int64(p) + d)
		for j := 0; j < w; j++ {
			buf[i+j] = byte(t >> (8 * j))
		}
	}
	return src, true
}

// emitMode appends the region's bytes under one transform.
func emitMode(mode int, pred, want, dst []byte) []byte {
	switch mode {
	case modeXor:
		for i := range want {
			dst = append(dst, pred[i]^want[i])
		}
	case modeSub:
		for i := range want {
			dst = append(dst, want[i]-pred[i])
		}
	case modeMP4, modeMP8:
		dst = mpDigits(pred, want, modeWord[mode], dst)
	default:
		dst = append(dst, want...)
	}
	return dst
}

// modalRegions enumerates the differing stretches. Boundaries depend only on
// the merge distance and the word alignment, never on the transform, so a
// mode chosen per ordinal lines up with what modalWrite produces.
func modalRegions(pred, want []byte) [][2]int {
	var out [][2]int
	prevEnd := 0
	for s := 0; s < len(want); {
		if pred[s] == want[s] {
			s++
			continue
		}
		e := s + 1
		for e < len(want) {
			k := e
			for k < len(want) && k-e < modalMerge && pred[k] == want[k] {
				k++
			}
			if k-e < modalMerge && k < len(want) {
				e = k + 1
				continue
			}
			break
		}
		s = s &^ (modalWord - 1)
		e = min((e+modalWord-1)&^(modalWord-1), len(want))
		if s < prevEnd {
			s = prevEnd
		}
		out = append(out, [2]int{s, e})
		prevEnd, s = e, e
	}
	return out
}

// A selector has to price a candidate, not measure it: the transforms that
// are not multiprecision all emit exactly one byte per byte, so choosing by
// length cannot separate literals from xor from sub at all. Scoring a
// candidate by what its bytes cost under an order-0 model of the sub-stream
// they would join is the cheapest score that is a compressed size rather than
// a raw one, and because each transform has its own stream, each has its own
// model. Three passes, each trained on what the last one chose, is enough;
// the models stop moving after that.
type modeModel struct {
	bits [nmodes][256]float64
	set  bool
}

func (m *modeModel) train(hist *[nmodes][256]int) {
	for mode := range hist {
		total := 0.0
		for _, c := range hist[mode] {
			total += float64(c)
		}
		for b, c := range hist[mode] {
			// Laplace, so a byte this pass never saw is expensive but not
			// impossible -- the next pass may well want it.
			m.bits[mode][b] = -math.Log2((float64(c) + 0.5) / (total + 128))
		}
	}
	m.set = true
}

func (m *modeModel) cost(mode int, b []byte) float64 {
	if !m.set {
		return 8 * float64(len(b)) // first pass: every byte costs a byte
	}
	c := 0.0
	tab := &m.bits[histOf(mode)]
	for _, v := range b {
		c += tab[v]
	}
	return c
}

// histOf is which model a mode is scored against. The two multiprecision
// widths share one: their bytes are the same alphabet -- small signed digits
// and a significance count -- and giving each its own turns the selector into
// a feedback loop, where whichever width the first pass happens to under-use
// looks expensive for every pass after. Sharing costs 0.5 % of the correction
// to leave out.
func histOf(mode int) int {
	if mode == modeMP8 {
		return modeMP4
	}
	return mode
}

// modalOut is the modal correction's streams before they are packed.
type modalOut struct {
	gaps, spans, ops, modes []byte
	lit                     [nmodes][]byte
	nregions                int
	hist                    [nmodes][256]int
}

func (o *modalOut) streams() [][]byte {
	out := [][]byte{o.gaps, o.spans, o.ops, o.modes}
	for i := range o.lit {
		out = append(out, o.lit[i])
	}
	return out
}

// modalWrite lays the correction out, choosing a mode per region. The lz mode
// is priced against the best literal mode by raw size, as the shipped shapes
// do, because an lz region's cost is the ops it adds to a shared stream and
// not a payload the model has seen.
func modalWrite(pred, want []byte, rs [][2]int, m *modeModel) *modalOut {
	o := &modalOut{}
	prevEnd := 0
	for _, r := range rs {
		s, e := r[0], r[1]
		n := e - s
		putU(&o.gaps, uint64(s-prevEnd))

		best, bestBuf, bestCost := modeLit, []byte(nil), math.Inf(1)
		for mode := 0; mode < nmodes; mode++ {
			if mode == modeLZ {
				continue
			}
			b := emitMode(mode, pred[s:e], want[s:e], nil)
			if c := m.cost(mode, b); c < bestCost {
				best, bestBuf, bestCost = mode, b, c
			}
		}

		// candidate: an lz stream over the prediction's own bytes here
		opMark, litMark := len(o.ops), len(o.lit[modeLit])
		c2, l2 := lz.Emit(pred[s:e+slackAt(e, len(pred))], want[s:e], o.ops, o.lit[modeLit])
		if len(c2)-opMark+len(l2)-litMark < len(bestBuf) {
			o.ops, o.lit[modeLit] = c2, l2
			best = modeLZ
		} else {
			o.ops, o.lit[modeLit] = o.ops[:opMark], o.lit[modeLit][:litMark]
			for _, v := range bestBuf {
				o.hist[histOf(best)][v]++
			}
			o.lit[best] = append(o.lit[best], bestBuf...)
		}
		putU(&o.spans, uint64(n))
		o.modes = append(o.modes, byte(best))
		o.nregions++
		prevEnd = e
	}
	return o
}

// modalPack writes the streams with their lengths. Two forms, because which
// one wins is a property of the file rather than of the coder: shape 2 hands
// the sub-streams to the container as one buffer, so they share a compression
// context, and shape 3 compresses each on its own first.
//
// Sharing helps when the streams are small -- prometheus's whole correction is
// 40 KB and its ten sub-streams have too little history each to pay for being
// separated. It hurts when they are large: libxul's mp8 digits and its .text
// literals have nothing to say to each other, and keeping them apart is worth
// 1.75 %. The encoder writes both and keeps the smaller, which is what the
// shape picker already does one level up.
func modalPack(o *modalOut, n int, split bool) []byte {
	shape := uint64(modalShape)
	if split {
		shape = modalSplitShape
	}
	w := &wbuf{}
	w.u(uint64(n))
	w.u(uint64(o.nregions)<<shapeBits | shape)
	if !split {
		for _, b := range o.streams() {
			w.u(uint64(len(b)))
		}
		for _, b := range o.streams() {
			w.raw(b)
		}
		return w.b
	}
	zs := make([][]byte, 0, 4+nmodes)
	for _, b := range o.streams() {
		codec, z := cz.Compress(b)
		w.u(uint64(len(b)))
		w.u(uint64(len(z)))
		w.raw([]byte{codec})
		zs = append(zs, z)
	}
	for _, z := range zs {
		w.raw(z)
	}
	return w.b
}

// applyModal rewrites buf, which holds the prediction, from a shape-2 stream.
// nregions has already been read; r is positioned at the stream lengths.
func applyModal(buf []byte, r *rbuf, nregions uint64, split bool) error {
	var st [4 + nmodes][]byte
	if split {
		var raw, zlen [4 + nmodes]uint64
		var codec [4 + nmodes]byte
		for i := range st {
			raw[i] = r.un(uint64(len(buf))+uint64(len(r.b)), "stream length")
			zlen[i] = r.un(uint64(len(r.b)), "compressed stream length")
			codec[i] = r.byteAt()
		}
		if r.err != nil {
			return r.err
		}
		for i := range st {
			z := r.take(zlen[i])
			if r.err != nil {
				return r.err
			}
			b, err := cz.Decompress(codec[i], z, int(raw[i]))
			if err != nil {
				return fmt.Errorf("%w: stream %d: %v", errCorrupt, i, err)
			}
			st[i] = b
		}
	} else {
		var lens [4 + nmodes]uint64
		for i := range lens {
			lens[i] = r.un(uint64(len(r.b)), "stream length")
		}
		if r.err != nil {
			return r.err
		}
		for i := range st {
			st[i] = r.take(lens[i])
		}
	}
	if err := r.done(); err != nil {
		return err
	}
	gaps, spans := &rbuf{b: st[0]}, &rbuf{b: st[1]}
	modes := st[3]
	lit := st[4:]
	rd := &lz.Reader{Ctrl: st[2], Lit: lit[modeLit]}

	var scratch []byte
	pos := 0
	for i := uint64(0); i < nregions; i++ {
		gap := gaps.un(uint64(len(buf)), "region gap")
		span := spans.un(uint64(len(buf)), "region span")
		if gaps.err != nil {
			return gaps.err
		}
		if spans.err != nil {
			return spans.err
		}
		if len(modes) == 0 {
			return fmt.Errorf("%w: region %d has no mode", errCorrupt, i)
		}
		mode := int(modes[0])
		modes = modes[1:]
		if mode >= nmodes {
			return fmt.Errorf("%w: region %d names mode %d", errCorrupt, i, mode)
		}
		if gap > uint64(len(buf)-pos) {
			return fmt.Errorf("%w: region %d starts past the end of the file", errCorrupt, i)
		}
		pos += int(gap)
		if span > uint64(len(buf)-pos) {
			return fmt.Errorf("%w: region %d spans past the end of the file", errCorrupt, i)
		}
		end := pos + int(span)

		if mode == modeLZ {
			slack := slackAt(end, len(buf))
			src := append(scratch[:0], buf[pos:end+slack]...)
			scratch = src
			rd.Lit = lit[modeLit]
			if err := rd.Apply(src, buf[pos:end]); err != nil {
				return fmt.Errorf("%w: region %d: %v", errCorrupt, i, err)
			}
			lit[modeLit] = rd.Lit
			pos = end
			continue
		}
		switch mode {
		case modeMP4, modeMP8:
			rest, ok := mpApply(buf[pos:end], modeWord[mode], lit[mode])
			if !ok {
				return fmt.Errorf("%w: region %d: short digit stream", errCorrupt, i)
			}
			lit[mode] = rest
		case modeXor, modeSub:
			if span > uint64(len(lit[mode])) {
				return fmt.Errorf("%w: region %d wants %d bytes, %d remain", errCorrupt, i, span, len(lit[mode]))
			}
			if mode == modeXor {
				for j := range int(span) {
					buf[pos+j] ^= lit[mode][j]
				}
			} else {
				for j := range int(span) {
					buf[pos+j] += lit[mode][j]
				}
			}
			lit[mode] = lit[mode][span:]
		default:
			if span > uint64(len(lit[modeLit])) {
				return fmt.Errorf("%w: region %d wants %d literals, %d remain", errCorrupt, i, span, len(lit[modeLit]))
			}
			copy(buf[pos:end], lit[modeLit][:span])
			lit[modeLit] = lit[modeLit][span:]
		}
		pos = end
	}
	for _, rest := range [][]byte{gaps.b, spans.b, rd.Ctrl, modes} {
		if len(rest) != 0 {
			return fmt.Errorf("%w: %d control bytes after the last region", errCorrupt, len(rest))
		}
	}
	for i, rest := range lit {
		if len(rest) != 0 {
			return fmt.Errorf("%w: %d bytes left in mode-%d stream", errCorrupt, len(rest), i)
		}
	}
	return nil
}

// encodeModal writes the correction with the mode chosen per region. The
// models are bootstrapped by a pass that prices every byte at a byte, and
// each further pass re-chooses against what the last one wrote.
func encodeModal(pred, want []byte) []byte {
	rs := modalRegions(pred, want)
	m := &modeModel{}
	var o *modalOut
	for pass := 0; pass < modalPasses; pass++ {
		o = modalWrite(pred, want, rs, m)
		m = &modeModel{}
		m.train(&o.hist)
	}
	o = modalWrite(pred, want, rs, m)
	a, b := modalPack(o, len(want), false), modalPack(o, len(want), true)
	if czLen(b) < czLen(a) {
		return b
	}
	return a
}

// modalPasses is how many times the selector re-chooses against its own
// output. Three is where the models stop moving; the passes cost less than
// the second compression the shape picker no longer has to do.
const modalPasses = 3
