package elfmod

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/presage"
	"github.com/wjordan/presage/presage/dwarf"
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
	windows := pairCodeWindows(oi, ni)
	if len(windows) == 0 {
		return nil, nil, presage.ErrDeclined
	}
	ranges := sectionRanges(oi, ni)

	// 1. One function map per code window, from the symbols when both
	// sides have them. Every window carries the same section ranges, so
	// each window's lookup is complete on its own.
	haveSymbols := m.Symbols[0] != nil && m.Symbols[1] != nil
	structures := make([]predictionPlan, len(windows))
	var structureBytes []byte
	var ms matchStats
	var oldUnitCount, newUnitCount int
	var derived []derivedStats
	for i, w := range windows {
		oldCode, newCode := bytesOf(old, w.Old), bytesOf(target, w.New)
		var oldUnits, newUnits []codeUnit
		if haveSymbols {
			if oldUnits, err = codeUnits(m.Symbols[0], w.Old); err != nil {
				return nil, nil, fmt.Errorf("elf: old symbols: %w", err)
			}
			if newUnits, err = codeUnits(m.Symbols[1], w.New); err != nil {
				return nil, nil, fmt.Errorf("elf: new symbols: %w", err)
			}
			oldUnitCount, newUnitCount = oldUnitCount+len(oldUnits), newUnitCount+len(newUnits)
		}
		p, wms := constructPlan(oldUnits, newUnits, oldCode, newCode, w.Old.Addr, w.New.Addr, ranges)
		p.Points = inferReferencePoints(p, oldCode, newCode)
		structures[i] = allMapped(p)
		ms.NameMapped += wms.NameMapped
		ms.ContentMapped += wms.ContentMapped
		ms.CopyUnits += wms.CopyUnits
		ms.CopyBytes += wms.CopyBytes
		st.Mappings += len(structures[i].Maps)
		st.Points += len(structures[i].Points)
		// The map columns are the plan's largest row, and most of what they
		// say the decoder can derive from the old image alone (derived.go).
		// A window the derived form cannot express falls back to the dense
		// columns; both forms decode to the same map.
		ds, dst, ok := buildDerivedForm(structures[i], old, w.Old, oldUnits, newUnits)
		if ok {
			derived = append(derived, dst)
		}
		b, err := structures[i].marshalMode(oldCode, ds)
		if err != nil {
			return nil, nil, fmt.Errorf("elf: %w", err)
		}
		structureBytes = appendStream(structureBytes, b)
	}
	if haveSymbols {
		st.Notes = append(st.Notes, fmt.Sprintf("symbols: %d old units, %d new units", oldUnitCount, newUnitCount))
	} else {
		st.Notes = append(st.Notes, "no symbols: whole-image equivalences and section ranges only")
	}
	st.Notes = append(st.Notes, fmt.Sprintf("windows: %d code (%s)", len(windows), windowSummary(oi, windows)))
	st.Notes = append(st.Notes, fmt.Sprintf("map: %d functions (%d by name, %d by content, %d canonical-equal), %d reference points",
		st.Mappings, ms.NameMapped, ms.ContentMapped, ms.CopyUnits, st.Points))
	if len(derived) != 0 {
		var d derivedStats
		for _, w := range derived {
			d.Enumerated += w.Enumerated
			d.Units += w.Units
			d.Dropped += w.Dropped
			d.Runs += w.Runs
			d.Reorders += w.Reorders
			d.Resizes += w.Resizes
			d.Inserts += w.Inserts
			d.Fixes += w.Fixes
			d.Boundary += w.Boundary
			d.Unrepresentable += w.Unrepresentable
			d.Align = max(d.Align, w.Align)
		}
		st.Notes = append(st.Notes, fmt.Sprintf("derived map: %d/%d windows; %d enumerated -> %d units (%d boundary exceptions); %d dropped in %d runs, %d reorders, %d resizes, %d inserts, %d layout fixups at align %d, %d unrepresentable",
			len(derived), len(windows), d.Enumerated, d.Units, d.Boundary, d.Dropped, d.Runs, d.Reorders, d.Resizes, d.Inserts, d.Fixes, d.Align, d.Unrepresentable))
	}

	// 2. Whole-image equivalences, matched on canonical code windows so
	// moved targets do not break runs; sources coded against the map.
	pred := newSrcPredictor(structures, windows)
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
	oldMasked, newMasked := maskedCode(old, windows, func(w codeWindow) section { return w.Old }),
		maskedCode(target, windows, func(w codeWindow) section { return w.New })
	runs := eqmatch.Match(oldMasked, newMasked, params)
	oldMasked, newMasked = nil, nil
	ep := equivalencePlan{OldLen: uint64(len(old)), NewLen: uint64(len(target)), Windows: windows}
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
	// the structural body, one window at a time.
	var choices []byte
	if st.Mappings != 0 {
		laid := layImage(old, ep, nil)
		parts := newOracleParts(ep, structures)
		oracle := parts.image(nil)
		for i, w := range windows {
			text := bytesOf(laid, w.New)
			retargetEquivalencePrediction(text, ep, w, structures[i], oracle)
			if len(structures[i].Maps) == 0 {
				choices = appendStream(choices, nil)
				continue
			}
			structuralPred, _, err := predictDecoded(bytesOf(old, w.Old), structures[i], parts.lk.target)
			if err != nil {
				return nil, nil, fmt.Errorf("elf: %w", err)
			}
			b, fns, bytes := chooseStructuralFunctions(text, structuralPred, bytesOf(target, w.New), structures[i])
			st.SelectedFunctions += fns
			st.SelectedBytes += bytes
			choices = appendStream(choices, b)
		}
		laid = nil
	}

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
		if rp, err = buildRelocPlan(old, target, base, newOracleParts(ep, structures).pointer(&base)); err != nil {
			return nil, nil, fmt.Errorf("elf: %w", err)
		}
		st.Notes = append(st.Notes, fmt.Sprintf("reloc: gap %d B, addend %d B, tail %d B", len(rp.GapCorrection), len(rp.AddendCorrection), len(rp.TailCorrection)))
	} else {
		st.Notes = append(st.Notes, "no .rela.dyn in both images: geometry only")
	}
	cp := planStreams{Equivalences: epBytes, Structure: structureBytes, Choices: choices, Reloc: rp.marshal()}

	// 4a. RELR relative relocations, where the linker packs them instead of
	// spelling them out: geometry only, the slots and their pointers are
	// derived (relr.go).
	oldRelr, okOldRelr := oi.Sections[".relr.dyn"]
	_, okNewRelr := ni.Sections[".relr.dyn"]
	if okOldRelr && okNewRelr {
		cp.Relr = relrPlan{OldOff: oldRelr.Off, OldSize: oldRelr.Size}.marshal()
	}

	// 5. The DWARF field layer, for an unstripped pair.
	parts := newOracleParts(ep, structures)
	pointer := parts.pointer(&rp)
	addrMap := func(addr uint64) (uint64, bool) {
		t := pointer(addr)
		return t.Addr, t.Known
	}
	withRecords := func(k int) bool {
		s, ok := ni.Debug[dwarf.Names[k]]
		return ok && (!eqCovers(ep, s.Off, s.Size) || dwarf.RecordsRequired(k))
	}
	if dp, ok := buildDwarfPlan(oi, ni, ep, withRecords, addrMap); ok {
		cp.Dwarf = dp.Marshal()
		st.Notes = append(st.Notes, fmt.Sprintf("dwarf: plan %d B", len(cp.Dwarf)))
	}

	// 6. Unwind tables: geometry only. .eh_frame is retargeted from the old
	// one; .eh_frame_hdr is the index over the finished section, rebuilt
	// after the residual wherever the target agrees it is derivable.
	oldEh, okOldEh := oi.Sections[".eh_frame"]
	newEh, okNewEh := ni.Sections[".eh_frame"]
	newHdr, okHdr := ni.Sections[".eh_frame_hdr"]
	if okOldEh && okNewEh && okHdr {
		fp := ehFramePlan{
			OldOff: oldEh.Off, OldSize: oldEh.Size, NewOff: newEh.Off, NewSize: newEh.Size,
			OldAddr: oldEh.Addr, NewAddr: newEh.Addr,
			HdrOff: newHdr.Off, HdrSize: newHdr.Size, HdrAddr: newHdr.Addr,
		}
		fp.HdrExact = ehFrameHdrDerivable(target, fp)
		cp.EhFrame = fp.marshal()
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
		p, _, err := predictImage(old, cp, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("elf: rodata selection: %w", err)
		}
		var sel roDataStats
		rd.Keep, sel = selectRoDataTables(p, old, target, rd, parts.sm, pointer)
		st.Notes = append(st.Notes, fmt.Sprintf("rodata: %d of %d spans kept", sel.Tables, sel.Candidates))
		cp.RoData = rd.marshal()
	}

	// 8. The field fix over each finished code window.
	{
		p, _, err := predictImage(old, cp, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("elf: field fix: %w", err)
		}
		var total fieldStats
		for i, w := range windows {
			fx, fst := encodeFieldFix(bytesOf(p, w.New), bytesOf(target, w.New), w.New.Addr, structures[i].Maps)
			total.Sites += fst.Sites
			total.Remaps += fst.Remaps
			total.Deltas += fst.Deltas
			cp.Fields = appendStream(cp.Fields, fx.marshal())
		}
		st.Notes = append(st.Notes, fmt.Sprintf("fields: %d sites, %d remaps, %d deltas", total.Sites, total.Remaps, total.Deltas))
	}

	// 9. What the decoder will build.
	plan, planNote := packPlan(cp.marshal())
	out, ps, err := predictImage(old, cp, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("elf: %w", err)
	}
	if len(out) != len(target) {
		return nil, nil, errors.New("elf: prediction length disagrees with the target")
	}
	st.Relocation = ps.Relocation
	if len(cp.EhFrame) != 0 {
		e, hdr := ps.EhFrame, "hdr from the residual"
		if fp, _ := unmarshalEhFramePlan(cp.EhFrame); fp.HdrExact {
			hdr = "hdr rebuilt after the residual"
		}
		st.Notes = append(st.Notes, fmt.Sprintf("eh_frame: %d FDEs, %d retargeted, %d unknown, %d resized; %s",
			e.FDEs, e.Retargeted, e.Unknown, e.Resized, hdr))
	}
	if len(cp.Relr) != 0 {
		r := ps.Relr
		st.Notes = append(st.Notes, fmt.Sprintf("relr: %d slots, %d retargeted, %d oracle unknown, %d unplaced",
			r.Slots, r.Retargeted, r.Unknown, r.Unplaced))
	}
	st.PredictErr = wrongCount(out, target)
	st.TextPredictErr = wrongCount(bytesOf(out, ni.Text), bytesOf(target, ni.Text))
	st.Notes = append(st.Notes, fmt.Sprintf("plan %d B (eq %d, structure %d, choices %d, reloc %d, eh %d, rodata %d, fields %d, dwarf %d, relr %d); %d mispredicted bytes, %d in .text",
		len(plan), len(cp.Equivalences), len(cp.Structure), len(cp.Choices), len(cp.Reloc), len(cp.EhFrame), len(cp.RoData), len(cp.Fields), len(cp.Dwarf), len(cp.Relr),
		st.PredictErr, st.TextPredictErr))
	st.Notes = append(st.Notes, planNote)
	st.Notes = append(st.Notes, sectionErrNote(out, target, ni, st.PredictErr))
	return plan, out, nil
}

