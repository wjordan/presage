package main

import (
	"encoding/binary"
	"errors"
	"slices"
	"sort"

	"github.com/wjordan/go-binsync/delta/x86"
)

// The DWARF layer models the fields in .debug_info that shift when a
// compilation unit changes size: DW_FORM_ref_addr type references (section
// offsets into .debug_info itself), DW_FORM_sec_offset references into the
// line, location, range and address tables, and DW_FORM_addr code addresses.
// The decoder locates every field by walking the old section's DIEs with the
// old abbreviation table, projects its position and its value into the new
// image, and writes the projected value. The plan carries the geometry of the
// debug sections in both images and the pairing of compilation units, which
// is all the decoder needs when there are no equivalences to place the bytes.

const (
	dwInfo = iota
	dwAbbrev
	dwLine
	dwLoclists
	dwRnglists
	dwAddr
	dwFrame
	dwSymtab
	dwStrtab
	dwarfSecCount
)

var dwarfSecNames = [dwarfSecCount]string{".debug_info", ".debug_abbrev", ".debug_line", ".debug_loclists", ".debug_rnglists", ".debug_addr", ".debug_frame", ".symtab", ".strtab"}

// recordSecs are the sections made of length-prefixed records whose
// pairing the plan can carry: units, line programs, list contributions,
// call-frame entries.
var recordSecs = []int{dwInfo, dwLine, dwLoclists, dwRnglists, dwAddr, dwFrame, dwSymtab, dwStrtab}

type debugSec struct{ OldOff, OldSize, NewOff, NewSize uint64 }

// unitPair is one old record and where it lands in the new section;
// NewLen is zero for a record with no counterpart. Unit marks a record that
// starts a length-prefixed unit (a compilation unit's header, a line
// program, a list contribution's header, a call-frame entry), and UnitLen
// is that unit's new length when it is not the record's own.
type unitPair struct {
	OldOff, OldLen, NewOff, NewLen uint64
	Unit                           bool
	UnitLen                        uint64
}

// dwarfPlan is the geometry of the debug sections in both images and, per
// record section, the pairing of old records with new ones. Units (the
// .debug_info table) is always present; the others are shipped only when
// no equivalences place the bytes, since they exist to project positions.
type dwarfPlan struct {
	Secs    [dwarfSecCount]debugSec
	Records [dwarfSecCount][]unitPair
	// Units is the unit pairing of .debug_info before splitting; Split
	// holds, per old unit index, the DIE records that replace it in
	// Records[dwInfo]. Encoder-side only; the decoder rebuilds Records.
	Units []unitPair
	Split map[int][]unitPair
}

func (p dwarfPlan) marshal() []byte {
	var b []byte
	for _, s := range p.Secs {
		for _, v := range []uint64{s.OldOff, s.OldSize, s.NewOff, s.NewSize} {
			b = appendU(b, v)
		}
	}
	var present uint64
	for _, k := range recordSecs {
		if p.Records[k] != nil {
			present |= 1 << k
		}
	}
	b = appendU(b, present)
	for _, k := range recordSecs {
		if p.Records[k] == nil {
			continue
		}
		var newPos uint64
		if k == dwInfo {
			b = marshalRecordTable(b, p.Units, &newPos, false, func(i int) []unitPair { return p.Split[i] })
		} else {
			b = marshalRecordTable(b, p.Records[k], &newPos, k == dwLoclists || k == dwRnglists, func(int) []unitPair { return nil })
		}
	}
	return b
}

// marshalRecordTable writes, per old record: 0 for no counterpart, 1 or 2
// for a counterpart without or with a gap before it (then the gap), then
// the length change; 3 says the record is split into the sub-records that
// split gives, whose table follows. New offsets follow from the lengths.
// The records of one table are in old order and their counterparts in new
// order; a split record's sub-records take its place in that order.
// A unit-starting record whose unit is longer than itself (a split unit's
// header, a list contribution's header) is followed by the unit's length
// change when unitExtra says the table carries them.
func marshalRecordTable(b []byte, recs []unitPair, newPos *uint64, unitExtra bool, split func(i int) []unitPair) []byte {
	for i, u := range recs {
		if sub := split(i); sub != nil {
			b = appendU(b, 3)
			b = marshalRecordTable(b, sub, newPos, true, func(int) []unitPair { return nil })
			continue
		}
		if u.NewLen == 0 {
			b = appendU(b, 0)
			continue
		}
		if u.NewOff > *newPos {
			b = appendU(b, 2)
			b = appendU(b, u.NewOff-*newPos)
		} else {
			b = appendU(b, 1)
		}
		b = appendS(b, int64(u.NewLen)-int64(u.OldLen))
		if unitExtra && u.Unit {
			b = appendS(b, int64(u.UnitLen)-int64(u.NewLen))
		}
		*newPos = u.NewOff + u.NewLen
	}
	return b
}

// unmarshalRecordTable reads a table over oldRecs, flattening split records
// through split, which gives the old sub-records of record i.
func unmarshalRecordTable(r *planReader, oldRecs []unitPair, newSize uint64, newPos *uint64, unitExtra bool, split func(i int) ([]unitPair, error)) ([]unitPair, error) {
	recs := make([]unitPair, 0, len(oldRecs))
	for i, u := range oldRecs {
		m := r.u()
		switch {
		case m == 0:
			recs = append(recs, unitPair{OldOff: u.OldOff, OldLen: u.OldLen, Unit: u.Unit})
			continue
		case m == 3:
			if split == nil {
				return nil, errors.New("DWARF plan splits a record that cannot be split")
			}
			sub, err := split(i)
			if err != nil {
				return nil, err
			}
			if sub, err = unmarshalRecordTable(r, sub, newSize, newPos, true, nil); err != nil {
				return nil, err
			}
			recs = append(recs, sub...)
			continue
		case m == 2:
			*newPos += r.u()
		}
		newLen := int64(u.OldLen) + r.s()
		if r.err != nil || m > 2 || newLen <= 0 || *newPos+uint64(newLen) > newSize {
			return nil, errors.New("invalid DWARF plan record table")
		}
		rec := unitPair{u.OldOff, u.OldLen, *newPos, uint64(newLen), u.Unit, uint64(newLen)}
		if unitExtra && u.Unit {
			unitLen := newLen + r.s()
			if r.err != nil || unitLen < newLen || *newPos+uint64(unitLen) > newSize {
				return nil, errors.New("invalid DWARF plan unit length")
			}
			rec.UnitLen = uint64(unitLen)
		}
		recs = append(recs, rec)
		*newPos += uint64(newLen)
	}
	return recs, nil
}

