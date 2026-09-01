package delta

import "fmt"

// The prediction-conditioned context-mixing coder (SPEC G6).
//
// Everything upstream of this file is about telling the decoder what the
// bytes mean; the terminal compressor then throws that away. brotli and zstd
// are general LZ coders: they cannot be told that byte k of a replacement
// stream lands at a known offset in a prediction the decoder already holds,
// nor that it is the third byte of a modrm-plus-disp encoding. This coder can.
//
// The machinery is the lpaq skeleton cut to the bone: a carryless binary
// arithmetic coder, a bank of direct-lookup adaptive counters selected by
// hashed contexts, a match model, and one logistic mixer whose weights are
// selected by a small context. No SSE chain, no state machine, no two-level
// hash tables -- the win is in the *contexts*, and a bigger engine would only
// add a constant to every stream.
//
// It is entirely integer: the mixer's squash and stretch are tables, not
// math.Exp, so a patch coded on one machine decodes identically on every
// other. Encode and decode run the same model loop, so a size reported by
// CMEncode is the size CMDecode reads back.
//
// It is slow -- about 1 MB/s each way -- which is why split.go offers it per
// stream and keeps it only where it beats the general compressor.

// ---------------------------------------------------------------------------
// arithmetic coder

type arEncoder struct {
	x1, x2 uint32
	out    []byte
}

func newArEncoder(n int) *arEncoder {
	return &arEncoder{x2: 0xFFFFFFFF, out: make([]byte, 0, n)}
}

// encode codes one bit under p = P(bit==1), scaled to 16 bits.
func (e *arEncoder) encode(bit int, p uint16) {
	xmid := e.x1 + uint32((uint64(e.x2-e.x1)*uint64(p))>>16)
	if bit == 1 {
		e.x2 = xmid
	} else {
		e.x1 = xmid + 1
	}
	for (e.x1^e.x2)&0xFF000000 == 0 {
		e.out = append(e.out, byte(e.x2>>24))
		e.x1 <<= 8
		e.x2 = e.x2<<8 | 255
	}
}

func (e *arEncoder) flush() []byte {
	e.out = append(e.out, byte(e.x1>>24), byte(e.x1>>16), byte(e.x1>>8), byte(e.x1))
	return e.out
}

type arDecoder struct {
	x1, x2, x uint32
	in        []byte
	pos       int
}

func newArDecoder(b []byte) *arDecoder {
	d := &arDecoder{x2: 0xFFFFFFFF, in: b}
	for i := 0; i < 4; i++ {
		d.x = d.x<<8 | uint32(d.next())
	}
	return d
}

func (d *arDecoder) next() byte {
	if d.pos < len(d.in) {
		b := d.in[d.pos]
		d.pos++
		return b
	}
	return 0
}

func (d *arDecoder) decode(p uint16) int {
	xmid := d.x1 + uint32((uint64(d.x2-d.x1)*uint64(p))>>16)
	bit := 0
	if d.x <= xmid {
		bit, d.x2 = 1, xmid
	} else {
		d.x1 = xmid + 1
	}
	for (d.x1^d.x2)&0xFF000000 == 0 {
		d.x1 <<= 8
		d.x2 = d.x2<<8 | 255
		d.x = d.x<<8 | uint32(d.next())
	}
	return bit
}

// ---------------------------------------------------------------------------
// logistic domain, in integers

// squash maps a stretched value in [-2047, 2047] to a 12-bit probability, by
// interpolating lpaq's 33-point table. stretchTab inverts it.
var squashTab = [33]int32{
	1, 2, 3, 6, 10, 16, 27, 45, 73, 120, 194, 310, 488, 747, 1101, 1546, 2047,
	2549, 2994, 3348, 3607, 3785, 3901, 3975, 4024, 4050, 4068, 4079, 4085,
	4089, 4092, 4093, 4094,
}

func squash(d int32) int32 {
	if d > 2047 {
		return 4095
	}
	if d < -2047 {
		return 0
	}
	w := d & 127
	i := (d >> 7) + 16
	return (squashTab[i]*(128-w) + squashTab[i+1]*w + 64) >> 7
}

var stretchTab = func() [4096]int32 {
	var t [4096]int32
	pi := int32(0)
	for x := int32(-2047); x <= 2047; x++ {
		v := squash(x)
		for p := pi; p <= v; p++ {
			t[p] = x
		}
		pi = v + 1
	}
	for p := pi; p < 4096; p++ {
		t[p] = 2047
	}
	return t
}()

