package delta

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"runtime"
	"slices"
	"sort"
	"sync"

	"github.com/wjordan/presage/delta/x86"
)

// The displacement column (research/pgo-churn.md §5.1b, chrome-elf-whole-image
// §14).
//
// The columnar correction's last bucket holds the replacement bytes for every
// wrong run of five bytes or more, which is where newly emitted instructions
// land. A module's field layer cannot reach the PC-relative fields inside
// them: it names fields by walking the *prediction*, and where the prediction
// holds a different instruction there is no field to name. So they ship as
// literal bytes, and a call target's low three bytes are noise the compressor
// pays for in full.
//
// Both sides can walk instead. Zeroing a displacement never changes an
// instruction's length, so after the replacement bytes are placed the decoder
// decodes exactly the stream the encoder did, re-derives each field's
// position, and fills the value back in from a column of its own.
//
// The gate is x86.WalkReferences, so this walk sees exactly the fields the
// relocator sees, VEX/EVEX RIP-relative included. A private re-implementation
// would silently disagree with the codec about which bytes are displacements.

// DispBody is one function body to walk, in the coordinate space of the
// buffer the correction is applied to.
type DispBody struct {
	Off, Size int
	PC        uint64 // the body's virtual address
}

// DispContext is what both sides need to agree on the field set: where the
// function bodies are, and the address domain an image-spanning target is
// indexed into. The domain is every new function's start address — §14
// measured the alternative populations and this is the one that pays.
type DispContext struct {
	bodies []DispBody
	starts []uint64 // sorted, unique
	// A restricted context views bodies in their original coordinates and
	// translates them by base. limit is the exclusive original-coordinate
	// boundary; zero marks an unrestricted context.
	base, limit int
}

// NewDispContext returns the context for a set of function bodies. It sorts
// and de-duplicates both inputs, so a caller may pass them in any order.
func NewDispContext(bodies []DispBody, starts []uint64) *DispContext {
	return newDispContext(slices.Clone(bodies), slices.Clone(starts))
}

// NewDispContextOwned is NewDispContext for freshly built slices whose
// ownership the caller transfers to the context. The context sorts and
// retains both slices; the caller must not use them again.
func NewDispContextOwned(bodies []DispBody, starts []uint64) *DispContext {
	return newDispContext(bodies, starts)
}

func newDispContext(bodies []DispBody, starts []uint64) *DispContext {
	d := &DispContext{bodies: bodies, starts: starts}
	slices.SortFunc(d.bodies, func(a, b DispBody) int { return cmp.Compare(a.Off, b.Off) })
	slices.Sort(d.starts)
	d.starts = slices.Compact(d.starts)
	return d
}

// Restrict re-expresses the context in the coordinates of the [lo, hi) slice
// a piecewise correction cuts the region into. A body that straddles a cut
// boundary is dropped: both sides drop it, so the field sets still agree.
func (d *DispContext) Restrict(lo, hi int) *DispContext {
	if d == nil {
		return nil
	}
	absLo, absHi := d.base+lo, d.base+hi
	if d.limit != 0 {
		absHi = min(absHi, d.limit)
	}
	// Bodies stay in their owner. sites and classify translate offsets while
	// walking the view and drop the rare body that straddles its upper cut.
	i := sort.Search(len(d.bodies), func(i int) bool { return d.bodies[i].Off >= absLo })
	j := sort.Search(len(d.bodies), func(i int) bool { return d.bodies[i].Off >= absHi })
	return &DispContext{starts: d.starts, bodies: d.bodies[i:j], base: absLo, limit: absHi}
}

func (d *DispContext) localBody(b DispBody) (DispBody, bool) {
	if b.Off < d.base || (d.limit != 0 && b.Off+b.Size > d.limit) {
		return DispBody{}, false
	}
	b.Off -= d.base
	return b, true
}

// dispRun is one long-bucket run: [start, end) in buffer coordinates, and
// where its bytes begin inside the concatenated last bucket.
type dispRun struct{ start, end, at int }

