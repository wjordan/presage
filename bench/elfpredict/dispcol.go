package main

import (
	"fmt"
	"os"
	"slices"
)

// probeDisplacementColumn tests §11.5's one open direction.
//
// The columnar correction splits replacement bytes by the length of the run
// they belong to, and the last bucket -- runs of five bytes and up -- is where
// newly emitted instructions land. Canonicalising that bucket drops it
// sharply, and what leaves is displacement fields: branch targets inside
// instructions the prediction never emitted, which §9.15's field layer cannot
// reach because it names fields by walking the prediction and there is no
// field there to name.
//
// §11.5 called them image-spanning values and predicted, from §9.17's rule,
// that they want an index into an enumerable domain -- and the decoder holds
// one, every new function's start address, straight out of the function map.
// The measurement below tests that, and also tests the rule's other half:
// §9.17 says *local* values want a byte basis instead, and a jump inside its
// own function is exactly that. So the fields are classified before they are
// coded, and three cuts are priced.
//
// Both sides can do the walk. Zeroing a displacement does not change any
// instruction's length, so after the correction is applied the decoder can
// decode the same instruction stream the encoder did and fill each field back
// in. columnarXZ already prices every stream with its own xz call, so adding
// columns and shrinking another is measured exactly.
func probeDisplacementColumn(predText, targetText []byte, maps []mapping, newTextAddr uint64) {
	starts := make([]uint64, 0, len(maps))
	for _, m := range maps {
		starts = append(starts, newTextAddr+m.Dst)
	}
	slices.Sort(starts)
	starts = slices.Compact(starts)

	const (
		classHit = iota // target is a new function start
		classLocal
		classFar
	)
	type field struct {
		pos, w int // pos indexes the concatenated bucket
		abs    uint64
		disp   int64
		class  int
	}
	var raw []byte
	var flat []field
	var widths [9]int
	var runs int

	for _, m := range maps {
		if m.Dst+m.DstSize > uint64(len(targetText)) {
			continue
		}
		pred := predText[m.Dst : m.Dst+m.DstSize]
		targ := targetText[m.Dst : m.Dst+m.DstSize]
		fnLo, fnHi := newTextAddr+m.Dst, newTextAddr+m.Dst+m.DstSize

		type site struct {
			off, w int
			abs    uint64
			disp   int64
		}
		var flds []site
		for i := 0; i < len(targ); {
			inst, ok := safeDecode(targ[i:])
			if !ok {
				i++
				continue
			}
			if inst.PCRel > 0 && inst.PCRelOff > 0 && i+inst.PCRelOff+inst.PCRel <= len(targ) {
				off := inst.PCRelOff + i
				var v int64
				for k := inst.PCRel - 1; k >= 0; k-- {
					v = v<<8 | int64(targ[off+k])
				}
				if shift := 64 - 8*inst.PCRel; shift > 0 {
					v = v << shift >> shift // sign-extend
				}
				next := int64(fnLo) + int64(i+inst.Len)
				flds = append(flds, site{off, inst.PCRel, uint64(next + v), v})
			}
			i += inst.Len
		}

		fi := 0
		for i := 0; i < len(targ); {
			if pred[i] == targ[i] {
				i++
				continue
			}
			j := i
			for j < len(targ) && pred[j] != targ[j] {
				j++
			}
			if j-i >= correctionBuckets {
				base := len(raw)
				raw = append(raw, targ[i:j]...)
				runs++
				for fi < len(flds) && flds[fi].off < i {
					fi++
				}
				for fi < len(flds) && flds[fi].off+flds[fi].w <= j {
					f := flds[fi]
					cl := classFar
					if _, ok := slices.BinarySearch(starts, f.abs); ok {
						cl = classHit
					} else if f.abs >= fnLo && f.abs < fnHi {
						cl = classLocal
					}
					flat = append(flat, field{base + f.off - i, f.w, f.abs, f.disp, cl})
					widths[f.w]++
					fi++
				}
			}
			i = j
		}
	}

	// Content with a chosen subset of the fields zeroed.
	canonWith := func(keep func(int) bool) []byte {
		out := append([]byte(nil), raw...)
		for _, f := range flat {
			if keep(f.class) {
				clear(out[f.pos : f.pos+f.w])
			}
		}
		return out
	}
	// Columns for a chosen subset, tagged only over the fields pulled out.
	columns := func(keep func(int) bool) (tag, idx, loc, far []byte, n int) {
		var prevIdx, prevFar int64
		for _, f := range flat {
			if !keep(f.class) {
				continue
			}
			n++
			tag = append(tag, byte(f.class))
			switch f.class {
			case classHit:
				i, _ := slices.BinarySearch(starts, f.abs)
				idx = appendS(idx, int64(i)-prevIdx)
				prevIdx = int64(i)
			case classLocal:
				loc = appendS(loc, f.disp)
			default:
				far = appendS(far, int64(f.abs)-prevFar)
				prevFar = int64(f.abs)
			}
		}
		return tag, idx, loc, far, n
	}

	rawXZ := xzSize(raw)
	n := max(len(flat), 1)
	var hits, locals, fars int
	for _, f := range flat {
		switch f.class {
		case classHit:
			hits++
		case classLocal:
			locals++
		default:
			fars++
		}
	}
	fmt.Fprintf(os.Stderr, "  probe dispcol: %d long runs, %d bytes; %d displacements contained (widths 1:%d 2:%d 4:%d)\n",
		runs, len(raw), len(flat), widths[1], widths[2], widths[4])
	fmt.Fprintf(os.Stderr, "    targets: %d function starts (%.1f%%), %d inside the same function (%.1f%%), %d elsewhere (%.1f%%)\n",
		hits, 100*float64(hits)/float64(n), locals, 100*float64(locals)/float64(n), fars, 100*float64(fars)/float64(n))
	fmt.Fprintf(os.Stderr, "    bucket as shipped                      xz %8d\n", rawXZ)

	for _, v := range []struct {
		name string
		keep func(int) bool
	}{
		{"pull out every field", func(int) bool { return true }},
		{"pull out non-local only", func(c int) bool { return c != classLocal }},
		{"pull out function-start calls only", func(c int) bool { return c == classHit }},
	} {
		content := xzSize(canonWith(v.keep))
		tag, idx, loc, far, cnt := columns(v.keep)
		tagXZ, idxXZ, locXZ, farXZ := xzSize(tag), xzSize(idx), xzSize(loc), xzSize(far)
		total := content + tagXZ + idxXZ + locXZ + farXZ
		fmt.Fprintf(os.Stderr, "    %-36s xz %8d = content %d + tag %d + idx %d + local %d + far %d, %d fields (%+d)\n",
			v.name, total, content, tagXZ, idxXZ, locXZ, farXZ, cnt, total-rawXZ)
	}

	// The probe re-derives the long-run bucket from the same rule
	// encodeColumnar uses, but only inside mapped functions; if the two
	// disagree by more than that, every figure above is against the wrong
	// baseline.
	if c, err := encodeColumnar(predText, targetText); err == nil {
		b := c.Bytes[correctionBuckets-1]
		fmt.Fprintf(os.Stderr, "    check: format ships bucket 4 as %d bytes / xz %d; probe covers %d / %d (the rest is outside any mapping)\n",
			len(b), xzSize(b), len(raw), rawXZ)
	}
}
