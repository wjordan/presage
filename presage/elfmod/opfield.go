package elfmod

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"

	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/internal/cz"
	"github.com/wjordan/presage/internal/trace"
)

// The field fix (fieldfix.go) corrects the four-byte PC-relative
// displacements, because those are the fields the retargeter writes. The rest
// of the operand it leaves alone: an instruction the prediction placed right
// and filled in wrong -- a frame offset that moved by eight, an immediate that
// changed -- reaches the residual as literal bytes when only the difference
// was needed.
//
// This layer is the same trade over the scalar operand fields. Its domain is
// every instruction the length decoder finds in a mapped body of the
// prediction, split into one list per field class; an entry is an index into
// one of those lists and the signed change to the field's value. The decoder
// holds the prediction and the map, so it enumerates the same lists, and
// instruction lengths do not depend on field values, so the lists are the same
// before and after the layer rewrites one.
//
// Only the scalar classes are here. The register and structural corrections
// the same domain could carry are a measured loss (chrome-elf-whole-image.md
// §11.4): a modrm byte three bits from the prediction's is exactly what the
// residual coder's prediction context already codes cheaply, and a column
// replacing it would have no prediction under it.

// The field classes. An instruction contributes at most one field to one
// immediate class and one to one displacement class; the class is what the
// values in a column have in common, which is why each has its own.
const (
	opImm       = iota // an immediate that is not a branch displacement
	opRel              // a branch displacement
	opDispSP           // a displacement based on rsp or rbp: a frame offset
	opDispOther        // one based on any other register
	opDispRIP          // a PC-relative displacement
	opDispAbs          // one based on nothing: the field is the address
	nOpClass
)

var opDispClass = [...]int{
	x86.BaseAbs:   opDispAbs,
	x86.BaseRIP:   opDispRIP,
	x86.BaseSP:    opDispSP,
	x86.BaseOther: opDispOther,
}

// opField is one located field of one instruction, at an offset from the
// instruction's first byte.
type opField struct {
	class, off, width int
}

// opFieldsOf lists an instruction's scalar fields, immediate first. buf is the
// caller's, so the walk allocates nothing.
func opFieldsOf(f x86.Fields, buf []opField) []opField {
	buf = buf[:0]
	if f.ImmLen != 0 {
		c := opImm
		if f.Rel {
			c = opRel
		}
		buf = append(buf, opField{c, f.ImmOff, f.ImmLen})
	}
	if f.DispLen != 0 {
		buf = append(buf, opField{opDispClass[f.Base], f.DispOff, f.DispLen})
	}
	return buf
}

// opWalkBody visits the scalar fields of every instruction of one body, in
// order. It reads the prediction alone, so both sides see the same sequence;
// an instruction the length tables do not vouch for has no fields and so
// belongs to no domain.
func opWalkBody(code []byte, visit func(start, length int, fields []opField)) {
	var buf [2]opField
	x86.WalkFields(code, func(start int, f x86.Fields, ok bool) {
		if !ok {
			return
		}
		if fs := opFieldsOf(f, buf[:0]); len(fs) != 0 {
			visit(start, f.Len, fs)
		}
	})
}

// opBody is one mapped function body of the prediction, with the index its
// first field of each class has in that class's domain.
type opBody struct {
	off, size int32
	base      [nOpClass]int32
}

// opBodies projects the function map into the bodies this layer enumerates:
// destination order, overlaps dropped, so an instruction belongs to exactly
// one body on both sides. It returns the per-class domain sizes; each body's
// bases are the prefix sums, so a body can be walked on its own.
func opBodies(text []byte, maps []mapping, w *textWalk) ([]opBody, [nOpClass]int) {
	var total [nOpClass]int
	if len(text) > math.MaxInt32 {
		return nil, total
	}
	if w == nil {
		w = newTextWalk(text, maps, false, true)
	}
	// cand carries the mapping index through the sort, so each kept body can
	// read the counts the shared walk left for it.
	type cand struct{ off, size, k int32 }
	cands := make([]cand, 0, len(maps))
	for k, m := range maps {
		if m.DstSize == 0 || m.Dst+m.DstSize > uint64(len(text)) {
			continue
		}
		cands = append(cands, cand{int32(m.Dst), int32(m.DstSize), int32(k)})
	}
	slices.SortFunc(cands, func(a, b cand) int { return cmp.Compare(a.off, b.off) })
	bodies, end := make([]opBody, 0, len(cands)), int32(0)
	for _, c := range cands {
		if c.off < end {
			continue
		}
		b := opBody{off: c.off, size: c.size}
		if w.counts != nil {
			copy(b.base[:], w.counts[int(c.k)*nOpClass:])
		}
		bodies = append(bodies, b)
		end = c.off + c.size
	}
	for i := range bodies {
		for c := range total {
			n := int(bodies[i].base[c])
			bodies[i].base[c] = int32(total[c])
			total[c] += n
		}
	}
	return bodies, total
}

