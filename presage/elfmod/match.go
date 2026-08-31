package elfmod

import (
	"bytes"
	"slices"

	"github.com/zeebo/blake3"

	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/presage/symbols"
)

// nameID is a fixed-size fingerprint of a symbol name: identity is all the
// matcher needs, and C++ aliases would bloat memory as strings.
type nameID [16]byte

func fingerprint(s string) nameID {
	sum := blake3.Sum256([]byte(s))
	var id nameID
	copy(id[:], sum[:len(id)])
	return id
}

// codeUnit is one function of .text as the symbols describe it, in .text
// offsets: the largest symbol at an address, clipped to the next start.
type codeUnit struct {
	Off   uint64
	Size  uint64
	Names []nameID
}

type symbolGroup struct {
	maxSize uint64
	names   []nameID
}

// codeUnits groups the function symbols of one image by address.
func codeUnits(r symbols.Reader, text section) ([]codeUnit, error) {
	groups := make(map[uint64]*symbolGroup)
	textEnd := text.Addr + text.Size
	err := r.Funcs(func(f symbols.Func) {
		if f.Size == 0 || f.Addr < text.Addr || f.Addr >= textEnd {
			return
		}
		g := groups[f.Addr]
		if g == nil {
			g = &symbolGroup{}
			groups[f.Addr] = g
		}
		g.maxSize = max(g.maxSize, f.Size)
		if f.Name != "" {
			g.names = append(g.names, fingerprint(symbols.CanonicalName(f.Name)))
		}
	})
	if err != nil {
		return nil, err
	}
	starts := make([]uint64, 0, len(groups))
	for addr := range groups {
		starts = append(starts, addr)
	}
	slices.Sort(starts)
	units := make([]codeUnit, 0, len(starts))
	for i, addr := range starts {
		limit := textEnd - addr
		if i+1 < len(starts) {
			limit = starts[i+1] - addr
		}
		sz := min(groups[addr].maxSize, limit)
		if sz == 0 {
			continue
		}
		names := groups[addr].names
		slices.SortFunc(names, func(a, b nameID) int { return bytes.Compare(a[:], b[:]) })
		names = slices.Compact(names)
		units = append(units, codeUnit{Off: addr - text.Addr, Size: sz, Names: names})
	}
	return units, nil
}

type hashKey struct {
	Size uint64
	Hash uint64
}

func code(b []byte, u codeUnit) []byte { return b[u.Off : u.Off+u.Size] }

// chooseNameCandidate scores the old units sharing a name with the new
// one: same size, then canonical-equal, then byte-equal.
func chooseNameCandidate(candidates []int, ou []codeUnit, nu codeUnit, oldText, newCode []byte) (int, int) {
	best, bestScore, bestDist := -1, -1, uint64(0)
	seen := make(map[int]struct{}, len(candidates))
	for _, oi := range candidates {
		if _, ok := seen[oi]; ok {
			continue
		}
		seen[oi] = struct{}{}
		o := ou[oi]
		score := 0
		if o.Size == nu.Size {
			score = 1
			oldCode := code(oldText, o)
			if x86.Equal(oldCode, newCode) {
				score = 2
			}
			if bytes.Equal(oldCode, newCode) {
				score = 3
			}
		}
		// Duplicate names (closures, monomorphisations, anonymous-namespace
		// templates) tie on score; the twin at the nearest position wins.
		dist := max(o.Off, nu.Off) - min(o.Off, nu.Off)
		if score > bestScore || (score == bestScore && dist < bestDist) {
			best, bestScore, bestDist = oi, score, dist
		}
	}
	return best, bestScore
}

// matchStats counts how the function map was built.
type matchStats struct {
	NameMapped, ContentMapped, CopyUnits, CopyBytes int
}

