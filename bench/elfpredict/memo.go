package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
)

// A content-addressed memo for the expensive construction stages.
//
//	key = sha256(stage name, code identity, input identities, flags)
//
// Code identity is what makes this safe to leave on disk across edits. Two are
// computed once at startup from the checkout this binary was built from:
//
//	codeHarness  the .go sources of bench/elfpredict, plus go.mod/go.sum
//	codeCodec    those plus delta/ -- the correction codec and the x86
//	             instruction decoder every prediction and reference walk runs on
//
// so an edit to delta/x86 invalidates every stage that decodes an instruction
// while leaving the symbol parse -- 2.9 GB of DWARF-bearing ELF -- alone.
//
// Sources are hashed rather than debug.ReadBuildInfo's VCS revision because
// uncommitted edits are exactly the case that has to invalidate and the
// revision cannot see them; and rather than the running executable because
// `go run` rebuilds that whenever any dependency changes, including ones no
// memoised stage depends on. If the checkout cannot be found -- a binary built
// with -trimpath, or moved away from its sources -- both identities fall back
// to the SHA-256 of the running executable, which over-invalidates and never
// under-invalidates.
//
// Input files are identified by path, size and mtime rather than content: the
// inputs are 3.5 GB of frozen release binaries and DWARF, and re-reading them
// only to hash them would cost more than several of the stages being memoised.
// Touch an input and the memo invalidates; rewrite one with an identical size
// and mtime and it will not.
//
// Memoised stages, and what invalidates each:
//
//	symbols/{old,new}  loadCodeUnits over a chrome.debug
//	                   -- codeHarness, the debug file, the .text geometry
//	reference-targets  the branch-target domain of the record's §9.12
//	                   -- codeCodec, the old .text bytes, the function map
//	plans              the five *-plan.bin artifacts, i.e. every stage from
//	                   symbol parse through per-function selection
//	                   -- codeCodec, all five input files
//
// The memo is only ever an optimisation: a miss, an unreadable file, a short
// read or a failed checksum falls through to the real work.

// memoDir holds the memo. It is deliberately not os.TempDir(): /tmp is tmpfs
// on this host, so the old reference-target cache -- 48 MB, 50 s to rebuild --
// was one reboot from gone.
var memoDir = defaultMemoDir()

func defaultMemoDir() string {
	if d := os.Getenv("ELFPREDICT_MEMO"); d != "" {
		return d
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "presage-chrome-zucchini", "memo")
	}
	return ""
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}

// moduleDir finds the checkout this binary was built from by walking up from
// this file's compiled-in path to the go.mod.
func moduleDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return ""
	}
	dir := filepath.Dir(file)
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// hashSources digests every non-test .go file under the named directories,
// plus the module files, plus the toolchain version.
func hashSources(root string, dirs []string) (string, bool) {
	h := sha256.New()
	h.Write([]byte(runtime.Version()))
	for _, name := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return "", false
		}
		h.Write(b)
	}
	for _, d := range dirs {
		var files []string
		err := filepath.WalkDir(filepath.Join(root, d), func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !e.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return "", false
		}
		slices.Sort(files)
		for _, p := range files {
			b, err := os.ReadFile(p)
			if err != nil {
				return "", false
			}
			rel, _ := filepath.Rel(root, p)
			fmt.Fprintf(h, "%s:%d:", rel, len(b))
			h.Write(b)
		}
	}
	return hexString(h.Sum(nil))[:16], true
}

func exeHash() string {
	p, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	f, err := os.Open(p)
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "unknown"
	}
	return hexString(h.Sum(nil))[:16]
}

var (
	codeOnce               sync.Once
	codeHarness, codeCodec string
)

func initCode() {
	codeOnce.Do(func() {
		root := moduleDir()
		if root != "" {
			h, ok1 := hashSources(root, []string{"bench/elfpredict"})
			c, ok2 := hashSources(root, []string{"bench/elfpredict", "delta"})
			if ok1 && ok2 {
				codeHarness, codeCodec = "src-"+h, "src-"+c
				return
			}
		}
		e := "exe-" + exeHash()
		codeHarness, codeCodec = e, e
	})
}

func harnessCode() string { initCode(); return codeHarness }
func codecCode() string   { initCode(); return codeCodec }

// fileID identifies an input by path, size and mtime. See the package comment
// for why this is not a content hash.
func fileID(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return path + "|missing"
	}
	return fmt.Sprintf("%s|%d|%d", path, st.Size(), st.ModTime().UnixNano())
}

