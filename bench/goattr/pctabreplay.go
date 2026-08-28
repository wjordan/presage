package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/wjordan/go-binsync/delta"
	"github.com/wjordan/go-binsync/delta/gobin"
)

// ---------------------------------------------------------------- level 'a'
//
// The pctab replay probe. Level 8 shows the _func pcsp/pcfile/pcln/pcdata
// offsets are the bulk of what is left of .gopclntab: for a function the
// release added the codec has no old record to re-target, so it fills those
// fields from the modal record, which is right only by accident.
//
// cmd/link/internal/ld.generatePctab allocates pctab strictly sequentially in
// function order -- one pad byte, then per function pcsp, pcfile, pcline,
// pcdata[0..n), pcinline -- and skips a table it has already emitted, because
// pc-value symbols are content-addressable (cmd/internal/obj: AttrPcdata |
// AttrContentAddressable). Two consequences, and this level measures both:
// a table whose content is new lands at the running high-water mark, and a
// table whose content repeats lands wherever its twin already is.
//
// The counterfactual is priced end to end: a rule's values are written into a
// copy of the prediction and the whole correction is re-encoded and
// re-compressed, so the number is patch bytes, not a yardstick estimate.

// pcSlotOrder returns the byte offsets, within a _func record, of the record's
// pc-table slots in the order generatePctab allocates them. Slot 2 is
// PCDATA_InlTreeIndex, which the linker never fills from a compiler pcdata
// symbol: it is either zero or pcinline, which is allocated after every other
// table of the same function.
func pcSlotOrder(npc uint32) []int {
	o := make([]int, 0, 3+int(npc))
	o = append(o, 16, 20, 24) // pcsp, pcfile, pcln
	for k := uint32(0); k < npc; k++ {
		if k != 2 {
			o = append(o, gobin.FuncSize+4*int(k))
		}
	}
	if npc > 2 {
		o = append(o, gobin.FuncSize+8)
	}
	return o
}

func slotName(o int) string {
	switch o {
	case 16:
		return "pcsp"
	case 20:
		return "pcfile"
	case 24:
		return "pcln"
	}
	return fmt.Sprintf("pcdata[%d]", (o-gobin.FuncSize)/4)
}

// probeNameKey mirrors delta.nameKey, which is unexported: the receiver form
// and the last dot-separated component with trailing digits stripped.
func probeNameKey(n string) string {
	if strings.HasSuffix(n, "-fm") {
		return "fm"
	}
	d, depth := -1, 0
	for i := 0; i < len(n); i++ {
		switch n[i] {
		case '[':
			depth++
		case ']':
			depth--
		case '.':
			if depth == 0 {
				d = i
			}
		}
	}
	last := n[d+1:]
	for len(last) > 0 && last[len(last)-1] >= '0' && last[len(last)-1] <= '9' {
		last = last[:len(last)-1]
	}
	if d < 0 {
		return last
	}
	rec := n[strings.LastIndexByte(n[:d], '.')+1 : d]
	if strings.HasPrefix(rec, "(*") {
		return "P|" + last
	}
	return last
}

// site is one _func pc-table slot the codec has to invent: every slot of a
// function the release added, plus the slots a reshape appended.
type site struct {
	o        int
	p        int // file offset of the slot word
	want     uint32
	hiBefore uint32
	reshaped bool
}

// rstate is what a rule may look at: the running high-water mark in pctab,
// the last value each name key used for each slot, and the last value any
// function used for each slot. All three are decoder-available -- they are fed
// from the matched functions' re-targeted offsets and from the rule's own
// earlier answers, never from the new records.
type rstate struct {
	hi    uint32
	key   string
	byKey map[string]map[int]uint32
	prev  map[int]uint32
}

func (s *rstate) keyed(o int) (uint32, bool) {
	m := s.byKey[s.key]
	if m == nil {
		return 0, false
	}
	v, ok := m[o]
	return v, ok
}

type pcrepl struct {
	c      *ctx
	sites  []site
	fbase  int
	ptab   []byte
	nto    []int
	hiAt   map[int]uint32
	trueAt func(int) uint32
	predAt func(int) uint32
	known  func(j, o int, npc uint32) bool
}

