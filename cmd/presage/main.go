// presage is the codec's command line: a patch from references to a
// target, and its application.
//
//	presage diff <old> <new> -o <patch>          make a patch
//	presage diff -symbols OLD,NEW <old> <new> -o <patch>
//	                                             the same, with the images' function
//	                                             symbols (encoder-only; ELF or Breakpad)
//	presage patch <old> <patch> -o <new>         apply one, verifying the result
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wjordan/presage/presage"
	"github.com/wjordan/presage/presage/elfmod"
	"github.com/wjordan/presage/presage/modules"
	"github.com/wjordan/presage/presage/symbols"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "diff":
		err = diff(os.Args[2:])
	case "patch":
		err = apply(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "presage:", err)
		var u *presage.ErrUnsupported
		if errors.As(err, &u) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// flagsFirst lets flags follow the positional arguments, as in
// "presage diff old new -o patch".
func flagsFirst(fs *flag.FlagSet, args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if f := fs.Lookup(name); f != nil && !strings.Contains(a, "=") {
				if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
					if i+1 < len(args) {
						i++
						flags = append(flags, args[i])
					}
				}
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: presage diff [-symbols OLD,NEW] <old> <new> -o <patch> | presage patch <old> <patch> -o <new>")
	os.Exit(2)
}

func diff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	out := fs.String("o", "", "write the patch here (required)")
	verbose := fs.Bool("v", false, "report where the patch bytes went")
	only := fs.String("modules", "", "restrict the encoder to these modules, by name (e.g. lz,eq)")
	symPaths := fs.String("symbols", "", "function symbols of the old and new images, comma-separated (ELF with .symtab, or Breakpad text); encoder-only")
	fs.Parse(flagsFirst(fs, args))
	if fs.NArg() != 2 || *out == "" {
		usage()
	}
	syms, err := openSymbols(*symPaths)
	if err != nil {
		return err
	}
	var elfStats elfmod.Stats
	reg := modules.RegistryStats(&elfStats, syms[0], syms[1])
	allowed, err := moduleIDs(reg, *only)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	target, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		return err
	}
	start := time.Now()
	var st presage.Stats
	patch, err := presage.Encode([][]byte{old}, target, presage.Options{Registry: reg, Modules: allowed, Stats: &st})
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, patch, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "presage: %d B patch for %d B target in %s\n", len(patch), len(target), time.Since(start).Round(time.Millisecond))
	if *verbose {
		for i, r := range st.Regions {
			fmt.Fprintf(os.Stderr, "  region %d: %s, %d B, plan %d B, residual %d B, %d mispredicted bytes\n",
				i, r.Module, r.Length, r.Plan, r.Residual, r.PredictErr)
		}
		for _, n := range st.Notes {
			fmt.Fprintf(os.Stderr, "  %s\n", n)
		}
		for _, n := range elfStats.Notes {
			fmt.Fprintf(os.Stderr, "  elf: %s\n", n)
		}
	}
	return nil
}

// openSymbols parses -symbols: empty for none, else exactly two paths,
// old then new. One path is not applied to both images unless written
// twice ("-symbols A,A"): symbols of the wrong image are worse than none.
func openSymbols(spec string) ([2]symbols.Reader, error) {
	var syms [2]symbols.Reader
	if spec == "" {
		return syms, nil
	}
	paths := strings.Split(spec, ",")
	if len(paths) != 2 {
		return syms, fmt.Errorf("-symbols wants OLD,NEW (two paths), got %d", len(paths))
	}
	for i, p := range paths {
		r, err := symbols.Open(p)
		if err != nil {
			return syms, err
		}
		syms[i] = r
	}
	return syms, nil
}

// moduleIDs resolves a comma-separated list of module names; empty means
// every registered module.
func moduleIDs(reg *presage.Registry, names string) ([]byte, error) {
	if names == "" {
		return nil, nil
	}
	var ids []byte
	for _, name := range strings.Split(names, ",") {
		found := false
		for _, m := range reg.Candidates() {
			if m.Name() == name {
				ids, found = append(ids, m.ID()), true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown module %q", name)
		}
	}
	return ids, nil
}

func apply(args []string) error {
	fs := flag.NewFlagSet("patch", flag.ExitOnError)
	out := fs.String("o", "", "write the result here (required)")
	fs.Parse(flagsFirst(fs, args))
	if fs.NArg() != 2 || *out == "" {
		usage()
	}
	old, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	patch, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		return err
	}
	start := time.Now()
	var buf bytes.Buffer
	if err := presage.Apply([][]byte{old}, patch, modules.Registry(), &buf); err != nil {
		return err
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "presage: applied, %d B in %s\n", buf.Len(), time.Since(start).Round(time.Millisecond))
	return nil
}
