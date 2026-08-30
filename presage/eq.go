package presage

import (
	"fmt"

	"github.com/wjordan/presage/presage/eqmatch"
)

// ModuleEq is the equivalence module: the region is predicted by copying
// matched runs of reference 0 (eqmatch) into place, zero elsewhere, and the
// positional correction does the rest.
//
// It is not in the default registry. On its own it loses to lz, which is the
// same fuzzy matching with a literal stream: 2.5-2.8x larger on every pair
// measured (docs/general/go-module-results.md). Its runs are the base under
// modules that model only part of a file — the layered DWARF plan — and the
// harness's -native-equivalences; that is where its measured gain is.
const ModuleEq = 3

// EqModule is ModuleEq; its parameters are the matcher's.
type EqModule struct{ Params eqmatch.Params }

func (EqModule) ID() byte     { return ModuleEq }
func (EqModule) Name() string { return "eq" }
func (EqModule) Exact() bool  { return true }

func (m EqModule) Analyse(refs [][]byte, target []byte) ([]byte, []byte, error) {
	runs := eqmatch.Match(refs[0], target, m.Params)
	if len(runs) == 0 {
		return nil, nil, ErrDeclined
	}
	return eqmatch.Encode(runs), layRuns(refs[0], runs, len(target)), nil
}

func (EqModule) Materialise(refs [][]byte, plan []byte, length int64) ([]byte, error) {
	if length < 0 || length > maxPatchSize {
		return nil, fmt.Errorf("%w: eq region of %d bytes", ErrCorrupt, length)
	}
	runs, err := eqmatch.Decode(plan, uint64(len(refs[0])), uint64(length))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return layRuns(refs[0], runs, int(length)), nil
}

func layRuns(src []byte, runs []eqmatch.Run, length int) []byte {
	out := make([]byte, length)
	for _, r := range runs {
		copy(out[r.Dst:r.Dst+r.N], src[r.Src:r.Src+r.N])
	}
	return out
}
