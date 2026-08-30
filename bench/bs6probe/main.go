// Command bs6probe prices Percival's difference-string modes (thesis 2.7,
// "bsdiff 6") against the correction presage ships.
//
// presage's positional correction writes a differing run either as literals
// (the shipped shape) or as want^pred (the near-miss shape). bsdiff has
// always written want-pred instead, and bsdiff 6 adds two multiprecision
// modes that let a borrow propagate across the bytes of one field, so a
// mispredicted rel32 costs one small digit rather than four bytes.
//
// This probe does not encode a patch. It takes a prediction and its target,
// enumerates the differing runs the correction would, and prices the value
// stream under each mode in the columnar layout the harness already found
// best (gaps, lengths, and values bucketed by run length).
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/internal/cz"
)

// buckets groups value bytes by the length of the run they belong to;
// four-byte runs are displacement fields, one-byte runs are lone opcodes.
const buckets = 5

func bucketOf(n int) int { return min(n, buckets) - 1 }

// a run of the correction: [lo,hi) in the file, already widened to word.
type run struct{ lo, hi int }

// runs finds the maximal differing stretches, merged when fewer than merge
// bytes apart, on a word-aligned grid.
func runs(pred, target []byte, word, merge int) []run {
	n := len(target)
	var out []run
	i := 0
	for i < n {
		if pred[i] == target[i] {
			i++
			continue
		}
		lo := i &^ (word - 1)
		hi := i + 1
		for hi < n {
			// look ahead: keep going while the identical stretch is short
			k := hi
			for k < n && k-hi < merge && pred[k] == target[k] {
				k++
			}
			if k-hi < merge && k < n {
				hi = k + 1
				continue
			}
			break
		}
		hi = min((hi+word-1)&^(word-1), n)
		out = append(out, run{lo, hi})
		i = hi
	}
	return out
}

// mode writes the value bytes for one run.
type mode struct {
	name string
	// emit appends the run's values; it may append fewer than hi-lo bytes,
	// in which case n is what the length column carries.
	emit func(pred, target []byte, dst []byte, word int) (out []byte, n int)
}

func balancedLE(pred, target []byte, dst []byte, w int, trim bool) []byte {
	var p, t uint64
	for i := w - 1; i >= 0; i-- {
		p = p<<8 | uint64(pred[i])
		t = t<<8 | uint64(target[i])
	}
	d := int64(t - p)
	if w < 8 { // sign-extend the difference to the field's width
		sh := uint(64 - 8*w)
		d = int64(uint64(d)<<sh) >> sh
	}
	var digits [8]byte
	nsig := 0
	for i := 0; i < w; i++ {
		v := d & 0xff
		if v > 127 {
			v -= 256
		}
		d = (d - v) >> 8
		digits[i] = byte(v)
		if v != 0 {
			nsig = i + 1
		}
	}
	if trim {
		return append(dst, digits[:nsig]...)
	}
	return append(dst, digits[:w]...)
}

func balancedBE(pred, target []byte, dst []byte, w int, trim bool) []byte {
	var p, t uint64
	for i := 0; i < w; i++ {
		p = p<<8 | uint64(pred[i])
		t = t<<8 | uint64(target[i])
	}
	d := int64(t - p)
	if w < 8 {
		sh := uint(64 - 8*w)
		d = int64(uint64(d)<<sh) >> sh
	}
	var digits [8]byte
	first := w
	for i := 0; i < w; i++ {
		v := d & 0xff
		if v > 127 {
			v -= 256
		}
		d = (d - v) >> 8
		digits[w-1-i] = byte(v)
		if v != 0 {
			first = w - 1 - i
		}
	}
	if trim {
		return append(dst, digits[first:]...)
	}
	return append(dst, digits[:w]...)
}

