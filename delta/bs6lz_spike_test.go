package delta

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wjordan/go-binsync/delta/internal/lz"
)

// TestBS6LZSpike prices the bsdiff-6 ideas that touch the *matcher* rather
// than the difference string: bsdiff 4.3's greedy scan with a fixed cost
// model, versus a scan whose margins are parameters, versus separating the
// control triples and the difference runs into their own streams.
//
// plain.go is bsdiff's shape with two substitutions -- a position-ordered
// hash index instead of a suffix array, and difference runs instead of a
// dense difference string. Both substitutions are cost-model choices that
// were never swept, and the thesis's own encoder (2.6) replaces the greedy
// scan with a shortest path over candidate alignments.
//
//	BS6_OLD=... BS6_NEW=... go test ./delta -run BS6LZSpike -v

type lzShape struct {
	probe   int  // index candidates per side of the hint
	cutoff  int  // "a fresh match wins by this much" margin (bsdiff: 8)
	extNum  int  // extension score numerator (bsdiff: 2 matched bytes per byte)
	extDen  int  // extension score denominator (bsdiff: 1)
	merge   int  // zero bytes a difference run absorbs
	cols    bool // lenf, nextra and seek as three streams
	splitDR bool // difference runs as positions and values, not interleaved
	// splitG and splitMin are the thesis's "unmatched" edge (2.6), which
	// bsdiff 4.3's greedy scan has no equivalent of: inside a matched run,
	// a stretch that mismatches densely is cheaper as literal bytes in the
	// extra stream -- which compresses like code -- than as difference bytes,
	// which are differences of unrelated bytes and compress like noise. A
	// stretch is excised when +splitG per mismatched byte less one per
	// matched byte exceeds splitMin, which is what a second control triple
	// plus the swallowed matched bytes cost.
	splitG   int
	splitMin int
	// collect, when set, is handed every matched run instead of the
	// difference writer, so a caller can price other representations.
	collect func(new, old []byte)
}

// excise finds the stretches of a matched run that are cheaper as literals.
// Kadane, restarted after each accepted stretch, so the intervals are
// disjoint and in order.
func (sh lzShape) excise(new, old []byte) [][2]int {
	if sh.splitG == 0 {
		return nil
	}
	var out [][2]int
	sum, start, best, bs, be := 0, 0, 0, -1, -1
	flush := func() {
		if bs >= 0 && best >= sh.splitMin {
			out = append(out, [2]int{bs, be})
		}
		best, bs, be = 0, -1, -1
	}
	for i := 0; i <= len(new); i++ {
		if i == len(new) {
			flush()
			break
		}
		v := -1
		if new[i] != old[i] {
			v = sh.splitG
		}
		if sum <= 0 {
			sum, start = v, i
		} else {
			sum += v
		}
		if sum > best {
			best, bs, be = sum, start, i+1
		}
		if sum <= 0 && best >= sh.splitMin {
			flush()
			sum = 0
		}
	}
	return out
}

type lzOut struct {
	ctrl, lenf, nextra, seek []byte
	dpos, dval               []byte
	diff, extra              []byte
	ntriples                 int
}

func (o *lzOut) streams() [][]byte {
	return [][]byte{o.ctrl, o.lenf, o.nextra, o.seek, o.diff, o.dpos, o.dval, o.extra}
}

// spikeDiffWriter is diffWriter with the value bytes optionally in their own
// stream, which is Percival's difference-map/non-zero split (2.7).
type spikeDiffWriter struct {
	o                *lzOut
	merge            bool
	split            bool
	mergeGap         int
	logical, lastEnd int64
}

func (d *spikeDiffWriter) write(new, old []byte) {
	pos, val := &d.o.diff, &d.o.diff
	if d.split {
		pos, val = &d.o.dpos, &d.o.dval
	}
	for i := 0; i < len(new); {
		if new[i] == old[i] {
			i++
			continue
		}
		s := i
		e := i + 1
		for e < len(new) {
			k := e
			for k < len(new) && k-e < d.mergeGap && new[k] == old[k] {
				k++
			}
			if k-e < d.mergeGap && k < len(new) {
				e = k + 1
				continue
			}
			break
		}
		putU(pos, uint64(d.logical+int64(s)-d.lastEnd))
		putU(pos, uint64(e-s))
		for k := s; k < e; k++ {
			*val = append(*val, new[k]-old[k])
		}
		d.lastEnd = d.logical + int64(e)
		i = e
	}
	d.logical += int64(len(new))
}

