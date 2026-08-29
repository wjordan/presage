package delta

import (
	"bytes"
	"compress/zlib"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Transparent decompression of SHF_COMPRESSED sections (docs/general/SPEC.md
// 4.5 (a)). A default `go build` keeps DWARF as zlib streams that differ
// throughout after any change, so the codec works on the plaintext: the
// encoder expands both files, codes the plaintext pair and sets FlagDebugZ;
// the decoder expands the old file, decodes, and recompresses the sections
// the old file compressed. Go's zlib at BestSpeed is what the linker used,
// so the recompression is exact and the transform ships nothing; the
// encoder checks that on the new file before it commits to the flag.

// FlagDebugZ in Header.Flags marks a patch coded on the expanded files.
const FlagDebugZ = 1

const shfCompressed = 0x800

type elfShdr struct {
	name, typ            uint32
	flags, addr, off, sz uint64
	link, info           uint32
	align, entsize       uint64
}

type elfSections struct {
	b     []byte
	shoff uint64
	sh    []elfShdr
}

func parseSections(b []byte) (*elfSections, error) {
	le := binary.LittleEndian
	if len(b) < 64 || string(b[:4]) != elf.ELFMAG || b[4] != byte(elf.ELFCLASS64) || b[5] != byte(elf.ELFDATA2LSB) {
		return nil, errors.New("not a little-endian ELF64 file")
	}
	f := &elfSections{b: b, shoff: le.Uint64(b[40:])}
	shnum := uint64(le.Uint16(b[60:]))
	if le.Uint16(b[58:]) != 64 || f.shoff > uint64(len(b)) || shnum*64 > uint64(len(b))-f.shoff {
		return nil, errors.New("section header table exceeds the file")
	}
	for i := range shnum {
		h := b[f.shoff+i*64:]
		s := elfShdr{le.Uint32(h), le.Uint32(h[4:]), le.Uint64(h[8:]), le.Uint64(h[16:]),
			le.Uint64(h[24:]), le.Uint64(h[32:]), le.Uint32(h[40:]), le.Uint32(h[44:]), le.Uint64(h[48:]), le.Uint64(h[56:])}
		if s.typ != uint32(elf.SHT_NOBITS) && (s.off > uint64(len(b)) || s.sz > uint64(len(b))-s.off) {
			return nil, fmt.Errorf("section %d exceeds the file", i)
		}
		f.sh = append(f.sh, s)
	}
	return f, nil
}

// hasCompressed reports whether any section carries SHF_COMPRESSED.
func (f *elfSections) hasCompressed() bool {
	for _, s := range f.sh {
		if s.flags&shfCompressed != 0 {
			return true
		}
	}
	return false
}

// relayout rewrites the file with each section's bytes replaced by
// replace(i) (nil keeps them), preserving the gaps between consecutive
// sections in file order. The section header table may sit anywhere; if
// it follows the sections it moves with them.
func (f *elfSections) relayout(replace func(i int) ([]byte, elfShdr, error)) ([]byte, error) {
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
	sh := append([]elfShdr(nil), f.sh...)
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

// ExpandDebug replaces every SHF_COMPRESSED section of an ELF64 file by
// its payload, moving what follows. It returns b itself when the file has
// no compressed section, and an error for a file it cannot expand — the
// caller then codes the file as it is.
func ExpandDebug(b []byte) ([]byte, error) {
	f, err := parseSections(b)
	if err != nil {
		return nil, err
	}
	if !f.hasCompressed() {
		return b, nil
	}
	return f.relayout(func(i int) ([]byte, elfShdr, error) {
		s := f.sh[i]
		if s.flags&shfCompressed == 0 {
			return nil, s, nil
		}
		body := b[s.off : s.off+s.sz]
		if len(body) < 24 || binary.LittleEndian.Uint32(body) != 1 {
			return nil, s, errors.New("unsupported compression header")
		}
		size, align := binary.LittleEndian.Uint64(body[8:]), binary.LittleEndian.Uint64(body[16:])
		if size > uint64(len(b))*64 {
			return nil, s, errors.New("implausible uncompressed size")
		}
		r, err := zlib.NewReader(bytes.NewReader(body[24:]))
		if err != nil {
			return nil, s, err
		}
		payload, err := io.ReadAll(io.LimitReader(r, int64(size)+1))
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

// PackDebug is the inverse of ExpandDebug: it compresses, in the expanded
// file, every section that the reference file compresses. The reference is
// the old file, which the decoder holds, so the set costs no patch bytes.
func PackDebug(plain, ref []byte) ([]byte, error) {
	f, err := parseSections(plain)
	if err != nil {
		return nil, err
	}
	rf, err := parseSections(ref)
	if err != nil {
		return nil, err
	}
	if len(rf.sh) != len(f.sh) {
		return nil, errors.New("reference has a different section count")
	}
	if !rf.hasCompressed() {
		return plain, nil
	}
	return f.relayout(func(i int) ([]byte, elfShdr, error) {
		s := f.sh[i]
		if rf.sh[i].flags&shfCompressed == 0 {
			return nil, s, nil
		}
		payload := plain[s.off : s.off+s.sz]
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

// expandPair expands old and new for the encoder. It commits to the
// transform only when the new file rebuilds exactly from its expansion
// with the old file as reference, which is what the decoder will do;
// otherwise it returns the files unchanged and ok=false.
func expandPair(old, new []byte) (pOld, pNew []byte, ok bool) {
	fo, err := parseSections(old)
	if err != nil || !fo.hasCompressed() {
		return old, new, false
	}
	pOld, err = ExpandDebug(old)
	if err != nil {
		return old, new, false
	}
	pNew, err = ExpandDebug(new)
	if err != nil {
		return old, new, false
	}
	back, err := PackDebug(pNew, old)
	if err != nil || !bytes.Equal(back, new) {
		return old, new, false
	}
	return pOld, pNew, true
}
