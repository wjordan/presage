package main

import (
	"encoding/binary"
	"testing"
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
	// Deliberately supplied out of address order, as the link order they are
	// walked in would give them.
	placed := []ehNewFDE{}
	for _, f := range fdes {
		stored := int32(binary.LittleEndian.Uint32(sec[f.locOff:]))
		placed = append(placed, ehNewFDE{
			target:    uint64(int64(secAddr+f.locOff) + int64(stored)),
			entryAddr: secAddr + f.entry,
		})
	}
	if n := regenerateEhFrameHdr(image, p, placed); n != 3 {
		t.Fatalf("regenerated %d header entries, want 3", n)
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
