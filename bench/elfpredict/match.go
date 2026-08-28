package main

import (
	"bytes"
	"slices"

	"github.com/wjordan/go-binsync/delta/x86"
)

type matchStats struct {
	NameMapped       int `json:"name_mapped_units"`
	ContentMapped    int `json:"content_mapped_units"`
	CopyUnits        int `json:"normalized_equal_units"`
	CopyBytes        int `json:"normalized_equal_bytes"`
	ByteEqualUnits   int `json:"byte_equal_units"`
	ByteEqualBytes   int `json:"byte_equal_bytes"`
	ChangedNameUnits int `json:"changed_name_units"`
}

type hashKey struct {
	Size uint64
	Hash uint64
}

func code(b []byte, u codeUnit) []byte { return b[u.Off : u.Off+u.Size] }

func chooseNameCandidate(candidates []int, ou []codeUnit, nu codeUnit, oldText, newCode []byte) (int, int) {
	best, bestScore := -1, -1
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
		if score > bestScore {
			best, bestScore = oi, score
		}
	}
	return best, bestScore
}

func constructPlan(oldUnits, newUnits []codeUnit, oldText, newText []byte, oldAddr, newAddr uint64, ranges []addressRange) (predictionPlan, matchStats) {
	byName := make(map[nameID]int)
	for i, u := range oldUnits {
		for _, name := range u.Names {
			byName[name] = i
		}
	}

	maps := make([]mapping, 0, len(newUnits))
	mappedNew := make([]bool, len(newUnits))
	var st matchStats
	for ni, n := range newUnits {
		var candidates []int
		for _, name := range n.Names {
			if oi, ok := byName[name]; ok {
				candidates = append(candidates, oi)
			}
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
		m := mapping{Src: o.Off, SrcSize: o.Size, Dst: n.Off, DstSize: n.Size}
		if score >= 2 {
			m.Copy = true
			st.CopyUnits++
			st.CopyBytes += int(n.Size)
			if score == 3 {
				st.ByteEqualUnits++
				st.ByteEqualBytes += int(n.Size)
			}
		} else {
			st.ChangedNameUnits++
		}
		maps = append(maps, m)
		mappedNew[ni] = true
		st.NameMapped++
	}

	// Content matching recovers stable code whose generated or local name
	// changed. Ambiguous hashes are retained and verified byte-for-byte with
	// PC-relative operands ignored before a mapping is admitted.
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
		for _, oi := range byHash[k] {
			o := oldUnits[oi]
			if !x86.Equal(code(oldText, o), newCode) {
				continue
			}
			maps = append(maps, mapping{Src: o.Off, SrcSize: o.Size, Dst: n.Off, DstSize: n.Size, Copy: true})
			st.ContentMapped++
			st.CopyUnits++
			st.CopyBytes += int(n.Size)
			if bytes.Equal(code(oldText, o), newCode) {
				st.ByteEqualUnits++
				st.ByteEqualBytes += int(n.Size)
			}
			break
		}
	}
	slices.SortFunc(maps, func(a, b mapping) int { return cmpU(a.Dst, b.Dst) })
	return predictionPlan{OldAddr: oldAddr, NewAddr: newAddr, TargetLen: uint64(len(newText)), Maps: maps, Ranges: ranges}, st
}

type pointChoice struct {
	new       uint64
	ambiguous bool
}

// inferReferencePoints is an encoder-only oracle rung. It compares operands
// only where canonical instruction bytes already prove that old and new
// bodies have the same instruction stream, and emits address pairs rather
// than target bytes. Ambiguous old targets are excluded.
func inferReferencePoints(plan predictionPlan, oldText, newText []byte) []addressPoint {
	choices := make(map[uint64]pointChoice)
	for _, m := range plan.Maps {
		if !m.Copy || m.SrcSize != m.DstSize {
			continue
		}
		oldRefs := x86.References(code(oldText, codeUnit{Off: m.Src, Size: m.SrcSize}), plan.OldAddr+m.Src)
		newRefs := x86.References(code(newText, codeUnit{Off: m.Dst, Size: m.DstSize}), plan.NewAddr+m.Dst)
		if len(oldRefs) != len(newRefs) {
			continue
		}
		for i, oldRef := range oldRefs {
			newRef := newRefs[i]
			if oldRef.Off != newRef.Off || oldRef.N != newRef.N {
				continue
			}
			// Negative-looking targets come from decoding embedded data or a
			// symbol boundary that cuts an instruction. They are not image
			// addresses and make poor global correspondence evidence.
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
	bySrc := slices.Clone(plan.Maps)
	slices.SortFunc(bySrc, func(a, b mapping) int {
		if a.Src != b.Src {
			return cmpU(a.Src, b.Src)
		}
		return cmpU(a.Dst, b.Dst)
	})
	baseLookup := func(addr uint64) (uint64, bool) {
		if addr >= plan.OldAddr {
			off := addr - plan.OldAddr
			i, ok := slices.BinarySearchFunc(bySrc, off, func(m mapping, off uint64) int {
				if m.Src > off {
					return 1
				}
				if m.Src+m.SrcSize <= off {
					return -1
				}
				return 0
			})
			if ok {
				m := bySrc[i]
				delta := off - m.Src
				if delta < m.DstSize {
					return plan.NewAddr + m.Dst + delta, true
				}
			}
		}
		i, ok := slices.BinarySearchFunc(plan.Ranges, addr, func(ar addressRange, addr uint64) int {
			if ar.Old > addr {
				return 1
			}
			if ar.Old+ar.Size <= addr {
				return -1
			}
			return 0
		})
		if ok {
			ar := plan.Ranges[i]
			return ar.New + addr - ar.Old, true
		}
		return 0, false
	}
	points := make([]addressPoint, 0, len(choices))
	for old, choice := range choices {
		predicted, known := baseLookup(old)
		if !choice.ambiguous && (!known || predicted != choice.new) {
			points = append(points, addressPoint{Old: old, New: choice.new})
		}
	}
	slices.SortFunc(points, func(a, b addressPoint) int { return cmpU(a.Old, b.Old) })
	return points
}
