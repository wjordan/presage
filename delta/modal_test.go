package delta

import (
	"bytes"
	"math/rand"
	"os"
	"testing"
)

// roundTripModal encodes with the adaptive picker and applies the result,
// which is the path presage uses.
func roundTripModal(t *testing.T, pred, want []byte) []byte {
	t.Helper()
	s, err := EncodeCorrectionAdaptive(pred, want)
	if err != nil {
		t.Fatal(err)
	}
	got := append([]byte(nil), pred...)
	if err := ApplyFlaggedCorrection(got, s); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !bytes.Equal(got, want) {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("replay differs at %d: got %#x want %#x", i, got[i], want[i])
			}
		}
		t.Fatal("replay differs in length")
	}
	return s
}

// TestModalRoundTrip covers the shapes the selector actually picks: a
// relocation table whose entries moved by small amounts (multiprecision), a
// near-miss code region (xor/sub), and fresh content (literals).
func TestModalRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, tc := range []struct {
		name string
		make func() (pred, want []byte)
	}{
		{"pointer table, every entry +0x40", func() ([]byte, []byte) {
			p := make([]byte, 8*4096)
			rng.Read(p)
			w := append([]byte(nil), p...)
			for i := 0; i < len(w); i += 8 {
				v := uint64(0)
				for j := 7; j >= 0; j-- {
					v = v<<8 | uint64(w[i+j])
				}
				v += 0x40
				for j := range 8 {
					w[i+j] = byte(v >> (8 * j))
				}
			}
			return p, w
		}},
		{"rel32 sites, sparse and small", func() ([]byte, []byte) {
			p := make([]byte, 1<<16)
			rng.Read(p)
			w := append([]byte(nil), p...)
			for i := 0; i < len(w)-4; i += 137 {
				v := int32(0)
				for j := 3; j >= 0; j-- {
					v = v<<8 | int32(w[i+j])
				}
				v -= 3
				for j := range 4 {
					w[i+j] = byte(uint32(v) >> (8 * j))
				}
			}
			return p, w
		}},
		{"fresh content", func() ([]byte, []byte) {
			p := make([]byte, 1<<16)
			w := make([]byte, 1<<16)
			rng.Read(p)
			rng.Read(w)
			return p, w
		}},
		{"identical", func() ([]byte, []byte) {
			p := make([]byte, 4096)
			rng.Read(p)
			return p, append([]byte(nil), p...)
		}},
		{"one byte", func() ([]byte, []byte) {
			return []byte{1}, []byte{2}
		}},
		{"empty", func() ([]byte, []byte) { return nil, nil }},
		{"tail shorter than a word", func() ([]byte, []byte) {
			p := make([]byte, 8*300+3)
			rng.Read(p)
			w := append([]byte(nil), p...)
			for i := range w {
				w[i]++
			}
			return p, w
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pred, want := tc.make()
			roundTripModal(t, pred, want)
		})
	}
}

// TestModalWins checks the shape is not merely correct but chosen: a table of
// pointers that all moved by the same small amount is the case it exists for.
func TestModalWins(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	p := make([]byte, 8*8192)
	rng.Read(p)
	w := append([]byte(nil), p...)
	for i := 0; i < len(w); i += 8 {
		v := uint64(0)
		for j := 7; j >= 0; j-- {
			v = v<<8 | uint64(w[i+j])
		}
		v -= 0x1000
		for j := range 8 {
			w[i+j] = byte(v >> (8 * j))
		}
	}
	s := roundTripModal(t, p, w)
	if !UsesModalCorrection(s) {
		t.Fatalf("adaptive picker did not choose the modal shape")
	}
	plain, near, err := CorrectionShapes(p, w)
	if err != nil {
		t.Fatal(err)
	}
	got, was := czLen(s), min(czLen(plain), czLen(near))
	if got >= was {
		t.Fatalf("modal %d is not smaller than the shipped shapes %d", got, was)
	}
	t.Logf("modal %d vs shipped %d (%.1f%%)", got, was, 100*float64(got-was)/float64(was))
}

