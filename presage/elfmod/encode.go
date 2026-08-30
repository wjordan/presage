package elfmod

import (
	"errors"
	"fmt"

	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/presage"
	"github.com/wjordan/presage/presage/eqmatch"
)

// Analyse builds the plan for target against reference 0 and returns the
// prediction its own Materialise produces from that plan, so the decoder's
// path is proven on every encode (elf-module.md §3).
func (m Module) Analyse(refs [][]byte, target []byte) ([]byte, []byte, error) {
	old := refs[0]
	oi, err := loadImage(old)
	if err != nil {
		return nil, nil, presage.ErrDeclined
	}
	ni, err := loadImage(target)
	if err != nil {
		return nil, nil, presage.ErrDeclined
	}
	st := m.Stats
	if st == nil {
		st = &Stats{}
	}
	oldText, newText := oi.textBytes(), ni.textBytes()

	// 1. The function map from the symbols, when both sides have them.
	var oldUnits, newUnits []codeUnit
	if m.Symbols[0] != nil && m.Symbols[1] != nil {
		if oldUnits, err = codeUnits(m.Symbols[0], oi.Text); err != nil {
			return nil, nil, fmt.Errorf("elf: old symbols: %w", err)
		}
		if newUnits, err = codeUnits(m.Symbols[1], ni.Text); err != nil {
			return nil, nil, fmt.Errorf("elf: new symbols: %w", err)
		}
		st.Notes = append(st.Notes, fmt.Sprintf("symbols: %d old units, %d new units", len(oldUnits), len(newUnits)))
	} else {
		st.Notes = append(st.Notes, "no symbols: whole-image equivalences and section ranges only")
	}
	structure, ms := constructPlan(oldUnits, newUnits, oldText, newText, oi.Text.Addr, ni.Text.Addr, sectionRanges(oi, ni))
	structure.Points = inferReferencePoints(structure, oldText, newText)
	structure = allMapped(structure)
	st.Mappings, st.Points = len(structure.Maps), len(structure.Points)
	st.Notes = append(st.Notes, fmt.Sprintf("map: %d functions (%d by name, %d by content, %d canonical-equal), %d reference points",
		len(structure.Maps), ms.NameMapped, ms.ContentMapped, ms.CopyUnits, len(structure.Points)))
	structureBytes, err := structure.marshal(oldText)
	if err != nil {
		return nil, nil, fmt.Errorf("elf: %w", err)
	}
	structuralPred, _, err := predictDecoded(oldText, structure, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("elf: %w", err)
	}

	// 2. Whole-image equivalences, matched on canonical .text so moved
	// targets do not break runs; sources coded against the map.
	pred := newSrcPredictor(structure.Maps, oi.Text, ni.Text)
	params := m.Params
	if params.Min == 0 && params.Drop == 0 {
		params = eqmatch.CodeDefaults
	}
	if pred != nil && params.Expect == nil {
		params.Expect = func(dst int) (int, bool) {
			s, ok := pred.at(uint64(dst))
			return int(s), ok
		}
	}
	runs := eqmatch.Match(maskedText(oi), maskedText(ni), params)
	ep := equivalencePlan{OldLen: uint64(len(old)), NewLen: uint64(len(target)), OldText: oi.Text, NewText: ni.Text}
	ep.Eqs = make([]equivalence, len(runs))
	for i, r := range runs {
		ep.Eqs[i] = equivalence{Src: r.Src, Dst: r.Dst, N: r.N}
	}
	runs = nil
	epBytes, err := ep.marshal(pred)
	if err != nil {
		return nil, nil, fmt.Errorf("elf: %w", err)
	}
	st.Equivalences = len(ep.Eqs)

	// 3. Per-function choice between the retargeted equivalence copy and
	// the structural body.
	var choices []byte
	if len(structure.Maps) != 0 {
		laid := layImage(old, ep)
		text := laid[ep.NewText.Off : ep.NewText.Off+ep.NewText.Size]
		retargetEquivalencePrediction(text, ep, structure, newOracleParts(ep, structure).image(nil))
		choices, st.SelectedFunctions, st.SelectedBytes = chooseStructuralFunctions(text, structuralPred, newText, structure)
		laid = nil
	}
	structuralPred = nil

	// 4. The relocation table, or the section geometry alone for the
	// layers that read it from this plan.
	base := relocPlan{OldSecs: newSectionMap(oi.Sections), NewSecs: newSectionMap(ni.Sections)}
	oldRela, okOld := relaSection(oi.Sections)
	newRela, okNew := relaSection(ni.Sections)
	haveRela := okOld && okNew
	if haveRela {
		rel, _, _ := parseRela(target[newRela.Off : newRela.Off+newRela.Size])
		haveRela = len(rel) != 0
	}
	rp := base
	if haveRela {
		base.OldOff, base.OldSize, base.NewOff, base.NewSize = oldRela.Off, oldRela.Size, newRela.Off, newRela.Size
		if rp, err = buildRelocPlan(old, target, base, newOracleParts(ep, structure).pointer(&base)); err != nil {
			return nil, nil, fmt.Errorf("elf: %w", err)
		}
		st.Notes = append(st.Notes, fmt.Sprintf("reloc: gap %d B, addend %d B, tail %d B", len(rp.GapCorrection), len(rp.AddendCorrection), len(rp.TailCorrection)))
	} else {
		st.Notes = append(st.Notes, "no .rela.dyn in both images: geometry only")
	}
	cp := planStreams{Equivalences: epBytes, Structure: structureBytes, Choices: choices, Reloc: rp.marshal()}

	// 5. The DWARF field layer, for an unstripped pair.
	parts := newOracleParts(ep, structure)
	pointer := parts.pointer(&rp)
	addrMap := func(addr uint64) (uint64, bool) {
		t := pointer(addr)
		return t.Addr, t.Known
	}
	withRecords := func(k int) bool {
		s, ok := ni.Debug[dwarfSecNames[k]]
		return ok && (!eqCovers(ep, s.Off, s.Size) || k == dwSymtab || k == dwAddr || k == dwStrtab || k == dwFrame)
	}
	if dp, ok := buildDwarfPlan(oi, ni, ep, withRecords, addrMap); ok {
		cp.Dwarf = dp.Marshal()
		st.Notes = append(st.Notes, fmt.Sprintf("dwarf: plan %d B", len(cp.Dwarf)))
	}

	// 6. Unwind tables: geometry only, both sections are regenerated.
	oldEh, okOldEh := oi.Sections[".eh_frame"]
	newEh, okNewEh := ni.Sections[".eh_frame"]
	newHdr, okHdr := ni.Sections[".eh_frame_hdr"]
	if okOldEh && okNewEh && okHdr {
		cp.EhFrame = ehFramePlan{
			OldOff: oldEh.Off, OldSize: oldEh.Size, NewOff: newEh.Off, NewSize: newEh.Size,
			OldAddr: oldEh.Addr, NewAddr: newEh.Addr,
			HdrOff: newHdr.Off, HdrSize: newHdr.Size, HdrAddr: newHdr.Addr,
		}.marshal()
	}

	// 7. .rodata switch tables: predict once, keep the candidates that help.
	oldRo, okOldRo := oi.Sections[".rodata"]
	newRo, okNewRo := ni.Sections[".rodata"]
	if okOldRo && okNewRo {
		rd := roDataPlan{
			OldOff: oldRo.Off, OldSize: oldRo.Size, NewOff: newRo.Off, NewSize: newRo.Size,
			OldAddr: oldRo.Addr, NewAddr: newRo.Addr,
			TextLo: oi.Text.Addr, TextHi: oi.Text.Addr + oi.Text.Size,
		}
		p, _, err := predictImage(old, cp)
		if err != nil {
			return nil, nil, fmt.Errorf("elf: rodata selection: %w", err)
		}
		var sel roDataStats
		rd.Keep, sel = selectRoDataTables(p, old, target, rd, parts.sm, pointer)
		st.Notes = append(st.Notes, fmt.Sprintf("rodata: %d of %d spans kept", sel.Tables, sel.Candidates))
		cp.RoData = rd.marshal()
	}

	// 8. The field fix over the finished .text.
	{
		p, _, err := predictImage(old, cp)
		if err != nil {
			return nil, nil, fmt.Errorf("elf: field fix: %w", err)
		}
		nt := ni.Text
		fx, fst := encodeFieldFix(p[nt.Off:nt.Off+nt.Size], target[nt.Off:nt.Off+nt.Size], nt.Addr, structure.Maps)
		st.Notes = append(st.Notes, fmt.Sprintf("fields: %d sites, %d remaps, %d deltas", fst.Sites, fst.Remaps, fst.Deltas))
		cp.Fields = fx.marshal()
	}

	// 9. What the decoder will build.
	plan := cp.marshal()
	out, ps, err := predictImage(old, cp)
	if err != nil {
		return nil, nil, fmt.Errorf("elf: %w", err)
	}
	if len(out) != len(target) {
		return nil, nil, errors.New("elf: prediction length disagrees with the target")
	}
	st.Relocation = ps.Relocation
	st.PredictErr = wrongCount(out, target)
	st.TextPredictErr = wrongCount(out[ni.Text.Off:ni.Text.Off+ni.Text.Size], newText)
	st.Notes = append(st.Notes, fmt.Sprintf("plan %d B (eq %d, structure %d, choices %d, reloc %d, eh %d, rodata %d, fields %d, dwarf %d); %d mispredicted bytes, %d in .text",
		len(plan), len(cp.Equivalences), len(cp.Structure), len(cp.Choices), len(cp.Reloc), len(cp.EhFrame), len(cp.RoData), len(cp.Fields), len(cp.Dwarf),
		st.PredictErr, st.TextPredictErr))
	return plan, out, nil
}

// maskedText is the image with .text canonicalised: PC-relative
// displacements zeroed (x86.Canonical), so two copies of the same code
// whose targets moved agree byte for byte and the retargeting stage fixes
// the displacements afterwards.
func maskedText(img *image) []byte {
	if img.Text.Size == 0 {
		return img.Data
	}
	out := append([]byte(nil), img.Data...)
	lo, hi := img.Text.Off, img.Text.Off+img.Text.Size
	canon, _ := x86.Canonical(img.Data[lo:hi])
	copy(out[lo:hi], canon)
	return out
}