// constructPlan builds the function map: by name first, then by content
// hash verified canonical-equal for units whose name changed.
func constructPlan(oldUnits, newUnits []codeUnit, oldText, newText []byte, oldAddr, newAddr uint64, ranges []addressRange) (predictionPlan, matchStats) {
	byName := make(map[nameID][]int)
	for i, u := range oldUnits {
		for _, name := range u.Names {
			byName[name] = append(byName[name], i)
		}
	}
	maps := make([]mapping, 0, len(newUnits))
	mappedNew := make([]bool, len(newUnits))
	var st matchStats
	for ni, n := range newUnits {
		var candidates []int
		for _, name := range n.Names {
			candidates = append(candidates, byName[name]...)
		}
		if len(candidates) == 0 {
			continue
		}
		newCode := code(newText, n)
		oi, score := chooseNameCandidate(candidates, oldUnits, n, oldText, newCode)
		if oi < 0 {
			continue
		}
		o := oldUnits[oi]
		// Copy marks a canonical-equal body; the reference points are
		// inferred from those only, and allMapped then copies everything.
		maps = append(maps, mapping{Src: o.Off, SrcSize: o.Size, Dst: n.Off, DstSize: n.Size, Copy: score >= 2})
		if score >= 2 {
			st.CopyUnits++
			st.CopyBytes += int(n.Size)
		}
		mappedNew[ni] = true
		st.NameMapped++
	}
	byHash := make(map[hashKey][]int, len(oldUnits))
	for oi, o := range oldUnits {
		k := hashKey{Size: o.Size, Hash: x86.ContentHash(code(oldText, o))}
		byHash[k] = append(byHash[k], oi)
	}
	for ni, n := range newUnits {
		if mappedNew[ni] {
			continue
		}
		newCode := code(newText, n)
		k := hashKey{Size: n.Size, Hash: x86.ContentHash(newCode)}
		// Monomorphisation and ICF give one canonical body many twins,
		// so take the nearest by position as chooseNameCandidate does
		// rather than the first: an arbitrary source poisons the
		// retarget and the matcher's expected-source hint.
		best, bestDist := -1, uint64(0)
		for _, oi := range byHash[k] {
			o := oldUnits[oi]
			if !x86.Equal(code(oldText, o), newCode) {
				continue
			}
			dist := max(o.Off, n.Off) - min(o.Off, n.Off)
			if best < 0 || dist < bestDist {
				best, bestDist = oi, dist
			}
			if dist == 0 {
				break
			}
		}
		if best < 0 {
			continue
		}
		o := oldUnits[best]
		maps = append(maps, mapping{Src: o.Off, SrcSize: o.Size, Dst: n.Off, DstSize: n.Size, Copy: true})
		st.ContentMapped++
		st.CopyUnits++
		st.CopyBytes += int(n.Size)
	}
	slices.SortFunc(maps, func(a, b mapping) int {
		if a.Dst != b.Dst {
			return cmpU(a.Dst, b.Dst)
		}
		return cmpU(a.Src, b.Src)
	})
	return predictionPlan{OldAddr: oldAddr, NewAddr: newAddr, TargetLen: uint64(len(newText)), Maps: maps, Ranges: ranges}, st
}

type pointChoice struct {
	new       uint64
	ambiguous bool
}

// inferReferencePoints compares operands where canonical bytes prove old
// and new bodies share an instruction stream, and emits the exact old→new
// target pairs the map and ranges would get wrong. Ambiguous targets are
// left out.
func inferReferencePoints(plan predictionPlan, oldText, newText []byte) []addressPoint {
	choices := make(map[uint64]pointChoice)
	for _, m := range plan.Maps {
		if !m.Copy || m.SrcSize != m.DstSize {
			continue
		}
		oldRefs := x86.References(oldText[m.Src:m.Src+m.SrcSize], plan.OldAddr+m.Src)
		newRefs := x86.References(newText[m.Dst:m.Dst+m.DstSize], plan.NewAddr+m.Dst)
		if len(oldRefs) != len(newRefs) {
			continue
		}
		for i, oldRef := range oldRefs {
			newRef := newRefs[i]
			if oldRef.Off != newRef.Off || oldRef.N != newRef.N {
				continue
			}
			// Negative-looking targets come from decoding embedded data
			// or a symbol boundary that cuts an instruction.
			if oldRef.Target >= 1<<63 || newRef.Target >= 1<<63 {
				continue
			}
			choice, ok := choices[oldRef.Target]
			if !ok {
				choices[oldRef.Target] = pointChoice{new: newRef.Target}
			} else if choice.new != newRef.Target {
				choice.ambiguous = true
				choices[oldRef.Target] = choice
			}
		}
	}
	lk := newAddressLookup(predictionPlan{OldAddr: plan.OldAddr, NewAddr: plan.NewAddr, Maps: plan.Maps, Ranges: plan.Ranges})
	points := make([]addressPoint, 0, len(choices))
	for old, choice := range choices {
		t := lk.target(old)
		if !choice.ambiguous && (!t.Known || t.Addr != choice.new) {
			points = append(points, addressPoint{Old: old, New: choice.new})
		}
	}
	slices.SortFunc(points, func(a, b addressPoint) int { return cmpU(a.Old, b.Old) })
	return points
}

// allMapped marks every map as copied: the structural prediction lays
// every matched body and the per-function choice decides what is used.
func allMapped(p predictionPlan) predictionPlan {
	p.Maps = slices.Clone(p.Maps)
	for i := range p.Maps {
		p.Maps[i].Copy = true
	}
	return p
}
