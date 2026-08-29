package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/wjordan/go-binsync/delta"
	"github.com/wjordan/go-binsync/delta/x86"
)

// Chrome's .rela.dyn is 26 MB of pure address churn: of the 796,001 slots
// present in both releases only 4 keep their addend. A byte matcher can do
// nothing with it, and neither can a byte-domain correction -- inserting one
// entry shifts every later slot value, so the correction pays for the whole
// tail of the table.
//
// The table is really three independent columns, and each has its own natural
// domain. Slots are a sorted address list whose *gaps* are a property of the
// data layout, not of where that data landed, so gap-coding turns a global
// shift into a handful of local edits. Addends are pointers, and pointers must
// be resolved by identity: the symbol-derived function map places 99.075% of
// them exactly, where the byte-derived equivalence map manages only 78.760%,
// because identical-code folding makes byte matches ambiguous for a pointer.
//
// Predicting in those domains and correcting there costs about 4 KB. Doing the
// same work in the byte domain costs 2.46 MB.

const relaEntrySize = 24

// relTypeRelative is R_X86_64_RELATIVE. The 116 entries of other types are
// carried separately because they sort after the relative block.
const relTypeRelative = 8

// sectionMap translates between image addresses and file offsets. The decoder
// needs the new image's geometry to know where a projected offset lands, and
// that geometry is target-derived, so it is shipped in the plan and charged.
type sectionMap []addressRange // Old holds the address, New holds the file offset

func newSectionMap(secs map[string]section) sectionMap {
	m := make(sectionMap, 0, len(secs))
	for _, s := range secs {
		m = append(m, addressRange{Old: s.Addr, New: s.Off, Size: s.Size})
	}
	slices.SortFunc(m, func(a, b addressRange) int { return cmpU(a.Old, b.Old) })
	return m
}

func (m sectionMap) offsetOf(addr uint64) (uint64, bool) {
	i, ok := slices.BinarySearchFunc(m, addr, func(r addressRange, addr uint64) int {
		if r.Old > addr {
			return 1
		}
		if r.Old+r.Size <= addr {
			return -1
		}
		return 0
	})
	if !ok {
		return 0, false
	}
	return m[i].New + addr - m[i].Old, true
}

func (m sectionMap) addrOf(off uint64) (uint64, bool) {
	for _, r := range m {
		if off >= r.New && off < r.New+r.Size {
			return r.Old + off - r.New, true
		}
	}
	return 0, false
}

func (m sectionMap) marshal(b []byte) []byte {
	b = appendU(b, uint64(len(m)))
	var prevAddr, prevOff uint64
	for _, r := range m {
		b = appendU(b, r.Old-prevAddr)
		if r.New >= prevOff {
			b = appendS(b, int64(r.New-prevOff))
		} else {
			b = appendS(b, -int64(prevOff-r.New))
		}
		b = appendU(b, r.Size)
		prevAddr, prevOff = r.Old, r.New
	}
	return b
}

func unmarshalSectionMap(r *planReader) (sectionMap, error) {
	n := r.u()
	if r.err != nil || n > 1<<20 {
		return nil, errors.New("implausible section map")
	}
	m := make(sectionMap, 0, n)
	var prevAddr, prevOff uint64
	for i := uint64(0); i < n; i++ {
		addrDelta, offDelta, size := r.u(), r.s(), r.u()
		if r.err != nil {
			return nil, errors.New("invalid section map entry")
		}
		addr := prevAddr + addrDelta
		var off uint64
		if offDelta >= 0 {
			off = prevOff + uint64(offDelta)
		} else {
			d := uint64(-(offDelta + 1)) + 1
			if d > prevOff {
				return nil, errors.New("section map offset underflows")
			}
			off = prevOff - d
		}
		m = append(m, addressRange{Old: addr, New: off, Size: size})
		prevAddr, prevOff = addr, off
	}
	if !slices.IsSortedFunc(m, func(a, b addressRange) int { return cmpU(a.Old, b.Old) }) {
		return nil, errors.New("section map is not sorted by address")
	}
	return m, nil
}

