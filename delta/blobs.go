package delta

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/wjordan/presage/delta/gobin"
)

// Stage 1 is the part of .gopclntab that cannot be predicted from the old
// binary alone, split in two:
//
//	1a  funcnametab and filetab are transmitted as a correction of the old
//	    tables. They are name lists: a release changes a handful of them.
//	1b  cutab, pctab and go:func.* are *predicted* from the old tables, the
//	    new function order and the 1a tables, and only the correction of
//	    that prediction is transmitted. This is what removes the cost of the
//	    offsets embedded in them -- filetab offsets in cutab, funcnametab
//	    offsets in inline trees, .rodata offsets in stack-object records --
//	    and of the re-layout into the new function order.
//
// The rules emulated are the linker's (cmd/link/internal/ld/pcln.go:
// generatePctab, generateFuncdata, generateFilenameTabs).

// blobPred is the stage-1b prediction plus the old -> predicted offset maps
// that the _func records are later re-based through.
type blobPred struct {
	Cutab, Pctab, Gofunc []byte
	PcOff                map[uint32]uint32 // old pctab offset -> predicted
	GfOff                map[uint32]uint32 // old go:func.* offset -> predicted
}

func (b *blobPred) concat() []byte {
	out := make([]byte, 0, len(b.Cutab)+len(b.Pctab)+len(b.Gofunc))
	out = append(out, b.Cutab...)
	out = append(out, b.Pctab...)
	return append(out, b.Gofunc...)
}

func stage1aBlobs(b *gobin.Bin) []byte { return concatRanges(b, stage1aRanges(b.Pcln)) }
func stage1bBlobs(b *gobin.Bin) []byte { return concatRanges(b, stage1bRanges(b.Pcln)) }

// nameOffMap maps an old funcnametab offset to the new one: through a
// content map of the two tables when the name at the mapped offset is the
// expected one, and otherwise by taking the k-th occurrence of the name in
// the new table for the k-th in the old.
type nameOffMap struct {
	old, new []byte
	fnmap    *dataMap
	newOccs  map[string][]uint32
	oldOcc   map[uint32]int
}

func newNameOffMap(oldTab, newTab []byte) *nameOffMap {
	nm := &nameOffMap{old: oldTab, new: newTab, newOccs: map[string][]uint32{}, oldOcc: map[uint32]int{}}
	nm.fnmap = buildDataMap(oldTab, newTab, 16, nil, 0)
	forEachName(newTab, func(off int, n string) { nm.newOccs[n] = append(nm.newOccs[n], uint32(off)) })
	cnt := map[string]int{}
	forEachName(oldTab, func(off int, n string) {
		nm.oldOcc[uint32(off)] = cnt[n]
		cnt[n]++
	})
	return nm
}

func forEachName(tab []byte, f func(off int, name string)) {
	for off := 0; off < len(tab); {
		e := bytes.IndexByte(tab[off:], 0)
		if e < 0 {
			return
		}
		f(off, string(tab[off:off+e]))
		off += e + 1
	}
}

func nameAt(tab []byte, off uint32) (string, bool) {
	if int(off) >= len(tab) {
		return "", false
	}
	e := bytes.IndexByte(tab[off:], 0)
	if e < 0 {
		return "", false
	}
	return string(tab[off : int(off)+e]), true
}

func (nm *nameOffMap) Map(off uint32) (uint32, bool) {
	name, ok := nameAt(nm.old, off)
	if !ok {
		return off, false
	}
	c := nm.fnmap.Map(uint64(off))
	if c+uint64(len(name)) < uint64(len(nm.new)) && string(nm.new[c:c+uint64(len(name))]) == name && nm.new[c+uint64(len(name))] == 0 {
		return uint32(c), true
	}
	occs := nm.newOccs[name]
	if len(occs) == 0 {
		return off, false
	}
	return occs[min(nm.oldOcc[off], len(occs)-1)], true
}

