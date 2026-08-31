package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/wjordan/presage/delta/x86"
	"golang.org/x/arch/x86/x86asm"
)

// The blocksidecar probe prices a second carried-forward sidecar: not the
// symbol table (that is sidecar.go) but a *block* map -- per-function basic
// block boundaries with stable identities, the shape of
// SHT_LLVM_BB_ADDR_MAP. A decoder that applied the previous patch could have
// been handed the old image's block map with it and carried it forward.
//
// The claim being tested: the equivalence stream's biggest job on mapped
// code is naming intra-function moves -- ext-TSP relayout shuffling blocks
// inside a function whose body is otherwise the same -- and a block
// permutation states those far more cheaply than image-level (dst, src, len)
// runs do.
//
// Everything here is report-only, and everything here has to be measured
// against what the plan already exploits. §9.16 already writes the source
// column as a residual against the function map, so a run landing at the
// map-predicted source is already nearly free; §9.18 already showed the
// length column is not derivable and that differencing the large columns
// loses. So the only population this probe may claim is the one the map
// cannot predict and the block model can.

// ---- basic blocks ---------------------------------------------------------

// isTerminator reports whether an instruction ends a basic block. Calls do
// not: LLVM's BB-addr-map does not split on them either.
func isTerminator(op x86asm.Op) bool {
	s := op.String()
	if s == "" {
		return false
	}
	switch s[0] {
	case 'J': // JMP and every Jcc, plus JCXZ/JECXZ/JRCXZ
		return true
	case 'R': // RET
		return len(s) >= 3 && s[:3] == "RET"
	case 'I':
		return len(s) >= 4 && s[:4] == "IRET"
	case 'L':
		return len(s) >= 4 && (s[:4] == "LOOP" || s[:4] == "LRET")
	case 'U': // UD0/UD1/UD2
		return len(s) >= 2 && s[:2] == "UD"
	case 'H':
		return s == "HLT"
	}
	return false
}

// blockBounds returns the basic-block start offsets of one function body,
// with len(code) appended as the terminating bound. Boundaries are the
// function start, every in-body target of a branch, and the instruction
// after every terminator. Undecodable bytes are stepped over one at a time,
// exactly as every other walk in this harness does.
func blockBounds(code []byte) []int32 {
	starts := map[int32]struct{}{0: {}}
	for i := 0; i < len(code); {
		inst, ok := safeDecode(code[i:])
		if !ok {
			i++
			continue
		}
		n := inst.Len
		if isTerminator(inst.Op) {
			if i+n < len(code) {
				starts[int32(i+n)] = struct{}{}
			}
			if len(inst.Args) > 0 {
				if rel, isRel := inst.Args[0].(x86asm.Rel); isRel {
					if t := i + n + int(rel); t > 0 && t < len(code) {
						starts[int32(t)] = struct{}{}
					}
				}
			}
		}
		i += n
	}
	out := make([]int32, 0, len(starts)+1)
	for s := range starts {
		out = append(out, s)
	}
	slices.Sort(out)
	return append(out, int32(len(code)))
}

// parallelDo runs do(k) for every k in [0,n) across one goroutine per core.
func parallelDo(n int, do func(k int)) {
	w := min(runtime.GOMAXPROCS(0), max(n, 1))
	var next atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < w; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				k := int(next.Add(1)) - 1
				if k >= n {
					return
				}
				do(k)
			}
		}()
	}
	wg.Wait()
}

// ---- classification -------------------------------------------------------

const (
	clsPredicted = iota // (a) source is exactly where the map says
	clsIntra            // (b) source inside the corresponding old function, elsewhere
	clsOther            // (c) source in some other old function, or outside old .text
	clsUnmapped         // (d) destination outside a mapped new function
	numClasses
)

var classNames = [numClasses]string{
	"(a) map-predicted source",
	"(b) intra-function move",
	"(c) source elsewhere",
	"(d) destination unmapped",
}

// mapIndexAt returns the index of the mapping covering the .text-relative
// offset off, or -1.
func mapIndexAt(maps []mapping, off uint64) int {
	i := sort.Search(len(maps), func(i int) bool { return maps[i].Dst > off })
	if i == 0 {
		return -1
	}
	if off >= maps[i-1].Dst+maps[i-1].DstSize {
		return -1
	}
	return i - 1
}

