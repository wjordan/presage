package elfmod

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/wjordan/presage/delta/x86"
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

// TestRoDataReplay runs the encoder's selection and the decoder's apply on a
// synthetic pair: one base-relative table whose targets all move, one
// self-relative table, and a chance run that must be left alone because it
// makes the prediction worse.
func TestRoDataReplay(t *testing.T) {
	const secAddr, secOff = uint64(0x10000), uint64(0x100)
	const textLo, textHi = uint64(0x80000), uint64(0x90000)
	const words = 24
	old := make([]byte, secOff+words*4)
	target := make([]byte, len(old))
	put := func(b []byte, i int, v int32) { binary.LittleEndian.PutUint32(b[secOff+uint64(i)*4:], uint32(v)) }
	shift := uint64(0x40) // every .text target moves by this
	// Words 0-5: base-relative table at word 0.
	for i := 0; i < 6; i++ {
		tgt := textLo + uint64(i)*0x20
		put(old, i, int32(int64(tgt)-int64(secAddr)))
		put(target, i, int32(int64(tgt+shift)-int64(secAddr)))
	}
	// Words 8-12: self-relative entries.
	for i := 8; i < 13; i++ {
		tgt := textLo + 0x1000 + uint64(i)*0x10
		put(old, i, int32(int64(tgt)-int64(secAddr+uint64(i)*4)))
		put(target, i, int32(int64(tgt+shift)-int64(secAddr+uint64(i)*4)))
	}
	// Words 16-19: land in .text by chance and do not change.
	for i := 16; i < 20; i++ {
		put(old, i, int32(int64(textLo+0x2000)-int64(secAddr)))
		put(target, i, int32(int64(textLo+0x2000)-int64(secAddr)))
	}
	pred := append([]byte(nil), old...) // the equivalence copy
	ep := equivalencePlan{OldLen: uint64(len(old)), NewLen: uint64(len(target)),
		Eqs: []equivalence{{Src: 0, Dst: 0, N: uint64(len(old))}}}
	mapper := newSourceEquivalenceMapper(ep)
	// Only the real targets are known: under a uniform shift the two
	// conventions would write the same bytes, and the point is that the
	// wrong one reads addresses nothing maps.
	known := map[uint64]bool{}
	for i := 0; i < 6; i++ {
		known[textLo+uint64(i)*0x20] = true
	}
	for i := 8; i < 13; i++ {
		known[textLo+0x1000+uint64(i)*0x10] = true
	}
	lookup := func(a uint64) x86.Target {
		if known[a] {
			return x86.Target{Addr: a + shift, Known: true}
		}
		return x86.Target{}
	}
	p := roDataPlan{OldOff: secOff, OldSize: words * 4, NewOff: secOff, NewSize: words * 4,
		OldAddr: secAddr, NewAddr: secAddr, TextLo: textLo, TextHi: textHi}
	p, est := selectRoDataTables(pred, old, target, p, mapper, lookup, noUnits)
	if est.Tables != 2 || est.SelfRel != 1 {
		t.Fatalf("encoder kept %+v, want two tables, one self-relative", est)
	}
	rt, err := unmarshalRoDataPlan(p.marshal())
	if err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), pred...)
	st := applyRoData(out, old, rt, mapper, lookup, noUnits)
	if st.Tables != 2 || st.Retargeted != 11 || st.Unresolved != 0 {
		t.Fatalf("decoder stats %+v", st)
	}
	if !bytes.Equal(out, target) {
		t.Fatalf("replay differs from target:\n got %x\nwant %x", out[secOff:], target[secOff:])
	}
}

// noUnits is a function map that owns nothing: a span it segments is one
// table, which is what a pair with no code window looks like.
func noUnits(uint64) (uint64, bool) { return 0, false }

// roSpanFixture builds a pair whose .rodata holds one span of self-relative
// entries assembled from four per-function tables, A B C D. In the new image
// B's function is gone and a five-word table nothing in the old image owns
// takes its place, so everything after A sits four words further on than the
// byte matcher, which has no matched run to work from, will guess.
type roSpanFixture struct {
	old, target, pred []byte
	p                 roDataPlan
	mapper            sourceEquivalenceMapper
	lookup            func(uint64) x86.Target
	unitAt            func(uint64) (uint64, bool)
	bounds            []int
}

