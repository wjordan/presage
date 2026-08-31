package main

import (
	"encoding/binary"
	"errors"
	"slices"
	"sync"
)

// The production correction codec is a matcher: it searches the prediction for
// the target's bytes. On a whole 291 MB image that is the wrong instrument.
// The prediction is 99.07% correct, so what is left is half a million short
// runs at unpredictable offsets, and a matcher spends its budget describing
// where each run is in a format built to describe long copies.
//
// The columnar form encodes the same edit list as three independent streams --
// the gap since the last edit, the length of the edit, and the replacement
// bytes -- so the compressor sees three columns of like-valued data instead of
// one interleaved stream. On .text that is worth 97,576 XZ bytes.
//
// The replacement bytes are then split again, by the length of the run they
// belong to. Nine tenths of .text's wrong runs are four bytes or shorter, and
// a four-byte run is usually a displacement field whose high bytes are nearly
// constant while its low bytes are noise; a one-byte run is a lone changed
// opcode. Interleaving them puts noise next to structure in the same stream.
// The decoder reads each run's length from the Lens column before it needs the
// bytes, so it always knows which bucket to draw from. Worth a further 60,488
// on .text.
const correctionBuckets = 5

type columnarCorrection struct {
	Gaps  []byte
	Lens  []byte
	Bytes [correctionBuckets][]byte
	// The displacement columns of §14, empty unless the encoder was given a
	// dispContext. Tags carries one class byte per field pulled out of the
	// last bucket; Idx, Loc and Far carry that class's value. See
	// dispfield.go.
	Tags, Idx, Loc, Far []byte
}

// streams lists every column in the order a patch would ship them.
func (c columnarCorrection) streams() [][]byte {
	s := append([][]byte{c.Gaps, c.Lens}, c.Bytes[:]...)
	if len(c.Tags) == 0 {
		// An empty column still costs an xz header, so a correction with no
		// displacement fields must not ship four of them: the shipped format's
		// price has to stay exactly what it was.
		return s
	}
	return append(s, c.Tags, c.Idx, c.Loc, c.Far)
}

// bucketOf groups runs by length, with everything from correctionBuckets bytes
// upwards sharing the last one.
func bucketOf(n int) int { return min(n, correctionBuckets) - 1 }

func encodeColumnar(pred, target []byte) (columnarCorrection, error) {
	return encodeColumnarDisp(pred, target, nil)
}

// encodeColumnarDisp is encodeColumnar plus, when d is non-nil, §14's
// displacement column: every PC-relative field lying wholly inside a long run
// is zeroed in the replacement bytes and shipped in a column of its own.
func encodeColumnarDisp(pred, target []byte, d *dispContext) (columnarCorrection, error) {
	if len(pred) != len(target) {
		return columnarCorrection{}, errors.New("columnar correction needs equal lengths")
	}
	var c columnarCorrection
	var runs []dispRun
	last := correctionBuckets - 1
	prevEnd := 0
	for i := 0; i < len(target); {
		if pred[i] == target[i] {
			i++
			continue
		}
		j := i
		for j < len(target) && pred[j] != target[j] {
			j++
		}
		c.Gaps = binary.AppendUvarint(c.Gaps, uint64(i-prevEnd))
		c.Lens = binary.AppendUvarint(c.Lens, uint64(j-i))
		b := bucketOf(j - i)
		if b == last && d != nil {
			runs = append(runs, dispRun{i, j, len(c.Bytes[last])})
		}
		c.Bytes[b] = append(c.Bytes[b], target[i:j]...)
		prevEnd, i = j, j
	}
	if d == nil {
		return c, nil
	}
	var prevIdx, prevFar int64
	for _, s := range d.sites(target, runs) {
		v := readDisp(target[s.off : s.off+s.n])
		abs := uint64(int64(s.next) + v)
		cl := d.class(s, abs)
		c.Tags = append(c.Tags, byte(cl))
		switch cl {
		case dispHit:
			i, _ := slices.BinarySearch(d.starts, abs)
			c.Idx = appendS(c.Idx, int64(i)-prevIdx)
			prevIdx = int64(i)
		case dispLocal:
			c.Loc = appendS(c.Loc, v)
		default:
			c.Far = appendS(c.Far, int64(abs)-prevFar)
			prevFar = int64(abs)
		}
		clear(c.Bytes[last][s.at : s.at+s.n])
	}
	return c, nil
}

func (c columnarCorrection) apply(buf []byte) error {
	return c.applyDisp(buf, nil)
}

