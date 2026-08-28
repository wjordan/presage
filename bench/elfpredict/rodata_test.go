package main

import (
	"encoding/binary"
	"testing"
)

// TestJumpTableDetection checks the signature the model relies on: a run of
// self-relative entries that all land in .text. It must find the real table,
// take the run's own start as the base, and not be fooled by isolated values
// that happen to point into .text.
func TestJumpTableDetection(t *testing.T) {
	const secAddr, textLo, textHi = uint64(0x10000), uint64(0x80000), uint64(0x90000)
	sec := make([]byte, 4*16)
	put := func(i int, v int32) { binary.LittleEndian.PutUint32(sec[i*4:], uint32(v)) }

	// Words 0 and 1 are noise pointing nowhere.
	put(0, 0x11)
	put(1, -0x400000)
	// A single stray word that does land in .text: too short to be a table.
	put(2, int32(textLo-(secAddr+2*4))+0x10)
	put(3, 0x7f)
	// Words 4..9 are a real table, all relative to the address of word 4.
	base := secAddr + 4*4
	for i := 4; i < 10; i++ {
		put(i, int32(int64(textLo+uint64((i-4)*0x20))-int64(base)))
	}
	// Trailing noise.
	for i := 10; i < 16; i++ {
		put(i, 0x1)
	}

	got := jumpTables(sec, secAddr, textLo, textHi, 4)
	if len(got) != 1 {
		t.Fatalf("found %d tables, want 1: %v", len(got), got)
	}
	if got[0][0] != 4 || got[0][1] != 10 {
		t.Errorf("table spans words [%d,%d), want [4,10)", got[0][0], got[0][1])
	}
}

// TestJumpTableRunTooShort guards the threshold: three consecutive in-range
// values are common by chance in 24 MB of constants and must not be taken for
// a table.
func TestJumpTableRunTooShort(t *testing.T) {
	const secAddr, textLo, textHi = uint64(0x10000), uint64(0x80000), uint64(0x90000)
	sec := make([]byte, 4*8)
	base := secAddr
	for i := 0; i < 3; i++ {
		binary.LittleEndian.PutUint32(sec[i*4:], uint32(int32(int64(textLo)-int64(base))))
	}
	if got := jumpTables(sec, secAddr, textLo, textHi, 4); len(got) != 0 {
		t.Errorf("found %d tables in a run of 3, want none at a threshold of 4", len(got))
	}
	// The same run is offered to the encoder as a short candidate.
	if got := shortCandidates(sec, secAddr, textLo, textHi, nil); len(got) != 1 {
		t.Errorf("found %d short candidates in a run of 3, want 1", len(got))
	}
}

// TestShortCandidatesSkipLongTables is the property that makes the short scan
// safe to add: it must never start a run that reaches into a table the long
// scan already claimed, because every entry would then be read against a base
// one word early.
func TestShortCandidatesSkipLongTables(t *testing.T) {
	const secAddr, textLo, textHi = uint64(0x10000), uint64(0x80000), uint64(0x90000)
	sec := make([]byte, 4*16)
	put := func(i int, v int32) { binary.LittleEndian.PutUint32(sec[i*4:], uint32(v)) }
	// Words 0-1 are a short run; words 2-9 are a long table; the rest is noise
	// that does not land in .text. Every word before the table also lands in
	// .text relative to word 0, which is exactly the trap.
	for i := 0; i < 10; i++ {
		put(i, int32(int64(textLo+uint64(i*0x20))-int64(secAddr)))
	}
	for i := 10; i < 16; i++ {
		put(i, 1)
	}
	long := jumpTables(sec, secAddr, textLo, textHi, jumpTableMinRun)
	if len(long) != 1 || long[0] != [2]int{0, 10} {
		t.Fatalf("long scan found %v, want one table spanning [0,10)", long)
	}
	// With the table starting at word 4 instead, the short scan gets the lead-in.
	for i := 0; i < 2; i++ {
		put(i, int32(int64(textLo)-int64(secAddr+uint64(i*4))))
	}
	put(2, 1)
	put(3, 1)
	long = jumpTables(sec, secAddr, textLo, textHi, jumpTableMinRun)
	if len(long) != 1 || long[0] != [2]int{4, 10} {
		t.Fatalf("long scan found %v, want one table spanning [4,10)", long)
	}
	for _, sp := range roDataSpans(sec, secAddr, textLo, textHi) {
		if sp == long[0] {
			continue
		}
		if sp[0] < long[0][1] && sp[1] > long[0][0] {
			t.Errorf("span %v overlaps the table at %v", sp, long[0])
		}
	}
}

// TestSpanVariantBits pins the variant addressing: an unset apply bit means
// the variant is skipped, the stride is fixed so a short span cannot shift a
// later span's bits, and each origin from the run start inwards is offered
// under both conventions.
func TestSpanVariantBits(t *testing.T) {
	got := spanVariants([2]int{4, 10})
	if len(got) != 2*roDataBaseShifts {
		t.Fatalf("span of six offered %d variants, want %d", len(got), 2*roDataBaseShifts)
	}
	if got[0].Span != [2]int{4, 10} || got[0].SelfRel {
		t.Errorf("first variant %+v, want [4,10) base-relative", got[0])
	}
	if got[2].Span != [2]int{5, 10} {
		t.Errorf("third variant spans %v, want [5,10)", got[2].Span)
	}
	// A span of two can only offer two origins, not four.
	if n := len(spanVariants([2]int{0, 2})); n != 4 {
		t.Errorf("span of two offered %d variants, want 4", n)
	}
	// The stride does not depend on how many variants a span offered.
	if spanBit(1, 0) != 2*roDataBaseShifts {
		t.Errorf("span 1 starts at bit %d, want %d", spanBit(1, 0), 2*roDataBaseShifts)
	}
	if bitSet([]byte{0x01}, 1) || !bitSet([]byte{0x02}, 1) {
		t.Error("bitSet read the wrong bit")
	}
}
