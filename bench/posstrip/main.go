// posstrip: how each differ sees a target binary, per slice, for the
// corrections strip in docs/blog.
//
//	posstrip -old A -new B [-rdiff d] [-xdelta p.xd] [-bsdiff p.bs] [-zucchini p.zuc] [-slice 1048576]
//
// Each row reports the bytes the patch sends literally or corrects (per
// slice of the new file), the raw bytes of its plan (copy commands,
// instructions, control stream, equivalences) and of any correction stream
// carried separately from the literals.
package main

import (
	"bufio"
	"compress/bzip2"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/wjordan/go-binsync/presage/gomod"
)

type row struct {
	Name      string    `json:"name"`
	Literal   int       `json:"literal"`
	Plan      int       `json:"plan"`
	Corr      int       `json:"corr"`
	CorrCount int       `json:"corrCount"`
	Frac      []float64 `json:"frac"`
}

type out struct {
	Total    int      `json:"total"`
	Slice    int      `json:"slice"`
	Sections [][3]any `json:"sections"`
	Rows     []row    `json:"rows"`
}

var slice int

func main() {
	oldP := flag.String("old", "", "")
	newP := flag.String("new", "", "")
	rd := flag.String("rdiff", "", "librsync delta")
	xd := flag.String("xdelta", "", "")
	bs := flag.String("bsdiff", "", "")
	zc := flag.String("zucchini", "", "")
	flag.IntVar(&slice, "slice", 1<<20, "")
	flag.Parse()
	old, err := os.ReadFile(*oldP)
	check(err)
	target, err := os.ReadFile(*newP)
	check(err)
	n := (len(target) + slice - 1) / slice
	o := out{Total: len(target), Slice: slice}
	f, err := elf.NewFile(strings.NewReader(string(target)))
	check(err)
	for _, s := range f.Sections {
		if s.Type != elf.SHT_NOBITS && s.Size >= 1<<20 {
			o.Sections = append(o.Sections, [3]any{s.Name, s.Offset, s.Size})
		}
	}
	add := func(r row, hits []int) {
		r.Frac = make([]float64, n)
		for i, h := range hits {
			r.Literal += h
			r.Frac[i] = float64(h) / float64(min(slice, len(target)-i*slice))
		}
		o.Rows = append(o.Rows, r)
	}
	if *rd != "" {
		add(rdiffRow(*rd, n))
	}
	if *xd != "" {
		add(xdeltaRow(*xd, n))
	}
	if *bs != "" {
		add(bsdiffRow(*bs, n))
	}
	if *zc != "" {
		add(zucchiniRow(*zc, n))
	}
	plan, _, err := gomod.Module{}.Analyse([][]byte{old}, target)
	check(err)
	pred, err := gomod.Module{}.Materialise([][]byte{old}, plan, int64(len(target)))
	check(err)
	hits := make([]int, n)
	for i := range target {
		if pred[i] != target[i] {
			hits[i/slice]++
		}
	}
	add(row{Name: "presage", Plan: len(plan)}, hits)
	check(json.NewEncoder(os.Stdout).Encode(o))
}

func hitRange(hits []int, pos, size int) {
	for size > 0 {
		b := pos / slice
		k := min(size, (b+1)*slice-pos)
		if b < len(hits) {
			hits[b] += k
		}
		pos += k
		size -= k
	}
}

// rdiffRow walks a librsync delta: literal commands are sent bytes, copy
// commands are the plan.
func rdiffRow(path string, n int) (row, []int) {
	b, err := os.ReadFile(path)
	check(err)
	if binary.BigEndian.Uint32(b) != 0x72730236 {
		panic("not an rdiff delta")
	}
	hits := make([]int, n)
	r := row{Name: "rsync (4 KB)"}
	be := func(p []byte, w int) int {
		v := 0
		for i := 0; i < w; i++ {
			v = v<<8 | int(p[i])
		}
		return v
	}
	pos, i := 0, 4
	for i < len(b) {
		op := int(b[i])
		i++
		switch {
		case op == 0:
			i = len(b)
		case op <= 64:
			hitRange(hits, pos, op)
			pos += op
			i += op
		case op <= 68:
			w := 1 << (op - 65)
			l := be(b[i:], w)
			i += w
			hitRange(hits, pos, l)
			pos += l
			i += l
		case op <= 84:
			k := op - 69
			sw, lw := 1<<(k/4), 1<<(k%4)
			l := be(b[i+sw:], lw)
			i += sw + lw
			r.Plan += 1 + sw + lw
			pos += l
		default:
			panic(fmt.Sprintf("rdiff op %d", op))
		}
	}
	return r, hits
}

