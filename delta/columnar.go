package delta

import (
	"encoding/binary"
	"fmt"
)

// The columnar correction is the positional correction for a region whose
// residual is short scattered runs — recompiled code, where the prediction
// is 99 % right and what is left is half a million displacement fields at
// unpredictable offsets. A matcher spends its budget describing where each
// run is in a format built for long copies; here the same edit list is
// written as columns the compressor sees separately: the gap since the
// last run, the run's length, and the replacement bytes bucketed by run
// length, because a four-byte run is usually a displacement whose high
// bytes are near-constant while a one-byte run is a lone changed opcode.
// Worth 7 % of Chrome's .text correction against the lz shapes.

// ColumnarBuckets is the number of byte columns: runs of 1, 2, 3, 4 and
// 5+ bytes each have their own.
const ColumnarBuckets = 5

// ColumnarStreams is how many streams EncodeColumnar returns.
const ColumnarStreams = 2 + ColumnarBuckets

func bucketOf(n int) int { return min(n, ColumnarBuckets) - 1 }

// EncodeColumnar writes the correction turning pred into want as
// ColumnarStreams streams: gaps, lengths, then the byte buckets.
func EncodeColumnar(pred, want []byte) ([][]byte, error) {
	if len(pred) != len(want) {
		return nil, fmt.Errorf("delta: prediction is %d bytes, target is %d", len(pred), len(want))
	}
	cols := make([][]byte, ColumnarStreams)
	prev := 0
	for i := 0; i < len(want); {
		if pred[i] == want[i] {
			i++
			continue
		}
		j := i + 1
		for j < len(want) && pred[j] != want[j] {
			j++
		}
		cols[0] = binary.AppendUvarint(cols[0], uint64(i-prev))
		cols[1] = binary.AppendUvarint(cols[1], uint64(j-i))
		b := 2 + bucketOf(j-i)
		cols[b] = append(cols[b], want[i:j]...)
		prev, i = j, j
	}
	return cols, nil
}

// ApplyColumnar rewrites buf, which holds the prediction, from the streams
// EncodeColumnar wrote. Every offset is checked against buf and every
// stream must be consumed exactly.
func ApplyColumnar(buf []byte, cols [][]byte) error {
	if len(cols) != ColumnarStreams {
		return fmt.Errorf("%w: columnar correction has %d streams", errCorrupt, len(cols))
	}
	gaps, lens := &rbuf{b: cols[0]}, &rbuf{b: cols[1]}
	src := cols[2:]
	at := 0
	for len(gaps.b) > 0 {
		gap, n := gaps.un(uint64(len(buf)-at), "columnar gap"), lens.un(uint64(len(buf)), "columnar run")
		if gaps.err != nil {
			return gaps.err
		}
		if lens.err != nil {
			return lens.err
		}
		at += int(gap)
		b := bucketOf(int(n))
		if n == 0 || n > uint64(len(buf)-at) || n > uint64(len(src[b])) {
			return fmt.Errorf("%w: columnar run of %d at %d", errCorrupt, n, at)
		}
		copy(buf[at:at+int(n)], src[b][:n])
		src[b], at = src[b][n:], at+int(n)
	}
	if len(lens.b) != 0 {
		return fmt.Errorf("%w: trailing columnar lengths", errCorrupt)
	}
	for _, s := range src {
		if len(s) != 0 {
			return fmt.Errorf("%w: trailing columnar bytes", errCorrupt)
		}
	}
	return nil
}
