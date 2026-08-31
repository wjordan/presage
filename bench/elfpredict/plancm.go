package main

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"time"
)

// probePlanCM answers the plan-side twin of the cmix probe: the correction is
// coded by a compressor that knows nothing about it, and so is the plan. The
// cmix probe took 16.4% off the correction by telling the coder what a byte
// meant. This one asks the same question of the plan, whose bytes are far more
// structured -- almost every one of them is a byte of a varint in a column of
// varints, and the column is known.
//
// Discipline is the cmix probe's: every coded size below is a real coded size
// from a real arithmetic coder, and every rung is decoded back and compared
// byte for byte before it is allowed into a table.
//
// Report only.

// --- decomposition ---------------------------------------------------------

type planStream struct {
	name string
	kind streamKind
	b    []byte
}

type streamKind int

const (
	skOpaque  streamKind = iota // no known interior structure
	skVarint                    // a column of LEB128 varints, unsigned or zigzag
	skBitmap                    // one bit per entity
	skRunPair                   // alternating varint run lengths
)

// decomposePlan splits a combined plan into the leaf streams it is built from,
// plus whatever bytes are left over (headers, lengths, the inline range map).
// The split follows the serializers exactly: combinedPlan.marshal, then the
// derived-form structure layout of marshalDerivedForm, then the equivalence and
// field column lists.
func decomposePlan(planBytes []byte) ([]planStream, []byte) {
	var out []planStream
	var residue []byte

	add := func(name string, kind streamKind, b []byte) {
		out = append(out, planStream{name, kind, b})
	}

	cp, err := unmarshalCombinedPlan(planBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  probe plancm: combined plan does not parse: %v\n", err)
		return nil, planBytes
	}

	// --- equivalences
	if ep, err := parseEquivalencePlan(cp.Equivalences); err == nil {
		add("eq src-skip", skVarint, ep.SrcSkip)
		add("eq src-residual", skVarint, ep.SrcResidual)
		add("eq dst-skip", skVarint, ep.DstSkip)
		add("eq copy-len", skVarint, ep.CopyLen)
		n := len(ep.SrcSkip) + len(ep.SrcResidual) + len(ep.DstSkip) + len(ep.CopyLen)
		residue = append(residue, make([]byte, max(len(cp.Equivalences)-n, 0))...)
	} else if len(cp.Equivalences) != 0 {
		add("equivalences (unparsed)", skOpaque, cp.Equivalences)
	}

	// --- structure, in derived form
	if leaves, rest, err := decomposeDerivedStructure(cp.Structure); err == nil {
		out = append(out, leaves...)
		residue = append(residue, rest...)
	} else if len(cp.Structure) != 0 {
		fmt.Fprintf(os.Stderr, "  probe plancm: structure not in derived form (%v); treating whole\n", err)
		add("structure (unparsed)", skOpaque, cp.Structure)
	}

	// --- fields
	if fp, err := unmarshalFieldPlan(cp.Fields); err == nil {
		add("field remap-index", skVarint, fp.RemapIndex)
		add("field remap-shift", skVarint, fp.RemapShift)
		add("field field-index", skVarint, fp.FieldIndex)
		add("field field-delta", skVarint, fp.FieldDelta)
	} else if len(cp.Fields) != 0 {
		add("fields (unparsed)", skOpaque, cp.Fields)
	}

	// --- the remaining top-level streams, whole
	for _, s := range []struct {
		name string
		b    []byte
	}{
		{"choices", cp.Choices},
		{"reloc", cp.Reloc},
		{"eh-frame", cp.EhFrame},
		{"rodata", cp.RoData},
		{"go-tables", cp.GoTables},
		{"dwarf", cp.Dwarf},
	} {
		if len(s.b) != 0 {
			add(s.name, skOpaque, s.b)
		}
	}
	return out, residue
}