// predictBlobs builds the stage-1b prediction. new is the skeleton: its
// function list is the new layout and its funcnametab and filetab are the
// stage-1a tables. Nothing of the real new binary is read, so the encoder
// and the decoder compute the same bytes.
func predictBlobs(old, new *gobin.Bin, m *match, mp *mapper) *blobPred {
	op, np := old.Pcln, new.Pcln
	bp := &blobPred{PcOff: map[uint32]uint32{}, GfOff: map[uint32]uint32{}}
	oft := op.Table(op.Functab)

	bp.Cutab = predictCutab(op.Table(op.Cutab), op.Table(op.Filetab), np.Table(np.Filetab))
	bp.Pctab = predictPctab(old, new, m, bp, oft)
	bp.Gofunc = predictGofunc(old, new, m, bp, mp, oft)

	// the linker pads each table to the alignment of the next symbol; the
	// layout carries the exact lengths, so pad -- never truncate -- to them
	bp.Cutab = padTo(bp.Cutab, np.Cutab.Len)
	bp.Pctab = padTo(bp.Pctab, np.Pctab.Len)
	bp.Gofunc = padTo(bp.Gofunc, np.Gofunc.Len)
	return bp
}

func padTo(b []byte, n uint64) []byte {
	for uint64(len(b)) < n {
		b = append(b, 0)
	}
	return b
}

// predictCutab re-targets each cutab entry -- an offset into filetab -- by
// looking its file name up in the new filetab.
func predictCutab(oldCutab, oldFiletab, newFiletab []byte) []byte {
	newFile := map[string]uint32{}
	forEachName(newFiletab, func(off int, n string) {
		if _, ok := newFile[n]; !ok {
			newFile[n] = uint32(off)
		}
	})
	out := append([]byte(nil), oldCutab...)
	for i := 0; i+4 <= len(oldCutab); i += 4 {
		e := binary.LittleEndian.Uint32(oldCutab[i:])
		if e == ^uint32(0) {
			continue
		}
		name, ok := nameAt(oldFiletab, e)
		if !ok {
			continue
		}
		if no, ok := newFile[name]; ok {
			binary.LittleEndian.PutUint32(out[i:], no)
		}
	}
	return out
}

// predictPctab replays the linker's generatePctab: walk the functions in the
// new order, emit each pc-value table the first time it is seen, and
// deduplicate identical ones. The tables themselves are unchanged bytes of
// the old pctab; what the prediction reproduces is their new order and the
// new offsets, which is the whole of the delta.
func predictPctab(old, new *gobin.Bin, m *match, bp *blobPred, oft []byte) []byte {
	op := old.Pcln
	oldPctab := op.Table(op.Pctab)
	pctab := make([]byte, 1, len(oldPctab))
	seen := map[string]uint32{}
	emit := func(off uint32) {
		if off == 0 || int(off) >= len(oldPctab) {
			return
		}
		if _, ok := bp.PcOff[off]; ok {
			return
		}
		content := oldPctab[off : int(off)+gobin.PcTableLen(oldPctab, int(off))]
		if p, ok := seen[string(content)]; ok {
			bp.PcOff[off] = p
			return
		}
		p := uint32(len(pctab))
		seen[string(content)] = p
		bp.PcOff[off] = p
		pctab = append(pctab, content...)
	}
	for j := range new.Funcs {
		i := m.NewToOld[j]
		if i < 0 {
			continue
		}
		f := old.Funcs[i]
		npc, _, _ := op.Record(f.FuncOff)
		rec := oft[f.FuncOff:]
		for _, o := range []int{16, 20, 24} { // pcsp, pcfile, pcln
			emit(binary.LittleEndian.Uint32(rec[o:]))
		}
		for k := range int(npc) {
			if k == 2 { // PCDATA_InlTreeIndex: the pcinline table comes last
				continue
			}
			emit(binary.LittleEndian.Uint32(rec[gobin.FuncSize+4*k:]))
		}
		if npc > 2 {
			emit(binary.LittleEndian.Uint32(rec[gobin.FuncSize+8:]))
		}
	}
	return pctab
}

// fdSym is one funcdata symbol of the old go:func.* blob.
type fdSym struct {
	off, size uint32
	kind      int // the FUNCDATA index it is used as
	first     int // the first old function that uses it
	region    int // the alignment region of the old blob it sits in
}

