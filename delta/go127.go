package delta

import (
	"fmt"
	"math/bits"

	"github.com/wjordan/go-binsync/delta/gobin"
	"github.com/wjordan/go-binsync/delta/x86"
)

// goPred is everything the prediction was built from. It exists so that
// encodeGoAMD64 and the measurement hook in measure.go run the same code:
// the prediction the correction is measured against must be the one the
// decoder produces.
type goPred struct {
	ob, nb, skel   *gobin.Bin
	m, m2          *match
	lay            *layout
	mp             *mapper
	layRaw         []byte
	s1a, s1b       []byte
	s1aNew, s1bNew []byte
	pred           []byte
	xst            x86.Stats
}

// predictGoAMD64 runs the transform up to and including the prediction. tf
// is the transform being written: transform 1 has no segment map, and the
// encoder must then predict without one, since that is what its decoder does.
func predictGoAMD64(old, new []byte, tf byte) (*goPred, error) {
	g := &goPred{}
	var err error
	if g.ob, err = gobin.Parse(old); err != nil {
		return nil, asUnsupported("old", err)
	}
	if g.nb, err = gobin.Parse(new); err != nil {
		return nil, asUnsupported("new", err)
	}
	ob, nb := g.ob, g.nb
	g.m = matchFuncs(ob, nb)

	dmaps, shifts := buildMaps(ob, nb, g.m)
	var segs []segMap
	if tf >= TransformGoSegmap {
		segs = buildSegMaps(ob, nb, g.m)
	}
	// deriveOverrides runs with the maps installed, so the override table
	// does not spend two varints re-fixing a target a map already places.
	ov := deriveOverrides(ob, nb, g.m, dmaps, shifts, segs)
	g.layRaw = buildLayout(ob, nb, g.m, dmaps, shifts, ov, segs, tf).encode(ob, tf)

	// From here the encoder runs the decoder's code on the decoder's inputs:
	// the layout as it will be decoded, and a skeleton built from it. That
	// is what guarantees the prediction the correction is measured against
	// is the prediction the decoder will produce.
	g.skel, g.m2, err = skeletonFrom(ob, g.layRaw, tf)
	if err != nil {
		return nil, err
	}
	g.lay, _ = decodeLayout(g.layRaw, ob, tf)

	g.s1aNew = stage1aBlobs(nb)
	g.s1a = plainDiff(stage1aBlobs(ob), g.s1aNew)
	if err := fillTables(g.skel, stage1aRanges(g.skel.Pcln), g.s1aNew); err != nil {
		return nil, err
	}

	g.mp = newMapper(ob, g.skel, g.m2, g.lay)
	bp := predictBlobs(ob, g.skel, g.m2, g.mp)
	g.s1bNew = stage1bBlobs(nb)
	g.s1b = plainDiff(bp.concat(), g.s1bNew)
	if err := fillTables(g.skel, stage1bRanges(g.skel.Pcln), g.s1bNew); err != nil {
		return nil, err
	}
	g.mp.blobs = bp

	g.pred = predictWhole(ob, g.skel, g.lay, g.mp, &g.xst)
	return g, nil
}

// The Go-aware transform. The patch body is four streams:
//
//	layout    the new file's shape: section table, moduledata values,
//	          function order and sizes, pclntab table offsets, the content
//	          maps and the shift tables (delta/layout.go)
//	stage 1a  a plain delta of funcnametab+filetab, which change length
//	stage 1b  a plain delta of the predicted cutab+pctab+go:func.* against
//	          the real ones -- the residual is shifted, not positional, so
//	          a bounded local window cannot follow it (docs/DESIGN.md 3.4)
//	stage 2   a positional correction of the predicted whole file
//
// plus the BLAKE3 of the predicted file, so that an encoder and a decoder
// that disagree say so instead of producing a wrong binary quietly. The
// four streams are compressed as one frame: a frame per stream would cost a
// 32-byte hash and a table entry each and buys nothing -- compressed
// separately they come to within 0.1 % of the same total.
func encodeGoAMD64(old, new []byte, tf byte, o Options, st *Stats) ([]byte, error) {
	g, err := predictGoAMD64(old, new, tf)
	if err != nil {
		return nil, err
	}
	st.Funcs = len(g.nb.Funcs)
	st.Matched = g.m.Exact + g.m.Norm + g.m.Content
	st.NewFuncs = g.m.Unmatched
	st.PredictErr = diffBytes(g.pred, new)
	st.Notes = append(st.Notes, fmt.Sprintf("%d insns, %d undecodable bytes, %d unrelocatable refs",
		g.xst.Insns, g.xst.Fails, g.xst.Unknown+g.xst.NoFit))
	if len(g.lay.Segs) > 0 {
		pieces, w := 0, &wbuf{}
		for _, sm := range g.lay.Segs {
			pieces += len(sm.Segs)
		}
		encodeSegMaps(w, g.lay.Segs)
		st.Notes = append(st.Notes, fmt.Sprintf("%d segment maps, %d pieces, %d B of layout",
			len(g.lay.Segs), pieces, len(w.b)))
	}
	if g.lay.NPcFresh > 0 {
		fresh := 0
		for _, b := range g.lay.PcFresh {
			fresh += bits.OnesCount8(b)
		}
		st.Notes = append(st.Notes, fmt.Sprintf("%d invented pctab slots, %d fresh, %d gaps",
			g.lay.NPcFresh, fresh, len(g.lay.PcGaps)))
	}
	s2, err := encodeCorrection(g.pred, new)
	if err != nil {
		return nil, err
	}

	st.Layout, st.Stage1a, st.Stage1b, st.Stage2 = len(g.layRaw), len(g.s1a), len(g.s1b), len(s2)
	w := &wbuf{}
	w.bytes(g.layRaw)
	w.u(uint64(len(g.s1aNew)))
	w.bytes(g.s1a)
	// The predicted 1b blobs are padded to the new tables' lengths but never
	// truncated, so a release that shrank a table leaves the prediction
	// longer than the truth; the decoder is told what to expect.
	w.u(uint64(len(g.s1bNew)))
	w.bytes(g.s1b)
	sum := hashOf(g.pred)
	w.raw(sum[:])
	w.raw(s2)
	return w.b, nil
}

