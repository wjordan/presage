package elfmod

import (
	"errors"
	"fmt"
	"slices"

	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/presage"
	"github.com/wjordan/presage/presage/eqmatch"
	"github.com/wjordan/presage/presage/symbols"
)

// ModuleELF is the module id of the ELF x86-64 module.
const ModuleELF = 4

// Module is the ELF x86-64 module. Register it with presage.Registry.Add
// after the Go module, which claims the Go binaries it models.
type Module struct {
	// Symbols are the encoder-only function symbols of the old and new
	// image (SPEC G7). Nil entries run the module without a function map.
	Symbols [2]symbols.Reader
	// Params tunes the equivalence matcher; the zero value is eqmatch.CodeDefaults.
	Params eqmatch.Params
	// Stats, if non-nil, receives the encode's statistics on Analyse.
	Stats *Stats
}

// Stats is what Analyse learnt about the pair.
type Stats struct {
	Mappings, Points, Equivalences   int
	SelectedFunctions, SelectedBytes int
	Relocation                       x86.Stats
	// PredictErr counts the bytes the returned prediction gets wrong;
	// TextPredictErr the ones inside .text.
	PredictErr, TextPredictErr int
	Notes                      []string
}

func (Module) ID() byte     { return ModuleELF }
func (Module) Name() string { return "elf" }
func (Module) Exact() bool  { return true }

// planStreams is the plan's wire form: eight uvarint-length-prefixed
// streams in a fixed order, an absent layer being an empty stream
// (elf-module.md §2).
type planStreams struct {
	Equivalences, Structure, Choices, Reloc, EhFrame, RoData, Fields, Dwarf []byte
}

func (p planStreams) marshal() []byte {
	var b []byte
	for _, s := range [][]byte{p.Equivalences, p.Structure, p.Choices, p.Reloc, p.EhFrame, p.RoData, p.Fields, p.Dwarf} {
		b = appendStream(b, s)
	}
	return b
}

func parsePlanStreams(b []byte) (planStreams, error) {
	r := &planReader{b: b}
	var p planStreams
	for _, s := range []*[]byte{&p.Equivalences, &p.Structure, &p.Choices, &p.Reloc, &p.EhFrame, &p.RoData, &p.Fields, &p.Dwarf} {
		*s = r.stream().b
	}
	if !r.done() {
		return planStreams{}, errors.New("invalid elf plan streams")
	}
	return p, nil
}

func corrupt(err error) error { return fmt.Errorf("%w: elf: %v", presage.ErrCorrupt, err) }

// Materialise expands the plan against reference 0 into exactly length
// bytes (elf-module.md §1, §6).
func (Module) Materialise(refs [][]byte, plan []byte, length int64) ([]byte, error) {
	cp, err := parsePlanStreams(plan)
	if err != nil {
		return nil, corrupt(err)
	}
	out, _, err := predictImage(refs[0], cp)
	if err != nil {
		return nil, corrupt(err)
	}
	if int64(len(out)) != length {
		return nil, corrupt(fmt.Errorf("plan predicts %d bytes, region is %d", len(out), length))
	}
	return out, nil
}

// predStats is what one prediction reports back to the encoder.
type predStats struct {
	Relocation                       x86.Stats
	SelectedFunctions, SelectedBytes int
}

