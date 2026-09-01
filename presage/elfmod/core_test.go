package elfmod

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/wjordan/presage/presage"
	"github.com/wjordan/presage/presage/symbols"
)

// pad extends b with 0xcc to the next multiple of 16.
func pad(b []byte) []byte {
	for len(b)%16 != 0 {
		b = append(b, 0xcc)
	}
	return b
}

// callRel32 is `call rel32` reaching target from the instruction at pc.
func callRel32(pc, target uint64) []byte {
	b := []byte{0xe8, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[1:], uint32(int32(int64(target)-int64(pc+5))))
	return b
}

// twoFuncs lays function A (a prologue, a call to B, a return) at 0 and B
// at bOff; B's body is bBody.
func twoFuncs(textAddr, bOff uint64, bBody []byte) []byte {
	a := []byte{0x55, 0x48, 0x89, 0xe5}
	a = append(a, callRel32(textAddr+4, textAddr+bOff)...)
	a = append(a, 0x5d, 0xc3)
	a = pad(a)
	for uint64(len(a)) < bOff {
		a = append(a, 0xcc)
	}
	return pad(append(a, bBody...))
}

var bodyB = []byte{0xb8, 0x01, 0, 0, 0, 0xc3}
var bodyB2 = []byte{0x48, 0x31, 0xc0, 0xb8, 0x02, 0, 0, 0, 0xc3} // xor rax,rax; mov eax,2; ret

func TestStructurePlanRoundTrip(t *testing.T) {
	t.Parallel()
	oldText := twoFuncs(0x1000, 32, bodyB)
	p := predictionPlan{OldAddr: 0x1000, NewAddr: 0x2000, TargetLen: 96,
		Maps: []mapping{
			{Src: 0, SrcSize: 16, Dst: 0, DstSize: 16, Copy: true},
			{Src: 32, SrcSize: 6, Dst: 48, DstSize: 9, Copy: false},
		},
		Points: []addressPoint{{Old: 0x1020, New: 0x2030}, {Old: 0x1040, New: 0x2060}},
		Ranges: []addressRange{{Old: 0x3000, New: 0x4000, Size: 0x100}},
	}
	b, err := p.marshal(oldText)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unmarshalPlan(b, oldText, section{Size: uint64(len(oldText))})
	if err != nil {
		t.Fatal(err)
	}
	if got.OldAddr != p.OldAddr || got.NewAddr != p.NewAddr || got.TargetLen != p.TargetLen ||
		len(got.Maps) != 2 || got.Maps[0] != p.Maps[0] || got.Maps[1] != p.Maps[1] ||
		len(got.Points) != 2 || got.Points[0] != p.Points[0] || got.Points[1] != p.Points[1] ||
		len(got.Ranges) != 1 || got.Ranges[0] != p.Ranges[0] {
		t.Fatalf("round trip differs: %+v vs %+v", got, p)
	}
	for n := 0; n < len(b); n++ {
		if _, err := unmarshalPlan(b[:n], oldText, section{Size: uint64(len(oldText))}); err == nil {
			t.Fatalf("truncated plan of %d bytes accepted", n)
		}
	}
	lk := newAddressLookup(got)
	if x := lk.target(0x1020); x.Addr != 0x2030 {
		t.Errorf("point lookup %#x", x.Addr)
	}
	if x := lk.target(0x1004); x.Addr != 0x2004 || !x.Known {
		t.Errorf("map lookup %+v", x)
	}
	if x := lk.target(0x3010); x.Addr != 0x4010 {
		t.Errorf("range lookup %#x", x.Addr)
	}
	if x := lk.target(0x9000); x.Known {
		t.Errorf("unknown lookup answered %+v", x)
	}
}

func TestEquivalencePlanRoundTrip(t *testing.T) {
	t.Parallel()
	window := codeWindow{Old: section{Addr: 0x1000, Off: 100, Size: 500}, New: section{Addr: 0x2000, Off: 100, Size: 600}}
	ep := equivalencePlan{OldLen: 1000, NewLen: 1200,
		Windows: []codeWindow{window},
		Eqs:     []equivalence{{Src: 0, Dst: 0, N: 100}, {Src: 150, Dst: 120, N: 40}, {Src: 700, Dst: 900, N: 100}}}
	maps := []mapping{{Src: 40, SrcSize: 60, Dst: 10, DstSize: 60, Copy: true}}
	structures := []predictionPlan{{OldAddr: window.Old.Addr, NewAddr: window.New.Addr, Maps: maps}}
	for _, pred := range []*srcPredictor{nil, newSrcPredictor(structures, ep.Windows)} {
		b, err := ep.marshal(pred)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseEquivalencePlan(b)
		if err != nil {
			t.Fatal(err)
		}
		if got.Predicted != (pred != nil) {
			t.Fatal("predicted flag lost")
		}
		eqs, err := decodeEquivalences(got, pred)
		if err != nil {
			t.Fatal(err)
		}
		if len(eqs) != 3 || eqs[0] != ep.Eqs[0] || eqs[1] != ep.Eqs[1] || eqs[2] != ep.Eqs[2] {
			t.Fatalf("runs differ: %v", eqs)
		}
		if _, err := decodeEquivalences(got, nil); pred != nil && err == nil {
			t.Fatal("predicted plan decoded without a map")
		}
	}
	sm := newSourceEquivalenceMapper(ep)
	if off, ok := sm.project(160); !ok || off != 130 {
		t.Errorf("project inside run: %d %v", off, ok)
	}
	if off, ok := sm.project(400); !ok || off != 370 {
		t.Errorf("project between runs extrapolates from the nearest: %d %v", off, ok)
	}
	if _, ok := sm.within(400); ok {
		t.Error("within extrapolated")
	}
}

