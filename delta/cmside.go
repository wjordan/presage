package delta

import "slices"

// Side information for the columnar correction's byte buckets.
//
// The gaps and lens columns come first in the stream table, so by the time a
// bucket is decoded both sides already know where every one of its bytes
// lands in the buffer being repaired -- and that buffer still holds the
// prediction. This is the whole conditioning the CM coder needs, and it is
// derived here once, by the same code on both sides, so encoder and decoder
// cannot disagree about it.

// CMColumnarSides returns one CMSide per byte bucket of a columnar
// correction, given the prediction the correction is applied over and the
// correction's gaps and lens columns. Buckets with no bytes get nil.
//
// When d is non-nil each bucket also gets the field context: the instruction
// class and offset of the prediction byte under every coded byte, walked out
// of d's bodies (dispfield.go). d is the piece's restricted context on both
// sides, so the classification is the same one at both ends.
func CMColumnarSides(pred, gaps, lens []byte, d *DispContext) ([]*CMSide, error) {
	sides := make([]*CMSide, ColumnarBuckets)
	var runs []dispRun
	var bucket []int
	g, l := &rbuf{b: gaps}, &rbuf{b: lens}
	at := 0
	for len(g.b) > 0 {
		gap := g.un(uint64(len(pred)-at), "columnar gap")
		n := l.un(uint64(len(pred)), "columnar run")
		if g.err != nil {
			return nil, g.err
		}
		if l.err != nil {
			return nil, l.err
		}
		at += int(gap)
		if n == 0 || n > uint64(len(pred)-at) {
			return nil, wrapCorrupt("columnar run of %d at %d", n, at)
		}
		b := bucketOf(int(n))
		s := sides[b]
		if s == nil {
			s = &CMSide{}
			sides[b] = s
		}
		if d != nil {
			runs = append(runs, dispRun{at, at + int(n), len(s.Pred)})
			bucket = append(bucket, b)
		}
		// Whole runs at a time: the prediction bytes are a contiguous slice
		// and the selector is the same short ramp followed by a constant for
		// every run, so both go in as block copies rather than 1.5 M
		// single-byte appends.
		s.Pred = append(s.Pred, pred[at:at+int(n)]...)
		k := min(int(n), CMSelMax)
		s.Sel = append(s.Sel, selRamp[:k]...)
		if rest := int(n) - k; rest > 0 {
			s.Sel = slices.Grow(s.Sel, rest)
			base := len(s.Sel)
			s.Sel = s.Sel[:base+rest]
			tail := s.Sel[base:]
			tail[0] = CMSelMax - 1
			for m := 1; m < len(tail); m *= 2 {
				copy(tail[m:], tail[:m])
			}
		}
		at += int(n)
	}
	if len(l.b) != 0 {
		return nil, wrapCorrupt("trailing columnar lengths")
	}
	if d == nil || !cmWorthClassifying(sides) {
		return sides, nil
	}
	for _, s := range sides {
		if s != nil {
			s.Cls = make([]byte, len(s.Pred))
			s.Off = make([]byte, len(s.Pred))
		}
	}
	d.classify(pred, runs, func(r, pos int, cls, off byte) {
		s := sides[bucket[r]]
		k := runs[r].at + pos - runs[r].start
		s.Cls[k], s.Off[k] = cls, off
	})
	return sides, nil
}

// cmWorthClassifying skips the walk for a piece no bucket of which the CM
// coder will be offered. It reads only the bucket lengths, which both sides
// have before either decides anything, so gating on it cannot make the two
// classify differently.
func cmWorthClassifying(sides []*CMSide) bool {
	for _, s := range sides {
		if s != nil && len(s.Pred) >= CMMinStream {
			return true
		}
	}
	return false
}

// CMMinStream is the smallest stream the CM coder is worth offering. It runs
// at about 1 MB/s each way; below a few kilobytes its adaptive models have not
// paid for themselves and the attempt is only encode time.
const CMMinStream = 4 << 10

// selRamp is the head of every run's selector column: byte k of a run is
// min(k, CMSelMax-1), so the first CMSelMax bytes count up and the rest are
// the last value.
var selRamp = func() [CMSelMax]byte {
	var r [CMSelMax]byte
	for i := range r {
		r[i] = byte(i)
	}
	return r
}()