// diff is plainDiff with every constant turned into a parameter.
func (sh lzShape) diff(old, new []byte) *lzOut {
	o := &lzOut{}
	d := &spikeDiffWriter{o: o, split: sh.splitDR, mergeGap: sh.merge}
	lenfDst, nextraDst, seekDst := &o.ctrl, &o.ctrl, &o.ctrl
	if sh.cols {
		lenfDst, nextraDst, seekDst = &o.lenf, &o.nextra, &o.seek
	}
	if len(old) == 0 {
		putU(lenfDst, 0)
		putU(nextraDst, uint64(len(new)))
		putS(seekDst, 0)
		o.extra = append(o.extra, new...)
		o.ntriples = 1
		return o
	}
	ix := lz.NewIndex(old)
	ix.SetProbe(sh.probe)
	inOld := func(i int) bool { return i >= 0 && i < len(old) }
	scan, mlen, pos := 0, 0, 0
	lastscan, lastpos, lastoffset := 0, 0, 0
	for scan < len(new) {
		oldscore := 0
		scan += mlen
		for scsc := scan; scan < len(new); scan++ {
			pos, mlen = ix.Find(new, scan, scan+lastoffset)
			for ; scsc < scan+mlen; scsc++ {
				if inOld(scsc+lastoffset) && old[scsc+lastoffset] == new[scsc] {
					oldscore++
				}
			}
			if mlen == oldscore && mlen != 0 || mlen > oldscore+sh.cutoff {
				break
			}
			if inOld(scan+lastoffset) && old[scan+lastoffset] == new[scan] {
				oldscore--
			}
		}
		if mlen != oldscore || scan == len(new) {
			var s, best, lenf int
			for i := 0; lastscan+i < scan && lastpos+i < len(old); i++ {
				if old[lastpos+i] == new[lastscan+i] {
					s++
				}
				if s*sh.extNum-(i+1)*sh.extDen > best*sh.extNum-lenf*sh.extDen {
					best, lenf = s, i+1
				}
			}
			lenb := 0
			if scan < len(new) {
				s, best = 0, 0
				for i := 1; scan >= lastscan+i && pos >= i; i++ {
					if old[pos-i] == new[scan-i] {
						s++
					}
					if s*sh.extNum-i*sh.extDen > best*sh.extNum-lenb*sh.extDen {
						best, lenb = s, i
					}
				}
			}
			if lastscan+lenf > scan-lenb {
				overlap := lastscan + lenf - (scan - lenb)
				s, best, lens := 0, 0, 0
				for i := 0; i < overlap; i++ {
					if new[lastscan+lenf-overlap+i] == old[lastpos+lenf-overlap+i] {
						s++
					}
					if new[scan-lenb+i] == old[pos-lenb+i] {
						s--
					}
					if s > best {
						best, lens = s, i+1
					}
				}
				lenf += lens - overlap
				lenb -= lens
			}
			mnew, mold := new[lastscan:lastscan+lenf], old[lastpos:lastpos+lenf]
			at := 0
			for _, e := range sh.excise(mnew, mold) {
				if sh.collect != nil {
					sh.collect(mnew[at:e[0]], mold[at:e[0]])
				} else {
					d.write(mnew[at:e[0]], mold[at:e[0]])
				}
				o.extra = append(o.extra, mnew[e[0]:e[1]]...)
				putU(lenfDst, uint64(e[0]-at))
				putU(nextraDst, uint64(e[1]-e[0]))
				putS(seekDst, int64(e[1]-e[0]))
				o.ntriples++
				at = e[1]
			}
			if sh.collect != nil {
				sh.collect(mnew[at:], mold[at:])
			} else {
				d.write(mnew[at:], mold[at:])
			}
			nextra := (scan - lenb) - (lastscan + lenf)
			o.extra = append(o.extra, new[lastscan+lenf:lastscan+lenf+nextra]...)
			putU(lenfDst, uint64(lenf-at))
			putU(nextraDst, uint64(nextra))
			putS(seekDst, int64((pos-lenb)-(lastpos+lenf)))
			o.ntriples++
			lastscan, lastpos, lastoffset = scan-lenb, pos-lenb, pos-scan
		}
	}
	return o
}