// sectionErrNote attributes the mispredicted bytes to the new image's
// sections, largest first. Which section pays is what decides whether a
// missing layer is worth building, and the total alone never says.
func sectionErrNote(out, target []byte, ni *image, total int) string {
	type row struct {
		name string
		err  int
	}
	var rows []row
	var covered int
	count := func(name string, s section) {
		if s.NoBits || s.Off+s.Size > uint64(len(target)) {
			return
		}
		n := wrongCount(out[s.Off:s.Off+s.Size], target[s.Off:s.Off+s.Size])
		covered += n
		if n != 0 {
			rows = append(rows, row{name, n})
		}
	}
	for name, s := range ni.Sections {
		count(name, s)
	}
	for name, s := range ni.Debug {
		count(name, s)
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Compare(b.err, a.err) })
	var b strings.Builder
	b.WriteString("mispredicted by section:")
	for i, r := range rows {
		if i == 12 {
			fmt.Fprintf(&b, " (+%d more)", len(rows)-i)
			break
		}
		fmt.Fprintf(&b, " %s=%d", r.name, r.err)
	}
	fmt.Fprintf(&b, "; elsewhere=%d", total-covered)
	return b.String()
}

// maskedCode is the image with every code window canonicalised: PC-relative
// displacements zeroed (x86.Canonical), so two copies of the same code
// whose targets moved agree byte for byte and the retargeting stage fixes
// the displacements afterwards. A window the module cannot reach is matched
// on raw bytes, where a body that moved agrees with nothing.
func maskedCode(data []byte, windows []codeWindow, pick func(codeWindow) section) []byte {
	out := append([]byte(nil), data...)
	for _, w := range windows {
		s := pick(w)
		if s.Size != 0 {
			maskWindow(out, s.Off, s.Off+s.Size)
		}
	}
	return out
}

func maskWindow(out []byte, lo, hi uint64) {
	canon, _ := x86.Canonical(out[lo:hi])
	copy(out[lo:hi], canon)
}

// windowSummary names the windows and their sizes for the encode's report.
func windowSummary(img *image, windows []codeWindow) string {
	byOff := make(map[uint64]string, len(img.Code))
	for _, s := range img.Code {
		byOff[s.Off] = s.Name
	}
	parts := make([]string, 0, len(windows))
	for _, w := range windows {
		parts = append(parts, fmt.Sprintf("%s %d B", byOff[w.Old.Off], w.Old.Size))
	}
	return strings.Join(parts, ", ")
}
