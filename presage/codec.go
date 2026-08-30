// Package presage is a predictive binary-patch codec: modules predict
// structural regions of the target from reference objects, the core
// materialises the predictions, verifies them, and codes what they got
// wrong (docs/general/SPEC.md; docs/general/presage-core.md for this
// milestone's shape).
package presage

import (
	"fmt"
	"io"

	"github.com/wjordan/presage/delta"
)

// Options controls Encode.
type Options struct {
	// Registry holds the modules the encoder may choose from. Nil means
	// the core modules only.
	Registry *Registry
	// Modules, if set, names the module ids the deployed decoder has; a
	// region whose module is not among them is coded with the core lz
	// module instead (lowering, SPEC §5.4). Nil means no restriction.
	Modules []byte
	// Stats, if non-nil, receives a breakdown of the patch.
	Stats *Stats
}

// Stats is an accounting of one encode.
type Stats struct {
	Flags    byte
	Regions  []RegionStats
	Body     int // uncompressed body
	Total    int // the patch
	Notes    []string
	DeltaOld int // for the debugz note: bytes as shipped
}

// RegionStats is one region's cost.
type RegionStats struct {
	Module     string
	Length     int64
	Plan       int
	Residual   int
	PredictErr int // bytes where the prediction differed from the target
}

// ErrPredictionDiverged means the decoder's materialisation of a region
// differs from the encoder's — the determinism bug class — named by region
// and module.
type ErrPredictionDiverged struct {
	Region int
	Module string
}

func (e *ErrPredictionDiverged) Error() string {
	return fmt.Sprintf("presage: region %d (%s): the prediction differs from the encoder's; fetch the blob", e.Region, e.Module)
}

func (o Options) registry() *Registry {
	if o.Registry != nil {
		return o.Registry
	}
	return NewRegistry()
}

func (o Options) allowed(id byte) bool {
	if o.Modules == nil {
		return true
	}
	for _, m := range o.Modules {
		if m == id {
			return true
		}
	}
	return false
}

// Encode produces a patch turning refs into target. Modules are tried in
// registry order on the whole target — the first that does not decline
// claims it, the core lz module last — so a module that models the input
// always beats the fallback. The patch reproduces target exactly; Apply
// verifies that before it hands the bytes over.
func Encode(refs [][]byte, target []byte, o Options) ([]byte, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("presage: at least one reference is needed")
	}
	st := o.Stats
	if st == nil {
		st = &Stats{}
	}
	h := &Header{Target: hashOf(target), Size: int64(len(target))}
	for _, r := range refs {
		h.Refs = append(h.Refs, Ref{B3: hashOf(r), Size: int64(len(r))})
	}
	shipped := len(target)
	if pOld, pNew, ok := delta.ExpandPair(refs[0], target); ok {
		h.Flags |= FlagDebugZ
		refs = append([][]byte{pOld}, refs[1:]...)
		target = pNew
		st.Notes = append(st.Notes, fmt.Sprintf("compressed debug sections expanded: %d -> %d bytes", shipped, len(target)))
	}
	st.Flags = h.Flags

	// One region, the whole target, by the first module that takes it.
	reg := o.registry()
	var chosen Module
	var plan, pred []byte
	for _, m := range reg.Candidates() {
		if m.ID() == ModuleLZ || !o.allowed(m.ID()) {
			continue
		}
		p, pr, err := m.Analyse(refs, target)
		if err == nil {
			chosen, plan, pred = m, p, pr
			break
		}
		if !isDeclined(err) {
			return nil, err
		}
		st.Notes = append(st.Notes, m.Name()+": "+err.Error())
	}
	if chosen == nil {
		chosen = reg.Get(ModuleLZ)
		plan, pred, _ = chosen.Analyse(refs, target)
	}
	res, rflags, err := residual(chosen, pred, target)
	if err != nil {
		return nil, err
	}
	h.Flags |= rflags
	st.Flags = h.Flags
	h.Regions = []Region{{Length: int64(len(target)), Module: chosen.ID(), PlanLen: int64(len(plan))}}
	st.Regions = []RegionStats{{Module: chosen.Name(), Length: int64(len(target)), Plan: len(plan), Residual: len(res), PredictErr: diffBytes(pred, target)}}

	w := &wbuf{}
	if h.Flags&FlagDebugZ != 0 {
		w.u(uint64(len(target)))
	}
	w.raw(plan)
	root := predictionHash(pred)
	w.raw(root[:])
	w.bytes(res)
	st.Body = len(w.b)
	var body []byte
	h.Frames, body = frameBody(w.b)
	patch := marshalHeader(h, body)
	st.Total = len(patch)
	return patch, nil
}

