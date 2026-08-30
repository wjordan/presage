package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"

	"github.com/wjordan/presage/delta"
)

// ---------------------------------------------------------------- level 9
//
// Level 5 found that .go.type's residual is almost all descriptors the old
// image does not hold, not fields the walker got wrong. A descriptor with no
// old counterpart can still be *derivable*: `*T`, `[]T`, `map[K]V` and
// `func(A) B` are mechanical from their element types plus a kind and a
// header, the way eh_frame_hdr is mechanical from the FDEs. This level
// classifies the new descriptors by kind and asks, per derived kind, whether
// the types they are built from already exist in the old image.

var kindNames = map[byte]string{
	1: "bool", 2: "int", 3: "int8", 4: "int16", 5: "int32", 6: "int64",
	7: "uint", 8: "uint8", 9: "uint16", 10: "uint32", 11: "uint64", 12: "uintptr",
	13: "float32", 14: "float64", 15: "complex64", 16: "complex128",
	17: "array", 18: "chan", 19: "func", 20: "interface", 21: "map",
	22: "ptr", 23: "slice", 24: "string", 25: "struct", 26: "unsafe.Pointer",
}

// derived is the set of kinds whose descriptor is a mechanical function of
// the types it names.
var derived = map[byte]bool{17: true, 18: true, 19: true, 21: true, 22: true, 23: true}

func kindName(k byte) string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("kind%d", k)
}

func (c *ctx) typeKinds() {
	sec := c.nb.SectionOf(c.nb.Mod.Types)
	osec := c.ob.SectionOf(c.ob.Mod.Types)
	if sec == nil || osec == nil {
		fmt.Fprintf(os.Stderr, "\n-- 9. no type section\n")
		return
	}
	lo, hi := int(sec.Off), int(sec.Off+sec.Size)
	nd := delta.WalkDescriptors(c.nb)
	od := delta.WalkDescriptors(c.ob)
	oldNames := make(map[string]bool, len(od))
	for _, d := range od {
		if d.Name != "" {
			oldNames[d.Name] = true
		}
	}
	byOff := make(map[uint64]int, len(nd))
	for i, d := range nd {
		byOff[d.Off] = i
	}
	// the level-5 coverage mask: bytes a placed old descriptor occupies
	covered := make([]bool, hi-lo)
	for _, st := range c.sites {
		if st.Role != 'D' {
			continue
		}
		for k := st.Off; k < st.Off+st.N && k < hi; k++ {
			if k >= lo {
				covered[k-lo] = true
			}
		}
	}

	type kstat struct {
		n, bytes, wrong     int
		newN, newB, newW    int
		derivN, derivB      int
		nameNewN            int
		refNewN, refMissing int
	}
	stats := map[byte]*kstat{}
	get := func(k byte) *kstat {
		if stats[k] == nil {
			stats[k] = &kstat{}
		}
		return stats[k]
	}
	var newDescs []delta.Descriptor
	var derivPos, otherPos []int
	for _, d := range nd {
		s := get(d.Kind)
		s.n++
		s.bytes += d.Size
		cov, wrong := 0, 0
		for k := 0; k < d.Size; k++ {
			p := lo + int(d.Off) + k
			if p >= hi {
				break
			}
			if covered[p-lo] {
				cov++
			}
			if c.pred[p] != c.new[p] {
				wrong++
			}
		}
		s.wrong += wrong
		if 2*cov >= d.Size { // the old image holds this descriptor
			continue
		}
		s.newN++
		s.newB += d.Size
		s.newW += wrong
		newDescs = append(newDescs, d)
		for k := 0; k < d.Size; k++ {
			p := lo + int(d.Off) + k
			if p < hi && c.pred[p] != c.new[p] {
				if derived[d.Kind] {
					derivPos = append(derivPos, p)
				} else {
					otherPos = append(otherPos, p)
				}
			}
		}
		if d.Name != "" && !oldNames[d.Name] {
			s.nameNewN++
		}
		if !derived[d.Kind] {
			continue
		}
		ok, miss := true, false
		for _, r := range d.Refs {
			i, found := byOff[r]
			if !found || nd[i].Name == "" {
				miss = true
				ok = false
				continue
			}
			if !oldNames[nd[i].Name] {
				ok = false
			}
		}
		switch {
		case ok:
			s.derivN++
			s.derivB += d.Size
		case miss:
			s.refMissing++
		default:
			s.refNewN++
		}
	}

	var ks []byte
	for k := range stats {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return stats[ks[i]].newB > stats[ks[j]].newB })
	fmt.Fprintf(os.Stderr, "\n-- 9. .go.type descriptors by kind (%d in the new image, %d in the old)\n", len(nd), len(od))
	h := "  %-14s %8s %10s %10s %8s %10s %10s %10s %8s\n"
	b := "  %-14s %8d %10d %10d %8d %10d %10d %10d %8d\n"
	fmt.Fprintf(os.Stderr, h, "kind", "descs", "bytes", "wrong B", "new", "new B", "new wrong", "derivable B", "new name")
	var tot kstat
	for _, k := range ks {
		s := stats[k]
		fmt.Fprintf(os.Stderr, b, kindName(k), s.n, s.bytes, s.wrong, s.newN, s.newB, s.newW, s.derivB, s.nameNewN)
		tot.n += s.n
		tot.bytes += s.bytes
		tot.wrong += s.wrong
		tot.newN += s.newN
		tot.newB += s.newB
		tot.newW += s.newW
		tot.derivB += s.derivB
		tot.nameNewN += s.nameNewN
	}
	fmt.Fprintf(os.Stderr, b, "TOTAL", tot.n, tot.bytes, tot.wrong, tot.newN, tot.newB, tot.newW, tot.derivB, tot.nameNewN)

	fmt.Fprintf(os.Stderr, "\n-- 9b. new descriptors of a derived kind, by where their element types live\n")
	fmt.Fprintf(os.Stderr, "  %-14s %10s %10s %10s %10s\n", "kind", "new", "all in old", "some new", "unresolved")
	var d1, d2, d3, d4 int
	for _, k := range ks {
		if !derived[k] {
			continue
		}
		s := stats[k]
		fmt.Fprintf(os.Stderr, "  %-14s %10d %10d %10d %10d\n", kindName(k), s.newN, s.derivN, s.refNewN, s.refMissing)
		d1 += s.newN
		d2 += s.derivN
		d3 += s.refNewN
		d4 += s.refMissing
	}
	fmt.Fprintf(os.Stderr, "  %-14s %10d %10d %10d %10d\n", "TOTAL", d1, d2, d3, d4)

	c.derivedPrice(newDescs, sec.Data, derivPos, otherPos)
	c.typeTemplate(newDescs, od, osec.Data, sec.Data)
}

