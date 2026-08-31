package main

import "math"

// A minimal context-mixing coder, built only to answer SPEC decision G6: is
// there enough left on the table, over xz, to justify replacing the terminal
// compressor with something that knows what the bytes mean?
//
// The machinery is the standard lpaq/paq skeleton, cut to the bone: a
// carryless binary arithmetic coder, a bank of direct-lookup adaptive
// counters selected by hashed contexts, and one logistic mixer whose weights
// are selected by a small context. No SSE chain, no state machine, no
// two-level hash tables. That is deliberate -- the question is whether the
// *contexts* (the prediction's byte at the same position, the field the byte
// sits in) are worth anything, and a bigger engine would only add a constant
// to every rung.
//
// Encoding and decoding run the identical model loop, so every reported size
// is a real coded size and every rung is round-trip checked.

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
// logistic domain

const stretchBits = 12 // counters are read at 12-bit resolution

var stretchTab = func() [1 << stretchBits]float32 {
	var t [1 << stretchBits]float32
	for i := range t {
		p := (float64(i) + 0.5) / float64(int(1)<<stretchBits)
		v := math.Log(p / (1 - p))
		t[i] = float32(v)
	}
	return t
}()

func squash(x float32) float32 {
	if x > 20 {
		x = 20
	} else if x < -20 {
		x = -20
	}
	return float32(1 / (1 + math.Exp(-float64(x))))
}

// adaptRate[n] is the 16-bit-scaled step a counter takes on its n-th update.
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
	b := &cmBank{probs: make([]uint16, n), cnt: make([]uint8, n), mask: uint32(n - 1)}
	for i := range b.probs {
		b.probs[i] = 1 << 15
	}
	return b
}

func (b *cmBank) selectBit(c0 uint32) {
	h := b.base*0x9E3779B1 + c0*0x85EBCA6B
	h ^= h >> 15
	h *= 0xC2B2AE35
	h ^= h >> 13
	b.idx = h & b.mask
}

func (b *cmBank) p() uint16 { return b.probs[b.idx] }

func (b *cmBank) update(bit int) {
	i := b.idx
	p := int32(b.probs[i])
	target := int32(0)
	if bit == 1 {
		target = 65535
	}
	b.probs[i] = uint16(p + ((target-p)*adaptRate[b.cnt[i]])>>16)
	if b.cnt[i] < 63 {
		b.cnt[i]++
	}
}

// ---------------------------------------------------------------------------
// match model: predicts the byte that followed the last occurrence of the
// current 6-byte suffix.

const matchHashBits = 22

type matchModel struct {
	ht      []uint32
	ptr     int
	length  int
	expect  byte
	valid   bool
	probs   [64 * 2]uint16
	cnt     [64 * 2]uint8
	idx     int
	predBit int
}