// opEntry is one field correction before it is written to a column.
type opEntry struct {
	class uint8
	idx   int32 // index in that class's domain
	dv    int64 // the target's field value less the prediction's
	gain  int32 // wrong bytes the write removes
}

type opStats struct {
	Domain, Entries, Kept, Gain, Cost [nOpClass]int
}

func (s *opStats) add(o opStats) {
	for c := range s.Domain {
		s.Domain[c] += o.Domain[c]
		s.Entries[c] += o.Entries[c]
		s.Kept[c] += o.Kept[c]
		s.Gain[c] += o.Gain[c]
		s.Cost[c] += o.Cost[c]
	}
}

// totals sums the ledger over the classes that shipped.
func (s opStats) totals() (domain, entries, kept, gain, cost int) {
	for c := range s.Domain {
		domain += s.Domain[c]
		entries += s.Entries[c]
		kept += s.Kept[c]
		if s.Kept[c] != 0 {
			gain += s.Gain[c]
			cost += s.Cost[c]
		}
	}
	return
}

// encodeOpField builds the layer over one code window. text is the prediction
// and is left untouched; applyOpField reproduces the result from it and the
// returned plan.
func encodeOpField(text, want []byte, maps []mapping, w *textWalk) ([]byte, opStats) {
	var st opStats
	bodies, total := opBodies(text, maps, w)
	st.Domain = total
	if len(bodies) == 0 {
		return nil, st
	}
	parts := shardRange(len(bodies), func(lo, hi int) []opEntry {
		var out []opEntry
		var fixed [16]byte
		for i := lo; i < hi; i++ {
			b := bodies[i]
			pred, target := text[b.off:b.off+b.size], want[b.off:b.off+b.size]
			if bytes.Equal(pred, target) {
				continue
			}
			local := b.base
			opWalkBody(pred, func(start, length int, fields []opField) {
				s, e := start, start+length
				// An instruction is repairable only when writing the target's
				// field bytes into the prediction's instruction reproduces it
				// exactly: what is left over is a different instruction, not a
				// different value, and belongs to the residual.
				repairable := !bytes.Equal(pred[s:e], target[s:e])
				if repairable {
					copy(fixed[:length], pred[s:e])
					for _, f := range fields {
						copy(fixed[f.off:f.off+f.width], target[s+f.off:s+f.off+f.width])
					}
					repairable = bytes.Equal(fixed[:length], target[s:e])
				}
				for _, f := range fields {
					idx := local[f.class]
					local[f.class]++
					if !repairable {
						continue
					}
					p, w := pred[s+f.off:], target[s+f.off:]
					pv, wv := opValue(p, f.width), opValue(w, f.width)
					if pv == wv {
						continue
					}
					if f.class == opImm {
						// An immediate that changed rarely changed by a little
						// -- it is a constant, not an offset -- so the column
						// carries the new value, which is small and repeats,
						// rather than a difference as wide as the two.
						pv = 0
					}
					gain := 0
					for k := range f.width {
						if p[k] != w[k] {
							gain++
						}
					}
					out = append(out, opEntry{uint8(f.class), idx, wv - pv, int32(gain)})
				}
			})
		}
		return out
	})

	// One index column and one value column per class, in domain order: the
	// shards are contiguous ranges of bodies and a body's entries come out in
	// walk order, so concatenating them leaves each class ascending.
	var idxCol, valCol [nOpClass][]byte
	var prev [nOpClass]int32
	for _, part := range parts {
		for _, e := range part {
			c := e.class
			idxCol[c] = appendU(idxCol[c], uint64(e.idx-prev[c]))
			valCol[c] = appendS(valCol[c], e.dv)
			prev[c] = e.idx
			st.Entries[c]++
			st.Gain[c] += int(e.gain)
		}
	}

	// A class ships only where the column costs less than the wrong bytes it
	// takes out of the residual -- .rodata's rule for a cursor correction, with
	// the column priced the way it ships rather than raw, because a column of
	// varints under the plan's own contexts compresses to well under half what
	// the residual charges for the bytes it replaces. A class that does not
	// clear that bar is left to the correction, and a window where no class
	// does ships no plan at all.
	var mask byte
	for c := range idxCol {
		if len(idxCol[c]) == 0 {
			continue
		}
		st.Cost[c] = cz.SizeProxy(idxCol[c]) + cz.SizeProxy(valCol[c])
		if st.Gain[c] <= st.Cost[c] && !opForceAll {
			continue
		}
		mask |= 1 << c
		st.Kept[c] = st.Entries[c]
	}
	if mask == 0 {
		return nil, st
	}
	b := []byte{mask}
	for c := range idxCol {
		if mask&(1<<c) != 0 {
			b = appendStream(b, idxCol[c])
			b = appendStream(b, valCol[c])
		}
	}
	return b, st
}

