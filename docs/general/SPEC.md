# presage — a general predictive codec with pluggable structure models

Design specification, round 1. Status: milestone 1 (§10) is built as the
`presage` package with the Go module (`presage-core.md` is its
implementation spec); the rest is design. Every number below is either measured in this
repository's research corpus (`docs/research/`, `docs/general/research/`)
or marked *estimate*.

## 0. One paragraph

Any serialised structure with internal offsets — an executable, an archive,
a tensor file, a database page — turns a small *first-order* edit into a
large *second-order* rewrite of addresses, sizes and tables (Percival 2006,
`research/percival-thesis.md`). General compressors and byte-level delta
coders pay for the rewrite; go-binsync showed that a model of the format can
*regenerate* it from a compact description of the first-order change, for
28–67× smaller patches than bsdiff on Go binaries
(`research/binsync-lessons.md`). presage makes that architecture generic: a
fixed **core** (a plan interpreter, one shared residual coder, a hashed
frame container) plus pluggable **structure modules** that turn an input
into a *plan* — a bounded declarative program of copy / relocate /
regenerate operations — from which the core rebuilds a deterministic
**prediction** of the target, then transmits only the correction. Modules
are selected per structural region by measured size (the Roaring-bitmap
pattern), compose as a bounded tree for nested containers, and are
referenced by hash so a decoder that lacks one can fetch it or have the
encoder *lower* the plan to core operations at a cost in bytes.

## 1. What the research settled

Findings that constrain the design; each links to the note that measured it.

