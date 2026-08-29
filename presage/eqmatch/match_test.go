package eqmatch

import (
	"math/rand"
	"testing"
)

// checkRuns asserts the contract Decode enforces — runs in destination
// order, non-overlapping there, non-empty, inside both images, at least
// Min long, and paying for themselves under the score — and returns the
// bytes covered and the bytes covered correctly.
func checkRuns(t *testing.T, src, dst []byte, runs []Run, p Params) (covered, correct uint64) {
	t.Helper()
	p = p.withDefaults()
	var prevEnd uint64
	for i, r := range runs {
		if r.N == 0 {
			t.Fatalf("run %d is empty", i)
		}
		if r.Dst < prevEnd {
			t.Fatalf("run %d starts at %d, inside the previous run ending at %d", i, r.Dst, prevEnd)
		}
		if r.Src+r.N > uint64(len(src)) || r.Dst+r.N > uint64(len(dst)) {
			t.Fatalf("run %d %+v leaves an image", i, r)
		}
		if r.N < uint64(p.Min) {
			t.Fatalf("run %d is %d bytes, below the floor of %d", i, r.N, p.Min)
		}
		var agree uint64
		for j := uint64(0); j < r.N; j++ {
			if src[r.Src+j] == dst[r.Dst+j] {
				agree++
			}
		}
		if agree*match < (r.N-agree)*uint64(-mismatch) {
			t.Fatalf("run %d of %d bytes agrees on only %d", i, r.N, agree)
		}
		prevEnd, covered, correct = r.Dst+r.N, covered+r.N, correct+agree
	}
	return covered, correct
}

func randomBytes(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func TestIdentical(t *testing.T) {
	b := randomBytes(1, 1<<16)
	runs := Match(b, b, Params{})
	if covered, correct := checkRuns(t, b, b, runs, Params{}); covered != uint64(len(b)) || correct != covered {
		t.Fatalf("covered %d (%d correct) of %d bytes in %d runs", covered, correct, len(b), len(runs))
	}
	if len(runs) != 1 || runs[0].Src != 0 || runs[0].Dst != 0 {
		t.Fatalf("identical images want one run at 0, got %+v", runs)
	}
}

// A block moved by a constant delta must come back as two runs with the
// insertion between them, not as a scatter.
func TestInsertion(t *testing.T) {
	src := randomBytes(2, 1<<16)
	inserted := randomBytes(3, 4096)
	dst := append(append(append([]byte{}, src[:1<<15]...), inserted...), src[1<<15:]...)
	runs := Match(src, dst, Params{})
	covered, correct := checkRuns(t, src, dst, runs, Params{})
	if want := uint64(len(src)); correct < want-64 {
		t.Fatalf("covered %d correctly (%d covered) of %d unchanged bytes in %d runs", correct, covered, want, len(runs))
	}
	if len(runs) > 4 {
		t.Fatalf("one insertion wants a handful of runs, got %d", len(runs))
	}
	last := runs[len(runs)-1]
	if int64(last.Dst)-int64(last.Src) != int64(len(inserted)) {
		t.Fatalf("the run after the insertion has delta %d, want %d", int64(last.Dst)-int64(last.Src), len(inserted))
	}
}

// Two blocks that swapped places: neither can keep the delta the other set.
func TestTransposition(t *testing.T) {
	a, b := randomBytes(4, 1<<15), randomBytes(5, 1<<15)
	src := append(append([]byte{}, a...), b...)
	dst := append(append([]byte{}, b...), a...)
	runs := Match(src, dst, Params{})
	if _, correct := checkRuns(t, src, dst, runs, Params{}); correct < uint64(len(dst))-64 || len(runs) > 4 {
		t.Fatalf("covered %d of %d bytes correctly in %d runs", correct, len(dst), len(runs))
	}
}

// An image peppered with single-byte edits comes back as one run.
func TestScattered(t *testing.T) {
	src := randomBytes(6, 1<<16)
	dst := append([]byte{}, src...)
	r := rand.New(rand.NewSource(7))
	for range 400 {
		dst[r.Intn(len(dst))] ^= 0xff
	}
	runs := Match(src, dst, Params{})
	if covered, _ := checkRuns(t, src, dst, runs, Params{}); covered < uint64(len(dst))-64 || len(runs) != 1 {
		t.Fatalf("400 scattered edits: covered %d of %d in %d runs, want one", covered, len(dst), len(runs))
	}
}

func TestFloor(t *testing.T) {
	src := randomBytes(8, 1<<16)
	dst := append(append(append([]byte{}, src[1<<12:1<<15]...), randomBytes(9, 999)...), src[:1<<12]...)
	for _, m := range []int{8, 12, 16, 24} {
		p := Params{Min: m}
		runs := Match(src, dst, p)
		if _, correct := checkRuns(t, src, dst, runs, p); correct < uint64(1<<15)-64 {
			t.Fatalf("min %d: covered %d of %d movable bytes correctly in %d runs", m, correct, 1<<15, len(runs))
		}
	}
}

func TestUnrelated(t *testing.T) {
	src, dst := randomBytes(10, 1<<16), randomBytes(11, 1<<16)
	runs := Match(src, dst, Params{})
	if covered, _ := checkRuns(t, src, dst, runs, Params{}); covered > 1<<10 {
		t.Fatalf("unrelated images matched %d bytes in %d runs", covered, len(runs))
	}
}

func TestDegenerate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, dst []byte
	}{
		{"empty", nil, nil},
		{"empty src", nil, randomBytes(12, 1024)},
		{"empty dst", randomBytes(12, 1024), nil},
		{"short", []byte("abc"), []byte("abc")},
		{"zeroes", make([]byte, 1<<16), make([]byte, 1<<16)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkRuns(t, tc.src, tc.dst, Match(tc.src, tc.dst, Params{}), Params{})
		})
	}
}

func TestEncodeDecode(t *testing.T) {
	src := randomBytes(13, 1<<16)
	dst := append(append(append([]byte{}, src[1<<12:1<<15]...), randomBytes(14, 999)...), src[:1<<12]...)
	runs := Match(src, dst, Params{})
	checkRuns(t, src, dst, runs, Params{})
	b := Encode(runs)
	back, err := Decode(b, uint64(len(src)), uint64(len(dst)))
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(runs) {
		t.Fatalf("decoded %d runs, encoded %d", len(back), len(runs))
	}
	for i := range runs {
		if back[i] != runs[i] {
			t.Fatalf("run %d decoded as %+v, encoded %+v", i, back[i], runs[i])
		}
	}
	if _, err := Decode(b, uint64(len(src)), uint64(len(dst))-1); err == nil {
		t.Fatal("a run leaving the destination was accepted")
	}
	if _, err := Decode(b[:len(b)-1], uint64(len(src)), uint64(len(dst))); err == nil {
		t.Fatal("a truncated plan was accepted")
	}
	if _, err := Decode(nil, 1, 1); err == nil {
		t.Fatal("an empty plan was accepted")
	}
	if runs, err := Decode(Encode(nil), 1, 1); err != nil || len(runs) != 0 {
		t.Fatalf("empty run list: %v %v", runs, err)
	}
}
