package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/delta/gobin"
)

// --------------------------------------------------------------- level 10
//
// Level 5 splits .go.type's residual into "a field the walker rewrote" and
// "everything else", and on the patch release the first of those is 19.7% of
// the section's wrong bytes -- 8,914 B, 7,170 of them method-table entries.
// A share that large in a field the codec already transmits nothing for is
// either a map bug (the .gopclntab block-mapping bug of level 8, in another
// table) or a field the walker never rewrites. This level decides which, by
// asking of every wrong field whether the value the new image holds names the
// same thing the prediction named:
//
//	10a  method-table entries, by sub-field, with the entity test
//	10b  the same table's alignment: is the new method array the old one with
//	     entries inserted or deleted
//	10c  changed descriptors, wrong bytes outside a rewritten field, by which
//	     header/array field they land in
//	10d  both classes priced by whole correction run, the way 3c prices a
//	     field-fix layer -- a byte-granular revert of a four-byte field inside
//	     a longer run is the misleading number.
//
// Run it with -levels 0 ('10' would select levels 1 and 0).

// abi.Type header fields, then the kind-specific body, the uncommon header,
// the variable-length array and the method array. Offsets are internal/abi's
// for the supported release, the same constants delta/typedesc.go uses.
type tfield struct {
	name string
	off  int
	n    int
}

var typeHdr = []tfield{
	{"Size_", 0, 8}, {"PtrBytes", 8, 8}, {"Hash", 16, 4},
	{"TFlag", 20, 1}, {"Align", 21, 1}, {"FieldAlign", 22, 1}, {"Kind", 23, 1},
	{"Equal fn", 24, 8}, {"GCData", 32, 8}, {"Str (nameOff)", 40, 4}, {"PtrToThis", 44, 4},
}

// bodyFields are the words between the header and the uncommon block, per kind.
var bodyFields = map[byte][]tfield{
	17: {{"array Elem", 48, 8}, {"array Slice", 56, 8}, {"array Len", 64, 8}},
	18: {{"chan Elem", 48, 8}, {"chan Dir", 56, 8}},
	19: {{"func InCount", 48, 2}, {"func OutCount", 50, 2}, {"func pad", 52, 4}},
	20: {{"iface PkgPath", 48, 8}, {"iface Methods.Data", 56, 8}, {"iface Methods.Len", 64, 8}, {"iface Methods.Cap", 72, 8}},
	21: {{"map Key", 48, 8}, {"map Elem", 56, 8}, {"map Group", 64, 8}, {"map Hasher", 72, 8},
		{"map body", 80, 8}, {"map body", 88, 8}, {"map body", 96, 8}, {"map body", 104, 8},
		{"map body", 112, 8}, {"map body", 120, 8}, {"map body", 128, 8}},
	22: {{"ptr Elem", 48, 8}},
	23: {{"slice Elem", 48, 8}},
	25: {{"struct PkgPath", 48, 8}, {"struct Fields.Data", 56, 8}, {"struct Fields.Len", 64, 8}, {"struct Fields.Cap", 72, 8}},
}

var uncommonFields = []tfield{
	{"uncommon PkgPath", 0, 4}, {"uncommon Mcount", 4, 2}, {"uncommon Xcount", 6, 2},
	{"uncommon Moff", 8, 4}, {"uncommon pad", 12, 4},
}

var methodFields = []tfield{
	{"method Name (nameOff)", 0, 4}, {"method Mtyp (typeOff)", 4, 4},
	{"method Ifn (textOff)", 8, 4}, {"method Tfn (textOff)", 12, 4},
}

func typeBase(kind byte) int {
	switch kind {
	case 17:
		return 72
	case 18:
		return 64
	case 19, 22, 23:
		return 56
	case 20, 25:
		return 80
	case 21:
		return 136
	}
	return 48
}

// dinfo is one descriptor's layout, resolved against the image that holds it.
type dinfo struct {
	off       uint64
	size      int
	kind      byte
	name      string
	unc       bool
	ut        uint64 // section offset of the uncommon header
	mcount    int
	marr      uint64 // section offset of the method array
	arrStart  uint64 // section offset of the variable-length array
	arrN      int
	arrStride int
}

