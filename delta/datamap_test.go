package delta

import "testing"

// blk is a distinctive 16-byte block, so that a window index over it has one
// occurrence and the map's choice is not an accident of repetition.
func blk(i int) []byte {
	b := make([]byte, 16)
	b[0], b[1], b[2], b[3] = byte(i), byte(i>>8), 0x5a, 0xa5
	for k := 4; k < 16; k++ {
		b[k] = byte(i*31 + k*17 + 7)
	}
	return b
}

func blocks(idx ...int) []byte {
	var out []byte
	for _, i := range idx {
		out = append(out, blk(i)...)
	}
	return out
}

func seq(lo, hi int) []int {
	var out []int
	for i := lo; i < hi; i++ {
		out = append(out, i)
	}
	return out
}

// A block whose content changed does not match at the incoming shift, and if
// its old bytes happen to occur somewhere else in the new section the map
// used to follow them there -- taking the rest of the symbol with it, since
// the following blocks inherit the shift. In .go.type that copied whole type
// descriptors megabytes away and left the right place empty. One block is not
// enough evidence for a shift change.
func TestDataMapIgnoresAShiftOnlyOneBlockAgreesWith(t *testing.T) {
	old := blocks(seq(0, 40)...)
	// blocks 2 and 3 both changed; block 2's old bytes still occur, far away
	n := append(blocks(0, 1, 100, 101), blocks(seq(4, 40)...)...)
	n = append(n, blk(2)...)

	m := buildDataMap(old, n, 16, nil, 0)
	for i := range 40 {
		if m.Delta[i] != 0 {
			t.Fatalf("block %d shifted by %d, want 0 (the decoy copy is not evidence)", i, m.Delta[i])
		}
	}
	if m.Matched[2] {
		t.Errorf("block 2 is reported matched; its content changed")
	}
}

// The same rule must not throw away a real move: a run of blocks that all
// match at one new shift is a symbol the linker reordered.
func TestDataMapFollowsACorroboratedMove(t *testing.T) {
	old := blocks(seq(0, 40)...)
	moved := []int{10, 11, 12, 13}
	var idx []int
	idx = append(idx, seq(0, 10)...)
	idx = append(idx, seq(14, 40)...)
	idx = append(idx, moved...)
	n := blocks(idx...)

	m := buildDataMap(old, n, 16, nil, 0)
	for _, i := range moved {
		if want := int64(16 * (36 - 10)); m.Delta[i] != want {
			t.Fatalf("moved block %d has shift %d, want %d", i, m.Delta[i], want)
		}
	}
	for i := 14; i < 40; i++ {
		if want := int64(-16 * 4); m.Delta[i] != want {
			t.Fatalf("block %d has shift %d, want %d", i, m.Delta[i], want)
		}
	}
}
