package main

import (
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"slices"
	"sync"

	"golang.org/x/arch/x86/x86asm"
)

// probeRegNormalDictionary re-runs §11.5's dictionary question under a
// stronger canonicalisation. §11.5 zeroed PC-relative fields, which is the
// weakest transform that makes machine code self-match at all: two copies of
// the same function still differ if the register allocator picked different
// registers, or if a struct offset or an immediate moved. Those differences
// are exactly what §15 measured as real and per-function consistent, so the
// dictionary question deserves to be asked once more with them normalised
// away.
//
// Five forms of the same measurement, each applied identically to the
// long-run content and to the 64 MiB dictionary drawn from the predicted
// image:
//
//	raw          the bytes as shipped
//	pc-relative  §11.5: zero every PC-relative field (reproduces 299,220)
//	all fields   zero every displacement and immediate, keeping length
//	rnf/bank     all fields, plus registers renumbered by order of first
//	             appearance within the low bank and within the high bank
//	rnf/full     all fields, plus one renumbering over all sixteen
//
// The bank-preserving form is exact: a register that needed a REX extension
// bit still gets one, so every rewrite is encodable and no two registers
// collide. The full form is stronger but lossy -- an instruction with no REX
// prefix has nowhere to put the fourth bit, so two registers can collapse to
// the same encoding. Both directions of that bias favour *finding*
// redundancy, so a null result under rnf/full is the stronger null.
//
// Report only. Nothing here touches the shipped format, and no decoder would
// have to reproduce the renumbering: this asks whether the residual's content
// exists elsewhere in the image at all, not how to reference it.
func probeRegNormalDictionary(predText, targetText []byte, maps []mapping) {
	const window = 64 << 20

	// Instruction-boundary restarts for the dictionary walk: the numbering
	// must restart at the same places on both sides, and the content side
	// restarts at each function start because it is transformed function by
	// function.
	lo := max(len(predText)-window, 0)
	var bounds []int
	for _, m := range maps {
		if int(m.Dst) > lo && int(m.Dst) < len(predText) {
			bounds = append(bounds, int(m.Dst)-lo)
		}
	}
	slices.Sort(bounds)
	bounds = slices.Compact(bounds)

	var checked, agreed int
	type row struct {
		name  string
		mode  int
		runs  int
		long  []byte
		dict  []byte
		alone int
		base  int
		with  int
	}
	rows := []*row{
		{name: "raw", mode: modeRaw},
		{name: "PC-relative zeroed (§11.5)", mode: modePCRel},
		{name: "all fields zeroed", mode: modeAllFields},
		{name: "register normal form (bank)", mode: modeRNFBank},
		{name: "register normal form (full)", mode: modeRNFFull},
	}
	for _, r := range rows {
		r.runs, r.long = longRuns(predText, targetText, maps, r.mode)
		r.dict = transformSegments(predText[lo:], r.mode, bounds, &checked, &agreed)
	}

	// One xz per stream, run concurrently: each 64 MiB compression is ~28s
	// single-threaded, and -T1 is mandatory (a multithreaded xz codes blocks
	// independently and hides the cross-boundary matches this asks about).
	var jobs [][]byte
	for _, r := range rows {
		jobs = append(jobs, r.long, r.dict, append(slices.Clone(r.dict), r.long...))
	}
	sizes := parallelXZContiguous(jobs)
	for i, r := range rows {
		r.alone, r.base, r.with = sizes[3*i], sizes[3*i+1], sizes[3*i+2]
	}

	fmt.Fprintf(os.Stderr, "  probe rnfdict: dictionary = last %d bytes of the predicted .text, xz -9e -T1\n", len(rows[0].dict))
	fmt.Fprintf(os.Stderr, "    field locator agrees with x86asm PCRelOff on %d of %d sites (%.4f%%)\n",
		agreed, checked, 100*float64(agreed)/float64(max(checked, 1)))
	fmt.Fprintf(os.Stderr, "    %-30s %10s %10s %10s %12s %8s\n", "", "long runs", "bytes", "alone xz", "marginal", "gain")
	for _, r := range rows {
		marginal := r.with - r.base
		fmt.Fprintf(os.Stderr, "    %-30s %10d %10d %10d %12d %7.1f%%\n",
			r.name, r.runs, len(r.long), r.alone, marginal,
			100*(1-float64(marginal)/float64(max(r.alone, 1))))
	}
}

