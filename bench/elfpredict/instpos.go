package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

// probeInstructionPositions asks whether the correction's position column is
// paying for a basis it does not need.
//
// The shipped columnar correction names a wrong run by the byte gap since the
// end of the previous one. On .text that column is the single largest piece of
// the correction, and its values are byte counts over a stream whose real
// quantum is the instruction: §11 measured that 99.4% of structurally wrong
// instructions sit on a real boundary in the prediction's own instruction
// stream, so a run start is nearly always an instruction start. Counting the
// gap in instructions instead of bytes divides the alphabet by the average
// instruction length and, more to the point, replaces "3, 7, 4, 11" with
// "1, 2, 1, 3".
//
// The decoder can do the walk: it holds the prediction before it applies the
// correction, so it can decode the same instruction stream the encoder did.
// Where a run start is not a boundary the position needs an escape -- the
// containing instruction plus a byte offset inside it -- and the probe prices
// that escape rather than assuming it away.
//
// Report only. Nothing here changes the shipped format.
func probeInstructionPositions(predText, targetText []byte) {
	c, err := encodeColumnar(predText, targetText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  probe instpos FAILED: %v\n", err)
		return
	}

	// One merged pass: the wrong runs and the prediction's instruction stream
	// are both scanned in increasing offset order, so the instruction walk
	// never restarts and nothing per-instruction is stored.
	curStart, curLen, curIdx, insts := 0, 0, -1, 0
	advanceTo := func(p int) {
		for curStart+curLen <= p && curStart+curLen < len(predText) {
			curStart += curLen
			curIdx++
			insts++
			if inst, ok := safeDecode(predText[curStart:]); ok {
				curLen = inst.Len
			} else {
				curLen = 1 // an undecodable byte is its own unit on both sides
			}
		}
	}

	var (
		gapsAll  []byte // instruction gap for every run
		gapsOn   []byte // instruction gap, boundary runs only
		gapsEsc  []byte // instruction gap, escape runs only
		flags    []byte // 0 = run starts on an instruction boundary, 1 = escape
		byteOffs []byte // escape only: byte offset inside the containing instruction
		offsAll  []byte // byte offset for every run, 0 where it is a boundary
	)
	runs, onBoundary, escapes, prevIdx := 0, 0, 0, 0
	for i := 0; i < len(targetText); {
		if predText[i] == targetText[i] {
			i++
			continue
		}
		j := i
		for j < len(targetText) && predText[j] != targetText[j] {
			j++
		}
		advanceTo(i)
		runs++
		gap := uint64(curIdx - prevIdx)
		gapsAll = binary.AppendUvarint(gapsAll, gap)
		offsAll = binary.AppendUvarint(offsAll, uint64(i-curStart))
		if i == curStart {
			onBoundary++
			flags = append(flags, 0)
			gapsOn = binary.AppendUvarint(gapsOn, gap)
		} else {
			escapes++
			flags = append(flags, 1)
			gapsEsc = binary.AppendUvarint(gapsEsc, gap)
			byteOffs = binary.AppendUvarint(byteOffs, uint64(i-curStart))
		}
		prevIdx = curIdx
		i = j
	}
	// Finish the walk so the instruction count covers the whole region.
	advanceTo(len(predText))

	shipped := xzSize(c.Gaps)
	shippedT1 := xzSizeContiguous(c.Gaps)
	fmt.Fprintf(os.Stderr, "  probe instpos: %d wrong runs over %d prediction instructions in %d bytes of .text\n",
		runs, insts, len(predText))
	fmt.Fprintf(os.Stderr, "    %d runs start on an instruction boundary (%.2f%%), %d need an escape (%.2f%%)\n",
		onBoundary, 100*float64(onBoundary)/float64(max(1, runs)),
		escapes, 100*float64(escapes)/float64(max(1, runs)))
	fmt.Fprintf(os.Stderr, "    shipped byte-gap column        %10d raw, xz %8d (-T1 %d)\n",
		len(c.Gaps), shipped, shippedT1)

	price := func(name string, cols ...[]byte) int {
		total, raw, parts := 0, 0, ""
		for _, b := range cols {
			s := xzSize(b)
			total += s
			raw += len(b)
			parts += fmt.Sprintf(" %d", s)
		}
		fmt.Fprintf(os.Stderr, "    %-30s %10d raw, xz %8d (%+d) =%s\n",
			name, raw, total, total-shipped, parts)
		return total
	}
	price("instruction gap + esc cols", gapsAll, flags, byteOffs)
	price("  gaps split by escape flag", gapsOn, gapsEsc, flags, byteOffs)
	price("  gap column alone", gapsAll)
	// The flag is redundant: a byte offset of 0 already says "boundary", so
	// dropping the flag and paying an offset for every run costs 70,097 extra
	// zero bytes and saves the whole flag column.
	price("flagless gap + offset", gapsAll, offsAll)

	t1 := func(name string, cols ...[]byte) {
		total := 0
		for _, b := range cols {
			total += xzSizeContiguous(b)
		}
		fmt.Fprintf(os.Stderr, "    %-30s xz -T1 %8d (%+d)\n", name, total, total-shippedT1)
	}
	t1("instruction gap + esc cols -T1", gapsAll, flags, byteOffs)
}
