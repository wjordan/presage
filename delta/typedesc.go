package delta

import (
	"encoding/binary"

	"github.com/wjordan/presage/delta/gobin"
)

// Type descriptors hold three kinds of 32-bit relative field that a content
// map cannot fix, because they are not absolute pointers: nameOff and
// typeOff, relative to moduledata.types, and textOff, relative to
// moduledata.text. Left alone they are the largest single source of
// mispredicted bytes in .go.type, and there are hundreds of thousands of
// them.
//
// The walker starts from the release's roots -- the sorted descriptor
// section's head and the itabs from 1.27, the .typelink and .itablink
// sections before it -- follows every *Type pointer and typeOff to reach the
// descriptors that are not typelinked, and re-targets each field through the
// mapper. The rewritten value is written into the predicted section at the
// field's own mapped position. Nothing about any of this is transmitted;
// both sides walk the old binary.
//
// Layout constants are internal/abi/type.go's; the two that a release moved
// are in the gobin.Layout descriptor (docs/go-module-design.md 2.9).
const (
	kindMask   = 0x1f
	kindArray  = 17
	kindChan   = 18
	kindFunc   = 19
	kindIface  = 20
	kindMap    = 21
	kindPtr    = 22
	kindSlice  = 23
	kindStruct = 25
	kindMax    = 26

	tflagUncommon = 1 << 0
	sizeType      = 48
	sizeUncommon  = 16
	sizeMethod    = 16
	sizeImethod   = 8
	sizeField     = 24
)

// baseSize is sizeof(the kind's descriptor struct) on amd64.
func baseSize(kind byte, lay *gobin.Layout) int {
	switch kind {
	case kindArray:
		return 72
	case kindChan:
		return 64
	case kindFunc:
		return 56
	case kindIface:
		return 80
	case kindMap:
		return lay.MapSize
	case kindPtr, kindSlice:
		return 56
	case kindStruct:
		return 80
	default:
		return sizeType
	}
}

// descSize is abi.Type.DescriptorSize for the descriptor at section offset o.
func descSize(d []byte, o uint64, lay *gobin.Layout) int {
	kind := d[o+23] & kindMask
	if kind == 0 || kind > kindMax {
		return -1
	}
	size := baseSize(kind, lay)
	mcount := 0
	if d[o+20]&tflagUncommon != 0 {
		ut := o + uint64(size)
		if ut+sizeUncommon > uint64(len(d)) {
			return -1
		}
		mcount = int(binary.LittleEndian.Uint16(d[ut+4:]))
		size += sizeUncommon
	}
	switch kind {
	case kindFunc:
		in := int(binary.LittleEndian.Uint16(d[o+48:]))
		out := int(binary.LittleEndian.Uint16(d[o+50:]) & 0x7fff)
		size += (in + out) * 8
	case kindIface:
		size += int(binary.LittleEndian.Uint64(d[o+64:])) * sizeImethod
	case kindStruct:
		size += int(binary.LittleEndian.Uint64(d[o+64:])) * sizeField
	}
	return size + mcount*sizeMethod
}

// typeWalk holds the state of one pass over the descriptors.
type typeWalk struct {
	old, new *gobin.Bin
	os       *gobin.Section
	d        []byte
	dm       *dataMap
	mp       *mapper
	pred     []byte // the predicted section, or nil on a vote-only pass
	// vote, when set, is called for every relative field with the field's
	// old section offset, the old absolute target, and the field kind
	// ('n' nameOff, 't' typeOff, 'x' textOff).
	vote func(off, target uint64, kind byte)
	// site, when set, is called for every field the walk rewrites and for
	// every descriptor it enters, with the old section offset, the width
	// and the role in w.role. It exists for measurement (delta/measure.go)
	// and is nil on every path the encoder or the decoder takes.
	site    func(off uint64, n int, role byte)
	role    byte
	visited map[uint64]bool
	queue   []uint64
}