// relocPlan is everything the decoder needs to regenerate .rela.dyn: where the
// table lives on each side, the section maps that turn an address into a file
// offset and back, and one correction per column.
type relocPlan struct {
	OldSecs, NewSecs sectionMap
	OldOff, OldSize  uint64
	NewOff, NewSize  uint64
	RelCount         uint64 // R_X86_64_RELATIVE entries in the new table
	TailCount        uint64 // entries of every other type
	Anchor           uint64 // first slot of the new relative block
	GapCorrection    []byte
	AddendCorrection []byte
	TailCorrection   []byte
	// PairByRow selects the positional addend pairing of §3.3 instead of the
	// slot join. It is off by default and is only marshalled when set, so a
	// plan that does not use it is byte-identical to one from before it
	// existed.
	PairByRow bool
	// NoAddends drops the addend column altogether: the plan predicts and
	// corrects the slot gaps and the tail only, and the decoder leaves every
	// entry's addend bytes exactly as the equivalence copy wrote them. That is
	// what upstream Zucchini does with .rela.dyn -- it treats r_offset as a
	// reference and never looks at r_addend -- so the addends it gets wrong
	// are paid for by the ordinary byte correction instead.
	NoAddends bool
}

// flags packs the optional trailing flag word. Zero means the word is absent,
// which keeps every plan written before the flags existed valid and
// byte-identical.
func (p relocPlan) flags() uint64 {
	var f uint64
	if p.PairByRow {
		f |= relocFlagPairByRow
	}
	if p.NoAddends {
		f |= relocFlagNoAddends
	}
	return f
}

func (p relocPlan) marshal() []byte {
	var b []byte
	for _, v := range []uint64{p.OldOff, p.OldSize, p.NewOff, p.NewSize, p.RelCount, p.TailCount, p.Anchor} {
		b = appendU(b, v)
	}
	b = p.OldSecs.marshal(b)
	b = p.NewSecs.marshal(b)
	b = appendStream(b, p.GapCorrection)
	b = appendStream(b, p.AddendCorrection)
	b = appendStream(b, p.TailCorrection)
	if f := p.flags(); f != 0 {
		b = appendU(b, f)
	}
	return b
}

const (
	relocFlagPairByRow = 1
	relocFlagNoAddends = 2
	relocFlagsKnown    = relocFlagPairByRow | relocFlagNoAddends
)

func unmarshalRelocPlan(b []byte) (relocPlan, error) {
	r := &planReader{b: b}
	p := relocPlan{OldOff: r.u(), OldSize: r.u(), NewOff: r.u(), NewSize: r.u(),
		RelCount: r.u(), TailCount: r.u(), Anchor: r.u()}
	var err error
	if p.OldSecs, err = unmarshalSectionMap(r); err != nil {
		return relocPlan{}, err
	}
	if p.NewSecs, err = unmarshalSectionMap(r); err != nil {
		return relocPlan{}, err
	}
	gap, addend, tail := r.stream(), r.stream(), r.stream()
	if len(r.b) != 0 {
		flags := r.u()
		if flags&^uint64(relocFlagsKnown) != 0 {
			return relocPlan{}, errors.New("unknown relocation plan flag")
		}
		p.PairByRow = flags&relocFlagPairByRow != 0
		p.NoAddends = flags&relocFlagNoAddends != 0
	}
	if r.err != nil || len(r.b) != 0 {
		return relocPlan{}, errors.New("trailing or invalid relocation plan data")
	}
	p.GapCorrection, p.AddendCorrection, p.TailCorrection = slices.Clone(gap.b), slices.Clone(addend.b), slices.Clone(tail.b)
	return p, nil
}

type relocStats struct {
	Entries          int `json:"entries"`
	RelativeEntries  int `json:"relative_entries"`
	AddendsProjected int `json:"addends_projected"`
	GapPlanBytes     int `json:"gap_correction_bytes"`
	AddendPlanBytes  int `json:"addend_correction_bytes"`
	TailPlanBytes    int `json:"tail_correction_bytes"`
}

