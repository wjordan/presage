// Command elfpredict measures a symbol-oracle structural predictor without
// allowing debug data or target bytes into the decoder. It is a feasibility
// benchmark, not a patch format.
package main

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/internal/cz"
)

type stageReport struct {
	CorrectBytes         int     `json:"correct_prediction_bytes"`
	CorrectPercent       float64 `json:"correct_prediction_percent"`
	CorrectionBytes      int     `json:"correction_bytes"`
	CorrectionZstd       int     `json:"correction_zstd_bytes"`
	PlanBytes            int     `json:"plan_bytes"`
	PlanZstd             int     `json:"plan_zstd_bytes"`
	TotalZstd            int     `json:"total_zstd_bytes"`
	PlanXZ               int     `json:"plan_xz_bytes,omitempty"`
	CorrectionXZ         int     `json:"correction_xz_bytes,omitempty"`
	CorrectionLZXZ       int     `json:"correction_lz_xz,omitempty"`
	CorrectionColumnarXZ int     `json:"correction_columnar_xz,omitempty"`
	CorrectionSplitXZ    int     `json:"correction_split_xz,omitempty"`
	CorrectionSplitPick  string  `json:"correction_split_pick,omitempty"`
	TotalXZ              int     `json:"total_xz_bytes,omitempty"`
	// JointXZ and JointBrotli are plan and correction as one stream: what a
	// patch of this prediction ships as, less a header, under xz and under
	// the Go-aware codec's compressor.
	JointXZ            int       `json:"joint_xz_bytes,omitempty"`
	JointBrotli        int       `json:"joint_brotli_bytes,omitempty"`
	VersusReferenceXZ  float64   `json:"versus_reference_xz_percent,omitempty"`
	VersusReferencePct float64   `json:"versus_reference_percent,omitempty"`
	Relocation         x86.Stats `json:"relocation"`
}

type oracleReport struct {
	WrongBytes      int `json:"wrong_prediction_bytes"`
	CorrectionBytes int `json:"correction_bytes"`
	CorrectionZstd  int `json:"correction_zstd_bytes"`
}

// wholeImageReport charges the hybrid for every byte of the ELF, not just
// .text, so that it can be compared with the whole-image incumbent patch
// without the scope favouring it.
type wholeImageReport struct {
	NewImageBytes               int         `json:"new_image_bytes"`
	NonTextBytes                int         `json:"non_text_bytes"`
	EquivalenceOnly             stageReport `json:"equivalence_only"`
	EquivalenceDerived          stageReport `json:"equivalence_derived_retargeting"`
	StructurallyRetargeted      stageReport `json:"structurally_retargeted"`
	PerFunctionSelection        stageReport `json:"per_function_selection"`
	EquivalenceRelocations      stageReport `json:"equivalence_relocations"`
	EquivalenceRelocationsByRow stageReport `json:"equivalence_relocations_byrow"`
	EquivalenceRelocationsSlots stageReport `json:"equivalence_relocations_slots"`
	StructuralRelocations       stageReport `json:"structural_relocations"`
	ProjectedRelocations        stageReport `json:"projected_relocations"`
	ModelledEhFrame             stageReport `json:"modelled_eh_frame"`
	ModelledRoData              stageReport `json:"modelled_rodata"`
	GoTables                    stageReport `json:"go_tables"`
	CorrectedFields             stageReport `json:"corrected_fields"`
	CorrectedFieldsGated        stageReport `json:"corrected_fields_gated"`
	SparsePlan                  stageReport `json:"sparse_plan"`
	Reloc                       relocStats  `json:"reloc_table"`
}

type combinedReport struct {
	EquivalenceCount       int               `json:"equivalence_count"`
	EquivalenceTextBytes   int               `json:"equivalence_text_bytes"`
	SelectedFunctions      int               `json:"selected_functions"`
	SelectedFunctionBytes  int               `json:"selected_function_bytes"`
	EquivalenceOnly        stageReport       `json:"equivalence_only"`
	EquivalenceDerived     stageReport       `json:"equivalence_derived_retargeting"`
	StructurallyRetargeted stageReport       `json:"structurally_retargeted"`
	PerFunctionSelection   stageReport       `json:"per_function_selection"`
	WholeImage             *wholeImageReport `json:"whole_image,omitempty"`
}

type report struct {
	Old                     string          `json:"old"`
	New                     string          `json:"new"`
	OldTextBytes            int             `json:"old_text_bytes"`
	NewTextBytes            int             `json:"new_text_bytes"`
	ReferencePatchBytes     int             `json:"reference_patch_bytes,omitempty"`
	TenXReferenceBytes      int             `json:"ten_x_reference_bytes,omitempty"`
	OldSymbols              symbolStats     `json:"old_symbols"`
	NewSymbols              symbolStats     `json:"new_symbols"`
	Matches                 matchStats      `json:"matches"`
	ReferencePoints         int             `json:"reference_points"`
	ChangedUnitStreamBytes  int             `json:"changed_unit_stream_bytes"`
	ChangedPredictionRefs   int             `json:"changed_prediction_references"`
	ChangedTargetRefs       int             `json:"changed_target_references"`
	StableRaw               stageReport     `json:"stable_raw_copy"`
	StableRelocated         stageReport     `json:"stable_relocated"`
	MappedRaw               stageReport     `json:"all_mapped_raw_copy"`
	MappedRelocated         stageReport     `json:"all_mapped_relocated"`
	PerfectStableOracle     oracleReport    `json:"oracle_perfect_stable_units"`
	PerfectMappedOracle     oracleReport    `json:"oracle_perfect_mapped_units"`
	PerfectFunctionsOracle  oracleReport    `json:"oracle_perfect_all_function_units"`
	Combined                *combinedReport `json:"combined,omitempty"`
	ConstructionElapsedSecs float64         `json:"construction_elapsed_seconds"`
}

func correctCount(a, b []byte) int {
	n := 0
	for i := range a {
		if a[i] == b[i] {
			n++
		}
	}
	return n
}

// smallestCorrection writes the correction in both shapes the production
// decoder reads and keeps the one that compresses smaller, as the Go-aware
// codec does from transform 2 on.
func smallestCorrection(pred, target []byte) ([]byte, error) {
	a, b, err := delta.CorrectionShapes(pred, target)
	if err != nil {
		return nil, err
	}
	z := xzSizes(a, b)
	if z[1] < z[0] {
		return b, nil
	}
	return a, nil
}

func measure(pred, target, planBytes []byte, stats x86.Stats, reference int, withXZ bool, secs map[string]section, disp *dispContext) (stageReport, []byte, error) {
	corr, err := smallestCorrection(pred, target)
	if err != nil {
		return stageReport{}, nil, err
	}
	check := append([]byte(nil), pred...)
	if err := delta.ApplyFlaggedCorrection(check, corr); err != nil {
		return stageReport{}, nil, err
	}
	if !bytes.Equal(check, target) {
		return stageReport{}, nil, fmt.Errorf("correction replay did not reproduce target")
	}
	correct := correctCount(pred, target)
	planZ := cz.CompressZstd(planBytes)
	corrZ := cz.CompressZstd(corr)
	r := stageReport{
		CorrectBytes:    correct,
		CorrectPercent:  100 * float64(correct) / float64(len(target)),
		CorrectionBytes: len(corr),
		CorrectionZstd:  len(corrZ),
		PlanBytes:       len(planBytes),
		PlanZstd:        len(planZ),
		TotalZstd:       len(planZ) + len(corrZ),
		Relocation:      stats,
	}
	if withXZ {
		// The four measurements are independent, and every xz call in them is
		// single-threaded (see xz.go), so they are overlapped. The sizes are
		// identical either way.
		var wg sync.WaitGroup
		var colErr, splitErr error
		var split int
		var pick string
		joint := append(append([]byte(nil), planBytes...), corr...)
		wg.Add(6)
		go func() { defer wg.Done(); r.JointXZ = xzSize(joint) }()
		go func() { defer wg.Done(); r.JointBrotli = brotliSize(joint) }()
		go func() { defer wg.Done(); r.PlanXZ = xzSize(planBytes) }()
		go func() { defer wg.Done(); r.CorrectionLZXZ = xzSize(corr) }()
		go func() { defer wg.Done(); r.CorrectionColumnarXZ, colErr = columnarXZ(pred, target, disp) }()
		go func() { defer wg.Done(); split, pick, splitErr = bestCorrectionXZ(pred, target, secs, disp) }()
		wg.Wait()
		if colErr != nil {
			return stageReport{}, nil, colErr
		}
		if splitErr != nil {
			return stageReport{}, nil, splitErr
		}
		r.CorrectionSplitXZ, r.CorrectionSplitPick = split, pick
		r.CorrectionXZ = min(r.CorrectionLZXZ, r.CorrectionColumnarXZ, split)
		planComponents(planBytes)
		r.TotalXZ = r.PlanXZ + r.CorrectionXZ
	}
	if reference > 0 {
		r.VersusReferencePct = 100 * float64(r.TotalZstd) / float64(reference)
		if r.TotalXZ > 0 {
			r.VersusReferenceXZ = 100 * float64(r.TotalXZ) / float64(reference)
		}
	}
	return r, corr, nil
}

