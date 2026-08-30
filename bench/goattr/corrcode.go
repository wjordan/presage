package main

// Level c -- probe F: how much of the correction's per-run cost is the
// *encoding*?
//
// Ladder 1 fits the correction at 0.606 B per run plus 0.244 B per wrong
// byte, and the minor pair pays the per-run term 163,010 times. This level
// re-encodes exactly the same information -- the same wrong bytes, in
// regions built by the same loop -- in other shapes, compresses each with
// the codec's own compressor at the codec's settings and with `xz -9e` as a
// control, and prices the difference. The tables are research 14.
//
// Report only. The shipped format is variant "base", and the probe checks
// byte-for-byte that its replay of it is what delta.EncodeCorrection writes;
// every other variant is decoded back and compared with the new file, so a
// variant cannot win by dropping information.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sync"

	"github.com/wjordan/presage/delta"
)

// regionsM is regions() with the merge threshold as a parameter: m is how
// many identical bytes a region absorbs rather than paying for a second
// header. m == delta.MergeGap is what the codec ships; m == 1 never merges.
func regionsM(pred, want []byte, m int) []reg {
	var out []reg
	for s := 0; s < len(want); {
		if pred[s] == want[s] {
			s++
			continue
		}
		e := s + 1
		for e < len(want) {
			k := e
			for k < len(want) && k-e < m && pred[k] == want[k] {
				k++
			}
			if k-e < m && k < len(want) {
				e = k + 1
				continue
			}
			break
		}
		out = append(out, reg{s, e})
		s = e
	}
	return out
}

// encOpt is one candidate representation of the correction.
type encOpt struct {
	name  string
	merge int  // identical bytes a region absorbs (delta.MergeGap ships)
	lz    bool // regions may be written as an lz stream over the prediction
	xor   bool // a literal region carries want^pred, not want
	// auto instead decides per region -- xor it only where that zeroes at
	// least auto% of it -- and says which in a second span bit.
	auto int

	whole    bool            // one region over the whole file (the merge limit)
	columnar bool            // gaps, spans, lz ops and literals as four streams
	perSect  bool            // one stream group per section
	stream   bool            // stream-major concatenation across groups
	word     map[string]bool // sections encoded in 4-byte words (literals only)
	xorIn    map[string]bool // sections whose literals are want^pred
}

func baseOpt() encOpt { return encOpt{name: "base", merge: delta.MergeGap, lz: true} }

// sgroup is one group of regions with its own streams. base is the file
// offset its gaps are relative to and ws the unit its gaps and spans are
// counted in.
type sgroup struct {
	name string
	base int
	ws   int
	lz   bool
	regs []reg

	xorAll                       bool
	ctrl, gaps, spans, ops, lits []byte
	nlz, nlit, nxor              int
}

func putU(dst *[]byte, v uint64) {
	var t [binary.MaxVarintLen64]byte
	*dst = append(*dst, t[:binary.PutUvarint(t[:], v)]...)
}

// alignRegs rounds regions out to ws-byte boundaries measured from base and
// merges the overlaps that creates.
func alignRegs(regs []reg, base, ws, n int) []reg {
	var out []reg
	for _, r := range regs {
		s := base + (r.s-base)/ws*ws
		e := min(base+((r.e-base+ws-1)/ws)*ws, n)
		if k := len(out); k > 0 && s <= out[k-1].e {
			out[k-1].e = max(out[k-1].e, e)
			continue
		}
		out = append(out, reg{s, e})
	}
	return out
}

