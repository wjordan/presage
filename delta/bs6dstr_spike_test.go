package delta

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
)

// TestBS6DiffString prices the representations of the difference string.
//
// plain.go's note says bsdiff's dense difference string "bzip2's block sort
// handles beautifully and every LZ compressor handles badly", and that
// carrying the zeros as gaps took the reference pair from 315 KB to 170 KB
// against bsdiff's 151 KB. bsdiff 6 (thesis 2.7) does neither: it splits the
// dense string into a difference *map* -- one bit per matched byte, saying
// where the changes are -- and a non-zero difference string saying by how
// much, on the grounds that the map is "distinctive and compressible"
// because instruction encodings put the changes at structured offsets.
//
// This measures all three shapes under three compressors on the same data.
//
//	BS6_OLD=... BS6_NEW=... go test ./delta -run BS6DiffString -v
func TestBS6DiffString(t *testing.T) {
	oldPath, newPath := os.Getenv("BS6_OLD"), os.Getenv("BS6_NEW")
	if oldPath == "" || newPath == "" {
		t.Skip("set BS6_OLD and BS6_NEW")
	}
	old, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	nw, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}

	// Re-run the shipped scan, keeping the matched runs so every shape below
	// describes exactly the same bytes.
	var dense []byte
	collect := func(new, o []byte) {
		for i := range new {
			dense = append(dense, new[i]-o[i])
		}
	}
	sh := lzShape{probe: plainProbe, cutoff: 8, extNum: 2, extDen: 1, merge: diffMergeGap}
	sh.collect = collect
	out := sh.diff(old, nw)

	nonzero := 0
	for _, b := range dense {
		if b != 0 {
			nonzero++
		}
	}
	t.Logf("%s -> %s", oldPath, newPath)
	t.Logf("%d triples, %d matched bytes, %d non-zero (%.4f%%), %d extra bytes",
		out.ntriples, len(dense), nonzero, 100*float64(nonzero)/float64(max(len(dense), 1)), len(out.extra))

	// bitmap: one bit per matched byte, plus the non-zero values.
	bitmap := make([]byte, (len(dense)+7)/8)
	var vals []byte
	for i, b := range dense {
		if b != 0 {
			bitmap[i/8] |= 1 << (uint(i) % 8)
			vals = append(vals, b)
		}
	}
	// runs: what ships -- (gap, length, bytes) interleaved.
	var runsStream []byte
	{
		d := &spikeDiffWriter{o: &lzOut{}, mergeGap: diffMergeGap}
		d.write(dense, make([]byte, len(dense)))
		runsStream = d.o.diff
	}
	// runs, positions and values apart.
	var rpos, rval []byte
	{
		o := &lzOut{}
		d := &spikeDiffWriter{o: o, split: true, mergeGap: diffMergeGap}
		d.write(dense, make([]byte, len(dense)))
		rpos, rval = o.dpos, o.dval
	}

	// Percival's own coder for the difference map, priced exactly.
	var positions []int
	for i, b := range dense {
		if b != 0 {
			positions = append(positions, i)
		}
	}
	// Alpha instructions are four bytes wide and aligned, so a correction
	// position is congruent to a constant mod 4 and the difference map is
	// "distinctive". x86-64 instructions are not. Measure it rather than
	// assume it.
	var mod4, mod2 [4]int
	for _, p := range positions {
		mod4[p%4]++
		mod2[p%2]++
	}
	t.Logf("non-zero positions mod 4: %v; mod 2: %v", mod4, mod2[:2])
	ipBits := interpolativeBits(positions, len(dense))
	ipBytes := int((ipBits + 7) / 8)
	valBest, valName := 1<<62, ""
	for _, c := range compressors {
		if n := c.size(vals); n < valBest {
			valBest, valName = n, c.name
		}
	}
	t.Logf("difference map, interpolative: %d bytes (%.2f bits per position); "+
		"non-zero values %d (%s); total %d", ipBytes, float64(ipBits)/float64(len(positions)),
		valBest, valName, ipBytes+valBest)

	type item struct {
		name  string
		parts [][]byte
	}
	items := []item{
		{"dense", [][]byte{dense}},
		{"  bitmap alone", [][]byte{bitmap}},
		{"  values alone", [][]byte{vals}},
		{"runs (shipped)", [][]byte{runsStream}},
		{"runs pos|val", [][]byte{rpos, rval}},
		{"bitmap|val", [][]byte{bitmap, vals}},
	}
	results := make([][]int, len(items))
	for i := range results {
		results[i] = make([]int, len(compressors))
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, it := range items {
		for ci, c := range compressors {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				n := 0
				for _, p := range it.parts {
					n += c.size(p)
				}
				results[i][ci] = n
			}()
		}
	}
	wg.Wait()
	// bsdiff compresses control, difference and extra as three streams;
	// plain.go concatenates them and the container compresses frames of the
	// concatenation. Price both under every compressor, so the comparison is
	// not an artefact of cz picking brotli-11 for a small stream.
	mono := DiffLZ(old, nw)
	t.Logf("%-16s %10s %10s %10s %10s %10s %10s", "packing", compressors[0].name,
		compressors[1].name, compressors[2].name, compressors[3].name,
		compressors[4].name, compressors[5].name)
	for _, pk := range []struct {
		name  string
		parts [][]byte
	}{
		{"one buffer", [][]byte{mono}},
		{"ctrl|diff|extra", [][]byte{out.ctrl, runsStream, out.extra}},
		// the same one buffer, but with each stream starting on a frame
		// boundary, which separates "each stream gets its own codec" from
		// "no frame straddles two streams".
		{"frame-aligned", [][]byte{framePad(out.ctrl, runsStream, out.extra)}},
	} {
		line := fmt.Sprintf("%-16s", pk.name)
		for _, c := range compressors {
			n := 0
			for _, b := range pk.parts {
				n += c.size(b)
			}
			line += fmt.Sprintf(" %10d", n)
		}
		t.Log(line)
	}
	t.Logf("control + extra: %d cz", czLen(out.ctrl)+czLen(out.extra))
	hdr := fmt.Sprintf("%-16s", "difference")
	for _, c := range compressors {
		hdr += fmt.Sprintf(" %10s", c.name)
	}
	t.Log(hdr + "        raw")
	for i, it := range items {
		raw := 0
		for _, p := range it.parts {
			raw += len(p)
		}
		line := fmt.Sprintf("%-16s", it.name)
		for ci := range compressors {
			line += fmt.Sprintf(" %10d", results[i][ci])
		}
		t.Logf("%s   %10d", line, raw)
	}
}

