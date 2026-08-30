package delta

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/wjordan/presage/delta/gobin"
)

// gfBlock and gfTol are the content-map parameters for the stage-1b tables.
// A tolerance is worth having there because a pc-value table's own bytes can
// change by a byte or two without the table moving.
const (
	gfBlock = 16
	gfTol   = 4
)

// predictPcln rebuilds .gopclntab for the new binary. Everything except the
// stage-1 tables is derived: the functab and its _func records are the old
// records re-laid in the new function order with their entry offsets, name
// offsets, pc-table offsets, funcdata offsets and file indexes re-targeted,
// and findfunctab is regenerated outright from the function list.
//
// new is the skeleton; its pclntab already holds the stage-1a and 1b tables
// at their final offsets, and bp says where each old pctab and go:func.*
// offset landed in the 1b prediction. The two hops -- old to predicted
// (exact, from the emulated layout) and predicted to final (a content map of
// two nearly identical blobs) -- are what make the offsets in the records
// come out right without transmitting any of them.
func predictPcln(old, new *gobin.Bin, m *match, l *layout, bp *blobPred) []byte {
	op, np := old.Pcln, new.Pcln
	nfunc := len(new.Funcs)

	shapes := make(map[int][2]uint32, len(l.RecShapes))
	for _, r := range l.RecShapes {
		shapes[r.Idx] = [2]uint32{r.Npcdata, r.Nfuncdata}
	}

	nameOff := resolveNameOffs(old, new, m)
	cuMap := buildDataMap(bp.Cutab, np.Table(np.Cutab), gfBlock, nil, 0)
	pcMap := buildDataMap(bp.Pctab, np.Table(np.Pctab), gfBlock, nil, gfTol)
	gfMap := buildDataMap(bp.Gofunc, new.Gofunc(), gfBlock, nil, gfTol)
	mapPc := func(x uint32) uint32 {
		if p, ok := bp.PcOff[x]; ok {
			return uint32(pcMap.Map(uint64(p)))
		}
		return x
	}
	mapGf := func(x uint32) uint32 {
		if p, ok := bp.GfOff[x]; ok {
			return uint32(gfMap.Map(uint64(p)))
		}
		return x
	}

	mode := gobin.ModalShape(old)
	oft := op.Table(op.Functab)
	hdr := uint64(nfunc*8 + 4)
	recOff := make([]uint64, nfunc)
	recs := make([][]byte, nfunc)

	// Pass 1: the matched functions' records, re-targeted. The functions the
	// release added are filled from these, so they have to exist first.
	for j := range new.Funcs {
		i := m.NewToOld[j]
		if i < 0 {
			continue
		}
		f := old.Funcs[i]
		npc, nfd, sz := op.Record(f.FuncOff)
		rec := append([]byte(nil), oft[f.FuncOff:uint64(f.FuncOff)+uint64(sz)]...)
		for _, o := range []int{16, 20, 24} { // pcsp, pcfile, pcln
			if x := binary.LittleEndian.Uint32(rec[o:]); x != 0 {
				binary.LittleEndian.PutUint32(rec[o:], mapPc(x))
			}
		}
		for k := range int(npc) {
			o := gobin.FuncSize + 4*k
			if x := binary.LittleEndian.Uint32(rec[o:]); x != 0 {
				binary.LittleEndian.PutUint32(rec[o:], mapPc(x))
			}
		}
		for k := range int(nfd) {
			o := gobin.FuncSize + 4*int(npc) + 4*k
			if x := binary.LittleEndian.Uint32(rec[o:]); x != ^uint32(0) {
				binary.LittleEndian.PutUint32(rec[o:], mapGf(x))
			}
		}
		if x := binary.LittleEndian.Uint32(rec[32:]); x != ^uint32(0) { // cuOffset
			binary.LittleEndian.PutUint32(rec[32:], uint32(cuMap.Map(uint64(x)*4)/4))
		}
		recs[j] = rec
	}
	tpl := modalRecord(recs)
	tpl.learnFuncID(old, oft)

	// Pass 2: reshape what the layout says changed shape, synthesise the
	// rest from the template, fill the invented pc-table slots from the
	// pctab cursor, and lay the records out.
	cur := &pcCursor{tab: np.Table(np.Pctab), off: 1, mask: l.PcFresh, nbit: l.NPcFresh, gaps: l.PcGaps}
	size := hdr
	var prevCU uint32
	for j, g := range new.Funcs {
		rec := recs[j]
		keep := -1 // pcdata slots inherited from the old record; -1 = the record is new
		if rec != nil {
			npc, nfd := binary.LittleEndian.Uint32(rec[28:]), uint32(rec[43])
			keep = int(npc)
			if sh, ok := shapes[j]; ok && sh != [2]uint32{npc, nfd} {
				rec = reshape(rec, npc, nfd, sh, tpl)
			}
			prevCU = binary.LittleEndian.Uint32(rec[32:])
		} else {
			npc, nfd := mode[0], mode[1]
			if sh, ok := shapes[j]; ok {
				npc, nfd = sh[0], sh[1]
			}
			rec = tpl.synth(npc, nfd, prevCU, g.Name)
		}
		cur.fill(rec, keep)
		binary.LittleEndian.PutUint32(rec[0:], uint32(g.Entry-new.Mod.Text))
		binary.LittleEndian.PutUint32(rec[4:], uint32(nameOff[j]))
		size = (size + 7) &^ 7
		recOff[j] = size
		recs[j] = rec
		size += uint64(len(rec))
	}
	functab := make([]byte, size)
	for j, g := range new.Funcs {
		binary.LittleEndian.PutUint32(functab[8*j:], uint32(g.Entry-new.Mod.Text))
		binary.LittleEndian.PutUint32(functab[8*j+4:], uint32(recOff[j]))
		copy(functab[recOff[j]:], recs[j])
	}
	binary.LittleEndian.PutUint32(functab[8*nfunc:], uint32(new.Funcs[nfunc-1].End-new.Mod.Text))

	// assemble: header, then each table at the offset the layout gives it,
	// which is what makes the prediction length-exact
	out := make([]byte, l.PclnLen)
	copy(out, op.Data[:op.FuncnameOff])
	place := func(tab []byte, at uint64) {
		if at+uint64(len(tab)) > uint64(len(out)) {
			tab = tab[:uint64(len(out))-min(at, uint64(len(out)))]
		}
		copy(out[at:], tab)
	}
	t := l.TabOff
	place(np.Table(np.Funcnametab), t[0])
	place(np.Table(np.Cutab), t[1])
	place(np.Table(np.Filetab), t[2])
	place(np.Table(np.Pctab), t[3])
	place(functab, t[4])
	place(new.Gofunc(), t[5])
	place(gobin.GenFindfunctab(new.Funcs), t[6])

	binary.LittleEndian.PutUint64(out[8:], uint64(nfunc))
	binary.LittleEndian.PutUint64(out[16:], l.NFiles)
	// pcHeader.textStart is 0 in the supported release -- the runtime takes
	// text from moduledata -- so it stays whatever the old header had.
	for k, off := range t[:5] {
		binary.LittleEndian.PutUint64(out[32+8*k:], off)
	}
	return out
}

