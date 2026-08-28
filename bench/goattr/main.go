// Command goattr attributes the Go-aware codec's residual -- the bytes its
// prediction gets wrong -- to the layer that would have to fix them, and
// prices every class the only way a correction stream can be priced: by
// what removing it does to the compressed patch.
//
// It is the Chrome ELF spike's methodology (docs/general/research/
// chrome-elf-whole-image.md 11.1-11.3, 15, 16.2) applied to delta/. Report
// only: nothing here changes a patch byte, and the codec's own
// EncodeCorrection and compressor are used so the prices are the shipped
// ones.
//
// Six ladders, each priced:
//
//	1  by section
//	2  .text by cause (how the function was matched)
//	3  .text inside a relocated field vs not, and whether a wrong field is
//	   a map error or a genuinely different target
//	4  .text by instruction and operand class, outside fields
//	5  .go.type by descriptor field
//	6  is the new code elsewhere in the old image (minor pair only)
package main

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/wjordan/go-binsync/delta"
	"github.com/wjordan/go-binsync/delta/gobin"
	"github.com/wjordan/go-binsync/internal/cz"
)

var (
	oldPath  = flag.String("old", "", "old binary")
	newPath  = flag.String("new", "", "new binary")
	label    = flag.String("label", "", "name for this pair in the report")
	cacheDir = flag.String("cache", "", "directory for the cached prediction")
	jobs     = flag.Int("jobs", 4, "concurrent marginal-cost measurements")
	levels   = flag.String("levels", "123456", "which ladders to print")
	dictWin  = flag.Int("dict-window", 0, "cap the level-6 dictionary at this many bytes (0 = whole .text)")
)

func main() {
	flag.Parse()
	if *oldPath == "" || *newPath == "" {
		fmt.Fprintln(os.Stderr, "goattr: -old and -new are required")
		os.Exit(2)
	}
	c, err := load(*oldPath, *newPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goattr: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "\n=== %s: %s -> %s\n", *label, filepath.Base(*oldPath), filepath.Base(*newPath))
	fmt.Fprintf(os.Stderr, "old %d B, new %d B, prediction %d wrong bytes in %d runs (%.4f%% of the file)\n",
		len(c.old), len(c.new), c.wrongTotal, len(c.regs), 100*float64(c.wrongTotal)/float64(len(c.new)))
	fmt.Fprintf(os.Stderr, "correction: %d raw, %d compressed; layout %d, stage1a %d, stage1b %d (raw)\n",
		c.rawFull, c.full, c.pr.LayoutLen, c.pr.Stage1aLen, c.pr.Stage1bLen)
	fmt.Fprintf(os.Stderr, "match: %d new funcs, %d exact, %d normalised, %d content, %d unmatched-new, %d unmatched-old\n",
		len(c.nb.Funcs), c.pr.Exact, c.pr.Norm, c.pr.Content, c.pr.Unmatched, c.pr.UnmatchedOld)

	if has('1') {
		yardstick(c.bySection())
	}
	if has('2') {
		c.byCause()
	}
	if has('3') {
		c.byField()
	}
	if has('4') {
		c.byInstruction()
	}
	if has('5') {
		c.byTypeField()
	}
	if has('6') {
		c.dictionary(*dictWin)
	}
}

func has(l byte) bool {
	for i := 0; i < len(*levels); i++ {
		if (*levels)[i] == l {
			return true
		}
	}
	return false
}

// reg is one region of the correction: the codec merges identical stretches
// shorter than mergeGap rather than paying for a second region header, so a
// run is not simply a maximal stretch of wrong bytes.
type reg struct{ s, e int }

const mergeGap = 6

// regions replays encodeCorrection's region loop. It must stay identical to
// delta/correct.go's, because every "runs" column here is priced against a
// correction the codec really wrote.
func regions(pred, want []byte) []reg {
	var out []reg
	for s := 0; s < len(want); {
		if pred[s] == want[s] {
			s++
			continue
		}
		e := s + 1
		for e < len(want) {
			k := e
			for k < len(want) && k-e < mergeGap && pred[k] == want[k] {
				k++
			}
			if k-e < mergeGap && k < len(want) {
				e = k + 1
				continue
			}
			break
		}
		out = append(out, reg{s, e})
		s = e
	}
	return out
}

type ctx struct {
	old, new []byte
	ob, nb   *gobin.Bin
	pred     []byte
	pr       *delta.Prediction
	sites    []delta.TypeSite

	regs       []reg
	wrongTotal int
	full       int // compressed size of the whole correction
	rawFull    int

	pool chan []byte
	mu   sync.Mutex
}

// czSize is the codec's own terminal compressor, framed the way the patch
// container frames a body (delta/container.go). Comparing anything measured
// here against the shipped patch means compressing it the same way.
func czSize(b []byte) int {
	n := 0
	for off := 0; ; off += delta.FrameSize {
		end := min(off+delta.FrameSize, len(b))
		_, z := cz.Compress(b[off:end])
		n += len(z)
		if end == len(b) {
			return n
		}
	}
}

func (c *ctx) correction(want []byte) (comp, raw int) {
	s, err := delta.EncodeCorrection(c.pred, want)
	if err != nil {
		panic(err)
	}
	return czSize(s), len(s)
}

func (c *ctx) take() []byte {
	select {
	case b := <-c.pool:
		return b
	default:
		return append([]byte(nil), c.new...)
	}
}

func (c *ctx) put(b []byte) {
	select {
	case c.pool <- b:
	default:
	}
}

