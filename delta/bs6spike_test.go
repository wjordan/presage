package delta

import (
	"bytes"
	"debug/elf"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjordan/go-binsync/delta/internal/lz"
)

// TestBS6Spike prices Percival's difference-string modes (thesis 2.7, the
// unreleased "bsdiff 6") inside the correction this package actually ships,
// so the comparison keeps the lz-region option that carries the long runs.
//
// The shipped coder writes a differing region either as an lz stream over the
// prediction's own bytes or as literals, and the literals are either the
// target's bytes (the default shape) or want^pred (the near-miss shape).
// bsdiff has always written want-pred instead, and bsdiff 6 adds
// multiprecision modes so a borrow can propagate across the bytes of one
// field. This measures all of them.
//
//	BS6_PRED=... BS6_TARGET=... go test ./delta -run BS6Spike -v
type spikeShape struct {
	merge  int
	tr     int  // literal transform, see trName
	word   int  // widen regions to this alignment (1 = off)
	cols   bool // gaps, spans, ops and literals as separate streams
	bucket bool // literals split by region length
	perReg bool // choose the transform per region, with a mode column
	// byPos names the transform for a region by where it starts, so a
	// section can pick its own mode while every region still writes into
	// the one set of streams. This is the shape SPEC 5 already implies and
	// the one the per-section table below argues for; the mode column is
	// what the region record would carry.
	byPos func(int) (tr, word int)
	// byMode sends a region's literals to the sub-stream of its transform
	// rather than of its length, so mixing modes does not mix statistics
	// (SPEC 6.3's typed sub-streams).
	byMode bool
	// byModeW routes by the full mode code rather than the transform, so
	// mp4 digits and mp8 digits get separate streams. They are different
	// alphabets -- a 4-byte significance count never exceeds 4 -- and
	// sharing one stream is exactly the dilution byMode was meant to stop.
	byModeW bool
	// cost, when set, is what a candidate's bytes are scored by. perReg
	// without it scores by length, which cannot separate lit, xor and sub at
	// all -- they always emit exactly one byte per byte -- so the selection
	// it measures is only "mp or not". A real selector needs to price the
	// bytes, not count them.
	cost func(tr int, b []byte) float64
	// byIdx names the mode for a region by its ordinal, so a selector that
	// looks at the whole sequence at once (viterbiModes) can drive write.
	byIdx []byte
	// noMPLen drops the per-region digit-stream length. It is derivable:
	// the decoder walks span/word significance counts and the digits that
	// follow each. On libxul that varint is paid 237,887 times.
	noMPLen bool
	// mpSplit sends the per-word significance counts to their own
	// sub-stream, which is Percival's map/value split at the granularity
	// where the map is a column of small counts rather than a bitmap over
	// the whole file. It also makes the length unconditionally derivable.
	mpSplit bool
}

// entropyCost scores a candidate by what its bytes cost under an order-0
// model of the sub-stream they would join: sum -log2 p(byte). It is the
// cheapest score that is a compressed size rather than a raw one, and with
// byMode routing it is the right one, because each transform's bytes then
// have their own model to be scored against.
type byteModel struct {
	tab [ntr][256]float64
	// seen counts how often each exact region payload was emitted under a
	// transform on the previous pass. An order-0 model cannot see that the
	// same relocation delta recurs across thousands of call sites, which is
	// the whole merit of sub -- so a payload the terminal stage will code as
	// a match is priced as one instead of as its bytes.
	seen  [ntr]map[string]int
	match float64 // bits a repeat costs; 0 disables the repeat term
}

func (m *byteModel) train(hist *[ntr][256]int) {
	for tr := range hist {
		total := 0.0
		for _, c := range hist[tr] {
			total += float64(c)
		}
		for b, c := range hist[tr] {
			// Laplace, so an unseen byte is expensive but not infinite.
			p := (float64(c) + 0.5) / (total + 128)
			m.tab[tr][b] = -math.Log2(p)
		}
	}
}

func (m *byteModel) cost(tr int, b []byte) float64 {
	if m.match > 0 && m.seen[tr] != nil && m.seen[tr][string(b)] > 1 {
		return m.match
	}
	c := 0.0
	for _, v := range b {
		c += m.tab[tr][v]
	}
	return c
}

// uniform is the first pass: every byte costs 8 bits, so the selector starts
// as the length one and the models bootstrap from what it picks.
func uniformCost(tr int, b []byte) float64 { return 8 * float64(len(b)) }

// The mode column names the transform and, for multiprecision, its width.
// Width is not a free parameter: mp4 and mp8 differ by 44 % on libxul's
// .data.rel.ro, so a per-region selector that cannot say which one it meant
// is measuring the wrong thing.
const (
	spikeMP4 = ntr
	spikeMP8 = ntr + 1
	spikeLZ  = ntr + 2
)

