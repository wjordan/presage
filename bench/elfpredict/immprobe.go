package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/wjordan/go-binsync/delta/x86"
	"golang.org/x/arch/x86/x86asm"
)

// probeImmediateDiff looks inside the two operand classes operands.go's table
// counted but never opened: "immediate only" (48,149 instructions, 88,294
// wrong bytes) and "displacement only, no base register" (14,426 / 43,421).
// Both are single-field residuals, so both are candidates for the field-fix
// treatment of §9.15 -- ship the difference, or an index into something the
// decoder can enumerate, instead of the literal bytes. What decides that is
// what the field holds.
//
//	A the immediate. If new-old is one constant per function it is H2 for
//	  immediates; if the value is a small constant it wants a delta; if it is
//	  an address it belongs in the address layer, not the byte correction; if
//	  it is another site's old immediate the function permuted its constants.
//	B the base-less displacement. x86asm reports Base=0 for two unrelated
//	  encodings, and separating them is the whole of this half. A SIB form
//	  with no index is a true absolute [disp32]; mod=00 rm=101 is
//	  *rip-relative*, and x86asm only labels it so for legacy encodings --
//	  under VEX and EVEX it leaves Base=0 and PCRel=0, which is exactly why
//	  delta/x86's WalkReferences, which keys off PCRel, never sees it.
//
// The walk is operands.go's, and the classification is operands.go's
// compareOperands/class, so the counts here tie back to that table row for
// row. Report only.
func probeImmediateDiff(predText, targetText []byte, maps []mapping, oldImage, newImage *image, planBytes []byte, structure predictionPlan) {
	newSecs, oldSecs := sectionIndex(newImage), sectionIndex(oldImage)
	newTextAddr, oldTextAddr := newImage.Text.Addr, oldImage.Text.Addr
	structural := newAddressLookup(structure).target
	wide, wideErr := wholeImageOracle(planBytes, structure)
	if wideErr != nil {
		fmt.Fprintf(os.Stderr, "  probe immprobe: whole-image oracle unavailable (%v), structural only\n", wideErr)
	}

	var immAll, dispAll opCount

	// A1/B4: per-function delta histograms.
	var immTop1, immTop3, immSites int
	var immDense, immDenseTop1, immDenseTop3 int
	immFuncs := 0
	var otherTop1, otherTop3, otherSites int
	otherPairs := map[[2]int64]int{}

	// A2: what the immediate looks like.
	var immSmall, immMedium, immAddr, immBig opCount
	immBySection := map[string]*opCount{}
	var immStructTally, immWideTally oracleTally

	// A3: by mnemonic.
	immByOp := map[string]*opCount{}

	// A4: the new value is some other site's old value in the same function.
	var immSwap opCount
	var immPermFuncs, immPermSites int

	// B0: which of the three encodings the Base=0 operand really is.
	var dispRIP, dispAbs, dispIdx, dispSeg opCount
	dispByOp := map[string]*opCount{}

	// B1/B2/B3, all of the rip-relative half.
	var ripTargetInSection, ripPredInSection opCount
	ripBySection := map[string]*opCount{}
	var ripStructTally, ripWideTally oracleTally
	ripShifts := map[int64]int{}
	ripGroups := map[uint64]map[uint64]int{}

	bump := func(m map[string]*opCount, k string, w int) {
		c := m[k]
		if c == nil {
			c = &opCount{}
			m[k] = c
		}
		c.insts++
		c.wrong += w
	}
	score := func(t *oracleTally, lookup func(uint64) x86.Target, old, want uint64, w int) {
		switch tgt := lookup(old); {
		case !tgt.Known:
			t.unknown.insts++
			t.unknown.wrong += w
		case tgt.Addr == want:
			t.right.insts++
			t.right.wrong += w
		default:
			t.wrong.insts++
			t.wrong.wrong += w
		}
	}

	type immSite struct {
		old, new int64
		wrong    int
	}
	var imms []immSite
	var otherDeltas map[int64]int

	for _, m := range maps {
		if m.Dst+m.DstSize > uint64(len(targetText)) {
			continue
		}
		pred := predText[m.Dst : m.Dst+m.DstSize]
		want := targetText[m.Dst : m.Dst+m.DstSize]
		dirty := false
		for i := range want {
			if pred[i] != want[i] {
				dirty = true
				break
			}
		}
		if !dirty {
			continue
		}
		imms = imms[:0]
		otherDeltas = map[int64]int{}
		// Where this function's bytes were relocated from. A displacement
		// copied verbatim out of the old image now points shift bytes past
		// where it did, which is what makes the retarget question answerable
		// in new-image coordinates alone.
		shift := int64(newTextAddr+m.Dst) - int64(oldTextAddr+m.Src)

		for s := 0; s < len(want); {
			ti, ok := safeDecode(want[s:])
			if !ok {
				s++
				continue
			}
			end := min(s+ti.Len, len(want))
			w := 0
			for k := s; k < end; k++ {
				if pred[k] != want[k] {
					w++
				}
			}
			if w == 0 {
				s = end
				continue
			}
			pi, pok := safeDecode(pred[s:])
			if !pok {
				s = end
				continue
			}
			f := compareOperands(pi, ti)
			switch f.class() {
			case clsImm:
				o, n, got := immPair(pi, ti)
				if !got {
					s = end
					continue
				}
				immAll.insts++
				immAll.wrong += w
				imms = append(imms, immSite{o, n, w})
				bump(immByOp, ti.Op.String(), w)
				name, inSection := newSecs.find(uint64(n))
				switch a := max(n, -n); {
				case a < 256:
					immSmall.insts++
					immSmall.wrong += w
				case a < 1<<20:
					// Below any section that holds content code refers to;
					// the low metadata sections start at 0x2e0, so calling
					// these addresses would be an artefact of the test.
					immMedium.insts++
					immMedium.wrong += w
				case n > 0 && inSection:
					immAddr.insts++
					immAddr.wrong += w
					bump(immBySection, name, w)
					score(&immStructTally, structural, uint64(o), uint64(n), w)
					if wide != nil {
						score(&immWideTally, wide, uint64(o), uint64(n), w)
					}
				default:
					immBig.insts++
					immBig.wrong += w
				}

			case clsDispAbs:
				o, n := f.disps[0][0], f.disps[0][1]
				dispAll.insts++
				dispAll.wrong += w
				bump(dispByOp, ti.Op.String(), w)
				mem, kind := baseless(ti)
				if mem.Segment != 0 {
					dispSeg.insts++
					dispSeg.wrong += w
				}
				if kind != memRIPRelative {
					if kind == memIndexed {
						dispIdx.insts++
						dispIdx.wrong += w
					} else {
						dispAbs.insts++
						dispAbs.wrong += w
					}
					otherDeltas[n-o]++
					otherPairs[[2]int64{o, n}]++
					s = end
					continue
				}

				// rip-relative under VEX/EVEX: the displacement is measured
				// from the end of the instruction, so the field names an
				// address rather than an offset.
				dispRIP.insts++
				dispRIP.wrong += w
				predAddr := uint64(int64(newTextAddr+m.Dst) + int64(s) + int64(pi.Len) + o)
				wantAddr := uint64(int64(newTextAddr+m.Dst) + int64(s) + int64(ti.Len) + n)
				if name, ok := newSecs.find(wantAddr); ok {
					ripTargetInSection.insts++
					ripTargetInSection.wrong += w
					bump(ripBySection, name, w)
				}
				if _, ok := newSecs.find(predAddr); ok {
					ripPredInSection.insts++
					ripPredInSection.wrong += w
				}
				ripShifts[int64(wantAddr)-int64(predAddr)]++
				g := ripGroups[predAddr]
				if g == nil {
					g = map[uint64]int{}
					ripGroups[predAddr] = g
				}
				g[wantAddr]++
				// The retarget question. The prediction kept the old
				// displacement, so the old address it named is predAddr less
				// the function's shift; would the oracle have sent it right?
				oldAddr := uint64(int64(predAddr) - shift)
				if _, ok := oldSecs.find(oldAddr); ok {
					score(&ripStructTally, structural, oldAddr, wantAddr, w)
					if wide != nil {
						score(&ripWideTally, wide, oldAddr, wantAddr, w)
					}
				} else {
					ripStructTally.unknown.insts++
					ripStructTally.unknown.wrong += w
					ripWideTally.unknown.insts++
					ripWideTally.unknown.wrong += w
				}
			}
			s = end
		}

		// A1: this function's immediate deltas.
		if len(imms) > 0 {
			immFuncs++
			deltas := map[int64]int{}
			oldVals := map[int64]int{}
			for _, st := range imms {
				deltas[st.new-st.old]++
				oldVals[st.old]++
			}
			t1, t3 := topShares(deltas)
			immSites += len(imms)
			immTop1 += t1
			immTop3 += t3
			if len(imms) >= denseFunc {
				immDense += len(imms)
				immDenseTop1 += t1
				immDenseTop3 += t3
			}

			// A4: a new value that is some other site's old value. The two
			// differ at every site, so a hit is necessarily a different site.
			perm := true
			newVals := map[int64]int{}
			for _, st := range imms {
				newVals[st.new]++
				if oldVals[st.new] > 0 {
					immSwap.insts++
					immSwap.wrong += st.wrong
				} else {
					perm = false
				}
			}
			if perm && sameMultiset(oldVals, newVals) {
				immPermFuncs++
				immPermSites += len(imms)
			}
		}

		// B4: this function's non-rip-relative displacement deltas.
		if len(otherDeltas) > 0 {
			t1, t3 := topShares(otherDeltas)
			for _, n := range otherDeltas {
				otherSites += n
			}
			otherTop1 += t1
			otherTop3 += t3
		}
	}

	pct := func(n, d int) float64 { return 100 * float64(n) / float64(max(1, d)) }
	row := func(name string, c opCount, d int) {
		fmt.Fprintf(os.Stderr, "        %-45s %8d instructions, %8d wrong bytes (%.1f%%)\n",
			name, c.insts, c.wrong, pct(c.insts, d))
	}
	oracles := func(what string, st, wd oracleTally, d int) {
		for _, o := range []struct {
			name string
			t    oracleTally
			on   bool
		}{
			{"structural lookup (points, function map, ranges)", st, true},
			{"whole-image oracle (equivalences + sections)", wd, wide != nil},
		} {
			if !o.on {
				continue
			}
			fmt.Fprintf(os.Stderr, "        %s\n", o.name)
			row("  sends "+what+" to the correct value", o.t.right, d)
			row("  answers, but with the wrong value", o.t.wrong, d)
			row("  no answer", o.t.unknown, d)
		}
	}

	fmt.Fprintf(os.Stderr, "  probe immprobe: inside two unopened operand classes\n")

	fmt.Fprintf(os.Stderr, "    A immediate only: %d instructions, %d wrong bytes, in %d functions\n",
		immAll.insts, immAll.wrong, immFuncs)
	fmt.Fprintf(os.Stderr, "      A1 delta histogram (new_imm - old_imm), per function\n")
	fmt.Fprintf(os.Stderr, "        explained by the function's single most common delta: %d (%.1f%%)\n",
		immTop1, pct(immTop1, immSites))
	fmt.Fprintf(os.Stderr, "        explained by the function's top 3 deltas:              %d (%.1f%%)\n",
		immTop3, pct(immTop3, immSites))
	fmt.Fprintf(os.Stderr, "        in functions with >=%d sites: %d sites, top delta %d (%.1f%%), top 3 %d (%.1f%%)\n",
		denseFunc, immDense, immDenseTop1, pct(immDenseTop1, immDense), immDenseTop3, pct(immDenseTop3, immDense))
	fmt.Fprintf(os.Stderr, "      A2 what the new immediate looks like\n")
	row("small constant (|imm| < 256)", immSmall, immAll.insts)
	row("medium constant (256 <= |imm| < 1M)", immMedium, immAll.insts)
	row("address-like (>= 1M, inside a mapped section)", immAddr, immAll.insts)
	row("large, but in no section", immBig, immAll.insts)
	printRanked(os.Stderr, "          target section", immBySection, immAddr.insts, 8)
	fmt.Fprintf(os.Stderr, "        of the address-like ones, does the oracle map old_imm -> new_imm?\n")
	oracles("old_imm", immStructTally, immWideTally, immAddr.insts)
	fmt.Fprintf(os.Stderr, "      A3 by mnemonic\n")
	printRanked(os.Stderr, "          op", immByOp, immAll.insts, 10)
	fmt.Fprintf(os.Stderr, "      A4 permutation within the function\n")
	row("new value is another site's old value", immSwap, immAll.insts)
	fmt.Fprintf(os.Stderr, "        whole function is a permutation of its own immediates: %d functions, %d sites (%.1f%%)\n",
		immPermFuncs, immPermSites, pct(immPermSites, immAll.insts))

	fmt.Fprintf(os.Stderr, "    B displacement only, no base register: %d instructions, %d wrong bytes\n",
		dispAll.insts, dispAll.wrong)
	fmt.Fprintf(os.Stderr, "      B0 which encoding is it really?\n")
	row("rip-relative, unlabelled by x86asm (VEX/EVEX)", dispRIP, dispAll.insts)
	row("true absolute [disp32] (SIB, no index)", dispAbs, dispAll.insts)
	row("index-only [index*s+disp32]", dispIdx, dispAll.insts)
	row("carries a segment override", dispSeg, dispAll.insts)
	printRanked(os.Stderr, "          op", dispByOp, dispAll.insts, 8)
	fmt.Fprintf(os.Stderr, "      B1 the rip-relative half: is the field an image address?\n")
	row("correct target inside a mapped new section", ripTargetInSection, dispRIP.insts)
	row("predicted target inside a mapped new section", ripPredInSection, dispRIP.insts)
	printRanked(os.Stderr, "          target section", ripBySection, dispRIP.insts, 8)
	fmt.Fprintf(os.Stderr, "      B2 would the existing address oracle retarget them correctly?\n")
	oracles("the old target", ripStructTally, ripWideTally, dispRIP.insts)
	fmt.Fprintf(os.Stderr, "      B3 what a field-fix layer extended to these fields would ship\n")
	consistent, consistentSites, groupSites := 0, 0, 0
	for _, g := range ripGroups {
		n := 0
		for _, c := range g {
			n += c
		}
		groupSites += n
		if len(g) == 1 {
			consistent++
			consistentSites += n
		}
	}
	fmt.Fprintf(os.Stderr, "        %d distinct predicted target addresses over %d sites; %d of them (%d sites, %.1f%%) want one correct address\n",
		len(ripGroups), groupSites, consistent, consistentSites, pct(consistentSites, groupSites))
	printPairCoverage(os.Stderr, "        ", ripShifts, dispRIP.insts, "distinct correct-minus-predicted shifts")
	fmt.Fprintf(os.Stderr, "      B4 the non-rip remainder: delta histogram, per function\n")
	fmt.Fprintf(os.Stderr, "        explained by the function's single most common delta: %d (%.1f%%)\n",
		otherTop1, pct(otherTop1, otherSites))
	fmt.Fprintf(os.Stderr, "        explained by the function's top 3 deltas:              %d (%.1f%%)\n",
		otherTop3, pct(otherTop3, otherSites))
	flat := map[int64]int{}
	for p, n := range otherPairs {
		flat[p[0]<<20^p[1]] += n
	}
	printPairCoverage(os.Stderr, "        ", flat, otherSites, "distinct (old,new) pairs")
}

