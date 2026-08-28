package main

import (
	"fmt"
	"os"
	"slices"

	"golang.org/x/arch/x86/x86asm"
)

// probeOperandDiff sub-classifies the instruction-level residual by *what*
// differs inside the instruction, rather than by whether the prediction got
// the op and the length right. §11's instruction diff established the shape of
// the residual (a big "different op and length" bucket, a 252,229-instruction
// "same op, same length" bucket) but never asked which *field* moved. Three
// hypotheses about the operand-only bucket have been repeated and never
// measured:
//
//	H1 register renaming -- the compiler allocated a different register and
//	   everything else is identical. If a single per-function bijection
//	   explains a function's substitutions, the correction could ship one
//	   small permutation per function instead of the bytes.
//	H2 stack-frame shift -- a [rsp+d] or [rbp+d] memory operand whose
//	   displacement moved, because a frame grew or shrank. If one delta per
//	   function explains most sites, that is one integer per function.
//	H3 struct-offset relabel -- a [reg+d] operand on some other base whose
//	   displacement moved, because a field's offset in a struct changed. That
//	   is image-spanning: the same (old,new) pair should recur at every use of
//	   the field, so a small dictionary of pairs would cover many sites.
//
// The walk is instdiff's: the prediction and the target share new-image
// coordinates, so the target instruction decoded at offset s is compared with
// whatever the prediction decodes at offset s. Report only; nothing here
// touches the shipped format.
func probeOperandDiff(predText, targetText []byte, maps []mapping) {
	var tab [nBucket][nOpClass]opCount

	// H1: register renaming.
	var h1All, h1Explained, h1Recurrent opCount
	var h1LenSame, h1LenOne, h1LenOther, h1REX opCount
	var h1Dense, h1DenseExplained opCount
	h1Funcs, h1MapEntries := 0, 0

	// H2: stack-frame shift.
	var h2All opCount
	var h2Top1, h2Top3 int
	var h2Dense, h2DenseTop1, h2DenseTop3 int
	h2Funcs := 0

	// H3: struct-offset relabel, image-spanning.
	var h3All opCount
	h3Pairs := map[[2]int64]int{}

	type site struct {
		class int
		wrong int
		pairs []regPair
		delta int64
	}
	var sites []site

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
		sites = sites[:0]
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
				tab[bkUndecodable][clsUndecodable].insts++
				tab[bkUndecodable][clsUndecodable].wrong += w
				s = end
				continue
			}
			bk := bkDiffOpDiffLen
			switch {
			case pi.Op == ti.Op && pi.Len == ti.Len:
				bk = bkSameOpSameLen
			case pi.Op == ti.Op:
				bk = bkSameOpDiffLen
			case pi.Len == ti.Len:
				bk = bkDiffOpSameLen
			}
			f := compareOperands(pi, ti)
			c := f.class()
			tab[bk][c].insts++
			tab[bk][c].wrong += w
			st := site{class: c, wrong: w}
			switch c {
			case clsReg:
				st.pairs = f.regs
				h1All.insts++
				h1All.wrong += w
				switch d := ti.Len - pi.Len; {
				case d == 0:
					h1LenSame.insts++
					h1LenSame.wrong += w
				case d == 1 || d == -1:
					h1LenOne.insts++
					h1LenOne.wrong += w
					if crossesREX(f.regs) {
						h1REX.insts++
						h1REX.wrong += w
					}
				default:
					h1LenOther.insts++
					h1LenOther.wrong += w
				}
			case clsDispSP:
				st.delta = f.disps[0][1] - f.disps[0][0]
				h2All.insts++
				h2All.wrong += w
			case clsDispOther:
				h3All.insts++
				h3All.wrong += w
				h3Pairs[[2]int64{f.disps[0][0], f.disps[0][1]}]++
			}
			if c == clsReg || c == clsDispSP {
				sites = append(sites, st)
			}
			s = end
		}

		// H1: fit one bijection per function, greedily by pair frequency, and
		// ask how many of the function's sites it explains.
		pairCount := map[regPair]int{}
		for _, st := range sites {
			for _, p := range st.pairs {
				pairCount[p]++
			}
		}
		if len(pairCount) > 0 {
			h1Funcs++
			ranked := make([]regPair, 0, len(pairCount))
			for p := range pairCount {
				ranked = append(ranked, p)
			}
			slices.SortFunc(ranked, func(a, b regPair) int {
				if d := pairCount[b] - pairCount[a]; d != 0 {
					return d
				}
				if d := a.fam - b.fam; d != 0 {
					return d
				}
				if d := a.from - b.from; d != 0 {
					return d
				}
				return a.to - b.to
			})
			type half struct{ fam, reg int }
			fit := map[half]int{}
			taken := map[half]bool{}
			for _, p := range ranked {
				src, dst := half{p.fam, p.from}, half{p.fam, p.to}
				if _, ok := fit[src]; ok || taken[dst] {
					continue
				}
				fit[src] = p.to
				taken[dst] = true
			}
			h1MapEntries += len(fit)
			nH1 := 0
			for _, st := range sites {
				if st.class == clsReg {
					nH1++
				}
			}
			for _, st := range sites {
				if st.class != clsReg {
					continue
				}
				explained, recurrent := true, true
				for _, p := range st.pairs {
					if to, ok := fit[half{p.fam, p.from}]; !ok || to != p.to {
						explained = false
					}
					if pairCount[p] < 2 {
						recurrent = false
					}
				}
				if explained {
					h1Explained.insts++
					h1Explained.wrong += st.wrong
				}
				if nH1 >= denseFunc {
					h1Dense.insts++
					h1Dense.wrong += st.wrong
					if explained {
						h1DenseExplained.insts++
						h1DenseExplained.wrong += st.wrong
					}
				}
				if recurrent {
					h1Recurrent.insts++
					h1Recurrent.wrong += st.wrong
				}
			}
		}

		// H2: histogram this function's displacement deltas.
		deltas := map[int64]int{}
		for _, st := range sites {
			if st.class == clsDispSP {
				deltas[st.delta]++
			}
		}
		if len(deltas) > 0 {
			h2Funcs++
			counts := make([]int, 0, len(deltas))
			for _, n := range deltas {
				counts = append(counts, n)
			}
			slices.SortFunc(counts, func(a, b int) int { return b - a })
			h2Top1 += counts[0]
			top3, n := 0, 0
			for i := 0; i < len(counts) && i < 3; i++ {
				top3 += counts[i]
			}
			for _, c := range counts {
				n += c
			}
			h2Top3 += top3
			if n >= denseFunc {
				h2Dense += n
				h2DenseTop1 += counts[0]
				h2DenseTop3 += top3
			}
		}
	}

	total := 0
	for b := range tab {
		for c := range tab[b] {
			total += tab[b][c].wrong
		}
	}
	fmt.Fprintf(os.Stderr, "  probe operand diff: %d wrong bytes classified\n", total)
	printOpClassTable(os.Stderr, "all differing instructions", sumBuckets(tab[:]))
	printOpClassTable(os.Stderr, "within \"same op, same length (operand only)\"", tab[bkSameOpSameLen])
	printOpClassTable(os.Stderr, "within \"same op, different length\"", tab[bkSameOpDiffLen])
	printOpClassTable(os.Stderr, "within \"different op, same length\"", tab[bkDiffOpSameLen])
	printOpClassTable(os.Stderr, "within \"different op and length\"", tab[bkDiffOpDiffLen])

	pct := func(n, d int) float64 { return 100 * float64(n) / float64(max(1, d)) }
	fmt.Fprintf(os.Stderr, "    H1 register renaming: %d instructions, %d wrong bytes, in %d functions, %d mapping entries\n",
		h1All.insts, h1All.wrong, h1Funcs, h1MapEntries)
	for _, r := range []struct {
		name string
		c    opCount
	}{
		{"explained by function's best-fit bijection", h1Explained},
		{"not explained by it", opCount{h1All.insts - h1Explained.insts, h1All.wrong - h1Explained.wrong}},
		{"every pair recurs in its function", h1Recurrent},
		{"length unchanged", h1LenSame},
		{"length differs by 1", h1LenOne},
		{"  of which a legacy<->r8-r15 swap (REX)", h1REX},
		{"length differs by more", h1LenOther},
		{"in functions with >=5 H1 sites", h1Dense},
		{"  of those, explained by the bijection", h1DenseExplained},
	} {
		fmt.Fprintf(os.Stderr, "      %-42s %9d instructions, %9d wrong bytes (%.1f%%)\n",
			r.name, r.c.insts, r.c.wrong, pct(r.c.insts, h1All.insts))
	}

	fmt.Fprintf(os.Stderr, "    H2 stack-frame shift: %d instructions, %d wrong bytes, in %d functions\n",
		h2All.insts, h2All.wrong, h2Funcs)
	fmt.Fprintf(os.Stderr, "      explained by the function's single most common delta: %d (%.1f%%)\n",
		h2Top1, pct(h2Top1, h2All.insts))
	fmt.Fprintf(os.Stderr, "      explained by the function's top 3 deltas:              %d (%.1f%%)\n",
		h2Top3, pct(h2Top3, h2All.insts))
	fmt.Fprintf(os.Stderr, "      in functions with >=5 H2 sites: %d sites, top delta %d (%.1f%%), top 3 %d (%.1f%%)\n",
		h2Dense, h2DenseTop1, pct(h2DenseTop1, h2Dense), h2DenseTop3, pct(h2DenseTop3, h2Dense))

	ranked := make([]int, 0, len(h3Pairs))
	for _, n := range h3Pairs {
		ranked = append(ranked, n)
	}
	slices.SortFunc(ranked, func(a, b int) int { return b - a })
	fmt.Fprintf(os.Stderr, "    H3 struct-offset relabel: %d instructions, %d wrong bytes, %d distinct (old,new) displacement pairs\n",
		h3All.insts, h3All.wrong, len(ranked))
	cum, k := 0, 0
	for _, n := range []int{10, 100, 1000} {
		for ; k < n && k < len(ranked); k++ {
			cum += ranked[k]
		}
		fmt.Fprintf(os.Stderr, "      top %4d pairs cover %8d sites (%.1f%%)\n", n, cum, pct(cum, h3All.insts))
	}
}

