package elfmod

import (
	"bytes"
	"testing"

	"github.com/wjordan/presage/presage/symbols"
)

// TestWindowMigration: a function that moves between code windows (BOLT
// re-tiers hot and cold code on every profile) is still retargeted. Old
// image: A in .text calls C in .text.cold, C calls back to A. New image: C
// migrated into .text, both calls' displacements changed. The equivalence
// runs carry both bodies; resolving them needs the retarget pass to accept a
// source from another window and the oracle to accept a projection landing
// in another window.
func TestWindowMigration(t *testing.T) {
	t.Parallel()
	const textAddr, coldAddr = 0x1000, 0x5000
	funcC := func(pc, target uint64) []byte {
		c := []byte{0x55, 0x48, 0x89, 0xe5}
		c = append(c, callRel32(pc+4, target)...)
		c = append(c, 0xb8, 0x0d, 0xf0, 0x0d, 0x60) // mov eax, 0x600df00d
		c = append(c, 0x5d, 0xc3)
		return pad(c)
	}
	funcA := func(pc, target uint64) []byte {
		a := []byte{0x55, 0x48, 0x89, 0xe5}
		a = append(a, callRel32(pc+4, target)...)
		a = append(a, 0x5d, 0xc3)
		return pad(a)
	}
	oldText := funcA(textAddr, coldAddr)
	oldCold := funcC(coldAddr, textAddr)
	newText := append(funcA(textAddr, textAddr+16), funcC(textAddr+16, textAddr)...)
	newCold := bytes.Repeat([]byte{0xcc}, 16)
	mk := func(text, cold []byte) []byte {
		return elfImage([]elfSec{
			{name: ".text", addr: textAddr, data: text, flags: 6, typ: 1},
			{name: ".text.cold", addr: coldAddr, data: cold, flags: 6, typ: 1},
		})
	}
	old, target := mk(oldText, oldCold), mk(newText, newCold)
	oldSyms := funcList{{Addr: textAddr, Size: 16, Name: "a"}, {Addr: coldAddr, Size: 16, Name: "c"}}
	newSyms := funcList{{Addr: textAddr, Size: 16, Name: "a"}, {Addr: textAddr + 16, Size: 16, Name: "c"}}
	for _, tc := range []struct {
		name string
		mod  Module
	}{
		{"no symbols", Module{}},
		{"symbols", Module{Symbols: [2]symbols.Reader{oldSyms, newSyms}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var st Stats
			m := tc.mod
			m.Stats = &st
			plan, pred, err := m.Analyse([][]byte{old}, target)
			if err != nil {
				t.Fatal(err)
			}
			if st.Relocation.Unknown != 0 || st.Relocation.NoFit != 0 {
				t.Errorf("cross-window references unresolved: %+v", st.Relocation)
			}
			if st.TextPredictErr != 0 {
				t.Errorf("%d mispredicted bytes in .text", st.TextPredictErr)
			}
			again, err := Module{}.Materialise([][]byte{old}, plan, int64(len(target)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, pred) {
				t.Fatal("Materialise disagrees with Analyse's prediction")
			}
		})
	}
}
