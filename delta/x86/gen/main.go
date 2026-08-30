// Command gen derives delta/x86's fast-path length table by probing
// golang.org/x/arch/x86/x86asm.
//
// The fast decoder must reproduce x86asm's observable behaviour exactly --
// instruction length, PC-relative field position, and the error/advance-one
// fallback -- because the reference walk built on it is the basis both sides
// of a patch share. Rather than re-deriving that behaviour from the manuals
// and hoping the quirks match, this program measures it: for every prefix
// context, opcode map, opcode, and modrm byte it assembles an encoding, asks
// x86asm, and only when every observation fits the standard length formula
// with one consistent immediate size does the opcode earn a table entry. An
// opcode whose observations disagree with the model is marked deferred, and
// the runtime hands those bytes to x86asm itself.
//
// Usage: go run ./delta/x86/gen > delta/x86/lendec_tables.go
package main

import (
	"fmt"
	"os"

	"golang.org/x/arch/x86/x86asm"
)

// outcome is the observable result of one decode, collapsed exactly the way
// the walk collapses it: a panic, an error, or a zero length all mean
// "advance one byte".
type outcome struct {
	ok  bool
	len int
	off int // pcrel field offset, 0 when none
	n   int // pcrel field width, 0 when none
}

func probe(code []byte) (o outcome) {
	defer func() {
		if recover() != nil {
			o = outcome{}
		}
	}()
	inst, err := x86asm.Decode(code, 64)
	if err != nil || inst.Len == 0 {
		return outcome{}
	}
	o = outcome{ok: true, len: inst.Len}
	if inst.PCRel > 0 && inst.PCRelOff >= 0 && inst.PCRelOff+inst.PCRel <= inst.Len {
		o.off, o.n = inst.PCRelOff, inst.PCRel
	}
	return o
}

// The contexts the runtime distinguishes. Contexts the runtime folds together
// (F2 with F3, every segment override, every REX without W) are probed
// separately and required to agree, or the opcode defers.
type context struct {
	name   string
	prefix []byte
}

var contexts = []context{
	{"plain", nil},
	{"66", []byte{0x66}},
	{"F2", []byte{0xF2}},
	{"F3", []byte{0xF3}},
	{"seg", []byte{0x65}},
	{"rex", []byte{0x40}},
	{"rexW", []byte{0x48}},
	{"66rex", []byte{0x66, 0x40}},
	{"66rexW", []byte{0x66, 0x48}},
	{"F2rexW", []byte{0xF2, 0x48}},
	{"F3rexW", []byte{0xF3, 0x48}},
}

var maps = []struct {
	name   string
	opcode []byte // bytes before the final opcode byte
}{
	{"one", nil},
	{"0F", []byte{0x0F}},
	{"0F38", []byte{0x0F, 0x38}},
	{"0F3A", []byte{0x0F, 0x3A}},
}

// filler supplies disp and imm bytes. None of its bytes is a prefix, so a
// misclassified boundary shifts lengths instead of silently re-anchoring.
var filler = []byte{0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7A, 0x7B}

// modrmTail returns the sib/disp bytes the standard encoding rules append
// after this modrm, using the given sib byte when one is called for, and
// whether the displacement (at its returned offset within the tail) is the
// rip-relative form.
func modrmTail(modrm, sib byte) (tail []byte, dispOff, dispLen int, rip bool) {
	mod, rm := modrm>>6, modrm&7
	if mod == 3 {
		return nil, 0, 0, false
	}
	if rm == 4 {
		tail = append(tail, sib)
		base5 := sib&7 == 5
		switch {
		case mod == 1:
			dispOff, dispLen = 1, 1
		case mod == 2 || (mod == 0 && base5):
			dispOff, dispLen = 1, 4
		}
	} else {
		switch {
		case mod == 1:
			dispLen = 1
		case mod == 2:
			dispLen = 4
		case mod == 0 && rm == 5:
			dispLen, rip = 4, true
		}
	}
	for i := 0; i < dispLen; i++ {
		tail = append(tail, filler[i])
	}
	return tail, dispOff, dispLen, rip
}

// ctxInfo is what one context's probes establish for one opcode.
type ctxInfo struct {
	defer_   bool
	invalid  bool // every form errors (or lone-prefixes out)
	hasModrm bool
	imm      int  // immediate bytes after modrm/disp (or after opcode)
	relOff   int  // pcrel field offset minus opcode end; -1 when none
	relN     int  // pcrel field width from an immediate
	ripRel   bool // memory-form mod=00 rm=101 disp is a pcrel field
	validMem uint8
	validReg uint8
}

