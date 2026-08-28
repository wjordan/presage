# Compiled-code formats as inputs to a predictive delta codec

Research notes for generalising go-binsync's predict-then-correct codec into
a framework with pluggable per-format predictors. The question for each
format is the same one pclntab answered for Go: **what survives in the shipped
artefact that gives a function/section-level correspondence between two
versions, and how much of the second-order change (the churn caused by things
moving) can be reconstructed from it?**

Conventions: "verified" means read from a primary source or reproduced
locally on 2026-08-27; "unverified" means recalled from general knowledge
and not confirmed against a source during this pass. Numbers are quoted with
their source. Local experiments were run on linux/amd64 with gcc, rustc
(stable, `strip` from binutils) and Go 1.25.

Related in-repo material: `docs/DESIGN.md` (the Go predictor and its measured
28-67x over bsdiff), `docs/research/binary-delta.md`, `docs/research/go-binary-layout.md`.

---

## 1. Prior art: Courgette and Zucchini

### 1.1 Courgette (2009): disassemble → adjust → assemble

Source: [Chromium design doc](https://www.chromium.org/developers/design-documents/software-updates-courgette/),
[Evan Martin's note](https://neugierig.org/software/chromium/notes/2009/05/courgette.html).

- A "primitive disassembler" turns x86 code into an instruction-like stream in
  which rel32/abs32 operands are replaced by *labels* (indices into a symbol
  table of targets). The old file's label numbering is then *adjusted*
  (renumbered) to maximise byte-identity with the new file's stream; bsdiff runs
  on the adjusted streams; the client re-assembles.
- Numbers (Chrome dev 190.1→190.4, verified from the design doc): full update
  10,385,920 B; bsdiff 704,512 B; Courgette 78,848 B (8.9x over bsdiff). The
  transform to assembly alone cut the bsdiff output ~30%; the label adjustment
  step did the rest.
- Stated limitations: disassembly/assembly must be exact inverses (any byte the
  disassembler mis-classifies must be repaired by a trailing bsdiff "fix-up"
  patch — Martin: "the patch-generated output isn't quite identical to the
  target executable"); only well-formed single executables; client-side cost.
- Courgette later gained ELF x86 and ELF ARM disassemblers (unverified: the
  Chromium tree had `courgette/disassembler_elf_32_arm.cc`; I did not find a
  published result for the ARM path in this pass).

### 1.2 Why Courgette was dropped

Sources: [chromium-dev "Is courgette deprecated"](https://groups.google.com/a/chromium.org/g/chromium-dev/c/M-FQRn6baB0),
[chromium-dev "zucchini performance compared to courgette"](https://groups.google.com/a/chromium.org/g/chromium-dev/c/-8JPrR7GQOg),
[Zucchini README](https://chromium.googlesource.com/chromium/src/+/main/components/zucchini/README.md).

- Samuel Huang (Zucchini author): the advantages of Zucchini over Courgette are
  "(compressed) patch size, patch-application memory, and modernity";
  generation speed is *not* significantly better. Courgette's BSDiff component
  is still used by the component updater.
- Courgette's generator crashed/ran out of memory on newer binaries
  (crbug 1286318, cited in the thread).
- Structural reason (my reading of the two designs, not a quote): Courgette's
  correctness depends on *round-tripping* the whole file through a disassembler
  and assembler. Anything the disassembler cannot model has to be carried as a
  side-channel bsdiff, and the client has to hold the whole assembled image in
  memory. Zucchini never reassembles: it treats the file as bytes plus a set of
  typed references, so a missed or mis-detected reference costs only a few
  correction bytes and can never break the output.

### 1.3 Zucchini (2017–): equivalences + typed references + labels

Source: [README](https://chromium.googlesource.com/chromium/src/+/main/components/zucchini/README.md)
(verified), plus header comments in `rel32_finder.h`, `encoded_view.h`,
`equivalence_map.h`, `disassembler_elf.h`, `disassembler_dex.h`, `arm_utils.h`,
`ensemble_matcher.h` (verified from the GitHub mirror).

Pipeline (generation):

1. **Ensemble matching.** Scan the archive for embedded *elements* (regions
   with an executable type); match each new element to an old one by heuristic
   (size ratio, offset distance). Exact-identical elements are skipped
   ("patched directly"). A new element may have no match.
2. **Disassemble** each element into *references* `(location, type, target)`.
   Two families:
   - **abs32**: locations come from the relocation table (PE `.reloc` types 3/10;
     ELF `R_386/X86_64/ARM/AARCH64_RELATIVE`). Fixed width; trusted.
   - **rel32**: found by a *heuristic linear scan* of executable sections
     ("naive scan for opcodes that have rel32 as an argument, and disregard
     instruction alignment"; "errors are tolerated"). Candidates whose target
     falls outside the executable section are rejected. The scan skips gaps
     occupied by abs32 bodies so bodies never overlap.
   - Per-ISA rel32 types (verified from `arm_utils.h` / `disassembler_elf.h`):
     x86/x64: one `kRel32` type (call/jmp/jcc/rip-relative).
     AArch32: `A24` (ARM B/BL), `T8`, `T11`, `T20`, `T24` (Thumb-2 B/BL/BLX
     forms). ARM-vs-Thumb mode is "assumed constant for an entire section".
     AArch64: `Immd14` (TBZ/TBNZ), `Immd19` (B.cond/LDR literal/CBZ),
     `Immd26` (B/BL). **No ADRP/ADD or ADRP/LDR page-pair handling, no
     MOVW/MOVT, no literal pools.** So the largest class of AArch64 data
     references (ADRP pairs) is *not* corrected by Zucchini; they show up as
     raw deltas.
   - DEX: ~30 typed references — string/type/proto/field/method *indices* from
     bytecode and from the id tables, `kCodeToRelCode8/16/32` branch offsets,
     and structural offsets (class_def→class_data, annotations, static values).
     This is the one place Zucchini handles *index-space* renumbering rather
     than address arithmetic, and it is the model for what a Wasm or .NET
     predictor would do.
   - ZTF ("Zucchini Text Format") is a debugging-only text format with
     `<line,col>` references; not used in production.
   - **There is no ZIP disassembler** (verified: header list). Archives are
     handled by element detection over raw bytes, which only works when the
     archive stores members uncompressed (chrome.7z; APKs with stored `.so`;
     Android partitions after puffin re-inflation).
3. **Targets → labels.** Within each *pool* (semantically related targets,
   e.g. "code addresses", "string-table indices"), a target's *key* is its
   index in the sorted target list. Old and new targets are *associated* when
   they sit at the same local offset inside a matched region and are not
   claimed by a larger region; a *target affinity* score from surrounding
   content and reference type breaks ties. Associated targets share a *label*;
   unmatched targets get label 0.
4. **Encoded view.** Project each byte to a scalar: raw bytes stay 0–255, the
   first byte of a reference becomes `256 + f(type,label)`, the remaining
   bytes of a reference become a padding value 256. Matching runs on this
   projection, so two copies of a function whose calls point at the "same"
   labelled targets match even though every rel32 byte differs.
5. **Equivalence map.** Suffix array over the old encoded view; seeds are
   extended forward/backward maximising a similarity score; candidates are
   pruned to be non-overlapping in the new image (overlap in old is allowed).
   Iterated: labels are re-derived from the new equivalences and the map is
   recomputed.
6. **Patch** = per element: equivalence list `(src_skip, dst_skip, copy_count)`
   varints, extra data (new bytes not covered), *raw deltas*
   `(copy_offset, diff byte)`, *reference deltas* (per reference, in new-image
   order, a signed jump through the target list — zero when the label
   prediction is right), and extra targets per pool. The whole thing is then
   7z/LZMA-compressed by the updater.

Costs (README, verified): "~15–20 min for Chrome, using 1 GB of RAM" to
generate; hydraulic.dev reports "20 minutes and many gigabytes of RAM"
([Deltas diffed](https://hydraulic.dev/blog/20-deltas-diffed.html)). Apply is
"about twice as long as mbsdiff (Courgette is about 4x mbsdiff)" partially
offset by faster LZMA decompression of the smaller patch
([Mozilla bug 1632374](https://bugzilla.mozilla.org/show_bug.cgi?id=1632374)).

A notable robustness bug: format 2.0 (2023-04) fixed an `std::sort` tie-break
non-determinism in `OffsetMapper` that made patches fail "across different
builds" — a reminder that any predictor's *client* half must be bit-exact
across toolchain versions.

### 1.4 Measured patch sizes

| Pair | mbsdiff | Courgette | Zucchini | Zucchini vs mbsdiff |
|---|---|---|---|---|
| Firefox 68.0.2→69.0 (major) | 27.33 MB | 23.83 MB | 21.18 MB | −22.5% |
| Firefox 69.0→69.0.1 (point) | 5.17 MB | 2.19 MB | 1.94 MB | −62.5% |
| Firefox 69.0.3→70.0 (major) | 18.42 MB | 14.19 MB | 12.52 MB | −32.0% |
| Firefox 70.0→70.0.1 (point) | 5.54 MB | 2.46 MB | 2.15 MB | −61.1% |

Source: Mozilla bug 1632374 comment 0 (verified). Mozilla's projection for
their full update pipeline was −33% on Windows, −10% Linux, −9.7% macOS
(comment 5), i.e. much of a Firefox MAR is not executable code.

Two things to take from this table. First, even on point releases Zucchini is
a 2.5–2.7x win over bsdiff, not 28–67x; Chrome/Firefox point releases contain
real code changes across hundreds of files, so the residual is dominated by
first-order change, and the Go numbers in `DESIGN.md` are for one-line
changes. Second, Courgette was already within 12–15% of Zucchini on patch
size; the replacement was about memory, robustness and maintainability, not a
compression breakthrough.

**Chrome ELF probe (measured).** On the public Google Chrome
151.0.7922.169→151.0.7922.173 linux/amd64 pair, current Zucchini drops all
1,105,974 candidate absolute references because its ELF reader does not process
RELA `r_addend`. A minimal prototype that feeds those addends through the
existing absolute-reference pool reduced the XZ-compressed Zucchini patch from
5,889,352 B to 5,263,732 B: **625,620 B / 10.62% smaller**, with byte-exact
application. See [`chrome-elf-zucchini.md`](chrome-elf-zucchini.md) for corpus
hashes, generic baselines, implementation, and regression checks.

### 1.5 Older prior art: Exediff and Percival's bsdiff 6

**Exediff** (Baker, Manber, Muth 1999, [PDF](https://robert.muth.org/Papers/1999-exediff.pdf),
verified): DEC Alpha ECOFF. Classifies changes as *primary* (source edits) and
*secondary* (compilation artefacts: shifted addresses, offsets, table
indices). "Pre-matching" aligns instruction sequences per function using the
ECOFF **symbol table** ("matchings each representing one function within the
executable"), then "value recovery" reconstructs secondary changes at patch
time so they are not stored. Table 1: netscape 3.01→3.04, bindiff.gz 1,471,610
B → Exediff 284,992 B (5.2x); "typically a fivefold reduction" for
minor-version upgrades, 2x otherwise; failed (patch 26% *larger* than the
gzipped new binary) on apache 1.2.4→1.3.0 where the source was reorganised.
This is the direct ancestor of go-binsync's approach — function-level alignment
by name plus deterministic reconstruction — and it needed the symbol table,
which is exactly what modern toolchains strip.

**Percival, "Matching with Mismatches" (2006 D.Phil,
[PDF](https://www.daemonology.net/papers/thesis.pdf), verified)**, ch. 2:

- *Block alignment*: split the new file into blocks of length ~n log n, find
  for each block the best alignment in the old file by matching-with-mismatches
  (FFT-based correlation, O(L log L) per block), then move block boundaries to
  reduce mismatches and drop blocks under 50% match. This tolerates the
  "mismatch in one or two out of every four bytes (the low-order bytes of each
  32-bit address)" pattern that defeats exact-match (suffix-array) seeding. It
  is a *symbol-free, disassembly-free aligner*.
- *Encoding of second-order change*: bytewise (or balanced multi-precision)
  subtraction of aligned regions, split into a difference *map* (BWT + position
  coding) and a non-zero difference string, because "the numerical differences
  between the addresses in the new and old files will take on certain values
  far more commonly than others".
- Table 2.1 (15 Alpha upgrade pairs, weighted mean patch/original): bzip2
  36.22%, Xdelta 20.83%, Vcdiff 19.66%, zdelta 19.52%, .RTPatch 10.88%,
  **Exediff 8.41%, bsdiff 6 7.67%**. Table 2.2 (82 FreeBSD 4.7 security-patch
  files): Xdelta 9.28%, .RTPatch 1.89%, bsdiff 6 **1.27%**, bsdiff 6 with
  block-alignment-only 1.38% ("on average only 8% larger" at far lower memory).
  Percival's own conclusion: for security patches "the resulting patch sizes
  depend almost totally upon the efficiency of encoding second-order changes".

The important empirical point: a symbol-free byte aligner (bsdiff 6) *beat*
the symbol-using platform-specific tool (Exediff) on the same corpus. The
platform-specific advantage only re-emerged with Courgette/Zucchini, which
correct references *exactly* rather than encoding their differences
statistically. The gain is in the reference-correction step, not the aligner.

---

## 2. Format survey: what survives, what a predictor can derive

Each subsection ends with a table: what survives in the shipped artefact →
what a predictor can derive from it → the expected residual after prediction.
"Residual" assumes a small source change; it is the class of bytes the
predictor cannot reconstruct and must ship.

A framing point that applies to every format below: **the sender has the
unstripped artefacts.** A release pipeline holds the ELF with `.symtab`, the PDB,
the dSYM, the map file, for *both* versions. The predictor's *inputs* (function
correspondence, layout) can be computed server-side from full information; only
the *client-side replay* must work from the stripped image. What must survive
stripping is therefore only what the client needs to (a) locate operands to
rewrite and (b) regenerate tables — not what is needed to match functions. This
relaxes almost every "no symbols" problem below, at the cost of making the
predictor's wire description (the "prediction inputs" of `DESIGN.md`) carry
the correspondence explicitly.

### 2.1 Go (baseline, done)

Verified locally: a `-ldflags=-s` Go 1.25 hello-world (1.5 MB) has `.text`,
`.rodata`, `.gopclntab` (625 KB of it) and **no `.eh_frame`**, no `.symtab`.
`.gopclntab` names every function with entry and size, plus pc-tables; Go 1.27
adds a sorted `.go.type` with `textOff`s (`DESIGN.md`). Measured 28–67x over
bsdiff on one-line/small changes; 24–28x on prometheus point releases.

Go linker layout (verified from `cmd/link/internal/ld/data.go`,
`textaddress()`): text symbols are laid out in `ctxt.Textp` order, which is
`AssignTextSymbolOrder(ctxt.Library, ...)` — package dependency order, then
per-package object order — followed by a *stable* sort by symbol type (so FIPS
symbols cluster). There is a `-randlayout=<seed>` flag to shuffle (off by
default). Trampolines are inserted in address order when branch limits are
exceeded (ARM64/PPC64). So for the same toolchain, function order is a
deterministic function of the package graph and source order, which is why
"same name → new address" prediction is exact.

| survives stripping | predictor derives | residual |
|---|---|---|
| pclntab: name, entry, size, pc→sp/file/line tables; moduledata; type descriptors; itabs | exact new layout; every rel32/rip-rel operand through the map; regenerated pclntab/findfunctab/type offsets | changed function bodies; new strings/types; pc-table deltas for changed functions |

### 2.2 C/C++ ELF (Linux servers, PIE and shared objects)

Local experiment (verified): `gcc -O2 -pie`, `strip`. Stripped binary keeps
`.dynsym`, `.rela.dyn` (6 `R_X86_64_RELATIVE` for a 3-entry function-pointer
table + startup pointers), `.rela.plt`, `.eh_frame_hdr`, `.eh_frame` (8 FDEs).
With `-Wl,-z,pack-relative-relocs` the relative relocations move into a
24-byte `.relr.dyn`.

- **`.eh_frame` / `.eh_frame_hdr`**: one FDE per function that has unwind
  info, i.e. essentially every non-leaf function compiled with
  `-fasynchronous-unwind-tables` (the default on x86-64 Linux distributions).
  `.eh_frame_hdr` is a *sorted binary-search table of (initial PC, FDE
  pointer)* — literally a function-start table, `pc_begin` + `pc_range` per
  FDE. No names. `strip` never removes it because the unwinder needs it at
  runtime. Caveat: `-fno-asynchronous-unwind-tables` (some embedded/kernel
  builds) or `-fomit-frame-pointer` on i386 removes it; on x86-64 it is on by
  default in GCC and Clang (unverified: exact distro defaults).
- **`.rela.dyn` / `.relr.dyn`**: for a PIE or shared object, the *complete*
  list of absolute-pointer locations in data (vtables, function-pointer
  tables, `.init_array`, `.data.rel.ro`) with their targets (addend). This is
  exactly Zucchini's abs32 pool, and it is also a *ground-truth map of which
  8-byte words in `.data*` are pointers* — a predictor can rewrite them
  through the map and then regenerate the relocation section itself from the
  predicted layout (RELR is a deterministic bitmap encoding of sorted offsets:
  glibc 2.36 / binutils 2.38 / LLVM 15 consumers, lld 17 and bfd 2.43
  producers — [Arch RFC 0023](https://rfc.archlinux.page/0023-pack-relative-relocs/)).
  Non-PIE executables have none of this; their absolute pointers are
  indistinguishable from data, as in Go's default non-PIE build.
- **`.dynsym`**: for shared objects, *names* for every exported (and imported)
  symbol with value and size — a real function map for the exported subset.
  With `-fvisibility=hidden` (the norm for large C++), the exported subset is
  small.
- **`.gnu.hash`, `.dynstr`, `.init_array`, `.got`, `.plt`**: all offset
  tables derivable from layout + symbol order; `.gnu.hash` is a function of
  the dynsym order and names.
- **Function matching without names**: `.eh_frame` gives boundaries; content
  hash of each body with rel32/rip-rel/abs operands masked (Ghidra FID-style,
  §4) matches unchanged functions exactly; call-graph structure over the
  matched set anchors the rest. Or, per the framing above, the sender uses
  `.symtab` from both unstripped builds and ships the correspondence.
- Prior art on this exact target: Zucchini's ELF x64 path intends to extract
  abs32 from `R_X86_64_RELATIVE`, but its current RELA handling reads the zero
  relocation slot rather than `r_addend`, so real Chrome x86-64 builds lose the
  entire pool (`chrome-elf-zucchini.md`). Its heuristic rel32 path remains the
  best public baseline. No tool I found uses `.eh_frame` boundaries to seed
  disassembly for delta purposes (RustBound/XDA-style papers use them or ML for
  RE, not for patching).

| survives stripping | predictor derives | residual |
|---|---|---|
| `.eh_frame_hdr`/`.eh_frame` (boundaries, sizes), `.rela.dyn`/`.relr.dyn` (all data pointers, PIE/so only), `.dynsym` (exported names), `.gnu.hash`, `.init_array`, PLT/GOT | function boundaries → reliable linear sweep; all abs pointers rewritten and reloc section regenerated; `.eh_frame_hdr`, `.gnu.hash`, GOT/PLT regenerated; rel32/rip-rel rewritten | changed bodies; changed CFI for changed functions; `.rodata` string moves (no map for rip-rel targets *into* rodata beyond the operands themselves — those are handled) |

### 2.3 Rust binaries (ELF, same substrate as 2.2 plus quirks)

Local experiment (verified, rustc stable, `--release`, then `strip`; and
`panic=abort` + `strip=true`): 351,816 B binary; **539 FDEs in `.eh_frame`
vs 568 text symbols in the unstripped build (95% coverage)**; 607
`R_X86_64_RELATIVE` in `.rela.dyn`; 55 `*.rs` path strings survive
(`src/main.rs`, `/rustc/<sha>/library/alloc/src/collections/btree/node.rs`,
...). With `panic=abort`, `.eh_frame` is *still emitted* (533 FDEs) — Rust
keeps unwind tables for backtraces regardless.

- **Panic `Location` records** (`&'static str` ptr+len, `line: u32`,
  `col: u32`), one per potential panic site, referenced from code and living
  in `.rodata`/`.data.rel.ro` ([cxiao.net](https://cxiao.net/posts/2023-12-08-rust-reversing-panic-metadata/);
  [rust-lang/rust#75263](https://github.com/rust-lang/rust/issues/75263)).
  They survive `strip` and `panic=abort`; removal needs nightly
  `-Zlocation-detail=none`. For a predictor they are a *stable per-function
  fingerprint* (file:line sets) that names functions approximately — not a
  symbol table, but enough to disambiguate hash collisions between versions
  and to anchor block matching when a body changed. Line numbers shift with
  edits above them, so the file path plus the multiset of `col` values is the
  stable part.
- Rust binaries are PIE by default on Linux, so `.rela.dyn` is a complete
  pointer list (vtables for `dyn Trait`, `&'static` tables, panic Location
  string pointers).
- Rust-specific churn: monomorphised generics produce many near-identical
  functions; symbol names include a hash suffix that changes with the crate
  graph, so even *name*-based matching from the sender's `.symtab` needs
  demangling and hash-stripping (v0 mangling encodes the instantiation
  structurally — better than legacy for matching; unverified which the
  default toolchain emits today).
- LTO/ICF: `cargo --release` defaults to thin-local LTO and no ICF; large
  projects often enable fat LTO, which inlines aggressively and makes body
  hashes more volatile (my inference).

| survives stripping | predictor derives | residual |
|---|---|---|
| everything in 2.2, plus panic `Location` records and file-path strings; `.eh_frame` present even with `panic=abort` | as 2.2; Location records give an approximate name per function and a stable anchor for block matching | as 2.2; monomorphisation churn when generic instantiations change |

### 2.4 PE/COFF (Windows x64; Electron, .NET AOT, games)

Sources: [MS x64 exception handling](https://learn.microsoft.com/en-us/cpp/build/exception-handling-x64),
[Leviathan: use of exception-handling metadata](https://www.leviathansecurity.com/blog/use-of-windows-exception-handling-metadata),
[MSDelta API doc](https://learn.microsoft.com/en-us/previous-versions/bb417345(v=msdn.10)) (verified),
Zucchini `disassembler_win32.h`.

- **`.pdata` (exception directory)**: sorted `RUNTIME_FUNCTION {BeginAddress,
  EndAddress, UnwindInfo}` for every function that has unwind info — the ABI
  requires it for every function that "allocates stack space or calls another
  function"; leaf functions are exempt (Leviathan). So on x64 the shipped
  binary carries a sorted **function-boundary table** with sizes, unstripped
  by definition, plus `.xdata` unwind codes (prologue structure) usable as a
  secondary fingerprint. ARM64 PE has the same (`.pdata` with packed or
  `.xdata` unwind).
- **`.reloc`**: base-relocation table — every absolute pointer location in the
  image (`IMAGE_REL_BASED_DIR64`, type 10), present in every ASLR-enabled image
  (i.e. all of them since Vista). On x64 code is rip-relative, so `.reloc`
  enumerates *data* pointers (vtables, function tables, pointer literals in
  `.rdata`), just like `.rela.dyn`. Regenerable from the predicted layout.
- **Import/export directories**: names for imports (by name or ordinal) and
  exports; IAT is an offset table; Load Config (CFG function table, sorted
  RVAs of every address-taken function — another boundary/identity table),
  TLS directory, debug directory (PDB GUID/age, which changes every build and
  must be treated as data), Rich header, checksum, timestamp (deterministic
  with `/Brepro`).
- **MSDelta** (Windows Update's engine, `msdelta.dll`, PA30 container) is the
  incumbent and it *is* a normalising, structure-aware differ: transforms for
  imports, exports, resources, relocs, `I386_JMPS`/`I386_CALLS`, `AMD64_DISASM`,
  `AMD64_PDATA`, `ARM_DISASM`, `ARM_PDATA`, `CLI_DISASM`/`CLI_METADATA` (.NET
  IL and metadata), `E8` (call-target rewriting of the *target* file, tried
  both ways and the smaller kept), plus a normaliser that rebases/unbinds and
  zeroes timestamps so one delta applies to "slightly different variations of
  the source file". MS's own claim (PatchAPI, i386): PE-aware treatment makes
  deltas "typically 50–70% smaller" than raw. Windows Update also computes
  every forward delta against the RTM binary, not the previous patch
  ([DebugOff MSU internals](https://debugoff.com/patch-diffing-windows-msu-internals-and-helper-scripts/)).
  No public measurements versus Zucchini/bsdiff exist that I found.
- Layout stability: MSVC `link /INCREMENTAL` pads functions and inserts jump
  thunks so layout is *unstable* between incremental builds; release builds
  (`/INCREMENTAL:NO`, `/OPT:REF,ICF`) are deterministic given `/Brepro` but
  `/LTCG` + PGO (`/USEPROFILE`) reorder hot/cold and split functions into
  `.text$hot`/`.text$cold` parts based on the profile (unverified: exact
  section names; the mechanism is well known).

| survives stripping | predictor derives | residual |
|---|---|---|
| `.pdata`/`.xdata` (boundaries+prologue shape, x64/ARM64), `.reloc` (all abs pointers), import/export/CFG tables, Load Config, TLS | boundaries → reliable sweep; abs pointers rewritten + `.reloc` regenerated; `.pdata`, IAT, export/CFG tables regenerated from map; rel32/rip-rel rewritten | changed bodies; resources; PDB GUID/age (must be shipped, 24 B); `.rdata` strings |

### 2.5 Mach-O (macOS/iOS arm64, x86_64)

Sources: [llios LC_FUNCTION_STARTS](https://github.com/qyang-nj/llios/blob/main/macho_parser/docs/LC_FUNCTION_STARTS.md),
[llios chained fixups](https://github.com/qyang-nj/llios/blob/main/dynamic_linking/chained_fixups.md),
[Ghidra #3586](https://github.com/NationalSecurityAgency/ghidra/issues/3586),
[ld64 man page](https://keith.github.io/xcode-man-pages/ld.1.html),
[Sparkle delta docs](https://sparkle-project.org/documentation/delta-updates/),
[The Apple Wiki, OTA updates](https://theapplewiki.com/wiki/OTA_Updates).

- **`LC_FUNCTION_STARTS`**: a ULEB128 delta-coded list of every function start
  offset from `__TEXT`. Emitted by ld64/lld by default; removed only with
  `-no_function_starts`. llios says this "is usually done in the release
  builds to save app size"; the Ghidra issue shows an iOS 15 release binary
  that still has it, and Apple's own system binaries carry it (unverified for
  App Store apps in general — **measure on a sample before relying on it**).
  Starts only, no sizes — but sizes are the gaps, and the next start bounds
  each function.
- **`LC_DYLD_CHAINED_FIXUPS`** (macOS 12 / iOS 15+, replaces
  `LC_DYLD_INFO` rebase/bind opcodes): per-segment, per-page "starts" arrays,
  then each pointer slot holds a 64-bit packed record `{next (×4 stride),
  target or bind ordinal, high8, isBind}` chained in place. A parser recovers
  every rebase location + target and every bind location + import ordinal —
  a complete absolute-pointer map like `.reloc`/`.rela.dyn`, but stored
  *inside* the pointers, so rewriting a pointer and rewriting its fixup record
  are the same operation, and the chain `next` fields must be regenerated
  after layout moves. One measured saving quoted by llios: 1.4 MB vs the old
  format for an unspecified binary. Older binaries: `LC_DYLD_INFO_ONLY` opcode
  streams (rebase/bind/lazy/export trie) — also fully parseable and
  regenerable. `LC_DYLD_EXPORTS_TRIE`: names for exports.
- **Split-seg info (`LC_SEGMENT_SPLIT_INFO`)**: lists of locations of
  cross-segment references (ADRP/ADD pairs, pointers) so the shared-cache
  builder can slide segments independently — present in system dylibs, *not*
  in app binaries (unverified: whether Xcode emits it for apps; ld64's man
  page describes it for dyld shared cache use). Where present it is exactly
  the ADRP-pair map Zucchini lacks.
- **`__unwind_info` / `__eh_frame`**: compact unwind, one entry per function
  with compact encoding (frame layout) — a second boundary table.
- **Objective-C metadata** (`__objc_classlist`, method lists with selector
  pointers, `__objc_selrefs`): pointer-dense, fully covered by chained fixups;
  method names are strings — a *name map for every ObjC method* in shipped
  apps. Swift: `__swift5_types`/`__swift5_proto` with relative (int32)
  pointers to type descriptors and mangled names — another named,
  relative-offset table a predictor must regenerate.
- **Layout**: ld64 without `-order_file` lays functions out "in object file
  order" and moves initialisers to the start of `__text`; `-order_file`
  (used by Apple's own startup-time work) reorders; `__DATA` is reordered by
  default so dyld-touched globals cluster ("dirty data ordering",
  `-no_data_order` to disable; `__DATA_DIRTY` segment). lld-macho supports
  `--icf=safe` and call-graph ordering. Deterministic given the same inputs
  (unverified for ld64's parallel paths).
- **Prior art**: Sparkle (the dominant third-party macOS updater) uses per-file
  **plain bsdiff** with a rename/clone heuristic and lzma, no Mach-O-aware
  transform; a hydraulic.dev measurement of an Electron hello-world
  single-line JS change gave ~32 KB on macOS vs ~115 KB on Windows deltas.
  Apple's own OTA uses a bsdiff-derived `BXDIFF41` (The Apple Wiki;
  unverified beyond that page, and Apple publishes nothing). No arm64-aware
  public delta tool exists for Mach-O that I found; Zucchini does not parse
  Mach-O at all.

| survives stripping | predictor derives | residual |
|---|---|---|
| `LC_FUNCTION_STARTS` (starts), `__unwind_info`, chained fixups (all abs pointers + binds), export trie, ObjC/Swift metadata with names, split-seg (system dylibs) | boundaries; all abs pointers rewritten and fixup chains regenerated; B/BL/ADR/ADRP+ADD/LDR pairs rewritten (needs pairing logic, §3); `__unwind_info`, function-starts, ObjC/Swift relative tables regenerated | changed bodies; string moves in `__cstring`; code-signature blob (`LC_CODE_SIGNATURE`, must be shipped or re-signed) |

### 2.6 Android DEX/OAT/ART and Java `.class`/`.jar`

- **DEX** is fully typed: index tables (`string_ids`, `type_ids`,
  `proto_ids`, `field_ids`, `method_ids` — all *sorted by content*, so an
  insertion renumbers everything after it), `class_defs`, then code items
  whose bytecode carries 16/32-bit indices and 8/16/32-bit relative branch
  offsets, plus a `map_list` of section offsets. Names are mandatory (methods
  need `method_id → name string`); ProGuard/R8 obfuscates them to `a`, `b`,
  `aa` but they remain names, and the mapping file exists server-side. This is
  the best-instrumented "compiled" format of all: a predictor can regenerate
  every id table and every index in bytecode from a class/method
  correspondence, which is what Zucchini's DEX disassembler approximates with
  its ~30 reference types and label pools. Zucchini is shipped in Android's
  `update_engine` (payload minor version 8) alongside `PUFFDIFF` and
  `SOURCE_BSDIFF` ([AOSP reduce OTA size](https://source.android.com/docs/core/ota/reduce_size)).
- **OAT/ART** (on-device AOT output of dex2oat): ELF containing the DEX plus
  compiled code; produced *on the device* (or in the system image), never
  shipped by app updates, so out of scope for app fleets; in-scope only for
  system-image OTAs where AOSP documents making dex2oat deterministic and
  removing padding to keep OTA deltas small.
- **Google Play file-by-file** ([Android Developers blog, 2016](https://android-developers.googleblog.com/2016/12/saving-data-reducing-the-size-of-app-updates-by-65-percent.html),
  [archive-patcher](https://github.com/google/archive-patcher)): inflate
  changed deflate members, bsdiff in "delta-friendly space", re-deflate on
  device by searching the (level, strategy, wrap) space against a ~9000-byte
  fingerprint corpus. Updates 65% smaller than the full APK on average (bsdiff
  on the raw APK: 47%); costs ~1 s/MB on 2015+ devices. This is a
  *container* predictor (deflate parameters), orthogonal to code prediction,
  and the same trick (puffin on Android partitions) is what lets Zucchini see
  uncompressed `.so`/`.dex` at all.
- **Java `.class`/`.jar`**: each class file has a constant pool whose indices
  are referenced by bytecode (u2 operands) and whose *order is compiler-
  determined and unstable under edits*; branch offsets are relative and
  in-method. The format carries full names (classes, methods, descriptors)
  unless obfuscated, so class-level correspondence is free. Per-class deltas
  are small (classes are KBs), so the container (jar = zip, file-by-file
  applies) dominates; Sun's JNLP `jardiff` did per-entry replacement only
  (unverified: from memory). Gain over bsdiff after inflation is limited to
  constant-pool renumbering inside changed classes — real but small.

| survives (DEX) | predictor derives | residual |
|---|---|---|
| everything: names, sorted id tables, typed bytecode, map_list | full renumbering of all id tables and every bytecode index; branch offsets inside moved code items; map_list | changed method bodies; new strings; APK signing block (v2/v3 must be shipped) |

### 2.7 .NET assemblies (IL) and .NET Native AOT

- Managed PE: CLI header → metadata streams (`#~` tables, `#Strings`, `#Blob`,
  `#GUID`, `#US`) with ~45 sorted tables (TypeDef, MethodDef, MemberRef, ...)
  addressed by *tokens* (table:row). IL method bodies reference tokens
  (`call 0x0A00001F`) and use *relative* in-method branch offsets, so method
  bodies are position-independent; only tokens and RVAs (MethodDef.RVA →
  body) shift. Full names are present (reflection needs them). MSDelta has
  `CLI_DISASM` and `CLI_METADATA` transforms for precisely this, so Microsoft
  judged the renumbering worth a transform. ReadyToRun/crossgen adds native
  code with its own fixup tables; Native AOT produces an ordinary PE/ELF/Mach-O
  covered by §2.2/2.4/2.5 (with large sorted frozen-object and method tables).
- No public numbers found. Expected gain over bsdiff for IL-only assemblies
  is modest (token renumbering is a small fraction of bytes; IL is compact
  and the residual is the changed bodies).

### 2.8 WebAssembly modules

- Format facts (from the spec; unverified against a source in this pass but
  well established): sections with LEB128 byte sizes; function bodies are
  self-delimiting (size-prefixed) with LEB128 immediates. `call` carries a
  function *index* (import space first, then defined functions — inserting a
  function or import renumbers everything after), `br`/`br_if` carry relative
  *label depths* (self-contained), `call_indirect` carries a type index and a
  table index, `global.get`, `ref.func`, `table.get` carry indices, and
  memory addresses are `i32.const` immediates (data layout pointers,
  indistinguishable from other constants — same ambiguity as x86 immediates).
  Shipped modules have no relocations (the `linking`/`reloc.*` custom
  sections exist only in object files); the `name` custom section is usually
  stripped by `wasm-opt --strip-debug`, sometimes kept.
- What a predictor gets: the entire module decodes *unambiguously* with no
  heuristic disassembly (structured stack machine, no data-in-code), so every
  index operand is known exactly, and function bodies are enumerated with
  sizes. Given a function correspondence, the predictor renumbers every index
  in every body, re-encodes LEB128 sizes, regenerates the function/type/
  export/element tables, and predicts data-segment offsets. The residual is
  changed bodies plus `i32.const` address immediates that moved (undecidable
  without the source-level relocations; the toolchain's `--emit-relocs`
  equivalent would fix this but is not shipped).
- Prior art: none found. `wa-diff` (radekdoulik) is an inspection tool, not
  a delta codec; the "WebAssembly-based Delta Sync" TOS'22 paper is rsync
  *implemented in* wasm, not deltas *of* wasm. Fleet-update relevance:
  edge-function platforms and plugin systems ship wasm to many nodes, but
  modules are typically 0.1–20 MB and bsdiff already handles them adequately
  (unverified — no measurements exist; the LEB128 renumbering cascade after an
  inserted function is the case to test).

### 2.9 JavaScript bundles, Python `.pyc`, LLVM bitcode

- **Minified JS bundles**: text; bsdiff/zstd `--patch-from` work well because
  there are no addresses — the analogue of second-order change is minifier
  renaming (`a`,`b`,`c` reassigned when a new identifier is inserted) and
  webpack/rollup module ids. Source maps name every span (server side) and are
  the correspondence; the predictor would be a "re-minify with the old name
  assignment" transform. Small win; the world already ships full bundles
  behind CDNs with content hashes. Not a code-format problem.
- **`.pyc`**: marshal-serialised nested code objects; bytecode jumps are
  *relative* instruction offsets (since 3.10) and names/constants are
  per-code-object tuples referenced by index; `co_linetable` shifts with line
  numbers. Everything is named (`co_name`, `co_qualname`). Effectively
  self-contained per function; a byte differ is close to optimal already.
  No prior art found.
- **LLVM bitcode**: bitstream with per-block abbreviations, forward-reference
  and *relative* value ids (operands encoded relative to the current
  instruction's id since LLVM 3.3), symbol names in a string table; sizes are
  32-bit word counts per block. Fully decodable, but nobody ships bitcode to a
  fleet (Apple's bitcode submission was removed in Xcode 14). Skip.

### 2.10 Firmware images (ARM Cortex-M, raw binaries)

Sources: [OTA survey, arXiv 2009.02260](https://arxiv.org/pdf/2009.02260) (verified via PDF text),
[Memfault delta updates](https://interrupt.memfault.com/blog/ota-delta-updates),
[bpatch/LoRaWAN, arXiv 2505.13764](https://arxiv.org/html/2505.13764v1),
[Chalmers Zephyr delta thesis](https://github.com/saralinnealindh/delta-updates-for-embedded-systems),
[Dfinder](https://www.sciencedirect.com/science/article/abs/pii/S2542660521001220) (abstract only),
Zucchini `arm_utils.h`.

- What the device holds: a raw flat image (`objcopy -O binary`) — no headers,
  no relocations, no symbols. Thumb-2 code with `BL`/`B.W` (24-bit split
  immediates, `T24`/`T20` in Zucchini's terms), short `B` (T8/T11), `ADR`,
  `LDR (literal)` and — crucially — **literal pools**: absolute 32-bit
  addresses of globals and functions stored inline after each function,
  loaded via `LDR Rt,[PC,#imm]`. There is no relocation table for them, but
  they are reachable from the LDR-literal instructions, so a sweep bounded by
  the vector table + function starts can enumerate them with high (not
  perfect) precision. `MOVW/MOVT` pairs carry 16+16-bit absolute halves.
- What the *sender* holds: the ELF with `.symtab` and, if built with
  `-Wl,--emit-relocs` (or `-q`), a complete relocation list for the final
  image — the exact Exediff/Courgette input. This is the strongest instance of
  the "sender has everything" framing: the client needs zero metadata because
  the patch can carry the old-image pointer map compactly (it is a sorted
  offset set, ~1–2 bits/entry after compression).
- Literature (survey Table 2, verified): R2/R3 (TinyOS/TelosB) attack the
  problem at the *linker* by emitting relocatable code plus an on-device
  relocation step so functions do not shift ("R3sim"); their differencers
  R3diff (O(n³) DP, byte-level) and DASA (suffix array, O(n log n)) are
  generic byte differs optimised for delta-script size on MSP430-class images;
  Zephyr/Hermes use indirection tables (call through a table so callers
  need not change); MoRE, Elon, in-place patching variants. Dfinder (2021,
  Contiki-NG) is an enhanced-suffix-array byte differ with halved device
  storage. Kachman & Baláž, "Optimized differencing algorithm for firmware
  updates of low-power devices" (IEEE 2016) and the terms "Delta-Flash" and
  "Fourier" from the brief: **not located** — the IEEE abstract was not
  retrievable and no paper under those two names surfaced; treat as
  unverified. Practical stacks: Zephyr's proposed delta backend is bsdiff
  without bzip2 + heatshrink; Chalmers/Scionova use `detools` (bsdiff +
  heatshrink) with a three-partition MCUboot flow; Memfault's tutorial uses
  jojodiff/janpatch (constant-space, in-place) and shows 7,908 B → 1,252 B
  (6.3x) for a toy app; bpatch (2025, STM32WL, LoRaWAN) reports 19.3x on
  minor and 5.8x on major updates and explicitly claims to be
  "independent of the firmware binary structure". **No ARM-aware (BL/literal-
  pool-rewriting) firmware differ with published numbers was found.** The
  device-side constraint that shaped this literature — patch in place in tens
  of KB of RAM — is exactly what a predictor satisfies: the replay is
  streaming (copy old function, rewrite its operands, emit), needs no suffix
  array, and no bzip2 window.

| survives in raw image | predictor derives (with sender-side ELF+relocs) | residual |
|---|---|---|
| nothing structural; vector table at 0; Thumb-2 code with literal pools | old pointer map shipped or recovered by LDR-literal sweep; all BL/B/ADR/LDR-literal/pool/MOVW-MOVT rewritten; new vector table | changed bodies; `.data` init image and `.rodata` moves |

---

## 3. Instruction-set-specific reference forms

What each ISA encodes, whether the reference is *fully determined* by the
(location, target) pair (so a piecewise map rewrites it exactly), and what is
ambiguous. "Capturable" below is my estimate of the fraction of second-order
byte churn a predictor can reconstruct once it knows the map; **no published
per-ISA fraction exists** — Zucchini and Courgette publish only whole-file
numbers, and Exediff was Alpha-only.

| ISA | Reference forms | Determinism | Notes |
|---|---|---|---|
| x86-64 | `E8`/`E9` rel32, `0F 8x` jcc rel32, `EB`/`7x` rel8, `ModRM.rm=101` rip-rel disp32 (data), imm32/imm64 absolute (non-PIE data pointers in code, `mov rax, imm64`) | rel32/rip-rel: exact from map. imm absolute: ambiguous with constants (Zucchini ignores; go-binsync ignores in code and relies on relocs/pclntab for data) | Variable-length; a linear sweep from *known function starts* (pclntab, `.eh_frame`, `.pdata`, function-starts) is far more reliable than Zucchini's opcode scan, which has no boundaries and "disregard[s] instruction alignment". Padding/jump tables in `.text` (MSVC, `-fjump-tables` in rodata for GCC/Clang) are the main confusers. Capturable: ~all of code churn; data churn only where relocations exist. |
| AArch64 | `B`/`BL` imm26 (±128 MB), `B.cond`/`CBZ`/`LDR lit` imm19, `TBZ` imm14, `ADR` imm21, **`ADRP` imm21 (4 KB pages) + `ADD`/`LDR`/`STR` imm12** pairs, `MOVZ/MOVK` absolute sequences (rare in PIC) | Fixed 4-byte instructions make sweeping trivial and *alignment-exact*. Branches: exact. ADRP pairs: exact *only if the pair is recognised* — the page delta depends on both the ADRP's own page and the target's page, so a target moving within a page changes the `ADD` imm12 but not the `ADRP`; crossing a page boundary flips the ADRP and may leave the ADD unchanged. Pairing requires following the destination register to the consumer (usually the next instruction, but compilers do schedule them apart and share one ADRP across several consumers). | Zucchini handles only imm14/19/26 branches (verified) — ADRP pairs are its largest uncorrected class on arm64, which is presumably why Chrome Android arm64 deltas gained less than Windows. Capturable with pairing: ~all; without: branches only (plausibly half the reference count in typical code — unverified). |
| ARM32 / Thumb-2 (Cortex-M/A) | ARM `B`/`BL` imm24; Thumb `B` T1 imm8/T2 imm11, `B.W`/`BL` T3/T4 with split immediates (S, J1, J2, imm10, imm11 — J1/J2 are *XOR-ed* with S), `BLX` (mode switch, halfword alignment), `ADR`, `LDR (literal)` imm8/imm12, `MOVW/MOVT`, literal pools (absolute 32-bit words in `.text`) | Branches exact (encoding is fiddly but deterministic). Literal pools are absolute addresses with no relocation list in a raw image; enumerable via LDR-literal operands. Mixed ARM/Thumb needs mode tracking (Zucchini assumes one mode per section). | Capturable with pool enumeration: near all; the residual is `MOVW/MOVT` halves that were not paired. |
| RISC-V (RV64GC) | `JAL` imm20, branches imm12, **`AUIPC` hi20 + `JALR`/`ADDI`/`LD` lo12** pairs (lo12 is *sign-extended*, so hi20 = (delta + 0x800) >> 12), `LUI/ADDI` absolute pairs, compressed `C.J`/`C.BEQZ` | Pairs are exact when paired (the lo12 consumer is usually adjacent; `%pcrel_lo` references the AUIPC by *label*, so in shipped code the consumer may be anywhere after it). **Linker relaxation** (`AUIPC+JALR` → `JAL`, `LUI+ADDI` → `ADDI` off `gp`) changes *instruction counts* between builds when a target comes into or leaves range, so function sizes change without source change — layout prediction must model relaxation or accept residual. | No delta tool with RISC-V awareness found. |
| Dalvik (DEX) | 16/32-bit index operands; 8/16/32-bit relative branches; `packed-switch`/`sparse-switch` payload offsets | Fully typed; exact | Zucchini's ~30 reference types. |
| Wasm | LEB128 indices (function/type/table/global/elem/data), relative label depths, `i32.const` addresses | Indices exact; addresses ambiguous | §2.8 |
| CIL (.NET IL) | metadata tokens (u4), relative branch offsets, RVAs in tables | tokens exact given table renumbering | MSDelta `CLI_DISASM`. |

Published ARM numbers: Courgette's ARM path and Zucchini's AArch64 path have no
separate published patch-size figures that I could find; Mozilla's per-platform
projection (Linux −10%, macOS −9.7% vs Windows −33%) is the only cross-platform
hint and it conflates Mach-O (unsupported → raw fallback) with arm64.
Percival's thesis compares aligners, not ISAs (Alpha only). Exediff's 2–5x is
on Alpha with a symbol table, i.e. the closest analogue to the go-binsync
setting, and it landed an order of magnitude below go-binsync's gain mainly
because it encoded rather than regenerated the pointer-dense sections.

---

## 4. Linker layout determinism

Prediction quality depends on "same function → predictable new address". What
each linker does by default and what perturbs it:

| Linker | Default text order | Perturbations | Determinism |
|---|---|---|---|
| Go `cmd/link` | package dependency order, then object order; stable sort by symbol type; trampolines inserted in address order (verified from source) | `-randlayout` (off), function alignment changes with toolchain, FIPS grouping | Deterministic; Go toolchains are reproducible (go.dev/blog/rebuild); input-order sensitivity fixed in cmd/compile (#38186) |
| lld (ELF) | input order of sections; `--symbol-ordering-file`; `--call-graph-profile-sort` (on by default when objects carry `.llvm.call-graph-profile`, i.e. PGO/`-fprofile-use` builds — so a profile refresh *reorders the binary*); `--icf=safe/all`; `--shuffle-sections=<seed>` (verified from `ld.lld.1`) | ICF folds functions that happen to become byte-identical (many-to-one mapping the predictor must model); PGO/orderfile churn; `--bp-*` balanced-partitioning options in recent lld (unverified) | Deterministic given identical inputs ([LLVM deterministic builds blog](https://blog.llvm.org/2019/11/deterministic-builds-with-clang-and-lld.html): `/Brepro`, no hash-table-order output) |
| mold | input order; `--shuffle-sections` opt-in; `--icf`; `--section-order` (verified from `mold.md`) | as lld | "bit-for-bit identical output" guaranteed for same inputs/options/version |
| GNU ld / gold | input order; `--sort-section`, gold `--section-ordering-file`; gold `--icf` | Firefox used gold section ordering for startup ([bug 603370](https://bugzilla.mozilla.org/show_bug.cgi?id=603370)) and had to fight ICF interfering | deterministic |
| MSVC link | object order; `/ORDER`; PGO reorders and splits hot/cold; `/INCREMENTAL` pads and thunks | incremental builds are *not* layout-stable; `/OPT:ICF` folds | `/Brepro` for timestamps |
| ld64 / lld-macho | object file order, initialisers first; `-order_file`; `__DATA` reordered for dirty-page locality by default (`-no_data_order`) | Apple's own builds use order files derived from launch profiles | assumed deterministic (unverified) |

Two organisational facts matter more than the linker defaults:

- **Chrome regenerates its Android orderfile on CI from PGO profiles**
  ([docs/orderfile.md](https://chromium.googlesource.com/chromium/src/+/main/docs/orderfile.md)):
  the orderfile builders run after each PGO profile refresh at the same
  commit. Every refresh reorders hot functions, which is layout churn that
  *no* predictor can derive from the old binary — it must be shipped (as a
  permutation, cheap) or the predictor must accept a large residual. Zucchini
  survives this because its equivalences are per-region, not per-layout.
- **AOSP's OTA-size guidance is entirely about build determinism**
  ([reduce_size](https://source.android.com/docs/core/ota/reduce_size)):
  sorted file lists, relative debug paths, no `__DATE__`, deterministic
  dex2oat, `make_ext4fs -d base_fs` for stable block allocation. The lesson
  generalises: a predictive codec should *specify* the layout-affecting build
  inputs it assumes (no ICF, no incremental linking, fixed orderfile) and
  measure the penalty when they are violated, rather than hope.

I found no literature on "layout-stable linking for delta updates" as such; the
closest are R2/R3 (relocatable code so nothing shifts, §2.10) and the
indirection-table schemes (Zephyr/Hermes), both from the sensor-network world,
plus Percival's remark that block alignment tolerates shifts precisely
because linkers move code in blocks. A predictor gets the same benefit
without touching the toolchain, as long as it models *where* the linker puts
things — which for every linker above is a deterministic function of inputs
the sender has.

---

## 5. Function correspondence without symbols

Needed for C/C++/Rust ELF, PE without PDB, Mach-O without dSYM on the
*client* — or, per §2's framing, not needed at all if the sender ships the
correspondence. Cheap deterministic methods, in order of strength:

1. **Names where they exist**: pclntab, `.dynsym`/export tables, ObjC/Swift
   metadata, DEX/CIL/class names, Rust panic `Location` file paths (approximate).
2. **Masked content hash of the body** (deterministic, O(n)): hash instruction
   bytes with *operand bytes that can hold addresses masked*. Ghidra FID does
   this per instruction using SLEIGH's operand masks and produces a "full hash"
   (opcode + registers, constants masked) and a "specific hash" (constants
   kept, addresses masked, address-vs-constant decided by instruction
   context — jumps/calls are addresses) ([Ghidra FID doc](https://github.com/NationalSecurityAgency/ghidra/blob/master/Ghidra/Features/FunctionID/src/main/doc/fid.xml),
   [nietaanraken write-up](https://blog.nietaanraken.nl/posts/ghidra-function-id/)).
   BinDiff's first two function-matching passes are exactly "hash of raw
   bytes" and "name hash" ([BinDiff concepts](https://github.com/google/bindiff/blob/main/docs/concepts.md));
   its "prime signature" (product of per-mnemonic primes, order-independent)
   is a cheap fallback for reordered blocks. For a predictor the full hash is
   the right primary key (it is invariant under relinking); collisions
   (templates, thunks, identical small functions) are broken by call-graph
   context (3) and position (4).
3. **Call-graph propagation** (BinDiff's MD-index/edge passes, Diaphora's
   "callee/caller" heuristics): if `f` matched and its k-th call site in both
   versions targets an unmatched pair, match them. Deterministic, cheap, and
   what Zucchini's *target affinity* does implicitly at byte level.
4. **Order**: functions keep their relative order across versions unless the
   linker reorders (§4). A longest-increasing-subsequence over the hash
   matches gives a monotone skeleton; unmatched functions between two matched
   neighbours are matched by position and size (this is what Exediff's
   pre-matching does with the symbol table and what go-binsync gets from name
   order).
5. **Symbol-free byte aligner** for what is left: Percival's FFT block
   alignment (§1.5) or Zucchini's suffix-array over an encoded view. These are
   the fallback, not the primary mechanism, and Percival's own data shows the
   aligner is not where the gain is.

ML matchers (asm2vec, jTrans, KEENHash, VEXIR2Vec) are irrelevant here: they
target cross-compiler/cross-optimisation similarity, are non-deterministic
across library versions, and cost more than a link. Diaphora's 51 heuristics
are SQL over IDA databases — the right *list* of signals, the wrong runtime.

---

## 6. Assessment for a predictive codec

### 6.1 Shared machinery vs format-specific parsing

Across every format above the predictor decomposes into the same four pieces:

1. **Function matcher** → a list of (old function, new function) pairs plus
   sizes. Inputs differ (names / masked hashes / sender-side symbols); the
   output type is identical. Generic.
2. **Layout predictor** → new address for every old object, i.e. a
   *piecewise-linear offset map* (`old_off → new_off` with one delta per
   matched run) — the same object Zucchini calls the pruned `EquivalenceMap`
   and go-binsync calls the maps. Generic; per-linker rules for alignment,
   padding, trampolines, relaxation (RISC-V) plug in.
3. **Operand relocator per ISA** → given code bytes, function boundaries, and
   the map, rewrite every reference. One module per ISA (x86-64, AArch64,
   Thumb-2, RISC-V, Dalvik, Wasm, CIL), each a decoder + a table of reference
   forms + pairing rules for split immediates. This is the piece with the
   most reuse: Go/C/Rust/PE/Mach-O on x86-64 all share one relocator; Mach-O
   arm64, Linux arm64 and Android arm64 share another. Zucchini's
   `Rel32Finder{X86,X64,AArch32,AArch64}` is already structured this way, and
   go-binsync's x86 pass (via `x/arch/x86asm`) is the x86-64 instance.
4. **Offset-table regenerator** → rebuild every sorted/offset-based table
   from the map: pclntab/findfunctab/`.go.type`; `.eh_frame_hdr`, `.rela.dyn`
   / `.relr.dyn`, `.gnu.hash`, GOT/PLT; `.pdata`, `.reloc`, IAT/export/CFG;
   `LC_FUNCTION_STARTS`, `__unwind_info`, chained-fixup chains, export trie,
   ObjC/Swift relative tables; DEX id tables and map_list; Wasm section
   sizes and index tables. Each table is format-specific to *parse*, but they
   are all "sorted list of (offset, payload) serialised deterministically", so
   the serialisers are small and the shared part (predict, then send a
   correction against the prediction) is the codec core that already exists.

Format-specific and *not* shareable: container parsing (ELF/PE/Mach-O/DEX/Wasm
readers — all have mature Go libraries: `debug/elf`, `debug/pe`,
`debug/macho`, and third-party DEX/Wasm parsers), the list of tables per
format, the code-signature/PDB-GUID/build-id fields that must be shipped
verbatim, and container transforms (deflate re-compression for
APK/JAR/MSIX/zip: puffin/archive-patcher territory, orthogonal to code).

### 6.2 Where byte-level matching is already near the floor

Be honest about where a predictor buys little:

- **Large real changes** (major releases, refactors): Zucchini vs bsdiff was
  only −22% to −32% on Firefox majors, and Exediff *lost* to bindiff on the
  apache 1.2→1.3 reorganisation. The residual is first-order change; the
  predictor only removes second-order churn. Fleet deployments of *one*
  service binary many times a day are the sweet spot precisely because each
  step is small.
- **Formats with no address arithmetic**: JS, `.pyc`, IL-heavy .NET, Java
  classes. Relative branches and per-object indices keep changes local;
  bsdiff/zstd `--patch-from` are within a small factor of optimal. Gains
  are in renumbering constant pools/tokens — real but single-digit percent.
- **Wasm**: likely a factor of a few on inserted-function cascades (LEB128
  renumbering), not 30x; modules are small; unmeasured.
- **Data-dominated artefacts**: ICU tables, V8 snapshots, embedded assets,
  `.rodata` string moves. Zucchini already treats them as raw bytes and so
  would we. go-binsync's Go numbers benefit from Go's unusually pointer-dense,
  fully described metadata (pclntab is 35% of a Go binary, `DESIGN.md`); a
  C binary of the same size has a few percent of such tables, so the
  *ceiling* over bsdiff is lower even with perfect prediction — the gain is
  bounded by (bytes of second-order churn) / (bytes of first-order change).
- **Where the linker reorders on profile refresh** (Chrome orderfile, PGO
  builds): the permutation must be shipped; prediction still wins on
  reference rewriting, but the "layout is a function of the old binary"
  premise fails and the codec needs Zucchini-style region matching as its
  fallback path anyway.

### 6.3 Ranking

Score = expected gain over bsdiff on small changes × how common "same binary
on a fleet, updated often" is × 1/effort. Ordinal, with reasoning:

1. **C/C++/Rust ELF PIE on Linux servers** — highest. Same fleet-update
   use-case as Go (containers, agents, sidecars, databases like the ones in
   `docs/research/benchmark-scale.md` are often C++/Rust). `.eh_frame` gives
   boundaries in every stripped binary (95% of functions in the Rust sample;
   all non-leaf in C), `.rela.dyn`/RELR is a complete data-pointer map, and
   the x86-64 relocator is already written. The missing piece is the matcher
   (masked-hash + sender-side `.symtab`), which is generic. Expected gain: the
   same class as Go for code churn (rel32/rip-rel/pointer tables), lower
   ceiling because there is less metadata to regenerate. Effort: moderate.
2. **PE x64** — Windows server/desktop fleets (Electron apps, agents). Best
   metadata of any native format (`.pdata` boundaries with sizes, `.reloc`,
   CFG table, imports/exports). The competitor is MSDelta, not bsdiff — an
   honest evaluation must beat `msdelta.dll` (freely callable on Windows for
   measurement), which already does disassembly-aware and pdata-aware
   transforms. Effort: moderate (same x86-64 relocator; new table set).
3. **Mach-O arm64** — macOS fleets are rarer, but the third-party updater
   ecosystem (Sparkle) is *plain bsdiff*, `LC_FUNCTION_STARTS` + chained
   fixups + ObjC/Swift metadata are unusually rich, and it forces the AArch64
   relocator with ADRP pairing that Zucchini lacks — which then serves Linux
   arm64 servers (Graviton/Ampere fleets running Go and C++), arguably a
   bigger population than macOS. Effort: high (new ISA, chained-fixup
   regeneration, code-signature handling).
4. **Cortex-M firmware** — enormous device counts, tiny images, constrained
   links, a literature that is entirely byte-level, and no ARM-aware public
   tool. The sender-side ELF+`--emit-relocs` makes prediction *easier* than
   on servers (perfect pointer map). The catch: bsdiff-class tools already
   reach 6–20x on these images, images are 100 KB–1 MB, and each vendor has
   its own bootloader/flash layout, so the framework's value is a streaming
   in-place replay, not just patch bytes. Effort: moderate (Thumb-2 relocator
   + literal-pool sweep); commercial fit uncertain.
5. **DEX/APK** — Zucchini already does the typed-index prediction and Play
   owns the distribution channel; only enterprise MDM/sideload fleets would
   benefit. Low marginal gain, moderate effort. Skip unless a customer asks.
6. **Wasm** — cheap to build (unambiguous decode, small tables) and untested
   anywhere; worth a one-day measurement before committing. Likely modest.
7. **.NET IL, Java class, JS, pyc, bitcode** — near the byte-level floor or
   no fleet demand. Skip.

### 6.4 Two design consequences for the framework

- **Ship the correspondence, don't recover it on the client.** Every
  "stripped" problem in §2 disappears if the patch carries the function map
  (a permutation plus sizes — a few bytes per changed function after
  delta-coding) computed from the sender's unstripped artefacts. The client
  then needs only boundaries-or-sizes to replay, which the map itself
  provides. This makes the matcher an *encoder-side* component and keeps the
  decoder tiny and format-agnostic, which is also what the firmware case
  requires.
- **Keep a Zucchini-style region fallback under the predictor.** Where layout
  is not predictable (orderfile/PGO refresh, ICF, relaxation) the codec must
  degrade to "matched regions + corrected references" rather than to bsdiff.
  Zucchini's encoded-view + label mechanism is the proven design for that
  layer, and its two documented failure modes — heuristic rel32 detection
  without boundaries, and no ADRP pairing — are exactly the two things the
  predictor's boundary tables and per-ISA relocator fix.

---

## Sources

- Courgette design doc — https://www.chromium.org/developers/design-documents/software-updates-courgette/
- Evan Martin on Courgette — https://neugierig.org/software/chromium/notes/2009/05/courgette.html
- Zucchini README — https://chromium.googlesource.com/chromium/src/+/main/components/zucchini/README.md
- Zucchini headers (GitHub mirror): `rel32_finder.h`, `arm_utils.h`, `disassembler_elf.h`, `disassembler_dex.h`, `encoded_view.h`, `equivalence_map.h`, `ensemble_matcher.h` — https://github.com/chromium/chromium/tree/main/components/zucchini
- chromium-dev threads — https://groups.google.com/a/chromium.org/g/chromium-dev/c/M-FQRn6baB0 ; https://groups.google.com/a/chromium.org/g/chromium-dev/c/-8JPrR7GQOg
- Mozilla bug 1632374 (Zucchini numbers) — https://bugzilla.mozilla.org/show_bug.cgi?id=1632374
- Mozilla bug 603370 (libxul symbol ordering) — https://bugzilla.mozilla.org/show_bug.cgi?id=603370
- Hydraulic, "Deltas diffed" — https://hydraulic.dev/blog/20-deltas-diffed.html
- Baker, Manber, Muth, "Compressing Differences of Executable Code" (1999) — https://robert.muth.org/Papers/1999-exediff.pdf
- Percival, "Matching with Mismatches and Assorted Applications" (2006) — https://www.daemonology.net/papers/thesis.pdf
- MSDelta / PatchAPI documentation — https://learn.microsoft.com/en-us/previous-versions/bb417345(v=msdn.10)
- MSU/PSF internals — https://debugoff.com/patch-diffing-windows-msu-internals-and-helper-scripts/
- x64 exception handling — https://learn.microsoft.com/en-us/cpp/build/exception-handling-x64 ; Leviathan — https://www.leviathansecurity.com/blog/use-of-windows-exception-handling-metadata
- Google Play file-by-file — https://android-developers.googleblog.com/2016/12/saving-data-reducing-the-size-of-app-updates-by-65-percent.html ; archive-patcher — https://github.com/google/archive-patcher
- AOSP reduce OTA size / puffin / zucchini — https://source.android.com/docs/core/ota/reduce_size ; https://android.googlesource.com/platform/external/puffin/
- Rust panic metadata — https://cxiao.net/posts/2023-12-08-rust-reversing-panic-metadata/ ; https://github.com/rust-lang/rust/issues/75263
- RELR — https://rfc.archlinux.page/0023-pack-relative-relocs/ ; https://github.com/llvm/llvm-project/commit/4a8de2832a2a730f63b71bdf1c1b446285ec5b6f
- Mach-O function starts / chained fixups — https://github.com/qyang-nj/llios/blob/main/macho_parser/docs/LC_FUNCTION_STARTS.md ; https://github.com/qyang-nj/llios/blob/main/dynamic_linking/chained_fixups.md ; https://github.com/NationalSecurityAgency/ghidra/issues/3586 ; dyld `fixup-chains.h` — https://github.com/apple-oss-distributions/dyld/blob/main/include/mach-o/fixup-chains.h
- ld64 man page — https://keith.github.io/xcode-man-pages/ld.1.html
- Sparkle delta updates — https://sparkle-project.org/documentation/delta-updates/ ; Apple OTA (BXDIFF41) — https://theapplewiki.com/wiki/OTA_Updates
- lld man page — https://github.com/llvm/llvm-project/blob/main/lld/docs/ld.lld.1 ; mold docs — https://github.com/rui314/mold/blob/main/docs/mold.md ; LLVM deterministic builds — https://blog.llvm.org/2019/11/deterministic-builds-with-clang-and-lld.html
- Chrome orderfile — https://chromium.googlesource.com/chromium/src/+/main/docs/orderfile.md
- Go linker `textaddress` — `$GOROOT/src/cmd/link/internal/ld/data.go`; reproducible Go — https://go.dev/blog/rebuild
- Ghidra Function ID — https://github.com/NationalSecurityAgency/ghidra/blob/master/Ghidra/Features/FunctionID/src/main/doc/fid.xml ; https://blog.nietaanraken.nl/posts/ghidra-function-id/
- BinDiff concepts — https://github.com/google/bindiff/blob/main/docs/concepts.md ; Diaphora — https://hex-rays.com/blog/plugin-focus-diaphora
- Firmware: OTA survey — https://arxiv.org/pdf/2009.02260 ; Memfault — https://interrupt.memfault.com/blog/ota-delta-updates ; bpatch/LoRaWAN — https://arxiv.org/html/2505.13764v1 ; Zephyr delta issue — https://github.com/zephyrproject-rtos/zephyr/issues/78529 ; Chalmers thesis — https://github.com/saralinnealindh/delta-updates-for-embedded-systems ; Dfinder — https://www.sciencedirect.com/science/article/abs/pii/S2542660521001220
- RustBound (function boundaries in stripped Rust) — https://homepages.uc.edu/~wang2ba/files/pub/smartsp24_ryan.pdf
