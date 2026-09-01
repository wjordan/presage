package elfmod

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/wjordan/presage/delta/x86"
)

// buildCIE emits a minimal CIE with a "zR" augmentation declaring the
// pcrel|sdata4 FDE pointer encoding clang uses.
func buildCIE() []byte {
	b := []byte{
		0, 0, 0, 0, // length, patched below
		0, 0, 0, 0, // CIE id
		1,           // version
		'z', 'R', 0, // augmentation
		1,    // code alignment factor
		0x78, // data alignment factor (-8)
		16,   // return address register
		1,    // augmentation data length
		ehPtrEncPCRelSData4,
		0, 0, 0, // padding
	}
	binary.LittleEndian.PutUint32(b, uint32(len(b)-4))
	return b
}

// buildFDE emits an FDE at secOff whose initial_location points at target.
func buildFDE(secAddr, secOff, cieOff, target, size uint64) []byte {
	b := make([]byte, 20)
	binary.LittleEndian.PutUint32(b, uint32(len(b)-4))
	binary.LittleEndian.PutUint32(b[4:], uint32(secOff+4-cieOff))
	loc := int64(target) - int64(secAddr+secOff+8)
	binary.LittleEndian.PutUint32(b[8:], uint32(int32(loc)))
	binary.LittleEndian.PutUint32(b[12:], uint32(size))
	return b
}

func TestEhFrameWalkAndHeader(t *testing.T) {
	const secAddr, secOff = uint64(0x40000), uint64(0x1000)
	const hdrAddr, hdrOff = uint64(0x30000), uint64(0x800)

	sec := buildCIE()
	// Deliberately out of target order: .eh_frame is emitted in link order,
	// and the header index has to come out sorted regardless.
	sec = append(sec, buildFDE(secAddr, uint64(len(sec)), 0, 0x9000, 0x40)...)
	sec = append(sec, buildFDE(secAddr, uint64(len(sec)), 0, 0x7000, 0x30)...)
	sec = append(sec, buildFDE(secAddr, uint64(len(sec)), 0, 0x8000, 0x20)...)
	sec = append(sec, 0, 0, 0, 0) // terminator

	fdes, _ := walkEhFrame(sec)
	if len(fdes) != 3 {
		t.Fatalf("walked %d FDEs, want 3", len(fdes))
	}

	image := make([]byte, 0x2000)
	copy(image[secOff:], sec)
	hdrSize := uint64(ehHdrPrefix + 3*8)
	hdr := image[hdrOff : hdrOff+hdrSize]
	hdr[0], hdr[1], hdr[2], hdr[3] = 1, ehPtrEncPCRelSData4, ehEncUData4, ehTableEncDataRel4

	p := ehFramePlan{
		NewOff: secOff, NewSize: uint64(len(sec)), NewAddr: secAddr,
		HdrOff: hdrOff, HdrSize: hdrSize, HdrAddr: hdrAddr,
	}
	if !buildEhFrameHdr(hdr, image[secOff:secOff+p.NewSize], p) {
		t.Fatal("buildEhFrameHdr declined a well-formed section")
	}
	if got := int32(binary.LittleEndian.Uint32(hdr[4:])); got != int32(secAddr)-int32(hdrAddr+4) {
		t.Errorf("eh_frame_ptr = %d, want %d", got, int32(secAddr)-int32(hdrAddr+4))
	}
	if got := binary.LittleEndian.Uint32(hdr[8:]); got != 3 {
		t.Errorf("fde_count = %d, want 3", got)
	}
	want := []int32{0x7000 - int32(hdrAddr), 0x8000 - int32(hdrAddr), 0x9000 - int32(hdrAddr)}
	for i, w := range want {
		got := int32(binary.LittleEndian.Uint32(hdr[ehHdrPrefix+i*8:]))
		if got != w {
			t.Errorf("header entry %d location = %d, want %d (table must be sorted by address)", i, got, w)
		}
	}
}