// mark reports one rewritten field to a measurement callback.
func (w *typeWalk) mark(off uint64) {
	if w.site != nil {
		w.site(off, 4, w.role)
	}
}

// rewriteTypeOffsets rewrites the relative fields of the old binary's
// descriptors into pred, the predicted new section named sect (the one
// holding moduledata.types), laid out through dm.
func rewriteTypeOffsets(old, new *gobin.Bin, sect string, dm *dataMap, mp *mapper, pred []byte, vote func(off, target uint64, kind byte)) {
	os, ns := old.Sects[sect], new.Sects[sect]
	if os == nil || ns == nil || dm == nil {
		return
	}
	w := &typeWalk{old: old, new: new, os: os, d: os.Data, dm: dm, mp: mp, pred: pred, vote: vote,
		visited: map[uint64]bool{}}
	w.roots()
	for len(w.queue) > 0 {
		a := w.queue[0]
		w.queue = w.queue[1:]
		w.descriptor(a - os.Addr)
	}
}

func (w *typeWalk) inSect(a uint64) bool {
	return a >= w.os.Addr && a < w.os.Addr+w.os.Size
}
func (w *typeWalk) u32(off uint64) uint32 { return binary.LittleEndian.Uint32(w.d[off:]) }
func (w *typeWalk) u64(off uint64) uint64 { return binary.LittleEndian.Uint64(w.d[off:]) }

// place is where an old section offset lands in the predicted section.
func (w *typeWalk) place(off uint64) int64 {
	return int64(off) + w.dm.Delta[min(int(off)/w.dm.Block, len(w.dm.Delta)-1)]
}

func (w *typeWalk) put32(off uint64, v uint32) {
	if w.pred == nil {
		return
	}
	if p := w.place(off); p >= 0 && p+4 <= int64(len(w.pred)) {
		binary.LittleEndian.PutUint32(w.pred[p:], v)
	}
}

// mapTypesOff maps a types-relative offset. The target is usually in the
// type section, and goes through its content map, but name data can sit in
// .noptrdata or .rodata, which the mapper handles the same way.
func (w *typeWalk) mapTypesOff(off uint32) (uint32, bool) {
	nv, cls := w.mp.mapAddr(w.old.Mod.Types+uint64(off), nil)
	if cls == rcOutside || cls == rcTextUnmatch {
		return off, false
	}
	return uint32(nv - w.new.Mod.Types), true
}

func (w *typeWalk) mapTextOff(off uint32) (uint32, bool) {
	nv, cls := w.mp.mapAddr(w.old.Mod.Text+uint64(off), nil)
	if cls != rcTextSelf && cls != rcTextMatched && cls != rcTextNone {
		return off, false
	}
	return uint32(nv - w.new.Mod.Text), true
}

func (w *typeWalk) enqueue(a uint64) {
	if a == 0 || !w.inSect(a) || w.visited[a] {
		return
	}
	w.visited[a] = true
	w.queue = append(w.queue, a)
}

func (w *typeWalk) doName(off uint64) {
	w.mark(off)
	if w.vote != nil {
		w.vote(off, w.old.Mod.Types+uint64(w.u32(off)), 'n')
	}
	if v, ok := w.mapTypesOff(w.u32(off)); ok {
		w.put32(off, v)
	}
}

// doNameData rewrites a name's trailing pkgPath nameOff (flag 1<<2), which
// lives at the end of the name's own variable-length encoding.
func (w *typeWalk) doNameData(a uint64) {
	if !w.inSect(a) {
		return
	}
	o := a - w.os.Addr
	if o+2 > uint64(len(w.d)) {
		return
	}
	flag := w.d[o]
	if flag&(1<<2) == 0 {
		return
	}
	p, ok := w.skipVarintString(o + 1)
	if !ok {
		return
	}
	if flag&(1<<1) != 0 { // a struct tag follows the name
		if p, ok = w.skipVarintString(p); !ok {
			return
		}
	}
	if p+4 > uint64(len(w.d)) {
		return
	}
	w.mark(p)
	if w.vote != nil {
		w.vote(p, w.old.Mod.Types+uint64(w.u32(p)), 'n')
	}
	if v, ok := w.mapTypesOff(w.u32(p)); ok {
		w.put32(p, v)
	}
}

