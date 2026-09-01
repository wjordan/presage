package elfmod

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/internal/trace"
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
	// ReleaseReferencePages is an apply-only residency hint. Materialise calls
	// it between phases that reread reference 0, allowing the owner of a
	// read-only file mapping to evict clean pages and fault back only the ranges
	// the next phase touches. Ordinary byte slices must leave it nil.
	ReleaseReferencePages func()
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

// planStreams is the plan's wire form: ten uvarint-length-prefixed
// streams in a fixed order, an absent layer being an empty stream
// (elf-module.md §2).
type planStreams struct {
	Equivalences, Structure, Choices, Reloc, EhFrame, RoData, Fields, Dwarf, Relr, OpField []byte
}

func (p planStreams) marshal() []byte {
	var b []byte
	for _, s := range [][]byte{p.Equivalences, p.Structure, p.Choices, p.Reloc, p.EhFrame, p.RoData, p.Fields, p.Dwarf, p.Relr, p.OpField} {
		b = appendStream(b, s)
	}
	return b
}

func parsePlanStreams(packed []byte) (planStreams, error) {
	b, err := unpackPlan(packed)
	if err != nil {
		return planStreams{}, err
	}
	r := &planReader{b: b}
	var p planStreams
	for _, s := range []*[]byte{&p.Equivalences, &p.Structure, &p.Choices, &p.Reloc, &p.EhFrame, &p.RoData, &p.Fields, &p.Dwarf, &p.Relr, &p.OpField} {
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
func (m Module) Materialise(refs [][]byte, plan []byte, length int64) ([]byte, error) {
	cp, err := parsePlanStreams(plan)
	if err != nil {
		return nil, corrupt(err)
	}
	out, _, err := predictImage(refs[0], cp, m.ReleaseReferencePages)
	if err != nil {
		return nil, corrupt(err)
	}
	if int64(len(out)) != length {
		return nil, corrupt(fmt.Errorf("plan predicts %d bytes, region is %d", len(out), length))
	}
	return out, nil
}

// DispContext implements presage.FieldRefiner: the PC-relative fields the
// correction's long runs contain, named by walking the repaired bytes of
// every mapped function body (delta/dispfield.go). Both sides build it from
// the old image and the plan alone, so the two walks see the same fields.
func (Module) DispContext(refs [][]byte, plan []byte, length int64) *delta.DispContext {
	cp, err := parsePlanStreams(plan)
	if err != nil {
		return nil
	}
	ep, structures, err := cachedPlanMaps(refs[0], cp)
	if err != nil {
		return nil
	}
	windows := ep.Windows
	n := 0
	for i := range windows {
		n += len(structures[i].Maps)
	}
	bodies := make([]delta.DispBody, 0, n)
	starts := make([]uint64, 0, n)
	for i, w := range windows {
		for _, m := range structures[i].Maps {
			if m.DstSize == 0 || m.Dst > w.New.Size || m.DstSize > w.New.Size-m.Dst {
				continue
			}
			off := int64(w.New.Off + m.Dst)
			if off+int64(m.DstSize) > length {
				continue
			}
			bodies = append(bodies, delta.DispBody{Off: int(off), Size: int(m.DstSize), PC: w.New.Addr + m.Dst})
			starts = append(starts, w.New.Addr+m.Dst)
		}
	}
	var d *delta.DispContext
	if len(bodies) != 0 {
		d = delta.NewDispContextOwned(bodies, starts)
	}
	return d
}

// planMaps parses the plan's code windows and their structural plans. It is
// the front of predictImage, split out so the correction's field context can
// be rebuilt without predicting the image again.
func planMaps(old []byte, cp planStreams) (equivalencePlan, []predictionPlan, error) {
	ep, err := parseEquivalencePlan(cp.Equivalences)
	if err != nil {
		return ep, nil, err
	}
	if uint64(len(old)) != ep.OldLen {
		return ep, nil, errors.New("old image does not match the equivalence plan")
	}
	structures := make([]predictionPlan, len(ep.Windows))
	sr := &planReader{b: cp.Structure}
	for i, w := range ep.Windows {
		b := sr.stream()
		if sr.err != nil {
			return ep, nil, errors.New("invalid structure stream")
		}
		if structures[i], err = unmarshalPlan(b.b, old, w.Old); err != nil {
			return ep, nil, err
		}
	}
	if !sr.done() {
		return ep, nil, errors.New("structure stream does not match the code windows")
	}
	return ep, structures, nil
}

// An apply asks for the same parse twice: once to predict the image and once
// to name the fields the correction's runs contain. The second ask used to
// re-run the whole thing -- the map columns, the derived reconstruction, the
// point replay -- for an answer identical to the first by construction, since
// both are functions of the old image and the plan alone. So it is memoised
// on the identity of those two buffers (memo.go). Nothing downstream mutates
// a parsed plan: the one place that marks maps clones them first
// (match.go:allMapped).
type planMapsKey struct{ old, eq, structure bufID }

type planMapsEntry struct {
	// Every buffer the key names is retained: an address only identifies its
	// contents for as long as the memory cannot be freed and handed out again.
	old, eq, structure []byte
	ep                 equivalencePlan
	structures         []predictionPlan
	err                error
}

var (
	planMapsMu    sync.Mutex
	planMapsCache = map[planMapsKey]planMapsEntry{}
)

func cachedPlanMaps(old []byte, cp planStreams) (equivalencePlan, []predictionPlan, error) {
	key := planMapsKey{old: idOf(old), eq: idOf(cp.Equivalences), structure: idOf(cp.Structure)}
	planMapsMu.Lock()
	e, ok := planMapsCache[key]
	planMapsMu.Unlock()
	if ok {
		return e.ep, e.structures, e.err
	}
	ep, structures, err := planMaps(old, cp)
	planMapsMu.Lock()
	if len(planMapsCache) > 4 {
		clear(planMapsCache)
	}
	planMapsCache[key] = planMapsEntry{
		old: old, eq: cp.Equivalences, structure: cp.Structure,
		ep: ep, structures: structures, err: err,
	}
	planMapsMu.Unlock()
	return ep, structures, err
}

// predStats is what one prediction reports back to the encoder.
type predStats struct {
	Relocation                       x86.Stats
	SelectedFunctions, SelectedBytes int
	EhFrame                          ehFrameStats
	Relr                             relrStats
}

// predictImage is the decoder: every stage reads the old image and the
// plan only, in the order elf-module.md §1 fixes.
func predictImage(old []byte, cp planStreams, releaseReferencePages func()) ([]byte, predStats, error) {
	var st predStats
	// One structural plan per code window, in window order.
	donePM := trace.Stage("planMaps")
	ep, structures, err := cachedPlanMaps(old, cp)
	donePM()
	if err != nil {
		return nil, st, err
	}
	if releaseReferencePages != nil {
		releaseStructuralCaches(old)
		runtime.GC()
	}
	if ep.NewLen > uint64(int(^uint(0)>>1)) {
		return nil, st, errors.New("old image does not match the equivalence plan")
	}
	for i, w := range ep.Windows {
		if structures[i].TargetLen != w.New.Size || structures[i].OldAddr != w.Old.Addr || structures[i].NewAddr != w.New.Addr {
			return nil, st, errors.New("structural and equivalence plans describe different code windows")
		}
	}
	doneEq := trace.Stage("decodeEquivalences")
	ep.Eqs, err = decodeEquivalences(ep, newSrcPredictor(structures, ep.Windows))
	doneEq()
	if err != nil {
		return nil, st, err
	}
	if releaseReferencePages != nil {
		releaseReferencePages()
	}
	doneLay := trace.Stage("layImage")
	out := layImage(old, ep, releaseReferencePages)
	doneLay()

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
	doneParts := trace.Stage("oracleParts")
	parts := newOracleParts(ep, structures)
	doneParts()
	if rp != nil && rp.NewSize != 0 {
		doneReloc := trace.Stage("applyReloc")
		_, err := applyReloc(out, old, *rp, parts.pointer(rp))
		doneReloc()
		if err != nil {
			return nil, st, err
		}
		if releaseReferencePages != nil {
			releaseRelaCache(old)
		}
	}
	if len(cp.Relr) != 0 {
		lp, err := unmarshalRelrPlan(cp.Relr)
		if err != nil {
			return nil, st, err
		}
		if lp.OldOff+lp.OldSize > uint64(len(old)) {
			return nil, st, errors.New("relr plan exceeds the old image")
		}
		if rp == nil {
			return nil, st, errors.New("relr plan needs the section geometry the relocation plan carries")
		}
		doneRelr := trace.Stage("applyRelr")
		st.Relr = applyRelr(out, old, lp, rp.OldSecs, parts.sm, parts.pointer(rp))
		doneRelr()
	}
	if len(cp.Dwarf) != 0 {
		dp, err := unmarshalDwarfPlan(cp.Dwarf, old)
		if err != nil {
			return nil, st, err
		}
		doneDw := trace.Stage("applyDwarf")
		_, err = applyDwarf(out, old, dp, ep, parts.pointer(rp), funcSizeDeltas(structures))
		doneDw()
		if err != nil {
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
		extents := make(map[uint64]extent)
		for _, structure := range structures {
			for _, m := range structure.Maps {
				extents[structure.OldAddr+m.Src] = extent{m.SrcSize, m.DstSize}
			}
		}
		extentOf := func(addr uint64) (uint64, uint64, bool) {
			e, ok := extents[addr]
			return e.old, e.new, ok
		}
		doneEh := trace.Stage("applyEhFrame")
		st.EhFrame = applyEhFrame(out, old, ep, fp, rp.OldSecs, parts.pointer(rp), extentOf)
		doneEh()
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
		doneRo := trace.Stage("applyRoData")
		applyRoData(out, old, rd, parts.sm, parts.pointer(rp), parts.lk.unitAt)
		doneRo()
	}
	doneOr := trace.Stage("oracleImage")
	oracle := parts.image(rp)
	doneOr()
	doneRt := trace.Stage("retarget")
	for i, w := range ep.Windows {
		st.Relocation.Add(retargetEquivalencePrediction(bytesOf(out, w.New), ep, w, structures[i], oracle))
	}
	doneRt()
	if len(cp.Choices) != 0 {
		doneCh := trace.Stage("choices")
		cr := &planReader{b: cp.Choices}
		for i, w := range ep.Windows {
			b := cr.stream()
			if cr.err != nil {
				return nil, st, errors.New("invalid choice stream")
			}
			if len(b.b) == 0 {
				continue
			}
			if len(b.b) != (len(structures[i].Maps)+7)/8 {
				return nil, st, errors.New("choice stream has the wrong size")
			}
			selected, err := applyStructuralChoices(bytesOf(out, w.New), bytesOf(old, w.Old), structures[i], b.b, parts.lk.target, releaseReferencePages)
			if err != nil {
				return nil, st, err
			}
			st.SelectedFunctions += selected.Functions
			st.SelectedBytes += selected.Bytes
		}
		if !cr.done() {
			return nil, st, errors.New("choice stream does not match the code windows")
		}
		doneCh()
	}
	if releaseReferencePages != nil {
		releaseReferencePages()
	}
	// The field fix is last: it names fields by position in a walk of the
	// finished prediction.
	if len(cp.Fields) != 0 {
		doneFF := trace.Stage("fieldFix")
		fr := &planReader{b: cp.Fields}
		for i, w := range ep.Windows {
			b := fr.stream()
			if fr.err != nil {
				return nil, st, errors.New("invalid field stream")
			}
			if len(b.b) == 0 {
				continue
			}
			if _, err := applyFieldFix(bytesOf(out, w.New), w.New.Addr, structures[i].Maps, b.b); err != nil {
				return nil, st, err
			}
		}
		if !fr.done() {
			return nil, st, errors.New("field stream does not match the code windows")
		}
		doneFF()
	}
	// The operand-field correction reads the fields the field fix does not
	// write, so it runs after it over the same walk of the same bytes.
	if len(cp.OpField) != 0 {
		doneOF := trace.Stage("opField")
		or := &planReader{b: cp.OpField}
		for i, w := range ep.Windows {
			b := or.stream()
			if or.err != nil {
				return nil, st, errors.New("invalid operand field stream")
			}
			if len(b.b) == 0 {
				continue
			}
			if _, err := applyOpField(bytesOf(out, w.New), structures[i].Maps, b.b); err != nil {
				return nil, st, err
			}
		}
		if !or.done() {
			return nil, st, errors.New("operand field stream does not match the code windows")
		}
		doneOF()
	}
	return out, st, nil
}

type selectedStats struct {
	Relocation x86.Stats
	Functions  int
	Bytes      int
}

// applyStructuralChoices writes the selected structural function predictions
// directly into text. Building a whole window-sized structural image first is
// unnecessary: mappings have disjoint destinations, and an unselected byte is
// never observed.
func applyStructuralChoices(text, oldText []byte, p predictionPlan, choices []byte, lookupFn func(uint64) x86.Target, releaseReferencePages func()) (selectedStats, error) {
	var out selectedStats
	if len(choices) != (len(p.Maps)+7)/8 {
		return out, errors.New("choice stream has the wrong size")
	}
	if uint64(len(text)) != p.TargetLen {
		return out, errors.New("structural choice window has the wrong size")
	}
	var (
		errMu sync.Mutex
		bad   error
	)
	fail := func(err error) {
		errMu.Lock()
		if bad == nil {
			bad = err
		}
		errMu.Unlock()
	}
	batch := len(p.Maps)
	if releaseReferencePages != nil {
		batch = 64 << 10
	}
	for lo := 0; lo < len(p.Maps); lo += batch {
		hi := min(lo+batch, len(p.Maps))
		stats := parallelStats(hi-lo, workers(), func(st *x86.Stats, k int) {
			i := lo + k
			if choices[i/8]&(1<<(i%8)) == 0 {
				return
			}
			m := p.Maps[i]
			if m.Dst > uint64(len(text)) || m.DstSize > uint64(len(text))-m.Dst {
				fail(fmt.Errorf("map %d destination exceeds target text", i))
				return
			}
			dst := text[m.Dst : m.Dst+m.DstSize]
			if !m.Copy {
				fill(dst, 0xcc)
				return
			}
			if m.Src > uint64(len(oldText)) || m.SrcSize > uint64(len(oldText))-m.Src {
				fail(fmt.Errorf("map %d source exceeds old text", i))
				return
			}
			x86.Relocate(oldText[m.Src:m.Src+m.SrcSize], dst, p.OldAddr+m.Src, p.NewAddr+m.Dst, lookupFn, st, nil)
		})
		out.Relocation.Add(stats)
		if releaseReferencePages != nil {
			releaseReferencePages()
		}
	}
	if bad != nil {
		return selectedStats{}, bad
	}
	for i, m := range p.Maps {
		if choices[i/8]&(1<<(i%8)) != 0 {
			out.Functions++
			out.Bytes += int(m.DstSize)
		}
	}
	return out, nil
}

// layImage is the prediction's base: zero everywhere, 0xcc inside .text
// (the padding byte between functions), then every run copied whole-image.
func layImage(old []byte, ep equivalencePlan, releaseReferencePages func()) []byte {
	out := make([]byte, int(ep.NewLen))
	for _, w := range ep.Windows {
		fill(out[w.New.Off:w.New.Off+w.New.Size], 0xcc)
	}
	if releaseReferencePages == nil {
		for _, eq := range ep.Eqs {
			copy(out[eq.Dst:eq.Dst+eq.N], old[eq.Src:eq.Src+eq.N])
		}
		return out
	}
	const releaseStep = 16 << 20
	sinceRelease := uint64(0)
	for _, eq := range ep.Eqs {
		copy(out[eq.Dst:eq.Dst+eq.N], old[eq.Src:eq.Src+eq.N])
		sinceRelease += eq.N
		if sinceRelease >= releaseStep {
			releaseReferencePages()
			sinceRelease = 0
		}
	}
	releaseReferencePages()
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