// xdeltaRow counts ADD/RUN bytes from `xdelta3 printdelta`; the plan is the
// instruction and address sections of every window.
func xdeltaRow(path string, n int) (row, []int) {
	cmd := exec.Command("xdelta3", "printdelta", path)
	rd, err := cmd.StdoutPipe()
	check(err)
	check(cmd.Start())
	hits := make([]int, n)
	r := row{Name: "xdelta3"}
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		for _, k := range []string{"VCDIFF inst section length:", "VCDIFF addr section length:"} {
			if strings.HasPrefix(line, k) {
				v, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, k)))
				r.Plan += v
			}
		}
		fs := strings.Fields(line)
		if len(fs) < 4 {
			continue
		}
		pos, err := strconv.Atoi(fs[0]) // absolute target offset
		if err != nil {
			continue
		}
		// fields: off code type1 size1 [@addr1] [type2 size2 [@addr2]]
		i := 2
		for i+1 < len(fs) {
			typ, sz := fs[i], fs[i+1]
			size, _ := strconv.Atoi(sz)
			i += 2
			if i < len(fs) && strings.Contains(fs[i], "@") {
				i++
			}
			switch typ {
			case "ADD":
				hitRange(hits, pos, size)
			case "RUN":
				hitRange(hits, pos, 1)
			}
			pos += size
		}
	}
	check(cmd.Wait())
	return r, hits
}

// bsdiffRow: extra bytes plus non-zero diff bytes are literal; the control
// stream is the plan.
func bsdiffRow(path string, n int) (row, []int) {
	b, err := os.ReadFile(path)
	check(err)
	if string(b[:8]) != "BSDIFF40" {
		panic("not a bsdiff patch")
	}
	ctrlLen, diffLen := offtin(b[8:]), offtin(b[16:])
	ctrl := bufio.NewReader(bzip2.NewReader(strings.NewReader(string(b[32 : 32+ctrlLen]))))
	diff := bufio.NewReader(bzip2.NewReader(strings.NewReader(string(b[32+ctrlLen : 32+ctrlLen+diffLen]))))
	hits := make([]int, n)
	r := row{Name: "bsdiff"}
	pos := 0
	var buf [24]byte
	for {
		if _, err := io.ReadFull(ctrl, buf[:]); err != nil {
			break
		}
		r.Plan += 24
		x, y := offtin(buf[:]), offtin(buf[8:])
		for j := 0; j < x; j++ {
			d, _ := diff.ReadByte()
			if d != 0 {
				hitRange(hits, pos, 1)
			}
			pos++
		}
		hitRange(hits, pos, y)
		pos += y
	}
	return r, hits
}

func offtin(b []byte) int {
	y := int(b[7]&0x7f)<<56 | int(b[6])<<48 | int(b[5])<<40 | int(b[4])<<32 | int(b[3])<<24 | int(b[2])<<16 | int(b[1])<<8 | int(b[0])
	if b[7]&0x80 != 0 {
		y = -y
	}
	return y
}

// zucchiniRow parses a Zucchini patch with one element: bytes not covered by
// an equivalence (extra data) and raw-delta units are literal; the
// equivalence streams and extra targets are the plan; reference deltas are
// the correction.
func zucchiniRow(path string, n int) (row, []int) {
	b, err := os.ReadFile(path)
	check(err)
	if string(b[:4]) != "Zucc" {
		panic("not a Zucchini patch")
	}
	total := int(binary.LittleEndian.Uint32(b[16:]))
	if binary.LittleEndian.Uint32(b[24:]) != 1 {
		panic("expected one element")
	}
	pos := 28 + 16 + 4 + 2 // element: old/new regions, type, version
	stream := func() []byte {
		l := int(binary.LittleEndian.Uint32(b[pos:]))
		pos += 4
		s := b[pos : pos+l]
		pos += l
		return s
	}
	srcSkip, dstSkip, copyCount := stream(), stream(), stream()
	extra, rdOff, _, refDelta := stream(), stream(), stream(), stream()
	rest := b[pos:] // extra targets, per reference pool
	r := row{Name: "Zucchini", Plan: len(srcSkip) + len(dstSkip) + len(copyCount) + len(rest), Corr: len(refDelta)}
	for _, c := range refDelta {
		if c&0x80 == 0 {
			r.CorrCount++
		}
	}
	ds, cc := varuints(dstSkip), varuints(copyCount)
	covered := make([]bool, total)
	var starts, cum []int
	dst, sum := 0, 0
	for i := range ds {
		dst += ds[i]
		starts = append(starts, dst)
		cum = append(cum, sum)
		for j := 0; j < cc[i]; j++ {
			covered[dst+j] = true
		}
		dst += cc[i]
		sum += cc[i]
	}
	lit := make([]bool, total)
	nExtra := 0
	for i, c := range covered {
		if !c {
			lit[i] = true
			nExtra++
		}
	}
	if nExtra != len(extra) {
		panic(fmt.Sprintf("extra data %d vs uncovered %d", len(extra), nExtra))
	}
	comp := 0
	for _, d := range varuints(rdOff) {
		o := d + comp
		comp = o + 1
		k := sort.SearchInts(cum, o+1) - 1
		lit[starts[k]+o-cum[k]] = true
	}
	hits := make([]int, n)
	for i, l := range lit {
		if l {
			hits[i/slice]++
		}
	}
	return r, hits
}

func varuints(s []byte) []int {
	var out []int
	v, sh := 0, 0
	for _, c := range s {
		v |= int(c&0x7f) << sh
		if c&0x80 != 0 {
			sh += 7
		} else {
			out = append(out, v)
			v, sh = 0, 0
		}
	}
	return out
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
