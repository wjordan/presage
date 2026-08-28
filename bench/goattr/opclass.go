// The instruction and operand classification is bench/elfpredict's
// (instdiff.go, operands.go), copied so that the two probes name the same
// classes. Only the classification travels; the probes ask different
// questions of it.
package main

import (
	"slices"

	"golang.org/x/arch/x86/x86asm"
)

// opInst names x86asm.Inst for the tests, which do not import x86asm.
type opInst = x86asm.Inst

func safeDecode(code []byte) (inst x86asm.Inst, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	i, err := x86asm.Decode(code, 64)
	if err != nil || i.Len == 0 {
		return x86asm.Inst{}, false
	}
	return i, true
}

// canonicalise zeroes every PC-relative displacement, so two copies of the
// same code become byte-identical wherever their call targets differ. A raw
// dictionary probe over machine code is close to uninformative without it.
func canonicalise(code []byte) []byte {
	out := append([]byte(nil), code...)
	for i := 0; i < len(out); {
		inst, ok := safeDecode(out[i:])
		if !ok {
			i++
			continue
		}
		if inst.PCRel > 0 && inst.PCRelOff > 0 && i+inst.PCRelOff+inst.PCRel <= len(out) {
			clear(out[i+inst.PCRelOff : i+inst.PCRelOff+inst.PCRel])
		}
		i += inst.Len
	}
	return out
}

const (
	bkSameOpSameLen = iota
	bkSameOpDiffLen
	bkDiffOpSameLen
	bkDiffOpDiffLen
	bkUndecodable
	nBucket
)

var bucketNames = [nBucket]string{
	bkSameOpSameLen: "same op, same length (operand only)",
	bkSameOpDiffLen: "same op, different length",
	bkDiffOpSameLen: "different op, same length",
	bkDiffOpDiffLen: "different op and length",
	bkUndecodable:   "prediction undecodable there",
}

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
