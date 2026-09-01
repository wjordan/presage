package elfmod

import (
	"encoding/binary"
	"errors"
)

// .relr.dyn (SHT_RELR) is the packed form of the relative relocations a
// linker would otherwise spell out in .rela.dyn: an even word names an
// address, and every odd word after it is a 63-bit bitmap of the words
// following it. A relocated slot is therefore named by nothing in the bytes
// around it, and the equivalence copy leaves libxul with 1,109,576
// mispredicted bytes of .data.rel.ro -- pointers copied whole from the old
// image, each still pointing where its target used to be.
//
// The fix is the .rela.dyn addend fix in the other encoding, and needs no
// more of the plan: enumerate the OLD table's slots, project each slot's
// file offset forward through the equivalence map, and write the pointer
// oracle's answer for the value that used to be there. That takes the
// section to 45,929 wrong bytes.
//
// Regenerating the new packed table the same way was measured and lost: the
// bitmap words are dense, the projection gets a few of their bits wrong, and
// a wrong bit costs more than the whole table's byte prediction saves.

// relrPlan is the geometry of the old image's packed table. Everything else
// -- which slots it names, where they land, what they point at -- the
// decoder derives from the old image, the section maps the relocation plan
// carries, and the oracles.
type relrPlan struct{ OldOff, OldSize uint64 }

func (p relrPlan) marshal() []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, p.OldOff), p.OldSize)
}

func unmarshalRelrPlan(b []byte) (relrPlan, error) {
	r := &planReader{b: b}
	p := relrPlan{OldOff: r.u(), OldSize: r.u()}
	if r.err != nil || len(r.b) != 0 {
		return relrPlan{}, errors.New("trailing or invalid relr plan data")
	}
	return p, nil
}

// eachRelrSlot calls fn with the address of every word a packed RELR table
// relocates. It calls rather than returning a list because the list is up to
// 63 times the size of the table, and the table's size comes from the plan.
func eachRelrSlot(table []byte, fn func(addr uint64)) {
	var next uint64
	for p := 0; p+8 <= len(table); p += 8 {
		e := binary.LittleEndian.Uint64(table[p:])
		if e&1 == 0 {
			fn(e)
			next = e + 8
			continue
		}
		base := next
		// Bit 0 is the tag; bit i marks the word i-1 slots past base.
		for i := 1; i < 64; i++ {
			if e&(1<<uint(i)) != 0 {
				fn(base + uint64(i-1)*8)
			}
		}
		next = base + 63*8
	}
}

type relrStats struct {
	Slots      int
	Retargeted int
	Unknown    int
	Unplaced   int
}

// applyRelr rewrites the pointer in every slot the old table names, at
// wherever the equivalence map put that slot in the new image.
func applyRelr(out, old []byte, p relrPlan, oldSecs sectionMap, mapper sourceEquivalenceMapper, pointer PointerOracle) relrStats {
	var st relrStats
	eachRelrSlot(old[p.OldOff:p.OldOff+p.OldSize], func(addr uint64) {
		st.Slots++
		oldOff, ok := oldSecs.offsetOf(addr)
		if !ok || oldOff+8 > uint64(len(old)) {
			st.Unplaced++
			return
		}
		newOff, ok := mapper.project(oldOff)
		if !ok || newOff+8 > uint64(len(out)) {
			st.Unplaced++
			return
		}
		t := pointer(binary.LittleEndian.Uint64(old[oldOff:]))
		if !t.Known {
			st.Unknown++
			return
		}
		binary.LittleEndian.PutUint64(out[newOff:], t.Addr)
		st.Retargeted++
	})
	return st
}