// specialOps get hand-written runtime handlers (their immediate size or
// validity varies with modrm.reg in ways the flat model cannot carry), so the
// generator skips them; the runtime and its differential tests own them.
// 8F is group 1A (pop r/m valid only for reg=0 -- the mask handles it), so it
// is NOT here; F6/F7 carry an immediate only for reg 0-1, and C6/C7 hide
// xabort/xbegin in mod=11 reg=7.
var specialOps = map[[2]int]bool{
	{0, 0xF6}: true, {0, 0xF7}: true, {0, 0xC6}: true, {0, 0xC7}: true,
}

// vexOps are the vector-prefix bytes: at position zero the runtime defers to
// x86asm before any table lookup, and after a legacy prefix they decode as the
// (64-bit-invalid) legacy opcodes, which the prefixed contexts probe normally.
var vexOps = map[int]bool{0xC4: true, 0xC5: true, 0x62: true}

func classify(ctx context, mapIdx, op int) ctxInfo {
	pre := append(append([]byte(nil), ctx.prefix...), maps[mapIdx].opcode...)
	pre = append(pre, byte(op))
	opEnd := len(pre)
	legacyPrefixed := len(ctx.prefix) > 0 && ctx.prefix[0]&0xF0 != 0x40

	// A context with legacy prefixes turns an invalid opcode into a one-byte
	// prefix pseudo-instruction; the plain and REX-only contexts turn it into
	// an error. Both are "invalid" to the model, and the runtime reproduces
	// the split itself.
	isInvalid := func(o outcome) bool {
		if legacyPrefixed {
			return o.ok && o.len == 1 && o.n == 0
		}
		return !o.ok
	}
	probeAt := func(next ...byte) outcome {
		code := append(append([]byte(nil), pre...), next...)
		code = append(code, filler...)
		return probe(code[:min(len(code), 15)])
	}

	// Both shapes are scored in full and the one that fits wins; probing a
	// couple of representative bytes first is how an opcode that is invalid
	// for exactly those bytes gets misfiled. fitOK false means the
	// observations fit neither discipline; inv true means no encoding was
	// valid at all.
	noInfo, noInv, noOK := classifyNoModrm(probeAt, isInvalid, opEnd)
	mInfo, mInv, mOK := classifyModrm(probeAt, isInvalid, opEnd)
	validNo, validM := noOK && !noInv, mOK && !mInv
	switch {
	case validNo && validM:
		return ctxInfo{defer_: true} // ambiguous: both shapes fit
	case validM:
		return mInfo
	case validNo:
		return noInfo
	case noOK || mOK:
		return ctxInfo{invalid: true}
	}
	return ctxInfo{defer_: true}
}

// classifyNoModrm tests the hypothesis that the byte after the opcode is
// data: every probe must then agree on one length and one pcrel field. The
// probe bytes cover a rip-looking modrm, prefixes, and both mod extremes, so
// a modrm opcode cannot slip through unless it is invalid for all of them --
// and then the modrm grid claims it first.
func classifyNoModrm(probeAt func(...byte) outcome, isInvalid func(outcome) bool, opEnd int) (info ctxInfo, inv, ok bool) {
	firsts := []byte{0x10, 0x90, 0x05, 0x66, 0xC0, 0xFF, 0x00}
	var seen []outcome
	invalid := 0
	for _, first := range firsts {
		o := probeAt(first)
		if isInvalid(o) {
			invalid++
			continue
		}
		if !o.ok {
			return ctxInfo{}, false, false
		}
		seen = append(seen, o)
	}
	if invalid == len(firsts) {
		return ctxInfo{}, true, true
	}
	if len(seen) != len(firsts) {
		return ctxInfo{}, false, false // partly invalid: not this shape
	}
	a := seen[0]
	for _, o := range seen[1:] {
		if o.len != a.len || o.off != a.off || o.n != a.n {
			return ctxInfo{}, false, false
		}
	}
	imm := a.len - opEnd
	if imm < 0 || imm > 8 {
		return ctxInfo{}, false, false
	}
	info = ctxInfo{imm: imm, relOff: -1}
	if a.n > 0 {
		if a.off < opEnd {
			return ctxInfo{}, false, false
		}
		info.relOff, info.relN = a.off-opEnd, a.n
	}
	return info, false, true
}

