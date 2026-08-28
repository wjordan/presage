package main

import (
	"bytes"
	"debug/elf"
	"fmt"
	"os"
	"runtime"
	"slices"

	"github.com/zeebo/blake3"
)

type section struct {
	Addr uint64
	Off  uint64
	Size uint64
}

type image struct {
	Data     []byte
	Text     section
	Sections map[string]section
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
	for _, sec := range f.Sections {
		if sec.Flags&elf.SHF_ALLOC == 0 || sec.Flags&elf.SHF_TLS != 0 || sec.Size == 0 || sec.Addr == 0 {
			continue
		}
		sections[sec.Name] = section{Addr: sec.Addr, Off: sec.Offset, Size: sec.Size}
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
	return &image{Data: b, Text: text, Sections: sections}, nil
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
	f, err := elf.Open(path)
	if err != nil {
		return nil, symbolStats{}, err
	}
	syms, err := f.Symbols()
	if err != nil {
		f.Close()
		return nil, symbolStats{}, fmt.Errorf("%s: read symbols: %w", path, err)
	}
	groups := make(map[uint64]*symbolGroup)
	var st symbolStats
	textEnd := text.Addr + text.Size
	for _, sym := range syms {
		if elf.ST_TYPE(sym.Info) != elf.STT_FUNC || sym.Size == 0 || sym.Value < text.Addr || sym.Value >= textEnd {
			continue
		}
		st.FunctionSymbols++
		g := groups[sym.Value]
		if g == nil {
			g = &symbolGroup{}
			groups[sym.Value] = g
		}
		g.maxSize = max(g.maxSize, sym.Size)
		if sym.Name != "" {
			g.names = append(g.names, fingerprint(sym.Name))
		}
	}
	if err := f.Close(); err != nil {
		return nil, symbolStats{}, err
	}
	syms = nil
	runtime.GC()

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
