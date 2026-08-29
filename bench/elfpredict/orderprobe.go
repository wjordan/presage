package main

import (
	"fmt"
	"os"

	"github.com/wjordan/go-binsync/delta/x86"
)

// wrongRuns counts both quantities the correction is priced on: the number of
// maximal runs of disagreement, and the number of bytes in them. The splitter
// charges roughly 1.061 patch bytes per run plus 0.624 per wrong byte, so a
// selector that counts only bytes is optimising the smaller of the two terms.
func wrongRuns(a, b []byte) (runs, bytes int) {
	in := false
	for i := range a {
		if a[i] != b[i] {
			bytes++
			if !in {
				runs++
				in = true
			}
		} else {
			in = false
		}
	}
	return runs, bytes
}

// correctionCost prices a residual with the measured yardstick of §10.
func correctionCost(runs, bytes int) float64 { return 1.061*float64(runs) + 0.624*float64(bytes) }

// probeOrdering asks whether the layers are composed in an order that lets each
// one see the best available input.
//
// Two suspicions, both visible in the decoder's own source. First, the choice
// layer overwrites a chosen function with predict()'s output, and predict()
// retargets against the structural map alone -- a map that describes .text and
// nothing else -- so every reference from a chosen function into .rodata or
// .data.rel.ro keeps its old displacement, while the equivalence path it
// replaced had retargeted exactly those through the whole-image oracle.
// Second, the choice bits are decided in the .text-only pass, where the
// equivalence side is built with no relocation plan and is therefore weaker
// than the one that actually runs. The selection is made against a strawman.
func probeOrdering(old, target, planBytes []byte, structure predictionPlan) {
	cp, err := unmarshalCombinedPlan(planBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe order FAILED: %v\n", err)
		return
	}
	// The prediction as it stands just before the choice layer runs.
	bare := cp
	bare.Choices, bare.Fields = nil, nil
	eqImage, _, err := predictImage(old, bare.marshal())
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe order FAILED (bare): %v\n", err)
		return
	}
	ep, err := parseEquivalencePlan(cp.Equivalences)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe order FAILED (ep): %v\n", err)
		return
	}
	var srcPred *srcPredictor
	if len(structure.Maps) != 0 {
		srcPred = &srcPredictor{maps: structure.Maps, oldOff: ep.OldText.Off, newOff: ep.NewText.Off, newSize: ep.NewText.Size}
	}
	if ep.Eqs, err = decodeEquivalences(ep, srcPred); err != nil {
		fmt.Fprintf(os.Stderr, "probe order FAILED (eqs): %v\n", err)
		return
	}
	var rp *relocPlan
	if len(cp.Reloc) != 0 {
		parsed, err := unmarshalRelocPlan(cp.Reloc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "probe order FAILED (reloc): %v\n", err)
			return
		}
		rp = &parsed
	}
	oracle := newImageOracle(ep, structure, rp)
	// The wide oracle answers an in-.text target from the equivalence
	// projection first. That is right for equivalence-copied bytes and wrong
	// for these: a function is chosen structurally precisely where the byte
	// matcher went astray, so its projection is the least trustworthy evidence
	// available. This third oracle keeps the structural answer and reaches for
	// the section geometry only where the structural map has nothing to say,
	// which is exactly the references that leave .text.
	structuralLookup := newAddressLookup(structure)
	sourceMapper := newSourceEquivalenceMapper(ep)
	sectionOracle := func(addr uint64) x86.Target {
		if t := structuralLookup.target(addr); t.Known {
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
		return x86.Target{}
	}

	oldText := old[ep.OldText.Off : ep.OldText.Off+ep.OldText.Size]
	derive := goMapDeriver(old, cp.GoTables, ep.OldText, ep.NewText)
	narrow, _, err := predict(oldText, cp.Structure, true, derive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe order FAILED (narrow): %v\n", err)
		return
	}
	wide, _, err := predictWith(oldText, cp.Structure, true, oracle, derive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe order FAILED (wide): %v\n", err)
		return
	}
	sect, _, err := predictWith(oldText, cp.Structure, true, sectionOracle, derive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe order FAILED (sect): %v\n", err)
		return
	}
	eqText := eqImage[ep.NewText.Off : ep.NewText.Off+ep.NewText.Size]
	targetText := target[ep.NewText.Off : ep.NewText.Off+ep.NewText.Size]

	// Five rules, all scored on the same per-function residuals. Runs are
	// counted inside each function, so a run spanning a boundary is counted
	// twice; the bias is identical across rules and tiny next to the 362,032
	// runs the section carries.
	type tally struct {
		runs, bytes, chosen, chosenBytes int
	}
	var asShipped, minBytesNarrow, minBytesWide, minCostNarrow, minCostWide, minCostSect tally
	add := func(t *tally, runs, bytes int, chose bool, size uint64) {
		t.runs += runs
		t.bytes += bytes
		if chose {
			t.chosen++
			t.chosenBytes += int(size)
		}
	}
	for i, m := range structure.Maps {
		lo, hi := m.Dst, m.Dst+m.DstSize
		tgt := targetText[lo:hi]
		eqR, eqB := wrongRuns(eqText[lo:hi], tgt)
		nR, nB := wrongRuns(narrow[lo:hi], tgt)
		wR, wB := wrongRuns(wide[lo:hi], tgt)

		shipped := i/8 < len(cp.Choices) && cp.Choices[i/8]&(1<<(i%8)) != 0
		if shipped {
			add(&asShipped, nR, nB, true, m.DstSize)
		} else {
			add(&asShipped, eqR, eqB, false, m.DstSize)
		}
		if nB < eqB {
			add(&minBytesNarrow, nR, nB, true, m.DstSize)
		} else {
			add(&minBytesNarrow, eqR, eqB, false, m.DstSize)
		}
		if wB < eqB {
			add(&minBytesWide, wR, wB, true, m.DstSize)
		} else {
			add(&minBytesWide, eqR, eqB, false, m.DstSize)
		}
		if correctionCost(nR, nB) < correctionCost(eqR, eqB) {
			add(&minCostNarrow, nR, nB, true, m.DstSize)
		} else {
			add(&minCostNarrow, eqR, eqB, false, m.DstSize)
		}
		if correctionCost(wR, wB) < correctionCost(eqR, eqB) {
			add(&minCostWide, wR, wB, true, m.DstSize)
		} else {
			add(&minCostWide, eqR, eqB, false, m.DstSize)
		}
		sR, sB := wrongRuns(sect[lo:hi], tgt)
		if correctionCost(sR, sB) < correctionCost(eqR, eqB) {
			add(&minCostSect, sR, sB, true, m.DstSize)
		} else {
			add(&minCostSect, eqR, eqB, false, m.DstSize)
		}
	}
	base := correctionCost(asShipped.runs, asShipped.bytes)
	show := func(name string, t tally) {
		fmt.Fprintf(os.Stderr, "    %-26s %8d runs %9d wrong  modelled %10.0f (%+.2f%%)  chose %6d fn / %9d B\n",
			name, t.runs, t.bytes, correctionCost(t.runs, t.bytes),
			100*(correctionCost(t.runs, t.bytes)-base)/base, t.chosen, t.chosenBytes)
	}
	fmt.Fprintf(os.Stderr, "  probe ordering: %d mapped functions\n", len(structure.Maps))
	show("as shipped", asShipped)
	show("reselect min-bytes narrow", minBytesNarrow)
	show("reselect min-bytes wide", minBytesWide)
	show("reselect min-cost narrow", minCostNarrow)
	show("reselect min-cost wide", minCostWide)
	show("reselect min-cost sections", minCostSect)

	// How much of the gap is the oracle alone, holding the selection fixed?
	var narrowChosen, wideChosen, sectChosen tally
	for i, m := range structure.Maps {
		if !(i/8 < len(cp.Choices) && cp.Choices[i/8]&(1<<(i%8)) != 0) {
			continue
		}
		lo, hi := m.Dst, m.Dst+m.DstSize
		tgt := targetText[lo:hi]
		nR, nB := wrongRuns(narrow[lo:hi], tgt)
		wR, wB := wrongRuns(wide[lo:hi], tgt)
		sR, sB := wrongRuns(sect[lo:hi], tgt)
		add(&narrowChosen, nR, nB, false, 0)
		add(&wideChosen, wR, wB, false, 0)
		add(&sectChosen, sR, sB, false, 0)
	}
	fmt.Fprintf(os.Stderr, "  probe ordering, chosen functions only: narrow %d runs / %d wrong; wide %d runs / %d wrong; sections %d runs / %d wrong\n",
		narrowChosen.runs, narrowChosen.bytes, wideChosen.runs, wideChosen.bytes, sectChosen.runs, sectChosen.bytes)
}
