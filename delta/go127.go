package delta

import (
	"fmt"
	"math/bits"

	"github.com/wjordan/presage/delta/gobin"
	"github.com/wjordan/presage/delta/x86"
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
	tf             byte // the effective transform, lowered from the caller's cap
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
	if tf, err = layoutTransform(ob, nb, tf); err != nil {
		return nil, err
	}
	g.tf = tf
	g.m = matchFuncs(ob, nb)

	dmaps, shifts := buildMaps(ob, nb, g.m)
	var segs []segMap
	if tf >= TransformGoSegmap {
		// scored against the maps alone: the overrides and the pieces
		// themselves are not known yet
		pm := &mapper{src: ob, dst: nb, m: g.m, srcToDst: g.m.OldToNew, dstToSrc: g.m.NewToOld, dataMaps: dmaps, shifts: shifts}
		segs = buildSegMaps(ob, nb, g.m, pm, tf)
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
//	          a bounded local window cannot follow it (docs/go-module-design.md 2.4)
//	stage 2   a positional correction of the predicted whole file
//
// plus the BLAKE3 of the predicted file, so that an encoder and a decoder
// that disagree say so instead of producing a wrong binary quietly. The
// four streams are compressed as one frame: a frame per stream would cost a
// 32-byte hash and a table entry each and buys nothing -- compressed
// separately they come to within 0.1 % of the same total.
func encodeGoAMD64(old, new []byte, tf byte, o Options, st *Stats) ([]byte, byte, error) {
	g, plan, err := goAnalyse(old, new, tf, st)
	if err != nil {
		return nil, 0, err
	}
	s2, err := encodeCorrection(g.pred, new, g.tf >= TransformGoSegmap)
	if err != nil {
		return nil, 0, err
	}
	st.Stage2 = len(s2)
	w := &wbuf{b: plan}
	sum := hashOf(g.pred)
	w.raw(sum[:])
	w.raw(s2)
	return w.b, g.tf, nil
}

// goAnalyse runs the transform to its prediction and serialises what the
// decoder needs to reproduce it: the layout, then the stage-1a and 1b
// streams, each preceded by the length of the table it rebuilds. The
// predicted 1b blobs are padded to the new tables' lengths but never
// truncated, so a release that shrank a table leaves the prediction longer
// than the truth; the decoder is told what to expect.
func goAnalyse(old, new []byte, tf byte, st *Stats) (*goPred, []byte, error) {
	g, err := predictGoAMD64(old, new, tf)
	if err != nil {
		return nil, nil, err
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
	st.Layout, st.Stage1a, st.Stage1b = len(g.layRaw), len(g.s1a), len(g.s1b)
	w := &wbuf{}
	w.bytes(g.layRaw)
	w.u(uint64(len(g.s1aNew)))
	w.bytes(g.s1a)
	w.u(uint64(len(g.s1bNew)))
	w.bytes(g.s1b)
	return g, w.b, nil
}

// GoAnalyse is the Go transform as a module for a codec built on top of
// this package: it returns the plan a decoder expands with GoPredict, the
// prediction it yields, and the transform's statistics. A binary the
// transform declines is an unsupported input (IsUnsupported).
func GoAnalyse(old, new []byte, st *Stats) (plan, pred []byte, err error) {
	if st == nil {
		st = &Stats{}
	}
	g, plan, err := goAnalyse(old, new, maxTransform, st)
	if err != nil {
		return nil, nil, err
	}
	return plan, g.pred, nil
}

// GoPredict expands a GoAnalyse plan against the old binary. maxLen bounds
// the tables the plan may ask for, so a corrupt plan cannot become an
// allocation; it is the target's declared size.
func GoPredict(old, plan []byte, maxLen int64) ([]byte, error) {
	g, err := GoExpand(old, plan, maxLen)
	if err != nil {
		return nil, err
	}
	return g.Pred, nil
}

// IsUnsupported reports whether err is the transform declining an input,
// which is never a failure: the caller codes the bytes another way.
func IsUnsupported(err error) bool { return isUnsupported(err) }

func applyGoAMD64(old, body []byte, h *Header) ([]byte, error) {
	g, rest, err := goPredict(old, body, h.Transform, h.NewSize)
	if err != nil {
		return nil, err
	}
	pred := g.pred
	r := &rbuf{b: rest}
	var sum Hash
	copy(sum[:], r.take(32))
	if r.err != nil {
		return nil, r.err
	}
	if hashOf(pred) != sum {
		return nil, fmt.Errorf("delta: the predicted base does not match the encoder's; " +
			"encoder and decoder disagree")
	}
	if err := applyCorrection(pred, r.b, h.Transform >= TransformGoSegmap); err != nil {
		return nil, err
	}
	return pred, nil
}

// goPredict reads a goAnalyse plan from the front of body and returns the
// decoder's state, its prediction in pred, and what follows the plan.
func goPredict(old, body []byte, tf byte, maxLen int64) (g *goPred, rest []byte, err error) {
	ob, err := gobin.Parse(old)
	if err != nil {
		return nil, nil, fmt.Errorf("delta: this patch needs a Go binary the codec understands: %w", err)
	}
	// The same lowering the encoder applied, from the same input: the
	// reference names the release, and the release fixes the transform a
	// pair on it is written at.
	if tf, err = layoutTransform(ob, ob, tf); err != nil {
		return nil, nil, err
	}
	r := &rbuf{b: body}
	layRaw := r.bytes()
	s1aLen := r.un(uint64(maxLen), "stage 1a table length")
	s1a := r.bytes()
	s1bLen := r.un(uint64(maxLen), "stage 1b table length")
	s1b := r.bytes()
	if r.err != nil {
		return nil, nil, r.err
	}
	skel, m, err := skeletonFrom(ob, layRaw, tf)
	if err != nil {
		return nil, nil, err
	}
	lay, err := decodeLayout(layRaw, ob, tf)
	if err != nil {
		return nil, nil, err
	}
	s1aNew, err := plainPatch(stage1aBlobs(ob), s1a, int64(s1aLen))
	if err != nil {
		return nil, nil, err
	}
	if err := fillTables(skel, stage1aRanges(skel.Pcln), s1aNew); err != nil {
		return nil, nil, err
	}
	mp := newMapper(ob, skel, m, lay)
	bp := predictBlobs(ob, skel, m, mp)
	s1bNew, err := plainPatch(bp.concat(), s1b, int64(s1bLen))
	if err != nil {
		return nil, nil, err
	}
	if err := fillTables(skel, stage1bRanges(skel.Pcln), s1bNew); err != nil {
		return nil, nil, err
	}
	mp.blobs = bp
	g = &goPred{ob: ob, skel: skel, m2: m, lay: lay, mp: mp}
	g.pred = predictWhole(ob, skel, lay, mp, nil)
	return g, r.b, nil
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
	mp := &mapper{
		src: old, dst: skel, m: m,
		srcToDst: m.OldToNew, dstToSrc: m.NewToOld,
		dataMaps: l.DataMaps, shifts: l.Shifts, overrides: overrideMap(l.Overrides),
	}
	mp.segs, mp.segLocal = segsByIdx(l.Segs, old, skel, m)
	return mp
}

// defaultLayout is the release the codec was pinned to before layouts became
// data. A pair on it encodes exactly as it did then: no layout id on the
// wire and transform 3, so every patch a decoder could read before this
// work it can still read.
const defaultLayout = gobin.LayoutGo127

// layoutTransform settles the transform a pair is written at, from the two
// images' layouts and the caller's cap. Both sides derive it from what they
// have -- the encoder from the pair, the decoder from the reference twice,
// which the same-layout rule makes the same answer.
func layoutTransform(ob, nb *gobin.Bin, cap byte) (byte, error) {
	if ob.Lay.ID != nb.Lay.ID {
		// Cross-layout prediction is a different problem from the one the
		// module solves, and this is also the cheap decline the encoder
		// wants: two build strings, milliseconds (docs 2.9).
		return 0, unsupported("old is %s and new is %s: the module predicts within one release", ob.Lay.Ver, nb.Lay.Ver)
	}
	if ob.Lay.ID == defaultLayout {
		return min(cap, TransformGoFar), nil
	}
	if cap < TransformGoLayout {
		return 0, unsupported("%s needs transform %d, capped at %d", ob.Lay.Ver, TransformGoLayout, cap)
	}
	return cap, nil
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
