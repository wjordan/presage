package main

import (
	"bufio"
	"bytes"
	"debug/elf"
	"debug/gosym"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/zeebo/blake3"
)

type section struct {
	Addr   uint64
	Off    uint64
	Size   uint64
	NoBits bool // occupies no file bytes
}

type image struct {
	Data     []byte
	Text     section
	Sections map[string]section
	// Debug holds the non-allocated sections with file contents.
	Debug map[string]section
}

func loadImage(path string) (*image, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	s := f.Section(".text")
	if s == nil {
		f.Close()
		return nil, fmt.Errorf("%s: no .text section", path)
	}
	text := section{Addr: s.Addr, Off: s.Offset, Size: s.Size}
	sections := make(map[string]section)
	debug := make(map[string]section)
	for _, sec := range f.Sections {
		if sec.Flags&elf.SHF_ALLOC == 0 && sec.Type != elf.SHT_NOBITS && sec.Size != 0 {
			debug[sec.Name] = section{Off: sec.Offset, Size: sec.Size}
		}
		if sec.Flags&elf.SHF_ALLOC == 0 || sec.Flags&elf.SHF_TLS != 0 || sec.Size == 0 || sec.Addr == 0 {
			continue
		}
		sections[sec.Name] = section{Addr: sec.Addr, Off: sec.Offset, Size: sec.Size, NoBits: sec.Type == elf.SHT_NOBITS}
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if text.Off > uint64(len(b)) || text.Size > uint64(len(b))-text.Off {
		return nil, fmt.Errorf("%s: .text lies outside the file", path)
	}
	return &image{Data: b, Text: text, Sections: sections, Debug: debug}, nil
}

func (im *image) textBytes() []byte {
	return im.Data[im.Text.Off : im.Text.Off+im.Text.Size]
}

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

type nameID [16]byte

func fingerprint(s string) nameID {
	sum := blake3.Sum256([]byte(s))
	var id nameID
	copy(id[:], sum[:len(id)])
	return id
}

type codeUnit struct {
	Off   uint64
	Size  uint64
	Names []nameID
}

type symbolStats struct {
	FunctionSymbols int `json:"function_symbols"`
	AddressUnits    int `json:"address_units"`
	CoveredBytes    int `json:"covered_bytes"`
}

type symbolGroup struct {
	maxSize uint64
	names   []nameID
}

// loadCodeUnits deliberately retains no symbol text. Names are encoder-only
// evidence for constructing an address plan, and a fixed-size fingerprint is
// enough to test exact identity without bloating memory on C++ aliases.
func loadCodeUnits(path string, text section) ([]codeUnit, symbolStats, error) {
	groups := make(map[uint64]*symbolGroup)
	var st symbolStats
	textEnd := text.Addr + text.Size
	add := func(addr, size uint64, name string) {
		if size == 0 || addr < text.Addr || addr >= textEnd {
			return
		}
		st.FunctionSymbols++
		g := groups[addr]
		if g == nil {
			g = &symbolGroup{}
			groups[addr] = g
		}
		g.maxSize = max(g.maxSize, size)
		if name != "" {
			g.names = append(g.names, fingerprint(name))
		}
	}
	if breakpad, err := isBreakpad(path); err != nil {
		return nil, symbolStats{}, err
	} else if breakpad {
		if err := readBreakpadFuncs(path, add); err != nil {
			return nil, symbolStats{}, err
		}
	} else {
		f, err := elf.Open(path)
		if err != nil {
			return nil, symbolStats{}, err
		}
		syms, err := f.Symbols()
		if errors.Is(err, elf.ErrNoSymbols) && f.Section(".gopclntab") != nil {
			// A stripped Go binary still carries every function boundary
			// and name in its pclntab.
			err = readPclntabFuncs(f, add)
		} else if err == nil {
			for _, sym := range syms {
				if elf.ST_TYPE(sym.Info) == elf.STT_FUNC {
					add(sym.Value, sym.Size, sym.Name)
				}
			}
		}
		if err != nil {
			f.Close()
			return nil, symbolStats{}, fmt.Errorf("%s: read symbols: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return nil, symbolStats{}, err
		}
		syms = nil
		runtime.GC()
	}

	starts := make([]uint64, 0, len(groups))
	for addr := range groups {
		starts = append(starts, addr)
	}
	slices.Sort(starts)
	units := make([]codeUnit, 0, len(starts))
	for i, addr := range starts {
		limit := textEnd - addr
		if i+1 < len(starts) {
			limit = starts[i+1] - addr
		}
		sz := min(groups[addr].maxSize, limit)
		if sz == 0 {
			continue
		}
		names := groups[addr].names
		slices.SortFunc(names, func(a, b nameID) int { return bytes.Compare(a[:], b[:]) })
		names = slices.Compact(names)
		units = append(units, codeUnit{Off: addr - text.Addr, Size: sz, Names: names})
		st.CoveredBytes += int(sz)
	}
	st.AddressUnits = len(units)
	return units, st, nil
}

// isBreakpad reports whether path is a breakpad symbol file (the MODULE/FUNC
// text Mozilla and Chrome publish) rather than an ELF.
func isBreakpad(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var head [7]byte
	n, _ := f.Read(head[:])
	h := string(head[:n])
	return strings.HasPrefix(h, "MODULE ") || strings.HasPrefix(h, "FUNC "), nil
}

// readBreakpadFuncs streams the FUNC records of a breakpad symbol file:
// "FUNC [m ]address size parameter_size name", addresses relative to the
// module base, which for the position-independent images measured here is
// the virtual address.
func readBreakpadFuncs(path string, add func(addr, size uint64, name string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "FUNC ") {
			continue
		}
		fields := strings.SplitN(strings.TrimPrefix(line[5:], "m "), " ", 4)
		if len(fields) < 4 {
			continue
		}
		addr, err1 := strconv.ParseUint(fields[0], 16, 64)
		size, err2 := strconv.ParseUint(fields[1], 16, 64)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("%s: bad FUNC record: %q", path, line)
		}
		add(addr, size, fields[3])
	}
	return sc.Err()
}

// relaSection finds the dynamic relocation table: .rela.dyn as ld/lld name
// it, or .rela as the Go linker does for a PIE build.
func relaSection(secs map[string]section) (section, bool) {
	for _, name := range []string{".rela.dyn", ".rela"} {
		if s, ok := secs[name]; ok {
			return s, true
		}
	}
	return section{}, false
}

// readPclntabFuncs walks the Go runtime's function table.
func readPclntabFuncs(f *elf.File, add func(addr, size uint64, name string)) error {
	text := f.Section(".text")
	pcln, err := f.Section(".gopclntab").Data()
	if err != nil {
		return err
	}
	tab, err := gosym.NewTable(nil, gosym.NewLineTable(pcln, text.Addr))
	if err != nil {
		return err
	}
	for _, fn := range tab.Funcs {
		add(fn.Entry, fn.End-fn.Entry, fn.Name)
	}
	return nil
}