const (
	roSecAddr, roSecOff        = uint64(0x10000), uint64(0x40)
	roTextLo, roTextHi         = uint64(0x80000), uint64(0x90000)
	roShift                    = uint64(0x400) // where every surviving .text target went
	roOldWords, roNewWords     = 15, 17
	roFnA, roFnB, roFnC, roFnD = roTextLo, roTextLo + 0x100, roTextLo + 0x200, roTextLo + 0x300
)

func newRoSpanFixture() *roSpanFixture {
	// Table t owns entries [start, end) and jumps into function fn.
	tables := []struct {
		start, end int
		fn         uint64
	}{{0, 4, roFnA}, {4, 7, roFnB}, {7, 11, roFnC}, {11, 15, roFnD}}
	f := &roSpanFixture{
		old:    make([]byte, roSecOff+roOldWords*4+16),
		target: make([]byte, roSecOff+roNewWords*4+16),
		bounds: []int{0, 4, 7, 11, 15},
	}
	// The old span, and the new one with B's three words replaced by five.
	newWord := func(k int) int {
		switch {
		case k < 4:
			return k
		case k < 7:
			return -1 // B is gone; five words of something else sit here
		default:
			return k + 2
		}
	}
	target := func(k int) uint64 {
		for _, t := range tables {
			if k >= t.start && k < t.end {
				return t.fn + uint64(k-t.start)*0x10
			}
		}
		panic("entry outside every table")
	}
	put := func(b []byte, i int, v int32) { binary.LittleEndian.PutUint32(b[roSecOff+uint64(i)*4:], uint32(v)) }
	for k := 0; k < roOldWords; k++ {
		put(f.old, k, int32(int64(target(k))-int64(roSecAddr+uint64(k)*4)))
	}
	for k := 4; k < 9; k++ {
		put(f.target, k, 0x5a5a5a5a) // the inserted table, which nothing predicts
	}
	for k := 0; k < roOldWords; k++ {
		if n := newWord(k); n >= 0 {
			put(f.target, n, int32(int64(target(k)+roShift)-int64(roSecAddr+uint64(n)*4)))
		}
	}
	// The matcher copies the old image over, which is all the span's
	// neighbourhood gives it: every entry after A is then four words early.
	f.pred = make([]byte, len(f.target))
	copy(f.pred, f.old)
	f.mapper = newSourceEquivalenceMapper(equivalencePlan{
		OldLen: uint64(len(f.old)), NewLen: uint64(len(f.target)),
		Eqs: []equivalence{{Src: 0, Dst: 0, N: uint64(len(f.old))}}})
	f.lookup = func(a uint64) x86.Target {
		if a >= roFnB && a < roFnC {
			return x86.Target{} // B is gone
		}
		if a >= roTextLo && a < roTextHi {
			return x86.Target{Addr: a + roShift, Known: true}
		}
		return x86.Target{}
	}
	f.unitAt = func(a uint64) (uint64, bool) {
		if a < roTextLo || a >= roTextHi {
			return 0, false
		}
		return a &^ 0xff, true
	}
	f.p = roDataPlan{
		OldOff: roSecOff, OldSize: roOldWords * 4, NewOff: roSecOff, NewSize: roNewWords * 4,
		OldAddr: roSecAddr, NewAddr: roSecAddr, TextLo: roTextLo, TextHi: roTextHi,
	}
	return f
}

// TestRoDataSegmentation checks the split both sides have to agree on: the
// entries of one span break at every change of owning function, and an entry
// whose target names no function stays with the table it follows.
func TestRoDataSegmentation(t *testing.T) {
	f := newRoSpanFixture()
	sec := f.old[f.p.OldOff : f.p.OldOff+f.p.OldSize]
	c := roDataCandidate{Span: [2]int{0, roOldWords}, SelfRel: true}
	got := segmentSpan(sec, f.p, c, f.unitAt)
	if !slices.Equal(got, f.bounds) {
		t.Errorf("segmented at %v, want %v", got, f.bounds)
	}
	// An owner nothing answers for does not start a table of its own.
	blind := func(a uint64) (uint64, bool) {
		if a >= roFnC {
			return 0, false
		}
		return f.unitAt(a)
	}
	if got := segmentSpan(sec, f.p, c, blind); !slices.Equal(got, []int{0, 4, roOldWords}) {
		t.Errorf("with C and D unowned, segmented at %v, want [0 4 15]", got)
	}
	// No function map at all is one table, which is what the projection does.
	if got := segmentSpan(sec, f.p, c, noUnits); !slices.Equal(got, []int{0, roOldWords}) {
		t.Errorf("with no owners, segmented at %v, want [0 15]", got)
	}
}