// denseFunc is the per-function site count above which a fitted per-function
// model (a permutation, a frame delta) is worth its own plan entry. Below it
// the fit is trivially perfect and says nothing.
const denseFunc = 5

// Buckets mirror probeInstructionDiff's, so the sub-classification can be read
// against the table that motivated it.
const (
	bkSameOpSameLen = iota
	bkSameOpDiffLen
	bkDiffOpSameLen
	bkDiffOpDiffLen
	bkUndecodable
	nBucket
)

// Classes name the single field that moved, or say that more than one did.
const (
	clsReg = iota
	clsDispSP
	clsDispOther
	clsDispRIP
	clsDispAbs
	clsImm
	clsRel
	clsOpOnly
	clsWidth
	clsMulti
	clsShape
	clsSameText
	clsUndecodable
	nOpClass
)

var opClassNames = [nOpClass]string{
	clsReg:         "registers only (H1)",
	clsDispSP:      "displacement only, rsp/rbp base (H2)",
	clsDispOther:   "displacement only, other reg base (H3)",
	clsDispRIP:     "displacement only, rip-relative",
	clsDispAbs:     "displacement only, no base register",
	clsImm:         "immediate only",
	clsRel:         "branch target only",
	clsOpOnly:      "op only, operands identical",
	clsWidth:       "operand width only (REX.W etc)",
	clsMulti:       "several fields differ",
	clsShape:       "operand shape or prefixes differ",
	clsSameText:    "identical disassembly, different bytes",
	clsUndecodable: "prediction undecodable there",
}

