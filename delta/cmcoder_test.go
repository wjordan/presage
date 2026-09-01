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

func TestCMCompactRoundTrip(t *testing.T) {
	src, side := cmSample(20000)
	coded, err := CMEncodeCompact(src, side)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CMDecodeCompact(coded, len(src), side)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, src) {
		t.Fatal("round trip differs")
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

func TestCMBankZeroState(t *testing.T) {
	b := newBank(12)
	b.idx = 17
	if b.st[b.idx] != 0 || hist.n0[0] != 0 || hist.n1[0] != 0 {
		t.Fatalf("new bank slot is state %d, want the (0,0) initial state", b.st[b.idx])
	}
	var in [2]int32
	b.inputs(in[:], 0)
	if in[0] != stretchTab[2048] || in[1] != 0 {
		t.Fatalf("unseen slot predicts %d/%d, want even odds", in[0], in[1])
	}
	b.update(1)
	s := b.st[b.idx]
	if hist.n1[s] != 1 || hist.n0[s] != 0 {
		t.Fatalf("after one 1 the slot is (%d,%d), want (0,1)", hist.n0[s], hist.n1[s])
	}
	b.inputs(in[:], 0)
	if in[0] <= stretchTab[2048] || in[1] <= 0 {
		t.Fatalf("after one 1 the slot predicts %d/%d, want both above neutral", in[0], in[1])
	}
}

// The state table is the model's alphabet: every state a slot can hold must
// have both successors defined and inside the table.
func TestCMHistoryStates(t *testing.T) {
	live := 0
	for i := 0; i < 256; i++ {
		if hist.next[i][0] == uint8(i) && hist.next[i][1] == uint8(i) && i != 0 {
			continue // the unreachable tail, a fixed point by construction
		}
		live++
	}
	if live != 255 {
		t.Fatalf("%d live states, want 255", live)
	}
	for i := 0; i < 255; i++ {
		n0, n1 := int(hist.n0[i]), int(hist.n1[i])
		if !histOK(n0, n1) {
			t.Fatalf("state %d is (%d,%d), outside the staircase", i, n0, n1)
		}
		if hist.initP[i] >= 4096 {
			t.Fatalf("state %d has initial p %d, not 12-bit", i, hist.initP[i])
		}
		for bit := 0; bit < 2; bit++ {
			n := int(hist.next[i][bit])
			if n >= 255 {
				t.Fatalf("state %d +%d leaves the live table at %d", i, bit, n)
			}
			m0, m1 := int(hist.n0[n]), int(hist.n1[n])
			if !histOK(m0, m1) {
				t.Fatalf("state %d +%d -> (%d,%d), outside the staircase", i, bit, m0, m1)
			}
			// The opposite count never rises, and the state's own odds
			// move toward the bit just seen. The observed count itself can
			// fall: leaving n0=1 for n0=0 lowers the staircase from 48 to
			// 20, so (1,48) +1 lands on (0,20).
			if bit == 0 && (m1 > n1 || hist.initP[n] > hist.initP[i]) {
				t.Fatalf("state %d (%d,%d) +0 -> (%d,%d)", i, n0, n1, m0, m1)
			}
			if bit == 1 && (m0 > n0 || hist.initP[n] < hist.initP[i]) {
				t.Fatalf("state %d (%d,%d) +1 -> (%d,%d)", i, n0, n1, m0, m1)
			}
		}
	}
	// A long run of one bit must settle, not cycle back to even odds.
	s := uint8(0)
	for i := 0; i < 1000; i++ {
		s = hist.next[s][1]
	}
	if hist.n1[s] < 20 || hist.n0[s] != 0 {
		t.Fatalf("a thousand 1s land in (%d,%d)", hist.n0[s], hist.n1[s])
	}
}

// A state map must learn what a history is worth, and an untrained APM row
// must pass its input through unchanged.
func TestCMStateMapAndAPM(t *testing.T) {
	sm := newStateMap()
	s, prev := uint8(0), int32(-1)
	for i := 0; i < 200; i++ {
		p := sm.p(s)
		if p < prev-64 {
			t.Fatalf("update %d: p fell from %d to %d under a run of 1s", i, prev, p)
		}
		prev = p
		sm.update(1)
		s = hist.next[s][1]
	}
	if prev < 3800 {
		t.Fatalf("after 200 ones the map still says %d/4096", prev)
	}

	a := newAPM(4)
	for pr := int32(1); pr < 4095; pr += 37 {
		if got := a.p(pr, 2); got < pr-24 || got > pr+24 {
			t.Fatalf("untrained apm maps %d to %d", pr, got)
		}
	}
	base := a.p(2048, 1)
	for i := 0; i < 300; i++ {
		a.p(2048, 1)
		a.update(1)
	}
	if got := a.p(2048, 1); got <= base+256 {
		t.Fatalf("apm row did not learn: %d -> %d", base, got)
	}
	if other := a.p(2048, 3); other < 2000 || other > 2100 {
		t.Fatalf("training row 1 moved row 3 to %d", other)
	}
}

// Both wire codecs round-trip the stream shapes production offers them:
// prediction-conditioned corrections and LEB128 plan columns.
func TestCMRoundTripShapes(t *testing.T) {
	src, full := cmSample(20000)
	r := rand.New(rand.NewSource(11))
	var plan []byte
	for i := 0; i < 6000; i++ {
		v := uint64(r.Intn(1 << uint(1+r.Intn(20))))
		for {
			c := byte(v & 0x7F)
			v >>= 7
			if v != 0 {
				c |= 0x80
			}
			plan = append(plan, c)
			if v == 0 {
				break
			}
		}
	}
	pair := CMParseVarints(plan)
	cases := []struct {
		name string
		src  []byte
		side *CMSide
	}{
		{"pred", src, &CMSide{Pred: full.Pred, Sel: full.Sel}},
		{"field", src, full},
		{"varint", plan, &CMSide{Varint: true}},
		{"paired", plan, &CMSide{Varint: true, Pair: pair}},
	}
	for _, c := range cases {
		for _, compact := range []bool{false, true} {
			enc, dec := CMEncode, CMDecode
			if compact {
				enc, dec = CMEncodeCompact, CMDecodeCompact
			}
			coded, err := enc(c.src, c.side)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			got, err := dec(coded, len(c.src), c.side)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if !bytes.Equal(got, c.src) {
				t.Fatalf("%s (compact=%v): round trip differs", c.name, compact)
			}
		}
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