func measureOracle(pred, target []byte) (oracleReport, []byte, error) {
	corr, err := smallestCorrection(pred, target)
	if err != nil {
		return oracleReport{}, nil, err
	}
	check := append([]byte(nil), pred...)
	if err := delta.ApplyFlaggedCorrection(check, corr); err != nil {
		return oracleReport{}, nil, err
	}
	if !bytes.Equal(check, target) {
		return oracleReport{}, nil, fmt.Errorf("oracle correction replay did not reproduce target")
	}
	return oracleReport{
		WrongBytes:      len(target) - correctCount(pred, target),
		CorrectionBytes: len(corr), CorrectionZstd: len(cz.CompressZstd(corr)),
	}, corr, nil
}

func writeFile(dir, name string, b []byte) error {
	if dir == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, name), b, 0o644)
}

func appendCanonical(codeStream, refStream []byte, body []byte) ([]byte, []byte, int) {
	canonical := append([]byte(nil), body...)
	refs := x86.References(body, 0)
	for _, ref := range refs {
		refStream = append(refStream, body[ref.Off:ref.Off+ref.N]...)
		clear(canonical[ref.Off : ref.Off+ref.N])
	}
	return append(codeStream, canonical...), refStream, len(refs)
}

func allMappedPlan(p predictionPlan) predictionPlan {
	p.Maps = slices.Clone(p.Maps)
	for i := range p.Maps {
		p.Maps[i].Copy = true
	}
	return p
}

func runCombined(externalPath string, oldImage, newImage *image, structure predictionPlan, structureBytes, structuralPred []byte, outDir string, reference int) (*combinedReport, planArtifacts, error) {
	t := startStage("equivalence parse")
	ep, err := buildEquivalencePlan(externalPath, oldImage, newImage, structure)
	if err != nil {
		return nil, planArtifacts{}, err
	}
	if noEquivalences {
		ep.Eqs, ep.SrcSkip, ep.SrcResidual, ep.DstSkip, ep.CopyLen = nil, nil, nil, nil, nil
	}
	if noTextEquivalences {
		ep.Eqs = slices.DeleteFunc(ep.Eqs, func(e equivalence) bool {
			return e.Dst < ep.NewText.Off+ep.NewText.Size && e.Dst+e.N > ep.NewText.Off
		})
		ep.SrcSkip, ep.DstSkip, ep.CopyLen = encodeColumns(ep.Eqs)
		ep.SrcResidual = nil
	}
	epBytes, err := ep.marshal(nil)
	if err != nil {
		return nil, planArtifacts{}, err
	}
	t.done("plan %d B", len(epBytes))

	rep := &combinedReport{}
	art := planArtifacts{Equivalence: epBytes, AllMapped: structureBytes}

	// The derived plan carries no function map, so serializing it is free, and
	// both -resume and the whole-image equivalence-derived rung need it on
	// disk. It is built whether or not its .text rung is measured.
	slimStructure := predictionPlan{
		OldAddr: structure.OldAddr, NewAddr: structure.NewAddr,
		TargetLen: structure.TargetLen, Ranges: slices.Clone(structure.Ranges),
	}
	slimBytes, err := slimStructure.marshal(oldImage.textBytes())
	if err != nil {
		return nil, planArtifacts{}, err
	}
	art.Derived = combinedPlan{Equivalences: epBytes, Structure: slimBytes}.marshal()

	if wantRung("text-ladder") {
		t = startStage("rung text-equivalence-only")
		eqPred, decoded, copied, err := predictEquivalences(oldImage.Data, epBytes)
		if err != nil {
			return nil, planArtifacts{}, err
		}
		eqReport, eqCorr, err := measure(eqPred, newImage.textBytes(), epBytes, x86.Stats{}, reference, true, nil, nil)
		if err != nil {
			return nil, planArtifacts{}, err
		}
		if err := writeFile(outDir, "equivalence-only.correction", eqCorr); err != nil {
			return nil, planArtifacts{}, err
		}
		rep.EquivalenceCount, rep.EquivalenceTextBytes = len(decoded.Eqs), copied
		rep.EquivalenceOnly = eqReport
		t.done("%d compressed bytes", eqReport.TotalZstd)

		t = startStage("rung text-equivalence-derived")
		derivedPred, derivedStats, err := predictCombined(oldImage.Data, art.Derived)
		if err != nil {
			return nil, planArtifacts{}, err
		}
		derivedReport, derivedCorr, err := measure(derivedPred, newImage.textBytes(), art.Derived, derivedStats.Relocation, reference, true, nil, nil)
		if err != nil {
			return nil, planArtifacts{}, err
		}
		if err := writeFile(outDir, "equivalence-derived.correction", derivedCorr); err != nil {
			return nil, planArtifacts{}, err
		}
		rep.EquivalenceDerived = derivedReport
		t.done("%d compressed bytes", derivedReport.TotalZstd)
	}

	// Structural retargeting is not optional at any -rungs setting: its
	// prediction is what the per-function selector scores the structural
	// prediction against, and every later rung inherits the choice bits.
	art.Retarget = combinedPlan{Equivalences: epBytes, Structure: structureBytes, GoTables: goTablesPlan}.marshal()
	t = startStage("predict text-retargeted")
	retargetPred, retargetStats, err := predictCombined(oldImage.Data, art.Retarget)
	if err != nil {
		return nil, planArtifacts{}, err
	}
	t.done("")
	if wantRung("text-ladder") {
		t = startStage("rung text-structurally-retargeted")
		retargetReport, retargetCorr, err := measure(retargetPred, newImage.textBytes(), art.Retarget, retargetStats.Relocation, reference, true, nil, nil)
		if err != nil {
			return nil, planArtifacts{}, err
		}
		if err := writeFile(outDir, "equivalence-retarget.correction", retargetCorr); err != nil {
			return nil, planArtifacts{}, err
		}
		rep.StructurallyRetargeted = retargetReport
		t.done("%d compressed bytes", retargetReport.TotalZstd)
	}

	t = startStage("per-function selection")
	choices, selectedFunctions, selectedBytes := chooseStructuralFunctions(retargetPred, structuralPred, newImage.textBytes(), structure)
	art.Selected = combinedPlan{Equivalences: epBytes, Structure: structureBytes, Choices: choices, GoTables: goTablesPlan}.marshal()
	t.done("%d functions / %d bytes chosen", selectedFunctions, selectedBytes)
	if onlyProbes["selscatter"] {
		// One row per mapped function: wrong bytes under each prediction, and the size.
		var b bytes.Buffer
		tt := newImage.textBytes()
		for _, m := range structure.Maps {
			body := tt[m.Dst : m.Dst+m.DstSize]
			fmt.Fprintf(&b, "%d\t%d\t%d\n", wrongCount(retargetPred[m.Dst:m.Dst+m.DstSize], body), wrongCount(structuralPred[m.Dst:m.Dst+m.DstSize], body), m.DstSize)
		}
		if err := writeFile(dumpDir, "selection.tsv", b.Bytes()); err != nil {
			return nil, planArtifacts{}, err
		}
	}

	if wantRung("text-ladder") {
		// The replay check on the choice bits lives here. Without this rung the
		// whole-image rungs still prove the same plan decodes byte-exactly,
		// which is the stronger statement; only the count is unchecked.
		t = startStage("rung text-per-function-selection")
		selectedPred, selectedStats, err := predictCombined(oldImage.Data, art.Selected)
		if err != nil {
			return nil, planArtifacts{}, err
		}
		if selectedStats.SelectedFunctions != selectedFunctions || selectedStats.SelectedBytes != selectedBytes {
			return nil, planArtifacts{}, errors.New("combined selection replay disagrees with encoder")
		}
		selectedReport, selectedCorr, err := measure(selectedPred, newImage.textBytes(), art.Selected, selectedStats.Relocation, reference, true, nil, nil)
		if err != nil {
			return nil, planArtifacts{}, err
		}
		if err := writeFile(outDir, "equivalence-selected.correction", selectedCorr); err != nil {
			return nil, planArtifacts{}, err
		}
		rep.PerFunctionSelection = selectedReport
		t.done("%d compressed bytes", selectedReport.TotalZstd)
		fmt.Fprintf(os.Stderr, "equivalence ladder: only=%d derived=%d retargeted=%d selected=%d compressed bytes; selected %d functions / %d bytes\n",
			rep.EquivalenceOnly.TotalZstd, rep.EquivalenceDerived.TotalZstd, rep.StructurallyRetargeted.TotalZstd,
			rep.PerFunctionSelection.TotalZstd, selectedFunctions, selectedBytes)
	}
	rep.SelectedFunctions, rep.SelectedFunctionBytes = selectedFunctions, selectedBytes
	return rep, art, nil
}

// rung names a whole-image measurement and the serialized plan that produces it.
type rung struct {
	name string
	plan []byte
}

// notes records the diagnostics a rung-plan build prints, so that a memo hit
// can reprint them rather than silently dropping them.
type notesBuf struct{ strings.Builder }

func (n *notesBuf) printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	n.WriteString(line)
	fmt.Fprint(os.Stderr, line)
}

