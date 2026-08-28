package delta

import (
	"encoding/binary"
	"testing"

	"github.com/wjordan/go-binsync/delta/gobin"
)

// rec builds a _func record with the given shape and array values.
func rec(t *testing.T, args uint32, pcsp uint32, funcID byte, pcdata, funcdata []uint32) []byte {
	t.Helper()
	r := make([]byte, gobin.FuncSize+4*len(pcdata)+4*len(funcdata))
	binary.LittleEndian.PutUint32(r[8:], args)
	binary.LittleEndian.PutUint32(r[16:], pcsp)
	binary.LittleEndian.PutUint32(r[28:], uint32(len(pcdata)))
	r[40] = funcID
	r[43] = byte(len(funcdata))
	for k, v := range pcdata {
		binary.LittleEndian.PutUint32(r[gobin.FuncSize+4*k:], v)
	}
	for k, v := range funcdata {
		binary.LittleEndian.PutUint32(r[gobin.FuncSize+4*len(pcdata)+4*k:], v)
	}
	return r
}

// A function the release added used to get a _func record of zeroes, with
// every funcdata slot ^0. That threw away the fact that a new function is
// nearly always a compiler-generated wrapper whose record repeats: on a
// prometheus minor release three constants covered 14,037 of the 28,581
// wrong funcdata fields. The record has to be synthesised from the modal
// record of the matched functions instead.
func TestSynthTakesTheModalRecord(t *testing.T) {
	recs := [][]byte{
		rec(t, 8, 0x577, 0, []uint32{0, 0}, []uint32{0x55224, 0x55230, ^uint32(0)}),
		rec(t, 8, 0x101, 0, []uint32{0, 7}, []uint32{0x55224, 0x55230, ^uint32(0)}),
		rec(t, 8, 0x999, 0, []uint32{0, 0}, []uint32{0x55224, 0x66000, ^uint32(0)}),
		rec(t, 24, 0x577, 23, []uint32{3, 0}, []uint32{0x77000, 0x55230, 0x1234}),
		nil, // an unmatched function contributes nothing
	}
	tpl := modalRecord(recs)
	got := tpl.synth(2, 3, 41, "example.com/x.T.M")

	want := map[string]struct{ off, val uint32 }{
		"args":        {8, 8},
		"pcsp":        {16, 0x577},
		"npcdata":     {28, 2},
		"cuOffset":    {32, 41},
		"pcdata[0]":   {gobin.FuncSize, 0},
		"pcdata[1]":   {gobin.FuncSize + 4, 0},
		"funcdata[0]": {gobin.FuncSize + 8, 0x55224},
		"funcdata[1]": {gobin.FuncSize + 12, 0x55230},
		"funcdata[2]": {gobin.FuncSize + 16, ^uint32(0)},
	}
	for name, w := range want {
		if v := binary.LittleEndian.Uint32(got[w.off:]); v != w.val {
			t.Errorf("%s = %#x, want %#x", name, v, w.val)
		}
	}
	if got[43] != 3 {
		t.Errorf("nfuncdata = %d, want 3", got[43])
	}
	// a slot past the template's reach falls back to the empty values
	if v := binary.LittleEndian.Uint32(tpl.synth(3, 4, 0, "x")[gobin.FuncSize+8:]); v != 0 {
		t.Errorf("pcdata[2] = %#x, want 0", v)
	}
	if v := binary.LittleEndian.Uint32(tpl.synth(3, 4, 0, "x")[gobin.FuncSize+12+12:]); v != ^uint32(0) {
		t.Errorf("funcdata[3] = %#x, want ^0", v)
	}
}

// A tie must not depend on map iteration order: the encoder and the decoder
// have to synthesise the same record.
func TestModeOfBreaksTiesBySmallestValue(t *testing.T) {
	for range 50 {
		if v := modeOf(map[uint32]int{9: 2, 4: 2, 7: 1}, 0); v != 4 {
			t.Fatalf("modeOf = %d, want 4", v)
		}
	}
	if v := modeOf(map[uint32]int{}, ^uint32(0)); v != ^uint32(0) {
		t.Fatalf("modeOf of nothing = %#x, want the default", v)
	}
}

// funcID: the synthesised record used to guess 0, but a wrapper's id is not
// 0 and a release adds mostly wrappers. The id is learned per name key from
// the old binary, so no Go-internal constant is baked in.
func TestFuncIDLearnedFromTheOldBinary(t *testing.T) {
	oft := make([]byte, 0)
	var funcs []*gobin.Func
	addOld := func(name string, id byte) {
		off := uint32(len(oft))
		r := make([]byte, gobin.FuncSize)
		r[40] = id
		oft = append(oft, r...)
		funcs = append(funcs, &gobin.Func{Name: name, FuncOff: off})
	}
	for _, n := range []string{"a/b.T.M-fm", "a/b.U.N-fm", "a/b.V.O-fm"} {
		addOld(n, 23)
	}
	addOld("a/b.(*T).String", 23)
	addOld("a/b.(*U).String", 23)
	addOld("a/b.T.String", 0)
	addOld("a/b.plain", 0)

	tpl := modalRecord([][]byte{rec(t, 0, 0, 0, nil, nil)})
	tpl.learnFuncID(&gobin.Bin{Funcs: funcs}, oft)

	for _, c := range []struct {
		name string
		want byte
	}{
		{"c/d.W.P-fm", 23},         // method value, learned from the -fm key
		{"c/d.(*W).String", 23},    // pointer wrapper, learned from "P|String"
		{"c/d.W.String", 0},        // the value form is the real method
		{"c/d.neverSeenBefore", 0}, // no key: the modal id
	} {
		if got := tpl.synth(0, 0, 0, c.name)[40]; got != c.want {
			t.Errorf("funcID(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestNameKey(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a/b.T.M-fm", "fm"},
		{"a/b.(*T).String", "P|String"},
		{"a/b.T.String", "String"},
		{"a/b.checkValid.deferwrap1", "deferwrap"},
		{"a/b.Bucket[uint64].FractionBelow", "FractionBelow"},
		{"plain", "plain"},
	} {
		if got := nameKey(c.in); got != c.want {
			t.Errorf("nameKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
