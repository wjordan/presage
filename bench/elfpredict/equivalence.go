package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/delta/x86"
)

var (
	equivalenceMagic = [4]byte{'E', 'Q', 'P', '2'}
	combinedMagic    = [4]byte{'C', 'B', 'P', '1'}
)

type equivalence struct {
	Src uint64
	Dst uint64
	N   uint64
}

type equivalencePlan struct {
	OldLen  uint64
	NewLen  uint64
	OldText section
	NewText section
	SrcSkip []byte
	// SrcResidual holds the sources the function map can predict, as the
	// difference from its prediction. Which of the two streams an entry uses
	// follows from its destination, so nothing marks the choice per entry.
	SrcResidual []byte
	DstSkip     []byte
	CopyLen     []byte
	Predicted   bool
	Eqs         []equivalence
}

// srcPredictor answers, for a destination offset, where the function map says
// those bytes came from. An equivalence run usually starts inside a mapped
// function, and where it does, the map has already paid for the correspondence
// once; the source column only has to say where the run's start differs from
// what the map implies.
type srcPredictor struct {
	maps           []mapping // in destination order
	oldOff, newOff uint64    // .text file offsets
	newSize        uint64
}

// restOfFunction reports how many bytes remain in the mapped function that
// contains dst, which is a length the decoder can compute for itself.
func (s *srcPredictor) restOfFunction(dst uint64) (uint64, bool) {
	if s == nil || len(s.maps) == 0 || dst < s.newOff || dst >= s.newOff+s.newSize {
		return 0, false
	}
	off := dst - s.newOff
	i := sort.Search(len(s.maps), func(i int) bool { return s.maps[i].Dst > off })
	if i == 0 {
		return 0, false
	}
	m := s.maps[i-1]
	if off >= m.Dst+m.DstSize {
		return 0, false
	}
	return m.Dst + m.DstSize - off, true
}

func (s *srcPredictor) at(dst uint64) (uint64, bool) {
	if s == nil || len(s.maps) == 0 || dst < s.newOff || dst >= s.newOff+s.newSize {
		return 0, false
	}
	off := dst - s.newOff
	i := sort.Search(len(s.maps), func(i int) bool { return s.maps[i].Dst > off })
	if i == 0 {
		return 0, false
	}
	m := s.maps[i-1]
	if off >= m.Dst+m.DstSize {
		return 0, false
	}
	return s.oldOff + m.Src + (off - m.Dst), true
}

type combinedPlan struct {
	Equivalences []byte
	Structure    []byte
	Choices      []byte
	Reloc        []byte
	EhFrame      []byte
	RoData       []byte
	Fields       []byte
	GoTables     []byte
	Dwarf        []byte
}

type combinedStats struct {
	GoTables             delta.GoTablesStats `json:"go_tables"`
	Dwarf                dwarfStats          `json:"dwarf"`
	Equivalences         int                 `json:"equivalences"`
	EquivalenceTextBytes int                 `json:"equivalence_text_bytes"`
	SelectedFunctions    int                 `json:"selected_functions"`
	SelectedBytes        int                 `json:"selected_function_bytes"`
	Relocation           x86.Stats           `json:"relocation"`
	Reloc                relocStats          `json:"reloc_table,omitempty"`
	EhFrame              ehFrameStats        `json:"eh_frame,omitempty"`
	RoData               roDataStats         `json:"rodata,omitempty"`
	Fields               fieldStats          `json:"fields,omitempty"`
}

type sourceEquivalenceMapper struct {
	eqs    []equivalence
	oldLen uint64
	newLen uint64
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
	return sourceEquivalenceMapper{eqs: eqs, oldLen: p.OldLen, newLen: p.NewLen}
}

// within projects an old offset through the equivalence that copies it,
// or reports false: unlike project it never extrapolates.
func (m sourceEquivalenceMapper) within(off uint64) (uint64, bool) {
	i := sort.Search(len(m.eqs), func(i int) bool { return m.eqs[i].Src > off })
	if i == 0 || off >= m.eqs[i-1].Src+m.eqs[i-1].N {
		return 0, false
	}
	return off - m.eqs[i-1].Src + m.eqs[i-1].Dst, true
}

