package main

import (
	"fmt"
	"math"
	"os"

	"github.com/wjordan/go-binsync/delta/x86"
)

// The byte correction is compressed by xz, which never sees the prediction.
// But the decoder holds the whole predicted image while it applies the
// correction, including the bytes *after* the one being decoded -- so the
// residual can be coded against a context no LZ pass can use. This prices that
// against what the byte columns cost today.
//
// The estimate is adaptive rather than empirical: each symbol is charged
// against counts accumulated from the symbols before it only, by the
// Krichevsky-Trofimov rule. That is what a real adaptive coder would pay, so
// it does not smuggle in the cost of a model it never ships.
type ktModel struct {
	counts map[uint32]map[byte]uint32
	total  map[uint32]uint32
	bits   float64
}

func newKT() *ktModel {
	return &ktModel{counts: map[uint32]map[byte]uint32{}, total: map[uint32]uint32{}}
}

func (m *ktModel) code(ctx uint32, sym byte) {
	c := m.counts[ctx]
	if c == nil {
		c = map[byte]uint32{}
		m.counts[ctx] = c
	}
	m.bits -= math.Log2((float64(c[sym]) + 0.5) / (float64(m.total[ctx]) + 128))
	c[sym]++
	m.total[ctx]++
}

func (m *ktModel) bytes() int { return int(m.bits/8 + 0.5) }

// probeConditionalCorrection prices the wrong bytes of a region under contexts
// drawn from the prediction. Position information is excluded: it is a
// separate column today and would stay one, so the comparison is against the
// byte columns alone.
func probeConditionalCorrection(pred, target []byte, byteColumns int) {
	at := func(b []byte, i int) uint32 {
		if i < 0 || i >= len(b) {
			return 0
		}
		return uint32(b[i])
	}
	kinds := []struct {
		name string
		ctx  func(i int) uint32
	}{
		{"order-0", func(int) uint32 { return 0 }},
		{"pred[i]", func(i int) uint32 { return at(pred, i) }},
		{"pred[i-1..i]", func(i int) uint32 { return at(pred, i-1)<<8 | at(pred, i) }},
		{"pred[i], pred[i+1]", func(i int) uint32 { return at(pred, i)<<8 | at(pred, i+1) }},
		{"pred[i-1..i+1]", func(i int) uint32 { return at(pred, i-1)<<16 | at(pred, i)<<8 | at(pred, i+1) }},
		{"out[i-1]", func(i int) uint32 { return at(target, i-1) }},
		{"out[i-2..i-1]", func(i int) uint32 { return at(target, i-2)<<8 | at(target, i-1) }},
		{"pred[i], out[i-1]", func(i int) uint32 { return at(pred, i)<<8 | at(target, i-1) }},
		{"pred[i], out[i-2..i-1]", func(i int) uint32 {
			return at(pred, i)<<16 | at(target, i-2)<<8 | at(target, i-1)
		}},
		{"pred[i-1..i+1], out[i-1]", func(i int) uint32 {
			return at(pred, i-1)<<24 | at(pred, i)<<16 | at(pred, i+1)<<8 | at(target, i-1)
		}},
	}
	wrong := 0
	for i := range target {
		if pred[i] != target[i] {
			wrong++
		}
	}
	fmt.Fprintf(os.Stderr, "  probe conditional correction: %d wrong bytes, byte columns cost %d (%.2f bits/byte)\n",
		wrong, byteColumns, 8*float64(byteColumns)/float64(wrong))
	for _, k := range kinds {
		m := newKT()
		for i := range target {
			if pred[i] != target[i] {
				m.code(k.ctx(i), target[i])
			}
		}
		fmt.Fprintf(os.Stderr, "    %-26s %9d B (%.2f bits/byte, %d contexts)\n",
			k.name, m.bytes(), m.bits/float64(wrong), len(m.total))
	}
}

// probeResidualCause decomposes the residual by *why* it is wrong, which is
// the question a prediction improvement has to answer. A function whose new
// body differs from its mapped old body only in PC-relative fields was a
// retargeting failure; one whose content matches a different old function was
// a matching failure; one that matches nothing was recompiled. Only the third
// is out of reach, and the three want completely different work.
func probeResidualCause(predText, targetText, oldText []byte, maps []mapping) {
	type bucket struct{ funcs, wrong, oldB, newB, runs int }
	var displacement, sameLen, resized, mapped bucket
	byContent := make(map[uint64]int, len(maps))
	for i, m := range maps {
		if m.Src+m.SrcSize > uint64(len(oldText)) {
			continue
		}
		byContent[x86.ContentHash(oldText[m.Src:m.Src+m.SrcSize])] = i
	}
	var covered, coveredWrong, total int
	for _, m := range maps {
		if m.Dst+m.DstSize > uint64(len(targetText)) || m.Src+m.SrcSize > uint64(len(oldText)) {
			continue
		}
		oldBody := oldText[m.Src : m.Src+m.SrcSize]
		newBody := targetText[m.Dst : m.Dst+m.DstSize]
		w := 0
		for i := range newBody {
			if predText[m.Dst+uint64(i)] != newBody[i] {
				w++
			}
		}
		covered += int(m.DstSize)
		coveredWrong += w
		total++
		if w == 0 {
			continue
		}
		runs := 0
		for i := 0; i < len(newBody); {
			if predText[m.Dst+uint64(i)] == newBody[i] {
				i++
				continue
			}
			runs++
			for i < len(newBody) && predText[m.Dst+uint64(i)] != newBody[i] {
				i++
			}
		}
		var b *bucket
		switch {
		case x86.Equal(oldBody, newBody):
			b = &displacement
		case m.SrcSize == m.DstSize:
			b = &sameLen
		default:
			b = &resized
		}
		b.funcs++
		b.wrong += w
		b.runs += runs
		b.oldB += int(m.SrcSize)
		b.newB += int(m.DstSize)
		if _, ok := byContent[x86.ContentHash(newBody)]; ok {
			mapped.funcs++
			mapped.wrong += w
			mapped.newB += int(m.DstSize)
		}
	}
	var uncoveredWrong int
	for i := range targetText {
		if predText[i] != targetText[i] {
			uncoveredWrong++
		}
	}
	uncoveredWrong -= coveredWrong
	fmt.Fprintf(os.Stderr, "  probe residual cause: %d mapped functions covering %d B; %d wrong inside, %d wrong outside any mapping (%d B uncovered)\n",
		total, covered, coveredWrong, uncoveredWrong, len(targetText)-covered)
	for _, b := range []struct {
		name string
		b    bucket
	}{{"displacements only", displacement}, {"same length, changed", sameLen}, {"resized", resized},
		{"content matches SOME old function", mapped}} {
		pct := 0.0
		if b.b.newB != 0 {
			pct = 100 * float64(b.b.wrong) / float64(b.b.newB)
		}
		fmt.Fprintf(os.Stderr, "    %-36s %8d functions, %9d wrong bytes in %9d runs, %11d B old -> %11d B new (%.2f%% wrong)\n",
			b.name, b.b.funcs, b.b.wrong, b.b.runs, b.b.oldB, b.b.newB, pct)
	}
}
