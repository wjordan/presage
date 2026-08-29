package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"sync"

	"github.com/wjordan/go-binsync/delta"
	"github.com/wjordan/go-binsync/delta/x86"
)

// attributeCorrection reports where a whole-image correction is actually
// spent. Optimising the codec without this is guesswork: the aggregate says
// only that 1% of the image is wrong, not which region is paying for it.
//
// Each region is corrected independently, so the sizes do not sum exactly to
// the whole-image correction -- a per-region encoder cannot share matches
// across regions -- but the attribution is what decides where to aim.
// wrongShape describes *how* a region is wrong, which decides what kind of
// model could fix it. Scattered runs of four or eight bytes on an aligned
// stride are an address column; long runs are changed content.
func wrongShape(name string, pred, target []byte) {
	var runs, addr4, addr8 int
	hist := map[int]int{}
	for i := 0; i < len(target); {
		if pred[i] == target[i] {
			i++
			continue
		}
		j := i
		for j < len(target) && pred[j] != target[j] {
			j++
		}
		n := j - i
		runs++
		switch {
		case n <= 4 && i%4 == 0:
			addr4++
		case n <= 8 && i%8 == 0:
			addr8++
		}
		switch {
		case n <= 4:
			hist[4]++
		case n <= 8:
			hist[8]++
		case n <= 32:
			hist[32]++
		case n <= 256:
			hist[256]++
		default:
			hist[0]++
		}
		i = j
	}
	fmt.Fprintf(os.Stderr, "  shape of %s: %d wrong runs; <=4B %d, <=8B %d, <=32B %d, <=256B %d, longer %d; aligned 4B-runs %d, aligned 8B-runs %d\n",
		name, runs, hist[4], hist[8], hist[32], hist[256], hist[0], addr4, addr8)
}

// correctionShapes compares the production correction encoder against a plain
// positional encoding of the same edits -- gap, length, then the bytes. The
// residual here is 1.17M runs averaging 1.4 bytes, which is close to the worst
// case for a matcher, so it is worth knowing whether the encoder or the edit
// list is the floor.
// It returns its line rather than printing it, so the callers can measure
// several sections at once and still print them in a stable order.
func correctionShapes(name string, pred, target, correction []byte) string {
	c, err := encodeColumnar(pred, target)
	if err != nil {
		return ""
	}
	z := xzSizes(append([][]byte{correction, c.Gaps, c.Lens}, c.Bytes[:]...)...)
	b, buckets := 0, ""
	for i, n := range z[3:] {
		b += n
		if i > 0 {
			buckets += "+"
		}
		buckets += fmt.Sprint(n)
	}
	return fmt.Sprintf("  encoding of %s: correction xz %d; positional gaps %d + lengths %d + bytes (%s) %d = %d\n",
		name, z[0], z[1], z[2], buckets, b, z[1]+z[2]+b)
}

func attributeCorrection(pred, target []byte, secs map[string]section, text section, maps []mapping) {
	type row struct {
		name    string
		size    uint64
		wrong   int
		corr    int
		corrXZ  int
		percent float64
	}
	// One EncodeCorrection and one xz per section, all independent: on a
	// whole image that is thirty-odd serial single-core compressions, so they
	// run together and the ordered output is assembled afterwards.
	names := make([]string, 0, len(secs))
	for name := range secs {
		names = append(names, name)
	}
	slices.Sort(names)
	rows := make([]row, len(names))
	shapes := make([]string, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		s := secs[name]
		if s.Size == 0 || s.Off+s.Size > uint64(len(target)) || s.Off+s.Size > uint64(len(pred)) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := pred[s.Off : s.Off+s.Size]
			t := target[s.Off : s.Off+s.Size]
			wrong := 0
			for j := range t {
				if p[j] != t[j] {
					wrong++
				}
			}
			if wrong == 0 {
				return
			}
			corr, err := delta.EncodeCorrection(p, t)
			if err != nil {
				return
			}
			rows[i] = row{name, s.Size, wrong, len(corr), xzSize(corr), 100 * float64(wrong) / float64(s.Size)}
			if s.Size > 1<<20 {
				shapes[i] = correctionShapes(name, p, t, corr)
			}
		}()
	}
	wg.Wait()
	for _, line := range shapes {
		if line != "" {
			fmt.Fprint(os.Stderr, line)
		}
	}
	rows = slices.DeleteFunc(rows, func(r row) bool { return r.name == "" })
	textResidualSplit(pred, target, text, maps)
	roDataResidualKind(pred, target, secs)
	sort.Slice(rows, func(i, j int) bool { return rows[i].corrXZ > rows[j].corrXZ })
	for _, r := range rows[:min(3, len(rows))] {
		s := secs[r.name]
		wrongShape(r.name, pred[s.Off:s.Off+s.Size], target[s.Off:s.Off+s.Size])
	}
	fmt.Fprintf(os.Stderr, "correction by region:\n")
	total := 0
	for _, r := range rows {
		total += r.corrXZ
		fmt.Fprintf(os.Stderr, "  %-20s %11d B  %9d wrong (%6.3f%%)  correction %9d -> xz %8d\n",
			r.name, r.size, r.wrong, r.percent, r.corr, r.corrXZ)
	}
	fmt.Fprintf(os.Stderr, "  %-20s %11s  %9s            %20s xz %8d\n", "TOTAL", "", "", "", total)
}