// pcSpan returns the total pc range a pc-value table covers. Every table of a
// function covers exactly the function's text, so this identifies which
// function a table in pctab belongs to -- and the decoder knows every new
// function's size from the layout.
func pcSpan(tab []byte, off int) uint32 {
	p, span, first := off, uint32(0), true
	for p < len(tab) {
		if tab[p] == 0 && !first {
			return span
		}
		for p < len(tab) && tab[p]&0x80 != 0 {
			p++
		}
		p++
		d, sh := uint32(0), uint(0)
		for p < len(tab) && tab[p]&0x80 != 0 {
			d |= uint32(tab[p]&0x7f) << sh
			sh += 7
			p++
		}
		if p < len(tab) {
			d |= uint32(tab[p]) << sh
		}
		p++
		span += d
		first = false
	}
	return span
}

func (r *pcrepl) end(off uint32) uint32 {
	if off == 0 || int(off) >= len(r.ptab) {
		return off
	}
	return off + uint32(gobin.PcTableLen(r.ptab, int(off)))
}

// pass walks every function in link order, feeding pick at the slots the codec
// has to invent and the (mapped) truth at the ones it re-targets.
func (r *pcrepl) pass(pick func(s *rstate, o int, base uint32) uint32) []uint32 {
	st := &rstate{hi: 1, byKey: map[string]map[int]uint32{}, prev: map[int]uint32{}}
	out := make([]uint32, 0, len(r.sites))
	for j, g := range r.c.nb.Funcs {
		npc, _, _ := r.c.nb.Pcln.Record(g.FuncOff)
		st.key = probeNameKey(g.Name)
		for _, o := range pcSlotOrder(npc) {
			p := r.fbase + int(g.FuncOff) + o
			var v uint32
			if r.known(j, o, npc) {
				v = r.trueAt(p)
			} else {
				v = pick(st, o, r.predAt(p))
				out = append(out, v)
			}
			if e := r.end(v); e > st.hi {
				st.hi = e
			}
			if st.byKey[st.key] == nil {
				st.byKey[st.key] = map[int]uint32{}
			}
			st.byKey[st.key][o] = v
			st.prev[o] = v
		}
	}
	return out
}

func wrongBytes32(a, b uint32) int {
	n := 0
	for i := range 4 {
		if byte(a>>(8*i)) != byte(b>>(8*i)) {
			n++
		}
	}
	return n
}

// score prices one rule end to end and reports how many slots it gets right.
func (r *pcrepl) score(name string, v []uint32) {
	buf := append([]byte(nil), r.c.pred...)
	ok, wb := 0, 0
	for i, s := range r.sites {
		binary.LittleEndian.PutUint32(buf[s.p:], v[i])
		if v[i] == s.want {
			ok++
		}
		wb += wrongBytes32(v[i], s.want)
	}
	enc, err := delta.EncodeCorrection(buf, r.c.new)
	if err != nil {
		panic(err)
	}
	n := czSize(enc)
	fmt.Fprintf(os.Stderr, "  %-28s %8s %10s %11s %11s\n", name, pct(ok, len(r.sites)), commas(wb), commas(n), commas(r.c.full-n))
}

