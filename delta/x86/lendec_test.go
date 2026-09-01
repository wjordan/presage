package x86

import (
	"math/rand"
	"testing"

	"golang.org/x/arch/x86/x86asm"
)

// referenceStep is the oracle: exactly what step's defer branch computes.
// Every test here asserts step == referenceStep, so a fast-path bug cannot
// hide behind the fallback.
func referenceStep(code []byte) (length, off, n int, ok bool) {
	inst, err := decode(code)
	if err != nil || inst.Len == 0 {
		return 0, 0, 0, false
	}
	off, n = pcrelField(inst, code)
	return inst.Len, off, n, true
}

func assertAgrees(t *testing.T, code []byte) {
	t.Helper()
	gl, go_, gn, gok := step(code)
	wl, wo, wn, wok := referenceStep(code)
	if gl != wl || go_ != wo || gn != wn || gok != wok {
		t.Fatalf("step(% x) = (%d,%d,%d,%v), x86asm says (%d,%d,%d,%v)",
			code[:min(len(code), 16)], gl, go_, gn, gok, wl, wo, wn, wok)
	}
}

// TestStepDifferentialRandom compares step against x86asm at every offset of
// a deterministic random buffer. Uniform random bytes hit the prefix bytes,
// the vector prefixes, and every opcode map hard.
func TestStepDifferentialRandom(t *testing.T) {
	buf := make([]byte, 1<<20)
	rand.New(rand.NewSource(2)).Read(buf)
	for i := range buf {
		assertAgrees(t, buf[i:min(i+18, len(buf))])
	}
}

// TestStepDifferentialPrefixed stacks realistic prefix runs in front of
// random tails, since uniform bytes rarely produce deep prefix+opcode+modrm
// chains that decode fully.
func TestStepDifferentialPrefixed(t *testing.T) {
	prefixes := []byte{0x66, 0xF2, 0xF3, 0x65, 0x64, 0x2E, 0x40, 0x48, 0x4C, 0x67, 0xF0}
	r := rand.New(rand.NewSource(3))
	code := make([]byte, 18)
	for round := 0; round < 60000; round++ {
		k := 0
		for d := r.Intn(3); k < d; k++ {
			code[k] = prefixes[r.Intn(len(prefixes))]
		}
		if r.Intn(2) == 0 {
			code[k] = 0x40 | byte(r.Intn(16)) // REX
			k++
		}
		if r.Intn(3) == 0 {
			code[k] = 0x0F
			k++
			if r.Intn(3) == 0 {
				code[k] = []byte{0x38, 0x3A}[r.Intn(2)]
				k++
			}
		}
		for ; k < len(code); k++ {
			code[k] = byte(r.Intn(256))
		}
		assertAgrees(t, code)
	}
}

// TestStepSpecialOpsExhaustive sweeps the four hand-written group handlers
// (F6/F7 group 3, C6/C7 group 11) across every modrm, three sib shapes, and
// the folded prefix contexts.
func TestStepSpecialOpsExhaustive(t *testing.T) {
	ctxs := [][]byte{nil, {0x66}, {0x48}, {0x66, 0x48}, {0x40}, {0xF2}, {0x65}}
	filler := []byte{0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79}
	for _, op := range []byte{0xF6, 0xF7, 0xC6, 0xC7} {
		for _, ctx := range ctxs {
			for modrm := 0; modrm < 256; modrm++ {
				for _, sib := range []byte{0x24, 0x25, 0x65} {
					code := append(append([]byte(nil), ctx...), op, byte(modrm), sib)
					code = append(code, filler...)
					assertAgrees(t, code[:min(len(code), 15)])
				}
			}
		}
	}
}

