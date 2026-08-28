package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/wjordan/go-binsync/delta/gobin"
	"github.com/wjordan/go-binsync/delta/x86"
)

// The name normalisation delta/match.go pairs on. It is copied rather than
// exported because the probe also needs a *wider* form of it (renumberKey
// below), and the two want to be read side by side.
var (
	reFuncN   = regexp.MustCompile(`\.func\d+`)
	reDeferN  = regexp.MustCompile(`\.deferwrap\d+`)
	reGowrapN = regexp.MustCompile(`\.gowrap\d+`)
)

func normName(s string) string {
	s = reFuncN.ReplaceAllString(s, ".func#")
	s = reDeferN.ReplaceAllString(s, ".deferwrap#")
	return reGowrapN.ReplaceAllString(s, ".gowrap#")
}

// renumberKey is normName plus generic instantiation arguments, which the
// codec does *not* collapse. A new function whose only difference from an
// old one is which types it was instantiated with has the same key.
func renumberKey(s string) string {
	k := normName(s)
	if !strings.ContainsRune(k, '[') {
		return k
	}
	var b strings.Builder
	depth := 0
	for i := 0; i < len(k); i++ {
		switch k[i] {
		case '[':
			if depth == 0 {
				b.WriteString("[#]")
			}
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteByte(k[i])
			}
		}
	}
	return b.String()
}

// textOff turns an address in the new .text into a file offset.
func (c *ctx) textOff(addr uint64) int {
	return int(addr-c.nb.Text.Addr) + int(c.nb.Text.Off)
}

// funcRange is one new function's span in file offsets.
func (c *ctx) funcRange(g *gobin.Func) (int, int) {
	return c.textOff(g.Entry), c.textOff(g.End)
}

// ---------------------------------------------------------------- level 2

const (
	cvUnmatched = iota
	cvNameSame
	cvNameResized
	cvContent
	cvOutside
	nCause
)

var causeNames = [nCause]string{
	cvUnmatched:   "unmatched-new function",
	cvNameSame:    "name-matched, same length",
	cvNameResized: "name-matched, resized",
	cvContent:     "content-matched (renamed)",
	cvOutside:     "outside any function",
}

// causeOf classifies a new function by how the codec found its old body,
// which is the same ladder chrome-elf-whole-image.md 11.2 walks.
func (c *ctx) causeOf(j int) int {
	i := c.pr.NewToOld[j]
	if i < 0 {
		return cvUnmatched
	}
	f, g := c.ob.Funcs[i], c.nb.Funcs[j]
	if f.Name != g.Name && normName(f.Name) != normName(g.Name) {
		return cvContent
	}
	if f.Size() == g.Size() {
		return cvNameSame
	}
	return cvNameResized
}

func (c *ctx) byCause() {
	tlo, thi := int(c.nb.Text.Off), int(c.nb.Text.Off+c.nb.Text.Size)
	cause := make([]int8, thi-tlo)
	for i := range cause {
		cause[i] = cvOutside
	}
	counts := [nCause]int{}
	region := [nCause]int{}
	for j, g := range c.nb.Funcs {
		k := c.causeOf(j)
		counts[k]++
		lo, hi := c.funcRange(g)
		if lo < tlo || hi > thi {
			continue
		}
		region[k] += hi - lo
		for p := lo; p < hi; p++ {
			cause[p-tlo] = int8(k)
		}
	}
	region[cvOutside] = (thi - tlo) - (region[cvUnmatched] + region[cvNameSame] + region[cvNameResized] + region[cvContent])
	pos := make([][]int, nCause)
	runs := [nCause]int{}
	for p := tlo; p < thi; p++ {
		if c.pred[p] != c.new[p] {
			pos[cause[p-tlo]] = append(pos[cause[p-tlo]], p)
		}
	}
	for _, r := range c.regs {
		if r.s >= tlo && r.s < thi {
			runs[cause[r.s-tlo]]++
		}
	}
	marg := c.marginals(pos)
	var rows []row
	for k := range nCause {
		rows = append(rows, row{fmt.Sprintf("%s (%d funcs)", causeNames[k], counts[k]),
			region[k], len(pos[k]), runs[k], marg[k].comp, marg[k].raw})
	}
	printRows("2. .text by cause", rows, true)
}

// ---------------------------------------------------------------- level 3

// field is one PC-relative operand the predictor rewrote, found by walking
// the *prediction* -- which is what the decoder could do too.
type field struct {
	off      int // file offset of the displacement field
	n        int
	oldT     uint64 // the old instruction's target
	predT    uint64 // the target the address maps produced
	actualT  uint64 // the target the real new file holds there
	wrong    int    // wrong bytes inside the field
	haveOld  bool
	haveReal bool
}