func memoKey(stage, code string, parts ...string) string {
	h := sha256.New()
	fmt.Fprintf(h, "elfpredict-memo-v1\x00%s\x00%s\x00", stage, code)
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return stage + "-" + hexString(h.Sum(nil))[:32]
}

func memoPath(key string) string {
	if memoDir == "" {
		return ""
	}
	return filepath.Join(memoDir, key+".bin")
}

// memoLoad returns a stored payload. Entries carry an 8-byte SHA-256 prefix:
// a stale or truncated entry that decoded as a plan would not fail loudly, it
// would make every measurement taken after it fiction.
func memoLoad(key string) ([]byte, bool) {
	p := memoPath(key)
	if p == "" {
		return nil, false
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) < 8 {
		return nil, false
	}
	sum := sha256.Sum256(b[8:])
	if string(sum[:8]) != string(b[:8]) {
		return nil, false
	}
	return b[8:], true
}

func memoStore(key string, payload []byte) {
	p := memoPath(key)
	if p == "" {
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	sum := sha256.Sum256(payload)
	b := append(append([]byte(nil), sum[:8]...), payload...)
	tmp := fmt.Sprintf("%s.tmp%d", p, os.Getpid())
	if os.WriteFile(tmp, b, 0o644) == nil {
		if os.Rename(tmp, p) != nil {
			os.Remove(tmp)
		}
	}
}

// packBlobs and unpackBlobs carry a fixed number of byte streams in one entry.
func packBlobs(bs ...[]byte) []byte {
	var b []byte
	for _, s := range bs {
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	return b
}

func unpackBlobs(b []byte, n int) ([][]byte, bool) {
	out := make([][]byte, 0, n)
	for range n {
		v, k := binary.Uvarint(b)
		if k <= 0 || v > uint64(len(b)-k) {
			return nil, false
		}
		b = b[k:]
		out = append(out, b[:v])
		b = b[v:]
	}
	return out, len(b) == 0
}

// --- memoised stage: symbol parse ---------------------------------------

func packUnits(units []codeUnit, st symbolStats) []byte {
	b := make([]byte, 0, len(units)*24+32)
	for _, v := range []int{len(units), st.FunctionSymbols, st.AddressUnits, st.CoveredBytes} {
		b = binary.AppendUvarint(b, uint64(v))
	}
	for _, u := range units {
		b = binary.AppendUvarint(b, u.Off)
		b = binary.AppendUvarint(b, u.Size)
		b = binary.AppendUvarint(b, uint64(len(u.Names)))
		for _, n := range u.Names {
			b = append(b, n[:]...)
		}
	}
	return b
}

func unpackUnits(b []byte) ([]codeUnit, symbolStats, bool) {
	next := func() (uint64, bool) {
		v, k := binary.Uvarint(b)
		if k <= 0 {
			return 0, false
		}
		b = b[k:]
		return v, true
	}
	n, ok := next()
	if !ok || n > 1<<30 {
		return nil, symbolStats{}, false
	}
	var st symbolStats
	for _, p := range []*int{&st.FunctionSymbols, &st.AddressUnits, &st.CoveredBytes} {
		v, ok := next()
		if !ok {
			return nil, symbolStats{}, false
		}
		*p = int(v)
	}
	units := make([]codeUnit, 0, n)
	for range n {
		off, ok1 := next()
		size, ok2 := next()
		names, ok3 := next()
		if !ok1 || !ok2 || !ok3 || names > uint64(len(b))/16 {
			return nil, symbolStats{}, false
		}
		u := codeUnit{Off: off, Size: size, Names: make([]nameID, names)}
		for i := range u.Names {
			copy(u.Names[i][:], b)
			b = b[16:]
		}
		units = append(units, u)
	}
	return units, st, len(b) == 0
}

// memoCodeUnits memoises the DWARF-bearing symbol parse. It is keyed on the
// harness sources only, so a change to delta/x86 does not throw away 2.9 GB of
// ELF parsing that never looked at an instruction.
func memoCodeUnits(label, path string, text section) ([]codeUnit, symbolStats, error) {
	key := memoKey("symbols", harnessCode(), fileID(path),
		fmt.Sprintf("%d|%d|%d", text.Addr, text.Off, text.Size))
	if b, ok := memoLoad(key); ok {
		if units, st, ok := unpackUnits(b); ok {
			startStage("symbols "+label).done("memo hit; %d symbols -> %d units", st.FunctionSymbols, st.AddressUnits)
			return units, st, nil
		}
	}
	t := startStage("symbols " + label)
	units, st, err := loadCodeUnits(path, text)
	if err != nil {
		return nil, symbolStats{}, err
	}
	t.done("%d symbols -> %d units", st.FunctionSymbols, st.AddressUnits)
	memoStore(key, packUnits(units, st))
	return units, st, nil
}

// --- memoised stage: reference-target domain -----------------------------

var (
	targetsMu    sync.Mutex
	targetsLocal = map[string][]uint64{}
)

// cachedReferenceTargets memoises the branch-target domain. Every point in a
// plan is an index into it, so a stale entry would not fail loudly: it would
// decode every point to the wrong address. Hence the code identity in the key
// and the checksum on the entry.
func cachedReferenceTargets(oldText []byte, maps []mapping, oldAddr uint64) []uint64 {
	h := sha256.New()
	h.Write(oldText)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], oldAddr)
	h.Write(buf[:])
	for _, m := range maps {
		binary.LittleEndian.PutUint64(buf[:], m.Src)
		h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], m.SrcSize)
		h.Write(buf[:])
	}
	digest := hexString(h.Sum(nil))

	targetsMu.Lock()
	if v, ok := targetsLocal[digest]; ok {
		targetsMu.Unlock()
		return v
	}
	targetsMu.Unlock()

	key := memoKey("reference-targets", codecCode(), digest)
	if b, ok := memoLoad(key); ok && len(b) != 0 && len(b)%8 == 0 {
		out := make([]uint64, len(b)/8)
		for i := range out {
			out[i] = binary.LittleEndian.Uint64(b[i*8:])
		}
		targetsMu.Lock()
		targetsLocal[digest] = out
		targetsMu.Unlock()
		return out
	}
	t := startStage("reference-targets")
	out := referenceTargets(oldText, maps, oldAddr)
	t.done("%d targets over %d mappings", len(out), len(maps))
	b := make([]byte, len(out)*8)
	for i, v := range out {
		binary.LittleEndian.PutUint64(b[i*8:], v)
	}
	memoStore(key, b)
	targetsMu.Lock()
	targetsLocal[digest] = out
	targetsMu.Unlock()
	return out
}