// TestStepTruncation checks every truncation of a set of full encodings:
// the fast path must either agree or defer, never answer wrongly short.
func TestStepTruncation(t *testing.T) {
	encodings := [][]byte{
		{0xE8, 0x44, 0x33, 0x22, 0x11},
		{0x48, 0x8B, 0x05, 0x44, 0x33, 0x22, 0x11},
		{0x66, 0x0F, 0x1F, 0x84, 0x25, 0x01, 0x02, 0x03, 0x04},
		{0xC7, 0x05, 0x01, 0x02, 0x03, 0x04, 0x11, 0x22, 0x33, 0x44},
		{0xC7, 0xF8, 0x11, 0x22, 0x33, 0x44},
		{0xF7, 0x05, 0x01, 0x02, 0x03, 0x04, 0x11, 0x22, 0x33, 0x44},
		{0x0F, 0x38, 0x00, 0x05, 0x01, 0x02, 0x03, 0x04},
		{0x0F, 0x3A, 0x0F, 0x05, 0x01, 0x02, 0x03, 0x04, 0x07},
		{0xC4, 0xE2, 0x79, 0x18, 0x05, 0x44, 0x33, 0x22, 0x11},
		{0x62, 0xF1, 0x7C, 0x48, 0x28, 0x05, 0x44, 0x33, 0x22, 0x11},
	}
	for _, enc := range encodings {
		for n := 0; n <= len(enc); n++ {
			assertAgrees(t, enc[:n])
		}
	}
}

// TestStepCorpus walks the real .text named by X86_BENCH_BIN, comparing step
// with x86asm at every position the walk visits, and reports the fast-path
// coverage. Manual: the corpus is hundreds of megabytes.
func TestStepCorpus(t *testing.T) {
	code, _ := benchText(t)
	var positions, deferred int
	for i := 0; i < len(code); {
		sub := code[i:]
		positions++
		if _, _, _, _, handled := fastStep(sub); !handled {
			deferred++
		}
		gl, go_, gn, gok := step(sub)
		wl, wo, wn, wok := referenceStep(sub)
		if gl != wl || go_ != wo || gn != wn || gok != wok {
			t.Fatalf("at %#x: step(% x) = (%d,%d,%d,%v), x86asm says (%d,%d,%d,%v)",
				i, sub[:min(len(sub), 16)], gl, go_, gn, gok, wl, wo, wn, wok)
		}
		if !gok {
			i++
			continue
		}
		i += gl
	}
	t.Logf("%d positions, %d deferred (%.3f%%)", positions, deferred, 100*float64(deferred)/float64(positions))
}

// fieldMask is the low bits of a field of the given width.
func fieldMask(width int) uint64 {
	if width == 8 {
		return ^uint64(0)
	}
	return uint64(1)<<(8*width) - 1
}