// The pctab replay (docs/go-module-design.md 2.2.2).
//
// cmd/link/internal/ld.generatePctab lays pctab out as the distinct pc-value
// tables in allocation order, starting at offset 1: per function pcsp,
// pcfile, pcln, pcdata[0], pcdata[1], pcdata[3..n) and finally pcinline,
// which writeFuncs stores in pcdata[2]. A table whose content is new lands on
// the running high-water mark and one whose content repeats lands wherever
// its twin already is, so a slot the codec has to invent -- every pc-table
// slot of a function the release added, and the pcdata slots a reshape
// appends -- is either the next unallocated table or unknowable. The layout
// says which, one bit per slot, plus one gap per record saying how many
// tables the linker allocated in between.

// eachPcSlot calls f with the offsets, within a _func record with npc pcdata
// slots, of the record's pc-table slots in the linker's allocation order.
func eachPcSlot(npc uint32, f func(o int)) {
	f(16) // pcsp
	f(20) // pcfile
	f(24) // pcln
	for k := uint32(0); k < npc; k++ {
		if k != 2 {
			f(gobin.FuncSize + 4*int(k))
		}
	}
	if npc > 2 {
		f(gobin.FuncSize + 8) // pcinline, allocated after the function's others
	}
}

// pcCursor walks the tables of pctab in allocation order. off is the table
// the next fresh allocation lands on; a record's gap skips the tables the
// functions in between claimed, and each slot the mask calls fresh takes off
// and steps past it. Nothing here reads a predicted offset, so the cursor
// cannot be led astray by a mispredicted record.
type pcCursor struct {
	tab  []byte
	off  uint32
	mask []byte
	nbit int
	i    int
	gaps []uint32
	g    int
}

