package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// row is one class of the residual, priced. marginal is the compressed
// patch bytes the class costs; spike is what the Chrome ELF yardstick
// (chrome-elf-handoff.md: 1.061 per run + 0.624 per wrong byte) would have
// predicted for it, printed as a sanity check on the measurement.
type row struct {
	name     string
	region   int
	wrong    int
	runs     int
	marginal int
	raw      int
}

func (r row) spike() float64 { return 1.061*float64(r.runs) + 0.624*float64(r.wrong) }

// fitA, fitB are the Go codec's own yardstick, fitted on the section rows of
// level 1 and then used to price every later class. They are this codec's
// answer to chrome-elf-handoff.md's "1.061 per run + 0.624 per wrong byte".
var fitA, fitB float64

func (r row) fit() float64 { return fitA*float64(r.runs) + fitB*float64(r.wrong) }

func printRows(title string, rows []row, showRegion bool) {
	fmt.Fprintf(os.Stderr, "\n-- %s\n", title)
	head := "  %-46s %11s %9s %8s %9s %9s %9s %9s\n"
	body := "  %-46s %11d %9d %8d %8.2f%% %9d %9d %9.0f\n"
	fmt.Fprintf(os.Stderr, head, "class", "region B", "wrong B", "runs", "density", "marginal", "raw", "fit")
	var tot row
	for _, r := range rows {
		d := 0.0
		if r.region > 0 {
			d = 100 * float64(r.wrong) / float64(r.region)
		}
		fmt.Fprintf(os.Stderr, body, r.name, r.region, r.wrong, r.runs, d, r.marginal, r.raw, r.fit())
		tot.region += r.region
		tot.wrong += r.wrong
		tot.runs += r.runs
		tot.marginal += r.marginal
		tot.raw += r.raw
	}
	fmt.Fprintf(os.Stderr, body, "TOTAL", tot.region, tot.wrong, tot.runs, 0.0, tot.marginal, tot.raw, tot.fit())
	fmt.Fprintf(os.Stderr, "  (spike yardstick for the total: %.0f; measured %d, ratio %.2f)\n",
		tot.spike(), tot.marginal, float64(tot.marginal)/max(1, tot.spike()))
}

// ---------------------------------------------------------------- level 1

type sect struct {
	name     string
	off, end int
}

// sections is the new file's byte map: every allocated section that occupies
// file bytes, plus one bucket for everything else -- the ELF and program
// headers, the inter-section padding and the tail.
func (c *ctx) sections() []sect {
	var out []sect
	for _, s := range c.nb.Order {
		if s.NoBits || s.Size == 0 || int(s.Off+s.Size) > len(c.new) {
			continue
		}
		out = append(out, sect{s.Name, int(s.Off), int(s.Off + s.Size)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].off < out[j].off })
	return out
}

func (c *ctx) sectionOf(secs []sect, off int) int {
	i := sort.Search(len(secs), func(i int) bool { return secs[i].end > off })
	if i < len(secs) && off >= secs[i].off {
		return i
	}
	return -1
}

func (c *ctx) bySection() []row {
	secs := c.sections()
	pos := make([][]int, len(secs)+1)
	runs := make([]int, len(secs)+1)
	for i := range c.new {
		if c.pred[i] == c.new[i] {
			continue
		}
		k := c.sectionOf(secs, i)
		if k < 0 {
			k = len(secs)
		}
		pos[k] = append(pos[k], i)
	}
	for _, r := range c.regs {
		k := c.sectionOf(secs, r.s)
		if k < 0 {
			k = len(secs)
		}
		runs[k]++
	}
	var idx []int
	for k := range pos {
		if len(pos[k]) > 0 {
			idx = append(idx, k)
		}
	}
	sets := make([][]int, len(idx))
	for i, k := range idx {
		sets[i] = pos[k]
	}
	marg := c.marginals(sets)
	rows := make([]row, 0, len(idx))
	for i, k := range idx {
		name, region := "(headers, padding, tail)", 0
		if k < len(secs) {
			name, region = secs[k].name, secs[k].end-secs[k].off
		}
		rows = append(rows, row{name, region, len(pos[k]), runs[k], marg[i].comp, marg[i].raw})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].marginal > rows[j].marginal })
	printRows("1. by section (marginal cost = compressed correction lost when the section is reverted to the prediction)", rows, true)
	return rows
}

// ---------------------------------------------------------------- yardstick

// yardstick fits marginal = a*runs + b*wrong over the section rows. It is
// the Go codec's own version of chrome-elf-handoff.md's "1.061 per run plus
// 0.624 per wrong byte", and it is what any proposed change here has to be
// priced against before it is built.
func yardstick(rows []row) {
	var sxx, sxy, syy, sxz, syz float64
	for _, r := range rows {
		x, y, z := float64(r.runs), float64(r.wrong), float64(r.marginal)
		sxx += x * x
		sxy += x * y
		syy += y * y
		sxz += x * z
		syz += y * z
	}
	det := sxx*syy - sxy*sxy
	if det == 0 {
		return
	}
	a := (sxz*syy - syz*sxy) / det
	b := (syz*sxx - sxz*sxy) / det
	fitA, fitB = a, b
	var ss, tot float64
	for _, r := range rows {
		p := a*float64(r.runs) + b*float64(r.wrong)
		ss += (p - float64(r.marginal)) * (p - float64(r.marginal))
		tot += float64(r.marginal) * float64(r.marginal)
	}
	fmt.Fprintf(os.Stderr, "\n-- yardstick, regressed on the %d section rows\n", len(rows))
	fmt.Fprintf(os.Stderr, "  marginal = %.3f per run + %.3f per wrong byte (residual RMS %.0f over rows)\n",
		a, b, sqrt(ss/float64(len(rows))))
	fmt.Fprintf(os.Stderr, "  spike (Chrome .text) was 1.061 per run + 0.624 per wrong byte\n")
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	g := x
	for range 40 {
		g = 0.5 * (g + x/g)
	}
	return g
}

func pct(a, b int) string {
	if b == 0 {
		return "  -  "
	}
	return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(b))
}

func commas(n int) string {
	s := fmt.Sprint(n)
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, out...), ",")
}
