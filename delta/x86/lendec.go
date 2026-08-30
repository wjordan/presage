package x86

// A fast instruction-length and PC-relative-field decoder. The reference walk
// needs three facts per position -- how long the instruction is, and where a
// PC-relative displacement field sits if it has one -- and x86asm computes
// them by building the entire instruction through a table-interpreter, which
// profiles at two-thirds of whole-image decode time. fastStep answers the
// same three facts from a table of per-opcode facts, and answers only where
// that table was derived from x86asm itself (see gen/main.go): every entry
// records what x86asm actually did across the opcode's encoding space, so a
// fast answer and an x86asm answer are the same answer. Everything the table
// does not cover -- vector prefixes, address-size and lock prefixes, x87,
// truncations, quirky opcodes -- is handed to x86asm unchanged, trading speed
// for exactness only where speed does not matter.

// opInfo is one generated table row; see gen/main.go for how the fields are
// measured and merged.
type opInfo struct {
	flags    uint16
	validMem uint8 // valid modrm.reg values when the operand is memory
	validReg uint8 // valid modrm.reg values when mod == 3
	immPlain int8  // immediate bytes; -1 when the form is invalid
	imm66    int8
	immW     int8
	relPlain int8 // pcrel immediate width, 0 when the immediate is absolute
	rel66    int8
	relW     int8
}

const (
	fValid  = 1 << 0 // row usable; unset means defer to x86asm
	fModrm  = 1 << 1
	fRipRel = 1 << 2 // memory form mod=00 rm=101: disp32 is a pcrel field
	fRel    = 1 << 3 // the immediate is a branch displacement
	fInval  = 1 << 4 // invalid in every probed context
	fF2OK   = 1 << 5 // behaviour under F2/F3 matches plain
	fSegOK  = 1 << 6 // behaviour under a segment override matches plain
	fSpec   = 1 << 7 // hand-written handler below
)

// fastStep decodes the head of code. handled reports whether the fast path
// owns this byte sequence; when false the caller must consult x86asm. When
// handled, (length, off, n, ok) mean exactly what a decode()+pcrelField pair
// would have produced: ok false is the advance-one-byte outcome, n is 0 when
// the instruction has no PC-relative field.
func fastStep(code []byte) (length, off, n int, ok, handled bool) {
	i := 0
	var has66, hasRep, hasSeg, hasLegacy, rexW bool
	for {
		if i >= len(code) || i >= 14 {
			return 0, 0, 0, false, false
		}
		switch code[i] {
		case 0x66:
			has66 = true
		case 0xF2, 0xF3:
			hasRep = true
		case 0x2E, 0x36, 0x3E, 0x26, 0x64, 0x65:
			hasSeg = true
		case 0x67, 0xF0:
			return 0, 0, 0, false, false // address-size & lock: defer
		default:
			goto opcode
		}
		hasLegacy = true
		i++
	}
opcode:
	b := code[i]
	if i == 0 && (b == 0xC4 || b == 0xC5 || b == 0x62) {
		return 0, 0, 0, false, false // vector prefix: defer
	}
	if b&0xF0 == 0x40 {
		rexW = b&8 != 0
		i++
		if i >= len(code) {
			return 0, 0, 0, false, false
		}
		b = code[i]
	}
	m := 0
	if b == 0x0F {
		i++
		if i >= len(code) {
			return 0, 0, 0, false, false
		}
		b = code[i]
		switch b {
		case 0x38, 0x3A:
			if b == 0x38 {
				m = 2
			} else {
				m = 3
			}
			i++
			if i >= len(code) {
				return 0, 0, 0, false, false
			}
			b = code[i]
		default:
			m = 1
		}
	}
	e := &lenTab[m][b]
	if e.flags&fSpec != 0 {
		return group3or11(code, i, b, has66, rexW, hasLegacy, hasRep, hasSeg)
	}
	if e.flags&fValid == 0 ||
		hasRep && e.flags&fF2OK == 0 ||
		hasSeg && e.flags&fSegOK == 0 {
		return 0, 0, 0, false, false
	}
	imm, rel := e.immPlain, e.relPlain
	if rexW {
		imm, rel = e.immW, e.relW
	} else if has66 {
		imm, rel = e.imm66, e.rel66
	}
	if e.flags&fInval != 0 || imm < 0 {
		return invalidOutcome(hasLegacy)
	}
	i++
	if e.flags&fModrm == 0 {
		length = i + int(imm)
		if length > len(code) || length > 15 {
			return 0, 0, 0, false, false
		}
		if rel > 0 {
			off, n = i, int(rel)
		}
		return length, off, n, true, true
	}
	if i >= len(code) {
		return 0, 0, 0, false, false
	}
	modrm := code[i]
	mod, reg, rm := modrm>>6, (modrm>>3)&7, modrm&7
	if mod == 3 {
		if e.validReg>>reg&1 == 0 {
			return invalidOutcome(hasLegacy)
		}
	} else if e.validMem>>reg&1 == 0 {
		return invalidOutcome(hasLegacy)
	}
	i++
	var dispLen int
	rip := false
	if mod != 3 {
		if rm == 4 {
			if i >= len(code) {
				return 0, 0, 0, false, false
			}
			sib := code[i]
			i++
			switch {
			case mod == 1:
				dispLen = 1
			case mod == 2, sib&7 == 5:
				dispLen = 4
			}
		} else {
			switch {
			case mod == 1:
				dispLen = 1
			case mod == 2:
				dispLen = 4
			case rm == 5:
				dispLen, rip = 4, true
			}
		}
	}
	dispPos := i
	i += dispLen
	length = i + int(imm)
	if length > len(code) || length > 15 {
		return 0, 0, 0, false, false
	}
	if rip && e.flags&fRipRel != 0 {
		off, n = dispPos, 4
	}
	return length, off, n, true, true
}