// probeDictionaryCalibration is §9.17's control. A dictionary probe that
// cannot show a large effect on a case that has one is not evidence of
// anything, so the same code path is pointed at fragments that are known to
// be duplicated: 440,666 bytes drawn as 5-40 byte pieces of an 8 MB random
// dictionary. §9.17 measured 437,492 alone, 71,144 marginal, a 6.1x effect.
func probeDictionaryCalibration() {
	rng := rand.New(rand.NewSource(1))
	dict := make([]byte, 8<<20)
	for i := range dict {
		dict[i] = byte(rng.Intn(256))
	}
	var frags []byte
	for len(frags) < 440666 {
		n := 5 + rng.Intn(36)
		off := rng.Intn(len(dict) - n)
		frags = append(frags, dict[off:off+n]...)
	}
	sizes := parallelXZContiguous([][]byte{frags, dict, append(slices.Clone(dict), frags...)})
	alone, base, with := sizes[0], sizes[1], sizes[2]
	fmt.Fprintf(os.Stderr, "  probe rnfdict calibration: %d bytes of 5-40 byte fragments of an %d-byte random dictionary; alone xz %d, marginal %d (%.1fx)\n",
		len(frags), len(dict), alone, with-base, float64(alone)/float64(max(with-base, 1)))
}

// longRuns extracts the correction's long-run bucket -- runs of five wrong
// bytes and up, which is where newly emitted instructions land -- after
// applying mode to both sides of every mapping. Function by function, so the
// register numbering restarts at each function start on this side too.
func longRuns(predText, targetText []byte, maps []mapping, mode int) (int, []byte) {
	type out struct {
		runs int
		b    []byte
	}
	parts := make([]out, len(maps))
	parallelFor(len(maps), func(i int) {
		m := maps[i]
		if m.Dst+m.DstSize > uint64(len(targetText)) {
			return
		}
		p := transformSegments(predText[m.Dst:m.Dst+m.DstSize], mode, nil, nil, nil)
		t := transformSegments(targetText[m.Dst:m.Dst+m.DstSize], mode, nil, nil, nil)
		for j := 0; j < len(t); {
			if p[j] == t[j] {
				j++
				continue
			}
			k := j
			for k < len(t) && p[k] != t[k] {
				k++
			}
			if k-j >= correctionBuckets {
				parts[i].b = append(parts[i].b, t[j:k]...)
				parts[i].runs++
			}
			j = k
		}
	})
	runs, n := 0, 0
	for _, p := range parts {
		runs += p.runs
		n += len(p.b)
	}
	long := make([]byte, 0, n)
	for _, p := range parts {
		long = append(long, p.b...)
	}
	return runs, long
}

const (
	modeRaw = iota
	modePCRel
	modeAllFields
	modeRNFBank
	modeRNFFull
)

// transformSegments applies mode to code, restarting the instruction walk and
// the register numbering at each offset in bounds. checked and agreed, when
// non-nil, accumulate the field locator's self-check.
func transformSegments(code []byte, mode int, bounds []int, checked, agreed *int) []byte {
	out := slices.Clone(code)
	if mode == modeRaw {
		return out
	}
	starts := append([]int{0}, bounds...)
	if len(starts) == 1 {
		c, a := transformSegment(out, mode)
		if checked != nil {
			*checked += c
			*agreed += a
		}
		return out
	}
	var mu sync.Mutex
	parallelFor(len(starts), func(i int) {
		hi := len(out)
		if i+1 < len(starts) {
			hi = starts[i+1]
		}
		c, a := transformSegment(out[starts[i]:hi], mode)
		if checked != nil {
			mu.Lock()
			*checked += c
			*agreed += a
			mu.Unlock()
		}
	})
	return out
}