// reencode rebuilds the shipped equivalence stream from a subset of its runs,
// with the source column written against the same function map. Dropping runs
// only widens destination gaps, so the subset is a legal stream.
func reencodeEq(ep equivalencePlan, eqs []equivalence, pred *srcPredictor) ([]byte, error) {
	p := ep
	p.Eqs = eqs
	_, p.DstSkip, p.CopyLen = encodeColumns(eqs)
	p.SrcSkip, p.SrcResidual = nil, nil
	return p.marshal(pred)
}

func probeBlockSidecar(planBytes []byte, structure predictionPlan, oldImage, newImage *image) {
	fmt.Fprintln(os.Stderr, "probe blocksidecar: a carried-forward block map against the equivalence stream")
	cp, err := unmarshalCombinedPlan(planBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  blocksidecar FAILED: %v\n", err)
		return
	}
	ep, err := parseEquivalencePlan(cp.Equivalences)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  blocksidecar FAILED: %v\n", err)
		return
	}
	if len(structure.Maps) == 0 {
		fmt.Fprintln(os.Stderr, "  blocksidecar: no function map in this plan; stopping")
		return
	}
	pred := &srcPredictor{maps: structure.Maps, oldOff: ep.OldText.Off, newOff: ep.NewText.Off, newSize: ep.NewText.Size}
	eqs, err := decodeEquivalences(ep, pred)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  blocksidecar FAILED: %v\n", err)
		return
	}
	ep.Eqs = eqs

	// --- calibration -----------------------------------------------------
	// The ledger's 539,300 is planComponents' standalone xz (-T0) of this
	// stream. Every marginal below is a -T1 difference of two re-encodings of
	// the same stream, so the -T1 baseline is printed beside it and the two
	// must be close for the subtractions to mean anything.
	roundTrip, err := reencodeEq(ep, eqs, pred)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  blocksidecar: re-encode failed: %v; calibration unsafe\n", err)
		return
	}
	if !bytes.Equal(roundTrip, cp.Equivalences) {
		fmt.Fprintf(os.Stderr, "  blocksidecar: re-encode does not reproduce the shipped stream (%d vs %d); calibration unsafe\n",
			len(roundTrip), len(cp.Equivalences))
		return
	}
	ledger := xzSize(cp.Equivalences)
	base := xzSizeContiguous(cp.Equivalences)
	fmt.Fprintf(os.Stderr, "  calibration: %d runs; equivalence stream %d raw, %d xz -T0 (the ledger's row), %d xz -T1 (the marginal basis)\n",
		len(eqs), len(cp.Equivalences), ledger, base)

	// --- phase 1: anatomy -------------------------------------------------
	oldText, newText := oldImage.textBytes(), newImage.textBytes()
	maps := structure.Maps
	oldLo, oldHi := ep.OldText.Off, ep.OldText.Off+ep.OldText.Size
	class := make([]uint8, len(eqs))
	fnOf := make([]int32, len(eqs)) // mapping index of the destination, -1 when none
	var counts [numClasses]int
	var covered [numClasses]uint64
	for i, e := range eqs {
		fnOf[i] = -1
		base, ok := pred.at(e.Dst)
		if !ok {
			class[i] = clsUnmapped
			counts[clsUnmapped]++
			covered[clsUnmapped] += e.N
			continue
		}
		mi := mapIndexAt(maps, e.Dst-ep.NewText.Off)
		fnOf[i] = int32(mi)
		m := maps[mi]
		switch {
		case e.Src == base:
			class[i] = clsPredicted
		case e.Src >= oldLo && e.Src < oldHi && e.Src >= oldLo+m.Src && e.Src < oldLo+m.Src+m.SrcSize:
			class[i] = clsIntra
		default:
			class[i] = clsOther
		}
		counts[class[i]]++
		covered[class[i]] += e.N
	}

	// Marginal cost of each class: re-encode the stream with that class's
	// rows removed and subtract. This is what a replacement that takes those
	// rows over would actually save; the remaining rows keep their basis.
	var marginal [numClasses]int
	for c := 0; c < numClasses; c++ {
		if counts[c] == 0 {
			continue
		}
		kept := make([]equivalence, 0, len(eqs)-counts[c])
		for i, e := range eqs {
			if int(class[i]) != c {
				kept = append(kept, e)
			}
		}
		b, err := reencodeEq(ep, kept, pred)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  blocksidecar: re-encode without %s failed: %v\n", classNames[c], err)
			continue
		}
		marginal[c] = base - xzSizeContiguous(b)
	}
	fmt.Fprintf(os.Stderr, "  PHASE 1 anatomy of the equivalence stream (%d runs, %d xz -T1):\n", len(eqs), base)
	fmt.Fprintf(os.Stderr, "  %-28s %10s %14s %12s %10s\n", "class", "runs", "bytes covered", "marginal xz", "xz/run")
	for c := 0; c < numClasses; c++ {
		per := 0.0
		if counts[c] > 0 {
			per = float64(marginal[c]) / float64(counts[c])
		}
		fmt.Fprintf(os.Stderr, "  %-28s %10d %14d %12d %10.2f\n", classNames[c], counts[c], covered[c], marginal[c], per)
	}
	if counts[clsIntra] == 0 {
		fmt.Fprintln(os.Stderr, "  PHASE 1 STOP: no intra-function moves at all; the block sidecar has no target")
		return
	}

	// --- phase 2: do blocks explain the class-(b) moves? ------------------
	affected := map[int32]bool{}
	for i := range eqs {
		if class[i] == clsIntra {
			affected[fnOf[i]] = true
		}
	}
	fnList := make([]int32, 0, len(affected))
	for f := range affected {
		fnList = append(fnList, f)
	}
	slices.Sort(fnList)
	t := startStage("blocksidecar blocks")
	newB := make([][]int32, len(fnList))
	oldB := make([][]int32, len(fnList))
	parallelDo(len(fnList), func(k int) {
		m := maps[fnList[k]]
		newB[k] = blockBounds(newText[m.Dst : m.Dst+m.DstSize])
		oldB[k] = blockBounds(oldText[m.Src : m.Src+m.SrcSize])
	})
	slot := make(map[int32]int, len(fnList))
	var blocksNew, blocksOld int
	for k, f := range fnList {
		slot[f] = k
		blocksNew += len(newB[k]) - 1
		blocksOld += len(oldB[k]) - 1
	}
	t.done("%d functions carrying intra-function moves; %d new blocks, %d old blocks", len(fnList), blocksNew, blocksOld)

	onBound := func(b []int32, off int32) bool {
		i := sort.Search(len(b), func(i int) bool { return b[i] >= off })
		return i < len(b) && b[i] == off
	}
	var bothStart, wholeBlock int
	var bothStartBytes, wholeBlockBytes, alignedBlockBytes uint64
	explained := make([]bool, len(eqs))
	var blockSizes []int
	for i, e := range eqs {
		if class[i] != clsIntra {
			continue
		}
		k := slot[fnOf[i]]
		m := maps[fnOf[i]]
		dRel := int32(e.Dst - ep.NewText.Off - m.Dst)
		sRel := int32(e.Src - oldLo - m.Src)
		if !onBound(newB[k], dRel) || !onBound(oldB[k], sRel) {
			continue
		}
		bothStart++
		bothStartBytes += e.N
		// A looser reading of the same question: not whether the run ends on a
		// boundary but how many complete new blocks it contains. This is the
		// most a block permutation could ever carry, ignoring that a run only
		// partly carried still has to ship an equivalence row.
		hi := int32(min(uint64(dRel)+e.N, m.DstSize))
		lo := sort.Search(len(newB[k]), func(x int) bool { return newB[k][x] >= dRel })
		for x := lo; x+1 < len(newB[k]) && newB[k][x+1] <= hi; x++ {
			alignedBlockBytes += uint64(newB[k][x+1] - newB[k][x])
		}
		// Whole blocks: the run's end must be a boundary on both sides too,
		// and the run must not spill out of either function.
		dEnd, sEnd := dRel+int32(e.N), sRel+int32(e.N)
		if dEnd > int32(m.DstSize) || sEnd > int32(m.SrcSize) {
			continue
		}
		if !onBound(newB[k], dEnd) || !onBound(oldB[k], sEnd) {
			continue
		}
		wholeBlock++
		wholeBlockBytes += e.N
		explained[i] = true
	}
	for k := range fnList {
		for j := 0; j+1 < len(newB[k]); j++ {
			blockSizes = append(blockSizes, int(newB[k][j+1]-newB[k][j]))
		}
	}
	slices.Sort(blockSizes)
	q := func(f float64) int {
		if len(blockSizes) == 0 {
			return 0
		}
		return blockSizes[min(len(blockSizes)-1, int(f*float64(len(blockSizes))))]
	}
	pctB := func(n, d uint64) float64 { return 100 * float64(n) / float64(max(1, d)) }
	fmt.Fprintf(os.Stderr, "  PHASE 2 block explanation of class (b): %d of %d runs start on a block boundary on both sides (%d B, %.2f%% of class-b bytes); %d cover whole blocks (%d B, %.2f%%)\n",
		bothStart, counts[clsIntra], bothStartBytes, pctB(bothStartBytes, covered[clsIntra]),
		wholeBlock, wholeBlockBytes, pctB(wholeBlockBytes, covered[clsIntra]))
	var affectedBytes uint64
	for _, f := range fnList {
		affectedBytes += maps[f].DstSize
	}
	fmt.Fprintf(os.Stderr, "  PHASE 2 blocks in the %d affected functions (%d B of new .text): %d new blocks, %.2f per function; sizes p10 %d, median %d, p90 %d, mean %.1f\n",
		len(fnList), affectedBytes, blocksNew, float64(blocksNew)/float64(max(1, len(fnList))),
		q(0.1), q(0.5), q(0.9), float64(affectedBytes)/float64(max(1, blocksNew)))
	fmt.Fprintf(os.Stderr, "  PHASE 2 loosest reading: complete new blocks inside a both-side-aligned class-b run cover %d B (%.2f%% of class-b bytes), but a partly-carried run still ships an equivalence row\n",
		alignedBlockBytes, pctB(alignedBlockBytes, covered[clsIntra]))
	if wholeBlockBytes*2 < covered[clsIntra] {
		// Before stopping, put a number on what proceeding could have won:
		// the marginal cost of exactly the rows a block stream could delete,
		// against the number of blocks it would have to ship to delete them.
		kept := make([]equivalence, 0, len(eqs))
		ship := map[int32]bool{}
		for i, e := range eqs {
			if explained[i] {
				ship[fnOf[i]] = true
				continue
			}
			kept = append(kept, e)
		}
		ceiling := 0
		if b, err := reencodeEq(ep, kept, pred); err == nil {
			ceiling = base - xzSizeContiguous(b)
		}
		shipBlocks := 0
		for f := range ship {
			shipBlocks += len(newB[slot[f]]) - 1
		}
		fmt.Fprintf(os.Stderr, "  PHASE 2 STOP: whole-block coverage is %.2f%% of class-b bytes, under half; the block model does not explain these moves\n",
			pctB(wholeBlockBytes, covered[clsIntra]))
		fmt.Fprintf(os.Stderr, "  PHASE 2 STOP: deleting exactly the %d explained rows saves %d xz marginally, and the block stream that would have to replace them spans %d functions and %d new blocks -- %.3f bytes per shipped block is the break-even, which no per-block record reaches\n",
			wholeBlock, ceiling, len(ship), shipBlocks, float64(ceiling)/float64(max(1, shipBlocks)))
		return
	}

	// --- phase 3: price the sidecar coding --------------------------------
	probeBlockSidecarPricing(ep, eqs, class, fnOf, explained, pred, maps, oldText, newText,
		fnList, slot, oldB, newB, base, marginal[clsIntra], covered, counts, blocksNew)
}

