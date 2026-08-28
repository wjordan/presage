package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// xzSizeContiguous compresses as a single block. -T1 is mandatory for any
// dictionary or marginal-cost probe: multithreaded xz splits its input into
// independently coded blocks and hides exactly the cross-boundary matches
// the probe asks about (chrome-elf-handoff.md, Traps).
func xzSizeContiguous(b []byte) int {
	cmd := exec.Command("xz", "-9e", "-T1", "-c")
	cmd.Stdin = bytes.NewReader(b)
	var out bytes.Buffer
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return 0
	}
	return out.Len()
}

func xzSizes(bs ...[]byte) []int {
	out := make([]int, len(bs))
	var wg sync.WaitGroup
	for i, b := range bs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = xzSizeContiguous(b)
		}()
	}
	wg.Wait()
	return out
}

// ---------------------------------------------------------------- level 6
//
// Is the code the prediction could not supply already in the old image under
// another name? Asked raw the answer is meaningless -- machine code does not
// self-match raw, because two copies of the same function diverge at every
// call site -- so both sides are canonicalised first, which is the form
// chrome-elf-whole-image.md 11.5 had to switch to before its answer meant
// anything.
func (c *ctx) dictionary(window int) {
	var body []byte
	nUn, unBytes := 0, 0
	for j, g := range c.nb.Funcs {
		if c.pr.NewToOld[j] >= 0 {
			continue
		}
		lo, hi := c.funcRange(g)
		if lo < 0 || hi > len(c.new) {
			continue
		}
		nUn++
		unBytes += hi - lo
		body = append(body, canonicalise(c.new[lo:hi])...)
	}
	if nUn == 0 {
		fmt.Fprintf(os.Stderr, "\n-- 6. no unmatched-new functions\n")
		return
	}
	dict := c.ob.Text.Data
	if window > 0 && len(dict) > window {
		dict = dict[len(dict)-window:]
	}
	dict = canonicalise(dict)
	// A control: an equal-length slice of the dictionary's own bytes. Its
	// marginal cost is what this probe reads when the answer is "yes, it is
	// already there", and it is what makes the real number interpretable.
	ctrl := dict[len(dict)/3 : min(len(dict)/3+len(body), len(dict))]
	with := append(append([]byte(nil), dict...), body...)
	withCtrl := append(append([]byte(nil), dict...), ctrl...)
	s := xzSizes(body, dict, with, withCtrl)
	alone, base := s[0], s[1]
	fmt.Fprintf(os.Stderr, "\n-- 6. is the new code elsewhere in the old image?\n")
	fmt.Fprintf(os.Stderr, "  %d unmatched-new functions, %d B; canonicalised %d B\n", nUn, unBytes, len(body))
	fmt.Fprintf(os.Stderr, "  dictionary: %d B of canonicalised old .text\n", len(dict))
	fmt.Fprintf(os.Stderr, "  xz -T1 alone %d; marginal after the dictionary %d (%.2fx cheaper)\n",
		alone, s[2]-base, float64(alone)/float64(max(1, s[2]-base)))
	fmt.Fprintf(os.Stderr, "  control (an equal slice of the dictionary itself): alone n/a, marginal %d -- the probe can see redundancy at %.1fx\n",
		s[3]-base, float64(alone)/float64(max(1, s[3]-base)))

	// Renumbered names: a new function whose name is an old one with a
	// different closure number or a different generic instantiation, and
	// whose body is nearly the same. Every one of these is a pairing the
	// matcher left on the table.
	byKey := map[string][]int{}
	for i, f := range c.ob.Funcs {
		byKey[renumberKey(f.Name)] = append(byKey[renumberKey(f.Name)], i)
	}
	near, nearBytes, exactName := 0, 0, 0
	for j, g := range c.nb.Funcs {
		if c.pr.NewToOld[j] >= 0 {
			continue
		}
		cands := byKey[renumberKey(g.Name)]
		if len(cands) == 0 {
			continue
		}
		exactName++
		lo, hi := c.funcRange(g)
		nb := canonicalise(c.new[lo:hi])
		bestDiff := 1.0
		for _, i := range cands {
			ob := canonicalise(c.ob.FuncBytes(c.ob.Funcs[i]))
			d := diffRatio(ob, nb)
			if d < bestDiff {
				bestDiff = d
			}
		}
		if bestDiff < 0.10 {
			near++
			nearBytes += hi - lo
		}
	}
	fmt.Fprintf(os.Stderr, "  renumbered names: %d of the %d unmatched-new functions share a renumbered/reinstantiated name with an old one;\n", exactName, nUn)
	fmt.Fprintf(os.Stderr, "    %d of those have a body <10%% different, holding %d B\n", near, nearBytes)
}

// diffRatio is a cheap similarity: differing bytes over the common prefix
// plus the length difference, against the longer body.
func diffRatio(a, b []byte) float64 {
	n := min(len(a), len(b))
	d := abs(len(a) - len(b))
	for i := range n {
		if a[i] != b[i] {
			d++
		}
	}
	return float64(d) / float64(max(1, max(len(a), len(b))))
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
