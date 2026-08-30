package delta

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/wjordan/presage/delta/gobin"
	"github.com/wjordan/presage/delta/x86"
)

func le32b(v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return b[:]
}

// asmBody is the resized function the round-trip is measured on: a forward
// branch over the edit, a rip-relative load, filler, the inserted block,
// more filler, and a back-edge to the top. Every displacement is computed
// from entry, so the same source laid at another address is a correctly
// relocated copy of itself.
func asmBody(entry uint64, insert int, data uint64) []byte {
	const (
		l0    = 12 // the back-edge's target
		mov   = 7  // mov rax, imm32
		preN  = 16 // filler instructions before the edit
		postN = 16 // and after it
	)
	movImm := func(v uint32) []byte { return append([]byte{0x48, 0xC7, 0xC0}, le32b(v)...) }
	lfwd := l0 + (preN+postN)*mov + insert
	var b []byte
	put := func(x ...byte) { b = append(b, x...) }
	put(0xE9)                                         // jmp rel32 -> lfwd
	put(le32b(uint32(int32(lfwd - 5)))...)            //
	put(0x48, 0x8B, 0x05)                             // mov rax, [rip+disp32] -> data
	put(le32b(uint32(int32(data - (entry + l0))))...) //
	for i := range preN {                             //
		put(movImm(uint32(i))...)
	}
	for i := 0; i < insert; i += mov {
		put(movImm(uint32(900 + i))...)
	}
	for i := range postN {
		put(movImm(uint32(100 + i))...)
	}
	put(0x48, 0x31, 0xC0)                         // xor rax, rax
	put(0x0F, 0x85)                               // jne rel32 -> l0
	put(le32b(uint32(int32(l0 - (lfwd + 9))))...) //
	put(0xC3)                                     // ret
	return b
}

func diffCount(a, b []byte) int {
	n := 0
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			n++
		}
	}
	return n + max(len(a), len(b)) - min(len(a), len(b))
}

// TestSegmapResizedRoundTrip runs the whole scheme on one edited function:
// the aligner, the decoder's piece-by-piece assembly, and the map lookup the
// two branches go through. Everything the map covers must come out exactly
// as the real new body, and the positional copy the codec used to make must
// come out much worse.
func TestSegmapResizedRoundTrip(t *testing.T) {
	const (
		oldEntry, newEntry = 0x400000, 0x401000
		oldData, newData   = 0x500000, 0x503000
		insert             = 28
		head               = 12 + 16*7 // up to the edit
	)
	oldBody := asmBody(oldEntry, 0, oldData)
	newBody := asmBody(newEntry, insert, newData)
	if len(newBody) != len(oldBody)+insert {
		t.Fatalf("bodies are %d and %d bytes", len(oldBody), len(newBody))
	}
	segs := alignFunc(oldBody, newBody)
	if len(segs) != 1 {
		t.Fatalf("aligner found %v, want one piece for the shifted tail", segs)
	}
	if s := segs[0]; s.New-s.Old != insert || s.New < head || int(s.New+s.N) != len(newBody) {
		t.Fatalf("piece %+v does not cover the tail of a %d-byte body", s, len(newBody))
	}

	// what the mapper does for a self-reference into a mapped function, plus
	// a constant shift for the one target outside it
	lookup := func(target uint64) x86.Target {
		if target >= oldEntry && target < oldEntry+uint64(len(oldBody)) {
			o := mapSegOff(segs, target-oldEntry, uint64(len(newBody)))
			return x86.Target{Addr: newEntry + o, Known: true}
		}
		return x86.Target{Addr: target + newData - oldData, Known: true}
	}
	var st x86.Stats
	out := make([]byte, len(newBody))
	relocatePieces(oldBody, oldBody, out, 0, oldEntry, newEntry, segs, lookup, &st)

	p := int(segs[0].New)
	if string(out[:head]) != string(newBody[:head]) {
		t.Errorf("the head is wrong: the forward branch or the rip-relative load did not follow the edit")
	}
	if string(out[p:]) != string(newBody[p:]) {
		t.Errorf("the mapped tail is wrong: the back-edge or the shifted body did not relocate")
	}
	// only the bytes no piece covers -- the inserted code and the slack the
	// piece was snapped back over -- may differ
	if n := diffCount(out, newBody); n > p-head {
		t.Errorf("%d bytes wrong, at most %d expected", n, p-head)
	}

	flat := make([]byte, len(newBody))
	x86.Relocate(oldBody, flat, oldEntry, newEntry, lookup, &st, nil)
	if n, was := diffCount(out, newBody), diffCount(flat, newBody); was < 4*n {
		t.Errorf("the segment map leaves %d bytes wrong and the positional copy %d; "+
			"the test is not measuring the map", n, was)
	}
}