func (w *typeWalk) skipVarintString(p uint64) (uint64, bool) {
	var n uint64
	for shift := 0; ; shift += 7 {
		if p >= uint64(len(w.d)) || shift > 56 {
			return 0, false
		}
		x := w.d[p]
		p++
		n |= uint64(x&0x7f) << shift
		if x&0x80 == 0 {
			break
		}
	}
	if p+n < p || p+n > uint64(len(w.d)) {
		return 0, false
	}
	return p + n, true
}

func (w *typeWalk) doNameOff(off uint64) {
	w.doNameData(w.old.Mod.Types + uint64(w.u32(off)))
	w.doName(off)
}

func (w *typeWalk) doType(off uint64) {
	v := w.u32(off)
	if v == 0 {
		return
	}
	w.mark(off)
	if w.vote != nil {
		w.vote(off, w.old.Mod.Types+uint64(v), 't')
	}
	w.enqueue(w.old.Mod.Types + uint64(v))
	if nv, ok := w.mapTypesOff(v); ok {
		w.put32(off, nv)
	}
}

func (w *typeWalk) doText(off uint64) {
	v := w.u32(off)
	if v == ^uint32(0) {
		return
	}
	w.mark(off)
	if w.vote != nil {
		w.vote(off, w.old.Mod.Text+uint64(v), 'x')
	}
	if nv, ok := w.mapTextOff(v); ok {
		w.put32(off, nv)
	}
}

// roots seeds the walk from the release's typelink-flagged descriptors and
// its itabs, which is the one place the descriptor layout reaches the walk.
func (w *typeWalk) roots() {
	if !w.old.Lay.SortedTypes {
		w.linkRoots()
		return
	}
	om := w.old.Mod
	a := om.Types + 8
	for end := om.Types + om.Typedesclen; a < end && w.inSect(a); {
		a = (a + 7) &^ 7
		o := a - w.os.Addr
		if o+sizeType > uint64(len(w.d)) {
			break
		}
		w.enqueue(a)
		sz := descSize(w.d, o, w.old.Lay)
		if sz <= 0 {
			break
		}
		a += uint64(sz)
	}
	// itabs: {Inter *InterfaceType, Type *Type, Hash u32, _ [4]byte, Fun [n]uintptr}
	a = om.Types + om.Itaboffset
	for end := a + om.Itabsize; a+24 <= end && w.inSect(a); {
		o := a - w.os.Addr
		inter, typ := w.u64(o), w.u64(o+8)
		w.enqueue(inter)
		w.enqueue(typ)
		n := 0
		if w.inSect(inter) && inter-w.os.Addr+72 <= uint64(len(w.d)) {
			n = int(w.u64(inter - w.os.Addr + 64)) // len(InterfaceType.Methods)
		}
		if n < 0 || n > 1<<16 {
			break
		}
		a += 24 + 8*uint64(n)
	}
}

// linkRoots seeds the walk from the .typelink and .itablink sections, which
// is where the releases before 1.27 keep what the sorted section's head and
// itab range hold after it. .typelink holds 32-bit offsets from
// moduledata.types; .itablink holds pointers to the itabs themselves.
func (w *typeWalk) linkRoots() {
	om := w.old.Mod
	if tl := w.old.Sects[".typelink"]; tl != nil {
		for i := 0; i+4 <= len(tl.Data); i += 4 {
			w.enqueue(om.Types + uint64(binary.LittleEndian.Uint32(tl.Data[i:])))
		}
	}
	if il := w.old.Sects[".itablink"]; il != nil {
		for i := 0; i+8 <= len(il.Data); i += 8 {
			a := binary.LittleEndian.Uint64(il.Data[i:])
			if !w.inSect(a) || a-w.os.Addr+16 > uint64(len(w.d)) {
				continue
			}
			w.enqueue(w.u64(a - w.os.Addr))
			w.enqueue(w.u64(a - w.os.Addr + 8))
		}
	}
}

