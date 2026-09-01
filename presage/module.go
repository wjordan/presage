package presage

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"unsafe"

	"github.com/wjordan/presage/delta"
)

// ErrDeclined is returned by a module's Analyse for an input it does not
// model. It is never a failure: the encoder codes the region another way.
var ErrDeclined = errors.New("presage: module declines this input")

// A Module predicts one region of the target from the references
// (docs/general/presage-core.md §4). Analyse runs on the encoder only;
// Materialise is the decoder's op and is a pure function of the
// references and the plan — no other state, so that encoder and decoder
// agree byte for byte, which the prediction hashes verify.
type Module interface {
	ID() byte
	Name() string
	Analyse(refs [][]byte, target []byte) (plan, pred []byte, err error)
	Materialise(refs [][]byte, plan []byte, length int64) ([]byte, error)
	// Exact modules materialise exactly length bytes and are corrected
	// positionally; the others declare a length and are corrected by a
	// shifted delta against their prediction.
	Exact() bool
}

// Finaliser is an optional module extension for bytes the decoder can
// recompute from the finished output -- a checksum the linker derives from
// the rest of the file. The encoder prices the residual against
// MaskResidual's copy of the prediction, where those bytes are already
// right and so cost nothing; the decoder calls Finalise to recompute them
// once the residual is applied. The container's target hash still checks
// the result, so a wrong recomputation is an error, never a wrong output.
type Finaliser interface {
	MaskResidual(plan, pred, target []byte) []byte
	Finalise(plan, out []byte) error
}

// A FieldRefiner is an exact module whose residual's long runs contain
// PC-relative fields both sides can re-derive rather than ship as literal
// bytes (delta/dispfield.go). The context is built from the references and
// the region's plan alone, so the decoder derives exactly the one the
// encoder used; a module that cannot build one returns nil and the
// correction is coded as it was before.
type FieldRefiner interface {
	DispContext(refs [][]byte, plan []byte, length int64) *delta.DispContext
}

// Core module ids. Ids above 15 are for admitted modules.
const (
	ModuleLZ   = 0 // shifted delta against reference 0; the fallback for anything
	ModuleCopy = 1 // bytes of a reference, verbatim
	ModuleGo   = 2 // the Go linux/amd64 module (presage/gomod)
	// ModuleEq = 3 is declared in eq.go.
)

// Registry is the set of modules an encoder may choose from or a decoder
// can run, by id.
type Registry struct{ mods map[byte]Module }

// NewRegistry returns a registry holding the core modules.
func NewRegistry() *Registry {
	r := &Registry{mods: map[byte]Module{}}
	r.Add(lzModule{})
	r.Add(copyModule{})
	return r
}

// Add registers a module; a duplicate id is a programming error.
func (r *Registry) Add(m Module) {
	if _, dup := r.mods[m.ID()]; dup {
		panic(fmt.Sprintf("presage: module id %d registered twice", m.ID()))
	}
	r.mods[m.ID()] = m
}

// Get returns the module with the given id, or nil.
func (r *Registry) Get(id byte) Module { return r.mods[id] }

// Candidates lists the registered modules in id order, the core ones first.
func (r *Registry) Candidates() []Module {
	ids := make([]int, 0, len(r.mods))
	for id := range r.mods {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	out := make([]Module, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.mods[byte(id)])
	}
	return out
}

// lzModule is the shifted delta: its prediction is the reference itself and
// the region's residual (an lz stream) does the work. It never declines.
type lzModule struct{}

func (lzModule) ID() byte     { return ModuleLZ }
func (lzModule) Name() string { return "lz" }
func (lzModule) Exact() bool  { return false }
func (lzModule) Analyse(refs [][]byte, target []byte) ([]byte, []byte, error) {
	return nil, refs[0], nil
}
func (lzModule) Materialise(refs [][]byte, plan []byte, length int64) ([]byte, error) {
	if len(plan) != 0 {
		return nil, fmt.Errorf("%w: lz region carries a plan", ErrCorrupt)
	}
	return refs[0], nil
}

// copyModule predicts a region as a verbatim slice of a reference.
type copyModule struct{}

func (copyModule) ID() byte     { return ModuleCopy }
func (copyModule) Name() string { return "copy" }
func (copyModule) Exact() bool  { return true }

func (copyModule) Analyse(refs [][]byte, target []byte) ([]byte, []byte, error) {
	for i, ref := range refs {
		if len(ref) >= len(target) && string(ref[:len(target)]) == string(target) {
			w := &wbuf{}
			w.u(uint64(i))
			w.u(0)
			return w.b, target, nil
		}
	}
	return nil, nil, ErrDeclined
}

