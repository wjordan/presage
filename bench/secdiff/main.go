// secdiff: per-section wrong-byte counts of prediction files against a target.
package main

import (
	"debug/elf"
	"fmt"
	"os"

	"github.com/wjordan/go-binsync/delta"
	"github.com/wjordan/go-binsync/internal/cz"
)

func main() {
	old, _ := os.ReadFile(os.Args[1])
	target, _ := os.ReadFile(os.Args[2])
	preds := map[string][]byte{}
	for _, p := range os.Args[3:] {
		b, err := os.ReadFile(p)
		if err != nil {
			panic(err)
		}
		preds[p] = b
	}
	if cp, err := delta.Predict(old, target); err == nil {
		preds["codec"] = cp.Pred
		os.WriteFile("codec.prediction", cp.Pred, 0o644)
	} else {
		fmt.Println("codec:", err)
	}
	f, _ := elf.NewFile(bytesReader(target))
	names := []string{}
	for name, b := range preds {
		names = append(names, name)
		fmt.Printf("%-40s total wrong %d\n", name, diff(b, target, 0, len(target)))
	}
	for _, s := range f.Sections {
		if s.Type == elf.SHT_NOBITS || s.Size == 0 {
			continue
		}
		line := fmt.Sprintf("%-24s %9d", s.Name, s.Size)
		for _, name := range names {
			line += fmt.Sprintf("  %s=%d", name[:min(12, len(name))], diff(preds[name], target, int(s.Offset), int(s.Offset+s.Size)))
		}
		fmt.Println(line)
	}
}

func diff(a, b []byte, lo, hi int) int {
	n := 0
	for i := lo; i < hi && i < len(a); i++ {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

type br struct{ b []byte }

func bytesReader(b []byte) *reader { return &reader{b: b} }

type reader struct {
	b   []byte
	off int64
}

func (r *reader) Read(p []byte) (int, error) {
	if r.off >= int64(len(r.b)) {
		return 0, fmt.Errorf("EOF")
	}
	n := copy(p, r.b[r.off:])
	r.off += int64(n)
	return n, nil
}
func (r *reader) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.b)) {
		return 0, fmt.Errorf("EOF")
	}
	n := copy(p, r.b[off:])
	if n < len(p) {
		return n, fmt.Errorf("EOF")
	}
	return n, nil
}
func (r *reader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case 0:
		r.off = off
	case 1:
		r.off += off
	case 2:
		r.off = int64(len(r.b)) + off
	}
	return r.off, nil
}

func init() {
	if len(os.Args) > 1 && os.Args[1] == "-runs" {
		target, _ := os.ReadFile(os.Args[2])
		pred, _ := os.ReadFile(os.Args[3])
		f, _ := elf.NewFile(bytesReader(target))
		secOf := func(off int) string {
			for _, s := range f.Sections {
				if s.Type != elf.SHT_NOBITS && off >= int(s.Offset) && off < int(s.Offset+s.Size) {
					return s.Name
				}
			}
			return "-"
		}
		runs := 0
		for i := 0; i < len(target); {
			if pred[i] == target[i] {
				i++
				continue
			}
			j := i
			for j < len(target) && pred[j] != target[j] {
				j++
			}
			fmt.Printf("%#x +%d %s\n", i, j-i, secOf(i))
			runs++
			i = j
		}
		fmt.Println("runs", runs)
		os.Exit(0)
	}
}

// -corr target pred [sec=from]... : correction cost of pred with sections
// (or "hdr" for bytes outside every section, "text", ...) overlaid from
// another prediction file (or "target" for exact).
func init() {
	if len(os.Args) > 1 && os.Args[1] == "-corr" {
		target, _ := os.ReadFile(os.Args[2])
		pred, _ := os.ReadFile(os.Args[3])
		f, _ := elf.NewFile(bytesReader(target))
		inSec := make([]bool, len(target))
		for _, s := range f.Sections {
			if s.Type != elf.SHT_NOBITS {
				for i := s.Offset; i < s.Offset+s.Size && i < uint64(len(target)); i++ {
					inSec[i] = true
				}
			}
		}
		for _, a := range os.Args[4:] {
			var name, from string
			fmt.Sscanf(a, "%s", &name)
			for i := range a {
				if a[i] == '=' {
					name, from = a[:i], a[i+1:]
				}
			}
			src := target
			if from != "target" {
				src, _ = os.ReadFile(from)
			}
			if name == "hdr" {
				for i := range target {
					if !inSec[i] {
						pred[i] = src[i]
					}
				}
				continue
			}
			for _, s := range f.Sections {
				if s.Name == "."+name || s.Name == name {
					copy(pred[s.Offset:s.Offset+s.Size], src[s.Offset:s.Offset+s.Size])
				}
			}
		}
		plain, near, err := delta.CorrectionShapes(pred, target)
		if err != nil {
			panic(err)
		}
		_, zp := cz.Compress(plain)
		_, zn := cz.Compress(near)
		fmt.Printf("wrong %d  raw plain %d near %d  cz plain %d near %d  best %d\n", diff(pred, target, 0, len(target)), len(plain), len(near), len(zp), len(zn), min(len(zp), len(zn)))
		os.Exit(0)
	}
}