func applyGoAMD64(old, body []byte, h *Header) ([]byte, error) {
	ob, err := gobin.Parse(old)
	if err != nil {
		return nil, fmt.Errorf("delta: this patch needs a Go binary the codec understands: %w", err)
	}
	r := &rbuf{b: body}
	layRaw := r.bytes()
	s1aLen := r.un(uint64(h.NewSize), "stage 1a table length")
	s1a := r.bytes()
	s1bLen := r.un(uint64(h.NewSize), "stage 1b table length")
	s1b := r.bytes()
	var sum Hash
	copy(sum[:], r.take(32))
	if r.err != nil {
		return nil, r.err
	}
	s2 := r.b

	skel, m, err := skeletonFrom(ob, layRaw, h.Transform)
	if err != nil {
		return nil, err
	}
	lay, err := decodeLayout(layRaw, ob, h.Transform)
	if err != nil {
		return nil, err
	}
	s1aNew, err := plainPatch(stage1aBlobs(ob), s1a, int64(s1aLen))
	if err != nil {
		return nil, err
	}
	if err := fillTables(skel, stage1aRanges(skel.Pcln), s1aNew); err != nil {
		return nil, err
	}

	mp := newMapper(ob, skel, m, lay)
	bp := predictBlobs(ob, skel, m, mp)
	s1bNew, err := plainPatch(bp.concat(), s1b, int64(s1bLen))
	if err != nil {
		return nil, err
	}
	if err := fillTables(skel, stage1bRanges(skel.Pcln), s1bNew); err != nil {
		return nil, err
	}
	mp.blobs = bp

	pred := predictWhole(ob, skel, lay, mp, nil)
	if hashOf(pred) != sum {
		return nil, fmt.Errorf("delta: the predicted base does not match the encoder's; " +
			"encoder and decoder disagree, fetch the blob")
	}
	if err := applyCorrection(pred, s2); err != nil {
		return nil, err
	}
	return pred, nil
}

// skeletonFrom decodes a layout and builds the decoder's view of the new
// binary from it. Both sides call it, on the same bytes.
func skeletonFrom(old *gobin.Bin, layRaw []byte, tf byte) (*gobin.Bin, *match, error) {
	l, err := decodeLayout(layRaw, old, tf)
	if err != nil {
		return nil, nil, err
	}
	return skeleton(old, l)
}

func newMapper(old, skel *gobin.Bin, m *match, l *layout) *mapper {
	return &mapper{
		src: old, dst: skel, m: m,
		srcToDst: m.OldToNew, dstToSrc: m.NewToOld,
		dataMaps: l.DataMaps, shifts: l.Shifts, overrides: overrideMap(l.Overrides),
		segs: segsByIdx(l.Segs),
	}
}

// asUnsupported turns a gobin rejection into the codec's own sentinel, so
// that Encode falls back to the plain codec instead of failing.
func asUnsupported(which string, err error) error {
	var u *gobin.Unsupported
	if errorAs(err, &u) {
		return unsupported("%s binary: %s", which, u.Reason)
	}
	return unsupported("%s binary: %v", which, err)
}

func diffBytes(a, b []byte) int {
	n := 0
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			n++
		}
	}
	return n + max(len(a), len(b)) - min(len(a), len(b))
}
