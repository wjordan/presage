// Package presage is a predictive binary-patch codec: modules predict
// structural regions of the target from reference objects, the core
// materialises the predictions, verifies them, and codes what they got
// wrong (docs/general/SPEC.md; docs/general/presage-core.md for this
// milestone's shape).
package presage

import (
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/internal/cz"
)

// Options controls Encode.
type Options struct {
	// Registry holds the modules the encoder may choose from. Nil means
	// the core modules only.
	Registry *Registry
	// Modules, if set, names the module ids the decoder has; a
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
	return fmt.Sprintf("presage: region %d (%s): the prediction differs from the encoder's", e.Region, e.Module)
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
	cz.PreferExactUnder(int64(len(target)), 128<<20)
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
	predRes := pred
	if f, ok := chosen.(Finaliser); ok {
		predRes = f.MaskResidual(plan, pred, target)
	}
	res, rflags, err := residual(chosen, refs, plan, predRes, target)
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
	}
	// The references are verified alongside the prediction rather than ahead
	// of it: hashing 291 MB is a tenth of a second that has nothing to wait
	// for. A wrong reference is still the error the caller gets, because this
	// result is consulted before any error the rest of the apply raises and
	// before the one place that writes.
	refsChecked := make(chan error, 1)
	go func() {
		for i, r := range h.Refs {
			if got := hashOf(refs[i]); got != r.B3 {
				refsChecked <- fmt.Errorf("presage: reference %d hashes to %s, patch expects %s", i, got, r.B3)
				return
			}
		}
		refsChecked <- nil
	}()
	err = applyBody(refs, patch, h, reg, w, refsChecked)
	if refsErr := <-refsChecked; refsErr != nil {
		return refsErr
	}
	return err
}

// applyBody is Apply once the header and the reference sizes are known.
// refsChecked carries the reference verification running alongside it; the
// body takes it from the channel and puts it back, so Apply can consult it
// however the body ended.
func applyBody(refs [][]byte, patch []byte, h *Header, reg *Registry, w io.Writer, refsChecked chan error) error {
	var err error
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
	// A single region -- what every exact module produces -- hands its own
	// buffer straight through: concatenating one region into a second
	// full-size buffer was 291 MB of memory and a tenth of a second of
	// memmove on Chrome, to produce a copy of what it was given.
	var out []byte
	if len(h.Regions) != 1 {
		out = make([]byte, 0, size)
	}
	for i, rg := range h.Regions {
		m := reg.Get(rg.Module)
		plan := r.take(uint64(rg.PlanLen))
		var root Hash
		copy(root[:], r.take(32))
		res := r.bytesMax(uint64(size)*4+1<<16, "residual length")
		if r.err != nil {
			return r.err
		}
		// The correction's side-free streams have everything they need
		// already: they start now and run against the prediction rather than
		// after it.
		var pf *splitPrefetch
		if h.Flags&FlagSplitResidual != 0 {
			pf = prefetchSplitResidual(res, int(rg.Length))
		}
		pred, err := m.Materialise(refs, plan, rg.Length)
		if err != nil {
			return err
		}
		if predictionHash(pred) != root {
			return &ErrPredictionDiverged{Region: i, Module: m.Name()}
		}
		bytes, err := applyResidual(m, refs, plan, pred, res, rg.Length, h.Flags, pf)
		if err != nil {
			return err
		}
		if f, ok := m.(Finaliser); ok {
			if err := f.Finalise(plan, bytes); err != nil {
				return err
			}
		}
		if len(h.Regions) == 1 {
			out = bytes
		} else {
			out = append(out, bytes...)
		}
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
	// Nothing has reached w yet, so this is the last moment the reference
	// check can still stop the write.
	refsErr := <-refsChecked
	refsChecked <- refsErr
	if refsErr != nil {
		return refsErr
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
	// The leaves are independent hashes of fixed chunks, so they are computed
	// concurrently and written back by index: the leaf string, and so the root,
	// is what a serial pass produced.
	n := (len(pred) + FrameSize - 1) / FrameSize
	leaves := make([]byte, n*len(Hash{}))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			off := i * FrameSize
			h := hashOf(pred[off:min(off+FrameSize, len(pred))])
			copy(leaves[i*len(h):], h[:])
		}(i)
	}
	wg.Wait()
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