func modes(word int) []mode {
	perWord := func(f func(p, t, dst []byte, w int, trim bool) []byte, trim bool) func(pred, target, dst []byte, w int) ([]byte, int) {
		return func(pred, target, dst []byte, w int) ([]byte, int) {
			start := len(dst)
			for i := 0; i+w <= len(target); i += w {
				dst = f(pred[i:], target[i:], dst, w, trim)
			}
			return dst, len(dst) - start
		}
	}
	return []mode{
		{"lit", func(pred, target, dst []byte, w int) ([]byte, int) {
			return append(dst, target...), len(target)
		}},
		{"xor", func(pred, target, dst []byte, w int) ([]byte, int) {
			for i := range target {
				dst = append(dst, pred[i]^target[i])
			}
			return dst, len(target)
		}},
		{"sub", func(pred, target, dst []byte, w int) ([]byte, int) {
			for i := range target {
				dst = append(dst, target[i]-pred[i])
			}
			return dst, len(target)
		}},
		{fmt.Sprintf("mple%d", word), perWord(balancedLE, false)},
		{fmt.Sprintf("mpbe%d", word), perWord(balancedBE, false)},
		{fmt.Sprintf("mple%dt", word), perWord(balancedLE, true)},
	}
}

type streams struct {
	gaps, lens []byte
	vals       [buckets][]byte
}

func (s *streams) all() [][]byte {
	out := [][]byte{s.gaps, s.lens}
	for i := range s.vals {
		out = append(out, s.vals[i])
	}
	return out
}

// czLen is what a stream costs once the container compresses it.
func czLen(b []byte) int {
	const frame = 8 << 20
	n := 0
	for off := 0; ; off += frame {
		end := min(off+frame, len(b))
		_, z := cz.Compress(b[off:end])
		n += len(z)
		if end == len(b) {
			return n
		}
	}
}

func main() {
	var (
		oldPath  = flag.String("old", "", "old file (prediction is delta.Predict(old,new))")
		newPath  = flag.String("new", "", "target file")
		predPath = flag.String("pred", "", "prediction file, instead of running the predictor")
		word     = flag.Int("word", 4, "field width the runs are widened to (1 = no widening)")
		merge    = flag.Int("merge", 6, "identical bytes a run absorbs")
		limit    = flag.Int("limit", 0, "only the first N bytes (0 = whole file)")
		dump     = flag.String("dumppred", "", "write the prediction here and exit")
		merges   = flag.String("merges", "", "comma-separated merge sweep, instead of -merge")
		words    = flag.String("words", "", "comma-separated word sweep, instead of -word")
	)
	flag.Parse()

	target, err := os.ReadFile(*newPath)
	must(err)
	var pred []byte
	if *predPath != "" {
		pred, err = os.ReadFile(*predPath)
		must(err)
	} else {
		old, err := os.ReadFile(*oldPath)
		must(err)
		cp, err := delta.Predict(old, target)
		must(err)
		pred = cp.Pred
	}
	if *dump != "" {
		must(os.WriteFile(*dump, pred, 0o644))
		return
	}
	if len(pred) != len(target) {
		fmt.Fprintf(os.Stderr, "prediction is %d bytes, target %d\n", len(pred), len(target))
		os.Exit(1)
	}
	if *limit > 0 && *limit < len(target) {
		pred, target = pred[:*limit], target[:*limit]
	}

	// The honest reference: what the deployed correction costs on this pair,
	// in the two shapes delta ships, priced with the container's compressor.
	if plain, near, err := delta.CorrectionShapes(pred, target); err == nil {
		fmt.Printf("shipped correction: plain %d (cz %d)  near-miss %d (cz %d)\n",
			len(plain), czLen(plain), len(near), czLen(near))
	}

	ws, mg := parseList(*words, *word), parseList(*merges, *merge)
	for _, w := range ws {
		for _, mrg := range mg {
			sweep(pred, target, w, mrg)
		}
	}
}