// TestModalSurvivesCorrupt checks the decoder fails *safely* on a damaged
// stream. It is not self-checking and is not meant to be -- integrity is the
// container's, via a hash per frame and one over the target -- and the shipped
// shapes silently produce wrong bytes for a third of single-byte flips, all of
// them in the literal stream. What the decoder must never do is panic, read or
// write out of bounds, or fail to terminate.
func TestModalSurvivesCorrupt(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	p := make([]byte, 4096)
	rng.Read(p)
	w := append([]byte(nil), p...)
	for i := 0; i < len(w); i += 8 {
		w[i] ^= 0x11
	}
	s, err := EncodeCorrectionAdaptive(p, w)
	if err != nil {
		t.Fatal(err)
	}
	if !UsesModalCorrection(s) {
		t.Skip("picker chose another shape for this input")
	}
	try := func(bad []byte) {
		buf := append([]byte(nil), p...)
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on corrupt stream: %v", r)
			}
		}()
		_ = ApplyFlaggedCorrection(buf, bad)
		if len(buf) != len(p) {
			t.Fatalf("decoder resized the buffer to %d", len(buf))
		}
	}
	for i := range s {
		for _, mask := range []byte{0xff, 0x01, 0x80} {
			bad := append([]byte(nil), s...)
			bad[i] ^= mask
			try(bad)
		}
	}
	for n := 0; n < 2000; n++ {
		bad := append([]byte(nil), s...)
		for k := 0; k < 1+rng.Intn(8); k++ {
			bad[rng.Intn(len(bad))] = byte(rng.Intn(256))
		}
		if n%3 == 0 && len(bad) > 4 {
			bad = bad[:rng.Intn(len(bad))]
		}
		try(bad)
	}
}

// FuzzModalApply is the same safety property, reachable by the fuzzer.
func FuzzModalApply(f *testing.F) {
	rng := rand.New(rand.NewSource(4))
	p := make([]byte, 512)
	rng.Read(p)
	w := append([]byte(nil), p...)
	for i := 0; i < len(w); i += 8 {
		w[i] ^= 0x11
	}
	if s, err := EncodeCorrectionAdaptive(p, w); err == nil {
		f.Add(s)
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, stream []byte) {
		buf := append([]byte(nil), p...)
		_ = ApplyFlaggedCorrection(buf, stream)
		if len(buf) != len(p) {
			t.Fatalf("decoder resized the buffer to %d", len(buf))
		}
	})
}

// TestModalOnCorpus prices the shipped shapes against the modal one on a real
// prediction, so the port can be checked against what the spike measured.
//
//	BS6_PRED=... BS6_TARGET=... go test ./delta -run ModalOnCorpus -v
func TestModalOnCorpus(t *testing.T) {
	pp, tp := os.Getenv("BS6_PRED"), os.Getenv("BS6_TARGET")
	if pp == "" || tp == "" {
		t.Skip("set BS6_PRED and BS6_TARGET")
	}
	pred, err := os.ReadFile(pp)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(tp)
	if err != nil {
		t.Fatal(err)
	}
	if len(pred) != len(want) {
		t.Fatalf("prediction %d, target %d", len(pred), len(want))
	}
	plain, near, err := CorrectionShapes(pred, want)
	if err != nil {
		t.Fatal(err)
	}
	np, nn := czLen(plain), czLen(near)
	s, err := EncodeCorrectionAdaptive(pred, want)
	if err != nil {
		t.Fatal(err)
	}
	got := append([]byte(nil), pred...)
	if err := ApplyFlaggedCorrection(got, s); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("replay differs at %d", i)
		}
	}
	if dir := os.Getenv("BS6_DUMPDIR"); dir != "" {
		rs := modalRegions(pred, want)
		mm := &modeModel{}
		var o *modalOut
		for pass := 0; pass < modalPasses; pass++ {
			o = modalWrite(pred, want, rs, mm)
			mm = &modeModel{}
			mm.train(&o.hist)
		}
		names := []string{"gaps", "spans", "ops", "modes", "lit", "lz", "xor", "sub", "mp4", "mp8"}
		for i, b := range modalWrite(pred, want, rs, mm).streams() {
			if len(b) == 0 {
				continue
			}
			if err := os.WriteFile(dir+"/"+names[i], b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	was, now := min(np, nn), czLen(s)
	t.Logf("raw: plain %d near %d modal %d", len(plain), len(near), len(s))
	t.Logf("shipped plain %d, near-miss %d; adaptive now %d (modal=%v) %.2f%%",
		np, nn, now, UsesModalCorrection(s), 100*float64(now-was)/float64(was))
}
