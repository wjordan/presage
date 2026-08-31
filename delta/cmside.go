package delta

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
func CMColumnarSides(pred, gaps, lens []byte) ([]*CMSide, error) {
	sides := make([]*CMSide, ColumnarBuckets)
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
		for k := 0; k < int(n); k++ {
			s.Pred = append(s.Pred, pred[at+k])
			s.Sel = append(s.Sel, byte(min(k, CMSelMax-1)))
		}
		at += int(n)
	}
	if len(l.b) != 0 {
		return nil, wrapCorrupt("trailing columnar lengths")
	}
	return sides, nil
}