type relaEntry struct{ slot, info, addend uint64 }

func parseRela(b []byte) (rel, tail []relaEntry) {
	for i := 0; i+relaEntrySize <= len(b); i += relaEntrySize {
		e := relaEntry{
			slot:   binary.LittleEndian.Uint64(b[i:]),
			info:   binary.LittleEndian.Uint64(b[i+8:]),
			addend: binary.LittleEndian.Uint64(b[i+16:]),
		}
		if e.info&0xffffffff == relTypeRelative {
			rel = append(rel, e)
		} else {
			tail = append(tail, e)
		}
	}
	return rel, tail
}

func putU64Col(col []byte, i int, v uint64) {
	if (i+1)*8 <= len(col) {
		binary.LittleEndian.PutUint64(col[i*8:], v)
	}
}

// Encoder and decoder build these columns from the same inputs, so a
// correction derived on one side applies on the other.
//
// relocGapColumn projects the old relative entries and lays their slot gaps
// out in old order. Slots are projected but deliberately NOT re-sorted:
// re-sorting by the projected slot looks right, since the linker emits
// R_X86_64_RELATIVE sorted by slot, but the projected column is already
// 99.93% ordered (809 inversions in 1,105,974 entries), so a sort has almost
// nothing to gain and loses on ties. Measured, it costs 718,543 B against
// 644,075 B for old order.
func relocGapColumn(old []byte, p relocPlan, lookup func(uint64) x86.Target) (gap []byte, proj []relaEntry, projected int) {
	oldRel, _ := parseRela(old[p.OldOff : p.OldOff+p.OldSize])
	gap = make([]byte, p.RelCount*8)
	project := func(v uint64) uint64 {
		if v == 0 {
			return 0
		}
		if t := lookup(v); t.Known {
			projected++
			return t.Addr
		}
		return v
	}
	proj = make([]relaEntry, len(oldRel))
	slots := make([]uint64, len(oldRel))
	for i, e := range oldRel {
		proj[i] = relaEntry{slot: project(e.slot), addend: project(e.addend)}
		slots[i] = proj[i].slot
	}
	// The slots are deliberately left in old order. Sorting them looks right
	// -- the new table is sorted, so a gap is only meaningful between
	// neighbours in that order -- and measures worse: 78.583% of gaps exact
	// and 718,543 B of correction sorted, against 80.755% and 646,192 B
	// unsorted. The projected column is already 99.93% ordered (809 inversions
	// in 1,105,974 entries), so sorting corrects little and displaces the
	// indices in between, which is what the positional comparison charges for.
	for i := range slots {
		if uint64(i) >= p.RelCount {
			break
		}
		if i > 0 {
			putU64Col(gap, i, slots[i]-slots[i-1])
		}
	}
	return gap, proj, projected
}

// relocAddendColumn places projected addends by *joining on the slot* rather
// than by position. The decoder can do this because it corrects the slot gaps
// first, which is cheap (81 KB compressed), and the exact new slots that fall
// out are the join key. Positional placement is what made this column
// expensive: an entry whose slot moves past others lands at a different index
// in the linker-sorted new table, so every addend after it is compared against
// the wrong entry.
func relocAddendColumn(proj []relaEntry, slots []uint64) []byte {
	bySlot := make(map[uint64]uint64, len(proj))
	for _, e := range proj {
		if _, seen := bySlot[e.slot]; !seen {
			bySlot[e.slot] = e.addend
		}
	}
	addend := make([]byte, len(slots)*8)
	for i, slot := range slots {
		v, ok := bySlot[slot]
		if !ok && i < len(proj) {
			// No entry projects here; the old-order value is the best guess
			// left, and the correction fixes what it gets wrong.
			v = proj[i].addend
		}
		putU64Col(addend, i, v)
	}
	return addend
}

