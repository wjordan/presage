package delta

import (
	"fmt"

	"github.com/wjordan/go-binsync/delta/gobin"
	"github.com/wjordan/go-binsync/delta/x86"
)

// The Go-table module: the Go-aware codec's metadata regeneration offered
// as one layer to a predictor that already handles the code.
//
// EncodeGoTables builds the same layout and stage-1 streams as the Go-aware
// codec -- section table, moduledata values, function list, data maps,
// transmitted funcnametab/filetab, corrected cutab/pctab/go:func.* -- and
// ApplyGoTables replays them into an image someone else is assembling,
// writing every section of the new image except those the caller keeps for
// itself (typically .text when it has a better code model, and the
// relocation table when a layer of its own rebuilds it). Sections the
// module has no model for are copied from the old image by name, as the
// Go-aware codec does. No correction is included: the caller measures its
// own.

// GoTablesStats describes what one application wrote.
type GoTablesStats struct {
	Sections int
	Bytes    int
	Funcs    int
	Matched  int
}

// goTablesReader opens a Go-table plan: its first byte names the transform
// the layout was written at, so a reader refuses one from a newer codec.
func goTablesReader(plan []byte) (*rbuf, byte, error) {
	if len(plan) == 0 {
		return nil, 0, fmt.Errorf("%w: empty go-table plan", errCorrupt)
	}
	if tf := plan[0]; tf < TransformGoAMD64 || tf > maxTransform {
		return nil, 0, fmt.Errorf("%w: go-table plan transform %d is not readable", errCorrupt, tf)
	}
	return &rbuf{b: plan[1:]}, plan[0], nil
}

// EncodeGoTables returns the Go-table plan for old -> new at the codec's
// current transform, or an *Unsupported-wrapped error when either is not a
// Go binary the module understands.
func EncodeGoTables(old, new []byte) ([]byte, error) {
	g, err := predictGoAMD64(old, new, maxTransform)
	if err != nil {
		return nil, err
	}
	w := &wbuf{}
	w.b = append(w.b, maxTransform)
	w.bytes(g.layRaw)
	w.u(uint64(len(g.s1aNew)))
	w.bytes(g.s1a)
	w.u(uint64(len(g.s1bNew)))
	w.bytes(g.s1b)
	return w.b, nil
}

// ApplyGoTables predicts the Go sections of the new binary into out, which
// is already the new file's length. Sections for which skip returns true
// are left as they are.
func ApplyGoTables(old, plan, out []byte, skip func(name string) bool) (GoTablesStats, error) {
	ob, err := gobin.Parse(old)
	if err != nil {
		return GoTablesStats{}, fmt.Errorf("delta: go tables need a Go binary the module understands: %w", err)
	}
	r, tf, err := goTablesReader(plan)
	if err != nil {
		return GoTablesStats{}, err
	}
	layRaw := r.bytes()
	s1aLen := r.un(uint64(len(out)), "stage 1a table length")
	s1a := r.bytes()
	s1bLen := r.un(uint64(len(out)), "stage 1b table length")
	s1b := r.bytes()
	if r.err != nil {
		return GoTablesStats{}, r.err
	}
	if len(r.b) != 0 {
		return GoTablesStats{}, fmt.Errorf("%w: trailing go-table plan bytes", errCorrupt)
	}
	skel, m, err := skeletonFrom(ob, layRaw, tf)
	if err != nil {
		return GoTablesStats{}, err
	}
	lay, err := decodeLayout(layRaw, ob, tf)
	if err != nil {
		return GoTablesStats{}, err
	}
	if lay.NewLen != uint64(len(out)) {
		return GoTablesStats{}, fmt.Errorf("%w: go-table layout is for a %d-byte file, the image is %d", errCorrupt, lay.NewLen, len(out))
	}
	s1aNew, err := plainPatch(stage1aBlobs(ob), s1a, int64(s1aLen))
	if err != nil {
		return GoTablesStats{}, err
	}
	if err := fillTables(skel, stage1aRanges(skel.Pcln), s1aNew); err != nil {
		return GoTablesStats{}, err
	}
	mp := newMapper(ob, skel, m, lay)
	bp := predictBlobs(ob, skel, m, mp)
	s1bNew, err := plainPatch(bp.concat(), s1b, int64(s1bLen))
	if err != nil {
		return GoTablesStats{}, err
	}
	if err := fillTables(skel, stage1bRanges(skel.Pcln), s1bNew); err != nil {
		return GoTablesStats{}, err
	}
	mp.blobs = bp
	predictHoles(out, ob, skel)
	predictHeaders(out, ob, skel, mp.entryLookup())
	st := GoTablesStats{Funcs: len(skel.Funcs), Matched: m.Exact + m.Norm + m.Content}
	predictSections(out, ob, skel, lay, mp, &x86.Stats{}, func(name string) bool {
		if skip != nil && skip(name) {
			return true
		}
		if s := skel.Sects[name]; s != nil && !s.NoBits {
			st.Sections++
			st.Bytes += int(s.Size)
		}
		return false
	})
	return st, nil
}

