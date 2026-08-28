package main

import (
	"fmt"
	"os"
	"sort"
)

// ---------------------------------------------------------------- level 5
//
// .go.type is the second-largest source of wrong bytes and the one the codec
// works hardest on: delta/typedesc.go walks the old binary's descriptors and
// rewrites every nameOff, typeOff, textOff and ptrToThis through the same
// address maps .text uses. The question this ladder answers is whether what
// is left is a field the walker rewrote and got wrong -- which a field-fix
// layer could repair -- or a descriptor that simply did not exist before.

const (
	tdName = iota
	tdType
	tdText
	tdPtrToThis
	tdMethod
	tdOther
	nTypeRole
)

var typeRoleNames = [nTypeRole]string{
	tdName:      "nameOff",
	tdType:      "typeOff",
	tdText:      "textOff",
	tdPtrToThis: "ptrToThis",
	tdMethod:    "method-table entry",
	tdOther:     "not a field the walker rewrote",
}

func roleIndex(r byte) int {
	switch r {
	case 'n':
		return tdName
	case 't':
		return tdType
	case 'x':
		return tdText
	case 'p':
		return tdPtrToThis
	case 'M':
		return tdMethod
	}
	return tdOther
}

func (c *ctx) byTypeField() {
	name := ""
	for _, s := range c.nb.Order {
		if s.Name == ".go.type" {
			name = s.Name
		}
	}
	if name == "" || len(c.sites) == 0 {
		fmt.Fprintf(os.Stderr, "\n-- 5. .go.type: no type section or no descriptor sites\n")
		return
	}
	s := c.nb.Sects[name]
	lo, hi := int(s.Off), int(s.Off+s.Size)
	role := make([]int8, hi-lo)
	for i := range role {
		role[i] = tdOther
	}
	// A descriptor's extent marks its bytes as having an old counterpart:
	// a wrong byte outside every placed descriptor belongs to something the
	// old binary did not hold.
	old := make([]bool, hi-lo)
	nDesc, descBytes := 0, 0
	for _, st := range c.sites {
		if st.Role == 'D' {
			nDesc++
			for k := st.Off; k < st.Off+st.N; k++ {
				if k >= lo && k < hi && !old[k-lo] {
					old[k-lo] = true
					descBytes++
				}
			}
			continue
		}
		r := int8(roleIndex(st.Role))
		for k := st.Off; k < st.Off+st.N; k++ {
			if k >= lo && k < hi {
				role[k-lo] = r
			}
		}
	}

	type cell struct {
		pos  []int
		runs int
	}
	// [role][0 = descriptor exists in the old image, 1 = new descriptor]
	var grid [nTypeRole][2]cell
	for p := lo; p < hi; p++ {
		if c.pred[p] == c.new[p] {
			continue
		}
		n := 0
		if !old[p-lo] {
			n = 1
		}
		g := &grid[role[p-lo]][n]
		g.pos = append(g.pos, p)
	}
	for _, r := range c.regs {
		if r.s < lo || r.s >= hi {
			continue
		}
		n := 0
		if !old[r.s-lo] {
			n = 1
		}
		grid[role[r.s-lo]][n].runs++
	}
	var sets [][]int
	type key struct{ r, n int }
	var keys []key
	for r := range nTypeRole {
		for n := range 2 {
			if len(grid[r][n].pos) > 0 {
				keys = append(keys, key{r, n})
				sets = append(sets, grid[r][n].pos)
			}
		}
	}
	marg := c.marginals(sets)
	var rows []row
	for i, k := range keys {
		which := "changed descriptor"
		if k.n == 1 {
			which = "NEW descriptor"
		}
		rows = append(rows, row{fmt.Sprintf("%-30s %s", typeRoleNames[k.r], which),
			0, len(grid[k.r][k.n].pos), grid[k.r][k.n].runs, marg[i].comp, marg[i].raw})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].marginal > rows[j].marginal })
	fmt.Fprintf(os.Stderr, "\n-- 5. .go.type by descriptor field (%d B section, %d descriptors placed covering %d B)\n",
		hi-lo, nDesc, descBytes)
	printRows("5. .go.type", rows, false)
}