func transformSegment(buf []byte, mode int) (checked, agreed int) {
	var gp, vec rankTable
	gp.full = mode == modeRNFFull
	vec.full = gp.full
	gp.reset()
	vec.reset()
	for i := 0; i < len(buf); {
		inst, ok := safeDecode(buf[i:])
		if !ok {
			i++
			continue
		}
		if i+inst.Len > len(buf) {
			break
		}
		if mode == modePCRel {
			// Byte-for-byte what canonicalise() does, so this row
			// reproduces §11.5 exactly.
			if inst.PCRel > 0 && inst.PCRelOff > 0 && i+inst.PCRelOff+inst.PCRel <= len(buf) {
				clear(buf[i+inst.PCRelOff : i+inst.PCRelOff+inst.PCRel])
			}
			i += inst.Len
			continue
		}
		f, fok := locateFields(buf[i : i+inst.Len])
		if !fok {
			i += inst.Len
			continue
		}
		if inst.PCRel > 0 && inst.PCRelOff > 0 {
			checked++
			if (f.dispLen == inst.PCRel && f.dispOff == inst.PCRelOff) ||
				(f.immLen == inst.PCRel && f.immOff == inst.PCRelOff) {
				agreed++
			}
		}
		if mode >= modeRNFBank {
			isVec := false
			for _, a := range inst.Args {
				r, ok := a.(x86asm.Reg)
				if !ok {
					continue
				}
				if fam, _, _ := canonReg(r); fam == famVec || fam == famMask {
					isVec = true
					break
				}
			}
			f.renumber(buf[i:i+inst.Len], &gp, &vec, isVec)
		}
		clear(buf[i+f.dispOff : i+f.dispOff+f.dispLen])
		clear(buf[i+f.immOff : i+f.immOff+f.immLen])
		i += inst.Len
	}
	return checked, agreed
}

// instFields is where each rewritable field sits inside one instruction.
// x86asm decodes operands but does not say where they were encoded, so the
// offsets are recovered by walking the encoding: prefixes, opcode, modrm, sib,
// displacement -- and then the immediate is whatever is left, which is what
// makes an immediate-size table unnecessary. Offsets are relative to the
// instruction, and -1 means the field is absent.
type instFields struct {
	op1, op2 byte
	opLen    int
	rexOff   int
	vexOff   int
	vexKind  int // 0 none, 2 VEX2, 3 VEX3, 4 EVEX
	opRegK   bool
	opRegAt  int
	modrm    int
	sib      int
	dispOff  int
	dispLen  int
	immOff   int
	immLen   int
}

func isLegacyPrefix(b byte) bool {
	switch b {
	case 0xF0, 0xF2, 0xF3, 0x2E, 0x36, 0x3E, 0x26, 0x64, 0x65, 0x66, 0x67:
		return true
	}
	return false
}

// modrmOneByte is the one-byte opcode map's modrm column. Rows 0x00-0x3F
// follow the arithmetic pattern (four modrm forms then two accumulator-and-
// immediate forms); the rest is the standard table.
var modrmOneByte = func() [256]bool {
	var t [256]bool
	for op := 0; op < 0x40; op++ {
		t[op] = op&7 <= 3
	}
	for _, op := range []byte{0x63, 0x69, 0x6B, 0x80, 0x81, 0x83, 0xC0, 0xC1, 0xC6, 0xC7,
		0xD0, 0xD1, 0xD2, 0xD3, 0xF6, 0xF7, 0xFE, 0xFF} {
		t[op] = true
	}
	for op := 0x84; op <= 0x8F; op++ {
		t[op] = true
	}
	for op := 0xD8; op <= 0xDF; op++ {
		t[op] = true
	}
	return t
}()

// modrmTwoByte is the 0F map's modrm column, written as its exceptions: all
// but a handful of two-byte opcodes take a modrm byte.
func modrmTwoByte(op byte) bool {
	switch {
	case op == 0x05, op == 0x06, op == 0x07, op == 0x08, op == 0x09, op == 0x0B, op == 0x0E:
		return false
	case op >= 0x30 && op <= 0x37, op == 0x77:
		return false
	case op >= 0x80 && op <= 0x8F: // jcc rel32
		return false
	case op == 0xA0, op == 0xA1, op == 0xA2, op == 0xA8, op == 0xA9, op == 0xAA:
		return false
	case op >= 0xC8 && op <= 0xCF: // bswap, register in the opcode
		return false
	}
	return true
}

// regIsExtension reports whether modrm.reg is an opcode extension (a /digit
// group) or a non-general register rather than a register operand.
// Renumbering it would rewrite the operation, so those fields are left alone.
func (f instFields) regIsExtension() bool {
	switch f.opLen {
	case 1:
		switch f.op1 {
		case 0x80, 0x81, 0x82, 0x83, 0x8C, 0x8E, 0x8F, 0xC0, 0xC1, 0xC6, 0xC7,
			0xD0, 0xD1, 0xD2, 0xD3, 0xF6, 0xF7, 0xFE, 0xFF:
			return true
		}
		return f.op1 >= 0xD8 && f.op1 <= 0xDF
	case 2:
		switch f.op2 {
		case 0x00, 0x01, 0x0D, 0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F,
			0x20, 0x21, 0x22, 0x23, 0x71, 0x72, 0x73, 0xAE, 0xB9, 0xBA, 0xC7:
			return true
		}
	}
	return false
}

