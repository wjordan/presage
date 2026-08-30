package elfmod

import "github.com/wjordan/presage/delta/x86"

// retargetEquivalencePrediction rewrites every PC-relative field in the
// laid .text window (text is the new .text, ep and structure describe it)
// whose whole instruction came from one run: the old target is resolved
// through lookup and the displacement recomputed for the new position.
func retargetEquivalencePrediction(text []byte, ep equivalencePlan, structure predictionPlan, lookup ImageOracle) x86.Stats {
	retargetBody := func(stats *x86.Stats, body []byte, dstBase uint64) {
		x86.WalkReferences(body, 0, func(ref x86.Reference) {
			stats.Refs++
			fullStart := ep.NewText.Off + dstBase + uint64(ref.Start)
			fullField := ep.NewText.Off + dstBase + uint64(ref.Off)
			fullLast := ep.NewText.Off + dstBase + uint64(ref.Next-1)
			srcStart, startEq, startOK := ep.sourceAt(fullStart)
			srcField, fieldEq, fieldOK := ep.sourceAt(fullField)
			srcLast, lastEq, lastOK := ep.sourceAt(fullLast)
			if !startOK || !fieldOK || !lastOK || startEq != fieldEq || startEq != lastEq || srcField < srcStart || srcLast < srcField {
				stats.Unknown++
				return
			}
			oldTextEnd := ep.OldText.Off + ep.OldText.Size
			if srcStart < ep.OldText.Off || srcLast >= oldTextEnd {
				stats.Unknown++
				return
			}
			disp, ok := readDisplacement(body, ref)
			if !ok {
				stats.Unknown++
				return
			}
			oldNext := ep.OldText.Addr + srcLast + 1 - ep.OldText.Off
			target := lookup(uint64(int64(oldNext) + disp))
			if !target.Known {
				stats.Unknown++
				return
			}
			newNext := ep.NewText.Addr + dstBase + uint64(ref.Next)
			if !writeDisplacement(body, ref, int64(target.Addr)-int64(newNext)) {
				stats.NoFit++
			}
		})
	}
	if len(structure.Maps) == 0 {
		var stats x86.Stats
		retargetBody(&stats, text, 0)
		return stats
	}
	// One body per map, concurrently: bodies are disjoint and every lookup
	// is a read, so the result matches the serial loop.
	return parallelStats(len(structure.Maps), workers(), func(stats *x86.Stats, i int) {
		m := structure.Maps[i]
		if m.Dst > uint64(len(text)) || m.DstSize > uint64(len(text))-m.Dst {
			return
		}
		retargetBody(stats, text[m.Dst:m.Dst+m.DstSize], m.Dst)
	})
}

func wrongCount(a, b []byte) int {
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

// chooseStructuralFunctions picks, per mapped function, the prediction
// with fewer wrong bytes against the target: one bit per map, set where
// the structural body wins over the retargeted equivalence copy.
func chooseStructuralFunctions(equivalencePred, structuralPred, target []byte, structure predictionPlan) (choices []byte, functions, selectedBytes int) {
	win := make([]bool, len(structure.Maps))
	const shard = 4096
	parallelFor((len(structure.Maps)+shard-1)/shard, func(s int) {
		for i := s * shard; i < min((s+1)*shard, len(structure.Maps)); i++ {
			m := structure.Maps[i]
			targetBody := target[m.Dst : m.Dst+m.DstSize]
			win[i] = wrongCount(structuralPred[m.Dst:m.Dst+m.DstSize], targetBody) < wrongCount(equivalencePred[m.Dst:m.Dst+m.DstSize], targetBody)
		}
	})
	choices = make([]byte, (len(structure.Maps)+7)/8)
	for i, m := range structure.Maps {
		if !win[i] {
			continue
		}
		choices[i/8] |= 1 << (i % 8)
		functions++
		selectedBytes += int(m.DstSize)
	}
	return choices, functions, selectedBytes
}
