package main

import "errors"

// predictImage is the whole-image counterpart of predictCombined. The plan is
// the same one the .text path uses: the decoder still receives only the old
// image and the serialized plans. Equivalences are applied across every byte
// of the image rather than being cropped to .text, and the structural
// retargeting and per-function choice then run inside the .text window
// exactly as before.
func predictImage(old []byte, encoded []byte) ([]byte, combinedStats, error) {
	cp, err := unmarshalCombinedPlan(encoded)
	if err != nil {
		return nil, combinedStats{}, err
	}
	// The equivalence sources may be written against the function map, so the
	// map is decoded first. It needs only the old image's .text, which the
	// equivalence header names before any equivalence is decoded.
	ep, err := parseEquivalencePlan(cp.Equivalences)
	if err != nil {
		return nil, combinedStats{}, err
	}
	if uint64(len(old)) != ep.OldLen || ep.NewLen > uint64(int(^uint(0)>>1)) {
		return nil, combinedStats{}, errors.New("old image does not match the equivalence plan")
	}
	var structure predictionPlan
	var pred *srcPredictor
	if len(cp.Structure) != 0 {
		structure, err = unmarshalPlan(cp.Structure, old[ep.OldText.Off:ep.OldText.Off+ep.OldText.Size])
		if err != nil {
			return nil, combinedStats{}, err
		}
		if structure.TargetLen != ep.NewText.Size || structure.OldAddr != ep.OldText.Addr || structure.NewAddr != ep.NewText.Addr {
			return nil, combinedStats{}, errors.New("combined structural and equivalence plans describe different text sections")
		}
		if len(structure.Maps) != 0 {
			pred = &srcPredictor{maps: structure.Maps, oldOff: ep.OldText.Off, newOff: ep.NewText.Off, newSize: ep.NewText.Size}
		}
	}
	if ep.Eqs, err = decodeEquivalences(ep, pred); err != nil {
		return nil, combinedStats{}, err
	}
	out := make([]byte, int(ep.NewLen))
	// 0xcc is the padding byte between functions, so it is the better guess
	// inside .text; everywhere else zero is the better guess.
	for i := ep.NewText.Off; i < ep.NewText.Off+ep.NewText.Size; i++ {
		out[i] = 0xcc
	}
	copied := 0
	for _, eq := range ep.Eqs {
		copy(out[eq.Dst:eq.Dst+eq.N], old[eq.Src:eq.Src+eq.N])
		copied += int(eq.N)
	}
	stats := combinedStats{Equivalences: len(ep.Eqs), EquivalenceTextBytes: copied}
	if len(cp.Structure) == 0 {
		return out, stats, nil
	}

	var rp *relocPlan
	if len(cp.Reloc) != 0 {
		parsed, err := unmarshalRelocPlan(cp.Reloc)
		if err != nil {
			return nil, combinedStats{}, err
		}
		if parsed.OldOff+parsed.OldSize > uint64(len(old)) || parsed.NewOff+parsed.NewSize > uint64(len(out)) {
			return nil, combinedStats{}, errors.New("relocation plan exceeds an image")
		}
		rp = &parsed
	}
	oracle := newImageOracle(ep, structure, rp)
	if rp != nil {
		st, err := applyReloc(out, old, *rp, newPointerOracle(ep, structure, rp))
		if err != nil {
			return nil, combinedStats{}, err
		}
		stats.Reloc = st
	}
	if len(cp.EhFrame) != 0 {
		fp, err := unmarshalEhFramePlan(cp.EhFrame)
		if err != nil {
			return nil, combinedStats{}, err
		}
		if fp.NewOff+fp.NewSize > uint64(len(out)) || fp.OldOff+fp.OldSize > uint64(len(old)) ||
			fp.HdrOff+fp.HdrSize > uint64(len(out)) {
			return nil, combinedStats{}, errors.New("eh_frame plan exceeds an image")
		}
		if rp == nil {
			return nil, combinedStats{}, errors.New("eh_frame plan needs the section geometry the relocation plan carries")
		}
		// Keyed by old address: an FDE names the function it describes by
		// where that function used to be.
		type extent struct{ old, new uint64 }
		extents := make(map[uint64]extent, len(structure.Maps))
		for _, m := range structure.Maps {
			extents[structure.OldAddr+m.Src] = extent{m.SrcSize, m.DstSize}
		}
		extentOf := func(addr uint64) (uint64, uint64, bool) {
			e, ok := extents[addr]
			return e.old, e.new, ok
		}
		stats.EhFrame = applyEhFrame(out, old, ep, fp, rp.OldSecs, newPointerOracle(ep, structure, rp), extentOf)
	}
	if len(cp.RoData) != 0 {
		rd, err := unmarshalRoDataPlan(cp.RoData)
		if err != nil {
			return nil, combinedStats{}, err
		}
		if rd.NewOff+rd.NewSize > uint64(len(out)) || rd.OldOff+rd.OldSize > uint64(len(old)) {
			return nil, combinedStats{}, errors.New("rodata plan exceeds an image")
		}
		if rp == nil {
			return nil, combinedStats{}, errors.New("rodata plan needs the section geometry the relocation plan carries")
		}
		stats.RoData = applyRoData(out, old, rd, newSourceEquivalenceMapper(ep), newPointerOracle(ep, structure, rp))
	}
	text := out[ep.NewText.Off : ep.NewText.Off+ep.NewText.Size]
	stats.Relocation = retargetEquivalencePrediction(text, ep, structure, oracle)
	if len(cp.Choices) == 0 {
		return out, stats, nil
	}
	if len(cp.Choices) != (len(structure.Maps)+7)/8 {
		return nil, combinedStats{}, errors.New("combined choice stream has the wrong size")
	}
	oldText := old[ep.OldText.Off : ep.OldText.Off+ep.OldText.Size]
	structural, _, err := predict(oldText, cp.Structure, true)
	if err != nil {
		return nil, combinedStats{}, err
	}
	for i, m := range structure.Maps {
		if cp.Choices[i/8]&(1<<(i%8)) == 0 {
			continue
		}
		copy(text[m.Dst:m.Dst+m.DstSize], structural[m.Dst:m.Dst+m.DstSize])
		stats.SelectedFunctions++
		stats.SelectedBytes += int(m.DstSize)
	}
	// The field layers come last: they name fields by position in a walk of
	// the finished prediction, so every model that can move a .text byte has
	// to have run already.
	if len(cp.Fields) != 0 {
		fs, err := applyFieldFix(text, ep.NewText.Addr, structure.Maps, cp.Fields)
		if err != nil {
			return nil, combinedStats{}, err
		}
		stats.Fields = fs
	}
	return out, stats, nil
}