type symKey struct {
	kind byte
	v    uint64
}

// fields walks every matched function's prediction and its old body, pairing
// the PC-relative operands by offset. The prediction is the old bytes with
// only the displacement fields rewritten, so the two decode identically.
func (c *ctx) fields() []field {
	var out []field
	for j, g := range c.nb.Funcs {
		i := c.pr.NewToOld[j]
		if i < 0 {
			continue
		}
		f := c.ob.Funcs[i]
		oldCode := c.ob.FuncBytes(f)
		lo, hi := c.funcRange(g)
		if lo < 0 || hi > len(c.new) {
			continue
		}
		n := min(len(oldCode), hi-lo)
		if n <= 0 {
			continue
		}
		predCode := c.pred[lo : lo+n]
		oldT := map[int]uint64{}
		x86.WalkReferences(oldCode[:n], f.Entry, func(r x86.Reference) { oldT[r.Off] = r.Target })
		x86.WalkReferences(predCode, g.Entry, func(r x86.Reference) {
			fl := field{off: lo + r.Off, n: r.N, predT: r.Target}
			fl.oldT, fl.haveOld = oldT[r.Off]
			for k := fl.off; k < fl.off+fl.n; k++ {
				if c.pred[k] != c.new[k] {
					fl.wrong++
				}
			}
			if fl.off+fl.n <= len(c.new) {
				d := int64(0)
				switch r.N {
				case 1:
					d = int64(int8(c.new[fl.off]))
				case 2:
					d = int64(int16(binary.LittleEndian.Uint16(c.new[fl.off:])))
				case 4:
					d = int64(int32(binary.LittleEndian.Uint32(c.new[fl.off:])))
				}
				fl.actualT = uint64(int64(g.Entry) + int64(r.Next) + d)
				fl.haveReal = true
			}
			out = append(out, fl)
		})
	}
	return out
}

// symOf pools a target with everything else that names the same symbol: an
// old function for a .text target, the address itself otherwise. A per-symbol
// consensus shift is what a field-fix layer or a better content map would
// have available; if the real new target equals it, the miss was a map
// error, and if it does not, the instruction genuinely points somewhere else.
func (c *ctx) symOf(t uint64) symKey {
	if t >= c.ob.Text.Addr && t < c.ob.Text.Addr+c.ob.Text.Size {
		if f := c.ob.FuncAt(t); f != nil {
			return symKey{'f', uint64(f.Idx)}
		}
	}
	return symKey{'a', t}
}

var fieldMaskCache []bool

// fieldMask marks the .text bytes that sit inside a relocated field.
func (c *ctx) fieldMask(fs []field) []bool {
	if fieldMaskCache != nil {
		return fieldMaskCache
	}
	m := make([]bool, len(c.new))
	for _, f := range fs {
		for k := f.off; k < f.off+f.n && k < len(m); k++ {
			m[k] = true
		}
	}
	fieldMaskCache = m
	return m
}

