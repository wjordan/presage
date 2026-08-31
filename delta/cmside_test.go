package delta

import (
	"bytes"
	"testing"
)

// cmFieldFixture builds a prediction of repeated "ten nops then a call rel32"
// and a target that changes the last nop and the call's displacement. That
// leaves two runs per 15-byte unit: a 1-byte one over a whole one-byte
// instruction, and a 4-byte one over nothing but displacement bytes. reps is
// chosen so the 4-run bucket clears CMMinStream and the walk is not skipped.
func cmFieldFixture(reps int) (pred, want []byte, d *DispContext) {
	unit := append(bytes.Repeat([]byte{0x90}, 10), 0xE8, 0x11, 0x22, 0x33, 0x44)
	pred = bytes.Repeat(unit, reps)
	want = append([]byte(nil), pred...)
	for i := 0; i+len(unit) <= len(want); i += len(unit) {
		want[i+9] = 0x91                                  // xchg ecx, eax: still one byte
		copy(want[i+11:], []byte{0x55, 0x66, 0x77, 0x00}) // a different call target
	}
	return pred, want, NewDispContext([]DispBody{{Off: 0, Size: len(pred), PC: 0x400000}}, nil)
}

func TestCMColumnarSidesFieldContext(t *testing.T) {
	pred, want, d := cmFieldFixture(1200)
	cols, err := EncodeColumnarDisp(pred, want, nil) // nil: keep the raw bytes, no disp columns
	if err != nil {
		t.Fatal(err)
	}
	sides, err := CMColumnarSides(pred, cols[0], cols[1], d)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		run  int
		cls  []byte
		offs []byte
	}{
		// The changed nop: a whole instruction, no PC-relative field.
		{1, []byte{fieldPlain}, []byte{0}},
		// The changed displacement: bytes 1..4 of the call that holds it.
		{4, []byte{fieldByte0, fieldByte1, fieldByte2, fieldByte3}, []byte{1, 2, 3, 4}},
	} {
		s := sides[bucketOf(c.run)]
		if s == nil {
			t.Fatalf("no bucket for runs of %d", c.run)
		}
		if s.Cls == nil || len(s.Cls) != len(s.Pred) || len(s.Off) != len(s.Pred) {
			t.Fatalf("run %d: field context is %d/%d bytes for %d", c.run, len(s.Cls), len(s.Off), len(s.Pred))
		}
		for i := 0; i+c.run <= len(s.Cls); i += c.run {
			if !bytes.Equal(s.Cls[i:i+c.run], c.cls) {
				t.Fatalf("run %d at %d classed %v, want %v", c.run, i, s.Cls[i:i+c.run], c.cls)
			}
			if !bytes.Equal(s.Off[i:i+c.run], c.offs) {
				t.Fatalf("run %d at %d offset %v, want %v", c.run, i, s.Off[i:i+c.run], c.offs)
			}
		}
	}
	if n := len(sides[bucketOf(4)].Pred); n < CMMinStream {
		t.Fatalf("fixture bucket is %d bytes, under the %d walk gate", n, CMMinStream)
	}
}

// The decoder derives the side information from the prediction it still holds,
// with the same restricted context; nothing else may enter the derivation.
func TestCMColumnarSidesDeriveFromPredictionOnly(t *testing.T) {
	pred, want, d := cmFieldFixture(1200)
	cols, err := EncodeColumnarDisp(pred, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := CMColumnarSides(pred, cols[0], cols[1], d)
	if err != nil {
		t.Fatal(err)
	}
	// Whatever the target held, the decoder starts from the prediction.
	buf := append([]byte(nil), pred...)
	dec, err := CMColumnarSides(buf, cols[0], cols[1], d)
	if err != nil {
		t.Fatal(err)
	}
	for b := range enc {
		if (enc[b] == nil) != (dec[b] == nil) {
			t.Fatalf("bucket %d present on one side only", b)
		}
		if enc[b] == nil {
			continue
		}
		for _, p := range []struct {
			name string
			a, b []byte
		}{{"pred", enc[b].Pred, dec[b].Pred}, {"sel", enc[b].Sel, dec[b].Sel},
			{"cls", enc[b].Cls, dec[b].Cls}, {"off", enc[b].Off, dec[b].Off}} {
			if !bytes.Equal(p.a, p.b) {
				t.Fatalf("bucket %d %s disagrees", b, p.name)
			}
		}
	}
}

// A piece with no field context, and one too small to be offered the coder,
// both leave Cls and Off unset -- which check() requires to be all-or-nothing.
func TestCMColumnarSidesNoFieldContext(t *testing.T) {
	pred, want, d := cmFieldFixture(1200)
	cols, err := EncodeColumnarDisp(pred, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	sides, err := CMColumnarSides(pred, cols[0], cols[1], nil)
	if err != nil {
		t.Fatal(err)
	}
	for b, s := range sides {
		if s != nil && (s.Cls != nil || s.Off != nil) {
			t.Fatalf("bucket %d got a field context with no DispContext", b)
		}
		if err := s.check(len(s.pred())); err != nil {
			t.Fatal(err)
		}
	}

	smallPred, smallWant, _ := cmFieldFixture(4)
	cols, err = EncodeColumnarDisp(smallPred, smallWant, nil)
	if err != nil {
		t.Fatal(err)
	}
	sides, err = CMColumnarSides(smallPred, cols[0], cols[1], d)
	if err != nil {
		t.Fatal(err)
	}
	for b, s := range sides {
		if s != nil && s.Cls != nil {
			t.Fatalf("bucket %d walked for a %d-byte stream", b, len(s.Pred))
		}
	}
}

func (s *CMSide) pred() []byte {
	if s == nil {
		return nil
	}
	return s.Pred
}