func fieldValue(b []byte, width int) uint64 {
	var v uint64
	for i := width - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

// TestFieldsDifferential checks the exported field layout against x86asm: a
// field the tables locate must hold the value x86asm reads out of that
// instruction, and a displacement's base class must be the register x86asm
// says the memory operand is based on. Locating a field one byte off, or
// classing r13 as rbp, would round-trip through a rewrite and still ship the
// wrong bytes, so the check is against the meaning and not the arithmetic.
func TestFieldsDifferential(t *testing.T) {
	buf := make([]byte, 1<<20)
	rand.New(rand.NewSource(11)).Read(buf)
	var seenBase [4]int
	imms, rels, disps, unnamed := 0, 0, 0, 0
	for i := range buf {
		code := buf[i:min(i+18, len(buf))]
		f, ok := FieldsAt(code)
		if !ok {
			continue
		}
		inst, err := decode(code)
		if err != nil || inst.Len != f.Len {
			continue // the one-byte pseudo-instruction of an invalid opcode
		}
		if f.ImmLen != 0 {
			mask := fieldMask(f.ImmLen)
			want := fieldValue(code[f.ImmOff:], f.ImmLen) & mask
			found := false
			for _, a := range inst.Args {
				switch v := a.(type) {
				case x86asm.Imm:
					found = found || !f.Rel && uint64(v)&mask == want
				case x86asm.Rel:
					found = found || f.Rel && uint64(int64(v))&mask == want
				case x86asm.Mem:
					// The moffs forms (A0-A3) carry an absolute address in the
					// trailing constant, where the table reports the immediate.
					found = found || !f.Rel && uint64(v.Disp)&mask == want
				}
			}
			if !found {
				t.Fatalf("% x: immediate at %d/%d reads %#x, %v says %v", code[:inst.Len], f.ImmOff, f.ImmLen, want, inst.Op, inst.Args)
			}
			if f.Rel {
				rels++
			} else {
				imms++
			}
		}
		if f.DispLen == 0 {
			continue
		}
		mask := fieldMask(f.DispLen)
		want := fieldValue(code[f.DispOff:], f.DispLen) & mask
		var mem x86asm.Mem
		found, hasMem := false, false
		for _, a := range inst.Args {
			m, ok := a.(x86asm.Mem)
			if !ok {
				continue
			}
			hasMem = true
			if uint64(m.Disp)&mask == want {
				mem, found = m, true
				break
			}
		}
		if !hasMem {
			// The mod-ignored forms -- 0F 20..26, the moves to and from the
			// control, debug and test registers -- consume a modrm byte and
			// whatever it says the displacement is, but name no memory operand.
			// Their bytes are still the instruction's, so a rewrite of them is
			// still a rewrite of it; there is nothing to check the value against.
			unnamed++
			continue
		}
		if !found {
			t.Fatalf("% x: displacement at %d/%d reads %#x, %v says %v", code[:inst.Len], f.DispOff, f.DispLen, want, inst.Op, inst.Args)
		}
		base := BaseOther
		switch mem.Base {
		case 0:
			base = BaseAbs
		case x86asm.RIP, x86asm.EIP:
			base = BaseRIP
		case x86asm.RSP, x86asm.RBP:
			base = BaseSP
		}
		if base != f.Base {
			t.Fatalf("% x: base class %d, x86asm says %v (%d)", code[:inst.Len], f.Base, mem.Base, base)
		}
		seenBase[f.Base]++
		disps++
	}
	for b, n := range seenBase {
		if n == 0 {
			t.Fatalf("base class %d never occurred; the sweep does not cover it", b)
		}
	}
	if unnamed*20 > disps {
		t.Fatalf("%d of %d displacements had no memory operand to check against", unnamed, disps)
	}
	t.Logf("checked %d immediates, %d branch displacements, %d displacements %v (%d unnamed)", imms, rels, disps, seenBase, unnamed)
}

// TestWalkFieldsBoundaries checks WalkFields places instruction boundaries
// exactly where WalkInsns does, and reports what FieldsAt would have said at
// each of them. The two walks are the field layer's encoder and decoder
// halves; a boundary they disagreed on would shift a whole domain.
func TestWalkFieldsBoundaries(t *testing.T) {
	buf := make([]byte, 1<<20)
	rand.New(rand.NewSource(13)).Read(buf)
	var want []Insn
	WalkInsns(buf, func(in Insn) { want = append(want, in) })
	i := 0
	WalkFields(buf, func(start int, f Fields, ok bool) {
		if i >= len(want) {
			t.Fatalf("WalkFields visits %d instructions, WalkInsns %d", i+1, len(want))
		}
		if want[i].Start != start || want[i].Length != f.Len {
			t.Fatalf("instruction %d at %d/%d, WalkInsns says %d/%d", i, start, f.Len, want[i].Start, want[i].Length)
		}
		if g, gok := FieldsAt(buf[start:]); gok != ok || ok && g != f {
			t.Fatalf("instruction %d at %d: walk says %v/%v, FieldsAt %v/%v", i, start, f, ok, g, gok)
		}
		i++
	})
	if i != len(want) {
		t.Fatalf("WalkFields visits %d instructions, WalkInsns %d", i, len(want))
	}
}
