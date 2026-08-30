# OpenZL: what a format-aware *compressor* framework does and does not give a format-aware *delta* codec

Assessed 2026-08-28 against OpenZL at `8cbe8c5` (Meta, BSD-3, C11/C++17), its
[whitepaper](https://arxiv.org/abs/2510.03203) and
[announcement](https://engineering.fb.com/2025/10/06/developer-tools/openzl-open-source-format-aware-compression-framework/).
Every citation below was opened; every number was measured here or is marked
*theirs*.

OpenZL and presage share a slogan — "understand the format, then compress" —
and almost nothing else. OpenZL turns **one** object into typed streams and
entropy-codes them. presage predicts a target **from a reference** and codes
the correction. The slogan overlap is real but shallow; the interesting
material is in three structural facts about OpenZL that bear directly on
decisions already made in [`../SPEC.md`](../SPEC.md), and in one measured
transform we have not tried.

**Bottom line.** Not a dependency, not a backend, not a competitor on this
repo's domain. It is (a) the strongest shipped precedent for two decisions the
SPEC already made on weaker evidence, (b) a source of one transform worth a
measured 26,788 XZ on the Chrome plan, and (c) the reason to reprice G6 and to
concede the zero-reference half of domain rank 6.

---

## 1. The three facts that decide everything

### 1.1 No codec can read a reference

This is the boundary, and it is enforced, not merely absent:

```c
int DT_isNbRegensCompatible(const DTransform* dt, size_t nbRegens)
{
    if (dt->miGraphDesc.lastInputIsVariable) return nbRegens >= dt->miGraphDesc.nbInputs - 1;
    return nbRegens == dt->miGraphDesc.nbInputs;      // decompress/dtransforms.c:144-151
}
```

with the comment two functions later: *"In the encoder direction, a `regen` is
an `input`"* (`dtransforms.c:156-158`). OpenZL codecs are multi-input — up to
2048 typed inputs per frame (`common/limits.c:4-9`) — but **every input is a
decoder output**. A hypothetical `(reference, target) → residual` codec would
produce an archive that regenerates the reference too. The rule is stated
plainly in the public header: the inputs *"will be regenerated together in the
same order at decompression time"* (`include/openzl/zl_compress.h:335-343`).

The one channel for bytes the frame does not reproduce is the materialized
dictionary, added in format v25 (post-paper): content plus a 256-bit ID, with a
"materializing codec" (`dict/dict.h:14-26`). Exactly one standard codec has a
materializer registered:

```c
DictLoader_registerStandardMaterializer(loader, (ZL_NodeID){ZL_StandardTransformID_zstd},
                                        &ZL_Zstd_ddict_materializer);   // dict/dictloader.c:267-278
```

So the only reference-bearing path OpenZL offers is *a zstd dictionary*. Fed
the old binary, that is `zstd --patch-from`, which this repo measures at 538 KB
on the one-liner and 8.5 MB on prometheus (`../../../README.md` §1.1) against
go-binsync's 2,262 B and 95,366 B. The delta problem is not addressed, and the
architecture does not have a seam where it could be without changing the
decoder contract.

### 1.2 The graph is not in the frame — an execution trace is

The claim that makes OpenZL interesting ("one universal decompressor") is true,
but the mechanism is narrower than the marketing implies. What ships per chunk
is a **Decoding Map** (`common/wire_format.h:86-97`): per decoder, a type flag,
an ID, a private-header size, an output arity, and *relative stream distances*.
Topology is reconstructed positionally:

```c
size_t const outputStreamIdx = inputEndIdx + node->regenDistances[n];  // decompress2.c:827-829
```

No graph, no selectors, and **no parameters**: `grep -c localParams
src/openzl/compress/encode_frameheader.c` → `0`. Anything the decoder needs is
re-emitted by the codec as a short out-of-band header
(`zl_ctransform.h:528-575`, 30 call sites).

Two consequences. First, this is the same shape as presage's plan (§5.3) with a
narrower op vocabulary, and OpenZL *transposes and bitpacks its own decoding
map* — decoder-type flags at 1 bit, standard IDs bitpacked, header sizes as a
zero/non-zero bitmap plus varints (`wire_format.h:98-124`). Applying the
codec's own philosophy to its own metadata is a trick worth stealing (§4.4).

Second, universality is bought by **forbidding** what presage's G4 makes cheap.
A custom codec is a hard decode failure, not a byte cost — the docs say so
outright (*"can only be decompressed by registering the same custom codec(s)"*,
`getting-started/concepts.md:26-33`), and there is no version gate on custom
transform IDs (`dtransforms.c:626-627`). OpenZL's answer to capability
negotiation is "don't need one"; presage's is "price it". These are opposite
trades on the same problem and both are defensible. The SPEC should say so.

### 1.3 SDDL never runs on a decoder

SDDL — the format-description language, with a real stack VM, bytecode and
disassembler — lives **entirely** under `src/openzl/compress/graphs/sddl2/`.
There is no SDDL in `src/openzl/decompress/`, no SDDL entry in
`ZL_StandardTransformID` (`wire_format.h:222-320`), and no SDDL under
`src/openzl/codecs/`. The parse is *always lowered*: it materialises as
parameterised `dispatch`/`splitN` invocations whose boundaries the decoder
inverts without knowing what a format is. The VM is loop-free by construction
(forward-only `jump_if`, `sddl2_vm.c:803-815`; the CALL family is
unimplemented, `sddl2_interpreter.c:537-539`) and bounded by three constants
(`common/limits.h:104-110`).

This is the single most useful thing in the codebase for presage, because it is
an industrial existence proof of G1 + G4: **a format model can be entirely
encoder-side, and its output can be a bounded declarative plan a dumb decoder
replays.** OpenZL's own PyTorch domain module is the same pattern with no
custom decoder at all — segmenter plus function graph over standard nodes
(`custom_parsers/pytorch_model_parser.c:289-315`; no `registerCustom*` anywhere
under `custom_parsers/`).

It also sharpens what the portable-module tier in §8 is *for*. SDDL covers
probe / region-tree / typing. It cannot express `relocate`, `map`, `table` or
any regeneration — the ops that need decode-time format knowledge. Whatever
survives of the wasm profile, it is not the parser; it is the regenerators.

---

## 2. What we measured

| measurement | result |
|---|---|
| OpenZL generic graph vs the coders this repo ships, on 16 MiB of prometheus 3.13.2 `.stripped` | openzl `4,396,658` · zstd -19 `4,396,750` · brotli -q11 `4,160,872` · xz -9e `4,136,260` |
| Decompress-only static link (`ZL_decompress` + registries, `-Os`, stripped) | **1,517,936 B** over a 14,464 B baseline |
| `sparse_num` on the eight structural-plan columns, marginal in the combined plan's one XZ stream | `841,596 → 814,488`, **−27,108 XZ** |
| Every OpenZL basis on the three equivalence-plan columns | **all lose**; best alternative is +104 B |

The first is decisive and reproducible: on unstructured executable bytes
**OpenZL is zstd** (92 bytes apart, 0.002%) and is 5.9% behind xz and 5.4%
behind brotli — the coders binsync already uses (`internal/cz`, `docs/go-module-design.md`
§D16). There is no generic-backend upgrade here; adopting OpenZL as a terminal
stage would *cost* 5–6% before any re-basis gain, because `deps/` is
`googletest, lz4, xgboost, zstd` — no brotli, no LZMA.

The third is the one genuine transform import, and §4.1 takes it up. The fourth
bounds it: the vocabulary pays only where a column is already dominated by one
value, and nowhere else.

---

## 3. What OpenZL corroborates, and what it reprices

### 3.1 Corroborates G1/G4 (encoder-side models, always lowerable)

§1.3. Add OpenZL to the "the stream carries its decoder" table in
[`portable-predictors.md`](portable-predictors.md) §4 — it is currently absent
from this entire repo, and it is the strongest shipped datapoint for the
*closed op set, no VM in the decoder* row. It is a sharper instance than the
Kaitai one already there on two axes: loop-free by construction rather than by
discipline, and it survives contact with real formats (Parquet, PyTorch,
genomics) while still being always-lowered.

### 3.2 Repricess G6 — and the contradiction is in-tree, not in OpenZL

G6 fixes four sub-streams on the evidence that "splitting statistics costs 2.5×
on small streams" and "0.1% from stream splitting". Three problems, none of
which needed OpenZL to find but all of which OpenZL's design makes obvious:

- `bench/elfpredict` already ships **seven** independently-xz'd sub-streams per
  region piece — `Gaps`, `Lens` and five run-length byte buckets
  (`correction.go:30-41`, `columnarXZ`) — across up to thirteen pieces, each
  picking its own terminal codec (`correctionCuts:119-142`,
  `bestCorrectionXZ:148-176`). That routing alone is worth 86,344 XZ on Chrome
  ([`chrome-elf-whole-image.md:611`](chrome-elf-whole-image.md)). G6 as written
  forbids a mechanism this repo has already implemented and measured.
- The 2.5× citation ([`adaptive-predictive-coding.md`](adaptive-predictive-coding.md))
  measures coding 850-byte *independent files* with no shared prior. It says
  nothing about splitting one already-collected residual into columns inside
  one frame, where the LZ window spans all of them. This is the weakest
  inference in the SPEC and should be struck rather than re-cited.
- The "0.1%" is Go-corpus-derived. The ELF domain measures materially more:
  run-length bucketing of one byte column was worth 70,920 XZ in situ
  (`chrome-elf-whole-image.md` §9.14).

OpenZL's contribution here is a **method and three constants**, not a verdict.
It does not split aggressively — it *searches* split-versus-merge and accepts
on measured compressed size, charging 3 bytes per extra partition
(`codecs/partition/encode_partition_bitpack.c:24`), requiring a min gain of
`max(32 B, 1%)` (`codecs/entropy/encode_entropy_binding.c:713-732`), and
storing outright below 64 elements (`:604-609`). Its clustering trainer merges
a pair when `splitCost / (marginal₁ + marginal₂) < 1.0`
(`tools/training/.../greedy_trainer.cpp:239-254`) — a directly transplantable
formula for the tier-3 selector of SPEC §6.4, which currently selects modules
per region but never selects the sub-stream grouping.

**Proposed edit.** Keep G6's claim A verbatim ("modules never bring their own
entropy coder"). Replace claim B: the number and identity of sub-streams is
declared per module, bounded, and chosen by a measured split/merge rule with a
size floor — not fixed at four.

### 3.3 Concedes the zero-reference half of domain rank 6

OpenZL ships the exponent/mantissa split as *standard wire codecs*
(`codecs/float_deconstruct/spec.md`, `include/openzl/codecs/zl_bitsplit.h:20-31`)
and a user-facing `zli -p pytorch` profile (`cli/utils/compress_profiles.cpp:349-354`),
in production, at roughly the entropy ceiling
[`domain-ai-weights.md`](domain-ai-weights.md) computes. SPEC §9 rank 6 should
drop "~1.5× standalone" and "safetensors/GGUF tensor alignment" from v1 scope,
and restate the row as **delta-only**: tensor-name/shape/dtype alignment across
re-sharding, plus a reference input, which no shipped system has. The
competitive axis in that row ("throughput") is also wrong — OpenZL decodes
float data below presage's own stated 2 GB/s/core bar.

More generally the right narrowing of presage's claim is to **reference-bearing**
artefacts, not address-bearing ones. That is the accurate statement of what
OpenZL cannot do in any domain.

---

## 4. Transforms and mechanisms worth taking

### 4.1 `sparse_num` — the one codec with no equivalent here, and it pays

Dominant-symbol run-distances plus literals, auto-detected from an input prefix
(`include/openzl/codecs/zl_sparse_num.h:14-44`). The elfpredict plan is now
built almost entirely of columns that are *mostly one value*: the source column
delta-coded against the destination column is 96.62% zero (§3.4), the point
shift column 99.200% zero (§9.10), the extent and start residuals nearly so.
That is precisely the shape `sparse_num` targets, and xz codes it as
zero-runs interleaved with literals in one stream rather than as two
populations.

Measured against `elfpredict-epp12/all-mapped-plan.bin`, every column
substituted and the whole plan re-coded as one XZ stream. It wins on all eight:

| column | current | `sparse_num` | Δ |
|---|---:|---:|---:|
| `srcIndexDeltas` | 125,880 | 118,032 | **−7,848** |
| `srcOffsets` | 71,788 | 61,300 | **−10,488** |
| `sizeDeltas` | 19,184 | 15,908 | −3,276 |
| `pointIndexDeltas` | 14,396 | 12,800 | −1,596 |
| `extentResiduals` | 12,660 | 11,196 | −1,464 |
| `startResiduals` | 10,476 | 9,388 | −1,088 |
| `pointShiftDeltas` | 4,464 | 4,276 | −188 |
| `pointOffsets` | 172 | 92 | −80 |
| **structure plan, in situ** | **256,164** | **228,848** | **−27,316** |
| **marginal in the combined plan** | 841,596 | 814,488 | **−27,108** |

Almost no §9.3 discount — the sparse columns share little with the equivalence
stream beside them — so the marginal is the honest figure: **−27,108 XZ, 1.01%
of the 2,678,488 whole image**, taking 49.11% to 49.63%. `tokenize`,
`range_pack` and `rle` were measured on the same columns and lose on all but
three, and never by as much.

Small, but it is the largest single unexplored item left in the plan and it
costs one transform with no decode-time walk.

### 4.1a Where it stops: the equivalence stream is immune

The 539,300-XZ equivalence stream is 20.1% of the budget and §9.8 judged it
lean on a bytes-per-entry argument. It is also immune to this whole vocabulary,
which is a stronger statement. Measured on `equivalence-plan.bin`'s three
populated columns:

| column | current | `sparse_num` | `tokenize` | `range_pack` | `transpose4` |
|---|---:|---:|---:|---:|---:|
| `SrcSkip` | 330,424 | +1,244 | +90,000 | +36,796 | +43,944 |
| `CopyLen` | 186,984 | +940 | +27,052 | +16,056 | +5,004 |
| `DstSkip` | 68,196 | +104 | +6,212 | +6,408 | +776 |

The rule the two tables together establish: **this vocabulary pays exactly when
a column is dominated by one value, and costs when it is not.** The structural
columns are 96.62% and 99.200% zero by construction (§3.4, §9.10);
`SrcSkip`'s dominant value covers 2.97% of entries and `CopyLen`'s 1.70%. The
mapped variant of the source column (`SrcResidual`, built against the function
map's predictor and not written to any cached artifact) is not measurable here,
but it costs ≈284,000 XZ over 158,544 entries — 1.79 compressed bytes each,
where a 96%-zero column of that length codes in the low tens of KB. It is not
dominated either, and the rule predicts it does not respond.

Nothing else in the ledger is a column at all: `.text` correction (47.3%),
`.rodata` (6.7%) and `.eh_frame` (2.8%) are byte-correction data, and §9.14
already measured the one transform from this family that applies to them
(transpose, `1,091,100 → 1,118,388`, lost to LZMA's period modelling).

### 4.2 `field_lz` — LZ that matches whole fields

`ZL_NODE_FIELD_LZ` matches entire fixed-width fields rather than bytes and
emits five typed streams: literals, 10-bit tokens, u32 offsets, extra literal
lengths, extra match lengths (`include/openzl/codecs/zl_field_lz.h:14-25`). It
is the one OpenZL codec with no binsync analogue whose shape fits the plan's
uint64 address columns. Untested here; worth a rung.

### 4.3 Do *not* import byte-plane split unconditionally

SPEC §6.3 lists byte-plane split as an allowed module pre-transform. It is
measured *worse* in-tree: transposing the four-byte replacement bucket into
per-position planes cost `1,091,100 → 1,118,388` XZ, because LZMA already
models the four-byte period and transposing destroys the match distances
(`chrome-elf-whole-image.md` §9.14). Every pre-transform must be gated on a
measured trial, which is exactly OpenZL's own discipline.

### 4.4 Self-apply the codec to the plan header

§1.2. presage's plan is currently one serialized blob of records with small
enumerated fields — the same shape as OpenZL's decoding map, which OpenZL
transposes and bitpacks lane by lane. Cheapest remaining plan saving after the
Chrome ledger's §9 series.

### 4.5 Wire-ID indirection and a written bump policy

OpenZL keeps `ZL_NodeID` (compile-time, `include/openzl/zl_nodes.h`) separate
from `ZL_StandardTransformID` (wire, `wire_format.h:206-320`) with a
`REGISTER_TRANSFORM` table (`codecs/encoder_registry.c:55-90`), so node IDs can
be reshuffled without breaking frames, and the wire enum is append-only. The
format version range is explicit and documented
(`include/openzl/zl_version.h:42-57`, `doc/mkdocs/doc/api/versioning/`).

binsync today has a flat transform byte and a `BSZ1` magic
(`delta/container.go:12-28`). **This is the one item with forward engineering
value and a deadline**: SPEC §3 calls the core "fixed, versioned" and nowhere
says what bumps it or what is illegal to change, and nothing is implemented
yet, so the op table can still be given a separate wire space for free. Do it
before the table is frozen.

Counter-example worth carrying: OpenZL's own serialized-compressor version
check is dead code — the entire body of
`ZL_CompressorDeserializer_checkVersion` is commented out and it
unconditionally returns success (`compress/compressor_serialization.c:3313-3336`),
while still being called at `:3523` and `:3590`. Copy the pattern, not the
implementation.

### 4.6 Per-chunk arena — a lead on open question 4

SPEC open question 4 (decoder at ≈2× the input, against binsync's measured
7.6×) is listed as unresolved. OpenZL's segmenter is a first-class object above
graphs that sets chunk boundaries, each chunk independently decodable and
flushable, default target 16 MiB (`include/openzl/zl_segmenter.h:19-76`),
implemented as a per-chunk arena freed at `decompress2.c:1775`. That is
structurally binsync's frame container (`delta/container.go:31-45`, 8 MiB,
per-frame BLAKE3) with the memory discipline attached. The convergence is worth
noting and the arena is the mechanism the open question is missing.

### 4.7 25 fuzz targets, against our zero

`tests/fuzz` has 25 targets. `grep -rln "func Fuzz" --include=*.go` over this
repo returns nothing, and SPEC mentions "malicious" once. The presage decoder
parses attacker-supplied patches inside a customer's product. Not an idea, but
the comparison is unflattering and worth recording.

---

## 5. Closed questions — do not reopen

| question | answer | evidence |
|---|---|---|
| Use OpenZL as a terminal/entropy backend? | No. It is zstd on our data and 5–6% behind brotli/xz; `deps/` has neither. | §2 |
| Express presage's prediction as an OpenZL graph? | No. `nbRegens == nbInputs`; an input is an output. | §1.1 |
| Run OpenZL's WASM build as a portable module under wazero? | No. OpenZL requires 64-bit (`shared/portability.h:270-273`) so its Emscripten build is forced to wasm64 (`CMakeLists.txt:40-45`); wazero v1.12.0 rejects limits flags outside `0x00–0x03` at binary decode (`internal/wasm/binary/limits.go:15-42`), and SPEC §8 bans memory64 anyway. |
| Link it via cgo? | No. Breaks the `CGO_ENABLED=0` premise for a 1.5 MB decoder that buys nothing on executable bytes. | §2 |
| Port its codecs? | Only `sparse_num` and possibly `field_lz` and `range_pack`, from their `spec.md` files rather than the C. Everything else is already present or already measured and rejected here. | §4 |
| Adopt its trainer? | No — it searches graph *composition* under 1%-sampled information for unseen files; presage's encoder has both artefacts and perfect information. Take the split/merge cost formula (§3.2), not the trainer. | `cli/commands/cmd_train.cpp`, `tools/training/` |
| Adopt the serialized-compressor artefact? | No. presage has no trained config, and the plan already ships in the patch. | §4.5 |

---

## 6. What this does not change

Nothing measured. Every item above is a documentation edit, a design
correction, or at most one transform worth ~1% of one experiment. The 28–67×
result, the plan architecture, the residual coders and the ranked domains all
stand.

The reason to have done the assessment anyway is [`chrome-elf-whole-image.md`
§10](chrome-elf-whole-image.md): eighteen consecutive experiments moved that
patch from 4,599,840 to 2,678,488 XZ **without changing a modelled byte** — all
of it re-basis — and then the encoding lever ran out. OpenZL is the industrial
version of exactly that search, automated over a fixed vocabulary. It arrives
one domain too late to help here, and its vocabulary is missing the two
operations that earned most of the Chrome ledger — *join on a key* and *delta
against a correlated neighbour column* — because both require a second input
and OpenZL has no second input.

That is the honest relationship. Not competitor, not component: the same
insight applied one layer down, in a system that structurally cannot reach the
layer this repo works at.