// marginal is the only honest price for a class: what the whole compressed
// correction loses when this class's wrong bytes are reverted to the
// prediction. A class's standalone size overstates it -- the spike saw 12x
// (chrome-elf-whole-image.md 9.3) -- because a concatenated stream shares
// context across classes.
func (c *ctx) marginal(pos []int) (comp, raw int) {
	if len(pos) == 0 {
		return 0, 0
	}
	buf := c.take()
	for _, p := range pos {
		buf[p] = c.pred[p]
	}
	gc, gr := c.correction(buf)
	for _, p := range pos {
		buf[p] = c.new[p]
	}
	c.put(buf)
	return c.full - gc, c.rawFull - gr
}

// marginals prices several classes at once, bounded by -jobs.
func (c *ctx) marginals(sets [][]int) []marg {
	out := make([]marg, len(sets))
	var wg sync.WaitGroup
	gate := make(chan struct{}, max(1, *jobs))
	for i, s := range sets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			out[i].comp, out[i].raw = c.marginal(s)
		}()
	}
	wg.Wait()
	return out
}

// marg is one class's price: what the compressed correction loses when the
// class is reverted, and what its raw stream loses. The two diverge where a
// class shares its regions with another -- reverting part of a region can
// flip it from a literal run to an lz one, which moves bytes out of the
// well-compressing literal stream and into the varint control stream.
type marg struct{ comp, raw int }

func load(oldP, newP string) (*ctx, error) {
	t0 := time.Now()
	oldB, err := os.ReadFile(oldP)
	if err != nil {
		return nil, err
	}
	newB, err := os.ReadFile(newP)
	if err != nil {
		return nil, err
	}
	c := &ctx{old: oldB, new: newB, pool: make(chan []byte, max(1, *jobs))}
	if c.ob, err = gobin.Parse(oldB); err != nil {
		return nil, err
	}
	if c.nb, err = gobin.Parse(newB); err != nil {
		return nil, err
	}
	cached, err := c.predict()
	if err != nil {
		return nil, err
	}
	c.regs = regions(c.pred, c.new)
	for i := range c.new {
		if c.pred[i] != c.new[i] {
			c.wrongTotal++
		}
	}
	s, err := delta.EncodeCorrection(c.pred, c.new)
	if err != nil {
		return nil, err
	}
	c.rawFull, c.full = len(s), czSize(s)
	_ = c.rawFull
	fmt.Fprintf(os.Stderr, "goattr: setup %.1fs (prediction %s)\n", time.Since(t0).Seconds(),
		map[bool]string{true: "from cache", false: "computed"}[cached])
	return c, nil
}

// cached is what the scratchpad holds for one pair. The key covers the two
// input files *and* the delta package's sources: a prediction memoised on
// its inputs alone is the spike's silent-fiction failure mode -- a stale
// entry still yields a valid, merely larger, correction that nothing checks.
type cached struct {
	Pred               []byte
	NewToOld, OldToNew []int32
	Sites              []delta.TypeSite
	Exact, Norm, Content, Unmatched, UnmatchedOld,
	LayoutLen, Stage1aLen, Stage1bLen int
}

func codeIdentity() string {
	h := sha256.New()
	var files []string
	for _, dir := range []string{"delta", "delta/x86", "delta/gobin", "delta/internal/lz", "internal/cz"} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if filepath.Ext(e.Name()) == ".go" {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "%s %d\n", f, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func (c *ctx) predict() (bool, error) {
	key := ""
	if *cacheDir != "" {
		ho, hn := sha256.Sum256(c.old), sha256.Sum256(c.new)
		key = filepath.Join(*cacheDir, fmt.Sprintf("pred-%s-%s-%s.gob",
			hex.EncodeToString(ho[:6]), hex.EncodeToString(hn[:6]), codeIdentity()))
		if f, err := os.Open(key); err == nil {
			defer f.Close()
			var e cached
			if gob.NewDecoder(f).Decode(&e) == nil && len(e.Pred) == len(c.new) {
				c.pred, c.sites = e.Pred, e.Sites
				c.pr = &delta.Prediction{
					NewToOld: widen(e.NewToOld), OldToNew: widen(e.OldToNew),
					Exact: e.Exact, Norm: e.Norm, Content: e.Content,
					Unmatched: e.Unmatched, UnmatchedOld: e.UnmatchedOld,
					LayoutLen: e.LayoutLen, Stage1aLen: e.Stage1aLen, Stage1bLen: e.Stage1bLen,
				}
				return true, nil
			}
		}
	}
	pr, err := delta.Predict(c.old, c.new)
	if err != nil {
		return false, err
	}
	c.pr, c.pred, c.sites = pr, pr.Pred, pr.TypeSites()
	if key != "" {
		if err := os.MkdirAll(*cacheDir, 0o755); err == nil {
			if f, err := os.Create(key + ".tmp"); err == nil {
				e := cached{Pred: c.pred, NewToOld: narrow(pr.NewToOld), OldToNew: narrow(pr.OldToNew),
					Sites: c.sites, Exact: pr.Exact, Norm: pr.Norm, Content: pr.Content,
					Unmatched: pr.Unmatched, UnmatchedOld: pr.UnmatchedOld,
					LayoutLen: pr.LayoutLen, Stage1aLen: pr.Stage1aLen, Stage1bLen: pr.Stage1bLen}
				err := gob.NewEncoder(f).Encode(&e)
				f.Close()
				if err == nil {
					os.Rename(key+".tmp", key)
				}
			}
		}
	}
	runtime.GC()
	return false, nil
}

func narrow(a []int) []int32 {
	out := make([]int32, len(a))
	for i, v := range a {
		out[i] = int32(v)
	}
	return out
}

func widen(a []int32) []int {
	out := make([]int, len(a))
	for i, v := range a {
		out[i] = int(v)
	}
	return out
}
