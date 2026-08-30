package x86

import (
	"debug/elf"
	"math/rand"
	"os"
	"reflect"
	"runtime"
	"testing"
)

// walkSlow is the pre-hoist WalkReferences: a deferred recover on every
// instruction. Kept here only to A/B the hoist against; the package uses the
// hoisted walkFrom.
func walkSlow(code []byte, pc uint64, visit func(Reference)) {
	for i := 0; i < len(code); {
		inst, err := decode(code[i:])
		if err != nil || inst.Len == 0 {
			i++
			continue
		}
		if off, n := pcrelField(inst, code[i:]); n == 1 || n == 2 || n == 4 {
			d := disp(code[i+off : i+off+n])
			visit(Reference{
				Start: i, Off: i + off, N: n, Next: i + inst.Len,
				Target: uint64(int64(pc) + int64(i) + int64(inst.Len) + d),
			})
		}
		i += inst.Len
	}
}

func collect(walk func(func(Reference))) []Reference {
	var refs []Reference
	walk(func(r Reference) { refs = append(refs, r) })
	return refs
}

// alignedBodies cuts code into ~n bodies at instruction boundaries, so a
// concatenated parallel walk must equal a serial one. This models the
// function-body list the whole-image predictor already holds; here the
// boundaries are found by a serial decode.
func alignedBodies(code []byte, pc uint64, n int) ([]Body, []int) {
	target := len(code) / n
	var bodies []Body
	var offs []int
	start := 0
	for i := 0; i < len(code); {
		inst, err := decode(code[i:])
		step := 1
		if err == nil && inst.Len > 0 {
			step = inst.Len
		}
		i += step
		if i-start >= target && i < len(code) {
			bodies = append(bodies, Body{Code: code[start:i], PC: pc + uint64(start)})
			offs = append(offs, start)
			start = i
		}
	}
	bodies = append(bodies, Body{Code: code[start:], PC: pc + uint64(start)})
	offs = append(offs, start)
	return bodies, offs
}

// flattenBodies concatenates per-body references in order, shifting the
// body-local offsets back to image positions, so the result is comparable to a
// serial walk over the whole buffer.
func flattenBodies(res [][]Reference, offs []int) []Reference {
	var out []Reference
	for k, refs := range res {
		g := offs[k]
		for _, r := range refs {
			r.Start += g
			r.Off += g
			r.Next += g
			out = append(out, r)
		}
	}
	return out
}

// TestWalkVariantsAgree checks the hoisted serial walk and the parallel
// body walk against the per-instruction-recover baseline, including the
// panic-resume path, on deterministic bytes so it stays fast.
func TestWalkVariantsAgree(t *testing.T) {
	code := make([]byte, 4<<20)
	r := rand.New(rand.NewSource(1))
	r.Read(code)

	want := collect(func(v func(Reference)) { walkSlow(code, 0x400000, v) })
	got := collect(func(v func(Reference)) { WalkReferences(code, 0x400000, v) })
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("hoisted walk differs: %d refs vs %d", len(got), len(want))
	}
	if len(want) == 0 {
		t.Fatal("no references found; input not exercising the decoder")
	}
	for _, n := range []int{2, 7, 64} {
		bodies, offs := alignedBodies(code, 0x400000, n)
		par := flattenBodies(WalkBodies(bodies, 4), offs)
		if !reflect.DeepEqual(want, par) {
			t.Fatalf("parallel walk over %d bodies differs: %d refs vs %d", n, len(par), len(want))
		}
	}
}

// benchText loads .text from the ELF named by X86_BENCH_BIN, skipping when
// unset so the normal suite never reads a 291 MB file.
func benchText(tb testing.TB) ([]byte, uint64) {
	path := os.Getenv("X86_BENCH_BIN")
	if path == "" {
		tb.Skip("set X86_BENCH_BIN to an ELF to benchmark the real .text")
	}
	f, err := elf.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	s := f.Section(".text")
	if s == nil {
		tb.Fatal("no .text")
	}
	code, err := s.Data()
	if err != nil {
		tb.Fatal(err)
	}
	return code, s.Addr
}

func BenchmarkWalkSerialSlow(b *testing.B) {
	code, pc := benchText(b)
	b.SetBytes(int64(len(code)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		walkSlow(code, pc, func(Reference) { n++ })
		_ = n
	}
}

func BenchmarkWalkSerialFast(b *testing.B) {
	code, pc := benchText(b)
	b.SetBytes(int64(len(code)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		WalkReferences(code, pc, func(Reference) { n++ })
		_ = n
	}
}

func BenchmarkWalkParallel(b *testing.B) {
	code, pc := benchText(b)
	bodies, _ := alignedBodies(code, pc, runtime.GOMAXPROCS(0)*4)
	b.SetBytes(int64(len(code)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		WalkBodies(bodies, runtime.GOMAXPROCS(0))
	}
}