// step moves off past the table it stands on. pc-value tables are
// self-delimiting, so the length is parsed out of pctab.
func (c *pcCursor) step() {
	if int(c.off) >= len(c.tab) {
		return
	}
	c.off += uint32(gobin.PcTableLen(c.tab, int(c.off)))
}

// skip advances n tables, and stops at the end of pctab however large a
// corrupt n is.
func (c *pcCursor) skip(n uint32) {
	for i := uint32(0); i < n && int(c.off) < len(c.tab); i++ {
		c.step()
	}
}

// end is where the table at off finishes; offset 0 is the empty table and
// consumes nothing.
func (c *pcCursor) end(off uint32) uint32 {
	if off == 0 || int(off) >= len(c.tab) {
		return off
	}
	return off + uint32(gobin.PcTableLen(c.tab, int(off)))
}

// bit reads the next mask bit. A bit past the end of the mask is not fresh,
// so a short mask costs bytes rather than indexing out of range.
func (c *pcCursor) bit() bool {
	k := c.i
	c.i++
	return k < c.nbit && c.mask[k/8]&(1<<(k%8)) != 0
}

func (c *pcCursor) set(v bool) {
	if c.i%8 == 0 {
		c.mask = append(c.mask, 0)
	}
	if v {
		c.mask[c.i/8] |= 1 << (c.i % 8)
	}
	c.i++
}

// fill is the decoder's half: it gives every invented slot of one record the
// cursor's table when the mask says the linker allocated one there. keep is
// how many leading pcdata slots the record inherited from its old
// counterpart, or -1 when the whole record was synthesised.
func (c *pcCursor) fill(rec []byte, keep int) {
	first := true
	eachPcSlot(binary.LittleEndian.Uint32(rec[28:]), func(o int) {
		if keep >= 0 && (o < gobin.FuncSize || (o-gobin.FuncSize)/4 < keep) {
			return
		}
		if first {
			first = false
			if c.g < len(c.gaps) {
				c.skip(c.gaps[c.g])
			}
			c.g++
		}
		if c.bit() {
			binary.LittleEndian.PutUint32(rec[o:], c.off)
			c.step()
		}
	})
}

// eachRecShape calls f for every new function with the pcdata count the
// decoder will give its record and how many pcdata slots that record
// inherits, which is the one place the set of invented slots is decided.
func eachRecShape(old *gobin.Bin, l *layout, m *match, f func(j int, npc uint32, keep int)) {
	shapes := make(map[int]uint32, len(l.RecShapes))
	for _, r := range l.RecShapes {
		shapes[r.Idx] = r.Npcdata
	}
	mode := gobin.ModalShape(old)
	for j, i := range m.NewToOld {
		npc, keep := mode[0], -1
		if i >= 0 {
			onpc, _, _ := old.Pcln.Record(old.Funcs[i].FuncOff)
			npc, keep = onpc, int(onpc)
		}
		if sh, ok := shapes[j]; ok {
			npc = sh
		}
		f(j, npc, keep)
	}
}