// compressors are the terminal stages worth comparing: what presage ships,
// what Zucchini and the harness measure with, what bsdiff ships, and an xz
// tuned for a byte stream with no 4-byte alignment.
var compressors = []struct {
	name string
	size func([]byte) int
}{
	{"cz", czLen},
	{"xz -9e", func(b []byte) int { return extLen(b, "xz", "-9e", "-T1", "-c") }},
	{"xz lc0pb0", func(b []byte) int {
		return extLen(b, "xz", "-T1", "-c", "--format=raw", "--lzma2=preset=9e,lc=0,pb=0")
	}},
	{"bzip2 -9", func(b []byte) int { return extLen(b, "bzip2", "-9", "-c") }},
	{"brotli -q11", func(b []byte) int { return extLen(b, "brotli", "-q", "11", "-w", "24", "-c") }},
	{"zstd -22", func(b []byte) int { return extLen(b, "zstd", "--ultra", "-22", "--long=27", "-c", "-q") }},
}

// framePad concatenates streams so each starts on an 8 MiB frame boundary.
func framePad(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
		if n := len(out) % FrameSize; n != 0 {
			out = append(out, make([]byte, FrameSize-n)...)
		}
	}
	return out
}

// interpolativeBits is the exact code length of the binary-interpolative code
// (Moffat-Stuiver) for a sorted position set in [0,n) -- what Percival's
// difference map costs after the BWT and the position enumeration, coded by
// "recursively divide this sequence in half, and encode the value at the
// midpoint using arithmetic compression" (thesis 2.7). An arithmetic coder
// reaches this within a fraction of a bit, so it is a fair price for the
// scheme without building the coder.
func interpolativeBits(pos []int, n int) int64 {
	var bits int64
	var rec func(lo, hi, l, h int) // pos[lo:hi] lies in [l,h]
	rec = func(lo, hi, l, h int) {
		if lo >= hi {
			return
		}
		m := (lo + hi) / 2
		// pos[m] is in [l+(m-lo), h-(hi-1-m)]
		low, high := l+(m-lo), h-(hi-1-m)
		if high > low {
			r := high - low + 1
			b := 0
			for 1<<b < r {
				b++
			}
			bits += int64(b)
		}
		rec(lo, m, l, pos[m]-1)
		rec(m+1, hi, pos[m]+1, h)
	}
	rec(0, len(pos), 0, n-1)
	return bits
}

func extLen(b []byte, prog string, args ...string) int {
	cmd := exec.Command(prog, args...)
	cmd.Stdin = bytes.NewReader(b)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return -1
	}
	return out.Len()
}
