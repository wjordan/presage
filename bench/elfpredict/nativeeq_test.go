package main

import (
	"math/rand"
	"testing"
)

// checkEquivalences asserts the contract decodeEquivalences enforces -- runs in
// destination order, non-overlapping in the destination, non-empty, inside both
// images -- and returns the bytes covered and the bytes covered correctly. Runs
// are fuzzy, so the second is what says whether a run was worth emitting.
func checkEquivalences(t *testing.T, old, nw []byte, eqs []equivalence) (covered, correct uint64) {
	t.Helper()
	var prevEnd uint64
	for i, e := range eqs {
		if e.N == 0 {
			t.Fatalf("run %d is empty", i)
		}
		if e.Dst < prevEnd {
			t.Fatalf("run %d starts at %d, inside the previous run ending at %d", i, e.Dst, prevEnd)
		}
		if e.Src+e.N > uint64(len(old)) || e.Dst+e.N > uint64(len(nw)) {
			t.Fatalf("run %d (%d,%d,%d) leaves an image", i, e.Src, e.Dst, e.N)
		}
		if e.N < uint64(nativeEqMin) {
			t.Fatalf("run %d is %d bytes, below the floor of %d", i, e.N, nativeEqMin)
		}
		var agree uint64
		for j := uint64(0); j < e.N; j++ {
			if old[e.Src+j] == nw[e.Dst+j] {
				agree++
			}
		}
		// The extension cuts at the score peak, so every run pays for itself.
		if agree*uint64(nativeEqMatch) < (e.N-agree)*uint64(-nativeEqMismatch) {
			t.Fatalf("run %d of %d bytes agrees on only %d", i, e.N, agree)
		}
		prevEnd, covered, correct = e.Dst+e.N, covered+e.N, correct+agree
	}
	return covered, correct
}

