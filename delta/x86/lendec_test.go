package x86

import (
	"math/rand"
	"testing"
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