// groups splits the regions the way the variant asks for. A region that
// straddles a section boundary is split, so that every group's regions are
// contained in it and the groups stay in file order -- which is what lets a
// region's source window still hold prediction bytes when it is decoded.
func (c *ctx) groups(o encOpt) []*sgroup {
	if o.whole {
		return []*sgroup{{name: "all", base: 0, ws: 1, regs: []reg{{0, len(c.new)}}}}
	}
	regs := regionsM(c.pred, c.new, o.merge)
	if !o.perSect {
		return []*sgroup{{name: "all", base: 0, ws: 1, lz: o.lz, regs: regs}}
	}
	secs := c.sections()
	gs := make([]*sgroup, len(secs)+1)
	for i, s := range secs {
		ws := 1
		if o.word[s.name] {
			ws = 4
		}
		gs[i] = &sgroup{name: s.name, base: s.off, ws: ws, lz: o.lz && ws == 1}
	}
	// Everything outside a section -- headers, padding, the tail -- is
	// scattered through the file, so its group is not in file order and its
	// regions are written as literals: an lz window there could read bytes
	// an earlier group has already corrected.
	gs[len(secs)] = &sgroup{name: "(other)", base: 0, ws: 1, lz: false}
	for _, r := range regs {
		for r.s < r.e {
			k := c.sectionOf(secs, r.s)
			if k < 0 {
				gs[len(secs)].regs = append(gs[len(secs)].regs, r)
				break
			}
			e := min(r.e, secs[k].end)
			gs[k].regs = append(gs[k].regs, reg{r.s, e})
			r.s = e
		}
	}
	var out []*sgroup
	for _, g := range gs {
		if len(g.regs) == 0 {
			continue
		}
		if g.ws > 1 {
			g.regs = alignRegs(g.regs, g.base, g.ws, len(c.new))
		}
		out = append(out, g)
	}
	return out
}

// encGroup writes one group's streams. The lz-or-literal choice is the
// shipped encoder's, priced identically in every variant so that the only
// thing a variant changes is the shape of the streams.
//
// A span carries its flags in its low bits: bit 0 says the region is an lz
// stream (only where the group allows one), and the next bit, under
// auto-xor, says its literals are want^pred rather than want.
func (c *ctx) encGroup(g *sgroup, o encOpt) {
	gapDst, spanDst, opDst := &g.ctrl, &g.ctrl, &g.ctrl
	if o.columnar {
		gapDst, spanDst, opDst = &g.gaps, &g.spans, &g.ops
	}
	sh, xorBit := 0, uint64(0)
	if g.lz {
		sh++
	}
	if o.auto > 0 {
		xorBit = 1 << sh
		sh++
	}
	g.xorAll = o.xor || o.xorIn[g.name]
	prev := g.base
	for _, r := range g.regs {
		putU(gapDst, uint64((r.s-prev)/g.ws))
		n := r.e - r.s
		span := uint64((n + g.ws - 1) / g.ws)
		prev = r.s + int(span)*g.ws
		if g.lz {
			mark := len(*spanDst)
			putU(spanDst, span<<sh|1)
			opMark, litMark := len(*opDst), len(g.lits)
			slack := min(delta.SrcSlack, len(c.pred)-r.e)
			c2, l2 := delta.EmitLZ(c.pred[r.s:r.e+slack], c.new[r.s:r.e], *opDst, g.lits)
			if len(*spanDst)-mark+len(c2)-opMark+len(l2)-litMark < n {
				*opDst, g.lits = c2, l2
				g.nlz++
				continue
			}
			*spanDst = (*spanDst)[:mark]
		}
		g.nlit++
		x := g.xorAll
		if o.auto > 0 {
			z := 0
			for i := r.s; i < r.e; i++ {
				if c.pred[i] == c.new[i] {
					z++
				}
			}
			// xor only where it turns most of the region into zeros: a
			// near-miss field or a merged gap gains, a region of genuinely
			// new content loses, because xor with an unrelated prediction
			// destroys the matches the literal stream has with itself.
			x = 100*z >= o.auto*n
		}
		v := span << sh
		if x {
			g.nxor++
			v |= xorBit
		}
		putU(spanDst, v)
		if x {
			for i := r.s; i < r.e; i++ {
				g.lits = append(g.lits, c.pred[i]^c.new[i])
			}
		} else {
			g.lits = append(g.lits, c.new[r.s:r.e]...)
		}
	}
}

