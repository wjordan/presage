package delta

import (
	"encoding/binary"

	"github.com/wjordan/go-binsync/delta/gobin"
	"github.com/wjordan/go-binsync/delta/internal/lz"
)

// This file exists for measurement. bench/goattr attributes the residual --
// the bytes the prediction gets wrong -- to the layer that would have to fix
// them, and to do that it needs the prediction itself and the tables behind
// it. Nothing in Encode or Apply calls anything here, and no patch byte
// depends on it.

// Prediction is the Go-aware transform's prediction of the new file,
// together with what an attribution probe needs to explain it.
//
// NewToOld and OldToNew are the decoder-side match -- the one predictText
// actually indexed -- so a probe classifying a function by how it was
// predicted reads the same table the predictor did.
type Prediction struct {
	Pred     []byte
	NewToOld []int
	OldToNew []int

	Exact, Norm, Content, Unmatched, UnmatchedOld int
	LayoutLen, Stage1aLen, Stage1bLen             int

	g *goPred
}

// Predict runs the Go-aware transform as far as its prediction and stops.
// It is exported for measurement only; Encode does not call it, and a
// prediction returned here is byte-identical to the one Encode's correction
// is written against.
func Predict(old, new []byte) (*Prediction, error) {
	g, err := predictGoAMD64(old, new, maxTransform)
	if err != nil {
		return nil, err
	}
	return &Prediction{
		Pred: g.pred, NewToOld: g.m2.NewToOld, OldToNew: g.m2.OldToNew,
		Exact: g.m.Exact, Norm: g.m.Norm, Content: g.m.Content,
		Unmatched: g.m.Unmatched, UnmatchedOld: g.m.UnmatchedOld,
		LayoutLen: len(g.layRaw), Stage1aLen: len(g.s1a), Stage1bLen: len(g.s1b),
		g: g,
	}, nil
}

// TypeSite is one place the type-descriptor rewriter wrote, in offsets into
// the predicted file. Role is 'n' nameOff, 't' typeOff, 'x' textOff, 'p'
// ptrToThis, 'M' a field of an uncommon-type method table, or 'D' for the
// extent of a whole descriptor (N is then the descriptor's size).
type TypeSite struct {
	Off  int
	Old  int // the field's offset in the old binary's type section
	N    int
	Role byte
}

// TypeSites replays the descriptor walk that predicted the type section and
// reports where it wrote. Sites are in the order the walk visits them, which
// is not offset order, and a descriptor's 'D' extent precedes its fields.
func (p *Prediction) TypeSites() []TypeSite {
	g := p.g
	tsec := g.ob.SectionOf(g.ob.Mod.Types)
	if tsec == nil {
		return nil
	}
	os, ns := g.ob.Sects[tsec.Name], g.skel.Sects[tsec.Name]
	dm := g.lay.DataMaps[tsec.Name]
	if os == nil || ns == nil || dm == nil {
		return nil
	}
	var out []TypeSite
	w := &typeWalk{old: g.ob, new: g.skel, os: os, d: os.Data, dm: dm, mp: g.mp,
		visited: map[uint64]bool{}}
	w.site = func(off uint64, n int, role byte) {
		q := w.place(off)
		if q < 0 || q+int64(n) > int64(ns.Size) {
			return
		}
		out = append(out, TypeSite{Off: int(ns.Off + uint64(q)), Old: int(off), N: n, Role: role})
	}
	w.roots()
	for len(w.queue) > 0 {
		a := w.queue[0]
		w.queue = w.queue[1:]
		w.descriptor(a - os.Addr)
	}
	return out
}

// TypeSectionName is the section moduledata.types lives in, or "".
func (p *Prediction) TypeSectionName() string {
	if s := p.g.ob.SectionOf(p.g.ob.Mod.Types); s != nil {
		return s.Name
	}
	return ""
}

// Descriptor is one type descriptor found by walking a single binary, which
// is what a probe asking "does this new descriptor have an old counterpart"
// needs and the encoder's walk (typedesc.go) cannot give: that one walks the
// old image and maps into the new, so it never enumerates a descriptor the
// old image does not hold.
type Descriptor struct {
	Off  uint64 // offset into the section holding moduledata.types
	Size int
	Kind byte
	Name string
	Refs []uint64 // section offsets of the descriptors this one is built from
}