func locateFields(code []byte) (f instFields, ok bool) {
	n := len(code)
	f = instFields{rexOff: -1, vexOff: -1, opRegAt: -1, modrm: -1, sib: -1}
	i := 0
	for i < n {
		b := code[i]
		if isLegacyPrefix(b) {
			f.rexOff = -1 // a REX is only effective immediately before the opcode
			i++
			continue
		}
		if b&0xF0 == 0x40 {
			f.rexOff = i
			i++
			continue
		}
		break
	}
	if i >= n {
		return f, false
	}
	opEnd := 0
	f.op1 = code[i]
	switch {
	case f.rexOff < 0 && (code[i] == 0xC5 || code[i] == 0xC4 || code[i] == 0x62):
		switch code[i] {
		case 0xC5:
			f.vexKind, f.vexOff = 2, i
			i += 2
		case 0xC4:
			f.vexKind, f.vexOff = 3, i
			i += 3
		default:
			f.vexKind, f.vexOff = 4, i
			i += 4
		}
		if i+1 > n {
			return f, false
		}
		f.op1, f.op2, f.opLen = 0x0F, code[i], 2 // VEX/EVEX carry the map in the prefix
		opEnd = i + 1
		f.modrm = opEnd
	case code[i] == 0x0F:
		if i+1 >= n {
			return f, false
		}
		op2 := code[i+1]
		f.op2, f.opLen = op2, 2
		if op2 == 0x38 || op2 == 0x3A {
			if i+2 >= n {
				return f, false
			}
			f.opLen = 3
			opEnd = i + 3
			f.modrm = opEnd
		} else {
			opEnd = i + 2
			if modrmTwoByte(op2) {
				f.modrm = opEnd
			} else if op2 >= 0xC8 && op2 <= 0xCF {
				f.opRegAt = i + 1
			}
		}
	default:
		op := code[i]
		f.opLen = 1
		opEnd = i + 1
		if modrmOneByte[op] {
			f.modrm = opEnd
		} else if op >= 0x50 && op <= 0x5F || op >= 0x91 && op <= 0x97 || op >= 0xB0 && op <= 0xBF {
			// push/pop r64, xchg eAX,r (0x90 is nop, left alone), mov r,imm
			f.opRegAt = i
		}
	}
	f.opRegK = f.opRegAt >= 0
	p := opEnd
	if f.modrm >= 0 {
		if f.modrm >= n {
			return f, false
		}
		m := code[f.modrm]
		mod, rm := m>>6, m&7
		p = f.modrm + 1
		if mod != 3 && rm == 4 {
			if p >= n {
				return f, false
			}
			f.sib = p
			p++
		}
		switch {
		case mod == 1:
			f.dispOff, f.dispLen = p, 1
		case mod == 2:
			f.dispOff, f.dispLen = p, 4
		case mod == 0 && rm == 5:
			f.dispOff, f.dispLen = p, 4 // RIP-relative
		case mod == 0 && f.sib >= 0 && code[f.sib]&7 == 5:
			f.dispOff, f.dispLen = p, 4
		default:
			f.dispOff, f.dispLen = p, 0
		}
		p += f.dispLen
	}
	if p > n {
		return f, false
	}
	f.immOff, f.immLen = p, n-p
	return f, true
}