// Apply reconstructs the target from refs and patch and writes it to w.
//
// The patch is untrusted input: every offset and length in it is checked,
// each region's materialisation is compared with the hashes the encoder
// recorded, and the output is compared with the target hash before a byte
// reaches w.
func Apply(refs [][]byte, patch []byte, reg *Registry, w io.Writer) error {
	h, err := ParseHeader(patch)
	if err != nil {
		return err
	}
	if len(refs) != len(h.Refs) {
		return fmt.Errorf("presage: %d references given, patch expects %d", len(refs), len(h.Refs))
	}
	for i, r := range h.Refs {
		if int64(len(refs[i])) != r.Size {
			return fmt.Errorf("presage: reference %d is %d bytes, patch expects %d", i, len(refs[i]), r.Size)
		}
		if got := hashOf(refs[i]); got != r.B3 {
			return fmt.Errorf("presage: reference %d hashes to %s, patch expects %s", i, got, r.B3)
		}
	}
	if reg == nil {
		reg = NewRegistry()
	}
	for _, rg := range h.Regions {
		if reg.Get(rg.Module) == nil {
			return &ErrUnsupported{fmt.Sprintf("module %d", rg.Module)}
		}
	}
	body, err := readBody(h, patch)
	if err != nil {
		return err
	}
	r := &rbuf{b: body}
	shipped := refs[0]
	size := h.Size
	if h.Flags&FlagDebugZ != 0 {
		size = int64(r.un(maxPatchSize, "expanded size"))
		if r.err != nil {
			return r.err
		}
		exp, err := delta.ExpandDebug(refs[0])
		if err != nil {
			return fmt.Errorf("presage: expanding the reference's debug sections: %w", err)
		}
		refs = append([][]byte{exp}, refs[1:]...)
	}
	var total int64
	for _, rg := range h.Regions {
		total += rg.Length
	}
	if total != size {
		return fmt.Errorf("%w: regions cover %d bytes, target is %d", ErrCorrupt, total, size)
	}
	out := make([]byte, 0, size)
	for i, rg := range h.Regions {
		m := reg.Get(rg.Module)
		plan := r.take(uint64(rg.PlanLen))
		var root Hash
		copy(root[:], r.take(32))
		res := r.bytesMax(uint64(size)*4+1<<16, "residual length")
		if r.err != nil {
			return r.err
		}
		pred, err := m.Materialise(refs, plan, rg.Length)
		if err != nil {
			return err
		}
		if predictionHash(pred) != root {
			return &ErrPredictionDiverged{Region: i, Module: m.Name()}
		}
		bytes, err := applyResidual(m, pred, res, rg.Length, h.Flags)
		if err != nil {
			return err
		}
		out = append(out, bytes...)
	}
	if err := r.done(); err != nil {
		return err
	}
	if h.Flags&FlagDebugZ != 0 {
		if out, err = delta.PackDebug(out, shipped); err != nil {
			return fmt.Errorf("%w: recompressing debug sections: %v", ErrCorrupt, err)
		}
	}
	if int64(len(out)) != h.Size {
		return fmt.Errorf("%w: output is %d bytes, header says %d", ErrCorrupt, len(out), h.Size)
	}
	if got := hashOf(out); got != h.Target {
		return fmt.Errorf("presage: output hashes to %s, patch promises %s", got, h.Target)
	}
	_, err = w.Write(out)
	return err
}

// predictionHash is the root of a two-level tree over FrameSize chunks of
// the prediction: the patch ships the root (32 B, whatever the size), and
// a decoder that wants the divergent chunk named recomputes the leaves.
// A declared-length module's prediction is hashed over its own length.
func predictionHash(pred []byte) Hash {
	if len(pred) <= FrameSize {
		return hashOf(pred)
	}
	var leaves []byte
	for off := 0; off < len(pred); off += FrameSize {
		h := hashOf(pred[off:min(off+FrameSize, len(pred))])
		leaves = append(leaves, h[:]...)
	}
	return hashOf(leaves)
}

func isDeclined(err error) bool {
	return err == ErrDeclined || delta.IsUnsupported(err)
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