func modeCode(tr, word int) byte {
	if tr == trMP {
		if word == 8 {
			return spikeMP8
		}
		return spikeMP4
	}
	return byte(tr)
}

func modeDecode(m byte) (tr, word int) {
	switch m {
	case spikeMP4:
		return trMP, 4
	case spikeMP8:
		return trMP, 8
	}
	return int(m), 1
}

// words are the candidate widths a per-region selector tries.
func (sh spikeShape) words() []int {
	if sh.word >= 8 {
		return []int{1, 4, 8}
	}
	return []int{1, sh.word}
}

const (
	trLit = iota
	trXor
	trSub
	trMP // multiprecision little-endian balanced digits, high zeros trimmed
	ntr
)

var trName = [ntr]string{"lit", "xor", "sub", "mp"}

const spikeBuckets = 5

// nLitStreams is the literal sub-stream count: the five length buckets (or
// four transforms under byMode), plus one for the multiprecision
// significance column when mpSplit peels it off.
const nLitStreams = ntr + 3
const sigStream = nLitStreams - 1

func spikeBucket(n int, on bool) int {
	if !on {
		return 0
	}
	return min(n, spikeBuckets) - 1
}

// spikeMPDigits writes want-pred over each word as balanced base-256 digits, high
// zeros dropped. A rel32 whose target moved by -1 is one digit, not four
// literals.
func spikeMPDigits(pred, want []byte, word int, dst []byte) []byte {
	for i := 0; i < len(want); i += word {
		w := min(word, len(want)-i)
		var p, t uint64
		for j := w - 1; j >= 0; j-- {
			p = p<<8 | uint64(pred[i+j])
			t = t<<8 | uint64(want[i+j])
		}
		d := int64(t - p)
		if w < 8 {
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

// mpDigitsSplit is spikeMPDigits with the significance column peeled off.
func mpDigitsSplit(pred, want []byte, word int, sig, val []byte) ([]byte, []byte) {
	for i := 0; i < len(want); i += word {
		w := min(word, len(want)-i)
		var p, t uint64
		for j := w - 1; j >= 0; j-- {
			p = p<<8 | uint64(pred[i+j])
			t = t<<8 | uint64(want[i+j])
		}
		d := int64(t - p)
		if w < 8 {
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
		sig = append(sig, byte(nsig))
		val = append(val, dig[:nsig]...)
	}
	return sig, val
}

func mpApplySplit(buf []byte, word int, sig, val []byte) ([]byte, []byte, bool) {
	for i := 0; i < len(buf); i += word {
		w := min(word, len(buf)-i)
		if len(sig) == 0 {
			return nil, nil, false
		}
		nsig := int(sig[0])
		sig = sig[1:]
		if nsig > w || nsig > len(val) {
			return nil, nil, false
		}
		var d int64
		for j := nsig - 1; j >= 0; j-- {
			d += int64(int8(val[j])) << (8 * j)
		}
		val = val[nsig:]
		var p uint64
		for j := w - 1; j >= 0; j-- {
			p = p<<8 | uint64(buf[i+j])
		}
		t := uint64(int64(p) + d)
		for j := 0; j < w; j++ {
			buf[i+j] = byte(t >> (8 * j))
		}
	}
	return sig, val, true
}

func spikeMPApply(buf []byte, word int, src []byte) ([]byte, bool) {
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
		// Digits are balanced: each is a signed byte weighted by 256^j.
		// Reconstructing them as an unsigned little-endian integer is wrong
		// whenever a digit is negative and a higher one is not.
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

func emitLiterals(tr int, pred, want []byte, word int, dst []byte) []byte {
	switch tr {
	case trXor:
		for i := range want {
			dst = append(dst, pred[i]^want[i])
		}
	case trSub:
		for i := range want {
			dst = append(dst, want[i]-pred[i])
		}
	case trMP:
		dst = spikeMPDigits(pred, want, word, dst)
	default:
		dst = append(dst, want...)
	}
	return dst
}

type spikeOut struct {
	gaps, spans, ops, modes []byte
	lit                     [nLitStreams][]byte
	nregions                int
	nlz, nlitreg            int
	trPicks                 [ntr]int
	mpWidth                 [3]int // [_, mp4, mp8]
	hist                    [ntr][256]int
	payload                 [ntr]map[string]int
}

func (o *spikeOut) streams() [][]byte {
	out := [][]byte{o.gaps, o.spans, o.ops, o.modes}
	for i := range o.lit {
		out = append(out, o.lit[i])
	}
	return out
}

// spikeWrite is corrShape.write with the extra transforms. Region finding is
// the shipped rule, plus optional widening to a word boundary so that a
// multiprecision digit covers a whole field.
func (sh spikeShape) write(pred, want []byte) *spikeOut {
	o := &spikeOut{}
	gapDst, spanDst, opDst := &o.gaps, &o.gaps, &o.gaps
	if sh.cols {
		spanDst, opDst = &o.spans, &o.ops
	}
	prevEnd := 0
	for s := 0; s < len(want); {
		if pred[s] == want[s] {
			s++
			continue
		}
		e := s + 1
		for e < len(want) {
			k := e
			for k < len(want) && k-e < sh.merge && pred[k] == want[k] {
				k++
			}
			if k-e < sh.merge && k < len(want) {
				e = k + 1
				continue
			}
			break
		}
		if sh.word > 1 {
			s = s &^ (sh.word - 1)
			e = min((e+sh.word-1)&^(sh.word-1), len(want))
			if s < prevEnd {
				s = prevEnd
			}
		}
		n := e - s
		putU(gapDst, uint64(s-prevEnd))

		// candidate 1: an lz stream over the prediction's own bytes here
		opMark, litMark := len(*opDst), len(o.lit[0])
		c2, l2 := lz.Emit(pred[s:e+slackAt(e, len(pred))], want[s:e], *opDst, o.lit[0])
		lzCost := len(c2) - opMark + len(l2) - litMark

		// candidate 2: literals under one transform, or the best transform
		bestTr, bestBuf := sh.tr, []byte(nil)
		word := sh.word
		if sh.byIdx != nil {
			if o.nregions >= len(sh.byIdx) {
				panic("byIdx shorter than the region sequence")
			}
			bestTr, word = modeDecode(sh.byIdx[o.nregions])
			bestBuf = emitLiterals(bestTr, pred[s:e], want[s:e], word, nil)
		} else if sh.byPos != nil {
			bestTr, word = sh.byPos(s)
			bestBuf = emitLiterals(bestTr, pred[s:e], want[s:e], word, nil)
		} else if sh.perReg {
			score := sh.cost
			if score == nil {
				score = uniformCost
			}
			bestCost := math.Inf(1)
			for tr := 0; tr < ntr; tr++ {
				for _, w := range sh.words() {
					if (tr == trMP) != (w > 1) {
						continue
					}
					b := emitLiterals(tr, pred[s:e], want[s:e], w, nil)
					if c := score(tr, b); c < bestCost {
						bestTr, bestCost, bestBuf, word = tr, c, b, w
					}
				}
			}
		} else {
			bestBuf = emitLiterals(sh.tr, pred[s:e], want[s:e], sh.word, nil)
		}
		perRegionMode := sh.perReg || sh.byPos != nil || sh.byIdx != nil

		if lzCost < len(bestBuf) {
			*opDst, o.lit[0] = c2, l2
			putU(spanDst, uint64(n)<<1|lzRegion)
			o.nlz++
			if perRegionMode {
				o.modes = append(o.modes, spikeLZ)
			}
		} else {
			*opDst, o.lit[0] = (*opDst)[:opMark], o.lit[0][:litMark]
			putU(spanDst, uint64(n)<<1)
			b := spikeBucket(n, sh.bucket)
			if sh.byModeW {
				b = int(modeCode(bestTr, word))
			} else if sh.byMode {
				b = bestTr
			}
			if bestTr == trMP && sh.mpSplit {
				o.lit[sigStream], o.lit[b] = mpDigitsSplit(pred[s:e], want[s:e], word, o.lit[sigStream], o.lit[b])
			} else {
				if bestTr == trMP && !sh.noMPLen {
					putU(opDst, uint64(len(bestBuf))) // digits are not one per byte
				}
				o.lit[b] = append(o.lit[b], bestBuf...)
			}
			o.nlitreg++
			for _, v := range bestBuf {
				o.hist[bestTr][v]++
			}
			if o.payload[bestTr] == nil {
				o.payload[bestTr] = map[string]int{}
			}
			o.payload[bestTr][string(bestBuf)]++
			o.trPicks[bestTr]++
			if bestTr == trMP {
				o.mpWidth[word/4]++
			}
			if perRegionMode {
				o.modes = append(o.modes, modeCode(bestTr, word))
			}
		}
		o.nregions++
		prevEnd, s = e, e
	}
	return o
}

// apply replays spikeWrite, so every number below is a number that decodes.
func (sh spikeShape) apply(buf []byte, o *spikeOut) error {
	c := &rbuf{b: o.gaps}
	sr, or := c, c
	if sh.cols {
		sr, or = &rbuf{b: o.spans}, &rbuf{b: o.ops}
	}
	lit := o.lit
	rd := &lz.Reader{Lit: lit[0]}
	modes := o.modes
	var scratch []byte
	pos := 0
	for i := 0; i < o.nregions; i++ {
		gap := c.un(uint64(len(buf)), "gap")
		v := sr.un(uint64(len(buf))<<1|1, "span")
		if c.err != nil {
			return c.err
		}
		if sr.err != nil {
			return sr.err
		}
		span, isLZ := int(v>>1), v&lzRegion != 0
		pos += int(gap)
		tr, word := sh.tr, sh.word
		if sh.perReg || sh.byPos != nil || sh.byIdx != nil {
			if len(modes) == 0 {
				return fmt.Errorf("mode column exhausted at region %d", i)
			}
			isLZ = modes[0] == spikeLZ
			tr, word = modeDecode(modes[0])
			modes = modes[1:]
		}
		if sh.byPos != nil {
			_, word = sh.byPos(pos)
		}
		if isLZ {
			slack := slackAt(pos+span, len(buf))
			src := append(scratch[:0], buf[pos:pos+span+slack]...)
			scratch = src
			rd.Ctrl = or.b
			rd.Lit = lit[0]
			if err := rd.Apply(src, buf[pos:pos+span]); err != nil {
				return fmt.Errorf("region %d: %v", i, err)
			}
			or.b, lit[0] = rd.Ctrl, rd.Lit
			pos += span
			continue
		}
		b := spikeBucket(span, sh.bucket)
		if sh.byModeW {
			b = int(modeCode(tr, word))
		} else if sh.byMode {
			b = tr
		}
		switch tr {
		case trMP:
			if sh.mpSplit {
				sig, val, ok := mpApplySplit(buf[pos:pos+span], word, lit[sigStream], lit[b])
				if !ok {
					return fmt.Errorf("region %d: mp replay failed", i)
				}
				lit[sigStream], lit[b] = sig, val
				break
			}
			n := 0
			if sh.noMPLen {
				// derivable: one significance count per word, then its digits
				for k := 0; k < span; k += word {
					if n >= len(lit[b]) {
						return fmt.Errorf("region %d: mp stream short", i)
					}
					n += 1 + int(lit[b][n])
				}
			} else {
				n = int(or.un(uint64(len(buf)), "mp length"))
			}
			if n > len(lit[b]) {
				return fmt.Errorf("region %d: mp stream short", i)
			}
			rest, ok := spikeMPApply(buf[pos:pos+span], word, lit[b][:n])
			if !ok || len(rest) != 0 {
				return fmt.Errorf("region %d: mp replay failed", i)
			}
			lit[b] = lit[b][n:]
		case trXor:
			for j := 0; j < span; j++ {
				buf[pos+j] ^= lit[b][j]
			}
			lit[b] = lit[b][span:]
		case trSub:
			for j := 0; j < span; j++ {
				buf[pos+j] += lit[b][j]
			}
			lit[b] = lit[b][span:]
		default:
			copy(buf[pos:pos+span], lit[b][:span])
			lit[b] = lit[b][span:]
		}
		pos += span
	}
	return nil
}

func TestBS6Spike(t *testing.T) {
	predPath, targetPath := os.Getenv("BS6_PRED"), os.Getenv("BS6_TARGET")
	if predPath == "" || targetPath == "" {
		t.Skip("set BS6_PRED and BS6_TARGET")
	}
	pred, err := os.ReadFile(predPath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(pred) != len(want) {
		t.Fatalf("prediction %d bytes, target %d", len(pred), len(want))
	}
	wrong := 0
	for i := range want {
		if pred[i] != want[i] {
			wrong++
		}
	}
	plain, near, err := CorrectionShapes(pred, want)
	if err != nil {
		t.Fatal(err)
	}
	base := min(czLen(plain), czLen(near))
	t.Logf("%s -> %s: %d bytes, %d wrong (%.3f%%)", predPath, targetPath, len(want), wrong,
		100*float64(wrong)/float64(len(want)))
	t.Logf("shipped: plain %d cz, near-miss %d cz, adaptive picks %d", czLen(plain), czLen(near), base)

	merges := []int{6, 16, 32, 48}
	if v := os.Getenv("BS6_MERGES"); v != "" {
		merges = nil
		for _, f := range strings.Split(v, ",") {
			n, _ := strconv.Atoi(f)
			merges = append(merges, n)
		}
	}
	type row struct {
		name       string
		sh         spikeShape
		total, raw int
		parts      []int
		o          *spikeOut
		err        error
		tuned      int     // passes of cost-model refinement before the real write
		viterbi    bool    // pick modes by Viterbi over the region sequence
		lambda     float64 // bits charged for a mode change
		match      float64 // bits a repeated region payload costs; 0 = off
	}
	var rows []row
	for _, m := range merges {
		for _, tr := range []int{trLit, trXor, trSub, trMP} {
			for _, word := range []int{1, 4, 8} {
				if tr == trMP && word == 1 {
					continue
				}
				if tr != trMP && word != 1 {
					continue
				}
				rows = append(rows, row{
					name: fmt.Sprintf("m%-2d %-3s%d cols", m, trName[tr], word),
					sh:   spikeShape{merge: m, tr: tr, word: word, cols: true},
				})
				if tr == trMP {
					// the fixed shapes pay the same redundant varint
					rows = append(rows, row{
						name: fmt.Sprintf("m%-2d %-3s%d noMPLen", m, trName[tr], word),
						sh:   spikeShape{merge: m, tr: tr, word: word, cols: true, noMPLen: true},
					})
					rows = append(rows, row{
						name: fmt.Sprintf("m%-2d %-3s%d mpSplit", m, trName[tr], word),
						sh:   spikeShape{merge: m, tr: tr, word: word, cols: true, mpSplit: true},
					})
				}
			}
		}
		rows = append(rows, row{
			name: fmt.Sprintf("m%-2d per-region", m),
			sh:   spikeShape{merge: m, word: 4, cols: true, perReg: true},
		})
		rows = append(rows, row{
			name: fmt.Sprintf("m%-2d per-region+buckets", m),
			sh:   spikeShape{merge: m, word: 4, cols: true, perReg: true, bucket: true},
		})
	}
	// A per-region selector is only as good as what it scores candidates
	// by, so the tuned rows re-run until the models stop moving.
	for _, byMode := range []bool{false, true} {
		base := spikeShape{merge: 32, word: 8, cols: true, perReg: true, byMode: byMode, bucket: !byMode}
		name := "m32 per-region tuned"
		if byMode {
			name += "+byMode"
		}
		rows = append(rows, row{name: name, sh: base, tuned: 3})
	}
	// Stickiness sweep: the same models, the same regions, a penalty for
	// changing mode. lambda 0 is the greedy per-region selector; large lambda
	// collapses to one mode for the whole file.
	// Two encodings of the multiprecision stream itself, on the shape that
	// wins above. noMPLen drops a redundant varint; mpSplit is Percival's
	// map/value split where the map is a significance column.
	for _, v := range []struct {
		name             string
		noMPLen, mpSplit bool
	}{{"noMPLen", true, false}, {"mpSplit", false, true}, {"mpSplit+noMPLen", true, true}} {
		rows = append(rows, row{
			name: "m32 per-region tuned+byMode " + v.name,
			sh: spikeShape{merge: 32, word: 8, cols: true, perReg: true, byMode: true,
				noMPLen: v.noMPLen, mpSplit: v.mpSplit},
			tuned: 3,
		})
	}
	for _, lam := range []float64{0, 8, 32, 128, 512, 2048} {
		rows = append(rows, row{
			name:    fmt.Sprintf("m32 viterbi L%-4.0f+byMode", lam),
			sh:      spikeShape{merge: 32, word: 8, cols: true, byMode: true},
			tuned:   3,
			viterbi: true,
			lambda:  lam,
		})
	}
	// Does the selector undervalue sub because an order-0 model cannot see
	// that the same delta recurs? Price a repeated payload as a match and
	// sweep what a match costs.
	for _, mb := range []float64{8, 16, 32} {
		rows = append(rows, row{
			name:  fmt.Sprintf("m32 byModeW noMPLen match%-2.0f", mb),
			sh:    spikeShape{merge: 32, word: 8, cols: true, perReg: true, byModeW: true, noMPLen: true},
			tuned: 3,
			match: mb,
		})
	}
	// The shapes that matter, with the redundant varint gone and each mode
	// given its own stream.
	for _, lam := range []float64{0, 8, 32} {
		rows = append(rows, row{
			name:    fmt.Sprintf("m32 vit L%-3.0f byModeW noMPLen", lam),
			sh:      spikeShape{merge: 32, word: 8, cols: true, byModeW: true, noMPLen: true},
			tuned:   3,
			viterbi: true,
			lambda:  lam,
		})
		rows = append(rows, row{
			name:    fmt.Sprintf("m32 vit L%-3.0f byModeW mpSplit", lam),
			sh:      spikeShape{merge: 32, word: 8, cols: true, byModeW: true, mpSplit: true},
			tuned:   3,
			viterbi: true,
			lambda:  lam,
		})
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := &rows[i]
			// The models are trained by a greedy per-region pass; the row's
			// own shape then uses them, either greedily or through Viterbi.
			train := r.sh
			train.perReg, train.byIdx = true, nil
			m := &byteModel{}
			for p := 0; p < r.tuned; p++ {
				prev := train.write(pred, want)
				m = &byteModel{match: r.match}
				m.train(&prev.hist)
				m.seen = prev.payload
				train.cost = m.cost
			}
			r.sh.cost = train.cost
			if r.viterbi {
				rs := spikeRegions(pred, want, r.sh.merge, r.sh.word)
				r.sh.byIdx = viterbiModes(pred, want, rs, m, r.lambda)
				r.sh.perReg = false
			}
			o := r.sh.write(pred, want)
			// every reported number must decode
			buf := append([]byte(nil), pred...)
			check := &spikeOut{nregions: o.nregions, gaps: o.gaps, spans: o.spans,
				ops: o.ops, modes: o.modes, lit: o.lit}
			if err := r.sh.apply(buf, check); err != nil {
				r.err = err
				return
			}
			for j := range buf {
				if buf[j] != want[j] {
					r.err = fmt.Errorf("replay differs at %d", j)
					return
				}
			}
			for _, b := range o.streams() {
				r.raw += len(b)
				n := czLen(b)
				r.parts = append(r.parts, n)
				r.total += n
			}
			r.o = o
		}()
	}
	wg.Wait()
	t.Logf("%-24s %10s %10s %9s  %s", "shape", "raw", "cz", "vs shipped", "regions lz/lit picks")
	for _, r := range rows {
		if r.err != nil {
			t.Errorf("%-24s FAILED: %v", r.name, r.err)
			continue
		}
		picks := ""
		if r.sh.perReg || r.sh.byIdx != nil {
			for tr := 0; tr < ntr; tr++ {
				if r.o.trPicks[tr] > 0 {
					picks += fmt.Sprintf(" %s=%d", trName[tr], r.o.trPicks[tr])
				}
			}
			if r.o.mpWidth[1]+r.o.mpWidth[2] > 0 {
				picks += fmt.Sprintf(" (mp4=%d mp8=%d)", r.o.mpWidth[1], r.o.mpWidth[2])
			}
		}
		t.Logf("%-24s %10d %10d %8.2f%%  %d/%d%s", r.name, r.raw, r.total,
			100*float64(r.total-base)/float64(base), r.o.nlz, r.o.nlitreg, picks)
	}
	// The terminal stage, on the streams of the best shape. plain.go's note
	// that a mostly-zero stream "bzip2's block sort handles beautifully and
	// every LZ compressor handles badly" is a claim about the difference
	// string; a correction whose literals are want-pred is the same kind of
	// stream, so it is priced the same way.
	best := -1
	for i := range rows {
		if rows[i].err == nil && (best < 0 || rows[i].total < rows[best].total) {
			best = i
		}
	}
	if best >= 0 {
		o := rows[best].sh.write(pred, want)
		hdr := fmt.Sprintf("%-24s", "terminal stage")
		for _, c := range compressors {
			hdr += fmt.Sprintf(" %10s", c.name)
		}
		t.Log(hdr)
		for _, nm := range []struct {
			name string
			b    []byte
		}{{"shipped plain", plain}, {"shipped near-miss", near}} {
			line := fmt.Sprintf("%-24s", nm.name)
			for _, c := range compressors {
				line += fmt.Sprintf(" %10d", c.size(nm.b))
			}
			t.Log(line)
		}
		line := fmt.Sprintf("%-24s", rows[best].name)
		for _, c := range compressors {
			n := 0
			for _, b := range o.streams() {
				n += c.size(b)
			}
			line += fmt.Sprintf(" %10d", n)
		}
		t.Log(line)
		// The codec question needs the streams themselves, so a pure-Go
		// LZMA can be priced on exactly what the container would ship.
		if dir := os.Getenv("BS6_DUMPDIR"); dir != "" {
			names := []string{"gaps", "spans", "ops", "modes", "lit0", "lit1", "lit2", "lit3", "lit4", "lit5", "lit6"}
			for i, b := range o.streams() {
				if len(b) == 0 {
					continue
				}
				if err := os.WriteFile(fmt.Sprintf("%s/best-%s", dir, names[i]), b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(dir+"/shipped-plain", plain, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("dumped %s streams to %s", rows[best].name, dir)
		}
	}
	// What the selector costs to run, against what the shipped adaptive
	// encoder already spends: two full writes plus two compressions.
	{
		sh := spikeShape{merge: 32, word: 8, cols: true, perReg: true, byModeW: true, noMPLen: true}
		t0 := time.Now()
		var m *byteModel
		for p := 0; p < 3; p++ {
			prev := sh.write(pred, want)
			m = &byteModel{}
			m.train(&prev.hist)
			sh.cost = m.cost
		}
		train := time.Since(t0)
		t0 = time.Now()
		o := sh.write(pred, want)
		write := time.Since(t0)
		t0 = time.Now()
		n := 0
		for _, b := range o.streams() {
			n += czLen(b)
		}
		comp := time.Since(t0)
		buf := append([]byte(nil), pred...)
		t0 = time.Now()
		if err := sh.apply(buf, &spikeOut{nregions: o.nregions, gaps: o.gaps, spans: o.spans,
			ops: o.ops, modes: o.modes, lit: o.lit}); err != nil {
			t.Fatal(err)
		}
		dec := time.Since(t0)
		t0 = time.Now()
		if _, _, err := CorrectionShapes(pred, want); err != nil {
			t.Fatal(err)
		}
		ship := time.Since(t0)
		t.Logf("cost: 3 training passes %v, final write %v, compress %v, decode %v"+
			"  (shipped writes both shapes in %v, then compresses both)", train, write, comp, dec, ship)
	}
	if os.Getenv("BS6_SECTIONS") != "" {
		perSection(t, pred, want)
	}
}

// perSection attributes the transform choice to the sections of the target,
// so a whole-file win is not one section's fluke. Each section is coded on
// its own, which is also how a multi-region patch would code it.
func perSection(t *testing.T, pred, want []byte) {
	f, err := elf.NewFile(bytes.NewReader(want))
	if err != nil {
		t.Logf("per-section: %v", err)
		return
	}
	type sec struct {
		name     string
		lo, hi   int
		wrong    int
		lit, xor int
		sub      int
		mp4, mp8 int
	}
	var secs []sec
	for _, s := range f.Sections {
		if s.Type == elf.SHT_NOBITS || s.Size < 1<<16 || s.Offset+s.Size > uint64(len(want)) {
			continue
		}
		secs = append(secs, sec{name: s.Name, lo: int(s.Offset), hi: int(s.Offset + s.Size)})
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range secs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sc := &secs[i]
			p, w := pred[sc.lo:sc.hi], want[sc.lo:sc.hi]
			for j := range w {
				if p[j] != w[j] {
					sc.wrong++
				}
			}
			if sc.wrong == 0 {
				return
			}
			size := func(tr, word int) int {
				o := spikeShape{merge: 32, tr: tr, word: word, cols: true, noMPLen: true}.write(p, w)
				n := 0
				for _, b := range o.streams() {
					n += czLen(b)
				}
				return n
			}
			sc.lit, sc.xor, sc.sub = size(trLit, 1), size(trXor, 1), size(trSub, 1)
			sc.mp4, sc.mp8 = size(trMP, 4), size(trMP, 8)
		}()
	}
	wg.Wait()
	t.Logf("%-20s %10s %8s %9s %9s %9s %9s %9s", "section", "wrong", "of", "lit", "xor", "sub", "mp4", "mp8")
	sum := 0
	for _, sc := range secs {
		if sc.wrong == 0 {
			continue
		}
		sum += min(min(sc.lit, sc.xor), min(sc.sub, min(sc.mp4, sc.mp8)))
		t.Logf("%-20s %10d %7.2f%% %9d %9d %9d %9d %9d", sc.name, sc.wrong,
			100*float64(sc.wrong)/float64(sc.hi-sc.lo), sc.lit, sc.xor, sc.sub, sc.mp4, sc.mp8)
	}
	t.Logf("%-20s %10s %8s %9s", "sum of per-section bests", "", "", fmt.Sprint(sum))

	// The same choice, but every region writing into one set of streams, so
	// the mode is per region and the compressor's context is not cut at a
	// section boundary. The mode is an oracle -- picked on this pair -- which
	// is what a sampled proxy (SPEC 6.4 tier 2) would approximate.
	type pick struct{ tr, word int }
	picks := make([]pick, 0, len(secs))
	for _, sc := range secs {
		p := pick{trLit, 1}
		best := sc.lit
		for _, c := range []struct {
			n, tr, w int
		}{{sc.xor, trXor, 1}, {sc.sub, trSub, 1}, {sc.mp4, trMP, 4}, {sc.mp8, trMP, 8}} {
			if sc.wrong > 0 && c.n < best {
				best, p = c.n, pick{c.tr, c.w}
			}
		}
		picks = append(picks, p)
	}
	byPos := func(off int) (int, int) {
		for i, sc := range secs {
			if off >= sc.lo && off < sc.hi {
				return picks[i].tr, picks[i].word
			}
		}
		return trSub, 1
	}
	for _, v := range []struct {
		name string
		sh   spikeShape
	}{
		{"per-section modes, one literal stream", spikeShape{merge: 32, word: 1, cols: true, byPos: byPos}},
		{"per-section modes, a stream per mode", spikeShape{merge: 32, word: 1, cols: true, byPos: byPos, byMode: true}},
	} {
		o := v.sh.write(pred, want)
		chk := append([]byte(nil), pred...)
		if err := v.sh.apply(chk, &spikeOut{nregions: o.nregions, gaps: o.gaps, spans: o.spans,
			ops: o.ops, modes: o.modes, lit: o.lit}); err != nil {
			t.Errorf("%s: %v", v.name, err)
			continue
		}
		if !bytes.Equal(chk, want) {
			t.Errorf("%s: replay differs", v.name)
			continue
		}
		n := 0
		for _, b := range o.streams() {
			n += czLen(b)
		}
		t.Logf("%-40s %d cz (%d regions, %d lz)", v.name, n, o.nregions, o.nlz)
	}
}

// --- selection granularity -------------------------------------------------
//
// Per-section mode choice wins on libxul and per-region choice loses, which
// leaves an obvious question: is the difference *sections*, or is it just that
// a section is a long sticky run of one mode? A per-region selector that pays
// a penalty to change mode answers it, and unlike the section table it needs
// no knowledge of the container format -- it would work on Mach-O, PE or a
// firmware image.
//
// This is Percival's dynamic program pointed at the encoder rather than the
// matcher: states are transforms, the emission cost is the trained order-0
// cost of that region's bytes under that transform, and the transition cost
// is the switch penalty. Regions are few enough (292,080 on libxul) that the
// full Viterbi is exact and instant, with no beam to prune.

// spikeRegions enumerates the regions a shape would write. Region boundaries
// depend only on merge and word, never on the transform, so a mode chosen per
// ordinal here lines up with what write produces.
func spikeRegions(pred, want []byte, merge, word int) [][2]int {
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
			for k < len(want) && k-e < merge && pred[k] == want[k] {
				k++
			}
			if k-e < merge && k < len(want) {
				e = k + 1
				continue
			}
			break
		}
		if word > 1 {
			s = s &^ (word - 1)
			e = min((e+word-1)&^(word-1), len(want))
			if s < prevEnd {
				s = prevEnd
			}
		}
		out = append(out, [2]int{s, e})
		prevEnd, s = e, e
	}
	return out
}

// modeCands are the (transform, width) pairs a region may choose between.
var modeCands = []struct{ tr, word int }{
	{trLit, 1}, {trXor, 1}, {trSub, 1}, {trMP, 4}, {trMP, 8},
}

// viterbiModes picks a mode per region minimising emission cost plus lambda
// bits per mode change.
func viterbiModes(pred, want []byte, rs [][2]int, m *byteModel, lambda float64) []byte {
	const nc = 5
	cost := make([]float64, nc)
	next := make([]float64, nc)
	back := make([][nc]uint8, len(rs))
	for i := range cost {
		cost[i] = 0
	}
	for i, r := range rs {
		for c, cd := range modeCands {
			b := emitLiterals(cd.tr, pred[r[0]:r[1]], want[r[0]:r[1]], cd.word, nil)
			em := m.cost(cd.tr, b)
			best, bi := math.Inf(1), 0
			for p := 0; p < nc; p++ {
				v := cost[p]
				if p != c {
					v += lambda
				}
				if v < best {
					best, bi = v, p
				}
			}
			next[c], back[i][c] = best+em, uint8(bi)
		}
		copy(cost, next)
	}
	bi := 0
	for c := 1; c < nc; c++ {
		if cost[c] < cost[bi] {
			bi = c
		}
	}
	out := make([]byte, len(rs))
	for i := len(rs) - 1; i >= 0; i-- {
		out[i] = modeCode(modeCands[bi].tr, modeCands[bi].word)
		bi = int(back[i][bi])
	}
	return out
}

// TestBS6Price prices dumped streams under the container's own compressor,
// so the per-stream codec question -- would a selection tag over more
// candidates pay? -- can be asked against cz's actual output rather than
// against a CLI stand-in.
//
//	BS6_PRICE=dir go test ./delta -run BS6Price -v
func TestBS6Price(t *testing.T) {
	dir := os.Getenv("BS6_PRICE")
	if dir == "" {
		t.Skip("set BS6_PRICE")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Regions are emitted in file order, so every sub-stream is in file
	// order too, and cutting one at a fixed size approximates cutting it at
	// a section boundary -- which is the only thing the per-section bound
	// still has over a per-region selector. This needs no section table and
	// no format knowledge; the decoder derives the cuts from the sizes it
	// already has.
	chunks := []int{0, 4 << 20, 2 << 20, 1 << 20, 512 << 10, 256 << 10, 128 << 10, 64 << 10}
	tot := make([]int, len(chunks))
	for _, e := range ents {
		b, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		line := fmt.Sprintf("%-14s raw %9d ", e.Name(), len(b))
		for i, c := range chunks {
			n := 0
			if c == 0 {
				n = czLen(b)
			} else {
				for off := 0; off < len(b) || off == 0; off += c {
					n += czLen(b[off:min(off+c, len(b))])
				}
			}
			tot[i] += n
			line += fmt.Sprintf(" %9d", n)
		}
		t.Log(line)
	}
	hdr := fmt.Sprintf("%-14s %13s ", "stream", "")
	for _, c := range chunks {
		if c == 0 {
			hdr += fmt.Sprintf(" %9s", "whole")
		} else {
			hdr += fmt.Sprintf(" %8dK", c>>10)
		}
	}
	t.Log(hdr)
	line := fmt.Sprintf("%-14s %13s ", "TOTAL", "")
	for _, n := range tot {
		line += fmt.Sprintf(" %9d", n)
	}
	t.Log(line)
}
