package gomod

import (
	"errors"
	"testing"

	"github.com/wjordan/presage/presage"
)

func TestPlanParts(t *testing.T) {
	p := plan{tf: []byte("layout"), dw: []byte("dwarf"), runs: nil}
	b := p.marshal()
	got, err := parsePlan(b)
	if err != nil || string(got.tf) != "layout" || string(got.dw) != "dwarf" || len(got.runs) != 0 {
		t.Fatalf("parsed %+v, %v", got, err)
	}
	for _, bad := range [][]byte{b[:len(b)-1], append(append([]byte{}, b...), 0), {0xff, 0xff, 0xff, 0xff, 0xff}} {
		if _, err := parsePlan(bad); !errors.Is(err, presage.ErrCorrupt) {
			t.Fatalf("plan %x: %v, want corrupt", bad, err)
		}
	}
}