// decomposeDerivedStructure walks the exact byte layout marshalDerivedForm
// emits. It returns the named leaf columns and the bytes that are not in any
// of them.
func decomposeDerivedStructure(b []byte) ([]planStream, []byte, error) {
	if len(b) < len(planMagic) || !bytes.Equal(b[:len(planMagic)], planMagic[:]) {
		return nil, nil, fmt.Errorf("bad structure magic")
	}
	r := &planReader{b: b[len(planMagic):]}
	start := len(b) - len(r.b)
	var out []planStream
	var residue []byte
	// residueFrom records the header bytes consumed since the last stream.
	mark := start
	flush := func() {
		at := len(b) - len(r.b)
		residue = append(residue, b[mark:at]...)
		mark = at
	}
	col := func(name string, kind streamKind) {
		flush()
		s := r.stream()
		if r.err != nil {
			return
		}
		mark = len(b) - len(r.b)
		out = append(out, planStream{name, kind, s.b})
	}

	r.u() // OldAddr
	r.u() // NewAddr
	r.u() // TargetLen
	mode := r.byteAt()
	r.u() // number of mappings
	if r.err != nil {
		return nil, nil, fmt.Errorf("truncated structure header")
	}
	if planMode(mode) != planDerived {
		return nil, nil, fmt.Errorf("structure mode %d is not planDerived", mode)
	}
	for _, n := range []string{"map src-index", "map src-offset", "map extent-residual", "map size-delta", "map start-residual"} {
		col(n, skVarint)
	}
	col("map copy bitmap", skBitmap)

	// the derived stream
	r.u() // derived enumeration length
	col("derived boundary exceptions", skVarint)
	col("derived suppression bitmap", skBitmap)
	col("derived size-fixup index", skVarint)
	col("derived size-fixup value", skVarint)

	// the sidecar delta
	r.u() // NewUnits
	r.u() // Align
	r.u() // Maps
	for _, c := range []struct {
		name string
		kind streamKind
	}{
		{"drop runs", skRunPair},
		{"order-exception index", skVarint},
		{"order-exception source", skVarint},
		{"size-delta index", skVarint},
		{"size-delta value", skVarint},
		{"insert position", skVarint},
		{"insert size", skVarint},
		{"insert name hashes", skOpaque},
		{"layout-fixup index", skVarint},
		{"layout-fixup value", skVarint},
		{"correspondence exc index", skVarint},
		{"correspondence exc source", skVarint},
		{"unrepresentable mappings", skOpaque},
	} {
		col(c.name, c.kind)
	}

	// the point tables
	r.u() // number of points
	col("point index-delta", skVarint)
	col("point offset", skVarint)
	col("point shift-delta", skVarint)

	// the range map is inline varints, not a stream: it is all residue.
	flush()
	residue = append(residue, r.b...)
	if r.err != nil {
		return nil, nil, fmt.Errorf("structure walk failed: %w", r.err)
	}
	return out, residue, nil
}

// --- generic context set ---------------------------------------------------

// genericCtx is the "no plan knowledge" arm: order-0/1/2, a sparse skip
// context, and the match model. It is what a context-mixing coder gives you
// for free, and it is the number a plan-aware set has to beat to be worth
// anything.
type genericCtx struct{}

func (g *genericCtx) numModels() int { return 4 }
func (g *genericCtx) bankBits(m int) uint {
	if m == 0 {
		return 8
	}
	return 22
}
func (g *genericCtx) mixerCtxs() int { return 1 }
func (g *genericCtx) useMatch() bool { return true }
func (g *genericCtx) setByte(i int, data []byte, h []uint32) int {
	var p1, p2, p4 uint32
	if i >= 1 {
		p1 = uint32(data[i-1])
	}
	if i >= 2 {
		p2 = uint32(data[i-2])
	}
	if i >= 4 {
		p4 = uint32(data[i-4])
	}
	h[0] = mix32(1)
	h[1] = mix32(0x10000 + p1)
	h[2] = mix32(0x20000 + p1<<8 + p2)
	h[3] = mix32(0x40000 + p1<<8 + p4)
	return 0
}

// --- varint-aware context set ----------------------------------------------

// varintState is the running parse of a LEB128 column, maintained from bytes
// already coded and therefore available to the decoder at every point.
//
//	k     how many bytes of the current varint are already done
//	prev  the last two complete varint values
//
// Nothing here reads data[i] itself.
type varintState struct {
	k           uint32
	v           uint64 // accumulator for the varint in progress
	prev, prev2 uint64
	idx         uint32 // how many complete varints have gone by
}

func (s *varintState) reset() { *s = varintState{} }

// consume folds in a byte that has just been decoded.
func (s *varintState) consume(c byte) {
	s.v |= uint64(c&0x7F) << (7 * s.k)
	if c&0x80 != 0 {
		if s.k < 9 {
			s.k++
		}
		return
	}
	s.prev2, s.prev = s.prev, s.v
	s.v, s.k = 0, 0
	s.idx++
}

