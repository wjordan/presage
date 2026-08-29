package presage

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrCorrupt is the base of every "this patch is not well-formed" error.
// Nothing derived from patch bytes is trusted: every length is checked
// against what remains and against the declared sizes before it becomes an
// allocation or an index.
var ErrCorrupt = errors.New("presage: corrupt patch")

type wbuf struct{ b []byte }

func (w *wbuf) u(v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	w.b = append(w.b, tmp[:binary.PutUvarint(tmp[:], v)]...)
}

func (w *wbuf) bytes(b []byte) { w.u(uint64(len(b))); w.b = append(w.b, b...) }
func (w *wbuf) raw(b []byte)   { w.b = append(w.b, b...) }

type rbuf struct {
	b   []byte
	err error
}

func (r *rbuf) fail(f string, a ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("%w: "+f, append([]any{ErrCorrupt}, a...)...)
	}
	r.b = nil
}

func (r *rbuf) u() uint64 {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.fail("bad uvarint")
		return 0
	}
	r.b = r.b[n:]
	return v
}

// un reads a uvarint and refuses a value above max.
func (r *rbuf) un(max uint64, what string) uint64 {
	v := r.u()
	if v > max {
		r.fail("%s is %d, at most %d expected", what, v, max)
		return 0
	}
	return v
}

func (r *rbuf) byte() byte {
	if len(r.b) == 0 {
		r.fail("truncated")
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *rbuf) take(n uint64) []byte {
	if uint64(len(r.b)) < n {
		r.fail("%d bytes wanted, %d left", n, len(r.b))
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

// bytesMax reads a length-prefixed field whose length may not exceed max.
func (r *rbuf) bytesMax(max uint64, what string) []byte { return r.take(r.un(max, what)) }

func (r *rbuf) done() error {
	if r.err != nil {
		return r.err
	}
	if len(r.b) != 0 {
		return fmt.Errorf("%w: %d trailing bytes", ErrCorrupt, len(r.b))
	}
	return nil
}
