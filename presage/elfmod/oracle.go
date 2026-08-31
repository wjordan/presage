package elfmod

import "github.com/wjordan/presage/delta/x86"

// oracleParts holds the two derived structures every oracle is built
// from, so a caller needing several oracles sorts the map and de-overlaps
// the runs once. Both parts are immutable after construction.
type oracleParts struct {
	ep equivalencePlan
	lk *codeLookup
	sm sourceEquivalenceMapper
}

func newOracleParts(ep equivalencePlan, structures []predictionPlan) oracleParts {
	return oracleParts{ep: ep, lk: newCodeLookup(structures), sm: newSourceEquivalenceMapper(ep)}
}

// image answers "where did this old address go" for an instruction
// displacement: inside .text the byte-level projection through the runs
// is authoritative; outside it an exact point wins, then the projection
// through the section geometry rp carries, then the structural lookup.
func (o oracleParts) image(rp *relocPlan) ImageOracle {
	ep, lk, sm := o.ep, o.lk, o.sm
	return func(addr uint64) x86.Target {
		// Inside a window the byte-level projection through the runs is
		// authoritative, and the window the bytes *land* in names the new
		// address: a run carrying a function into another window is a real
		// move (BOLT re-tiers hot and cold code on every profile). Only a
		// projection landing outside every window says nothing.
		for _, w := range ep.Windows {
			if addr < w.Old.Addr || addr >= w.Old.Addr+w.Old.Size {
				continue
			}
			oldFile := w.Old.Off + addr - w.Old.Addr
			if newFile, ok := sm.project(oldFile); ok {
				for _, x := range ep.Windows {
					if newFile >= x.New.Off && newFile < x.New.Off+x.New.Size {
						return x86.Target{Addr: x.New.Addr + newFile - x.New.Off, Known: true}
					}
				}
			}
			return lk.target(addr)
		}
		if rp != nil {
			if t := lk.pointTarget(addr); t.Known {
				return t
			}
			if off, ok := rp.OldSecs.offsetOf(addr); ok {
				if newOff, ok := sm.project(off); ok {
					if newAddr, ok := rp.NewSecs.addrOf(newOff); ok {
						return x86.Target{Addr: newAddr, Known: true}
					}
				}
			}
		}
		return lk.target(addr)
	}
}

// pointer resolves an absolute pointer, which names a function rather than
// a place: identity evidence (points, the map) wins over the byte
// projection, or identical-code folding sends a pointer to the wrong twin.
func (o oracleParts) pointer(rp *relocPlan) PointerOracle {
	lk, sm := o.lk, o.sm
	return func(addr uint64) x86.Target {
		if t := lk.pointTarget(addr); t.Known {
			return t
		}
		if t := lk.mapTarget(addr); t.Known {
			return t
		}
		if rp != nil {
			if off, ok := rp.OldSecs.offsetOf(addr); ok {
				if newOff, ok := sm.project(off); ok {
					if newAddr, ok := rp.NewSecs.addrOf(newOff); ok {
						return x86.Target{Addr: newAddr, Known: true}
					}
				}
			}
		}
		return lk.target(addr)
	}
}

// funcSizeDeltas gives the size change of the unit starting at an old
// address, from every window's function map.
func funcSizeDeltas(structures []predictionPlan) func(uint64) (int64, bool) {
	deltas := make(map[uint64]int64)
	for _, structure := range structures {
		for _, m := range structure.Maps {
			deltas[structure.OldAddr+m.Src] = int64(m.DstSize) - int64(m.SrcSize)
		}
	}
	return func(addr uint64) (int64, bool) {
		d, ok := deltas[addr]
		return d, ok
	}
}