// bucket compresses a varint value to a small class: its magnitude, with the
// low bit of a zigzag value kept because sign alternates in delta columns.
func bucket(v uint64) uint32 {
	var n uint32
	for v > 0 {
		n++
		v >>= 1
	}
	return n
}

// varintCtx codes a column of varints. Rung 0 is position-in-varint only,
// rung 1 adds the previous value's magnitude, rung 2 adds the one before that
// and the index parity (which is what separates the two halves of a paired
// index/value column and the two halves of a run column).
type varintCtx struct {
	st   varintState
	rung int
	// parity is how many varints make one logical record; 1 for a plain
	// column, 2 for the alternating run column.
	parity uint32
}

var varintModels = []int{3, 5, 7}

func (c *varintCtx) numModels() int { return varintModels[c.rung] }
func (c *varintCtx) bankBits(m int) uint {
	if m == 0 {
		return 12
	}
	return 22
}
func (c *varintCtx) mixerCtxs() int { return 10 }
func (c *varintCtx) useMatch() bool { return true }

func (c *varintCtx) setByte(i int, data []byte, h []uint32) int {
	if i == 0 {
		c.st.reset()
	} else {
		c.st.consume(data[i-1])
	}
	k := c.st.k
	if k > 9 {
		k = 9
	}
	var p1, p2 uint32
	if i >= 1 {
		p1 = uint32(data[i-1])
	}
	if i >= 2 {
		p2 = uint32(data[i-2])
	}
	// The partial value of the varint being built, which for a multi-byte
	// varint says a great deal about the byte still to come.
	part := uint32(c.st.v & 0xFFFF)

	h[0] = mix32(0x100 + k)
	h[1] = mix32(0x10000 + k<<8 + p1)
	h[2] = mix32(0x20000 + k<<16 + p1<<8 + p2)
	if len(h) > 3 {
		b1 := bucket(c.st.prev)
		h[3] = mix32(0x30000 + k<<12 + b1<<4)
		h[4] = mix32(0x40000 + k<<20 + b1<<12 + part)
	}
	if len(h) > 5 {
		b1, b2 := bucket(c.st.prev), bucket(c.st.prev2)
		par := c.st.idx % max32(c.parity, 1)
		h[5] = mix32(0x50000 + k<<24 + b1<<16 + b2<<8 + par)
		h[6] = mix32(0x60000 + par<<28 + k<<20 + uint32(c.st.prev&0xFFF))
	}
	sel := int(k)
	if c.parity > 1 {
		sel = int(k%5) + 5*int(c.st.idx%2)
	}
	return sel
}

func max32(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}

// pairCtx is the cross-column arm. The plan's columns are parallel arrays:
// every exception, remap and equivalence run contributes one entry to an index
// column and one to a value column, and the index column ships first. So when
// the value column's record i is being coded, the decoder already holds index
// record i -- and index records are gaps, which say how far apart two edits
// are, which is exactly the thing a value's magnitude correlates with.
//
// This is the context xz structurally cannot have: it sees one flat byte
// stream and no notion that byte 90,000 of this column and byte 40,000 of that
// one describe the same edit.
type pairCtx struct {
	st    varintState
	other []uint64 // the paired index column, one value per record
	deep  bool     // carry a third value of same-column history
	prev3 uint64
}

func (c *pairCtx) numModels() int { return 7 }
func (c *pairCtx) bankBits(m int) uint {
	if m == 0 {
		return 14
	}
	return 22
}
func (c *pairCtx) mixerCtxs() int { return 10 }
func (c *pairCtx) useMatch() bool { return true }

