package delta

import (
	"bytes"
	"math/rand"
	"testing"
)

func checkCorrection(t *testing.T, pred, want []byte) int {
	t.Helper()
	s, err := encodeCorrection(pred, want, false)
	if err != nil {
		t.Fatal(err)
	}
	buf := append([]byte(nil), pred...)
	if err := applyCorrection(buf, s, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, want) {
		t.Fatalf("correction did not reproduce the target (%d bytes)", len(want))
	}
	return len(s)
}

func TestCorrection(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	base := make([]byte, 200000)
	r.Read(base)

	t.Run("identical", func(t *testing.T) {
		if n := checkCorrection(t, base, base); n > 8 {
			t.Fatalf("an exact prediction cost %d bytes", n)
		}
	})
	t.Run("scattered bytes", func(t *testing.T) {
		want := append([]byte(nil), base...)
		for k := 0; k < 100; k++ {
			want[r.Intn(len(want))] ^= 0xff
		}
		checkCorrection(t, base, want)
	})
	t.Run("shifted function tail", func(t *testing.T) {
		// what a function that grew by five bytes looks like: identical
		// head, five new bytes, then the old bytes shifted along
		want := append([]byte(nil), base...)
		copy(want[50005:], base[50000:len(base)-5])
		copy(want[50000:50005], []byte("HELLO"))
		n := checkCorrection(t, base, want)
		if n > 300 {
			t.Fatalf("a shifted tail cost %d bytes; the local match did not fire", n)
		}
	})
	t.Run("whole file differs", func(t *testing.T) {
		want := make([]byte, len(base))
		r.Read(want)
		checkCorrection(t, base, want)
	})
	t.Run("empty", func(t *testing.T) { checkCorrection(t, nil, nil) })
	t.Run("last byte", func(t *testing.T) {
		want := append([]byte(nil), base...)
		want[len(want)-1] ^= 1
		checkCorrection(t, base, want)
	})
}

