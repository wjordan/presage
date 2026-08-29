package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"
)

// buildELF assembles a minimal ELF64 with the given section bodies laid out
// in order (a 3-byte gap between them) and the section header table before
// or after the bodies.
func buildELF(t *testing.T, bodies [][]byte, flags []uint64, aligns []uint64, shdrFirst bool) []byte {
	t.Helper()
	le := binary.LittleEndian
	n := len(bodies) + 1
	hdr := make([]byte, 64)
	copy(hdr, "\x7fELF\x02\x01\x01")
	le.PutUint16(hdr[60:], uint16(n))
	tbl := func(offs []uint64) []byte {
		var b []byte
		b = append(b, make([]byte, 64)...) // SHT_NULL
		for i, body := range bodies {
			s := shdr{name: uint32(i + 1), typ: 1, flags: flags[i], off: offs[i], sz: uint64(len(body)), align: aligns[i]}
			b = le.AppendUint32(b, s.name)
			b = le.AppendUint32(b, s.typ)
			for _, v := range []uint64{s.flags, s.addr, s.off, s.sz} {
				b = le.AppendUint64(b, v)
			}
			b = le.AppendUint32(b, s.link)
			b = le.AppendUint32(b, s.info)
			b = le.AppendUint64(b, s.align)
			b = le.AppendUint64(b, s.entsize)
		}
		return b
	}
	out := hdr
	var shoff uint64
	if shdrFirst {
		shoff = uint64(len(out))
		out = append(out, make([]byte, n*64)...)
	}
	offs := make([]uint64, len(bodies))
	for i, body := range bodies {
		out = append(out, 'g', 'a', 'p')
		offs[i] = uint64(len(out))
		out = append(out, body...)
	}
	if !shdrFirst {
		out = append(out, 0, 0)
		shoff = uint64(len(out))
		out = append(out, make([]byte, n*64)...)
	}
	copy(out[shoff:], tbl(offs))
	le.PutUint64(out[40:], shoff)
	return out
}

func compressed(t *testing.T, payload []byte, align uint64) []byte {
	t.Helper()
	var b bytes.Buffer
	b.Write(make([]byte, 24))
	w, _ := zlib.NewWriterLevel(&b, zlib.BestSpeed)
	w.Write(payload)
	w.Close()
	h := b.Bytes()
	binary.LittleEndian.PutUint32(h, 1)
	binary.LittleEndian.PutUint64(h[8:], uint64(len(payload)))
	binary.LittleEndian.PutUint64(h[16:], align)
	return h
}

func TestRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("debug info "), 200)
	for _, shdrFirst := range []bool{false, true} {
		bodies := [][]byte{[]byte("text"), compressed(t, payload, 8), []byte("strtab\x00")}
		f := buildELF(t, bodies, []uint64{6, shfCompressed, 0}, []uint64{16, 1, 1}, shdrFirst)
		p, err := plain(f)
		if err != nil {
			t.Fatal(err)
		}
		pf, err := parse(p)
		if err != nil {
			t.Fatal(err)
		}
		s := pf.sh[2]
		if s.flags&shfCompressed != 0 || s.align != 8 || !bytes.Equal(p[s.off:s.off+s.sz], payload) {
			t.Fatalf("shdrFirst=%v: plain section wrong: flags %x align %d size %d", shdrFirst, s.flags, s.align, s.sz)
		}
		if next := pf.sh[3]; string(p[next.off-3:next.off]) != "gap" || string(p[next.off:next.off+next.sz]) != "strtab\x00" {
			t.Fatalf("shdrFirst=%v: following section or gap moved wrongly", shdrFirst)
		}
		back, err := pack(p, f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(back, f) {
			t.Fatalf("shdrFirst=%v: pack(plain(f)) != f", shdrFirst)
		}
	}
}

func TestPlainIsIdentityWithoutCompressedSections(t *testing.T) {
	f := buildELF(t, [][]byte{[]byte("text"), []byte("data")}, []uint64{6, 3}, []uint64{16, 8}, false)
	p, err := plain(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p, f) {
		t.Fatal("plain changed a file with no compressed sections")
	}
}