func (c *pairCtx) setByte(i int, data []byte, h []uint32) int {
	if i == 0 {
		c.st.reset()
		c.prev3 = 0
	} else {
		before := c.st.idx
		c.st.consume(data[i-1])
		if c.st.idx != before {
			c.prev3 = c.st.prev2
		}
	}
	k := c.st.k
	if k > 9 {
		k = 9
	}
	var p1, p2 uint32
	if i >= 1 {
		p1 = uint32(data[i-1])
	}
	if i >= 2 {
		p2 = uint32(data[i-2])
	}
	// The paired index record for the value now being coded.
	var o, oNext uint64
	if int(c.st.idx) < len(c.other) {
		o = c.other[c.st.idx]
	}
	if int(c.st.idx)+1 < len(c.other) {
		oNext = c.other[c.st.idx+1]
	}
	ob, b1, b2 := bucket(o), bucket(c.st.prev), bucket(c.st.prev2)
	part := uint32(c.st.v & 0xFFFF)

	h[0] = mix32(0x100 + k<<8 + ob)
	h[1] = mix32(0x10000 + k<<12 + ob<<6 + b1)
	h[2] = mix32(0x20000 + k<<16 + uint32(o&0xFF)<<8 + p1)
	h[3] = mix32(0x30000 + k<<20 + ob<<14 + part)
	h[4] = mix32(0x40000 + k<<24 + b1<<16 + b2<<8 + bucket(c.prev3))
	h[5] = mix32(0x50000 + k<<16 + p1<<8 + p2)
	// The next gap as well: a value's magnitude often tracks the distance to
	// the following edit as much as the preceding one.
	h[6] = mix32(0x60000 + k<<16 + ob<<8 + bucket(oNext))
	return int(k)
}

// parseVarints decodes a whole varint column, so a value column can be coded
// against it. The decoder can do exactly this, because the column it parses
// has already been decoded.
func parseVarints(b []byte) []uint64 {
	var out []uint64
	var v uint64
	var k uint
	for _, c := range b {
		v |= uint64(c&0x7F) << k
		if c&0x80 != 0 {
			if k < 63 {
				k += 7
			}
			continue
		}
		out = append(out, v)
		v, k = 0, 0
	}
	return out
}

// bitmapCtx codes a bitmap: a long run of mostly-set or mostly-clear bits
// where the useful context is the preceding bits, not the preceding bytes.
// The coder already carries the partial byte, so this set supplies the window
// of bits before it at three depths.
type bitmapCtx struct{}

func (b *bitmapCtx) numModels() int { return 4 }
func (b *bitmapCtx) bankBits(m int) uint {
	if m == 0 {
		return 12
	}
	return 22
}
func (b *bitmapCtx) mixerCtxs() int { return 1 }
func (b *bitmapCtx) useMatch() bool { return true }
func (b *bitmapCtx) setByte(i int, data []byte, h []uint32) int {
	var p1, p2, p3 uint32
	if i >= 1 {
		p1 = uint32(data[i-1])
	}
	if i >= 2 {
		p2 = uint32(data[i-2])
	}
	if i >= 3 {
		p3 = uint32(data[i-3])
	}
	// popcount of the last four bytes: the local density of a sparse bitmap.
	dens := uint32(popcnt8(byte(p1)) + popcnt8(byte(p2)) + popcnt8(byte(p3)))
	h[0] = mix32(0x100 + dens)
	h[1] = mix32(0x10000 + p1)
	h[2] = mix32(0x20000 + p1<<8 + p2)
	h[3] = mix32(0x30000 + p1<<16 + p2<<8 + p3)
	return 0
}

func popcnt8(b byte) int {
	n := 0
	for ; b != 0; b &= b - 1 {
		n++
	}
	return n
}

// --- concatenated set ------------------------------------------------------

// concatCtx codes every column end to end with the column id as a context and
// as the mixer selector, so one adaptive model serves all of them and the
// per-stream model warm-up is paid once. col[i] is the column of byte i, and
// is known to the decoder because the column lengths ship in the plan already.
type concatCtx struct {
	col   []byte
	kinds []streamKind
	st    varintState
	cur   byte
	ncols int
}

func (c *concatCtx) numModels() int { return 6 }
func (c *concatCtx) bankBits(m int) uint {
	if m == 0 {
		return 14
	}
	return 22
}
func (c *concatCtx) mixerCtxs() int { return c.ncols }
func (c *concatCtx) useMatch() bool { return true }

func (c *concatCtx) setByte(i int, data []byte, h []uint32) int {
	col := c.col[i]
	if i == 0 || col != c.cur {
		c.st.reset()
		c.cur = col
	} else {
		c.st.consume(data[i-1])
	}
	k := c.st.k
	if k > 9 {
		k = 9
	}
	var p1, p2 uint32
	if i >= 1 {
		p1 = uint32(data[i-1])
	}
	if i >= 2 {
		p2 = uint32(data[i-2])
	}
	cc := uint32(col)
	b1 := bucket(c.st.prev)
	h[0] = mix32(0x100 + cc<<8 + k)
	h[1] = mix32(0x10000 + cc<<16 + k<<8 + p1)
	h[2] = mix32(0x20000 + cc<<24 + k<<20 + p1<<8 + p2)
	h[3] = mix32(0x30000 + cc<<16 + k<<8 + b1)
	h[4] = mix32(0x40000 + cc<<20 + k<<16 + uint32(c.st.v&0xFFFF))
	h[5] = mix32(0x50000 + p1<<8 + p2)
	return int(col)
}

