package gomod

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"fmt"

	"github.com/wjordan/presage/presage"
)

// The linker's FIPS integrity sum (cmd/link, crypto/internal/fips140/check)
// is an HMAC-SHA256 over the module's text and data ranges, recorded in
// .go.fipsinfo for the runtime's power-on self-test. It is a pure function
// of the rest of the binary, so when the encoder verifies the rule holds on
// the target, the 32 bytes ride the plan as one flag and the decoder
// recomputes them (presage.Finaliser).

// fipsSum locates .go.fipsinfo and recomputes the sum the runtime's check
// would. sumOff is the file offset of the recorded sum.
func fipsSum(file []byte) (sumOff int, sum [32]byte, ok bool) {
	f, err := elf.NewFile(bytes.NewReader(file))
	if err != nil {
		return 0, sum, false
	}
	fs := f.Section(".go.fipsinfo")
	if fs == nil || fs.Type == elf.SHT_NOBITS || fs.Size < 120 || fs.Offset+fs.Size > uint64(len(file)) {
		return 0, sum, false
	}
	d := file[fs.Offset : fs.Offset+fs.Size]
	if d[0] != 0xff || string(d[1:16]) != " Go fipsinfo \xff\x00" {
		return 0, sum, false
	}
	toOff := func(a uint64) (uint64, bool) {
		for _, s := range f.Sections {
			if s.Type != elf.SHT_NOBITS && a >= s.Addr && a <= s.Addr+s.Size && s.Offset+s.Size <= uint64(len(file)) {
				return s.Offset + (a - s.Addr), true
			}
		}
		return 0, false
	}
	h := hmac.New(sha256.New, make([]byte, 32))
	h.Write([]byte("go fips object v1\n"))
	var nbuf [8]byte
	for i := 0; i < 4; i++ {
		st := binary.LittleEndian.Uint64(d[56+16*i:])
		en := binary.LittleEndian.Uint64(d[64+16*i:])
		so, ok1 := toOff(st)
		eo, ok2 := toOff(en)
		if !ok1 || !ok2 || en < st || eo-so != en-st {
			return 0, sum, false
		}
		binary.BigEndian.PutUint64(nbuf[:], en-st)
		h.Write(nbuf[:])
		h.Write(file[so:eo])
	}
	copy(sum[:], h.Sum(nil))
	return int(fs.Offset) + 16, sum, true
}

// fipsPart is the plan's fips part: one byte when the target's recorded sum
// is the recomputable one, empty otherwise.
func fipsPart(target []byte) []byte {
	if off, sum, ok := fipsSum(target); ok && bytes.Equal(target[off:off+32], sum[:]) {
		return []byte{1}
	}
	return nil
}

// MaskResidual gives the encoder a prediction whose fips sum is already the
// target's, so the residual does not carry it.
func (m Module) MaskResidual(planb, pred, target []byte) []byte {
	p, err := parsePlan(planb)
	if err != nil || len(p.fips) == 0 {
		return pred
	}
	off, _, ok := fipsSum(target)
	if !ok || off+32 > len(pred) {
		return pred
	}
	masked := append([]byte(nil), pred...)
	copy(masked[off:], target[off:off+32])
	return masked
}

// Finalise recomputes the fips sum over the decoded output.
func (m Module) Finalise(planb, out []byte) error {
	p, err := parsePlan(planb)
	if err != nil {
		return err
	}
	if len(p.fips) == 0 {
		return nil
	}
	off, sum, ok := fipsSum(out)
	if !ok {
		return fmt.Errorf("%w: the plan says to recompute the fips sum, but the output has no valid .go.fipsinfo", presage.ErrCorrupt)
	}
	copy(out[off:], sum[:])
	return nil
}
