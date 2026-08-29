package delta

import (
	"encoding/binary"
	"fmt"

	"github.com/wjordan/go-binsync/delta/internal/lz"
	"github.com/wjordan/go-binsync/internal/cz"
)

// The correction turns a prediction into the real file (docs/DESIGN.md 3.4).
//
// Where the prediction is right -- most of the file -- it costs nothing: the
// stream names a gap and moves on. Where it is wrong, the differing bytes
// form a region, and inside a region the new bytes are re-expressed as
// copies from the prediction's own bytes there plus literals, so a function
// that grew by five bytes costs five literals and a copy instead of its
// whole shifted tail.
//
// Two invariants make the decoder cheap and safe. A region occupies the same
// span in the prediction and in the output, because the prediction is
// length-exact by construction. And a region's source window only ever
// extends *forward* from the region's start, into bytes no earlier region
// has touched -- so the decoder snapshots that window, writes the region in
// place, and never holds prediction and output at the same time.

const (
	// mergeGap is how many identical bytes are cheaper to re-send than to
	// pay for a second region header.
	mergeGap = 6
	// srcSlack is how far past a region its source window reaches, so that
	// content the prediction placed slightly early can still be copied.
	srcSlack = 256
)

// A region is written one of two ways, and the encoder picks whichever is
// smaller: as an lz stream over the prediction's own bytes there, which is
// what makes a function that grew cost its inserted bytes rather than its
// whole shifted tail, or as plain literals, which is cheaper for the many
// two- and four-byte regions a relocation mistake leaves behind. The choice
// is the low bit of the span. The source window's slack is not transmitted:
// it follows from the region's end and the file's length.
const lzRegion = 1

// The whole correction is in turn written one of two ways, and which one
// wins is a property of the release rather than of the codec (docs/DESIGN.md
// 3.4, research 14.5). A patch release's residual is near-misses a few bytes
// apart: merging runs up to 32 correct bytes apart costs almost nothing --
// the swallowed bytes are correct, so under xor they are zeros -- and saves
// a region header each. A minor release's residual is new content, where the
// same two transforms cost 3%, because there the literal stream's value is
// that it matches *itself*, and xor against an unrelated prediction destroys
// those matches.
var (
	shipped  = corrShape{merge: mergeGap}
	nearmiss = corrShape{merge: 32, xor: true, cols: true, bit: 1}
)

// corrShape is one way of laying the same regions out.
type corrShape struct {
	merge int    // identical bytes a region absorbs rather than pay a header
	xor   bool   // a literal region carries want^pred, not want
	cols  bool   // gaps, spans, lz ops and literals as four streams
	bit   uint64 // the header flag that names this shape
}

func slackAt(end, n int) int { return min(srcSlack, n-end) }

func putU(dst *[]byte, v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	*dst = append(*dst, tmp[:binary.PutUvarint(tmp[:], v)]...)
}

// write lays the correction out in this shape. flagged says the stream is a
// transform-2 one, whose region count carries the shape in its low bit; a
// transform-1 stream has no flag because its decoder knows only one shape.
func (sh corrShape) write(pred, want []byte, flagged bool) []byte {
	// An interleaved shape writes gaps, spans and ops into one stream; a
	// columnar one gives each its own.
	var ctrl, spans, ops, lit []byte
	gapDst, spanDst, opDst := &ctrl, &ctrl, &ctrl
	if sh.cols {
		spanDst, opDst = &spans, &ops
	}
	nregions, prevEnd := 0, 0
	for s := 0; s < len(want); {
		if pred[s] == want[s] {
			s++
			continue
		}
		// grow the region while the identical stretch ahead is shorter than
		// a new region header would cost
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
		putU(gapDst, uint64(s-prevEnd))
		n := e - s
		mark := len(*spanDst)
		putU(spanDst, uint64(n)<<1|lzRegion)
		opMark, litMark := len(*opDst), len(lit)
		c2, l2 := lz.Emit(pred[s:e+slackAt(e, len(pred))], want[s:e], *opDst, lit)
		if len(*spanDst)-mark+len(c2)-opMark+len(l2)-litMark < n {
			*opDst, lit = c2, l2
		} else {
			*spanDst = (*spanDst)[:mark]
			putU(spanDst, uint64(n)<<1)
			if sh.xor {
				for i := s; i < e; i++ {
					lit = append(lit, pred[i]^want[i])
				}
			} else {
				lit = append(lit, want[s:e]...)
			}
		}
		nregions++
		prevEnd, s = e, e
	}

	w := &wbuf{}
	w.u(uint64(len(want)))
	nr := uint64(nregions)
	if flagged {
		nr = nr<<1 | sh.bit
	}
	w.u(nr)
	w.u(uint64(len(ctrl)))
	if sh.cols {
		w.u(uint64(len(spans)))
		w.u(uint64(len(ops)))
	}
	w.u(uint64(len(lit)))
	w.raw(ctrl)
	w.raw(spans) // empty unless columnar, where ctrl then holds only gaps
	w.raw(ops)
	w.raw(lit)
	return w.b
}

// encodeCorrection writes the stream that turns pred into want. The two must
// be the same length.
//
// adaptive is transform 2 and above: the correction is written both ways,
// both are compressed by the compressor that will ship them, and the smaller
// is kept. The second pass is the price -- one more compression of the
// largest stream in the patch -- and it is what makes a patch release pay
// its own encoding rather than a minor release's.
func encodeCorrection(pred, want []byte, adaptive bool) ([]byte, error) {
	if len(pred) != len(want) {
		return nil, fmt.Errorf("delta: prediction is %d bytes, target is %d", len(pred), len(want))
	}
	a := shipped.write(pred, want, adaptive)
	if !adaptive {
		return a, nil
	}
	b := nearmiss.write(pred, want, true)
	var nb int
	done := make(chan struct{})
	go func() { defer close(done); nb = czLen(b) }()
	na := czLen(a)
	<-done
	if nb < na {
		return b, nil
	}
	return a, nil
}