func TestRetargetAndChoice(t *testing.T) {
	t.Parallel()
	const oldAddr, newAddr = 0x1000, 0x2000
	oldText := twoFuncs(oldAddr, 32, bodyB)
	newText := twoFuncs(newAddr, 64, bodyB) // B moved by 32; A's call changes
	window := codeWindow{
		Old: section{Addr: oldAddr, Off: 0, Size: uint64(len(oldText))},
		New: section{Addr: newAddr, Off: 0, Size: uint64(len(newText))},
	}
	ep := equivalencePlan{OldLen: uint64(len(oldText)), NewLen: uint64(len(newText)),
		Windows: []codeWindow{window},
		Eqs:     []equivalence{{Src: 0, Dst: 0, N: 16}, {Src: 32, Dst: 64, N: 16}}}
	structure := predictionPlan{OldAddr: oldAddr, NewAddr: newAddr, TargetLen: uint64(len(newText)),
		Maps: []mapping{{Src: 0, SrcSize: 16, Dst: 0, DstSize: 16, Copy: true}, {Src: 32, SrcSize: 6, Dst: 64, DstSize: 6, Copy: true}}}
	structures := []predictionPlan{structure}
	out := layImage(oldText, ep)
	if bytes.Equal(out, newText) {
		t.Fatal("test needs a moved call")
	}
	st := retargetEquivalencePrediction(out, ep, window, structure, newOracleParts(ep, structures).image(nil))
	if st.Refs == 0 || st.Unknown != 0 || st.NoFit != 0 {
		t.Fatalf("retarget stats %+v", st)
	}
	if !bytes.Equal(out, newText) {
		t.Fatalf("retargeted text differs:\n%x\n%x", out, newText)
	}
	// The structural body wins where the equivalence copy is wrong.
	structural, _, err := predictDecoded(oldText, structure, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrongEq := bytes.Clone(out)
	wrongEq[2] ^= 0xff
	choices, n, nb := chooseStructuralFunctions(wrongEq, structural, newText, structure)
	if n != 1 || nb != 16 || choices[0] != 1 {
		t.Errorf("choice %v %d %d", choices, n, nb)
	}
	want := bytes.Clone(wrongEq)
	copy(want[:16], structural[:16])
	selected, err := applyStructuralChoices(wrongEq, oldText, structure, choices, newAddressLookup(structure).target)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Functions != n || selected.Bytes != nb || !bytes.Equal(wrongEq, want) {
		t.Fatalf("direct structural choices differ: stats %+v\n%x\n%x", selected, wrongEq, want)
	}
}

// elfImage builds a minimal ELF64 x86-64 image from named sections.
type elfSec struct {
	name  string
	addr  uint64
	data  []byte
	flags uint64
	typ   uint32
}

func elfImage(secs []elfSec) []byte {
	const ehsize, shentsize = 64, 64
	names := []byte{0}
	nameOff := make([]uint32, len(secs))
	for i, s := range secs {
		nameOff[i] = uint32(len(names))
		names = append(names, s.name...)
		names = append(names, 0)
	}
	shstr := uint32(len(names))
	names = append(names, ".shstrtab"...)
	names = append(names, 0)
	body := make([]byte, ehsize)
	offs := make([]uint64, len(secs))
	for i, s := range secs {
		for len(body)%16 != 0 {
			body = append(body, 0)
		}
		offs[i] = uint64(len(body))
		if s.typ != 8 { // SHT_NOBITS
			body = append(body, s.data...)
		}
	}
	shstrOff := uint64(len(body))
	body = append(body, names...)
	for len(body)%8 != 0 {
		body = append(body, 0)
	}
	shoff := uint64(len(body))
	sh := func(name uint32, typ uint32, flags, addr, off, size uint64) {
		var e [shentsize]byte
		binary.LittleEndian.PutUint32(e[0:], name)
		binary.LittleEndian.PutUint32(e[4:], typ)
		binary.LittleEndian.PutUint64(e[8:], flags)
		binary.LittleEndian.PutUint64(e[16:], addr)
		binary.LittleEndian.PutUint64(e[24:], off)
		binary.LittleEndian.PutUint64(e[32:], size)
		binary.LittleEndian.PutUint64(e[48:], 16)
		body = append(body, e[:]...)
	}
	sh(0, 0, 0, 0, 0, 0)
	for i, s := range secs {
		sh(nameOff[i], s.typ, s.flags, s.addr, offs[i], uint64(len(s.data)))
	}
	sh(shstr, 3, 0, 0, shstrOff, uint64(len(names)))
	copy(body, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(body[16:], 2)  // ET_EXEC
	binary.LittleEndian.PutUint16(body[18:], 62) // EM_X86_64
	binary.LittleEndian.PutUint32(body[20:], 1)  // version
	binary.LittleEndian.PutUint64(body[40:], shoff)
	binary.LittleEndian.PutUint16(body[52:], ehsize)
	binary.LittleEndian.PutUint16(body[58:], shentsize)
	binary.LittleEndian.PutUint16(body[60:], uint16(len(secs)+2))
	binary.LittleEndian.PutUint16(body[62:], uint16(len(secs)+1))
	return body
}

type funcList []symbols.Func

func (l funcList) Funcs(visit func(symbols.Func)) error {
	for _, f := range l {
		visit(f)
	}
	return nil
}

func TestSyntheticModule(t *testing.T) {
	t.Parallel()
	const textAddr = 0x1000
	rodata := bytes.Repeat([]byte("rodata!!"), 8)
	oldText := twoFuncs(textAddr, 32, bodyB)
	newText := twoFuncs(textAddr, 64, bodyB2)
	mk := func(text []byte) []byte {
		return elfImage([]elfSec{
			{name: ".text", addr: textAddr, data: text, flags: 6, typ: 1},
			{name: ".rodata", addr: 0x3000, data: rodata, flags: 2, typ: 1},
			{name: ".bss", addr: 0x4000, data: make([]byte, 64), flags: 3, typ: 8},
		})
	}
	old, target := mk(oldText), mk(newText)
	if _, err := loadImage([]byte("not an elf")); err == nil {
		t.Fatal("non-ELF accepted")
	}
	syms := func(bOff uint64, bSize uint64) symbols.Reader {
		return funcList{{Addr: textAddr, Size: 16, Name: "a"}, {Addr: textAddr + bOff, Size: bSize, Name: "b"}}
	}
	for _, tc := range []struct {
		name string
		mod  Module
	}{
		{"no symbols", Module{}},
		{"symbols", Module{Symbols: [2]symbols.Reader{syms(32, 6), syms(64, 9)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var st Stats
			m := tc.mod
			m.Stats = &st
			plan, pred, err := m.Analyse([][]byte{old}, target)
			if err != nil {
				t.Fatal(err)
			}
			if len(pred) != len(target) {
				t.Fatalf("prediction is %d bytes, target %d", len(pred), len(target))
			}
			again, err := Module{}.Materialise([][]byte{old}, plan, int64(len(target)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, pred) {
				t.Fatal("Materialise disagrees with Analyse's prediction")
			}
			if _, err := (Module{}).Materialise([][]byte{old}, plan, int64(len(target))+1); err == nil {
				t.Fatal("wrong length accepted")
			}
			if tc.mod.Symbols[0] != nil && st.Mappings != 2 {
				t.Errorf("mapped %d functions, want 2", st.Mappings)
			}
			// The whole target through the codec, with the module registered.
			reg := presage.NewRegistry()
			reg.Add(m)
			var cs presage.Stats
			patch, err := presage.Encode([][]byte{old}, target, presage.Options{Registry: reg, Modules: []byte{presage.ModuleLZ, ModuleELF}, Stats: &cs})
			if err != nil {
				t.Fatal(err)
			}
			if cs.Regions[0].Module != "elf" {
				t.Fatalf("region taken by %s", cs.Regions[0].Module)
			}
			var out bytes.Buffer
			if err := presage.Apply([][]byte{old}, patch, reg, &out); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), target) {
				t.Fatal("Apply did not reproduce the target")
			}
			t.Logf("%s: %d B patch, %d mispredicted, %d in .text; %v", tc.name, len(patch), st.PredictErr, st.TextPredictErr, st.Notes)
		})
	}
	if _, _, err := (Module{}).Analyse([][]byte{[]byte("plain")}, []byte("text")); err != presage.ErrDeclined {
		t.Fatalf("non-ELF: %v, want declined", err)
	}
}
