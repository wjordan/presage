package elfmod

import (
	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/presage/dwarf"
	"github.com/wjordan/presage/presage/eqmatch"
)

// The DWARF field layer is presage/dwarf; this file only adapts the ELF
// module's image geometry, equivalences and address map to it.

// dwarfSecs is the geometry of the layer's sections in the two images.
func dwarfSecs(oldImage, newImage *image) [dwarf.NSec]dwarf.Sec {
	var secs [dwarf.NSec]dwarf.Sec
	for i, name := range dwarf.Names {
		o, okOld := oldImage.Debug[name]
		n, okNew := newImage.Debug[name]
		if okOld && okNew {
			secs[i] = dwarf.Sec{OldOff: o.Off, OldSize: o.Size, NewOff: n.Off, NewSize: n.Size}
		}
	}
	return secs
}

// sectionRuns clips the equivalences to one section: the part of each whose
// source lies in the old section and whose destination lies in the new one,
// in section-relative offsets.
func sectionRuns(ep equivalencePlan, s dwarf.Sec) []eqmatch.Run {
	if s.OldSize == 0 || s.NewSize == 0 {
		return nil
	}
	var runs []eqmatch.Run
	for _, e := range ep.Eqs {
		lo := max(int64(0), int64(s.OldOff)-int64(e.Src), int64(s.NewOff)-int64(e.Dst))
		hi := min(int64(e.N), int64(s.OldOff+s.OldSize)-int64(e.Src), int64(s.NewOff+s.NewSize)-int64(e.Dst))
		if lo < hi {
			runs = append(runs, eqmatch.Run{Src: e.Src + uint64(lo) - s.OldOff, Dst: e.Dst + uint64(lo) - s.NewOff, N: uint64(hi - lo)})
		}
	}
	return runs
}

func buildDwarfPlan(oldImage, newImage *image, ep equivalencePlan, withRecords func(k int) bool, addrMap func(uint64) (uint64, bool)) (dwarf.Plan, bool) {
	secs := dwarfSecs(oldImage, newImage)
	return dwarf.Build(oldImage.Data, newImage.Data, secs, withRecords, addrMap, func(k int) []eqmatch.Run {
		return sectionRuns(ep, secs[k])
	})
}

func unmarshalDwarfPlan(b, old []byte) (dwarf.Plan, error) { return dwarf.Unmarshal(b, old) }

// applyDwarf hands the layer the equivalences of each section it does not
// place itself. The whole-image equivalence pass has already written those
// bytes; the layer writes the same ones again.
func applyDwarf(out, old []byte, p dwarf.Plan, ep equivalencePlan, ptr func(uint64) x86.Target, sizeDelta func(uint64) (int64, bool)) (dwarf.Stats, error) {
	for k := range p.Runs {
		p.Runs[k] = sectionRuns(ep, p.Secs[k])
	}
	return dwarf.Apply(out, old, p, func(a uint64) (uint64, bool) {
		t := ptr(a)
		return t.Addr, t.Known
	}, sizeDelta)
}

// eqCovers reports whether any equivalence writes into [off, off+size).
func eqCovers(ep equivalencePlan, off, size uint64) bool {
	for _, e := range ep.Eqs {
		if e.Dst < off+size && e.Dst+e.N > off {
			return true
		}
	}
	return false
}

// textEquivalences counts the equivalences that write into a code window.
func textEquivalences(ep equivalencePlan) int {
	n := 0
	for _, e := range ep.Eqs {
		for _, w := range ep.Windows {
			if e.Dst < w.New.Off+w.New.Size && e.Dst+e.N > w.New.Off {
				n++
				break
			}
		}
	}
	return n
}