// adaptRate[n] is the 16-bit-scaled step a counter takes on its n-th update:
// 1/(n+1.5) early, floored at 1/48 so the counter keeps tracking drift.
var adaptRate = func() [64]int32 {
	var t [64]int32
	for n := range t {
		d := float64(n) + 1.5
		if d > 48 {
			d = 48
		}
		t[n] = int32(65536 / d)
	}
	return t
}()

func mix32(h uint32) uint32 {
	h ^= h >> 16
	h *= 0x7FEB352D
	h ^= h >> 15
	h *= 0x846CA68B
	h ^= h >> 16
	return h
}

// ---------------------------------------------------------------------------
// a context model: one hashed direct-lookup counter bank

type cmBank struct {
	probs []uint16
	cnt   []uint8
	mask  uint32
	base  uint32 // hash of the byte-level context, set once per byte
	idx   uint32 // slot for the bit being coded
}

func newBank(bits uint) *cmBank {
	n := 1 << bits
	// Probabilities are stored XORed with their neutral 1/2 value. An
	// untouched slot is therefore represented by the zero value Go and the OS
	// provide lazily, instead of faulting every page in merely to fill 0x8000.
	return &cmBank{probs: make([]uint16, n), cnt: make([]uint8, n), mask: uint32(n - 1)}
}

func (b *cmBank) prob() uint16 { return b.probs[b.idx] ^ (1 << 15) }

func (b *cmBank) selectBit(c0 uint32) {
	h := b.base*0x9E3779B1 + c0*0x85EBCA6B
	h ^= h >> 15
	h *= 0xC2B2AE35
	h ^= h >> 13
	b.idx = h & b.mask
}

func (b *cmBank) update(bit int) {
	i := b.idx
	p := int32(b.prob())
	target := int32(0)
	if bit == 1 {
		target = 65535
	}
	b.probs[i] = uint16(p+((target-p)*adaptRate[b.cnt[i]])>>16) ^ (1 << 15)
	if b.cnt[i] < 63 {
		b.cnt[i]++
	}
}

// ---------------------------------------------------------------------------
// match model: predicts the byte that followed the last occurrence of the
// current 6-byte suffix.

type matchModel struct {
	ht      []uint32
	hmask   uint32
	ptr     int
	length  int
	expect  byte
	valid   bool
	probs   [64 * 2]uint16
	cnt     [64 * 2]uint8
	idx     int
	predBit int
}

func newMatchModel(bits uint) *matchModel {
	m := &matchModel{ht: make([]uint32, 1<<bits), hmask: 1<<bits - 1}
	for i := range m.probs {
		m.probs[i] = 1 << 15
	}
	return m
}

// advance is called after each byte is known, with data holding every byte
// coded so far (data[:n]).
func (m *matchModel) advance(data []byte, n int) {
	if m.valid && m.ptr < n-1 && data[m.ptr] == data[n-1] {
		m.ptr++
		if m.length < 63 {
			m.length++
		}
	} else {
		m.length, m.valid = 0, false
	}
	if n < 6 {
		return
	}
	var h uint32 = 0x811C9DC5
	for _, c := range data[n-6 : n] {
		h = (h ^ uint32(c)) * 16777619
	}
	h = (h ^ h>>15) & m.hmask
	if !m.valid {
		if p := int(m.ht[h]); p > 0 && p < n {
			m.ptr, m.valid, m.length = p, true, 1
		}
	}
	m.ht[h] = uint32(n)
	if m.valid && m.ptr < n {
		m.expect = data[m.ptr]
	} else {
		m.valid = false
	}
}

// stretchIn returns the mixer input for the current bit, given the partial
// byte c0 (leading 1 sentinel) and how many bits of the byte are done.
func (m *matchModel) stretchIn(c0 uint32, done uint) int32 {
	m.idx = -1
	if !m.valid || m.length == 0 {
		return 0
	}
	// The expectation is only usable while the bits already coded agree.
	if done > 0 && uint32(m.expect)>>(8-done) != c0&(1<<done-1) {
		m.valid = false
		m.length = 0
		return 0
	}
	m.predBit = int(m.expect>>(7-done)) & 1
	m.idx = m.length*2 + m.predBit
	return stretchTab[m.probs[m.idx]>>4]
}