func (copyModule) Materialise(refs [][]byte, plan []byte, length int64) ([]byte, error) {
	r := &rbuf{b: plan}
	ref := r.un(uint64(len(refs))-1, "reference index")
	off := r.un(uint64(len(refs[ref])), "copy offset")
	if err := r.done(); err != nil {
		return nil, err
	}
	if uint64(length) > uint64(len(refs[ref]))-off {
		return nil, fmt.Errorf("%w: copy of %d bytes at %d runs past reference %d", ErrCorrupt, length, off, ref)
	}
	return refs[ref][off : off+uint64(length)], nil
}

// residual codes what the prediction got wrong. Exact regions use the
// positional correction; declared regions use the shifted delta, whose
// stream replaces the prediction by the target outright.
// residual codes what the prediction got wrong and returns the header flags
// the stream needs. Exact regions use the positional correction, in pieces
// where the module cuts the target and that is smaller (split.go); declared
// regions use the shifted delta, whose stream replaces the prediction by
// the target outright.
func residual(m Module, refs [][]byte, plan, pred, target []byte, price int) ([]byte, byte, error) {
	if !m.Exact() {
		return delta.DiffLZ(pred, target), 0, nil
	}
	if len(pred) != len(target) {
		return nil, 0, fmt.Errorf("presage: module %s predicted %d bytes for a %d-byte region", m.Name(), len(pred), len(target))
	}
	whole, wholeCost, err := delta.EncodeCorrectionPricedSized(pred, target, price)
	if err != nil {
		return nil, 0, err
	}
	var flags byte
	if delta.UsesModalCorrection(whole) {
		flags |= FlagModalCorrection
	}
	c, ok := m.(Cutter)
	if !ok {
		return whole, flags, nil
	}
	cuts := c.Cuts(target)
	if len(cuts) == 0 {
		return whole, flags, nil
	}
	// The piecewise coding tries every piece several ways. Even if it drove
	// the correction to nothing it could not save more than the whole one
	// costs, so where that bound does not reach what the caller pays for
	// the seconds it is modelled to take, it is not run.
	if wholeCost < delta.WorthOf(price, len(whole), delta.SplitCorrectionRate) {
		return whole, flags, nil
	}
	var disp *delta.DispContext
	if fr, ok := m.(FieldRefiner); ok {
		disp = fr.DispContext(refs, plan, int64(len(target)))
	}
	split, modal, size, err := splitResidual(pred, target, cuts, disp)
	if err != nil {
		return nil, 0, err
	}
	if size >= wholeCost {
		return whole, flags, nil
	}
	flags = FlagSplitResidual
	if modal {
		flags |= FlagModalCorrection
	}
	return split, flags, nil
}

func applyResidual(m Module, refs [][]byte, plan, pred, stream []byte, length int64, flags byte, pf *splitPrefetch) ([]byte, error) {
	if !m.Exact() {
		return delta.PatchLZ(pred, stream, length)
	}
	if int64(len(pred)) != length {
		return nil, fmt.Errorf("%w: module %s materialised %d bytes for a %d-byte region", ErrCorrupt, m.Name(), len(pred), length)
	}
	// The correction is applied over the prediction in place. Apply hashes the
	// prediction before this point and never reads it again, and the field
	// context is rebuilt from the references and the plan rather than from
	// these bytes -- so the copy this used to make unconditionally was a whole
	// spare image, 291 MB and a tenth of a second on Chrome.
	//
	// It is only unconditionally safe for a module that returns a buffer of
	// its own, though: Materialise may legitimately return a slice of a
	// reference (the core lz module's identity case does), and writing through
	// that would corrupt the caller's input. So the copy is kept exactly where
	// the prediction still overlaps something it must not touch.
	out := pred
	if overlapsAny(pred, refs) || overlaps(pred, stream) {
		out = append([]byte(nil), pred...)
	}
	if flags&FlagSplitResidual != 0 {
		// Built only if a piece actually carries displacement columns: the
		// context costs a second parse of the plan.
		disp := func() *delta.DispContext { return nil }
		if fr, ok := m.(FieldRefiner); ok {
			disp = sync.OnceValue(func() *delta.DispContext { return fr.DispContext(refs, plan, length) })
		}
		if err := applySplitResidual(out, stream, disp, pf); err != nil {
			return nil, err
		}
		return out, nil
	}
	if err := delta.ApplyFlaggedCorrection(out, stream); err != nil {
		return nil, err
	}
	return out, nil
}

// overlaps reports whether a and b share any byte of backing array. It is the
// test that lets applyResidual correct a prediction in place: a prediction
// that shares no memory with the reference it came from, or with the patch
// body the correction is read out of, can be written through.
func overlaps(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	pa := uintptr(unsafe.Pointer(unsafe.SliceData(a)))
	pb := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	return pa < pb+uintptr(len(b)) && pb < pa+uintptr(len(a))
}

func overlapsAny(a []byte, bs [][]byte) bool {
	for _, b := range bs {
		if overlaps(a, b) {
			return true
		}
	}
	return false
}
