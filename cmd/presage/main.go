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
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"runtime/pprof"
	rtrace "runtime/trace"
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

type profileFlags struct {
	cpu, alloc, trace string
}

func addProfileFlags(fs *flag.FlagSet) *profileFlags {
	p := &profileFlags{}
	fs.StringVar(&p.cpu, "cpuprofile", "", "write a CPU profile here")
	fs.StringVar(&p.alloc, "allocprofile", "", "write a cumulative allocation profile here")
	fs.StringVar(&p.trace, "traceprofile", "", "write a runtime execution trace here")
	return p
}

// startProfiles opens every requested destination before starting work, so a
// bad diagnostic path fails the command early rather than after a long encode.
// The returned closure stops sampling and tracing before it snapshots
// allocations.
func startProfiles(p *profileFlags) (func(), error) {
	type destination struct {
		name, path string
		file       *os.File
	}
	dsts := []*destination{{"cpuprofile", p.cpu, nil}, {"allocprofile", p.alloc, nil}, {"traceprofile", p.trace, nil}}
	seen := make(map[string]string)
	for _, d := range dsts {
		if d.path == "" {
			continue
		}
		if other := seen[d.path]; other != "" {
			return nil, fmt.Errorf("-%s and -%s need different files", other, d.name)
		}
		seen[d.path] = d.name
	}
	closeAll := func() {
		for _, d := range dsts {
			if d.file != nil {
				d.file.Close()
			}
		}
	}
	for _, d := range dsts {
		if d.path == "" {
			continue
		}
		var err error
		d.file, err = os.Create(d.path)
		if err != nil {
			closeAll()
			return nil, err
		}
	}
	cpu, alloc, trace := dsts[0].file, dsts[1].file, dsts[2].file
	if trace != nil {
		if err := rtrace.Start(trace); err != nil {
			closeAll()
			return nil, err
		}
	}
	if cpu != nil {
		if err := pprof.StartCPUProfile(cpu); err != nil {
			if trace != nil {
				rtrace.Stop()
			}
			closeAll()
			return nil, err
		}
	}
	return func() {
		if cpu != nil {
			pprof.StopCPUProfile()
		}
		if trace != nil {
			rtrace.Stop()
		}
		if alloc != nil {
			if err := pprof.Lookup("allocs").WriteTo(alloc, 0); err != nil {
				fmt.Fprintln(os.Stderr, "presage: write allocation profile:", err)
			}
		}
		closeAll()
	}, nil
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
	prof := addProfileFlags(fs)
	out := fs.String("o", "", "write the patch here (required)")
	verbose := fs.Bool("v", false, "report where the patch bytes went")
	only := fs.String("modules", "", "restrict the encoder to these modules, by name (e.g. lz,eq)")
	price := fs.Int("price", 0, "what a second of encoding is worth in patch bytes (0: the default; -1: nothing, spend any time for the smallest patch)")
	symPaths := fs.String("symbols", "", "function symbols of the old and new images, comma-separated (ELF with .symtab, or Breakpad text); encoder-only")
	fs.Parse(flagsFirst(fs, args))
	if fs.NArg() != 2 || *out == "" {
		usage()
	}
	stopProfiles, err := startProfiles(prof)
	if err != nil {
		return err
	}
	defer stopProfiles()
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
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(defaultEncodeMemoryLimit(int64(len(old)), int64(len(target))))
	}
	start := time.Now()
	var st presage.Stats
	patch, err := presage.Encode([][]byte{old}, target, presage.Options{
		Registry: reg, Modules: allowed, Stats: &st, Price: *price,
	})
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

func defaultEncodeMemoryLimit(referenceSize, targetSize int64) int64 {
	const minLimit = 1536 << 20
	// The ELF whole-image matcher holds the inputs, two canonical matching
	// copies, and a four-byte-per-source-offset seed index plus bucket table.
	// Eight times the larger image, with this floor, is the measured GC knee
	// on the C++ corpus; forcing it lower adds collection work without lowering
	// RSS because this working set is simultaneously live.
	size := max(referenceSize, targetSize)
	if size > int64(^uint64(0)>>1)/8 {
		return int64(^uint64(0) >> 1)
	}
	limit := size * 8
	if limit < minLimit {
		return minLimit
	}
	return limit
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
	prof := addProfileFlags(fs)
	out := fs.String("o", "", "write the result here (required)")
	fs.Parse(flagsFirst(fs, args))
	if fs.NArg() != 2 || *out == "" {
		usage()
	}
	stopProfiles, err := startProfiles(prof)
	if err != nil {
		return err
	}
	defer stopProfiles()
	old, unmap, err := readWhole(fs.Arg(0))
	if err != nil {
		return err
	}
	if unmap != nil {
		defer unmap()
	}
	patch, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		return err
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		h, err := presage.ParseHeader(patch)
		if err != nil {
			return err
		}
		// Applying is the CLI's only substantial job, so give its transient
		// heap a target-derived soft ceiling. The reference mmap and runtime
		// metadata sit outside this budget; an explicit GOMEMLIMIT always wins.
		debug.SetMemoryLimit(defaultApplyMemoryLimit(h.Size))
	}
	start := time.Now()
	// Straight to the file: buffering the result first copied the whole image
	// a third time and held a second 291 MB of it.
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	w := &countWriter{w: f}
	var releaseReferencePages func()
	if unmap != nil && canDiscardWhole() {
		releaseReferencePages = func() { discardWhole(old) }
	}
	if err := presage.Apply([][]byte{old}, patch, modules.RegistryForApply(releaseReferencePages), w); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "presage: applied, %d B in %s\n", w.n, time.Since(start).Round(time.Millisecond))
	return nil
}

func defaultApplyMemoryLimit(targetSize int64) int64 {
	const minLimit = 256 << 20
	limit := targetSize * 3 / 2
	if limit < minLimit {
		return minLimit
	}
	return limit
}

// countWriter reports how many bytes reached the file, for the summary line.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