func TestSegmapWireRoundTrip(t *testing.T) {
	maps := []segMap{
		{Idx: 0, Segs: []segPiece{{0, 0, 20}, {40, 30, 15}}},
		{Idx: 7, Segs: []segPiece{{100, 0, 64}}},
		{Idx: 8, Segs: []segPiece{{5, 5, 12}, {200, 100, 30}, {400, 500, 9}}},
	}
	w := &wbuf{}
	encodeSegMaps(w, maps)
	r := &rbuf{b: w.b}
	got := decodeSegMaps(r, 9, false)
	if err := r.done(); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(maps) {
		t.Fatalf("decoded %d maps, want %d", len(got), len(maps))
	}
	for i, sm := range got {
		if sm.Idx != maps[i].Idx || len(sm.Segs) != len(maps[i].Segs) {
			t.Fatalf("map %d is %+v, want %+v", i, sm, maps[i])
		}
		for k, s := range sm.Segs {
			if s != maps[i].Segs[k] {
				t.Errorf("map %d piece %d is %+v, want %+v", i, k, s, maps[i].Segs[k])
			}
		}
	}
	if n := decodeSegMaps(&rbuf{b: (&wbuf{}).b}, 9, false); n != nil {
		t.Errorf("an empty stream decoded to %v", n)
	}
}

// segStream writes the five columns straight, so a test can put a list on
// the wire the encoder would never produce.
func segStream(idxGap, nseg []uint64, gap, lens []uint64, shift []int64) []byte {
	w := &wbuf{}
	w.u(uint64(len(idxGap)))
	for _, v := range idxGap {
		w.u(v)
	}
	for _, v := range nseg {
		w.u(v)
	}
	for _, v := range gap {
		w.u(v)
	}
	for _, v := range lens {
		w.u(v)
	}
	for _, v := range shift {
		w.s(v)
	}
	return w.b
}

func TestSegmapCorrupt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		b     []byte
		nfunc int
	}{
		{"index past the function list",
			segStream([]uint64{9}, []uint64{1}, []uint64{0}, []uint64{16}, []int64{4}), 9},
		{"index repeated",
			segStream([]uint64{1, 0}, []uint64{1, 1}, []uint64{0, 0}, []uint64{16, 16}, []int64{4, 4}), 9},
		{"no pieces",
			segStream([]uint64{1}, []uint64{0}, nil, nil, nil), 9},
		{"zero-length piece",
			segStream([]uint64{1}, []uint64{1}, []uint64{0}, []uint64{0}, []int64{4}), 9},
		{"pieces overlapping in the old body",
			segStream([]uint64{1}, []uint64{2}, []uint64{0, 0}, []uint64{16, 16}, []int64{0, -8}), 9},
		{"truncated",
			segStream([]uint64{1}, []uint64{2}, []uint64{0, 0}, []uint64{16}, nil), 9},
		{"count past the stream",
			segStream(make([]uint64, 64), nil, nil, nil, nil)[:1], 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &rbuf{b: tc.b}
			maps := decodeSegMaps(r, tc.nfunc, false)
			if err := r.done(); err == nil {
				t.Fatalf("decoded %v, want a corrupt-patch error", maps)
			} else if !errors.Is(err, errCorrupt) {
				t.Fatalf("error %v, want a corrupt-patch error", err)
			}
		})
	}
}