// encode builds the whole stream for a variant: a header naming each group
// and its stream lengths, then the bodies, group-major or stream-major.
func (c *ctx) encode(o encOpt) ([]byte, []*sgroup) {
	gs := c.groups(o)
	for _, g := range gs {
		c.encGroup(g, o)
	}
	var h []byte
	mode := uint64(0)
	if o.stream {
		mode = 1
	}
	if o.xor {
		mode |= 2
	}
	if o.auto > 0 {
		mode |= 4
	}
	// mode bit 0 is the stream-major concatenation and bit 2 the per-region
	// auto-xor; bit 1 records the variant's global xor, which the decoder
	// reads from the per-group flag instead.
	putU(&h, uint64(len(c.new)))
	putU(&h, mode)
	putU(&h, uint64(len(gs)))
	for _, g := range gs {
		putU(&h, uint64(g.base))
		putU(&h, uint64(g.ws))
		flags := uint64(0)
		if g.lz {
			flags |= 1
		}
		if g.xorAll {
			flags |= 2
		}
		putU(&h, flags)
		putU(&h, uint64(g.nlz+g.nlit))
		for _, s := range [][]byte{g.ctrl, g.gaps, g.spans, g.ops, g.lits} {
			putU(&h, uint64(len(s)))
		}
	}
	out := h
	if o.stream {
		for _, pick := range []int{0, 1, 2, 3, 4} {
			for _, g := range gs {
				out = append(out, [][]byte{g.ctrl, g.gaps, g.spans, g.ops, g.lits}[pick]...)
			}
		}
	} else {
		for _, g := range gs {
			for _, s := range [][]byte{g.ctrl, g.gaps, g.spans, g.ops, g.lits} {
				out = append(out, s...)
			}
		}
	}
	return out, gs
}

// vr reads the varints and the slices a variant's stream is made of.
type vr struct {
	b   []byte
	err error
}

func (r *vr) u() uint64 {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		if r.err == nil {
			r.err = fmt.Errorf("truncated varint")
		}
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *vr) take(n int) []byte {
	if n < 0 || n > len(r.b) {
		if r.err == nil {
			r.err = fmt.Errorf("want %d bytes, %d left", n, len(r.b))
		}
		return nil
	}
	b := r.b[:n]
	r.b = r.b[n:]
	return b
}

// decode rebuilds the new file from the prediction and a variant's stream.
// It is the check that a variant carries the same information as the shipped
// format and not less.
func (c *ctx) decode(raw []byte, buf []byte) error {
	copy(buf, c.pred)
	r := &vr{b: raw}
	if n := r.u(); n != uint64(len(buf)) {
		return fmt.Errorf("length %d, want %d", n, len(buf))
	}
	mode := r.u()
	streamMajor, auto := mode&1 != 0, mode&4 != 0
	ng := int(r.u())
	if r.err != nil || ng < 0 || ng > 1<<16 {
		return fmt.Errorf("bad group count: %v", r.err)
	}
	type ghdr struct {
		base, ws, nreg int
		lz, xor        bool
		ln             [5]int
	}
	gh := make([]ghdr, ng)
	for i := range gh {
		gh[i].base = int(r.u())
		gh[i].ws = int(r.u())
		f := r.u()
		gh[i].lz, gh[i].xor = f&1 != 0, f&2 != 0
		gh[i].nreg = int(r.u())
		for k := range gh[i].ln {
			gh[i].ln[k] = int(r.u())
		}
	}
	if r.err != nil {
		return r.err
	}
	st := make([][5][]byte, ng)
	if streamMajor {
		for k := range 5 {
			for i := range gh {
				st[i][k] = r.take(gh[i].ln[k])
			}
		}
	} else {
		for i := range gh {
			for k := range 5 {
				st[i][k] = r.take(gh[i].ln[k])
			}
		}
	}
	if r.err != nil {
		return r.err
	}
	if len(r.b) != 0 {
		return fmt.Errorf("%d bytes after the last stream", len(r.b))
	}
	var scratch []byte
	for i, h := range gh {
		// An interleaved group writes everything into stream 0 and leaves
		// the other three empty; a columnar one is the other way round.
		cr := &vr{b: st[i][0]}
		gr, sr, or := cr, cr, cr
		if len(st[i][1]) > 0 || len(st[i][2]) > 0 || len(st[i][3]) > 0 {
			gr, sr, or = &vr{b: st[i][1]}, &vr{b: st[i][2]}, &vr{b: st[i][3]}
		}
		lit := st[i][4]
		pos := h.base
		for k := 0; k < h.nreg; k++ {
			pos += int(gr.u()) * h.ws
			v := sr.u()
			isLZ := false
			if h.lz {
				isLZ = v&1 != 0
				v >>= 1
			}
			rxor := h.xor
			if auto {
				rxor = v&1 != 0
				v >>= 1
			}
			n := min(int(v)*h.ws, len(buf)-pos)
			if gr.err != nil || sr.err != nil || pos < 0 || n < 0 || pos+n > len(buf) {
				return fmt.Errorf("group %d region %d: out of range", i, k)
			}
			if isLZ {
				slack := min(delta.SrcSlack, len(buf)-(pos+n))
				scratch = append(scratch[:0], buf[pos:pos+n+slack]...)
				var err error
				or.b, lit, err = delta.ApplyLZ(or.b, lit, scratch, buf[pos:pos+n])
				if err != nil {
					return err
				}
			} else {
				if n > len(lit) {
					return fmt.Errorf("region wants %d literals, %d left", n, len(lit))
				}
				if rxor {
					for j := range n {
						buf[pos+j] ^= lit[j]
					}
				} else {
					copy(buf[pos:pos+n], lit[:n])
				}
				lit = lit[n:]
			}
			pos += n
		}
		if len(lit) != 0 {
			return fmt.Errorf("%d literal bytes left over", len(lit))
		}
	}
	if !bytes.Equal(buf, c.new) {
		return fmt.Errorf("output differs from the new file")
	}
	return nil
}

