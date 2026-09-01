package elfmod

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

// insn appends an instruction whose trailing four bytes are the field value.
func insn(b []byte, head []byte, v uint32) []byte {
	b = append(b, head...)
	return binary.LittleEndian.AppendUint32(b, v)
}

// The two field values every case here moves between: one away from a carry,
// so the difference is a byte and the bytes it fixes are three.
const opLo, opHi = 0x007fffff, 0x00800000

// opCase builds a body holding one instruction of every class the layer
// codes, plus one the layer must refuse. imm is separate because the
// immediate class ships the value and not the difference.
func opCase(v, imm uint32, opcode byte) []byte {
	var b []byte
	b = insn(b, []byte{0x48, 0x8b, 0x85}, v)       // mov rax, [rbp+disp32]
	b = insn(b, []byte{0x48, 0x8b, 0x87}, v)       // mov rax, [rdi+disp32]
	b = insn(b, []byte{0x48, 0x8b, 0x05}, v)       // mov rax, [rip+disp32]
	b = insn(b, []byte{0x48, 0x8b, 0x04, 0x25}, v) // mov rax, [disp32]
	b = insn(b, []byte{0x48, 0xc7, 0xc0}, imm)     // mov rax, imm32
	b = insn(b, []byte{0xe8}, v)                   // call rel32
	b = insn(b, []byte{opcode, 0xc7, 0xc0}, imm)   // same, under a changed opcode
	return b
}

func diffBytes(a, b []byte) int {
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

// TestOpFieldRoundTrip covers the layer's whole contract on one body: every
// scalar class is located, corrected and replayed by the decode-side apply
// over an untouched prediction, an instruction whose opcode moved is left to
// the residual, and the wrong bytes the plan claims to remove are exactly the
// ones it removes.
func TestOpFieldRoundTrip(t *testing.T) {
	pred := opCase(opLo, opLo, 0x48)
	want := opCase(opHi, 1, 0x49)
	maps := []mapping{{Dst: 0, DstSize: uint64(len(pred))}}

	plan, st := encodeOpField(pred, want, maps)
	if len(plan) == 0 {
		t.Fatal("no plan for a body of six correctable fields")
	}
	for c, n := range st.Kept {
		if n != 1 {
			t.Fatalf("class %s shipped %d entries, want 1 (entries %d, domain %d)",
				opClassNames[c], n, st.Entries[c], st.Domain[c])
		}
	}

	got := bytes.Clone(pred)
	ast, err := applyOpField(got, maps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ast.Kept != st.Kept {
		t.Fatalf("applied %v entries, encoder shipped %v", ast.Kept, st.Kept)
	}
	// Everything but the refused instruction is now exact, and the refused
	// one is untouched: its opcode is not a field.
	const tail = 7 // the last instruction's length
	if !bytes.Equal(got[:len(got)-tail], want[:len(want)-tail]) {
		t.Fatalf("corrected body\n got %x\nwant %x", got, want)
	}
	if !bytes.Equal(got[len(got)-tail:], pred[len(pred)-tail:]) {
		t.Fatalf("the refused instruction was rewritten: %x", got[len(got)-tail:])
	}
	if _, _, _, gain, _ := st.totals(); diffBytes(pred, want)-diffBytes(got, want) != gain {
		t.Fatalf("plan removed %d wrong bytes, priced at %d", diffBytes(pred, want)-diffBytes(got, want), gain)
	}
}

// TestOpFieldDeclines is the drop-to-zero-bytes property: a prediction with
// nothing to fix, and one whose only correction is narrower than the column
// that would carry it, both ship no plan at all.
func TestOpFieldDeclines(t *testing.T) {
	pred := opCase(opLo, opLo, 0x48)
	maps := []mapping{{Dst: 0, DstSize: uint64(len(pred))}}
	if plan, _ := encodeOpField(pred, bytes.Clone(pred), maps); len(plan) != 0 {
		t.Fatalf("an exact prediction shipped %d plan bytes", len(plan))
	}
	// One byte of one disp8: the index and the value cost more than the one
	// byte the residual would have carried.
	small := []byte{0x48, 0x8b, 0x45, 0x08, 0x90}
	target := bytes.Clone(small)
	target[3] = 0x10
	if plan, st := encodeOpField(small, target, []mapping{{Dst: 0, DstSize: uint64(len(small))}}); len(plan) != 0 {
		t.Fatalf("a one-byte correction shipped %d plan bytes (%v)", len(plan), st.Entries)
	}
}

// TestOpFieldRandomRoundTrip runs the encoder and the decode-side apply over
// many bodies of instructions, half of which move a field: the shard cursors,
// the per-body bases and the index gaps only appear at this size, and the plan
// must remove exactly the wrong bytes it prices.
func TestOpFieldRandomRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	heads := [][]byte{
		{0x48, 0x8b, 0x85}, {0x48, 0x8b, 0x87}, {0x48, 0x8b, 0x05},
		{0x48, 0x8b, 0x04, 0x25}, {0x48, 0xc7, 0xc0}, {0xe8},
	}
	var pred, want []byte
	var maps []mapping
	for len(pred) < 64<<10 {
		// A third of the bodies the prediction got entirely right, so the
		// replay's skip of a body holding no entry is exercised.
		exact := r.Intn(3) == 0
		start := len(pred)
		for len(pred) < start+4096 {
			h := heads[r.Intn(len(heads))]
			v := uint32(opLo)
			if exact || r.Intn(4) == 0 {
				v = opHi
			}
			pred = insn(pred, h, v)
			want = insn(want, h, opHi)
		}
		maps = append(maps, mapping{Dst: uint64(start), DstSize: uint64(len(pred) - start)})
	}
	plan, st := encodeOpField(pred, want, maps)
	if len(plan) == 0 {
		t.Fatal("no plan for thousands of correctable fields")
	}
	got := bytes.Clone(pred)
	ast, err := applyOpField(got, maps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ast.Kept != st.Kept || ast.Domain != st.Domain {
		t.Fatalf("apply saw %v/%v, encoder %v/%v", ast.Domain, ast.Kept, st.Domain, st.Kept)
	}
	if _, _, _, gain, _ := st.totals(); diffBytes(pred, want)-diffBytes(got, want) != gain {
		t.Fatalf("plan removed %d wrong bytes, priced at %d", diffBytes(pred, want)-diffBytes(got, want), gain)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("a correctable field was left wrong")
	}
}

// TestOpFieldRejectsBadPlan checks the decoder refuses a plan that names a
// field outside the domain it enumerates rather than writing somewhere else.
func TestOpFieldRejectsBadPlan(t *testing.T) {
	pred := opCase(opLo, opLo, 0x48)
	maps := []mapping{{Dst: 0, DstSize: uint64(len(pred))}}
	plan := []byte{1 << opImm}
	plan = appendStream(plan, appendU(nil, 99))
	plan = appendStream(plan, appendS(nil, 1))
	if _, err := applyOpField(bytes.Clone(pred), maps, plan); err == nil {
		t.Fatal("a plan naming field 99 of a two-field domain was accepted")
	}
}