// oracleTally splits a projection test three ways: right, answered wrongly,
// and no answer at all. The middle one is the expensive case -- a confident
// wrong address costs a correction where silence costs nothing.
type oracleTally struct{ right, wrong, unknown opCount }

// immPair returns the first immediate that differs between the prediction and
// the target. class() has already established that nothing else differs, so
// there is exactly one pair in all but a handful of two-immediate encodings.
func immPair(pi, ti x86asm.Inst) (old, new int64, ok bool) {
	for i := 0; i < argCount(ti); i++ {
		p, pok := pi.Args[i].(x86asm.Imm)
		t, tok := ti.Args[i].(x86asm.Imm)
		if pok && tok && p != t {
			return int64(p), int64(t), true
		}
	}
	return 0, 0, false
}

const (
	memRIPRelative = iota
	memAbsolute
	memIndexed
)

// baseless returns the first memory operand with no base register, and says
// which of the three encodings that share Base=0 it is. Scale is the tell: the
// mod=00 rm=101 rip-relative form carries no SIB byte at all, so x86asm leaves
// Scale zero, while a true absolute needs a SIB and reports Scale 1.
func baseless(i x86asm.Inst) (x86asm.Mem, int) {
	for k := 0; k < argCount(i); k++ {
		m, ok := i.Args[k].(x86asm.Mem)
		if !ok || m.Base != 0 {
			continue
		}
		switch {
		case m.Index != 0:
			return m, memIndexed
		case m.Scale == 0:
			return m, memRIPRelative
		default:
			return m, memAbsolute
		}
	}
	return x86asm.Mem{}, memAbsolute
}

