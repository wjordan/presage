package main

import "testing"

// regions must agree with delta/correct.go's region loop: every "runs"
// column in the report is priced against a correction the codec wrote.
func TestRegionsMergeShortGaps(t *testing.T) {
	pred := []byte("abcdefghijklmnop")
	want := []byte("Xbcdefghijklmnop")
	if got := regions(pred, want); len(got) != 1 || got[0] != (reg{0, 1}) {
		t.Fatalf("single wrong byte: %v", got)
	}
	// two wrong bytes three apart: the gap is shorter than mergeGap, so the
	// codec pays one region header, not two
	want = []byte("XbcXefghijklmnop")
	if got := regions(pred, want); len(got) != 1 || got[0] != (reg{0, 4}) {
		t.Fatalf("short gap should merge: %v", got)
	}
	// six identical bytes between them is a second region
	want = []byte("Xbcdefg]ijklmnop")
	if got := regions(pred, want); len(got) != 2 {
		t.Fatalf("long gap should split: %v", got)
	}
}

func TestCanonicaliseZeroesBranchTargets(t *testing.T) {
	// e8 rel32 call, twice, to different targets
	a := []byte{0xe8, 0x10, 0x00, 0x00, 0x00, 0xc3}
	b := []byte{0xe8, 0x44, 0x33, 0x22, 0x11, 0xc3}
	if string(canonicalise(a)) != string(canonicalise(b)) {
		t.Fatalf("canonicalise did not zero the rel32: %x vs %x", canonicalise(a), canonicalise(b))
	}
	if canonicalise(a)[5] != 0xc3 {
		t.Fatal("canonicalise moved a non-displacement byte")
	}
}

func TestOperandClass(t *testing.T) {
	dec := func(b []byte) opInst {
		i, ok := safeDecode(b)
		if !ok {
			t.Fatalf("decode %x", b)
		}
		return opInst(i)
	}
	cases := []struct {
		name string
		a, b []byte
		want int
	}{
		// mov rax,rcx vs mov rax,rdx -- one register moved
		{"register", []byte{0x48, 0x89, 0xc8}, []byte{0x48, 0x89, 0xd0}, clsReg},
		// mov eax,[rsp+8] vs mov eax,[rsp+0x10] -- frame displacement
		{"frame disp", []byte{0x8b, 0x44, 0x24, 0x08}, []byte{0x8b, 0x44, 0x24, 0x10}, clsDispSP},
		// add eax,1 vs add eax,2 -- immediate only
		{"immediate", []byte{0x83, 0xc0, 0x01}, []byte{0x83, 0xc0, 0x02}, clsImm},
		// mov eax,ecx vs add eax,ecx -- op only
		{"op only", []byte{0x89, 0xc8}, []byte{0x01, 0xc8}, clsOpOnly},
	}
	for _, tc := range cases {
		if got := compareOperands(dec(tc.a), dec(tc.b)).class(); got != tc.want {
			t.Errorf("%s: class %s, want %s", tc.name, opClassNames[got], opClassNames[tc.want])
		}
	}
}

func TestRenumberKey(t *testing.T) {
	for _, tc := range [][2]string{
		{"main.run.func1", "main.run.func#"},
		{"main.run.func12", "main.run.func#"},
		{"main.f.deferwrap3", "main.f.deferwrap#"},
		{"slices.Sort[go.shape.int]", "slices.Sort[#]"},
		{"maps.keys[go.shape.string,go.shape.[]uint8]", "maps.keys[#]"},
	} {
		if got := renumberKey(tc[0]); got != tc[1] {
			t.Errorf("renumberKey(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
	if renumberKey("main.a") == renumberKey("main.b") {
		t.Error("renumberKey collapsed two unrelated names")
	}
}

func TestRenumberKeyNestedAndRepeated(t *testing.T) {
	for _, tc := range [][2]string{
		{"a[x][y]", "a[#][#]"},
		{"a[b[c]]d", "a[#]d"},
		{"a[unterminated", "a[#]"},
		{"plain", "plain"},
	} {
		if got := renumberKey(tc[0]); got != tc[1] {
			t.Errorf("renumberKey(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}
