package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/wjordan/presage/presage/eqmatch"
)

// The whole-image equivalence stream matched here (presage/eqmatch) instead
// of read out of an external Zucchini patch. The matcher's design notes and
// its measurements against Zucchini are in
// docs/general/research/matcher-spike.md.
var (
	// nativeEq selects this matcher over -equivalence-patch.
	nativeEq bool
	// nativeEqMin and nativeEqDrop are eqmatch.Params, exposed for sweeps.
	nativeEqMin  = eqmatch.Defaults.Min
	nativeEqDrop = eqmatch.Defaults.Drop
)

func nativeEquivalences(old, nw []byte) []equivalence {
	runs := eqmatch.Match(old, nw, eqmatch.Params{Min: nativeEqMin, Drop: nativeEqDrop})
	eqs := make([]equivalence, len(runs))
	for i, r := range runs {
		eqs[i] = equivalence{Src: r.Src, Dst: r.Dst, N: r.N}
	}
	return eqs
}

// nativeEquivalencePlan is parseExternalEquivalence's replacement: the same
// plan, with the runs matched here instead of read out of a Zucchini patch.
func nativeEquivalencePlan(oldImage, newImage *image) (equivalencePlan, error) {
	t := startStage("native equivalences")
	eqs := nativeEquivalences(oldImage.Data, newImage.Data)
	var covered uint64
	for _, e := range eqs {
		covered += e.N
	}
	p := equivalencePlan{
		OldLen: uint64(len(oldImage.Data)), NewLen: uint64(len(newImage.Data)),
		OldText: oldImage.Text, NewText: newImage.Text, Eqs: eqs,
	}
	p.SrcSkip, p.DstSkip, p.CopyLen = encodeColumns(eqs)
	t.done("min %d: %d runs covering %d of %d new bytes (%.3f%%)",
		nativeEqMin, len(eqs), covered, len(newImage.Data),
		100*float64(covered)/float64(max(len(newImage.Data), 1)))
	return p, nil
}

// buildEquivalencePlan supplies runCombined's equivalences from whichever
// source the flags name.
func buildEquivalencePlan(externalPath string, oldImage, newImage *image) (equivalencePlan, error) {
	var p equivalencePlan
	var err error
	if nativeEq {
		p, err = nativeEquivalencePlan(oldImage, newImage)
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
