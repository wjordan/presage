package presage

import (
	"encoding/hex"
	"fmt"

	"github.com/zeebo/blake3"

	"github.com/wjordan/presage/internal/cz"
)

// Magic is the first four bytes of every patch.
const Magic = "PSG1"

// Version is the container version this build writes and the newest it reads.
const Version = 1

// Header flags.
const (
	// FlagDebugZ marks a patch coded on the references and target with
	// their compressed debug sections expanded (delta/debugz.go).
	FlagDebugZ = 1

	// FlagModalCorrection marks a patch whose corrections may name the
	// residual transform per region (delta/modal.go, SPEC 6.1). A build
	// without that decoder rejects the patch by name here and fetches the
	// blob, rather than misreading a stream whose region count carries a
	// shape it does not know.
	FlagModalCorrection = 2

	knownFlags = FlagDebugZ | FlagModalCorrection
)

// FrameSize is the uncompressed size of one body frame and of one
// prediction chunk. Frames are independently decodable so a partial fetch
// resumes at a frame boundary; chunks localise a prediction divergence.
const FrameSize = 8 << 20

// maxPatchSize bounds every size a header may declare.
const maxPatchSize = 1 << 34

// Hash is a BLAKE3-256 digest.
type Hash [32]byte

func (h Hash) String() string { return "b3:" + hex.EncodeToString(h[:]) }

func hashOf(b []byte) Hash { return Hash(blake3.Sum256(b)) }

// Ref describes one reference object the patch is applied against.
type Ref struct {
	B3   Hash
	Size int64
}

// Region is one structural region of the target, in order. Regions tile
// the target; each names the module that predicts it and carries that
// module's plan in the body.
type Region struct {
	Length  int64
	Module  byte
	PlanLen int64
}

// Frame describes one compressed piece of the body.
type Frame struct {
	Off, Len, ZLen int64
	Codec          byte
	B3             Hash
}

// Header is the plaintext prologue of a patch: enough to decide whether the
// patch applies here, how large the result will be, and how to verify each
// frame of it as it arrives.
type Header struct {
	Flags   byte
	Refs    []Ref
	Target  Hash
	Size    int64 // target size as shipped
	Regions []Region
	Frames  []Frame
	BodyOff int64 // offset in the patch at which frame 0 starts
}

// ErrUnsupported means the patch needs something this build lacks: a newer
// container version, a header flag, or a module. The caller should fetch
// the blob instead.
type ErrUnsupported struct{ What string }

func (e *ErrUnsupported) Error() string {
	return "presage: patch needs " + e.What + ", which this build lacks"
}

func marshalHeader(h *Header, body []byte) []byte {
	w := &wbuf{}
	w.raw([]byte(Magic))
	w.raw([]byte{Version, h.Flags})
	f := &wbuf{}
	f.u(uint64(len(h.Refs)))
	for _, r := range h.Refs {
		f.raw(r.B3[:])
		f.u(uint64(r.Size))
	}
	f.raw(h.Target[:])
	f.u(uint64(h.Size))
	f.u(uint64(len(h.Regions)))
	for _, r := range h.Regions {
		f.u(uint64(r.Length))
		f.raw([]byte{r.Module})
		f.u(uint64(r.PlanLen))
	}
	f.u(uint64(len(h.Frames)))
	for _, fr := range h.Frames {
		f.u(uint64(fr.Off))
		f.u(uint64(fr.Len))
		f.u(uint64(fr.ZLen))
		f.raw([]byte{fr.Codec})
		f.raw(fr.B3[:])
	}
	w.bytes(f.b)
	return append(w.b, body...)
}

