// dwarfz is the transparent-decompression transform for SHF_COMPRESSED
// sections (SPEC §4.5 (a)), as a pair of inverse commands:
//
//	dwarfz plain <elf> <out>        expand every compressed section in place
//	dwarfz pack <plain> <ref> <out> recompress the sections <ref> compresses
//	dwarfz verify <elf>...          check pack(plain(f)) == f
//
// plain rewrites each compressed section as its payload, moving the
// sections after it and the section header table by the growth and
// keeping every inter-section gap; pack inverts it with Go's zlib at level
// 1, which is what the Go linker uses. The decoder needs no plan: the set
// of sections to compress is read from the old file it already holds.
package main

import (
	"bytes"
	"compress/zlib"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

const shfCompressed = 0x800

type shdr struct {
	name, typ            uint32
	flags, addr, off, sz uint64
	link, info           uint32
	align, entsize       uint64
}

type file struct {
	b     []byte
	shoff uint64
	sh    []shdr
}

func parse(b []byte) (*file, error) {
	le := binary.LittleEndian
	if len(b) < 64 || string(b[:4]) != elf.ELFMAG || b[4] != byte(elf.ELFCLASS64) || b[5] != byte(elf.ELFDATA2LSB) {
		return nil, errors.New("not a little-endian ELF64 file")
	}
	f := &file{b: b, shoff: le.Uint64(b[40:])}
	shnum := int(le.Uint16(b[60:]))
	if f.shoff+uint64(shnum)*64 > uint64(len(b)) {
		return nil, errors.New("section header table exceeds the file")
	}
	for i := range shnum {
		h := b[f.shoff+uint64(i)*64:]
		f.sh = append(f.sh, shdr{le.Uint32(h), le.Uint32(h[4:]), le.Uint64(h[8:]), le.Uint64(h[16:]),
			le.Uint64(h[24:]), le.Uint64(h[32:]), le.Uint32(h[40:]), le.Uint32(h[44:]), le.Uint64(h[48:]), le.Uint64(h[56:])})
	}
	return f, nil
}

// relayout rewrites the file with each section's bytes replaced by
// replace(i) (nil keeps them), preserving the gaps between consecutive
// sections in file order. The section header table may sit anywhere; if
// it follows the sections it moves with them.
func (f *file) relayout(replace func(i int) ([]byte, shdr, error)) ([]byte, error) {
	le := binary.LittleEndian
	var order []int
	for i, s := range f.sh {
		if s.typ != uint32(elf.SHT_NOBITS) && s.typ != uint32(elf.SHT_NULL) {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return f.sh[order[a]].off < f.sh[order[b]].off })
	shdrEnd := f.shoff + uint64(len(f.sh))*64
	shdrAfter := len(order) > 0 && f.shoff >= f.sh[order[len(order)-1]].off
	if len(order) > 0 && !shdrAfter && shdrEnd > f.sh[order[0]].off {
		return nil, errors.New("section header table overlaps the sections")
	}
	first := uint64(len(f.b))
	if len(order) > 0 {
		first = f.sh[order[0]].off
	}
	out := append([]byte(nil), f.b[:first]...)
	sh := append([]shdr(nil), f.sh...)
	prevEnd := first
	for _, i := range order {
		s := f.sh[i]
		if s.off < prevEnd {
			return nil, fmt.Errorf("section %d overlaps its predecessor", i)
		}
		out = append(out, f.b[prevEnd:s.off]...) // the gap, verbatim
		body, h, err := replace(i)
		if err != nil {
			return nil, fmt.Errorf("section %d: %w", i, err)
		}
		if body == nil {
			body, h = f.b[s.off:s.off+s.sz], s
		}
		h.off = uint64(len(out))
		h.sz = uint64(len(body))
		out = append(out, body...)
		sh[i] = h
		prevEnd = s.off + s.sz
	}
	shoff := f.shoff
	if shdrAfter {
		out = append(out, f.b[prevEnd:f.shoff]...)
		shoff = uint64(len(out))
	} else {
		out = append(out, f.b[prevEnd:]...)
	}
	var tbl []byte
	for _, h := range sh {
		tbl = le.AppendUint32(tbl, h.name)
		tbl = le.AppendUint32(tbl, h.typ)
		for _, v := range []uint64{h.flags, h.addr, h.off, h.sz} {
			tbl = le.AppendUint64(tbl, v)
		}
		tbl = le.AppendUint32(tbl, h.link)
		tbl = le.AppendUint32(tbl, h.info)
		tbl = le.AppendUint64(tbl, h.align)
		tbl = le.AppendUint64(tbl, h.entsize)
	}
	if shdrAfter {
		out = append(out, tbl...)
		out = append(out, f.b[shdrEnd:]...)
	} else {
		copy(out[shoff:], tbl)
	}
	le.PutUint64(out[40:], shoff)
	return out, nil
}

func plain(b []byte) ([]byte, error) {
	f, err := parse(b)
	if err != nil {
		return nil, err
	}
	return f.relayout(func(i int) ([]byte, shdr, error) {
		s := f.sh[i]
		if s.flags&shfCompressed == 0 {
			return nil, s, nil
		}
		body := b[s.off : s.off+s.sz]
		if len(body) < 24 || binary.LittleEndian.Uint32(body) != 1 {
			return nil, s, errors.New("unsupported compression header")
		}
		size, align := binary.LittleEndian.Uint64(body[8:]), binary.LittleEndian.Uint64(body[16:])
		r, err := zlib.NewReader(bytes.NewReader(body[24:]))
		if err != nil {
			return nil, s, err
		}
		payload, err := io.ReadAll(r)
		if err != nil {
			return nil, s, err
		}
		if uint64(len(payload)) != size {
			return nil, s, fmt.Errorf("payload %d bytes, header says %d", len(payload), size)
		}
		s.flags &^= shfCompressed
		s.align = align
		return payload, s, nil
	})
}

// pack compresses, in the plain file, every section index the reference
// file compresses.
func pack(b, ref []byte) ([]byte, error) {
	f, err := parse(b)
	if err != nil {
		return nil, err
	}
	rf, err := parse(ref)
	if err != nil {
		return nil, err
	}
	if len(rf.sh) != len(f.sh) {
		return nil, errors.New("reference has a different section count")
	}
	return f.relayout(func(i int) ([]byte, shdr, error) {
		s := f.sh[i]
		if rf.sh[i].flags&shfCompressed == 0 {
			return nil, s, nil
		}
		payload := b[s.off : s.off+s.sz]
		var body bytes.Buffer
		body.Write(make([]byte, 24))
		w, err := zlib.NewWriterLevel(&body, zlib.BestSpeed)
		if err == nil {
			_, err = w.Write(payload)
		}
		if err == nil {
			err = w.Close()
		}
		if err != nil {
			return nil, s, err
		}
		h := body.Bytes()
		binary.LittleEndian.PutUint32(h, 1)
		binary.LittleEndian.PutUint64(h[8:], uint64(len(payload)))
		binary.LittleEndian.PutUint64(h[16:], s.align)
		s.flags |= shfCompressed
		s.align = 1
		return h, s, nil
	})
}

func main() {
	args := os.Args[1:]
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "dwarfz:", err)
		os.Exit(1)
	}
	read := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			fail(err)
		}
		return b
	}
	switch {
	case len(args) == 3 && args[0] == "plain":
		b := read(args[1])
		out, err := plain(b)
		if err != nil {
			fail(err)
		}
		if err := os.WriteFile(args[2], out, 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("%d -> %d bytes\n", len(b), len(out))
	case len(args) == 4 && args[0] == "pack":
		b := read(args[1])
		out, err := pack(b, read(args[2]))
		if err != nil {
			fail(err)
		}
		if err := os.WriteFile(args[3], out, 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("%d -> %d bytes\n", len(b), len(out))
	case len(args) >= 2 && args[0] == "verify":
		for _, path := range args[1:] {
			b := read(path)
			p, err := plain(b)
			if err != nil {
				fail(fmt.Errorf("%s: %w", path, err))
			}
			back, err := pack(p, b)
			if err != nil {
				fail(fmt.Errorf("%s: %w", path, err))
			}
			if !bytes.Equal(back, b) {
				fail(fmt.Errorf("%s: pack(plain(f)) differs from f", path))
			}
			fmt.Printf("%s: %d bytes, plain %d, round trip exact\n", path, len(b), len(p))
		}
	default:
		fail(errors.New("usage: dwarfz plain <elf> <out> | pack <plain> <ref> <out> | verify <elf>..."))
	}
}