// buildRungPlans is everything runWholeImage has to do before it can measure
// anything: the equivalence-source rewrite, the relocation projection, the
// eh_frame and rodata models and the field-fix layer, each an encoder-side
// pass over the whole 291 MB image. On Chrome it is ~52 s, and it is a pure
// function of the five serialized plans, the requested rungs and the codec --
// so memoRungPlans wraps it.
func buildRungPlans(oldImage, newImage *image, ep equivalencePlan, structure predictionPlan,
	epBytes, structureBytes, choices, derivedPlan, retargetPlan, selectedPlan []byte) ([]rung, string, error) {
	target := newImage.Data
	var notes notesBuf
	// The relocation column diagnostics are printed from inside the codec;
	// routing them through notes means a memo hit reprints them too.
	prevDiag := relocDiag
	relocDiag = notes.printf
	defer func() { relocDiag = prevDiag }()
	// Every rung that carries the full map can write its equivalence sources
	// against it. The two rungs below that point cannot, and keep the plan as
	// it arrived.
	epMapped := epBytes
	if len(structure.Maps) != 0 {
		t := startStage("equivalence sources")
		pred := &srcPredictor{maps: structure.Maps, oldOff: ep.OldText.Off, newOff: ep.NewText.Off, newSize: ep.NewText.Size}
		if b, err := ep.marshal(pred); err != nil {
			fmt.Fprintf(os.Stderr, "predicted equivalence sources FAILED: %v\n", err)
		} else {
			if onlyProbes == nil {
				notes.printf("equivalence sources against the function map: %d -> %d xz\n", xzSize(epBytes), xzSize(b))
			}
			epMapped = b
		}
		if onlyProbes["eqsrc"] {
			probeEquivalenceSources(ep, pred)
		}
		t.done("")
	}
	rungs := []rung{
		{"equivalence-only", combinedPlan{Equivalences: epBytes}.marshal()},
		{"equivalence-derived", derivedPlan},
		{"structurally-retargeted", withEquivalences(retargetPlan, epMapped)},
		{"per-function-selection", withEquivalences(selectedPlan, epMapped)},
	}
	// Each structural layer below is built only when the sections it models
	// exist in both images; a Go binary has no .rela.dyn or .eh_frame, and its
	// rungs simply carry no plan for them. The decoder treats every sub-plan
	// as optional.
	if wantLateRungs() {
		base := relocPlan{OldSecs: newSectionMap(oldImage.Sections), NewSecs: newSectionMap(newImage.Sections)}
		if goTablesPlan != nil {
			// The Go-table plan describes both images' sections already.
			o, n, err := goGeometry(oldImage.Data, goTablesPlan)
			if err != nil {
				return nil, "", err
			}
			base.OldSecs, base.NewSecs, base.DerivedGeometry = o, n, true
		}
		oldRela, okOld := relaSection(oldImage.Sections)
		newRela, okNew := relaSection(newImage.Sections)
		haveRela := okOld && okNew
		if haveRela {
			// A table with no relative entries (a static Go binary's PLT
			// relocations) has nothing for the layer to rebuild.
			rel, _, _ := parseRela(newImage.Data[newRela.Off : newRela.Off+newRela.Size])
			haveRela = len(rel) != 0
		}
		rp := base
		var relocBytes []byte
		if haveRela {
			base.OldOff, base.OldSize, base.NewOff, base.NewSize = oldRela.Off, oldRela.Size, newRela.Off, newRela.Size
			t := startStage("relocation plan")
			notes.printf("relocation oracle: structural (reference points, function map, then equivalences)\n")
			var err error
			rp, err = buildRelocPlan(oldImage.Data, newImage.Data, base, newPointerOracle(ep, structure, &base))
			if err != nil {
				return nil, "", err
			}
			t.done("")
			notes.printf("relocation columns: gap correction %d B, addend correction %d B, tail correction %d B\n",
				len(rp.GapCorrection), len(rp.AddendCorrection), len(rp.TailCorrection))
			relocBytes = rp.marshal()
		} else {
			// No table to rebuild, but the later layers still need the section
			// geometry this plan carries.
			notes.printf("no .rela.dyn in both images: relocation plan carries section geometry only\n")
			relocBytes = base.marshal()
		}
		// The rung between the two: everything the projected-relocation rung
		// has except the per-function choice. An empty choice stream leaves
		// the decoder's selection loop unreached, so every mapped function
		// keeps its structurally retargeted body.
		rungs = append(rungs, rung{"structural-relocations", combinedPlan{
			Equivalences: epMapped, Structure: structureBytes, Reloc: relocBytes, GoTables: goTablesPlan}.marshal()})
		rungs = append(rungs, rung{"projected-relocations", combinedPlan{
			Equivalences: epMapped, Structure: structureBytes, Choices: choices, Reloc: relocBytes, GoTables: goTablesPlan}.marshal()})

		// The Go-table module: the Go-aware codec's metadata regeneration
		// (pclntab, type descriptors, data maps) as one layer. Absent for
		// anything that is not a Go binary it understands.
		goTables := goTablesPlan
		if goTables != nil {
			notes.printf("go tables: plan %d B\n", len(goTables))
		}
		rungs = append(rungs, rung{"go-tables", combinedPlan{
			Equivalences: epMapped, Structure: structureBytes, Choices: choices, Reloc: relocBytes, GoTables: goTables,
		}.marshal()})

		// The fair fight: Zucchini's method assembled from this project's
		// parts. Whole-image equivalences, every reference retargeted through
		// the equivalence projection, and the relocation table rebuilt by the
		// projected-relocation plan under that same projection. It carries the
		// derived plan's slim structure -- section shift ranges only -- so no
		// function map, no reference points and no per-function selection
		// reach either the retargeting or the relocation oracle.
		//
		// The by-row variant is the same rung with one column predicted the
		// old way: addends paired by position rather than joined on the slot.
		// It is the measurement behind §3.3, so it is kept runnable.
		//
		// The slots variant goes one step further back, to what upstream
		// Zucchini actually does: the slot is the only part of an entry it
		// models, and r_addend is left to the byte correction.
		if wantRung("equivalence-relocations") || wantRung("equivalence-relocations-byrow") ||
			wantRung("equivalence-relocations-slots") {
			derived, err := unmarshalCombinedPlan(derivedPlan)
			if err != nil {
				return nil, "", err
			}
			slim, err := unmarshalPlanFile(derived.Structure, oldImage.Data, oldImage.Text, goMapDeriver(oldImage.Data, goTablesPlan, oldImage.Text, newImage.Text))
			if err != nil {
				return nil, "", err
			}
			for _, v := range []struct {
				name      string
				pairing   string // stage-name tag
				note      string // how the addend column is handled
				pairByRow bool
				noAddends bool
			}{
				{"equivalence-relocations", "slot join", "addends paired by slot join", false, false},
				{"equivalence-relocations-byrow", "row index", "addends paired by row index", true, false},
				{"equivalence-relocations-slots", "slots only", "no addend column: addend bytes stay as the equivalence copy left them", false, true},
			} {
				if !wantRung(v.name) {
					continue
				}
				var eqReloc []byte
				if haveRela {
					eb := base
					eb.PairByRow, eb.NoAddends = v.pairByRow, v.noAddends
					t := startStage("relocation plan (equivalence oracle, " + v.pairing + ")")
					notes.printf("relocation oracle: equivalence map only (no function map, no reference points); %s\n", v.note)
					erp, err := buildRelocPlan(oldImage.Data, newImage.Data, eb, newPointerOracle(ep, slim, &eb))
					if err != nil {
						return nil, "", err
					}
					t.done("")
					notes.printf("%s columns: gap correction %d B, addend correction %d B, tail correction %d B; reloc plan xz %d\n",
						v.name, len(erp.GapCorrection), len(erp.AddendCorrection), len(erp.TailCorrection), xzSize(erp.marshal()))
					eqReloc = erp.marshal()
				}
				rungs = append(rungs, rung{v.name, combinedPlan{
					Equivalences: derived.Equivalences, Structure: derived.Structure, Reloc: eqReloc,
				}.marshal()})
			}
		}

		var dwarfBytes []byte
		pointer := newPointerOracle(ep, structure, &rp)
		addrMap := func(addr uint64) (uint64, bool) {
			t := pointer(addr)
			return t.Addr, t.Known
		}
		// Fixed-record sections keep their tables beside equivalences: an
		// inserted symbol or address puts every later field between two
		// equivalences, where only the table says which row it is.
		withRecords := func(k int) bool {
			s, ok := newImage.Debug[dwarfSecNames[k]]
			return ok && (!eqCovers(ep, s.Off, s.Size) || k == dwSymtab || k == dwAddr || k == dwStrtab || k == dwFrame)
		}
		if dp, ok := buildDwarfPlan(oldImage, newImage, ep, withRecords, addrMap); ok && !noDwarf {
			dwarfBytes = dp.Marshal()
			paired := 0
			for _, u := range dp.Records[dwInfo] {
				if u.NewLen != 0 {
					paired++
				}
			}
			notes.printf("dwarf: %d of %d old units paired, plan %d B\n", paired, len(dp.Records[dwInfo]), len(dwarfBytes))
		}

		var ehBytes []byte
		oldEh, okOldEh := oldImage.Sections[".eh_frame"]
		newEh, okNewEh := newImage.Sections[".eh_frame"]
		newHdr, okHdr := newImage.Sections[".eh_frame_hdr"]
		if okOldEh && okNewEh && okHdr {
			ehBytes = ehFramePlan{
				OldOff: oldEh.Off, OldSize: oldEh.Size, NewOff: newEh.Off, NewSize: newEh.Size,
				OldAddr: oldEh.Addr, NewAddr: newEh.Addr,
				HdrOff: newHdr.Off, HdrSize: newHdr.Size, HdrAddr: newHdr.Addr,
			}.marshal()
		} else {
			notes.printf("no .eh_frame/.eh_frame_hdr in both images: unwind layer omitted\n")
		}
		ehPlan := combinedPlan{
			Equivalences: epMapped, Structure: structureBytes, Choices: choices,
			Reloc: relocBytes, EhFrame: ehBytes, GoTables: goTables, Dwarf: dwarfBytes,
		}.marshal()
		rungs = append(rungs, rung{"modelled-eh-frame", ehPlan})

		var roBytes []byte
		oldRo, okOldRo := oldImage.Sections[".rodata"]
		newRo, okNewRo := newImage.Sections[".rodata"]
		// The .rodata layer models what the equivalences got wrong there; on
		// a Go binary the module wrote the section and the layer has nothing
		// to select (0 of 0 spans on every pair measured).
		if okOldRo && okNewRo && goTablesPlan == nil {
			rd := roDataPlan{
				OldOff: oldRo.Off, OldSize: oldRo.Size, NewOff: newRo.Off, NewSize: newRo.Size,
				OldAddr: oldRo.Addr, NewAddr: newRo.Addr,
				TextLo: oldImage.Text.Addr, TextHi: oldImage.Text.Addr + oldImage.Text.Size,
			}
			// The short candidates need the target to decide, so the
			// encoder predicts once without them and asks which help.
			t := startStage("rodata selection")
			if pred, _, err := predictImage(oldImage.Data, ehPlan); err != nil {
				fmt.Fprintf(os.Stderr, "rodata selection FAILED: %v\n", err)
			} else {
				var sel roDataStats
				rd.Keep, sel = selectRoDataTables(pred, oldImage.Data, newImage.Data, rd,
					newSourceEquivalenceMapper(ep), newPointerOracle(ep, structure, &rp))
				notes.printf("rodata selection: %d of %d spans kept (%d self-relative, %d rebased), bitmap %d B\n",
					sel.Tables, sel.Candidates, sel.SelfRel, sel.Rebased, len(rd.Keep))
			}
			t.done("")
			roBytes = rd.marshal()
		}
		roPlan := combinedPlan{
			Equivalences: epMapped, Structure: structureBytes, Choices: choices,
			Reloc: relocBytes, EhFrame: ehBytes, RoData: roBytes, GoTables: goTables, Dwarf: dwarfBytes,
		}.marshal()
		rungs = append(rungs, rung{"modelled-rodata", roPlan})

		// The field layers need the finished prediction to score
		// against, so the encoder predicts the rung above and then
		// corrects it.
		if !wantRung("corrected-fields") && !wantRung("corrected-fields-gated") {
			// Both the extra prediction and the two walks of 8.7
			// million field sites below exist only for these rungs.
		} else if pred, _, err := timedPredictImage("predict for field fix", oldImage.Data, roPlan); err != nil {
			fmt.Fprintf(os.Stderr, "field fix FAILED: %v\n", err)
		} else {
			nt := newImage.Text
			for _, v := range []struct {
				name string
				gate bool
			}{{"corrected-fields", false}, {"corrected-fields-gated", true}} {
				if !wantRung(v.name) {
					continue
				}
				t := startStage("field fix " + v.name)
				fx, fst := encodeFieldFix(pred[nt.Off:nt.Off+nt.Size], target[nt.Off:nt.Off+nt.Size], nt.Addr, structure.Maps, v.gate)
				t.done("%d sites", fst.Sites)
				notes.printf("field fix (%s): %d sites, domain %d; %d remaps kept (%d rejected) rewriting %d fields, %d field deltas, %d declined\n",
					v.name, fst.Sites, fst.Domain, fst.Remaps, fst.Skipped, fst.Remade, fst.Deltas, fst.Ungated)
				rungs = append(rungs, rung{v.name, combinedPlan{
					Equivalences: epMapped, Structure: structureBytes, Choices: choices,
					Reloc: relocBytes, EhFrame: ehBytes, RoData: roBytes, GoTables: goTables, Dwarf: dwarfBytes,
					Fields: fx.marshal(),
				}.marshal()})
			}
		}

		if wantRung("sparse-plan") {
			sparse := sparseStructure(structure, choices, oldImage.Text.Size)
			sparseBytes, err := sparse.marshal(oldImage.textBytes())
			if err != nil {
				fmt.Fprintf(os.Stderr, "sparse plan FAILED to serialize: %v\n", err)
				sparseBytes = nil
			}
			// Every mapping the sparse plan retains is one the selector chose.
			sparseChoices := make([]byte, (len(sparse.Maps)+7)/8)
			for i := range sparse.Maps {
				sparseChoices[i/8] |= 1 << (i % 8)
			}
			notes.printf("sparse plan: %d shift ranges, %d retained mappings (from %d), structure %d -> %d B\n",
				len(sparse.Ranges), len(sparse.Maps), len(structure.Maps), len(structureBytes), len(sparseBytes))
			if sparseBytes != nil {
				// The equivalence sources are written against the function map
				// in the same plan, and this plan's map is a different, smaller
				// one -- so it needs its own encoding, not the dense rung's.
				epSparse := epBytes
				if len(sparse.Maps) != 0 {
					sp := &srcPredictor{maps: sparse.Maps, oldOff: ep.OldText.Off, newOff: ep.NewText.Off, newSize: ep.NewText.Size}
					if b, err := ep.marshal(sp); err != nil {
						fmt.Fprintf(os.Stderr, "sparse equivalence sources FAILED: %v\n", err)
					} else {
						epSparse = b
					}
				}
				rungs = append(rungs, rung{"sparse-plan", combinedPlan{
					Equivalences: epSparse, Structure: sparseBytes, Choices: sparseChoices,
					Reloc: relocBytes, EhFrame: ehBytes, RoData: roBytes, GoTables: goTables, Dwarf: dwarfBytes,
				}.marshal()})
			}
		}
	}
	return rungs, notes.String(), nil
}