// classifyModrm tests the modrm hypothesis: one immediate size must explain
// every valid modrm and sib shape, validity may vary only by
// (memory-vs-register, reg), and the only pcrel field allowed is the
// rip-relative disp32 exactly where the encoding puts it.
func classifyModrm(probeAt func(...byte) outcome, isInvalid func(outcome) bool, opEnd int) (info ctxInfo, inv, ok bool) {
	info = ctxInfo{hasModrm: true, relOff: -1}
	immSeen := -1
	// Validity may vary only with (memory-vs-register, reg). x87 opcodes
	// carve the mod=11 space per rm as well; a group with both valid and
	// invalid probes fits neither mask and the opcode defers.
	var validCnt, invalidCnt [16]int
	group := func(mod, reg byte) int {
		if mod == 3 {
			return int(reg) + 8
		}
		return int(reg)
	}
	for modrm := 0; modrm < 256; modrm++ {
		mod, reg := byte(modrm)>>6, (byte(modrm)>>3)&7
		for _, sib := range []byte{0x24, 0x25, 0x65} {
			if byte(modrm)&7 != 4 || mod == 3 {
				if sib != 0x24 {
					continue // sib byte unused; one probe is enough
				}
			}
			tail, dispOff, dispLen, rip := modrmTail(byte(modrm), sib)
			o := probeAt(append([]byte{byte(modrm)}, tail...)...)
			if isInvalid(o) {
				invalidCnt[group(mod, reg)]++
				continue
			}
			validCnt[group(mod, reg)]++
			if !o.ok {
				return ctxInfo{}, false, false
			}
			structural := opEnd + 1 + len(tail)
			imm := o.len - structural
			if imm < 0 || imm > 8 || (immSeen >= 0 && imm != immSeen) {
				return ctxInfo{}, false, false
			}
			immSeen = imm
			if rip {
				wantOff := opEnd + 1 + dispOff
				switch {
				case o.n == 0:
				case o.off == wantOff && o.n == 4 && dispLen == 4:
					info.ripRel = true
				default:
					return ctxInfo{}, false, false
				}
			} else if o.n != 0 {
				return ctxInfo{}, false, false
			}
			if mod == 3 {
				info.validReg |= 1 << reg
			} else {
				info.validMem |= 1 << reg
			}
		}
	}
	if immSeen < 0 {
		return ctxInfo{}, true, true
	}
	for g := range validCnt {
		if validCnt[g] > 0 && invalidCnt[g] > 0 {
			return ctxInfo{}, false, false
		}
	}
	info.imm = immSeen
	return info, false, true
}

// entry is the merged, runtime-facing table row.
type entry struct {
	flags    uint16
	validMem uint8
	validReg uint8
	immPlain int8
	imm66    int8
	immW     int8
	relPlain int8
	rel66    int8
	relW     int8
}

const (
	fValid  = 1 << 0 // table entry usable at all (else defer)
	fModrm  = 1 << 1
	fRipRel = 1 << 2
	fRel    = 1 << 3 // pcrel immediate; width per context in relPlain/rel66/relW
	fInval  = 1 << 4 // opcode invalid in every probed context
	fF2OK   = 1 << 5 // F2/F3 contexts agree with plain (else defer under them)
	fSegOK  = 1 << 6 // segment-override context agrees with plain
	fSpec   = 1 << 7 // hand-written handler in lendec.go
)

func main() {
	var table [4][256]entry
	ctxByName := func(res map[string]ctxInfo, n string) ctxInfo { return res[n] }
	for m := range maps {
		for op := 0; op < 256; op++ {
			if m == 0 && vexOps[op] {
				// Probed like any opcode under legacy prefixes; at position
				// zero the runtime defers first. The prefixed contexts below
				// still classify it (invalid in 64-bit mode).
			}
			if m == 0 && specialOps[[2]int{m, op}] {
				table[m][op] = entry{flags: fSpec}
				continue
			}
			res := map[string]ctxInfo{}
			for _, ctx := range contexts {
				if m == 0 && vexOps[op] && len(ctx.prefix) == 0 {
					// A bare C4/C5/62 is a vector prefix, not this opcode.
					res[ctx.name] = ctxInfo{defer_: true}
					continue
				}
				res[ctx.name] = classify(ctx, m, op)
			}
			e, ok := merge(res, ctxByName)
			if !ok {
				table[m][op] = entry{}
				continue
			}
			table[m][op] = e
		}
	}
	emit(table)
}