// GoFuncMap is one matched function of a Go-table plan: where the old body
// starts and how long it is, and the same for the new one.
type GoFuncMap struct {
	OldEntry, OldSize, NewEntry, NewSize uint64
}

// GoFunctionMap derives the function map a Go-table plan implies, in new
// address order, so a predictor built on the module can share it instead of
// transmitting its own.
func GoFunctionMap(old, plan []byte) ([]GoFuncMap, error) {
	ob, err := gobin.Parse(old)
	if err != nil {
		return nil, fmt.Errorf("delta: go function map needs a Go binary the module understands: %w", err)
	}
	r, tf, err := goTablesReader(plan)
	if err != nil {
		return nil, err
	}
	layRaw := r.bytes()
	if r.err != nil {
		return nil, r.err
	}
	skel, m, err := skeletonFrom(ob, layRaw, tf)
	if err != nil {
		return nil, err
	}
	out := make([]GoFuncMap, 0, len(skel.Funcs))
	for j, nf := range skel.Funcs {
		i := m.NewToOld[j]
		if i < 0 {
			continue
		}
		of := ob.Funcs[i]
		out = append(out, GoFuncMap{OldEntry: of.Entry, OldSize: of.End - of.Entry, NewEntry: nf.Entry, NewSize: nf.End - nf.Entry})
	}
	return out, nil
}

// GoAddressLookup returns the address map a Go-table plan implies, as the
// x86 relocator sees it: function entries through the function map, data
// through the content maps and shift tables, unknown where the module has
// no answer. Both sides of a patch can build it from the plan alone.
func GoAddressLookup(old, plan []byte) (func(uint64) x86.Target, error) {
	ob, err := gobin.Parse(old)
	if err != nil {
		return nil, fmt.Errorf("delta: go address map needs a Go binary the module understands: %w", err)
	}
	r, tf, err := goTablesReader(plan)
	if err != nil {
		return nil, err
	}
	layRaw := r.bytes()
	if r.err != nil {
		return nil, r.err
	}
	skel, m, err := skeletonFrom(ob, layRaw, tf)
	if err != nil {
		return nil, err
	}
	lay, err := decodeLayout(layRaw, ob, tf)
	if err != nil {
		return nil, err
	}
	return newMapper(ob, skel, m, lay).lookup(nil), nil
}

// GoSection is one allocated section as the Go-table module sees it.
type GoSection struct {
	Addr, Off, Size uint64
}

// GoSectionGeometry returns the allocated sections of the old binary and of
// the new one the plan describes, in address order. A layer that needs the
// section geometry of both images can take it from here instead of
// transmitting it.
func GoSectionGeometry(old, plan []byte) (oldSecs, newSecs []GoSection, err error) {
	ob, err := gobin.Parse(old)
	if err != nil {
		return nil, nil, fmt.Errorf("delta: go section geometry needs a Go binary the module understands: %w", err)
	}
	r, tf, err := goTablesReader(plan)
	if err != nil {
		return nil, nil, err
	}
	layRaw := r.bytes()
	if r.err != nil {
		return nil, nil, r.err
	}
	skel, _, err := skeletonFrom(ob, layRaw, tf)
	if err != nil {
		return nil, nil, err
	}
	// Sections without file bytes have no offsets to project through; two
	// of them share one, and an address in the second would come back in
	// the first.
	for _, s := range ob.Order {
		if s.NoBits {
			continue
		}
		oldSecs = append(oldSecs, GoSection{Addr: s.Addr, Off: s.Off, Size: s.Size})
	}
	for _, s := range skel.Order {
		if s.NoBits {
			continue
		}
		newSecs = append(newSecs, GoSection{Addr: s.Addr, Off: s.Off, Size: s.Size})
	}
	return oldSecs, newSecs, nil
}
