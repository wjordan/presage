package main

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

// Every context set the plan probe reports a size for must decode back to the
// bytes it was given. The probe checks this per run, but a failure there costs
// a minute of pipeline; this catches it in milliseconds.
func TestPlanCMSetsRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	var col []byte
	var idx []byte
	for i := 0; i < 3000; i++ {
		col = binary.AppendUvarint(col, uint64(r.Intn(1<<uint(1+r.Intn(20)))))
		idx = binary.AppendUvarint(idx, uint64(r.Intn(64)))
	}
	bits := make([]byte, 2048)
	r.Read(bits)
	pair := parseVarints(idx)
	if len(pair) != 3000 {
		t.Fatalf("parseVarints round trip: got %d values, want 3000", len(pair))
	}

	cases := []struct {
		name string
		b    []byte
		set  func() cmContexts
	}{
		{"generic", col, func() cmContexts { return &genericCtx{} }},
		{"varint r0", col, func() cmContexts { return &varintCtx{rung: 0, parity: 1} }},
		{"varint r1", col, func() cmContexts { return &varintCtx{rung: 1, parity: 1} }},
		{"varint r2", col, func() cmContexts { return &varintCtx{rung: 2, parity: 1} }},
		{"runpair", col, func() cmContexts { return &varintCtx{rung: 2, parity: 2} }},
		{"paired", col, func() cmContexts { return &pairCtx{other: pair} }},
		{"bitmap", bits, func() cmContexts { return &bitmapCtx{} }},
	}
	for _, c := range cases {
		coded := cmEncode(c.b, c.set())
		back := cmDecode(coded, len(c.b), c.set())
		if !bytes.Equal(back, c.b) {
			t.Errorf("%s: round trip failed", c.name)
		}
	}

	// The concatenated set, with a column id per byte.
	cat := append(append([]byte(nil), col...), bits...)
	colID := make([]byte, len(cat))
	for i := len(col); i < len(cat); i++ {
		colID[i] = 1
	}
	set := func() cmContexts { return &concatCtx{col: colID, ncols: 2} }
	if back := cmDecode(cmEncode(cat, set()), len(cat), set()); !bytes.Equal(back, cat) {
		t.Error("concat: round trip failed")
	}
}
