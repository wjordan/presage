package presage

import (
	"errors"
	"fmt"
	"sort"

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
func residual(m Module, pred, target []byte) ([]byte, byte, error) {
	if !m.Exact() {
		return delta.DiffLZ(pred, target), 0, nil
	}
	if len(pred) != len(target) {
		return nil, 0, fmt.Errorf("presage: module %s predicted %d bytes for a %d-byte region", m.Name(), len(pred), len(target))
	}
	whole, err := delta.EncodeCorrectionAdaptive(pred, target)
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
	split, modal, size, err := splitResidual(pred, target, cuts)
	if err != nil {
		return nil, 0, err
	}
	if size >= frameCost(whole) {
		return whole, flags, nil
	}
	flags = FlagSplitResidual
	if modal {
		flags |= FlagModalCorrection
	}
	return split, flags, nil
}

func applyResidual(m Module, pred, stream []byte, length int64, flags byte) ([]byte, error) {
	if !m.Exact() {
		return delta.PatchLZ(pred, stream, length)
	}
	if int64(len(pred)) != length {
		return nil, fmt.Errorf("%w: module %s materialised %d bytes for a %d-byte region", ErrCorrupt, m.Name(), len(pred), length)
	}
	out := append([]byte(nil), pred...)
	if flags&FlagSplitResidual != 0 {
		if err := applySplitResidual(out, stream); err != nil {
			return nil, err
		}
		return out, nil
	}
	if err := delta.ApplyFlaggedCorrection(out, stream); err != nil {
		return nil, err
	}
	return out, nil
}