func randomBytes(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func TestNativeEquivalencesIdentical(t *testing.T) {
	b := randomBytes(1, 1<<16)
	eqs := nativeEquivalences(b, b)
	if covered, correct := checkEquivalences(t, b, b, eqs); covered != uint64(len(b)) || correct != covered {
		t.Fatalf("covered %d (%d correct) of %d bytes in %d runs", covered, correct, len(b), len(eqs))
	}
	if len(eqs) != 1 {
		t.Fatalf("identical images want one run, got %d", len(eqs))
	}
	if eqs[0].Src != 0 || eqs[0].Dst != 0 {
		t.Fatalf("run is %+v", eqs[0])
	}
}

// TestNativeEquivalencesInsertion checks the shape a real pair has: a block
// moved by a constant delta. The two halves must come back as two runs with the
// insertion between them, not as a scatter.
func TestNativeEquivalencesInsertion(t *testing.T) {
	old := randomBytes(2, 1<<16)
	inserted := randomBytes(3, 4096)
	nw := append(append(append([]byte{}, old[:1<<15]...), inserted...), old[1<<15:]...)

	eqs := nativeEquivalences(old, nw)
	covered, correct := checkEquivalences(t, old, nw, eqs)
	if want := uint64(len(old)); correct < want-64 {
		t.Fatalf("covered %d bytes correctly (%d covered) of the %d unchanged, in %d runs",
			correct, covered, want, len(eqs))
	}
	if len(eqs) > 4 {
		t.Fatalf("one insertion wants a handful of runs, got %d", len(eqs))
	}
	last := eqs[len(eqs)-1]
	if int64(last.Dst)-int64(last.Src) != int64(len(inserted)) {
		t.Fatalf("the run after the insertion has delta %d, want %d",
			int64(last.Dst)-int64(last.Src), len(inserted))
	}
}

// TestNativeEquivalencesTransposition is the case the delta-consistency rule
// exists for: two blocks that swapped places, so neither can keep the delta the
// other set.
func TestNativeEquivalencesTransposition(t *testing.T) {
	a, b := randomBytes(4, 1<<15), randomBytes(5, 1<<15)
	old := append(append([]byte{}, a...), b...)
	nw := append(append([]byte{}, b...), a...)

	eqs := nativeEquivalences(old, nw)
	_, correct := checkEquivalences(t, old, nw, eqs)
	if correct < uint64(len(nw))-64 {
		t.Fatalf("covered %d of %d bytes correctly in %d runs", correct, len(nw), len(eqs))
	}
	if len(eqs) > 4 {
		t.Fatalf("one transposition wants a handful of runs, got %d", len(eqs))
	}
}

// TestNativeEquivalencesScattered is what the fuzzy extension is for: an image
// peppered with single-byte edits has to come back as one run, not as one run
// per undisturbed stretch.
func TestNativeEquivalencesScattered(t *testing.T) {
	old := randomBytes(6, 1<<16)
	nw := append([]byte{}, old...)
	r := rand.New(rand.NewSource(7))
	for range 400 {
		nw[r.Intn(len(nw))] ^= 0xff
	}
	eqs := nativeEquivalences(old, nw)
	covered, _ := checkEquivalences(t, old, nw, eqs)
	if covered < uint64(len(nw))-64 {
		t.Fatalf("covered %d of %d bytes in %d runs", covered, len(nw), len(eqs))
	}
	if len(eqs) != 1 {
		t.Fatalf("400 scattered edits want one run, got %d", len(eqs))
	}
}

// TestNativeEquivalencesFloor checks the floor is respected at every setting
// and that a longer one never leaves the image mostly uncovered.
func TestNativeEquivalencesFloor(t *testing.T) {
	old := randomBytes(8, 1<<16)
	nw := append(append(append([]byte{}, old[1<<12:1<<15]...), randomBytes(9, 999)...), old[:1<<12]...)
	defer func(v int) { nativeEqMin = v }(nativeEqMin)

	for _, m := range []int{8, 12, 16, 24} {
		nativeEqMin = m
		eqs := nativeEquivalences(old, nw)
		_, correct := checkEquivalences(t, old, nw, eqs)
		if want := uint64(1<<15 - 1<<12 + 1<<12); correct < want-64 {
			t.Fatalf("min %d: covered %d of %d movable bytes correctly in %d runs", m, correct, want, len(eqs))
		}
	}
}

// TestNativeEquivalencesUnrelated checks the matcher does not invent runs
// between images that share nothing.
func TestNativeEquivalencesUnrelated(t *testing.T) {
	old, nw := randomBytes(10, 1<<16), randomBytes(11, 1<<16)
	eqs := nativeEquivalences(old, nw)
	if covered, _ := checkEquivalences(t, old, nw, eqs); covered > 1<<10 {
		t.Fatalf("unrelated images matched %d bytes in %d runs", covered, len(eqs))
	}
}

func TestNativeEquivalencesDegenerate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		old, nw []byte
	}{
		{"empty", nil, nil},
		{"empty old", nil, randomBytes(12, 1024)},
		{"empty new", randomBytes(12, 1024), nil},
		{"short", []byte("abc"), []byte("abc")},
		{"zeroes", make([]byte, 1<<16), make([]byte, 1<<16)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkEquivalences(t, tc.old, tc.nw, nativeEquivalences(tc.old, tc.nw))
		})
	}
}

// TestNativeEquivalencesRoundTrip runs the matcher's output through the encoder
// and decoder the plan actually ships, which is the only proof the runs are
// encodable at all.
func TestNativeEquivalencesRoundTrip(t *testing.T) {
	old := randomBytes(13, 1<<16)
	nw := append(append(append([]byte{}, old[1<<12:1<<15]...), randomBytes(14, 999)...), old[:1<<12]...)

	eqs := nativeEquivalences(old, nw)
	checkEquivalences(t, old, nw, eqs)
	p := equivalencePlan{OldLen: uint64(len(old)), NewLen: uint64(len(nw)), Eqs: eqs}
	p.SrcSkip, p.DstSkip, p.CopyLen = encodeColumns(eqs)
	b, err := p.marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	back, err := unmarshalEquivalencePlan(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Eqs) != len(eqs) {
		t.Fatalf("decoded %d runs, encoded %d", len(back.Eqs), len(eqs))
	}
	for i := range eqs {
		if back.Eqs[i] != eqs[i] {
			t.Fatalf("run %d decoded as %+v, encoded %+v", i, back.Eqs[i], eqs[i])
		}
	}
}