// pcReplayShape counts what the layout's replay fields must hold: one bit
// per invented slot and one gap per record that has any. The decoder checks
// both before it trusts them.
func pcReplayShape(old *gobin.Bin, l *layout, m *match) (slots, gaps int) {
	eachRecShape(old, l, m, func(j int, npc uint32, keep int) {
		n := 0
		eachPcSlot(npc, func(o int) {
			if keep < 0 || o >= gobin.FuncSize && (o-gobin.FuncSize)/4 >= keep {
				n++
			}
		})
		if n > 0 {
			gaps++
		}
		slots += n
	})
	return slots, gaps
}

// checkPcReplay is the rest of the decoder's bounds check: the mask must
// hold one bit per slot the layout says the codec invents, and one gap per
// record that has such a slot. A pair that added no function sends neither.
func checkPcReplay(old *gobin.Bin, l *layout, m *match) error {
	if l.NPcFresh == 0 && len(l.PcGaps) == 0 {
		return nil
	}
	if slots, gaps := pcReplayShape(old, l, m); l.NPcFresh != slots || len(l.PcGaps) != gaps {
		return fmt.Errorf("%w: the pctab replay carries %d slots and %d gaps, the layout implies %d and %d",
			errCorrupt, l.NPcFresh, len(l.PcGaps), slots, gaps)
	}
	return nil
}

// buildPcFresh is the encoder's half. It replays the allocation over the
// real new records -- where the high-water mark is exact, since every table
// is in front of it -- and writes down, per invented slot, whether the mark
// is where that slot points, and per record, how far the cursor has to move
// to reach the mark.
func buildPcFresh(old, new *gobin.Bin, m *match, l *layout) (int, []byte, []uint32) {
	np := new.Pcln
	c := &pcCursor{tab: np.Table(np.Pctab), off: 1}
	nft := np.Table(np.Functab)
	hi := uint32(1)
	eachRecShape(old, l, m, func(j int, npc uint32, keep int) {
		rec := nft[new.Funcs[j].FuncOff:]
		first := true
		eachPcSlot(npc, func(o int) {
			v := uint32(0) // a record too short to hold the slot has no table there
			if o+4 <= len(rec) {
				v = binary.LittleEndian.Uint32(rec[o:])
			}
			if keep < 0 || o >= gobin.FuncSize && (o-gobin.FuncSize)/4 >= keep {
				if first {
					first = false
					n := uint32(0)
					for c.off < hi && int(c.off) < len(c.tab) {
						c.step()
						n++
					}
					c.gaps = append(c.gaps, n)
				}
				fresh := v == hi
				c.set(fresh)
				if fresh {
					c.step()
				}
			}
			if e := c.end(v); e > hi {
				hi = e
			}
		})
	})
	return c.i, c.mask, c.gaps
}

// reshape rewrites a _func record whose pcdata/funcdata counts changed,
// keeping the header and whatever of the arrays still exists. A slot the old
// record has no counterpart for takes the template's value.
func reshape(rec []byte, npc, nfd uint32, sh [2]uint32, tpl *funcTemplate) []byte {
	nr := make([]byte, gobin.FuncSize+4*sh[0]+4*sh[1])
	copy(nr, rec[:gobin.FuncSize])
	for k := range sh[0] {
		o := gobin.FuncSize + 4*k
		if k < npc {
			copy(nr[o:], rec[o:o+4])
		} else {
			binary.LittleEndian.PutUint32(nr[o:], tpl.pcdataAt(k))
		}
	}
	for k := range sh[1] {
		o := gobin.FuncSize + 4*sh[0] + 4*k
		if k < nfd {
			copy(nr[o:], rec[gobin.FuncSize+4*npc+4*k:gobin.FuncSize+4*npc+4*k+4])
		} else {
			binary.LittleEndian.PutUint32(nr[o:], tpl.funcdataAt(k))
		}
	}
	binary.LittleEndian.PutUint32(nr[28:], sh[0])
	nr[43] = byte(sh[1])
	return nr
}

