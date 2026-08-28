package x86

import "testing"

func TestTruncatedInstructionDoesNotPanic(t *testing.T) {
	code := []byte{0xc5, 0xf4}
	out := make([]byte, len(code))
	var st Stats
	Relocate(code, out, 0x1000, 0x2000, func(uint64) Target { return Target{} }, &st, nil)
	ContentHash(code)
	if !Equal(code, code) {
		t.Fatal("truncated code does not equal itself")
	}
	if st.Fails == 0 {
		t.Fatal("truncated instruction was not counted as a decode failure")
	}
}

func TestReferences(t *testing.T) {
	refs := References([]byte{0xe8, 5, 0, 0, 0}, 0x1000)
	if len(refs) != 1 || refs[0].Start != 0 || refs[0].Off != 1 || refs[0].N != 4 || refs[0].Next != 5 || refs[0].Target != 0x100a {
		t.Fatalf("references = %+v", refs)
	}
}

// hand-assembled encodings. x86asm decodes all of these -- its Len is right --
// but it reports PCRel=0 and renders the memory operand as an absolute
// [+disp32] for every VEX and EVEX form, which is the bug pcrelField covers.
var pcrelCases = []struct {
	name     string
	code     []byte
	wantOff  int // -1 means "no PC-relative field"
	wantLen  int // instruction length, which is what fixes the target
	wantDisp int64
}{
	{
		// VEX3, 2-byte payload: vbroadcastss xmm0, [rip+0x11223344]
		name: "vex3/vbroadcastss", wantOff: 5, wantLen: 9, wantDisp: 0x11223344,
		code: []byte{0xc4, 0xe2, 0x79, 0x18, 0x05, 0x44, 0x33, 0x22, 0x11},
	},
	{
		// VEX2, 1-byte payload: vmovss xmm0, [rip-0x10]
		name: "vex2/vmovss", wantOff: 4, wantLen: 8, wantDisp: -0x10,
		code: []byte{0xc5, 0xfa, 0x10, 0x05, 0xf0, 0xff, 0xff, 0xff},
	},
	{
		// EVEX, 3-byte payload: vmovaps zmm0, [rip+0x11223344]
		name: "evex/vmovaps", wantOff: 6, wantLen: 10, wantDisp: 0x11223344,
		code: []byte{0x62, 0xf1, 0x7c, 0x48, 0x28, 0x05, 0x44, 0x33, 0x22, 0x11},
	},
	{
		// EVEX with an imm8 after the displacement: vpalignr zmm0, zmm0,
		// [rip+0x11223344], 7. RIP is the end of the instruction, so the imm8
		// counts -- this is the case that pins the target on Len rather than
		// on where the displacement stops.
		name: "evex/vpalignr+imm8", wantOff: 6, wantLen: 11, wantDisp: 0x11223344,
		code: []byte{0x62, 0xf3, 0x7d, 0x48, 0x0f, 0x05, 0x44, 0x33, 0x22, 0x11, 0x07},
	},
	{
		// The same shape under VEX3: vpalignr ymm0, ymm0, [rip+..], 7.
		name: "vex3/vpalignr+imm8", wantOff: 5, wantLen: 10, wantDisp: 0x11223344,
		code: []byte{0xc4, 0xe3, 0x7d, 0x0f, 0x05, 0x44, 0x33, 0x22, 0x11, 0x07},
	},
	{
		// mod=11: vmovaps xmm0, xmm1. Register operand, no displacement.
		name: "vex2/register", wantOff: -1, wantLen: 4,
		code: []byte{0xc5, 0xf8, 0x28, 0xc1},
	},
	{
		// mod=00 rm=100: vmovdqa xmm0, [rax+rcx*4]. A SIB byte, not a
		// displacement.
		name: "vex2/sib", wantOff: -1, wantLen: 5,
		code: []byte{0xc5, 0xf9, 0x6f, 0x04, 0x88},
	},
	{
		// Legacy rip-relative, which x86asm does label: mov rax, [rip+..].
		name: "legacy/mov", wantOff: 3, wantLen: 7, wantDisp: 0x11223344,
		code: []byte{0x48, 0x8b, 0x05, 0x44, 0x33, 0x22, 0x11},
	},
	{
		name: "legacy/call rel32", wantOff: 1, wantLen: 5, wantDisp: 0x11223344,
		code: []byte{0xe8, 0x44, 0x33, 0x22, 0x11},
	},
	{
		name: "legacy/jmp rel8", wantOff: 1, wantLen: 2, wantDisp: -2,
		code: []byte{0xeb, 0xfe},
	},
}

func TestPCRelField(t *testing.T) {
	for _, c := range pcrelCases {
		t.Run(c.name, func(t *testing.T) {
			inst, err := decode(c.code)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if inst.Len != c.wantLen {
				t.Fatalf("Len = %d, want %d", inst.Len, c.wantLen)
			}
			off, n := pcrelField(inst, c.code)
			if c.wantOff < 0 {
				if n != 0 {
					t.Fatalf("pcrelField = (%d, %d), want no field", off, n)
				}
				return
			}
			if off != c.wantOff {
				t.Fatalf("pcrelField off = %d, want %d", off, c.wantOff)
			}
			if got := disp(c.code[off : off+n]); got != c.wantDisp {
				t.Fatalf("disp = %#x, want %#x", got, c.wantDisp)
			}
		})
	}
}

