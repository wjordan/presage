package delta

import (
	"encoding/binary"

	"github.com/wjordan/presage/delta/gobin"
)

// The ELF header, the program headers and the section headers are a few
// hundred bytes the linker derives from the section geometry, and the
// layout carries that geometry, so they are derived here too rather than
// copied from the old file and corrected: every section that grew or moved
// would otherwise cost a few bytes in each table that names it.
//
// Go reserves the same room for the headers in every binary it links, so
// the tables sit at the old file's offsets. A field the geometry does not
// determine -- a flag, an alignment, a program header that covers no
// section -- keeps the old value.

const (
	ehdrSize = 64
	phdrSize = 56
	shdrSize = 64
)

// predictHeaders lays the old file's leading bytes (up to the first
// allocated section) into pred and rewrites the geometry fields.
func predictHeaders(pred []byte, old, new *gobin.Bin, lookup func(uint64) (uint64, bool)) {
	le := binary.LittleEndian
	src := old.File
	if len(src) < ehdrSize || len(pred) < ehdrSize {
		return
	}
	lead := len(src)
	for _, s := range old.Order {
		if !s.NoBits && s.Size > 0 && int(s.Off) < lead {
			lead = int(s.Off)
		}
	}
	for _, s := range new.Order {
		if !s.NoBits && s.Size > 0 && int(s.Off) < lead {
			lead = int(s.Off)
		}
	}
	copy(pred[:min(lead, len(pred))], src)

	if entry, ok := lookup(le.Uint64(src[24:])); ok {
		le.PutUint64(pred[24:], entry)
	}
	phoff, phnum := int(le.Uint64(src[32:])), int(le.Uint16(src[56:]))
	shoff, shnum := int(le.Uint64(src[40:])), int(le.Uint16(src[60:]))
	if le.Uint16(src[54:]) != phdrSize || le.Uint16(src[58:]) != shdrSize {
		return
	}
	if end := phoff + phnum*phdrSize; phoff >= ehdrSize && end <= lead && end <= len(pred) {
		for i := range phnum {
			predictPhdr(pred[phoff+i*phdrSize:][:phdrSize], old, new)
		}
	}
	if end := shoff + shnum*shdrSize; shoff >= ehdrSize && end <= lead && end <= len(pred) {
		names := sectionNames(src, shoff, shnum)
		for i := range shnum {
			predictShdr(pred[shoff+i*shdrSize:][:shdrSize], names[i], old, new, len(pred))
		}
	}
}

// predictPhdr recomputes one program header from the new extents of the
// sections its old extent covered. A header covering none is left alone.
// The linker rounds some extents (the writable segment's file size, to
// 16), so each size keeps whatever rounding the old value shows.
func predictPhdr(h []byte, old, new *gobin.Bin) {
	le := binary.LittleEndian
	off, vaddr := le.Uint64(h[8:]), le.Uint64(h[16:])
	filesz, memsz := le.Uint64(h[32:]), le.Uint64(h[40:])
	var first, firstNew *gobin.Section
	var oldFileEnd, oldMemEnd, newFileEnd, newMemEnd uint64
	for _, s := range old.Order {
		if s.Size == 0 || s.Addr < vaddr || s.Addr+s.Size > vaddr+memsz {
			continue
		}
		ns := new.Sects[s.Name]
		if ns == nil {
			return
		}
		if first == nil {
			first, firstNew = s, ns
		}
		oldMemEnd, newMemEnd = max(oldMemEnd, s.Addr+s.Size), max(newMemEnd, ns.Addr+ns.Size)
		if !s.NoBits && !ns.NoBits {
			oldFileEnd, newFileEnd = max(oldFileEnd, s.Off+s.Size), max(newFileEnd, ns.Off+ns.Size)
		}
	}
	if first == nil {
		return
	}
	// the segment keeps its lead over its first section (the headers, for
	// the first load segment)
	newVaddr := vaddr + (firstNew.Addr - first.Addr)
	newOff := off + (firstNew.Off - first.Off)
	le.PutUint64(h[8:], newOff)
	le.PutUint64(h[16:], newVaddr)
	le.PutUint64(h[24:], newVaddr) // paddr, which the linker sets equal
	if oldFileEnd != 0 {
		le.PutUint64(h[32:], rounded(oldFileEnd-off, filesz, newFileEnd-newOff))
	}
	le.PutUint64(h[40:], rounded(oldMemEnd-vaddr, memsz, newMemEnd-newVaddr))
}

// rounded applies to want the rounding that took oldRaw to oldGot: the
// smallest power of two that explains it, or the old value unchanged when
// none does. With identical geometry it returns oldGot, which is what keeps
// self-prediction exact.
func rounded(oldRaw, oldGot, want uint64) uint64 {
	for a := uint64(1); a <= 1<<12; a <<= 1 {
		if (oldRaw+a-1)&^(a-1) == oldGot {
			return (want + a - 1) &^ (a - 1)
		}
	}
	return oldGot
}

// predictShdr rewrites the address, offset and size of one section header.
// A section outside the layout is left alone, except the string table at
// the file's tail, which moves with the tail.
func predictShdr(h []byte, name string, old, new *gobin.Bin, newLen int) {
	le := binary.LittleEndian
	if ns := new.Sects[name]; ns != nil {
		le.PutUint64(h[16:], ns.Addr)
		le.PutUint64(h[24:], ns.Off)
		le.PutUint64(h[32:], ns.Size)
		return
	}
	off, size := le.Uint64(h[24:]), le.Uint64(h[32:])
	if off+size == uint64(len(old.File)) && size <= uint64(newLen) {
		le.PutUint64(h[24:], uint64(newLen)-size)
	}
}

// sectionNames reads the names of the old section headers.
func sectionNames(src []byte, shoff, shnum int) []string {
	le := binary.LittleEndian
	names := make([]string, shnum)
	strndx := int(le.Uint16(src[62:]))
	if strndx >= shnum {
		return names
	}
	sh := src[shoff+strndx*shdrSize:]
	toff, tsize := le.Uint64(sh[24:]), le.Uint64(sh[32:])
	if toff+tsize > uint64(len(src)) {
		return names
	}
	tab := src[toff : toff+tsize]
	for i := range names {
		n := int(le.Uint32(src[shoff+i*shdrSize:]))
		if n >= len(tab) {
			continue
		}
		end := n
		for end < len(tab) && tab[end] != 0 {
			end++
		}
		names[i] = string(tab[n:end])
	}
	return names
}