// --- the probe -------------------------------------------------------------

// planCMMin is the size below which a column is not worth coding: the coder's
// own model warm-up dominates and the answer is noise.
const planCMMin = 4096

func probePlanCM(planBytes []byte) {
	leaves, residue := decomposePlan(planBytes)
	if len(leaves) == 0 {
		fmt.Fprintf(os.Stderr, "  probe plancm: nothing to decompose\n")
		return
	}

	wholeXZ := xzSizeContiguous(planBytes)
	rawTotal := len(planBytes)

	// --- 1. anatomy
	type row struct {
		s      planStream
		xz     int
		cm     int
		cmName string
		encS   float64
		decS   float64
	}
	rows := make([]row, len(leaves))
	bs := make([][]byte, len(leaves))
	for i, s := range leaves {
		bs[i] = s.b
	}
	sizes := xzSizesContiguous(bs...)
	leafRaw, leafXZ := 0, 0
	for i, s := range leaves {
		rows[i] = row{s: s, xz: sizes[i], cm: -1}
		leafRaw += len(s.b)
		leafXZ += sizes[i]
	}
	residueXZ := xzSizeContiguous(residue)

	fmt.Fprintf(os.Stderr, "  probe plancm: derived-map plan %d raw, %d xz as one stream\n", rawTotal, wholeXZ)
	fmt.Fprintf(os.Stderr, "    %-30s %10s %10s %8s %7s\n", "stream", "raw", "xz alone", "bits/B", "%plan")
	byXZ := make([]int, len(rows))
	for i := range byXZ {
		byXZ[i] = i
	}
	sort.SliceStable(byXZ, func(a, b int) bool { return rows[byXZ[a]].xz > rows[byXZ[b]].xz })
	for _, i := range byXZ {
		r := rows[i]
		if len(r.s.b) == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "    %-30s %10d %10d %8.4f %6.1f%%\n",
			r.s.name, len(r.s.b), r.xz, 8*float64(r.xz)/float64(max(len(r.s.b), 1)),
			100*float64(r.xz)/float64(max(wholeXZ, 1)))
	}
	fmt.Fprintf(os.Stderr, "    %-30s %10d %10d\n", "residue (headers, ranges)", len(residue), residueXZ)
	fmt.Fprintf(os.Stderr, "    %-30s %10d %10d   (vs %d for the plan as one xz stream)\n",
		"sum of the parts", leafRaw+len(residue), leafXZ+residueXZ, wholeXZ)

	// --- 2 & 3. code each stream above the floor, generic then plan-aware.
	fmt.Fprintf(os.Stderr, "\n    %-30s %9s %9s %9s %9s %8s\n",
		"stream", "xz alone", "CM plain", "CM aware", "best-xz", "MB/s dec")
	cmTotal, xzTotalCoded := 0, 0
	var cmRaw, cmSeconds float64
	for i := range rows {
		r := &rows[i]
		if len(r.s.b) < planCMMin {
			cmTotal += r.xz
			xzTotalCoded += r.xz
			continue
		}
		// The paired index column, when the plan has one and its record count
		// agrees. A disagreement means the two columns are not the parallel
		// arrays this arm assumes, and the arm is dropped rather than fudged.
		var pair []uint64
		if other, ok := planColumnPairs[r.s.name]; ok {
			for j := range rows {
				if rows[j].s.name != other || len(rows[j].s.b) == 0 {
					continue
				}
				v := parseVarints(rows[j].s.b)
				if len(v) == countVarints(r.s.b) {
					pair = v
				} else {
					fmt.Fprintf(os.Stderr, "    %-30s not parallel to %s (%d vs %d records); no paired arm\n",
						r.s.name, other, countVarints(r.s.b), len(v))
				}
			}
		}
		plain, okP, plainDec := codeRoundTrip(r.s.b, func() cmContexts { return &genericCtx{} })
		aware, okA, awareName, encS, decS := codeAware(r.s, pair)

		best := r.xz
		bestName := "xz"
		bestDec := 0.0
		if okP && plain < best {
			best, bestName, bestDec = plain, "CM plain", plainDec
		}
		if okA && aware < best {
			best, bestName, bestDec = aware, awareName, decS
		}
		r.cm, r.cmName, r.encS, r.decS = best, bestName, encS, bestDec
		cmTotal += best
		xzTotalCoded += r.xz
		if bestName != "xz" {
			cmRaw += float64(len(r.s.b))
			cmSeconds += bestDec
		}
		fmt.Fprintf(os.Stderr, "    %-30s %9d %9s %9s %9d %8s  %s\n",
			r.s.name, r.xz, num(plain, okP), num(aware, okA), best,
			rate(len(r.s.b), bestDec), bestName)
	}

	// Which flips are worth their decode time? A stream that saves 200 bytes
	// for a third of a second is not a trade a patch applier would take, and
	// the honest total should say so rather than bury it in a sum.
	flipped := make([]int, 0, len(rows))
	for i := range rows {
		if rows[i].cm > 0 && rows[i].cmName != "xz" && rows[i].xz > rows[i].cm {
			flipped = append(flipped, i)
		}
	}
	sort.SliceStable(flipped, func(a, b int) bool {
		ga := float64(rows[flipped[a]].xz-rows[flipped[a]].cm) / max(rows[flipped[a]].decS, 1e-9)
		gb := float64(rows[flipped[b]].xz-rows[flipped[b]].cm) / max(rows[flipped[b]].decS, 1e-9)
		return ga > gb
	})
	fmt.Fprintf(os.Stderr, "\n    %-30s %9s %8s %10s %9s %9s\n",
		"flip, best value first", "saved", "dec s", "B saved/s", "cum saved", "cum dec s")
	var cumG int
	var cumS float64
	for _, i := range flipped {
		g := rows[i].xz - rows[i].cm
		cumG += g
		cumS += rows[i].decS
		fmt.Fprintf(os.Stderr, "    %-30s %9d %8.3f %10.0f %9d %9.2f\n",
			rows[i].s.name, g, rows[i].decS, float64(g)/max(rows[i].decS, 1e-9), cumG, cumS)
	}

	// --- the concatenated arm: one model over every column above the floor.
	var cat []byte
	var colID []byte
	ncols := 0
	catXZParts := 0
	for i := range rows {
		if len(rows[i].s.b) < planCMMin {
			continue
		}
		cat = append(cat, rows[i].s.b...)
		for range rows[i].s.b {
			colID = append(colID, byte(ncols))
		}
		catXZParts += rows[i].xz
		ncols++
		if ncols == 255 {
			break
		}
	}
	if ncols > 0 {
		set := func() cmContexts { return &concatCtx{col: colID, ncols: ncols} }
		n, ok, sec := codeRoundTrip(cat, set)
		fmt.Fprintf(os.Stderr, "    %-30s %9d %9s %9s %9s %8s  (%d columns, %d raw)\n",
			"CONCATENATED, column ctx", catXZParts, "-", num(n, ok), "-",
			rate(len(cat), sec), ncols, len(cat))
		if ok && n < cmTotal-(xzTotalCoded-catXZParts) {
			fmt.Fprintf(os.Stderr, "      concatenated beats per-stream by %d\n",
				(cmTotal-(xzTotalCoded-catXZParts))-n)
		}
	}

	// --- 4. the honest total
	total := cmTotal + residueXZ
	fmt.Fprintf(os.Stderr, "\n    plan as shipped (one xz stream)        %10d\n", wholeXZ)
	fmt.Fprintf(os.Stderr, "    per-stream xz, summed                  %10d  (%+d)\n", leafXZ+residueXZ, leafXZ+residueXZ-wholeXZ)
	fmt.Fprintf(os.Stderr, "    best per stream (CM or xz), summed     %10d  (%+d vs shipped)\n", total, total-wholeXZ)
	if cmSeconds > 0 {
		fmt.Fprintf(os.Stderr, "    decode cost of the flipped streams     %10.0f raw B at %.2f MB/s = %.2f s\n",
			cmRaw, cmRaw/1e6/cmSeconds, cmSeconds)
	} else {
		fmt.Fprintf(os.Stderr, "    decode cost: no stream flipped to CM\n")
	}
}