func (m sourceEquivalenceMapper) project(off uint64) (uint64, bool) {
	if off >= m.oldLen || len(m.eqs) == 0 || m.newLen == 0 {
		return 0, false
	}
	i := sort.Search(len(m.eqs), func(i int) bool { return m.eqs[i].Src > off })
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

func readFixedStream(b []byte, pos *int) ([]byte, error) {
	if *pos < 0 || *pos > len(b)-4 {
		return nil, errors.New("missing external stream length")
	}
	n := uint64(binary.LittleEndian.Uint32(b[*pos:]))
	*pos += 4
	if n > uint64(len(b)-*pos) {
		return nil, errors.New("external stream exceeds patch")
	}
	stream := slices.Clone(b[*pos : *pos+int(n)])
	*pos += int(n)
	return stream, nil
}

func parseExternalEquivalence(path string, oldImage, newImage *image) (equivalencePlan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return equivalencePlan{}, err
	}
	if len(b) < 50 || !bytes.Equal(b[:4], []byte{'Z', 'u', 'c', 'c'}) {
		return equivalencePlan{}, errors.New("unsupported external patch header")
	}
	oldLen := uint64(binary.LittleEndian.Uint32(b[8:12]))
	newLen := uint64(binary.LittleEndian.Uint32(b[16:20]))
	if oldLen != uint64(len(oldImage.Data)) || newLen != uint64(len(newImage.Data)) {
		return equivalencePlan{}, fmt.Errorf("external patch image sizes are %d/%d, want %d/%d", oldLen, newLen, len(oldImage.Data), len(newImage.Data))
	}
	if binary.LittleEndian.Uint32(b[24:28]) != 1 {
		return equivalencePlan{}, errors.New("external patch must contain one whole-image element")
	}
	oldOff := uint64(binary.LittleEndian.Uint32(b[28:32]))
	oldElementLen := uint64(binary.LittleEndian.Uint32(b[32:36]))
	newOff := uint64(binary.LittleEndian.Uint32(b[36:40]))
	newElementLen := uint64(binary.LittleEndian.Uint32(b[40:44]))
	if oldOff != 0 || newOff != 0 || oldElementLen != oldLen || newElementLen != newLen {
		return equivalencePlan{}, errors.New("external patch element does not cover both whole images")
	}
	pos := 50
	srcSkip, err := readFixedStream(b, &pos)
	if err != nil {
		return equivalencePlan{}, err
	}
	dstSkip, err := readFixedStream(b, &pos)
	if err != nil {
		return equivalencePlan{}, err
	}
	copyLen, err := readFixedStream(b, &pos)
	if err != nil {
		return equivalencePlan{}, err
	}
	p := equivalencePlan{
		OldLen: oldLen, NewLen: newLen, OldText: oldImage.Text, NewText: newImage.Text,
		SrcSkip: srcSkip, DstSkip: dstSkip, CopyLen: copyLen,
	}
	p.Eqs, err = decodeEquivalences(p, nil)
	if err != nil {
		return equivalencePlan{}, err
	}
	return p, nil
}

// encodeColumns is decodeEquivalences' inverse for unpredicted sources:
// it rebuilds the three columns from the equivalences, in order.
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

