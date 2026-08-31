package delta

import (
	"encoding/binary"

	"github.com/wjordan/presage/delta/gobin"
)

// predictModuledata overwrites the moduledata fields whose new values the
// skeleton already fixes: the pclntab table pointers and lengths, the
// section bounds, and the transmitted moduledata values. The pointer
// rewrite that precedes it gets these nearly right but not exactly -- a
// table length is not a pointer, and a bound can move further than its
// section's shift. Each field is written only when the old binary's field
// equals the same derivation on the old skeleton, so a build this rule does
// not describe keeps the positional prediction and pays correction bytes,
// never correctness.
func predictModuledata(old, new *gobin.Bin, dst []byte) {
	md := new.Sects[".go.module"]
	os := old.Sects[".go.module"]
	if md == nil || os == nil || old.Mod == nil || new.Mod == nil {
		return
	}
	type field struct {
		off      int
		old, new uint64
	}
	var fs []field
	tab := func(off int, o, n gobin.Range, oldAddr, newAddr uint64, elem uint64) {
		fs = append(fs, field{off, oldAddr + o.Off, newAddr + n.Off},
			field{off + 8, o.Len / elem, n.Len / elem},
			field{off + 16, o.Len / elem, n.Len / elem})
	}
	op, np := old.Pcln, new.Pcln
	oa, na := op.Addr, np.Addr
	fs = append(fs, field{0, oa, na})
	tab(8, op.Funcnametab, np.Funcnametab, oa, na, 1)
	tab(32, op.Cutab, np.Cutab, oa, na, 4)
	tab(56, op.Filetab, np.Filetab, oa, na, 1)
	tab(80, op.Pctab, np.Pctab, oa, na, 1)
	tab(104, op.Functab, np.Functab, oa, na, 1)
	fs = append(fs, field{128, oa + op.Functab.Off, na + np.Functab.Off},
		field{136, uint64(op.NFunc + 1), uint64(np.NFunc + 1)},
		field{144, uint64(op.NFunc + 1), uint64(np.NFunc + 1)})
	om, nm := old.Mod, new.Mod
	for _, v := range [][3]uint64{
		{152, om.Findfunctab, nm.Findfunctab},
		{160, om.Minpc, nm.Minpc}, {168, om.Maxpc, nm.Maxpc},
		{176, om.Text, nm.Text},
		{184, (om.Maxpc + 15) &^ 15, (nm.Maxpc + 15) &^ 15},
		{296, om.Types, nm.Types}, {304, om.Typedesclen, nm.Typedesclen},
		{312, om.Etypes, nm.Etypes},
		{320, om.Itaboffset, nm.Itaboffset}, {328, om.Itabsize, nm.Itabsize},
		{336, om.Rodata, nm.Rodata}, {344, om.Gofunc, nm.Gofunc},
		{352, om.Epclntab, nm.Epclntab},
	} {
		fs = append(fs, field{int(v[0]), v[1], v[2]})
	}
	// noptrdata .. enoptrbss, then covctrs, ecovctrs and end, which a build
	// without coverage counters all leaves at the noptrbss bound.
	bounds := []struct {
		off  int
		name string
		end  bool
	}{
		{192, ".noptrdata", false}, {200, ".noptrdata", true},
		{208, ".data", false}, {216, ".data", true},
		{224, ".bss", false}, {232, ".bss", true},
		{240, ".noptrbss", false}, {248, ".noptrbss", true},
		{256, ".noptrbss", true}, {264, ".noptrbss", true}, {272, ".noptrbss", true},
	}
	for _, b := range bounds {
		o, n := old.Sects[b.name], new.Sects[b.name]
		if o == nil || n == nil {
			continue
		}
		ov, nv := o.Addr, n.Addr
		if b.end {
			ov, nv = o.Addr+o.Size, n.Addr+n.Size
		}
		fs = append(fs, field{b.off, ov, nv})
	}
	for _, f := range fs {
		if f.off+8 > len(os.Data) || f.off+8 > len(dst) {
			continue
		}
		if binary.LittleEndian.Uint64(os.Data[f.off:]) != f.old {
			continue
		}
		binary.LittleEndian.PutUint64(dst[f.off:], f.new)
	}
}