// tplWords are the _func header words a synthesised record takes from the
// template, by their offset in the record.
var tplWords = [...]int{8, 12, 16, 20, 24} // args, deferreturn, pcsp, pcfile, pcln

// A funcTemplate is the commonest value of each repeating _func field across
// the matched functions' re-targeted records. What a release adds is mostly
// compiler-generated wrappers, and a wrapper's record is the same record over
// and over -- the same argument and locals pointer maps, the same arg-info
// streams, the same frame size -- so the modal record predicts most of a new
// function's funcdata array and its argument size, where the zeroed record
// the codec used to synthesise predicted none of it. Both sides build the
// template from the same re-targeted records, so it costs no patch bytes and
// stays symmetric.
//
// funcID is the exception the mode cannot carry: it is 0 for an ordinary
// function and the wrapper id for a wrapper, so it is taken per name key
// instead (byKey). entryOff, nameOff and cuOffset are per-function and the
// caller sets them; startLine is a source line and does not repeat, so it
// stays zero.
type funcTemplate struct {
	word             [len(tplWords)]uint32
	funcID, flag     byte
	pcdata, funcdata []uint32
	byKey            map[string]byte // modal funcID per name key
}

// nameKey is a coarse shape of a function name: the receiver form and the
// last dot-separated component with trailing digits stripped, or "fm" for a
// method value. Compiler-generated wrappers share it, which is what lets the
// modal funcID be learned per key.
func nameKey(n string) string {
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

func modalRecord(recs [][]byte) *funcTemplate {
	var word [len(tplWords)]map[uint32]int
	for w := range word {
		word[w] = map[uint32]int{}
	}
	id, flag := map[uint32]int{}, map[uint32]int{}
	var pcd, fnd []map[uint32]int
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		npc, nfd := int(binary.LittleEndian.Uint32(rec[28:])), int(rec[43])
		if gobin.FuncSize+4*npc+4*nfd > len(rec) {
			continue
		}
		for w, o := range tplWords {
			word[w][binary.LittleEndian.Uint32(rec[o:])]++
		}
		id[uint32(rec[40])]++
		flag[uint32(rec[41])]++
		for len(pcd) < npc {
			pcd = append(pcd, map[uint32]int{})
		}
		for len(fnd) < nfd {
			fnd = append(fnd, map[uint32]int{})
		}
		for k := range npc {
			pcd[k][binary.LittleEndian.Uint32(rec[gobin.FuncSize+4*k:])]++
		}
		for k := range nfd {
			fnd[k][binary.LittleEndian.Uint32(rec[gobin.FuncSize+4*npc+4*k:])]++
		}
	}
	t := &funcTemplate{}
	for w, c := range word {
		t.word[w] = modeOf(c, 0)
	}
	t.funcID, t.flag = byte(modeOf(id, 0)), byte(modeOf(flag, 0))
	for _, c := range pcd {
		t.pcdata = append(t.pcdata, modeOf(c, 0))
	}
	for _, c := range fnd {
		t.funcdata = append(t.funcdata, modeOf(c, ^uint32(0)))
	}
	return t
}

// learnFuncID takes the modal funcID of each name key in the old binary. A
// new function is mostly a compiler-generated wrapper, and a wrapper's
// funcID is not the modal 0.
func (t *funcTemplate) learnFuncID(old *gobin.Bin, oft []byte) {
	c := map[string]map[uint32]int{}
	for _, f := range old.Funcs {
		k := nameKey(f.Name)
		if c[k] == nil {
			c[k] = map[uint32]int{}
		}
		c[k][uint32(oft[uint64(f.FuncOff)+40])]++
	}
	t.byKey = make(map[string]byte, len(c))
	for k, m := range c {
		t.byKey[k] = byte(modeOf(m, 0))
	}
}