// memoRungPlans memoises buildRungPlans. The key covers the codec, the five
// plans it reads and the rung set asked for; the eqsrc probe reports from
// inside the build, so a run asking for it skips the memo.
func memoRungPlans(oldImage, newImage *image, ep equivalencePlan, structure predictionPlan,
	epBytes, structureBytes, choices, derivedPlan, retargetPlan, selectedPlan []byte) ([]rung, error) {
	rungNames := "all"
	if onlyRungs != nil {
		rungNames = strings.Join(slices.Sorted(maps.Keys(onlyRungs)), ",")
	}
	h := sha256.New()
	for _, b := range [][]byte{epBytes, structureBytes, choices, derivedPlan, retargetPlan, selectedPlan, goTablesPlan} {
		binary.Write(h, binary.LittleEndian, uint64(len(b)))
		h.Write(b)
	}
	key := memoKey("rung-plans", codecCode(), hexString(h.Sum(nil)), rungNames)
	use := !onlyProbes["eqsrc"]
	if use {
		if b, ok := memoLoad(key); ok {
			if rungs, notes, ok := unpackRungs(b); ok {
				startStage("rung plans").done("memo hit; %d rungs", len(rungs))
				fmt.Fprint(os.Stderr, notes)
				return rungs, nil
			}
		}
	}
	rungs, notes, err := buildRungPlans(oldImage, newImage, ep, structure,
		epBytes, structureBytes, choices, derivedPlan, retargetPlan, selectedPlan)
	if err != nil {
		return nil, err
	}
	if use {
		memoStore(key, packRungs(rungs, notes))
	}
	return rungs, nil
}

func packRungs(rungs []rung, notes string) []byte {
	b := binary.AppendUvarint(nil, uint64(len(rungs)))
	for _, r := range rungs {
		b = binary.AppendUvarint(b, uint64(len(r.name)))
		b = append(b, r.name...)
		b = binary.AppendUvarint(b, uint64(len(r.plan)))
		b = append(b, r.plan...)
	}
	return append(binary.AppendUvarint(b, uint64(len(notes))), notes...)
}

func unpackRungs(b []byte) ([]rung, string, bool) {
	next := func() ([]byte, bool) {
		v, k := binary.Uvarint(b)
		if k <= 0 || v > uint64(len(b)-k) {
			return nil, false
		}
		b = b[k:]
		out := b[:v]
		b = b[v:]
		return out, true
	}
	n, k := binary.Uvarint(b)
	if k <= 0 || n > 1<<10 {
		return nil, "", false
	}
	b = b[k:]
	rungs := make([]rung, 0, n)
	for range n {
		name, ok1 := next()
		plan, ok2 := next()
		if !ok1 || !ok2 {
			return nil, "", false
		}
		rungs = append(rungs, rung{string(name), plan})
	}
	notes, ok := next()
	return rungs, string(notes), ok && len(b) == 0
}

