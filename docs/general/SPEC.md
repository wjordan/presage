# presage — a general predictive codec with pluggable structure models

*Working name; provisional.* Design specification, round 1. Status: design
only, nothing implemented. Every number below is either measured in this
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

1. **Core extraction.** Lift go-binsync's container, frames, positional
   corrector, `lz` engine and compression tier into the core with the
   region tree and plan interpreter; re-express the Go codec as a module
   whose ops are `map`/`relocate(x86-64)`/`go:pclntab`/`go:type`. Exit:
   every pair in `docs/DESIGN.md` §3.2 within 2 % of its v1 size, the
   self-prediction gate green. Adds modes 2–3 to the corrector and measures
   them on the prometheus pair (the `.text` residual is 63 KB; *estimate*
   10–20 % of that recoverable).
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
