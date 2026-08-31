package presage

import (
	"fmt"
	"slices"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/internal/cz"
)

// A Cutter is an exact module that can say where the target's character
// changes, so the correction may be coded piecewise (SPEC G6: one residual
// coder, but its shape and its compressor context chosen per piece). The
// cuts are offsets into the target, encoder-side only; the decoder reads
// each piece's length from the stream.
type Cutter interface {
	Cuts(target []byte) []int64
}

// splitResidual codes the correction in pieces: each piece is its span's
// correction in whichever form is smaller once compressed on its own — the
// adaptive lz shapes, or the columnar form for a span of short scattered
// runs (delta/columnar.go) — so a section of recompiled code and one of
// relocation slots each get the coder and the compressor statistics that
// suit them. It is chosen only where it beats the single stream.
//
//	u(npieces) { u(len) byte(kind) u(nstreams) { u(rawLen) u(zLen) byte(codec) }* }* z...
func splitResidual(pred, target []byte, cuts []int64, disp *delta.DispContext) (stream []byte, modal bool, size int, err error) {
	bounds := append([]int64{0}, cuts...)
	bounds = append(bounds, int64(len(target)))
	w := &wbuf{}
	var zs [][]byte
	n := 0
	for i := 0; i+1 < len(bounds); i++ {
		a, b := bounds[i], bounds[i+1]
		if a < 0 || b <= a || b > int64(len(target)) {
			return nil, false, 0, fmt.Errorf("presage: cut %d..%d outside a %d-byte region", a, b, len(target))
		}
		corr, err := delta.EncodeCorrectionAdaptive(pred[a:b], target[a:b])
		if err != nil {
			return nil, false, 0, err
		}
		cols, err := delta.EncodeColumnarDisp(pred[a:b], target[a:b], disp.Restrict(int(a), int(b)))
		if err != nil {
			return nil, false, 0, err
		}
		colKind := pieceColumnar
		if len(cols) != delta.ColumnarStreams {
			colKind = pieceColumnarDisp
		}
		sides, err := delta.CMColumnarSides(pred[a:b], cols[0], cols[1])
		if err != nil {
			return nil, false, 0, err
		}
		kind, streams := pieceLZ, [][]byte{corr}
		zl, cl := compressAll(streams, nil), compressAll(cols, colSides(sides))
		if total(cl) < total(zl) {
			kind, streams, zl = colKind, cols, cl
		} else {
			modal = modal || delta.UsesModalCorrection(corr)
		}
		w.u(uint64(b - a))
		w.raw([]byte{kind})
		w.u(uint64(len(streams)))
		for j, z := range zl {
			w.u(uint64(len(streams[j])))
			w.u(uint64(len(z.b)))
			w.raw([]byte{z.codec})
			zs = append(zs, z.b)
		}
		n++
	}
	head := &wbuf{}
	head.u(uint64(n))
	stream = append(head.b, w.b...)
	for _, z := range zs {
		stream = append(stream, z...)
	}
	return stream, modal, len(stream), nil
}

// Piece kinds.
const (
	pieceLZ           byte = 0 // one stream: the flagged adaptive correction
	pieceColumnar     byte = 1 // delta.ColumnarStreams streams
	pieceColumnarDisp byte = 2 // and delta.DispStreams displacement columns
)

type zstream struct {
	codec byte
	b     []byte
}

// codecCM names the prediction-conditioned context-mixing coder
// (delta/cmcoder.go) in a piece's stream table. It is not a cz tag: every cz
// codec is context-free and this one is not, so split.go dispatches it here
// and cz never sees it. An id neither side knows is refused by name in
// applySplitResidual.
const codecCM byte = 3

// cmMinStream is the smallest stream the CM coder is offered. It runs at
// about 1 MB/s each way; below a few kilobytes its adaptive models have not
// paid for themselves and the attempt is only encode time.
const cmMinStream = 4 << 10

// colSides maps the per-bucket side information onto the columnar stream
// table, where the buckets start after the gaps and lens columns.
func colSides(buckets []*delta.CMSide) []*delta.CMSide {
	sides := make([]*delta.CMSide, 2+len(buckets))
	copy(sides[2:], buckets)
	return sides
}

// compressAll compresses each stream with cz. When sides is non-nil the
// large streams are additionally offered to the CM coder, under whatever
// per-position conditioning they have, and the smaller of the two ships:
// the coder is a candidate, never a commitment.
func compressAll(streams [][]byte, sides []*delta.CMSide) []zstream {
	out := make([]zstream, len(streams))
	for i, s := range streams {
		out[i].codec, out[i].b = cz.Compress(s)
		if sides == nil || len(s) < cmMinStream {
			continue
		}
		var side *delta.CMSide
		if i < len(sides) {
			side = sides[i]
		}
		if c, err := delta.CMEncode(s, side); err == nil && len(c) < len(out[i].b) {
			out[i] = zstream{codecCM, c}
		}
	}
	return out
}

