// Package symbols reads the function symbols of an image for the encoder.
// Symbols are encoder-only evidence (SPEC decision G7: correspondence is
// shipped, not recovered); no reader keeps names, and nothing here is ever
// needed to apply a patch. See docs/general/elf-module.md §3.4.
package symbols

import (
	"bufio"
	"bytes"
	"debug/elf"
	"debug/gosym"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Func is one function symbol: virtual address, size in bytes, name.
type Func struct {
	Addr, Size uint64
	Name       string
}

// Reader visits every function symbol of one image, in file order.
type Reader interface {
	Funcs(visit func(Func)) error
}

// Open sniffs path: a Breakpad text file ("MODULE " or "FUNC " first) or
// an ELF. The file is opened again on every Funcs call, so an Open'd
// reader holds nothing between visits.
func Open(path string) (Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var head [7]byte
	n, _ := io.ReadFull(f, head[:])
	if isBreakpad(head[:n]) {
		return breakpadFile{path}, nil
	}
	if n < 4 || string(head[:4]) != elf.ELFMAG {
		return nil, fmt.Errorf("%s: not a Breakpad symbol file or an ELF", path)
	}
	return elfFile{path}, nil
}

func isBreakpad(head []byte) bool {
	h := string(head)
	return strings.HasPrefix(h, "MODULE ") || strings.HasPrefix(h, "FUNC ")
}

type breakpadFile struct{ path string }

func (b breakpadFile) Funcs(visit func(Func)) error {
	f, err := os.Open(b.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := FromBreakpad(f).Funcs(visit); err != nil {
		return fmt.Errorf("%s: %w", b.path, err)
	}
	return nil
}

type elfFile struct{ path string }

func (e elfFile) Funcs(visit func(Func)) error {
	f, err := elf.Open(e.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := FromELF(f).Funcs(visit); err != nil {
		return fmt.Errorf("%s: %w", e.path, err)
	}
	return nil
}

// FromBreakpad reads the FUNC records of a Breakpad symbol file:
// "FUNC [m ]address size parameter_size name", hex, addresses relative to
// the module base, which for a position-independent image is the virtual
// address.
func FromBreakpad(r io.Reader) Reader { return breakpadReader{r} }

type breakpadReader struct{ r io.Reader }

func (b breakpadReader) Funcs(visit func(Func)) error {
	sc := bufio.NewScanner(b.r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "FUNC ") {
			continue
		}
		fields := strings.SplitN(strings.TrimPrefix(line[5:], "m "), " ", 4)
		if len(fields) < 4 {
			continue
		}
		addr, err1 := strconv.ParseUint(fields[0], 16, 64)
		size, err2 := strconv.ParseUint(fields[1], 16, 64)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("bad FUNC record: %q", line)
		}
		visit(Func{Addr: addr, Size: size, Name: fields[3]})
	}
	return sc.Err()
}

// FromELF reads STT_FUNC symbols from .symtab, streaming the table rather
// than loading it (a debug-info image's symtab runs to hundreds of
// megabytes); an image without a symtab but with a .gopclntab yields the
// Go runtime's function table instead.
func FromELF(f *elf.File) Reader { return elfReader{f} }

type elfReader struct{ f *elf.File }

func (e elfReader) Funcs(visit func(Func)) error {
	symtab := e.f.Section(".symtab")
	if symtab == nil {
		if e.f.Section(".gopclntab") != nil {
			return pclntabFuncs(e.f, visit)
		}
		return elf.ErrNoSymbols
	}
	if symtab.Type != elf.SHT_SYMTAB {
		return fmt.Errorf(".symtab has type %v", symtab.Type)
	}
	if int(symtab.Link) >= len(e.f.Sections) {
		return errors.New(".symtab links to a missing string table")
	}
	strtab, err := e.f.Sections[symtab.Link].Data()
	if err != nil {
		return err
	}
	name := func(off uint32) string {
		if uint64(off) >= uint64(len(strtab)) {
			return ""
		}
		s := strtab[off:]
		if i := bytes.IndexByte(s, 0); i >= 0 {
			s = s[:i]
		}
		return string(s)
	}
	r := bufio.NewReaderSize(symtab.Open(), 1<<20)
	order := e.f.ByteOrder
	var ent int
	switch e.f.Class {
	case elf.ELFCLASS64:
		ent = elf.Sym64Size
	case elf.ELFCLASS32:
		ent = elf.Sym32Size
	default:
		return fmt.Errorf("unknown ELF class %v", e.f.Class)
	}
	buf := make([]byte, ent)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var f Func
		var info byte
		if ent == elf.Sym64Size {
			info = buf[4]
			f = Func{Addr: order.Uint64(buf[8:]), Size: order.Uint64(buf[16:])}
		} else {
			info = buf[12]
			f = Func{Addr: uint64(order.Uint32(buf[4:])), Size: uint64(order.Uint32(buf[8:]))}
		}
		if elf.ST_TYPE(info) == elf.STT_FUNC {
			f.Name = name(order.Uint32(buf[0:]))
			visit(f)
		}
	}
}

func pclntabFuncs(f *elf.File, visit func(Func)) error {
	text := f.Section(".text")
	if text == nil {
		return errors.New("no .text")
	}
	pcln, err := f.Section(".gopclntab").Data()
	if err != nil {
		return err
	}
	tab, err := gosym.NewTable(nil, gosym.NewLineTable(pcln, text.Addr))
	if err != nil {
		return err
	}
	for _, fn := range tab.Funcs {
		visit(Func{Addr: fn.Entry, Size: fn.End - fn.Entry, Name: fn.Name})
	}
	return nil
}