func putS(dst *[]byte, v int64) {
	u := uint64(v) << 1
	if v < 0 {
		u = ^uint64(-v)<<1 | 1
	}
	putU(dst, u)
}

func TestBS6LZSpike(t *testing.T) {
	oldPath, newPath := os.Getenv("BS6_OLD"), os.Getenv("BS6_NEW")
	if oldPath == "" || newPath == "" {
		t.Skip("set BS6_OLD and BS6_NEW")
	}
	old, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	nw, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	base := czLen(DiffLZ(old, nw))
	t.Logf("%s -> %s: %d -> %d bytes", oldPath, newPath, len(old), len(nw))
	t.Logf("shipped DiffLZ: %d cz", base)

	probes := []int{5}
	if v := os.Getenv("BS6_PROBES"); v != "" {
		probes = nil
		for _, f := range strings.Split(v, ",") {
			n, _ := strconv.Atoi(f)
			probes = append(probes, n)
		}
	}
	type row struct {
		name  string
		sh    lzShape
		total int
		parts []int
		n     int
	}
	var rows []row
	add := func(name string, sh lzShape) { rows = append(rows, row{name: name, sh: sh}) }
	for _, p := range probes {
		def := lzShape{probe: p, cutoff: 8, extNum: 2, extDen: 1, merge: 6}
		add(fmt.Sprintf("probe%-3d baseline", p), def)
		s := def
		s.cols = true
		add(fmt.Sprintf("probe%-3d +ctrl cols", p), s)
		s.splitDR = true
		add(fmt.Sprintf("probe%-3d +ctrl cols +diff split", p), s)
		s2 := def
		s2.splitDR = true
		add(fmt.Sprintf("probe%-3d +diff split", p), s2)
	}
	// cost-model sweeps, at the shipped probe, with the best packing on
	for _, c := range []int{2, 4, 8, 16, 32} {
		for _, num := range []int{2, 3, 4} {
			add(fmt.Sprintf("cutoff%-2d ext%d:1", c, num),
				lzShape{probe: 5, cutoff: c, extNum: num, extDen: 1, merge: 6, cols: true, splitDR: true})
		}
	}
	for _, m := range []int{2, 4, 6, 10, 16} {
		add(fmt.Sprintf("merge%-2d", m),
			lzShape{probe: 5, cutoff: 8, extNum: 2, extDen: 1, merge: m, cols: true, splitDR: true})
	}
	// the unmatched edge
	// The correction's per-region selector turned out to be measuring its
	// own cost model rather than the idea, so the excision grid is extended
	// far enough out that the curve either crosses zero or is shown not to.
	for _, g := range []int{1, 2, 3, 6} {
		for _, mn := range []int{8, 24, 64, 256, 1024, 4096, 16384} {
			add(fmt.Sprintf("excise g%d min%d", g, mn),
				lzShape{probe: 5, cutoff: 8, extNum: 2, extDen: 1, merge: 6,
					splitG: g, splitMin: mn})
		}
	}

	if only := os.Getenv("BS6_ONLY"); only != "" {
		var keep []row
		for _, r := range rows {
			if strings.Contains(r.name, only) || strings.Contains(r.name, "baseline") {
				keep = append(keep, r)
			}
		}
		rows = keep
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := &rows[i]
			o := r.sh.diff(old, nw)
			r.n = o.ntriples
			for _, b := range o.streams() {
				n := czLen(b)
				r.parts = append(r.parts, n)
				r.total += n
			}
		}()
	}
	wg.Wait()
	t.Logf("%-34s %10s %9s %8s  ctrl/lenf/nextra/seek/diff/dpos/dval/extra", "shape", "cz", "vs ship", "triples")
	for _, r := range rows {
		t.Logf("%-34s %10d %8.2f%% %8d  %v", r.name, r.total,
			100*float64(r.total-base)/float64(base), r.n, r.parts)
	}
}