func layout(d []byte, off uint64, size int, kind byte, name string) dinfo {
	di := dinfo{off: off, size: size, kind: kind, name: name}
	base := uint64(typeBase(kind))
	if off+base > uint64(len(d)) {
		return di
	}
	u32 := func(o uint64) uint32 { return binary.LittleEndian.Uint32(d[o:]) }
	u64 := func(o uint64) uint64 { return binary.LittleEndian.Uint64(d[o:]) }
	if off+base+16 <= uint64(len(d)) && d[off+20]&1 != 0 {
		di.unc = true
		di.ut = off + base
		di.mcount = int(binary.LittleEndian.Uint16(d[di.ut+4:]))
		di.marr = di.ut + uint64(u32(di.ut+8))
	}
	di.arrStart = off + base
	if di.unc {
		di.arrStart += 16
	}
	switch kind {
	case 19:
		di.arrN = int(binary.LittleEndian.Uint16(d[off+48:])) + int(binary.LittleEndian.Uint16(d[off+50:])&0x7fff)
		di.arrStride = 8
	case 20:
		di.arrN, di.arrStride = int(u64(off+64)), 8
	case 25:
		di.arrN, di.arrStride = int(u64(off+64)), 24
	}
	if di.arrN < 0 || di.arrN > 1<<16 {
		di.arrN = 0
	}
	if di.mcount < 0 || di.marr+16*uint64(di.mcount) > uint64(len(d)) {
		di.mcount = 0
	}
	return di
}

// field names the descriptor field an offset inside the descriptor lands in,
// with the field's own start and width so a delta can be read from it.
func (di dinfo) field(rel int) (string, int, int) {
	switch {
	case rel < 48:
		for _, f := range typeHdr {
			if rel >= f.off && rel < f.off+f.n {
				return f.name, f.off, f.n
			}
		}
	case rel < typeBase(di.kind):
		for _, f := range bodyFields[di.kind] {
			if rel >= f.off && rel < f.off+f.n {
				return f.name, f.off, f.n
			}
		}
		return fmt.Sprintf("%s body", kindName(di.kind)), rel &^ 7, 8
	}
	if di.unc {
		if u := int(di.ut - di.off); rel >= u && rel < u+16 {
			for _, f := range uncommonFields {
				if rel-u >= f.off && rel-u < f.off+f.n {
					return f.name, u + f.off, f.n
				}
			}
		}
		if m := int(di.marr - di.off); di.mcount > 0 && rel >= m && rel < m+16*di.mcount {
			k := (rel - m) % 16
			for _, f := range methodFields {
				if k >= f.off && k < f.off+f.n {
					return f.name, rel - k + f.off, f.n
				}
			}
		}
	}
	if a := int(di.arrStart - di.off); di.arrN > 0 && rel >= a && rel < a+di.arrN*di.arrStride {
		k := (rel - a) % di.arrStride
		switch di.kind {
		case 19:
			return "func arg *Type", rel - k, 8
		case 20:
			if k < 4 {
				return "imethod Name (nameOff)", rel - k, 4
			}
			return "imethod Typ (typeOff)", rel - k + 4, 4
		case 25:
			switch {
			case k < 8:
				return "struct field Name ptr", rel - k, 8
			case k < 16:
				return "struct field Typ ptr", rel - k + 8, 8
			default:
				return "struct field Offset", rel - k + 16, 8
			}
		}
	}
	return "descriptor tail/pad", rel &^ 3, 4
}

// readName reads an abi.Name at a types-relative offset; name data can sit
// outside the type section, so the section is resolved per name.
func readName(b *gobin.Bin, off uint32) string {
	a := b.Mod.Types + uint64(off)
	s := b.SectionOf(a)
	if s == nil || s.Data == nil {
		return ""
	}
	d, o := s.Data, a-s.Addr
	if o+2 > uint64(len(d)) {
		return ""
	}
	p, n := o+1, uint64(0)
	for shift := 0; ; shift += 7 {
		if p >= uint64(len(d)) || shift > 56 {
			return ""
		}
		x := d[p]
		p++
		n |= uint64(x&0x7f) << shift
		if x&0x80 == 0 {
			break
		}
	}
	if p+n > uint64(len(d)) {
		return ""
	}
	return string(d[p : p+n])
}

