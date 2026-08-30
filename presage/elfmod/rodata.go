package elfmod

import (
	"encoding/binary"
	"errors"
	"slices"

	"github.com/wjordan/presage/delta/x86"
)

// .rodata is the largest non-.text cost at 409,800 XZ, and the shape of its
// error says what it is: of 71,494 wrong runs, 66,550 are four bytes or
// shorter and 54,849 start on a four-byte boundary. That is an array of 32-bit
// values churning like every other address column here.
//
// The values are clang's switch tables. Each entry holds its target minus the
// address of the table, so nothing relocates them -- they are self-relative --
// and the byte matcher has no way to know they are addresses at all. They are
// found by their signature instead: a run of entries that all land inside
// .text when added to the address of the run's first entry.

// A run of four is the length at which a false positive becomes unlikely by
// chance: .text is 77% of the image, so a random word lands in it about 5% of
// the time and four in a row about one time in 160,000. Shorter runs are real
// tables too -- small switches are the common case -- but they cannot be
// identified by the signature alone, so the encoder verifies them against the
// target and ships one bit each. See selectRoDataTables.
const (
	jumpTableMinRun   = 4
	jumpTableShortRun = 2
)

type roDataPlan struct {
	OldOff, OldSize  uint64
	NewOff, NewSize  uint64
	OldAddr, NewAddr uint64
	TextLo, TextHi   uint64 // old .text address range, the target test
	// Keep holds one bit per candidate, in the order roDataCandidates lists
	// them, saying the encoder checked that candidate against the target and
	// it made the prediction better. Every candidate is verified, long runs
	// included: a run that starts one word before a real table reads every
	// entry against the wrong base, and the signature cannot tell.
	Keep []byte
}

func (p roDataPlan) marshal() []byte {
	var b []byte
	for _, v := range []uint64{p.OldOff, p.OldSize, p.NewOff, p.NewSize, p.OldAddr, p.NewAddr, p.TextLo, p.TextHi} {
		b = binary.AppendUvarint(b, v)
	}
	b = binary.AppendUvarint(b, uint64(len(p.Keep)))
	return append(b, p.Keep...)
}

func unmarshalRoDataPlan(b []byte) (roDataPlan, error) {
	r := &planReader{b: b}
	p := roDataPlan{
		OldOff: r.u(), OldSize: r.u(), NewOff: r.u(), NewSize: r.u(),
		OldAddr: r.u(), NewAddr: r.u(), TextLo: r.u(), TextHi: r.u(),
	}
	n := r.u()
	if r.err != nil || n > uint64(len(r.b)) {
		return roDataPlan{}, errors.New("invalid rodata plan")
	}
	p.Keep = r.b[:n]
	return p, nil
}

type roDataStats struct {
	Candidates int
	Tables     int
	SelfRel    int
	Rebased    int
	Entries    int
	Retargeted int
	Unresolved int
	Unplaced   int
}

// jumpTables lists the switch tables in an exact .rodata image. Scanning left
// to right and consuming each run keeps the base honest: an entry list read
// from the wrong starting word would still land in .text, because .text is
// large, so the first position that begins a long run is taken as the base.
func jumpTables(sec []byte, secAddr, textLo, textHi uint64, minRun int) [][2]int {
	n := len(sec) / 4
	var out [][2]int
	for i := 0; i < n; {
		if j := runEnd(sec, secAddr, textLo, textHi, i, n, i); j-i >= minRun {
			out = append(out, [2]int{i, j})
			i = j
			continue
		}
		i++
	}
	return out
}

// runEnd reports how far the run starting at word i reaches, reading each
// entry relative to word base and stopping at limit.
func runEnd(sec []byte, secAddr, textLo, textHi uint64, i, limit, base int) int {
	baseAddr := secAddr + uint64(base*4)
	j := i
	for j < limit {
		v := int32(binary.LittleEndian.Uint32(sec[j*4:]))
		if t := uint64(int64(baseAddr) + int64(v)); t < textLo || t >= textHi {
			break
		}
		j++
	}
	return j
}

