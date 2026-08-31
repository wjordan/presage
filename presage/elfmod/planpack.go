package elfmod

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/zeebo/blake3"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/internal/cz"
)

// The plan ships as one blob inside the patch body, which the container
// compresses jointly with brotli. That is the plan-side twin of what the
// correction was before the CM coder: nearly every byte of it is a byte of a
// LEB128 varint in a column of varints, the column is known, and the general
// compressor is told none of it.
//
// packPlan carves the columns worth carving out of the marshalled plan, codes
// each on its own under the plan contexts (delta/cmplan.go), and leaves the
// rest where it was. A column is carved only where the coder beats brotli-11
// on that column by planCMMinGain -- decode is about a megabyte a second, so a
// column that saves two hundred bytes for a third of a second is not a trade a
// patch applier would take.
//
// The envelope ships each column's *offset in the plan*, so unpacking is a
// splice and nothing about it depends on the walk that chose the columns: a
// column list that is wrong, or a serializer that moves, costs compression and
// cannot cost correctness.
//
//	byte(mode) [ u(planLen) u(nspans) { u(off) u(rawLen) u(zLen) byte(codec)
//	  byte(ctx) [u(pair)] }* u(residueLen) residue z... ]

const (
	planPackNone  byte = 0 // the plan follows whole
	planPackSpans byte = 1 // columns carved out and coded on their own
)

// Per-column context sets, named on the wire so an unknown one is refused.
const (
	ctxGeneric byte = 0 // no interior structure known
	ctxVarint  byte = 1 // a column of LEB128 varints
	ctxPair    byte = 2 // and coded against its index column
)

// planCodecCM names the context-mixing coder in a plan span's table. cz's tags
// are 0..2 and every cz codec is context-free; this one is not, so it is
// dispatched here and cz never sees it. Shared with split.go's numbering so
// one id means one coder across the format.
const planCodecCM byte = 3

// planCMMin is the smallest column offered to the coder; below a few
// kilobytes its adaptive models have not paid for themselves.
const planCMMin = 4 << 10

// Which columns are coded is an encoder-side policy and nothing else: every
// column carries its own codec and context bytes, so the decoder follows the
// table it is given and never has to agree about a threshold. planCMPolicy is
// settable by PRESAGE_PLAN_CM so the size/apply-time frontier can be
// re-measured without a rebuild:
//
//	off      no column is coded; the plan ships as one blob
//	reloc    the relocation stream alone, the single largest win per second
//	<n>      every column that saves at least n bytes
//
// The default is 2000. Apply time is a first-class cost here: the coder
// decodes at about a megabyte a second, so each tier down the ladder buys
// bytes with wall time.
var planCMPolicy, planCMMinGain = func() (string, int) {
	switch v := os.Getenv("PRESAGE_PLAN_CM"); v {
	case "":
		return "gain", 2000
	case "off", "reloc":
		return v, 1000
	default:
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return "gain", n
		}
		return "gain", 2000
	}
}()

// planCMWanted is the policy's verdict on one column, before its measured
// gain is known.
func planCMWanted(c planColumn) bool {
	switch planCMPolicy {
	case "off":
		return false
	case "reloc":
		return c.name == "reloc"
	}
	return true
}

// planColumn is one candidate column of the marshalled plan. name is
// encoder-side only, for the encode's report.
type planColumn struct {
	off, n int
	ctx    byte
	pair   int // index of the paired index column in the list, or -1
	name   string
}

