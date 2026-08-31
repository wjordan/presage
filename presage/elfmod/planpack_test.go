package elfmod

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
)

// bigPlan is a marshalled plan with three columns large enough to be offered
// to the coder: an opaque one, and an index/value pair inside a field plan.
func bigPlan() []byte {
	r := rand.New(rand.NewPCG(1, 2))
	var reloc []byte
	for range 20000 {
		reloc = appendU(reloc, uint64(r.Uint32()%4096))
	}
	// The value column's magnitude tracks the index column's, which is the
	// whole of what the paired arm knows and the general compressor does not.
	fp := fieldPlan{Basis: remapShiftBasis}
	for range 20000 {
		gap := uint64(r.Uint32() % 512)
		fp.RemapIndex = appendU(fp.RemapIndex, gap)
		fp.RemapShift = appendS(fp.RemapShift, int64(gap)*4096+int64(r.Uint32()%16))
	}
	return planStreams{Reloc: reloc, Fields: appendStream(nil, fp.marshal())}.marshal()
}

func TestPlanPackRoundTrip(t *testing.T) {
	defer func(g int) { planCMMinGain = g }(planCMMinGain)
	planCMMinGain = 0
	plan := bigPlan()
	packed, note := packPlan(plan)
	if packed[0] != planPackSpans {
		t.Fatalf("nothing was carved out: %s", note)
	}
	if !strings.Contains(note, "field remap-target") {
		t.Errorf("the paired column is missing from the ledger: %s", note)
	}
	// packPlan primes the cache with what it packed, which is the answer this
	// test is checking: drop it so the columns are really decoded.
	planCacheKey = [32]byte{}
	got, err := unpackPlan(packed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plan) {
		t.Fatal("the unpacked plan differs from the marshalled one")
	}
	// The whole point: the columns are smaller than the blob they left.
	if len(packed) >= len(plan) {
		t.Errorf("packed plan is %d bytes, plan is %d", len(packed), len(plan))
	}
}

func TestPlanPackUnknownForms(t *testing.T) {
	defer func(g int) { planCMMinGain = g }(planCMMinGain)
	planCMMinGain = 0
	packed, _ := packPlan(bigPlan())
	if _, err := unpackPlan([]byte{9, 1, 2}); err == nil {
		t.Error("an unknown plan packing was accepted")
	}
	// The codec and context bytes of the first span sit just past the header.
	for _, at := range []int{0, 1} {
		bad := bytes.Clone(packed)
		r := &planReader{b: bad[1:]}
		r.u()
		r.u()
		r.u()
		r.u()
		r.u()
		off := len(bad) - len(r.b) + at
		bad[off] = 0x7f
		if _, err := unpackPlan(bad); err == nil {
			t.Errorf("an unknown byte at %d was accepted", off)
		}
	}
}

func TestPlanPackOff(t *testing.T) {
	defer func(p string) { planCMPolicy = p }(planCMPolicy)
	planCMPolicy = "off"
	plan := bigPlan()
	packed, _ := packPlan(plan)
	if packed[0] != planPackNone {
		t.Fatal("a column was coded with the policy off")
	}
	got, err := unpackPlan(packed)
	if err != nil || !bytes.Equal(got, plan) {
		t.Fatalf("whole-plan round trip failed: %v", err)
	}
}