| # | Finding | Consequence | Source |
|---|---|---|---|
| R1 | The 10× beyond bsdiff comes from *regenerating* second-order structure, not from a better matcher (Percival's symbol-free bsdiff 6 already matched a disassembler). | The unit of extension is a format model that regenerates, not a smarter differ. | `percival-thesis.md`, `binsync-lessons.md` §1 |
| R2 | Length-exact prediction makes the residual positional and removes the suffix array (2.4 s vs 40 s encode on 94 MB). Length-changing residuals need a shifted (LZ) delta instead (66 KB vs 17 KB on binsync stage 1b). | Two residual coders, chosen by the module's declared length property. | `binsync-lessons.md` §2 |
| R3 | A wasm predictor that crunches bytes runs at 37 MB/s AOT-compiled and 1 MB/s interpreted against 98 MB/s native (same output hash in all three). | Modules must be *plan-emitting*, O(metadata); the core materialises bytes natively. Interpreters are a correctness path only. | `wasm-throughput-probe.md`, `portable-predictors.md` §6.3 |
| R4 | Wasm's non-determinism is a short closed list (NaN bits, relaxed SIMD, threads, imports, grow/stack failure); blockchains solved it with no-floats, injected gas, explicit stack limits. ZPAQ proved "the stream names its decoder" works for 12 years, and that a VM without calls/fuel gets no ecosystem. | Restricted wasm profile, no floats, no imports, fuel + memory declared in the header. | `portable-predictors.md` §1–2, §6.2 |
| R5 | Hard per-region selection beats mixing for strong, mutually exclusive structural models; a stream-coded selector costs ≈ log₂\|M\| per switch vs log₂\|M\| + log₂ n decoder-side. BtrBlocks: 1 % sample picks the optimum 77 % of the time at 3.3 % size loss. | Encoder-side selection, structural regions, three-tier cost model. | `adaptive-predictive-coding.md` §9 |
| R6 | One shared terminal residual coder with 2–4 typed sub-streams beats per-module coders (dictionary evidence: splitting statistics costs 2.5× on small streams). | Modules may transform residuals (xor, delta, byte-plane split) but never bring an entropy coder. | `adaptive-predictive-coding.md` §9c |
| R7 | The encoder holds unstripped artefacts for both versions; the function correspondence can be *shipped* (a permutation + sizes) rather than recovered on the client. | Matching is an encoder-side component; decoders replay. | `domain-executables.md` §6.4 |
| R8 | Every native format decomposes into the same four pieces: matcher → piecewise offset map → per-ISA operand relocator → offset-table regenerators. Only container parsing and the table list are format-specific. | Those four are core plan operations; a format module is mostly a parser. | `domain-executables.md` §6.1 |
| R9 | Where layout is not a function of the old file (PGO/orderfile refresh, ICF, RISC-V relaxation) prediction must degrade to Zucchini-style matched regions + corrected references, not to bsdiff. | The plan language must express "matched region with typed references" as well as "regenerated layout". | `domain-executables.md` §6.2, §6.4 |
| R10 | Model weights are i.i.d. to a lossless coder: 1.5× floor standalone, fine-tune deltas are 50–65 % of the file; the only 100× lossless cases are sparse per-step syncs and bit-exact recomputation of derived artefacts. | Weights are a second-tier module (tensor alignment + exponent split + FSE); throughput ≥ 2 GB/s/core is the competitive axis, not ratio. | `domain-ai-weights.md` §7 |
| R11 | Universal (one-patch-for-many-old-files) deltas via RS syndromes cost ~7× a pairwise patch on Percival's corpus but become attractive once a prediction leaves only sparse substitutions. | An optional syndrome correction mode, module-level, not default. | `percival-thesis.md` ch. 3 |
| R12 | Every OCI layer from moby/BuildKit/containerd is Go `compress/gzip` at default level, and no published recompressor models Go's encoder (preflate-rs: zlib 0.01 % correction, other encoders ~1 %, unknown ones round-trip at higher overhead). Shipping fleet-delta tools look inside nothing (librsync/xdelta3/block hashes); archive-aware gains are 1–6×. Only 2.7 % of Dockerfiles rebuild bit-identically. | A core `recompress` op with preflate's contract (any input round-trips; correction size is the metric), never encoder pinning; a Go-deflate model is an unfilled niche; container gain = the inner binary's gain in a container costume. | `domain-containers-packages.md` §7 |

## 2. Scope

In scope for v1:

- One **target** object (bytes) predicted from a **reference set** of zero
  or more objects. Zero references = compression; one = delta (binsync's
  case); several = delta against a fleet of near-identical variants.
- **Structural modules** for: Go linux/amd64 (port of the existing codec),
  generic ELF (C/C++/Rust, PIE and non-PIE, x86-64 then arm64), PE x64,
  tar/zip/gzip containers with deterministic recompression, safetensors/GGUF
  tensor alignment. Ranking and rationale in §9.
- **Portable modules**: a restricted-wasm profile so a decoder can run a
  module it was not built with; and **plan lowering** so a decoder need not
  run any module at all.
- The existing go-binsync container (frames, hashes, pointer/store) reused
  unchanged where possible.

Out of scope for v1: lossy anything; learned/neural predictors; universal
syndrome patches (§7.5, reserved); P2P distribution; the update-lifecycle
half of go-binsync (it is a client of this codec, not part of it).

## 3. Architecture

```
                 ┌──────────────── encoder (publisher) ─────────────────┐
 refs, target ──▶│ probe → region tree → per region: candidate modules  │
                 │   module.analyse(refs, target) → plan + side tables   │
                 │   core.materialise(plan) → prediction                 │
                 │   residual = target ⊖ prediction; choose smallest     │
                 │   lower plan ops the deployed decoder cannot run      │
                 └─────────────────────┬────────────────────────────────┘
                                       │ patch = header + region tree + plans + residual streams
                 ┌─────────────────────▼────────────────────────────────┐
 refs ──────────▶│ decoder: verify header → for each region in DAG order │──▶ target
                 │   core.materialise(plan) [module ops native or wasm] │
                 │   check H_pred → apply residual → check H_target     │
                 └──────────────────────────────────────────────────────┘
```

Three layers with a strict dependency direction (each may depend only on
the ones above it):

1. **Core** (fixed, versioned, in every decoder): container and frames;
   region tree; plan language and its interpreter (§5); residual coders and
   the terminal entropy stage (§6); hashing and verification (§7); module
   loading and resource limits (§8).
2. **Modules** (pluggable, referenced by hash): a *probe* that recognises a
   format, an *analyser* that runs on the encoder and produces a plan, and
   zero or more *plan operations* the decoder must implement to expand that
   plan (§4). A module is one of: built into the decoder (native), portable
   (wasm), or absent (its ops must have been lowered).
3. **Distribution** (out of the codec's hands): stores publish modules by
   hash next to patches; decoders cache them. go-binsync's store layout gains
   one directory, `modules/<hash>.wasm`.

The design rule that follows from R3 and R8: **anything O(bytes) is in the
core; anything O(metadata) may be in a module.** Copying, relocating
operands through a map, serialising a sorted table, LZ-matching and entropy
coding are core operations; deciding *which* bytes are functions, *which*
tables exist and *what* moved is module work.

## 4. Modules

### 4.1 Interface

A module is a pure function of its declared inputs. Two entry points:

```
probe(window)                       → {format id, confidence, region proposals}
analyse(refs, target, hints)        → plan, side tables          (encoder only)
```

and a set of **plan operations** it registers (§5.3), each a pure function
`op(inputs…, side table) → bytes` with a declared output length. The
decoder never calls `analyse`; it only executes ops named in the plan.

Declared properties, carried in the module manifest and checked at
admission:

| property | meaning |
|---|---|
| `id` | BLAKE3 of the admitted module binary (or a well-known id for built-ins) |
| `formats` | magic/probe rules; used by tier-1 selection (§6.4) |
| `ops` | plan operations implemented, each with `length: exact \| declared` and an expansion bound *k* (output ≤ k·input + c) |
| `inputs` | which other regions' outputs an op may read (a DAG edge, §5.2) |
| `budget` | max memory (pages) and fuel per MB of input; the decoder refuses a plan that exceeds what the header declares |
| `lowering` | for each op, the core-op sequence the encoder may substitute (§5.4) |

### 4.2 Execution tiers

| tier | where the op runs | when |
|---|---|---|
| native | compiled into the decoder | built-in modules; the fast path |
| portable | restricted wasm (§8) under the core's runtime | the decoder lacks the native module and the store offers the wasm one |
| lowered | nowhere — the encoder replaced the op with core ops + more residual bytes | the decoder is known to have neither |

A module ships as one source that produces both the native and the wasm
build (`portable-predictors.md` §6.5). The wasm build is the reference
semantics; the native build must produce the same prediction hash, and
every patch verifies that (§7). This is how a module written in Rust or Zig
by a format's owner becomes usable from a Go decoder without cgo.

### 4.3 Determinism contract

Modules may not: read a clock, randomness, environment, thread count or
any host state; use floating point; allocate beyond the declared budget;
read any region not declared as an input. The wasm profile enforces this
mechanically (§8); native modules are held to it by the **self-prediction
gate**: for every corpus object, `analyse(obj, obj)` must yield a plan whose
materialisation is byte-identical to `obj` with a residual ≤ 64 B, and the
native and wasm builds must agree on the prediction hash across the corpus.
This gate is what caught binsync's silent 8-byte `textStart` bug
(`binsync-lessons.md` §4) and is a required test for every module.

### 4.4 Built-in module: Go tables

The Go-aware codec's metadata regeneration, offered as one layer of the
ELF predictor rather than a separate codec (`delta/gotables.go`,
`bench/elfpredict`). The two tools had become complementary: the generic
predictor's `.text` model (Zucchini equivalences, structural map, field
correction) beats the Go codec's on real releases, while the Go codec
regenerates `.gopclntab`, the type descriptors and the data sections the
generic predictor can only copy. Results in `go-module-results.md`.

**Plan.** One stream, `GoTables`, carried beside the structural plan: a
transform byte, then the Go layout (section table, moduledata, function
list with segment maps, data maps, shift tables, overrides) and the stage-1
streams (funcnametab/filetab delta; cutab/pctab/`go:func.*` correction),
byte-identical to what the Go codec writes at its current transform. The
module's prediction is the codec's, byte for byte (checked on the
prometheus minor release: 0 differing bytes); the transform byte is what
makes that hold, a plan written at transform 1 lacks the segment maps and
mispredicts 700 KB more of `.text`.

**What it writes.** Every section of the new image except those the caller
keeps: `.text` when there are equivalences to copy it from, and the
relocation table when the relocation layer has rebuilt it. Data sections go
through the content maps and pointer rewriting; sections the module has no
model for are copied by name. It runs after the equivalence copy and the
relocation layer, before `.eh_frame` and the field layers. The `.rodata`
layer is skipped on a Go binary (it selected 0 of 0 spans on every pair).

**What it lends the other layers,** all derived by both sides from the plan
alone, so they cost no bytes:

- the **function map** (`planGoDerived`): a structural plan in this mode
  carries no map columns; `delta.GoFunctionMap` (pclntab entries, paired by
  the layout's match) is the map, every unit copied. On the synthetic pair
  this removed 18.5 KB of a 19.3 KB plan; the derived map shares 99.8 % of
  its entries with the symbol-built one.
- an **address prior** (`delta.GoAddressLookup`): the module's data maps
  answer for data addresses after the plan's exact reference points and
  before its section geometry. Restricted to addresses outside `.text`, and
  placed after the points: letting it answer first cost 5–11 % on the
  prometheus patch release either way.
- the **section geometry** (`delta.GoSectionGeometry`): the relocation plan
  carries a flag instead of its two section maps (76 B instead of 284 B on
  the synthetic pair).
- the **code model** as the `.text` base when there are no equivalences:
  the module predicts `.text` (segment maps make it better than the
  structural copy on a minor release, 1.78 MB wrong against 2.23 MB) and
  the per-function choice refines it with the structural prediction. This
  is where the minor release's gain comes from: 1,371 KB → 1,274 KB.

**Equivalences are optional.** With the derived map and the code model in
place, Zucchini's equivalence stream is worth its cost only when the change
is tiny (one-line synthetic: 1,844 B with, 2,444 B without). On a real
release it is not (prometheus patch: 165,840 B with, 72,192 B without; the
stream alone is 94.9 KB). The harness takes `-no-equivalences`; a codec
would encode both and keep the smaller, as the Go codec already does for
its correction shape.

**PIE.** Go PIE binaries are ET_DYN with `.data.rel.ro*` sections and a
`.rela` whose GLOB_DAT entries precede the RELATIVE block (ld and lld sort
them after it). `gobin` accepts ET_DYN, the data maps cover the
`.data.rel.ro*` names, and the relocation plan carries a head count. The
synthetic PIE pair patches to 1,828 B against 1,844 B for its non-PIE twin;
the Go codec alone handles it too now (1,277 B, where it used to fall back
to the plain delta at 72,681 B).

**Self-prediction gate.** `TestGoTablesSelfPredict` (corpus-gated) applies
the module to a binary against itself: every written section byte-exact,
the derived map and address prior identities.

**Follow-up (2026-08-29).** The module lays down the file's holes and
headers as the codec does (`predictHoles`, `predictHeaders`), so the
no-equivalence path starts from the old file's leading bytes rather than
zeros; headers are recomputed from the layout's section table (DESIGN.md
§3.2.2); and the codec's transform 3 lets a resized function's segment
map borrow code from anywhere in old `.text` (far pieces, DESIGN.md
§3.2.1), which the module inherits through `maxTransform`. Synthetic pair,
no equivalences: 2,356 → 1,536 xz, joint brotli 1,314 against the codec's
1,184. Numbers and the DWARF findings in `go-module-results.md`.

### 4.5 Built-in module: DWARF

Status: (a) built in the codec (`delta/debugz.go`, container flag
`debugz`; `bench/dwarfz` is its CLI) and measured end to end on shipped
files; (b) built in the harness (`bench/elfpredict/dwarf.go`), not yet
ported to the codec.
Research: `dwarf-research.md`; measurements: `go-module-results.md` "DWARF
builds". The problem it answers: a default `go build` keeps DWARF — 13 MB
of zlib-compressed sections on the 43 MB synthetic, 75 MB plaintext in
26 MB on prometheus — and on a one-line change every byte-level tool, this
codec included, ships ~9.3 MB, because a recompressed stream whose input
changed anywhere is a different stream throughout.

**Two layers.**

*(a) Transparent decompression* is a core region transform, not a Go
concern: any `SHF_COMPRESSED` section (and `.zdebug_*`) becomes a child
region holding the decompressed bytes, whose parent plan is
`recompress(codec, child)` (§5.3). Every existing layer then works on the
plaintext. `codec` names a pinned, versioned encoder from a registry —
`go-flate-1@go1.27` today; Go 1.27 replaced flate's fast encoders and a
1.25-linked binary recompresses under no 1.27 level, so the id carries the
toolchain version read from the binary's build info, and the encoder stores
the id that round-tripped, never an assumption. cgo builds are compressed by
GNU ld (`zlib-6`), which Go's flate matches at no level; those take the
fallback ladder — preflate-style reconstruction, then a Puffin-style
Huffman-only re-encode (deterministic, pure Go, ~320 B per 32 KB block),
then opaque bytes. A mismatch is a size cost, never a correctness one: the
region hash (§4.3) catches it before a byte ships. Verified
(`bench/dwarfprobe`): `zlib.BestSpeed` reproduces every section byte for
byte on the synthetic and on the prometheus build. `bench/dwarfz plain`
expands every `SHF_COMPRESSED` section in place (payload replaces header
plus stream, flag cleared, `sh_addralign` restored from the `Elf64_Chdr`,
later sections and the section header table shifted with every
inter-section gap kept) and `dwarfz pack` inverts it, taking the set of
sections to compress from the old file the decoder already holds, so the
transform ships nothing — not even the codec id, until a second encoder
joins the registry. Verified `pack(plain(f)) == f` on every unstripped
build and `pack(plain(new), old) == new` on both pairs, which is the
decoder's path; the harness measures the `dwarfz plain` pair and that
number is the shipped-file number. (`objcopy --decompress-debug-sections`
is not the plaintext: it realigns sections and rewrites `.strtab`, so the
shipped file cannot be rebuilt from it.)

*(b) The DWARF layer* is a field locator over a record map, not a DWARF
model. The decoder walks the *old* `.debug_info` against the old
`.debug_abbrev`, and for every reference field projects its position and its
value into the new image and writes the projected value; the section is
otherwise copied. Fields and what projects them:

| form | count (synthetic) | projected through |
|---|---:|---|
| `DW_FORM_ref_addr` (`DW_AT_type`, `abstract_origin`) | 480,828 | `.debug_info`'s own record map |
| `DW_FORM_sec_offset` (`location`, `ranges`, `stmt_list`, `addr_base`) | 305,740 | the named section's record map |
| `DW_FORM_addr` (CU/lexical-block pc, `DW_AT_go_runtime_type`) | 17,902 | the address oracle (function map, then section geometry) |
| `DW_FORM_addrx` | 32,610 | unchanged: an index; `.debug_addr` carries the address |

The same walk-and-project handles `.debug_addr` entries, `.debug_frame` FDE
addresses and sizes, `DW_LNE_set_address` in every `.debug_line` program,
and `.symtab` values and sizes (sizes as old size plus the function map's
delta; `.strtab` is copied). The forms make this cheap for a DWARF-5-specific
reason: a subprogram's `low_pc` is `addrx` and `high_pc` a length, and Go
pads addrx indices to a fixed width, so moving or resizing functions does
not resize DIEs.

**The record map.** Each debug section is a sequence of records the decoder
can delimit from the old bytes alone: compilation units, line programs and
call-frame entries by their length prefix, location and range lists by their
end-of-list entries. The plan is one table per section: per old record, no
counterpart / counterpart (with an optional gap before it) and its length
change; new offsets follow. A compilation unit whose length changed is
*split* (table code 3) into its header and one record per DIE — a DIE's
trailing null entries belong to it — and the sub-table takes its place, so
references into the unit's tail still project. Pairing at the encoder: units
by `DW_AT_name` in order; line programs through each paired unit's
`stmt_list`; FDEs by function address through the address oracle; DIEs and
lists by key (abbreviation code and tag mixed with the name; a list's
content hash) with an anchored diff — keys unique on both sides give the
longest increasing chain, and equal keys pair in order between anchors.
Linear space: the Myers diff it replaced reached 30 GB on prometheus's
million list records. Position projection: where equivalences write into a
section they carry both the bytes and the projection (the Zucchini stream is
a finer map than any table), and the table is not shipped; elsewhere the
table places the bytes, rewrites each unit's length prefix, and projects.

**Region DAG.** `.debug_line`, `.debug_loclists`, `.debug_rnglists`,
`.debug_addr` are produced first; `.debug_info` names them as inputs because
its `sec_offset` map is their record map. All of them name `.text` through
the Go module's function map.

**Measured** (harness on the `dwarfz plain` pairs, which is the end-to-end
number for the files as shipped; details in `go-module-results.md`):
synthetic one-line pair (59 MB) 2,652 B with the equivalence stream, 2,980
with none, against Zucchini's 597,416 and bsdiff's 476,887 on the
plaintext (9,397,472 as shipped; 1,458,732 before the layer); prometheus
3.13.1 → 3.13.2 default build (181 MB) 650,708 with the stream kept out of
`.text`, 2,325,208 with none, against Zucchini's 5,622,564 and bsdiff's
4,832,993 on the plaintext (28,963,240 / 29,004,245 on the compressed
files as shipped). Two harness bugs the
unstripped pairs exposed, both fixed in `delta` so the codec has them too:
the Go module's tail copy (`.shstrtab` aligned at the end of the file) was
applied to the whole post-section tail, shifting 13 MB of debug sections;
and `.bss`/`.noptrbss` share a file offset, so an address in the second
projected into the first (118 KB of `.text` wrong on every unstripped pair;
1 KB on the stripped prometheus patch). Three rules the real pair taught:
positions project through the equivalence that copied them, else through a
table, else with their neighbours (Zucchini's matches stop at exactly the
fields to rewrite); fixed-row tables (`.symtab`, `.debug_addr`, `.strtab`,
`.debug_frame`) ship beside the stream and own their sections, because byte
matching aligns rows by chance once every value changes; and pairing is
never in-order name matching (383 of 1,263 units) but the anchored diff
(1,045). Caveats: DWARF 4 (Go ≤ 1.24) uses absolute `DW_FORM_addr` for
subprogram bounds and is unmeasured; a unit that changed content without
changing length is not split, and unpaired units, changed lists and line
programs are left to the correction — that is the 2.3 MB of the no-stream
path on prometheus, and the reason the stream still wins there.

## 5. Regions and plans

### 5.1 Region tree

The target is partitioned into **structural regions** — sections,
sub-objects, container members — not fixed blocks (R5). Each region names
one module (or the core's fallback) and one plan. Regions nest for
containers: a `tar` region's members are child regions with their own
modules. Constraints (R6, `adaptive-predictive-coding.md` §9d):

- Depth ≤ 4 (container → member → format → sub-structure).
- Every region declares its output length before its plan runs (R2).
- No region smaller than 64 B; smaller ones are absorbed by a neighbour.
- Regions are serialised as a sorted `(gap, length, module, plan ref)` list,
  varint-coded, grouped by module so the module id is mostly implicit.

### 5.2 Inputs and the DAG

A plan may read: the reference objects (through bounded windows), its own
side tables, and the *outputs* of regions it names as inputs. Named inputs
make the region set a DAG the decoder topologically orders; an undeclared
read is impossible (native ops receive only what the plan passes; wasm ops
have no other memory). Typical edge: the Go `text` region's function map is
an input to the `pclntab` and `type` regions, exactly as in binsync.

### 5.3 Plan language

A plan is a bounded, canonical, declarative op sequence. Core ops (present
in every decoder, versioned with the core):

| op | semantics | used for |
|---|---|---|
| `copy(ref, off, len)` | bytes from a reference | matched, unchanged content |
| `fill(byte, len)` | constant run | padding, zeroed slots for unmatched functions |
| `map(runs[])` | define a piecewise-constant offset map `old → new` (run-length `(start, Δ)` pairs); pointer-bearing runs constrained to their alignment | layout shift of code and data (binsync's data maps; Zucchini's equivalence map; Percival's block alignment output) |
| `relocate(isa, src, map…)` | copy `src` and rewrite every reference form the ISA defines (x86-64 rel32/rip-rel; arm64 ADRP+ADD/LDR pairs, B/BL; Thumb-2 split immediates) through the named maps, with majority-vote consensus per target symbol | code sections of any native format |
| `rewrite(ptrs[], width, map)` | rewrite absolute pointers at listed or map-derived positions | `.data`, relocation-described pointers, GOT |
| `table(kind, entries)` | serialise a sorted `(key, payload)` table deterministically in the named layout | offset tables: `.eh_frame_hdr`, `.pdata`, `.reloc`, RELR, function-starts, DEX id tables, wasm section sizes |
| `lz(ref, ops[])` | (literal, copy, seek) stream against a reference | length-changing residuals (binsync stage 1) |
| `refs(kind, positions[], targets[])` | Zucchini-style typed references inside a matched region, corrected by delta | layouts that are not functions of the old file (R9) |
| `recompress(codec, raw, correction)` | re-encode `raw` with the core's reference encoder and apply a correction to reach the bit-exact original (preflate's contract: any stream round-trips, a well-modelled one at ~0.01–1 % overhead); zstd/brotli likewise, never by pinning an external encoder version | compressed members in containers (R12) |
| `child(region)` | splice the output of a child region | container members |

Module ops extend this table under a module id (e.g. `go:pclntab`,
`elf:eh_frame_hdr`, `pe:pdata`, `safetensors:header`). Each declares a
lowering (§5.4). Ops are executed by the core's native interpreter; a
module op runs natively or in wasm, reading only its inputs.

Every op has a declared output length; the interpreter enforces it and the
expansion bound, so a malicious plan is bounded in memory and time before
any byte is produced (`adaptive-predictive-coding.md` §9d, xz's rule).

### 5.4 Lowering

Every module op must be expressible as core ops plus residual bytes: at
worst `copy` + `fill` and a larger correction. The encoder lowers an op when
the deployed decoder (known from the old binary's build info, as go-binsync
already reads it) cannot run it. Lowering is the generalisation of
go-binsync's "publisher picks the transform the deployed decoder can read"
and guarantees that a v1 core decoder can read every future patch, at a
measurable cost in bytes. The encoder reports the cost so a fleet operator
can see what shipping the module would save.

## 6. Residual coding

### 6.1 Positional correction (length-exact regions)

binsync's stage-2 coder, generalised with Percival's difference modes. Per
differing region (maximal runs, merged when < 6 B apart):

```
gap  span<<2 | mode
mode 0  literals
mode 1  local match: (lit, copy, seek) triples over the prediction's own
        bytes from region start to min(256, end-of-file) past its end
mode 2  multiprecision difference, little-endian balanced digits
mode 3  multiprecision difference, big-endian balanced digits
```

Modes 2–3 are new: a mispredicted rel32 costs one or two small bytes
instead of four literals (thesis §2.7). The encoder tries all modes per
region and keeps the smallest. Applied in place over the prediction buffer
(the match window runs forward into untouched bytes), so the decoder holds
references + one output buffer.

### 6.2 Shifted delta (length-declared regions)

The `lz` op against the prediction, for modules whose prediction is
padded/truncated rather than exact (binsync stage 1a/1b). The module's
`length: declared` property selects this coder automatically.

### 6.3 Typed sub-streams and the terminal stage

All regions' residuals go to **four framework-fixed sub-streams** — control
varints, literal bytes, pointer/operand differences, table payloads — each
compressed as one stream per 8 MiB frame with the smaller of zstd and
brotli (binsync D16; quality 11 ≤ 4 MiB, 10 above). Modules may apply a
declared pre-transform to their residual (xor against prediction, integer
delta, byte-plane split for fixed-width records — the weights module's
exponent/mantissa split is this) but not their own entropy coder (R6).
Coder state is not reset at region boundaries.

### 6.4 Selection

Per region, the encoder chooses among candidate modules by measured size,
in three tiers, stopping at the first that decides
(`adaptive-predictive-coding.md` §9e):

1. **Metadata rules**: probe results, section type, magic. Rules out most
   modules at zero cost and sets the prior for the selector code.
2. **Sampled proxy**: run survivors on a few scattered sub-ranges (≈ 1 %)
   and count mispredicted bytes; drop candidates > 2× worse than the
   leader. Never decides for non-local (LZ-style) modules.
3. **Full trial** of the top one or two: materialise, code the residual,
   measure the marginal size under the shared coder.

Plus: hysteresis (a module switch must beat the neighbour's module by more
than the selector entry costs); cheap-first branch-and-bound (`copy` with
zero residual ends the search); and the proxy-vs-actual accuracy is
recorded on the corpus and published, as BtrBlocks did.

Cost = selector entry + side tables + marginal residual. This is the Roaring
rule without Roaring's closed forms: sizes are measured, not modelled.

## 7. Verification

The patch header carries `H_refs[]`, `H_modules[]`, `H_pred` and `H_target`
(BLAKE3-256), plus the declared memory and fuel budgets. `H_pred` is a
**tree hash over 8 MiB chunks** of the prediction so a divergence is
localised to a chunk (which region, which module) and prediction can be
verified while streaming. Failure classes are named:

| failure | meaning | action |
|---|---|---|
| reference hash mismatch | wrong old object | fetch blob / other reference |
| module hash mismatch | different module version than the encoder used | fetch the named module or fail |
| prediction hash mismatch | encoder and decoder disagree — the determinism bug class | named error `ErrPredictionDiverged` with the chunk; fall back to blob; report |
| target hash mismatch | residual corrupt or wrong | fail |

Every plan op is bounds-checked against declared lengths; allocation is
bounded by `H_target`'s declared size and the header budget. As in
go-binsync, a malformed patch fails verification and cannot write outside
the output.

### 7.5 Reserved: syndrome correction

A correction mode carrying a Reed–Solomon syndrome of the target instead of
a positional residual (thesis ch. 3). It costs ≈ 2× per differing byte plus
a log factor but does not name *which* reference it applies to, so one
patch serves near-identical variants (locally rebuilt binaries, several
build flavours). Reserved a mode id; not in v1.

## 8. Portable module profile

Derived from `portable-predictors.md` §6.2 and the probe:

- Core wasm (`lime1`-class: MVP + bulk memory + sign-ext + non-trapping
  conversions + multi-value) plus optional `simd128`. **No `f32`/`f64`
  anywhere** — validated on the raw binary, so NaN canonicalisation is never
  needed and wazero's lack of it does not matter.
- No imports except one host-provided `read(ref, off, len) → ptr` into a
  bounded window and `abort`. No WASI, threads, atomics, relaxed SIMD,
  memory64, shared memory.
- `memory.max` ≤ the budget declared in the module manifest and echoed in
  the patch header; a failed `memory.grow` is a module failure, not a
  host OOM.
- Fuel instrumented at *publish* time by a wasm→wasm transform (the
  NEAR/finite-wasm pattern) so termination is engine-independent; engines
  with native fuel may use it as a second limit.
- Explicit stack-height instrumentation, because stack-overflow points
  differ across engines.
- Admission validator runs before instantiation; the module id is the hash
  of the admitted binary.
- Runtime in the Go decoder: wazero, compiler backend on linux/darwin
  amd64/arm64, interpreter elsewhere (correctness path only, R3). Optional
  wasmtime backend for publishers.

Because ops are O(metadata), the measured 2.6× AOT penalty applies to
seconds of parsing, not to the byte-copying, which the core does natively.
The first engineering milestone is to *measure* that claim by porting the
Go pclntab walk to a wasm op (§10).

## 9. Domains, ranked

Score = expected gain over the best generic tool × how common the
"same object, many targets, updated often" shape is × 1/effort. From the
domain notes:

| rank | domain | metadata a predictor uses | expected gain (small changes) | status of evidence |
|---|---|---|---|---|
| 1 | **Go binaries** (existing) | pclntab, moduledata, `.go.type` | 28–67× vs bsdiff, measured | done; port to module form |
| 2 | **ELF C/C++/Rust PIE** | `.eh_frame_hdr` boundaries (95 % of functions in a Rust sample), `.rela.dyn`/RELR complete pointer map, `.dynsym`; encoder-side `.symtab` for the match | same class as Go for code churn, lower ceiling (fewer tables to regenerate); *estimate* | `domain-executables.md` §2, §6.3 |
| 3 | **PE x64** | `.pdata` boundaries with sizes, `.reloc`, CFG, imports/exports | must beat MSDelta (disassembly + pdata aware), not bsdiff | `domain-executables.md` §2 |
| 4 | **Container layers / packages** | gzip/zstd frame → tar (pairing by path, predicted headers) → members dispatched to formats 1–3; a `recompress` correction for Go's deflate | Go-service image patch release ≈ 0.1–0.5 MB vs 2–10 MB bsdiff on the raw layer vs 10–30 MB pull (*estimate*); Python/Java images 1.5–6× like Play file-by-file | `domain-containers-packages.md` §7 |
| 5 | **Mach-O arm64 → Linux arm64** | `LC_FUNCTION_STARTS`, chained fixups; unlocks the AArch64 relocator (ADRP pairing, which Zucchini lacks) for Graviton/Ampere fleets | high effort, high reach | `domain-executables.md` §6.3 |
| 6 | **Model weights** | safetensors/GGUF tensor map; alignment by name/shape/dtype across re-sharding | ~1.5× standalone, ~2× vs what hubs store for fine-tunes, 100× only for sparse syncs; throughput-bound | `domain-ai-weights.md` §7 |
| 7 | Cortex-M firmware, DEX, wasm modules | boundaries via `--emit-relocs` / typed id tables / unambiguous decode | modest or already served | `domain-executables.md` §6.3 |

Honest ceiling everywhere: gain is bounded by (second-order churn) /
(first-order change). Major releases, data-dominated artefacts and
address-free formats stay within a small factor of bsdiff regardless of
module quality; the fleet-update shape — one artefact, many targets, many
small steps — is where every domain's number is large.

## 10. Milestones

1. **Core extraction.** *Built* (`presage/`, `presage-core.md`): the
   container with multiple references, regions, prediction chunk hashes,
   the module registry with lowering, `lz`/`copy` core modules and the Go
   module as one coarse op over go-binsync's transform (open question 2
   answered "coarse first"; `map`/`relocate`/`go:pclntab` as separate ops
   are deferred to the module that needs them). Exit met: the corpus gate
   (`presage/gomod/corpus_test.go`) holds every pair within 2 % of
   `delta.Encode` and the self-prediction gate green; prometheus 3.13.1 →
   3.13.2 is 74,135 vs 74,126; the unstripped pair 8,712,789 vs 8,714,361.
   Corrector modes 2–3 are still open (the `.text` residual is 63 KB;
   *estimate* 10–20 % of that recoverable).
2. **Portable path.** Build the `go:pclntab` op to wasm from the same
   source; run under wazero compiler and interpreter on the 30 MB and 94 MB
   pairs; publish ns/function. Exit: identical `H_pred` native vs wasm on
   the corpus; AOT decode within 3× native.
3. **ELF module.** Matcher (masked-operand hash + encoder-side symbols),
   `.eh_frame_hdr`/RELR/`.dynsym` regenerators, reuse of the x86-64
   relocator. Corpus: a Rust and a C++ server binary across three point
   releases each. Exit: measured vs bsdiff, hdiffz, Zucchini.
4. **Container module.** tar + gzip/zstd with `recompress`; members
   dispatched to modules 1–3. Exit: an OCI layer rebuild with one changed Go
   binary costs within 1.5× of the binary's own patch.
5. **Lowering and selection.** The three-tier selector, hysteresis, and
   lowering against a decoder capability set; publish proxy accuracy.
6. **Second tier.** PE x64; safetensors/GGUF; arm64 relocator.

## 11. Decisions

| # | Decision | Reason |
|---|---|---|
| G1 | Modules emit plans; the core materialises bytes | 100× interpreter / 2.6× AOT penalty on byte work (R3); Courgette's "adjustment" shape |
| G2 | Restricted wasm, no floats, no imports, publish-time fuel | only substrate with spec'd determinism, toolchains, and a pure-Go runtime; ZPAQL lacks calls/fuel, eBPF cannot loop/allocate, Lua unsafe (R4) |
| G3 | Modules referenced by hash, never inlined in a patch | RFC 9842 / Shared Brotli precedent; module ≈ patch size; cache once |
| G4 | Every module op has a lowering to core ops | a v1 decoder reads every future patch; capability negotiation becomes a byte cost, not a failure |
| G5 | Hard per-region selection over structural regions, encoder-side | switching-cost theory and every fast system in the survey (R5) |
| G6 | One terminal residual coder with four typed sub-streams; modules only pre-transform | shared statistics (R6); binsync's 6–7 % from stream choice, 0.1 % from stream splitting |
| G7 | Function/tensor correspondence is shipped, not recovered | encoder has both unstripped artefacts (R7); decoder stays O(metadata) |
| G8 | Prediction hash is a chunked tree hash | localises divergence to a region/module; enables streaming verification |
| G9 | Length-exact vs length-declared is a module property that selects the residual coder | D17 in binsync: 66 KB vs 17 KB when the wrong coder is used |
| G10 | Zucchini-style `refs` op kept under the layout predictor | layout is not always a function of the old file (R9) |
| G11 | Weights are a second-tier module and not a headline claim | 1.5×/2× measured ceilings (R10) |
| G12 | Syndrome correction reserved, not built | 7× cost pairwise on Percival's corpus; attractive only after prediction (R11) |

## 12. Open questions

1. Does the plan-emitting ABI hold for every format, or does some
   regenerator need O(bytes) work inside the module (candidate: deflate
   recompression, which is O(bytes) by nature and must therefore be a
   *core* op with a pinned encoder)?
2. Module op granularity: one op per table (`elf:eh_frame_hdr`) or one per
   format (`elf:*`)? Finer ops lower better; coarser ops share parsing.
3. The map consensus rule (majority vote per target symbol) is in the core
   `relocate` op; is one rule enough across ISAs and formats, or does it
   need to be a module-supplied policy?
4. Memory ceiling: binsync's decoder is 7.6× the input; the region DAG lets
   the core drop a region's inputs once its dependants have run, but the
   target ≈ 2× has not been demonstrated.
5. Multi-reference selection: with several references, does the encoder
   choose one per region, or may `copy` address any of them (cheap in the
   format, costly in encoder search)?
6. Recompression is O(bytes) by nature and so must be a core op (§5.3), and
   the client CPU it costs is the universal complaint in the package-delta
   literature (Play: 1 s/MB). The container module needs a decode-cost
   budget and per-member fallback to raw, and the first measurement is
   preflate-class overhead on Go-gzip output — unpublished anywhere.