// sweep prices every difference mode for one (word, merge) shape.
func sweep(pred, target []byte, word, merge int) {
	rs := runs(pred, target, word, merge)
	wrong, cover := 0, 0
	var hist [buckets]int
	for i := range target {
		if pred[i] != target[i] {
			wrong++
		}
	}
	for _, r := range rs {
		cover += r.hi - r.lo
		hist[bucketOf(r.hi-r.lo)]++
	}
	fmt.Printf("\nword %d merge %d: runs %d  covered %d (%.2fx the %d wrong bytes)  lengths ",
		word, merge, len(rs), cover, float64(cover)/float64(max(wrong, 1)), wrong)
	for i, n := range hist {
		label := fmt.Sprint(i + 1)
		if i == buckets-1 {
			label = fmt.Sprintf("%d+", i+1)
		}
		fmt.Printf("%s=%d ", label, n)
	}
	fmt.Println()

	ms := modes(word)
	results := make([]struct {
		raw, cz int
		parts   []int
	}, len(ms))
	var wg sync.WaitGroup
	for mi, m := range ms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var s streams
			prev := 0
			for _, r := range rs {
				var n int
				b := bucketOf(r.hi - r.lo)
				s.vals[b], n = m.emit(pred[r.lo:r.hi], target[r.lo:r.hi], s.vals[b], word)
				s.gaps = binary.AppendUvarint(s.gaps, uint64(r.lo-prev))
				s.lens = binary.AppendUvarint(s.lens, uint64(n))
				prev = r.hi
			}
			raw, total := 0, 0
			var parts []int
			for _, b := range s.all() {
				raw += len(b)
				n := czLen(b)
				parts = append(parts, n)
				total += n
			}
			results[mi].raw, results[mi].cz, results[mi].parts = raw, total, parts
		}()
	}
	wg.Wait()

	base := results[0].cz
	fmt.Printf("%-10s %12s %12s %9s   %s\n", "mode", "raw", "cz", "vs lit", "gaps/lens/b1..b5")
	for mi, m := range ms {
		r := results[mi]
		fmt.Printf("%-10s %12d %12d %8.2f%%   %v\n", m.name, r.raw, r.cz,
			100*float64(r.cz-base)/float64(base), r.parts)
	}
	perRun(pred, target, rs, word, base)
}

func parseList(s string, def int) []int {
	if s == "" {
		return []int{def}
	}
	var out []int
	for _, f := range strings.Split(s, ",") {
		var v int
		fmt.Sscan(f, &v)
		out = append(out, v)
	}
	return out
}

// perRun prices a correction that names a mode per run rather than per file.
func perRun(pred, target []byte, rs []run, word, base int) {
	var s streams
	var sel []byte
	prev := 0
	counts := map[string]int{}
	cand := modes(word)
	for _, r := range rs {
		b := bucketOf(r.hi - r.lo)
		bestI, bestN, bestBuf := 0, 1<<30, []byte(nil)
		for i, m := range cand {
			buf, n := m.emit(pred[r.lo:r.hi], target[r.lo:r.hi], nil, word)
			nz := 0
			for _, v := range buf {
				if v != 0 {
					nz++
				}
			}
			// score: bytes carried, then non-zero bytes
			score := n*4 + nz
			if score < bestN {
				bestI, bestN, bestBuf = i, score, buf
			}
		}
		counts[cand[bestI].name]++
		sel = append(sel, byte(bestI))
		s.vals[b] = append(s.vals[b], bestBuf...)
		s.gaps = binary.AppendUvarint(s.gaps, uint64(r.lo-prev))
		s.lens = binary.AppendUvarint(s.lens, uint64(len(bestBuf)))
		prev = r.hi
	}
	total, raw := czLen(sel), len(sel)
	for _, b := range s.all() {
		raw += len(b)
		total += czLen(b)
	}
	fmt.Printf("%-10s %12d %12d %8.2f%%   (selector %d B)\n", "per-run", raw, total,
		100*float64(total-base)/float64(base), czLen(sel))
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("  picks: ")
	for _, k := range keys {
		fmt.Printf("%s=%d ", k, counts[k])
	}
	fmt.Println()
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
