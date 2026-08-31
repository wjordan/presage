package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// probeCMCoder answers SPEC decision G6: how much of the correction's cost is
// xz's fault rather than the data's?
//
// The shipped correction ships its replacement bytes as five xz streams. xz is
// a general LZ77 coder with a range coder behind it; it cannot be told that
// byte k of the stream lands at a known position in a prediction the decoder
// already holds, nor that it is the third byte of a modrm-plus-disp encoding.
// A context-mixing coder can be told both. This probe builds one, feeds it the
// same bytes, and prices each context against the xz number it would replace.
//
// Every context used here is available to the decoder before the byte is
// decoded: the Gaps and Lens columns arrive first, so every run's position is
// known; the prediction is in hand; and the prediction's instruction stream can
// be walked from it. Nothing reads the target.
//
// Report only. Nothing here changes the shipped format.

// --- context set -----------------------------------------------------------

// cmModelCount[r] is how many hashed banks rung r uses, as a prefix of the
// fixed model list below. Rung 4 additionally turns the match model on.
var cmRungModels = []int{2, 4, 6, 8, 8, 6}

var cmRungNames = []string{
	"CM order-1",
	"  + order-2, sparse i-4",
	"  + pred[i]",
	"  + field class / offset",
	"  + match model",
	"ablation: pred + match, no field",
}

type corrCtx struct {
	buck, predB, fcls, foff, roff []byte
	rung                          int
}

func (c *corrCtx) numModels() int { return cmRungModels[c.rung] }

func (c *corrCtx) bankBits(m int) uint {
	if m == 0 {
		return 12
	}
	return 22
}

func (c *corrCtx) mixerCtxs() int { return correctionBuckets }
func (c *corrCtx) useMatch() bool { return c.rung >= 4 }

func mix32(h uint32) uint32 {
	h ^= h >> 16
	h *= 0x7FEB352D
	h ^= h >> 15
	h *= 0x846CA68B
	h ^= h >> 16
	return h
}

func (c *corrCtx) setByte(i int, data []byte, h []uint32) int {
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
	b := uint32(c.buck[i])
	n := len(h)
	h[0] = mix32(b + 1)
	h[1] = mix32(b<<8 | p1)
	if n > 2 {
		h[2] = mix32(0x20000 + p1<<8 + p2)
		h[3] = mix32(0x40000 + b<<16 + p4)
	}
	if n > 4 {
		pb := uint32(c.predB[i])
		h[4] = mix32(0x60000 + b<<12 + pb)
		h[5] = mix32(0x80000 + pb<<8 + p1)
	}
	if n > 6 {
		fc, fo, ro := uint32(c.fcls[i]), uint32(c.foff[i]), uint32(c.roff[i])
		h[6] = mix32(0xA0000 + b<<20 + fc<<12 + fo<<4 + ro)
		h[7] = mix32(0xC0000 + fc<<20 + fo<<12 + uint32(c.predB[i]))
	}
	return int(b)
}

// A plain order-N set, for the columns that carry no per-position side info.
type plainCtx struct{ order int }

func (p *plainCtx) numModels() int    { return p.order + 1 }
func (p *plainCtx) bankBits(int) uint { return 20 }
func (p *plainCtx) mixerCtxs() int    { return 1 }
func (p *plainCtx) useMatch() bool    { return true }
func (p *plainCtx) setByte(i int, data []byte, h []uint32) int {
	var v uint32
	h[0] = mix32(1)
	for k := 1; k <= p.order; k++ {
		if i >= k {
			v = v<<8 | uint32(data[i-k])
		}
		h[k] = mix32(uint32(k)<<28 + v + 0x1000)
	}
	return 0
}

// --- field walk ------------------------------------------------------------

const (
	fcPrefix = iota
	fcOpcode
	fcModRM
	fcSIB
	fcDisp
	fcImm
	fcUndecoded
	fcCount
)

// instClasses fills cls[0:n] and off[0:n] for one instruction's bytes.
func instClasses(code []byte, cls, off []byte) {
	for i := range code {
		off[i] = byte(min(i, 15))
		cls[i] = fcUndecoded
	}
	f, ok := locateFields(code)
	if !ok {
		return
	}
	opEnd := f.immOff
	if f.modrm >= 0 {
		opEnd = f.modrm
	}
	opBytes := f.opLen
	if f.vexKind > 0 {
		opBytes = 1
	}
	for i := range code {
		switch {
		case f.dispLen > 0 && i >= f.dispOff && i < f.dispOff+f.dispLen:
			cls[i] = fcDisp
		case f.immLen > 0 && i >= f.immOff && i < f.immOff+f.immLen:
			cls[i] = fcImm
		case i == f.modrm:
			cls[i] = fcModRM
		case i == f.sib:
			cls[i] = fcSIB
		case i >= opEnd-opBytes && i < opEnd:
			cls[i] = fcOpcode
		default:
			cls[i] = fcPrefix
		}
	}
}

