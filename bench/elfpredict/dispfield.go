package main

import (
	"cmp"
	"fmt"
	"os"
	"slices"

	"github.com/wjordan/presage/delta/x86"
)

// The displacement column, §14 of chrome-elf-whole-image.md.
//
// The columnar correction's last bucket holds the replacement bytes for every
// wrong run of five bytes or more, which is where newly emitted instructions
// land. §9.15's field layer cannot reach the PC-relative fields inside them:
// it names fields by walking the *prediction*, and where the prediction holds
// a different instruction there is no field to name. So they ship as literal
// bytes, and a call target's low three bytes are noise the compressor pays
// for in full.
//
// Both sides can walk instead. Zeroing a displacement never changes an
// instruction's length, so after the correction is applied the decoder decodes
// exactly the stream the encoder did, re-derives each field's position, and
// fills the value back in from a column of its own. The decoder already holds
// everything the walk needs -- the function map is decoded before any
// correction is applied, and the run boundaries are the Gaps/Lens columns it
// is reading anyway.
//
// The gate is x86.WalkReferences, so this walk sees exactly the fields
// delta/x86's pcrelField sees, VEX/EVEX RIP-relative included (§17). A
// private re-implementation keyed off x86asm.PCRel -- which is what the §14
// probe used -- would silently disagree with the codec about which bytes are
// displacements.

// dispBody is one function body to walk, in the coordinate space of the buffer
// the correction is being applied to.
type dispBody struct {
	off, size int
	pc        uint64
}

// dispContext is what both sides need to agree on the field set: where the
// function bodies are, and the address domain an image-spanning target is
// indexed into. The domain is every new function's start address, straight out
// of the function map -- §14 measured the alternative populations and this is
// the one that pays.
type dispContext struct {
	bodies []dispBody
	starts []uint64 // sorted, unique
}

// newDispContext builds the context from the dense function map, in whole-image
// coordinates.
func newDispContext(maps []mapping, text section, imageLen int) *dispContext {
	d := &dispContext{}
	for _, m := range maps {
		if m.DstSize == 0 || m.Dst+m.DstSize > text.Size {
			continue
		}
		off := int(text.Off + m.Dst)
		if off+int(m.DstSize) > imageLen {
			continue
		}
		d.bodies = append(d.bodies, dispBody{off, int(m.DstSize), text.Addr + m.Dst})
		d.starts = append(d.starts, text.Addr+m.Dst)
	}
	slices.SortFunc(d.bodies, func(a, b dispBody) int { return cmp.Compare(a.off, b.off) })
	slices.Sort(d.starts)
	d.starts = slices.Compact(d.starts)
	return d
}

// restrict re-expresses the context in the coordinates of the [lo, hi) slice
// bestCorrectionXZ cuts the image into. A body that straddles a cut boundary is
// dropped: both sides drop it, so the field sets still agree.
func (d *dispContext) restrict(lo, hi int) *dispContext {
	if d == nil {
		return nil
	}
	out := &dispContext{starts: d.starts}
	for _, b := range d.bodies {
		if b.off >= lo && b.off+b.size <= hi {
			out.bodies = append(out.bodies, dispBody{b.off - lo, b.size, b.pc})
		}
	}
	return out
}

// dispRun is one long-bucket run: [start, end) in buffer coordinates, and where
// its bytes begin inside the concatenated last bucket.
type dispRun struct {
	start, end, at int
}

// dispSite is one PC-relative field that lies wholly inside a long run.
type dispSite struct {
	off, n int    // buffer coordinates
	next   uint64 // address of the following instruction: target = next + disp
	lo, hi uint64 // the enclosing function's address range
	at     int    // position inside the concatenated last bucket
}

// sites finds every PC-relative field that lies wholly inside one of runs. The
// encoder calls it on the target and the decoder on the repaired buffer, which
// differ only in the field bytes themselves -- and a displacement byte is never
// read to decide an instruction's length -- so the two walks return the same
// list.
func (d *dispContext) sites(buf []byte, runs []dispRun) []dispSite {
	if d == nil || len(runs) == 0 {
		return nil
	}
	var out []dispSite
	for _, b := range d.bodies {
		if b.off+b.size > len(buf) {
			continue
		}
		// Skip a body no run touches, which is most of them.
		k, _ := slices.BinarySearchFunc(runs, b.off, func(r dispRun, v int) int { return cmp.Compare(r.end, v) })
		if k >= len(runs) || runs[k].start >= b.off+b.size {
			continue
		}
		lo, hi := b.pc, b.pc+uint64(b.size)
		x86.WalkReferences(buf[b.off:b.off+b.size], b.pc, func(ref x86.Reference) {
			fo, fe := b.off+ref.Off, b.off+ref.Off+ref.N
			i, _ := slices.BinarySearchFunc(runs, fo, func(r dispRun, v int) int { return cmp.Compare(r.end, v) })
			if i >= len(runs) || runs[i].start > fo || fe > runs[i].end {
				return
			}
			out = append(out, dispSite{fo, ref.N, b.pc + uint64(ref.Next), lo, hi, runs[i].at + fo - runs[i].start})
		})
	}
	return out
}

// Field classes. §14: only 13.1% of these sites are genuinely image-spanning
// calls to a known function start; 79.4% are jumps inside their own function,
// which §9.17 says want a byte basis instead. The remaining 7.4% escape to an
// absolute address.
const (
	dispHit   = iota // target is a new function start: ship its index
	dispLocal        // target is inside the same function: ship the displacement
	dispFar          // anything else: ship the absolute address
)

func (d *dispContext) class(s dispSite, abs uint64) int {
	if _, ok := slices.BinarySearch(d.starts, abs); ok {
		return dispHit
	}
	if abs >= s.lo && abs < s.hi {
		return dispLocal
	}
	return dispFar
}

// reportDispColumns prints what the variant cost and what it saved, split the
// way §14 predicted it: an index column for the image-spanning calls, an
// escape column for everything else that leaves the function, and the local
// jumps on a byte basis.
func reportDispColumns(c columnarCorrection) {
	if len(c.Tags) == 0 {
		return
	}
	var hits, locals, fars int
	for _, t := range c.Tags {
		switch int(t) {
		case dispHit:
			hits++
		case dispLocal:
			locals++
		default:
			fars++
		}
	}
	n := float64(len(c.Tags))
	z := xzSizes(c.Tags, c.Idx, c.Loc, c.Far)
	fmt.Fprintf(os.Stderr, "  dispcol: %d fields pulled out (%d function-start calls %.1f%%, %d local %.1f%%, %d elsewhere %.1f%%); columns xz tag %d + idx %d + local %d + far %d = %d\n",
		len(c.Tags), hits, 100*float64(hits)/n, locals, 100*float64(locals)/n, fars, 100*float64(fars)/n,
		z[0], z[1], z[2], z[3], z[0]+z[1]+z[2]+z[3])
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
