package elfmod

import (
	"encoding/binary"
	"errors"
	"slices"
	"sort"

	"github.com/wjordan/presage/delta/x86"
)

// equivalencePlan is the whole-image run list: the geometry of both images
// and their .text sections, and the four run columns. Sources of runs that
// start inside a mapped function are coded as a residual against the
// function map (SrcResidual); the others as a skip from the previous
// run's end (SrcSkip). Which column an entry uses follows from its
// destination, so nothing marks the choice per entry.
type equivalencePlan struct {
	OldLen      uint64
	NewLen      uint64
	Windows     []codeWindow
	SrcSkip     []byte
	SrcResidual []byte
	DstSkip     []byte
	CopyLen     []byte
	Predicted   bool
	Eqs         []equivalence
}

// srcPredictor answers, for a destination offset, where the function map
// says those bytes came from.
type srcWindow struct {
	maps           []mapping // in destination order
	oldOff, newOff uint64    // window file offsets
	newSize        uint64
}

type srcPredictor struct {
	win []srcWindow
}

// newSrcPredictor holds the mapped windows only; a window with no map
// contributes nothing and an image with no map at all gets a nil predictor,
// which marshals every source as a skip.
func newSrcPredictor(structures []predictionPlan, windows []codeWindow) *srcPredictor {
	var win []srcWindow
	for i, w := range windows {
		if i >= len(structures) || len(structures[i].Maps) == 0 {
			continue
		}
		win = append(win, srcWindow{maps: structures[i].Maps, oldOff: w.Old.Off, newOff: w.New.Off, newSize: w.New.Size})
	}
	if len(win) == 0 {
		return nil
	}
	return &srcPredictor{win: win}
}

func (s *srcPredictor) at(dst uint64) (uint64, bool) {
	if s == nil {
		return 0, false
	}
	for _, w := range s.win {
		if dst < w.newOff || dst >= w.newOff+w.newSize {
			continue
		}
		off := dst - w.newOff
		i := sort.Search(len(w.maps), func(i int) bool { return w.maps[i].Dst > off })
		if i == 0 {
			return 0, false
		}
		m := w.maps[i-1]
		if off >= m.Dst+m.DstSize {
			return 0, false
		}
		return w.oldOff + m.Src + (off - m.Dst), true
	}
	return 0, false
}

// encodeColumns rebuilds the three unpredicted columns from the runs.
func encodeColumns(eqs []equivalence) (srcSkip, dstSkip, copyLen []byte) {
	var prevSrcEnd, prevDstEnd uint64
	for _, e := range eqs {
		srcSkip = appendS(srcSkip, int64(e.Src)-int64(prevSrcEnd))
		dstSkip = appendU(dstSkip, e.Dst-prevDstEnd)
		copyLen = appendU(copyLen, e.N)
		prevSrcEnd, prevDstEnd = e.Src+e.N, e.Dst+e.N
	}
	return srcSkip, dstSkip, copyLen
}

func decodeEquivalences(p equivalencePlan, pred *srcPredictor) ([]equivalence, error) {
	if p.Predicted != (pred != nil) {
		return nil, errors.New("equivalence plan and function map disagree about predicted sources")
	}
	skip, residual, dst, count := p.SrcSkip, p.SrcResidual, p.DstSkip, p.CopyLen
	var eqs []equivalence
	var prevSrcEnd, prevDstEnd uint64
	for len(dst) != 0 {
		dstGap, ndst := binary.Uvarint(dst)
		n, ncount := binary.Uvarint(count)
		if ndst <= 0 || ncount <= 0 || n == 0 {
			return nil, errors.New("invalid equivalence stream value")
		}
		dst, count = dst[ndst:], count[ncount:]
		if dstGap > ^uint64(0)-prevDstEnd {
			return nil, errors.New("equivalence destination overflows")
		}
		dstOff := prevDstEnd + dstGap
		base, predicted := pred.at(dstOff)
		stream := &skip
		if predicted {
			stream = &residual
		} else {
			base = prevSrcEnd
		}
		delta, nsrc := binary.Varint(*stream)
		if nsrc <= 0 {
			return nil, errors.New("invalid equivalence source value")
		}
		*stream = (*stream)[nsrc:]
		srcOff, err := shiftOffset(base, delta)
		if err != nil {
			return nil, err
		}
		if srcOff > p.OldLen || n > p.OldLen-srcOff || dstOff > p.NewLen || n > p.NewLen-dstOff {
			return nil, errors.New("equivalence exceeds an image")
		}
		eqs = append(eqs, equivalence{Src: srcOff, Dst: dstOff, N: n})
		prevSrcEnd, prevDstEnd = srcOff+n, dstOff+n
	}
	if len(skip) != 0 || len(residual) != 0 || len(count) != 0 {
		return nil, errors.New("equivalence streams have unequal counts")
	}
	return eqs, nil
}