// xzSize is the control: a general-purpose compressor with a much larger
// window and an explicit context model, so that a variant that only helps
// brotli's local matching shows up as such.
func xzSize(b []byte) int {
	cmd := exec.Command("xz", "-9e", "-T1", "-c")
	cmd.Stdin = bytes.NewReader(b)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return -1
	}
	return out.Len()
}

// ---------------------------------------------------------------- driver

type cres struct {
	o        encOpt
	raw, cz  int
	xz       int
	nreg     int
	nxor     int
	litRaw   int
	ctrlRaw  int
	verified string
}

func (c *ctx) corrCode() {
	base, gs := c.encode(baseOpt())
	ship, err := delta.EncodeCorrection(c.pred, c.new)
	if err != nil {
		panic(err)
	}
	// The variant header is the probe's own, so the check is on the two
	// streams the shipped format is made of.
	g := gs[0]
	shipCtrl, shipLit := shippedStreams(ship)
	same := "replay == shipped ctrl+lit, byte for byte"
	if !bytes.Equal(g.ctrl, shipCtrl) || !bytes.Equal(g.lits, shipLit) {
		same = fmt.Sprintf("REPLAY MISMATCH (ctrl %d vs %d, lit %d vs %d)",
			len(g.ctrl), len(shipCtrl), len(g.lits), len(shipLit))
	}
	if err := c.decode(base, make([]byte, len(c.new))); err != nil {
		same += "; BASE DECODE FAILED: " + err.Error()
	}

	fmt.Fprintf(os.Stderr, "\n-- c. correction encoding\n")
	fmt.Fprintf(os.Stderr, "  shipped correction: %d raw (%d ctrl, %d lit), %d compressed; %s\n",
		len(ship), len(shipCtrl), len(shipLit), c.full, same)
	fmt.Fprintf(os.Stderr, "  ctrl alone %d compressed, lit alone %d compressed, sum %d (joint %d)\n",
		czSize(shipCtrl), czSize(shipLit), czSize(shipCtrl)+czSize(shipLit), c.full)
	fmt.Fprintf(os.Stderr, "  regions: %d total, %d lz, %d literal\n", g.nlz+g.nlit, g.nlz, g.nlit)

	col := baseOpt()
	col.columnar = true
	_, cg := c.encode(col)
	q := cg[0]
	fmt.Fprintf(os.Stderr, "  the same regions as four streams: gaps %d raw / %d cz, spans %d / %d, lz ops %d / %d, literals %d / %d\n",
		len(q.gaps), czSize(q.gaps), len(q.spans), czSize(q.spans),
		len(q.ops), czSize(q.ops), len(q.lits), czSize(q.lits))

	c.jitter(base)
	c.entropy()
	c.histograms()
	c.bySectionStreams(baseOpt(), "shipped format")
	best := baseOpt()
	best.merge, best.xor = *mergeBest, true
	c.bySectionStreams(best, fmt.Sprintf("merge=%d + xor", *mergeBest))
	c.variants()
}

