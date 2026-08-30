package elfmod

import (
	"testing"

	"github.com/wjordan/presage/presage/dwarf"
)

// TestSectionRunsClip pins how a whole-image equivalence is cut to one
// section: only the part whose source lies in the old section and whose
// destination lies in the new one survives, in section-relative offsets.
func TestSectionRunsClip(t *testing.T) {
	s := dwarf.Sec{OldOff: 100, OldSize: 50, NewOff: 200, NewSize: 40}
	ep := equivalencePlan{Eqs: []equivalence{
		{Src: 90, Dst: 190, N: 30},  // straddles both starts: clipped to 20 at 10/10
		{Src: 140, Dst: 230, N: 40}, // runs past both ends: 10 bytes
		{Src: 0, Dst: 200, N: 10},   // source outside: dropped
		{Src: 120, Dst: 300, N: 10}, // destination outside: dropped
	}}
	runs := sectionRuns(ep, s)
	want := []struct{ src, dst, n uint64 }{{0, 0, 20}, {40, 30, 10}}
	if len(runs) != len(want) {
		t.Fatalf("got %d runs %v, want %d", len(runs), runs, len(want))
	}
	for i, w := range want {
		if runs[i].Src != w.src || runs[i].Dst != w.dst || runs[i].N != w.n {
			t.Errorf("run %d = %+v, want %+v", i, runs[i], w)
		}
	}
	if sectionRuns(ep, dwarf.Sec{}) != nil {
		t.Error("an absent section produced runs")
	}
}