// runWholeImage replays the same four rungs against every byte of the image.
// The plans are unchanged: only the prediction buffer grows from .text to the
// whole file, so the extra cost is exactly the cost of the regions the .text
// scope was omitting.
func runWholeImage(oldImage, newImage *image, epBytes, structureBytes, choices, derivedPlan, retargetPlan, selectedPlan []byte, outDir string, reference int) (*wholeImageReport, error) {
	target := newImage.Data
	// The function map is decoded once: it now walks the old image to build
	// the reference-target domain of §9.12, which is not something to repeat.
	t := startStage("decode function map")
	structure, err := unmarshalPlanFile(structureBytes, oldImage.Data, oldImage.Text, goMapDeriver(oldImage.Data, goTablesPlan, oldImage.Text, newImage.Text))
	if err != nil {
		return nil, err
	}
	ep, err := unmarshalEquivalencePlan(epBytes)
	if err != nil {
		return nil, err
	}
	t.done("%d mappings", len(structure.Maps))
	rungs, err := memoRungPlans(oldImage, newImage, ep, structure, epBytes, structureBytes, choices, derivedPlan, retargetPlan, selectedPlan)
	if err != nil {
		return nil, err
	}
	// The sidecar rung is the corrected-fields plan with its map columns
	// replaced by a delta against a symbol table the client carries. It is
	// built here rather than in buildRungPlans so that the symbol tables it
	// reads are not folded into that stage's memo key.
	if wantRung("sidecar-map") {
		i := slices.IndexFunc(rungs, func(r rung) bool { return r.name == "corrected-fields" })
		if i < 0 {
			fmt.Fprintln(os.Stderr, "sidecar-map needs the corrected-fields rung in -rungs")
		} else if plan, err := buildSidecarRung(rungs[i].plan, oldImage, newImage, structure, outDir); err != nil {
			fmt.Fprintf(os.Stderr, "sidecar-map FAILED: %v\n", err)
		} else {
			rungs = append(rungs, rung{"sidecar-map", plan})
		}
	}
	// The derived-map rung is the same replacement with no carried file: the
	// enumeration the delta is keyed against is derived from the old image on
	// both sides, so the plan decodes under the plain "old binary + patch"
	// contract.
	if wantRung("derived-map") {
		i := slices.IndexFunc(rungs, func(r rung) bool { return r.name == "corrected-fields" })
		if i < 0 {
			fmt.Fprintln(os.Stderr, "derived-map needs the corrected-fields rung in -rungs")
		} else if plan, err := buildDerivedRung(rungs[i].plan, oldImage, newImage, structure); err != nil {
			fmt.Fprintf(os.Stderr, "derived-map FAILED: %v\n", err)
		} else {
			rungs = append(rungs, rung{"derived-map", plan})
		}
	}
	// The attribution walk needs function spans; take them from the dense map,
	// which every measured rung ships.
	attributionMaps := structure.Maps
	// §14's displacement column walks function bodies, and the decoder has
	// them here: the function map is decoded above, before any correction is
	// applied. Both rungs share the correction, so both see the change.
	var dispCtx *dispContext
	if dispColumn {
		dispCtx = newDispContext(structure.Maps, newImage.Text, len(target))
		fmt.Fprintf(os.Stderr, "dispcol: %d bodies, %d distinct function starts\n", len(dispCtx.bodies), len(dispCtx.starts))
	}
	var correctedPrediction []byte
	if onlyProbes == nil {
		t = startStage("plan column diagnostics")
		planColumnCost(structure)
		t.done("")
	}
	out := &wholeImageReport{
		NewImageBytes: len(target),
		NonTextBytes:  len(target) - int(newImage.Text.Size),
	}
	into := map[string]*stageReport{
		"equivalence-only": &out.EquivalenceOnly, "equivalence-derived": &out.EquivalenceDerived,
		"structurally-retargeted": &out.StructurallyRetargeted, "per-function-selection": &out.PerFunctionSelection,
		"equivalence-relocations":       &out.EquivalenceRelocations,
		"equivalence-relocations-byrow": &out.EquivalenceRelocationsByRow,
		"equivalence-relocations-slots": &out.EquivalenceRelocationsSlots,
		"structural-relocations":        &out.StructuralRelocations,
		"projected-relocations":         &out.ProjectedRelocations, "modelled-eh-frame": &out.ModelledEhFrame,
		"modelled-rodata": &out.ModelledRoData, "go-tables": &out.GoTables, "corrected-fields": &out.CorrectedFields,
		"corrected-fields-gated": &out.CorrectedFieldsGated, "sparse-plan": &out.SparsePlan,
	}
	for _, r := range rungs {
		if onlyRungs != nil && !onlyRungs[r.name] {
			continue
		}
		// A rung that fails is reported and skipped rather than aborting the
		// run: the later rungs are experiments, and losing the measured ones
		// to an unmeasured one wastes the whole pass.
		pred, stats, err := timedPredictImage("predict "+r.name, oldImage.Data, r.plan)
		if err != nil {
			fmt.Fprintf(os.Stderr, "whole image %-24s FAILED: %v\n", r.name, err)
			continue
		}
		if onlyProbes != nil {
			t = startStage("probes " + r.name)
			runProbes(r.name, r.plan, pred, target, oldImage, newImage, structureBytes, structure)
			t.done("")
			continue
		}
		// The sidecar rung must be the same prediction as the rung it
		// replaces: only the encoding of the map changed, so any difference is
		// a decoder bug, not a result.
		switch r.name {
		case "corrected-fields":
			correctedPrediction = pred
		case "sidecar-map":
			if correctedPrediction == nil {
				fmt.Fprintln(os.Stderr, "sidecar-map: no corrected-fields prediction to compare against")
			} else if !bytes.Equal(pred, correctedPrediction) {
				return nil, errors.New("sidecar-map prediction differs from corrected-fields")
			} else {
				fmt.Fprintln(os.Stderr, "sidecar-map: prediction is byte-identical to corrected-fields")
			}
		case "derived-map":
			if correctedPrediction == nil {
				fmt.Fprintln(os.Stderr, "derived-map: no corrected-fields prediction to compare against")
			} else if !bytes.Equal(pred, correctedPrediction) {
				return nil, errors.New("derived-map prediction differs from corrected-fields")
			} else {
				fmt.Fprintln(os.Stderr, "derived-map: prediction is byte-identical to corrected-fields")
			}
		}
		t = startStage("measure " + r.name)
		rep, corr, err := measure(pred, target, r.plan, stats.Relocation, reference, true, newImage.Sections, dispCtx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "whole image %-24s FAILED: %v\n", r.name, err)
			continue
		}
		t.done("plan %d + correction %d = %d XZ", rep.PlanXZ, rep.CorrectionXZ, rep.TotalXZ)
		if err := writeFile(outDir, "whole-image-"+r.name+".correction", corr); err != nil {
			return nil, err
		}
		if r := into[r.name]; r != nil {
			*r = rep
		}
		out.Reloc = stats.Reloc
		if stats.EhFrame.FDEs != 0 {
			reportEhFrame(stats.EhFrame)
		}
		if stats.RoData.Tables != 0 {
			reportRoData(stats.RoData)
		}
		if r.name == "corrected-fields" {
			t = startStage("attribute " + r.name)
			attributeCorrection(pred, target, newImage.Sections, newImage.Text, attributionMaps)
			t.done("")
		}
		fmt.Fprintf(os.Stderr, "whole image %-24s %.3f%% correct, plan %d + correction %d = %d XZ bytes (%.2f%% of reference); joint xz %d, brotli %d; correction lz %d, columnar %d, split %d (%s); refs %d, unknown %d, nofit %d\n",
			r.name, rep.CorrectPercent, rep.PlanXZ, rep.CorrectionXZ, rep.TotalXZ, rep.VersusReferenceXZ,
			rep.JointXZ, rep.JointBrotli,
			rep.CorrectionLZXZ, rep.CorrectionColumnarXZ, rep.CorrectionSplitXZ, rep.CorrectionSplitPick,
			stats.Relocation.Refs, stats.Relocation.Unknown, stats.Relocation.NoFit)
	}
	return out, nil
}

// replayWholeImage runs the whole-image rungs from a set of serialized plans,
// whatever produced them: -resume, the stage memo, or the construction that
// just finished. Every plan they consume is symbol-derived and already
// serialized, so the symbol parsing and matching that produced them does not
// have to run again to test a new encoding.
func replayWholeImage(a planArtifacts, oldImage, newImage *image, outDir string, reference int) (*wholeImageReport, error) {
	selected, err := unmarshalCombinedPlan(a.Selected)
	if err != nil {
		return nil, err
	}
	return runWholeImage(oldImage, newImage, a.Equivalence, a.AllMapped, selected.Choices,
		a.Derived, a.Retarget, a.Selected, outDir, reference)
}

// resumeWholeImage replays the whole-image rungs from a previous run's
// artifact directory.
func resumeWholeImage(oldPath, newPath, resume, outDir string, reference int) error {
	if oldPath == "" || newPath == "" {
		return fmt.Errorf("-old and -new are required with -resume")
	}
	art, err := readPlanArtifacts(resume)
	if err != nil {
		return err
	}
	oldImage, newImage, err := loadImages(oldPath, newPath)
	if err != nil {
		return err
	}
	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
	}
	_, err = replayWholeImage(art, oldImage, newImage, outDir, reference)
	reportTotals()
	return err
}