// packPlan is the encoder side. It never fails: a plan it cannot walk, or one
// no column of which earns its decode time, ships whole. The note it returns
// is the ledger of what it coded.
func packPlan(plan []byte) ([]byte, string) {
	cols := planColumns(plan)
	// Every candidate is coded both ways it can be, so the choice below --
	// which depends on what else was carved -- never has to code anything
	// again.
	type coded struct {
		zc       int    // what brotli-11 makes of the column, the incumbent
		plain    []byte // the generic or plain-varint arm
		plainCtx byte
		paired   []byte // the cross-column arm, when the column has an index
	}
	out := make([]coded, len(cols))
	var wg sync.WaitGroup
	for i, c := range cols {
		if c.n < planCMMin || !planCMWanted(c) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			col := plan[c.off : c.off+c.n]
			_, z := cz.Compress(col)
			r := coded{zc: len(z), plainCtx: ctxGeneric}
			if c.ctx != ctxGeneric {
				r.plainCtx = ctxVarint
			}
			var side *delta.CMSide
			if r.plainCtx == ctxVarint {
				side = &delta.CMSide{Varint: true}
			}
			if b, err := delta.CMEncode(col, side); err == nil {
				r.plain = b
			}
			if c.pair >= 0 && c.ctx == ctxPair {
				p := cols[c.pair]
				pair := delta.CMParseVarints(plan[p.off : p.off+p.n])
				if b, err := delta.CMEncode(col, &delta.CMSide{Varint: true, Pair: pair}); err == nil {
					r.paired = b
				}
			}
			out[i] = r
		}()
	}
	wg.Wait()

	// A column coded against another must be able to name it: the paired
	// column is decoded first, so it has to be carved out too and listed
	// before this one. The list is walked in offset order, so a column whose
	// index column ships after it falls back to the plain varint arm.
	at := make([]int, len(cols))
	for i := range at {
		at[i] = -1
	}
	var spans []planColumn
	var zs [][]byte
	var note noteBuilder
	for i, c := range cols {
		r := out[i]
		if r.plain == nil && r.paired == nil {
			continue
		}
		z, ctx, pair := r.plain, r.plainCtx, -1
		if r.paired != nil && len(r.paired) < len(z) {
			if p := at[c.pair]; p >= 0 {
				z, ctx, pair = r.paired, ctxPair, p
			}
		}
		if len(z)+planCMMinGain > r.zc {
			continue
		}
		at[i] = len(spans)
		spans = append(spans, planColumn{off: c.off, n: c.n, ctx: ctx, pair: pair})
		zs = append(zs, z)
		note.add(c.name, c.n, r.zc, len(z), ctx)
	}
	if len(spans) == 0 {
		return append([]byte{planPackNone}, plan...), "plan columns: none beat brotli by " + fmt.Sprint(planCMMinGain)
	}

	b := []byte{planPackSpans}
	b = appendU(b, uint64(len(plan)))
	b = appendU(b, uint64(len(spans)))
	for i, s := range spans {
		b = appendU(b, uint64(s.off))
		b = appendU(b, uint64(s.n))
		b = appendU(b, uint64(len(zs[i])))
		b = append(b, planCodecCM, s.ctx)
		if s.ctx == ctxPair {
			b = appendU(b, uint64(s.pair))
		}
	}
	var residue []byte
	prev := 0
	for _, s := range spans {
		residue = append(residue, plan[prev:s.off]...)
		prev = s.off + s.n
	}
	residue = append(residue, plan[prev:]...)
	b = appendStream(b, residue)
	for _, z := range zs {
		b = append(b, z...)
	}
	// The encoder parses its own plan back once, to build the correction's
	// field context. Priming the cache with what it just packed spares that
	// path a decode of every column it has the answer to already.
	planCacheMu.Lock()
	planCacheKey, planCacheVal = blake3.Sum256(b), plan
	planCacheMu.Unlock()
	return b, note.note()
}

// noteBuilder accumulates the encode's ledger of coded columns: what each one
// cost brotli, what it costs the coder, and how much of the decoder's second
// each one buys.
type noteBuilder struct {
	n, raw, was, now int
	rows             []string
}

func (b *noteBuilder) add(name string, raw, was, now int, ctx byte) {
	b.n, b.raw, b.was, b.now = b.n+1, b.raw+raw, b.was+was, b.now+now
	arm := ""
	if ctx == ctxPair {
		arm = " paired"
	}
	b.rows = append(b.rows, fmt.Sprintf("%s %+d%s", name, now-was, arm))
}

