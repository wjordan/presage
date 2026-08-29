package delta

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// buildELF assembles a minimal ELF64 with the given section bodies laid out
// in order (a 3-byte gap between them) and the section header table before
// or after the bodies.
func buildELF(bodies [][]byte, flags, aligns []uint64, shdrFirst bool) []byte {
	le := binary.LittleEndian
	n := len(bodies) + 1
	out := make([]byte, 64)
	copy(out, "\x7fELF\x02\x01\x01")
	le.PutUint16(out[58:], 64)
	le.PutUint16(out[60:], uint16(n))
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
	tbl := out[shoff+64:]
	for i, body := range bodies {
		h := tbl[i*64:]
		le.PutUint32(h, uint32(i+1))
		le.PutUint32(h[4:], 1)
		le.PutUint64(h[8:], flags[i])
		le.PutUint64(h[24:], offs[i])
		le.PutUint64(h[32:], uint64(len(body)))
		le.PutUint64(h[48:], aligns[i])
	}
	le.PutUint64(out[40:], shoff)
	return out
}

func compressedSection(payload []byte, align uint64) []byte {
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

func debugFixture(payload []byte, shdrFirst bool) []byte {
	bodies := [][]byte{[]byte("text"), compressedSection(payload, 8), []byte("strtab\x00")}
	return buildELF(bodies, []uint64{6, shfCompressed, 0}, []uint64{16, 1, 1}, shdrFirst)
}

func TestExpandPackDebug(t *testing.T) {
	payload := bytes.Repeat([]byte("debug info "), 200)
	for _, shdrFirst := range []bool{false, true} {
		f := debugFixture(payload, shdrFirst)
		p, err := ExpandDebug(f)
		if err != nil {
			t.Fatal(err)
		}
		pf, err := parseSections(p)
		if err != nil {
			t.Fatal(err)
		}
		s := pf.sh[2]
		if s.flags&shfCompressed != 0 || s.align != 8 || !bytes.Equal(p[s.off:s.off+s.sz], payload) {
			t.Fatalf("shdrFirst=%v: expanded section wrong: flags %x align %d size %d", shdrFirst, s.flags, s.align, s.sz)
		}
		if next := pf.sh[3]; string(p[next.off-3:next.off]) != "gap" || string(p[next.off:next.off+next.sz]) != "strtab\x00" {
			t.Fatalf("shdrFirst=%v: following section or gap moved wrongly", shdrFirst)
		}
		back, err := PackDebug(p, f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(back, f) {
			t.Fatalf("shdrFirst=%v: pack(expand(f)) != f", shdrFirst)
		}
	}
}

func TestExpandDebugIdentityWithoutCompressedSections(t *testing.T) {
	f := buildELF([][]byte{[]byte("text"), []byte("data")}, []uint64{6, 3}, []uint64{16, 8}, false)
	p, err := ExpandDebug(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p, f) {
		t.Fatal("ExpandDebug changed a file with no compressed sections")
	}
}

// A patch between two files with compressed debug sections is coded on the
// plaintext (FlagDebugZ) and applies back to the shipped bytes.
func TestEncodeApplyDebugZ(t *testing.T) {
	oldPayload := bytes.Repeat([]byte("debug info for release one\n"), 300)
	newPayload := bytes.Replace(oldPayload, []byte("one"), []byte("two"), 7)
	old, new := debugFixture(oldPayload, false), debugFixture(newPayload, false)
	var st Stats
	patch, err := Encode(old, new, Options{PlainOnly: true, Stats: &st})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 || !strings.HasPrefix(st.Notes[0], "compressed debug sections expanded") {
		t.Fatalf("notes %q, want the expansion recorded", st.Notes)
	}
	h, err := ParseHeader(patch)
	if err != nil {
		t.Fatal(err)
	}
	if h.Flags&FlagDebugZ == 0 {
		t.Fatal("FlagDebugZ not set on a compressed-debug pair")
	}
	if h.OldSize != int64(len(old)) || h.NewSize != int64(len(new)) {
		t.Fatalf("header sizes %d/%d describe the expanded files, want the shipped %d/%d", h.OldSize, h.NewSize, len(old), len(new))
	}
	var out bytes.Buffer
	if err := Apply(old, patch, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), new) {
		t.Fatal("Apply did not reproduce the shipped new file")
	}
}

func TestUnknownHeaderFlagIsUnsupported(t *testing.T) {
	old, new := []byte("old bytes"), []byte("new bytes")
	patch, err := Encode(old, new, Options{PlainOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	patch[5] |= 0x80
	var ut *ErrUnsupportedTransform
	if err := Apply(old, patch, &bytes.Buffer{}); !errors.As(err, &ut) || ut.Flags != 0x80 {
		t.Fatalf("got %v, want ErrUnsupportedTransform with flags 0x80", err)
	}
}