func (m *matchModel) update(bit int) {
	if m.idx < 0 {
		return
	}
	i := m.idx
	p := int32(m.probs[i])
	target := int32(0)
	if bit == 1 {
		target = 65535
	}
	m.probs[i] = uint16(p + ((target-p)*adaptRate[m.cnt[i]])>>16)
	if m.cnt[i] < 63 {
		m.cnt[i]++
	}
}

// ---------------------------------------------------------------------------
// mixer

type mixer struct {
	w   []int32 // [ctx][n], weights scaled by 1<<16
	n   int
	ctx int
	in  []int32
	pr  int32
}

func newMixer(n, ctxs int) *mixer {
	m := &mixer{w: make([]int32, n*ctxs), n: n, in: make([]int32, n)}
	for i := range m.w {
		m.w[i] = 1 << 14
	}
	return m
}

func (m *mixer) mix() uint16 {
	w := m.w[m.ctx*m.n : m.ctx*m.n+m.n]
	var dot int32
	for i, x := range m.in {
		dot += (w[i] * x) >> 8
	}
	m.pr = squash(dot >> 8)
	p := m.pr << 4
	if p < 1 {
		p = 1
	} else if p > 65535 {
		p = 65535
	}
	return uint16(p)
}

func (m *mixer) update(bit int) {
	err := ((int32(bit) << 12) - m.pr) * 7
	w := m.w[m.ctx*m.n : m.ctx*m.n+m.n]
	for i, x := range m.in {
		w[i] += (x * err * 2) >> 16
	}
}

// ---------------------------------------------------------------------------
// side information

// CMSelMax bounds the mixer selector; a larger value would only split the
// mixer's statistics further than the streams here can support.
const CMSelMax = 8

// CMSide is what the coder knows about byte i of a stream beyond the bytes
// before it. Every field is optional and every one must be derivable by the
// decoder before byte i is decoded; a field that is present must be as long
// as the stream.
//
//   - Pred is the prediction's byte at the position this byte replaces. It
//     is the whole point: the correction's bytes are mostly small edits of a
//     byte the decoder already holds.
//   - Sel is the mixer selector, 0..CMSelMax-1 -- the byte's column, so a
//     displacement's high byte and its low byte do not share weights.
//   - Cls and Off are the instruction field the byte sits in and its offset
//     within the instruction, from an x86 walk of the prediction under the
//     piece's DispContext (dispfield.go). They separate a call target's high
//     byte from its low one, which no byte-history context can.
//   - Varint marks a plan column instead: the coder runs the LEB128 contexts
//     of cmplan.go, and Pair, when set, is the paired index column's values.
//     Neither is per-position side information, so neither is length-checked;
//     they select a model set, and a stream decoded under the wrong one comes
//     back wrong, which the container's hashes catch.
type CMSide struct {
	Pred   []byte
	Sel    []byte
	Cls    []byte
	Off    []byte
	Varint bool
	Pair   []uint64
}

func (s *CMSide) check(n int) error {
	if s == nil {
		return nil
	}
	for _, f := range [][]byte{s.Pred, s.Sel, s.Cls, s.Off} {
		if f != nil && len(f) != n {
			return fmt.Errorf("%w: cm side info is %d bytes for a %d-byte stream", errCorrupt, len(f), n)
		}
	}
	if (s.Cls == nil) != (s.Off == nil) {
		return fmt.Errorf("%w: cm field context needs both class and offset", errCorrupt)
	}
	return nil
}

// ---------------------------------------------------------------------------
// the model set and the shared loop

type cmCoder struct {
	side   *CMSide
	banks  []*cmBank
	mm     *matchModel
	mx     *mixer
	hashes []uint32
	npred  int // index of the first Pred-conditioned bank, or -1
	ncls   int // index of the first Cls-conditioned bank, or -1
	vs     varintState
}

// bankBits sizes the tables to the stream, so a 4 KiB stream does not
// allocate the 12 MiB a whole-image one wants. Both sides compute it from
// the same length.
func bankBits(n int) uint {
	b := uint(12)
	for 1<<b < n && b < 19 {
		b++
	}
	return b + 3
}

func matchBits(n int) uint {
	b := uint(12)
	for 1<<b < n && b < 22 {
		b++
	}
	return b
}