// unmarshalDwarfPlan needs the old image to recover the record lengths.
func unmarshalDwarfPlan(b, old []byte) (dwarfPlan, error) {
	r := &planReader{b: b}
	var p dwarfPlan
	for i := range p.Secs {
		p.Secs[i] = debugSec{r.u(), r.u(), r.u(), r.u()}
		if s := p.Secs[i]; r.err != nil || s.OldOff+s.OldSize > uint64(len(old)) {
			return dwarfPlan{}, errors.New("invalid DWARF plan geometry")
		}
	}
	present := r.u()
	for _, k := range recordSecs {
		if present&(1<<k) == 0 {
			continue
		}
		s := p.Secs[k]
		sec := old[s.OldOff : s.OldOff+s.OldSize]
		oldRecs, err := recordBoundsIn(k, old, p.Secs)
		if err != nil {
			return dwarfPlan{}, err
		}
		var split func(int) ([]unitPair, error)
		if k == dwInfo {
			abbrev := p.Secs[dwAbbrev]
			split = func(i int) ([]unitPair, error) {
				recs, _, err := dieBounds(sec, old[abbrev.OldOff:abbrev.OldOff+abbrev.OldSize], oldRecs[i])
				return recs, err
			}
		}
		var newPos uint64
		if p.Records[k], err = unmarshalRecordTable(r, oldRecs, s.NewSize, &newPos, k == dwLoclists || k == dwRnglists, split); err != nil {
			return dwarfPlan{}, err
		}
	}
	if !r.done() {
		return dwarfPlan{}, errors.New("trailing DWARF plan data")
	}
	return p, nil
}

// unitBounds lists the compilation units of a .debug_info section as
// (offset, length) pairs; only the 32-bit DWARF format is understood.
func unitBounds(info []byte) ([]unitPair, error) {
	var units []unitPair
	for off := uint64(0); off < uint64(len(info)); {
		if off+4 > uint64(len(info)) {
			return nil, errors.New("truncated DWARF unit header")
		}
		n := uint64(binary.LittleEndian.Uint32(info[off:]))
		if n >= 0xfffffff0 || off+4+n > uint64(len(info)) {
			return nil, errors.New("unsupported or truncated DWARF unit")
		}
		units = append(units, unitPair{OldOff: off, OldLen: 4 + n, Unit: true})
		off += 4 + n
	}
	return units, nil
}

// recordBoundsIn is recordBounds over section k of an image; the address
// table's records are the per-unit groups the units' addr_base name.
func recordBoundsIn(k int, data []byte, secs [dwarfSecCount]debugSec) ([]unitPair, error) {
	s := secs[k]
	sec := data[s.OldOff : s.OldOff+s.OldSize]
	if k != dwAddr {
		return recordBounds(k, sec)
	}
	info, abbrev := secs[dwInfo], secs[dwAbbrev]
	units, err := unitBounds(data[info.OldOff : info.OldOff+info.OldSize])
	if err != nil {
		return nil, err
	}
	var bases []uint64
	for _, u := range units {
		_, _, base, ok := unitAttrs(data[info.OldOff:info.OldOff+info.OldSize], data[abbrev.OldOff:abbrev.OldOff+abbrev.OldSize], u)
		if ok {
			bases = append(bases, base)
		}
	}
	return addrBounds(sec, bases)
}

// addrBounds splits an address table into contribution headers and the
// groups starting at each unit's addr_base.
func addrBounds(sec []byte, bases []uint64) ([]unitPair, error) {
	units, err := unitBounds(sec)
	if err != nil {
		return nil, err
	}
	slices.Sort(bases)
	bases = slices.Compact(bases)
	var recs []unitPair
	bi := 0
	for _, u := range units {
		end := u.OldOff + u.OldLen
		recs = append(recs, unitPair{OldOff: u.OldOff, OldLen: min(8, u.OldLen), Unit: true})
		start := u.OldOff + min(8, u.OldLen)
		for bi < len(bases) && bases[bi] < start {
			bi++
		}
		for ; bi < len(bases) && bases[bi] < end; bi++ {
			if bases[bi] > start {
				recs = append(recs, unitPair{OldOff: start, OldLen: bases[bi] - start})
				start = bases[bi]
			}
		}
		if start < end {
			recs = append(recs, unitPair{OldOff: start, OldLen: end - start})
		}
	}
	return recs, nil
}

// recordBounds splits a section into the records its table is over:
// units, line programs and call-frame entries by their length prefix,
// location and range lists by their end-of-list entries.
func recordBounds(k int, sec []byte) ([]unitPair, error) {
	switch k {
	case dwLoclists, dwRnglists:
		return listBounds(sec, k == dwLoclists)
	case dwSymtab:
		var recs []unitPair
		for off := uint64(0); off+24 <= uint64(len(sec)); off += 24 {
			recs = append(recs, unitPair{OldOff: off, OldLen: 24})
		}
		return recs, nil
	case dwStrtab:
		var recs []unitPair
		for off := uint64(0); off < uint64(len(sec)); {
			n := uint64(slices.Index(sec[off:], 0)) + 1
			if n == 0 {
				n = uint64(len(sec)) - off
			}
			recs = append(recs, unitPair{OldOff: off, OldLen: n})
			off += n
		}
		return recs, nil
	}
	return unitBounds(sec)
}

