package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/wjordan/go-binsync/delta/x86"
)

// .eh_frame is the same shape of problem as .rela.dyn: 98.211% of the new
// table's FDE bodies exist somewhere in the old table, but only 2 of 31,477
// FDEs keep their target address, so every initial_location churns. It is
// emitted in link order rather than target order (only 74.520% of FDEs are in
// ascending target order), so the entries themselves are matched by the
// equivalence map as ordinary bytes; what needs a model is the 4-byte
// PC-relative target field inside each one.
//
// .eh_frame_hdr is the derived index over it -- a header plus fde_count
// (initial_location, fde_pointer) pairs sorted by address -- and is 86.312%
// wrong in the byte prediction while being fully regenerable from .eh_frame.

const (
	ehPtrEncPCRelSData4 = 0x1b
	ehEncUData4         = 0x03
	ehTableEncDataRel4  = 0x3b
	ehHdrPrefix         = 12 // version, 3 encodings, eh_frame_ptr, fde_count
)

// ehFramePlan carries the geometry the decoder cannot derive for itself. The
// contents of both sections are predicted, never shipped.
type ehFramePlan struct {
	OldOff, OldSize  uint64
	NewOff, NewSize  uint64
	OldAddr, NewAddr uint64
	HdrOff, HdrSize  uint64
	HdrAddr          uint64
}

func (p ehFramePlan) marshal() []byte {
	var b []byte
	for _, v := range []uint64{p.OldOff, p.OldSize, p.NewOff, p.NewSize, p.OldAddr, p.NewAddr, p.HdrOff, p.HdrSize, p.HdrAddr} {
		b = binary.AppendUvarint(b, v)
	}
	return b
}

func unmarshalEhFramePlan(b []byte) (ehFramePlan, error) {
	r := &planReader{b: b}
	p := ehFramePlan{
		OldOff: r.u(), OldSize: r.u(), NewOff: r.u(), NewSize: r.u(),
		OldAddr: r.u(), NewAddr: r.u(), HdrOff: r.u(), HdrSize: r.u(), HdrAddr: r.u(),
	}
	if r.err != nil {
		return ehFramePlan{}, errors.New("invalid eh_frame plan")
	}
	return p, nil
}

type ehFrameStats struct {
	FDEs       int `json:"fdes"`
	Retargeted int `json:"retargeted"`
	Unknown    int `json:"unknown"`
	HdrEntries int `json:"hdr_entries"`
	Resized    int `json:"resized"`
}

// ehFDE is one frame description entry located in a walked .eh_frame image.
type ehFDE struct {
	locOff uint64 // offset of the initial_location field within the section
	entry  uint64 // offset of the entry itself within the section
}

// walkEhFrame lists the FDEs in a .eh_frame image. It is deliberately
// defensive: it runs over a *predicted* section, so an entry length can be
// wrong, and the right response is to stop rather than to walk off into
// unrelated bytes. Whatever it misses the correction pays for.
func walkEhFrame(b []byte) (fdes []ehFDE, cieEnc map[uint64]byte) {
	cieEnc = map[uint64]byte{}
	for p := uint64(0); p+8 <= uint64(len(b)); {
		l := uint64(binary.LittleEndian.Uint32(b[p:]))
		if l == 0 || l == 0xffffffff {
			break
		}
		total := l + 4
		if total < 8 || p+total > uint64(len(b)) {
			break
		}
		id := binary.LittleEndian.Uint32(b[p+4:])
		if id == 0 {
			cieEnc[p] = cieFDEPointerEncoding(b[p : p+total])
		} else {
			cie := p + 4 - uint64(id)
			if enc, ok := cieEnc[cie]; ok && enc == ehPtrEncPCRelSData4 && p+16 <= uint64(len(b)) {
				fdes = append(fdes, ehFDE{locOff: p + 8, entry: p})
			}
		}
		p += total
	}
	return fdes, cieEnc
}

// cieFDEPointerEncoding reads the 'R' augmentation of a CIE, which gives the
// encoding of every FDE initial_location it governs.
func cieFDEPointerEncoding(cie []byte) byte {
	p := 9 // length, id, version
	start := p
	for p < len(cie) && cie[p] != 0 {
		p++
	}
	aug := string(cie[start:p])
	p++
	skipULEB := func() {
		for p < len(cie) && cie[p]&0x80 != 0 {
			p++
		}
		p++
	}
	skipULEB() // code alignment factor
	skipULEB() // data alignment factor
	skipULEB() // return address register
	if aug == "" || aug[0] != 'z' {
		return 0
	}
	skipULEB() // augmentation data length
	for i := 1; i < len(aug); i++ {
		if p >= len(cie) {
			return 0
		}
		switch aug[i] {
		case 'R':
			return cie[p]
		case 'L':
			p++
		default:
			// A personality routine ('P') is encoded with its own pointer
			// encoding and length; rather than decode it, decline the CIE.
			return 0
		}
	}
	return 0
}