// opRun is one decoded entry.
type opRun struct {
	idx int32
	dv  int64
}

// applyOpField replays the layer over a predicted code window.
func applyOpField(text []byte, maps []mapping, b []byte, w *textWalk) (opStats, error) {
	var st opStats
	r := &planReader{b: b}
	mask := r.byteAt()
	var cols [nOpClass][2][]byte
	for c := range cols {
		if mask&(1<<c) == 0 {
			continue
		}
		cols[c][0], cols[c][1] = r.stream().b, r.stream().b
	}
	if r.err != nil || !r.done() {
		return st, errors.New("invalid operand field plan")
	}
	doneB := trace.Stage("  opField/bodies")
	bodies, total := opBodies(text, maps, w)
	doneB()
	st.Domain = total
	var ents [nOpClass][]opRun
	for c := range cols {
		ir, vr := &planReader{b: cols[c][0]}, &planReader{b: cols[c][1]}
		at, first := int64(0), true
		for !ir.done() {
			gap, dv := ir.u(), vr.s()
			if ir.err != nil || vr.err != nil {
				return st, errors.New("invalid operand field entry")
			}
			if !first && gap == 0 {
				return st, errors.New("operand field entry repeats an index")
			}
			at += int64(gap)
			if at >= int64(total[c]) {
				return st, errors.New("operand field entry runs past its domain")
			}
			ents[c] = append(ents[c], opRun{int32(at), dv})
			first = false
		}
		if !vr.done() {
			return st, errors.New("trailing operand field values")
		}
		st.Entries[c] = len(ents[c])
	}

	// Bodies own disjoint bytes and the entry lists are read-only by here, so
	// the replay runs one shard of bodies per worker. A shard finds its own
	// starting cursor by index, because the bases ascend with the bodies -- and
	// the same ordering says whether a body holds an entry at all, from the
	// next body's bases alone. Nearly none do, and the ones that do not are
	// never walked.
	defer trace.Stage("  opField/replay")()
	applied := shardRange(len(bodies), func(lo, hi int) [nOpClass]int {
		var n [nOpClass]int
		if lo >= hi {
			return n
		}
		var cur [nOpClass]int
		for c := range cur {
			cur[c], _ = slices.BinarySearchFunc(ents[c], bodies[lo].base[c],
				func(e opRun, k int32) int { return cmp.Compare(e.idx, k) })
		}
		for i := lo; i < hi; i++ {
			b := bodies[i]
			next := total
			if i+1 < len(bodies) {
				for c := range next {
					next[c] = int(bodies[i+1].base[c])
				}
			}
			any := false
			for c := range cur {
				any = any || cur[c] < len(ents[c]) && int(ents[c][cur[c]].idx) < next[c]
			}
			if !any {
				continue
			}
			code := text[b.off : b.off+b.size]
			local := b.base
			opWalkBody(code, func(start, _ int, fields []opField) {
				for _, f := range fields {
					c := f.class
					idx := local[c]
					local[c]++
					if cur[c] >= len(ents[c]) || ents[c][cur[c]].idx != idx {
						continue
					}
					p, v := code[start+f.off:], ents[c][cur[c]].dv
					if c != opImm {
						v += opValue(p, f.width)
					}
					opPut(p, f.width, v)
					cur[c]++
					n[c]++
				}
			})
		}
		return n
	})
	for _, n := range applied {
		for c := range n {
			st.Kept[c] += n[c]
		}
	}
	return st, nil
}

// opValue reads a field as a sign-extended little-endian integer, and opPut
// writes one back truncated to the field's width.
func opValue(b []byte, width int) int64 {
	var v uint64
	for i := width - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return int64(v<<(64-8*width)) >> (64 - 8*width)
}

func opPut(b []byte, width int, v int64) {
	for i := range width {
		b[i] = byte(v >> (8 * i))
	}
}

// opForceAll ships every class whatever it prices at; it exists to measure
// the pricing rule against the corpus.
var opForceAll = os.Getenv("PRESAGE_OPFIELD") == "all"

// opClassNames name the classes in the encode's report.
var opClassNames = [nOpClass]string{"imm", "rel", "disp/sp", "disp/reg", "disp/rip", "disp/abs"}

// opClassNote is the per-class ledger of what shipped: which classes paid for
// themselves is the layer's only tuning decision, and it is measured.
func opClassNote(st opStats) string {
	var b strings.Builder
	for c := range st.Entries {
		if st.Entries[c] == 0 {
			continue
		}
		if b.Len() != 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %d/%d (%d B for %d)", opClassNames[c], st.Kept[c], st.Entries[c], st.Cost[c], st.Gain[c])
	}
	if b.Len() == 0 {
		return "nothing"
	}
	return b.String()
}