// loadImages reads both release binaries, timed: on a cold page cache this is
// 580 MB off disk and is worth telling apart from the work that follows.
func loadImages(oldPath, newPath string) (*image, *image, error) {
	t := startStage("load old image")
	oldImage, err := loadImage(oldPath)
	if err != nil {
		return nil, nil, err
	}
	t.done("%d B", len(oldImage.Data))
	t = startStage("load new image")
	newImage, err := loadImage(newPath)
	if err != nil {
		return nil, nil, err
	}
	t.done("%d B", len(newImage.Data))
	return oldImage, newImage, nil
}

func timedPredictImage(name string, old, plan []byte) ([]byte, combinedStats, error) {
	t := startStage(name)
	pred, stats, err := predictImage(old, plan)
	t.done("")
	return pred, stats, err
}

// onlyRungs and onlyProbes keep an experiment's cost proportional to the
// question it asks. A rung costs an xz of a multi-megabyte correction; a probe
// costs a prediction and an xz of a few small columns. Narrowing to the rung a
// hypothesis lives on, and to reporting rather than measuring, is the
// difference between a three-minute answer and a half-hour one.
var onlyRungs, onlyProbes map[string]bool

// goTablesPlan is the Go-table module's plan for the pair, nil when the
// module does not apply. Built once, before matching, because the function
// map is derived from it.
var goTablesPlan []byte

// noEquivalences keeps the equivalence plan's geometry and drops its
// equivalences, to price what the map layer adds over the other layers.
var noEquivalences bool

// dispColumn selects §14's correction variant. Off, the correction is exactly
// the format that shipped; on, the last bucket's PC-relative fields move into
// columns of their own and the decoder walks the repaired bytes to put them
// back. See dispfield.go.
var dispColumn bool

// noDwarf keeps the DWARF layer out, to price it.
var noDwarf bool

// noTextEquivalences drops the equivalences that write into .text and
// keeps the rest: the Go code model owns .text, the stream owns the data
// and debug sections.
var noTextEquivalences bool

// noGoText keeps the Go-table module out of .text even when there are no
// equivalences, to price its code model against the structural one.
var noGoText bool

// noPoints drops the inferred reference points from a Go-derived plan, to
// price them against the module's address prior.
var noPoints bool

var dictProbeWindow int

// dumpDir receives each rung's whole-image prediction under the "dump"
// probe, for external per-region analysis.
var dumpDir string

// oldDebugPath and newDebugPath are the -old-debug/-new-debug arguments,
// kept for probes that read the symbol tables themselves.
var oldDebugPath, newDebugPath string

func wantRung(name string) bool { return onlyRungs == nil || onlyRungs[name] }

// wantLateRungs reports whether anything downstream of the plain retargeting
// rungs is wanted. Everything from the relocation plan on is built eagerly and
// costs minutes, so a run aimed at an early rung should not pay for it.
func wantLateRungs() bool {
	for _, n := range []string{"projected-relocations", "equivalence-relocations", "equivalence-relocations-byrow",
		"equivalence-relocations-slots", "structural-relocations",
		"go-tables", "modelled-eh-frame", "modelled-rodata", "corrected-fields", "corrected-fields-gated", "sparse-plan",
		"sidecar-map", "derived-map"} {
		if wantRung(n) {
			return true
		}
	}
	return false
}

func commaSet(s string) map[string]bool {
	if s == "" {
		return nil
	}
	out := map[string]bool{}
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out[f] = true
		}
	}
	return out
}

// historyRungs are the measurements that exist only to reproduce the
// scoreboard in the record. Each one xz-compresses a 30-64 MB correction --
// "stable raw copy" alone is 64 MB -- and nothing downstream reads their
// output. They are off unless asked for, which is what turns the default run
// from a scoreboard replay into a measurement of the headline.
//
//	stable-raw, stable-relocated,
//	all-mapped-raw, all-mapped-relocated  the four .text-scope plan rungs
//	text-ladder                           the four .text-scope equivalence rungs
//	changed-units                         175 MB of diagnostic streams on disk
//	oracles                               the three perfect-prediction bounds
var historyRungs = []string{"stable-raw", "stable-relocated", "all-mapped-raw",
	"all-mapped-relocated", "text-ladder", "changed-units", "oracles"}

// wantHistory reports whether any scoreboard rung was asked for. When none
// was, construction only has to reach the five serialized plans, and the stage
// memo can supply those outright.
func wantHistory() bool {
	for _, n := range historyRungs {
		if wantRung(n) {
			return true
		}
	}
	return false
}

