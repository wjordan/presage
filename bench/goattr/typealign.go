package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"

	"github.com/wjordan/presage/delta"
)

// --------------------------------------------------------------- level 12
//
// Level 10b found that most of the wrong method-table fields are not wrong
// values but right values in the wrong place: the codec copies a matched
// descriptor's uncommon-type method array positionally, so one method the
// release inserted misaligns every entry after it -- the .text failure the
// segment map of DESIGN 3.2.1 just fixed, in another table.
//
// This level prices the per-descriptor fix before it is built. The method
// array is sorted by name, so aligning the old and new arrays is an LCS over
// their name lists; a matched entry is then predicted by copying the old
// entry's four fields *after* the walker retargeted them, which is exactly
// what the walker already wrote at the old entry's own placed position. So
// the realistic prediction needs no re-implementation of the mapper: it is
// pred[place(old entry)], read back.
//
//	12a  the census: descriptors with a method table, paired, edited
//	12b  the edit sizes, and whether the un-edited ones are already right
//	12c  per field, how often the retargeted old value is the new value
//	12d  priced -- oracle (every aligned entry correct), realistic (only the
//	     fields the existing maps retarget correctly), the alignment list's
//	     own cost, and the same two sets by correction run
//
// Run it with -levels b.

// mpair is one aligned entry: old index i holds the same method name as new
// index j.
type mpair struct{ i, j int }

// alignMethods is the LCS of two method-name lists. The arrays are sorted by
// name (exported first, then unexported, each sorted -- cmd/link writes them
// in reflect's required order), so the LCS is the merge an encoder would do.
func alignMethods(a, b []string) []mpair {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return nil
	}
	dp := make([]int32, (n+1)*(m+1))
	at := func(i, j int) int { return i*(m+1) + j }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[at(i, j)] = dp[at(i+1, j+1)] + 1
			case dp[at(i+1, j)] >= dp[at(i, j+1)]:
				dp[at(i, j)] = dp[at(i+1, j)]
			default:
				dp[at(i, j)] = dp[at(i, j+1)]
			}
		}
	}
	out := make([]mpair, 0, min(n, m))
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case a[i] == b[j]:
			out = append(out, mpair{i, j})
			i++
			j++
		case dp[at(i+1, j)] >= dp[at(i, j+1)]:
			i++
		default:
			j++
		}
	}
	return out
}

// mseg is one run of consecutive aligned entries at a constant index shift --
// the unit the alignment list would transmit.
type mseg struct{ oldIdx, newIdx, n int }

func segsOf(ps []mpair) []mseg {
	var out []mseg
	for _, p := range ps {
		if k := len(out); k > 0 {
			s := &out[k-1]
			if s.oldIdx+s.n == p.i && s.newIdx+s.n == p.j {
				s.n++
				continue
			}
		}
		out = append(out, mseg{p.i, p.j, 1})
	}
	return out
}

// mplan is one descriptor's alignment, as it would be transmitted.
type mplan struct {
	id    int // index in the old-offset-sorted descriptor list
	mdiff int // new mcount - old mcount
	segs  []mseg
}

// mplanCost encodes the alignment list in the shape align.go's planCost uses
// -- one contiguous varint column per quantity, so each column's own
// statistics compress -- and prices it with xz -T1 and with the codec's own
// compressor.
func mplanCost(ps []mplan) (xz, cz, raw int, cols [6]int) {
	var ids, mdiff, count, gaps, lens, shifts []byte
	var buf [10]byte
	put := func(dst *[]byte, v uint64) { *dst = append(*dst, buf[:binary.PutUvarint(buf[:], v)]...) }
	puts := func(dst *[]byte, v int64) { *dst = append(*dst, buf[:binary.PutVarint(buf[:], v)]...) }
	prevID := 0
	for _, p := range ps {
		put(&ids, uint64(p.id-prevID))
		prevID = p.id
		puts(&mdiff, int64(p.mdiff))
		put(&count, uint64(len(p.segs)))
		end, prevS := 0, 0
		for _, s := range p.segs {
			put(&gaps, uint64(max(0, s.newIdx-end)))
			put(&lens, uint64(s.n))
			puts(&shifts, int64(s.oldIdx-s.newIdx-prevS))
			end, prevS = s.newIdx+s.n, s.oldIdx-s.newIdx
		}
	}
	all := append(append(append(append(append(append([]byte(nil), ids...), mdiff...), count...), gaps...), lens...), shifts...)
	s := xzSizes(ids, mdiff, count, gaps, lens, shifts, all)
	return s[6], czSize(all), len(all), [6]int{s[0], s[1], s[2], s[3], s[4], s[5]}
}

