package delta

import (
	"encoding/binary"
	"errors"
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

// The pctab replay (docs/DESIGN.md 3.2.2). The fixture is a pctab of nine
// self-delimiting tables and four functions: one matched, one the release
// added, one matched but reshaped to two more pcdata slots, one matched. The
// invented slots between them are a fresh allocation, a backward dedup and a
// zero-length table, which is every case the mask has to carry.
// rplPad is the fixture's pctab length.
const rplPad = 32

// pcTab builds a pctab whose tables are at the offsets it returns: one pad
// byte, then a (value, pc) pair per table and a terminating zero.
func pcTab(pairs ...int) ([]byte, []uint32) {
	tab := []byte{0}
	var off []uint32
	for _, n := range pairs {
		off = append(off, uint32(len(tab)))
		for k := range n {
			tab = append(tab, byte(1+2*k), byte(2+2*k))
		}
		tab = append(tab, 0)
	}
	return tab, off
}

// rplRec is one _func record with npc pcdata slots and one funcdata slot.
func rplRec(npc uint32, pc []uint32) []byte {
	r := make([]byte, gobin.FuncSize+4*npc+4)
	binary.LittleEndian.PutUint32(r[28:], npc)
	r[43] = 1
	for i, v := range pc {
		switch i {
		case 0, 1, 2: // pcsp, pcfile, pcln
			binary.LittleEndian.PutUint32(r[16+4*i:], v)
		default:
			binary.LittleEndian.PutUint32(r[gobin.FuncSize+4*(i-3):], v)
		}
	}
	return r
}

// rplFixture returns the old and new binaries, the match and the layout the
// replay runs on, plus the pctab table offsets.
func rplFixture(t *testing.T) (*gobin.Bin, *gobin.Bin, *match, *layout, []uint32) {
	t.Helper()
	tab, off := pcTab(1, 1, 2, 1, 1, 1, 2, 1, 1)
	if len(tab) != rplPad {
		t.Fatalf("fixture pctab is %d bytes, want %d", len(tab), rplPad)
	}
	// old: three identical two-pcdata records, so the modal shape is (2,1)
	var oft []byte
	var ofn []*gobin.Func
	for i := range 3 {
		ofn = append(ofn, &gobin.Func{Idx: i, FuncOff: uint32(len(oft))})
		oft = append(oft, rplRec(2, nil)...)
	}
	old := &gobin.Bin{Funcs: ofn, Pcln: &gobin.Pcln{Data: oft, Functab: gobin.Range{Len: uint64(len(oft))}}}

	// new: the true records, laid after the pctab in one Data buffer
	recs := [][]uint32{
		{off[0], off[1], off[2], 0, off[0]},            // matched, nothing invented
		{off[3], off[1], off[4], 0, off[5]},            // added: fresh, dedup, fresh, zero, fresh
		{off[6], off[0], off[7], off[2], 0, off[8], 0}, // reshaped: the k=3 slot is fresh, k=2 a dedup
		{off[0], off[0], off[0], 0, 0},
	}
	nfn := make([]*gobin.Func, len(recs))
	nft := []byte{}
	for j, pc := range recs {
		npc := uint32(len(pc) - 3)
		nfn[j] = &gobin.Func{Idx: j, FuncOff: uint32(len(nft))}
		nft = append(nft, rplRec(npc, pc)...)
	}
	new := &gobin.Bin{Funcs: nfn, Pcln: &gobin.Pcln{
		Data:    append(append([]byte(nil), tab...), nft...),
		Pctab:   gobin.Range{Len: rplPad},
		Functab: gobin.Range{Off: rplPad, Len: uint64(len(nft))},
	}}
	m := &match{NewToOld: []int{0, -1, 1, 2}, OldToNew: []int{0, 2, 3}}
	l := &layout{RecShapes: []recShape{{Idx: 2, Npcdata: 4, Nfuncdata: 1}}}
	return old, new, m, l, off
}

func TestPcReplayRoundTrip(t *testing.T) {
	old, new, m, l, off := rplFixture(t)
	l.NPcFresh, l.PcFresh, l.PcGaps = buildPcFresh(old, new, m, l)

	if slots, gaps := pcReplayShape(old, l, m); slots != l.NPcFresh || gaps != len(l.PcGaps) {
		t.Fatalf("pcReplayShape says %d slots and %d gaps, the replay produced %d and %d",
			slots, gaps, l.NPcFresh, len(l.PcGaps))
	}
	if l.NPcFresh != 7 || len(l.PcGaps) != 2 {
		t.Fatalf("%d invented slots and %d gaps, want 7 and 2", l.NPcFresh, len(l.PcGaps))
	}
	// the added function's pcsp, pcln and pcdata[1]; then, in the reshaped
	// record, pcdata[3] is empty and pcdata[2] -- which the linker allocates
	// last of all -- is the fresh one
	if l.PcFresh[0] != 0b1010101 {
		t.Errorf("mask %#b, want %#b", l.PcFresh[0], 0b1010101)
	}

	// the decoder: the invented slots start at a sentinel the template would
	// have put there, and only the fresh ones may move
	const sentinel = 0x5eed
	cur := &pcCursor{tab: new.Pcln.Table(new.Pcln.Pctab), off: 1,
		mask: l.PcFresh, nbit: l.NPcFresh, gaps: l.PcGaps}
	want := [][]uint32{
		{off[0], off[1], off[2], 0, off[0]},
		{off[3], sentinel, off[4], sentinel, off[5]},
		{off[6], off[0], off[7], off[2], 0, off[8], sentinel},
		{off[0], off[0], off[0], 0, 0},
	}
	for j := range new.Funcs {
		npc, _, _ := new.Pcln.Record(new.Funcs[j].FuncOff)
		keep := -1
		if i := m.NewToOld[j]; i >= 0 {
			onpc, _, _ := old.Pcln.Record(old.Funcs[i].FuncOff)
			keep = int(onpc)
		}
		rec := rplRec(npc, want[j])
		for _, o := range []int{16, 20, 24} { // only invented slots start blank
			if keep < 0 {
				binary.LittleEndian.PutUint32(rec[o:], sentinel)
			}
		}
		for k := range npc {
			if keep < 0 || int(k) >= keep {
				binary.LittleEndian.PutUint32(rec[gobin.FuncSize+4*k:], sentinel)
			}
		}
		cur.fill(rec, keep)
		for i, w := range want[j] {
			o := 16 + 4*i
			if i > 2 {
				o = gobin.FuncSize + 4*(i-3)
			}
			if got := binary.LittleEndian.Uint32(rec[o:]); got != w {
				t.Errorf("function %d slot at %d is %#x, want %#x", j, o, got, w)
			}
		}
	}
	if cur.i != l.NPcFresh || cur.g != len(l.PcGaps) {
		t.Errorf("the decoder read %d bits and %d gaps, the encoder wrote %d and %d",
			cur.i, cur.g, l.NPcFresh, len(l.PcGaps))
	}
}

func TestPcReplayWireRoundTrip(t *testing.T) {
	l := &layout{NPcFresh: 11, PcFresh: []byte{0b10110101, 0b101}, PcGaps: []uint32{0, 3, 1 << 20}}
	w := &wbuf{}
	encodePcReplay(w, l)
	got := &layout{}
	r := &rbuf{b: w.b}
	if err := decodePcReplay(r, got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := r.done(); err != nil {
		t.Fatalf("done: %v", err)
	}
	if got.NPcFresh != l.NPcFresh || string(got.PcFresh) != string(l.PcFresh) {
		t.Errorf("mask %d/%v, want %d/%v", got.NPcFresh, got.PcFresh, l.NPcFresh, l.PcFresh)
	}
	if len(got.PcGaps) != len(l.PcGaps) {
		t.Fatalf("%d gaps, want %d", len(got.PcGaps), len(l.PcGaps))
	}
	for i, g := range got.PcGaps {
		if g != l.PcGaps[i] {
			t.Errorf("gap %d is %d, want %d", i, g, l.PcGaps[i])
		}
	}
}

// rplStream writes the replay fields straight, so a test can put on the wire
// what the encoder would never produce.
func rplStream(nbits uint64, mask []byte, gaps []uint64) []byte {
	w := &wbuf{}
	w.u(nbits)
	w.bytes(mask)
	w.u(uint64(len(gaps)))
	for _, g := range gaps {
		w.u(g)
	}
	return w.b
}

func TestPcReplayCorrupt(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"mask shorter than the bits it claims", rplStream(64, []byte{1}, []uint64{0})},
		{"mask longer than the bits it claims", rplStream(1, []byte{1, 2}, []uint64{0})},
		{"bit count past the stream", rplStream(1<<20, []byte{1}, nil)},
		{"gap count past the stream", rplStream(8, []byte{1}, nil)[:3]},
		{"truncated gaps", rplStream(8, []byte{1}, []uint64{1, 2, 3})[:5]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &rbuf{b: tc.b}
			l := &layout{}
			err := decodePcReplay(r, l)
			if err == nil {
				err = r.done()
			}
			if err == nil {
				t.Fatalf("decoded %d bits and %d gaps, want a corrupt-patch error", l.NPcFresh, len(l.PcGaps))
			}
			if !errors.Is(err, errCorrupt) {
				t.Fatalf("error %v, want a corrupt-patch error", err)
			}
		})
	}
}