// derivedPrice measures what the new descriptors of a derived kind cost the
// patch, and what a regenerator would have to ship instead of them: the
// descriptor's own name offset and the type it is built from, which is
// everything 9c shows a same-kind template does not already supply.
func (c *ctx) derivedPrice(newDescs []delta.Descriptor, newD []byte, derivPos, otherPos []int) {
	marg := c.marginals([][]int{derivPos, otherPos})
	var offs, kinds, names, elems, gcd, eqf, tfl, ptb []byte
	var buf [10]byte
	put := func(dst *[]byte, v uint64) { *dst = append(*dst, buf[:binary.PutUvarint(buf[:], v)]...) }
	puts := func(dst *[]byte, v int64) { *dst = append(*dst, buf[:binary.PutVarint(buf[:], v)]...) }
	prev, n := uint64(0), 0
	for _, d := range newDescs {
		if !derived[d.Kind] || d.Off+56 > uint64(len(newD)) {
			continue
		}
		n++
		put(&offs, d.Off-prev)
		prev = d.Off
		kinds = append(kinds, d.Kind)
		put(&names, uint64(binary.LittleEndian.Uint32(newD[d.Off+40:])))
		e := int64(0)
		if len(d.Refs) > 0 {
			e = int64(d.Refs[0]) - int64(d.Off)
		}
		puts(&elems, e)
		// the four fields 9c shows a same-kind template does not supply
		put(&gcd, binary.LittleEndian.Uint64(newD[d.Off+32:]))
		put(&eqf, binary.LittleEndian.Uint64(newD[d.Off+24:]))
		tfl = append(tfl, newD[d.Off+20:d.Off+24]...)
		put(&ptb, binary.LittleEndian.Uint64(newD[d.Off+8:]))
	}
	cat := func(bs ...[]byte) []byte {
		var o []byte
		for _, b := range bs {
			o = append(o, b...)
		}
		return o
	}
	all := cat(offs, kinds, names, elems)
	full := cat(offs, kinds, names, elems, gcd, eqf, tfl, ptb)
	sz := xzSizes(offs, kinds, names, elems, all, full, gcd, eqf, tfl, ptb)
	rows := []row{
		{"new descriptors of a derived kind", 0, len(derivPos), 0, marg[0].comp, marg[0].raw},
		{"new descriptors of any other kind", 0, len(otherPos), 0, marg[1].comp, marg[1].raw},
	}
	printRows("9d. new descriptors, priced", rows, false)
	fmt.Fprintf(os.Stderr, "  a regenerator would ship %d derived descriptors as (offset, kind, nameOff, elem): %d B xz, net %d\n",
		n, sz[4], marg[0].comp-sz[4])
	fmt.Fprintf(os.Stderr, "    columns xz: offsets %d, kinds %d, nameOffs %d, elem deltas %d\n", sz[0], sz[1], sz[2], sz[3])
	fmt.Fprintf(os.Stderr, "  plus the four fields a same-kind template does not supply: %d B xz, net %d\n",
		sz[5], marg[0].comp-sz[5])
	fmt.Fprintf(os.Stderr, "    columns xz: GCData %d, Equal fn %d, TFlag/Align/Kind %d, PtrBytes %d\n", sz[6], sz[7], sz[8], sz[9])
}

