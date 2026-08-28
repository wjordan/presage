package delta

import (
	"encoding/binary"
	"sort"
	"strings"

	"github.com/wjordan/go-binsync/delta/gobin"
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
	// rest from the template, and lay the records out.
	size := hdr
	var prevCU uint32
	for j, g := range new.Funcs {
		rec := recs[j]
		if rec != nil {
			npc, nfd := binary.LittleEndian.Uint32(rec[28:]), uint32(rec[43])
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
