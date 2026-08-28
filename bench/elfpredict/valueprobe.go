package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/wjordan/go-binsync/delta/x86"
)

// probeEquivalenceValue asks what the equivalence stream is still paying for.
//
// The runs are Zucchini's, chosen by Zucchini's cost model before any of this
// system's later layers existed. Two ways that can be wasteful. A run whose
// destination lies wholly inside a function the choice layer overwrites is
// dead weight -- the decoder copies it and then throws it away. And a run
// whose bytes are wrong in the finished prediction anyway bought nothing,
// while still costing its four plan columns.
func probeEquivalenceValue(old, target, planBytes []byte, structure predictionPlan, final []byte) {
	cp, err := unmarshalCombinedPlan(planBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe eqvalue FAILED: %v\n", err)
		return
	}
	ep, err := parseEquivalencePlan(cp.Equivalences)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe eqvalue FAILED (ep): %v\n", err)
		return
	}
	var srcPred *srcPredictor
	if len(structure.Maps) != 0 {
		srcPred = &srcPredictor{maps: structure.Maps, oldOff: ep.OldText.Off, newOff: ep.NewText.Off, newSize: ep.NewText.Size}
	}
	if ep.Eqs, err = decodeEquivalences(ep, srcPred); err != nil {
		fmt.Fprintf(os.Stderr, "probe eqvalue FAILED (eqs): %v\n", err)
		return
	}
	// Destination extent of every chosen function, in image offsets.
	type span struct{ lo, hi uint64 }
	var chosen []span
	for i, m := range structure.Maps {
		if i/8 < len(cp.Choices) && cp.Choices[i/8]&(1<<(i%8)) != 0 {
			chosen = append(chosen, span{ep.NewText.Off + m.Dst, ep.NewText.Off + m.Dst + m.DstSize})
		}
	}
	inChosen := func(lo, hi uint64) bool {
		i, j := 0, len(chosen)
		for i < j {
			h := (i + j) / 2
			if chosen[h].hi <= lo {
				i = h + 1
			} else {
				j = h
			}
		}
		return i < len(chosen) && chosen[i].lo <= lo && hi <= chosen[i].hi
	}
	var dead, deadBytes, useless, uselessBytes int
	var live []equivalence
	for _, e := range ep.Eqs {
		if inChosen(e.Dst, e.Dst+e.N) {
			dead++
			deadBytes += int(e.N)
			continue
		}
		correct := 0
		for i := e.Dst; i < e.Dst+e.N; i++ {
			if final[i] == target[i] {
				correct++
			}
		}
		if correct == 0 {
			useless++
			uselessBytes += int(e.N)
		}
		live = append(live, e)
	}
	full := xzSize(cp.Equivalences)
	pruned := full
	ep2 := ep
	ep2.Eqs = live
	if b, err := ep2.marshal(srcPred); err == nil {
		pruned = xzSize(b)
	}
	fmt.Fprintf(os.Stderr, "  probe eqvalue: %d runs; %d dead inside chosen functions (%d B), %d contribute no correct byte (%d B); equivalence stream %d -> %d xz if the dead are dropped (%+d)\n",
		len(ep.Eqs), dead, deadBytes, useless, uselessBytes, full, pruned, pruned-full)

	// What each length of run is actually buying. A run costs three varints in
	// the plan whatever its length, and buys the bytes of it that end up
	// right. Below some length the trade stops paying, and that is the
	// threshold a cost model would set -- Zucchini's matcher never saw this
	// cost function, because it was pricing its own patch format.
	fmt.Fprintln(os.Stderr, "    length bucket        runs      bytes    correct   plan est   corr saved   net")
	bounds := []uint64{1, 2, 4, 8, 16, 32, 64, 256, 1 << 62}
	lo := uint64(0)
	for _, hi := range bounds {
		var n, total, correct, planEst int
		for _, e := range ep.Eqs {
			if e.N <= lo || e.N > hi {
				continue
			}
			n++
			total += int(e.N)
			for i := e.Dst; i < e.Dst+e.N; i++ {
				if final[i] == target[i] {
					correct++
				}
			}
			planEst += len(appendU(nil, e.N)) + len(appendU(nil, e.Dst)) + 2
		}
		if n == 0 {
			lo = hi
			continue
		}
		// The plan estimate is raw varint bytes; xz prices the equivalence
		// stream at 539,300 for 1,929,796 raw, so scale by that ratio.
		planXZ := float64(planEst) * 0.2795
		saved := correctionCost(n, correct)
		fmt.Fprintf(os.Stderr, "    %5d-%-12d %8d %10d %10d %10.0f %12.0f %6.0f\n",
			lo+1, hi, n, total, correct, planXZ, saved, saved-planXZ)
		lo = hi
	}
}

// probeSourceCanonicalisation asks whether the largest equivalence column is
// paying for a choice nobody needed to make. The source residual is the gap
// between where the function map says a run's bytes came from and where the
// matcher said they came from. When both places hold the same bytes, the
// matcher's answer carries no information the decoder needs -- the encoder
// could have named the predicted source instead and shipped a zero.
//
// The catch is retargeting: it recovers a reference's old absolute target from
// the run's source address, so two byte-identical sources at different
// addresses resolve to different targets. The count below is therefore an
// upper bound, split by whether the run's bytes contain a reference at all.
func probeSourceCanonicalisation(old, planBytes []byte, structure predictionPlan) {
	cp, err := unmarshalCombinedPlan(planBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe eqcanon FAILED: %v\n", err)
		return
	}
	ep, err := parseEquivalencePlan(cp.Equivalences)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe eqcanon FAILED (ep): %v\n", err)
		return
	}
	var srcPred *srcPredictor
	if len(structure.Maps) != 0 {
		srcPred = &srcPredictor{maps: structure.Maps, oldOff: ep.OldText.Off, newOff: ep.NewText.Off, newSize: ep.NewText.Size}
	}
	if ep.Eqs, err = decodeEquivalences(ep, srcPred); err != nil {
		fmt.Fprintf(os.Stderr, "probe eqcanon FAILED (eqs): %v\n", err)
		return
	}
	var predicted, nonzero, equalBytes, equalNoRefs int
	var canonical, asIs []byte
	for _, e := range ep.Eqs {
		base, ok := srcPred.at(e.Dst)
		if !ok {
			continue
		}
		predicted++
		r := int64(e.Src) - int64(base)
		asIs = appendS(asIs, r)
		if r == 0 {
			canonical = appendS(canonical, 0)
			continue
		}
		nonzero++
		if base+e.N <= uint64(len(old)) && bytes.Equal(old[base:base+e.N], old[e.Src:e.Src+e.N]) {
			equalBytes++
			refs := 0
			x86.WalkReferences(old[e.Src:e.Src+e.N], 0, func(x86.Reference) { refs++ })
			if refs == 0 {
				equalNoRefs++
				canonical = appendS(canonical, 0)
				continue
			}
		}
		canonical = appendS(canonical, r)
	}
	fmt.Fprintf(os.Stderr, "  probe eqcanon: %d predicted runs, %d with a nonzero residual; %d have byte-identical sources, %d of those carry no reference; column %d -> %d xz (%+d)\n",
		predicted, nonzero, equalBytes, equalNoRefs, xzSize(asIs), xzSize(canonical), xzSize(canonical)-xzSize(asIs))
}