// --- the probe -------------------------------------------------------------

type cmRun struct{ lo, hi, buck int }

func probeCMCoder(predText, targetText []byte, maps []mapping, text section, imageLen int, secs map[string]section, wholePred, wholeTarget []byte) {
	// The correction as it ships, restricted to .text, with §14's displacement
	// column on when -dispcol is set -- so the bytes coded below are exactly
	// the bytes the shipped format would compress.
	var d *dispContext
	if dispColumn {
		d = newDispContext(maps, text, imageLen).restrict(int(text.Off), int(text.Off+text.Size))
	}
	c, err := encodeColumnarDisp(predText, targetText, d)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  probe cmix FAILED: %v\n", err)
		return
	}

	// The same run walk encodeColumnarDisp performs, for side info only.
	var runs []cmRun
	for i := 0; i < len(targetText); {
		if predText[i] == targetText[i] {
			i++
			continue
		}
		j := i
		for j < len(targetText) && predText[j] != targetText[j] {
			j++
		}
		runs = append(runs, cmRun{i, j, bucketOf(j - i)})
		i = j
	}

	// Per-bucket side info, appended in the same order as the bytes.
	var sBuck, sPred, sCls, sOff, sRoff [correctionBuckets][]byte
	var cls, off [16]byte
	curStart, curLen := 0, 0
	advance := func(p int) {
		for curStart+curLen <= p {
			curStart += curLen
			if curStart >= len(predText) {
				curLen = 1
				return
			}
			if inst, ok := safeDecode(predText[curStart:]); ok {
				curLen = inst.Len
				instClasses(predText[curStart:curStart+curLen], cls[:curLen], off[:curLen])
			} else {
				curLen = 1
				cls[0], off[0] = fcUndecoded, 0
			}
		}
	}
	for _, r := range runs {
		b := r.buck
		for p := r.lo; p < r.hi; p++ {
			advance(p)
			k := p - curStart
			if k < 0 || k >= curLen {
				k = 0
			}
			sBuck[b] = append(sBuck[b], byte(b))
			sPred[b] = append(sPred[b], predText[p])
			sCls[b] = append(sCls[b], cls[k])
			sOff[b] = append(sOff[b], off[k])
			sRoff[b] = append(sRoff[b], byte(min(p-r.lo, 15)))
		}
	}

	// The coded stream: the five buckets end to end, exactly the bytes the
	// shipped format ships, in the shipped order.
	var stream, sideBuck, sidePred, sideCls, sideOff, sideRoff []byte
	for b := 0; b < correctionBuckets; b++ {
		if len(sBuck[b]) != len(c.Bytes[b]) {
			fmt.Fprintf(os.Stderr, "  probe cmix FAILED: bucket %d side info %d vs bytes %d\n", b, len(sBuck[b]), len(c.Bytes[b]))
			return
		}
		stream = append(stream, c.Bytes[b]...)
		sideBuck = append(sideBuck, sBuck[b]...)
		sidePred = append(sidePred, sPred[b]...)
		sideCls = append(sideCls, sCls[b]...)
		sideOff = append(sideOff, sOff[b]...)
		sideRoff = append(sideRoff, sRoff[b]...)
	}

	bucketXZ := 0
	for _, n := range xzSizes(c.Bytes[:]...) {
		bucketXZ += n
	}
	otherStreams := [][]byte{c.Gaps, c.Lens, c.Tags, c.Idx, c.Loc, c.Far}
	otherXZ := 0
	for _, n := range xzSizes(otherStreams...) {
		otherXZ += n
	}
	textXZ := bucketXZ + otherXZ

	fmt.Fprintf(os.Stderr, "  probe cmix: .text correction, dispcol=%v\n", dispColumn)
	fmt.Fprintf(os.Stderr, "    %d wrong runs, bucket bytes %d raw; buckets xz %d, other columns xz %d, .text piece %d\n",
		len(runs), len(stream), bucketXZ, otherXZ, textXZ)
	var clsHist [fcCount]int
	for _, v := range sideCls {
		clsHist[v]++
	}
	fmt.Fprintf(os.Stderr, "    field class of the bucket bytes: prefix %d opcode %d modrm %d sib %d disp %d imm %d undecoded %d\n",
		clsHist[0], clsHist[1], clsHist[2], clsHist[3], clsHist[4], clsHist[5], clsHist[6])

	bpb := func(n int) float64 { return 8 * float64(n) / float64(max(len(stream), 1)) }
	fmt.Fprintf(os.Stderr, "    %-32s %9s %8s %9s %9s\n", "rung (buckets only)", "coded", "bits/B", "enc MB/s", "dec MB/s")
	report := func(name string, n int, encS, decS float64) {
		e, dd := "-", "-"
		if encS > 0 {
			e = fmt.Sprintf("%.2f", float64(len(stream))/1e6/encS)
		}
		if decS > 0 {
			dd = fmt.Sprintf("%.2f", float64(len(stream))/1e6/decS)
		}
		fmt.Fprintf(os.Stderr, "    %-32s %9d %8.4f %9s %9s  (%+d vs xz, .text piece %d)\n",
			name, n, bpb(n), e, dd, n-bucketXZ, n+otherXZ)
	}

	xz1 := 0
	for _, b := range c.Bytes {
		xz1 += xzSizeContiguous(b)
	}
	report("xz -9e -T0 (shipped, 5 streams)", bucketXZ, 0, 0)
	report("xz -9e -T1 (shipped, 5 streams)", xz1, 0, 0)
	report("xz -9e (one concatenated stream)", xzSize(stream), 0, 0)
	report("brotli -q11 -w24 (concatenated)", brotliSize(stream), 0, 0)
	for _, z := range []string{"19", "22"} {
		report("zstd --ultra -"+z+" (concatenated)", zstdSize(stream, z), 0, 0)
	}

	for r := range cmRungModels {
		set := &corrCtx{sideBuck, sidePred, sideCls, sideOff, sideRoff, r}
		t := time.Now()
		coded := cmEncode(stream, set)
		encSec := time.Since(t).Seconds()
		set2 := &corrCtx{sideBuck, sidePred, sideCls, sideOff, sideRoff, r}
		t = time.Now()
		back := cmDecode(coded, len(stream), set2)
		decSec := time.Since(t).Seconds()
		if !bytes.Equal(back, stream) {
			fmt.Fprintf(os.Stderr, "    %-32s ROUND TRIP FAILED\n", cmRungNames[r])
			continue
		}
		report(cmRungNames[r], len(coded), encSec, decSec)
	}

	// The position columns are the rest of the .text piece. They carry no
	// per-position side info, so they get a plain order-3 set -- enough to say
	// whether the same coder helps there too.
	for _, col := range []struct {
		name string
		b    []byte
	}{{"gaps", c.Gaps}, {"lens", c.Lens}} {
		if len(col.b) == 0 {
			continue
		}
		set := &plainCtx{3}
		t := time.Now()
		coded := cmEncode(col.b, set)
		sec := time.Since(t).Seconds()
		if back := cmDecode(coded, len(col.b), &plainCtx{3}); !bytes.Equal(back, col.b) {
			fmt.Fprintf(os.Stderr, "    column %s ROUND TRIP FAILED\n", col.name)
			continue
		}
		x := xzSize(col.b)
		fmt.Fprintf(os.Stderr, "    column %-11s %8d raw, xz %8d, CM order-3+match %8d (%+d), %.2f MB/s\n",
			col.name, len(col.b), x, len(coded), len(coded)-x, float64(len(col.b))/1e6/max(sec, 1e-9))
	}

	// Anchor the .text piece against the whole shipped correction, so the
	// delta can be priced against the headline.
	if split, pick, err := bestCorrectionXZ(wholePred, wholeTarget, secs, dispColumnCtx(maps, text, imageLen)); err == nil {
		fmt.Fprintf(os.Stderr, "    whole split correction xz %d (pick %s); .text piece %d = %.1f%% of it\n",
			split, pick, textXZ, 100*float64(textXZ)/float64(max(split, 1)))
	} else {
		fmt.Fprintf(os.Stderr, "    whole split correction FAILED: %v\n", err)
	}
}

func dispColumnCtx(maps []mapping, text section, imageLen int) *dispContext {
	if !dispColumn {
		return nil
	}
	return newDispContext(maps, text, imageLen)
}

// zstdSize is the third general-purpose baseline; it returns 0 when zstd is
// absent.
func zstdSize(b []byte, level string) int {
	cmd := exec.Command("zstd", "--ultra", "-"+level, "-c", "-q", "--long=27")
	cmd.Stdin = bytes.NewReader(b)
	var out bytes.Buffer
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return 0
	}
	return out.Len()
}