func writeReport(outDir string, rep report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := writeFile(outDir, "report.json", b); err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

func run() error {
	oldPath := flag.String("old", "", "old ELF release binary")
	newPath := flag.String("new", "", "new ELF release binary")
	oldDebug := flag.String("old-debug", "", "unstripped old ELF with symbols")
	newDebug := flag.String("new-debug", "", "unstripped new ELF with symbols")
	flag.BoolVar(&noDwarf, "no-dwarf", false, "leave the DWARF layer out")
	flag.BoolVar(&noTextEquivalences, "no-text-equivalences", false, "drop the equivalences that write into .text, keeping the rest")
	flag.BoolVar(&noEquivalences, "no-equivalences", false, "drop the equivalences, keeping the section geometry they carry")
	flag.BoolVar(&dispColumn, "dispcol", false, "ship §14's displacement column: zero the PC-relative fields inside the correction's long-run bucket and send them as columns of their own")
	flag.BoolVar(&noPoints, "no-points", false, "drop the inferred reference points from a Go-derived plan")
	flag.BoolVar(&noGoText, "no-go-text", false, "keep the Go-table module out of .text when there are no equivalences")
	noGoTables := flag.Bool("no-go-tables", false, "leave the Go-table module out even for a Go binary")
	ownMap := flag.Bool("own-map", false, "transmit the symbol-built function map even when the Go-table module could derive one")
	flag.StringVar(&sidecarTablePath, "sidecar-table", "", "carried symbol table the sidecar rung joins against, instead of a fresh hash of the old binary's symbols")
	flag.StringVar(&sidecarEmitPath, "sidecar-emit", "", "write the table the client would hold after this patch (kept units keep their carried hashes, inserts get the one shipped hash)")
	outDir := flag.String("out", "", "optional artifact directory")
	reference := flag.Int("reference", 0, "reference whole-patch byte count")
	equivalencePatch := flag.String("equivalence-patch", "", "optional raw patch supplying whole-file equivalences")
	flag.BoolVar(&nativeEq, "native-equivalences", false, "match the whole-file equivalences here instead of reading them from -equivalence-patch")
	flag.IntVar(&nativeEqMin, "native-eq-min", nativeEqMin, "shortest run -native-equivalences will emit")
	flag.IntVar(&nativeEqDrop, "native-eq-drop", nativeEqDrop, "how far -native-equivalences lets a run's score fall below its peak before cutting it")
	resume := flag.String("resume", "", "artifact directory of a previous run; replays the whole-image rungs from its cached plans")
	rungs := flag.String("rungs", "corrected-fields", `comma-separated rung names to measure; "all" restores the full ladder including the scoreboard rungs`)
	probes := flag.String("probes", "", "comma-separated probes to run instead of measuring: chunks, dict, rnfdict, ...")
	dictWindow := flag.Int("dict-window", 16<<20, "bytes of prediction the dict probe uses as a dictionary")
	memoFlag := flag.String("memo", memoDir, "directory for the stage memo; empty disables it")
	jobs := flag.Int("xz-jobs", xzJobs, "concurrent xz processes")
	selector := flag.String("select", selectStrategy, "per-function selection score: bytes, corr, or fields")
	flag.Parse()
	switch *selector {
	case "bytes", "corr", "fields":
		selectStrategy = *selector
	default:
		return fmt.Errorf("-select must be bytes, corr, or fields")
	}
	if err := checkNativeEqFlags(*equivalencePatch); err != nil {
		return err
	}
	if *rungs == "all" {
		onlyRungs = nil
	} else {
		onlyRungs = commaSet(*rungs)
	}
	onlyProbes = commaSet(*probes)
	dictProbeWindow = *dictWindow
	memoDir, xzJobs = *memoFlag, *jobs
	// Set before the two early returns below: -resume and a plans-memo hit
	// both reach the rungs without passing the later assignment, and a dump
	// probe that silently writes nothing is worse than no probe at all.
	dumpDir = *outDir
	// The sidecar probe reads the two symbol tables directly, and -resume
	// reaches the rungs without ever loading them.
	oldDebugPath, newDebugPath = *oldDebug, *newDebug
	if *resume != "" {
		return resumeWholeImage(*oldPath, *newPath, *resume, *outDir, *reference)
	}
	if *oldPath == "" || *newPath == "" || *oldDebug == "" || *newDebug == "" {
		return fmt.Errorf("-old, -new, -old-debug, and -new-debug are required")
	}
	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "code identity: harness %s, codec %s; memo %s; xz jobs %d\n",
		harnessCode(), codecCode(), cmp.Or(memoDir, "(disabled)"), xzJobs)
	start := time.Now()
	oldImage, newImage, err := loadImages(*oldPath, *newPath)
	if err != nil {
		return err
	}

	// The Go-table module: the Go-aware codec's metadata regeneration
	// (pclntab, type descriptors, data maps) as one layer, and the source of
	// the function map when it applies. Absent for anything that is not a
	// Go binary it understands.
	if wantLateRungs() && !*noGoTables {
		t := startStage("go tables")
		b, err := delta.EncodeGoTables(oldImage.Data, newImage.Data)
		if err != nil {
			t.done("not applicable: %v", err)
		} else {
			goTablesPlan = b
			t.done("plan %d B", len(b))
		}
	}

	// Without a scoreboard rung, nothing between here and the whole-image
	// rungs is read for anything but the five serialized plans, so a memo hit
	// skips the lot: two DWARF symbol parses, the matcher, the reference-point
	// oracle and four .text predictions.
	var plansKey string
	if *equivalencePatch != "" && !wantHistory() {
		variant := fmt.Sprintf("go=%d own=%v noeq=%v nopoints=%v nogotext=%v notexteq=%v nodwarf=%v", len(goTablesPlan), *ownMap, noEquivalences, noPoints, noGoText, noTextEquivalences, noDwarf)
		plansKey = plansMemoKey(selectStrategy+" "+variant, *oldPath, *newPath, *oldDebug, *newDebug, *equivalencePatch)
		if art, ok := loadPlansMemo(plansKey); ok {
			startStage("plan construction").done("memo hit")
			if err := art.write(*outDir); err != nil {
				return err
			}
			whole, err := replayWholeImage(art, oldImage, newImage, *outDir, *reference)
			if err != nil {
				return err
			}
			reportTotals()
			return writeReport(*outDir, report{
				Old: *oldPath, New: *newPath,
				OldTextBytes: int(oldImage.Text.Size), NewTextBytes: int(newImage.Text.Size),
				ReferencePatchBytes: *reference, TenXReferenceBytes: *reference / 10,
				Combined:                &combinedReport{WholeImage: whole},
				ConstructionElapsedSecs: time.Since(start).Seconds(),
			})
		}
	}

	oldUnits, oldStats, err := memoCodeUnits("old", *oldDebug, oldImage.Text)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "old: %d function symbols -> %d address units\n", oldStats.FunctionSymbols, oldStats.AddressUnits)
	newUnits, newStats, err := memoCodeUnits("new", *newDebug, newImage.Text)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "new: %d function symbols -> %d address units\n", newStats.FunctionSymbols, newStats.AddressUnits)
	oldText, newText := oldImage.textBytes(), newImage.textBytes()

	t := startStage("structural matching")
	plan, matches := constructPlan(oldUnits, newUnits, oldText, newText, oldImage.Text.Addr, newImage.Text.Addr, sectionRanges(oldImage, newImage))
	t.done("%d mappings, %d name-mapped, %d content-mapped", len(plan.Maps), matches.NameMapped, matches.ContentMapped)
	goDerived := false
	if goTablesPlan != nil && !*ownMap {
		t = startStage("go-derived map")
		derived, err := goMapDeriver(oldImage.Data, goTablesPlan, oldImage.Text, newImage.Text)()
		if err != nil {
			return err
		}
		own := make(map[mapping]bool, len(plan.Maps))
		for _, m := range plan.Maps {
			m.Copy = true
			own[m] = true
		}
		same := 0
		for _, m := range derived.maps {
			if own[m] {
				same++
			}
		}
		plan.Maps, plan.Prior, goDerived = derived.maps, derived.prior, true
		t.done("%d mappings, %d shared with the symbol map's %d", len(derived.maps), same, len(own))
	}

	t = startStage("reference points")
	plan.Points = inferReferencePoints(plan, oldText, newText)
	if goDerived && noPoints {
		plan.Points = nil
	}
	t.done("%d exact points", len(plan.Points))

	// plan.bin is read by nothing else: it exists for the two stable rungs and
	// as a record of the sparse map.
	var planBytes []byte
	if wantRung("stable-raw") || wantRung("stable-relocated") {
		t = startStage("plan serialisation")
		planBytes, err = plan.marshal(oldText)
		if err != nil {
			return err
		}
		t.done("%d B", len(planBytes))
		if err := writeFile(*outDir, "plan.bin", planBytes); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "plan: %d mappings, %d exact reference points, %d normalized-equal units, %d bytes\n", len(plan.Maps), len(plan.Points), matches.CopyUnits, matches.CopyBytes)

	var stableRawReport, stableRelocReport, mappedRawReport, mappedRelocReport stageReport
	if wantRung("stable-raw") {
		t = startStage("rung stable-raw")
		pred, reloc, err := predict(oldText, planBytes, false, nil)
		if err != nil {
			return err
		}
		var corr []byte
		if stableRawReport, corr, err = measure(pred, newText, planBytes, reloc, *reference, true, nil, nil); err != nil {
			return err
		}
		if err := writeFile(*outDir, "stable-raw.correction", corr); err != nil {
			return err
		}
		t.done("%d compressed bytes", stableRawReport.TotalZstd)
	}
	if wantRung("stable-relocated") {
		t = startStage("rung stable-relocated")
		pred, reloc, err := predict(oldText, planBytes, true, nil)
		if err != nil {
			return err
		}
		var corr []byte
		if stableRelocReport, corr, err = measure(pred, newText, planBytes, reloc, *reference, true, nil, nil); err != nil {
			return err
		}
		if err := writeFile(*outDir, "stable-relocated.correction", corr); err != nil {
			return err
		}
		t.done("%d compressed bytes", stableRelocReport.TotalZstd)
	}

	mappedPlan := allMappedPlan(plan)
	if goDerived {
		mappedPlan.Mode = planGoDerived
	}
	deriveMap := goMapDeriver(oldImage.Data, goTablesPlan, oldImage.Text, newImage.Text)
	t = startStage("mapped plan serialisation")
	mappedPlanBytes, err := mappedPlan.marshal(oldText)
	if err != nil {
		return err
	}
	t.done("%d B", len(mappedPlanBytes))
	if err := writeFile(*outDir, "all-mapped-plan.bin", mappedPlanBytes); err != nil {
		return err
	}
	if wantRung("all-mapped-raw") {
		t = startStage("rung all-mapped-raw")
		pred, stats, err := predict(oldText, mappedPlanBytes, false, deriveMap)
		if err != nil {
			return err
		}
		var corr []byte
		if mappedRawReport, corr, err = measure(pred, newText, mappedPlanBytes, stats, *reference, true, nil, nil); err != nil {
			return err
		}
		if err := writeFile(*outDir, "all-mapped-raw.correction", corr); err != nil {
			return err
		}
		t.done("%d compressed bytes", mappedRawReport.TotalZstd)
	}

	// The relocated all-mapped prediction is not optional: it is the
	// structural candidate the per-function selector chooses against.
	t = startStage("predict all-mapped-relocated")
	mappedRelocPred, mappedRelocStats, err := predict(oldText, mappedPlanBytes, true, deriveMap)
	if err != nil {
		return err
	}
	t.done("")
	if wantRung("all-mapped-relocated") {
		t = startStage("rung all-mapped-relocated")
		var corr []byte
		if mappedRelocReport, corr, err = measure(mappedRelocPred, newText, mappedPlanBytes, mappedRelocStats, *reference, true, nil, nil); err != nil {
			return err
		}
		if err := writeFile(*outDir, "all-mapped-relocated.correction", corr); err != nil {
			return err
		}
		t.done("%d compressed bytes", mappedRelocReport.TotalZstd)
	}

	var combined *combinedReport
	if *equivalencePatch != "" || nativeEq {
		var art planArtifacts
		combined, art, err = runCombined(*equivalencePatch, oldImage, newImage, mappedPlan, mappedPlanBytes, mappedRelocPred, *outDir, *reference)
		if err != nil {
			return err
		}
		if err := art.write(*outDir); err != nil {
			return err
		}
		if plansKey != "" {
			storePlansMemo(plansKey, art)
		}
		if combined.WholeImage, err = replayWholeImage(art, oldImage, newImage, *outDir, *reference); err != nil {
			return err
		}
	}

	var changedTarget []byte
	var changedPredictionRefCount, changedTargetRefCount int
	if wantRung("changed-units") {
		t = startStage("changed-unit streams")
		var changedPrediction []byte
		var changedPredictionCanonical, changedTargetCanonical []byte
		var changedPredictionRefs, changedTargetRefs []byte
		for _, m := range plan.Maps {
			if m.Copy {
				continue
			}
			predBody := mappedRelocPred[m.Dst : m.Dst+m.DstSize]
			targetBody := newText[m.Dst : m.Dst+m.DstSize]
			changedPrediction = append(changedPrediction, predBody...)
			changedTarget = append(changedTarget, targetBody...)
			var n int
			changedPredictionCanonical, changedPredictionRefs, n = appendCanonical(changedPredictionCanonical, changedPredictionRefs, predBody)
			changedPredictionRefCount += n
			changedTargetCanonical, changedTargetRefs, n = appendCanonical(changedTargetCanonical, changedTargetRefs, targetBody)
			changedTargetRefCount += n
		}
		for _, f := range []struct {
			name string
			b    []byte
		}{
			{"changed-units.prediction", changedPrediction},
			{"changed-units.target", changedTarget},
			{"changed-units.prediction.canonical", changedPredictionCanonical},
			{"changed-units.target.canonical", changedTargetCanonical},
			{"changed-units.prediction.refs", changedPredictionRefs},
			{"changed-units.target.refs", changedTargetRefs},
		} {
			if err := writeFile(*outDir, f.name, f.b); err != nil {
				return err
			}
		}
		t.done("%d B of changed units", len(changedTarget))
	}

	var perfectStableReport, perfectMappedReport, perfectFunctionsReport oracleReport
	if wantRung("oracles") {
		t = startStage("oracle bounds")
		perfectStable := append([]byte(nil), mappedRelocPred...)
		for _, m := range plan.Maps {
			if m.Copy {
				copy(perfectStable[m.Dst:m.Dst+m.DstSize], newText[m.Dst:m.Dst+m.DstSize])
			}
		}
		perfectMapped := append([]byte(nil), mappedRelocPred...)
		for _, m := range mappedPlan.Maps {
			copy(perfectMapped[m.Dst:m.Dst+m.DstSize], newText[m.Dst:m.Dst+m.DstSize])
		}
		perfectFunctions := bytes.Repeat([]byte{0xcc}, len(newText))
		for _, u := range newUnits {
			copy(perfectFunctions[u.Off:u.Off+u.Size], newText[u.Off:u.Off+u.Size])
		}
		for _, o := range []struct {
			name string
			pred []byte
			into *oracleReport
		}{
			{"oracle-perfect-stable.correction", perfectStable, &perfectStableReport},
			{"oracle-perfect-mapped.correction", perfectMapped, &perfectMappedReport},
			{"oracle-perfect-functions.correction", perfectFunctions, &perfectFunctionsReport},
		} {
			r, corr, err := measureOracle(o.pred, newText)
			if err != nil {
				return err
			}
			if err := writeFile(*outDir, o.name, corr); err != nil {
				return err
			}
			*o.into = r
		}
		t.done("stable=%d mapped=%d all-functions=%d compressed bytes",
			perfectStableReport.CorrectionZstd, perfectMappedReport.CorrectionZstd, perfectFunctionsReport.CorrectionZstd)
		fmt.Fprintf(os.Stderr, "oracle corrections: stable=%d mapped=%d all-functions=%d compressed bytes\n",
			perfectStableReport.CorrectionZstd, perfectMappedReport.CorrectionZstd, perfectFunctionsReport.CorrectionZstd)
	}

	rep := report{
		Old: *oldPath, New: *newPath,
		OldTextBytes: len(oldText), NewTextBytes: len(newText),
		ReferencePatchBytes: *reference, OldSymbols: oldStats, NewSymbols: newStats,
		ReferencePoints: len(plan.Points), ChangedUnitStreamBytes: len(changedTarget),
		ChangedPredictionRefs: changedPredictionRefCount, ChangedTargetRefs: changedTargetRefCount,
		Matches: matches, StableRaw: stableRawReport, StableRelocated: stableRelocReport,
		MappedRaw: mappedRawReport, MappedRelocated: mappedRelocReport,
		PerfectStableOracle: perfectStableReport, PerfectMappedOracle: perfectMappedReport,
		PerfectFunctionsOracle: perfectFunctionsReport, Combined: combined,
		ConstructionElapsedSecs: time.Since(start).Seconds(),
	}
	if *reference > 0 {
		rep.TenXReferenceBytes = *reference / 10
	}
	reportTotals()
	return writeReport(*outDir, rep)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "elfpredict:", err)
		os.Exit(1)
	}
}