// predictImage is the decoder: every stage reads the old image and the
// plan only, in the order elf-module.md §1 fixes.
func predictImage(old []byte, cp planStreams) ([]byte, predStats, error) {
	var st predStats
	ep, err := parseEquivalencePlan(cp.Equivalences)
	if err != nil {
		return nil, st, err
	}
	if uint64(len(old)) != ep.OldLen || ep.NewLen > uint64(int(^uint(0)>>1)) {
		return nil, st, errors.New("old image does not match the equivalence plan")
	}
	oldText := old[ep.OldText.Off : ep.OldText.Off+ep.OldText.Size]
	structure, err := unmarshalPlan(cp.Structure, oldText)
	if err != nil {
		return nil, st, err
	}
	if structure.TargetLen != ep.NewText.Size || structure.OldAddr != ep.OldText.Addr || structure.NewAddr != ep.NewText.Addr {
		return nil, st, errors.New("structural and equivalence plans describe different text sections")
	}
	if ep.Eqs, err = decodeEquivalences(ep, newSrcPredictor(structure.Maps, ep.OldText, ep.NewText)); err != nil {
		return nil, st, err
	}
	out := layImage(old, ep)

	var rp *relocPlan
	if len(cp.Reloc) != 0 {
		parsed, err := unmarshalRelocPlan(cp.Reloc)
		if err != nil {
			return nil, st, err
		}
		if parsed.OldOff+parsed.OldSize > uint64(len(old)) || parsed.NewOff+parsed.NewSize > uint64(len(out)) {
			return nil, st, errors.New("relocation plan exceeds an image")
		}
		rp = &parsed
	}
	parts := newOracleParts(ep, structure)
	if rp != nil && rp.NewSize != 0 {
		if _, err := applyReloc(out, old, *rp, parts.pointer(rp)); err != nil {
			return nil, st, err
		}
	}
	if len(cp.Dwarf) != 0 {
		dp, err := unmarshalDwarfPlan(cp.Dwarf, old)
		if err != nil {
			return nil, st, err
		}
		if _, err := applyDwarf(out, old, dp, ep, parts.pointer(rp), funcSizeDeltas(structure)); err != nil {
			return nil, st, err
		}
	}
	if len(cp.EhFrame) != 0 {
		fp, err := unmarshalEhFramePlan(cp.EhFrame)
		if err != nil {
			return nil, st, err
		}
		if fp.NewOff+fp.NewSize > uint64(len(out)) || fp.OldOff+fp.OldSize > uint64(len(old)) || fp.HdrOff+fp.HdrSize > uint64(len(out)) {
			return nil, st, errors.New("eh_frame plan exceeds an image")
		}
		if rp == nil {
			return nil, st, errors.New("eh_frame plan needs the section geometry the relocation plan carries")
		}
		// Keyed by old address: an FDE names the function it describes
		// by where that function used to be.
		type extent struct{ old, new uint64 }
		extents := make(map[uint64]extent, len(structure.Maps))
		for _, m := range structure.Maps {
			extents[structure.OldAddr+m.Src] = extent{m.SrcSize, m.DstSize}
		}
		extentOf := func(addr uint64) (uint64, uint64, bool) {
			e, ok := extents[addr]
			return e.old, e.new, ok
		}
		applyEhFrame(out, old, ep, fp, rp.OldSecs, parts.pointer(rp), extentOf)
	}
	if len(cp.RoData) != 0 {
		rd, err := unmarshalRoDataPlan(cp.RoData)
		if err != nil {
			return nil, st, err
		}
		if rd.NewOff+rd.NewSize > uint64(len(out)) || rd.OldOff+rd.OldSize > uint64(len(old)) {
			return nil, st, errors.New("rodata plan exceeds an image")
		}
		if rp == nil {
			return nil, st, errors.New("rodata plan needs the section geometry the relocation plan carries")
		}
		applyRoData(out, old, rd, parts.sm, parts.pointer(rp))
	}
	text := out[ep.NewText.Off : ep.NewText.Off+ep.NewText.Size]
	st.Relocation = retargetEquivalencePrediction(text, ep, structure, parts.image(rp))
	if len(cp.Choices) != 0 {
		if len(cp.Choices) != (len(structure.Maps)+7)/8 {
			return nil, st, errors.New("choice stream has the wrong size")
		}
		structural, _, err := predictDecoded(oldText, structure, parts.lk.target)
		if err != nil {
			return nil, st, err
		}
		for i, m := range structure.Maps {
			if cp.Choices[i/8]&(1<<(i%8)) == 0 {
				continue
			}
			copy(text[m.Dst:m.Dst+m.DstSize], structural[m.Dst:m.Dst+m.DstSize])
			st.SelectedFunctions++
			st.SelectedBytes += int(m.DstSize)
		}
	}
	// The field fix is last: it names fields by position in a walk of the
	// finished prediction.
	if len(cp.Fields) != 0 {
		if _, err := applyFieldFix(text, ep.NewText.Addr, structure.Maps, cp.Fields); err != nil {
			return nil, st, err
		}
	}
	return out, st, nil
}

// layImage is the prediction's base: zero everywhere, 0xcc inside .text
// (the padding byte between functions), then every run copied whole-image.
func layImage(old []byte, ep equivalencePlan) []byte {
	out := make([]byte, int(ep.NewLen))
	for i := ep.NewText.Off; i < ep.NewText.Off+ep.NewText.Size; i++ {
		out[i] = 0xcc
	}
	for _, eq := range ep.Eqs {
		copy(out[eq.Dst:eq.Dst+eq.N], old[eq.Src:eq.Src+eq.N])
	}
	return out
}

// Cuts implements presage.Cutter: the correction is coded in pieces at the
// boundaries of the target's large sections, where the residual's character
// changes — .text is short scattered runs, .data.rel.ro repetitive
// relocation slots. A cut costs a compressor context, so only sections of
// a megabyte or more get their own piece.
func (Module) Cuts(target []byte) []int64 {
	const minSection = 1 << 20
	im, err := loadImage(target)
	if err != nil {
		return nil
	}
	set := map[int64]bool{}
	for _, s := range im.Sections {
		if s.NoBits || s.Size < minSection || s.Off+s.Size > uint64(len(target)) {
			continue
		}
		set[int64(s.Off)] = true
		set[int64(s.Off+s.Size)] = true
	}
	delete(set, 0)
	delete(set, int64(len(target)))
	cuts := make([]int64, 0, len(set))
	for c := range set {
		cuts = append(cuts, c)
	}
	slices.Sort(cuts)
	return cuts
}
