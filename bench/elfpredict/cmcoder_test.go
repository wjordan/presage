package main

import (
	"bytes"
	"math/rand"
	"testing"
)

// The probe's numbers are only real coded sizes if the coder is a coder, so the
// arithmetic coder and the model loop are round-tripped on data with both
// structure and noise.
func TestCMCoderRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	src := make([]byte, 40000)
	for i := range src {
		switch {
		case i%7 == 0:
			src[i] = byte(i)
		case i%3 == 0:
			src[i] = 0
		default:
			src[i] = byte(r.Intn(256))
		}
	}
	for order := 0; order <= 3; order++ {
		coded := cmEncode(src, &plainCtx{order})
		back := cmDecode(coded, len(src), &plainCtx{order})
		if !bytes.Equal(back, src) {
			t.Fatalf("order %d: round trip mismatch", order)
		}
		if len(coded) >= len(src) {
			t.Errorf("order %d: coded %d >= raw %d", order, len(coded), len(src))
		}
	}
}