// A gap large enough to walk off the end of pctab must stop there rather
// than spin, and the slot it lands on is wrong but in range.
func TestPcCursorStopsAtTheEndOfPctab(t *testing.T) {
	tab, _ := pcTab(1, 1, 1)
	cur := &pcCursor{tab: tab, off: 1, mask: []byte{0xff}, nbit: 8, gaps: []uint32{1 << 30}}
	rec := rplRec(2, []uint32{0, 0, 0, 0, 0})
	cur.fill(rec, -1)
	if int(cur.off) != len(tab) {
		t.Fatalf("the cursor stopped at %d, want the end of a %d-byte pctab", cur.off, len(tab))
	}
}

func TestPcReplayCheck(t *testing.T) {
	old, new, m, l, _ := rplFixture(t)
	l.NPcFresh, l.PcFresh, l.PcGaps = buildPcFresh(old, new, m, l)
	if err := checkPcReplay(old, l, m); err != nil {
		t.Fatalf("the encoder's own replay was rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		bend func(*layout)
	}{
		{"a bit too many", func(l *layout) { l.NPcFresh++ }},
		{"a bit too few", func(l *layout) { l.NPcFresh-- }},
		{"a gap too many", func(l *layout) { l.PcGaps = append(l.PcGaps, 0) }},
		{"a gap too few", func(l *layout) { l.PcGaps = l.PcGaps[:len(l.PcGaps)-1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := *l
			c.PcGaps = append([]uint32(nil), l.PcGaps...)
			tc.bend(&c)
			if err := checkPcReplay(old, &c, m); !errors.Is(err, errCorrupt) {
				t.Fatalf("error %v, want a corrupt-patch error", err)
			}
		})
	}
	// the field is absent for a pair that invented nothing
	if err := checkPcReplay(old, &layout{}, m); err != nil {
		t.Errorf("an absent replay was rejected: %v", err)
	}
}