// topShares is H2's question asked of any histogram: how many sites the single
// most common value covers, and how many the top three cover.
func topShares(h map[int64]int) (top1, top3 int) {
	counts := make([]int, 0, len(h))
	for _, n := range h {
		counts = append(counts, n)
	}
	slices.SortFunc(counts, func(a, b int) int { return b - a })
	for i := 0; i < len(counts) && i < 3; i++ {
		top3 += counts[i]
	}
	if len(counts) > 0 {
		top1 = counts[0]
	}
	return top1, top3
}

func sameMultiset(a, b map[int64]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, n := range a {
		if b[k] != n {
			return false
		}
	}
	return true
}

func printRanked(w *os.File, label string, m map[string]*opCount, total, limit int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y string) int {
		if d := m[y].insts - m[x].insts; d != 0 {
			return d
		}
		return len(x) - len(y)
	})
	for i, k := range keys {
		if i >= limit {
			break
		}
		fmt.Fprintf(w, "%s %-22s %8d instructions, %8d wrong bytes (%.1f%%)\n",
			label, k, m[k].insts, m[k].wrong, 100*float64(m[k].insts)/float64(max(1, total)))
	}
}

// printPairCoverage prices a dictionary of recurring values the way
// operands.go prices H3's.
func printPairCoverage(w *os.File, indent string, h map[int64]int, total int, what string) {
	ranked := make([]int, 0, len(h))
	for _, n := range h {
		ranked = append(ranked, n)
	}
	slices.SortFunc(ranked, func(a, b int) int { return b - a })
	fmt.Fprintf(w, "%s%d %s over %d sites\n", indent, len(ranked), what, total)
	cum, k := 0, 0
	for _, n := range []int{1, 10, 100, 1000} {
		for ; k < n && k < len(ranked); k++ {
			cum += ranked[k]
		}
		fmt.Fprintf(w, "%s  top %4d cover %8d sites (%.1f%%)\n", indent, n, cum,
			100*float64(cum)/float64(max(1, total)))
	}
}

