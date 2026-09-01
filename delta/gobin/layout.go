package gobin

import "strings"

// LayoutID is the wire identity of a layout descriptor. It is written into
// the patch, so the values are stable and a decoder that meets one it does
// not have refuses by name.
type LayoutID byte

const (
	LayoutGo126 LayoutID = 1
	LayoutGo127 LayoutID = 2
)

// Layout is what one Go release moved, as data rather than as a code path
// (docs/go-module-design.md 2.9, D23). Everything the codec depends on that
// is common to the releases -- the pclntab magic and shape since 1.20, the
// moduledata fields up to types, 32-byte function alignment -- is absent
// from here; a field that appears is one a release changed.
//
// The offsets are byte offsets into the moduledata; zero marks a field the
// release does not have. Parse validates a descriptor against the image's
// own invariants before using it (see Parse), so a wrong descriptor is a
// decline, not a misparse.
type Layout struct {
	ID  LayoutID
	Ver string // the debug/buildinfo version prefix that selects it

	// moduledata field offsets, from types onward: everything before that
	// has been in the same place since 1.20.
	Types, Etypes                     int
	Typedesclen, Itaboffset, Itabsize int // 1.27's sorted descriptor section
	Typelinks, Itablinks              int // the slices it replaced
	Rodata, Gofunc, Epclntab          int
	Need                              int // bytes of moduledata the offsets reach

	// SortedTypes says the type descriptors form one sorted section whose
	// head is the walk's roots (1.27); otherwise the roots are the
	// .typelink and .itablink sections.
	SortedTypes bool

	// MapSize is sizeof(abi.MapType), which grew 24 B in 1.27.
	MapSize int
}

// layouts are tried in order; the first whose Ver prefixes the image's
// version string is the candidate. A release with no descriptor declines,
// which is the codec's whole answer to a Go version it has not been taught:
// the plain codec still patches it, just less well.
var layouts = []*Layout{
	{
		ID: LayoutGo126, Ver: "go1.26",
		Types: 296, Etypes: 304, Rodata: 312, Gofunc: 320, Epclntab: 328,
		Typelinks: 360, Itablinks: 384, Need: 408,
		MapSize: 112,
	},
	{
		ID: LayoutGo127, Ver: "go1.27",
		Types: 296, Typedesclen: 304, Etypes: 312, Itaboffset: 320, Itabsize: 328,
		Rodata: 336, Gofunc: 344, Epclntab: 352, Need: 360,
		SortedTypes: true, MapSize: 136,
	},
}

// LayoutFor returns the descriptor for a buildinfo version string, or nil.
func LayoutFor(goVer string) *Layout {
	for _, l := range layouts {
		if strings.HasPrefix(goVer, l.Ver) {
			return l
		}
	}
	return nil
}

// LayoutByID returns the descriptor a patch names, or nil.
func LayoutByID(id LayoutID) *Layout {
	for _, l := range layouts {
		if l.ID == id {
			return l
		}
	}
	return nil
}

// SupportedGo lists the releases with a descriptor, for messages.
func SupportedGo() string {
	var vs []string
	for _, l := range layouts {
		vs = append(vs, l.Ver)
	}
	return strings.Join(vs, ", ")
}