// TestEhFrameReplay is the decoder path end to end on a synthetic pair: the
// old section's FDEs are projected through an identity-shifted equivalence,
// retargeted through a lookup that moves every function and resized where
// the map agrees about the old size. The result must equal a section built
// directly for the new layout, and the header rebuilt from it must equal the
// header the new layout implies.
func TestEhFrameReplay(t *testing.T) {
	const oldAddr, oldOff = uint64(0x40000), uint64(0x1000)
	const newAddr, newOff = uint64(0x50000), uint64(0x1800) // section moved
	const hdrAddr, hdrOff = uint64(0x30000), uint64(0x800)
	build := func(secAddr uint64, targets [3]uint64, sizes [3]uint64) []byte {
		sec := buildCIE()
		for i := range targets {
			sec = append(sec, buildFDE(secAddr, uint64(len(sec)), 0, targets[i], sizes[i])...)
		}
		return append(sec, 0, 0, 0, 0)
	}
	oldSec := build(oldAddr, [3]uint64{0x9000, 0x7000, 0x8000}, [3]uint64{0x40, 0x30, 0x20})
	// Every function moves by 0x100; the second grows, and the map knows it.
	newSec := build(newAddr, [3]uint64{0x9100, 0x7100, 0x8100}, [3]uint64{0x40, 0x38, 0x20})
	old := make([]byte, 0x2000)
	copy(old[oldOff:], oldSec)
	out := make([]byte, 0x2000)
	copy(out[newOff:], oldSec) // the equivalence copy laid the old bytes down
	hdrSize := uint64(ehHdrPrefix + 3*8)
	out[hdrOff], out[hdrOff+1], out[hdrOff+2], out[hdrOff+3] = 1, ehPtrEncPCRelSData4, ehEncUData4, ehTableEncDataRel4
	ep := equivalencePlan{OldLen: uint64(len(old)), NewLen: uint64(len(out)),
		Eqs: []equivalence{{Src: oldOff, Dst: newOff, N: uint64(len(oldSec))}}}
	p := ehFramePlan{OldOff: oldOff, OldSize: uint64(len(oldSec)), NewOff: newOff, NewSize: uint64(len(newSec)),
		OldAddr: oldAddr, NewAddr: newAddr, HdrOff: hdrOff, HdrSize: hdrSize, HdrAddr: hdrAddr, HdrExact: true}
	rt, err := unmarshalEhFramePlan(p.marshal())
	if err != nil || rt != p {
		t.Fatalf("plan round trip: %v %+v", err, rt)
	}
	oldSecs := sectionMap{{Old: oldAddr, New: oldOff, Size: uint64(len(oldSec))}}
	lookup := func(a uint64) x86.Target { return x86.Target{Addr: a + 0x100, Known: true} }
	extent := func(a uint64) (uint64, uint64, bool) {
		if a == 0x7000 {
			return 0x30, 0x38, true
		}
		return 0, 0, false
	}
	st := applyEhFrame(out, old, ep, rt, oldSecs, lookup, extent)
	if st.FDEs != 3 || st.Retargeted != 3 || st.Resized != 1 {
		t.Fatalf("stats %+v", st)
	}
	if got := out[newOff : newOff+uint64(len(newSec))]; !bytes.Equal(got, newSec) {
		t.Fatalf("replayed .eh_frame differs:\n got %x\nwant %x", got, newSec)
	}
	// The header is the decoder's last step, over the corrected section.
	if !buildEhFrameHdr(out[hdrOff:hdrOff+hdrSize], out[newOff:newOff+uint64(len(newSec))], rt) {
		t.Fatal("buildEhFrameHdr declined the replayed section")
	}
	hdr := out[hdrOff : hdrOff+hdrSize]
	want := []int32{0x7100 - int32(hdrAddr), 0x8100 - int32(hdrAddr), 0x9100 - int32(hdrAddr)}
	for i, w := range want {
		if got := int32(binary.LittleEndian.Uint32(hdr[ehHdrPrefix+i*8:])); got != w {
			t.Errorf("header entry %d = %d, want %d", i, got, w)
		}
	}
}

// A CIE with a personality routine ("zPLR", as every C++ CIE with landing
// pads has) still governs FDEs the header must index.
func TestCIEPersonalityEncoding(t *testing.T) {
	cie := []byte{
		0, 0, 0, 0, 0, 0, 0, 0, 1,
		'z', 'P', 'L', 'R', 0,
		1, 0x78, 16,
		7,                // augmentation data length
		0x9b, 1, 2, 3, 4, // personality: indirect|pcrel|sdata4 + pointer
		0x1b,                // LSDA encoding
		ehPtrEncPCRelSData4, // FDE pointer encoding
	}
	binary.LittleEndian.PutUint32(cie, uint32(len(cie)-4))
	if enc := cieFDEPointerEncoding(cie); enc != ehPtrEncPCRelSData4 {
		t.Fatalf("encoding %#x, want %#x", enc, ehPtrEncPCRelSData4)
	}
}