// shippedStreams splits a correction the codec wrote into its two streams.
func shippedStreams(s []byte) (ctrl, lit []byte) {
	r := &vr{b: s}
	r.u() // file length
	r.u() // region count
	nc := int(r.u())
	nl := int(r.u())
	return r.take(nc), r.take(nl)
}

// entropy is the order-0 cost of the region headers themselves: the two
// numbers a run costs, its gap and its span, coded at their own empirical
// distribution and nothing else. It is the floor any re-encoding of the
// same regions works against -- and the answer to this level's question,
// because the compressor is already below it.
func (c *ctx) entropy() {
	gapC, spanC := map[int]int{}, map[int]int{}
	prev := 0
	for _, r := range c.regs {
		gapC[r.s-prev]++
		spanC[r.e-r.s]++
		prev = r.e
	}
	n := len(c.regs)
	hg, hs := h0(gapC, n), h0(spanC, n)
	ship, _ := delta.EncodeCorrection(c.pred, c.new)
	ctrl, lit := shippedStreams(ship)
	czl := czSize(lit)
	fmt.Fprintf(os.Stderr, "\n  order-0 floor for the region headers: gap %.2f bits + span %.2f bits = %.2f per run, %.0f B for %d runs\n",
		hg, hs, hg+hs, float64(n)*(hg+hs)/8, n)
	fmt.Fprintf(os.Stderr, "  the codec spends: %d B on the control stream alone, %d B marginal (%.2f bits per run); literals alone %d B\n",
		czSize(ctrl), c.full-czl, 8*float64(c.full-czl)/float64(n), czl)
}

// jitter is the measurement's own noise floor. The same stream with a few
// more header bytes carries exactly the same information, but brotli's
// block boundaries move, so a variant that differs from the baseline by
// less than this spread has not been shown to differ at all.
func (c *ctx) jitter(base []byte) {
	lo, hi := 1<<62, 0
	for k := range 8 {
		n := czSize(append(make([]byte, k), base...))
		lo, hi = min(lo, n), max(hi, n)
	}
	fmt.Fprintf(os.Stderr, "  noise floor: the same stream with 0-7 extra header bytes compresses to %d..%d (spread %d)\n", lo, hi, hi-lo)
}