func funcName(b *gobin.Bin, addr uint64) string {
	if f := b.FuncAt(addr); f != nil {
		return f.Name
	}
	return ""
}

// deltas is a histogram of new-pred for one field, printed the way 8b prints
// the _func fields: a field whose error is one constant is a map bug, one
// whose error is spread out is new content.
type deltas struct {
	changed, wrong int
	d              map[int64]int
}

func newDeltas() *deltas { return &deltas{d: map[int64]int{}} }

func (s *deltas) add(dv int64, wrong int) {
	s.changed++
	s.wrong += wrong
	s.d[dv]++
}

func (s *deltas) top(n int) (string, string, int) {
	type kv struct{ d, n int64 }
	var ds []kv
	for d, c := range s.d {
		ds = append(ds, kv{d, int64(c)})
	}
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].n != ds[j].n {
			return ds[i].n > ds[j].n
		}
		return ds[i].d < ds[j].d
	})
	out, top := "", 0
	for i, k := range ds {
		if i < 10 {
			top += int(k.n)
		}
		if i < n {
			out += fmt.Sprintf("%+d (%d)  ", k.d, k.n)
		}
	}
	return out, pct(top, max(1, s.changed)), len(s.d)
}

func rdLE(b []byte, off, n int) int64 {
	switch n {
	case 1:
		return int64(int8(b[off]))
	case 2:
		return int64(int16(binary.LittleEndian.Uint16(b[off:])))
	case 4:
		return int64(int32(binary.LittleEndian.Uint32(b[off:])))
	case 8:
		return int64(binary.LittleEndian.Uint64(b[off:]))
	}
	return 0
}

