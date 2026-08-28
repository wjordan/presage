package main

import (
	"bytes"
	"testing"
)

// TestColumnarCorrectionRoundTrip covers the edit shapes the whole-image
// residual is made of: a run at offset zero, single wrong bytes, adjacent
// runs separated by one correct byte, and a run that ends the buffer. It also
// pins the bucketing, since a run's bytes are only findable if both sides
// agree which bucket its length puts it in.
func TestColumnarCorrectionRoundTrip(t *testing.T) {
	pred := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	target := []byte{9, 9, 3, 4, 0, 6, 1, 1, 1, 0}
	c, err := encodeColumnar(pred, target)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := append([]byte(nil), pred...)
	if err := c.apply(got); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("apply produced %v, want %v", got, target)
	}
	// The three runs are two, one and four bytes long, so each lands in its
	// own bucket and the two unused buckets stay empty.
	want := [correctionBuckets]int{1, 2, 0, 4, 0}
	for i, b := range c.Bytes {
		if len(b) != want[i] {
			t.Errorf("bucket %d holds %d bytes, want %d", i+1, len(b), want[i])
		}
	}
	// An identical pair must produce no edits at all.
	empty, err := encodeColumnar(pred, pred)
	if err != nil {
		t.Fatalf("encode identical: %v", err)
	}
	total := 0
	for _, b := range empty.Bytes {
		total += len(b)
	}
	if len(empty.Gaps) != 0 || len(empty.Lens) != 0 || total != 0 {
		t.Errorf("identical inputs produced %d/%d/%d bytes of edits", len(empty.Gaps), len(empty.Lens), total)
	}
}
