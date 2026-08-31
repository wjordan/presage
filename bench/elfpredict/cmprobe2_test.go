package main

import (
	"bytes"
	"math/rand"
	"testing"
)

// The reference match models feed the mixer from state the decoder rebuilds as
// it goes, so a desync would show up as a silent size win and a broken decode.
// Round-trip both flavours on a synthetic prediction/target pair.
func TestRefMatchRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	const n = 60000
	pred := make([]byte, n)
	for i := range pred {
		if i > 2000 && r.Intn(3) > 0 {
			// Repeat earlier content so the global index has real candidates.
			pred[i] = pred[i-1000]
		} else {
			pred[i] = byte(r.Intn(64))
		}
	}
	target := bytes.Clone(pred)
	var runs []cmRun
	for i := 50; i < n-50; {
		if r.Intn(9) != 0 {
			i++
			continue
		}
		ln := 1 + r.Intn(6)
		for k := 0; k < ln; k++ {
			// Guarantee every byte of the run differs.
			target[i+k] = pred[i+k] ^ byte(1+r.Intn(255))
		}
		runs = append(runs, cmRun{i, i + ln, bucketOf(ln)})
		i += ln
	}
	if len(runs) < 100 {
		t.Fatalf("test data has only %d runs", len(runs))
	}

	var coded [correctionBuckets][]byte
	byBucket := make([][]cmRun, correctionBuckets)
	for _, rn := range runs {
		byBucket[rn.buck] = append(byBucket[rn.buck], rn)
		coded[rn.buck] = append(coded[rn.buck], target[rn.lo:rn.hi]...)
	}
	side := buildSide(pred, target, byBucket, coded)
	if len(side.stream) == 0 {
		t.Fatal("empty stream")
	}
	ix := buildRefIndex(pred, false)
	bix := buildRefIndex(pred, true)

	mk := func(f func() []*refMatch) func() cmContexts {
		return func() cmContexts { return &refCtx{side.ctx(4), f()} }
	}
	cases := []struct {
		name string
		set  func() cmContexts
	}{
		{"local", mk(func() []*refMatch {
			return []*refMatch{newRefMatch(pred, side.pos, rmLocal, 128, 0, 4, nil)}
		})},
		{"global", mk(func() []*refMatch {
			return []*refMatch{newRefMatch(pred, side.pos, rmGlobal, 0, 32, 6, ix)}
		})},
		{"local-indexed", mk(func() []*refMatch {
			return []*refMatch{newRefMatch(pred, side.pos, rmLocalIdx, 4096, 32, 6, bix)}
		})},
		{"both", mk(func() []*refMatch {
			return []*refMatch{
				newRefMatch(pred, side.pos, rmLocal, 128, 0, 4, nil),
				newRefMatch(pred, side.pos, rmGlobal, 0, 32, 6, ix),
			}
		})},
	}
	for _, c := range cases {
		out := cmEncode(side.stream, c.set())
		back := cmDecode(out, len(side.stream), c.set())
		if !bytes.Equal(back, side.stream) {
			t.Fatalf("%s: round trip mismatch", c.name)
		}
	}
}

// buildSide must reproduce the shipped byte order when the runs are given in
// address order, and must be a pure permutation under any other order.
func TestBuildSideOrdering(t *testing.T) {
	pred := bytes.Repeat([]byte("abcdefgh"), 2000)
	target := bytes.Clone(pred)
	var runs []cmRun
	for i := 100; i < len(pred)-100; i += 37 {
		ln := 1 + i%5
		for k := 0; k < ln; k++ {
			target[i+k] = pred[i+k] ^ 0x5A
		}
		runs = append(runs, cmRun{i, i + ln, bucketOf(ln)})
	}
	var coded [correctionBuckets][]byte
	byBucket := make([][]cmRun, correctionBuckets)
	for _, rn := range runs {
		byBucket[rn.buck] = append(byBucket[rn.buck], rn)
		coded[rn.buck] = append(coded[rn.buck], target[rn.lo:rn.hi]...)
	}
	addr := buildSide(pred, target, byBucket, coded)
	var want []byte
	for b := 0; b < correctionBuckets; b++ {
		want = append(want, coded[b]...)
	}
	if !bytes.Equal(addr.stream, want) {
		t.Fatal("address order does not reproduce the shipped bucket layout")
	}
	// Every stream byte must be the target byte at its recorded position.
	for i, p := range addr.pos {
		if addr.stream[i] != target[p] {
			t.Fatalf("stream[%d] != target[%d]", i, p)
		}
	}
	for _, k := range orderKeys[1:] {
		ord := make([][]cmRun, correctionBuckets)
		for b := range byBucket {
			ord[b] = append([]cmRun(nil), byBucket[b]...)
			sortRuns(ord[b], pred, k)
		}
		s := buildSide(pred, target, ord, coded)
		if len(s.stream) != len(addr.stream) {
			t.Fatalf("%s: length %d != %d", k.name, len(s.stream), len(addr.stream))
		}
		for i, p := range s.pos {
			if s.stream[i] != target[p] {
				t.Fatalf("%s: stream[%d] != target[%d]", k.name, i, p)
			}
		}
		a, b := bytes.Clone(s.stream), bytes.Clone(addr.stream)
		sortBytes(a)
		sortBytes(b)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: not a permutation of the shipped bytes", k.name)
		}
	}
}
