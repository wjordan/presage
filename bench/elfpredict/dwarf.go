package main

import (
	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/presage/dwarf"
	"github.com/wjordan/presage/presage/eqmatch"
)

// The DWARF field layer is presage/dwarf; this is the harness's adapter to
// it: the section geometry comes from the two images, and the equivalence
// runs the layer projects through are the harness's equivalences clipped to
// one section.

type dwarfPlan = dwarf.Plan
type dwarfStats = dwarf.Stats

const (
	dwInfo        = dwarf.Info
	dwAbbrev      = dwarf.Abbrev
	dwLine        = dwarf.Line
	dwLoclists    = dwarf.Loclists
	dwRnglists    = dwarf.Rnglists
	dwAddr        = dwarf.Addr
	dwFrame       = dwarf.Frame
	dwSymtab      = dwarf.Symtab
	dwStrtab      = dwarf.Strtab
	dwarfSecCount = dwarf.NSec
)

var dwarfSecNames = dwarf.Names

// dwarfSecs is the geometry of the layer's sections in the two images.
func dwarfSecs(oldImage, newImage *image) [dwarfSecCount]dwarf.Sec {
	var secs [dwarfSecCount]dwarf.Sec
	for i, name := range dwarfSecNames {
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

func buildDwarfPlan(oldImage, newImage *image, ep equivalencePlan, withRecords func(k int) bool, addrMap func(uint64) (uint64, bool)) (dwarfPlan, bool) {
	secs := dwarfSecs(oldImage, newImage)
	return dwarf.Build(oldImage.Data, newImage.Data, secs, withRecords, addrMap, func(k int) []eqmatch.Run {
		return sectionRuns(ep, secs[k])
	})
}

func unmarshalDwarfPlan(b, old []byte) (dwarfPlan, error) { return dwarf.Unmarshal(b, old) }

// applyDwarf hands the layer the equivalences of each section it does not
// place itself. The whole-image equivalence pass has already written those
// bytes; the layer writes the same ones again.
func applyDwarf(out, old []byte, p dwarfPlan, ep equivalencePlan, ptr func(uint64) x86.Target, sizeDelta func(uint64) (int64, bool)) (dwarfStats, error) {
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

// textEquivalences counts the equivalences that write into .text.
func textEquivalences(ep equivalencePlan) int {
	n := 0
	for _, e := range ep.Eqs {
		if e.Dst < ep.NewText.Off+ep.NewText.Size && e.Dst+e.N > ep.NewText.Off {
			n++
		}
	}
	return n
}