// textResidualSplit answers the question the .text residual poses: are the
// wrong bytes inside the PC-relative fields the walker rewrote, or outside
// them? Inside means the address model is wrong and a better one can fix it;
// outside means the content prediction is wrong and only a better source
// choice can. The two want completely different work, so guessing is expensive.
func textResidualSplit(pred, target []byte, text section, maps []mapping) {
	if text.Size == 0 || text.Off+text.Size > uint64(len(pred)) {
		return
	}
	p := pred[text.Off : text.Off+text.Size]
	t := target[text.Off : text.Off+text.Size]
	field := make([]bool, len(p))
	var fieldBytes int
	for _, m := range maps {
		if m.Dst > uint64(len(p)) || m.DstSize > uint64(len(p))-m.Dst {
			continue
		}
		base := int(m.Dst)
		x86.WalkReferences(p[base:base+int(m.DstSize)], 0, func(ref x86.Reference) {
			for i := base + ref.Off; i < base+ref.Off+ref.N && i < len(field); i++ {
				if !field[i] {
					field[i], fieldBytes = true, fieldBytes+1
				}
			}
		})
	}
	var inField, outField, wrongInsts int
	var lastRunOutside bool
	for i := range t {
		if p[i] == t[i] {
			lastRunOutside = false
			continue
		}
		if field[i] {
			inField++
		} else {
			outField++
			if !lastRunOutside {
				wrongInsts++
			}
			lastRunOutside = true
		}
	}
	fmt.Fprintf(os.Stderr, "  .text residual: %d wrong bytes; %d inside relocated fields (%.1f%%), %d outside in %d runs; fields cover %d bytes (%.2f%% of section)\n",
		inField+outField, inField, 100*float64(inField)/float64(max(1, inField+outField)),
		outField, wrongInsts, fieldBytes, 100*float64(fieldBytes)/float64(len(p)))
}

// roDataResidualKind asks what the .rodata residual still is after the switch
// tables are modelled. 85.5% of its wrong runs start on a four-byte boundary,
// so the question is what those words point at: adding the word's own address
// gives a self-relative pointer, and the section it lands in names the table.
func roDataResidualKind(pred, target []byte, secs map[string]section) {
	ro, ok := secs[".rodata"]
	if !ok || ro.Off+ro.Size > uint64(len(target)) {
		return
	}
	within := func(addr uint64) string {
		for name, s := range secs {
			if s.Size != 0 && addr >= s.Addr && addr < s.Addr+s.Size {
				return name
			}
		}
		return ""
	}
	p := pred[ro.Off : ro.Off+ro.Size]
	t := target[ro.Off : ro.Off+ro.Size]
	kinds := map[string]int{}
	var words, wrongWords int
	for i := 0; i+4 <= len(t); i += 4 {
		words++
		if bytes.Equal(p[i:i+4], t[i:i+4]) {
			continue
		}
		wrongWords++
		v := int32(binary.LittleEndian.Uint32(t[i:]))
		self := uint64(int64(ro.Addr+uint64(i)) + int64(v))
		if name := within(self); name != "" {
			kinds["self->"+name]++
			continue
		}
		if name := within(uint64(v)); name != "" && v > 0 {
			kinds["absolute->"+name]++
			continue
		}
		kinds["neither"]++
	}
	fmt.Fprintf(os.Stderr, "  .rodata residual: %d of %d aligned words wrong (%.3f%%)\n",
		wrongWords, words, 100*float64(wrongWords)/float64(max(1, words)))
	for _, k := range slices.Sorted(maps.Keys(kinds)) {
		fmt.Fprintf(os.Stderr, "    %-24s %8d (%.1f%%)\n", k, kinds[k], 100*float64(kinds[k])/float64(max(1, wrongWords)))
	}
}

// planColumnCost prices the destination side of the function map under both
// parameterisations. Only two of {start delta, leading gap, length} need to be
// shipped, and the choice is not neutral to a compressor: a start delta is the
// previous function's length plus this one's gap, so a start column mixes
// smooth code lengths with alignment padding that is almost always one of
// sixteen values. This says whether separating them is worth a format change.
func planColumnCost(p predictionPlan) {
	maps := slices.Clone(p.Maps)
	slices.SortFunc(maps, func(a, b mapping) int { return cmpU(a.Dst, b.Dst) })
	var starts, gaps, sizes []byte
	var prevEnd, prevStart uint64
	for _, m := range maps {
		starts = binary.AppendUvarint(starts, m.Dst-prevStart)
		gaps = binary.AppendUvarint(gaps, m.Dst-prevEnd)
		sizes = binary.AppendUvarint(sizes, m.DstSize)
		prevEnd, prevStart = m.Dst+m.DstSize, m.Dst
	}
	z := xzSizes(starts, gaps, sizes)
	st, g, sz := z[0], z[1], z[2]
	fmt.Fprintf(os.Stderr, "plan columns: starts %d + gaps %d = %d; sizes %d + gaps %d = %d\n",
		st, g, st+g, sz, g, sz+g)
	// A gap is usually just the padding that aligns the next function, which
	// the decoder can compute for itself from where the previous one ended.
	// If so the column only has to carry where that guess is wrong.
	for _, align := range []uint64{16, 32} {
		var residuals []byte
		exact := 0
		prevEnd = 0
		for _, m := range maps {
			guess := (align - prevEnd%align) % align
			gap := m.Dst - prevEnd
			if gap == guess {
				exact++
			}
			residuals = binary.AppendVarint(residuals, int64(gap)-int64(guess))
			prevEnd = m.Dst + m.DstSize
		}
		fmt.Fprintf(os.Stderr, "  gaps against %d-byte alignment: %d of %d exact (%.3f%%); residual xz %d\n",
			align, exact, len(maps), 100*float64(exact)/float64(max(1, len(maps))), xzSize(residuals))
	}
}

