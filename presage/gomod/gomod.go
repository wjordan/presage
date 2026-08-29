// Package gomod is presage's Go linux/amd64 module: the predict-then-correct
// transform of delta, exposed as one region-level op. Its plan is the
// transform's layout and stage-1 streams; its materialisation is the
// predicted whole file (SPEC §4.4; presage-core.md §4).
package gomod

import (
	"github.com/wjordan/go-binsync/delta"
	"github.com/wjordan/go-binsync/presage"
)

// Module is the Go module. Register it with presage.Registry.Add.
type Module struct {
	// Stats, if non-nil, receives the transform's statistics on Analyse.
	Stats *delta.Stats
}

func (Module) ID() byte     { return presage.ModuleGo }
func (Module) Name() string { return "go" }
func (Module) Exact() bool  { return true }

// Analyse runs the transform on reference 0 and the target. A pair the
// transform declines — not a Go binary of the supported release — is
// reported as declined, and the core falls back to lz.
func (m Module) Analyse(refs [][]byte, target []byte) (plan, pred []byte, err error) {
	return delta.GoAnalyse(refs[0], target, m.Stats)
}

// Materialise expands the plan against reference 0.
func (Module) Materialise(refs [][]byte, plan []byte, length int64) ([]byte, error) {
	return delta.GoPredict(refs[0], plan, length)
}

// Registry returns the core modules plus this one.
func Registry() *presage.Registry {
	r := presage.NewRegistry()
	r.Add(Module{})
	return r
}