// shortCandidates finds the two- and three-word runs the long scan left over.
// It searches only the space between accepted tables, so adding it cannot
// re-segment a table the long scan already found -- a run that began one word
// early would take the whole table with it and read every entry against the
// wrong base.
func shortCandidates(sec []byte, secAddr, textLo, textHi uint64, long [][2]int) [][2]int {
	n := len(sec) / 4
	var out [][2]int
	at := 0
	for k := 0; k <= len(long); k++ {
		end := n
		if k < len(long) {
			end = long[k][0]
		}
		for i := at; i < end; {
			if j := runEnd(sec, secAddr, textLo, textHi, i, end, i); j-i >= jumpTableShortRun {
				out = append(out, [2]int{i, j})
				i = j
				continue
			}
			i++
		}
		if k < len(long) {
			at = long[k][1]
		}
	}
	return out
}

// roDataCandidate is one run the decoder will consider, with the convention
// its entries are read under. Both conventions occur -- a compiler switch
// table is relative to the table base, a generated table of relative pointers
// is usually relative to each entry's own address -- and for a short table
// they are indistinguishable by signature, because the two interpretations
// differ only by the entry's offset within the table and both land in .text.
// So the scan finds spans and the encoder chooses the convention.
type roDataCandidate struct {
	Span    [2]int
	SelfRel bool
}

// roDataBaseShifts is how many origins are offered per span. The scan starts a
// run at the first word that begins one, which can be earlier than the table
// itself: a junk word before a real table still lands in .text under its own
// base, and the table's entries then land in .text under that base too,
// because they miss their true targets by only the shift. Reading a table one
// word early gives every entry the wrong origin, which is why 7,282 of the
// 10,837 runs found by signature alone made the prediction worse.
const roDataBaseShifts = 4

// roDataSpans enumerates every run both sides can find from the old section
// alone, in a fixed order so a fixed number of bits per entry addresses them.
func roDataSpans(sec []byte, secAddr, textLo, textHi uint64) [][2]int {
	long := jumpTables(sec, secAddr, textLo, textHi, jumpTableMinRun)
	spans := append(long, shortCandidates(sec, secAddr, textLo, textHi, long)...)
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	return spans
}

// spanVariants lists what the encoder may choose from for one span: each
// origin from the run's start up to three words in, under each convention.
func spanVariants(span [2]int) []roDataCandidate {
	out := make([]roDataCandidate, 0, 2*roDataBaseShifts)
	for shift := 0; shift < roDataBaseShifts; shift++ {
		if span[0]+shift >= span[1] {
			break
		}
		for _, self := range []bool{false, true} {
			out = append(out, roDataCandidate{Span: [2]int{span[0] + shift, span[1]}, SelfRel: self})
		}
	}
	return out
}

func bitSet(b []byte, i int) bool { return i/8 < len(b) && b[i/8]&(1<<(i%8)) != 0 }

// spanBit is where the encoder's decision for variant v of span i lives. The
// stride is fixed rather than packed so that a span near the section end,
// which offers fewer origins, does not shift every later span's bits.
func spanBit(i, v int) int { return i*2*roDataBaseShifts + v }