func (b *noteBuilder) note() string {
	return fmt.Sprintf("plan columns: %d coded (%s tier), %d raw B, %d -> %d (%+d); %s",
		b.n, planCMTier(), b.raw, b.was, b.now, b.now-b.was, strings.Join(b.rows, ", "))
}

func planCMTier() string {
	if planCMPolicy == "gain" {
		return "gain>=" + strconv.Itoa(planCMMinGain)
	}
	return planCMPolicy
}

// unpackPlan reverses packPlan. Materialise and DispContext each parse the
// plan, so the result is cached: the coder is slow enough that decoding the
// same columns twice would be most of what the second parse costs.
func unpackPlan(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("empty plan")
	}
	switch b[0] {
	case planPackNone:
		return b[1:], nil
	case planPackSpans:
	default:
		return nil, fmt.Errorf("unsupported plan packing %d", b[0])
	}
	key := blake3.Sum256(b)
	planCacheMu.Lock()
	hit := planCacheKey == key
	val := planCacheVal
	planCacheMu.Unlock()
	if hit {
		return val, nil
	}
	plan, err := decodePlanSpans(b)
	if err != nil {
		return nil, err
	}
	planCacheMu.Lock()
	planCacheKey, planCacheVal = key, plan
	planCacheMu.Unlock()
	return plan, nil
}

var (
	planCacheMu  sync.Mutex
	planCacheKey [32]byte
	planCacheVal []byte
)

func decodePlanSpans(b []byte) ([]byte, error) {
	r := &planReader{b: b[1:]}
	planLen := r.u()
	n := r.u()
	if r.err != nil || planLen > maxPlanLen || n > planLen {
		return nil, errors.New("implausible plan span table")
	}
	spans := make([]planColumn, 0, n)
	zlen := make([]uint64, 0, n)
	var covered uint64
	for i := uint64(0); i < n; i++ {
		off, size, z := r.u(), r.u(), r.u()
		codec, ctx := r.byteAt(), r.byteAt()
		pair := -1
		if ctx == ctxPair {
			p := r.u()
			if r.err != nil || p >= i {
				return nil, errors.New("plan span names a later column as its pair")
			}
			pair = int(p)
		} else if ctx != ctxGeneric && ctx != ctxVarint {
			return nil, fmt.Errorf("unsupported plan column context %d", ctx)
		}
		if codec != planCodecCM {
			return nil, fmt.Errorf("unsupported plan column codec %d", codec)
		}
		if r.err != nil || off > planLen || size == 0 || size > planLen-off {
			return nil, errors.New("plan span lies outside the plan")
		}
		if len(spans) != 0 {
			if last := spans[len(spans)-1]; uint64(last.off+last.n) > off {
				return nil, errors.New("plan spans overlap or are unsorted")
			}
		}
		covered += size
		spans = append(spans, planColumn{off: int(off), n: int(size), ctx: ctx, pair: pair})
		zlen = append(zlen, z)
	}
	residue := r.stream()
	if r.err != nil || uint64(len(residue.b))+covered != planLen {
		return nil, errors.New("plan residue does not fill the plan")
	}
	// The columns form a forest: a ctxPair column is conditioned on an
	// earlier one and nothing else, every other column on nothing at all.
	// So they decode concurrently, each waiting only on the column it names
	// -- at about a megabyte a second, the wall is the depth of the deepest
	// chain, not the sum of the columns.
	cols := make([][]byte, len(spans))
	errs := make([]error, len(spans))
	ready := make([]chan struct{}, len(spans))
	zs := make([][]byte, len(spans))
	for i := range spans {
		ready[i] = make(chan struct{})
		zs[i] = r.take(zlen[i])
		if r.err != nil {
			return nil, errors.New("truncated plan column")
		}
	}
	if !r.done() {
		return nil, errors.New("trailing plan column data")
	}
	var wg sync.WaitGroup
	for i, s := range spans {
		wg.Add(1)
		go func(i int, s planColumn) {
			defer wg.Done()
			defer close(ready[i])
			var side *delta.CMSide
			switch s.ctx {
			case ctxVarint:
				side = &delta.CMSide{Varint: true}
			case ctxPair:
				<-ready[s.pair]
				if errs[s.pair] != nil {
					errs[i] = errs[s.pair]
					return
				}
				side = &delta.CMSide{Varint: true, Pair: delta.CMParseVarints(cols[s.pair])}
			}
			col, err := delta.CMDecode(zs[i], s.n, side)
			if err != nil {
				errs[i] = err
				return
			}
			cols[i] = col
		}(i, s)
	}
	wg.Wait()
	// In column order, so a corrupt plan draws the same error every run.
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	plan := make([]byte, 0, planLen)
	prev := 0
	rest := residue.b
	for i, s := range spans {
		plan = append(plan, rest[:s.off-prev]...)
		rest = rest[s.off-prev:]
		plan = append(plan, cols[i]...)
		prev = s.off + s.n
	}
	return append(plan, rest...), nil
}