func (c *ctx) pctabReplay() {
	sec := c.nb.Sects[".gopclntab"]
	if sec == nil {
		fmt.Fprintf(os.Stderr, "\n-- a. no .gopclntab\n")
		return
	}
	np := c.nb.Pcln
	r := &pcrepl{
		c:     c,
		fbase: int(sec.Off) + int(np.Functab.Off),
		ptab:  np.Table(np.Pctab),
		nto:   c.pr.NewToOld,
	}
	r.trueAt = func(p int) uint32 { return binary.LittleEndian.Uint32(c.new[p:]) }
	r.predAt = func(p int) uint32 { return binary.LittleEndian.Uint32(c.pred[p:]) }
	// A slot is re-targeted when the function matched and the slot existed in
	// the old record; every other slot is invented.
	r.known = func(j, o int, npc uint32) bool {
		i := r.nto[j]
		if i < 0 {
			return false
		}
		if o < gobin.FuncSize {
			return true
		}
		onpc, _, _ := c.ob.Pcln.Record(c.ob.Funcs[i].FuncOff)
		return uint32((o-gobin.FuncSize)/4) < onpc
	}

	// pass 1: the true high-water mark, so each invented slot can be called a
	// fresh allocation or a backward dedup.
	hi := uint32(1)
	for j, g := range c.nb.Funcs {
		npc, _, _ := np.Record(g.FuncOff)
		for _, o := range pcSlotOrder(npc) {
			p := r.fbase + int(g.FuncOff) + o
			v := r.trueAt(p)
			if !r.known(j, o, npc) {
				r.sites = append(r.sites, site{o, p, v, hi, r.nto[j] >= 0})
			}
			if e := r.end(v); e > hi {
				hi = e
			}
		}
	}

	type stat struct{ n, zero, fresh, dedup, baseOK, keyOK, prevOK int }
	st := map[string]*stat{}
	base := r.pass(func(s *rstate, o int, b uint32) uint32 { return b })
	keyv := r.pass(func(s *rstate, o int, b uint32) uint32 {
		if v, ok := s.keyed(o); ok {
			return v
		}
		return b
	})
	prevv := r.pass(func(s *rstate, o int, b uint32) uint32 {
		if v, ok := s.prev[o]; ok {
			return v
		}
		return b
	})
	for i, s := range r.sites {
		n := slotName(s.o)
		if s.reshaped {
			n = "reshaped " + n
		}
		if st[n] == nil {
			st[n] = &stat{}
		}
		t := st[n]
		t.n++
		switch {
		case s.want == 0:
			t.zero++
		case s.want >= s.hiBefore:
			t.fresh++
		default:
			t.dedup++
		}
		if base[i] == s.want {
			t.baseOK++
		}
		if keyv[i] == s.want {
			t.keyOK++
		}
		if prevv[i] == s.want {
			t.prevOK++
		}
	}
	fmt.Fprintf(os.Stderr, "\n-- a. pctab: the %s _func pc-table slots the codec has to invent\n", commas(len(r.sites)))
	fmt.Fprintf(os.Stderr, "  %-18s %8s %8s %8s %8s %9s %9s %9s\n",
		"slot", "slots", "true=0", "fresh", "dedup", "modal ok", "key ok", "prev ok")
	names := make([]string, 0, len(st))
	for n := range st {
		names = append(names, n)
	}
	sort.Strings(names)
	tot := stat{}
	for _, n := range names {
		t := st[n]
		fmt.Fprintf(os.Stderr, "  %-18s %8d %8d %8d %8d %9s %9s %9s\n",
			n, t.n, t.zero, t.fresh, t.dedup, pct(t.baseOK, t.n), pct(t.keyOK, t.n), pct(t.prevOK, t.n))
		tot.n += t.n
		tot.zero += t.zero
		tot.fresh += t.fresh
		tot.dedup += t.dedup
		tot.baseOK += t.baseOK
		tot.keyOK += t.keyOK
		tot.prevOK += t.prevOK
	}
	fmt.Fprintf(os.Stderr, "  %-18s %8d %8d %8d %8d %9s %9s %9s\n",
		"TOTAL", tot.n, tot.zero, tot.fresh, tot.dedup, pct(tot.baseOK, tot.n), pct(tot.keyOK, tot.n), pct(tot.prevOK, tot.n))

	// the rules, priced.
	fmt.Fprintf(os.Stderr, "\n  %-28s %8s %10s %11s %11s\n", "rule", "slots ok", "wrong B", "correction", "saved")
	r.score("modal record (shipped)", base)
	r.score("nearest same-key function", keyv)
	r.score("previous function", prevv)
	// cursor: every invented slot takes the next unallocated table.
	r.score("high-water cursor", r.pass(func(s *rstate, o int, b uint32) uint32 {
		if b == 0 {
			return 0
		}
		return s.hi
	}))
	// key, with the cursor for pcln -- the one table that is almost never a
	// repeat -- and for any slot the key has not seen.
	r.score("key + cursor for pcln", r.pass(func(s *rstate, o int, b uint32) uint32 {
		if o == 24 {
			return s.hi
		}
		if v, ok := s.keyed(o); ok {
			return v
		}
		if b == 0 {
			return 0
		}
		return s.hi
	}))
	r.score("key, cursor when unseen", r.pass(func(s *rstate, o int, b uint32) uint32 {
		if v, ok := s.keyed(o); ok {
			return v
		}
		if b == 0 {
			return 0
		}
		return s.hi
	}))

	// the strongest form: the tables at the cursor whose pc span is this
	// function's own are counted first, then handed to the slots the old
	// binary says are likeliest to be a fresh allocation, in slot order.
	prior := freshPrior(c.ob)
	block := func(pad uint32) []uint32 {
		st := &rstate{hi: 1}
		out := make([]uint32, 0, len(r.sites))
		for j, g := range c.nb.Funcs {
			npc, _, _ := np.Record(g.FuncOff)
			order := pcSlotOrder(npc)
			sz := uint32(g.End - g.Entry)
			var unk []int
			for _, o := range order {
				if !r.known(j, o, npc) && r.predAt(r.fbase+int(g.FuncOff)+o) != 0 {
					unk = append(unk, o)
				}
			}
			fit := func(off uint32) bool {
				if int(off) >= len(r.ptab) {
					return false
				}
				sp := pcSpan(r.ptab, int(off))
				return sp <= sz && sz-sp <= pad
			}
			take := map[int]uint32{}
			if len(unk) > 0 {
				for range 8 { // skip what the function before left behind
					if fit(st.hi) {
						break
					}
					st.hi = r.end(st.hi)
				}
				var offs []uint32
				cur := st.hi
				for len(offs) < len(unk) && fit(cur) {
					offs = append(offs, cur)
					cur = r.end(cur)
				}
				pick := append([]int(nil), unk...)
				sort.SliceStable(pick, func(a, b int) bool { return prior[pick[a]] > prior[pick[b]] })
				pick = pick[:len(offs)]
				sort.Ints(pick)
				for i, o := range pick {
					take[o] = offs[i]
				}
			}
			for _, o := range order {
				pp := r.fbase + int(g.FuncOff) + o
				var v uint32
				if r.known(j, o, npc) {
					v = r.trueAt(pp)
				} else if x, ok := take[o]; ok {
					v = x
					out = append(out, v)
				} else {
					out = append(out, r.predAt(pp))
					continue
				}
				if e := r.end(v); e > st.hi {
					st.hi = e
				}
			}
		}
		return out
	}
	r.score("size-gated block + priors", block(48))

	// precision over recall: only touch a function whose span-matched block
	// holds exactly one table, and give it to pcln -- the one slot that is
	// almost always a fresh allocation.
	lone := func() []uint32 {
		st := &rstate{hi: 1}
		out := make([]uint32, 0, len(r.sites))
		var hit, fired int
		for j, g := range c.nb.Funcs {
			npc, _, _ := np.Record(g.FuncOff)
			order := pcSlotOrder(npc)
			sz := uint32(g.End - g.Entry)
			unk := false
			for _, o := range order {
				if !r.known(j, o, npc) {
					unk = true
				}
			}
			fit := func(off uint32) bool {
				if int(off) >= len(r.ptab) {
					return false
				}
				sp := pcSpan(r.ptab, int(off))
				return sp <= sz && sz-sp <= 48
			}
			var give uint32
			if unk {
				for range 8 {
					if fit(st.hi) {
						break
					}
					st.hi = r.end(st.hi)
				}
				if fit(st.hi) && !fit(r.end(st.hi)) {
					give = st.hi
				}
			}
			for _, o := range order {
				pp := r.fbase + int(g.FuncOff) + o
				var v uint32
				if r.known(j, o, npc) {
					v = r.trueAt(pp)
				} else if o == 24 && give != 0 {
					v = give
					fired++
					if v == r.trueAt(pp) {
						hit++
					}
					out = append(out, v)
				} else {
					out = append(out, r.predAt(pp))
					continue
				}
				if e := r.end(v); e > st.hi {
					st.hi = e
				}
			}
		}
		fmt.Fprintf(os.Stderr, "  lone-table rule fired on %d pcln slots, %s exact\n", fired, pct(hit, max(1, fired)))
		return out
	}
	r.score("lone table -> pcln", lone())

	// how close a table's pc span is to the function's spacing: Func.End is the
	// next function's entry, so the difference is the alignment padding.
	{
		d := map[int]int{}
		for j, g := range c.nb.Funcs {
			npc, _, _ := np.Record(g.FuncOff)
			for _, o := range pcSlotOrder(npc) {
				if r.known(j, o, npc) {
					continue
				}
				v := r.trueAt(r.fbase + int(g.FuncOff) + o)
				if v == 0 || v < r.sitesHi(j, o) {
					continue
				}
				d[int(g.End-g.Entry)-int(pcSpan(r.ptab, int(v)))]++
			}
		}
		type kv struct{ d, n int }
		var ks []kv
		for k, n := range d {
			ks = append(ks, kv{k, n})
		}
		sort.Slice(ks, func(i, j int) bool { return ks[i].n > ks[j].n })
		out := ""
		for i, k := range ks {
			if i < 8 {
				out += fmt.Sprintf("%d:%d  ", k.d, k.n)
			}
		}
		fmt.Fprintf(os.Stderr, "  (End-Entry) - table span over fresh slots, %d distinct: %s\n", len(ks), out)
	}

	// how well the emission-order model itself holds: a fresh slot should land
	// exactly on the high-water mark, never past it.
	exact, past := 0, 0
	for _, s := range r.sites {
		if s.want != 0 && s.want >= s.hiBefore {
			if s.want == s.hiBefore {
				exact++
			} else {
				past++
			}
		}
	}
	fmt.Fprintf(os.Stderr, "  emission-order model: %d of %d fresh slots land exactly on the high-water mark, %d past it\n",
		exact, exact+past, past)

	// the ceiling, and the two halves it splits into: a replay can only ever
	// reach the slots the linker allocated fresh; the ones it deduped
	// backwards point at a table chosen by content, which nothing in the
	// decoder's hands identifies.
	part := func(name string, keep func(site) bool) {
		v := make([]uint32, len(r.sites))
		for i, s := range r.sites {
			if keep(s) {
				v[i] = s.want
			} else {
				v[i] = base[i]
			}
		}
		r.score(name, v)
	}
	part("oracle (ceiling)", func(site) bool { return true })
	part("oracle: fresh slots only", func(s site) bool { return s.want != 0 && s.want >= s.hiBefore })
	part("oracle: pcln only", func(s site) bool { return s.o == 24 })
	part("oracle: dedup slots only", func(s site) bool { return s.want != 0 && s.want < s.hiBefore })

	// Since a fresh slot always lands exactly on the high-water mark, knowing
	// which slots are fresh is the whole problem: a transmitted mask would
	// make the replay exact on every one of them. This is what that mask
	// would cost, one bit per invented slot in link order.
	mask := make([]byte, (len(r.sites)+7)/8)
	for i, s := range r.sites {
		if s.want != 0 && s.want >= s.hiBefore {
			mask[i/8] |= 1 << (i % 8)
		}
	}
	fmt.Fprintf(os.Stderr, "  a transmitted fresh/dedup mask: %s B raw, %s compressed\n",
		commas(len(mask)), commas(czSize(mask)))
	fmt.Fprintf(os.Stderr, "  yardstick for reference: %.3f per run + %.3f per wrong byte\n", fitA, fitB)
}