// hdrFields are the fields of abi.Type, the 48-byte header every descriptor
// starts with, plus the first pointer after it -- which for a derived kind is
// the element type.
var hdrFields = []struct {
	name string
	off  int
	n    int
}{
	{"Size_", 0, 8}, {"PtrBytes", 8, 8}, {"Hash", 16, 4},
	{"TFlag/Align/Kind", 20, 4}, {"Equal fn", 24, 8}, {"GCData", 32, 8},
	{"Str (nameOff)", 40, 4}, {"PtrToThis", 44, 4}, {"Elem ptr", 48, 8},
}

// typeTemplate asks how much of a new derived descriptor a same-kind,
// same-Size_ old descriptor already supplies: a regenerator that copies a
// template and patches n fields pays for those n fields, not for the
// descriptor.
func (c *ctx) typeTemplate(newDescs []delta.Descriptor, od []delta.Descriptor, oldD, newD []byte) {
	type key struct {
		k byte
		s uint64
	}
	tmpl := map[key]uint64{}
	for _, d := range od {
		if !derived[d.Kind] || d.Off+48 > uint64(len(oldD)) {
			continue
		}
		kk := key{d.Kind, binary.LittleEndian.Uint64(oldD[d.Off:])}
		if _, ok := tmpl[kk]; !ok {
			tmpl[kk] = d.Off
		}
	}
	diff := make([]int, len(hdrFields))
	n, noTmpl := 0, 0
	for _, d := range newDescs {
		if !derived[d.Kind] || d.Off+56 > uint64(len(newD)) {
			continue
		}
		o, ok := tmpl[key{d.Kind, binary.LittleEndian.Uint64(newD[d.Off:])}]
		if !ok || o+56 > uint64(len(oldD)) {
			noTmpl++
			continue
		}
		n++
		for i, f := range hdrFields {
			for k := 0; k < f.n; k++ {
				if oldD[int(o)+f.off+k] != newD[int(d.Off)+f.off+k] {
					diff[i]++
					break
				}
			}
		}
	}
	fmt.Fprintf(os.Stderr, "\n-- 9c. a new derived descriptor against a same-kind, same-Size_ old template\n")
	fmt.Fprintf(os.Stderr, "  %d of %d matched a template (%d had none)\n", n, n+noTmpl, noTmpl)
	fmt.Fprintf(os.Stderr, "  %-20s %10s %8s\n", "field", "differs", "share")
	for i, f := range hdrFields {
		fmt.Fprintf(os.Stderr, "  %-20s %10d %8s\n", f.name, diff[i], pct(diff[i], max(1, n)))
	}
}
