package delta

import (
	"bytes"
	"testing"
)

func TestColumnarRoundTrip(t *testing.T) {
	pred := make([]byte, 4096)
	for i := range pred {
		pred[i] = byte(i)
	}
	want := append([]byte(nil), pred...)
	for i := 0; i < len(want); i += 61 {
		for k := 0; k < 1+i%7; k++ {
			want[i+k] ^= 0x80
		}
	}
	want[len(want)-1] ^= 1
	cols, err := EncodeColumnar(pred, want)
	if err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), pred...)
	if err := ApplyColumnar(out, cols); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Fatal("columnar correction did not reproduce the target")
	}
	// Short and inconsistent streams are refused.
	bad := append([][]byte(nil), cols...)
	bad[1] = bad[1][:len(bad[1])-1]
	if err := ApplyColumnar(append([]byte(nil), pred...), bad); err == nil {
		t.Fatal("truncated length column accepted")
	}
	if err := ApplyColumnar(pred, cols[:3]); err == nil {
		t.Fatal("wrong stream count accepted")
	}
}
