package delta

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func putRel(b []byte, v int64) { binary.LittleEndian.PutUint32(b, uint32(int32(v))) }

// oneBody lays a 48-byte function at PC 0x1000 holding a call to a second
// function start and a jump back inside itself, then a prediction that gets
// a long run across both fields wrong.
func oneBody() (pred, want []byte, d *DispContext) {
	const base = 0x1000
	want = bytes.Repeat([]byte{0x90}, 48)
	// call rel32 at 8: next = 0x100d, target 0x2000.
	want[8] = 0xe8
	putRel(want[9:], 0x2000-(base+13))
	// jmp rel32 at 16: next = 0x1015, target 0x1004 (inside this body, not a start).
	want[16] = 0xe9
	putRel(want[17:], (base+4)-(base+21))
	pred = append([]byte(nil), want...)
	// One wrong run of 5+ bytes covering both fields; 0x01 collides with no
	// byte either field holds, so the run does not split.
	for i := 6; i < 24; i++ {
		pred[i] = 0x01
	}
	d = NewDispContext([]DispBody{{Off: 0, Size: 48, PC: base}}, []uint64{base, 0x2000})
	return pred, want, d
}

func TestDispColumnRoundTrip(t *testing.T) {
	t.Parallel()
	pred, want, d := oneBody()
	cols, err := EncodeColumnarDisp(pred, want, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != ColumnarStreams+DispStreams {
		t.Fatalf("%d streams, want %d", len(cols), ColumnarStreams+DispStreams)
	}
	tags := cols[ColumnarStreams]
	if len(tags) != 2 || tags[0] != dispHit || tags[1] != dispLocal {
		t.Fatalf("tags %v, want [%d %d]", tags, dispHit, dispLocal)
	}
	// The fields are out of the byte column: every displacement byte is zero.
	last := cols[2+ColumnarBuckets-1]
	for _, k := range []int{9 - 6, 10 - 6, 11 - 6, 12 - 6, 17 - 6, 18 - 6, 19 - 6, 20 - 6} {
		if last[k] != 0 {
			t.Fatalf("byte column keeps displacement byte %d = %#x", k, last[k])
		}
	}
	buf := append([]byte(nil), pred...)
	if err := ApplyColumnarDisp(buf, cols, d); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, want) {
		t.Fatalf("apply did not reproduce the target:\n got %x\nwant %x", buf, want)
	}
}

func TestDispColumnFarField(t *testing.T) {
	t.Parallel()
	pred, want, d := oneBody()
	// A domain with neither target in it: both fields escape.
	d = NewDispContext([]DispBody{{Off: 0, Size: 48, PC: 0x1000}}, []uint64{0x9000})
	cols, err := EncodeColumnarDisp(pred, want, d)
	if err != nil {
		t.Fatal(err)
	}
	tags := cols[ColumnarStreams]
	if len(tags) != 2 || tags[0] != dispFar || tags[1] != dispLocal {
		t.Fatalf("tags %v", tags)
	}
	buf := append([]byte(nil), pred...)
	if err := ApplyColumnarDisp(buf, cols, d); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, want) {
		t.Fatal("apply did not reproduce the target")
	}
}

// With no context the columns are absent and the shipped format's price is
// exactly what it was.
func TestDispColumnAbsentWithoutContext(t *testing.T) {
	t.Parallel()
	pred, want, _ := oneBody()
	cols, err := EncodeColumnarDisp(pred, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := EncodeColumnar(pred, want)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != ColumnarStreams {
		t.Fatalf("%d streams without a context", len(cols))
	}
	for i := range cols {
		if !bytes.Equal(cols[i], plain[i]) {
			t.Fatalf("stream %d differs from EncodeColumnar", i)
		}
	}
}

// Restrict drops a body that straddles a cut, so both sides walk the same set.
func TestDispContextRestrict(t *testing.T) {
	t.Parallel()
	d := NewDispContext([]DispBody{{Off: 0, Size: 48, PC: 0x1000}, {Off: 64, Size: 16, PC: 0x2000}}, []uint64{0x1000, 0x2000})
	count := func(d *DispContext) int {
		n := 0
		for _, b := range d.bodies {
			if _, ok := d.localBody(b); ok {
				n++
			}
		}
		return n
	}
	if got := count(d.Restrict(0, 48)); got != 1 {
		t.Fatalf("%d bodies in [0,48)", got)
	}
	if got := count(d.Restrict(0, 40)); got != 0 {
		t.Fatalf("%d bodies in [0,40), want the straddling body dropped", got)
	}
	r := d.Restrict(64, 80)
	b, ok := r.localBody(r.bodies[0])
	if !ok || count(r) != 1 || b.Off != 0 {
		t.Fatalf("restricted body %+v, ok %v", b, ok)
	}
}

func TestDispColumnRejectsMissingContext(t *testing.T) {
	t.Parallel()
	pred, want, d := oneBody()
	cols, err := EncodeColumnarDisp(pred, want, d)
	if err != nil {
		t.Fatal(err)
	}
	buf := append([]byte(nil), pred...)
	if err := ApplyColumnarDisp(buf, cols, nil); err == nil {
		t.Fatal("a displacement correction was applied with no field context")
	}
}
