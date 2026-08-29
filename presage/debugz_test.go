package presage

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"
)

// debugFixture is a minimal ELF64 with one SHF_COMPRESSED section holding
// payload, so that the debugz path — coding the expanded file, whose
// regions are larger than the shipped one — is exercised end to end.
func debugFixture(payload []byte) []byte {
	le := binary.LittleEndian
	var z bytes.Buffer
	z.Write(make([]byte, 24))
	w, _ := zlib.NewWriterLevel(&z, zlib.BestSpeed)
	w.Write(payload)
	w.Close()
	comp := z.Bytes()
	le.PutUint32(comp, 1)
	le.PutUint64(comp[8:], uint64(len(payload)))
	le.PutUint64(comp[16:], 8)
	bodies := [][]byte{[]byte("text"), comp, []byte("strtab\x00")}
	flags := []uint64{6, 0x800, 0}
	out := make([]byte, 64)
	copy(out, "\x7fELF\x02\x01\x01")
	le.PutUint16(out[58:], 64)
	le.PutUint16(out[60:], uint16(len(bodies)+1))
	offs := make([]uint64, len(bodies))
	for i, b := range bodies {
		offs[i] = uint64(len(out))
		out = append(out, b...)
	}
	shoff := uint64(len(out))
	out = append(out, make([]byte, (len(bodies)+1)*64)...)
	for i, b := range bodies {
		h := out[shoff+uint64(i+1)*64:]
		le.PutUint32(h, uint32(i+1))
		le.PutUint32(h[4:], 1)
		le.PutUint64(h[8:], flags[i])
		le.PutUint64(h[24:], offs[i])
		le.PutUint64(h[32:], uint64(len(b)))
		le.PutUint64(h[48:], 1)
	}
	le.PutUint64(out[40:], shoff)
	return out
}

func TestDebugZRoundTrip(t *testing.T) {
	oldPayload := bytes.Repeat([]byte("debug info for release one\n"), 300)
	newPayload := bytes.Replace(oldPayload, []byte("one"), []byte("two"), 7)
	old, new := debugFixture(oldPayload), debugFixture(newPayload)
	var st Stats
	patch := roundTrip(t, [][]byte{old}, new, Options{Stats: &st})
	h, err := ParseHeader(patch)
	if err != nil {
		t.Fatal(err)
	}
	if h.Flags&FlagDebugZ == 0 || h.Size != int64(len(new)) || h.Regions[0].Length <= h.Size {
		t.Fatalf("flags %#x size %d region %d: want debugz with the region covering the expanded file", h.Flags, h.Size, h.Regions[0].Length)
	}
}