func (c *ctx) typeMethods() {
	sec := c.nb.SectionOf(c.nb.Mod.Types)
	osec := c.ob.SectionOf(c.ob.Mod.Types)
	if sec == nil || osec == nil || len(c.sites) == 0 {
		fmt.Fprintf(os.Stderr, "\n-- 10. no type section or no descriptor sites\n")
		return
	}
	lo, hi := int(sec.Off), int(sec.Off+sec.Size)
	od, nd := osec.Data, sec.Data

	// The old descriptors the walk placed, and where it placed them.
	type placed struct{ old, pred uint64; size int }
	var pl []placed
	oldOf := make([]int32, hi-lo) // predicted-file byte -> old section offset
	for i := range oldOf {
		oldOf[i] = -1
	}
	role := make([]int8, hi-lo)
	for i := range role {
		role[i] = tdOther
	}
	inOld := make([]bool, hi-lo)
	for _, st := range c.sites {
		if st.Role == 'D' {
			pl = append(pl, placed{uint64(st.Old), uint64(st.Off - lo), st.N})
			for k := 0; k < st.N; k++ {
				if p := st.Off + k - lo; p >= 0 && p < hi-lo {
					inOld[p] = true
					oldOf[p] = int32(st.Old + k)
				}
			}
			continue
		}
		r := int8(roleIndex(st.Role))
		for k := 0; k < st.N; k++ {
			if p := st.Off + k - lo; p >= 0 && p < hi-lo {
				role[p] = r
				if oldOf[p] < 0 {
					oldOf[p] = int32(st.Old + k)
				}
			}
		}
	}
	sort.Slice(pl, func(i, j int) bool { return pl[i].old < pl[j].old })
	oldDesc := func(o uint64) int { // index in pl of the descriptor holding old offset o
		i := sort.Search(len(pl), func(i int) bool { return pl[i].old+uint64(pl[i].size) > o })
		if i < len(pl) && o >= pl[i].old {
			return i
		}
		return -1
	}

	// The new image's own descriptors, for the target side of every question.
	news := delta.WalkDescriptors(c.nb)
	sort.Slice(news, func(i, j int) bool { return news[i].Off < news[j].Off })
	newInfo := make([]dinfo, len(news))
	for i, d := range news {
		newInfo[i] = layout(nd, d.Off, d.Size, d.Kind, d.Name)
	}
	newHolding := func(so uint64) int {
		i := sort.Search(len(news), func(i int) bool { return news[i].Off+uint64(news[i].Size) > so })
		if i < len(news) && so >= news[i].Off {
			return i
		}
		return -1
	}
	mnames := func(b *gobin.Bin, d []byte, di dinfo) []string {
		out := make([]string, 0, di.mcount)
		for k := 0; k < di.mcount; k++ {
			o := di.marr + 16*uint64(k)
			if o+16 > uint64(len(d)) {
				break
			}
			out = append(out, readName(b, binary.LittleEndian.Uint32(d[o:])))
		}
		return out
	}

	// ---- 10a: the method-table sites, by sub-field.
	type mstat struct {
		*deltas
		same, diff, shifted, unknown int
		shiftD                       map[int]int
	}
	sub := make([]mstat, len(methodFields)+1) // +1: the pkgPath nameOff inside name data
	for i := range sub {
		sub[i] = mstat{deltas: newDeltas(), shiftD: map[int]int{}}
	}
	subNames := []string{"method Name (nameOff)", "method Mtyp (typeOff)",
		"method Ifn (textOff)", "method Tfn (textOff)", "method name's pkgPath nameOff"}
	// per old descriptor: how its method array compares with the new one
	type tstat struct{ same, ins, del, mixed, noCounter int }
	var ts tstat
	seenDesc := map[uint64]bool{}
	var methPos []int
	seenSite, dupSite := map[int]bool{}, 0
	for _, st := range c.sites {
		if st.Role != 'M' || st.N != 4 {
			continue
		}
		if seenSite[st.Off] {
			dupSite++
			continue
		}
		seenSite[st.Off] = true
		w := 0
		for k := 0; k < 4; k++ {
			if p := st.Off + k; p >= lo && p < hi && c.pred[p] != c.new[p] {
				w++
			}
		}
		if w == 0 {
			continue
		}
		for k := 0; k < 4; k++ {
			methPos = append(methPos, st.Off+k)
		}
		oi := oldDesc(uint64(st.Old))
		idx, which := -1, 4
		var odi dinfo
		if oi >= 0 {
			p := pl[oi]
			kind := od[p.old+23] & 0x1f
			odi = layout(od, p.old, p.size, kind, "")
			if odi.mcount > 0 && uint64(st.Old) >= odi.marr && uint64(st.Old) < odi.marr+16*uint64(odi.mcount) {
				rel := uint64(st.Old) - odi.marr
				idx, which = int(rel/16), int(rel%16)/4
			}
		}
		s := &sub[which]
		newV := binary.LittleEndian.Uint32(c.new[st.Off:])
		predV := binary.LittleEndian.Uint32(c.pred[st.Off:])
		s.add(int64(int32(newV))-int64(int32(predV)), w)
		// the entity test: does the value the new image holds name the same
		// thing the prediction named?
		if st.Old+4 > len(od) {
			continue
		}
		oldV := binary.LittleEndian.Uint32(od[st.Old:])
		var wantEnt, gotEnt string
		switch which {
		case 0, 4:
			wantEnt, gotEnt = readName(c.ob, oldV), readName(c.nb, newV)
		case 1:
			wantEnt = descNameAt(c.ob, oldV)
			gotEnt = descNameAt(c.nb, newV)
		case 2, 3:
			if oldV != ^uint32(0) {
				wantEnt = funcName(c.ob, c.ob.Mod.Text+uint64(oldV))
			}
			if newV != ^uint32(0) {
				gotEnt = funcName(c.nb, c.nb.Mod.Text+uint64(newV))
			}
		}
		switch {
		case wantEnt != "" && wantEnt == gotEnt:
			s.same++
		case wantEnt == "" || gotEnt == "":
			s.unknown++
		default:
			s.diff++
		}
		// alignment: is the same method at another index of the new table?
		if oi >= 0 && idx >= 0 {
			ni := newHolding(pl[oi].pred)
			if ni >= 0 {
				ndi := newInfo[ni]
				om, nm := mnames(c.ob, od, odi), mnames(c.nb, nd, ndi)
				if idx < len(om) {
					for j, nmj := range nm {
						if nmj == om[idx] && j != idx {
							s.shifted++
							s.shiftD[j-idx]++
							break
						}
					}
				}
				if !seenDesc[pl[oi].old] {
					seenDesc[pl[oi].old] = true
					switch {
					case eqStr(om, nm):
						ts.same++
					case subseq(om, nm):
						ts.ins++
					case subseq(nm, om):
						ts.del++
					default:
						ts.mixed++
					}
				}
			} else if !seenDesc[pl[oi].old] {
				seenDesc[pl[oi].old] = true
				ts.noCounter++
			}
		}
	}
	fmt.Fprintf(os.Stderr, "\n-- 10a. method-table entries, by sub-field (%d duplicate sites: two old offsets mapped to one place) (entity test: does the new value name what the prediction named?)\n", dupSite)
	fmt.Fprintf(os.Stderr, "  %-30s %8s %8s %7s %7s %8s %8s %8s   %s\n",
		"sub-field", "wrong", "wrong B", "same", "differs", "unknown", "values", "top 10", "most common new-pred (count)")
	for i, s := range sub {
		if s.changed == 0 {
			continue
		}
		top, share, nv := s.top(3)
		fmt.Fprintf(os.Stderr, "  %-30s %8d %8d %7d %7d %8d %8d %8s   %s\n",
			subNames[i], s.changed, s.wrong, s.same, s.diff, s.unknown, nv, share, top)
	}
	fmt.Fprintf(os.Stderr, "\n-- 10b. is the wrong entry the same method at another index of the new table?\n")
	for i, s := range sub {
		if s.shifted == 0 {
			continue
		}
		var ks []int
		for k := range s.shiftD {
			ks = append(ks, k)
		}
		sort.Slice(ks, func(a, b int) bool { return s.shiftD[ks[a]] > s.shiftD[ks[b]] })
		out := ""
		for i, k := range ks {
			if i < 5 {
				out += fmt.Sprintf("%+d (%d)  ", k, s.shiftD[k])
			}
		}
		fmt.Fprintf(os.Stderr, "  %-30s %d of %d wrong fields sit at a shifted index: %s\n", subNames[i], s.shifted, s.changed, out)
	}
	fmt.Fprintf(os.Stderr, "  descriptors holding a wrong method field (%d): method list identical %d, entries inserted %d, deleted %d, otherwise different %d, no new counterpart at the placed offset %d\n",
		ts.same+ts.ins+ts.del+ts.mixed+ts.noCounter, ts.same, ts.ins, ts.del, ts.mixed, ts.noCounter)

	// ---- 10c: changed descriptors, wrong bytes outside a rewritten field.
	type fstat struct {
		*deltas
		bytes   int
		kinds   map[byte]int
		same    int // the new value names what the old value named
		differs int
		unknown int
	}
	fs := map[string]*fstat{}
	getf := func(n string) *fstat {
		if fs[n] == nil {
			fs[n] = &fstat{deltas: newDeltas(), kinds: map[byte]int{}}
		}
		return fs[n]
	}
	seenField := map[int]bool{}
	var changedPos []int
	noDesc := 0
	for p := lo; p < hi; p++ {
		if c.pred[p] == c.new[p] || !inOld[p-lo] || role[p-lo] != tdOther {
			continue
		}
		changedPos = append(changedPos, p)
		ni := newHolding(uint64(p - lo))
		if ni < 0 {
			noDesc++
			getf("outside any new descriptor").bytes++
			continue
		}
		di := newInfo[ni]
		name, fo, fn := di.field(p - lo - int(di.Off()))
		s := getf(name)
		s.bytes++
		s.kinds[di.kind]++
		start := int(di.off) + fo + lo
		if seenField[start] || start+fn > hi {
			continue
		}
		seenField[start] = true
		w := 0
		for k := 0; k < fn; k++ {
			if c.pred[start+k] != c.new[start+k] {
				w++
			}
		}
		s.add(rdLE(c.new, start, fn)-rdLE(c.pred, start, fn), w)
		// entity test where the field names something: a pointer into the
		// image, a nameOff or a typeOff.
		oo := oldOf[start-lo]
		if oo < 0 || int(oo)+fn > len(od) {
			continue
		}
		var want, got string
		switch name {
		case "struct field Typ ptr", "ptr Elem", "slice Elem", "array Elem", "map Key", "map Elem", "chan Elem", "func arg *Type", "iface PkgPath", "struct PkgPath":
			want = ptrDescName(c.ob, binary.LittleEndian.Uint64(od[oo:]))
			got = ptrDescName(c.nb, binary.LittleEndian.Uint64(c.new[start:]))
		case "Equal fn":
			want = funcName(c.ob, binary.LittleEndian.Uint64(od[oo:]))
			got = funcName(c.nb, binary.LittleEndian.Uint64(c.new[start:]))
		case "struct field Name ptr", "iface Methods.Data", "struct Fields.Data":
			want = ptrName(c.ob, binary.LittleEndian.Uint64(od[oo:]))
			got = ptrName(c.nb, binary.LittleEndian.Uint64(c.new[start:]))
		case "Str (nameOff)", "uncommon PkgPath", "imethod Name (nameOff)":
			want = readName(c.ob, binary.LittleEndian.Uint32(od[oo:]))
			got = readName(c.nb, binary.LittleEndian.Uint32(c.new[start:]))
		case "imethod Typ (typeOff)":
			want = descNameAt(c.ob, binary.LittleEndian.Uint32(od[oo:]))
			got = descNameAt(c.nb, binary.LittleEndian.Uint32(c.new[start:]))
		default:
			continue
		}
		switch {
		case want != "" && want == got:
			s.same++
		case want == "" || got == "":
			s.unknown++
		default:
			s.differs++
		}
	}
	fmt.Fprintf(os.Stderr, "\n-- 10f. the method-array bytes of 10c: why did the walker not rewrite them?\n")
	{
		type why struct{ inOldArr, oldNoUnc, mcountDiff, moffDiff, other, n int }
		var w why
		samples := 0
		for _, p := range changedPos {
			ni := newHolding(uint64(p - lo))
			if ni < 0 {
				continue
			}
			di := newInfo[ni]
			name, _, _ := di.field(p - lo - int(di.off))
			if len(name) < 6 || name[:6] != "method" {
				continue
			}
			w.n++
			oi := oldDesc(uint64(oldOf[p-lo]))
			if oi < 0 {
				w.other++
				continue
			}
			pd := pl[oi]
			odi := layout(od, pd.old, pd.size, od[pd.old+23]&0x1f, "")
			oo := uint64(oldOf[p-lo])
			switch {
			case !odi.unc:
				w.oldNoUnc++
			case odi.mcount > 0 && oo >= odi.marr && oo < odi.marr+16*uint64(odi.mcount):
				w.inOldArr++
			case odi.mcount != di.mcount:
				w.mcountDiff++
			case odi.marr-odi.off != di.marr-di.off:
				w.moffDiff++
			default:
				w.other++
			}
			if samples < 8 && (w.n%137 == 1) {
				samples++
				fmt.Fprintf(os.Stderr, "    sample: new %s %q mcount %d moff %d | old desc off %#x size %d unc %v mcount %d moff %d | field %s pred %#x new %#x old %#x\n",
					kindName(di.kind), di.name, di.mcount, di.marr-di.off, pd.old, pd.size, odi.unc, odi.mcount, odi.marr-odi.off,
					name, binary.LittleEndian.Uint32(c.pred[p&^3:]), binary.LittleEndian.Uint32(c.new[p&^3:]),
					binary.LittleEndian.Uint32(od[oo&^3:]))
			}
		}
		fmt.Fprintf(os.Stderr, "  %d wrong bytes in a new-image method array: old offset already inside the old method array %d, old descriptor has no uncommon block %d, mcount differs %d, moff differs %d, other %d\n",
			w.n, w.inOldArr, w.oldNoUnc, w.mcountDiff, w.moffDiff, w.other)
	}

	var fnames []string
	for n := range fs {
		fnames = append(fnames, n)
	}
	sort.Slice(fnames, func(i, j int) bool { return fs[fnames[i]].bytes > fs[fnames[j]].bytes })
	fmt.Fprintf(os.Stderr, "\n-- 10c. changed descriptors, wrong bytes outside a field the walker rewrote, by descriptor field\n")
	fmt.Fprintf(os.Stderr, "  %-26s %8s %8s %7s %7s %8s %8s %7s   %s\n",
		"field", "wrong B", "fields", "same", "differs", "unknown", "values", "top 10", "most common new-pred (count) / kinds")
	tot := 0
	for _, n := range fnames {
		s := fs[n]
		tot += s.bytes
		top, share, nv := s.top(2)
		var ks []byte
		for k := range s.kinds {
			ks = append(ks, k)
		}
		sort.Slice(ks, func(i, j int) bool { return s.kinds[ks[i]] > s.kinds[ks[j]] })
		kd := ""
		for i, k := range ks {
			if i < 3 {
				kd += fmt.Sprintf("%s %d ", kindName(k), s.kinds[k])
			}
		}
		fmt.Fprintf(os.Stderr, "  %-26s %8d %8d %7d %7d %8d %8d %7s   %s| %s\n",
			n, s.bytes, s.changed, s.same, s.differs, s.unknown, nv, share, top, kd)
	}
	fmt.Fprintf(os.Stderr, "  %-26s %8d (%d B in no new descriptor at all)\n", "TOTAL", tot, noDesc)

	// ---- 10g: is the descriptor torn? A block whose map delta differs from
	// its descriptor's leaves the new position uncopied -- zero -- while the
	// old bytes land somewhere else. The walker's own field writes measure
	// the tear directly: it writes at place(fieldOff), while the descriptor's
	// bytes were copied at place(descOff) + (fieldOff - descOff).
	tear := map[int64]int{}
	tornDesc := map[uint64]bool{}
	for _, st := range c.sites {
		if st.Role == 'D' {
			continue
		}
		oi := oldDesc(uint64(st.Old))
		if oi < 0 {
			continue
		}
		t := int64(st.Off-lo) - int64(pl[oi].pred) - (int64(st.Old) - int64(pl[oi].old))
		tear[t]++
		if t != 0 {
			tornDesc[pl[oi].old] = true
		}
	}
	var holePos, holeRecov, recov []int
	holeRuns, holeRunB := 0, 0
	inHole := false
	for p := lo; p < hi; p++ {
		if c.pred[p] != c.new[p] && c.pred[p] == 0 {
			holePos = append(holePos, p)
			holeRunB++
			if !inHole {
				holeRuns++
				inHole = true
			}
			if oo := oldOf[p-lo]; oo >= 0 && int(oo) < len(od) && od[oo] == c.new[p] {
				holeRecov = append(holeRecov, p)
			}
		} else {
			inHole = false
		}
		if c.pred[p] != c.new[p] {
			if oo := oldOf[p-lo]; oo >= 0 && int(oo) < len(od) && od[oo] == c.new[p] {
				recov = append(recov, p)
			}
		}
	}
	var ks []int64
	for k := range tear {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return tear[ks[i]] > tear[ks[j]] })
	out := ""
	for i, k := range ks {
		if i < 6 {
			out += fmt.Sprintf("%+d (%d)  ", k, tear[k])
		}
	}
	fmt.Fprintf(os.Stderr, "\n-- 10g. torn descriptors: the map delta inside a descriptor differs from the delta at its start\n")
	fmt.Fprintf(os.Stderr, "  %d descriptors torn of %d placed; rewritten-field tears (offset of the walker's write minus the descriptor's own copy): %s\n",
		len(tornDesc), len(pl), out)
	fmt.Fprintf(os.Stderr, "  wrong bytes the copy left zero: %d in %d holes; %d of them (%s) are the byte the old image holds at the same descriptor offset\n",
		len(holePos), holeRuns, len(holeRecov), pct(len(holeRecov), max(1, len(holePos))))
	var tornPos []int
	for p := lo; p < hi; p++ {
		if c.pred[p] == c.new[p] || !inOld[p-lo] {
			continue
		}
		if oi := oldDesc(uint64(oldOf[p-lo])); oi >= 0 && tornDesc[pl[oi].old] {
			tornPos = append(tornPos, p)
		}
	}
	mh := c.marginals([][]int{holePos, holeRecov, recov, tornPos})
	printRows("10h. priced by byte", []row{
		{"wrong bytes the copy left zero", 0, len(holePos), 0, mh[0].comp, mh[0].raw},
		{"-- of which the old image holds the right byte", 0, len(holeRecov), 0, mh[1].comp, mh[1].raw},
		{"all wrong bytes the old image holds at the mapped offset", 0, len(recov), 0, mh[2].comp, mh[2].raw},
		{fmt.Sprintf("every wrong byte inside a torn descriptor (%d)", len(tornDesc)), 0, len(tornPos), 0, mh[3].comp, mh[3].raw},
	}, false)
	c.runPrice("10i. wrong bytes the old image holds at the mapped offset, by run", lo, hi, recov)

	// ---- 10d: priced by whole correction run, the way 3c prices a field fix.
	c.runPrice("10d. method-table entries", lo, hi, methPos)
	c.runPrice("10e. changed-descriptor bytes outside a rewritten field", lo, hi, changedPos)
}

