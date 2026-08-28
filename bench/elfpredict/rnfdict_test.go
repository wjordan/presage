package main

import (
	"bytes"
	"testing"

	"golang.org/x/arch/x86/x86asm"
)

// The field locator is the only new piece of decoding in the register-normal-
// form probe, and everything it reports rests on it pointing at the right
// bytes. x86asm agrees with it on PC-relative sites at run time; this pins the
// rest of the layout on encodings whose shape is known by hand.
func TestLocateFields(t *testing.T) {
	for _, c := range []struct {
		name             string
		code             []byte
		modrm, sib       int
		dispOff, dispLen int
		immOff, immLen   int
		opReg            int
		ext              bool
	}{
		{name: "mov rbp,rsp", code: []byte{0x48, 0x89, 0xe5}, modrm: 2, sib: -1, immOff: 3, opReg: -1},
		{name: "mov rax,[rbp-8]", code: []byte{0x48, 0x8b, 0x45, 0xf8}, modrm: 2, sib: -1,
			dispOff: 3, dispLen: 1, immOff: 4, opReg: -1},
		{name: "lea rdi,[rip+d32]", code: []byte{0x48, 0x8d, 0x3d, 0x12, 0x34, 0x56, 0x78}, modrm: 2, sib: -1,
			dispOff: 3, dispLen: 4, immOff: 7, opReg: -1},
		{name: "call rel32", code: []byte{0xe8, 0x44, 0x33, 0x22, 0x11}, modrm: -1, sib: -1,
			immOff: 1, immLen: 4, opReg: -1},
		{name: "je rel32", code: []byte{0x0f, 0x84, 0x10, 0x00, 0x00, 0x00}, modrm: -1, sib: -1,
			immOff: 2, immLen: 4, opReg: -1},
		{name: "push r15", code: []byte{0x41, 0x57}, modrm: -1, sib: -1, immOff: 2, opReg: 1},
		{name: "mov r14,[rsp+0x20]", code: []byte{0x4c, 0x8b, 0x74, 0x24, 0x20}, modrm: 2, sib: 3,
			dispOff: 4, dispLen: 1, immOff: 5, opReg: -1},
		{name: "mov eax,imm32", code: []byte{0xb8, 0x01, 0x00, 0x00, 0x00}, modrm: -1, sib: -1,
			immOff: 1, immLen: 4, opReg: 0},
		{name: "mov rax,-1", code: []byte{0x48, 0xc7, 0xc0, 0xff, 0xff, 0xff, 0xff}, modrm: 2, sib: -1,
			dispOff: 3, immOff: 3, immLen: 4, opReg: -1, ext: true},
		{name: "pxor xmm0,xmm0", code: []byte{0x66, 0x0f, 0xef, 0xc0}, modrm: 3, sib: -1, immOff: 4, opReg: -1},
		{name: "vxorps xmm0,xmm0,xmm0", code: []byte{0xc5, 0xf8, 0x57, 0xc0}, modrm: 3, sib: -1, immOff: 4, opReg: -1},
		{name: "mov [rdx+rax*4+8],ecx", code: []byte{0x89, 0x4c, 0x82, 0x08}, modrm: 1, sib: 2,
			dispOff: 3, dispLen: 1, immOff: 4, opReg: -1},
		{name: "movzx eax,[rax+rcx]", code: []byte{0x0f, 0xb6, 0x04, 0x08}, modrm: 2, sib: 3, immOff: 4, opReg: -1},
		{name: "ret", code: []byte{0xc3}, modrm: -1, sib: -1, immOff: 1, opReg: -1},
		{name: "add rsp,0x20", code: []byte{0x48, 0x83, 0xc4, 0x20}, modrm: 2, sib: -1,
			dispOff: 3, immOff: 3, immLen: 1, opReg: -1, ext: true},
	} {
		inst, err := x86asm.Decode(c.code, 64)
		if err != nil || inst.Len != len(c.code) {
			t.Fatalf("%s: x86asm decoded %d of %d bytes (%v)", c.name, inst.Len, len(c.code), err)
		}
		f, ok := locateFields(c.code)
		if !ok {
			t.Fatalf("%s: locateFields failed", c.name)
		}
		got := [7]int{f.modrm, f.sib, f.dispOff, f.dispLen, f.immOff, f.immLen, f.opRegAt}
		want := [7]int{c.modrm, c.sib, c.dispOff, c.dispLen, c.immOff, c.immLen, c.opReg}
		if c.dispLen == 0 {
			want[2], got[2] = 0, 0 // dispOff is meaningless without a displacement
		}
		if got != want {
			t.Errorf("%s: modrm/sib/disp/dispLen/imm/immLen/opReg = %v, want %v", c.name, got, want)
		}
		if f.regIsExtension() != c.ext {
			t.Errorf("%s: regIsExtension = %v, want %v", c.name, f.regIsExtension(), c.ext)
		}
		if inst.PCRel > 0 && f.dispLen != inst.PCRel && f.immLen != inst.PCRel {
			t.Errorf("%s: no field of width %d at %d", c.name, inst.PCRel, inst.PCRelOff)
		}
	}
}

func TestRegisterNormalForm(t *testing.T) {
	// mov rbp,rsp then mov rsp,rbp: rsp is seen first (in the reg field of the
	// first instruction) so it ranks 0 and rbp ranks 1, and the two
	// instructions become each other's mirror.
	in := []byte{0x48, 0x89, 0xe5, 0x48, 0x89, 0xec}
	want := []byte{0x48, 0x89, 0xc1, 0x48, 0x89, 0xc8}
	if got := transformSegments(in, modeRNFBank, nil, nil, nil); !bytes.Equal(got, want) {
		t.Errorf("renumbered = % x, want % x", got, want)
	}

	// The same code with the two registers swapped normalises to the same
	// bytes -- that is the whole point of the form.
	swapped := []byte{0x48, 0x89, 0xd9, 0x48, 0x89, 0xcb} // mov rcx,rbx; mov rbx,rcx
	if got := transformSegments(swapped, modeRNFBank, nil, nil, nil); !bytes.Equal(got, want) {
		t.Errorf("swapped renumbered = % x, want % x", got, want)
	}

	// A high-bank register keeps its bank, so the REX bit it needs is still
	// there: mov r14,r15 -> mov r8,r9.
	if got := transformSegments([]byte{0x4d, 0x89, 0xfe}, modeRNFBank, nil, nil, nil); !bytes.Equal(got, []byte{0x4d, 0x89, 0xc1}) {
		t.Errorf("high bank renumbered = % x, want 4d 89 c1", got)
	}

	// Group opcodes spend modrm.reg on the operation, so it must survive.
	if got := transformSegments([]byte{0x48, 0x83, 0xc4, 0x20}, modeRNFBank, nil, nil, nil); got[2]&0x38 != 0 {
		t.Errorf("add rsp,0x20 renumbered its /digit: % x", got)
	}

	// All-fields zeroing keeps instruction length and clears both the
	// displacement and the immediate.
	all := transformSegments([]byte{0x48, 0xc7, 0x45, 0xf8, 0x2a, 0x00, 0x00, 0x00}, modeAllFields, nil, nil, nil)
	if !bytes.Equal(all, []byte{0x48, 0xc7, 0x45, 0x00, 0x00, 0x00, 0x00, 0x00}) {
		t.Errorf("all fields zeroed = % x", all)
	}
}