// namedRange is one mapped section, kept as an address interval so a value can
// be tested for "is this an address at all".
type namedRange struct {
	name   string
	lo, hi uint64
}

type sectionTable []namedRange

func sectionIndex(im *image) sectionTable {
	out := make(sectionTable, 0, len(im.Sections))
	for name, s := range im.Sections {
		out = append(out, namedRange{name, s.Addr, s.Addr + s.Size})
	}
	slices.SortFunc(out, func(a, b namedRange) int { return cmpU(a.lo, b.lo) })
	return out
}

func (t sectionTable) find(addr uint64) (string, bool) {
	i, ok := slices.BinarySearchFunc(t, addr, func(r namedRange, addr uint64) int {
		if r.lo > addr {
			return 1
		}
		if r.hi <= addr {
			return -1
		}
		return 0
	})
	if !ok {
		return "", false
	}
	return t[i].name, true
}

// wholeImageOracle rebuilds the oracle the whole-image rungs retarget with,
// from the plan alone -- the same reconstruction probeOrdering does, so the
// answer here is the one the shipped decoder would give.
func wholeImageOracle(planBytes []byte, structure predictionPlan) (func(uint64) x86.Target, error) {
	cp, err := unmarshalCombinedPlan(planBytes)
	if err != nil {
		return nil, err
	}
	ep, err := parseEquivalencePlan(cp.Equivalences)
	if err != nil {
		return nil, err
	}
	var srcPred *srcPredictor
	if len(structure.Maps) != 0 {
		srcPred = &srcPredictor{maps: structure.Maps, oldOff: ep.OldText.Off, newOff: ep.NewText.Off, newSize: ep.NewText.Size}
	}
	if ep.Eqs, err = decodeEquivalences(ep, srcPred); err != nil {
		return nil, err
	}
	var rp *relocPlan
	if len(cp.Reloc) != 0 {
		parsed, err := unmarshalRelocPlan(cp.Reloc)
		if err != nil {
			return nil, err
		}
		rp = &parsed
	}
	return newImageOracle(ep, structure, rp), nil
}