// opCount is one cell of the class tables: how many instructions fell in a
// class, and how many of the residual's wrong bytes they hold.
type opCount struct{ insts, wrong int }

func sumBuckets(tab [][nOpClass]opCount) [nOpClass]opCount {
	var out [nOpClass]opCount
	for _, b := range tab {
		for c := range b {
			out[c].insts += b[c].insts
			out[c].wrong += b[c].wrong
		}
	}
	return out
}

func printOpClassTable(w *os.File, title string, row [nOpClass]opCount) {
	insts, wrong := 0, 0
	for _, c := range row {
		insts += c.insts
		wrong += c.wrong
	}
	fmt.Fprintf(w, "    %s: %d instructions, %d wrong bytes\n", title, insts, wrong)
	for c, name := range opClassNames {
		if row[c].insts == 0 {
			continue
		}
		fmt.Fprintf(w, "      %-40s %9d instructions, %9d wrong bytes (%.1f%%)\n",
			name, row[c].insts, row[c].wrong, 100*float64(row[c].wrong)/float64(max(1, wrong)))
	}
}

// regPair is one substitution, canonicalised so that a rename shows up the
// same whatever operand width the instruction used: eax->ebx and rax->rbx are
// the same pair.
type regPair struct{ fam, from, to int }

const (
	famGP = iota
	famHighByte
	famVec
	famMask
	famIP
	famSeg
	famOther
)