// sitesHi is the true high-water mark recorded for the site at (j, o); it is
// only used by the diagnostics.
func (r *pcrepl) sitesHi(j, o int) uint32 {
	if r.hiAt == nil {
		r.hiAt = make(map[int]uint32, len(r.sites))
		for _, s := range r.sites {
			r.hiAt[s.p] = s.hiBefore
		}
	}
	return r.hiAt[r.fbase+int(r.c.nb.Funcs[j].FuncOff)+o]
}

// freshPrior measures, on the old binary, how often each _func pc-table slot
// is a table the linker allocated fresh rather than one it deduped backwards.
// The decoder can compute it from the old binary alone, so it costs nothing.
func freshPrior(b *gobin.Bin) map[int]float64 {
	p := b.Pcln
	ptab, ftab := p.Table(p.Pctab), p.Table(p.Functab)
	n, f := map[int]int{}, map[int]int{}
	hi := uint32(1)
	for _, g := range b.Funcs {
		npc, _, _ := p.Record(g.FuncOff)
		for _, o := range pcSlotOrder(npc) {
			v := binary.LittleEndian.Uint32(ftab[uint64(g.FuncOff)+uint64(o):])
			n[o]++
			if v != 0 && v >= hi {
				f[o]++
			}
			if v != 0 && int(v) < len(ptab) {
				if e := v + uint32(gobin.PcTableLen(ptab, int(v))); e > hi {
					hi = e
				}
			}
		}
	}
	out := map[int]float64{}
	for o, c := range n {
		out[o] = float64(f[o]) / float64(c)
	}
	return out
}