// dispSite is one PC-relative field that lies wholly inside a long run.
type dispSite struct {
	off, n int    // buffer coordinates
	next   uint64 // address of the following instruction: target = next + disp
	lo, hi uint64 // the enclosing function's address range
	at     int    // position inside the concatenated last bucket
}

// sites finds every PC-relative field that lies wholly inside one of runs.
// The encoder calls it on the target and the decoder on the repaired buffer,
// which differ only in the field bytes themselves — and a displacement byte is
// never read to decide an instruction's length — so the two walks return the
// same list.
func (d *DispContext) sites(buf []byte, runs []dispRun) []dispSite {
	if d == nil || len(runs) == 0 {
		return nil
	}
	// The bodies are disjoint spans of buf and each is walked from its own
	// start, so they walk concurrently; the shards are concatenated in body
	// order, which is the list a serial walk built.
	shards := shardBodies(len(d.bodies), func(lo, hi int) []dispSite {
		var out []dispSite
		for _, b := range d.bodies[lo:hi] {
			var ok bool
			if b, ok = d.localBody(b); !ok {
				continue
			}
			if b.Off < 0 || b.Size < 0 || b.Off+b.Size > len(buf) {
				continue
			}
			// Skip a body no run touches, which is most of them.
			k, _ := slices.BinarySearchFunc(runs, b.Off, func(r dispRun, v int) int { return cmp.Compare(r.end, v) })
			if k >= len(runs) || runs[k].start >= b.Off+b.Size {
				continue
			}
			lo, hi := b.PC, b.PC+uint64(b.Size)
			x86.WalkReferences(buf[b.Off:b.Off+b.Size], b.PC, func(ref x86.Reference) {
				fo, fe := b.Off+ref.Off, b.Off+ref.Off+ref.N
				i, _ := slices.BinarySearchFunc(runs, fo, func(r dispRun, v int) int { return cmp.Compare(r.end, v) })
				if i >= len(runs) || runs[i].start > fo || fe > runs[i].end {
					return
				}
				out = append(out, dispSite{fo, ref.N, b.PC + uint64(ref.Next), lo, hi, runs[i].at + fo - runs[i].start})
			})
		}
		return out
	})
	n := 0
	for _, s := range shards {
		n += len(s)
	}
	out := make([]dispSite, 0, n)
	for _, s := range shards {
		out = append(out, s...)
	}
	return out
}

// shardBodies splits [0,n) into one contiguous range per worker and collects
// what f makes of each, in range order.
func shardBodies[T any](n int, f func(lo, hi int) T) []T {
	nsh := min(max(n, 1), runtime.GOMAXPROCS(0))
	out := make([]T, nsh)
	var wg sync.WaitGroup
	for s := range nsh {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			out[s] = f(s*n/nsh, (s+1)*n/nsh)
		}(s)
	}
	wg.Wait()
	return out
}

// ---------------------------------------------------------------------------
// per-byte field context for the CM coder (cmside.go)

// Byte classes for the CM coder's field context. A correction byte is
// classified by where it sits in the instruction the *prediction* holds at
// that position — not the target's, which the decoder does not have yet.
// Where the two agree on the instruction (they usually do: a recompile moves
// a call target, not the opcode around it) this says whether the byte is a
// displacement byte and which one, which is exactly the split a whole-byte
// context cannot make.
const (
	fieldNone  = iota // no instruction the length decoder accepted covers it
	fieldPlain        // an instruction with no PC-relative field
	fieldLead         // before the field: prefixes, opcode, modrm, sib
	fieldByte0        // the field's low byte, then its next three
	fieldByte1
	fieldByte2
	fieldByte3
	fieldTail // after the field: a trailing immediate
)

// cmOffMax caps the offset-within-instruction context. x86 instructions run
// to 15 bytes, so nothing is actually clamped; the constant is here so the
// context's width is stated where the hash in cmcoder.go relies on it.
const cmOffMax = 15