func h0(counts map[int]int, n int) float64 {
	h := 0.0
	for _, k := range counts {
		p := float64(k) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}

func bucket(n int) int {
	switch {
	case n <= 4:
		return n - 1
	case n <= 8:
		return 4
	case n <= 16:
		return 5
	case n <= 64:
		return 6
	}
	return 7
}

var bucketName = [...]string{"1", "2", "3", "4", "5-8", "9-16", "17-64", ">64"}

func gapBucket(n int) int {
	switch {
	case n <= 8:
		return 0
	case n <= 32:
		return 1
	case n <= 128:
		return 2
	case n <= 512:
		return 3
	case n <= 4096:
		return 4
	case n <= 65536:
		return 5
	}
	return 6
}

var gapName = [...]string{"<=8", "9-32", "33-128", "129-512", "513-4K", "4K-64K", ">64K"}

// histograms shows the shape the per-run cost is paid on: how long a run is
// and how far apart runs are, overall and for the four sections that hold
// almost all of them.
func (c *ctx) histograms() {
	secs := c.sections()
	want := map[string]bool{".text": true, ".go.type": true, ".gopclntab": true, ".rodata": true}
	names := []string{"ALL", ".text", ".go.type", ".gopclntab", ".rodata"}
	idx := map[string]int{"ALL": 0}
	for i, n := range names[1:] {
		idx[n] = i + 1
	}
	var runs [5][8]int
	var gaps [5][7]int
	prev := make([]int, 5)
	for _, r := range c.regs {
		k := c.sectionOf(secs, r.s)
		row := -1
		if k >= 0 && want[secs[k].name] {
			row = idx[secs[k].name]
		}
		runs[0][bucket(r.e-r.s)]++
		gaps[0][gapBucket(r.s-prev[0])]++
		prev[0] = r.e
		if row > 0 {
			runs[row][bucket(r.e-r.s)]++
			gaps[row][gapBucket(r.s-prev[row])]++
			prev[row] = r.e
		}
	}
	fmt.Fprintf(os.Stderr, "\n  run-length histogram (regions)\n    %-12s", "")
	for _, b := range bucketName {
		fmt.Fprintf(os.Stderr, "%9s", b)
	}
	fmt.Fprintf(os.Stderr, "%9s\n", "total")
	for i, n := range names {
		fmt.Fprintf(os.Stderr, "    %-12s", n)
		t := 0
		for _, v := range runs[i] {
			fmt.Fprintf(os.Stderr, "%9d", v)
			t += v
		}
		fmt.Fprintf(os.Stderr, "%9d\n", t)
	}
	fmt.Fprintf(os.Stderr, "\n  gap histogram (correct bytes between regions)\n    %-12s", "")
	for _, b := range gapName {
		fmt.Fprintf(os.Stderr, "%9s", b)
	}
	fmt.Fprintln(os.Stderr)
	for i, n := range names {
		fmt.Fprintf(os.Stderr, "    %-12s", n)
		for _, v := range gaps[i] {
			fmt.Fprintf(os.Stderr, "%9d", v)
		}
		fmt.Fprintln(os.Stderr)
	}
}

// bySectionStreams is the shipped format's cost split by section: each
// section's regions encoded as their own group and compressed alone. The
// sum overstates the joint stream -- that is the context the sections share
// -- and the gap between the two is what a per-section split would forfeit.
func (c *ctx) bySectionStreams(o encOpt, title string) {
	o.perSect = true
	_, gs := c.encode(o)
	fmt.Fprintf(os.Stderr, "\n  by section, %s, encoded as its own group (compressed alone)\n", title)
	fmt.Fprintf(os.Stderr, "    %-14s %8s %8s %10s %10s %10s %10s\n",
		"section", "regions", "lz", "ctrl raw", "lit raw", "ctrl cz", "lit cz")
	var tc, tl, tcz, tlz int
	type sj struct {
		g        *sgroup
		czc, czl int
	}
	sjs := make([]sj, len(gs))
	var wg sync.WaitGroup
	gate := make(chan struct{}, max(1, *jobs))
	for i, g := range gs {
		sjs[i].g = g
		wg.Add(1)
		go func(i int, g *sgroup) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			sjs[i].czc, sjs[i].czl = czSize(g.ctrl), czSize(g.lits)
		}(i, g)
	}
	wg.Wait()
	for _, j := range sjs {
		g := j.g
		fmt.Fprintf(os.Stderr, "    %-14s %8d %8d %10d %10d %10d %10d\n",
			g.name, g.nlz+g.nlit, g.nlz, len(g.ctrl), len(g.lits), j.czc, j.czl)
		tc += len(g.ctrl)
		tl += len(g.lits)
		tcz += j.czc
		tlz += j.czl
	}
	fmt.Fprintf(os.Stderr, "    %-14s %8s %8s %10d %10d %10d %10d  (sum %d vs joint %d)\n",
		"TOTAL", "", "", tc, tl, tcz, tlz, tcz+tlz, c.full)
}

