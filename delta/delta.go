// Package delta is the Go linux/amd64 predictor, which presage drives as its
// `go` module (docs/go-module-design.md).
//
// A patch turns the bytes of one binary into the bytes of another. Two
// transforms share one container (docs/go-module-design.md 2.5): a Go-aware
// predict-then-correct codec for stripped Go binaries, and a bsdiff-class
// codec for everything else. Both are verified by hash before their output
// is used, so a mispredicting encoder costs bytes, never correctness.
package delta

import (
	"fmt"
	"io"
	"runtime"
)

// Options controls Encode. The zero value encodes for a decoder of this
// same build.
type Options struct {
	// MaxTransform caps the wire format at what a decoder older than this
	// build can read (docs/go-module-design.md 2.6). Zero means "no limit":
	// use the best available.
	MaxTransform int

	// PlainOnly skips the Go-aware codec even for a supported binary. It
	// exists for the self-check corpus and for a plain-only encode.
	PlainOnly bool

	// Workers bounds the encoder's parallelism. Zero means GOMAXPROCS.
	// It does not affect the bytes produced.
	Workers int

	// Stats, if non-nil, receives a breakdown of where the patch bytes went.
	Stats *Stats
}

// Stats is an accounting of one encode, for the benchmark harness and for
// `presage diff -v`.
//
// The per-stream sizes are the streams as the codec produced them, before
// compression: the body is compressed as one piece, because the streams
// share enough context that splitting them costs more than it saves.
type Stats struct {
	Transform byte

	Layout  int
	Stage1a int
	Stage1b int
	Stage2  int
	Body    int // the four together, plus the fixed fields between them
	Header  int // the compressed patch, less the body
	Total   int // the compressed patch

	Funcs      int
	Matched    int
	NewFuncs   int
	PredictErr int // bytes where the prediction differed from the new file
	Notes      []string
}

func (o Options) workers() int {
	if o.Workers > 0 {
		return o.Workers
	}
	return runtime.GOMAXPROCS(0)
}

func (o Options) transformCap() int {
	if o.MaxTransform <= 0 {
		return maxTransform
	}
	return min(o.MaxTransform, maxTransform)
}

// Encode produces a patch turning old into new.
//
// It tries the Go-aware transform when both binaries qualify and
// MaxTransform allows it, and falls back to the plain codec — never to an
// error — when they do not. The returned patch always reproduces new
// exactly; Apply verifies that before it hands the bytes over.
func Encode(old, new []byte, o Options) ([]byte, error) {
	h := &Header{
		From:    hashOf(old),
		To:      hashOf(new),
		OldSize: int64(len(old)),
		NewSize: int64(len(new)),
	}
	st := o.Stats
	if st == nil {
		st = &Stats{}
	}
	if pOld, pNew, ok := expandPair(old, new); ok {
		// Code the expanded files; the decoder recompresses (debugz.go).
		h.Flags |= FlagDebugZ
		old, new = pOld, pNew
		st.Notes = append(st.Notes, fmt.Sprintf("compressed debug sections expanded: %d -> %d bytes", h.NewSize, len(new)))
	}
	var body []byte
	var err error
	if !o.PlainOnly && o.transformCap() >= TransformGoAMD64 {
		tf := byte(o.transformCap())
		body, err = encodeGoAMD64(old, new, tf, o, st)
		if err == nil {
			h.Transform = tf
		} else if !isUnsupported(err) {
			return nil, err
		} else {
			st.Notes = append(st.Notes, "plain codec: "+err.Error())
		}
	}
	if body == nil {
		if body, err = encodePlain(old, new, o, st); err != nil {
			return nil, err
		}
		h.Transform = TransformPlain
	}
	if h.Flags&FlagDebugZ != 0 {
		w := &wbuf{}
		w.u(uint64(len(new)))
		body = append(w.b, body...)
	}
	st.Transform = h.Transform
	st.Body = len(body)
	h.Frames, body = frameBody(body)
	patch := marshalHeader(h, body)
	st.Header = len(patch) - len(body)
	st.Total = len(patch)
	return patch, nil
}

// Apply reconstructs the new binary from old and patch and writes it to w.
//
// The patch is untrusted input: every offset and length in it is checked,
// the predicted base is compared with the hash the encoder recorded, and
// the output is compared with the release hash before a byte reaches w.
func Apply(old, patch []byte, w io.Writer) error {
	h, err := ParseHeader(patch)
	if err != nil {
		return err
	}
	if int64(len(old)) != h.OldSize {
		return fmt.Errorf("delta: old file is %d bytes, patch expects %d", len(old), h.OldSize)
	}
	if got := hashOf(old); got != h.From {
		return fmt.Errorf("delta: old file hashes to %s, patch expects %s", got, h.From)
	}
	body, err := readBody(h, patch)
	if err != nil {
		return err
	}
	shipped := old
	inner := *h // the codec sees the expanded sizes
	if h.Flags&FlagDebugZ != 0 {
		r := &rbuf{b: body}
		inner.NewSize = int64(r.un(maxPatchSize, "expanded size"))
		if r.err != nil {
			return r.err
		}
		body = r.b
		if old, err = ExpandDebug(old); err != nil {
			return fmt.Errorf("delta: expanding the old file's debug sections: %w", err)
		}
		inner.OldSize = int64(len(old))
	}
	var out []byte
	switch h.Transform {
	case TransformPlain:
		out, err = applyPlain(old, body, &inner)
	case TransformGoAMD64, TransformGoSegmap, TransformGoFar:
		out, err = applyGoAMD64(old, body, &inner)
	default:
		return &ErrUnsupportedTransform{Transform: h.Transform}
	}
	if err != nil {
		return err
	}
	if h.Flags&FlagDebugZ != 0 {
		if int64(len(out)) != inner.NewSize {
			return fmt.Errorf("%w: expanded output is %d bytes, body says %d", errCorrupt, len(out), inner.NewSize)
		}
		if out, err = PackDebug(out, shipped); err != nil {
			return fmt.Errorf("%w: recompressing debug sections: %v", errCorrupt, err)
		}
	}
	if int64(len(out)) != h.NewSize {
		return fmt.Errorf("%w: output is %d bytes, header says %d", errCorrupt, len(out), h.NewSize)
	}
	if got := hashOf(out); got != h.To {
		return fmt.Errorf("delta: output hashes to %s, patch promises %s", got, h.To)
	}
	_, err = w.Write(out)
	return err
}

// unsupportedError marks an input the Go-aware codec declines. It is never a
// failure: the encoder falls back to the plain codec, and the decoder reports
// a patch it cannot read as unsupported.
type unsupportedError struct{ msg string }

func (e *unsupportedError) Error() string { return e.msg }

func unsupported(f string, a ...any) error { return &unsupportedError{fmt.Sprintf(f, a...)} }

func isUnsupported(err error) bool {
	var u *unsupportedError
	return errorAs(err, &u)
}