func total(zs []zstream) int {
	n := 0
	for _, z := range zs {
		n += len(z.b)
	}
	return n
}

// applySplitResidual is splitResidual's decoder: each piece is decompressed
// and applied in place over its span of out, which holds the prediction.
func applySplitResidual(out, stream []byte, disp func() *delta.DispContext) error {
	r := &rbuf{b: stream}
	n := r.un(1<<20, "piece count")
	type piece struct {
		length    uint64
		kind      byte
		raw, zlen []uint64
		codec     []byte
	}
	ps := make([]piece, 0, min(n, 1<<12))
	var totalLen uint64
	for i := uint64(0); i < n && r.err == nil; i++ {
		var p piece
		p.length = r.un(uint64(len(out)), "piece length")
		p.kind = r.byte()
		ns := r.un(delta.ColumnarStreams+delta.DispStreams, "piece stream count")
		for j := uint64(0); j < ns && r.err == nil; j++ {
			p.raw = append(p.raw, r.un(uint64(len(out))*4+1<<16, "piece stream length"))
			p.zlen = append(p.zlen, r.un(uint64(len(stream)), "piece compressed length"))
			p.codec = append(p.codec, r.byte())
		}
		totalLen += p.length
		ps = append(ps, p)
	}
	if r.err != nil {
		return r.err
	}
	if totalLen != uint64(len(out)) {
		return fmt.Errorf("%w: pieces cover %d bytes, region is %d", ErrCorrupt, totalLen, len(out))
	}
	var at uint64
	for _, p := range ps {
		blobs := make([][]byte, len(p.raw))
		for j := range p.raw {
			blobs[j] = r.take(p.zlen[j])
			if r.err != nil {
				return r.err
			}
		}
		span := out[at : at+p.length]
		streams := make([][]byte, len(p.raw))
		var sides []*delta.CMSide
		get := func(j int) error {
			var side *delta.CMSide
			if j < len(sides) {
				side = sides[j]
			}
			s, err := decodeStream(p.codec[j], blobs[j], int(p.raw[j]), side)
			if err != nil {
				return fmt.Errorf("%w: piece: %v", ErrCorrupt, err)
			}
			streams[j] = s
			return nil
		}
		// A byte bucket coded by the CM coder is conditioned on the
		// prediction under each of its bytes, which the gaps and lens
		// columns before it fix. So those two are decoded first and the
		// conditioning derived from them and from span, which still holds
		// the prediction.
		lead := len(streams)
		if p.kind != pieceLZ {
			lead = min(lead, 2)
		}
		for j := 0; j < lead; j++ {
			if err := get(j); err != nil {
				return err
			}
		}
		if lead == 2 && slices.Contains(p.codec[2:], codecCM) {
			buckets, err := delta.CMColumnarSides(span, streams[0], streams[1])
			if err != nil {
				return fmt.Errorf("%w: piece: %v", ErrCorrupt, err)
			}
			sides = colSides(buckets)
		}
		for j := lead; j < len(streams); j++ {
			if err := get(j); err != nil {
				return err
			}
		}
		var err error
		switch {
		case p.kind == pieceLZ && len(streams) == 1:
			err = delta.ApplyFlaggedCorrection(span, streams[0])
		case p.kind == pieceColumnar:
			err = delta.ApplyColumnar(span, streams)
		case p.kind == pieceColumnarDisp:
			err = delta.ApplyColumnarDisp(span, streams, disp().Restrict(int(at), int(at)+int(p.length)))
		default:
			err = fmt.Errorf("%w: piece kind %d with %d streams", ErrCorrupt, p.kind, len(streams))
		}
		if err != nil {
			return err
		}
		at += p.length
	}
	return r.done()
}

// decodeStream reverses one entry of a piece's stream table. An id neither
// cz nor the CM coder knows is refused by name.
func decodeStream(codec byte, z []byte, n int, side *delta.CMSide) ([]byte, error) {
	if codec == codecCM {
		return delta.CMDecode(z, n, side)
	}
	return cz.Decompress(codec, z, n)
}

// frameCost is what a stream costs once the container frames and
// compresses it, so a piecewise correction is chosen on the number that
// ships.
func frameCost(b []byte) int {
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