func (c *ctx) byField() {
	fs := c.fields()
	tlo, thi := int(c.nb.Text.Off), int(c.nb.Text.Off+c.nb.Text.Size)
	mask := c.fieldMask(fs)

	// per-symbol consensus shift, voted by every field naming that symbol
	votes := map[symKey]map[int64]int{}
	for _, f := range fs {
		if !f.haveOld || !f.haveReal {
			continue
		}
		k := c.symOf(f.oldT)
		if votes[k] == nil {
			votes[k] = map[int64]int{}
		}
		votes[k][int64(f.actualT)-int64(f.oldT)]++
	}
	best := map[symKey]struct {
		shift        int64
		count, total int
	}{}
	for k, v := range votes {
		var sh int64
		bn, tot := 0, 0
		for d, n := range v {
			tot += n
			if n > bn || n == bn && d < sh {
				sh, bn = d, n
			}
		}
		best[k] = struct {
			shift        int64
			count, total int
		}{sh, bn, tot}
	}

	var inField, mapErr, diffTarget, single, noInfo []int
	nFields, nWrongFields := len(fs), 0
	nMapErr, nDiff, nSingle := 0, 0, 0
	for _, f := range fs {
		if f.wrong == 0 {
			continue
		}
		nWrongFields++
		var bucket *[]int
		switch {
		case !f.haveOld || !f.haveReal:
			bucket = &noInfo
		default:
			b := best[c.symOf(f.oldT)]
			switch {
			case b.total < 2:
				nSingle++
				bucket = &single
			case int64(f.actualT) == int64(f.oldT)+b.shift:
				nMapErr++
				bucket = &mapErr
			default:
				nDiff++
				bucket = &diffTarget
			}
		}
		for k := f.off; k < f.off+f.n; k++ {
			if c.pred[k] != c.new[k] {
				*bucket = append(*bucket, k)
				inField = append(inField, k)
			}
		}
	}
	var outside []int
	for p := tlo; p < thi; p++ {
		if c.pred[p] != c.new[p] && !mask[p] {
			outside = append(outside, p)
		}
	}
	runIn, runOut := 0, 0
	for _, r := range c.regs {
		if r.s < tlo || r.s >= thi {
			continue
		}
		if mask[r.s] {
			runIn++
		} else {
			runOut++
		}
	}
	// A byte-level revert is only a clean price where the class owns whole
	// correction regions. Where it does not -- a four-byte field inside a
	// long wrong region -- reverting it leaves the region in place and hands
	// the region's lz coder a short match to encode, which moves bytes out of
	// the literal stream and into the varint control stream. That is why the
	// mixed-run row below is *negative*: the price of a field-fix layer is
	// the field-only-run row, not the whole-field row.
	var fieldOnlyRuns, mixedIn, mixedAll, noFieldRuns []int
	nFieldOnly, nMixed, nNoField := 0, 0, 0
	for _, r := range c.regs {
		if r.s < tlo || r.s >= thi {
			continue
		}
		in, out := 0, 0
		for p := r.s; p < r.e; p++ {
			if c.pred[p] == c.new[p] {
				continue
			}
			if mask[p] {
				in++
			} else {
				out++
			}
		}
		switch {
		case in > 0 && out == 0:
			nFieldOnly++
		case in > 0:
			nMixed++
		default:
			nNoField++
		}
		for p := r.s; p < r.e; p++ {
			if c.pred[p] == c.new[p] {
				continue
			}
			switch {
			case in > 0 && out == 0:
				fieldOnlyRuns = append(fieldOnlyRuns, p)
			case in > 0:
				mixedAll = append(mixedAll, p)
				if mask[p] {
					mixedIn = append(mixedIn, p)
				}
			default:
				noFieldRuns = append(noFieldRuns, p)
			}
		}
	}

	sets := [][]int{inField, outside, mapErr, diffTarget, single, noInfo,
		fieldOnlyRuns, mixedIn, mixedAll, noFieldRuns}
	marg := c.marginals(sets)
	fmt.Fprintf(os.Stderr, "\n-- 3. .text: inside a relocated field or not\n")
	fmt.Fprintf(os.Stderr, "  %d relocated fields in matched functions; %d hold a wrong byte (%s)\n",
		nFields, nWrongFields, pct(nWrongFields, nFields))
	rows := []row{
		{"wrong bytes inside a relocated field", 0, len(inField), runIn, marg[0].comp, marg[0].raw},
		{"wrong bytes elsewhere in .text", 0, len(outside), runOut, marg[1].comp, marg[1].raw},
	}
	printRows("3a. field vs not", rows, false)
	rows = []row{
		{fmt.Sprintf("map error: consensus shift fixes it (%d fields)", nMapErr), 0, len(mapErr), 0, marg[2].comp, marg[2].raw},
		{fmt.Sprintf("genuinely different target (%d fields)", nDiff), 0, len(diffTarget), 0, marg[3].comp, marg[3].raw},
		{fmt.Sprintf("single-site symbol, undecidable (%d fields)", nSingle), 0, len(single), 0, marg[4].comp, marg[4].raw},
		{"field the old body has no counterpart for", 0, len(noInfo), 0, marg[5].comp, marg[5].raw},
	}
	printRows("3b. wrong fields, by whether the address maps could have got it right", rows, false)
	rows = []row{
		{fmt.Sprintf("runs whose every wrong byte is in a field (%d)", nFieldOnly), 0, len(fieldOnlyRuns), nFieldOnly, marg[6].comp, marg[6].raw},
		{fmt.Sprintf("mixed runs (%d), the in-field bytes only", nMixed), 0, len(mixedIn), 0, marg[7].comp, marg[7].raw},
		{fmt.Sprintf("mixed runs (%d), all their wrong bytes", nMixed), 0, len(mixedAll), nMixed, marg[8].comp, marg[8].raw},
		{fmt.Sprintf("runs with no field byte at all (%d)", nNoField), 0, len(noFieldRuns), nNoField, marg[9].comp, marg[9].raw},
	}
	printRows("3c. by run, which is the only clean price for a field-fix layer", rows, false)
}

// ---------------------------------------------------------------- level 4