// num renders a coded size; a stream whose kind has no plan-aware set at all
// prints "n/a" rather than a number, and is never confused with a round-trip
// failure, which is reported loudly at the point it happens.
func num(n int, ok bool) string {
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%d", n)
}

func rate(n int, sec float64) string {
	if sec <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", float64(n)/1e6/sec)
}

// planColumnPairs names, for each value column, the index column that ships
// before it and carries one entry per the same record. It is the whole of the
// plan knowledge the cross-column arm uses.
var planColumnPairs = map[string]string{
	"eq copy-len":              "eq dst-skip",
	"eq src-residual":          "eq dst-skip",
	"field remap-shift":        "field remap-index",
	"field field-delta":        "field field-index",
	"order-exception source":   "order-exception index",
	"size-delta value":         "size-delta index",
	"layout-fixup value":       "layout-fixup index",
	"derived size-fixup value": "derived size-fixup index",
	"point offset":             "point index-delta",
	"point shift-delta":        "point index-delta",
	"insert size":              "insert position",
}

// countVarints is how many complete varints a column holds.
func countVarints(b []byte) int {
	n := 0
	for _, c := range b {
		if c&0x80 == 0 {
			n++
		}
	}
	return n
}

// codeAware runs the context set the stream's kind calls for, and returns the
// best of its rungs. pair, when non-nil, is the paired index column's values.
func codeAware(s planStream, pair []uint64) (int, bool, string, float64, float64) {
	type arm struct {
		name string
		set  func() cmContexts
	}
	var arms []arm
	if len(pair) != 0 && s.kind == skVarint {
		arms = append(arms,
			arm{"paired", func() cmContexts { return &pairCtx{other: pair} }})
	}
	switch s.kind {
	case skVarint:
		for r := range varintModels {
			r := r
			arms = append(arms, arm{fmt.Sprintf("varint r%d", r), func() cmContexts { return &varintCtx{rung: r, parity: 1} }})
		}
	case skRunPair:
		for r := range varintModels {
			r := r
			arms = append(arms, arm{fmt.Sprintf("runpair r%d", r), func() cmContexts { return &varintCtx{rung: r, parity: 2} }})
		}
	case skBitmap:
		arms = append(arms, arm{"bitmap", func() cmContexts { return &bitmapCtx{} }})
	default:
		if len(arms) == 0 {
			return 0, false, "", 0, 0
		}
	}
	best, bestName := -1, ""
	var bestEnc, bestDec float64
	for _, a := range arms {
		t := time.Now()
		c := newCMCoder(a.set())
		enc := newArEncoder(len(s.b)/2 + 64)
		c.code(s.b, enc, nil, nil)
		coded := enc.flush()
		encS := time.Since(t).Seconds()
		t = time.Now()
		back := cmDecode(coded, len(s.b), a.set())
		decS := time.Since(t).Seconds()
		if !bytes.Equal(back, s.b) {
			fmt.Fprintf(os.Stderr, "    %-30s %s ROUND TRIP FAILED\n", s.name, a.name)
			continue
		}
		if best < 0 || len(coded) < best {
			best, bestName, bestEnc, bestDec = len(coded), a.name, encS, decS
		}
	}
	if best < 0 {
		return 0, false, "", 0, 0
	}
	return best, true, bestName, bestEnc, bestDec
}

// codeRoundTrip encodes, decodes and compares. A rung that does not round trip
// reports no size at all. The second return is the decode wall time, which is
// what a production port would pay per apply.
func codeRoundTrip(b []byte, set func() cmContexts) (int, bool, float64) {
	coded := cmEncode(b, set())
	t := time.Now()
	back := cmDecode(coded, len(b), set())
	sec := time.Since(t).Seconds()
	if !bytes.Equal(back, b) {
		return 0, false, sec
	}
	return len(coded), true, sec
}

// xzSizesContiguous is xzSizes with the single-block flag, so a column's size
// is comparable with the marginal numbers the derived rung prints.
func xzSizesContiguous(bs ...[]byte) []int {
	out := make([]int, len(bs))
	done := make(chan int, len(bs))
	for i, b := range bs {
		go func(i int, b []byte) {
			out[i] = xzSizeContiguous(b)
			done <- i
		}(i, b)
	}
	for range bs {
		<-done
	}
	return out
}