// canonReg splits a register into (family, architectural number, width). The
// width is kept out of the pair so that a rename is one pair, but compared
// separately so that a REX.W flip is not mistaken for one.
func canonReg(r x86asm.Reg) (fam, idx, width int) {
	switch {
	case r >= x86asm.AL && r <= x86asm.BL:
		return famGP, int(r - x86asm.AL), 8
	case r >= x86asm.AH && r <= x86asm.BH:
		return famHighByte, int(r - x86asm.AH), 8
	case r >= x86asm.SPB && r <= x86asm.DIB:
		return famGP, 4 + int(r-x86asm.SPB), 8
	case r >= x86asm.R8B && r <= x86asm.R15B:
		return famGP, 8 + int(r-x86asm.R8B), 8
	case r >= x86asm.AX && r <= x86asm.R15W:
		return famGP, int(r - x86asm.AX), 16
	case r >= x86asm.EAX && r <= x86asm.R15L:
		return famGP, int(r - x86asm.EAX), 32
	case r >= x86asm.RAX && r <= x86asm.R15:
		return famGP, int(r - x86asm.RAX), 64
	case r >= x86asm.X0 && r <= x86asm.X31:
		return famVec, int(r - x86asm.X0), 128
	case r >= x86asm.Y0 && r <= x86asm.Y31:
		return famVec, int(r - x86asm.Y0), 256
	case r >= x86asm.Z0 && r <= x86asm.Z31:
		return famVec, int(r - x86asm.Z0), 512
	case r >= x86asm.K0 && r <= x86asm.K7:
		return famMask, int(r - x86asm.K0), 64
	case r >= x86asm.IP && r <= x86asm.RIP:
		return famIP, 0, 16 << (r - x86asm.IP)
	case r >= x86asm.ES && r <= x86asm.GS:
		return famSeg, int(r - x86asm.ES), 16
	}
	return famOther, int(r), 0
}

func crossesREX(pairs []regPair) bool {
	for _, p := range pairs {
		if p.fam == famGP && (p.from < 8) != (p.to < 8) {
			return true
		}
	}
	return false
}

// operandDiff records, field by field, how the prediction's instruction and
// the target's differ.
type operandDiff struct {
	op    bool
	width bool
	shape bool
	imm   int
	rel   int
	regs  []regPair
	disps [][2]int64 // old, new
	base  x86asm.Reg // base register of the first differing displacement
}

