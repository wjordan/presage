package elfmod

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/delta/x86"
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
	// HeadCount is how many of the other-type entries precede the relative
	// block: zero for ld.bfd and lld, which sort them after it, but Go's
	// linker writes its GLOB_DAT entries first. Carried behind a flag so a
	// plan without a head is byte-identical to one from before it existed.
	HeadCount uint64
}

// flags packs the optional trailing flag word. Zero means the word is absent,
// which keeps every plan written before the flags existed valid and
// byte-identical.
func (p relocPlan) flags() uint64 {
	if p.HeadCount != 0 {
		return relocFlagHead
	}
	return 0
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
		if p.HeadCount != 0 {
			b = appendU(b, p.HeadCount)
		}
	}
	return b
}

// relocFlagHead is the only flag the module writes. The harness's
// experiment flags (pair-by-row, no-addends, derived geometry) held bits 1,
// 2 and 8 and are rejected here as unknown.
const (
	relocFlagHead   = 4
	relocFlagsKnown = relocFlagHead
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
		if flags&relocFlagHead != 0 {
			p.HeadCount = r.u()
		}
	}
	if p.HeadCount > p.TailCount {
		return relocPlan{}, errors.New("relocation head exceeds the other-type entries")
	}
	if r.err != nil || len(r.b) != 0 {
		return relocPlan{}, errors.New("trailing or invalid relocation plan data")
	}
	p.GapCorrection, p.AddendCorrection, p.TailCorrection = slices.Clone(gap.b), slices.Clone(addend.b), slices.Clone(tail.b)
	return p, nil
}

type relocStats struct {
	Entries          int
	RelativeEntries  int
	AddendsProjected int
	GapPlanBytes     int
	AddendPlanBytes  int
	TailPlanBytes    int
}

type relaEntry struct{ slot, info, addend uint64 }

// parseRela splits a table into its R_X86_64_RELATIVE entries and the rest,
// each in table order, and counts how many of the rest come before the
// first relative entry.
//
// The table is parsed up to three times per prediction -- twice by the
// relocation columns and once by the derived enumeration's E2 -- always from
// the same bytes of the same image. A million entries of appends is worth
// doing once, so the result is memoised on the identity of the slice it was
// given (memo.go). Every caller only reads what it gets back.
type relaParse struct {
	b         []byte // retained, so the key's buffer identity stays meaningful
	rel, tail []relaEntry
	head      int
}

var (
	relaMu    sync.Mutex
	relaCache = map[bufID]relaParse{}
)

func parseRela(b []byte) (rel, tail []relaEntry, head int) {
	key := idOf(b)
	relaMu.Lock()
	e, ok := relaCache[key]
	relaMu.Unlock()
	if ok {
		return e.rel, e.tail, e.head
	}
	rel, tail, head = parseRelaUncached(b)
	relaMu.Lock()
	if len(relaCache) > 4 {
		clear(relaCache)
	}
	relaCache[key] = relaParse{b: b, rel: rel, tail: tail, head: head}
	relaMu.Unlock()
	return rel, tail, head
}