// marshal writes the plan, rebuilding the source columns against pred. A nil
// pred keeps the source column exactly as it arrived, which is what every
// caller without a function map needs.
func (p equivalencePlan) marshal(pred *srcPredictor) ([]byte, error) {
	p.Predicted = pred != nil
	if p.Predicted {
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
	}
	if _, err := decodeEquivalences(p, pred); err != nil {
		return nil, err
	}
	b := append([]byte(nil), equivalenceMagic[:]...)
	b = appendU(b, p.OldLen)
	b = appendU(b, p.NewLen)
	for _, s := range []section{p.OldText, p.NewText} {
		b = appendU(b, s.Addr)
		b = appendU(b, s.Off)
		b = appendU(b, s.Size)
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

// parseEquivalencePlan reads everything but the equivalences themselves. Their
// source column may be written against the function map, which is decoded from
// a different stream of the same plan, so the caller decodes them once it has
// whatever predictor applies.
func parseEquivalencePlan(b []byte) (equivalencePlan, error) {
	if len(b) < 4 || !bytes.Equal(b[:4], equivalenceMagic[:]) {
		return equivalencePlan{}, errors.New("invalid equivalence plan magic")
	}
	r := &planReader{b: b[4:]}
	p := equivalencePlan{OldLen: r.u(), NewLen: r.u()}
	p.OldText = section{Addr: r.u(), Off: r.u(), Size: r.u()}
	p.NewText = section{Addr: r.u(), Off: r.u(), Size: r.u()}
	flag := r.byteAt()
	src, residual, dst, count := r.stream(), r.stream(), r.stream(), r.stream()
	if r.err != nil || len(r.b) != 0 || flag > 1 {
		return equivalencePlan{}, errors.New("invalid equivalence plan streams")
	}
	p.Predicted = flag == 1
	p.SrcSkip, p.SrcResidual = slices.Clone(src.b), slices.Clone(residual.b)
	p.DstSkip, p.CopyLen = slices.Clone(dst.b), slices.Clone(count.b)
	if p.OldText.Off > p.OldLen || p.OldText.Size > p.OldLen-p.OldText.Off || p.NewText.Off > p.NewLen || p.NewText.Size > p.NewLen-p.NewText.Off {
		return equivalencePlan{}, errors.New("text section exceeds equivalence image")
	}
	return p, nil
}

func unmarshalEquivalencePlan(b []byte) (equivalencePlan, error) {
	p, err := parseEquivalencePlan(b)
	if err != nil {
		return equivalencePlan{}, err
	}
	p.Eqs, err = decodeEquivalences(p, nil)
	if err != nil {
		return equivalencePlan{}, err
	}
	return p, nil
}

func predictEquivalences(old []byte, encoded []byte) ([]byte, equivalencePlan, int, error) {
	p, err := unmarshalEquivalencePlan(encoded)
	if err != nil {
		return nil, equivalencePlan{}, 0, err
	}
	if uint64(len(old)) != p.OldLen || p.NewText.Size > uint64(int(^uint(0)>>1)) {
		return nil, equivalencePlan{}, 0, errors.New("old image or prediction size does not match equivalence plan")
	}
	out := make([]byte, int(p.NewText.Size))
	for i := range out {
		out[i] = 0xcc
	}
	textStart, textEnd := p.NewText.Off, p.NewText.Off+p.NewText.Size
	copied := 0
	for _, eq := range p.Eqs {
		start, end := max(eq.Dst, textStart), min(eq.Dst+eq.N, textEnd)
		if start >= end {
			continue
		}
		src := eq.Src + start - eq.Dst
		copy(out[start-textStart:end-textStart], old[src:src+end-start])
		copied += int(end - start)
	}
	return out, p, copied, nil
}

func (p equivalencePlan) sourceAt(dst uint64) (uint64, int, bool) {
	i, ok := slices.BinarySearchFunc(p.Eqs, dst, func(eq equivalence, dst uint64) int {
		if eq.Dst > dst {
			return 1
		}
		if eq.Dst+eq.N <= dst {
			return -1
		}
		return 0
	})
	if !ok {
		return 0, 0, false
	}
	eq := p.Eqs[i]
	return eq.Src + dst - eq.Dst, i, true
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

// newImageOracle answers "where did this old address go". Inside .text the
// equivalence projection is authoritative, because it tracks movement at byte
// granularity. Outside it, an exact reference correspondence is preferred, and
// the same projection is then used across the rest of the image -- which is
// what makes the relocation table predictable. rp is nil for the .text-only
// rungs, which keeps their behaviour, and therefore their measurements,
// exactly as they were.
// oracleParts holds the two derived structures every oracle is built from, so
// a caller that needs several oracles -- predictImage builds five -- sorts the
// 925k-mapping lookup and clips the equivalence list once instead of per
// oracle. Both parts are immutable after construction.
type oracleParts struct {
	ep equivalencePlan
	lk *addressLookup
	sm sourceEquivalenceMapper
}

func newOracleParts(ep equivalencePlan, structure predictionPlan) oracleParts {
	return oracleParts{ep: ep, lk: newAddressLookup(structure), sm: newSourceEquivalenceMapper(ep)}
}

func newImageOracle(ep equivalencePlan, structure predictionPlan, rp *relocPlan) func(uint64) x86.Target {
	return newOracleParts(ep, structure).image(rp)
}

func (o oracleParts) image(rp *relocPlan) func(uint64) x86.Target {
	ep, structuralLookup, sourceMapper := o.ep, o.lk, o.sm
	return func(addr uint64) x86.Target {
		if addr >= ep.OldText.Addr && addr < ep.OldText.Addr+ep.OldText.Size {
			oldFile := ep.OldText.Off + addr - ep.OldText.Addr
			if newFile, ok := sourceMapper.project(oldFile); ok && newFile >= ep.NewText.Off && newFile < ep.NewText.Off+ep.NewText.Size {
				return x86.Target{Addr: ep.NewText.Addr + newFile - ep.NewText.Off, Known: true}
			}
			return structuralLookup.target(addr)
		}
		if rp != nil {
			if t := structuralLookup.pointTarget(addr); t.Known {
				return t
			}
			if off, ok := rp.OldSecs.offsetOf(addr); ok {
				if newOff, ok := sourceMapper.project(off); ok {
					if newAddr, ok := rp.NewSecs.addrOf(newOff); ok {
						return x86.Target{Addr: newAddr, Known: true}
					}
				}
			}
		}
		return structuralLookup.target(addr)
	}
}

// newPointerOracle resolves absolute pointer targets, which is a different
// question from resolving an instruction displacement. A displacement names a
// place, so the byte-level equivalence projection is the best evidence. A
// pointer names a *function*, so identity evidence has to win: identical-code
// folding gives Chrome many byte-identical bodies, and a byte match will
// happily send a function pointer to the wrong twin. On .rela.dyn's 1.1
// million addends this ordering is the difference between 78.760% and 99.075%
// exact.
func newPointerOracle(ep equivalencePlan, structure predictionPlan, rp *relocPlan) func(uint64) x86.Target {
	return newOracleParts(ep, structure).pointer(rp)
}

func (o oracleParts) pointer(rp *relocPlan) func(uint64) x86.Target {
	structuralLookup, sourceMapper := o.lk, o.sm
	return func(addr uint64) x86.Target {
		if t := structuralLookup.pointTarget(addr); t.Known {
			return t
		}
		if t := structuralLookup.mapTarget(addr); t.Known {
			return t
		}
		if rp != nil {
			if off, ok := rp.OldSecs.offsetOf(addr); ok {
				if newOff, ok := sourceMapper.project(off); ok {
					if newAddr, ok := rp.NewSecs.addrOf(newOff); ok {
						return x86.Target{Addr: newAddr, Known: true}
					}
				}
			}
		}
		return structuralLookup.target(addr)
	}
}

// sparseWalkOffsets returns non-overlapping [start,end) spans within the new
// text section, anchored at every shift-range start and every reference-point
// destination that lands inside it.
func sparseWalkOffsets(structure predictionPlan, textLen uint64) [][2]uint64 {
	anchors := make([]uint64, 0, len(structure.Ranges)+len(structure.Points)+len(structure.Maps))
	add := func(addr uint64) {
		if addr < structure.NewAddr {
			return
		}
		if off := addr - structure.NewAddr; off < textLen {
			anchors = append(anchors, off)
		}
	}
	for _, ar := range structure.Ranges {
		add(ar.New)
	}
	for _, pt := range structure.Points {
		add(pt.New)
	}
	// The retained functions are shipped anyway, and their starts are
	// instruction boundaries too.
	for _, m := range structure.Maps {
		add(structure.NewAddr + m.Dst)
	}
	slices.Sort(anchors)
	anchors = slices.Compact(anchors)
	if len(anchors) == 0 || anchors[0] != 0 {
		anchors = append([]uint64{0}, anchors...)
	}
	spans := make([][2]uint64, 0, len(anchors))
	for i, a := range anchors {
		end := textLen
		if i+1 < len(anchors) {
			end = anchors[i+1]
		}
		if end > a {
			spans = append(spans, [2]uint64{a, end})
		}
	}
	return spans
}

func retargetEquivalencePrediction(out []byte, ep equivalencePlan, structure predictionPlan, lookup func(uint64) x86.Target) x86.Stats {
	retargetBody := func(stats *x86.Stats, body []byte, dstBase uint64) {
		x86.WalkReferences(body, 0, func(ref x86.Reference) {
			stats.Refs++
			fullStart := ep.NewText.Off + dstBase + uint64(ref.Start)
			fullField := ep.NewText.Off + dstBase + uint64(ref.Off)
			fullLast := ep.NewText.Off + dstBase + uint64(ref.Next-1)
			srcStart, startEq, startOK := ep.sourceAt(fullStart)
			srcField, fieldEq, fieldOK := ep.sourceAt(fullField)
			srcLast, lastEq, lastOK := ep.sourceAt(fullLast)
			if !startOK || !fieldOK || !lastOK || startEq != fieldEq || startEq != lastEq || srcField < srcStart || srcLast < srcField {
				stats.Unknown++
				return
			}
			oldTextEnd := ep.OldText.Off + ep.OldText.Size
			if srcStart < ep.OldText.Off || srcLast >= oldTextEnd {
				stats.Unknown++
				return
			}
			disp, ok := readDisplacement(body, ref)
			if !ok {
				stats.Unknown++
				return
			}
			oldNext := ep.OldText.Addr + srcLast + 1 - ep.OldText.Off
			target := lookup(uint64(int64(oldNext) + disp))
			if !target.Known {
				stats.Unknown++
				return
			}
			newNext := ep.NewText.Addr + dstBase + uint64(ref.Next)
			if !writeDisplacement(body, ref, int64(target.Addr)-int64(newNext)) {
				stats.NoFit++
			}
		})
	}
	if structure.Mode == planSparse {
		// A sparse map does not cover the section, so the walker needs its
		// spans from somewhere else. What it actually requires is instruction
		// boundaries, not function boundaries, and the plan already carries
		// 259,469 of them for free: a reference point's destination is a
		// branch or call target, which is by construction an instruction
		// start. Those and the shift-range starts together anchor the walk.
		//
		// Spans are taken in destination order and never overlap: retargeting
		// rewrites a field in place, so walking any byte twice would decode an
		// already-rewritten displacement.
		var stats x86.Stats
		for _, off := range sparseWalkOffsets(structure, uint64(len(out))) {
			retargetBody(&stats, out[off[0]:off[1]], off[0])
		}
		return stats
	}
	if len(structure.Maps) == 0 {
		var stats x86.Stats
		retargetBody(&stats, out, 0)
		return stats
	}
	// One function body per map, retargeted concurrently: each rewrites only
	// its own disjoint [Dst,Dst+DstSize) span and every lookup is a read, so
	// the mutated buffer and the summed stats match the serial loop.
	return parallelStats(len(structure.Maps), workers(), func(stats *x86.Stats, i int) {
		m := structure.Maps[i]
		if m.Dst > uint64(len(out)) || m.DstSize > uint64(len(out))-m.Dst {
			return
		}
		retargetBody(stats, out[m.Dst:m.Dst+m.DstSize], m.Dst)
	})
}

func (p combinedPlan) marshal() []byte {
	b := append([]byte(nil), combinedMagic[:]...)
	b = appendStream(b, p.Equivalences)
	b = appendStream(b, p.Structure)
	b = appendStream(b, p.Choices)
	// The later streams are optional so that plans predating each serialize
	// to exactly the same bytes and their measurements stay comparable: a
	// stream is written when it, or any stream after it, is present.
	tail := [][]byte{p.Reloc, p.EhFrame, p.RoData, p.Fields, p.GoTables, p.Dwarf}
	last := -1
	for i, s := range tail {
		if len(s) != 0 {
			last = i
		}
	}
	for _, s := range tail[:last+1] {
		b = appendStream(b, s)
	}
	return b
}

func unmarshalCombinedPlan(b []byte) (combinedPlan, error) {
	if len(b) < 4 || !bytes.Equal(b[:4], combinedMagic[:]) {
		return combinedPlan{}, errors.New("invalid combined plan magic")
	}
	r := &planReader{b: b[4:]}
	eq, structure, choices := r.stream(), r.stream(), r.stream()
	if r.err != nil {
		return combinedPlan{}, errors.New("invalid combined plan streams")
	}
	cp := combinedPlan{Equivalences: slices.Clone(eq.b), Structure: slices.Clone(structure.b), Choices: slices.Clone(choices.b)}
	if len(r.b) != 0 {
		reloc := r.stream()
		if r.err != nil {
			return combinedPlan{}, errors.New("invalid combined plan streams")
		}
		cp.Reloc = slices.Clone(reloc.b)
	}
	if len(r.b) != 0 {
		eh := r.stream()
		if r.err != nil {
			return combinedPlan{}, errors.New("invalid combined plan streams")
		}
		cp.EhFrame = slices.Clone(eh.b)
	}
	if len(r.b) != 0 {
		ro := r.stream()
		if r.err != nil {
			return combinedPlan{}, errors.New("invalid combined plan streams")
		}
		cp.RoData = slices.Clone(ro.b)
	}
	if len(r.b) != 0 {
		fields := r.stream()
		if r.err != nil {
			return combinedPlan{}, errors.New("invalid combined plan streams")
		}
		cp.Fields = slices.Clone(fields.b)
	}
	if len(r.b) != 0 {
		gt := r.stream()
		if r.err != nil {
			return combinedPlan{}, errors.New("invalid combined plan streams")
		}
		cp.GoTables = slices.Clone(gt.b)
	}
	if len(r.b) != 0 {
		dw := r.stream()
		if r.err != nil {
			return combinedPlan{}, errors.New("invalid combined plan streams")
		}
		cp.Dwarf = slices.Clone(dw.b)
	}
	if len(r.b) != 0 {
		return combinedPlan{}, errors.New("invalid combined plan streams")
	}
	return cp, nil
}

func predictCombined(old []byte, encoded []byte) ([]byte, combinedStats, error) {
	cp, err := unmarshalCombinedPlan(encoded)
	if err != nil {
		return nil, combinedStats{}, err
	}
	out, ep, copied, err := predictEquivalences(old, cp.Equivalences)
	if err != nil {
		return nil, combinedStats{}, err
	}
	structure, err := unmarshalPlanFile(cp.Structure, old, ep.OldText, goMapDeriver(old, cp.GoTables, ep.OldText, ep.NewText))
	if err != nil {
		return nil, combinedStats{}, err
	}
	if structure.TargetLen != ep.NewText.Size || structure.OldAddr != ep.OldText.Addr || structure.NewAddr != ep.NewText.Addr {
		return nil, combinedStats{}, errors.New("combined structural and equivalence plans describe different text sections")
	}
	stats := combinedStats{Equivalences: len(ep.Eqs), EquivalenceTextBytes: copied}
	// With no equivalences to copy .text from, the Go-table module's code
	// model is the base, as in the whole-image decoder.
	if len(cp.GoTables) != 0 && textEquivalences(ep) == 0 && !noGoText {
		whole := make([]byte, ep.NewLen)
		if _, err := delta.ApplyGoTables(old, cp.GoTables, whole, func(name string) bool { return name != ".text" }); err != nil {
			return nil, combinedStats{}, err
		}
		copy(out, whole[ep.NewText.Off:ep.NewText.Off+ep.NewText.Size])
	}
	stats.Relocation = retargetEquivalencePrediction(out, ep, structure, newImageOracle(ep, structure, nil))
	if len(cp.Choices) == 0 {
		return out, stats, nil
	}
	if len(cp.Choices) != (len(structure.Maps)+7)/8 {
		return nil, combinedStats{}, errors.New("combined choice stream has the wrong size")
	}
	oldText := old[ep.OldText.Off : ep.OldText.Off+ep.OldText.Size]
	structural, _, err := predict(oldText, cp.Structure, true, goMapDeriver(old, cp.GoTables, ep.OldText, ep.NewText))
	if err != nil {
		return nil, combinedStats{}, err
	}
	for i, m := range structure.Maps {
		if cp.Choices[i/8]&(1<<(i%8)) == 0 {
			continue
		}
		copy(out[m.Dst:m.Dst+m.DstSize], structural[m.Dst:m.Dst+m.DstSize])
		stats.SelectedFunctions++
		stats.SelectedBytes += int(m.DstSize)
	}
	return out, stats, nil
}

func wrongCount(a, b []byte) int {
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

// fieldCount prices a body as fields rather than bytes: one maximal run of
// mismatch, clipped to four bytes, is one wrong field.
func fieldCount(a, b []byte) int {
	n, run := 0, 0
	for i := range a {
		if a[i] == b[i] {
			run = 0
			continue
		}
		if run == 0 {
			n++
		}
		if run++; run == 4 {
			run = 0
		}
	}
	return n
}

// corrCount prices a body through the correction encoder that actually ships
// it, so a run of mismatch costs what its copies and literals cost.
func corrCount(a, b []byte) int {
	c, err := delta.EncodeCorrection(a, b)
	if err != nil {
		return len(b)
	}
	return len(c)
}

// selectStrategy names how per-function selection scores the two predictions.
var selectStrategy = "bytes"

func chooseStructuralFunctions(equivalencePred, structuralPred, target []byte, structure predictionPlan) ([]byte, int, int) {
	score := wrongCount
	switch selectStrategy {
	case "corr":
		score = corrCount
	case "fields":
		score = fieldCount
	}
	win := make([]bool, len(structure.Maps))
	const shard = 4096
	parallelFor((len(structure.Maps)+shard-1)/shard, func(s int) {
		for i := s * shard; i < min((s+1)*shard, len(structure.Maps)); i++ {
			m := structure.Maps[i]
			targetBody := target[m.Dst : m.Dst+m.DstSize]
			win[i] = score(structuralPred[m.Dst:m.Dst+m.DstSize], targetBody) < score(equivalencePred[m.Dst:m.Dst+m.DstSize], targetBody)
		}
	})
	choices := make([]byte, (len(structure.Maps)+7)/8)
	var functions, selectedBytes int
	for i, m := range structure.Maps {
		if !win[i] {
			continue
		}
		choices[i/8] |= 1 << (i % 8)
		functions++
		selectedBytes += int(m.DstSize)
	}
	return choices, functions, selectedBytes
}

// withEquivalences swaps a combined plan's equivalence stream, leaving every
// other stream byte-identical. Rungs are cached as finished plans, and only
// the equivalence encoding depends on whether the rung carries a function map.
func withEquivalences(planBytes, eq []byte) []byte {
	cp, err := unmarshalCombinedPlan(planBytes)
	if err != nil {
		return planBytes
	}
	cp.Equivalences = eq
	return cp.marshal()
}

// probeChunkChoices asks what a finer choice would be worth. One bit per
// function is the coarsest possible selection: where a function is half
// recompiled and half moved, the whole of it has to take the prediction that
// is better on balance. The oracle count below is an upper bound -- it assumes
// the selection itself is free -- so a small number here closes the question
// and a large one opens it.
func probeChunkChoices(equivalencePred, structuralPred, target []byte, structure predictionPlan) {
	var fnWrong int
	for _, m := range structure.Maps {
		eq := wrongCount(equivalencePred[m.Dst:m.Dst+m.DstSize], target[m.Dst:m.Dst+m.DstSize])
		st := wrongCount(structuralPred[m.Dst:m.Dst+m.DstSize], target[m.Dst:m.Dst+m.DstSize])
		fnWrong += min(eq, st)
	}
	for _, chunk := range []uint64{16, 64, 256} {
		var wrong, chunks int
		for _, m := range structure.Maps {
			for off := uint64(0); off < m.DstSize; off += chunk {
				end := min(off+chunk, m.DstSize)
				lo, hi := m.Dst+off, m.Dst+end
				eq := wrongCount(equivalencePred[lo:hi], target[lo:hi])
				st := wrongCount(structuralPred[lo:hi], target[lo:hi])
				wrong += min(eq, st)
				chunks++
			}
		}
		fmt.Fprintf(os.Stderr, "  probe %d-byte choices: %d wrong bytes vs %d per function, %d bits (%d functions)\n",
			chunk, wrong, fnWrong, chunks, len(structure.Maps))
	}
}

// probeEquivalenceSources asks whether the source residual is written against
// the right thing in turn. §9.16 predicts each covered source from the
// function map and ships the difference, and only 16.68% of those differences
// are zero -- but a difference is only expensive if it is unlike its
// neighbour. Consecutive runs inside one function share that function's
// internal shift, so their residuals should be nearly equal, and the column
// should be delta-coded rather than absolute. The same question is asked of
// the length column, which is the second largest.
func probeEquivalenceSources(p equivalencePlan, pred *srcPredictor) {
	var absolute, delta, deltaKeyed []byte
	var prevResid, prevBase int64
	for _, e := range p.Eqs {
		base, ok := pred.at(e.Dst)
		if !ok {
			continue
		}
		r := int64(e.Src) - int64(base)
		absolute = appendS(absolute, r)
		delta = appendS(delta, r-prevResid)
		// Keyed: reset the reference when the map's projection jumps, which is
		// the signal that this run belongs to a different function.
		if int64(base)-prevBase < 0 || int64(base)-prevBase > 1<<20 {
			deltaKeyed = appendS(deltaKeyed, r)
		} else {
			deltaKeyed = appendS(deltaKeyed, r-prevResid)
		}
		prevResid, prevBase = r, int64(base)
	}
	var lenAbs, lenDelta []byte
	var prevLen int64
	for _, e := range p.Eqs {
		lenAbs = appendU(lenAbs, e.N)
		lenDelta = appendS(lenDelta, int64(e.N)-prevLen)
		prevLen = int64(e.N)
	}
	fmt.Fprintf(os.Stderr, "  probe equivalence sources: residual absolute %d, delta %d, delta-keyed %d; length absolute %d, delta %d\n",
		xzSize(absolute), xzSize(delta), xzSize(deltaKeyed), xzSize(lenAbs), xzSize(lenDelta))

	// The structural question, as against the rebasing ones above: is a run's
	// length something the plan already says? Two candidates the decoder can
	// compute -- the rest of the mapped function the run starts in, and the
	// distance to where the next run begins.
	byExtent, byNext := 0, 0
	var extentResid, nextResid []byte
	for i, e := range p.Eqs {
		if rest, ok := pred.restOfFunction(e.Dst); ok {
			if rest == e.N {
				byExtent++
			}
			extentResid = appendS(extentResid, int64(e.N)-int64(rest))
		} else {
			extentResid = appendS(extentResid, int64(e.N))
		}
		gap := int64(e.N)
		if i+1 < len(p.Eqs) && p.Eqs[i+1].Dst > e.Dst {
			gap = int64(p.Eqs[i+1].Dst - e.Dst)
		}
		if gap == int64(e.N) {
			byNext++
		}
		nextResid = appendS(nextResid, int64(e.N)-gap)
	}
	fmt.Fprintf(os.Stderr, "  probe equivalence lengths: %d of %d run to the end of their function (residual xz %d); %d abut the next run (residual xz %d)\n",
		byExtent, len(p.Eqs), xzSize(extentResid), byNext, xzSize(nextResid))
}

// probeWhyChosen asks why the per-function selector prefers the function map
// over the equivalence copy. Each mapped function is classified by how many
// equivalence runs cover its destination and, for the run covering its start,
// whether the implied old offset lands inside the function's own old body.
func probeWhyChosen(planBytes []byte, structure predictionPlan) {
	cp, err := unmarshalCombinedPlan(planBytes)
	if err == nil && len(cp.Choices) != (len(structure.Maps)+7)/8 {
		err = errors.New("plan carries no per-function choice stream")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe whychosen FAILED: %v\n", err)
		return
	}
	ep, err := parseEquivalencePlan(cp.Equivalences)
	if err == nil {
		var pred *srcPredictor
		if len(structure.Maps) != 0 {
			pred = &srcPredictor{maps: structure.Maps, oldOff: ep.OldText.Off, newOff: ep.NewText.Off, newSize: ep.NewText.Size}
		}
		ep.Eqs, err = decodeEquivalences(ep, pred)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe whychosen FAILED: %v\n", err)
		return
	}
	// [chosen][pieces 0/1/2+][source none/same body/different body]
	var count, byteSum [2][3][3]int64
	for i, m := range structure.Maps {
		// Maps are in .text offsets; equivalences are whole-image offsets.
		lo, hi := ep.NewText.Off+m.Dst, ep.NewText.Off+m.Dst+m.DstSize
		j, _ := slices.BinarySearchFunc(ep.Eqs, lo, func(e equivalence, lo uint64) int {
			if e.Dst+e.N <= lo {
				return -1
			}
			return 1
		})
		pieces, widest := 0, uint64(0)
		var pick equivalence
		covered := false
		for k := j; k < len(ep.Eqs) && ep.Eqs[k].Dst < hi; k++ {
			e := ep.Eqs[k]
			s, t := max(e.Dst, lo), min(e.Dst+e.N, hi)
			if s >= t {
				continue
			}
			pieces++
			if !covered && e.Dst <= lo && lo < e.Dst+e.N {
				pick, covered = e, true
			} else if !covered && t-s > widest {
				pick, widest = e, t-s
			}
		}
		source := 0
		if pieces > 0 {
			implied := int64(pick.Src) + int64(lo) - int64(pick.Dst) - int64(ep.OldText.Off)
			source = 2
			if implied >= int64(m.Src) && implied < int64(m.Src+m.SrcSize) {
				source = 1
			}
		}
		chosen := 0
		if cp.Choices[i/8]&(1<<(i%8)) != 0 {
			chosen = 1
		}
		count[chosen][min(pieces, 2)][source]++
		byteSum[chosen][min(pieces, 2)][source] += int64(m.DstSize)
	}
	pieceName := [3]string{"0 (uncovered)", "1", "2+"}
	sourceName := [3]string{"none", "same body", "different body"}
	fmt.Fprintf(os.Stderr, "  probe whychosen: %d mapped functions\n", len(structure.Maps))
	fmt.Fprintf(os.Stderr, "    %-13s %-15s %12s %14s %12s %14s\n", "pieces", "source", "chosen", "chosen B", "not-chosen", "not-chosen B")
	for p := range 3 {
		for s := range 3 {
			if count[0][p][s] == 0 && count[1][p][s] == 0 {
				continue
			}
			fmt.Fprintf(os.Stderr, "    %-13s %-15s %12d %14d %12d %14d\n",
				pieceName[p], sourceName[s], count[1][p][s], byteSum[1][p][s], count[0][p][s], byteSum[0][p][s])
		}
	}
	var tc, tb [2]int64
	for c := range 2 {
		for p := range 3 {
			for s := range 3 {
				tc[c] += count[c][p][s]
				tb[c] += byteSum[c][p][s]
			}
		}
	}
	fmt.Fprintf(os.Stderr, "    %-13s %-15s %12d %14d %12d %14d\n", "total", "", tc[1], tb[1], tc[0], tb[0])
	fmt.Fprintf(os.Stderr, "  probe whychosen residual (chosen, pieces==1, same body): %d functions / %d bytes\n",
		count[1][1][1], byteSum[1][1][1])
}