// insnClass places byte r of in.
func insnClass(in x86.Insn, r int) byte {
	if in.N == 0 {
		return fieldPlain
	}
	switch fo := in.Off - in.Start; {
	case r < fo:
		return fieldLead
	case r < fo+in.N:
		return byte(fieldByte0 + min(r-fo, 3))
	default:
		return fieldTail
	}
}

// classify stamps a field class and an offset-within-instruction onto every
// byte of buf that lies inside one of runs, by walking only the bodies those
// runs touch — the same skip-most-bodies test sites uses, and most bodies are
// untouched. buf is the prediction on both sides, so both derive the same
// stamps; a byte no body covers keeps fieldNone.
//
// stamp is called with the index of the run the byte falls in and its position
// in buf. runs must be sorted by start and non-overlapping.
func (d *DispContext) classify(buf []byte, runs []dispRun, stamp func(run, pos int, cls, off byte)) {
	if d == nil || len(runs) == 0 {
		return
	}
	// Bodies are disjoint spans of buf, so every byte position each one stamps
	// belongs to it alone: the walks run concurrently and no two of them write
	// the same byte of the context. The run cursor k is per body already.
	shardBodies(len(d.bodies), func(lo, hi int) struct{} {
		for _, b := range d.bodies[lo:hi] {
			var ok bool
			if b, ok = d.localBody(b); !ok {
				continue
			}
			if b.Off < 0 || b.Size < 0 || b.Off+b.Size > len(buf) {
				continue
			}
			k, _ := slices.BinarySearchFunc(runs, b.Off, func(r dispRun, v int) int { return cmp.Compare(r.end, v) })
			if k >= len(runs) || runs[k].start >= b.Off+b.Size {
				continue
			}
			x86.WalkInsns(buf[b.Off:b.Off+b.Size], func(in x86.Insn) {
				s, e := b.Off+in.Start, b.Off+in.Start+in.Length
				for k < len(runs) && runs[k].end <= s {
					k++
				}
				for j := k; j < len(runs) && runs[j].start < e; j++ {
					for p := max(s, runs[j].start); p < min(e, runs[j].end); p++ {
						stamp(j, p, insnClass(in, p-s), byte(min(p-s, cmOffMax)))
					}
				}
			})
		}
		return struct{}{}
	})
}

// Field classes. §14: only 13.1 % of these sites are genuinely image-spanning
// calls to a known function start; 79.4 % are jumps inside their own function,
// which want a byte basis instead. The rest escape to an absolute address.
const (
	dispHit   = iota // target is a new function start: ship its index
	dispLocal        // target is inside the same function: ship the displacement
	dispFar          // anything else: ship the absolute address
)

func (d *DispContext) class(s dispSite, abs uint64) int {
	if _, ok := slices.BinarySearch(d.starts, abs); ok {
		return dispHit
	}
	if abs >= s.lo && abs < s.hi {
		return dispLocal
	}
	return dispFar
}

// readDisp and writeDisp move an n-byte little-endian signed field.
func readDisp(b []byte) int64 {
	var v int64
	for k := len(b) - 1; k >= 0; k-- {
		v = v<<8 | int64(b[k])
	}
	if shift := 64 - 8*len(b); shift > 0 {
		v = v << shift >> shift
	}
	return v
}

func writeDisp(b []byte, v int64) {
	for k := range b {
		b[k] = byte(v)
		v >>= 8
	}
}

// DispStreams is how many extra streams the displacement columns add:
// a class tag per field, then the three value columns.
const DispStreams = 4