func TestCorrectionRejectsCorruptStreams(t *testing.T) {
	pred := bytes.Repeat([]byte("abcdefgh"), 500)
	want := append([]byte(nil), pred...)
	copy(want[100:], "ZZZZ")
	s, err := encodeCorrection(pred, want, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := range s {
		bad := append([]byte(nil), s...)
		bad[i] ^= 0xff
		buf := append([]byte(nil), pred...)
		// it may legitimately apply (a flipped literal byte), but it must
		// never panic or write outside buf
		_ = applyCorrection(buf, bad, false)
		if len(buf) != len(pred) {
			t.Fatalf("byte %d: buffer resized", i)
		}
	}
	for _, n := range []int{0, 1, len(s) / 2, len(s) - 1} {
		buf := append([]byte(nil), pred...)
		if err := applyCorrection(buf, s[:n], false); err == nil {
			t.Fatalf("truncation to %d bytes accepted", n)
		}
	}
}

// nearMissPair is what a patch release's residual looks like: fields the
// prediction placed a bit off, close enough together that no local match
// spans them, so want^pred is mostly zeros. The tail is shifted as well, so
// the stream also carries a match region.
func nearMissPair(n int) (pred, want []byte) {
	r := rand.New(rand.NewSource(21))
	pred = make([]byte, n)
	r.Read(pred)
	want = append([]byte(nil), pred...)
	for i := 0; i < n*3/4; i += 5 {
		want[i] ^= 0x40
	}
	if n > 2000 {
		copy(want[n-1000:], pred[n-1005:n-5])
	}
	return pred, want
}

// newContentPair is what a minor release's residual looks like: stretches of
// genuinely new content, far apart, whose value to the literal stream is
// that they match each other -- which xor against an unrelated prediction
// would destroy.
func newContentPair(n int) (pred, want []byte) {
	r := rand.New(rand.NewSource(22))
	pred = make([]byte, n)
	r.Read(pred)
	fresh := make([]byte, 200)
	r.Read(fresh)
	want = append([]byte(nil), pred...)
	for i := 0; i+len(fresh) < n; i += 2000 {
		copy(want[i:], fresh)
	}
	return pred, want
}

// TestCorrectionShapes round-trips both shapes of the transform-2 stream and
// checks that the encoder picks the one that is actually smaller. Which one
// that is is a property of the release, not of the codec (docs/go-module-design.md
// 3.4): a near-miss residual wants the merged, xor-ed shape and a residual
// of new content wants the shipped one.
func TestCorrectionShapes(t *testing.T) {
	for _, c := range []struct {
		name string
		pair func(int) ([]byte, []byte)
		pick corrShape
	}{
		{"near miss", nearMissPair, nearmiss},
		{"new content", newContentPair, shipped},
	} {
		t.Run(c.name, func(t *testing.T) {
			pred, want := c.pair(1 << 16)
			for _, sh := range []corrShape{shipped, nearmiss} {
				buf := append([]byte(nil), pred...)
				if err := applyCorrection(buf, sh.write(pred, want, true), true); err != nil {
					t.Fatalf("shape %v: %v", sh, err)
				}
				if !bytes.Equal(buf, want) {
					t.Fatalf("shape %v did not reproduce the target", sh)
				}
			}
			s, err := encodeCorrection(pred, want, true)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(s, c.pick.write(pred, want, true)) {
				t.Errorf("the encoder did not choose the %v shape", c.pick)
			}
			buf := append([]byte(nil), pred...)
			if err := applyCorrection(buf, s, true); err != nil || !bytes.Equal(buf, want) {
				t.Fatalf("the chosen stream did not apply: %v", err)
			}
		})
	}
}

// TestCorrectionRejectsWrongShape flips the shape flag. The two shapes carry
// different numbers of streams, so a decoder told the wrong one must refuse
// the stream rather than write the wrong bytes.
func TestCorrectionRejectsWrongShape(t *testing.T) {
	pred, want := nearMissPair(4096)
	for _, sh := range []corrShape{shipped, nearmiss} {
		s := sh.write(pred, want, true)
		// the flag is the low bit of the region count, the second field
		r := &rbuf{b: s}
		r.u()
		bad := append([]byte(nil), s...)
		bad[len(s)-len(r.b)] ^= 1
		buf := append([]byte(nil), pred...)
		if err := applyCorrection(buf, bad, true); err == nil && bytes.Equal(buf, want) {
			t.Errorf("shape %v: the flipped flag decoded as if nothing happened", sh)
		}
		if len(buf) != len(pred) {
			t.Fatalf("shape %v: buffer resized", sh)
		}
		// and the same stream read as a transform-1 one, whose region count
		// carries no flag
		buf = append([]byte(nil), pred...)
		_ = applyCorrection(buf, s, false)
		if len(buf) != len(pred) {
			t.Fatalf("shape %v: buffer resized", sh)
		}
	}
}

// TestCorrectionRejectsCorruptShapedStreams is the corrupt-stream sweep over
// the flagged streams, and over the columnar shape in particular: it has two
// more length fields and reads its regions from three cursors.
func TestCorrectionRejectsCorruptShapedStreams(t *testing.T) {
	pred, want := nearMissPair(600)
	for _, sh := range []corrShape{shipped, nearmiss} {
		s := sh.write(pred, want, true)
		for i := range s {
			bad := append([]byte(nil), s...)
			bad[i] ^= 0xff
			buf := append([]byte(nil), pred...)
			_ = applyCorrection(buf, bad, true)
			if len(buf) != len(pred) {
				t.Fatalf("shape %v byte %d: buffer resized", sh, i)
			}
		}
		for _, n := range []int{0, 1, len(s) / 2, len(s) - 1} {
			buf := append([]byte(nil), pred...)
			if err := applyCorrection(buf, s[:n], true); err == nil {
				t.Fatalf("shape %v: truncation to %d bytes accepted", sh, n)
			}
		}
	}
}
