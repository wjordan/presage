package main

import "testing"

// body builds a decodable run of one-byte instructions (push/pop reg, nop)
// so boundaries() and canonicalise() see the same instruction stream a real
// function would.
func body(n, seed int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(0x50 + (seed+i*7)%16)
	}
	return out
}

func TestAlignBodiesFindsAnInsertion(t *testing.T) {
	old := body(40, 1)
	ins := body(4, 9)
	nw := append(append(append([]byte(nil), old[:20]...), ins...), old[20:]...)
	segs := alignBodies(old, nw, alignWin, alignGap)
	if len(segs) != 2 {
		t.Fatalf("want 2 segments, got %d: %v", len(segs), segs)
	}
	if segs[0] != (seg{0, 0, 20}) {
		t.Errorf("prefix segment %v, want {0 0 20}", segs[0])
	}
	if segs[1] != (seg{20, 24, 20}) {
		t.Errorf("tail segment %v, want {20 24 20}", segs[1])
	}
	// every covered byte really matches, and the insertion is not covered
	for _, s := range segs {
		for k := range s.n {
			if old[s.oldOff+k] != nw[s.newOff+k] {
				t.Fatalf("segment %v mismatches at %d", s, k)
			}
		}
	}
}

func TestAlignBodiesIdenticalIsOneSegment(t *testing.T) {
	b := body(64, 3)
	segs := alignBodies(b, append([]byte(nil), b...), alignWin, alignGap)
	if len(segs) != 1 || segs[0] != (seg{0, 0, 64}) {
		t.Fatalf("identical bodies: %v", segs)
	}
}

// A segment must never send the decoder backwards in the old body: the
// assembled body is written forward, and the plan's offset column is coded as
// a delta against the previous segment.
func TestAlignBodiesIsMonotone(t *testing.T) {
	a := body(30, 2)
	b := body(30, 5)
	nw := append(append(append([]byte(nil), b...), a...), b...) // a re-ordering
	segs := alignBodies(append(append([]byte(nil), a...), b...), nw, alignWin, alignGap)
	po, pn := -1, -1
	for _, s := range segs {
		if s.oldOff < po || s.newOff < pn {
			t.Fatalf("segments not monotone: %v", segs)
		}
		po, pn = s.oldOff+s.n, s.newOff+s.n
	}
}

func TestCountRunsMergesAndDeduplicates(t *testing.T) {
	// 10 and 13 are within mergeGap of each other, 40 is not; 10 is repeated
	runs, bytes := countRuns([]int{40, 10, 13, 10})
	if runs != 2 || bytes != 3 {
		t.Fatalf("runs %d bytes %d, want 2 and 3", runs, bytes)
	}
	if r, b := countRuns(nil); r != 0 || b != 0 {
		t.Fatalf("empty: %d %d", r, b)
	}
}
