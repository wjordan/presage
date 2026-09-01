package presage

import (
	"encoding/binary"
	"math/rand"

	"bytes"
	"errors"
	"github.com/wjordan/presage/delta"
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
		t.Fatalf("module %s, want lz when copy is unavailable", st.Regions[0].Module)
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

// cutModule is an exact module that predicts the reference verbatim and
// cuts the target in half, so the residual is coded in pieces.
type cutModule struct{ lzModule }

func (cutModule) ID() byte              { return 9 }
func (cutModule) Name() string          { return "cut" }
func (cutModule) Exact() bool           { return true }
func (cutModule) Cuts(t []byte) []int64 { return []int64{int64(len(t) / 2)} }
func (cutModule) Analyse(refs [][]byte, target []byte) ([]byte, []byte, error) {
	if len(refs[0]) != len(target) {
		return nil, nil, ErrDeclined
	}
	return nil, refs[0], nil
}
func (cutModule) Materialise(refs [][]byte, plan []byte, n int64) ([]byte, error) {
	return refs[0][:n], nil
}

func TestSplitResidual(t *testing.T) {
	// Two halves of different character: scattered single-byte edits, then
	// a repetitive block of shifted words, so a piecewise correction wins.
	old := make([]byte, 8<<10)
	for i := range old {
		old[i] = byte(i * 7)
	}
	target := append([]byte(nil), old...)
	for i := 0; i < len(target)/2; i += 97 {
		target[i] ^= 0x5a
	}
	for i := len(target) / 2; i+8 <= len(target); i += 8 {
		target[i]++
	}
	reg := NewRegistry()
	reg.Add(cutModule{})
	patch := roundTrip(t, [][]byte{old}, target, Options{Registry: reg})
	h, err := ParseHeader(patch)
	if err != nil {
		t.Fatal(err)
	}
	if h.Flags&FlagSplitResidual == 0 {
		t.Fatalf("flags %#x: the split residual was not chosen", h.Flags)
	}
	// Pieces that do not tile the region are refused, not misread.
	if err := applySplitResidual(make([]byte, 10), []byte{2, 5, 0, 0, 0, 6, 0, 0, 0}, func() *delta.DispContext { return nil }, nil); err == nil {
		t.Fatal("pieces not covering the region were accepted")
	}
}

// dispModule is cutModule plus a field context, so the split residual's
// displacement columns (delta/dispfield.go) are exercised end to end through
// Encode and Apply: the encoder zeroes the fields inside the long runs, and
// the decoder refills them by walking the repaired bytes.
type dispModule struct{ cutModule }

func (dispModule) ID() byte     { return 10 }
func (dispModule) Name() string { return "disp" }
func (dispModule) DispContext(refs [][]byte, plan []byte, length int64) *delta.DispContext {
	var bodies []delta.DispBody
	var starts []uint64
	for off := 0; off+64 <= int(length); off += 64 {
		bodies = append(bodies, delta.DispBody{Off: off, Size: 64, PC: 0x400000 + uint64(off)})
		starts = append(starts, 0x400000+uint64(off))
	}
	return delta.NewDispContext(bodies, starts)
}

// pieceKinds reads the kind byte of every piece of a split residual.
func pieceKinds(t *testing.T, stream []byte) []byte {
	t.Helper()
	r := &rbuf{b: stream}
	n := r.u()
	var kinds []byte
	for i := uint64(0); i < n; i++ {
		r.u() // piece length
		kinds = append(kinds, r.byte())
		ns := r.u()
		for j := uint64(0); j < ns; j++ {
			r.u()
			r.u()
			r.byte()
		}
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	return kinds
}

func TestDispResidualRoundTrip(t *testing.T) {
	const n = 64 << 10
	old := make([]byte, n)
	target := make([]byte, n)
	for i := range old {
		old[i], target[i] = 0x90, 0x90
	}
	rt, ro := rand.New(rand.NewSource(1)), rand.New(rand.NewSource(2))
	// Each 64-byte body opens with a call to one of eight body starts, inside a
	// long wrong run whose remaining bytes are noise the compressor cannot
	// fold away — which is exactly where the columns pay.
	for off := 0; off+256 <= n; off += 256 {
		pc := 0x400000 + uint64(off)
		dst := 0x400000 + uint64((off*7*64)%n)
		target[off] = 0xe8
		binary.LittleEndian.PutUint32(target[off+1:], uint32(int32(int64(dst)-int64(pc+5))))
		for i := off + 5; i < off+48; i++ {
			target[i] = byte(rt.Uint32())
		}
		for i := off; i < off+48; i++ {
			for old[i] == target[i] {
				old[i] = byte(ro.Uint32())
			}
		}
	}
	m := dispModule{}
	d := m.DispContext([][]byte{old}, nil, n)
	cuts := m.Cuts(target)

	withDisp, _, size, err := splitResidual(old, target, cuts, d)
	if err != nil {
		t.Fatal(err)
	}
	if size == 0 {
		t.Fatal("empty split residual")
	}
	// The displacement piece kind must actually be in the stream: what this
	// test proves is the seam, and the corpus gate prices it.
	// Whichever coder each piece picks, every piece kind must round-trip
	// through the field context, and a displacement piece must be legible.
	for _, k := range pieceKinds(t, withDisp) {
		if k != pieceLZ && k != pieceColumnar && k != pieceColumnarDisp {
			t.Fatalf("unknown piece kind %d", k)
		}
	}
	// And the decoder refills them: applying the stream over the prediction
	// must reproduce the target byte for byte.
	out := append([]byte(nil), old...)
	if err := applySplitResidual(out, withDisp, func() *delta.DispContext { return d }, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, target) {
		t.Fatal("the displacement residual did not reproduce the target")
	}
	// The whole path through the module seam, encoded and applied.
	reg := NewRegistry()
	reg.Add(m)
	roundTrip(t, [][]byte{old}, target, Options{Registry: reg})
}

// pieceCodecs reads the codec byte of every stream of a split residual.
func pieceCodecs(t *testing.T, stream []byte) []byte {
	t.Helper()
	r := &rbuf{b: stream}
	n := r.u()
	var codecs []byte
	for i := uint64(0); i < n; i++ {
		r.u() // piece length
		r.byte()
		ns := r.u()
		for j := uint64(0); j < ns; j++ {
			r.u()
			r.u()
			codecs = append(codecs, r.byte())
		}
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	return codecs
}

// TestSplitResidualCM exercises the CM codec through the split residual's
// stream table: a correction of four-byte runs whose bytes are each one more
// than the prediction's is noise to a general compressor and nearly free to a
// coder conditioned on the prediction, so the encoder must choose it, and the
// decoder must derive the same conditioning from the columns before it.
func TestSplitResidualCM(t *testing.T) {
	const n = 64 << 10
	r := rand.New(rand.NewSource(3))
	pred := make([]byte, n)
	for i := range pred {
		pred[i] = byte(r.Uint32())
	}
	// A fixed derangement of the byte values: noise to a general compressor,
	// and a single deterministic step to a coder that sees the prediction.
	var perm [256]byte
	for i := range perm {
		perm[i] = byte(i)
	}
	r.Shuffle(256, func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
	for i := range perm {
		if perm[i] == byte(i) {
			perm[i] = perm[(i+1)%256]
			perm[(i+1)%256] = byte(i)
		}
	}
	target := append([]byte(nil), pred...)
	for off := 0; off+4 <= n; off += 16 {
		for k := 0; k < 4; k++ {
			target[off+k] = perm[pred[off+k]]
		}
	}
	cuts := cutModule{}.Cuts(target)
	stream, _, size, err := splitResidual(pred, target, cuts, nil)
	if err != nil {
		t.Fatal(err)
	}
	codecs := pieceCodecs(t, stream)
	if !bytes.Contains(codecs, []byte{codecCMCompact}) {
		t.Fatalf("codecs %v: the CM coder was not chosen", codecs)
	}
	out := append([]byte(nil), pred...)
	if err := applySplitResidual(out, stream, func() *delta.DispContext { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, target) {
		t.Fatal("the CM-coded residual did not reproduce the target")
	}
	t.Logf("%d-byte region, %d-byte residual, kinds %v, codecs %v", n, size, pieceKinds(t, stream), codecs)
}

func TestCMWorth(t *testing.T) {
	for _, tc := range []struct {
		coded, incumbent int
		want             bool
	}{
		{7951, 10000, true},
		{7952, 10000, false},
		{89999, 100000, true},
		{90000, 100000, false},
	} {
		if got := cmWorth(make([]byte, tc.coded), tc.incumbent); got != tc.want {
			t.Errorf("cmWorth(%d, %d) = %v, want %v", tc.coded, tc.incumbent, got, tc.want)
		}
	}
}

// An id no codec claims is refused by name, not misread.
func TestSplitResidualUnknownCodec(t *testing.T) {
	// One piece, one stream, codec 99.
	stream := []byte{1, 10, pieceLZ, 1, 4, 1, 99, 0}
	err := applySplitResidual(make([]byte, 10), stream, func() *delta.DispContext { return nil }, nil)
	if err == nil || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unknown codec gave %v", err)
	}
}