// retargetEhFrame rewrites each FDE's initial_location in the predicted new
// section.
//
// It drives from the *old* section, not the predicted one. Walking the
// prediction looks natural and does not work: the predicted .eh_frame is
// 12.876% wrong, so an entry length field is eventually wrong too, and a
// defensive walker stops there -- in practice after 530 of 31,477 FDEs. The
// old section is exact, so each FDE is located there and its field projected
// forward through the equivalence map to wherever those bytes landed.
func retargetEhFrame(out, old []byte, ep equivalencePlan, p ehFramePlan, oldSecs sectionMap, mapper sourceEquivalenceMapper, lookup func(uint64) x86.Target, extentOf func(uint64) (uint64, uint64, bool)) (ehFrameStats, []ehNewFDE) {
	var st ehFrameStats
	oldSec := old[p.OldOff : p.OldOff+p.OldSize]
	fdes, _ := walkEhFrame(oldSec)
	st.FDEs = len(fdes)
	placed := make([]ehNewFDE, 0, len(fdes))
	for _, f := range fdes {
		newLoc, ok := mapper.project(p.OldOff + f.locOff)
		newEntry, ok2 := mapper.project(p.OldOff + f.entry)
		if !ok || !ok2 || newLoc < p.NewOff || newLoc+4 > p.NewOff+p.NewSize {
			st.Unknown++
			continue
		}
		oldFieldAddr, ok := oldSecs.addrOf(p.OldOff + f.locOff)
		if !ok {
			st.Unknown++
			continue
		}
		stored := int32(binary.LittleEndian.Uint32(oldSec[f.locOff:]))
		target := lookup(uint64(int64(oldFieldAddr) + int64(stored)))
		if !target.Known {
			st.Unknown++
			continue
		}
		newFieldAddr := p.NewAddr + (newLoc - p.NewOff)
		binary.LittleEndian.PutUint32(out[newLoc:], uint32(int32(int64(target.Addr)-int64(newFieldAddr))))
		st.Retargeted++
		// address_range follows initial_location and holds the extent of the
		// function described. It is a size, not an address, so the projection
		// says nothing about it and the plan's mapping has to supply it.
		//
		// Rewriting it from the mapping unconditionally was measured and lost
		// 848 XZ across 8,809 resized FDEs: the map and the FDE do not always
		// describe the same thing, because an FDE can cover a fragment rather
		// than a whole function. Requiring them to agree about the *old* size
		// first is the test of whether they are talking about the same extent.
		if newLoc+8 <= p.NewOff+p.NewSize {
			if oldSize, newSize, ok := extentOf(uint64(int64(oldFieldAddr) + int64(stored))); ok {
				stored := binary.LittleEndian.Uint32(out[newLoc+4:])
				if oldSize == uint64(stored) && newSize != oldSize && newSize == uint64(uint32(newSize)) {
					binary.LittleEndian.PutUint32(out[newLoc+4:], uint32(newSize))
					st.Resized++
				}
			}
		}
		placed = append(placed, ehNewFDE{target: target.Addr, entryAddr: p.NewAddr + (newEntry - p.NewOff)})
	}
	return st, placed
}

// ehNewFDE is one FDE as it is expected to appear in the new image: where its
// entry starts and which function it describes.
type ehNewFDE struct{ target, entryAddr uint64 }

// regenerateEhFrameHdr rebuilds .eh_frame_hdr from the projected FDEs. The
// section is a pure index: a header, then one (initial_location, fde_pointer)
// pair per FDE, both section-relative and sorted by address.
func regenerateEhFrameHdr(out []byte, p ehFramePlan, fdes []ehNewFDE) int {
	if p.HdrSize < ehHdrPrefix || len(fdes) == 0 {
		return 0
	}
	hdr := out[p.HdrOff : p.HdrOff+p.HdrSize]
	if hdr[0] != 1 || hdr[1] != ehPtrEncPCRelSData4 || hdr[2] != ehEncUData4 || hdr[3] != ehTableEncDataRel4 {
		return 0
	}
	sorted := slices.Clone(fdes)
	slices.SortFunc(sorted, func(a, b ehNewFDE) int { return cmpU(a.target, b.target) })
	binary.LittleEndian.PutUint32(hdr[4:], uint32(int32(int64(p.NewAddr)-int64(p.HdrAddr+4))))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(sorted)))
	for i, e := range sorted {
		off := uint64(ehHdrPrefix + i*8)
		if off+8 > p.HdrSize {
			break
		}
		binary.LittleEndian.PutUint32(hdr[off:], uint32(int32(int64(e.target)-int64(p.HdrAddr))))
		binary.LittleEndian.PutUint32(hdr[off+4:], uint32(int32(int64(e.entryAddr)-int64(p.HdrAddr))))
	}
	return len(sorted)
}

func applyEhFrame(out, old []byte, ep equivalencePlan, p ehFramePlan, oldSecs sectionMap, lookup func(uint64) x86.Target, extentOf func(uint64) (uint64, uint64, bool)) ehFrameStats {
	mapper := newSourceEquivalenceMapper(ep)
	st, placed := retargetEhFrame(out, old, ep, p, oldSecs, mapper, lookup, extentOf)
	st.HdrEntries = regenerateEhFrameHdr(out, p, placed)
	return st
}

func reportEhFrame(st ehFrameStats) {
	fmt.Fprintf(os.Stderr, "eh_frame: %d FDEs, %d retargeted, %d resized, %d unresolved; eh_frame_hdr %d entries\n",
		st.FDEs, st.Retargeted, st.Resized, st.Unknown, st.HdrEntries)
}
