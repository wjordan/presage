package elfmod

import (
	"bytes"
	"debug/elf"
	"errors"
	"slices"
)

// errNotELF marks an input the module does not model: not an ELF x86-64
// image with a .text section. Analyse maps it to presage.ErrDeclined.
var errNotELF = errors.New("elfmod: not an ELF x86-64 image with .text")

// loadImage reads the section geometry of an ELF image from its bytes.
func loadImage(b []byte) (*image, error) {
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return nil, errNotELF
	}
	if f.Class != elf.ELFCLASS64 || f.Machine != elf.EM_X86_64 {
		return nil, errNotELF
	}
	s := f.Section(".text")
	if s == nil || s.Type == elf.SHT_NOBITS {
		return nil, errNotELF
	}
	text := section{Addr: s.Addr, Off: s.Offset, Size: s.Size}
	if text.Off > uint64(len(b)) || text.Size > uint64(len(b))-text.Off {
		return nil, errNotELF
	}
	sections := make(map[string]section)
	debug := make(map[string]section)
	for _, sec := range f.Sections {
		if sec.Flags&elf.SHF_ALLOC == 0 && sec.Type != elf.SHT_NOBITS && sec.Size != 0 &&
			sec.Offset <= uint64(len(b)) && sec.Size <= uint64(len(b))-sec.Offset {
			debug[sec.Name] = section{Off: sec.Offset, Size: sec.Size}
		}
		if sec.Flags&elf.SHF_ALLOC == 0 || sec.Flags&elf.SHF_TLS != 0 || sec.Size == 0 || sec.Addr == 0 {
			continue
		}
		nobits := sec.Type == elf.SHT_NOBITS
		if !nobits && (sec.Offset > uint64(len(b)) || sec.Size > uint64(len(b))-sec.Offset) {
			return nil, errNotELF
		}
		sections[sec.Name] = section{Addr: sec.Addr, Off: sec.Offset, Size: sec.Size, NoBits: nobits}
	}
	return &image{Data: b, Text: text, Sections: sections, Debug: debug}, nil
}

// sectionRanges is the piecewise-constant shift of every section but
// .text that both images have, over the smaller of the two sizes.
func sectionRanges(old, newImage *image) []addressRange {
	var ranges []addressRange
	for name, oldSection := range old.Sections {
		if name == ".text" {
			continue
		}
		newSection, ok := newImage.Sections[name]
		if !ok {
			continue
		}
		ranges = append(ranges, addressRange{
			Old:  oldSection.Addr,
			New:  newSection.Addr,
			Size: min(oldSection.Size, newSection.Size),
		})
	}
	slices.SortFunc(ranges, func(a, b addressRange) int { return cmpU(a.Old, b.Old) })
	return ranges
}

// relaSection finds the dynamic relocation table: .rela.dyn as ld/lld
// name it, or .rela as the Go linker does for a PIE build.
func relaSection(secs map[string]section) (section, bool) {
	for _, name := range []string{".rela.dyn", ".rela"} {
		if s, ok := secs[name]; ok {
			return s, true
		}
	}
	return section{}, false
}