// Off is a shim so dinfo reads like delta.Descriptor at the call site.
func (di dinfo) Off() uint64 { return di.off }

func eqStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// subseq reports whether a is a subsequence of b, i.e. b is a with entries
// inserted.
func subseq(a, b []string) bool {
	i := 0
	for _, x := range b {
		if i < len(a) && a[i] == x {
			i++
		}
	}
	return i == len(a)
}

func descNameAt(b *gobin.Bin, off uint32) string {
	if off == 0 {
		return ""
	}
	a := b.Mod.Types + uint64(off)
	s := b.SectionOf(a)
	if s == nil || s.Data == nil || a-s.Addr+48 > uint64(len(s.Data)) {
		return ""
	}
	return readName(b, binary.LittleEndian.Uint32(s.Data[a-s.Addr+40:]))
}

// ptrDescName reads the name of the descriptor at an absolute address.
func ptrDescName(b *gobin.Bin, addr uint64) string {
	s := b.SectionOf(addr)
	if s == nil || s.Data == nil || addr-s.Addr+48 > uint64(len(s.Data)) {
		return ""
	}
	return readName(b, binary.LittleEndian.Uint32(s.Data[addr-s.Addr+40:]))
}

// ptrName reads an abi.Name at an absolute address.
func ptrName(b *gobin.Bin, addr uint64) string {
	if addr < b.Mod.Types {
		return ""
	}
	if s := b.SectionOf(addr); s == nil || s.Data == nil {
		return ""
	}
	return readName(b, uint32(addr-b.Mod.Types))
}

