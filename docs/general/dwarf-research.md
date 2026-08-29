# DWARF-carrying Go binaries: what a predictive codec can do about them

Research note for presage (`SPEC.md` §1–§5). Question: default `go build`
(no `-ldflags=-w`) ships ~2–13 MB of zlib-compressed DWARF, and a one-line
source change makes those compressed bytes unrecognisable to any byte-level
differ. What is derivable, what must be transmitted, and what layer does the
work?

Everything marked **[measured]** was run locally in this session; everything
marked **[source]** was read in `/usr/local/go` (go1.27.0); everything else
is inference and is labelled as such. The measurement corpus is a small
`net/http` program (8.67 MB with DWARF, 6.37 MB with `-w`), built A→B (a new
*inlined* function) and A→C (a new `//go:noinline` function). Ratios should
be re-checked on the real corpus (prometheus etc.) before being quoted.

## 1. What Go's linker actually emits

**Sections** [source, `cmd/link/internal/ld/dwarf.go:2223-2232`]: `.debug_abbrev`,
`.debug_line`, `.debug_frame`, `.debug_info`, `.debug_gdb_scripts`, and under
DWARF 5 `.debug_rnglists`, `.debug_loclists`, `.debug_addr` (under DWARF ≤4,
`.debug_ranges` and `.debug_loc` instead).

Go emits **no `.debug_str`, `.debug_line_str`, `.debug_str_offsets`,
`.debug_aranges`, `.debug_names`, or `.eh_frame`** (the last comes only from
cgo/external linking). All strings are inline `DW_FORM_string`, confirmed in
the abbrev table of a real binary [measured]. `.debug_pubnames`/`pubtypes`
were dropped in Go 1.16 (CL 237426).

