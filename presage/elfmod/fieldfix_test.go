package elfmod

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// call writes a five-byte call at off whose displacement points at target.
func call(text []byte, textAddr uint64, off int, target uint64) {
	text[off] = 0xe8
	binary.LittleEndian.PutUint32(text[off+1:], uint32(int32(int64(target)-int64(textAddr)-int64(off+5))))
}

// TestFieldFixRoundTrip covers the three decisions the layer makes: a wrong
// address shared by several fields is remapped once, the majority destination
// wins and the minority falls through to a per-field delta, and a remap that
// would break more fields than it fixes is rejected in favour of deltas.
func TestFieldFixRoundTrip(t *testing.T) {
	const textAddr = 0x2000
	const A, B, C, D, E = 0x2100, 0x2200, 0x2300, 0x2400, 0x2500
	pred := bytes.Repeat([]byte{0x90}, 48)
	want := bytes.Repeat([]byte{0x90}, 48)
	// Three fields chose A; two of them should have chosen B and one C.
	for _, off := range []int{0, 8, 16} {
		call(pred, textAddr, off, A)
	}
	call(want, textAddr, 0, B)
	call(want, textAddr, 8, B)
	call(want, textAddr, 16, C)
	// Three fields chose D; only one is wrong, so remapping D would break two.
	for _, off := range []int{24, 32, 40} {
		call(pred, textAddr, off, D)
	}
	call(want, textAddr, 24, E)
	call(want, textAddr, 32, D)
	call(want, textAddr, 40, D)

	maps := []mapping{{Dst: 0, DstSize: 48}}
	if n := len(fieldSites(pred, maps, nil)); n != 6 {
		t.Fatalf("walked %d fields, want 6", n)
	}
	fx, st := encodeFieldFix(pred, want, textAddr, maps, nil)
	if st.Remaps != 1 || st.Skipped != 1 {
		t.Errorf("kept %d remaps and rejected %d, want 1 and 1", st.Remaps, st.Skipped)
	}
	if st.Remade != 3 {
		t.Errorf("remap rewrote %d fields, want 3", st.Remade)
	}
	if st.Deltas != 2 {
		t.Errorf("shipped %d field deltas, want 2", st.Deltas)
	}

	got := append([]byte(nil), pred...)
	rst, err := applyFieldFix(got, textAddr, maps, fx.marshal(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("replay = %x\nwant     %x", got, want)
	}
	if rst.Remaps != st.Remaps || rst.Deltas != st.Deltas || rst.Remade != st.Remade {
		t.Errorf("decoder stats %+v disagree with encoder %+v", rst, st)
	}
}

// TestFieldFixRejectsTruncation keeps a corrupt stream from writing anywhere.
func TestFieldFixRejectsTruncation(t *testing.T) {
	const textAddr = 0x2000
	pred := bytes.Repeat([]byte{0x90}, 32)
	want := bytes.Repeat([]byte{0x90}, 32)
	call(pred, textAddr, 0, 0x2100)
	call(want, textAddr, 0, 0x2200)
	maps := []mapping{{Dst: 0, DstSize: 32}}
	fx, _ := encodeFieldFix(pred, want, textAddr, maps, nil)
	b := fx.marshal()
	for n := range b {
		buf := append([]byte(nil), pred...)
		if _, err := applyFieldFix(buf, textAddr, maps, b[:n], nil); err == nil {
			t.Fatalf("accepted truncation to %d bytes", n)
		}
	}
}

// TestFieldFixNoWork is the case every rung below the last one hits.
func TestFieldFixNoWork(t *testing.T) {
	const textAddr = 0x2000
	pred := bytes.Repeat([]byte{0x90}, 32)
	call(pred, textAddr, 0, 0x2100)
	maps := []mapping{{Dst: 0, DstSize: 32}}
	fx, st := encodeFieldFix(pred, pred, textAddr, maps, nil)
	if st.Remaps != 0 || st.Deltas != 0 {
		t.Fatalf("a correct prediction produced %+v", st)
	}
	got := append([]byte(nil), pred...)
	if _, err := applyFieldFix(got, textAddr, maps, fx.marshal(), nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pred) {
		t.Error("an empty layer changed the prediction")
	}
}

// TestFieldFixWrappingAddress covers what a position-independent image makes
// routine and what the first whole-image run rejected: a displacement whose
// target address is below zero, so the address arithmetic wraps. The encoder
// wraps, so the decoder has to wrap identically rather than call it invalid.
func TestFieldFixWrappingAddress(t *testing.T) {
	const textAddr = 0x100
	pred := bytes.Repeat([]byte{0x90}, 32)
	want := bytes.Repeat([]byte{0x90}, 32)
	// Both fields point below the image base, at different wrapped addresses.
	below := func(n int64) uint64 { return uint64(-n) }
	call(pred, textAddr, 0, below(0x400))
	call(pred, textAddr, 8, below(0x400))
	call(want, textAddr, 0, below(0x800))
	call(want, textAddr, 8, below(0x800))
	maps := []mapping{{Dst: 0, DstSize: 32}}
	fx, st := encodeFieldFix(pred, want, textAddr, maps, nil)
	if st.Remaps != 1 || st.Remade != 2 {
		t.Fatalf("encoder produced %+v, want one remap over two fields", st)
	}
	got := append([]byte(nil), pred...)
	if _, err := applyFieldFix(got, textAddr, maps, fx.marshal(), nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("replay = %x\nwant     %x", got, want)
	}
}