// ParseHeader reads the prologue of a patch. b may be a prefix of the
// patch: it needs only the magic, the fixed bytes and the header record.
func ParseHeader(b []byte) (*Header, error) {
	if len(b) < 6 || string(b[:4]) != Magic {
		return nil, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if b[4] != Version {
		return nil, &ErrUnsupported{fmt.Sprintf("container version %d", b[4])}
	}
	h := &Header{Flags: b[5]}
	if unknown := h.Flags &^ knownFlags; unknown != 0 {
		return nil, &ErrUnsupported{fmt.Sprintf("header flags %#x", unknown)}
	}
	r := &rbuf{b: b[6:]}
	rec := r.bytesMax(1<<24, "header length")
	if r.err != nil {
		return nil, r.err
	}
	h.BodyOff = int64(len(b) - len(r.b))
	f := &rbuf{b: rec}
	n := f.un(16, "reference count")
	for i := uint64(0); i < n && f.err == nil; i++ {
		var ref Ref
		copy(ref.B3[:], f.take(32))
		ref.Size = int64(f.un(maxPatchSize, "reference size"))
		h.Refs = append(h.Refs, ref)
	}
	copy(h.Target[:], f.take(32))
	h.Size = int64(f.un(maxPatchSize, "target size"))
	n = f.un(1<<20, "region count")
	var total int64
	for i := uint64(0); i < n && f.err == nil; i++ {
		var rg Region
		rg.Length = int64(f.un(maxPatchSize, "region length"))
		rg.Module = f.byte()
		rg.PlanLen = int64(f.un(maxPatchSize, "plan length"))
		total += rg.Length
		h.Regions = append(h.Regions, rg)
	}
	n = f.un(1<<20, "frame count")
	var off int64
	for i := uint64(0); i < n && f.err == nil; i++ {
		var fr Frame
		fr.Off = int64(f.un(maxPatchSize, "frame offset"))
		fr.Len = int64(f.un(FrameSize, "frame length"))
		fr.ZLen = int64(f.un(maxPatchSize, "frame compressed length"))
		fr.Codec = f.byte()
		copy(fr.B3[:], f.take(32))
		if fr.Off != off {
			f.fail("frame %d starts at %d, want %d", i, fr.Off, off)
			break
		}
		off += fr.Len
		h.Frames = append(h.Frames, fr)
	}
	if err := f.done(); err != nil {
		return nil, err
	}
	return h, nil
}

// BodySize is the uncompressed length of the body.
func (h *Header) BodySize() int64 {
	var n int64
	for _, f := range h.Frames {
		n += f.Len
	}
	return n
}

// frameBody cuts the body into frames and compresses each.
func frameBody(body []byte) ([]Frame, []byte) {
	var frames []Frame
	var out []byte
	for off := 0; off < len(body) || (off == 0 && len(frames) == 0); off += FrameSize {
		end := min(off+FrameSize, len(body))
		codec, z := cz.Compress(body[off:end])
		frames = append(frames, Frame{Off: int64(off), Len: int64(end - off), ZLen: int64(len(z)), Codec: codec, B3: hashOf(z)})
		out = append(out, z...)
		if end == len(body) {
			break
		}
	}
	return frames, out
}

// readBody verifies and decompresses every frame.
func readBody(h *Header, patch []byte) ([]byte, error) {
	body := make([]byte, 0, h.BodySize())
	pos := h.BodyOff
	for i, f := range h.Frames {
		if f.ZLen > int64(len(patch))-pos {
			return nil, fmt.Errorf("%w: frame %d runs past the end of the patch", ErrCorrupt, i)
		}
		z := patch[pos : pos+f.ZLen]
		pos += f.ZLen
		if hashOf(z) != f.B3 {
			return nil, fmt.Errorf("%w: frame %d hash mismatch", ErrCorrupt, i)
		}
		raw, err := cz.Decompress(f.Codec, z, int(f.Len))
		if err != nil {
			return nil, fmt.Errorf("%w: frame %d: %v", ErrCorrupt, i, err)
		}
		body = append(body, raw...)
	}
	if pos != int64(len(patch)) {
		return nil, fmt.Errorf("%w: %d bytes after the last frame", ErrCorrupt, int64(len(patch))-pos)
	}
	return body, nil
}