func (c *ctx) byInstruction() {
	fs := c.fields()
	mask := c.fieldMask(fs)
	var inst [nBucket]opCount
	var onB, offB opCount
	var byClass [nOpClass]opCount
	classPos := make([][]int, nOpClass)
	tlo, thi := int(c.nb.Text.Off), int(c.nb.Text.Off+c.nb.Text.Size)
	classAt := make([]int8, thi-tlo)
	for i := range classAt {
		classAt[i] = -1
	}

	for j, g := range c.nb.Funcs {
		if c.pr.NewToOld[j] < 0 {
			continue
		}
		lo, hi := c.funcRange(g)
		if lo < 0 || hi > len(c.new) {
			continue
		}
		pred, want := c.pred[lo:hi], c.new[lo:hi]
		dirty := false
		for i := range want {
			if pred[i] != want[i] && !mask[lo+i] {
				dirty = true
				break
			}
		}
		if !dirty {
			continue
		}
		bound := make([]bool, len(pred)+1)
		for k := 0; k < len(pred); {
			bound[k] = true
			pi, ok := safeDecode(pred[k:])
			if !ok {
				k++
				continue
			}
			k += pi.Len
		}
		for s := 0; s < len(want); {
			ti, ok := safeDecode(want[s:])
			if !ok {
				s++
				continue
			}
			end := min(s+ti.Len, len(want))
			w := 0
			for k := s; k < end; k++ {
				if pred[k] != want[k] && !mask[lo+k] {
					w++
				}
			}
			if w == 0 {
				s = end
				continue
			}
			pi, pok := safeDecode(pred[s:])
			b := bkUndecodable
			switch {
			case !pok:
			case pi.Op == ti.Op && pi.Len == ti.Len:
				b = bkSameOpSameLen
			case pi.Op == ti.Op:
				b = bkSameOpDiffLen
			case pi.Len == ti.Len:
				b = bkDiffOpSameLen
			default:
				b = bkDiffOpDiffLen
				if bound[s] {
					onB.insts++
					onB.wrong += w
				} else {
					offB.insts++
					offB.wrong += w
				}
			}
			inst[b].insts++
			inst[b].wrong += w
			cl := clsUndecodable
			if pok {
				cl = compareOperands(pi, ti).class()
			}
			byClass[cl].insts++
			byClass[cl].wrong += w
			for k := s; k < end; k++ {
				if pred[k] != want[k] && !mask[lo+k] {
					classPos[cl] = append(classPos[cl], lo+k)
					if lo+k >= tlo && lo+k < thi {
						classAt[lo+k-tlo] = int8(cl)
					}
				}
			}
			s = end
		}
	}
	fmt.Fprintf(os.Stderr, "\n-- 4. .text by instruction, wrong bytes outside relocated fields\n")
	fmt.Fprintf(os.Stderr, "  %-40s %10s %12s %8s\n", "relation to the target instruction", "insts", "wrong B", "share")
	tot := 0
	for _, b := range inst {
		tot += b.wrong
	}
	for b, name := range bucketNames {
		fmt.Fprintf(os.Stderr, "  %-40s %10d %12d %8s\n", name, inst[b].insts, inst[b].wrong, pct(inst[b].wrong, tot))
	}
	fmt.Fprintf(os.Stderr, "  %-40s %10d %12d %8s\n", "  of which pred is on an insn boundary", onB.insts, onB.wrong, pct(onB.wrong, tot))
	fmt.Fprintf(os.Stderr, "  %-40s %10d %12d %8s\n", "  of which pred is mid-instruction", offB.insts, offB.wrong, pct(offB.wrong, tot))

	// Correction runs, attributed to the class of the first wrong
	// non-field byte in the run, so the fitted price applies.
	classRuns := make([]int, nOpClass)
	for _, r := range c.regs {
		if r.s < tlo || r.s >= thi {
			continue
		}
		for p := r.s; p < r.e && p < thi; p++ {
			if cl := classAt[p-tlo]; cl >= 0 {
				classRuns[cl]++
				break
			}
		}
	}

	order := make([]int, 0, nOpClass)
	for cl := range nOpClass {
		if byClass[cl].insts > 0 {
			order = append(order, cl)
		}
	}
	sort.Slice(order, func(i, j int) bool { return byClass[order[i]].wrong > byClass[order[j]].wrong })
	sets := make([][]int, len(order))
	for i, cl := range order {
		sets[i] = classPos[cl]
	}
	marg := c.marginals(sets)
	var rows []row
	for i, cl := range order {
		rows = append(rows, row{fmt.Sprintf("%s [%d insns]", opClassNames[cl], byClass[cl].insts),
			0, byClass[cl].wrong, classRuns[cl], marg[i].comp, marg[i].raw})
	}
	printRows("4b. by operand class (runs = correction runs this class starts; marginal is unreliable here, see 3c)", rows, false)
}
