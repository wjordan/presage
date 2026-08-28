package delta

// This file exists for measurement. bench/goattr attributes the residual --
// the bytes the prediction gets wrong -- to the layer that would have to fix
// them, and to do that it needs the prediction itself and the tables behind
// it. Nothing in Encode or Apply calls anything here, and no patch byte
// depends on it.

// Prediction is the Go-aware transform's prediction of the new file,
// together with what an attribution probe needs to explain it.
//
// NewToOld and OldToNew are the decoder-side match -- the one predictText
// actually indexed -- so a probe classifying a function by how it was
// predicted reads the same table the predictor did.
type Prediction struct {
	Pred     []byte
	NewToOld []int
	OldToNew []int

	Exact, Norm, Content, Unmatched, UnmatchedOld int
	LayoutLen, Stage1aLen, Stage1bLen             int

	g *goPred
}

// Predict runs the Go-aware transform as far as its prediction and stops.
// It is exported for measurement only; Encode does not call it, and a
// prediction returned here is byte-identical to the one Encode's correction
// is written against.
func Predict(old, new []byte) (*Prediction, error) {
	g, err := predictGoAMD64(old, new)
	if err != nil {
		return nil, err
	}
	return &Prediction{
		Pred: g.pred, NewToOld: g.m2.NewToOld, OldToNew: g.m2.OldToNew,
		Exact: g.m.Exact, Norm: g.m.Norm, Content: g.m.Content,
		Unmatched: g.m.Unmatched, UnmatchedOld: g.m.UnmatchedOld,
		LayoutLen: len(g.layRaw), Stage1aLen: len(g.s1a), Stage1bLen: len(g.s1b),
		g: g,
	}, nil
}

// TypeSite is one place the type-descriptor rewriter wrote, in offsets into
// the predicted file. Role is 'n' nameOff, 't' typeOff, 'x' textOff, 'p'
// ptrToThis, 'M' a field of an uncommon-type method table, or 'D' for the
// extent of a whole descriptor (N is then the descriptor's size).
type TypeSite struct {
	Off  int
	N    int
	Role byte
}

// TypeSites replays the descriptor walk that predicted the type section and
// reports where it wrote. Sites are in the order the walk visits them, which
// is not offset order, and a descriptor's 'D' extent precedes its fields.
func (p *Prediction) TypeSites() []TypeSite {
	g := p.g
	tsec := g.ob.SectionOf(g.ob.Mod.Types)
	if tsec == nil {
		return nil
	}
	os, ns := g.ob.Sects[tsec.Name], g.skel.Sects[tsec.Name]
	dm := g.lay.DataMaps[tsec.Name]
	if os == nil || ns == nil || dm == nil {
		return nil
	}
	var out []TypeSite
	w := &typeWalk{old: g.ob, new: g.skel, os: os, d: os.Data, dm: dm, mp: g.mp,
		visited: map[uint64]bool{}}
	w.site = func(off uint64, n int, role byte) {
		q := w.place(off)
		if q < 0 || q+int64(n) > int64(ns.Size) {
			return
		}
		out = append(out, TypeSite{Off: int(ns.Off + uint64(q)), N: n, Role: role})
	}
	w.roots()
	for len(w.queue) > 0 {
		a := w.queue[0]
		w.queue = w.queue[1:]
		w.descriptor(a - os.Addr)
	}
	return out
}

// TypeSectionName is the section moduledata.types lives in, or "".
func (p *Prediction) TypeSectionName() string {
	if s := p.g.ob.SectionOf(p.g.ob.Mod.Types); s != nil {
		return s.Name
	}
	return ""
}