func newCMCoder(n int, side *CMSide) *cmCoder {
	c := &cmCoder{side: side, npred: -1, ncls: -1}
	nb := 4
	if side != nil && side.Varint {
		nb = cmPlanBanks
	} else if side != nil && side.Pred != nil {
		c.npred = nb
		nb += 2
	}
	if side != nil && side.Cls != nil {
		c.ncls = nb
		nb += 2
	}
	bits := bankBits(n)
	for i := 0; i < nb; i++ {
		b := bits
		if i == 0 {
			b = 12 // order-0 within the selector: a direct table, no hashing loss
		}
		c.banks = append(c.banks, newBank(b))
	}
	c.hashes = make([]uint32, nb)
	c.mm = newMatchModel(matchBits(n))
	c.mx = newMixer(nb+1, CMSelMax*8)
	return c
}

// setByte fills the byte-level context hashes for position i and returns the
// mixer selector.
func (c *cmCoder) setByte(i int, data []byte) uint32 {
	var p1, p2, p4 uint32
	if i >= 1 {
		p1 = uint32(data[i-1])
	}
	if i >= 2 {
		p2 = uint32(data[i-2])
	}
	if i >= 4 {
		p4 = uint32(data[i-4])
	}
	var sel uint32
	if c.side != nil && c.side.Sel != nil {
		sel = uint32(c.side.Sel[i]) & (CMSelMax - 1)
	}
	h := c.hashes
	h[0] = sel
	h[1] = mix32(sel<<8 | p1)
	h[2] = mix32(0x20000 + p1<<8 + p2)
	h[3] = mix32(0x40000 + sel<<16 + p4)
	if c.npred >= 0 {
		pb := uint32(c.side.Pred[i])
		h[c.npred] = mix32(0x60000 + sel<<12 + pb)
		h[c.npred+1] = mix32(0x80000 + pb<<8 + p1)
	}
	if c.ncls >= 0 {
		fc, fo := uint32(c.side.Cls[i]), uint32(c.side.Off[i])
		h[c.ncls] = mix32(0xA0000 + sel<<20 + fc<<12 + fo<<4)
		var pb uint32
		if c.npred >= 0 {
			pb = uint32(c.side.Pred[i])
		}
		h[c.ncls+1] = mix32(0xC0000 + fc<<20 + fo<<12 + pb)
	}
	return sel
}

// code runs the shared model loop. When dec is nil it encodes src; otherwise
// it decodes into dst, which is len(src)-long and filled in.
func (c *cmCoder) code(src []byte, enc *arEncoder, dec *arDecoder, dst []byte) {
	data := src
	if dec != nil {
		data = dst
	}
	set := c.setByte
	if c.side != nil && c.side.Varint {
		set = c.setBytePlan
	}
	for i := range data {
		sel := int(set(i, data))
		for j, b := range c.banks {
			b.base = c.hashes[j]
		}
		c0 := uint32(1)
		for k := uint(0); k < 8; k++ {
			for j, b := range c.banks {
				b.selectBit(c0)
				c.mx.in[j] = stretchTab[b.prob()>>4]
			}
			c.mx.in[len(c.banks)] = c.mm.stretchIn(c0, k)
			c.mx.ctx = sel*8 + int(k)
			p := c.mx.mix()
			var bit int
			if dec != nil {
				bit = dec.decode(p)
			} else {
				bit = int(src[i]>>(7-k)) & 1
				enc.encode(bit, p)
			}
			for _, b := range c.banks {
				b.update(bit)
			}
			c.mm.update(bit)
			c.mx.update(bit)
			c0 = c0<<1 | uint32(bit)
		}
		if dec != nil {
			dst[i] = byte(c0)
		}
		c.mm.advance(data, i+1)
	}
}

// CMEncode codes src under side and returns the coded bytes.
func CMEncode(src []byte, side *CMSide) ([]byte, error) {
	if err := side.check(len(src)); err != nil {
		return nil, err
	}
	c := newCMCoder(len(src), side)
	enc := newArEncoder(len(src)/2 + 64)
	c.code(src, enc, nil, nil)
	return enc.flush(), nil
}

// CMDecode reverses CMEncode. side must describe the same n bytes it did at
// encode time; a stream decoded under different side information yields
// different bytes, which the container's target hash catches.
func CMDecode(coded []byte, n int, side *CMSide) ([]byte, error) {
	if err := side.check(n); err != nil {
		return nil, err
	}
	c := newCMCoder(n, side)
	dst := make([]byte, n)
	c.code(nil, nil, newArDecoder(coded), dst)
	return dst, nil
}