// retargetTable computes what one candidate table should hold in the new
// image. It drives from the old section, which is exact, and projects each
// entry forward to wherever the equivalence map put those bytes.
func retargetTable(old []byte, p roDataPlan, c roDataCandidate, mapper sourceEquivalenceMapper, lookup func(uint64) x86.Target, st *roDataStats, visit func(off uint64, v uint32)) {
	sec := old[p.OldOff : p.OldOff+p.OldSize]
	tb := c.Span
	newBaseOff, ok := mapper.project(p.OldOff + uint64(tb[0]*4))
	if !ok || newBaseOff < p.NewOff || newBaseOff >= p.NewOff+p.NewSize {
		st.Unplaced += tb[1] - tb[0]
		return
	}
	newBaseAddr := p.NewAddr + (newBaseOff - p.NewOff)
	for k := tb[0]; k < tb[1]; k++ {
		st.Entries++
		newOff, ok := mapper.project(p.OldOff + uint64(k*4))
		if !ok || newOff < p.NewOff || newOff+4 > p.NewOff+p.NewSize {
			st.Unplaced++
			continue
		}
		// The entry is relative to the table base, or to itself.
		oldRef, newRef := p.OldAddr+uint64(tb[0]*4), newBaseAddr
		if c.SelfRel {
			oldRef, newRef = p.OldAddr+uint64(k*4), p.NewAddr+(newOff-p.NewOff)
		}
		v := int32(binary.LittleEndian.Uint32(sec[k*4:]))
		t := lookup(uint64(int64(oldRef) + int64(v)))
		if !t.Known {
			st.Unresolved++
			continue
		}
		st.Retargeted++
		visit(newOff, uint32(int32(int64(t.Addr)-int64(newRef))))
	}
}

// applyRoData retargets the tables the encoder vouched for.
func applyRoData(out, old []byte, p roDataPlan, mapper sourceEquivalenceMapper, lookup func(uint64) x86.Target) roDataStats {
	var st roDataStats
	sec := old[p.OldOff : p.OldOff+p.OldSize]
	write := func(off uint64, v uint32) { binary.LittleEndian.PutUint32(out[off:], v) }
	for i, span := range roDataSpans(sec, p.OldAddr, p.TextLo, p.TextHi) {
		st.Candidates++
		for v, c := range spanVariants(span) {
			if !bitSet(p.Keep, spanBit(i, v)) {
				continue
			}
			st.Tables++
			if c.SelfRel {
				st.SelfRel++
			}
			if c.Span[0] > span[0] {
				st.Rebased++
			}
			retargetTable(old, p, c, mapper, lookup, &st, write)
			break
		}
	}
	return st
}

// selectRoDataTables is the encoder half: it scores every variant of every
// span against the target and keeps the best one, or none. A run that lands in
// .text by chance, and a real table read one word early, are both rejected
// here rather than paid for in the correction.
func selectRoDataTables(pred, old, target []byte, p roDataPlan, mapper sourceEquivalenceMapper, lookup func(uint64) x86.Target) ([]byte, roDataStats) {
	sec := old[p.OldOff : p.OldOff+p.OldSize]
	spans := roDataSpans(sec, p.OldAddr, p.TextLo, p.TextHi)
	keep := make([]byte, (2*roDataBaseShifts*len(spans)+7)/8)
	var st roDataStats
	st.Candidates = len(spans)
	for i, span := range spans {
		bestGain, bestV := 0, -1
		var best roDataCandidate
		for v, c := range spanVariants(span) {
			var ignored roDataStats
			var wrongBefore, wrongAfter int
			retargetTable(old, p, c, mapper, lookup, &ignored, func(off uint64, val uint32) {
				var buf [4]byte
				binary.LittleEndian.PutUint32(buf[:], val)
				for k := 0; k < 4; k++ {
					if pred[off+uint64(k)] != target[off+uint64(k)] {
						wrongBefore++
					}
					if buf[k] != target[off+uint64(k)] {
						wrongAfter++
					}
				}
			})
			if gain := wrongBefore - wrongAfter; gain > bestGain {
				bestGain, bestV, best = gain, v, c
			}
		}
		if bestV < 0 {
			continue
		}
		bit := spanBit(i, bestV)
		keep[bit/8] |= 1 << (bit % 8)
		st.Tables++
		if best.SelfRel {
			st.SelfRel++
		}
		if best.Span[0] > span[0] {
			st.Rebased++
		}
	}
	return keep, st
}