// TestPCRelTargetIsInstructionEnd checks the convention the whole package
// depends on: the target is measured from the byte after the instruction,
// which for the imm8 forms is past the immediate, not past the displacement.
func TestPCRelTargetIsInstructionEnd(t *testing.T) {
	const pc = 0x1000
	for _, c := range pcrelCases {
		if c.wantOff < 0 {
			continue
		}
		refs := References(c.code, pc)
		if len(refs) != 1 {
			t.Fatalf("%s: %d references, want 1", c.name, len(refs))
		}
		want := uint64(pc + int64(c.wantLen) + c.wantDisp)
		got := refs[0]
		if got.Target != want || got.Off != c.wantOff || got.Next != c.wantLen || got.Start != 0 {
			t.Fatalf("%s: ref = %+v, want target %#x off %d next %d",
				c.name, got, want, c.wantOff, c.wantLen)
		}
	}
}

func TestRelocateVEX(t *testing.T) {
	// vbroadcastss xmm0, [rip+0x1000] at 0x400000 -> the pool moved to
	// 0x500100 while the code moved to 0x480000.
	code := []byte{0xc4, 0xe2, 0x79, 0x18, 0x05, 0x00, 0x10, 0x00, 0x00}
	const srcPC, dstPC = 0x400000, 0x480000
	oldTarget := uint64(srcPC + len(code) + 0x1000)
	out := make([]byte, len(code))
	var st Stats
	var refs []Ref
	Relocate(code, out, srcPC, dstPC, func(a uint64) Target {
		if a != oldTarget {
			t.Errorf("lookup(%#x), want %#x", a, oldTarget)
		}
		return Target{Addr: 0x500100, Known: true}
	}, &st, &refs)
	if st.Refs != 1 || st.Unknown != 0 || st.NoFit != 0 || st.Fails != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if len(refs) != 1 || refs[0].Off != 5 || refs[0].N != 4 {
		t.Fatalf("refs = %+v", refs)
	}
	inst, err := decode(out)
	if err != nil {
		t.Fatalf("decode relocated: %v", err)
	}
	off, n := pcrelField(inst, out)
	if got, want := disp(out[off:off+n]), int64(0x500100-(dstPC+len(out))); got != want {
		t.Fatalf("new displacement = %#x, want %#x", got, want)
	}
}

// TestVEXDisplacementIsMasked is the reason ContentHash exists: a constant
// pool that moved must not make the function look changed.
func TestVEXDisplacementIsMasked(t *testing.T) {
	a := []byte{0xc4, 0xe2, 0x79, 0x18, 0x05, 0x44, 0x33, 0x22, 0x11}
	b := []byte{0xc4, 0xe2, 0x79, 0x18, 0x05, 0x99, 0x88, 0x77, 0x66}
	if !Equal(a, b) {
		t.Fatal("VEX rip-relative bodies differing only in displacement compare unequal")
	}
	if ContentHash(a) != ContentHash(b) {
		t.Fatal("VEX rip-relative displacement is not masked by ContentHash")
	}
	// The opcode still has to matter.
	c := append([]byte(nil), b...)
	c[3] = 0x19 // vbroadcastsd
	if Equal(a, c) || ContentHash(a) == ContentHash(c) {
		t.Fatal("a different opcode compares equal")
	}
}

// TestCanonical checks the mask the segment aligner runs on: the field
// pcrelField finds, zeroed in place, and the instruction boundaries the walk
// crossed, ending with the body length.
func TestCanonical(t *testing.T) {
	// two calls whose targets differ, then a ret
	a := []byte{0xE8, 0x10, 0, 0, 0, 0xE8, 0x20, 0, 0, 0, 0xC3}
	b := []byte{0xE8, 0x40, 0, 0, 0, 0xE8, 0x50, 0, 0, 0, 0xC3}
	ca, bounds := Canonical(a)
	cb, _ := Canonical(b)
	if string(ca) != string(cb) {
		t.Errorf("bodies differing only in their targets canonicalise to %x and %x", ca, cb)
	}
	if string(a) == string(ca) {
		t.Error("the displacements were not masked")
	}
	if a[1] != 0x10 {
		t.Error("Canonical wrote through to its input")
	}
	for i, want := range []int32{0, 5, 10, 11} {
		if i >= len(bounds) || bounds[i] != want {
			t.Fatalf("boundaries %v, want [0 5 10 11]", bounds)
		}
	}

	for _, tc := range pcrelCases {
		if tc.wantOff < 0 || tc.wantLen != len(tc.code) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			canon, bounds := Canonical(tc.code)
			if len(bounds) != 2 || bounds[0] != 0 || int(bounds[1]) != len(tc.code) {
				t.Fatalf("boundaries %v for one %d-byte instruction", bounds, len(tc.code))
			}
			inst, err := decode(tc.code)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			off, n := pcrelField(inst, tc.code)
			if off != tc.wantOff {
				t.Fatalf("the field is at %d, want %d", off, tc.wantOff)
			}
			if string(canon[:off]) != string(tc.code[:off]) ||
				string(canon[off+n:]) != string(tc.code[off+n:]) {
				t.Errorf("%x masked to %x: something outside the displacement changed", tc.code, canon)
			}
			for _, c := range canon[off : off+n] {
				if c != 0 {
					t.Fatalf("%x masked to %x: the displacement is not zero", tc.code, canon)
				}
			}
		})
	}
}