// predictGofunc replays generateFuncdata: the funcdata symbols in new
// first-use order, grouped by decreasing alignment class, with their
// embedded references re-targeted.
func predictGofunc(old, new *gobin.Bin, m *match, bp *blobPred, mp *mapper, oft []byte) []byte {
	op, np := old.Pcln, new.Pcln
	oldGf := old.Gofunc()
	syms := map[uint32]*fdSym{}
	var offs []uint32
	for fi, f := range old.Funcs {
		npc, nfd, _ := op.Record(f.FuncOff)
		rec := oft[f.FuncOff:]
		for k := range int(nfd) {
			off := binary.LittleEndian.Uint32(rec[gobin.FuncSize+4*int(npc)+4*k:])
			if off == ^uint32(0) || int(off) >= len(oldGf) {
				continue
			}
			if syms[off] == nil {
				syms[off] = &fdSym{off: off, kind: k, first: fi}
				offs = append(offs, off)
			}
		}
	}
	sort.Slice(offs, func(a, b int) bool { return offs[a] < offs[b] })
	aligns := classify(syms, offs, uint32(len(oldGf)))

	regions := make([][]*fdSym, len(aligns))
	placed := map[uint32]bool{}
	for j := range new.Funcs {
		i := m.NewToOld[j]
		if i < 0 {
			continue
		}
		f := old.Funcs[i]
		npc, nfd, _ := op.Record(f.FuncOff)
		rec := oft[f.FuncOff:]
		for k := range int(nfd) {
			off := binary.LittleEndian.Uint32(rec[gobin.FuncSize+4*int(npc)+4*k:])
			if s := syms[off]; s != nil && !placed[off] {
				placed[off] = true
				regions[s.region] = append(regions[s.region], s)
			}
		}
	}

	nmap := newNameOffMap(op.Table(op.Funcnametab), np.Table(np.Funcnametab))
	om, nm := old.Mod, new.Mod
	var gf []byte
	for r, cl := range regions {
		for uint64(len(gf))%uint64(aligns[r]) != 0 {
			gf = append(gf, 0)
		}
		for _, s := range cl {
			p := uint32(len(gf))
			bp.GfOff[s.off] = p
			gf = append(gf, oldGf[s.off:s.off+s.size]...)
			d := gf[p : p+s.size]
			switch s.kind {
			case 3: // inline tree: {funcID u8, pad[3], nameOff u32, parentPc u32, startLine u32}
				for e := 0; e+16 <= len(d); e += 16 {
					if no, ok := nmap.Map(binary.LittleEndian.Uint32(d[e+4:])); ok {
						binary.LittleEndian.PutUint32(d[e+4:], no)
					}
				}
			case 2: // stack objects: n uintptr, then {off, size, ptrBytes int32, gcdataoff u32}
				if len(d) >= 8 {
					n := binary.LittleEndian.Uint64(d)
					for r := uint64(0); r < n && 8+16*(r+1) <= uint64(len(d)); r++ {
						o := 8 + 16*r + 12
						v := binary.LittleEndian.Uint32(d[o:])
						if nv, cls := mp.mapAddr(om.Rodata+uint64(v), nil); cls == rcData {
							binary.LittleEndian.PutUint32(d[o:], uint32(nv-nm.Rodata))
						}
					}
				}
			case 7: // wrapinfo: the textOff of the wrapped function
				if len(d) >= 4 {
					v := binary.LittleEndian.Uint32(d)
					if nv, cls := mp.mapAddr(om.Text+uint64(v), nil); cls != rcTextUnmatch && cls != rcOutside {
						binary.LittleEndian.PutUint32(d, uint32(nv-nm.Text))
					}
				}
			}
		}
	}
	return gf
}

// classify splits the old blob into the linker's alignment regions and sizes
// every symbol. The linker sorts the funcdata symbols by decreasing symbol
// alignment, stably (cmd/link/internal/ld/pcln.go, generateFuncdata), so the
// blob is a sequence of regions each in first-use order, and a region ends
// exactly where first-use order restarts. Which alignment the compiler gave
// which symbol is not modelled: that is a table that has changed between
// releases, and the image states the answer. Each symbol's size is the gap
// to the next, so the padding the linker inserted travels with the symbol
// before it and a blob whose order does not change is rebuilt byte for byte.
//
// The regions' own alignments are inferred the same way -- the largest power
// of two, up to the linker's maximum, that divides every offset in the
// region -- and are used only where a region has to start somewhere new.
func classify(syms map[uint32]*fdSym, offs []uint32, total uint32) []uint32 {
	const maxAlign = 32
	aligns := []uint32{maxAlign}
	region, prevFirst := 0, -1
	for k, off := range offs {
		s := syms[off]
		if k+1 < len(offs) {
			s.size = offs[k+1] - off
		} else {
			s.size = total - off
		}
		if s.first < prevFirst {
			region++
			aligns = append(aligns, maxAlign)
		}
		prevFirst = s.first
		s.region = region
		for aligns[region] > 1 && off%aligns[region] != 0 {
			aligns[region] >>= 1
		}
	}
	return aligns
}