// blockMatch joins old and new blocks of one function. Pessimistic pass:
// canonical content hash (PC-relative fields zeroed), each old block claimed
// once, nearest index winning. Optimistic pass: whatever the hash could not
// join is joined by relative order, which is what a real BB-addr-map with
// stable IDs would still answer after the block's bytes changed.
func blockMatch(oldCode, newCode []byte, oldB, newB []int32, optimistic bool) []int {
	hashes := make(map[uint64][]int, len(oldB))
	for j := 0; j+1 < len(oldB); j++ {
		h := x86.ContentHash(oldCode[oldB[j]:oldB[j+1]])
		hashes[h] = append(hashes[h], j)
	}
	used := make([]bool, max(0, len(oldB)-1))
	out := make([]int, max(0, len(newB)-1))
	for i := range out {
		out[i] = -1
	}
	for i := 0; i+1 < len(newB); i++ {
		h := x86.ContentHash(newCode[newB[i]:newB[i+1]])
		best := -1
		for _, j := range hashes[h] {
			if used[j] {
				continue
			}
			if best < 0 || abs(j-i) < abs(best-i) {
				best = j
			}
		}
		if best >= 0 {
			used[best] = true
			out[i] = best
		}
	}
	if optimistic {
		free := make([]int, 0, len(used))
		for j, u := range used {
			if !u {
				free = append(free, j)
			}
		}
		k := 0
		for i := range out {
			if out[i] < 0 && k < len(free) {
				out[i] = free[k]
				k++
			}
		}
	}
	return out
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func probeBlockSidecarPricing(ep equivalencePlan, eqs []equivalence, class []uint8, fnOf []int32,
	explained []bool, pred *srcPredictor, maps []mapping, oldText, newText []byte,
	fnList []int32, slot map[int32]int, oldB, newB [][]int32, base, intraMarginal int,
	covered [numClasses]uint64, counts [numClasses]int, blocksNew int) {

	// The rows the block stream would actually take over are the explained
	// ones; the rest keep their place in the equivalence stream. Price that
	// subset marginally, the same way phase 1 priced the whole class.
	var explainedRuns int
	kept := make([]equivalence, 0, len(eqs))
	shipFns := map[int32]bool{}
	for i, e := range eqs {
		if explained[i] {
			explainedRuns++
			shipFns[fnOf[i]] = true
			continue
		}
		kept = append(kept, e)
	}
	b, err := reencodeEq(ep, kept, pred)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  blocksidecar: re-encode without the explained rows failed: %v\n", err)
		return
	}
	explainedMarginal := base - xzSizeContiguous(b)

	ship := make([]int32, 0, len(shipFns))
	for f := range shipFns {
		ship = append(ship, f)
	}
	slices.Sort(ship)

	type built struct {
		funcGap, count, flags, ref, sizeDelta, newSize []byte
		blocks, matched, news                          int
	}
	build := func(optimistic bool) built {
		var out built
		results := make([][]int, len(ship))
		parallelDo(len(ship), func(k int) {
			f := ship[k]
			m := maps[f]
			s := slot[f]
			results[k] = blockMatch(oldText[m.Src:m.Src+m.SrcSize], newText[m.Dst:m.Dst+m.DstSize],
				oldB[s], newB[s], optimistic)
		})
		prevFn := int32(0)
		for k, f := range ship {
			s := slot[f]
			out.funcGap = binary.AppendUvarint(out.funcGap, uint64(f-prevFn))
			prevFn = f
			join := results[k]
			out.count = binary.AppendUvarint(out.count, uint64(len(join)))
			expect := 0
			for i, j := range join {
				out.blocks++
				nsz := int(newB[s][i+1] - newB[s][i])
				if j < 0 {
					out.flags = append(out.flags, 1)
					out.newSize = binary.AppendUvarint(out.newSize, uint64(nsz))
					out.news++
					continue
				}
				out.flags = append(out.flags, 0)
				out.ref = binary.AppendVarint(out.ref, int64(j-expect))
				expect = j + 1
				osz := int(oldB[s][j+1] - oldB[s][j])
				out.sizeDelta = binary.AppendVarint(out.sizeDelta, int64(nsz-osz))
				out.matched++
			}
		}
		return out
	}

	type col struct {
		name string
		b    []byte
	}
	price := func(o built) (int, []col) {
		cols := []col{
			{"function gap", o.funcGap},
			{"blocks per function", o.count},
			{"new-block flag", o.flags},
			{"old-block reference", o.ref},
			{"block size delta", o.sizeDelta},
			{"new-block size", o.newSize},
		}
		var whole []byte
		for _, c := range cols {
			whole = append(whole, c.b...)
		}
		return xzSizeContiguous(whole), cols
	}

	pess := build(false)
	opt := build(true)
	zPess, colsPess := price(pess)
	zOpt, _ := price(opt)
	bs := make([][]byte, len(colsPess))
	raw := 0
	for i, c := range colsPess {
		bs[i] = c.b
		raw += len(c.b)
	}
	z := xzSizes(bs...)

	fmt.Fprintf(os.Stderr, "  PHASE 3 the block stream covers %d functions, %d new blocks; hash join matched %d, left %d new (optimistic: %d matched, %d new)\n",
		len(ship), pess.blocks, pess.matched, pess.news, opt.matched, opt.news)
	fmt.Fprintf(os.Stderr, "  %-26s %10s %10s\n", "column", "raw", "xz alone")
	for i, c := range colsPess {
		fmt.Fprintf(os.Stderr, "  %-26s %10d %10d\n", c.name, len(c.b), z[i])
	}
	fmt.Fprintf(os.Stderr, "  %-26s %10d %10d  (optimistic variant %d)\n", "TOTAL, one contiguous stream", raw, zPess, zOpt)

	// What a real block map costs that this join does not: every function
	// whose body changed at all has to ship its own block-map delta, even
	// when the equivalence stream never named it. Bound it by counting those
	// functions and charging the measured blocks-per-function at ~2 bytes a
	// block. Byte inequality overcounts churn (a function whose only change
	// is a moved branch target counts), so this is an upper bound.
	churned := 0
	for _, m := range maps {
		if m.SrcSize != m.DstSize || !bytes.Equal(oldText[m.Src:m.Src+m.SrcSize], newText[m.Dst:m.Dst+m.DstSize]) {
			churned++
		}
	}
	perFn := float64(pess.blocks) / float64(max(1, len(ship)))
	untouched := max(0, churned-len(ship))
	bound := int(float64(untouched) * perFn * 2)
	fmt.Fprintf(os.Stderr, "  PHASE 3 carried-map upkeep: %d of %d mapped functions changed; %d of them the block stream above never mentions, at %.1f blocks x ~2 B = %d bytes a real BB-addr-map delta must also ship\n",
		churned, len(maps), untouched, perFn, bound)

	share := func(n int) float64 { return 100 * float64(n) / 2_621_664 }
	fmt.Fprintf(os.Stderr, "  VERDICT: class (b) is %d runs / %d bytes / %d xz marginal; the whole-block subset the stream can replace is %d runs / %d xz marginal\n",
		counts[clsIntra], covered[clsIntra], intraMarginal, explainedRuns, explainedMarginal)
	fmt.Fprintf(os.Stderr, "  VERDICT: pessimistic %+d bytes (%+.3f%% of the 2,621,664 patch); optimistic %+d (%+.3f%%); with the carried-map upkeep bound, %+d (%+.3f%%) and %+d (%+.3f%%)\n",
		zPess-explainedMarginal, share(zPess-explainedMarginal),
		zOpt-explainedMarginal, share(zOpt-explainedMarginal),
		zPess+bound-explainedMarginal, share(zPess+bound-explainedMarginal),
		zOpt+bound-explainedMarginal, share(zOpt+bound-explainedMarginal))
	fmt.Fprintf(os.Stderr, "  NOT replaced by the block sidecar: the %d runs of classes (a), (c) and (d), the class-(b) runs no block pair explains, the function map and its columns (the symbol sidecar's -98,460 is disjoint from this and both could ship), the choice bitmap, the field corrections and every byte correction.\n",
		counts[clsPredicted]+counts[clsOther]+counts[clsUnmapped]+(counts[clsIntra]-explainedRuns))
}
