package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// putRel writes the displacement that makes the field at buf[off:off+n] point
// at want, given that the instruction's next byte lives at address next.
func putRel(buf []byte, off, n int, next, want uint64) {
	v := int64(want) - int64(next)
	for k := 0; k < n; k++ {
		buf[off+k] = byte(v)
		v >>= 8
	}
}

// dispTestBody builds a 48-byte function at 0x1000 containing one call to a
// known function start, one short local jump, two VEX RIP-relative loads (the
// second with a trailing imm8, where the target is pc+Len+disp and not
// disp-end), one long jump to another function start, and nop padding.
func dispTestBody() []byte {
	b := make([]byte, 48)
	const base = 0x1000
	// 0: call rel32 -> 0x2000, a function start.
	b[0] = 0xE8
	putRel(b, 1, 4, base+5, 0x2000)
	// 5: jmp rel8 -> 0x1020, inside this function.
	b[5] = 0xEB
	putRel(b, 6, 1, base+7, 0x1020)
	// 7: VEX2 vmovss xmm0, [rip+disp32] -> 0x9000, outside any function.
	copy(b[7:], []byte{0xC5, 0xFA, 0x10, 0x05})
	putRel(b, 11, 4, base+15, 0x9000)
	// 15: VEX3 vinsertps xmm0, xmm0, [rip+disp32], imm8 -> 0x9100. The imm8
	// sits after the displacement, so the field is not at the instruction's
	// end and the target is still pc+Len+disp.
	copy(b[15:], []byte{0xC4, 0xE3, 0x79, 0x21, 0x05})
	putRel(b, 20, 4, base+25, 0x9100)
	b[24] = 0x40
	// 25: jmp rel32 -> 0x3000, a function start.
	b[25] = 0xE9
	putRel(b, 26, 4, base+30, 0x3000)
	for i := 30; i < 48; i++ {
		b[i] = 0x90
	}
	return b
}

func dispTestContext(size int) *dispContext {
	return &dispContext{
		bodies: []dispBody{{0, size, 0x1000}},
		starts: []uint64{0x1000, 0x2000, 0x3000},
	}
}

// TestDispColumnRoundTrip is the whole claim in one test: the encoder zeroes
// every PC-relative field inside the long run, the decoder walks bytes whose
// displacements are gone and puts the values back, and the result is the
// target byte for byte. The prediction is a different instruction stream --
// forty one-byte pushes against six instructions -- so a decoder that walked
// the prediction rather than the repaired bytes would find nothing right.
func TestDispColumnRoundTrip(t *testing.T) {
	target := dispTestBody()
	pred := make([]byte, len(target))
	for i := range pred {
		if i < 30 {
			pred[i] = 0x50 // push rax; one byte, so every boundary differs
		} else {
			pred[i] = target[i]
		}
	}
	d := dispTestContext(len(target))

	c, err := encodeColumnarDisp(pred, target, d)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(c.Tags) != 5 {
		t.Fatalf("pulled out %d fields, want 5 (tags %v)", len(c.Tags), c.Tags)
	}
	want := []byte{dispHit, dispLocal, dispFar, dispFar, dispHit}
	if !bytes.Equal(c.Tags, want) {
		t.Errorf("classes %v, want %v", c.Tags, want)
	}
	// The zeroing must have reached the shipped bytes, not just a copy: the
	// call's displacement is the four bytes after the run's first.
	last := c.Bytes[correctionBuckets-1]
	if len(last) != 30 {
		t.Fatalf("long-run bucket holds %d bytes, want 30", len(last))
	}
	if binary.LittleEndian.Uint32(last[1:]) != 0 || binary.LittleEndian.Uint32(last[11:]) != 0 {
		t.Errorf("displacement bytes survived into the shipped bucket: %x", last)
	}
	if last[24] != 0x40 {
		t.Errorf("the imm8 after a VEX displacement was zeroed too: %x", last[15:25])
	}

	got := append([]byte(nil), pred...)
	if err := c.applyDisp(got, d); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("apply produced\n%x\nwant\n%x", got, target)
	}
}

// TestDispColumnEscape pins the escape path on its own: with an empty
// function-start domain every field that leaves its function must fall through
// to the absolute-address column and still replay.
func TestDispColumnEscape(t *testing.T) {
	target := dispTestBody()
	pred := make([]byte, len(target))
	for i := range pred {
		if i < 30 {
			pred[i] = 0x50
		} else {
			pred[i] = target[i]
		}
	}
	d := dispTestContext(len(target))
	d.starts = nil

	c, err := encodeColumnarDisp(pred, target, d)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(c.Idx) != 0 {
		t.Errorf("index column is %d bytes with an empty domain", len(c.Idx))
	}
	fars := 0
	for _, tag := range c.Tags {
		if int(tag) == dispFar {
			fars++
		}
	}
	if fars != 4 {
		t.Errorf("%d fields escaped, want 4 (tags %v)", fars, c.Tags)
	}
	got := append([]byte(nil), pred...)
	if err := c.applyDisp(got, d); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("escape path did not replay")
	}
}

// TestDispColumnShortRuns checks the two boundaries the format turns on: a
// field is pulled out only if it lies wholly inside a run of correctionBuckets
// bytes or more, and a correction with no context is byte-identical to the
// format that shipped.
func TestDispColumnShortRuns(t *testing.T) {
	target := dispTestBody()
	// Only the call's low two displacement bytes are wrong, which is a
	// two-byte run: too short for the last bucket, so nothing is pulled out.
	pred := append([]byte(nil), target...)
	pred[1] ^= 0xFF
	pred[2] ^= 0xFF
	d := dispTestContext(len(target))

	c, err := encodeColumnarDisp(pred, target, d)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(c.Tags) != 0 {
		t.Errorf("a %d-byte run yielded %d fields", 2, len(c.Tags))
	}
	plain, err := encodeColumnar(pred, target)
	if err != nil {
		t.Fatalf("encode plain: %v", err)
	}
	if !bytes.Equal(plain.Gaps, c.Gaps) || !bytes.Equal(plain.Lens, c.Lens) {
		t.Errorf("the displacement variant moved the gap or length column")
	}
	if len(plain.streams()) != correctionBuckets+2 {
		t.Errorf("the shipped format grew to %d streams", len(plain.streams()))
	}
	got := append([]byte(nil), pred...)
	if err := c.applyDisp(got, d); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("short-run case did not replay")
	}
}
