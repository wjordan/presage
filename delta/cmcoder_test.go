package delta

import (
	"bytes"
	"math/rand"
	"testing"
)

// cmSample builds a stream with the shape the coder is built for: bytes that
// are mostly a small edit of a prediction byte, in four-byte columns.
func cmSample(n int) (src []byte, side *CMSide) {
	r := rand.New(rand.NewSource(7))
	side = &CMSide{}
	for i := 0; i < n; i++ {
		p := byte(r.Intn(256))
		v := p
		switch i % 4 {
		case 0:
			v = p + byte(r.Intn(8))
		case 3:
			v = p // high byte of a displacement: unchanged
		default:
			if r.Intn(4) == 0 {
				v = byte(r.Intn(256))
			}
		}
		src = append(src, v)
		side.Pred = append(side.Pred, p)
		side.Sel = append(side.Sel, byte(i%4))
		side.Cls = append(side.Cls, byte(i%3))
		side.Off = append(side.Off, byte(i%16))
	}
	return src, side
}

func TestCMRoundTrip(t *testing.T) {
	src, full := cmSample(20000)
	sides := map[string]*CMSide{
		"none":  nil,
		"empty": {},
		"pred":  {Pred: full.Pred, Sel: full.Sel},
		"full":  full,
	}
	for name, side := range sides {
		coded, err := CMEncode(src, side)
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		got, err := CMDecode(coded, len(src), side)
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if !bytes.Equal(got, src) {
			t.Fatalf("%s: round trip differs", name)
		}
		t.Logf("%s: %d -> %d bytes", name, len(src), len(coded))
	}
}

// The prediction context is the reason this coder exists: with it the same
// bytes must cost measurably less than without.
func TestCMPredictionContextPays(t *testing.T) {
	src, full := cmSample(20000)
	plain, err := CMEncode(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	withPred, err := CMEncode(src, full)
	if err != nil {
		t.Fatal(err)
	}
	if len(withPred) >= len(plain) {
		t.Fatalf("prediction context did not pay: %d with, %d without", len(withPred), len(plain))
	}
	t.Logf("plain %d, conditioned %d (%.1f%%)", len(plain), len(withPred),
		100*float64(len(withPred))/float64(len(plain)))
}

func TestCMEdges(t *testing.T) {
	for _, n := range []int{0, 1, 2, 7, 300} {
		src := make([]byte, n)
		for i := range src {
			src[i] = byte(i * 7)
		}
		side := &CMSide{Pred: make([]byte, n), Sel: make([]byte, n)}
		coded, err := CMEncode(src, side)
		if err != nil {
			t.Fatal(err)
		}
		got, err := CMDecode(coded, n, side)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, src) {
			t.Fatalf("n=%d: round trip differs", n)
		}
	}
}

func TestCMSideLengthChecked(t *testing.T) {
	if _, err := CMEncode(make([]byte, 10), &CMSide{Pred: make([]byte, 9)}); err == nil {
		t.Fatal("short side info accepted")
	}
	if _, err := CMDecode(nil, 10, &CMSide{Cls: make([]byte, 10)}); err == nil {
		t.Fatal("class without offset accepted")
	}
}

func TestCMBankZeroEncoding(t *testing.T) {
	b := newBank(12)
	b.idx = 17
	if b.probs[b.idx] != 0 || b.prob() != 1<<15 {
		t.Fatalf("new bank slot stores %#x, reads %#x", b.probs[b.idx], b.prob())
	}
	b.update(1)
	want := uint16(int32(1<<15) + ((int32(65535)-int32(1<<15))*adaptRate[0])>>16)
	if b.prob() != want || b.cnt[b.idx] != 1 {
		t.Fatalf("updated bank reads %#x/%d, want %#x/1", b.prob(), b.cnt[b.idx], want)
	}
}

func BenchmarkCMDecode(b *testing.B) {
	src, side := cmSample(1 << 20)
	coded, err := CMEncode(src, side)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		if _, err := CMDecode(coded, len(src), side); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCMEncode(b *testing.B) {
	src, side := cmSample(1 << 20)
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		if _, err := CMEncode(src, side); err != nil {
			b.Fatal(err)
		}
	}
}
