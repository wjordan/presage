package symbols

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func collect(t *testing.T, r Reader) []Func {
	t.Helper()
	var out []Func
	if err := r.Funcs(func(f Func) { out = append(out, f) }); err != nil {
		t.Fatal(err)
	}
	return out
}

const breakpad = "MODULE Linux x86_64 ABCDEF0 libxul.so\nFILE 0 a.cc\nFUNC 1000 20 0 foo(int)\nFUNC m 1040 8 0 bar\nPUBLIC 2000 0 baz\nFUNC zz 1 0 bad\n"

func TestBreakpad(t *testing.T) {
	t.Parallel()
	good := strings.Replace(breakpad, "FUNC zz 1 0 bad\n", "", 1)
	fs := collect(t, FromBreakpad(strings.NewReader(good)))
	want := []Func{{0x1000, 0x20, "foo(int)"}, {0x1040, 8, "bar"}}
	if len(fs) != 2 || fs[0] != want[0] || fs[1] != want[1] {
		t.Fatalf("got %+v", fs)
	}
	if err := FromBreakpad(strings.NewReader(breakpad)).Funcs(func(Func) {}); err == nil {
		t.Fatal("bad record accepted")
	}
}

// tinyELF builds a 64-bit ELF with a .symtab of two functions and a
// .strtab; sections only, no program headers.
func tinyELF() []byte {
	strtab := []byte("\x00main\x00helper\x00.symtab\x00.strtab\x00.shstrtab\x00")
	sym := func(name uint32, info byte, value, size uint64) []byte {
		var b [24]byte
		binary.LittleEndian.PutUint32(b[0:], name)
		b[4] = info
		binary.LittleEndian.PutUint16(b[6:], 1)
		binary.LittleEndian.PutUint64(b[8:], value)
		binary.LittleEndian.PutUint64(b[16:], size)
		return b[:]
	}
	var symtab []byte
	symtab = append(symtab, sym(0, 0, 0, 0)...)
	symtab = append(symtab, sym(1, byte(elf.STB_GLOBAL)<<4|byte(elf.STT_FUNC), 0x401000, 0x30)...)
	symtab = append(symtab, sym(6, byte(elf.STB_LOCAL)<<4|byte(elf.STT_OBJECT), 0x500000, 8)...)
	symtab = append(symtab, sym(6, byte(elf.STB_LOCAL)<<4|byte(elf.STT_FUNC), 0x401030, 0x10)...)

	hdrLen := 64
	symOff := hdrLen
	strOff := symOff + len(symtab)
	shOff := strOff + len(strtab)
	var out bytes.Buffer
	eh := make([]byte, hdrLen)
	copy(eh, elf.ELFMAG)
	eh[4], eh[5], eh[6] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), 1
	binary.LittleEndian.PutUint16(eh[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(eh[18:], uint16(elf.EM_X86_64))
	binary.LittleEndian.PutUint32(eh[20:], 1)
	binary.LittleEndian.PutUint64(eh[40:], uint64(shOff))
	binary.LittleEndian.PutUint16(eh[52:], 64)
	binary.LittleEndian.PutUint16(eh[58:], 64)
	binary.LittleEndian.PutUint16(eh[60:], 4)
	binary.LittleEndian.PutUint16(eh[62:], 3) // shstrndx: reuse strtab
	out.Write(eh)
	out.Write(symtab)
	out.Write(strtab)
	sh := func(name uint32, typ elf.SectionType, off, size uint64, link uint32, entsize uint64) {
		var b [64]byte
		binary.LittleEndian.PutUint32(b[0:], name)
		binary.LittleEndian.PutUint32(b[4:], uint32(typ))
		binary.LittleEndian.PutUint64(b[24:], off)
		binary.LittleEndian.PutUint64(b[32:], size)
		binary.LittleEndian.PutUint32(b[40:], link)
		binary.LittleEndian.PutUint64(b[56:], entsize)
		out.Write(b[:])
	}
	sh(0, elf.SHT_NULL, 0, 0, 0, 0)
	sh(13, elf.SHT_SYMTAB, uint64(symOff), uint64(len(symtab)), 2, 24)
	sh(21, elf.SHT_STRTAB, uint64(strOff), uint64(len(strtab)), 0, 0)
	sh(29, elf.SHT_STRTAB, uint64(strOff), uint64(len(strtab)), 0, 0)
	return out.Bytes()
}

func TestELF(t *testing.T) {
	t.Parallel()
	f, err := elf.NewFile(bytes.NewReader(tinyELF()))
	if err != nil {
		t.Fatal(err)
	}
	fs := collect(t, FromELF(f))
	want := []Func{{0x401000, 0x30, "main"}, {0x401030, 0x10, "helper"}}
	if len(fs) != 2 || fs[0] != want[0] || fs[1] != want[1] {
		t.Fatalf("got %+v", fs)
	}
}

func TestOpenSniffs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bp := filepath.Join(dir, "x.sym")
	el := filepath.Join(dir, "x.debug")
	os.WriteFile(bp, []byte(strings.Replace(breakpad, "FUNC zz 1 0 bad\n", "", 1)), 0o644)
	os.WriteFile(el, tinyELF(), 0o644)
	for _, p := range []string{bp, el} {
		r, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(collect(t, r)); n != 2 {
			t.Fatalf("%s: %d funcs", p, n)
		}
	}
	os.WriteFile(filepath.Join(dir, "junk"), []byte("hello"), 0o644)
	if _, err := Open(filepath.Join(dir, "junk")); err == nil {
		t.Fatal("junk accepted")
	}
}
