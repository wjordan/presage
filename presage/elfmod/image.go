package elfmod

import (
	"bytes"
	"debug/elf"
	"errors"
	"slices"
	"strings"
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
	var code []namedSection
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
		s := section{Addr: sec.Addr, Off: sec.Offset, Size: sec.Size, NoBits: nobits}
		sections[sec.Name] = s
		if !nobits && sec.Flags&elf.SHF_EXECINSTR != 0 && isCodeWindowName(sec.Name) {
			code = append(code, namedSection{Name: sec.Name, section: s})
		}
	}
	slices.SortFunc(code, func(a, b namedSection) int { return cmpU(a.Addr, b.Addr) })
	return &image{Data: b, Text: text, Code: code, Sections: sections, Debug: debug}, nil
}

// isCodeWindowName reports whether a section name marks a code window: some
// dot-separated component is exactly "text".
//
// The rule admits .text, the compiler's .text.cold and .text.warm, and the
// .bolt.org.text that BOLT's lite mode leaves behind holding the functions
// it did not re-emit; it leaves out .init, .fini, .plt and .plt.sec, which
// are executable but are stubs a linker regenerates rather than code a
// function map or a canonical match has anything to say about. An image
// whose only such section is .text therefore keeps exactly one window, and
// nothing about how it is modelled changes.
func isCodeWindowName(name string) bool {
	for _, part := range strings.Split(name, ".") {
		if part == "text" {
			return true
		}
	}
	return false
}

// pairCodeWindows matches the two images' code windows by name. A window
// exists only where both images have the section, since the module models a
// window by predicting the new one from the old.
//
// The plan format orders windows ascending and non-overlapping in both
// images (parseEquivalencePlan enforces it), so a pair whose sections
// changed relative order between builds keeps the subset that fits rather
// than emitting a plan the module's own decoder rejects; a dropped window
// is merely unmodelled, covered by the whole-image equivalences.
func pairCodeWindows(old, newImage *image) []codeWindow {
	byName := make(map[string]section, len(newImage.Code))
	for _, s := range newImage.Code {
		byName[s.Name] = s.section
	}
	windows := make([]codeWindow, 0, len(old.Code))
	for _, s := range old.Code {
		n, ok := byName[s.Name]
		if !ok {
			continue
		}
		windows = append(windows, codeWindow{Old: s.section, New: n})
	}
	slices.SortFunc(windows, func(a, b codeWindow) int { return cmpU(a.Old.Off, b.Old.Off) })
	kept := windows[:0]
	var oldEnd, newEnd uint64
	for _, w := range windows {
		if w.Old.Off < oldEnd || w.New.Off < newEnd {
			continue
		}
		kept = append(kept, w)
		oldEnd, newEnd = w.Old.Off+w.Old.Size, w.New.Off+w.New.Size
	}
	// The plan format also caps the count; an unmerged `.text.<func>`
	// image keeps the first windows rather than failing the encode.
	return kept[:min(len(kept), 1<<16)]
}

// windowNames is the set of section names the windows occupy, which the
// section ranges must exclude: inside a window the function map and the
// byte-level projection are authoritative, and a constant section shift
// would contradict them.
func windowNames(old, newImage *image) map[string]bool {
	names := make(map[string]bool)
	byName := make(map[string]bool, len(newImage.Code))
	for _, s := range newImage.Code {
		byName[s.Name] = true
	}
	for _, s := range old.Code {
		if byName[s.Name] {
			names[s.Name] = true
		}
	}
	return names
}

// sectionRanges is the piecewise-constant shift of every section outside a
// code window that both images have, over the smaller of the two sizes.
func sectionRanges(old, newImage *image) []addressRange {
	windows := windowNames(old, newImage)
	var ranges []addressRange
	for name, oldSection := range old.Sections {
		if windows[name] {
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