// relocAddendByRow is the positional pairing the slot join replaced: new entry
// i takes the projected addend of old entry i. It is kept as a measurable
// option because it is what Zucchini-style row pairing does, and the gap
// between the two numbers is the whole argument of §3.3. Where the new table
// is longer than the old the surplus rows predict zero; where it is shorter
// the surplus old rows are dropped.
func relocAddendByRow(proj []relaEntry, n int) []byte {
	addend := make([]byte, n*8)
	for i := 0; i < n && i < len(proj); i++ {
		putU64Col(addend, i, proj[i].addend)
	}
	return addend
}

// relocAddendPrediction is the addend column both sides build, chosen by the
// plan's flags. A NoAddends plan has no addend column at all, and returns nil:
// the decoder then leaves the addend bytes of every entry as it found them.
func relocAddendPrediction(p relocPlan, proj []relaEntry, gap []byte) []byte {
	switch {
	case p.NoAddends:
		return nil
	case p.PairByRow:
		return relocAddendByRow(proj, int(p.RelCount))
	default:
		return relocAddendColumn(proj, slotsFromGaps(gap, p.Anchor, p.RelCount))
	}
}

func relocTailColumn(old []byte, p relocPlan, lookup func(uint64) x86.Target) []byte {
	_, oldTail := parseRela(old[p.OldOff : p.OldOff+p.OldSize])
	project := func(v uint64) uint64 {
		if v == 0 {
			return 0
		}
		if t := lookup(v); t.Known {
			return t.Addr
		}
		return v
	}
	tail := make([]byte, p.TailCount*relaEntrySize)
	for i, e := range oldTail {
		if uint64(i) >= p.TailCount {
			break
		}
		o := tail[i*relaEntrySize:]
		binary.LittleEndian.PutUint64(o, project(e.slot))
		binary.LittleEndian.PutUint64(o[8:], e.info)
		binary.LittleEndian.PutUint64(o[16:], project(e.addend))
	}
	return tail
}

// slotsFromGaps rebuilds the absolute slot list from the corrected gap column.
func slotsFromGaps(gap []byte, anchor, relCount uint64) []uint64 {
	slots := make([]uint64, relCount)
	slot := anchor
	for i := uint64(0); i < relCount; i++ {
		if i > 0 {
			slot += binary.LittleEndian.Uint64(gap[i*8:])
		}
		slots[i] = slot
	}
	return slots
}

// relocDiag receives the per-column diagnostics. buildRungPlans points it at
// the notes buffer of the rung-plan build, so that a memo hit reprints them
// rather than silently dropping them.
var relocDiag = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format, args...) }

// relocCodecStats separates the two candidate causes of an expensive column
// correction: a projection that gets the values wrong, or a byte-oriented
// correction codec applied to a column of 8-byte values, where every value
// differing in its low bytes is the worst case for a byte matcher. It reports
// how many projected values are already exact, and what the same column costs
// encoded as a numeric residual instead.
func relocCodecStats(name string, got, want, correction []byte) {
	n := len(want) / 8
	if n == 0 {
		return
	}
	var exact int
	var resid []byte
	for i := 0; i < n; i++ {
		w := binary.LittleEndian.Uint64(want[i*8:])
		var g uint64
		if (i+1)*8 <= len(got) {
			g = binary.LittleEndian.Uint64(got[i*8:])
		}
		if g == w {
			exact++
		}
		resid = binary.AppendVarint(resid, int64(w-g))
	}
	if onlyProbes == nil {
		relocDiag("  %s column: %d values, %d exact (%.3f%%); correction %d B -> xz %d; numeric residual %d B -> xz %d\n",
			name, n, exact, 100*float64(exact)/float64(n),
			len(correction), xzSize(correction), len(resid), xzSize(resid))
	}
}

func assembleRela(dst, gap, addend, tail []byte, anchor uint64, relCount uint64) {
	slot := anchor
	for i := uint64(0); i < relCount; i++ {
		if i > 0 {
			slot += binary.LittleEndian.Uint64(gap[i*8:])
		}
		o := dst[i*relaEntrySize:]
		binary.LittleEndian.PutUint64(o, slot)
		binary.LittleEndian.PutUint64(o[8:], relTypeRelative)
		// A nil column means the plan says nothing about addends, so the bytes
		// the prediction already holds are left in place.
		if addend != nil {
			binary.LittleEndian.PutUint64(o[16:], binary.LittleEndian.Uint64(addend[i*8:]))
		}
	}
	copy(dst[relCount*relaEntrySize:], tail)
}

