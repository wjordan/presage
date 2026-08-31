package elfmod

import "github.com/wjordan/presage/delta/x86"

// retargetEquivalencePrediction rewrites every PC-relative field in one
// laid code window (text is the window's new bytes, w its geometry and
// structure its map) whose whole instruction came from one run: the old
// target is resolved through lookup and the displacement recomputed for the
// new position.
func retargetEquivalencePrediction(text []byte, ep equivalencePlan, w codeWindow, structure predictionPlan, lookup ImageOracle) x86.Stats {
	retargetBody := func(stats *x86.Stats, body []byte, dstBase uint64) {
		// References arrive in ascending address order, and so do the three
		// probes each one makes, so one cursor over the runs serves the
		// whole body.
		runs := newEqCursor(ep.Eqs)
		x86.WalkReferences(body, 0, func(ref x86.Reference) {
			stats.Refs++
			fullStart := w.New.Off + dstBase + uint64(ref.Start)
			fullField := w.New.Off + dstBase + uint64(ref.Off)
			fullLast := w.New.Off + dstBase + uint64(ref.Next-1)
			srcStart, startEq, startOK := runs.at(fullStart)
			srcField, fieldEq, fieldOK := runs.at(fullField)
			srcLast, lastEq, lastOK := runs.at(fullLast)
			if !startOK || !fieldOK || !lastOK || startEq != fieldEq || startEq != lastEq || srcField < srcStart || srcLast < srcField {
				stats.Unknown++
				return
			}
			// The instruction's source may sit in any code window: a body
			// that migrated between windows (BOLT re-tiers on every
			// profile) is copied here by a run from its old home. The
			// source window's geometry names the old address.
			sw, ok := oldWindowOf(ep.Windows, srcStart, srcLast)
			if !ok {
				stats.Unknown++
				return
			}
			disp, ok := readDisplacement(body, ref)
			if !ok {
				stats.Unknown++
				return
			}
			oldNext := sw.Old.Addr + srcLast + 1 - sw.Old.Off
			target := lookup(uint64(int64(oldNext) + disp))
			if !target.Known {
				stats.Unknown++
				return
			}
			newNext := w.New.Addr + dstBase + uint64(ref.Next)
			if !writeDisplacement(body, ref, int64(target.Addr)-int64(newNext)) {
				stats.NoFit++
			}
		})
	}
	// Every byte of the window, as disjoint spans: each mapped body from
	// its own start, and the gaps between them.
	//
	// The gaps are not slack. A gap is code the symbols do not describe,
	// and on a BOLT'd image that is the orphaned original of every function
	// BOLT moved into the new .text -- 57% of .bolt.org.text, still mapped,
	// still executable, still full of PC-relative displacements. Retargeting
	// only the mapped bodies left 39% of librustc_driver's code with the old
	// image's displacements, which cost far more than the map won.
	spans := windowSpans(structure.Maps, uint64(len(text)))
	return parallelStats(len(spans), workers(), func(stats *x86.Stats, i int) {
		s := spans[i]
		retargetBody(stats, text[s.Off:s.Off+s.Size], s.Off)
	})
}

// oldWindowOf finds the window whose old section contains [lo, hi].
func oldWindowOf(windows []codeWindow, lo, hi uint64) (codeWindow, bool) {
	for _, w := range windows {
		if lo >= w.Old.Off && hi < w.Old.Off+w.Old.Size {
			return w, true
		}
	}
	return codeWindow{}, false
}

// span is a half-open byte range of a code window.
type span struct{ Off, Size uint64 }

// windowSpans covers [0, size) with the maps' destination bodies and the
// gaps between them, in order. Maps are sorted by destination and disjoint;
// anything that is not is skipped rather than trusted.
func windowSpans(maps []mapping, size uint64) []span {
	spans := make([]span, 0, 2*len(maps)+1)
	var pos uint64
	for _, m := range maps {
		if m.Dst < pos || m.Dst >= size {
			continue
		}
		end := min(m.Dst+m.DstSize, size)
		if end <= m.Dst {
			continue
		}
		if m.Dst > pos {
			spans = append(spans, span{Off: pos, Size: m.Dst - pos})
		}
		spans = append(spans, span{Off: m.Dst, Size: end - m.Dst})
		pos = end
	}
	if pos < size {
		spans = append(spans, span{Off: pos, Size: size - pos})
	}
	return spans
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