// maxPlanLen bounds what a packed plan may declare; the plan of a 4 GiB image
// is orders of magnitude below it.
const maxPlanLen = 1 << 32

// take reads n bytes.
func (r *planReader) take(n uint64) []byte {
	if r.err != nil || n > uint64(len(r.b)) {
		r.err = errors.New("plan truncated")
		return nil
	}
	b := r.b[:n]
	r.b = r.b[n:]
	return b
}

// ---------------------------------------------------------------------------
// the column walk

// planWalk reads the marshalled plan for its column boundaries alone, tracking
// the absolute offset of everything it passes. It is encoder-only and gives up
// silently: what it produces is a hint about where the columns are, and a hint
// that is wrong only costs compression.
type planWalk struct {
	b    []byte
	base int
	at   int
	bad  bool
}

func (w *planWalk) u() uint64 {
	if w.bad {
		return 0
	}
	v, n := binary.Uvarint(w.b[w.at:])
	if n <= 0 {
		w.bad = true
		return 0
	}
	w.at += n
	return v
}

func (w *planWalk) skip(n int) {
	if w.bad || n > len(w.b)-w.at {
		w.bad = true
		return
	}
	w.at += n
}

// stream reads a length-prefixed stream and returns a walk over its contents.
func (w *planWalk) stream() *planWalk {
	n := w.u()
	if w.bad || n > uint64(len(w.b)-w.at) {
		w.bad = true
		return &planWalk{bad: true}
	}
	s := &planWalk{b: w.b[w.at : w.at+int(n)], base: w.base + w.at}
	w.at += int(n)
	return s
}

func (w *planWalk) done() bool { return !w.bad && w.at == len(w.b) }