// modeOf is the commonest key of c, ties broken by the smaller value so that
// the encoder and the decoder pick the same one, and def when c is empty.
func modeOf(c map[uint32]int, def uint32) uint32 {
	best, bestN := def, 0
	for v, n := range c {
		if n > bestN || n == bestN && v < best {
			best, bestN = v, n
		}
	}
	return best
}

func (t *funcTemplate) pcdataAt(k uint32) uint32 {
	if int(k) < len(t.pcdata) {
		return t.pcdata[k]
	}
	return 0
}

func (t *funcTemplate) funcdataAt(k uint32) uint32 {
	if int(k) < len(t.funcdata) {
		return t.funcdata[k]
	}
	return ^uint32(0)
}

// synth builds the _func record of a function the release added.
func (t *funcTemplate) synth(npc, nfd, cu uint32, name string) []byte {
	rec := make([]byte, gobin.FuncSize+4*npc+4*nfd)
	for w, o := range tplWords {
		binary.LittleEndian.PutUint32(rec[o:], t.word[w])
	}
	binary.LittleEndian.PutUint32(rec[28:], npc)
	binary.LittleEndian.PutUint32(rec[32:], cu)
	id := t.funcID
	if v, ok := t.byKey[nameKey(name)]; ok {
		id = v
	}
	rec[40], rec[41], rec[43] = id, t.flag, byte(nfd)
	for k := range npc {
		binary.LittleEndian.PutUint32(rec[gobin.FuncSize+4*k:], t.pcdataAt(k))
	}
	for k := range nfd {
		binary.LittleEndian.PutUint32(rec[gobin.FuncSize+4*npc+4*k:], t.funcdataAt(k))
	}
	return rec
}

// resolveNameOffs decides each new function's offset into the new
// funcnametab. A matched function's old offset is mapped through a content
// map of the two tables and accepted when the name found there is the one
// expected -- which reproduces the linker's choice among identical name
// strings, since inlined-only twins produce extra entries. Anything left
// falls back to the k-th occurrence of the name, in old-offset order.
func resolveNameOffs(old, new *gobin.Bin, m *match) []int32 {
	op, np := old.Pcln, new.Pcln
	fnt := np.Table(np.Funcnametab)
	nameOff := make([]int32, len(new.Funcs))
	byName := map[string][]int32{}
	forEachName(fnt, func(off int, n string) { byName[n] = append(byName[n], int32(off)) })

	fnmap := buildDataMap(op.Table(op.Funcnametab), fnt, 16, nil, 0)
	taken := map[int32]bool{}
	groups := map[string][]int{}
	for j, g := range new.Funcs {
		name := g.Name
		if i := m.NewToOld[j]; i >= 0 {
			name = old.Funcs[i].Name
			c := fnmap.Map(uint64(old.Funcs[i].NameOff))
			if c+uint64(len(name)) < uint64(len(fnt)) && string(fnt[c:c+uint64(len(name))]) == name &&
				fnt[c+uint64(len(name))] == 0 && !taken[int32(c)] {
				nameOff[j] = int32(c)
				taken[int32(c)] = true
				continue
			}
		}
		groups[name] = append(groups[name], j)
	}
	for name, js := range groups {
		sort.SliceStable(js, func(a, b int) bool {
			ia, ib := m.NewToOld[js[a]], m.NewToOld[js[b]]
			if ia < 0 || ib < 0 {
				return ia >= 0 && ib < 0
			}
			return old.Funcs[ia].NameOff < old.Funcs[ib].NameOff
		})
		offs := byName[name]
		var free []int32
		for _, o := range offs {
			if !taken[o] {
				free = append(free, o)
			}
		}
		if len(free) > 0 {
			offs = free
		}
		for k, j := range js {
			switch {
			case k < len(offs):
				nameOff[j] = offs[k]
			case len(offs) > 0:
				nameOff[j] = offs[len(offs)-1]
			}
		}
	}
	return nameOff
}