// merge folds the per-context classifications into one runtime row, deciding
// which contexts may share entries and which force a defer.
func merge(res map[string]ctxInfo, get func(map[string]ctxInfo, string) ctxInfo) (entry, bool) {
	plain := get(res, "plain")
	if plain.defer_ {
		return entry{}, false
	}
	same := func(a, b ctxInfo) bool {
		return a.defer_ == b.defer_ && a.invalid == b.invalid && a.hasModrm == b.hasModrm &&
			a.imm == b.imm && a.relOff == b.relOff && a.relN == b.relN && a.ripRel == b.ripRel &&
			a.validMem == b.validMem && a.validReg == b.validReg
	}
	// REX without W must behave exactly like no REX (lengths and validity),
	// under both plain and 66; if not, the whole opcode defers.
	if !same(plain, get(res, "rex")) || !same(get(res, "66"), get(res, "66rex")) {
		return entry{}, false
	}
	c66, cW, c66W := get(res, "66"), get(res, "rexW"), get(res, "66rexW")
	if c66.defer_ || cW.defer_ || c66W.defer_ {
		return entry{}, false
	}
	// 66+REX.W must behave like REX.W alone (W wins; x86asm marks the 66
	// ignored). Otherwise defer.
	if !same(cW, c66W) {
		return entry{}, false
	}
	// Structure must agree across the imm-bearing contexts: same modrm-ness,
	// same validity, same rip behaviour. Only the immediate/rel width and
	// invalidity may vary by context.
	structural := func(a, b ctxInfo) bool {
		if a.invalid || b.invalid {
			return true
		}
		return a.hasModrm == b.hasModrm && a.validMem == b.validMem &&
			a.validReg == b.validReg && a.ripRel == b.ripRel &&
			(a.relOff >= 0) == (b.relOff >= 0) && a.relOff <= 0 && b.relOff <= 0
	}
	if !structural(plain, c66) || !structural(plain, cW) {
		return entry{}, false
	}
	e := entry{flags: fValid}
	base := plain
	if base.invalid {
		base = c66
	}
	if base.invalid {
		base = cW
	}
	if base.invalid {
		e.flags |= fInval
		// still valid: the runtime knows how to answer for invalid opcodes.
	}
	if base.hasModrm {
		e.flags |= fModrm
		e.validMem, e.validReg = base.validMem, base.validReg
	}
	if base.ripRel {
		e.flags |= fRipRel
	}
	width := func(c ctxInfo) (imm, rel int8) {
		if c.invalid {
			return -1, 0
		}
		if c.relOff >= 0 {
			if c.relOff != 0 {
				return -2, 0 // rel not at the immediate start: defer
			}
			return int8(c.imm), int8(c.relN)
		}
		return int8(c.imm), 0
	}
	var relSeen bool
	for _, w := range []struct {
		c        ctxInfo
		imm, rel *int8
	}{{plain, &e.immPlain, &e.relPlain}, {c66, &e.imm66, &e.rel66}, {cW, &e.immW, &e.relW}} {
		imm, rel := width(w.c)
		if imm == -2 {
			return entry{}, false
		}
		*w.imm, *w.rel = imm, rel
		if rel > 0 {
			relSeen = true
		}
	}
	if relSeen {
		e.flags |= fRel
	}
	// F2/F3 and segment contexts: fold when identical to their base
	// (F2rexW compares against rexW), else mark so the runtime defers.
	if same(get(res, "F2"), plain) && same(get(res, "F3"), plain) &&
		same(get(res, "F2rexW"), cW) && same(get(res, "F3rexW"), cW) {
		e.flags |= fF2OK
	}
	if same(get(res, "seg"), plain) {
		e.flags |= fSegOK
	}
	return e, true
}

func emit(table [4][256]entry) {
	w := os.Stdout
	fmt.Fprintf(w, "// Code generated by go run ./delta/x86/gen. DO NOT EDIT.\n")
	fmt.Fprintf(w, "// Derived by probing golang.org/x/arch/x86/x86asm; regenerate after\n")
	fmt.Fprintf(w, "// changing that dependency and rerun the differential tests.\n\npackage x86\n\n")
	fmt.Fprintf(w, "var lenTab = [4][256]opInfo{\n")
	var stats [4][2]int
	for m := range table {
		fmt.Fprintf(w, "\t{ // map %s\n", maps[m].name)
		for op, e := range table[m] {
			if e.flags&(fValid|fSpec) != 0 {
				stats[m][0]++
			} else {
				stats[m][1]++
			}
			fmt.Fprintf(w, "\t\t%#02x: {%#04x, %#02x, %#02x, %d, %d, %d, %d, %d, %d},\n",
				op, e.flags, e.validMem, e.validReg, e.immPlain, e.imm66, e.immW, e.relPlain, e.rel66, e.relW)
		}
		fmt.Fprintf(w, "\t},\n")
	}
	fmt.Fprintf(w, "}\n")
	for m := range stats {
		fmt.Fprintf(os.Stderr, "map %-4s: %d table, %d defer\n", maps[m].name, stats[m][0], stats[m][1])
	}
}