func histAdd(h map[int]int, k int) { h[k]++ }

func histLine(h map[int]int, n int) string {
	var ks []int
	for k := range h {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	out := ""
	for i, k := range ks {
		if i < n {
			out += fmt.Sprintf("%d:%d  ", k, h[k])
		}
	}
	if len(ks) > n {
		out += fmt.Sprintf("(+%d more)", len(ks)-n)
	}
	return out
}

func (c *ctx) typeAlign() {
	sec := c.nb.SectionOf(c.nb.Mod.Types)
	osec := c.ob.SectionOf(c.ob.Mod.Types)
	if sec == nil || osec == nil || len(c.sites) == 0 {
		fmt.Fprintf(os.Stderr, "\n-- 12. no type section or no descriptor sites\n")
		return
	}
	lo, hi := int(sec.Off), int(sec.Off+sec.Size)
	od, nd := osec.Data, sec.Data
	u32 := func(b []byte, o int) uint32 { return binary.LittleEndian.Uint32(b[o:]) }

	// Where the walk placed each old descriptor, and where it wrote each
	// method field: mSite is the retargeted value's own position, which is
	// what an aligned copy would move.
	type placed struct {
		old, pred uint64
		size      int
	}
	var pl []placed
	mSite := make(map[int]int, 1<<16)
	for _, st := range c.sites {
		if st.Role == 'D' {
			pl = append(pl, placed{uint64(st.Old), uint64(st.Off - lo), st.N})
			continue
		}
		if st.Role == 'M' && st.N == 4 {
			if _, ok := mSite[st.Old]; !ok {
				mSite[st.Old] = st.Off
			}
		}
	}
	sort.Slice(pl, func(i, j int) bool { return pl[i].old < pl[j].old })

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
	var (
		withMeth, noCounter, nameMismatch, noNewMeth, paired int
		noEdit, insOnly, delOnly, mixedEdit                  int
		noEditWrongB, noEditWrongDesc                        int
		matchedEnt, shiftedEnt, insertedEnt, deletedEnt      int
		noSite, siteAtPlace, siteTorn, entryAllOK            int
	)
	insH, delH := map[int]int{}, map[int]int{}
	var fieldOK, fieldBad [4]int
	var oraclePos, realPos, allOraclePos, insertedPos []int
	var plans []mplan

	for k, p := range pl {
		if int(p.old)+24 > len(od) || p.size <= 0 {
			continue
		}
		odi := layout(od, p.old, p.size, od[p.old+23]&0x1f, "")
		if !odi.unc || odi.mcount == 0 {
			continue
		}
		withMeth++
		ni := newHolding(p.pred)
		if ni < 0 {
			noCounter++
			continue
		}
		ndi := newInfo[ni]
		if int(p.old)+44 > len(od) {
			continue
		}
		if oname := readName(c.ob, u32(od, int(p.old)+40)); oname == "" || oname != ndi.name {
			nameMismatch++
			continue
		}
		if !ndi.unc || ndi.mcount == 0 {
			noNewMeth++
			continue
		}
		paired++

		om := make([]string, 0, odi.mcount)
		for i := 0; i < odi.mcount; i++ {
			om = append(om, readName(c.ob, u32(od, int(odi.marr)+16*i)))
		}
		nm := make([]string, 0, ndi.mcount)
		for j := 0; j < ndi.mcount; j++ {
			nm = append(nm, readName(c.nb, u32(nd, int(ndi.marr)+16*j)))
		}
		ps := alignMethods(om, nm)
		segs := segsOf(ps)
		trivial := len(om) == len(nm) && len(ps) == len(om) && len(segs) == 1 &&
			segs[0].oldIdx == 0 && segs[0].newIdx == 0
		ins, del := len(nm)-len(ps), len(om)-len(ps)

		// Every aligned entry, whether or not this descriptor was edited:
		// the secondary oracle also covers the retargeting errors.
		for _, pr := range ps {
			q := lo + int(ndi.marr) + 16*pr.j
			if q < lo || q+16 > hi {
				continue
			}
			for b := 0; b < 16; b++ {
				if c.pred[q+b] != c.new[q+b] {
					allOraclePos = append(allOraclePos, q+b)
				}
			}
		}
		if trivial {
			noEdit++
			w := 0
			for j := 0; j < ndi.mcount; j++ {
				q := lo + int(ndi.marr) + 16*j
				if q+16 > hi {
					break
				}
				// The mechanism check: where the alignment is the identity
				// the walker's own write site must already be the new
				// entry's position, so reading pred back there is reading
				// what an aligned copy would have written.
				if s, ok := mSite[int(odi.marr)+16*j]; ok {
					if s == q {
						siteAtPlace++
					} else {
						siteTorn++
					}
				}
				for b := 0; b < 16; b++ {
					if c.pred[q+b] != c.new[q+b] {
						w++
					}
				}
			}
			noEditWrongB += w
			if w > 0 {
				noEditWrongDesc++
			}
			continue
		}
		switch {
		case ins > 0 && del == 0:
			insOnly++
		case del > 0 && ins == 0:
			delOnly++
		default:
			mixedEdit++
		}
		histAdd(insH, ins)
		histAdd(delH, del)
		insertedEnt += ins
		deletedEnt += del
		plans = append(plans, mplan{id: k, mdiff: ndi.mcount - odi.mcount, segs: segs})

		aligned := make([]bool, ndi.mcount)
		for _, pr := range ps {
			q := lo + int(ndi.marr) + 16*pr.j
			if q < lo || q+16 > hi {
				continue
			}
			aligned[pr.j] = true
			matchedEnt++
			if pr.i != pr.j {
				shiftedEnt++
			}
			for b := 0; b < 16; b++ {
				if c.pred[q+b] != c.new[q+b] {
					oraclePos = append(oraclePos, q+b)
				}
			}
			s, ok := mSite[int(odi.marr)+16*pr.i]
			if !ok || s+16 > len(c.pred) {
				noSite++
				continue
			}
			ok4 := 0
			for f := 0; f < 16; f += 4 {
				if u32(c.pred, s+f) == u32(c.new, q+f) {
					fieldOK[f/4]++
					ok4++
					for b := f; b < f+4; b++ {
						if c.pred[q+b] != c.new[q+b] {
							realPos = append(realPos, q+b)
						}
					}
				} else {
					fieldBad[f/4]++
				}
			}
			if ok4 == 4 {
				entryAllOK++
			}
		}
		for j, a := range aligned {
			if a {
				continue
			}
			q := lo + int(ndi.marr) + 16*j
			if q+16 > hi {
				continue
			}
			for b := 0; b < 16; b++ {
				if c.pred[q+b] != c.new[q+b] {
					insertedPos = append(insertedPos, q+b)
				}
			}
		}
	}

	fmt.Fprintf(os.Stderr, "\n-- 12a. descriptors with an uncommon-type method table\n")
	fmt.Fprintf(os.Stderr, "  %d placed old descriptors carry one; %d have no new descriptor at the placed offset, %d name a different type there, %d have no method table on the new side\n",
		withMeth, noCounter, nameMismatch, noNewMeth)
	fmt.Fprintf(os.Stderr, "  %d paired: %d need no edit (the name lists are identical), %d have entries inserted only, %d deleted only, %d both\n",
		paired, noEdit, insOnly, delOnly, mixedEdit)
	fmt.Fprintf(os.Stderr, "  edited descriptors hold %d aligned entries (%d of them at a shifted index), %d inserted and %d deleted\n",
		matchedEnt, shiftedEnt, insertedEnt, deletedEnt)
	if noSite > 0 {
		fmt.Fprintf(os.Stderr, "  %d aligned entries had no walker site to read a retargeted value from\n", noSite)
	}
	fmt.Fprintf(os.Stderr, "  mechanism check, un-edited descriptors: %d of %d entries have the walker's write site exactly at the new entry's position (%d torn), so pred[place(old entry)] is what an aligned copy would write\n",
		siteAtPlace, siteAtPlace+siteTorn, siteTorn)

	fmt.Fprintf(os.Stderr, "\n-- 12b. edit sizes, and the un-edited descriptors\n")
	fmt.Fprintf(os.Stderr, "  inserted entries per edited descriptor: %s\n", histLine(insH, 12))
	fmt.Fprintf(os.Stderr, "  deleted  entries per edited descriptor: %s\n", histLine(delH, 12))
	fmt.Fprintf(os.Stderr, "  un-edited descriptors: %d of %d hold a wrong byte in their method array, %d B in all -- the same-index case is %s\n",
		noEditWrongDesc, noEdit, noEditWrongB, map[bool]string{true: "already right", false: "NOT already right"}[noEditWrongB == 0])

	fmt.Fprintf(os.Stderr, "\n-- 12c. on an aligned entry, does the walker's retargeted old value equal the new value?\n")
	fmt.Fprintf(os.Stderr, "  %-24s %10s %10s %8s\n", "field", "correct", "wrong", "share")
	for i, n := range []string{"Name (nameOff)", "Mtyp (typeOff)", "Ifn (textOff)", "Tfn (textOff)"} {
		fmt.Fprintf(os.Stderr, "  %-24s %10d %10d %8s\n", n, fieldOK[i], fieldBad[i], pct(fieldOK[i], max(1, fieldOK[i]+fieldBad[i])))
	}
	fmt.Fprintf(os.Stderr, "  all four fields right: %d of %d aligned entries (%s)\n",
		entryAllOK, matchedEnt, pct(entryAllOK, max(1, matchedEnt)))

	xz, cz, rawList, cols := mplanCost(plans)
	m := c.marginals([][]int{oraclePos, realPos, allOraclePos, insertedPos})
	printRows("12d. priced (marginal = compressed correction lost when the class is made correct)", []row{
		{"(a) oracle: every aligned entry of an edited descriptor", 0, len(oraclePos), 0, m[0].comp, m[0].raw},
		{"(b) realistic: only the fields the maps retarget right", 0, len(realPos), 0, m[1].comp, m[1].raw},
		{"    aligned entries of every paired descriptor (oracle)", 0, len(allOraclePos), 0, m[2].comp, m[2].raw},
		{"    inserted entries, left to the correction", 0, len(insertedPos), 0, m[3].comp, m[3].raw},
	}, false)
	fmt.Fprintf(os.Stderr, "  alignment list for %d descriptors: %d B raw, %d B xz, %d B cz; columns xz -- ids %d, mcount delta %d, seg count %d, gaps %d, lengths %d, shifts %d\n",
		len(plans), rawList, xz, cz, cols[0], cols[1], cols[2], cols[3], cols[4], cols[5])
	fmt.Fprintf(os.Stderr, "  net: oracle %d, realistic %d (xz list); oracle %d, realistic %d (cz list)\n",
		m[0].comp-xz, m[1].comp-xz, m[0].comp-cz, m[1].comp-cz)

	c.runPrice("12e. (a) oracle set, by correction run", lo, hi, oraclePos)
	c.runPrice("12f. (b) realistic set, by correction run", lo, hi, realPos)
}