// listBounds splits a DWARF 5 list section into its contribution headers
// (with their offset tables) and the lists in them.
func listBounds(sec []byte, loc bool) ([]unitPair, error) {
	le := binary.LittleEndian
	var recs []unitPair
	units, err := unitBounds(sec)
	if err != nil {
		return nil, err
	}
	for _, u := range units {
		if u.OldLen < 12 {
			return nil, errors.New("short list contribution header")
		}
		addrSize := sec[u.OldOff+6]
		hdr := 12 + 4*uint64(le.Uint32(sec[u.OldOff+8:]))
		if hdr > u.OldLen {
			return nil, errors.New("list offset table exceeds the contribution")
		}
		recs = append(recs, unitPair{OldOff: u.OldOff, OldLen: hdr, Unit: true})
		r := &lebReader{b: sec[:u.OldOff+u.OldLen], pos: u.OldOff + hdr}
		start := r.pos
		for r.pos < uint64(len(r.b)) && !r.err {
			kind := r.byte()
			var operands int // uleb operands
			var addrs int
			switch {
			case kind == 0: // end of list
				recs = append(recs, unitPair{OldOff: start, OldLen: r.pos - start})
				start = r.pos
				continue
			case kind == 1: // base_addressx
				operands = 1
			case kind == 2, kind == 3, kind == 4: // startx_endx, startx_length, offset_pair
				operands = 2
			case kind == 5 && loc: // default_location
			case kind == 5 && !loc: // base_address
				addrs = 1
			case kind == 6 && loc, kind == 7 && !loc: // base_address / start_end
				if loc {
					addrs = 1
				} else {
					addrs = 2
				}
			case kind == 7 && loc: // start_end
				addrs = 2
			case kind == 8 && loc: // start_length
				addrs, operands = 1, 1
			default:
				return nil, errors.New("unknown list entry kind")
			}
			r.pos += uint64(addrs) * uint64(addrSize)
			for range operands {
				r.uleb()
			}
			// Location entries other than base addresses carry a
			// counted description.
			if loc && kind != 1 && kind != 6 {
				r.pos += r.uleb()
			}
		}
		if r.err || r.pos != uint64(len(r.b)) {
			return nil, errors.New("malformed list contribution")
		}
		if start != r.pos {
			recs = append(recs, unitPair{OldOff: start, OldLen: r.pos - start})
		}
	}
	return recs, nil
}

// dieBounds splits a unit into its header and one record per DIE (a DIE's
// trailing null entries belong to it), with a key per record for pairing:
// the abbreviation code, mixed with the name when there is one.
func dieBounds(info, abbrev []byte, u unitPair) ([]unitPair, []uint64, error) {
	d, err := parseUnitHeader(info, u)
	if err != nil {
		return nil, nil, err
	}
	recs := []unitPair{{OldOff: u.OldOff, OldLen: d.dies - u.OldOff, Unit: true}}
	keys := []uint64{0}
	var key uint64
	err = walkDIEs(info, abbrev, d, func(off uint64, code uint64, a dwarfAbbrev) {
		recs = append(recs, unitPair{OldOff: off})
		key = code*0x9e3779b97f4a7c15 + a.tag
		keys = append(keys, key)
	}, func(pos uint64, at dwarfAttr, _ uint8, r *lebReader) {
		if at.name == 0x03 && at.form == 0x08 {
			if end := slices.Index(r.b[pos:], 0); end > 0 {
				keys[len(keys)-1] = key ^ hashBytes(r.b[pos:pos+uint64(end)])
			}
		}
	})
	if err != nil {
		return nil, nil, err
	}
	end := u.OldOff + u.OldLen
	for i := len(recs) - 1; i >= 0; i-- {
		recs[i].OldLen = end - recs[i].OldOff
		end = recs[i].OldOff
	}
	return recs, keys, nil
}

func hashBytes(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b {
		h = (h ^ uint64(c)) * 1099511628211
	}
	return h
}

