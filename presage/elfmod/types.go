package elfmod

import (
	"encoding/binary"
	"errors"
	"slices"

	"github.com/wjordan/presage/delta/x86"
)

// section is one allocated section of an image.
type section struct {
	Addr   uint64
	Off    uint64
	Size   uint64
	NoBits bool // occupies no file bytes
}

// image is one ELF file and the geometry the module reads from it.
type image struct {
	Data     []byte
	Text     section
	Sections map[string]section
	// Debug holds the non-allocated sections with file contents.
	Debug map[string]section
}

func (im *image) textBytes() []byte {
	return im.Data[im.Text.Off : im.Text.Off+im.Text.Size]
}

// mapping is one matched function: where its old body is, where the new
// one goes, and whether the old bytes are copied.
type mapping struct {
	Src     uint64
	SrcSize uint64
	Dst     uint64
	DstSize uint64
	Copy    bool
}

// addressRange is a piecewise-constant shift: [Old, Old+Size) lands at New.
type addressRange struct {
	Old  uint64
	New  uint64
	Size uint64
}

// addressPoint is one exact old→new address correspondence.
type addressPoint struct {
	Old uint64
	New uint64
}

// equivalence copies N bytes of the old image at Src to Dst in the new one.
type equivalence struct {
	Src uint64
	Dst uint64
	N   uint64
}

// ImageOracle answers where an old address lands in the new image, for an
// instruction displacement: the byte-level projection wins inside .text.
type ImageOracle = func(uint64) x86.Target

// PointerOracle answers the same question for an absolute pointer, where
// identity evidence (points, function map) wins over byte projection.
type PointerOracle = func(uint64) x86.Target

// sectionMap translates between image addresses and file offsets. Old holds
// the address, New holds the file offset.
type sectionMap []addressRange

func newSectionMap(secs map[string]section) sectionMap {
	m := make(sectionMap, 0, len(secs))
	for _, s := range secs {
		if s.NoBits {
			// No file bytes: .bss and .noptrbss share an offset, and an
			// address in one would project into the other.
			continue
		}
		m = append(m, addressRange{Old: s.Addr, New: s.Off, Size: s.Size})
	}
	slices.SortFunc(m, func(a, b addressRange) int { return cmpU(a.Old, b.Old) })
	return m
}

func (m sectionMap) offsetOf(addr uint64) (uint64, bool) {
	i, ok := slices.BinarySearchFunc(m, addr, func(r addressRange, addr uint64) int {
		if r.Old > addr {
			return 1
		}
		if r.Old+r.Size <= addr {
			return -1
		}
		return 0
	})
	if !ok {
		return 0, false
	}
	return m[i].New + addr - m[i].Old, true
}

func (m sectionMap) addrOf(off uint64) (uint64, bool) {
	for _, r := range m {
		if off >= r.New && off < r.New+r.Size {
			return r.Old + off - r.New, true
		}
	}
	return 0, false
}

func (m sectionMap) marshal(b []byte) []byte {
	b = appendU(b, uint64(len(m)))
	var prevAddr, prevOff uint64
	for _, r := range m {
		b = appendU(b, r.Old-prevAddr)
		if r.New >= prevOff {
			b = appendS(b, int64(r.New-prevOff))
		} else {
			b = appendS(b, -int64(prevOff-r.New))
		}
		b = appendU(b, r.Size)
		prevAddr, prevOff = r.Old, r.New
	}
	return b
}

func unmarshalSectionMap(r *planReader) (sectionMap, error) {
	n := r.u()
	if r.err != nil || n > 1<<20 {
		return nil, errors.New("implausible section map")
	}
	m := make(sectionMap, 0, n)
	var prevAddr, prevOff uint64
	for i := uint64(0); i < n; i++ {
		addrDelta, offDelta, size := r.u(), r.s(), r.u()
		if r.err != nil {
			return nil, errors.New("invalid section map entry")
		}
		addr := prevAddr + addrDelta
		var off uint64
		if offDelta >= 0 {
			off = prevOff + uint64(offDelta)
		} else {
			d := uint64(-(offDelta + 1)) + 1
			if d > prevOff {
				return nil, errors.New("section map offset underflows")
			}
			off = prevOff - d
		}
		m = append(m, addressRange{Old: addr, New: off, Size: size})
		prevAddr, prevOff = addr, off
	}
	if !slices.IsSortedFunc(m, func(a, b addressRange) int { return cmpU(a.Old, b.Old) }) {
		return nil, errors.New("section map is not sorted by address")
	}
	return m, nil
}

func appendU(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }
func appendS(b []byte, v int64) []byte  { return binary.AppendVarint(b, v) }

func appendStream(b, stream []byte) []byte {
	b = appendU(b, uint64(len(stream)))
	return append(b, stream...)
}

// planReader reads the varint columns of a plan; the first error sticks.
type planReader struct {
	b   []byte
	err error
}

func (r *planReader) u() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = errors.New("invalid unsigned integer in plan")
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *planReader) s() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.b)
	if n <= 0 {
		r.err = errors.New("invalid signed integer in plan")
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *planReader) stream() *planReader {
	n := r.u()
	if r.err != nil || n > uint64(len(r.b)) {
		r.err = errors.New("invalid typed stream length in plan")
		return &planReader{err: r.err}
	}
	s := &planReader{b: r.b[:n]}
	r.b = r.b[n:]
	return s
}

func (r *planReader) done() bool { return r.err == nil && len(r.b) == 0 }

func (r *planReader) byteAt() byte {
	if r.err != nil || len(r.b) == 0 {
		r.err = errors.New("plan truncated")
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func cmpU(a, b uint64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