// czLen is what a stream will cost once the container compresses it: the
// same compressor at the same frame size, so the shape is chosen on the
// number that ships.
func czLen(b []byte) int {
	n := 0
	for off := 0; ; off += FrameSize {
		end := min(off+FrameSize, len(b))
		_, z := cz.Compress(b[off:end])
		n += len(z)
		if end == len(b) {
			return n
		}
	}
}

// EncodeCorrection writes the stream that turns pred into want, in the one
// shape every deployed decoder reads. It is exported so experimental
// predictors can use the production correction format while their prediction
// formats are still being evaluated.
func EncodeCorrection(pred, want []byte) ([]byte, error) {
	return encodeCorrection(pred, want, false)
}

// EncodeCorrectionAdaptive writes the correction in whichever of the two
// transform-2 shapes compresses smaller, for a codec built on top of this
// package; the stream is applied with ApplyFlaggedCorrection.
func EncodeCorrectionAdaptive(pred, want []byte) ([]byte, error) {
	return encodeCorrection(pred, want, true)
}

// CorrectionShapes writes the correction in both shapes a transform-2
// decoder reads, the shipped default and the near-miss form, so an
// experimental predictor can price them with its own compressor. Either is
// applied with ApplyFlaggedCorrection.
func CorrectionShapes(pred, want []byte) (plain, near []byte, err error) {
	if len(pred) != len(want) {
		return nil, nil, fmt.Errorf("delta: prediction is %d bytes, target is %d", len(pred), len(want))
	}
	return shipped.write(pred, want, true), nearmiss.write(pred, want, true), nil
}

// ApplyFlaggedCorrection is ApplyCorrection for a stream from
// CorrectionShapes, whose region count carries the shape.
func ApplyFlaggedCorrection(buf, stream []byte) error {
	return applyCorrection(buf, stream, true)
}

// applyCorrection rewrites buf, which holds the prediction, into the real
// file. It allocates only the scratch copy of one region's source window.
func applyCorrection(buf, stream []byte, flagged bool) error {
	r := &rbuf{b: stream}
	n := r.u()
	maxRegions := uint64(len(buf)) + 1
	if flagged {
		maxRegions = maxRegions<<1 | 1
	}
	v := r.un(maxRegions, "region count")
	nregions, alt := v, false
	if flagged {
		nregions, alt = v>>1, v&1 != 0
	}
	gapLen := r.un(uint64(len(stream)), "control stream length")
	var spanLen, opLen uint64
	if alt {
		spanLen = r.un(uint64(len(stream)), "span stream length")
		opLen = r.un(uint64(len(stream)), "op stream length")
	}
	litLen := r.un(uint64(len(stream)), "literal stream length")
	gaps, spans, ops, lit := r.take(gapLen), r.take(spanLen), r.take(opLen), r.take(litLen)
	if err := r.done(); err != nil {
		return err
	}
	if n != uint64(len(buf)) {
		return fmt.Errorf("%w: correction is for a %d-byte file, prediction is %d", errCorrupt, n, len(buf))
	}

	var scratch []byte
	pos := 0
	// The interleaved shape reads gaps, spans and ops from one cursor.
	c := &rbuf{b: gaps}
	sr, or := c, c
	if alt {
		sr, or = &rbuf{b: spans}, &rbuf{b: ops}
	}
	rd := &lz.Reader{Lit: lit}
	for i := uint64(0); i < nregions; i++ {
		gap := c.un(uint64(len(buf)), "region gap")
		v := sr.un(uint64(len(buf))<<1|1, "region span")
		if c.err != nil {
			return c.err
		}
		if sr.err != nil {
			return sr.err
		}
		span, isLZ := v>>1, v&lzRegion != 0
		if gap > uint64(len(buf)-pos) {
			return fmt.Errorf("%w: region %d starts past the end of the file", errCorrupt, i)
		}
		pos += int(gap)
		if span > uint64(len(buf)-pos) {
			return fmt.Errorf("%w: region %d spans past the end of the file", errCorrupt, i)
		}
		if !isLZ {
			if span > uint64(len(rd.Lit)) {
				return fmt.Errorf("%w: region %d wants %d literal bytes, %d remain", errCorrupt, i, span, len(rd.Lit))
			}
			if alt {
				for j := range int(span) {
					buf[pos+j] ^= rd.Lit[j]
				}
			} else {
				copy(buf[pos:], rd.Lit[:span])
			}
			rd.Lit = rd.Lit[span:]
			pos += int(span)
			continue
		}
		slack := slackAt(pos+int(span), len(buf))
		src := append(scratch[:0], buf[pos:pos+int(span)+slack]...)
		scratch = src
		rd.Ctrl = or.b
		if err := rd.Apply(src, buf[pos:pos+int(span)]); err != nil {
			return fmt.Errorf("%w: region %d: %v", errCorrupt, i, err)
		}
		or.b = rd.Ctrl
		pos += int(span)
	}
	for _, rest := range [][]byte{c.b, sr.b, or.b} {
		if len(rest) != 0 {
			return fmt.Errorf("%w: %d control bytes after the last region", errCorrupt, len(rest))
		}
	}
	if len(rd.Lit) != 0 {
		return fmt.Errorf("%w: %d literal bytes after the last region", errCorrupt, len(rd.Lit))
	}
	return nil
}

// ApplyCorrection rewrites buf, which holds a prediction, using stream.
func ApplyCorrection(buf, stream []byte) error {
	return applyCorrection(buf, stream, false)
}