// applyDisp places the replacement bytes and then, when the correction carries
// displacement columns, walks the repaired buffer and fills the zeroed fields
// back in. The walk happens after every run is placed, because an instruction
// may span a run boundary and only then are all its bytes present.
func (c columnarCorrection) applyDisp(buf []byte, d *dispContext) error {
	gaps, lens := &planReader{b: c.Gaps}, &planReader{b: c.Lens}
	src, at := c.Bytes, 0
	var runs []dispRun
	last := correctionBuckets - 1
	placed := 0
	for !gaps.done() {
		gap, n := gaps.u(), lens.u()
		if gaps.err != nil || lens.err != nil || n == 0 {
			return errors.New("invalid columnar correction stream")
		}
		if gap > uint64(len(buf)-at) {
			return errors.New("columnar correction gap runs past the buffer")
		}
		at += int(gap)
		b := bucketOf(int(n))
		if n > uint64(len(buf)-at) || n > uint64(len(src[b])) {
			return errors.New("columnar correction run runs past the buffer")
		}
		copy(buf[at:at+int(n)], src[b][:n])
		if b == last && d != nil {
			runs = append(runs, dispRun{at, at + int(n), placed})
			placed += int(n)
		}
		src[b], at = src[b][n:], at+int(n)
	}
	if !lens.done() {
		return errors.New("trailing columnar correction data")
	}
	for _, s := range src {
		if len(s) != 0 {
			return errors.New("trailing columnar correction bytes")
		}
	}
	if d == nil {
		if len(c.Tags) != 0 {
			return errors.New("columnar correction carries displacement columns with no context")
		}
		return nil
	}
	idx, loc, far := &planReader{b: c.Idx}, &planReader{b: c.Loc}, &planReader{b: c.Far}
	var prevIdx, prevFar int64
	tags := c.Tags
	for _, s := range d.sites(buf, runs) {
		if len(tags) == 0 {
			return errors.New("columnar correction ran out of displacement tags")
		}
		cl := int(tags[0])
		tags = tags[1:]
		var abs uint64
		var v int64
		switch cl {
		case dispHit:
			prevIdx += idx.s()
			if prevIdx < 0 || prevIdx >= int64(len(d.starts)) {
				return errors.New("displacement index outside the function-start domain")
			}
			abs = d.starts[prevIdx]
			v = int64(abs) - int64(s.next)
		case dispLocal:
			v = loc.s()
		case dispFar:
			prevFar += far.s()
			v = prevFar - int64(s.next)
		default:
			return errors.New("invalid displacement class")
		}
		if idx.err != nil || loc.err != nil || far.err != nil {
			return errors.New("invalid displacement column stream")
		}
		writeDisp(buf[s.off:s.off+s.n], v)
	}
	if len(tags) != 0 || !idx.done() || !loc.done() || !far.done() {
		return errors.New("trailing displacement column data")
	}
	return nil
}

// columnarXZ reports what the columnar form costs, or 0 if it does not
// round-trip -- which must never happen, and is checked rather than assumed.
func columnarXZ(pred, target []byte, d *dispContext) (int, error) {
	c, err := encodeColumnarDisp(pred, target, d)
	if err != nil {
		return 0, err
	}
	check := append([]byte(nil), pred...)
	if err := c.applyDisp(check, d); err != nil {
		return 0, err
	}
	for i := range check {
		if check[i] != target[i] {
			return 0, errors.New("columnar correction replay did not reproduce target")
		}
	}
	total := 0
	for _, n := range xzSizes(c.streams()...) {
		total += n
	}
	if d != nil {
		reportDispColumns(c)
	}
	return total, nil
}

// correctionCuts splits the image where the codec choice can change. Only the
// large sections get their own piece: a cut costs an xz header and the context
// the compressor would have carried across it, so it has to be paid for by a
// region big enough to have its own character.
func correctionCuts(secs map[string]section, n int) [][2]int {
	const minSection = 1 << 20
	var bounds []int
	for _, s := range secs {
		if s.Size < minSection || s.Off+s.Size > uint64(n) {
			continue
		}
		bounds = append(bounds, int(s.Off), int(s.Off+s.Size))
	}
	bounds = append(bounds, 0, n)
	slices.Sort(bounds)
	bounds = slices.Compact(bounds)
	var cuts [][2]int
	for i := 0; i+1 < len(bounds); i++ {
		cuts = append(cuts, [2]int{bounds[i], bounds[i+1]})
	}
	return cuts
}

// bestCorrectionXZ measures the correction the way a patch would actually ship
// it: split at the large section boundaries and let each piece pick its own
// codec. The two codecs disagree by region -- .text is half a million short
// scattered runs, which the columnar form wins by 97,576 XZ, while
// .data.rel.ro is repetitive enough for the matcher to win -- and over the
// whole image the majority swamps the minority. Splitting keeps both.
// The cuts are independent, so they are measured concurrently; the xz bound in
// xz.go keeps the process count sane. Results are collected in cut order so the
// pick string still reads left to right across the image.
func bestCorrectionXZ(pred, target []byte, secs map[string]section, d *dispContext) (int, string, error) {
	cuts := correctionCuts(secs, len(target))
	type result struct {
		size int
		pick byte
		err  error
	}
	out := make([]result, len(cuts))
	var wg sync.WaitGroup
	for i, c := range cuts {
		if c[1] <= c[0] {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, t := pred[c[0]:c[1]], target[c[0]:c[1]]
			corr, err := smallestCorrection(p, t)
			if err != nil {
				out[i].err = err
				return
			}
			var lz, col int
			var colErr error
			var inner sync.WaitGroup
			inner.Add(2)
			go func() { defer inner.Done(); lz = xzSize(corr) }()
			go func() { defer inner.Done(); col, colErr = columnarXZ(p, t, d.restrict(c[0], c[1])) }()
			inner.Wait()
			if colErr != nil {
				out[i].err = colErr
				return
			}
			if col < lz {
				out[i] = result{col, 'c', nil}
			} else {
				out[i] = result{lz, 'm', nil}
			}
		}()
	}
	wg.Wait()
	total, pick := 0, make([]byte, 0, len(cuts))
	for _, r := range out {
		if r.err != nil {
			return 0, "", r.err
		}
		if r.pick == 0 {
			continue
		}
		total, pick = total+r.size, append(pick, r.pick)
	}
	return total, string(pick), nil
}