**DWARF version 5 is the default since Go 1.25**
([release notes](https://go.dev/doc/go1.25); tracking issue
[#26379](https://github.com/golang/go/issues/26379); disable with
`GOEXPERIMENT=nodwarf5`). darwin/ios/aix stay on DWARF 4
[source, `internal/buildcfg/exp.go:79`]. `.debug_line` units are version 5
under DWARF 5 [measured]; the version-2 header Go carried for XCode 9's
dsymutil is now only the DWARF-≤4 branch [source, `ld/dwarf.go:1308`].
`.debug_frame` CIEs are still version 3 [source, `ld/dwarf.go:1465`] —
CIE version is independent of the DWARF version. Location lists arrived in
Go 1.11 (`cmd/compile/internal/ssa/debug.go`).

**Compression** [source, `ld/data.go:3362` `compressSyms`]: on by default
since Go 1.11 (`-compressdwarf`, `ld/main.go:209`), per section, with

```go
z, err := zlib.NewWriterLevel(&buf, zlib.BestSpeed)   // level 1
```

prefixed on ELF by an `elf.Chdr64{Type: COMPRESS_ZLIB, Size, Addralign}` and
flagged `SHF_COMPRESSED` with the section keeping its `.debug_*` name
[`ld/elf.go:1278`] — the `.zdebug_` form was retired for ELF in Go 1.19
([#50796](https://github.com/golang/go/issues/50796)) and survives only on
Mach-O/PE. If compression does not shrink the section it is left alone (true
of `.debug_gdb_scripts` [measured]). **Compression is skipped entirely under
external linking** [`ld/dwarf.go:2425`]; instead the linker passes
`-Wl,--compress-debug-sections=zlib` to the host linker [`ld/lib.go:1861`].

`-ldflags=-w` omits DWARF but **keeps** `.symtab`/`.strtab`; `-ldflags=-s`
omits both, and implies `-w` [source, `ld/main.go:274`; measured].

**Determinism**: two identical builds are byte-identical [measured]. Output
is *not* stable across Go versions — see §5.

## 2. What DWARF costs a byte-level differ [measured]

`bsdiff` on the whole binary, A→B and A→C:

| build | size | bsdiff(A→B) | bsdiff(A→C) |
|---|---|---|---|
| default (DWARF) | 8,673,982 | **2,158,096** | **2,206,141** |
| `-ldflags=-w` | 6,372,748 | 35,564 | 30,236 |
| `-ldflags=-s` | 5,886,215 | 35,340 | — |

DWARF inflates the patch **61×–73×**, and accounts for 98 % of its bytes
(§3). This is the whole problem, and it is not subtle.

## 3. Transparent decompression alone: 27× [measured]

Decompress each `SHF_COMPRESSED` section, delta the plaintext, per section
(A→B; `bsdiff`, so approximate matching, which flatters the compressed
column less than a copy-based differ like xdelta3 would):

| section | raw A | delta of *compressed* | delta of *raw* |
|---|---|---|---|
| `.debug_abbrev` | 614 | 136 | 136 |
| `.debug_line` | 986,894 | 526,643 | **1,446** |
| `.debug_frame` | 316,644 | 17,850 | **571** |
| `.debug_info` | 2,172,594 | 941,331 | **75,973** |
| `.debug_loclists` | 1,152,724 | 416,523 | **235** |
| `.debug_rnglists` | 338,514 | 202,604 | **271** |
| `.debug_addr` | 48,848 | 16,961 | **394** |
| **total** | 5,016,832 | **2,122,048** | **79,026** |
| `.symtab` / `.strtab` | 486,517 | (never compressed) | 622 / 143 |

**2.12 MB → 79 KB, a 26.9× win from decompression alone**, and A→C is the
same shape (2,175,129 → 81,952). Note that this is the *entire* value of a
Zucchini-class tool here: it cannot see through zlib, so it pays 2.1 MB.

`bsdiff` is not a straw man on this data. `zstd -19 --patch-from --long=27`
was run on every section both ways [measured]; it is better on a few small
sections (`.debug_abbrev` 23 B, `.debug_rnglists` 160 B) and much *worse*
where it matters — `.debug_info` 269,424 B against bsdiff's 75,973 — because
scattered one-byte field edits break its copy matches while bsdiff's
byte-wise difference stream absorbs them. Best-of-both per section is 78,780 B,
i.e. the 27× figure is not an artifact of the differ chosen.

After decompression **`.debug_info` is 96 % of what is left**. Every other
section is already near-free. That is the single most useful fact in this
note: the structural work has exactly one target.

## 4. Section by section: derivable, or transmitted?

### `.debug_info` — the only hard case, and it is not hard

Go's abbrev table is 614 bytes and fully enumerable [measured]. The forms
that matter:

- `DW_AT_low_pc` on subprograms is **`DW_FORM_addrx`** (an index into
  `.debug_addr`), `DW_AT_high_pc` is **`DW_FORM_udata`** (a *length*)
  [source, `cmd/internal/dwarf/dwarf.go:387-395`, the `DW_FORM_lo_pc_pseudo`
  / `hi_pc_pseudo` switch]. So moving a function does not
  touch its DIE at all — only the `.debug_addr` slot it points at. This is a
  large gift and it is DWARF-5-specific: under DWARF 4 Go emitted absolute
  `DW_FORM_addr` for both.
- `DW_AT_type` / `DW_AT_abstract_origin` are **`DW_FORM_ref_addr`**: 4-byte
  absolute offsets into `.debug_info`. There are **100,624** of them in this
  binary [measured]. Any insertion anywhere shifts all of them that point
  past it.
- `DW_AT_location`, `DW_AT_ranges`, `DW_AT_stmt_list`, `DW_AT_addr_base` are
  **`DW_FORM_sec_offset`** into `.debug_loclists` / `.debug_rnglists` /
  `.debug_line` / `.debug_addr`. Go writes `offset_entry_count = 0` in the
  loclists/rnglists headers [source, `ld/dwarf.go:2547`], so `loclistx`/
  `rnglistx` never appear and every list reference is a raw section offset
  that shifts when the target section grows.
- A residue of true absolute addresses: CU `DW_AT_low_pc`, `lexical_block`
  low/high pc, and Go's `DW_AT_go_runtime_type` (0x2904) on base types
  [source, `cmd/internal/dwarf/dwarf.go:315`], which points at the runtime
  type descriptor the Go module already models.
- Crucially, **DIE sizes are stable under function movement**. Go writes
  addrx indices as *fixed-length padded* ULEB128 — the `R_DWTXTADDR_U1..U4`
  relocation family picks the byte width at compile time and
  `writeUleb128FixedLength` pads with continuation bits [source,
  `objabi/reloctype.go:425`, `ld/data.go:3448`]. Adding functions therefore
  does not resize DIEs unless a package crosses a width threshold. This is
  what makes a pure field-relocation prediction viable rather than a
  re-serialisation.

I ran an **oracle** [measured]: predict B's `.debug_info` as A's with one
87-byte insertion, then correct every reference field by the relevant
section's length delta. Result:

| corrected | count |
|---|---|
| `ref_addr` (+87, `.debug_info`'s own growth) | 99,158 |
| `sec_offset` → `.debug_loclists` (+41) | 40,197 |
| `sec_offset` → `.debug_rnglists` (+13) | 20,335 |
| `sec_offset` → `.debug_line` (+34) | 178 |
| 8-byte absolute addresses | 1,206 |
| **residual differing bytes (of 2.17 MB)** | **1,195** |

`bsdiff` of that prediction against the target: **380 bytes**, from 75,973.

So `.debug_info` goes **941,331 → 75,973 → 380**. The correction deltas are just
the *length deltas of the sections presage already regenerates*; nothing
about DWARF semantics is needed beyond knowing where the fields are.

The A→C pair (a real new function, not an inlined one) reproduces this
exactly [measured]. Its five section length deltas are `.debug_info` +119,
`.debug_loclists` +98, `.debug_line` +62, `.debug_addr` +8,
`.debug_rnglists` +7 — and the field corrections found are 99,160 at +119
(`ref_addr`), 40,199 at +98, 20,335 at +7, 178 at +62, and **179 at +8**
(`DW_AT_addr_base`, which only shows up once `.debug_addr` actually grows).
Residual 1,136 differing bytes; `bsdiff` **401 bytes**, from 78,870 raw and
942,602 compressed. Every correction delta is, exactly, some section's
length delta. There is no residual mystery in this section.

Two honest caveats. (a) The oracle is handed the insertion point and its 87
bytes free; a real encoder pays a small plan cost for the DIE-level edit
script. (b) The oracle identifies fields by "this value changed by exactly
Δ", which is an oracle, not an algorithm — a real implementation must walk
DIEs against the abbrev table to know field positions. That walk is
mechanical (Go's own `debug/dwarf` does it) and, unlike the oracle, has no
false positives. The offset correction is also not really a scalar: it is a
piecewise map old→new per referenced section, i.e. presage's existing `map`
op, which happens to be near-constant here.

### `.debug_addr` — 100 % derivable

48,848 bytes = one flat table of 8-byte addresses (Go emits a single
section-wide table rather than per-CU contributions, which is why readelf
complains — [#77246](https://github.com/golang/go/issues/77246)). A→B changed
**634 entries, every one by exactly +96, the `.text` growth; nothing else**
[measured]. This is `rewrite(ptrs, 8, map)` with the function map presage's
Go module already derives. Cost after prediction: ~0.

### `.debug_frame` — 99.3 % derivable

FDE `initial_location` fields are 8-byte absolute [DWARF5 §6.4.1]. A→B:
634 of them changed by +96, and **6 other bytes in the whole 316 KB section**
[measured]. The CFI opcode streams themselves are unchanged. Relationship to
pclntab: `.debug_frame` encodes CFA/register rules while pclntab's `pcsp`
encodes SP deltas; they overlap in intent but not in encoding, and I did
**not** verify that one is computable from the other. Given the section
costs 571 bytes after retargeting, deriving it from pclntab is not worth
attempting — retarget and move on.

### `.debug_loclists` / `.debug_rnglists` — cheap for a structural reason

Go emits exactly one shape per variable: `DW_LLE_base_addressx` (an addrx
index) followed by N × `DW_LLE_offset_pair` — **PC offsets relative to the
function base, as ULEB128** — then `end_of_list`
[source, `ssa/debug.go:1485`, `cmd/internal/dwarf/dwarf.go:1057`]. Because
the offsets are function-relative, moving a function changes *nothing* in its
list. That is why 1.15 MB of loclists deltas to 235 bytes.

Is loclists content derivable from elsewhere? **No.** The `DW_OP_*`
expressions and the PC boundaries at which a variable migrates between
registers and stack slots are the register allocator's decisions and exist
nowhere else in the artifact — pclntab records PC→func/file/line and inlining
trees, not variable locations. It must be transmitted. Fortunately, when a
function is unchanged, "transmitted" means a `copy`.

### `.debug_line` — cheap, overlapping with pclntab in principle

The only absolute address in a line program is `DW_LNE_set_address`
[DWARF5 §6.2.5.3]; everything else is a delta. Content-wise this *is*
redundant with pclntab's `pcfile`/`pcln` tables — both encode PC→file/line
for the same code — so a regenerator is conceivable. It is not worth it:
986 KB deltas to 1,446 bytes after decompression, and the two encodings
(DWARF special opcodes vs Go's varint pcdata) are different enough that a
transcoder would be a large, fragile module for <1.5 KB.

### `.symtab` / `.strtab` — 99.8 % derivable, and never compressed

`.symtab` is an array of 24-byte `Elf64_Sym`. A→B: 2,634 differing bytes,
**99.8 % of them `st_value` fields shifted by their section's delta**
(+96 text, +64 pclntab, +32 data) [measured]; `.strtab` was byte-identical
because no symbol name changed. A→C added exactly one 24-byte symbol and 15
bytes of name. Names, sizes and addresses map 1:1 onto functions the Go
module already enumerates, so `.symtab` is a `table(kind, entries)` op over
the existing function/data maps and `.strtab` is a name-list delta. These
sections survive `-w`, so this work pays off on `-w` builds too — it is
worth ~700 bytes of the 35 KB stripped patch, i.e. ~2 %.

## 5. Recompression: can we put the bytes back exactly?

This is the load-bearing question for the whole design, and the answer is
better than expected.

**Go-internal-linked binaries: yes, exactly.** For every compressed section
of a go1.27-linked binary, `zlib.NewWriterLevel(w, zlib.BestSpeed)` over the
decompressed bytes reproduced the section payload **byte for byte** — all
seven sections, `recompress_exact=true` [measured]. No preflate needed, no
reconstruction data, zero overhead. (`bench/dwarfprobe/main.go` in this repo
already asks this question; this confirms it.)

**But the compressor is version-pinned, not implementation-pinned.** Go 1.27
replaced `compress/flate`'s fast encoders (`deflatefast.go` → `level1.go`…
`level6.go`). A binary linked by **go1.25.7 fails to recompress under
go1.27's `compress/zlib` at every level** — and go1.25's output is *smaller*
(`.debug_info` 788,375 vs 844,115), so the encoder genuinely changed
[measured]. Any implementation must key the compressor on the toolchain
version (readable from the target's build info) and must verify, never
assume.

**Externally linked (cgo) binaries: different compressor, still exact.**
A cgo build's debug sections are compressed by GNU ld, not Go, and pick up
`.debug_str`/`.debug_line_str`/`.debug_aranges` from the C toolchain
[measured]. Go's `compress/zlib` reproduces **none** of them at any level —
but stock zlib at `(level=6, Z_DEFAULT_STRATEGY, windowBits=15, memLevel=8)`
reproduces **all** of them byte for byte [measured, via python `zlib`]. So a
two-candidate set — "Go flate BestSpeed at version V" and "zlib level 6" —
covers both linking modes.

The catch: a pure-Go decoder cannot produce zlib-6 output (Go's flate matches
it at no level [measured]), so covering cgo builds needs cgo zlib, a
byte-exact Go port of zlib's deflate, or a reconstruction scheme. §7 supplies
the clean answer: a **Puffin-style Huffman-only re-encode is deterministic by
construction and implementable in pure Go**, because it never re-runs LZ77 —
it re-derives the Huffman tables from the code-length arrays already present
in the stream. That makes the cgo case a bounded size cost rather than an
open correctness question.

The wider lesson from preflate-rs's measurements (§7) is that "same
compression level" is not a specification: zlib, zlib-ng, libdeflate and
miniz at level 6 produce four different bitstreams, and preflate needs
~1 % corrections to bridge them. Neither zlib nor the deflate spec documents
any guarantee of bit-stable output across versions. The Go-1.25-to-1.27
break found above is the same phenomenon inside one library, and it means
**the codec registry must be keyed and verified, never inferred**.

## 6. Recommended architecture

**(a) A generic transparent-decompression layer, as a core region
transform.** This is not a Go module concern; it belongs beside the existing
`recompress(codec, raw, correction)` op (SPEC §5.3, R12). Shape:

1. Probe ELF sections for `SHF_COMPRESSED` (and `.zdebug_`/`"ZLIB"` on
   Mach-O/PE). Decompress; the section becomes a child region whose parent
   plan is `recompress(codec_id, child_output)`.
2. `codec_id` names a *pinned, versioned* encoder from a small registry —
   `go-flate-1@go1.25`, `go-flate-1@go1.27`, `zlib-6` — not "whatever
   compress/zlib does today". The encoder tries the candidates and stores
   the id of the one that round-trips. Borrow archive-patcher's fingerprint
   check (§7): a small fixed corpus whose compressed digest identifies the
   local encoder exactly, so a decoder detects a `compress/flate` change
   before it produces a wrong byte rather than after.
3. Fallback ladder when none matches, in order: **preflate**-style
   reconstruction data (0.01 % of plaintext against a well-modelled zlib,
   ~1–3 % against an unmodelled encoder — §7); then a **Puffin**-style
   Huffman-only re-encode, which cannot fail because it never re-runs LZ77
   (~320 B per 32 KB block, but the delta is then taken over an LZ77 token
   stream rather than plaintext, which deltas worse); then opaque bytes,
   losing nothing relative to today. Rungs 1–2 delta plaintext; rung 3
   trades delta quality for a guarantee. Note that preflate's reconstruction
   records for the old and new versions of a section will themselves be
   nearly identical, so **delta them against the reference rather than
   shipping them whole** — pristine-tar is the only prior tool that does
   anything adjacent (it stores a binary diff against the closest
   regeneratable output), and no recompressor does it.
4. The self-prediction gate (SPEC §4.3) and the per-region hash (§7) already
   catch a recompression mismatch before it ships. Keep the invariant that a
   mismatch is a *size* regression, never a correctness one.

This step alone is 27× and is the bulk of the win. It is also reusable:
every `SHF_COMPRESSED` C/C++/Rust binary, every `.gnu_debugdata`, and
(with a different codec) OCI layers benefit from the same op. Cost is not a
concern: Go's zlib does 225 MB/s decompress and 171 MB/s recompress
single-threaded on this data [measured], so a 13 MB DWARF payload adds well
under 150 ms to a patch application, and the linker itself already
parallelises per section.

**(b) A DWARF field-relocator: yes, worth it, and it is small.** Not a DWARF
model — a *field locator*. It needs to walk DIEs against the abbrev table and
report the position and form-class of every `ref_addr`, `sec_offset`,
`addr`, and `addrx` field, then hand those to presage's existing `map` /
`rewrite` / `refs` ops with a piecewise old→new offset map per target
section. Measured payoff on the one section that matters: **75,973 → 380
bytes**, on top of the 27×. Everything else — `.debug_addr`, `.debug_frame`,
`.symtab` — is the same machinery with a simpler locator.

The region DAG (SPEC §5.2) falls out naturally and must be respected:
`.debug_line`, `.debug_loclists`, `.debug_rnglists`, `.debug_addr` are
produced first (they depend only on the function map); `.debug_info` names
them as inputs because its `sec_offset` corrections need their *lengths*.

**What is not worth building**: a `.debug_line` regenerator from pclntab
(≤1.5 KB at stake, large fragile module), a `.debug_frame` regenerator from
`pcsp` (≤600 B), and any attempt to derive `.debug_loclists` content
(impossible — it is register-allocator state).

**Rough end state.** DWARF's marginal contribution to an A→B patch would fall
from **2,158,096 bytes to roughly 3.4 KB** (the per-section raw deltas with
`.debug_info` at its oracle value), i.e. ~10 % on top of the 35.5 KB
`-w` patch, versus 61× today. Even stopping at step (a) — decompress,
recompress, delta the plaintext with the existing layers — is a ~27× win for
a few hundred lines of code, and should be built and measured first.

## 7. Prior art

**Nobody delta-codes debug information between two builds.** The space splits
into tools that shrink debug info *within* one artifact and tools that diff
bytes without looking inside, and the two have never met.

### Executable differs exclude `.debug_*` structurally

Zucchini's `JudgeSection()` returns `SECTION_IS_USELESS` for any section with
`sh_addr == 0` — *"these tend to duplicates (can cause problems for lookup)
and uninteresting"*
([disassembler_elf.cc](https://chromium.googlesource.com/chromium/src/+/main/components/zucchini/disassembler_elf.cc)).
Every DWARF section has `sh_addr == 0` [measured]. The consequences are
traceable: no offset↔RVA mapping is registered, and
`ExtractInterestingSectionHeaders()` gates both reloc- and exec-section
collection on bits those sections never get, so neither abs32 nor rel32
extraction ever touches them. Those bytes fall through to the generic layer —
suffix-array equivalence matching plus `int8_t diff = new - old` — i.e. **over
`.debug_*`, Zucchini degenerates to bsdiff**, while still paying suffix-array
construction on them. A grep of all 173 files in `components/zucchini` for
`dwarf|debug_info|debug_line|debug_str` finds three hits, none DWARF-related.

Courgette is moot: it was **deleted from Chromium** in commit
[1c9e567c70c7](https://github.com/chromium/chromium/commit/1c9e567c70c74d134000fe38abdd34e8037a6a13)
(2024-08-29). Its ELF support had been 32-bit x86 only, with rel32 extraction
name-gated to `.text`.

This is not an oversight. Chrome ships stripped binaries —
`build/linux/strip_binary.py` runs `--only-keep-debug` → `--strip-debug
--strip-unneeded` → `--add-gnu-debuglink` — so the case never arose. **No
published delta number, from Chromium or Mozilla, was measured on a binary
carrying debug info.** That is an unmeasured gap, not a settled negative.

### Structured DWARF tools optimise; none of them differences

[dwz](https://manpages.debian.org/testing/dwz/dwz.1.en.html) (Jelinek 2012)
hoists duplicate DIE subtrees into `DW_TAG_partial_unit`s. Real numbers from
the [announcement](https://gcc.gnu.org/legacy-ml/gcc/2012-04/msg00686.html):
LibreOffice's `.debug_{info,abbrev,types}` 1,433,895,483 B → **44.2 %**;
`libxoflo.so.debug` → 23.7 %; cost 18.8 s and 2.2 GB RAM for 16 M DIEs.
**dwz cannot process Go's output at all**: it supports neither
`DW_FORM_strx`/`addrx` nor `rnglistx`/`loclistx`. LLVM's DWARFLinker (behind
`dsymutil`, now defaulting to the parallel implementation) dead-strips from
address-bearing roots and uniques types by a structural synthetic name.
[Bloaty](https://github.com/google/bloaty) parses DWARF only to attribute
bytes; its only compression code is read-side `SHF_COMPRESSED` handling.

The redundancy census from Split DWARF is worth knowing because of how it
*fails* to apply here: the
[DebugFission wiki](http://web.archive.org/web/20230314070429/https://gcc.gnu.org/wiki/DebugFission)
measures **93 % of `.debug_str` and 85 % of `.debug_types` discarded as
duplicate** at link time, and LLD's RFC found 72 % of clang's `.debug_info`
was duplicate type descriptions. **Go has already collected all of this.**
The linker keeps one global type-DIE tree memoised through `tmap`/`tdmap`/
`rtmap` in `dwctxt`, so each Go type gets exactly one DIE per link
[source, `ld/dwarf.go`]. There is no dwz-style win left on a Go binary — which
is the flip side of §4's finding that what remains is nearly all unique
content plus references to it.

**Academic literature: none.** A DBLP full-text search for *"debug
information compression"* returns zero hits; *"DWARF"* returns white dwarfs
and a mongoose metaheuristic. Existing DWARF papers are about *correctness*
of debug info under optimisation, not size. The nearest neighbour, ΔBreakpad
([arXiv:1705.00713](https://arxiv.org/abs/1705.00713)), patches Breakpad
symbol files across *diversified variants of one build*, not across versions.
There is no baseline to measure against.

### Decompress-then-diff is proven; four strategies exist

| strategy | guarantee | used by |
|---|---|---|
| guess-and-verify at build, else leave compressed | strong, producer-side | archive-patcher, debdelta |
| guess at build, verify by hash at apply | weak — fails on the client | deltarpm |
| don't guarantee the compressed form | none | zsync |
| make re-encoding structurally deterministic | strong by construction | **Puffin** (Chrome, AOSP) |

[archive-patcher](https://github.com/google/archive-patcher) (Google Play)
is architecture (a): uncompress changed members, bsdiff the plaintext,
recompress to a bit-exact archive. Production impact: **65 % smaller updates
on average, 6 PB/day saved**; Netflix 16.2 MB → 1.2 MB. Details worth
copying, all verified in its source:

- The search space is **exactly 32 candidates** — strategy 0 (levels
  6,9,1,4,2,3,5,7,8), strategy 1 (6,9,4,5,7,8), strategy 2 (1), × `nowrap` —
  ordered by real-world popularity, with an early `break` on the wrap loop
  when the inflater throws.
- Verification is **fail-fast byte-for-byte** (`MatchingOutputStream` throws
  on the first mismatching byte, plus `expectEof()`), not a prefix check.
  Contrast zsync, which compares 900 bytes.
- **The consumer-side fingerprint is the part nobody else ships**:
  `DefaultDeflateCompatibilityWindow` deflates a 9,045-byte corpus (135
  successively longer lorem-ipsum prefixes, chosen to exercise zlib's hash
  chaining so all 9 levels differ) under every combination, SHA-256s each,
  and compares against hardcoded values. The applier **refuses to run** on a
  mismatching zlib. Empirically every zlib since 1.2.0.4 (2003) fingerprints
  identically.
- **Caveat that bounds the technique**: `memLevel` and `windowBits` are *not*
  in the search space and cannot be — `java.util.zip` hardcodes
  `DEF_MEM_LEVEL 8` and `MAX_WBITS`. Precomp searches all 81 level×memLevel
  combinations precisely because that assumption fails outside zip archives.

The others are instructive mostly as failure modes. deltarpm reserved a
`targetcomppara` field for compression parameters and has written zero there
for twenty years, relying instead on a bundled zlib fork and an apply-time
MD5 — i.e. mismatches surface on the client after full patch application.
debdelta's author: *"I had to be able to gunzip those files, diff them, and
gzip back them exactly identical … **90 % of the times**"*, and it documents
the cross-version problem as unsolved. zsync explicitly abandoned the idea:
its Z-Map2 header is marked *"(Legacy)"* and generators *"SHOULD NOT include"*
it. Only **pristine-tar** degrades gracefully, storing a binary diff between
the closest regeneratable output and the original — ~99.5 % success.

There is real academic work here, though none of it on debug info: **Donag**
(May, *ACM TOS* 18(3), 2022, [doi:10.1145/3507919](https://dl.acm.org/doi/10.1145/3507919)),
content-aware differencing of compressed archives, 10–89 % smaller than
bsdiff/xdelta3; and a systematic study of *"decompressing-before-differencing"*
over 200 apps (*IEEE TMC* 23(12), 2024,
[doi:10.1109/TMC.2024.3407867](https://dl.acm.org/doi/10.1109/TMC.2024.3407867)).

### Recompression: guess, predict-and-correct, or restructure

[preflate](https://github.com/deus-libri/preflate) splits a deflate stream
into plaintext plus reconstruction data, motivated by exactly this problem:
*"Reconstructing the original deflate stream becomes important if the
position or size of the reconstructed deflate streams must not differ, e.g.
if those streams are embedded into executables."* For ~20–30 % of zlib
streams the reconstruction data exceeds 3 bytes, *"usually only a few tens of
bytes"*. Microsoft's [preflate-rs](https://github.com/microsoft/preflate-rs)
is in production cloud storage and publishes overhead **as a fraction of
uncompressed size**: zlib **0.01 %** at almost every level, but zlib-ng
0.90–1.07 % at levels 4–6 and libdeflate ~1 % throughout — *at nominally
identical levels*. It CABAC-codes divergences from a predicted token stream,
encoding a wrong distance as a *hop count back through the hash chain*. Its
`verify_compression` defaults on and errors with `RoundtripMismatch` rather
than emitting an unverified blob, so it is **guaranteed-verified, not
guaranteed-successful**.

**Puffin** is the find that changes the fallback design
([README](https://chromium.googlesource.com/chromium/src.git/+/main/third_party/puffin/README.md)).
It undoes *only the Huffman layer*: *"There is no need to perform LZ77
algorithm… The dynamic Huffman tables can be recreated uniquely from the code
length array stored inside the `puff` stream."* Re-encoding is therefore
**deterministic by construction** — no guessing, no model, no failure mode —
and it even preserves the undefined padding bits of stored blocks. Cost:
*"maximum 320 bytes for each block"* (~one block per 32 KB), and `huff` is
*"in order of 10× faster"* than full recompression. The tradeoff is that a
puffed stream is still LZ77 tokens, so *"bsdiff of two puffed streams … is
larger than uncompressed streams"*. Chrome ships it today
(`components/update_client/op_puffin.cc`), as does AOSP for A/B OTA.
Archive-patcher's README lists precisely this as unimplemented future work.

### The gap

Zucchini excludes `.debug_*` by construction; no DWARF tool differences one
build against another; and no deflate recompressor scans ELF
`SHF_COMPRESSED`/`.zdebug_*` at all — preflate-rs's container scanner handles
zlib, gzip, PNG, ZIP and JPEG only. The intersection is unoccupied, and §3–§4
say it is worth roughly 600× on the sections in question.

## 8. Loose ends

- Every measurement here is one small `net/http` binary and two synthetic
  edits. Re-run on prometheus/CockroachDB-scale binaries and on a real
  release pair before quoting any ratio.
- The DWARF-4 path (Go ≤1.24, and darwin/ios/aix on any version) has
  `DW_FORM_addr` for subprogram `low_pc`/`high_pc` instead of
  `addrx`/`udata`, and `.debug_loc`/`.debug_ranges` instead of the v5
  sections. That is *more* absolute addresses in `.debug_info`, so the
  field relocator matters more there, not less. Unmeasured.
- Whether a package crossing an `R_DWTXTADDR_U1→U2` width boundary
  perturbs DIE sizes enough to matter is unmeasured; expected to be rare
  and localised.
- The cgo/external-link case (different section set, zlib-6, `.eh_frame`,
  `.debug_str`) was probed but not delta-measured. Note it also reintroduces
  `.debug_str`, which Go alone never emits, and which is where the classic
  93 %-duplicate-strings result applies.
- Whether the preflate or Puffin rung is ever actually needed is untested
  here: both linking modes recompressed exactly. Build the registry and the
  fingerprint check first; add a reconstruction rung only when a real corpus
  produces a miss.
- `bench/dwarfprobe/main.go` already exists in this repo and asks the §5
  question; it is the natural place to grow the codec registry and the
  Archive-Patcher-style fingerprint check.