// invalidOutcome is what x86asm does with an unrecognized opcode: with any
// legacy prefix present it emits the first prefix byte as a one-byte
// pseudo-instruction; bare, it errors and the walk advances one byte. The
// generator verified this shape for every invalid table row.
func invalidOutcome(hasLegacy bool) (length, off, n int, ok, handled bool) {
	if hasLegacy {
		return 1, 0, 0, true, true
	}
	return 0, 0, 0, false, true
}

// group3or11 handles the four opcodes whose immediate depends on modrm.reg:
// F6/F7 (group 3: TEST carries an immediate, NOT..IDIV do not) and C6/C7
// (group 11: MOV carries one, and mod=11 reg=7 rm=0 hides XABORT/XBEGIN --
// XBEGIN's immediate being a branch displacement). i indexes the opcode byte.
// The differential tests sweep this space exhaustively against x86asm.
func group3or11(code []byte, i int, op byte, has66, rexW, hasLegacy, hasRep, hasSeg bool) (length, off, n int, ok, handled bool) {
	if hasRep || hasSeg {
		return 0, 0, 0, false, false // unprobed combinations: defer
	}
	i++
	if i >= len(code) {
		return 0, 0, 0, false, false
	}
	modrm := code[i]
	mod, reg, rm := modrm>>6, (modrm>>3)&7, modrm&7
	immz := 4
	if has66 && !rexW {
		immz = 2
	}
	var imm int
	rel := false
	switch {
	case op == 0xF6 || op == 0xF7:
		if reg == 1 { // the TEST alias, which x86asm does not implement
			return invalidOutcome(hasLegacy)
		}
		if reg == 0 { // TEST
			if op == 0xF6 {
				imm = 1
			} else {
				imm = immz
			}
		}
	case mod == 3 && reg == 7 && rm == 0: // XABORT / XBEGIN
		if op == 0xC6 {
			imm = 1
		} else {
			imm, rel = immz, true
		}
	case reg == 0: // MOV
		if op == 0xC6 {
			imm = 1
		} else {
			imm = immz
		}
	default:
		return invalidOutcome(hasLegacy)
	}
	i++
	var dispLen int
	rip := false
	if mod != 3 {
		if rm == 4 {
			if i >= len(code) {
				return 0, 0, 0, false, false
			}
			sib := code[i]
			i++
			switch {
			case mod == 1:
				dispLen = 1
			case mod == 2, sib&7 == 5:
				dispLen = 4
			}
		} else {
			switch {
			case mod == 1:
				dispLen = 1
			case mod == 2:
				dispLen = 4
			case rm == 5:
				dispLen, rip = 4, true
			}
		}
	}
	dispPos := i
	i += dispLen
	length = i + imm
	if length > len(code) || length > 15 {
		return 0, 0, 0, false, false
	}
	switch {
	case rel: // XBEGIN: the displacement is the trailing immediate itself
		off, n = length-imm, imm
	case rip:
		off, n = dispPos, 4
	}
	return length, off, n, true, true
}

// step is the one decode the walking passes share: fastStep where the table
// vouches for the answer, x86asm everywhere else. It never panics; the defer
// path uses the recovering decode wrapper.
func step(code []byte) (length, off, n int, ok bool) {
	if length, off, n, ok, handled := fastStep(code); handled {
		return length, off, n, ok
	}
	inst, err := decode(code)
	if err != nil || inst.Len == 0 {
		return 0, 0, 0, false
	}
	off, n = pcrelField(inst, code)
	return inst.Len, off, n, true
}
