// posstrip: per-slice literal/wrong byte fractions of several differs over a
// target binary, for the corrections strip in docs/blog.
//
//	posstrip -old A -new B [-xdelta p.xd] [-bsdiff p.bs] [-slice 1048576]
//
// xdelta3 rows count bytes in ADD/RUN instructions (parsed from
// `xdelta3 printdelta`); bsdiff rows count extra bytes plus non-zero diff
// bytes; the presage row counts bytes the Go codec's prediction gets wrong.
package main

import (
	"bufio"
	"compress/bzip2"
	"debug/elf"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/wjordan/go-binsync/presage/gomod"
)

type row struct {
	Name  string    `json:"name"`
	Bytes int       `json:"bytes"`
	Frac  []float64 `json:"frac"`
}

type out struct {
	Total    int      `json:"total"`
	Slice    int      `json:"slice"`
	Sections [][3]any `json:"sections"`
	Rows     []row    `json:"rows"`
}

func main() {
	oldP := flag.String("old", "", "")
	newP := flag.String("new", "", "")
	xd := flag.String("xdelta", "", "")
	bs := flag.String("bsdiff", "", "")
	slice := flag.Int("slice", 1<<20, "")
	flag.Parse()
	old, err := os.ReadFile(*oldP)
	check(err)
	target, err := os.ReadFile(*newP)
	check(err)
	n := (len(target) + *slice - 1) / *slice
	o := out{Total: len(target), Slice: *slice}
	f, err := elf.NewFile(strings.NewReader(string(target)))
	check(err)
	for _, s := range f.Sections {
		if s.Type != elf.SHT_NOBITS && s.Size >= 1<<20 {
			o.Sections = append(o.Sections, [3]any{s.Name, s.Offset, s.Size})
		}
	}
	mark := func(name string, hits []int) {
		r := row{Name: name, Frac: make([]float64, n)}
		for i, h := range hits {
			r.Bytes += h
			r.Frac[i] = float64(h) / float64(min(*slice, len(target)-i**slice))
		}
		o.Rows = append(o.Rows, r)
	}
	if *xd != "" {
		mark("xdelta3", xdeltaHits(*xd, n, *slice))
	}
	if *bs != "" {
		mark("bsdiff", bsdiffHits(*bs, n, *slice))
	}
	plan, _, err := gomod.Module{}.Analyse([][]byte{old}, target)
	check(err)
	pred, err := gomod.Module{}.Materialise([][]byte{old}, plan, int64(len(target)))
	check(err)
	hits := make([]int, n)
	for i := range target {
		if pred[i] != target[i] {
			hits[i / *slice]++
		}
	}
	mark("presage", hits)
	check(json.NewEncoder(os.Stdout).Encode(o))
}

func xdeltaHits(path string, n, slice int) []int {
	cmd := exec.Command("xdelta3", "printdelta", path)
	rd, err := cmd.StdoutPipe()
	check(err)
	check(cmd.Start())
	hits := make([]int, n)
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
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
			switch {
			case typ == "ADD":
				hitRange(hits, pos, size, slice)
			case typ == "RUN":
				hitRange(hits, pos, 1, slice)
			}
			pos += size
		}
	}
	check(cmd.Wait())
	return hits
}

func hitRange(hits []int, pos, size, slice int) {
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

func bsdiffHits(path string, n, slice int) []int {
	b, err := os.ReadFile(path)
	check(err)
	if string(b[:8]) != "BSDIFF40" {
		panic("not a bsdiff patch")
	}
	ctrlLen, diffLen := offtin(b[8:]), offtin(b[16:])
	ctrl := bufio.NewReader(bzip2.NewReader(strings.NewReader(string(b[32 : 32+ctrlLen]))))
	diff := bufio.NewReader(bzip2.NewReader(strings.NewReader(string(b[32+ctrlLen : 32+ctrlLen+diffLen]))))
	extra := bufio.NewReader(bzip2.NewReader(strings.NewReader(string(b[32+ctrlLen+diffLen:]))))
	hits := make([]int, n)
	pos, oldPos := 0, 0
	var buf [8]byte
	for {
		if _, err := io.ReadFull(ctrl, buf[:]); err != nil {
			break
		}
		x := offtin(buf[:])
		io.ReadFull(ctrl, buf[:])
		y := offtin(buf[:])
		io.ReadFull(ctrl, buf[:])
		z := offtin(buf[:])
		for j := 0; j < x; j++ {
			d, _ := diff.ReadByte()
			if d != 0 && pos/slice < n {
				hits[pos/slice]++
			}
			pos++
		}
		oldPos += x
		for j := 0; j < y; j++ {
			extra.ReadByte()
			if pos/slice < n {
				hits[pos/slice]++
			}
			pos++
		}
		oldPos += z
	}
	return hits
}

func offtin(b []byte) int {
	y := int(b[7]&0x7f)<<56 | int(b[6])<<48 | int(b[5])<<40 | int(b[4])<<32 | int(b[3])<<24 | int(b[2])<<16 | int(b[1])<<8 | int(b[0])
	if b[7]&0x80 != 0 {
		y = -y
	}
	return y
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
