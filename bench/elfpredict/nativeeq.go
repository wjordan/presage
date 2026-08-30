package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/presage/eqmatch"
)

// nativeEqMask matches .text with every PC-relative displacement zeroed
// (x86.Canonical), so two copies of the same code whose targets moved
// agree byte for byte; the relocation stage fixes the displacements from
// the function map afterwards. This is Zucchini's label comparison in its
// cheapest form.
var nativeEqMask = true

// nativeEqExpect hands the function map to the matcher as its expected
// source; nativeEqSlack is eqmatch.Params.Slack.
var (
	nativeEqExpect = true
	nativeEqSlack  = eqmatch.CodeDefaults.Slack
	nativeEqMinFar = eqmatch.CodeDefaults.MinFar
)

// maskedText returns a copy of img.Data with .text canonicalised, or the
// data itself when masking is off.
func maskedText(img *image) []byte {
	if !nativeEqMask || img.Text.Size == 0 {
		return img.Data
	}
	out := append([]byte(nil), img.Data...)
	lo, hi := img.Text.Off, img.Text.Off+img.Text.Size
	canon, _ := x86.Canonical(img.Data[lo:hi])
	copy(out[lo:hi], canon)
	return out
}

// The whole-image equivalence stream matched here (presage/eqmatch) instead
// of read out of an external Zucchini patch. The matcher's design notes and
// its measurements against Zucchini are in
// docs/general/research/matcher-spike.md.
var (
	// nativeEq selects this matcher over -equivalence-patch.
	nativeEq bool
	// nativeEqMin and nativeEqDrop are eqmatch.Params, exposed for sweeps.
	nativeEqMin  = eqmatch.CodeDefaults.Min
	nativeEqDrop = eqmatch.CodeDefaults.Drop
)

func nativeEquivalences(oldImage, newImage *image, pred *srcPredictor) []equivalence {
	params := eqmatch.Params{Min: nativeEqMin, Drop: nativeEqDrop, Slack: nativeEqSlack, MinFar: nativeEqMinFar}
	if pred != nil && nativeEqExpect {
		params.Expect = func(dst int) (int, bool) {
			s, ok := pred.at(uint64(dst))
			return int(s), ok
		}
	}
	runs := eqmatch.Match(maskedText(oldImage), maskedText(newImage), params)
	eqs := make([]equivalence, len(runs))
	for i, r := range runs {
		eqs[i] = equivalence{Src: r.Src, Dst: r.Dst, N: r.N}
	}
	return eqs
}

// nativeEquivalencePlan is parseExternalEquivalence's replacement: the same
// plan, with the runs matched here instead of read out of a Zucchini patch.
func nativeEquivalencePlan(oldImage, newImage *image, structure predictionPlan) (equivalencePlan, error) {
	t := startStage("native equivalences")
	var pred *srcPredictor
	if len(structure.Maps) != 0 {
		pred = &srcPredictor{maps: structure.Maps, oldOff: oldImage.Text.Off, newOff: newImage.Text.Off, newSize: newImage.Text.Size}
	}
	eqs := nativeEquivalences(oldImage, newImage, pred)
	if os.Getenv("EQSTATS") != "" {
		// residual histogram against the map, .text runs only
		var zero, small, mid, big, unmapped, outText int
		var bytesZero, bytesUnmapped uint64
		tlo, thi := newImage.Text.Off, newImage.Text.Off+newImage.Text.Size
		for _, e := range eqs {
			if e.Dst < tlo || e.Dst >= thi {
				outText++
				continue
			}
			base, ok := pred.at(e.Dst)
			if !ok {
				unmapped++
				bytesUnmapped += e.N
				continue
			}
			d := int64(e.Src) - int64(base)
			switch {
			case d == 0:
				zero++
				bytesZero += e.N
			case d > -128 && d < 128:
				small++
			case d > -65536 && d < 65536:
				mid++
			default:
				big++
			}
		}
		fmt.Fprintf(os.Stderr, "  eqstats: text runs residual 0: %d (%d B), |r|<128: %d, <64K: %d, big: %d; unmapped %d (%d B); outside .text %d\n",
			zero, bytesZero, small, mid, big, unmapped, bytesUnmapped, outText)
	}
	var covered uint64
	for _, e := range eqs {
		covered += e.N
	}
	p := equivalencePlan{
		OldLen: uint64(len(oldImage.Data)), NewLen: uint64(len(newImage.Data)),
		OldText: oldImage.Text, NewText: newImage.Text, Eqs: eqs,
	}
	p.SrcSkip, p.DstSkip, p.CopyLen = encodeColumns(eqs)
	t.done("min %d mask %v: %d runs covering %d of %d new bytes (%.3f%%)",
		nativeEqMin, nativeEqMask, len(eqs), covered, len(newImage.Data),
		100*float64(covered)/float64(max(len(newImage.Data), 1)))
	return p, nil
}

// buildEquivalencePlan supplies runCombined's equivalences from whichever
// source the flags name.
func buildEquivalencePlan(externalPath string, oldImage, newImage *image, structure predictionPlan) (equivalencePlan, error) {
	var p equivalencePlan
	var err error
	if nativeEq {
		p, err = nativeEquivalencePlan(oldImage, newImage, structure)
	} else {
		p, err = parseExternalEquivalence(externalPath, oldImage, newImage)
	}
	if err != nil {
		return equivalencePlan{}, err
	}
	textLo, textHi := p.NewText.Off, p.NewText.Off+p.NewText.Size
	var inText, textBytes, covered uint64
	for _, e := range p.Eqs {
		covered += e.N
		if e.Dst < textHi && e.Dst+e.N > textLo {
			inText++
			textBytes += min(e.Dst+e.N, textHi) - max(e.Dst, textLo)
		}
	}
	fmt.Fprintf(os.Stderr, "  equivalences: %d runs, %d bytes covered; %d runs touch .text, covering %d of its %d bytes\n",
		len(p.Eqs), covered, inText, textBytes, p.NewText.Size)
	return p, nil
}

// checkNativeEqFlags rejects the combinations that would silently measure
// something other than what was asked for.
func checkNativeEqFlags(externalPath string) error {
	if nativeEq && externalPath != "" {
		return errors.New("-native-equivalences and -equivalence-patch are alternatives")
	}
	if nativeEqMin < 4 {
		return errors.New("-native-eq-min must be at least 4")
	}
	return nil
}

func init() {
	flag.BoolVar(&nativeEqMask, "native-eq-mask", nativeEqMask, "match .text with PC-relative displacements zeroed")
	flag.BoolVar(&nativeEqExpect, "native-eq-expect", nativeEqExpect, "give the matcher the function map as its expected source")
	flag.IntVar(&nativeEqMinFar, "native-eq-minfar", nativeEqMinFar, "shortest run whose source is far from the expected one")
	flag.IntVar(&nativeEqSlack, "native-eq-slack", nativeEqSlack, "exact-prefix advantage needed to leave the expected source")
}
