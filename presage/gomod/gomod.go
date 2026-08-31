// Package gomod is presage's Go linux/amd64 module: the predict-then-correct
// transform of delta, exposed as one region-level op, with the sections the
// transform leaves as positional copies — the debug sections, .symtab,
// .strtab — placed by equivalence runs and their reference fields projected
// by the DWARF layer (SPEC §4.4, §4.5; presage-core.md §4, §8).
package gomod

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/internal/cz"
	"github.com/wjordan/presage/presage"
	"github.com/wjordan/presage/presage/dwarf"
	"github.com/wjordan/presage/presage/eqmatch"
)

// Module is the Go module. Register it with presage.Registry.Add.
type Module struct {
	// Stats, if non-nil, receives the transform's statistics on Analyse.
	Stats *delta.Stats
}

func (Module) ID() byte     { return presage.ModuleGo }
func (Module) Name() string { return "go" }
func (Module) Exact() bool  { return true }

// The plan is four length-prefixed parts: the transform's plan, the DWARF
// plan, the equivalence runs of the sections the DWARF plan does not table,
// and the fips flag (fips.go). The middle two are empty for a binary
// without .debug_info.
type plan struct{ tf, dw, runs, fips []byte }

func (p plan) marshal() []byte {
	var b []byte
	for _, part := range [][]byte{p.tf, p.dw, p.runs, p.fips} {
		b = binary.AppendUvarint(b, uint64(len(part)))
		b = append(b, part...)
	}
	return b
}

func parsePlan(b []byte) (plan, error) {
	var parts [4][]byte
	for i := range parts {
		n, k := binary.Uvarint(b)
		if k <= 0 || n > uint64(len(b)-k) {
			return plan{}, fmt.Errorf("%w: go plan part %d", presage.ErrCorrupt, i)
		}
		parts[i], b = b[k:k+int(n)], b[k+int(n):]
	}
	if len(b) != 0 {
		return plan{}, fmt.Errorf("%w: %d trailing go plan bytes", presage.ErrCorrupt, len(b))
	}
	return plan{parts[0], parts[1], parts[2], parts[3]}, nil
}

// Analyse runs the transform on reference 0 and the target. A pair the
// transform declines — not a Go binary of the supported release — is
// reported as declined, and the core falls back to lz.
func (m Module) Analyse(refs [][]byte, target []byte) ([]byte, []byte, error) {
	old := refs[0]
	st := m.Stats
	if st == nil {
		st = &delta.Stats{}
	}
	tf, img, err := delta.GoAnalyseImage(old, target, st)
	if err != nil {
		return nil, nil, err
	}
	fp := fipsPart(target)
	bare := plan{tf: tf, fips: fp}
	secs, ok := debugSecs(old, target)
	if !ok {
		return bare.marshal(), img.Pred, nil
	}
	var runs [dwarf.NSec][]eqmatch.Run
	for k, s := range secs {
		if s.OldSize != 0 && s.NewSize != 0 {
			runs[k] = eqmatch.Match(old[s.OldOff:s.OldOff+s.OldSize], target[s.NewOff:s.NewOff+s.NewSize], eqmatch.Params{})
		}
	}
	// Fixed-row tables keep their record tables beside the runs: an
	// inserted row puts every later field between two runs, where only
	// the table says which row it is.
	withRecords := func(k int) bool {
		return len(runs[k]) == 0 || k == dwarf.Symtab || k == dwarf.Addr || k == dwarf.Strtab || k == dwarf.Frame
	}
	dp, ok := dwarf.Build(old, target, secs, withRecords, img.Lookup, func(k int) []eqmatch.Run { return runs[k] })
	if !ok {
		return bare.marshal(), img.Pred, nil
	}
	layered := plan{tf: tf, dw: dp.Marshal(), runs: dp.MarshalRuns(), fips: fp}
	// The prediction is what the decoder will build from the plan, not
	// what the encoder knows; and the layer is kept only where it pays,
	// which a release whose debug sections barely changed does not — its
	// tables cost more than the positional copy's correction.
	pred := append([]byte(nil), img.Pred...)
	if _, err := layer(old, &delta.GoImage{Pred: pred, Lookup: img.Lookup, SizeDelta: img.SizeDelta}, layered); err != nil {
		return nil, nil, err
	}
	// Pricing the bare prediction costs a correction of everything it
	// missed; where that is megabytes the layer has won already.
	bareErr := diffBytes(img.Pred, target)
	if bareErr < priceBelow {
		with, without := price(layered, pred, target), price(bare, img.Pred, target)
		st.Notes = append(st.Notes, fmt.Sprintf("dwarf layer priced: %d with, %d without", with, without))
		if without <= with {
			return bare.marshal(), img.Pred, nil
		}
	}
	st.Notes = append(st.Notes, fmt.Sprintf("dwarf layer: plan %d B, runs %d B, mispredicted %d B (bare %d)",
		len(layered.dw), len(layered.runs), diffBytes(pred, target), bareErr))
	st.PredictErr = diffBytes(pred, target)
	return layered.marshal(), pred, nil
}

