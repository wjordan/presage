//go:build corpus

package presage_test

import (
	"io"
	"os"
	"testing"

	"github.com/wjordan/presage/presage"
	"github.com/wjordan/presage/presage/modules"
)

// BenchmarkApplyCorpus provides a one-shot apply target for allocation and
// heap profiles without paying for an encode. It is deliberately opt-in: the
// caller supplies a reference and an already encoded patch from the same
// build through PRESAGE_APPLY_OLD and PRESAGE_APPLY_PATCH.
//
// Example:
//
//	PRESAGE_APPLY_OLD=old PRESAGE_APPLY_PATCH=patch \
//	  go test -tags corpus ./presage -run '^$' \
//	  -bench BenchmarkApplyCorpus -benchtime=1x -memprofile apply.mem
func BenchmarkApplyCorpus(b *testing.B) {
	oldPath, patchPath := os.Getenv("PRESAGE_APPLY_OLD"), os.Getenv("PRESAGE_APPLY_PATCH")
	if oldPath == "" || patchPath == "" {
		b.Skip("PRESAGE_APPLY_OLD and PRESAGE_APPLY_PATCH are required")
	}
	old, err := os.ReadFile(oldPath)
	if err != nil {
		b.Fatal(err)
	}
	patch, err := os.ReadFile(patchPath)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(old)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := presage.Apply([][]byte{old}, patch, modules.Registry(), io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