// descriptor rewrites one descriptor and enqueues everything it points at.
func (w *typeWalk) descriptor(o uint64) {
	if o+sizeType > uint64(len(w.d)) {
		return
	}
	kind := w.d[o+23] & kindMask
	if kind == 0 || kind > kindMax {
		return
	}
	tflag := w.d[o+20]
	if w.site != nil {
		if sz := descSize(w.d, o, w.old.Lay); sz > 0 {
			w.site(o, sz, 'D')
		}
	}
	w.role = 'n'
	w.doNameOff(o + 40) // Str
	w.role = 'p'
	w.doType(o + 44) // PtrToThis
	base := uint64(baseSize(kind, w.old.Lay))
	var ut uint64
	if tflag&tflagUncommon != 0 {
		ut = o + base
		if ut+sizeUncommon <= uint64(len(w.d)) {
			w.role = 'n'
			w.doNameOff(ut) // PkgPath
		}
	}
	addStart := o + base
	if ut != 0 {
		addStart += sizeUncommon
	}
	switch kind {
	case kindArray:
		w.enqueue(w.u64(o + 48))
		w.enqueue(w.u64(o + 56))
	case kindChan, kindPtr, kindSlice:
		w.enqueue(w.u64(o + 48))
	case kindMap:
		w.enqueue(w.u64(o + 48))
		w.enqueue(w.u64(o + 56))
		w.enqueue(w.u64(o + 64))
	case kindFunc:
		in := uint64(binary.LittleEndian.Uint16(w.d[o+48:]))
		out := uint64(binary.LittleEndian.Uint16(w.d[o+50:]) & 0x7fff)
		for k := range in + out {
			if p := addStart + 8*k; p+8 <= uint64(len(w.d)) {
				w.enqueue(w.u64(p))
			}
		}
	case kindIface:
		w.role = 'n'
		w.doNameData(w.u64(o + 48)) // PkgPath
		ms, n := w.u64(o+56), w.u64(o+64)
		if w.inSect(ms) && n < 1<<16 {
			mo := ms - w.os.Addr
			for k := uint64(0); k < n && mo+sizeImethod*(k+1) <= uint64(len(w.d)); k++ {
				w.role = 'n'
				w.doNameOff(mo + sizeImethod*k)
				w.role = 't'
				w.doType(mo + sizeImethod*k + 4)
			}
		}
	case kindStruct:
		w.role = 'n'
		w.doNameData(w.u64(o + 48)) // PkgPath
		fs, n := w.u64(o+56), w.u64(o+64)
		if w.inSect(fs) && n < 1<<16 {
			fo := fs - w.os.Addr
			for k := uint64(0); k < n && fo+sizeField*(k+1) <= uint64(len(w.d)); k++ {
				w.role = 'n'
				w.doNameData(w.u64(fo + sizeField*k))
				w.enqueue(w.u64(fo + sizeField*k + 8))
			}
		}
	}
	if ut != 0 && ut+sizeUncommon <= uint64(len(w.d)) {
		mcount := uint64(binary.LittleEndian.Uint16(w.d[ut+4:]))
		moff := uint64(w.u32(ut + 8))
		w.role = 'M'
		for k := range mcount {
			mo := ut + moff + sizeMethod*k
			if mo+sizeMethod > uint64(len(w.d)) {
				break
			}
			w.doNameOff(mo)
			w.doType(mo + 4)
			w.doText(mo + 8)
			w.doText(mo + 12)
		}
	}
}
