package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"

	"github.com/wjordan/go-binsync/delta/gobin"
)

// ---------------------------------------------------------------- level 8
//
// .gopclntab is the third-largest residual and the one the codec regenerates
// most aggressively (delta/pcln.go): the functab, its _func records and
// findfunctab are all derived, and the variable-length blobs are shipped as
// stage-1b. This level asks which of those regenerations is wrong, and for
// the _func records which field -- because a field whose error is a constant
// or small delta is a regenerator bug, while one whose error is arbitrary is
// new content.

const (
	pcHdr = iota
	pcFuncname
	pcCutab
	pcFiletab
	pcPctab
	pcFtabIdx
	pcRec
	pcPcdata
	pcFuncdata
	pcFtabPad
	pcGofunc
	pcFindfunc
	nPcRegion
)

var pcRegionNames = [nPcRegion]string{
	pcHdr:      "pcHeader",
	pcFuncname: "funcnametab (stage 1a)",
	pcCutab:    "cutab (stage 1b)",
	pcFiletab:  "filetab (stage 1a)",
	pcPctab:    "pctab (stage 1b)",
	pcFtabIdx:  "functab index (entryOff,funcOff)",
	pcRec:      "_func headers",
	pcPcdata:   "_func pcdata arrays",
	pcFuncdata: "_func funcdata arrays",
	pcFtabPad:  "functab alignment padding",
	pcGofunc:   "go:func.* (stage 1b)",
	pcFindfunc: "findfunctab (regenerated)",
}

// funcField names the 11 words of a _func record, plus the four bytes packed
// into the last one (internal/abi.FuncInfo for the supported release).
type funcField struct {
	name string
	off  int
	n    int
}

var funcFields = []funcField{
	{"entryOff", 0, 4}, {"nameOff", 4, 4}, {"args", 8, 4}, {"deferreturn", 12, 4},
	{"pcsp", 16, 4}, {"pcfile", 20, 4}, {"pcln", 24, 4}, {"npcdata", 28, 4},
	{"cuOffset", 32, 4}, {"startLine", 36, 4},
	{"funcID", 40, 1}, {"flag", 41, 1}, {"pad", 42, 1}, {"nfuncdata", 43, 1},
}

func (c *ctx) rd(off, n int, b []byte) int64 {
	switch n {
	case 1:
		return int64(b[off])
	case 4:
		return int64(int32(binary.LittleEndian.Uint32(b[off:])))
	}
	return 0
}

func (c *ctx) pclnRegions() {
	sec := c.nb.Sects[".gopclntab"]
	if sec == nil {
		fmt.Fprintf(os.Stderr, "\n-- 8. no .gopclntab\n")
		return
	}
	base, size := int(sec.Off), int(sec.Size)
	p := c.nb.Pcln
	role := make([]int8, size)
	for i := range role {
		role[i] = pcFtabPad
	}
	paint := func(r gobin.Range, k int) {
		for i := int(r.Off); i < int(r.End()) && i < size; i++ {
			role[i] = int8(k)
		}
	}
	for i := 0; i < int(p.FuncnameOff) && i < size; i++ {
		role[i] = pcHdr
	}
	paint(p.Funcnametab, pcFuncname)
	paint(p.Cutab, pcCutab)
	paint(p.Filetab, pcFiletab)
	paint(p.Pctab, pcPctab)
	paint(p.Gofunc, pcGofunc)
	paint(p.Findfunctab, pcFindfunc)
	paint(gobin.Range{Off: p.Functab.Off, Len: uint64(8*p.NFunc + 4)}, pcFtabIdx)
	for _, g := range c.nb.Funcs {
		npc, nfd, _ := p.Record(g.FuncOff)
		o := int(p.Functab.Off) + int(g.FuncOff)
		paint(gobin.Range{Off: uint64(o), Len: gobin.FuncSize}, pcRec)
		paint(gobin.Range{Off: uint64(o + gobin.FuncSize), Len: uint64(4 * npc)}, pcPcdata)
		paint(gobin.Range{Off: uint64(o + gobin.FuncSize + 4*int(npc)), Len: uint64(4 * nfd)}, pcFuncdata)
	}

	region := [nPcRegion]int{}
	for _, r := range role {
		region[r]++
	}
	pos := make([][]int, nPcRegion)
	runs := [nPcRegion]int{}
	for i := 0; i < size; i++ {
		if c.pred[base+i] != c.new[base+i] {
			pos[role[i]] = append(pos[role[i]], base+i)
		}
	}
	for _, r := range c.regs {
		if r.s >= base && r.s < base+size {
			runs[role[r.s-base]]++
		}
	}
	marg := c.marginals(pos)
	var rows []row
	for k := range nPcRegion {
		rows = append(rows, row{pcRegionNames[k], region[k], len(pos[k]), runs[k], marg[k].comp, marg[k].raw})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].marginal > rows[j].marginal })
	printRows("8. .gopclntab by subtable", rows, true)

	c.pclnFields(base)
}

