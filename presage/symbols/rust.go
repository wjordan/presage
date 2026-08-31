package symbols

import "strings"

// CanonicalName erases the parts of a symbol name that a rebuild changes
// without changing what the symbol *is*, so the encoder's matcher can pair
// the two builds' functions by name.
//
// Only Rust's v0 mangling is rewritten. A v0 name spells every crate
// reference as `Cs<disambiguator>_<len><name>`, and the disambiguator is a
// hash of the package id — it moves whenever the crate's version, features
// or (for rustc itself) commit move, so between two builds of the same
// program almost every v0 name differs while naming the same function.
// Erasing it is safe because v0 spells the generic instantiation out
// structurally: two monomorphisations of one path differ in their type
// arguments, not in a hash. Measured on two adjacent rustc nightlies, name
// identity over `librustc_driver.so` goes from 6.0 % to 93.3 %
// (docs/general/research/domain-rust.md §2).
//
// Legacy Rust mangling (`_ZN...17h<hash>E`) is deliberately left alone: its
// hash is the only field separating two monomorphisations of the same path,
// and it churns ~4 % across a version bump, so erasing it would lose more
// than it recovers. Itanium C++ names are untouched for the same reason.
//
// This is encoder-side evidence only. An over-eager rewrite can pair two
// functions that are not the same one, which costs patch bytes and never
// correctness: the prediction hash and the residual still hold.
func CanonicalName(name string) string {
	if len(name) < 3 || name[0] != '_' || name[1] != 'R' {
		return name
	}
	var b strings.Builder
	last := 0
	for i := 0; i+2 < len(name); i++ {
		if name[i] != 'C' || name[i+1] != 's' {
			continue
		}
		j := i + 2
		for j < len(name) && isBase62(name[j]) {
			j++
		}
		// `Cs<base-62-number>_` must be followed by the identifier's
		// decimal length; without that this is some other `Cs`.
		if j >= len(name) || name[j] != '_' || j+1 >= len(name) || name[j+1] < '0' || name[j+1] > '9' {
			continue
		}
		if j == i+2 {
			i = j // already `Cs_`
			continue
		}
		b.WriteString(name[last:i])
		b.WriteString("Cs_")
		last = j + 1
		i = j
	}
	if last == 0 {
		return name
	}
	b.WriteString(name[last:])
	return b.String()
}

func isBase62(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