func parseRelaUncached(b []byte) (rel, tail []relaEntry, head int) {
	for i := 0; i+relaEntrySize <= len(b); i += relaEntrySize {
		e := relaEntry{
			slot:   binary.LittleEndian.Uint64(b[i:]),
			info:   binary.LittleEndian.Uint64(b[i+8:]),
			addend: binary.LittleEndian.Uint64(b[i+16:]),
		}
		if e.info&0xffffffff == relTypeRelative {
			rel = append(rel, e)
		} else {
			if len(rel) == 0 {
				head++
			}
			tail = append(tail, e)
		}
	}
	return rel, tail, head
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
	oldRel, _, _ := parseRela(old[p.OldOff : p.OldOff+p.OldSize])
	gap = make([]byte, p.RelCount*8)
	// Each entry projects on its own -- the oracle is a read-only lookup over
	// finished tables -- so the million-entry projection runs in shards. The
	// hit count is summed per shard so it cannot depend on the split.
	proj = make([]relaEntry, len(oldRel))
	slots := make([]uint64, len(oldRel))
	hits := shardRange(len(oldRel), func(lo, hi int) int {
		n := 0
		count := func(v uint64) uint64 {
			if v == 0 {
				return 0
			}
			if t := lookup(v); t.Known {
				n++
				return t.Addr
			}
			return v
		}
		for i := lo; i < hi; i++ {
			e := oldRel[i]
			proj[i] = relaEntry{slot: count(e.slot), addend: count(e.addend)}
			slots[i] = proj[i].slot
		}
		return n
	})
	for _, n := range hits {
		projected += n
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

// relocAddendPrediction is the addend column both sides build.
func relocAddendPrediction(p relocPlan, proj []relaEntry, gap []byte) []byte {
	return relocAddendColumn(proj, slotsFromGaps(gap, p.Anchor, p.RelCount))
}

func relocTailColumn(old []byte, p relocPlan, lookup func(uint64) x86.Target) []byte {
	_, oldTail, _ := parseRela(old[p.OldOff : p.OldOff+p.OldSize])
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
	n := min(len(oldTail), int(p.TailCount))
	eachRange(n, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			e := oldTail[i]
			o := tail[i*relaEntrySize:]
			binary.LittleEndian.PutUint64(o, project(e.slot))
			binary.LittleEndian.PutUint64(o[8:], e.info)
			binary.LittleEndian.PutUint64(o[16:], project(e.addend))
		}
	})
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

func assembleRela(dst, gap, addend, tail []byte, anchor uint64, relCount, headCount uint64) {
	head := headCount * relaEntrySize
	copy(dst, tail[:head])
	tail = tail[head:]
	dst = dst[head:]
	// The slot column is a running sum of the gaps, so a shard starting at lo
	// needs the sum of gaps [1,lo). Those partial sums are cheap next to the
	// three stores per entry, and computing them per shard leaves each shard
	// writing a disjoint run of records.
	slots := slotsFromGaps(gap, anchor, relCount)
	eachRange(int(relCount), func(lo, hi int) {
		for i := lo; i < hi; i++ {
			o := dst[uint64(i)*relaEntrySize:]
			binary.LittleEndian.PutUint64(o, slots[i])
			binary.LittleEndian.PutUint64(o[8:], relTypeRelative)
			binary.LittleEndian.PutUint64(o[16:], binary.LittleEndian.Uint64(addend[i*8:]))
		}
	})
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
	if err := delta.ApplyCorrection(addend, p.AddendCorrection); err != nil {
		return relocStats{}, fmt.Errorf("relocation addend column: %w", err)
	}
	tail := relocTailColumn(old, p, lookup)
	if err := delta.ApplyCorrection(tail, p.TailCorrection); err != nil {
		return relocStats{}, fmt.Errorf("relocation tail: %w", err)
	}
	if (p.RelCount+p.TailCount)*relaEntrySize > p.NewSize {
		return relocStats{}, errors.New("relocation counts exceed the new section")
	}
	assembleRela(out[p.NewOff:p.NewOff+p.NewSize], gap, addend, tail, p.Anchor, p.RelCount, p.HeadCount)
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
	newRel, newTail, head := parseRela(target[base.NewOff : base.NewOff+base.NewSize])
	if len(newRel) == 0 {
		return relocPlan{}, errors.New("new relocation table has no relative entries")
	}
	p := base
	p.RelCount, p.TailCount, p.Anchor, p.HeadCount = uint64(len(newRel)), uint64(len(newTail)), newRel[0].slot, uint64(head)

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
	// The gap correction is exact, so the decoder's join key is wantGap.
	addend := relocAddendPrediction(p, proj, wantGap)
	if p.AddendCorrection, err = delta.EncodeCorrection(addend, wantAddend); err != nil {
		return relocPlan{}, err
	}
	tail := relocTailColumn(old, p, lookup)
	if p.TailCorrection, err = delta.EncodeCorrection(tail, wantTail); err != nil {
		return relocPlan{}, err
	}
	return p, nil
}