// planColumns lists the columns of a marshalled plan, in offset order. It
// follows the serializers: planStreams.marshal, then equivalencePlan.marshal,
// predictionPlan.marshalMode with its derived stream, and fieldPlan.marshal.
func planColumns(plan []byte) []planColumn {
	var cols []planColumn
	// add records a column; pairOf is the index in cols of the index column it
	// is a value column of, or -1.
	add := func(s *planWalk, name string, ctx byte, pairOf int) int {
		if s.bad || len(s.b) == 0 {
			return -1
		}
		if ctx == ctxPair && pairOf < 0 {
			ctx = ctxVarint
		}
		cols = append(cols, planColumn{off: s.base, n: len(s.b), ctx: ctx, pair: pairOf, name: name})
		return len(cols) - 1
	}
	top := &planWalk{b: plan}
	eq, structure, choices, reloc := top.stream(), top.stream(), top.stream(), top.stream()
	ehFrame, roData, fields, dwarf := top.stream(), top.stream(), top.stream(), top.stream()
	if !top.done() {
		return nil
	}

	// --- equivalences: geometry, then the four run columns.
	eq.u()
	eq.u()
	nwin := eq.u()
	for i := uint64(0); i < nwin && !eq.bad; i++ {
		for range 6 {
			eq.u()
		}
	}
	eq.skip(1) // the predicted flag
	srcSkip, srcResidual, dstSkip, copyLen := eq.stream(), eq.stream(), eq.stream(), eq.stream()
	add(srcSkip, "eq src-skip", ctxVarint, -1)
	iSrcRes := add(srcResidual, "eq src-residual", ctxVarint, -1)
	iDst := add(dstSkip, "eq dst-skip", ctxVarint, -1)
	add(copyLen, "eq copy-len", ctxPair, iDst)
	// The source residual's index column ships after it, so it can only be
	// named once both are carved: fix the pairing up now that dst-skip has an
	// index, and let packPlan demote it if the order does not work out.
	if iSrcRes >= 0 && iDst >= 0 {
		cols[iSrcRes].ctx, cols[iSrcRes].pair = ctxPair, iDst
	}

	// --- one structural plan per code window.
	for !structure.done() && !structure.bad {
		walkStructure(structure.stream(), add)
	}
	// --- one field plan per code window.
	for !fields.done() && !fields.bad {
		walkFields(fields.stream(), add)
	}
	// --- the layers with no known interior structure, whole.
	for i, s := range []*planWalk{choices, reloc, ehFrame, roData, dwarf} {
		add(s, [...]string{"choices", "reloc", "eh-frame", "rodata", "dwarf"}[i], ctxGeneric, -1)
	}
	// Offset order, with the pair references carried through the permutation.
	order := make([]int, len(cols))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return cols[a].off - cols[b].off })
	rank := make([]int, len(cols))
	for r, i := range order {
		rank[i] = r
	}
	sorted := make([]planColumn, len(cols))
	for r, i := range order {
		c := cols[i]
		if c.pair >= 0 {
			c.pair = rank[c.pair]
		}
		sorted[r] = c
	}
	return sorted
}

type planAdd func(s *planWalk, name string, ctx byte, pairOf int) int

func walkStructure(s *planWalk, add planAdd) {
	s.u() // OldAddr
	s.u() // NewAddr
	s.u() // TargetLen
	if s.bad || s.at >= len(s.b) {
		return
	}
	mode := s.b[s.at]
	s.skip(1)
	s.u() // mapping count
	for _, n := range []string{"map src-index", "map src-offset", "map extent-residual", "map size-delta", "map start-residual"} {
		add(s.stream(), n, ctxVarint, -1)
	}
	add(s.stream(), "map copy bitmap", ctxGeneric, -1)
	if mode == planDerived {
		s.u() // derived enumeration length
		add(s.stream(), "derived boundary", ctxVarint, -1)
		add(s.stream(), "derived suppression", ctxGeneric, -1)
		i := add(s.stream(), "derived size-fix index", ctxVarint, -1)
		add(s.stream(), "derived size-fix value", ctxPair, i)
		s.u() // NewUnits
		s.u() // Align
		s.u() // Maps
		add(s.stream(), "drop runs", ctxVarint, -1)
		for _, n := range [][2]string{
			{"order-exception index", "order-exception source"},
			{"size-delta index", "size-delta value"},
			{"insert position", "insert size"},
			{"layout-fixup index", "layout-fixup value"},
		} {
			i := add(s.stream(), n[0], ctxVarint, -1)
			add(s.stream(), n[1], ctxPair, i)
		}
		add(s.stream(), "unrepresentable maps", ctxGeneric, -1)
	}
	s.u() // point count
	i := add(s.stream(), "point index-delta", ctxVarint, -1)
	add(s.stream(), "point offset", ctxPair, i)
	add(s.stream(), "point shift-delta", ctxPair, i)
	// The range map is inline varints, not a stream: it stays where it is.
}

func walkFields(s *planWalk, add planAdd) {
	if s.bad || s.at >= len(s.b) {
		return
	}
	basis := s.b[s.at]
	s.skip(1)
	i := add(s.stream(), "field remap-index", ctxVarint, -1)
	add(s.stream(), "field remap-target", ctxPair, i)
	if basis == remapIndexBasis {
		add(s.stream(), "field remap-escape", ctxVarint, -1)
		add(s.stream(), "field remap-tag", ctxGeneric, -1)
	}
	j := add(s.stream(), "field field-index", ctxVarint, -1)
	add(s.stream(), "field field-delta", ctxPair, j)
}
