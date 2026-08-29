package delta

import (
	"errors"
	"testing"

	"github.com/wjordan/go-binsync/delta/gobin"
	"github.com/wjordan/go-binsync/delta/x86"
)

// TestSegfarRoundTrip edits a function by inserting a block that another
// old function already contains, and checks the far pass finds it there,
// the decoder lays it down exactly, and the wire format carries it only
// from transform 3.
func TestSegfarRoundTrip(t *testing.T) {
	const (
		textAddr, newText = 0x400000, 0x800000
		oldData, newData  = 0x500000, 0x503000
		insert            = 70
		pad               = 64
	)
	donor := asmBody(textAddr, insert, oldData) // holds the block at its own PC
	oldB := asmBody(textAddr+uint64(len(donor)+pad), 0, oldData)
	newB := asmBody(newText, insert, newData)
	text := append(append(append([]byte(nil), donor...), make([]byte, pad)...), oldB...)
	for i := len(donor); i < len(donor)+pad; i++ {
		text[i] = 0xCC
	}
	mk := func(addr uint64, data []byte, sizes ...uint64) *gobin.Bin {
		sec := &gobin.Section{Name: ".text", Addr: addr, Size: uint64(len(data)), Data: data}
		b := &gobin.Bin{Sects: map[string]*gobin.Section{".text": sec}, Order: []*gobin.Section{sec}, Text: sec}
		at := addr
		for i, n := range sizes {
			b.Funcs = append(b.Funcs, &gobin.Func{Idx: i, Entry: at, End: at + n})
			at += n
		}
		return b
	}
	old := mk(textAddr, text, uint64(len(donor)+pad), uint64(len(oldB)))
	new := mk(newText, newB, uint64(len(newB)))
	f, g := old.Funcs[1], new.Funcs[0]
	m := &match{NewToOld: []int{1}, OldToNew: []int{-1, 0}}

	local := alignFunc(oldB, newB)
	lookup := func(target uint64) x86.Target {
		if target >= f.Entry && target < f.End {
			return x86.Target{Addr: g.Entry + mapSegOff(local, target-f.Entry, uint64(len(newB))), Known: true}
		}
		if target >= textAddr && target < textAddr+uint64(len(text)) {
			return x86.Target{} // the donor has no home in the new binary
		}
		return x86.Target{Addr: target + newData - oldData, Known: true}
	}
	ix := indexText(text)
	segs := ix.farAlign(old, new, f, g, local, lookup)
	var far []segPiece
	for _, s := range segs {
		if isFar(s, int64(f.Size())) {
			far = append(far, s)
		}
	}
	if len(far) != 1 || far[0].N < insert {
		t.Fatalf("far pass produced %v, want one piece of at least %d bytes from the donor", far, insert)
	}
	if err := checkSegMaps([]segMap{{0, segs}}, old, new.Funcs, m, true); err != nil {
		t.Fatalf("a far piece inside .text was rejected: %v", err)
	}
	if err := checkSegMaps([]segMap{{0, segs}}, old, new.Funcs, m, false); !errors.Is(err, errCorrupt) {
		t.Fatalf("transform 2 accepted a far piece: %v", err)
	}

	var st x86.Stats
	out := make([]byte, len(newB))
	relocatePieces(text, oldB, out, f.Entry-textAddr, f.Entry, g.Entry, segs, lookup, &st)
	if n := diffCount(out, newB); n != 0 {
		t.Errorf("%d bytes wrong after laying the far piece down", n)
	}

	w := &wbuf{}
	encodeSegMaps(w, []segMap{{0, segs}})
	got := decodeSegMaps(&rbuf{b: w.b}, 1, true)
	if len(got) != 1 || len(got[0].Segs) != len(segs) {
		t.Fatalf("decoded %v, want %v", got, segs)
	}
	for k, s := range got[0].Segs {
		if s != segs[k] {
			t.Errorf("piece %d decoded as %+v, want %+v", k, s, segs[k])
		}
	}
}