// priceBelow is the bare misprediction under which the two are priced.
const priceBelow = 1 << 20

// price is what a plan and its prediction's correction come to after the
// terminal stage, the number the encoder chooses by.
func price(p plan, pred, target []byte) int {
	corr, err := delta.EncodeCorrectionAdaptive(pred, target)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	_, z := cz.Compress(append(p.marshal(), corr...))
	return len(z)
}

// Materialise expands the plan against reference 0.
func (Module) Materialise(refs [][]byte, b []byte, length int64) ([]byte, error) {
	p, err := parsePlan(b)
	if err != nil {
		return nil, err
	}
	img, err := delta.GoExpand(refs[0], p.tf, length)
	if err != nil {
		return nil, err
	}
	return layer(refs[0], img, p)
}

// layer lays the runs and projects the debug fields over the transform's
// prediction, in place.
func layer(old []byte, img *delta.GoImage, p plan) ([]byte, error) {
	if len(p.dw) == 0 {
		if len(p.runs) != 0 {
			return nil, fmt.Errorf("%w: runs without a dwarf plan", presage.ErrCorrupt)
		}
		return img.Pred, nil
	}
	dp, err := dwarf.Unmarshal(p.dw, old)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", presage.ErrCorrupt, err)
	}
	if err := dp.UnmarshalRuns(p.runs); err != nil {
		return nil, fmt.Errorf("%w: %v", presage.ErrCorrupt, err)
	}
	if _, err := dwarf.Apply(img.Pred, old, dp, img.Lookup, img.SizeDelta); err != nil {
		return nil, fmt.Errorf("%w: %v", presage.ErrCorrupt, err)
	}
	return img.Pred, nil
}

// debugSecs is the geometry of the sections the DWARF layer handles in
// both files. They are unallocated, so the transform's layout does not
// carry them and the DWARF plan does; the layer needs .debug_info and
// .debug_abbrev in both. A file that is not an ELF the standard parser
// reads has none.
func debugSecs(old, new []byte) ([dwarf.NSec]dwarf.Sec, bool) {
	var secs [dwarf.NSec]dwarf.Sec
	for i, b := range [][]byte{old, new} {
		f, err := elf.NewFile(bytes.NewReader(b))
		if err != nil {
			return secs, false
		}
		for k, name := range dwarf.Names {
			s := f.Section(name)
			if s == nil || s.Type == elf.SHT_NOBITS || s.Flags&elf.SHF_COMPRESSED != 0 || s.Offset+s.Size > uint64(len(b)) {
				continue
			}
			if i == 0 {
				secs[k].OldOff, secs[k].OldSize = s.Offset, s.Size
			} else {
				secs[k].NewOff, secs[k].NewSize = s.Offset, s.Size
			}
		}
	}
	info, abbrev := secs[dwarf.Info], secs[dwarf.Abbrev]
	return secs, info.OldSize != 0 && info.NewSize != 0 && abbrev.OldSize != 0 && abbrev.NewSize != 0
}

func diffBytes(a, b []byte) int {
	n := 0
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			n++
		}
	}
	return n + max(len(a), len(b)) - min(len(a), len(b))
}

// Registry returns the core modules plus this one.
func Registry() *presage.Registry {
	r := presage.NewRegistry()
	r.Add(Module{})
	return r
}