// Two FDEs for one location (identical-code folding) index once, by the
// first FDE in section order.
func TestEhFrameHeaderFoldedDuplicates(t *testing.T) {
	const secAddr, secOff = uint64(0x40000), uint64(0x1000)
	const hdrAddr, hdrOff = uint64(0x30000), uint64(0x800)
	sec := buildCIE()
	sec = append(sec, buildFDE(secAddr, uint64(len(sec)), 0, 0x8000, 0x40)...)
	first := uint64(len(sec))
	sec = append(sec, buildFDE(secAddr, uint64(len(sec)), 0, 0x7000, 0x30)...)
	sec = append(sec, buildFDE(secAddr, uint64(len(sec)), 0, 0x7000, 0x30)...)
	image := make([]byte, 0x2000)
	copy(image[secOff:], sec)
	hdrSize := uint64(ehHdrPrefix + 3*8)
	copy(image[hdrOff:], []byte{1, ehPtrEncPCRelSData4, ehEncUData4, ehTableEncDataRel4})
	p := ehFramePlan{OldOff: secOff, OldSize: uint64(len(sec)), NewOff: secOff, NewSize: uint64(len(sec)),
		OldAddr: secAddr, NewAddr: secAddr, HdrOff: hdrOff, HdrSize: hdrSize, HdrAddr: hdrAddr}
	if !buildEhFrameHdr(image[hdrOff:hdrOff+hdrSize], image[secOff:secOff+p.NewSize], p) {
		t.Fatal("buildEhFrameHdr declined a folded section")
	}
	hdr := image[hdrOff:]
	if got := binary.LittleEndian.Uint32(hdr[8:]); got != 2 {
		t.Fatalf("fde_count = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(hdr[ehHdrPrefix+4:]); uint64(int32(got)) != secAddr+first-hdrAddr {
		t.Fatalf("first entry points at %#x, want the first FDE at %#x", got, secAddr+first-hdrAddr)
	}
}

// The Finaliser seam: the encoder masks the section it does not ship, the
// decoder rebuilds it from the corrected .eh_frame, and what the two agree
// on is the target's own bytes.
func TestEhFrameHdrFinaliser(t *testing.T) {
	const secAddr, secOff = uint64(0x40000), uint64(0x1000)
	const hdrAddr, hdrOff = uint64(0x30000), uint64(0x800)
	sec := buildCIE()
	sec = append(sec, buildFDE(secAddr, uint64(len(sec)), 0, 0x9000, 0x40)...)
	sec = append(sec, buildFDE(secAddr, uint64(len(sec)), 0, 0x7000, 0x30)...)
	sec = append(sec, 0, 0, 0, 0)
	hdrSize := uint64(ehHdrPrefix + 2*8)
	p := ehFramePlan{NewOff: secOff, NewSize: uint64(len(sec)), NewAddr: secAddr,
		HdrOff: hdrOff, HdrSize: hdrSize, HdrAddr: hdrAddr}

	target := make([]byte, 0x2000)
	copy(target[secOff:], sec)
	if ehFrameHdrDerivable(target, p) {
		t.Fatal("an unwritten header must not read as derivable")
	}
	buildEhFrameHdr(target[hdrOff:hdrOff+hdrSize], target[secOff:secOff+p.NewSize], p)
	if !ehFrameHdrDerivable(target, p) {
		t.Fatal("the target's own header must read as derivable")
	}

	p.HdrExact = true
	plan := packedPlan(t, planStreams{EhFrame: p.marshal()})
	// The prediction has the section right and the header wrong.
	pred := make([]byte, len(target))
	copy(pred[secOff:], sec)
	if masked := (Module{}).MaskResidual(plan, pred, target); !bytes.Equal(masked[hdrOff:hdrOff+hdrSize], target[hdrOff:hdrOff+hdrSize]) {
		t.Fatal("MaskResidual left the header unmasked")
	} else if bytes.Equal(pred[hdrOff:hdrOff+hdrSize], target[hdrOff:hdrOff+hdrSize]) {
		t.Fatal("MaskResidual must not disturb the hashed prediction")
	}
	if err := (Module{}).Finalise(plan, pred); err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if !bytes.Equal(pred, target) {
		t.Fatal("Finalise did not reproduce the target's header")
	}
}

// packedPlan marshals and packs a plan the way Analyse ships it, so the
// Finaliser seam is exercised on the bytes the container carries.
func packedPlan(t *testing.T, cp planStreams) []byte {
	t.Helper()
	b, _ := packPlan(cp.marshal())
	return b
}

// An FDE's cie_ptr is a distance, not an address: it changes whenever the
// entry and its CIE move apart, which is every FDE downstream of an
// insertion. Two CIEs, and a gap opened in front of each group of FDEs they
// govern, so no FDE keeps the distance it was copied with.
func TestEhFrameCiePointers(t *testing.T) {
	const addr, oldOff, newOff = uint64(0x40000), uint64(0x1000), uint64(0x1000)
	sec := buildCIE()
	off1 := uint64(len(sec))
	sec = append(sec, buildFDE(addr, off1, 0, 0x8000, 0x10)...)
	off2 := uint64(len(sec))
	sec = append(sec, buildCIE()...) // a second CIE, governing what follows
	off3 := uint64(len(sec))
	sec = append(sec, buildFDE(addr, off3, off2, 0x9000, 0x10)...)
	off4 := uint64(len(sec))
	sec = append(sec, buildFDE(addr, off4, off2, 0xa000, 0x10)...)
	sec = append(sec, 0, 0, 0, 0)

	// The new layout inserts 4 bytes before the first FDE and 8 more before
	// the second CIE's FDEs. The section keeps its address, so nothing but
	// the distances moves.
	shifts := []struct{ src, n, shift uint64 }{
		{0, off1, 0},                        // the first CIE
		{off1, off2 - off1, 4},              // its FDE
		{off2, off3 - off2, 4},              // the second CIE
		{off3, uint64(len(sec)) - off3, 12}, // its two FDEs
	}
	old := make([]byte, 0x4000)
	copy(old[oldOff:], sec)
	out := make([]byte, 0x4000)
	var eqs []equivalence
	for _, s := range shifts {
		eqs = append(eqs, equivalence{Src: oldOff + s.src, Dst: newOff + s.src + s.shift, N: s.n})
		copy(out[newOff+s.src+s.shift:], old[oldOff+s.src:oldOff+s.src+s.n])
	}
	newSize := uint64(len(sec)) + 12

	ep := equivalencePlan{OldLen: uint64(len(old)), NewLen: uint64(len(out)), Eqs: eqs}
	p := ehFramePlan{OldOff: oldOff, OldSize: uint64(len(sec)), NewOff: newOff, NewSize: newSize,
		OldAddr: addr, NewAddr: addr}
	oldSecs := sectionMap{{Old: addr, New: oldOff, Size: uint64(len(sec))}}
	identity := func(a uint64) x86.Target { return x86.Target{Addr: a, Known: true} }
	noExtent := func(uint64) (uint64, uint64, bool) { return 0, 0, false }

	st := applyEhFrame(out, old, ep, p, oldSecs, identity, noExtent)
	if st.FDEs != 3 || st.Retargeted != 3 || st.CiePtrs != 3 {
		t.Fatalf("stats %+v, want 3 FDEs all retargeted with their cie pointers fixed", st)
	}
	// Each FDE's cie_ptr must be the new distance back to its CIE, and each
	// must differ from the distance the copy carried over.
	for _, want := range []struct{ entry, cie uint64 }{
		{off1 + 4, 0}, {off3 + 12, off2 + 4}, {off4 + 12, off2 + 4},
	} {
		got := binary.LittleEndian.Uint32(out[newOff+want.entry+4:])
		if uint64(got) != want.entry+4-want.cie {
			t.Errorf("FDE at %#x: cie_ptr = %d, want %d", want.entry, got, want.entry+4-want.cie)
		}
		if uint64(got) == uint64(binary.LittleEndian.Uint32(sec[want.entry+4:])) {
			t.Errorf("FDE at %#x: cie_ptr unchanged at %d, the test cannot see the fix", want.entry, got)
		}
	}
}