// renumber rewrites every register field in place. The high bit of a register
// number lives in REX.R/X/B (or the inverted VEX/EVEX bits), so a rewrite is
// only encodable where that slot exists -- which is why the bank-preserving
// ranking never needs one that was not already there.
func (f instFields) renumber(code []byte, gp, vec *rankTable, isVec bool) {
	hasR := f.rexOff >= 0 || f.vexKind != 0
	hasXB := f.rexOff >= 0 || f.vexKind == 3 || f.vexKind == 4
	getHi := func(which int) int {
		if f.rexOff >= 0 {
			return int(code[f.rexOff]>>(2-which)) & 1
		}
		switch f.vexKind {
		case 2:
			if which == 0 {
				return int(^code[f.vexOff+1]>>7) & 1
			}
		case 3, 4:
			return int(^code[f.vexOff+1]>>(7-which)) & 1
		}
		return 0
	}
	setHi := func(which, v int) {
		if f.rexOff >= 0 {
			b := byte(1) << (2 - which)
			code[f.rexOff] = code[f.rexOff]&^b | byte(v)<<(2-which)
			return
		}
		switch f.vexKind {
		case 2:
			if which == 0 {
				code[f.vexOff+1] = code[f.vexOff+1]&^0x80 | byte(1-v)<<7
			}
		case 3, 4:
			b := byte(1) << (7 - which)
			code[f.vexOff+1] = code[f.vexOff+1]&^b | byte(1-v)<<(7-which)
		}
	}
	// low renumbers the three-bit field at code[off]<<shift, taking its
	// fourth bit from the REX/VEX slot named by which (0=R, 1=X, 2=B).
	field := func(off int, shift uint, tab *rankTable, which int, hasHi bool) {
		cur := int(code[off]>>shift) & 7
		if hasHi {
			cur |= getHi(which) << 3
		}
		v := tab.rank(cur)
		code[off] = code[off]&^(7<<shift) | byte(v&7)<<shift
		if hasHi {
			setHi(which, v>>3)
		}
	}
	tab := gp
	if isVec {
		tab = vec
	}
	if f.modrm >= 0 {
		m := code[f.modrm]
		mod, rm := m>>6, m&7
		if f.opLen == 1 && f.op1 >= 0xD8 && f.op1 <= 0xDF && mod == 3 {
			return // x87 spends the whole modrm byte on the operation
		}
		if !f.regIsExtension() {
			field(f.modrm, 3, tab, 0, hasR)
		}
		switch {
		case mod == 3:
			field(f.modrm, 0, tab, 2, hasXB)
		case rm == 4 && f.sib >= 0:
			s := code[f.sib]
			// index == 4 with no REX.X extension means "no index register"
			if idx := int(s>>3&7) | getHi(1)<<3; idx != 4 {
				field(f.sib, 3, gp, 1, hasXB)
			}
			// base == 5 with mod == 0 means "no base register"
			if !(mod == 0 && s&7 == 5) {
				field(f.sib, 0, gp, 2, hasXB)
			}
		case !(mod == 0 && rm == 5): // not RIP-relative
			field(f.modrm, 0, gp, 2, hasXB)
		}
	}
	if f.opRegK {
		field(f.opRegAt, 0, gp, 2, hasXB)
	}
}

// rankTable renumbers registers by order of first appearance. In bank mode the
// low eight and the high eight are ranked separately, so a register that
// needed a REX extension bit still has one and the rewrite is always
// encodable; in full mode one ranking covers all sixteen, which is stronger
// but can collapse two registers onto the same encoding where the instruction
// has no REX prefix.
type rankTable struct {
	m              [16]int8
	nLo, nHi, nAll int
	full           bool
}

func (t *rankTable) reset() {
	for i := range t.m {
		t.m[i] = -1
	}
	t.nLo, t.nHi, t.nAll = 0, 0, 0
}

func (t *rankTable) rank(v int) int {
	v &= 15
	if t.m[v] >= 0 {
		return int(t.m[v])
	}
	var r int
	switch {
	case t.full:
		r = t.nAll
		t.nAll++
	case v < 8:
		r = t.nLo
		t.nLo++
	default:
		r = 8 + t.nHi
		t.nHi++
	}
	t.m[v] = int8(r)
	return r
}

func parallelFor(n int, fn func(int)) {
	if n == 0 {
		return
	}
	workers := min(runtime.NumCPU(), n)
	var wg sync.WaitGroup
	ch := make(chan int, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				fn(i)
			}
		}()
	}
	for i := range n {
		ch <- i
	}
	close(ch)
	wg.Wait()
}

// parallelXZContiguous compresses several streams at once. Each call is
// single-threaded by necessity, so the only way to keep a ten-compression
// probe under a minute is to run the compressions side by side; the bound
// keeps peak memory near six xz encoders.
func parallelXZContiguous(streams [][]byte) []int {
	out := make([]int, len(streams))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, s := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = xzSizeContiguous(s)
		}()
	}
	wg.Wait()
	return out
}
