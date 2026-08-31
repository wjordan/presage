package presage

import (
	"fmt"

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
		kind, streams := pieceLZ, [][]byte{corr}
		zl, cl := compressAll(streams), compressAll(cols)
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

func compressAll(streams [][]byte) []zstream {
	out := make([]zstream, len(streams))
	for i, s := range streams {
		out[i].codec, out[i].b = cz.Compress(s)
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
		streams := make([][]byte, len(p.raw))
		for j := range p.raw {
			z := r.take(p.zlen[j])
			if r.err != nil {
				return r.err
			}
			s, err := cz.Decompress(p.codec[j], z, int(p.raw[j]))
			if err != nil {
				return fmt.Errorf("%w: piece: %v", ErrCorrupt, err)
			}
			streams[j] = s
		}
		span := out[at : at+p.length]
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