func shiftOffset(base uint64, delta int64) (uint64, error) {
	if delta >= 0 {
		if uint64(delta) > ^uint64(0)-base {
			return 0, errors.New("equivalence source overflows")
		}
		return base + uint64(delta), nil
	}
	d := uint64(-(delta + 1)) + 1
	if d > base {
		return 0, errors.New("equivalence source underflows")
	}
	return base - d, nil
}

// marshal writes the plan, rebuilding the source columns against pred; a
// nil pred writes every source as a skip.
func (p equivalencePlan) marshal(pred *srcPredictor) ([]byte, error) {
	p.Predicted = pred != nil
	p.SrcSkip, p.SrcResidual = nil, nil
	var prevSrcEnd uint64
	for _, e := range p.Eqs {
		if base, ok := pred.at(e.Dst); ok {
			p.SrcResidual = appendS(p.SrcResidual, int64(e.Src)-int64(base))
		} else {
			p.SrcSkip = appendS(p.SrcSkip, int64(e.Src)-int64(prevSrcEnd))
		}
		prevSrcEnd = e.Src + e.N
	}
	p.DstSkip, p.CopyLen = nil, nil
	_, p.DstSkip, p.CopyLen = encodeColumns(p.Eqs)
	if _, err := decodeEquivalences(p, pred); err != nil {
		return nil, err
	}
	var b []byte
	b = appendU(b, p.OldLen)
	b = appendU(b, p.NewLen)
	b = appendU(b, uint64(len(p.Windows)))
	for _, w := range p.Windows {
		for _, s := range []section{w.Old, w.New} {
			b = appendU(b, s.Addr)
			b = appendU(b, s.Off)
			b = appendU(b, s.Size)
		}
	}
	if p.Predicted {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	b = appendStream(b, p.SrcSkip)
	b = appendStream(b, p.SrcResidual)
	b = appendStream(b, p.DstSkip)
	b = appendStream(b, p.CopyLen)
	return b, nil
}

// parseEquivalencePlan reads everything but the runs themselves: their
// source column may be written against the function map, which the caller
// decodes from another stream first.
func parseEquivalencePlan(b []byte) (equivalencePlan, error) {
	r := &planReader{b: b}
	p := equivalencePlan{OldLen: r.u(), NewLen: r.u()}
	n := r.u()
	if r.err != nil || n == 0 || n > 1<<16 {
		return equivalencePlan{}, errors.New("implausible code window count")
	}
	p.Windows = make([]codeWindow, n)
	for i := range p.Windows {
		p.Windows[i] = codeWindow{
			Old: section{Addr: r.u(), Off: r.u(), Size: r.u()},
			New: section{Addr: r.u(), Off: r.u(), Size: r.u()},
		}
	}
	flag := r.byteAt()
	src, residual, dst, count := r.stream(), r.stream(), r.stream(), r.stream()
	if !r.done() || flag > 1 {
		return equivalencePlan{}, errors.New("invalid equivalence plan streams")
	}
	p.Predicted = flag == 1
	p.SrcSkip, p.SrcResidual = slices.Clone(src.b), slices.Clone(residual.b)
	p.DstSkip, p.CopyLen = slices.Clone(dst.b), slices.Clone(count.b)
	for i, w := range p.Windows {
		if w.Old.Off > p.OldLen || w.Old.Size > p.OldLen-w.Old.Off || w.New.Off > p.NewLen || w.New.Size > p.NewLen-w.New.Off {
			return equivalencePlan{}, errors.New("code window exceeds equivalence image")
		}
		// Overlapping windows would let two maps claim one byte, and the
		// order the decoder happens to apply them in would decide the image.
		if i != 0 && (w.Old.Off < p.Windows[i-1].Old.Off+p.Windows[i-1].Old.Size ||
			w.New.Off < p.Windows[i-1].New.Off+p.Windows[i-1].New.Size) {
			return equivalencePlan{}, errors.New("code windows overlap or are unsorted")
		}
	}
	return p, nil
}

// sourceEquivalenceMapper projects old offsets through the runs, keyed by
// source; overlapping sources are de-overlapped, the longer run winning.
type sourceEquivalenceMapper struct {
	eqs    []equivalence
	bySrc  *pageIndex
	oldLen uint64
	newLen uint64
}

// after is the number of runs starting at or before off, the index the
// callers' sort.Search returns.
func (m sourceEquivalenceMapper) after(off uint64) int {
	lo, hi := m.bySrc.bounds(off, len(m.eqs))
	return lo + sort.Search(hi-lo, func(i int) bool { return m.eqs[lo+i].Src > off })
}

func newSourceEquivalenceMapper(p equivalencePlan) sourceEquivalenceMapper {
	eqs := slices.Clone(p.Eqs)
	slices.SortFunc(eqs, func(a, b equivalence) int {
		if a.Src != b.Src {
			return cmpU(a.Src, b.Src)
		}
		if a.N != b.N {
			return -cmpU(a.N, b.N)
		}
		return cmpU(a.Dst, b.Dst)
	})
	for current := 0; current < len(eqs); current++ {
		if eqs[current].N == 0 {
			continue
		}
		currentEnd := eqs[current].Src + eqs[current].N
		next, reaper := current+1, false
		for ; next < len(eqs); next++ {
			if eqs[next].Src >= currentEnd {
				break
			}
			if eqs[current].N < eqs[next].N {
				eqs[current].N -= currentEnd - eqs[next].Src
				reaper = true
				break
			}
		}
		if reaper {
			for i := current + 1; i < next; i++ {
				eqs[i].N = 0
			}
			current = next - 1
			continue
		}
		for i := current + 1; i < next; i++ {
			delta := currentEnd - eqs[i].Src
			capped := min(eqs[i].N, delta)
			eqs[i].N -= capped
			eqs[i].Src = currentEnd
			eqs[i].Dst += capped
		}
	}
	eqs = slices.DeleteFunc(eqs, func(eq equivalence) bool { return eq.N == 0 })
	bySrc := newPageIndex(len(eqs), func(i int) uint64 { return eqs[i].Src }, nil)
	return sourceEquivalenceMapper{eqs: eqs, bySrc: bySrc, oldLen: p.OldLen, newLen: p.NewLen}
}

// within projects an old offset through the run that copies it, or reports
// false: unlike project it never extrapolates.
func (m sourceEquivalenceMapper) within(off uint64) (uint64, bool) {
	i := m.after(off)
	if i == 0 || off >= m.eqs[i-1].Src+m.eqs[i-1].N {
		return 0, false
	}
	return off - m.eqs[i-1].Src + m.eqs[i-1].Dst, true
}

// project extrapolates to the nearest run.
func (m sourceEquivalenceMapper) project(off uint64) (uint64, bool) {
	if off >= m.oldLen || len(m.eqs) == 0 || m.newLen == 0 {
		return 0, false
	}
	i := m.after(off)
	if i > 0 && (i == len(m.eqs) || off < m.eqs[i-1].Src+m.eqs[i-1].N || off-(m.eqs[i-1].Src+m.eqs[i-1].N) < m.eqs[i].Src-off) {
		i--
	}
	eq := m.eqs[i]
	projected := int64(off) - int64(eq.Src) + int64(eq.Dst)
	if projected < 0 {
		return 0, true
	}
	if uint64(projected) >= m.newLen {
		return m.newLen - 1, true
	}
	return uint64(projected), true
}

func readDisplacement(body []byte, ref x86.Reference) (int64, bool) {
	if ref.Off < 0 || ref.N <= 0 || ref.Off > len(body)-ref.N {
		return 0, false
	}
	switch ref.N {
	case 1:
		return int64(int8(body[ref.Off])), true
	case 2:
		return int64(int16(binary.LittleEndian.Uint16(body[ref.Off:]))), true
	case 4:
		return int64(int32(binary.LittleEndian.Uint32(body[ref.Off:]))), true
	default:
		return 0, false
	}
}

func writeDisplacement(body []byte, ref x86.Reference, disp int64) bool {
	switch ref.N {
	case 1:
		if disp < -128 || disp > 127 {
			return false
		}
		body[ref.Off] = byte(int8(disp))
	case 2:
		if disp < -32768 || disp > 32767 {
			return false
		}
		binary.LittleEndian.PutUint16(body[ref.Off:], uint16(int16(disp)))
	case 4:
		if disp < -1<<31 || disp >= 1<<31 {
			return false
		}
		binary.LittleEndian.PutUint32(body[ref.Off:], uint32(int32(disp)))
	default:
		return false
	}
	return true
}