// WalkDescriptors enumerates one binary's type descriptors, reusing
// typedesc.go's layout constants and reachability rule (typelink-flagged
// descriptors and itabs as roots, then every *Type the descriptors point at).
// Measurement only.
func WalkDescriptors(b *gobin.Bin) []Descriptor {
	sec := b.SectionOf(b.Mod.Types)
	if sec == nil {
		return nil
	}
	d, base := sec.Data, sec.Addr
	in := func(a uint64) bool { return a >= base && a < base+uint64(len(d)) }
	u32 := func(o uint64) uint32 { return binary.LittleEndian.Uint32(d[o:]) }
	u64 := func(o uint64) uint64 { return binary.LittleEndian.Uint64(d[o:]) }

	seen := map[uint64]bool{}
	var queue []uint64
	push := func(a uint64) {
		if a != 0 && in(a) && !seen[a] {
			seen[a] = true
			queue = append(queue, a)
		}
	}
	a := b.Mod.Types + 8
	for end := b.Mod.Types + b.Mod.Typedesclen; a < end && in(a); {
		a = (a + 7) &^ 7
		if a-base+sizeType > uint64(len(d)) {
			break
		}
		push(a)
		sz := descSize(d, a-base)
		if sz <= 0 {
			break
		}
		a += uint64(sz)
	}
	a = b.Mod.Types + b.Mod.Itaboffset
	for end := a + b.Mod.Itabsize; a+24 <= end && in(a); {
		o := a - base
		inter, typ := u64(o), u64(o+8)
		push(inter)
		push(typ)
		n := 0
		if in(inter) && inter-base+72 <= uint64(len(d)) {
			n = int(u64(inter - base + 64))
		}
		if n < 0 || n > 1<<16 {
			break
		}
		a += 24 + 8*uint64(n)
	}

	var out []Descriptor
	for len(queue) > 0 {
		addr := queue[0]
		queue = queue[1:]
		o := addr - base
		if o+sizeType > uint64(len(d)) {
			continue
		}
		kind := d[o+23] & kindMask
		sz := descSize(d, o)
		if kind == 0 || kind > kindMax || sz <= 0 {
			continue
		}
		de := Descriptor{Off: o, Size: sz, Kind: kind, Name: descName(b, sec, u32(o+40))}
		ref := func(t uint64) {
			push(t)
			if in(t) {
				de.Refs = append(de.Refs, t-base)
			}
		}
		base0 := uint64(baseSize(kind))
		addStart := o + base0
		if d[o+20]&tflagUncommon != 0 {
			addStart += sizeUncommon
		}
		switch kind {
		case kindArray:
			ref(u64(o + 48))
			ref(u64(o + 56))
		case kindChan, kindPtr, kindSlice:
			ref(u64(o + 48))
		case kindMap:
			ref(u64(o + 48))
			ref(u64(o + 56))
			ref(u64(o + 64))
		case kindFunc:
			in0 := uint64(binary.LittleEndian.Uint16(d[o+48:]))
			out0 := uint64(binary.LittleEndian.Uint16(d[o+50:]) & 0x7fff)
			for k := range in0 + out0 {
				if p := addStart + 8*k; p+8 <= uint64(len(d)) {
					ref(u64(p))
				}
			}
		case kindIface:
			ms, n := u64(o+56), u64(o+64)
			if in(ms) && n < 1<<16 {
				mo := ms - base
				for k := uint64(0); k < n && mo+sizeImethod*(k+1) <= uint64(len(d)); k++ {
					if v := u32(mo + sizeImethod*k + 4); v != 0 {
						ref(b.Mod.Types + uint64(v))
					}
				}
			}
		case kindStruct:
			fs, n := u64(o+56), u64(o+64)
			if in(fs) && n < 1<<16 {
				fo := fs - base
				for k := uint64(0); k < n && fo+sizeField*(k+1) <= uint64(len(d)); k++ {
					ref(u64(fo + sizeField*k + 8))
				}
			}
		}
		if d[o+20]&tflagUncommon != 0 {
			ut := o + base0
			if ut+sizeUncommon <= uint64(len(d)) {
				mcount := uint64(binary.LittleEndian.Uint16(d[ut+4:]))
				moff := uint64(u32(ut + 8))
				for k := range mcount {
					mo := ut + moff + sizeMethod*k
					if mo+sizeMethod > uint64(len(d)) {
						break
					}
					if v := u32(mo + 4); v != 0 {
						push(b.Mod.Types + uint64(v))
					}
				}
			}
		}
		out = append(out, de)
	}
	return out
}

// descName reads the abi.Name at a types-relative offset. Name data can sit
// outside the type section, so the section is resolved per name.
func descName(b *gobin.Bin, self *gobin.Section, off uint32) string {
	a := b.Mod.Types + uint64(off)
	s := self
	if a < s.Addr || a >= s.Addr+s.Size {
		if s = b.SectionOf(a); s == nil || s.Data == nil {
			return ""
		}
	}
	d, o := s.Data, a-s.Addr
	if o+2 > uint64(len(d)) {
		return ""
	}
	p := o + 1
	var n uint64
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

// The correction encoder's two parameters and its match engine, exposed so
// that a probe can re-encode the same regions a different way and price the
// result against the shipped format (bench/goattr level c). Measurement
// only: nothing here is reachable from Encode or Apply.
const (
	MergeGap = mergeGap
	SrcSlack = srcSlack
)

// EmitLZ appends to ctrl and lit the ops that rebuild dst from src.
func EmitLZ(src, dst, ctrl, lit []byte) (ctrlOut, litOut []byte) {
	return lz.Emit(src, dst, ctrl, lit)
}

// ApplyLZ rebuilds out from src and the streams, and returns what is left of
// them, so a probe can check that its own stream really decodes.
func ApplyLZ(ctrl, lit, src, out []byte) (ctrlRest, litRest []byte, err error) {
	r := &lz.Reader{Ctrl: ctrl, Lit: lit}
	err = r.Apply(src, out)
	return r.Ctrl, r.Lit, err
}
