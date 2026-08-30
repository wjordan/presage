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
func residual(m Module, pred, target []byte) ([]byte, error) {
	if !m.Exact() {
		return delta.DiffLZ(pred, target), nil
	}
	if len(pred) != len(target) {
		return nil, fmt.Errorf("presage: module %s predicted %d bytes for a %d-byte region", m.Name(), len(pred), len(target))
	}
	return delta.EncodeCorrectionAdaptive(pred, target)
}

func applyResidual(m Module, pred, stream []byte, length int64) ([]byte, error) {
	if !m.Exact() {
		return delta.PatchLZ(pred, stream, length)
	}
	if int64(len(pred)) != length {
		return nil, fmt.Errorf("%w: module %s materialised %d bytes for a %d-byte region", ErrCorrupt, m.Name(), len(pred), length)
	}
	out := append([]byte(nil), pred...)
	if err := delta.ApplyFlaggedCorrection(out, stream); err != nil {
		return nil, err
	}
	return out, nil
}