// --- memoised stage: the whole of plan construction ----------------------

// planArtifacts is the whole of construction as the whole-image rungs see it:
// five serialized plans and nothing else. -resume already replays a run from
// these five files, so memoising them memoises every stage above them.
type planArtifacts struct {
	Equivalence []byte
	AllMapped   []byte
	Derived     []byte
	Retarget    []byte
	Selected    []byte
}

var planArtifactNames = [5]string{
	"equivalence-plan.bin", "all-mapped-plan.bin", "equivalence-derived-plan.bin",
	"equivalence-retarget-plan.bin", "equivalence-selected-plan.bin",
}

func (a planArtifacts) blobs() [][]byte {
	return [][]byte{a.Equivalence, a.AllMapped, a.Derived, a.Retarget, a.Selected}
}

func (a planArtifacts) write(dir string) error {
	if dir == "" {
		return nil
	}
	for i, b := range a.blobs() {
		if err := writeFile(dir, planArtifactNames[i], b); err != nil {
			return err
		}
	}
	return nil
}

func readPlanArtifacts(dir string) (planArtifacts, error) {
	var a planArtifacts
	into := []*[]byte{&a.Equivalence, &a.AllMapped, &a.Derived, &a.Retarget, &a.Selected}
	for i, name := range planArtifactNames {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return planArtifacts{}, err
		}
		*into[i] = b
	}
	return a, nil
}

func plansMemoKey(inputs ...string) string {
	parts := make([]string, len(inputs))
	for i, p := range inputs {
		parts[i] = fileID(p)
	}
	return memoKey("plans", codecCode(), parts...)
}

func loadPlansMemo(key string) (planArtifacts, bool) {
	b, ok := memoLoad(key)
	if !ok {
		return planArtifacts{}, false
	}
	bs, ok := unpackBlobs(b, 5)
	if !ok {
		return planArtifacts{}, false
	}
	return planArtifacts{bs[0], bs[1], bs[2], bs[3], bs[4]}, true
}

func storePlansMemo(key string, a planArtifacts) {
	memoStore(key, packBlobs(a.blobs()...))
}
