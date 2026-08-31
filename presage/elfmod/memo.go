package elfmod

import "unsafe"

// The apply path memoises three pure functions of the old image and the plan:
// the reference-target domain, the derived enumeration, and the parsed
// structural plans. All three were keyed by a BLAKE3 hash of their inputs,
// which meant hashing 217 MB of .text and 291 MB of file on every apply --
// a quarter of a second spent building keys for a cache the decoder,
// predicting once, was never going to hit.
//
// bufID keys them on buffer identity instead. It is sound because every entry
// retains the buffers its key names: while the entry lives that memory cannot
// be freed, so no other live slice can share its address and length, and a
// matching bufID is therefore the same bytes -- provided those bytes do not
// change, which holds for the reference image and for the unpacked plan, both
// of which are read-only for the life of a call.
type bufID struct {
	p *byte
	n int
}

func idOf(b []byte) bufID {
	if len(b) == 0 {
		return bufID{}
	}
	return bufID{p: unsafe.SliceData(b), n: len(b)}
}