// pairByDiff pairs two key sequences in order, returning the new index of
// each old element or -1. Keys unique on both sides anchor the pairing
// (the longest increasing chain of them); between anchors, equal keys pair
// in order. Linear space, so a million list records cost nothing to speak of.
func pairByDiff(a, b []uint64) []int {
	pairing := make([]int, len(a))
	for i := range pairing {
		pairing[i] = -1
	}
	count := func(keys []uint64) map[uint64]int {
		m := make(map[uint64]int, len(keys))
		for _, k := range keys {
			m[k]++
		}
		return m
	}
	ca, cb := count(a), count(b)
	posB := make(map[uint64]int, len(b))
	for j, k := range b {
		if cb[k] == 1 && ca[k] == 1 {
			posB[k] = j
		}
	}
	// Candidate anchors in old order; keep the longest increasing chain of
	// new indices (patience sorting).
	type cand struct{ i, j int }
	var cands []cand
	for i, k := range a {
		if j, ok := posB[k]; ok {
			cands = append(cands, cand{i, j})
		}
	}
	tails := []int{} // index into cands of the chain end per length
	prev := make([]int, len(cands))
	for c := range cands {
		lo, hi := 0, len(tails)
		for lo < hi {
			mid := (lo + hi) / 2
			if cands[tails[mid]].j < cands[c].j {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		prev[c] = -1
		if lo > 0 {
			prev[c] = tails[lo-1]
		}
		if lo == len(tails) {
			tails = append(tails, c)
		} else {
			tails[lo] = c
		}
	}
	var anchors []cand
	if len(tails) != 0 {
		for c := tails[len(tails)-1]; c >= 0; c = prev[c] {
			anchors = append(anchors, cands[c])
		}
		slices.Reverse(anchors)
	}
	anchors = append(anchors, cand{len(a), len(b)})
	// Between anchors, walk both sides pairing equal keys in order.
	ai, bj := 0, 0
	for _, an := range anchors {
		for ai < an.i && bj < an.j {
			if a[ai] == b[bj] {
				pairing[ai] = bj
				ai++
				bj++
				continue
			}
			// Skip whichever side's key does not occur ahead on the other
			// before the anchor; when both do, skip the old one.
			if ca[b[bj]] == 0 {
				bj++
			} else {
				ai++
			}
		}
		if an.i < len(a) {
			pairing[an.i] = an.j
		}
		ai, bj = an.i+1, an.j+1
	}
	return pairing
}

// dwarfUnit is the parsed header of one compilation unit.
type dwarfUnit struct {
	off, length uint64
	version     uint16
	addrSize    uint8
	abbrevOff   uint64
	dies        uint64 // offset of the first DIE within the section
}

func parseUnitHeader(info []byte, u unitPair) (dwarfUnit, error) {
	le := binary.LittleEndian
	b := info[u.OldOff : u.OldOff+u.OldLen]
	if len(b) < 11 {
		return dwarfUnit{}, errors.New("short DWARF unit header")
	}
	d := dwarfUnit{off: u.OldOff, length: u.OldLen, version: le.Uint16(b[4:])}
	switch {
	case d.version >= 5:
		if len(b) < 12 {
			return dwarfUnit{}, errors.New("short DWARF 5 unit header")
		}
		d.addrSize, d.abbrevOff, d.dies = b[7], uint64(le.Uint32(b[8:])), u.OldOff+12
	case d.version >= 2:
		d.abbrevOff, d.addrSize, d.dies = uint64(le.Uint32(b[6:])), b[10], u.OldOff+11
	default:
		return dwarfUnit{}, errors.New("unsupported DWARF version")
	}
	return d, nil
}

type dwarfAttr struct {
	name, form uint64
	implicit   int64
}

type dwarfAbbrev struct {
	tag      uint64
	children bool
	attrs    []dwarfAttr
}

// parseAbbrevs reads one abbreviation table.
func parseAbbrevs(abbrev []byte, off uint64) (map[uint64]dwarfAbbrev, error) {
	if off > uint64(len(abbrev)) {
		return nil, errors.New("abbreviation offset outside .debug_abbrev")
	}
	r := &lebReader{b: abbrev, pos: off}
	table := make(map[uint64]dwarfAbbrev)
	for {
		code := r.uleb()
		if code == 0 || r.err {
			break
		}
		a := dwarfAbbrev{tag: r.uleb(), children: r.byte() != 0}
		for {
			name, form := r.uleb(), r.uleb()
			if name == 0 && form == 0 {
				break
			}
			at := dwarfAttr{name: name, form: form}
			if form == 0x21 { // DW_FORM_implicit_const
				at.implicit = r.sleb()
			}
			a.attrs = append(a.attrs, at)
		}
		table[code] = a
	}
	if r.err {
		return nil, errors.New("truncated abbreviation table")
	}
	return table, nil
}

type lebReader struct {
	b   []byte
	pos uint64
	err bool
}

func (r *lebReader) byte() byte {
	if r.pos >= uint64(len(r.b)) {
		r.err = true
		return 0
	}
	c := r.b[r.pos]
	r.pos++
	return c
}

func (r *lebReader) uleb() uint64 {
	var v uint64
	for shift := 0; ; shift += 7 {
		c := r.byte()
		if r.err || shift > 63 {
			r.err = true
			return 0
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v
		}
	}
}

func (r *lebReader) sleb() int64 {
	var v int64
	shift := 0
	for {
		c := r.byte()
		if r.err || shift > 63 {
			r.err = true
			return 0
		}
		v |= int64(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			if c&0x40 != 0 && shift < 64 {
				v |= -1 << shift
			}
			return v
		}
	}
}

// skipForm advances past one attribute value, returning its size when it is
// a fixed-width field the layer models (0 otherwise).
func (r *lebReader) skipForm(form uint64, addrSize uint8) {
	fixed := func(n uint64) { r.pos += n }
	switch form {
	case 0x01: // addr
		fixed(uint64(addrSize))
	case 0x03, 0x05, 0x12, 0x26, 0x2a: // block2, data2, ref2, strx2, addrx2
		if form == 0x03 {
			fixed(uint64(binary.LittleEndian.Uint16(r.peek(2))) + 2)
		} else {
			fixed(2)
		}
	case 0x04: // block4
		fixed(uint64(binary.LittleEndian.Uint32(r.peek(4))) + 4)
	case 0x06, 0x0e, 0x10, 0x13, 0x17, 0x1c, 0x1d, 0x1f, 0x28, 0x2c: // data4, strp, ref_addr, ref4, sec_offset, ref_sup4, strp_sup, line_strp, strx4, addrx4
		fixed(4)
	case 0x07, 0x14, 0x20, 0x24: // data8, ref8, ref_sig8, ref_sup8
		fixed(8)
	case 0x08: // string
		for !r.err && r.byte() != 0 {
		}
	case 0x09, 0x18: // block, exprloc
		fixed(r.uleb())
	case 0x0a: // block1
		fixed(uint64(r.byte()))
	case 0x0b, 0x0c, 0x11, 0x25, 0x29: // data1, flag, ref1, strx1, addrx1
		fixed(1)
	case 0x0d: // sdata
		r.sleb()
	case 0x0f, 0x15, 0x1a, 0x1b, 0x22, 0x23: // udata, ref_udata, strx, addrx, loclistx, rnglistx
		r.uleb()
	case 0x16: // indirect
		r.skipForm(r.uleb(), addrSize)
	case 0x19, 0x21: // flag_present, implicit_const
	case 0x1e: // data16
		fixed(16)
	case 0x27, 0x2b: // strx3, addrx3
		fixed(3)
	default:
		r.err = true
	}
	if r.pos > uint64(len(r.b)) {
		r.err = true
	}
}

func (r *lebReader) peek(n uint64) []byte {
	if r.pos+n > uint64(len(r.b)) {
		r.err = true
		return make([]byte, n)
	}
	return r.b[r.pos : r.pos+n]
}

// walkUnit calls visit for every attribute of every DIE in a unit, with the
// attribute's section offset. The callback runs before the value is skipped.
func walkUnit(info, abbrev []byte, u unitPair, visit func(pos uint64, at dwarfAttr, addrSize uint8, r *lebReader)) error {
	d, err := parseUnitHeader(info, u)
	if err != nil {
		return err
	}
	return walkDIEs(info, abbrev, d, nil, visit)
}

// walkDIEs is walkUnit over a parsed header, also calling die at the start
// of each DIE.
func walkDIEs(info, abbrev []byte, d dwarfUnit, die func(off, code uint64, a dwarfAbbrev), visit func(pos uint64, at dwarfAttr, addrSize uint8, r *lebReader)) error {
	table, err := parseAbbrevs(abbrev, d.abbrevOff)
	if err != nil {
		return err
	}
	r := &lebReader{b: info[:d.off+d.length], pos: d.dies}
	for r.pos < uint64(len(r.b)) {
		start := r.pos
		code := r.uleb()
		if r.err {
			return errors.New("truncated DIE")
		}
		if code == 0 {
			continue
		}
		a, ok := table[code]
		if !ok {
			return errors.New("DIE uses an unknown abbreviation")
		}
		if die != nil {
			die(start, code, a)
		}
		for _, at := range a.attrs {
			if visit != nil {
				visit(r.pos, at, d.addrSize, r)
			}
			r.skipForm(at.form, d.addrSize)
			if r.err {
				return errors.New("unsupported attribute form")
			}
		}
	}
	return nil
}

// unitAttrs reads the unit DIE's DW_AT_name (when an inline string) and
// DW_AT_stmt_list.
func unitAttrs(info, abbrev []byte, u unitPair) (name string, stmt uint64, addrBase uint64, hasBase bool) {
	first, hasStmt := true, false
	_ = walkUnit(info, abbrev, u, func(pos uint64, at dwarfAttr, _ uint8, r *lebReader) {
		if !first && at.name == 0x01 { // DW_AT_sibling: past the unit DIE
			return
		}
		first = false
		switch {
		case at.name == 0x03 && at.form == 0x08 && name == "":
			if end := slices.Index(r.b[pos:], 0); end > 0 {
				name = string(r.b[pos : pos+uint64(end)])
			}
		case at.name == 0x10 && at.form == 0x17 && !hasStmt:
			stmt, hasStmt = uint64(binary.LittleEndian.Uint32(r.peek(4))), true
		case at.name == 0x73 && at.form == 0x17 && !hasBase:
			addrBase, hasBase = uint64(binary.LittleEndian.Uint32(r.peek(4))), true
		}
	})
	if !hasStmt {
		stmt = ^uint64(0)
	}
	return name, stmt, addrBase, hasBase
}

// buildDwarfPlan pairs the units of the two images by name, in order; a
// unit whose name recurs or is missing pairs by position among the rest.
// With no equivalences to place the bytes it also pairs the per-unit line
// programs and list contributions through the unit pairing, and the
// call-frame entries by function address through addrMap.
func buildDwarfPlan(oldImage, newImage *image, withRecords func(k int) bool, addrMap func(uint64) (uint64, bool)) (dwarfPlan, bool) {
	var p dwarfPlan
	for i, name := range dwarfSecNames {
		o, okOld := oldImage.Debug[name]
		n, okNew := newImage.Debug[name]
		if okOld && okNew {
			p.Secs[i] = debugSec{o.Off, o.Size, n.Off, n.Size}
		}
	}
	info, abbrev := p.Secs[dwInfo], p.Secs[dwAbbrev]
	if info.OldSize == 0 || info.NewSize == 0 || abbrev.OldSize == 0 || abbrev.NewSize == 0 {
		return dwarfPlan{}, false
	}
	oldInfo := oldImage.Data[info.OldOff : info.OldOff+info.OldSize]
	newInfo := newImage.Data[info.NewOff : info.NewOff+info.NewSize]
	oldUnits, err := unitBounds(oldInfo)
	if err != nil {
		return dwarfPlan{}, false
	}
	newUnits, err := unitBounds(newInfo)
	if err != nil {
		return dwarfPlan{}, false
	}
	oldAbbrev := oldImage.Data[abbrev.OldOff : abbrev.OldOff+abbrev.OldSize]
	newAbbrev := newImage.Data[abbrev.NewOff : abbrev.NewOff+abbrev.NewSize]
	// Units pair by name with the anchored diff: package order is stable
	// but not guaranteed, and one out-of-order match must not derail the
	// rest.
	unitKeys := func(info, abbrev []byte, units []unitPair) (keys []uint64, stmts []int64) {
		for i, u := range units {
			name, stmt, _, _ := unitAttrs(info, abbrev, u)
			ok := stmt != ^uint64(0)
			k := hashBytes([]byte(name))
			if name == "" {
				k += uint64(i)
			}
			keys = append(keys, k)
			stmts = append(stmts, -1)
			if ok {
				stmts[i] = int64(stmt)
			}
		}
		return keys, stmts
	}
	oldKeys, oldStmt := unitKeys(oldInfo, oldAbbrev, oldUnits)
	newKeys, newStmt := unitKeys(newInfo, newAbbrev, newUnits)
	pairing := pairByDiff(oldKeys, newKeys) // new unit index per old unit, -1 if none
	for i, u := range oldUnits {
		if j := pairing[i]; j >= 0 {
			p.Units = append(p.Units, unitPair{OldOff: u.OldOff, OldLen: u.OldLen, NewOff: newUnits[j].OldOff, NewLen: newUnits[j].OldLen, Unit: true, UnitLen: newUnits[j].OldLen})
		} else {
			p.Units = append(p.Units, u)
		}
	}
	if withRecords == nil || withRecords(dwInfo) {
		p.Records[dwInfo] = p.Units
	}
	if withRecords == nil {
		return p, true
	}
	// A unit whose length changed is split into DIE records, paired by a
	// diff over their keys, so that the references into its tail project.
	if withRecords(dwInfo) {
		p.Split = make(map[int][]unitPair)
		var flat []unitPair
		for i, u := range p.Units {
			if u.NewLen == 0 || u.NewLen == u.OldLen {
				flat = append(flat, u)
				continue
			}
			o, ok, err1 := dieBounds(oldInfo, oldAbbrev, u)
			n, nk, err2 := dieBounds(newInfo, newAbbrev, unitPair{OldOff: u.NewOff, OldLen: u.NewLen})
			if err1 != nil || err2 != nil {
				flat = append(flat, u)
				continue
			}
			sub := pairRecords(o, n, pairByDiff(ok, nk))
			sub[0].NewOff, sub[0].NewLen, sub[0].UnitLen = n[0].OldOff, n[0].OldLen, u.NewLen
			p.Split[i] = sub
			flat = append(flat, sub...)
		}
		p.Records[dwInfo] = flat
	}
	// Line programs pair through the units' stmt_list references; the
	// list sections are one contribution each, paired positionally.
	// newSecs is the geometry seen from the new image, for its record bounds.
	var newSecs [dwarfSecCount]debugSec
	for k, s := range p.Secs {
		newSecs[k] = debugSec{OldOff: s.NewOff, OldSize: s.NewSize, NewOff: s.NewOff, NewSize: s.NewSize}
	}
	for _, k := range []int{dwLine, dwLoclists, dwRnglists, dwAddr, dwSymtab, dwStrtab} {
		s := p.Secs[k]
		if s.OldSize == 0 || s.NewSize == 0 || !withRecords(k) {
			continue
		}
		oldSec := oldImage.Data[s.OldOff : s.OldOff+s.OldSize]
		newSec := newImage.Data[s.NewOff : s.NewOff+s.NewSize]
		o, err1 := recordBoundsIn(k, oldImage.Data, p.Secs)
		n, err2 := recordBoundsIn(k, newImage.Data, newSecs)
		if err1 != nil || err2 != nil {
			continue
		}
		if k == dwAddr {
			// Groups pair through the units whose addr_base names them;
			// their content is addresses, which all move.
			index := func(recs []unitPair) map[uint64]int {
				m := make(map[uint64]int, len(recs))
				for i, r := range recs {
					if !r.Unit {
						m[r.OldOff] = i
					}
				}
				return m
			}
			oi, ni := index(o), index(n)
			pr := make([]int, len(o))
			for i := range pr {
				pr[i] = -1
			}
			for i, j := range pairing {
				if j < 0 {
					continue
				}
				_, _, ob, okO := unitAttrs(oldInfo, oldAbbrev, oldUnits[i])
				_, _, nb, okN := unitAttrs(newInfo, newAbbrev, newUnits[j])
				if a, ok := oi[ob]; okO && okN && ok {
					if b, ok := ni[nb]; ok {
						pr[a] = b
					}
				}
			}
			// Headers by position; the chain must stay monotone.
			hi := 0
			for i, r := range o {
				if r.Unit {
					for hi < len(n) && !n[hi].Unit {
						hi++
					}
					if hi < len(n) {
						pr[i], hi = hi, hi+1
					}
				}
			}
			next := -1
			for i := range pr {
				if pr[i] <= next {
					pr[i] = -1
				} else {
					next = pr[i]
				}
			}
			p.Records[k] = pairRecords(o, n, pr)
			if nu, err := unitBounds(newSec); err == nil {
				unitLens(p.Records[k], nu)
			}
			continue
		}
		if k == dwSymtab {
			// Symbols pair by name and kind: values and sizes are what
			// change.
			key := func(sec, strtab []byte, recs []unitPair) []uint64 {
				keys := make([]uint64, len(recs))
				for i, r := range recs {
					e := sec[r.OldOff:]
					name := uint64(binary.LittleEndian.Uint32(e))
					if end := slices.Index(strtab[min(name, uint64(len(strtab))):], 0); end >= 0 {
						keys[i] = hashBytes(strtab[name:name+uint64(end)])*31 + uint64(e[4])
					}
				}
				return keys
			}
			os, ns := p.Secs[dwStrtab], newSecs[dwStrtab]
			p.Records[k] = pairRecords(o, n, pairByDiff(key(oldSec, oldImage.Data[os.OldOff:os.OldOff+os.OldSize], o), key(newSec, newImage.Data[ns.OldOff:ns.OldOff+ns.OldSize], n)))
			continue
		}
		if k != dwLine {
			// Lists and strings pair by content: they are leaves, and the
			// ones that changed are few. Contribution headers pair by
			// position.
			pairing := pairByDiff(recordKeys(oldSec, o), recordKeys(newSec, n))
			hdrs := func(recs []unitPair) (h []int) {
				for i, r := range recs {
					if r.Unit {
						h = append(h, i)
					}
				}
				return h
			}
			oh, nh := hdrs(o), hdrs(n)
			for i := range oh {
				if i < len(nh) {
					pairing[oh[i]] = nh[i]
				}
			}
			p.Records[k] = pairRecords(o, n, pairing)
			if nu, err := unitBounds(newSec); err == nil {
				unitLens(p.Records[k], nu)
			}
			continue
		}
		p.Records[k] = pairRecords(o, n, stmtPairing(o, n, oldStmt, newStmt, pairing))
	}
	if s := p.Secs[dwFrame]; s.OldSize != 0 && s.NewSize != 0 && addrMap != nil && withRecords(dwFrame) {
		o, err1 := unitBounds(oldImage.Data[s.OldOff : s.OldOff+s.OldSize])
		n, err2 := unitBounds(newImage.Data[s.NewOff : s.NewOff+s.NewSize])
		if err1 == nil && err2 == nil {
			p.Records[dwFrame] = pairFrames(oldImage.Data[s.OldOff:s.OldOff+s.OldSize], o, newImage.Data[s.NewOff:s.NewOff+s.NewSize], n, addrMap)
		}
	}
	return p, true
}

// recordKeys hashes each record's bytes.
func recordKeys(sec []byte, recs []unitPair) []uint64 {
	keys := make([]uint64, len(recs))
	for i, r := range recs {
		keys[i] = hashBytes(sec[r.OldOff : r.OldOff+r.OldLen])
	}
	return keys
}

// stmtPairing pairs line programs through the unit pairing: a unit's
// program is the record its DW_AT_stmt_list names.
func stmtPairing(o, n []unitPair, oldStmt, newStmt []int64, units []int) []int {
	index := func(recs []unitPair) map[int64]int {
		m := make(map[int64]int, len(recs))
		for i, r := range recs {
			m[int64(r.OldOff)] = i
		}
		return m
	}
	oi, ni := index(o), index(n)
	p := make([]int, len(o))
	for i := range p {
		p[i] = -1
	}
	next := -1
	for i, j := range units {
		if j < 0 || oldStmt[i] < 0 || newStmt[j] < 0 {
			continue
		}
		a, okA := oi[oldStmt[i]]
		b, okB := ni[newStmt[j]]
		if okA && okB && b > next && p[a] < 0 {
			p[a], next = b, b
		}
	}
	return p
}

// pairRecords pairs old record i with new record pairing[i].
func pairRecords(o, n []unitPair, pairing []int) []unitPair {
	recs := make([]unitPair, len(o))
	for i, u := range o {
		recs[i] = unitPair{OldOff: u.OldOff, OldLen: u.OldLen, Unit: u.Unit}
		if j := pairing[i]; j >= 0 {
			recs[i].NewOff, recs[i].NewLen, recs[i].UnitLen = n[j].OldOff, n[j].OldLen, n[j].OldLen
		}
	}
	return recs
}

// unitLens fills in, for the unit-starting records of a paired table, the
// new length of the unit each starts, from the new section's units.
func unitLens(recs []unitPair, newUnits []unitPair) {
	starts := make(map[uint64]uint64, len(newUnits))
	for _, u := range newUnits {
		starts[u.OldOff] = u.OldLen
	}
	for i := range recs {
		if recs[i].Unit && recs[i].NewLen != 0 {
			if n, ok := starts[recs[i].NewOff]; ok {
				recs[i].UnitLen = n
			}
		}
	}
}

// frameAddr is the initial location of a call-frame record, or false for
// a CIE.
func frameAddr(frame []byte, r unitPair) (uint64, bool) {
	if r.OldLen < 24 || binary.LittleEndian.Uint32(frame[r.OldOff+4:]) == 0xffffffff {
		return 0, false
	}
	return binary.LittleEndian.Uint64(frame[r.OldOff+8:]), true
}

// pairFrames pairs FDEs whose function addresses correspond, in order, and
// CIEs by position.
func pairFrames(oldFrame []byte, o []unitPair, newFrame []byte, n []unitPair, addrMap func(uint64) (uint64, bool)) []unitPair {
	byAddr := make(map[uint64]int, len(n))
	var newCIEs []int
	for j, r := range n {
		if a, ok := frameAddr(newFrame, r); ok {
			byAddr[a] = j
		} else {
			newCIEs = append(newCIEs, j)
		}
	}
	recs := make([]unitPair, len(o))
	next, cie := -1, 0
	for i, r := range o {
		recs[i] = unitPair{OldOff: r.OldOff, OldLen: r.OldLen, Unit: r.Unit}
		j := -1
		if a, ok := frameAddr(oldFrame, r); ok {
			if na, ok := addrMap(a); ok {
				if jj, ok := byAddr[na]; ok {
					j = jj
				}
			}
		} else if cie < len(newCIEs) {
			j, cie = newCIEs[cie], cie+1
		}
		if j <= next {
			continue
		}
		next = j
		recs[i].NewOff, recs[i].NewLen, recs[i].UnitLen = n[j].OldOff, n[j].OldLen, n[j].OldLen
	}
	return recs
}

type dwarfStats struct {
	Units       int `json:"units"`
	UnitsPaired int `json:"units_paired"`
	Refs        int `json:"ref_addr_fields"`
	SecOffsets  int `json:"sec_offset_fields"`
	Addrs       int `json:"addr_fields"`
	AddrEntries int `json:"addr_table_entries"`
	Frames      int `json:"frame_entries"`
	LineAddrs   int `json:"line_addresses"`
	Symbols     int `json:"symbols"`
}

// applyDwarf writes the projected debug fields into out. With equivalences
// the bytes are already in place and positions and offsets project through
// them; without, the record tables place the bytes and project the
// offsets, positionally where a section has no table. sizeOf gives the new
// size of the function at an old address.
func applyDwarf(out, old []byte, p dwarfPlan, ep equivalencePlan, ptr func(uint64) x86.Target, sizeDelta func(uint64) (int64, bool)) (dwarfStats, error) {
	le := binary.LittleEndian
	var st dwarfStats
	var covered [dwarfSecCount]bool
	for k, s := range p.Secs {
		if s.OldOff+s.OldSize > uint64(len(old)) || s.NewOff+s.NewSize > uint64(len(out)) {
			return st, errors.New("DWARF plan exceeds an image")
		}
		// A section with a record table is placed by the table even where
		// equivalences write into it: in a table of fixed-size rows whose
		// values all change, byte matching aligns rows by chance.
		covered[k] = p.Records[k] == nil && eqCovers(ep, s.NewOff, s.NewSize)
	}
	eqs := newSourceEquivalenceMapper(ep)
	oldSec := func(k int) []byte { s := p.Secs[k]; return old[s.OldOff : s.OldOff+s.OldSize] }
	newSec := func(k int) []byte { s := p.Secs[k]; return out[s.NewOff : s.NewOff+s.NewSize] }
	// project maps an old offset within section k to a new one. A value
	// (a reference into the section) projects through the nearest
	// equivalence, since bytes between equivalences move with their
	// neighbours; a position (a field to write) only through the
	// equivalence that copied it, since a write anywhere else lands on
	// bytes some other layer placed.
	projectWith := func(k int, off uint64, strict bool) (uint64, bool) {
		s := p.Secs[k]
		if off >= s.OldSize {
			return 0, false
		}
		if covered[k] {
			n, ok := eqs.within(s.OldOff + off)
			if !ok && (!strict || p.Records[k] == nil) {
				// Zucchini's equivalences stop at the bytes that changed,
				// which are the very fields to rewrite; without a table
				// they project with their neighbours.
				n, ok = eqs.project(s.OldOff + off)
			}
			if ok {
				if n < s.NewOff || n >= s.NewOff+s.NewSize {
					return 0, false
				}
				return n - s.NewOff, true
			}
		}
		if recs := p.Records[k]; recs != nil {
			i := sort.Search(len(recs), func(i int) bool { return recs[i].OldOff > off }) - 1
			if i < 0 || recs[i].NewLen == 0 || off-recs[i].OldOff >= recs[i].NewLen {
				return 0, false
			}
			return recs[i].NewOff + off - recs[i].OldOff, true
		}
		if off >= s.NewSize {
			return 0, false
		}
		return off, true
	}
	project := func(k int, off uint64) (uint64, bool) { return projectWith(k, off, false) }
	position := func(k int, off uint64) (uint64, bool) { return projectWith(k, off, true) }
	for k, s := range p.Secs {
		if s.NewSize == 0 || covered[k] {
			continue
		}
		if p.Records[k] == nil {
			copy(newSec(k), oldSec(k))
			continue
		}
		for _, u := range p.Records[k] {
			if u.NewLen == 0 {
				continue
			}
			copy(newSec(k)[u.NewOff:u.NewOff+u.NewLen], oldSec(k)[u.OldOff:u.OldOff+u.OldLen])
			if u.Unit && u.NewLen >= 4 {
				le.PutUint32(newSec(k)[u.NewOff:], uint32(u.UnitLen-4))
			}
		}
	}
	// put writes an n-byte little-endian value at the projection of pos.
	put := func(k int, pos uint64, n int, v uint64) bool {
		npos, ok := position(k, pos)
		if !ok || npos+uint64(n) > p.Secs[k].NewSize {
			return false
		}
		b := newSec(k)[npos : npos+uint64(n)]
		for i := range b {
			b[i] = byte(v >> (8 * i))
		}
		return true
	}
	putAddr := func(k int, pos uint64, addr uint64) bool {
		t := ptr(addr)
		return t.Known && put(k, pos, 8, t.Addr)
	}

	// .debug_info: type references, section offsets, addresses.
	info := p.Secs[dwInfo]
	if info.NewSize != 0 {
		units, err := unitBounds(oldSec(dwInfo))
		if err != nil {
			return st, err
		}
		st.Units = len(units)
		for _, u := range units {
			if _, ok := position(dwInfo, u.OldOff); !ok {
				continue
			}
			st.UnitsPaired++
			err := walkUnit(oldSec(dwInfo), oldSec(dwAbbrev), u, func(pos uint64, at dwarfAttr, addrSize uint8, r *lebReader) {
				switch at.form {
				case 0x10: // ref_addr
					if v, ok := project(dwInfo, uint64(le.Uint32(r.peek(4)))); ok && put(dwInfo, pos, 4, v) {
						st.Refs++
					}
				case 0x17: // sec_offset
					k := -1
					switch at.name {
					case 0x10: // stmt_list
						k = dwLine
					case 0x02, 0x8c: // location, loclists_base
						k = dwLoclists
					case 0x55, 0x74: // ranges, rnglists_base
						k = dwRnglists
					case 0x73: // addr_base
						k = dwAddr
					}
					if k < 0 || p.Secs[k].NewSize == 0 {
						return
					}
					if v, ok := project(k, uint64(le.Uint32(r.peek(4)))); ok && put(dwInfo, pos, 4, v) {
						st.SecOffsets++
					}
				case 0x01: // addr
					if addrSize == 8 && putAddr(dwInfo, pos, le.Uint64(r.peek(8))) {
						st.Addrs++
					}
				}
			})
			if err != nil {
				return st, err
			}
		}
	}
	// .debug_addr: per-unit contributions of 8-byte addresses behind an
	// 8-byte header (length, version, address size, segment selector size).
	if s := p.Secs[dwAddr]; s.NewSize != 0 {
		b := oldSec(dwAddr)
		for off := uint64(0); off+8 <= uint64(len(b)); {
			n := uint64(le.Uint32(b[off:]))
			if n < 4 || off+4+n > uint64(len(b)) || b[off+6] != 8 {
				break
			}
			for e := off + 8; e+8 <= off+4+n; e += 8 {
				if putAddr(dwAddr, e, le.Uint64(b[e:])) {
					st.AddrEntries++
				}
			}
			off += 4 + n
		}
	}
	// .debug_frame: each FDE names its function's address and size.
	if s := p.Secs[dwFrame]; s.NewSize != 0 {
		b := oldSec(dwFrame)
		recs, err := unitBounds(b)
		if err != nil {
			return st, err
		}
		for _, r := range recs {
			a, ok := frameAddr(b, r)
			if !ok {
				continue
			}
			if putAddr(dwFrame, r.OldOff+8, a) {
				st.Frames++
			}
			if d, ok := sizeDelta(a); ok && d != 0 {
				put(dwFrame, r.OldOff+16, 8, uint64(int64(le.Uint64(b[r.OldOff+16:]))+d))
			}
		}
	}
	// .debug_line: DW_LNE_set_address in each unit's program.
	if s := p.Secs[dwLine]; s.NewSize != 0 {
		b := oldSec(dwLine)
		recs, err := unitBounds(b)
		if err != nil {
			return st, err
		}
		for _, r := range recs {
			n, err := walkLineAddrs(b, r, func(pos, addr uint64) bool { return putAddr(dwLine, pos, addr) })
			if err != nil {
				return st, err
			}
			st.LineAddrs += n
		}
	}
	// .symtab: values are addresses, sizes follow the function map.
	if s := p.Secs[dwSymtab]; s.NewSize != 0 {
		b := oldSec(dwSymtab)
		for off := uint64(0); off+24 <= uint64(len(b)); off += 24 {
			if name, ok := project(dwStrtab, uint64(le.Uint32(b[off:]))); ok {
				put(dwSymtab, off, 4, name)
			}
			v := le.Uint64(b[off+8:])
			if v == 0 {
				continue
			}
			if putAddr(dwSymtab, off+8, v) {
				st.Symbols++
			}
			if d, ok := sizeDelta(v); ok && d != 0 && b[off+4]&0xf == 2 { // STT_FUNC
				put(dwSymtab, off+16, 8, uint64(int64(le.Uint64(b[off+16:]))+d))
			}
		}
	}
	return st, nil
}

// walkLineAddrs visits every DW_LNE_set_address operand in one line-number
// program, with its section offset.
func walkLineAddrs(line []byte, r unitPair, visit func(pos, addr uint64) bool) (int, error) {
	le := binary.LittleEndian
	b := line[r.OldOff : r.OldOff+r.OldLen]
	if len(b) < 16 {
		return 0, errors.New("short line program header")
	}
	version := le.Uint16(b[4:])
	var hdrLenAt uint64 = 6
	if version >= 5 {
		hdrLenAt = 8
	}
	prog := hdrLenAt + 4 + uint64(le.Uint32(b[hdrLenAt:]))
	if version < 2 || prog > uint64(len(b)) {
		return 0, errors.New("unsupported line program")
	}
	// The standard opcode lengths follow the fixed header fields.
	fixed := hdrLenAt + 4 + 5 // minimum_instruction_length, default_is_stmt, line_base, line_range, opcode_base
	if version >= 4 {
		fixed++ // maximum_operations_per_instruction
	}
	if fixed > uint64(len(b)) {
		return 0, errors.New("short line program header")
	}
	opcodeBase := b[fixed-1]
	lens := b[fixed : fixed+uint64(opcodeBase)-1]
	rd := &lebReader{b: b, pos: prog}
	n := 0
	for rd.pos < uint64(len(b)) && !rd.err {
		op := rd.byte()
		switch {
		case op == 0:
			l := rd.uleb()
			start := rd.pos
			if l != 0 && rd.byte() == 2 && l == 9 {
				if visit(r.OldOff+rd.pos, le.Uint64(rd.peek(8))) {
					n++
				}
			}
			rd.pos = start + l
		case op < opcodeBase:
			if op == 9 { // DW_LNS_fixed_advance_pc takes a uhalf
				rd.pos += 2
				break
			}
			for range lens[op-1] {
				rd.uleb()
			}
		}
	}
	if rd.err || rd.pos > uint64(len(b)) {
		return n, errors.New("truncated line program")
	}
	return n, nil
}

// eqCovers reports whether any equivalence writes into [off, off+size).
func eqCovers(ep equivalencePlan, off, size uint64) bool {
	for _, e := range ep.Eqs {
		if e.Dst < off+size && e.Dst+e.N > off {
			return true
		}
	}
	return false
}

// textEquivalences counts the equivalences that write into .text.
func textEquivalences(ep equivalencePlan) int {
	n := 0
	for _, e := range ep.Eqs {
		if e.Dst < ep.NewText.Off+ep.NewText.Size && e.Dst+e.N > ep.NewText.Off {
			n++
		}
	}
	return n
}