// TestRoDataCursorPlacement is the round trip over that fixture: the encoder
// must choose the cursor arm, spend one correction per table, and the decoder
// must rebuild every entry whose function survived -- across both a table
// deleted from the new image and one inserted into it.
func TestRoDataCursorPlacement(t *testing.T) {
	f := newRoSpanFixture()
	p, est := selectRoDataTables(f.pred, f.old, f.target, f.p, f.mapper, f.lookup, f.unitAt)
	if est.Segmented != 1 || est.Corrections != len(f.bounds)-1 {
		t.Fatalf("encoder chose %+v, want one segmented span and %d corrections", est, len(f.bounds)-1)
	}
	rt, err := unmarshalRoDataPlan(p.marshal())
	if err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), f.pred...)
	st := applyRoData(out, f.old, rt, f.mapper, f.lookup, f.unitAt)
	if st.Segmented != 1 || st.Retargeted != 12 || st.Unresolved != 3 {
		t.Fatalf("decoder stats %+v, want 12 entries retargeted and B's 3 unresolved", st)
	}
	// Everything but the words the inserted table occupies must be exact.
	for k := 0; k < roNewWords; k++ {
		off := roSecOff + uint64(k)*4
		got := binary.LittleEndian.Uint32(out[off:])
		want := binary.LittleEndian.Uint32(f.target[off:])
		if k >= 4 && k < 9 {
			continue
		}
		if got != want {
			t.Errorf("word %d is %#x, want %#x", k, got, want)
		}
	}
}

// TestRoDataCursorBounds is the decoder's guarantee against a plan it did not
// write: a correction of any size may lose the table, and may never move a
// write outside the new section.
func TestRoDataCursorBounds(t *testing.T) {
	f := newRoSpanFixture()
	bounds := []int{0, 4, 7, 11, 15}
	for _, d := range []int64{0, 1 << 40, -1 << 40, 1<<63 - 1, -1 << 63, int64(f.p.NewSize)} {
		pos := placeTables(f.p, bounds, roSecOff, []int64{d, d, d, d})
		for i := range pos {
			if pos[i] == roDataUnplaced {
				continue
			}
			end := pos[i] + uint64(bounds[i+1]-bounds[i])*4
			if pos[i] < f.p.NewOff || end > f.p.NewOff+f.p.NewSize {
				t.Fatalf("correction %d placed table %d at [%d,%d), outside [%d,%d)",
					d, i, pos[i], end, f.p.NewOff, f.p.NewOff+f.p.NewSize)
			}
		}
	}
	// A span list that runs off the end names no span at all.
	if got := readSegList(appendU(appendU(nil, 3), 1<<40), 5); !slices.Equal(got, []int{3}) {
		t.Errorf("readSegList took %v from a list whose second index is out of range, want [3]", got)
	}
	// The same through the decoder, with the span segmented and the
	// corrections nothing but a wild jump.
	p := f.p
	p.Keep = make([]byte, 8*roOldWords)
	p.Seg = appendU(nil, 0)
	bit := spanBit(0, 1)
	p.Keep[bit/8] |= 1 << (bit % 8)
	for range 8 {
		p.Cursor = appendS(p.Cursor, 1<<62)
	}
	out := append([]byte(nil), f.pred...)
	guard := append([]byte(nil), f.pred...)
	applyRoData(out, f.old, p, f.mapper, f.lookup, f.unitAt)
	if !bytes.Equal(out[:roSecOff], guard[:roSecOff]) || !bytes.Equal(out[roSecOff+p.NewSize:], guard[roSecOff+p.NewSize:]) {
		t.Error("apply wrote outside the new section")
	}
}
