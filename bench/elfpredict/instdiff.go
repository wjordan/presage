package main

import (
	"fmt"
	"os"

	"golang.org/x/arch/x86/x86asm"
)

func safeDecode(code []byte) (inst x86asm.Inst, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	i, err := x86asm.Decode(code, 64)
	if err != nil || i.Len == 0 {
		return x86asm.Inst{}, false
	}
	return i, true
}

// probeInstructionDiff asks what the residual is at instruction granularity,
// which is the question direction 1 turns on. §9.15 built a field layer for
// four-byte PC-relative displacements and won 180,360; only 1.9% of the
// remaining wrong bytes are in those. If the rest sit in instructions whose
// operation and length the prediction already got right -- differing only in a
// modrm, SIB or immediate field -- then the same layer generalises to them.
// If they sit in instructions the prediction got structurally wrong, it does
// not.
//
// The prediction and the target share new-image coordinates, so an instruction
// found at offset s in the target is compared with whatever the prediction has
// at offset s. No global alignment is needed or assumed.
func probeInstructionDiff(predText, targetText []byte, maps []mapping) {
	type cls struct{ insts, wrong int }
	var sameOp, sameOpLen, diffOp, undecodable, unaligned cls
	// Split the structurally-wrong bucket by whether the target's instruction
	// boundary is also a boundary in the prediction's own instruction stream.
	// If it is not, the prediction is misaligned there and "different op" is an
	// artefact of decoding mid-instruction -- a matcher problem, not new code.
	var offBoundary, onBoundary cls
	for _, m := range maps {
		if m.Dst+m.DstSize > uint64(len(targetText)) {
			continue
		}
		pred := predText[m.Dst : m.Dst+m.DstSize]
		want := targetText[m.Dst : m.Dst+m.DstSize]
		dirty := false
		for i := range want {
			if pred[i] != want[i] {
				dirty = true
				break
			}
		}
		if !dirty {
			continue
		}
		bound := make([]bool, len(pred)+1)
		for k := 0; k < len(pred); {
			bound[k] = true
			pi, ok := safeDecode(pred[k:])
			if !ok {
				k++
				continue
			}
			k += pi.Len
		}
		for s := 0; s < len(want); {
			ti, ok := safeDecode(want[s:])
			if !ok {
				s++
				continue
			}
			end := min(s+ti.Len, len(want))
			w := 0
			for k := s; k < end; k++ {
				if pred[k] != want[k] {
					w++
				}
			}
			if w == 0 {
				s = end
				continue
			}
			pi, pok := safeDecode(pred[s:])
			switch {
			case !pok:
				undecodable.insts++
				undecodable.wrong += w
			case pi.Op == ti.Op && pi.Len == ti.Len:
				sameOp.insts++
				sameOp.wrong += w
			case pi.Op == ti.Op:
				sameOpLen.insts++
				sameOpLen.wrong += w
			case pi.Len == ti.Len:
				unaligned.insts++
				unaligned.wrong += w
			default:
				diffOp.insts++
				diffOp.wrong += w
				if bound[s] {
					onBoundary.insts++
					onBoundary.wrong += w
				} else {
					offBoundary.insts++
					offBoundary.wrong += w
				}
			}
			s = end
		}
	}
	total := sameOp.wrong + sameOpLen.wrong + diffOp.wrong + undecodable.wrong + unaligned.wrong
	fmt.Fprintf(os.Stderr, "  probe instruction diff: %d wrong bytes classified\n", total)
	for _, c := range []struct {
		name string
		c    cls
	}{{"same op, same length (operand only)", sameOp}, {"same op, different length", sameOpLen},
		{"different op, same length", unaligned}, {"different op and length", diffOp},
		{"prediction undecodable there", undecodable},
		{"  of which: pred is on an instruction boundary", onBoundary},
		{"  of which: pred is mid-instruction (misaligned)", offBoundary}} {
		fmt.Fprintf(os.Stderr, "    %-36s %9d instructions, %9d wrong bytes (%.1f%%)\n",
			c.name, c.c.insts, c.c.wrong, 100*float64(c.c.wrong)/float64(max(1, total)))
	}
}

// canonicalise zeroes every PC-relative displacement, so two copies of the
// same code compiled the same way become byte-identical wherever their call
// targets differ. This is the transform x86.ContentHash applies before
// hashing, and its absence is what makes a raw-byte LZ probe over machine code
// close to uninformative.
func canonicalise(code []byte) []byte {
	out := append([]byte(nil), code...)
	for i := 0; i < len(out); {
		inst, ok := safeDecode(out[i:])
		if !ok {
			i++
			continue
		}
		if inst.PCRel > 0 && inst.PCRelOff > 0 && i+inst.PCRelOff+inst.PCRel <= len(out) {
			clear(out[i+inst.PCRelOff : i+inst.PCRelOff+inst.PCRel])
		}
		i += inst.Len
	}
	return out
}

// probeCanonicalDictionary redoes §9.17's dictionary measurement on
// canonicalised bytes. That probe asked whether the residual's long runs occur
// elsewhere in the predicted image and answered no -- but it asked with raw
// bytes, and machine code does not self-match raw: two instances of identical
// code diverge at every call site. This asks the same question with the
// displacements zeroed on both sides, which is the only form in which the
// answer means anything.
func probeCanonicalDictionary(predText, targetText []byte, maps []mapping, window int) {
	var long []byte
	runs := 0
	for _, m := range maps {
		if m.Dst+m.DstSize > uint64(len(targetText)) {
			continue
		}
		p := canonicalise(predText[m.Dst : m.Dst+m.DstSize])
		t := canonicalise(targetText[m.Dst : m.Dst+m.DstSize])
		for i := 0; i < len(t); {
			if p[i] == t[i] {
				i++
				continue
			}
			j := i
			for j < len(t) && p[j] != t[j] {
				j++
			}
			if j-i >= correctionBuckets {
				long = append(long, t[i:j]...)
				runs++
			}
			i = j
		}
	}
	dict := predText
	if len(dict) > window {
		dict = dict[len(dict)-window:]
	}
	dict = canonicalise(dict)
	alone := xzSizeContiguous(long)
	base := xzSizeContiguous(dict)
	with := xzSizeContiguous(append(append([]byte(nil), dict...), long...))
	fmt.Fprintf(os.Stderr, "  probe canonical dictionary: %d long runs, %d bytes; alone xz %d, marginal after %d-byte canonical prediction %d\n",
		runs, len(long), alone, len(dict), with-base)
}