// applyReloc is the decoder side: predict the columns, apply the shipped
// corrections, reassemble.
func applyReloc(out, old []byte, p relocPlan, lookup func(uint64) x86.Target) (relocStats, error) {
	gap, proj, projected := relocGapColumn(old, p, lookup)
	if err := delta.ApplyCorrection(gap, p.GapCorrection); err != nil {
		return relocStats{}, fmt.Errorf("relocation gap column: %w", err)
	}
	// The corrected gaps give the exact new slots, which are the join key for
	// the addend column.
	addend := relocAddendPrediction(p, proj, gap)
	if addend != nil {
		if err := delta.ApplyCorrection(addend, p.AddendCorrection); err != nil {
			return relocStats{}, fmt.Errorf("relocation addend column: %w", err)
		}
	}
	tail := relocTailColumn(old, p, lookup)
	if err := delta.ApplyCorrection(tail, p.TailCorrection); err != nil {
		return relocStats{}, fmt.Errorf("relocation tail: %w", err)
	}
	if (p.RelCount+p.TailCount)*relaEntrySize > p.NewSize {
		return relocStats{}, errors.New("relocation counts exceed the new section")
	}
	assembleRela(out[p.NewOff:p.NewOff+p.NewSize], gap, addend, tail, p.Anchor, p.RelCount)
	return relocStats{
		Entries: int(p.RelCount + p.TailCount), RelativeEntries: int(p.RelCount),
		AddendsProjected: projected,
		GapPlanBytes:     len(p.GapCorrection), AddendPlanBytes: len(p.AddendCorrection),
		TailPlanBytes: len(p.TailCorrection),
	}, nil
}

// buildRelocPlan is the encoder side. It sees the new table, derives the three
// column corrections, and returns a plan the decoder can replay.
func buildRelocPlan(old, target []byte, base relocPlan, lookup func(uint64) x86.Target) (relocPlan, error) {
	newRel, newTail := parseRela(target[base.NewOff : base.NewOff+base.NewSize])
	if len(newRel) == 0 {
		return relocPlan{}, errors.New("new relocation table has no relative entries")
	}
	p := base
	p.RelCount, p.TailCount, p.Anchor = uint64(len(newRel)), uint64(len(newTail)), newRel[0].slot

	wantGap := make([]byte, p.RelCount*8)
	wantAddend := make([]byte, p.RelCount*8)
	for i, e := range newRel {
		if i > 0 {
			putU64Col(wantGap, i, e.slot-newRel[i-1].slot)
		}
		putU64Col(wantAddend, i, e.addend)
	}
	wantTail := make([]byte, p.TailCount*relaEntrySize)
	for i, e := range newTail {
		o := wantTail[i*relaEntrySize:]
		binary.LittleEndian.PutUint64(o, e.slot)
		binary.LittleEndian.PutUint64(o[8:], e.info)
		binary.LittleEndian.PutUint64(o[16:], e.addend)
	}

	gap, proj, _ := relocGapColumn(old, p, lookup)
	var err error
	if p.GapCorrection, err = delta.EncodeCorrection(gap, wantGap); err != nil {
		return relocPlan{}, err
	}
	relocCodecStats("gap", gap, wantGap, p.GapCorrection)
	// The gap correction is exact, so the decoder's join key is wantGap.
	if addend := relocAddendPrediction(p, proj, wantGap); addend != nil {
		if p.AddendCorrection, err = delta.EncodeCorrection(addend, wantAddend); err != nil {
			return relocPlan{}, err
		}
		relocCodecStats("addend", addend, wantAddend, p.AddendCorrection)
	}
	tail := relocTailColumn(old, p, lookup)
	if p.TailCorrection, err = delta.EncodeCorrection(tail, wantTail); err != nil {
		return relocPlan{}, err
	}
	return p, nil
}
