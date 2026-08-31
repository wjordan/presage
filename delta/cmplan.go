package delta

// Plan-side contexts for the context-mixing coder.
//
// The correction's bytes are conditioned on the prediction underneath them
// (cmside.go). A plan's bytes have no prediction underneath: almost every one
// of them is a byte of a LEB128 varint in a column of varints, and what says
// the most about it is where in the varint it sits and how large the previous
// value in the column was. Both are a running parse of the bytes already
// coded, so the decoder has them at every point without being told anything.
//
// A value column additionally has its index column, which ships before it and
// carries one entry per the same record: when record i of a value column is
// coded the decoder already holds record i of the index column, and an index
// record is a gap -- how far apart two edits are -- which is exactly what a
// value's magnitude tracks. That is the context a general compressor
// structurally cannot have: it sees one flat byte stream and no notion that
// byte 90,000 of this column and byte 40,000 of that one describe one edit.

// varintState is the running parse of a LEB128 column, maintained from bytes
// already coded and so available to the decoder at every position. Nothing
// here reads the byte being coded.
type varintState struct {
	k           uint32 // bytes of the current varint already done
	v           uint64 // accumulator for the varint in progress
	prev, prev2 uint64 // the last two complete values
	prev3       uint64
	idx         uint32 // complete varints so far
}

func (s *varintState) consume(c byte) {
	s.v |= uint64(c&0x7F) << (7 * min(s.k, 9))
	if c&0x80 != 0 {
		if s.k < 9 {
			s.k++
		}
		return
	}
	s.prev3, s.prev2, s.prev = s.prev2, s.prev, s.v
	s.v, s.k = 0, 0
	s.idx++
}

// cmBucket compresses a value to its magnitude class.
func cmBucket(v uint64) uint32 {
	var n uint32
	for v > 0 {
		n++
		v >>= 1
	}
	return n
}

// CMParseVarints decodes a whole varint column, so a value column can be coded
// against its index column. The decoder can do exactly this, because the
// column it parses has already been decoded.
func CMParseVarints(b []byte) []uint64 {
	out := make([]uint64, 0, len(b))
	var v uint64
	var k uint
	for _, c := range b {
		v |= uint64(c&0x7F) << k
		if c&0x80 != 0 {
			if k < 63 {
				k += 7
			}
			continue
		}
		out = append(out, v)
		v, k = 0, 0
	}
	return out
}

// cmPlanBanks is how many context models a plan column gets. Both arms use
// the same count, so the mixer geometry does not depend on which one runs.
const cmPlanBanks = 7

// setBytePlan is setByte for a plan column: the varint arm when Pair is
// empty, the cross-column arm when it is not.
func (c *cmCoder) setBytePlan(i int, data []byte) uint32 {
	if i == 0 {
		c.vs = varintState{}
	} else {
		c.vs.consume(data[i-1])
	}
	k := min(c.vs.k, 9)
	var p1, p2 uint32
	if i >= 1 {
		p1 = uint32(data[i-1])
	}
	if i >= 2 {
		p2 = uint32(data[i-2])
	}
	part := uint32(c.vs.v & 0xFFFF)
	b1, b2 := cmBucket(c.vs.prev), cmBucket(c.vs.prev2)
	h := c.hashes
	if pair := c.side.Pair; len(pair) != 0 {
		var o, oNext uint64
		if int(c.vs.idx) < len(pair) {
			o = pair[c.vs.idx]
		}
		if int(c.vs.idx)+1 < len(pair) {
			oNext = pair[c.vs.idx+1]
		}
		ob := cmBucket(o)
		h[0] = mix32(0x100 + k<<8 + ob)
		h[1] = mix32(0x10000 + k<<12 + ob<<6 + b1)
		h[2] = mix32(0x20000 + k<<16 + uint32(o&0xFF)<<8 + p1)
		h[3] = mix32(0x30000 + k<<20 + ob<<14 + part)
		h[4] = mix32(0x40000 + k<<24 + b1<<16 + b2<<8 + cmBucket(c.vs.prev3))
		h[5] = mix32(0x50000 + k<<16 + p1<<8 + p2)
		// The next gap as well: a value's magnitude often tracks the distance
		// to the following edit as much as the preceding one.
		h[6] = mix32(0x60000 + k<<16 + ob<<8 + cmBucket(oNext))
	} else {
		h[0] = mix32(0x100 + k)
		h[1] = mix32(0x10000 + k<<8 + p1)
		h[2] = mix32(0x20000 + k<<16 + p1<<8 + p2)
		h[3] = mix32(0x30000 + k<<12 + b1<<4)
		h[4] = mix32(0x40000 + k<<20 + b1<<12 + part)
		h[5] = mix32(0x50000 + k<<24 + b1<<16 + b2<<8)
		h[6] = mix32(0x60000 + k<<20 + uint32(c.vs.prev&0xFFF))
	}
	return min(k, CMSelMax-1)
}