// runProbes answers a staged question against a rung's prediction without
// measuring the rung. Each probe is attached to the cheapest rung that can
// answer it, so -probes and -rungs together bound a run to one prediction and
// a handful of small compressions.
func runProbes(name string, planBytes, pred, target []byte, oldImage, newImage *image, structureBytes []byte, structure predictionPlan) {
	nt := newImage.Text
	predText, targetText := pred[nt.Off:nt.Off+nt.Size], target[nt.Off:nt.Off+nt.Size]
	if onlyProbes["dump"] {
		if err := writeFile(dumpDir, "whole-image-"+name+".prediction", pred); err != nil {
			fmt.Fprintf(os.Stderr, "probe dump FAILED: %v\n", err)
		}
		if err := writeFile(dumpDir, "whole-image-"+name+".plan", planBytes); err != nil {
			fmt.Fprintf(os.Stderr, "probe dump FAILED: %v\n", err)
		}
	}
	if onlyProbes["chunks"] && name == "structurally-retargeted" {
		sp, _, err := predict(oldImage.textBytes(), structureBytes, true, goMapDeriver(oldImage.Data, goTablesPlan, oldImage.Text, newImage.Text))
		if err != nil {
			fmt.Fprintf(os.Stderr, "probe chunks FAILED: %v\n", err)
		} else {
			probeChunkChoices(predText, sp, targetText, structure)
		}
	}
	if onlyProbes["whychosen"] && name == "per-function-selection" {
		probeWhyChosen(planBytes, structure)
	}
	if onlyProbes["cond"] && name == "corrected-fields" {
		c, err := encodeColumnar(predText, targetText)
		if err != nil {
			fmt.Fprintf(os.Stderr, "probe cond FAILED: %v\n", err)
		} else {
			cols := 0
			for _, b := range c.Bytes {
				cols += xzSize(b)
			}
			probeConditionalCorrection(predText, targetText, cols)
		}
	}
	if onlyProbes["cause"] && name == "corrected-fields" {
		probeResidualCause(predText, targetText, oldImage.textBytes(), structure.Maps)
	}
	if onlyProbes["insts"] && name == "corrected-fields" {
		probeInstructionDiff(predText, targetText, structure.Maps)
	}
	if onlyProbes["operands"] && name == "corrected-fields" {
		probeOperandDiff(predText, targetText, structure.Maps)
	}
	if onlyProbes["immprobe"] && name == "corrected-fields" {
		probeImmediateDiff(predText, targetText, structure.Maps, oldImage, newImage, planBytes, structure)
	}
	if onlyProbes["packing"] && name == "corrected-fields" {
		fmt.Fprintln(os.Stderr, "  probe column packing:")
		cp, _ := unmarshalCombinedPlan(planBytes)
		if ep, err := parseEquivalencePlan(cp.Equivalences); err == nil {
			probeColumnPacking("eq src-residual", ep.SrcResidual)
			probeColumnPacking("eq copy-len", ep.CopyLen)
			probeColumnPacking("eq dst-skip", ep.DstSkip)
			probeColumnPacking("eq src-skip", ep.SrcSkip)
		}
		if fp, err := unmarshalFieldPlan(cp.Fields); err == nil {
			probeColumnPacking("field remap-shift", fp.RemapShift)
			probeColumnPacking("field field-delta", fp.FieldDelta)
			probeColumnPacking("field remap-index", fp.RemapIndex)
			probeColumnPacking("field field-index", fp.FieldIndex)
		}
		var all [][]byte
		if ep, err := parseEquivalencePlan(cp.Equivalences); err == nil {
			all = append(all, ep.SrcResidual, ep.CopyLen, ep.DstSkip, ep.SrcSkip)
		}
		if fp, err := unmarshalFieldPlan(cp.Fields); err == nil {
			all = append(all, fp.RemapShift, fp.FieldDelta, fp.RemapIndex, fp.FieldIndex)
		}
		if c, err := encodeColumnar(predText, targetText); err == nil {
			probeColumnPacking("correction gaps", c.Gaps)
			probeColumnPacking("correction lens", c.Lens)
			all = append(all, c.Gaps, c.Lens)
		}
		probeColumnPackingInSitu(all)
	}
	if onlyProbes["order"] && name == "corrected-fields" {
		probeOrdering(oldImage.Data, target, planBytes, structure)
	}
	if onlyProbes["eqvalue"] && name == "corrected-fields" {
		probeEquivalenceValue(oldImage.Data, target, planBytes, structure, pred)
	}
	if onlyProbes["eqcanon"] && name == "corrected-fields" {
		probeSourceCanonicalisation(oldImage.Data, planBytes, structure)
	}
	if onlyProbes["instpos"] && name == "corrected-fields" {
		probeInstructionPositions(predText, targetText)
	}
	if onlyProbes["cmix"] && name == "corrected-fields" {
		probeCMCoder(predText, targetText, structure.Maps, nt, len(target), newImage.Sections, pred, target)
	}
	if onlyProbes["dispcol"] && name == "corrected-fields" {
		probeDisplacementColumn(predText, targetText, structure.Maps, nt.Addr)
	}
	if onlyProbes["cdict"] && name == "corrected-fields" {
		probeCanonicalDictionary(predText, targetText, structure.Maps, dictProbeWindow)
	}
	if onlyProbes["dict"] && name == "corrected-fields" {
		probeCorrectionDictionary(predText, targetText, dictProbeWindow)
	}
	if onlyProbes["sidecar"] && name == "corrected-fields" {
		probeSidecar(planBytes, structure, oldImage, newImage)
	}
	if onlyProbes["derivedmap"] && name == "corrected-fields" {
		probeDerivedMap(planBytes, structure, oldImage, newImage)
	}
	if onlyProbes["blocksidecar"] && name == "corrected-fields" {
		probeBlockSidecar(planBytes, structure, oldImage, newImage)
	}
	if onlyProbes["rnfdict"] && name == "corrected-fields" {
		probeDictionaryCalibration()
		probeRegNormalDictionary(predText, targetText, structure.Maps)
	}
}