// pclnFields splits the _func header residual by field and asks, per field,
// whether new-pred is one value repeated -- a regenerator that is off by a
// constant -- or spread out.
func (c *ctx) pclnFields(base int) {
	p := c.nb.Pcln
	type stat struct {
		changed, wrong int
		d              map[int64]int
	}
	st := make([]stat, len(funcFields))
	for i := range st {
		st[i].d = map[int64]int{}
	}
	var arr [2]stat
	arr[0].d, arr[1].d = map[int64]int{}, map[int64]int{}
	for _, g := range c.nb.Funcs {
		o := base + int(p.Functab.Off) + int(g.FuncOff)
		for k, f := range funcFields {
			w := 0
			for b := 0; b < f.n; b++ {
				if c.pred[o+f.off+b] != c.new[o+f.off+b] {
					w++
				}
			}
			if w == 0 {
				continue
			}
			st[k].changed++
			st[k].wrong += w
			st[k].d[c.rd(o+f.off, f.n, c.new)-c.rd(o+f.off, f.n, c.pred)]++
		}
		npc, nfd, _ := p.Record(g.FuncOff)
		for a, n := range [2]int{int(npc), int(nfd)} {
			b0 := gobin.FuncSize
			if a == 1 {
				b0 += 4 * int(npc)
			}
			for k := range n {
				q := o + b0 + 4*k
				w := 0
				for b := 0; b < 4; b++ {
					if c.pred[q+b] != c.new[q+b] {
						w++
					}
				}
				if w == 0 {
					continue
				}
				arr[a].changed++
				arr[a].wrong += w
				arr[a].d[c.rd(q, 4, c.new)-c.rd(q, 4, c.pred)]++
			}
		}
	}
	fmt.Fprintf(os.Stderr, "\n-- 8b. _func records, by field (new - predicted)\n")
	fmt.Fprintf(os.Stderr, "  %-20s %10s %10s %8s %8s   %s\n", "field", "changed", "wrong B", "values", "top 10", "most common new-pred (count)")
	show := func(name string, s stat) {
		if s.changed == 0 {
			return
		}
		type kv struct {
			d, n int64
		}
		var ds []kv
		for d, n := range s.d {
			ds = append(ds, kv{d, int64(n)})
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i].n > ds[j].n })
		out, top := "", int64(0)
		for i, k := range ds {
			if i < 10 {
				top += k.n
			}
			if i < 3 {
				out += fmt.Sprintf("%+d (%d)  ", k.d, k.n)
			}
		}
		fmt.Fprintf(os.Stderr, "  %-20s %10d %10d %8d %8s   %s\n",
			name, s.changed, s.wrong, len(s.d), pct(int(top), s.changed), out)
	}
	for k, f := range funcFields {
		show(f.name, st[k])
	}
	show("pcdata[]", arr[0])
	show("funcdata[]", arr[1])
}
