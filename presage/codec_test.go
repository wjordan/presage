package presage

import (
	"bytes"
	"errors"
	"testing"
)

func roundTrip(t *testing.T, refs [][]byte, target []byte, o Options) []byte {
	t.Helper()
	patch, err := Encode(refs, target, o)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Apply(refs, patch, o.Registry, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), target) {
		t.Fatal("Apply did not reproduce the target")
	}
	return patch
}

func TestLZFallback(t *testing.T) {
	old := bytes.Repeat([]byte("the quick brown fox "), 500)
	new := append(append([]byte("prefix "), old[:4000]...), []byte(" tail")...)
	var st Stats
	roundTrip(t, [][]byte{old}, new, Options{Stats: &st})
	if st.Regions[0].Module != "lz" {
		t.Fatalf("module %s, want lz", st.Regions[0].Module)
	}
	if st.Total > 400 {
		t.Fatalf("patch is %d bytes for a shifted copy", st.Total)
	}
}

func TestCopyClaimsIdenticalTarget(t *testing.T) {
	old := bytes.Repeat([]byte("same bytes "), 1000)
	var st Stats
	roundTrip(t, [][]byte{[]byte("unrelated"), old}, old, Options{Stats: &st})
	if st.Regions[0].Module != "copy" || st.Regions[0].Residual > 8 {
		t.Fatalf("module %s residual %d, want copy with an empty correction", st.Regions[0].Module, st.Regions[0].Residual)
	}
}

func TestEmptyTarget(t *testing.T) {
	roundTrip(t, [][]byte{[]byte("old")}, nil, Options{})
}

func TestLoweringDropsAModule(t *testing.T) {
	old := bytes.Repeat([]byte("same bytes "), 1000)
	var st Stats
	roundTrip(t, [][]byte{old}, old, Options{Modules: []byte{ModuleLZ}, Stats: &st})
	if st.Regions[0].Module != "lz" {
		t.Fatalf("module %s, want lz when copy is not deployed", st.Regions[0].Module)
	}
}

func TestApplyRejectsWrongReference(t *testing.T) {
	old := []byte("old bytes here")
	patch, err := Encode([][]byte{old}, []byte("new bytes here"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply([][]byte{[]byte("not the old")}, patch, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("wrong reference accepted")
	}
	wrongSize := append([]byte(nil), old...)
	wrongSize[0]++
	if err := Apply([][]byte{wrongSize}, patch, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("reference with the right size and wrong hash accepted")
	}
}

func TestUnknownVersionFlagAndModule(t *testing.T) {
	old := []byte("old bytes here")
	patch, err := Encode([][]byte{old}, []byte("new bytes here"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var u *ErrUnsupported
	for _, mut := range []struct {
		name string
		at   int
		bit  byte
	}{{"version", 4, 0x40}, {"flag", 5, 0x80}} {
		p := append([]byte(nil), patch...)
		p[mut.at] |= mut.bit
		if err := Apply([][]byte{old}, p, nil, &bytes.Buffer{}); !errors.As(err, &u) {
			t.Fatalf("%s: got %v, want ErrUnsupported", mut.name, err)
		}
	}
	// A region naming a module the decoder lacks is unsupported, not corrupt.
	h, err := ParseHeader(patch)
	if err != nil {
		t.Fatal(err)
	}
	h.Regions[0].Module = 200
	p := marshalHeader(h, patch[h.BodyOff:])
	if err := Apply([][]byte{old}, p, nil, &bytes.Buffer{}); !errors.As(err, &u) {
		t.Fatalf("unknown module: got %v, want ErrUnsupported", err)
	}
}

func TestCorruptBodyIsRefused(t *testing.T) {
	old := bytes.Repeat([]byte("old bytes here "), 100)
	target := append(append([]byte(nil), old[:700]...), []byte("changed")...)
	patch, err := Encode([][]byte{old}, target, Options{})
	if err != nil {
		t.Fatal(err)
	}
	h, err := ParseHeader(patch)
	if err != nil {
		t.Fatal(err)
	}
	for i := int(h.BodyOff); i < len(patch); i += 3 {
		p := append([]byte(nil), patch...)
		p[i] ^= 0x55
		if err := Apply([][]byte{old}, p, nil, &bytes.Buffer{}); err == nil {
			t.Fatalf("corruption at %d produced output", i)
		}
	}
}