func (f *operandDiff) addReg(p, t x86asm.Reg) {
	pf, pi, pw := canonReg(p)
	tf, ti, tw := canonReg(t)
	if pw != tw || pf != tf {
		f.width = true
		if pf != tf {
			f.shape = true
		}
	}
	if pf == tf && pi != ti {
		f.regs = append(f.regs, regPair{pf, pi, ti})
	} else if pf != tf {
		f.shape = true
	}
}

func argCount(i x86asm.Inst) int {
	n := 0
	for _, a := range i.Args {
		if a == nil {
			break
		}
		n++
	}
	return n
}

// meaningfulPrefixes drops the prefixes that only restate what the operands
// already say: REX/VEX/EVEX carry the register numbers and widths compared
// above, and implicit or ignored prefixes are not in the encoding's meaning.
func meaningfulPrefixes(i x86asm.Inst) []x86asm.Prefix {
	var out []x86asm.Prefix
	for _, p := range i.Prefix {
		if p == 0 {
			break
		}
		if p&(x86asm.PrefixImplicit|x86asm.PrefixIgnored) != 0 || p.IsREX() || p.IsVEX() || p.IsEVEX() {
			continue
		}
		out = append(out, p&0xff)
	}
	slices.Sort(out)
	return out
}

func compareOperands(pi, ti x86asm.Inst) operandDiff {
	var f operandDiff
	f.op = pi.Op != ti.Op
	if !slices.Equal(meaningfulPrefixes(pi), meaningfulPrefixes(ti)) {
		f.shape = true
	}
	np, nt := argCount(pi), argCount(ti)
	if np != nt {
		f.shape = true
		return f
	}
	for i := 0; i < np; i++ {
		switch pv := pi.Args[i].(type) {
		case x86asm.Reg:
			tv, ok := ti.Args[i].(x86asm.Reg)
			if !ok {
				f.shape = true
				continue
			}
			if pv != tv {
				f.addReg(pv, tv)
			}
		case x86asm.Mem:
			tv, ok := ti.Args[i].(x86asm.Mem)
			if !ok {
				f.shape = true
				continue
			}
			if pv.Segment != tv.Segment {
				f.shape = true
			}
			if (pv.Index != 0 || tv.Index != 0) && pv.Scale != tv.Scale {
				f.shape = true
			}
			if pv.Base != tv.Base {
				f.addReg(pv.Base, tv.Base)
			}
			if pv.Index != tv.Index {
				f.addReg(pv.Index, tv.Index)
			}
			if pv.Disp != tv.Disp {
				if len(f.disps) == 0 {
					f.base = tv.Base
				}
				f.disps = append(f.disps, [2]int64{pv.Disp, tv.Disp})
			}
		case x86asm.Imm:
			tv, ok := ti.Args[i].(x86asm.Imm)
			if !ok {
				f.shape = true
				continue
			}
			if pv != tv {
				f.imm++
			}
		case x86asm.Rel:
			tv, ok := ti.Args[i].(x86asm.Rel)
			if !ok {
				f.shape = true
				continue
			}
			if pv != tv {
				f.rel++
			}
		default:
			if pi.Args[i] != ti.Args[i] {
				f.shape = true
			}
		}
	}
	return f
}

func (f operandDiff) class() int {
	if f.shape {
		return clsShape
	}
	cats := 0
	for _, on := range []bool{f.op, f.width, len(f.regs) > 0, len(f.disps) > 0, f.imm > 0, f.rel > 0} {
		if on {
			cats++
		}
	}
	switch {
	case cats == 0:
		return clsSameText
	case cats > 1:
		return clsMulti
	case len(f.regs) > 0:
		return clsReg
	case len(f.disps) > 0:
		fam, idx, _ := canonReg(f.base)
		switch {
		case f.base == 0:
			return clsDispAbs
		case fam == famIP:
			return clsDispRIP
		case fam == famGP && (idx == 4 || idx == 5):
			return clsDispSP
		default:
			return clsDispOther
		}
	case f.imm > 0:
		return clsImm
	case f.rel > 0:
		return clsRel
	case f.width:
		return clsWidth
	default:
		return clsOpOnly
	}
}