// runPrice prices a class the way 3c does: a byte-granular revert of a
// four-byte field inside a longer wrong region is not a price a fix could
// collect, because the region stays.
func (c *ctx) runPrice(title string, lo, hi int, pos []int) {
	in := make(map[int]bool, len(pos))
	for _, p := range pos {
		in[p] = true
	}
	var only, mixedIn, mixedAll, none []int
	nOnly, nMixed, nNone := 0, 0, 0
	for _, r := range c.regs {
		if r.s < lo || r.s >= hi {
			continue
		}
		a, b := 0, 0
		for p := r.s; p < r.e; p++ {
			if c.pred[p] == c.new[p] {
				continue
			}
			if in[p] {
				a++
			} else {
				b++
			}
		}
		switch {
		case a > 0 && b == 0:
			nOnly++
		case a > 0:
			nMixed++
		default:
			nNone++
		}
		for p := r.s; p < r.e; p++ {
			if c.pred[p] == c.new[p] {
				continue
			}
			switch {
			case a > 0 && b == 0:
				only = append(only, p)
			case a > 0:
				mixedAll = append(mixedAll, p)
				if in[p] {
					mixedIn = append(mixedIn, p)
				}
			default:
				none = append(none, p)
			}
		}
	}
	marg := c.marginals([][]int{only, mixedIn, mixedAll, none})
	rows := []row{
		{fmt.Sprintf("runs whose every wrong byte is in the class (%d)", nOnly), 0, len(only), nOnly, marg[0].comp, marg[0].raw},
		{fmt.Sprintf("mixed runs (%d), the class bytes only", nMixed), 0, len(mixedIn), 0, marg[1].comp, marg[1].raw},
		{fmt.Sprintf("mixed runs (%d), all their wrong bytes", nMixed), 0, len(mixedAll), nMixed, marg[2].comp, marg[2].raw},
		{fmt.Sprintf("runs with no class byte at all (%d)", nNone), 0, len(none), nNone, marg[3].comp, marg[3].raw},
	}
	printRows(title, rows, false)
}