// EncodeColumnarDisp is EncodeColumnar plus the displacement columns: every
// PC-relative field lying wholly inside a long run is zeroed in the
// replacement bytes and shipped in a column of its own. It returns
// ColumnarStreams streams when d is nil or finds no field — the shipped
// format's price stays exactly what it was — and ColumnarStreams+DispStreams
// otherwise.
func EncodeColumnarDisp(pred, want []byte, d *DispContext) ([][]byte, error) {
	if len(pred) != len(want) {
		return nil, fmt.Errorf("delta: prediction is %d bytes, target is %d", len(pred), len(want))
	}
	cols := make([][]byte, ColumnarStreams)
	var runs []dispRun
	last := 2 + ColumnarBuckets - 1
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
		if b == last && d != nil {
			runs = append(runs, dispRun{i, j, len(cols[last])})
		}
		cols[b] = append(cols[b], want[i:j]...)
		prev, i = j, j
	}
	sites := d.sites(want, runs)
	if len(sites) == 0 {
		return cols, nil
	}
	tags, idx, loc, far := []byte(nil), []byte(nil), []byte(nil), []byte(nil)
	var prevIdx, prevFar int64
	for _, s := range sites {
		v := readDisp(want[s.off : s.off+s.n])
		abs := uint64(int64(s.next) + v)
		cl := d.class(s, abs)
		tags = append(tags, byte(cl))
		switch cl {
		case dispHit:
			i, _ := slices.BinarySearch(d.starts, abs)
			idx = binary.AppendVarint(idx, int64(i)-prevIdx)
			prevIdx = int64(i)
		case dispLocal:
			loc = binary.AppendVarint(loc, v)
		default:
			far = binary.AppendVarint(far, int64(abs)-prevFar)
			prevFar = int64(abs)
		}
		clear(cols[last][s.at : s.at+s.n])
	}
	return append(cols, tags, idx, loc, far), nil
}

// ApplyColumnarDisp places the replacement bytes and then, when the
// correction carries displacement columns, walks the repaired buffer and
// fills the zeroed fields back in. The walk happens after every run is
// placed, because an instruction may span a run boundary and only then are
// all its bytes present.
func ApplyColumnarDisp(buf []byte, cols [][]byte, d *DispContext) error {
	if len(cols) != ColumnarStreams+DispStreams {
		return fmt.Errorf("%w: displacement correction has %d streams", errCorrupt, len(cols))
	}
	if d == nil {
		return fmt.Errorf("%w: displacement correction with no field context", errCorrupt)
	}
	base, tags := cols[:ColumnarStreams], cols[ColumnarStreams]
	idx := &rbuf{b: cols[ColumnarStreams+1]}
	loc := &rbuf{b: cols[ColumnarStreams+2]}
	far := &rbuf{b: cols[ColumnarStreams+3]}

	gaps, lens := &rbuf{b: base[0]}, &rbuf{b: base[1]}
	src := append([][]byte(nil), base[2:]...)
	last := ColumnarBuckets - 1
	var runs []dispRun
	at, placed := 0, 0
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
		if b == last {
			runs = append(runs, dispRun{at, at + int(n), placed})
			placed += int(n)
		}
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

	var prevIdx, prevFar int64
	for _, s := range d.sites(buf, runs) {
		if len(tags) == 0 {
			return fmt.Errorf("%w: correction ran out of displacement tags", errCorrupt)
		}
		cl := int(tags[0])
		tags = tags[1:]
		var v int64
		switch cl {
		case dispHit:
			prevIdx += idx.s()
			if idx.err != nil {
				return idx.err
			}
			if prevIdx < 0 || prevIdx >= int64(len(d.starts)) {
				return fmt.Errorf("%w: displacement index outside the function-start domain", errCorrupt)
			}
			v = int64(d.starts[prevIdx]) - int64(s.next)
		case dispLocal:
			v = loc.s()
		case dispFar:
			prevFar += far.s()
			v = prevFar - int64(s.next)
		default:
			return fmt.Errorf("%w: invalid displacement class %d", errCorrupt, cl)
		}
		if loc.err != nil {
			return loc.err
		}
		if far.err != nil {
			return far.err
		}
		writeDisp(buf[s.off:s.off+s.n], v)
	}
	if len(tags) != 0 || len(idx.b) != 0 || len(loc.b) != 0 || len(far.b) != 0 {
		return fmt.Errorf("%w: trailing displacement column data", errCorrupt)
	}
	return nil
}
