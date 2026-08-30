// Package modules assembles the registry of every module this build ships:
// the core ones, then go, then elf. Candidates run in id order, so a Go
// binary is taken by go and elf sees only what go declined.
package modules

import (
	"github.com/wjordan/presage/presage"
	"github.com/wjordan/presage/presage/elfmod"
	"github.com/wjordan/presage/presage/gomod"
	"github.com/wjordan/presage/presage/symbols"
)

// Registry returns the full registry. syms are the encoder-side function
// symbols for reference 0 and the target (nil for none); decoders pass
// nothing.
func Registry(syms ...symbols.Reader) *presage.Registry {
	var s [2]symbols.Reader
	copy(s[:], syms)
	r := presage.NewRegistry()
	r.Add(gomod.Module{})
	r.Add(elfmod.Module{Symbols: s})
	return r
}