// planComponents prices the combined plan one stream at a time. The numbers
// are standalone rather than marginal -- §9.3 -- so they navigate rather than
// budget, but they say which stream a change actually moved.
func planComponents(planBytes []byte) {
	cp, err := unmarshalCombinedPlan(planBytes)
	if err != nil {
		return
	}
	type stream struct {
		name string
		b    []byte
	}
	// Every column below is independent, so they are compressed together and
	// printed afterwards: serially this was a couple of dozen one-core xz runs
	// on every measured rung.
	streams := []stream{{"", planBytes}, {"equivalences", cp.Equivalences}, {"structure", cp.Structure},
		{"choices", cp.Choices}, {"reloc", cp.Reloc}, {"eh_frame", cp.EhFrame}, {"rodata", cp.RoData},
		{"go_tables", cp.GoTables}}
	const planStreams = 8
	ep, epErr := parseEquivalencePlan(cp.Equivalences)
	if epErr == nil {
		streams = append(streams, stream{"dst", ep.DstSkip}, stream{"len", ep.CopyLen},
			stream{"src-skip", ep.SrcSkip}, stream{"src-residual", ep.SrcResidual})
	}
	fpErr := errors.New("no field plan")
	if len(cp.Fields) != 0 {
		var fp fieldPlan
		if fp, fpErr = unmarshalFieldPlan(cp.Fields); fpErr == nil {
			streams = append(streams, stream{"remap-index", fp.RemapIndex}, stream{"remap-shift", fp.RemapShift},
				stream{"field-index", fp.FieldIndex}, stream{"field-delta", fp.FieldDelta})
		}
	}
	bs := make([][]byte, len(streams))
	for i, s := range streams {
		bs[i] = s.b
	}
	z := xzSizes(bs...)
	fmt.Fprintf(os.Stderr, "  plan streams (standalone xz): total %d;", z[0])
	for i := 1; i < planStreams; i++ {
		if len(streams[i].b) != 0 {
			fmt.Fprintf(os.Stderr, " %s %d", streams[i].name, z[i])
		}
	}
	fmt.Fprintln(os.Stderr)
	at := planStreams
	if epErr == nil {
		fmt.Fprintf(os.Stderr, "  equivalence columns (standalone xz): dst %d, len %d, src-skip %d, src-residual %d\n",
			z[at], z[at+1], z[at+2], z[at+3])
		at += 4
	}
	if fpErr == nil {
		fmt.Fprintf(os.Stderr, "  field columns (standalone xz): remap-index %d, remap-shift %d, field-index %d, field-delta %d\n",
			z[at], z[at+1], z[at+2], z[at+3])
	}
}

// probeCorrectionDictionary asks the question that has paid seven times, of
// the one layer it has never been asked of. The plan learned to name what the
// old image already says; the byte correction still states its replacement
// bytes outright, even though the decoder holds the whole predicted image
// while it applies them. Long runs are new instruction sequences, and .text is
// full of near-identical code -- so a coder that could reference the
// prediction might not have to spell them out.
//
// The measurement is the marginal cost of the run bytes after the prediction:
// xz of dictionary+bytes less xz of the dictionary alone. xz -9 looks back 64
// MiB, so the tail of the prediction is the dictionary a real implementation
// would get.
func probeCorrectionDictionary(pred, target []byte, window int) {
	var long []byte
	runs := 0
	for i := 0; i < len(target); {
		if pred[i] == target[i] {
			i++
			continue
		}
		j := i
		for j < len(target) && pred[j] != target[j] {
			j++
		}
		if j-i >= correctionBuckets {
			long = append(long, target[i:j]...)
			runs++
		}
		i = j
	}
	dict := pred
	if len(dict) > window {
		dict = dict[len(dict)-window:]
	}
	alone := xzSizeContiguous(long)
	base := xzSizeContiguous(dict)
	with := xzSizeContiguous(append(slices.Clone(dict), long...))
	fmt.Fprintf(os.Stderr, "  probe correction dictionary: %d long runs, %d bytes; alone xz %d, marginal after %d-byte prediction %d\n",
		runs, len(long), alone, len(dict), with-base)
}