// variants prices every candidate representation.
func (c *ctx) variants() {
	word := map[string]bool{".gopclntab": true, ".go.type": true}
	var vs []encOpt
	add := func(o encOpt, name string) {
		o.name = name
		vs = append(vs, o)
	}
	b := baseOpt()
	add(b, "base (shipped)")

	col := b
	col.columnar = true
	add(col, "columnar")

	ps := b
	ps.perSect = true
	add(ps, "per-section")

	psc := ps
	psc.columnar = true
	add(psc, "per-section + columnar")

	pscs := psc
	pscs.stream = true
	add(pscs, "per-section + columnar, stream-major")

	x := b
	x.xor = true
	add(x, "xor literals")

	xc := x
	xc.columnar = true
	add(xc, "xor + columnar")

	xps := x
	xps.perSect, xps.columnar = true, true
	add(xps, "xor + per-section + columnar")

	w := ps
	w.word = word
	add(w, "word mode (pcln, type)")

	wx := w
	wx.xor = true
	add(wx, "word mode + xor")

	wxc := wx
	wxc.columnar = true
	add(wxc, "word mode + xor + columnar")

	for _, m := range []int{1, 2, 3, 4, 5, 8, 12, 16, 20, 24, 28, 32, 40, 48, 64, 96, 128} {
		o := b
		o.merge = m
		add(o, fmt.Sprintf("merge=%d", m))
		o2 := x
		o2.merge = m
		add(o2, fmt.Sprintf("merge=%d + xor", m))
	}
	for _, m := range []int{16, 24, 32, 48} {
		o := xc
		o.merge = m
		add(o, fmt.Sprintf("merge=%d + xor + columnar", m))
		o2 := xps
		o2.merge = m
		add(o2, fmt.Sprintf("merge=%d + xor + per-section + columnar", m))
		o3 := wx
		o3.merge = m
		add(o3, fmt.Sprintf("merge=%d + word mode + xor", m))
	}
	// (d) asks whether xor helps the pointer- and offset-shaped fields
	// specifically, so xor is restricted to those sections and to the
	// word-mode groups.
	for _, sel := range []struct {
		name string
		in   map[string]bool
	}{
		{".gopclntab", map[string]bool{".gopclntab": true}},
		{".gopclntab+.go.type", word},
		{".text", map[string]bool{".text": true}},
	} {
		o := ps
		o.xorIn = sel.in
		add(o, "per-section, xor in "+sel.name)
		o2 := w
		o2.xorIn = sel.in
		add(o2, "word mode, xor in "+sel.name)
	}

	// the best of the grid, combined
	for _, m := range []int{3, 4, 5} {
		o := w
		o.xorIn, o.merge = map[string]bool{".gopclntab": true}, m
		add(o, fmt.Sprintf("word mode + xor in .gopclntab + merge=%d", m))
		o2 := ps
		o2.xorIn, o2.merge = map[string]bool{".gopclntab": true}, m
		add(o2, fmt.Sprintf("per-section + xor in .gopclntab + merge=%d", m))
	}

	for _, th := range []int{50, 75, 90} {
		for _, m := range []int{delta.MergeGap, 4, 8, 16, 32} {
			o := b
			o.auto, o.merge = th, m
			add(o, fmt.Sprintf("auto-xor>=%d%%, merge=%d", th, m))
		}
	}
	add(encOpt{whole: true, xor: true}, "whole-file xor (the merge limit)")

	res := make([]cres, len(vs))
	var wg sync.WaitGroup
	gate := make(chan struct{}, max(1, *jobs))
	bufs := make(chan []byte, max(1, *jobs))
	for i, o := range vs {
		wg.Add(1)
		go func(i int, o encOpt) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			raw, gs := c.encode(o)
			r := cres{o: o, raw: len(raw)}
			for _, g := range gs {
				r.nxor += g.nxor
				r.nreg += g.nlz + g.nlit
				r.ctrlRaw += len(g.ctrl) + len(g.gaps) + len(g.spans) + len(g.ops)
				r.litRaw += len(g.lits)
			}
			r.cz = czSize(raw)
			if *xzCtl {
				r.xz = xzSize(raw)
			}
			var buf []byte
			select {
			case buf = <-bufs:
			default:
				buf = make([]byte, len(c.new))
			}
			if err := c.decode(raw, buf); err != nil {
				r.verified = "DECODE FAILED: " + err.Error()
			} else {
				r.verified = "ok"
			}
			select {
			case bufs <- buf:
			default:
			}
			res[i] = r
		}(i, o)
	}
	wg.Wait()

	b0 := res[0].cz
	x0 := res[0].xz
	fmt.Fprintf(os.Stderr, "\n  encodings (same information, compressed by the codec's own compressor)\n")
	fmt.Fprintf(os.Stderr, "    %-38s %8s %10s %10s %9s %10s %9s  %s\n",
		"encoding", "regions", "ctrl raw", "lit raw", "raw", "cz", "d cz", "xz / d xz")
	for _, r := range res {
		xzs := ""
		if r.xz > 0 {
			xzs = fmt.Sprintf("%d / %+d", r.xz, r.xz-x0)
		}
		note := ""
		if r.verified != "ok" {
			note = "  " + r.verified
		}
		if r.nxor > 0 {
			note = fmt.Sprintf("  (%d xor regions)%s", r.nxor, note)
		}
		fmt.Fprintf(os.Stderr, "    %-38s %8d %10d %10d %9d %10d %+9d  %s%s\n",
			r.o.name, r.nreg, r.ctrlRaw, r.litRaw, r.raw, r.cz, r.cz-b0, xzs, note)
	}
}