func newMatchModel() *matchModel {
	m := &matchModel{ht: make([]uint32, 1<<matchHashBits)}
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
	h = (h ^ h>>15) & (1<<matchHashBits - 1)
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
func (m *matchModel) stretchIn(c0 uint32, done uint) float32 {
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
	p := m.probs[m.idx]
	s := stretchTab[p>>(16-stretchBits)]
	return s
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
	w    []float32 // [ctx][n]
	n    int
	ctx  int
	in   []float32
	pr   float32
	rate float32
}

func newMixer(n, ctxs int) *mixer {
	m := &mixer{w: make([]float32, n*ctxs), n: n, in: make([]float32, n), rate: 0.0015}
	for i := range m.w {
		m.w[i] = 0.3
	}
	return m
}

func (m *mixer) mix() uint16 {
	w := m.w[m.ctx*m.n : m.ctx*m.n+m.n]
	var dot float32
	for i, x := range m.in {
		dot += w[i] * x
	}
	m.pr = squash(dot)
	p := int(m.pr * 65536)
	if p < 1 {
		p = 1
	} else if p > 65535 {
		p = 65535
	}
	return uint16(p)
}

func (m *mixer) update(bit int) {
	err := (float32(bit) - m.pr) * m.rate
	w := m.w[m.ctx*m.n : m.ctx*m.n+m.n]
	for i, x := range m.in {
		w[i] += err * x
	}
}

// ---------------------------------------------------------------------------
// the model set

// cmContexts supplies, for byte position i of the stream, the byte-level
// context hash of each model. Everything it reads must be available to the
// decoder before that byte is decoded.
type cmContexts interface {
	// numModels is how many hashed banks the set uses.
	numModels() int
	// bankBits is the table size of each bank.
	bankBits(m int) uint
	// setByte fills hashes[0:numModels] for stream position i, given the bytes
	// already coded, and returns the mixer selector.
	setByte(i int, data []byte, hashes []uint32) int
	// mixerCtxs bounds the selector.
	mixerCtxs() int
	// useMatch says whether the match model is in the set.
	useMatch() bool
}

// cmRefSet is an optional extension: a context set may supply extra match
// models that predict from a reference image the decoder already holds. See
// cmprobe2.go.
type cmRefSet interface{ refModels() []*refMatch }

type cmCoder struct {
	set    cmContexts
	banks  []*cmBank
	mm     *matchModel
	refs   []*refMatch
	mx     *mixer
	hashes []uint32
}

func newCMCoder(set cmContexts) *cmCoder {
	n := set.numModels()
	c := &cmCoder{set: set, hashes: make([]uint32, n)}
	for i := 0; i < n; i++ {
		c.banks = append(c.banks, newBank(set.bankBits(i)))
	}
	inputs := n
	if set.useMatch() {
		c.mm = newMatchModel()
		inputs++
	}
	if rs, ok := set.(cmRefSet); ok {
		c.refs = rs.refModels()
		inputs += len(c.refs)
	}
	c.mx = newMixer(inputs, set.mixerCtxs()*8)
	return c
}

// code runs the shared model loop. When dec is nil it encodes src; otherwise it
// decodes into dst, which must be len(src) long and is filled in.
func (c *cmCoder) code(src []byte, enc *arEncoder, dec *arDecoder, dst []byte) {
	data := src
	if dec != nil {
		data = dst
	}
	nb := len(c.banks)
	if c.mm != nil {
		nb++
	}
	for i := range data {
		sel := c.set.setByte(i, data, c.hashes)
		for j, b := range c.banks {
			b.base = c.hashes[j]
		}
		for _, r := range c.refs {
			r.startByte(i)
		}
		c0 := uint32(1)
		for k := uint(0); k < 8; k++ {
			for j, b := range c.banks {
				b.selectBit(c0)
				c.mx.in[j] = stretchTab[b.p()>>(16-stretchBits)]
			}
			if c.mm != nil {
				c.mx.in[len(c.banks)] = c.mm.stretchIn(c0, k)
			}
			for j, r := range c.refs {
				c.mx.in[nb+j] = r.stretchIn(c0, k)
			}
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
			if c.mm != nil {
				c.mm.update(bit)
			}
			for _, r := range c.refs {
				r.update(bit)
			}
			c.mx.update(bit)
			c0 = c0<<1 | uint32(bit)
		}
		if dec != nil {
			dst[i] = byte(c0)
		}
		if c.mm != nil {
			c.mm.advance(data, i+1)
		}
		for _, r := range c.refs {
			r.endByte(i, data[i])
		}
	}
}

// cmEncode codes src under set and returns the coded bytes.
func cmEncode(src []byte, set cmContexts) []byte {
	c := newCMCoder(set)
	enc := newArEncoder(len(src)/2 + 64)
	c.code(src, enc, nil, nil)
	return enc.flush()
}

// cmDecode reverses cmEncode; set must be re-created (its state is consumed).
func cmDecode(coded []byte, n int, set cmContexts) []byte {
	c := newCMCoder(set)
	dst := make([]byte, n)
	c.code(nil, nil, newArDecoder(coded), dst)
	return dst
}
