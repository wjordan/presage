package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

// Every numeric column in the plan is a LEB128 varint stream. Two
// representation changes are worth pricing against that, both in the spirit of
// §9.14's byte bucketing -- which won 70,920 by giving xz homogeneous streams
// instead of one mixed one.
//
//   - Bucketed: partition the stream by each value's encoded width, so a
//     column of mostly-small values with a few large ones stops interleaving
//     them.
//   - Packed: the same partition, but each bucket written at its natural fixed
//     width, which drops LEB128's continuation bits -- one bit in eight.
//
// Packing is the "packed bitfield" idea done at byte granularity. Finer than
// that is not worth testing: transposing a four-byte bucket already cost
// 27,288 (§9.14), because sub-byte repacking destroys the byte alignment the
// literal coder and the match finder both depend on.
// transposeColumn is the decodable cousin of bucketColumn. Partitioning by
// encoded width is not self-describing -- the decoder would need each value's
// width to know which bucket to draw from, and that width is precisely what
// LEB128 states inline. Grouping by byte *position* keeps it: the k-th stream
// holds the k-th byte of every value long enough to have one, and the
// continuation bit in stream k says which values contribute to stream k+1.
func transposeColumn(col []byte) [][]byte {
	const maxW = 10
	var pos [maxW][]byte
	for i := 0; i < len(col); {
		_, w := binary.Uvarint(col[i:])
		if w <= 0 || w > maxW {
			return nil
		}
		for k := 0; k < w; k++ {
			pos[k] = append(pos[k], col[i+k])
		}
		i += w
	}
	var out [][]byte
	for k := 0; k < maxW; k++ {
		if len(pos[k]) != 0 {
			out = append(out, pos[k])
		}
	}
	return out
}

// bucketColumn re-partitions a varint stream by encoded width, returning the
// sub-streams in width order. Nothing is re-encoded: the same bytes come back,
// grouped.
func bucketColumn(col []byte) [][]byte {
	const maxW = 10
	var bucket [maxW + 1][]byte
	for i := 0; i < len(col); {
		_, w := binary.Uvarint(col[i:])
		if w <= 0 {
			return nil
		}
		bucket[w] = append(bucket[w], col[i:i+w]...)
		i += w
	}
	var out [][]byte
	for w := 1; w <= maxW; w++ {
		if len(bucket[w]) != 0 {
			out = append(out, bucket[w])
		}
	}
	return out
}

// probeColumnPackingInSitu is the measurement that decides it. Summing
// standalone compressions of sub-streams counts a fresh dictionary for each,
// and §9.3 found that overstating a column by 12x. Here both arrangements are
// concatenated into one buffer and compressed once, which is how a plan
// actually ships.
func probeColumnPackingInSitu(cols [][]byte) {
	var flat, split, trans []byte
	tStandalone := 0
	for _, c := range cols {
		flat = append(flat, c...)
		for _, b := range bucketColumn(c) {
			split = append(split, b...)
		}
		for _, b := range transposeColumn(c) {
			trans = append(trans, b...)
			tStandalone += xzSize(b)
		}
	}
	a, b, t := xzSize(flat), xzSize(split), xzSize(trans)
	fmt.Fprintf(os.Stderr, "  probe packing in situ: %d columns, %d B in one stream; as-is %d, width-bucketed %d (%+d), byte-transposed %d (%+d); transposed standalone sum %d\n",
		len(cols), len(flat), a, b, b-a, t, t-a, tStandalone)
}

func probeColumnPacking(name string, col []byte) {
	if len(col) == 0 {
		return
	}
	const maxW = 10
	var bucket, packed [maxW + 1][]byte
	n := 0
	for i := 0; i < len(col); {
		v, w := binary.Uvarint(col[i:])
		if w <= 0 {
			return
		}
		bucket[w] = append(bucket[w], col[i:i+w]...)
		// A w-byte varint carries 7w bits, so it fits in ceil(7w/8) bytes.
		fixed := min((7*w+7)/8, 8)
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], v)
		packed[w] = append(packed[w], buf[:fixed]...)
		i += w
		n++
	}
	base := xzSize(col)
	bSum, pSum, widths := 0, 0, ""
	for w := 1; w <= maxW; w++ {
		if len(bucket[w]) == 0 {
			continue
		}
		bSum += xzSize(bucket[w])
		pSum += xzSize(packed[w])
		widths += fmt.Sprintf(" %d:%d", w, len(bucket[w])/w)
	}
	fmt.Fprintf(os.Stderr, "    %-22s %8d values, as-is %8d, bucketed %8d (%+.1f%%), packed %8d (%+.1f%%);%s\n",
		name, n, base, bSum, 100*float64(bSum-base)/float64(base),
		pSum, 100*float64(pSum-base)/float64(base), widths)
}