// textBin is a one-function binary, enough for the mapper's text path.
func textBin(addr, entry, size uint64) *gobin.Bin {
	sec := &gobin.Section{Name: ".text", Addr: addr, Size: 0x10000}
	return &gobin.Bin{
		Sects: map[string]*gobin.Section{".text": sec}, Order: []*gobin.Section{sec}, Text: sec,
		Funcs: []*gobin.Func{{Idx: 0, Entry: entry, End: entry + size}},
	}
}

func TestSegmapCheck(t *testing.T) {
	old := textBin(0x400000, 0x400000, 0x80)
	dst := textBin(0x800000, 0x800000, 0x100)
	m := &match{NewToOld: []int{0}, OldToNew: []int{0}}
	for _, tc := range []struct {
		name string
		maps []segMap
		m    *match
	}{
		{"past the new body", []segMap{{0, []segPiece{{0, 0x40, 0x100}}}}, m},
		{"past the old body", []segMap{{0, []segPiece{{0x40, 0, 0x60}}}}, m},
		{"function does not exist", []segMap{{3, []segPiece{{0, 0, 16}}}}, m},
		{"function is unmatched", []segMap{{0, []segPiece{{0, 0, 16}}}}, &match{NewToOld: []int{-1}, OldToNew: []int{-1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkSegMaps(tc.maps, old, dst.Funcs, tc.m, false); !errors.Is(err, errCorrupt) {
				t.Fatalf("error %v, want a corrupt-patch error", err)
			}
		})
	}
	if err := checkSegMaps([]segMap{{0, []segPiece{{0x40, 0x40, 0x40}}}}, old, dst.Funcs, m, false); err != nil {
		t.Fatalf("a piece that fits both bodies was rejected: %v", err)
	}
}

// TestMapperSegLookup walks the three-step lookup of docs/go-module-design.md 2.2.1
// through the mapper itself: a transmitted piece, the implicit shift-0
// piece, and the fallback for an old byte no piece places.
func TestMapperSegLookup(t *testing.T) {
	const oldAddr, newAddr = 0x400000, 0x800000
	newMap := func(segs []segPiece, oldSize, newSize uint64) *mapper {
		src, dst := textBin(oldAddr, oldAddr, oldSize), textBin(newAddr, newAddr, newSize)
		return &mapper{src: src, dst: dst, srcToDst: []int{0}, dstToSrc: []int{0},
			segs: map[int][]segPiece{0: segs}, segLocal: map[int][]segPiece{0: segs}}
	}
	grew := newMap([]segPiece{{Old: 100, New: 150, N: 50}}, 300, 350)
	shrank := newMap([]segPiece{{Old: 200, New: 50, N: 50}}, 400, 100)
	for _, tc := range []struct {
		name string
		mp   *mapper
		o    uint64
		want uint64
	}{
		{"1: inside a transmitted piece", grew, 120, 170},
		{"1: the piece's first byte", grew, 100, 150},
		{"2: the implicit piece before it", grew, 50, 50},
		{"2: the implicit piece after it", grew, 210, 210},
		{"3: displaced, the piece's shift applies", grew, 160, 210},
		{"3: clamped into the new body", shrank, 390, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, cls := tc.mp.mapAddrBase(oldAddr+tc.o, nil)
			if cls != rcTextMatched {
				t.Fatalf("class %d, want a matched-function reference", cls)
			}
			if a != newAddr+tc.want {
				t.Errorf("old +%d maps to new +%d, want +%d", tc.o, a-newAddr, tc.want)
			}
		})
	}
}
