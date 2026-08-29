package delta

import "fmt"

// GoImage is the Go transform's prediction together with what a layer
// built over it needs: which sections the transform modelled and which it
// merely copied, the old→new address map, and how every matched function's
// size changed. Both sides of a codec build it from the same plan, so the
// layers above see the same image.
type GoImage struct {
	Pred     []byte
	Sections []GoImageSection
	// Lookup is the new address of an old one, when the transform knows it.
	Lookup func(uint64) (uint64, bool)
	// SizeDelta is new size minus old size for the function at an old entry.
	SizeDelta func(uint64) (int64, bool)
}

// GoImageSection is a section present in both files. An unmodelled section's
// prediction is the old bytes copied positionally.
type GoImageSection struct {
	Name                             string
	OldOff, OldSize, NewOff, NewSize uint64
	Modelled                         bool
}

// GoAnalyseImage is GoAnalyse returning the image instead of the bare
// prediction.
func GoAnalyseImage(old, new []byte, st *Stats) ([]byte, *GoImage, error) {
	if st == nil {
		st = &Stats{}
	}
	g, plan, err := goAnalyse(old, new, maxTransform, st)
	if err != nil {
		return nil, nil, err
	}
	return plan, g.image(), nil
}

// GoExpand is GoPredict returning the image.
func GoExpand(old, plan []byte, maxLen int64) (*GoImage, error) {
	g, rest, err := goPredict(old, plan, maxTransform, maxLen)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: %d trailing plan bytes", errCorrupt, len(rest))
	}
	return g.image(), nil
}

func (g *goPred) image() *GoImage {
	ob, skel, m, mp := g.ob, g.skel, g.m2, g.mp
	modelled := map[string]bool{".text": true, ".gopclntab": true, ".plt": true}
	for _, n := range ptrRewriteSects {
		modelled[n] = true
	}
	for n := range mp.dataMaps {
		modelled[n] = true
	}
	img := &GoImage{Pred: g.pred, Lookup: mp.entryLookup()}
	for _, ns := range skel.Order {
		os := ob.Sects[ns.Name]
		if ns.NoBits || os == nil || os.NoBits {
			continue
		}
		img.Sections = append(img.Sections, GoImageSection{
			Name: ns.Name, OldOff: os.Off, OldSize: os.Size, NewOff: ns.Off, NewSize: ns.Size,
			Modelled: modelled[ns.Name],
		})
	}
	deltas := make(map[uint64]int64, len(ob.Funcs))
	for i, f := range ob.Funcs {
		if j := m.OldToNew[i]; j >= 0 {
			deltas[f.Entry] = int64(skel.Funcs[j].Size()) - int64(f.Size())
		}
	}
	img.SizeDelta = func(entry uint64) (int64, bool) {
		d, ok := deltas[entry]
		return d, ok
	}
	return img
}
