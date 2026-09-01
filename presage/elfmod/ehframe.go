package elfmod

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"

	"github.com/wjordan/presage/delta/x86"
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
// wrong in the byte prediction while being a pure function of the finished
// .eh_frame. Predicting it from the *projected* FDEs was a guess that a few
// unplaced entries shifted wholesale; rebuilding it from the *corrected*
// section is exact, so it is done after the residual through
// presage.Finaliser and the residual is told the section is already right.

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
	// HdrExact says the encoder checked that rebuilding .eh_frame_hdr from
	// the target's own .eh_frame reproduces it byte for byte, so the
	// residual does not carry the section and Finalise rebuilds it.
	HdrExact bool
}

func (p ehFramePlan) marshal() []byte {
	var b []byte
	for _, v := range []uint64{p.OldOff, p.OldSize, p.NewOff, p.NewSize, p.OldAddr, p.NewAddr, p.HdrOff, p.HdrSize, p.HdrAddr} {
		b = binary.AppendUvarint(b, v)
	}
	return append(b, boolByte(p.HdrExact))
}

func unmarshalEhFramePlan(b []byte) (ehFramePlan, error) {
	r := &planReader{b: b}
	p := ehFramePlan{
		OldOff: r.u(), OldSize: r.u(), NewOff: r.u(), NewSize: r.u(),
		OldAddr: r.u(), NewAddr: r.u(), HdrOff: r.u(), HdrSize: r.u(), HdrAddr: r.u(),
	}
	p.HdrExact = r.u() != 0
	if r.err != nil || len(r.b) != 0 {
		return ehFramePlan{}, errors.New("trailing or invalid eh_frame plan data")
	}
	return p, nil
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

type ehFrameStats struct {
	FDEs       int
	Retargeted int
	Unknown    int
	Resized    int
	CiePtrs    int
}

// ehFDE is one frame description entry located in a walked .eh_frame image.
type ehFDE struct {
	locOff uint64 // offset of the initial_location field within the section
	entry  uint64 // offset of the entry itself within the section
	cieOff uint64 // offset of the CIE that governs the entry
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
				fdes = append(fdes, ehFDE{locOff: p + 8, entry: p, cieOff: cie})
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
		case 'P':
			// The personality routine: its own encoding byte, then a
			// pointer of that encoding's width.
			enc := cie[p]
			p++
			switch enc & 0x0f {
			case 0x00: // absptr
				p += 8
			case 0x01: // uleb128
				skipULEB()
			case 0x02, 0x0a: // udata2, sdata2
				p += 2
			case 0x03, 0x0b: // udata4, sdata4
				p += 4
			case 0x04, 0x0c: // udata8, sdata8
				p += 8
			default:
				return 0
			}
		default:
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
func retargetEhFrame(out, old []byte, ep equivalencePlan, p ehFramePlan, oldSecs sectionMap, mapper sourceEquivalenceMapper, lookup func(uint64) x86.Target, extentOf func(uint64) (uint64, uint64, bool)) ehFrameStats {
	var st ehFrameStats
	oldSec := old[p.OldOff : p.OldOff+p.OldSize]
	fdes, _ := walkEhFrame(oldSec)
	st.FDEs = len(fdes)
	// One CIE governs thousands of FDEs, so project each one once.
	type projection struct {
		off uint64
		ok  bool
	}
	cies := make(map[uint64]projection)
	cieAt := func(off uint64) (uint64, bool) {
		if v, seen := cies[off]; seen {
			return v.off, v.ok
		}
		newOff, ok := mapper.project(p.OldOff + off)
		cies[off] = projection{newOff, ok}
		return newOff, ok
	}
	for _, f := range fdes {
		newLoc, ok := mapper.project(p.OldOff + f.locOff)
		newEntry, okEntry := mapper.project(p.OldOff + f.entry)
		if !ok || !okEntry || newLoc < p.NewOff || newLoc+4 > p.NewOff+p.NewSize {
			st.Unknown++
			continue
		}
		// cie_ptr, at entry+4, is the only field of an FDE that states a
		// position rather than an address: the distance back from itself to
		// the CIE that governs the entry. Every FDE that moved relative to
		// its CIE -- which is every FDE downstream of an insertion or a
		// resize -- carries the old distance in the byte prediction and is
		// wrong. The right value follows from where the entry and its CIE
		// landed, and the projection already says both, so it costs nothing.
		if newCie, ok := cieAt(f.cieOff); ok && newEntry >= p.NewOff && newEntry+8 <= p.NewOff+p.NewSize &&
			newCie >= p.NewOff && newCie < newEntry+4 {
			binary.LittleEndian.PutUint32(out[newEntry+4:], uint32(newEntry+4-newCie))
			st.CiePtrs++
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
	}
	return st
}

// buildEhFrameHdr writes into hdr the .eh_frame_hdr that a finished
// .eh_frame implies: the four encoding bytes, the pointer back to
// .eh_frame, the entry count, then one (initial_location, fde_pointer) pair
// per FDE, both relative to the header's own address and sorted by address.
// It writes every byte of the section, so what it produces depends on the
// finished .eh_frame alone and on nothing that was there before.
func buildEhFrameHdr(hdr, frame []byte, p ehFramePlan) bool {
	if uint64(len(hdr)) < ehHdrPrefix {
		return false
	}
	fdes, _ := walkEhFrame(frame)
	entries := make([]ehHdrEntry, 0, len(fdes))
	for _, f := range fdes {
		stored := int32(binary.LittleEndian.Uint32(frame[f.locOff:]))
		fieldAddr := p.NewAddr + f.locOff
		entries = append(entries, ehHdrEntry{
			loc: uint64(int64(fieldAddr) + int64(stored)),
			ptr: p.NewAddr + f.entry,
		})
	}
	// Folded functions leave several FDEs with one initial_location; the
	// linker indexes only the first of them in .eh_frame order.
	slices.SortStableFunc(entries, func(a, b ehHdrEntry) int { return cmpU(a.loc, b.loc) })
	entries = slices.CompactFunc(entries, func(a, b ehHdrEntry) bool { return a.loc == b.loc })
	if ehHdrPrefix+8*len(entries) > len(hdr) {
		return false
	}
	hdr[0], hdr[1], hdr[2], hdr[3] = 1, ehPtrEncPCRelSData4, ehEncUData4, ehTableEncDataRel4
	binary.LittleEndian.PutUint32(hdr[4:], uint32(int32(int64(p.NewAddr)-int64(p.HdrAddr+4))))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(entries)))
	for i, e := range entries {
		off := ehHdrPrefix + i*8
		binary.LittleEndian.PutUint32(hdr[off:], uint32(int32(int64(e.loc)-int64(p.HdrAddr))))
		binary.LittleEndian.PutUint32(hdr[off+4:], uint32(int32(int64(e.ptr)-int64(p.HdrAddr))))
	}
	fill(hdr[ehHdrPrefix+8*len(entries):], 0)
	return true
}

// ehHdrEntry is one row of the index: the function an FDE describes and the
// FDE's own address.
type ehHdrEntry struct{ loc, ptr uint64 }

// ehFrameHdrDerivable is the encoder's check that the rule holds on this
// target. Nothing is masked unless rebuilding the section from the target's
// own .eh_frame reproduces the target's own bytes.
func ehFrameHdrDerivable(target []byte, p ehFramePlan) bool {
	if p.HdrSize == 0 || p.HdrOff+p.HdrSize > uint64(len(target)) || p.NewOff+p.NewSize > uint64(len(target)) {
		return false
	}
	want := target[p.HdrOff : p.HdrOff+p.HdrSize]
	got := make([]byte, len(want))
	return buildEhFrameHdr(got, target[p.NewOff:p.NewOff+p.NewSize], p) && bytes.Equal(got, want)
}

// hdrPlan is the eh_frame geometry of a plan that says .eh_frame_hdr is
// rebuilt after the residual, or false.
func hdrPlan(planb []byte) (ehFramePlan, bool) {
	cp, err := parsePlanStreams(planb)
	if err != nil || len(cp.EhFrame) == 0 {
		return ehFramePlan{}, false
	}
	p, err := unmarshalEhFramePlan(cp.EhFrame)
	if err != nil || !p.HdrExact {
		return ehFramePlan{}, false
	}
	return p, true
}

// finalisedSection is the section MaskResidual reveals, if any: the bytes
// the residual is not priced for, and so not bytes the encoder's own error
// counts should charge anyone for.
func finalisedSection(cp planStreams) (section, bool) {
	if len(cp.EhFrame) == 0 {
		return section{}, false
	}
	p, err := unmarshalEhFramePlan(cp.EhFrame)
	if err != nil || !p.HdrExact {
		return section{}, false
	}
	return section{Off: p.HdrOff, Size: p.HdrSize}, true
}

// MaskResidual implements presage.Finaliser: the prediction the residual is
// priced against already holds the target's .eh_frame_hdr, so the section
// costs nothing. The hashed prediction is the unmasked one.
func (Module) MaskResidual(planb, pred, target []byte) []byte {
	p, ok := hdrPlan(planb)
	if !ok || p.HdrOff+p.HdrSize > uint64(len(pred)) || p.HdrOff+p.HdrSize > uint64(len(target)) {
		return pred
	}
	masked := slices.Clone(pred)
	copy(masked[p.HdrOff:p.HdrOff+p.HdrSize], target[p.HdrOff:p.HdrOff+p.HdrSize])
	return masked
}

// Finalise implements presage.Finaliser: rebuild the index over the
// corrected .eh_frame, which is exact where the prediction was a guess.
func (Module) Finalise(planb, out []byte) error {
	p, ok := hdrPlan(planb)
	if !ok {
		return nil
	}
	if p.HdrOff+p.HdrSize > uint64(len(out)) || p.NewOff+p.NewSize > uint64(len(out)) {
		return corrupt(errors.New("eh_frame plan exceeds the output"))
	}
	if !buildEhFrameHdr(out[p.HdrOff:p.HdrOff+p.HdrSize], out[p.NewOff:p.NewOff+p.NewSize], p) {
		return corrupt(errors.New("the plan says to rebuild .eh_frame_hdr, but the corrected .eh_frame does not index into it"))
	}
	return nil
}

func applyEhFrame(out, old []byte, ep equivalencePlan, p ehFramePlan, oldSecs sectionMap, lookup func(uint64) x86.Target, extentOf func(uint64) (uint64, uint64, bool)) ehFrameStats {
	return retargetEhFrame(out, old, ep, p, oldSecs, newSourceEquivalenceMapper(ep), lookup, extentOf)
}
