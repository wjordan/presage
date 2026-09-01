package presage

import (
	"errors"
	"fmt"
	"slices"
	"sync"

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
	n := len(bounds) - 1
	// Each piece is independent, so they are coded concurrently and written
	// back by index: the stream this returns does not depend on the order
	// they finish in. The pieces are extremely skewed -- on a browser one of
	// them carries most of the correction -- so this is worth a fifth, not
	// the piece count, and the limit is there because each worker holds its
	// own copy of a piece's streams.
	out := make([]pieceCode, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	sem := make(chan struct{}, min(n, splitWorkers))
	for i := range n {
		a, b := bounds[i], bounds[i+1]
		if a < 0 || b <= a || b > int64(len(target)) {
			return nil, false, 0, fmt.Errorf("presage: cut %d..%d outside a %d-byte region", a, b, len(target))
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i], errs[i] = codePiece(pred[a:b], target[a:b], disp.Restrict(int(a), int(b)))
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, false, 0, err
	}

	w := &wbuf{}
	var zs [][]byte
	for i, p := range out {
		modal = modal || p.modal
		w.u(uint64(bounds[i+1] - bounds[i]))
		w.raw([]byte{p.kind})
		w.u(uint64(len(p.streams)))
		for j, z := range p.zl {
			w.u(uint64(len(p.streams[j])))
			w.u(uint64(len(z.b)))
			w.raw([]byte{z.codec})
			zs = append(zs, z.b)
		}
	}
	head := &wbuf{}
	head.u(uint64(n))
	stream = append(head.b, w.b...)
	for _, z := range zs {
		stream = append(stream, z...)
	}
	return stream, modal, len(stream), nil
}

// splitWorkers bounds how many pieces are coded at once. A piece's coder
// holds the span's correction, its columns and both compressed forms, so the
// bound is about peak memory rather than about cores; the skew means a
// higher one buys almost nothing anyway.
const splitWorkers = 4

// pieceCode is one coded piece, held until every piece is done so the
// stream is assembled in cut order.
type pieceCode struct {
	kind    byte
	streams [][]byte
	zl      []zstream
	modal   bool
}

// codePiece codes one piece both ways and keeps the smaller.
func codePiece(pred, target []byte, disp *delta.DispContext) (p pieceCode, err error) {
	corr, err := delta.EncodeCorrectionAdaptive(pred, target)
	if err != nil {
		return p, err
	}
	cols, err := delta.EncodeColumnarDisp(pred, target, disp)
	if err != nil {
		return p, err
	}
	colKind := pieceColumnar
	if len(cols) != delta.ColumnarStreams {
		colKind = pieceColumnarDisp
	}
	sides, err := delta.CMColumnarSides(pred, cols[0], cols[1], disp)
	if err != nil {
		return p, err
	}
	p.kind, p.streams = pieceLZ, [][]byte{corr}
	// The lz shape's single stream is not offered the CM coder. It has no
	// positional alignment with the prediction -- it is a flagged copy/replace
	// program, not a byte column -- so the coder would run on its generic and
	// match models alone, and measured that way it is worth -1,187 on Chrome
	// and nothing at all on libxul for +7 s of encode. brotli is already good
	// at this stream; the CM win lives entirely in pred[i].
	lzT, colT := trialAll(p.streams, nil), trialAll(cols, colSides(sides))
	won := lzT
	if colT.total < lzT.total {
		p.kind, p.streams, won = colKind, cols, colT
	} else {
		p.modal = delta.UsesModalCorrection(corr)
	}
	p.zl = compressAll(p.streams, won.cm)
	return p, nil
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

// codecCM names the original prediction-conditioned context-mixing coder
// (delta/cmcoder.go) in a piece's stream table. It is not a cz tag: every cz
// codec is context-free and this one is not, so split.go dispatches it here
// and cz never sees it. An id neither side knows is refused by name in
// applySplitResidual.
const (
	codecCM        byte = 3
	codecCMCompact byte = 4
)

// cmMinStream is the smallest stream the CM coder is offered; delta gates the
// field-context walk on the same threshold.
const cmMinStream = delta.CMMinStream

// cmMinGain keeps the slower terminal coder for streams where it makes a
// material contribution. Codec choice is encoder policy carried in the
// stream table, so changing this threshold does not affect compatibility.
const cmMinGain = 2 << 10

func cmWorth(coded []byte, incumbent int) bool {
	return len(coded)+max(cmMinGain, incumbent/10) < incumbent
}

// colSides maps the per-bucket side information onto the columnar stream
// table, where the buckets start after the gaps and lens columns.
func colSides(buckets []*delta.CMSide) []*delta.CMSide {
	sides := make([]*delta.CMSide, 2+len(buckets))
	copy(sides[2:], buckets)
	return sides
}

// A pieceTrial prices one candidate shape of a piece. Only one of the two shapes
// ships, so the price is a cz.SizeProxy rather than a real compression --
// but the CM coder's price is its real output, because that coder has no
// cheap proxy and its bytes are worth keeping: if the shape it belongs to
// wins, compressAll ships them without coding them twice.
type pieceTrial struct {
	cm    [][]byte // the CM coder's output per stream, nil where not offered
	total int
}

// trialAll prices each stream. When sides is non-nil the large streams are
// additionally offered to the CM coder, under whatever per-position
// conditioning they have: the coder is a candidate, never a commitment.
func trialAll(streams [][]byte, sides []*delta.CMSide) pieceTrial {
	t := pieceTrial{cm: make([][]byte, len(streams))}
	for i, s := range streams {
		n := cz.SizeProxy(s)
		if sides != nil && len(s) >= cmMinStream {
			var side *delta.CMSide
			if i < len(sides) {
				side = sides[i]
			}
			if c, err := delta.CMEncodeCompact(s, side); err == nil {
				if cmWorth(c, n) {
					t.cm[i] = c
					n = len(c)
				}
			}
		}
		t.total += n
	}
	return t
}

// compressAll makes the shipping table for the shape that won: each stream
// compressed with cz for real, and the CM coder's bytes from the trial kept
// wherever they are still smaller.
func compressAll(streams [][]byte, cm [][]byte) []zstream {
	out := make([]zstream, len(streams))
	for i, s := range streams {
		out[i].codec, out[i].b = cz.Compress(s)
		if i < len(cm) && cm[i] != nil && cmWorth(cm[i], len(out[i].b)) {
			out[i] = zstream{codecCMCompact, cm[i]}
		}
	}
	return out
}

// piece is one entry of a split residual's table, with the compressed blob of
// each of its streams.
type piece struct {
	length    uint64
	kind      byte
	raw, zlen []uint64
	codec     []byte
	blobs     [][]byte
}

// leadStreams is how many of a piece's streams carry no side information, and
// so can be decoded before the buffer they will be applied over even exists.
// For a columnar piece they are the gaps and lens columns, which are what the
// side information for the rest is derived *from*.
func (p piece) leadStreams() int {
	if p.kind == pieceLZ {
		return len(p.raw)
	}
	return min(len(p.raw), 2)
}

// parsePieces reads a split residual's piece table and the blobs behind it.
func parsePieces(stream []byte, outLen int) ([]piece, error) {
	r := &rbuf{b: stream}
	n := r.un(1<<20, "piece count")
	ps := make([]piece, 0, min(n, 1<<12))
	var totalLen uint64
	for i := uint64(0); i < n && r.err == nil; i++ {
		var p piece
		p.length = r.un(uint64(outLen), "piece length")
		p.kind = r.byte()
		ns := r.un(delta.ColumnarStreams+delta.DispStreams, "piece stream count")
		for j := uint64(0); j < ns && r.err == nil; j++ {
			p.raw = append(p.raw, r.un(uint64(outLen)*4+1<<16, "piece stream length"))
			p.zlen = append(p.zlen, r.un(uint64(len(stream)), "piece compressed length"))
			p.codec = append(p.codec, r.byte())
		}
		totalLen += p.length
		ps = append(ps, p)
	}
	if r.err != nil {
		return nil, r.err
	}
	if totalLen != uint64(outLen) {
		return nil, fmt.Errorf("%w: pieces cover %d bytes, region is %d", ErrCorrupt, totalLen, outLen)
	}
	for i := range ps {
		ps[i].blobs = make([][]byte, len(ps[i].raw))
		for j := range ps[i].raw {
			ps[i].blobs[j] = r.take(ps[i].zlen[j])
			if r.err != nil {
				return nil, r.err
			}
		}
	}
	if err := r.done(); err != nil {
		return nil, err
	}
	return ps, nil
}

// splitPrefetch decodes a split residual's side-free streams while the
// prediction they will be applied over is still being built. Those streams are
// self-contained coded blobs -- their conditioning is fixed before any of the
// prediction is known -- so this is the same work moved off the critical path,
// not different work. It is advisory throughout: anything it cannot do it
// simply does not do, and applySplitResidual decodes it the usual way and
// raises the usual error.
type splitPrefetch struct {
	done   chan struct{}
	pieces []piece
	lead   [][][]byte // per piece, the decoded lead streams; nil entries missing
}

func prefetchSplitResidual(stream []byte, outLen int) *splitPrefetch {
	pf := &splitPrefetch{done: make(chan struct{})}
	go func() {
		defer close(pf.done)
		ps, err := parsePieces(stream, outLen)
		if err != nil {
			return
		}
		lead := make([][][]byte, len(ps))
		var wg sync.WaitGroup
		for i, p := range ps {
			lead[i] = make([][]byte, p.leadStreams())
			for j := range lead[i] {
				wg.Add(1)
				go func(i, j int) {
					defer wg.Done()
					if s, err := decodeStream(ps[i].codec[j], ps[i].blobs[j], int(ps[i].raw[j]), nil); err == nil {
						lead[i][j] = s
					}
				}(i, j)
			}
		}
		wg.Wait()
		pf.pieces, pf.lead = ps, lead
	}()
	return pf
}

// take returns what the prefetch decoded for piece i's stream j, or nil.
func (pf *splitPrefetch) take(i, j int) []byte {
	if pf == nil || i >= len(pf.lead) || j >= len(pf.lead[i]) {
		return nil
	}
	return pf.lead[i][j]
}

// applySplitResidual is splitResidual's decoder: each piece is decompressed
// and applied in place over its span of out, which holds the prediction.
func applySplitResidual(out, stream []byte, disp func() *delta.DispContext, pf *splitPrefetch) error {
	if pf != nil {
		<-pf.done
	}
	ps, err := parsePieces(stream, len(out))
	if err != nil {
		return err
	}
	if pf != nil && len(pf.pieces) != len(ps) {
		pf = nil // a table it read differently is a table it cannot speak for
	}
	var at uint64
	for pi, p := range ps {
		blobs := p.blobs
		pieceAt := at
		span := out[pieceAt : pieceAt+p.length]
		pieceDisp := sync.OnceValue(func() *delta.DispContext {
			return disp().Restrict(int(pieceAt), int(pieceAt)+int(p.length))
		})
		streams := make([][]byte, len(p.raw))
		var sides []*delta.CMSide
		get := func(j int) error {
			var side *delta.CMSide
			if j < len(sides) {
				side = sides[j]
			}
			if side == nil {
				if s := pf.take(pi, j); s != nil && len(s) == int(p.raw[j]) {
					streams[j] = s
					return nil
				}
			}
			s, err := decodeStream(p.codec[j], blobs[j], int(p.raw[j]), side)
			if err != nil {
				return fmt.Errorf("%w: piece: %v", ErrCorrupt, err)
			}
			streams[j] = s
			return nil
		}
		// The streams of one round are independent of each other: each is a
		// self-contained coded blob whose only context is its own side, fixed
		// before the round starts. Decoding them concurrently costs nothing
		// and no byte of the result moves. The CM coder runs about a megabyte
		// a second, so the round's wall is its slowest stream, not their sum.
		getRange := func(lo, hi int) error {
			if hi-lo <= 1 {
				if hi-lo == 1 {
					return get(lo)
				}
				return nil
			}
			errs := make([]error, hi-lo)
			var wg sync.WaitGroup
			for j := lo; j < hi; j++ {
				wg.Add(1)
				go func(j int) { defer wg.Done(); errs[j-lo] = get(j) }(j)
			}
			wg.Wait()
			// Reported in stream order, so the error a corrupt patch draws
			// does not depend on the scheduler.
			for _, err := range errs {
				if err != nil {
					return err
				}
			}
			return nil
		}
		// A byte bucket coded by the CM coder is conditioned on the
		// prediction under each of its bytes, and on the instruction that
		// prediction byte sits in — the gaps and lens columns before it fix
		// the former and the piece's field context the latter. So those two
		// columns are decoded first and the conditioning derived from them
		// and from span, which still holds the prediction.
		lead := p.leadStreams()
		if err := getRange(0, lead); err != nil {
			return err
		}
		if lead == 2 && slices.ContainsFunc(p.codec[2:], isCMCodec) {
			buckets, err := delta.CMColumnarSides(span, streams[0], streams[1],
				pieceDisp())
			if err != nil {
				return fmt.Errorf("%w: piece: %v", ErrCorrupt, err)
			}
			sides = colSides(buckets)
		}
		if err := getRange(lead, len(streams)); err != nil {
			return err
		}
		var err error
		switch {
		case p.kind == pieceLZ && len(streams) == 1:
			err = delta.ApplyFlaggedCorrection(span, streams[0])
		case p.kind == pieceColumnar:
			err = delta.ApplyColumnar(span, streams)
		case p.kind == pieceColumnarDisp:
			err = delta.ApplyColumnarDisp(span, streams, pieceDisp())
		default:
			err = fmt.Errorf("%w: piece kind %d with %d streams", ErrCorrupt, p.kind, len(streams))
		}
		if err != nil {
			return err
		}
		at += p.length
	}
	return nil
}

// decodeStream reverses one entry of a piece's stream table. An id neither
// cz nor the CM coder knows is refused by name.
func decodeStream(codec byte, z []byte, n int, side *delta.CMSide) ([]byte, error) {
	if codec == codecCM {
		return delta.CMDecode(z, n, side)
	}
	if codec == codecCMCompact {
		return delta.CMDecodeCompact(z, n, side)
	}
	return cz.Decompress(codec, z, n)
}

func isCMCodec(codec byte) bool { return codec == codecCM || codec == codecCMCompact }
