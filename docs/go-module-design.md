# presage — the Go module's design

Architecture and reasoning behind `delta/`, the Go linux/amd64 predictor that
presage's `go` module is a façade over (`docs/general/presage-core.md` §2).
Measurements are in `docs/research/` (index: `docs/research/README.md`); the
general codec this module plugs into is `docs/general/SPEC.md`.

Numbers quoted below were measured on 2026-08-26 (linux/amd64; Go 1.26.4 for
the first two research rounds, Go 1.27.0 for the codec prototype's final
pass, §2.2) unless marked *estimate*.

---

## 1. Workload and assumptions

| Assumption | Value used for design | Source |
|---|---|---|
| Binary | Go, linux/amd64, stripped (`-s -w`), exe or PIE; 30 MB typical, 100–250 MB common, up to 1 GB | user; corpus in `benchmark-scale.md` |
| Change per release | *not* one line: several lines across several packages (a normal patch release); sometimes a minor release with dependency bumps | user |

What the measurements say about that workload:

- **How much of the file changes.** A multi-package edit that grows `.text`
  by 2.3 KB (`v1→v5`, 29.6 MB) changes **70 %** of the bytes in ~1.95 M
  runs (median run 3 B); a one-line edit changes 13 %. Both are the same
  mechanism: every function after the first grown one moves by a multiple of
  32 B, and every PC-relative reference crossing a moved boundary is
  rewritten; `.gopclntab` (35 % of the file) is offset-based and is rewritten
  almost entirely. Chunk-based transfer (CDC/CAS) therefore re-sends ~90 % of
  a full download (`cdc-cas.md`, `benchmark-local.md`) and is not used.
- **Generic delta encoders.** bsdiff-class approximate matching gets a real
  patch release of an 88 MB binary (kube-apiserver 1.36.3→1.36.4) down to
  **2.06 MB** against a 19.1 MB full download (10.8 %); terraform 5.4 MB /
  25.5 MB; a minor release (prometheus 3.13→3.14) 8.6 MB / 24.3 MB; the
  multi-package synthetic `v1→v5` 470 KB / 8.4 MB. But bsdiff needs ~12× the
  input in RAM and 50–190 s for ~90 MB, and took 267 s and 3.4 GB for a
  393 MB stripped binary; hdiffz manages 46 s at 1.7 GB RSS for a 537 MB
  file (`benchmark-scale.md`).
- **The Go-aware transform (§2).** Aligning old and new by *function name*
  (from `.gopclntab`) and re-deriving the layout-induced churn removes most of
  those bytes: on Go 1.27, one-line 150,475 → 2,207 B (68×; the new sorted
  `.go.type` section makes bsdiff 2.5× worse than on 1.26 while the Go-aware
  patch is unchanged), multi-package `v1→v4` 145,205 → 2,733 B (53×), and
  prometheus 3.13.1→3.13.2 built with Go 1.27 2,691,644 → 111,552 B (24×),
  encoded, decoded and byte-verified end to end, in 2.1 s. The remaining
  bytes are mostly genuinely changed code, plus new type descriptors and the
  pc tables of changed functions (`go-aware-transform.md` §11).

Design consequences: patch bytes are the product, so the codec is where the
effort goes; and the encoder must scale to 1 GB inputs in seconds, not
minutes, without a whole-file suffix array.

## 2. Codec: predict-then-correct

### 2.1 Principle

A patch has two parts:

1. **Prediction inputs** — a compact description of the new binary's
   *layout* (function order and sizes; where data blocks moved), from which
   the decoder rebuilds, deterministically, a *predicted* new binary out of
   the old one: every function relocated to its new address with every
   PC-relative operand re-targeted, every offset-based table regenerated.
2. **Correction** — the difference between the prediction and the real new
   binary, encoded positionally and compressed.

The prediction only has to be deterministic, not correct: an imperfect
prediction costs bytes in the correction, never correctness, because the
output is verified against the release hash before use. This is the
Courgette/Zucchini idea, but keyed on Go's own metadata rather than on
disassembly heuristics: `.gopclntab` survives `-s -w` and names every
function with its entry offset and size, which gives an exact
function-by-function correspondence between releases and the exact new
layout. Nothing in the pipeline needs symbols, DWARF or relocations.

### 2.2 Pipeline

Encoder and decoder run the same prediction; the encoder additionally has the
real new file and emits the correction.

```
parse(old)            ELF sections; pclntab (funcs: name, entry, size, pc tables); moduledata
                      ─ unsupported (non-Go, non-amd64, not the supported Go release — D14): plain codec §2.8
match(old, new)       new function j ↔ old function i by name (exact; then normalised for
                      closure/deferwrap/generic-instantiation numbering; then by content hash)
layout table          for each new function in order: {same-as-next-old | old index | new name}, Δsize
data maps             per data section (.rodata .noptrdata .data): piecewise-constant shift of old
                      16-byte blocks to their new offsets (run-length: (block, Δ) pairs);
                      blocks containing pointers may only shift by multiples of 8
shift tables          .bss/.noptrbss: (old offset, Δ) runs derived from symbol-order deltas
predict .text         for each new function: copy the matched old body to its new entry;
                      decode x86 (x/arch/x86asm); rewrite every rip-relative / rel32 operand
                      through {function map, data maps, shift tables}; unmatched → zero-filled slot
predict .gopclntab    regenerate functab (entryOff), findfunctab, _func.entryOff, and re-base
                      nameOff/pcdata/funcdata offsets after applying stage-1 deltas to the
                      variable-length blobs (funcnametab, cutab, filetab, pctab, gofunc)
predict data          re-lay blocks through the data maps; rewrite absolute pointers into
                      .text/.rodata/.data/.bss through the same maps
predict type descs    walk the descriptors and itabs from moduledata (Go 1.27: `.go.type`,
                      `typedesclen`/`itaboffset`); rewrite nameOff/typeOff/textOff/ptrToThis and
                      method tables through the same maps — nothing extra is transmitted
correction            new ⊖ predicted, positionally; inside each differing region an exact
                      local match over the prediction's own bytes there; brotli/zstd (§2.5);
                      body split into 8 MiB frames
```

Every Go-aware row was encoded to a patch file, decoded from the old binary
and byte-verified; layout tables, data maps and stage-1 blobs are transmitted
and counted (nothing is an oracle input). The prototype column is
`bench/gotransform/` (`go-aware-transform.md` §10–11); **v1** is this
repository's `delta` package, including its patch header and frame table:

| Pair (toolchain) | bsdiff | hdiffz -p-8 | prototype | v1 | **presage** | vs bsdiff |
|---|---:|---:|---:|---:|---:|---:|
| one-line (`v1→v2c`, 29.6 MB, Go 1.27) | 150,475 | 176,929 | 2,207 | 2,262 | **1,129** | 133× |
| +3-byte string (`v1→v2l`, 1.27) | 24,874 | 33,713 | 566 | 438 | **459** | 54× |
| multi-package (`v1→v4`, 1.27) | 145,205 | 171,760 | 2,733 | 2,745 | **1,635** | 89× |
| `v3→v4` (1.27) | 30,196 | 40,523 | 578 | 440 | **479** | 63× |
| prometheus 3.13.1→3.13.2, built with Go 1.27 (94 MB) | 2,691,644 | 2,719,152 | 111,552 | 95,366 | **69,933** | **38×** |
| prometheus 3.13.1→3.13.2, default build with DWARF (181 MB) | 4,832,993 | — | — | 8,714,361 | **330,557** | 15× |

The **presage** column is what the shipped CLI writes: the same transform,
driven as one module of the presage codec (`docs/general/presage-core.md`),
with the segment maps, far pieces, pointer consensus and the header prediction of
§2.2–§2.4 landed after v1, and for a default build the DWARF field layer
(§2.5, `presage/dwarf`). The two smallest pairs are above v1 by the presage
container's ~40 B — a region record, a frame table and the prediction's
32-byte hash — which the per-pair corpus gate accepts (within 2 % + 64 B of
`delta.Encode`). 3,982 B of the real pair's presage figure is the modal
correction (`docs/general/SPEC.md` §6.1): the same encoder without it writes
74,177 B, and 1,101 B of the DWARF row is the same lever. The v1 column and
the discussion below are the history of how the transform got there.

The real pair is 14 % below the prototype and within 1 % of the 94,470 B that
an hdiffz stage 2 reached (the range §2.4 was aiming for), from an encoder
with no suffix array and a decoder that applies in place. The synthetic pairs
move both ways: the two large ones are tens of bytes above the prototype,
paying for the container (a 120-byte header where the prototype wrote none)
and for the second stage-1 blob's floor, and the two small ones are ~23 %
below it, where a patch that is mostly floor gains most from brotli
(§2.5) and from predicting the unallocated bytes (§7).

The official Go 1.26 builds (prometheus 3.13.1→3.13.2: 291,214 B, 9.3×;
kube-apiserver 1.36.3→1.36.4: 292,972 B, 7.0×) were measured with the
previous codec revision — before pc-table regeneration and pointer consensus —
and were not re-run; the codec targets Go 1.27 only (D14).

Earlier oracle-map pass (`go-aware-transform.md` §7.5; maps not transmitted, no decoder, now
superseded): terraform 1.15.8→1.15.9 (Go 1.25) 5,427,575 → 1,990,549 (2.7×);
cockroach 26.2.4→26.2.5 (Go 1.25, cgo) 8,588,309 → 4,190,756 (2.0×);
prometheus 3.13.2→3.14.0 minor release (hdiffz) 8,599,007 → 5,416,050 (1.6×
— 6,326 new functions; content-dominated, as it should be).

Go 1.27 moved type descriptors and itabs out of `.rodata` into a sorted
`.go.type` section full of `textOff` fields, which makes generic deltas 2.5×
*worse* on 1.27 for the same change (bsdiff 60,478 → 150,475 B for the
one-liner) and leaves the Go-aware patch unchanged. Three regenerations took
the real pair from ~7× to 24×: the type-descriptor offsets
(nameOff/typeOff/textOff, walking the descriptors from `moduledata`; without
it prometheus/1.27 is 960,168 B), the pc tables (`pctab`, `cutab`,
`go:func.*` are emulated from the new function layout instead of being sent
as a generic delta — they were 100,949 B, 42 % of the patch), and pointer
shifts chosen by majority vote among all pointers into the same symbol
(which fixed a 5-byte-off shift into the short, repetitive `runtime.gcbits`
data that had held `v3→v4` at 4.7×).

What is left, on v1's prometheus/1.27 patch. By stream (raw → compressed on
its own; the patch ships them in one frame, which is why the parts come to
95,232 B and the file to 95,366 B):

| stream | raw | compressed |
|---|---:|---:|
| header | – | 120 |
| layout | 239,371 | 10,379 |
| stage 1a (`funcnametab`+`filetab`) | 2,205 | 1,360 |
| stage 1b (`cutab`+`pctab`+`go:func.*`) | 38,216 | 17,441 |
| stage 2 (whole file) | 119,493 | 66,052 |

And by where the prediction was wrong — 120,852 bytes of a 93.8 MB file,
0.13 %:

| section | mispredicted |
|---|---:|
| `.text` | 63,179 |
| `.go.type` | 45,170 |
| `.gopclntab` | 4,549 |
| `.rodata` | 3,782 |
| `.go.func` | 2,568 |
| `.data`, `.noptrdata`, `.go.buildinfo`, `.go.module`, `.go.fipsinfo` | 1,495 |
| ELF program and section headers | 109 |

`.text` and `.go.type` are 90 % of it and mostly real change: the 52 new
functions and the descriptors that came with them. The remainder is the
open question in §5.1.

Encoder and decoder cost (`/usr/bin/time`, one run each; prometheus/1.27,
94 MB): v1 encode 2.4 s / 902 MB (x86 decoding runs on all cores; the profile
is 38 % `x86asm`, 30 % content-index build); bsdiff 39–43 s / 805 MB;
hdiffz -p-8 7.4 s / 388 MB (8 threads); full `zstd -19 -T0` 9.4 s / 364 MB.
v1 decode 0.9 s / 921 MB RSS, 714 MB of it live heap — against bspatch 0.47 s
/ 192 MB and hpatchz 0.08 s / 25 MB. The correction applies in place (§2.4),
so the decoder holds old + prediction rather than old + prediction + new, but
7.6× is still far from the ≈ 2× §5.3 wants; the rest is the pclntab work,
not the buffers. Wall-clock is no longer the encoder's problem; memory is.

Two lessons the prototype carries into the design:

- Per-Go-release layout handling is real work, not a table of constants:
  1.25 → 1.26 changed `entryOff` handling; 1.26 → 1.27 changed `moduledata`,
  removed `.typelink`/`.itablink`, added `.go.type`/`.go.func`, grew
  `MapType` by 24 B and made `go:func.*` alignment data-dependent. Hence D14:
  one supported Go release, gated by a byte-exact self-prediction check
  (old → old through the whole pipeline), plain codec for everything else.
- Prediction is a compression context, not a correctness mechanism: the
  decoder hashes the predicted base before applying the correction, so
  encoder/decoder divergence is detected, and a wrong prediction costs
  bytes, never a wrong output.

#### 3.2.1 Segment map for resized functions

The `predict .text` row above copies a matched old body *positionally* — byte
k of the old body lands at byte k of the new one — and relocates the whole
body as if it had moved by one constant. That is right for a function that
only moved, and wrong from the first inserted byte onwards for a function
that was edited: every later instruction is copied to the wrong offset *and*
relocated with the wrong PC, so the residual pays for it twice. Attribution
measured the size of that on the 3.13.2→3.14.0 minor pair: 721 resized
matched functions — 0.62 % of the function count, 3.5 % of `.text` — hold
47 % of the `.text` residual at 76 % density, and 58.4 % of the `.text`
residual outside a relocated field sits where the prediction is not even on
an instruction boundary (`go-residual-attribution.md` §3, §8).

The fix is to stop assuming one shift per function. A resized matched
function carries a **segment map** in the layout — a short list of
`(old offset, new offset, length)` pieces — and the decoder lays the body down
piece by piece at its true new position, relocating each piece at its own PC.

**Encoder.** For every matched pair whose layout sizes differ: canonicalise
both bodies (one decode pass, every PC-relative field zeroed *in place*
through `x86.pcrelField` — the same walk `ContentHash` uses, so masking means
one thing everywhere and offsets stay offsets into the raw body; it also
covers the VEX/EVEX rip forms probe A's own canonicaliser missed, so the
shipped alignment can only beat the measured one). Then hash a 12-byte window
at every instruction boundary of the old body (≤ 8 candidate positions per
hash), look up each new boundary's window, keep the longest chain of anchors
monotone in both bodies (patience/LIS; bodies average 2 KB), grow each anchor
byte-wise both ways, merge neighbours that share a shift across up to 16
mismatching bytes, and snap each surviving piece's start forward and its end
back to an instruction boundary of the old body, so that a piece decodes as
instructions when the decoder restarts there.

Pricing, in the pair's own yardstick (≈ 0.6 B per correction run + 0.24 B per
wrong byte compressed, `go-residual-attribution.md` §1): a piece entry cost
1.65 B compressed in the columns below and a piece can split a correction run
at each end, so it must fix (1.65 + 2 × 0.606) / 0.244 ≈ 12 bytes to pay for
itself. **Drop any piece shorter than 12 B** — which is the anchor window, so
the rule only bites after clipping and merging. Drop, too, every piece whose
shift is zero: the decoder's fill already produces exactly those bytes, so
they are free by omission (244,060 of the 1,018,575 covered bytes in the minor
pair's resized bodies are at shift 0, the first piece — old 0 → new 0 —
normally among them). A function with no surviving piece carries no map.

**Wire format.** The layout (`delta/layout.go`) gains one field after the
function op stream — a sparse list, five contiguous varint columns, for the
same reason the correction splits `ctrl` from `lit`: the five have statistics
an order of magnitude apart and each compresses against itself.

```
segmap := uvarint nmapped                 functions carrying a map
          uvarint idxGap × nmapped        new function index, delta-coded
          uvarint nseg   × nmapped        pieces in that function, ≥ 1
          uvarint gap    × Σnseg          newOff − end of the previous piece
          uvarint len    × Σnseg          piece length
          varint  shift  × Σnseg          (oldOff−newOff) − the previous piece's,
                                          per function, starting from 0
```

**Far pieces (transform 3).** The aligner above only looks inside the
function's own old body, so the code an edit *added* is left to the
correction. It is rarely new to the binary: the compiler emits the same
sequences for the same constructs, and on the one-line synthetic pair the
inserted code was found elsewhere in old `.text` by Zucchini's whole-file
search but not by the codec (372 wrong bytes in the function against 79).
From transform 3 a piece's old offset may fall outside the old body — before
it or after it, relative to the old entry — and the decoder copies those
bytes from wherever in old `.text` they are and relocates them at their true
old PC, exactly as it would a local piece. The old-body monotonicity check
is dropped for such pieces; new-body monotonicity stays, and the offset map
for branches into the function uses only the local, monotone subset
(`segfar.go`).

The encoder runs local alignment for every resized function first, installs
those pieces in a provisional mapper, then lays each body down as the
decoder would and, wherever that fill has an instruction wrong, looks the
new body's 12-byte canonical window up in an index of every instruction
boundary of old `.text` (sorted `(hash, offset)` pairs, ≤ 256 hits per
window). The four longest canonical extensions are *scored by relocating
them*: a candidate is priced as the correction would price the result (a
wrong run costs its bytes plus a two-byte header) and kept only when it
beats the fill by 20. An unscored version — canonical match, no relocation
check — lost 19 KB on the minor release, because a sequence with the wrong
call targets is worse than the positional fill. Measured: synthetic 1,275 →
1,184, PIE 1,229 → 1,140, patch release 74,500 → 74,126, minor release
1,352,085 → 1,350,486 (threshold 20 chosen by a sweep over all three; 32
takes 1.4 KB more off the minor release and gives 200 B back on the patch).

#### 3.2.2 Headers from geometry

The ELF header, program headers and section headers were copied from the
old file and corrected: 44 wrong bytes on the synthetic pair, every
`p_filesz`/`p_memsz` and `sh_addr`/`sh_offset`/`sh_size` a grown section
touched, in 29 runs. The layout already carries the new section table, so
`predictHeaders` recomputes them: each program header from the new extents
of the sections its old extent covered, keeping whatever rounding the old
value shows (the writable segment's file size is rounded to 16 — the
self-prediction gate caught the first version, which was not); each section
header's address, offset and size from the layout, and `.shstrtab`'s offset
from the new file length. A header covering no section keeps its old
bytes. 44 → 1 wrong bytes; 1,334 → 1,275 on the synthetic, 74,550 → 74,500
on the patch release, 1,353,084 → 1,352,085 on the minor.


Measured on probe A's lists, before shift-0 pieces are dropped: 720 functions
/ 13,709 pieces = 23,176 B under `xz -9e`, 23,869 B under the codec's own
compressor — ~1.6 % of the minor patch; 25 functions / 434 pieces = 1,252 B /
1,198 B on the patch pair. The decoder bounds-checks everything: indices
strictly increasing and below `NFunc`, the function matched, pieces strictly
monotone and non-overlapping in *both* bodies, `newOff+len` within the new
size and `oldOff+len` within the old one; anything else is `errCorrupt`. The
layout is a new shape, so the transform number becomes 2 and §2.6 does the
rest — a decoder that implements only transform 1 is served a transform-1
patch, or is told the patch is one it cannot read (§2.6).

**Decoder.** A mapped function's *covering list* is its transmitted pieces
plus an implicit shift-0 piece over every new byte they do not cover.
`predictText` INT3-fills the new body and then calls today's `x86.Relocate`
once per piece, with `code = oldBody[oldOff:oldOff+len]`,
`out = newBody[newOff:newOff+len]`, `srcPC = f.Entry+oldOff` and
`dstPC = g.Entry+newOff`. `Relocate` copies before it relocates, so assembly
and relocation stay one pass over the body with no second buffer, and a
function without a map is a single implicit piece — exactly the call made
today. Gaps therefore hold the positional copy rather than zeroes: that is
what makes a shift-0 piece free to omit, and the positional copy is already
right for 438,457 of the 1,512,992 bytes in the minor pair's resized bodies,
mostly the head before the first edit. The choice does not move the ceiling —
probe A charges every uncovered byte to the scheme in full either way. An
implicit piece after the first starts at an arbitrary offset and its decode
can be out of phase; the bytes it covers are inserted code with no old
counterpart, which the correction pays for regardless.

**Addressing.** `mapper.mapAddrBase` (`delta/reloc.go`) makes the same
positional assumption on the *target* side: an old address inside a matched
function becomes `g.Entry + (t − f.Entry)`. A branch into a resized function,
and a back-edge inside one (`rcTextSelf`), must go through that function's
map. `newMapper` keeps the decoded maps by new function index, and for an old
offset `o = t − f.Entry` the lookup is, in order:

1. a transmitted piece with `oldOff ≤ o < oldOff+len` → `newOff + (o−oldOff)`;
2. else, if `o` is below the new size and no transmitted piece covers *new*
   offset `o` → `o`, the implicit piece;
3. else → `o` plus the preceding piece's shift, clamped into the new body.

Transmitted pieces take precedence, which is what makes this the inverse of
the covering list; case 3 is an old byte that was deleted or displaced, where
every answer is a guess and only determinism matters, and the clamp keeps the
result inside the function. Two binary searches over a list of tens of
entries, for the 0.6 % of functions that have one. Everything else that asks
the mapper — data pointers into the middle of a function, jump tables in
`.rodata`, `go:func.*` wrapinfo, the descriptors' `textOff` — gets the finer
map for free, which is the point of having one mapper. `deriveOverrides` runs
*after* the maps are built and with them installed in its own mapper, so the
override table does not spend two varints re-fixing a target the map already
places.

**Determinism.** The aligner runs on the encoder only; the decoder reads a
list. A bad alignment costs bytes, never correctness (§2.1), and the
prediction hash in the patch body still turns a divergence into a named
failure (§2.7). Alignment is per function and order-independent, so the
fan-out constant does not change the patch, and candidate lists are kept in
position order rather than map order. `x86.ContentHash` and `x86.Equal` are
untouched: they price whole bodies for `matchFuncs`, they run before any
alignment, and `Equal` requires equal lengths so it never meets a resized
pair. The map changes how a matched body is laid down, never which functions
are matched.

**Scope.** Name- or normalised-name-matched pairs whose layout sizes differ,
and nothing else. Non-goals:

- *Same-length matched functions.* 8,501,811 of their 8,616,525 alignable
  bytes are already at offset 0; the fitted net is +9,100 B on the minor pair
  and +201 B on the patch pair, and reaching it means canonicalising all
  110 K functions — the whole of `.text` rather than 3.5 % of it. Growth that
  fits inside a function's 32-byte padding lands in this bucket and is left
  there.
- *Unmatched functions* (no old body to segment) and *non-`.text` sections*
  (the data maps already shift piecewise, at 16-byte granularity).
- The `deferreturn` word of a `_func` record is a within-function PC offset
  the same map could re-base. That is a pclntab change, not this one.

**Expected effect.** Net **+70,013 B measured / +86,463 B fitted** on the
minor pair (4.8–5.9 %) and **+2,087 / +5,611** on the patch release
(2.2–6.0 %), both after paying for the segment list; the two synthetic pairs
are one resized function each and would gain tens of bytes. The ceiling is a
marginal measurement rather than an estimate — the bucket's wrong bytes are
reverted to the prediction and the whole patch re-encoded and re-compressed,
with the segment list and the fitted price of what alignment would still
leave wrong subtracted from the difference (`go-residual-attribution.md` §8).
It was measured against patches of 1,467,993 B and 93,965 B, since reduced to
1,454,272 B and 82,288 B. Two things bound it: 494,417 of the 1,512,992 bytes
in the minor pair's resized bodies (32.7 %) have no old counterpart at any
offset — that is the actual code change — and fragmentation, which takes the
wrong bytes from 1,152,688 to 632,149 but the correction runs from 10,451 to
39,239, eating 23,779 B of the gain at 0.6 B a run.

#### 3.2.2 Replaying the linker's `pctab` allocation

The `predict .gopclntab` row re-targets a matched function's pc-table
offsets, but a function the release *added* has no old record to re-target:
`synth` fills its `pcsp`/`pcfile`/`pcln`/`pcdata[]` from the modal record,
which is right 15 % of the time. The linker allocates `pctab` strictly
sequentially — `cmd/link/internal/ld.generatePctab` walks the functions in
link order and per function appends `pcsp`, `pcfile`, `pcln`, `pcdata[0]`,
`pcdata[1]`, `pcdata[3..n)` and finally `pcinline`, which `writeFuncs` stores
in `pcdata[2]`; it starts at offset 1, records a zero-length table as offset
0, and skips a table it has already emitted, since pc-value symbols are
content-addressable. So `pctab` *is* the distinct tables in allocation order,
a table whose content is new lands exactly on the running high-water mark —
9,572 of 9,572 invented fresh slots on the minor pair, none past it — and a
table whose content repeats lands wherever its twin already is, which nothing
the decoder holds identifies (`go-residual-attribution.md` §11).

**Wire format.** The layout gains a field at its end: a bit per *invented*
slot — every pc-table slot of an unmatched function's record, plus the
`pcdata` slots a reshape appends — in record order and, within a record, in
the emission order above; and a `uvarint` gap per record that has any such
slot. 1 means the linker allocated that table fresh; 0 means anything else
and the slot keeps the modal template's value, which is what the codec sends
today. Zero-length tables need no third state: the template is already 0 for
the slots that are usually empty (`pcdata[2]`, right 86.6 % of the time that
way). The bits are packed rather than run-length coded — the fresh slots are
22 % of the total and scattered, so the runs are short. Compressed on its own
the run form looks 5 % better (12,001 B raw, 1,063 B compressed, against
5,357 B and 1,120 B for the vector), but the patch body is compressed as one
frame and the 6.6 KB of extra raw bytes cost more than the runs save: the
minor pair's patch is 1,354,582 B with runs and 1,352,768 B with the vector.

**Decoder.** One cursor over the real `pctab` (a stage-1b table, so both
sides hold it), standing on the table the next fresh allocation will take and
starting at offset 1. At a record's first invented slot the cursor skips that
record's gap — the tables the functions in between claimed — and then each
slot whose bit is set takes the cursor and steps it past that table, whose
length is parsed out of `pctab`, since pc-value tables are self-delimiting.
The cursor never reads a predicted offset, which is the point: driving it
from the matched records' own re-targeted offsets was measured first and one
mispredicted offset in 880,000 poisons every allocation after it — 56 of 142
slots on the patch pair, 346 of 9,572 on the minor pair, and a net loss on
both. The gaps cost 959 B compressed on the minor pair and 71 B on the patch
pair, and make the replay exact on every fresh slot. The bit count must be
the number of invented slots the layout implies, the vector must be that many
bits and there must be exactly one gap per record that has one; anything else
is `errCorrupt`, and a gap that would walk off the end of `pctab` stops there.
Transform 2 and above; the field is that transform's rather than a new one,
since transform 2 has not shipped. It is written only when the release added
a function, and it is last in the layout so that a pair with nothing to
replay pays not one byte for it.

**Effect.** The minor pair goes from 1,371,442 B to 1,352,768 (**+18,674**,
1.4 %) and the patch pair from 78,703 to 78,462 (**+241**, 0.3 %), both after
paying for the bits and the gaps; the replay is exact on all 9,572 and all
142 fresh slots. The four synthetic pairs add no function and are unchanged
to the byte.

### 2.3 Why no suffix array

Once the layout table is applied the prediction has **exactly** the new
file's length and structure (every function is at its final entry, unmatched
functions as zero-filled slots of the right size, tables regenerated at the
right sizes). The layout table carries the size of every variable-length
pclntab blob and of every function, so the prediction is length-exact even
where its *content* is wrong — which is what makes the correction positional:
walk predicted and new in lockstep, emit a region where they differ. Where a
region falls inside a function whose body genuinely changed, the shifted tail
is recovered by matching against the prediction's own bytes there (§2.4),
which is the same "copy/insert/copy" bsdiff would find, at O(region) memory
and without needing the old body as a second buffer. Unmatched (new)
functions are sent literally; the compressor handles the rest.

Cost model for a 1 GB binary (600 MB `.text`, ~400 K functions): x86 decode
ran at 17–29 MB/s per core in the prototype (6 s for vault's 159 MB `.text`)
and is embarrassingly parallel per function (relocation with 24 goroutines:
1.6 s on vault); the `.rodata` content-map build is O(n) hashing (13 s for
95 MB single-threaded, parallelisable). Measured on the 94 MB pair, the
encoder is 2.4 s and 9.6× the input in RSS and the decoder 0.9 s and 7.6× in
live heap (§2.2) — against bsdiff's 8.6–12× and a suffix array's 5–8×, but
well above the 2–3× this section first estimated, because the working set is
dominated by the per-function pclntab structures rather than by the file
buffers (§5.3). Linear extrapolation puts a 1 GB pair at ~25 s of encode,
which a CI box can afford, and at a decoder footprint no target should be
asked for; that is the constraint §5.3 has to remove.

### 2.4 Correction format (stage 2)

The prototype's stage 2 was purely positional -- runs of `(gap, len, bytes)`
where the prediction differs from the new file. That is optimal where the
prediction is right and wasteful inside a function whose code genuinely
changed: an insertion of five bytes shifts the rest of the function, and a
positional encoder re-sends all of it. Measured on prometheus/1.27, positional
runs cost 111,552 B against the 94,470 B an hdiffz stage 2 reaches (§2.2), and
the gap is entirely inside changed `.text`.

v1 therefore encodes the correction as **positional regions, each written
whichever of two ways is smaller**:

```
correction := uvarint newLen, uvarint nRegions, uvarint ctrlLen, uvarint litLen,
              ctrl[ctrlLen], lit[litLen]                 ← two streams, compressed together
ctrl       := per region, in output order:
                uvarint gap        bytes identical to the prediction since the previous region
                uvarint span<<1|m  length of the region; m = 1 for a match stream, 0 for literals
                if m: triples until span bytes have been emitted:
                  uvarint lit      literal bytes taken from the lit stream
                  uvarint copy     bytes copied from the source cursor
                  varint  seek     signed move of the source cursor before the copy
```

Regions are maximal runs of differing bytes, merged when closer than 6 B. A
region's source window is the *prediction's own bytes* from the region's
start to `min(256, fileLen - regionEnd)` past its end, so a shifted function
tail is one `copy`. The window is not transmitted: it follows from the
region's end and the file's length, which both sides know. Most regions are
two to four bytes -- a relocation the mapper placed wrongly -- and for those
the match stream costs more than the bytes themselves, so the mode bit picks
literals and the encoding degenerates to exactly the prototype's positional
one. Control varints and literals are separate streams because their
statistics differ by an order of magnitude.

**Transform 2 chooses between two shapes of this stream.** Which one is
smaller is a property of the release rather than of the codec (research 14):
a patch release's residual is near-misses a few bytes apart, so merging runs
up to 32 correct bytes apart costs almost nothing -- the swallowed bytes are
correct, and xor-ed against the prediction they are zeros -- and saves a
region header each; a minor release's residual is genuinely new content,
where the literal stream's value is that it matches *itself*, and xor against
an unrelated prediction destroys those matches. The encoder therefore writes
the correction twice -- once as above, once with `merge = 32`, literal
regions carrying `want^pred`, and gaps, spans, match ops and literals as four
streams -- compresses both with the compressor that will ship them, and keeps
the smaller. The choice is the low bit of `nRegions`, and the columnar shape
declares two more stream lengths:

```
correction := uvarint newLen, uvarint nRegions<<1|alt, then
  alt = 0:     uvarint ctrlLen, uvarint litLen, ctrl, lit
  alt = 1:     uvarint gapLen, uvarint spanLen, uvarint opLen, uvarint litLen,
               gaps, spans, ops, lit    ← a literal region carries want^pred
```

A transform-1 stream carries no flag bit and only the first shape, because
that is the only one a transform-1 decoder reads. Measured on prometheus: the
patch release 3.13.1→3.13.2 takes the merged shape and its patch falls from
78,462 to 74,550 B (−5.0 %); the minor release 3.13.2→3.14.0 keeps the
shipped shape and moves by +316 B, a tenth of that stream's 3,411 B
compression noise floor and entirely the flag bit shifting brotli's block
boundaries. The price is a second compression of the largest stream in the
patch: the minor pair encodes in 13.1 s rather than 7.6 s.

Two properties matter operationally. The source window starts at the region
and runs *forward*, into bytes no earlier region has touched, so the decoder
snapshots it, writes the region in place, and **applies the correction over
the prediction buffer** rather than holding prediction and output at once
(§5, memory). And the prediction is length-exact by construction, so `span`
is the same on both sides and every read and write is bounds-checked against
one buffer.

Measured on prometheus/1.27: 119,493 B of correction stream for 120,852
differing bytes, 66,052 B compressed -- against 79,696 B for the prototype's
purely positional stage 2.

**Stages 1a and 1b use the plain codec (§2.8), not this one.** Both are
length-changing: 1a is `funcnametab`+`filetab` old → new, and 1b's predicted
`cutab`/`pctab`/`go:func.*` are padded to the new tables' lengths but never
truncated, so a release that shrank a table leaves the prediction longer than
the truth (the decoder is told the target length). More importantly 1b's
residual is *shifted*, not positional: one pc-value table that changed length
moves every table after it, which a bounded local window cannot follow. On
prometheus, encoding stage 1b positionally costs 66,372 B compressed against
17,441 B for the plain codec (D17).

### 2.5 Compression, and the patch container (`.bsz`)

*Status: this container is frozen. presage writes its own container
(`docs/general/presage-core.md`); `delta` still reads and writes `BSZ1` so
that patches made before the switch still apply.*

Payload streams are compressed with the smallest of the candidates the
encoder is willing to spend time on, and the choice is recorded per frame.
Measured on the prometheus/1.27 patch's own streams (pure-Go encoders,
against the `zstd -19` CLI the prototype shelled out to):

| stream | raw | `zstd -19` (CLI) | klauspost zstd best | brotli-11 (`LGWin` 24) |
|---|---:|---:|---:|---:|
| layout | 239,379 | 11,271 | 12,885 (+14 %) | **10,388 (−8 %)** |
| correction | 162,758 | 79,696 | 85,569 (+7 %) | **74,631 (−6 %)** |

Pure-Go zstd is 6–14 % *worse* than the CLI the research numbers were taken
with — klauspost's best level is the equivalent of `zstd -11` (it has no
btopt/btultra), and the CLI at -19/-22/`--long` is itself still 2–4.5 %
behind brotli on the same frames. Pure-Go brotli at quality 11 matches the
reference encoder byte for byte. Since patch bytes are the product, streams
take the smaller of zstd and brotli-11; zstd stays a candidate because its
framing wins by a few dozen bytes on the smallest patches (the `v3→v4` pair
is 580 B with it and 613 B without) and costs ~30 ms per 8 MiB frame to try.

Brotli is always quality 11. An earlier tier dropped to quality 10 above
4 MiB on the belief that it was 25× faster and within 2 %; re-measured on the
frames themselves it is 1.7–1.9× faster and 4–5 % larger. Whole-file, on
prometheus 3.13.2 (93.8 MB) cut into the same 8 MiB frames, encoding
eight-way parallel and decoding sequentially:

| frame codec | bytes | encode | decode |
|---|---:|---:|---:|
| klauspost zstd, best | 23,838,795 | 2.5 s | 78 ms |
| brotli-10 | 20,363,316 | 13.3 s | 317 ms |
| **brotli-11** | **19,579,925** | **25.7 s** | **268 ms** |
| `zstd -19 -T0` (CLI, one stream, for reference) | 20,579,646 | 9.4 s | – |

The encoder pays 26 s once; the decoder pays 268 ms for 780 KB less than at
quality 10. On the 8.7 MB debug-build residual (three frames) quality 11 is
4.9 % smaller for 10 s more wall clock. The first draft's "the largest
payloads must stream, so they are always zstd" was an assumption about
brotli's cost curve (D22).

```
magic "BSZ1"  u8 transform (0 = plain, 1 = go-amd64-v1)  u8 flags (bit 0 = debugz)
header: uvarint header_len, then
        b3 from[32], b3 to[32], uvarint old_size, uvarint new_size,
        uvarint nframes, per frame: uvarint off, uvarint len, uvarint zlen,
                                    u8 codec (0 raw, 1 zstd, 2 brotli), b3 hash[32]
frames: independently decodable, each ≤ 8 MiB decompressed, concatenating in
        order to the patch body, which begins with uvarint expanded_new_size
        when debugz is set, and for transform 1 is then
          layout (section table, moduledata values, function layout, pclntab
                  table offsets, data maps, shift tables, pointer overrides),
          stage-1a delta, stage-1b delta, b3 of the prediction[32],
          stage-2 correction
```

The debugz flag (`delta/debugz.go`) says both files were coded with their
`SHF_COMPRESSED` sections expanded: the encoder expands old and new, checks
that new rebuilds exactly from its expansion with Go's zlib at level 1 (what
the linker used) and the old file's set of compressed sections, and only
then sets the flag; the decoder expands old, decodes to the expanded new,
recompresses, and verifies against `to` as always. `from`, `to`, `old_size`
and `new_size` describe the files as shipped. A decoder that does not know a
flag bit reports the patch as an unsupported transform rather than guessing.

Frames are cut at 8 MiB, not at stream boundaries: a frame costs a 32-byte
hash and a table entry, and compressing the streams separately came to within
0.1 % of compressing them together on both the 94 MB pair and the one-line
one. The prediction's own hash rides in the body so that an encoder and a
decoder that disagree say so (§2.7) instead of producing a wrong binary and
relying on the release hash to catch it.

The header is a bounds-checked varint record rather than CBOR: 60 lines of
varint reader are less surface than a codec dependency. Each frame's BLAKE3
is in the header, so a partially fetched patch is verified frame by frame and
resumed at a frame boundary. All offsets read from the patch are
bounds-checked against `old_size`/`new_size`; a malformed patch fails
verification, it cannot crash the decoder or write outside the output.

### 2.6 Transform versioning and the supported Go release

The container's transform byte names the wire format, so an encoder can be
held to the newest transform a given decoder is known to read
(`Options.MaxTransform`). Transforms so far: 1 (Go-aware), 2 (+ segment
maps, adaptive correction shape), 3 (+ far pieces, §2.2.1). Header
prediction (§2.2.2) changed no format: it is a better prediction of bytes
every transform corrected. A decoder that meets a transform it does not
support, or a Go version whose pclntab format it does not know, says so as a
named `ErrUnsupportedTransform` — the caller's cue to send the whole file
instead — never a wrong result.

Format stability: the codec depends on `.gopclntab` (`runtime/symtab.go`,
magic `0xfffffff1` since Go 1.20), `moduledata` field order, and the 32-byte
function alignment. The magic is stable across 1.20–1.27 but the surrounding
layout is not: the prototype needed separate code paths for 1.25
(`entryOff`), 1.26 and 1.27 (`.go.type`, `moduledata`, `go:func.*`
alignment).
The codec therefore keys its
pclntab/moduledata/type-descriptor handling on the Go version from
`debug/buildinfo`, and **supports exactly one Go release at a time: the
current stable one (1.27 today; D14)**. A binary built by any other
toolchain gets the plain codec (§2.8). Each new Go release is enabled only
after a byte-exact self-prediction check (old → old through the full
pipeline) on a small corpus built with that release; the previous release is
kept only for as long as it costs nothing. This trades a wider compatibility
matrix — one that the prototype showed is real work per Go minor — for a
codec that is small enough to be reviewed and tested exhaustively; a binary
built by an older Go still patches correctly, just less well.

*Less well* has since been measured, and it is worse than this paragraph
implies: the same prometheus pair is 74,636 B in 4.2 s when both sides are
rebuilt with go1.27 and 2,137,152 B in 27.9 s as upstream ships it, built
with go1.26.5 — 28.6× the bytes for 6.6× the time — and no upstream release
binary in the local corpus is built with the pinned release at all
(`general/research/toolchain-skew.md`). That document proposes keying the
handling on a `gobin.Layout` descriptor validated against the image's own
invariants, one reader and one writer per release rather than a pair matrix,
with the self-prediction check as the per-release acceptance test and as a
runtime probe for a release with no descriptor. Nothing of it is built; D14
stands until it is.

### 2.7 Determinism and safety

- The prediction uses only the old file plus bytes from the patch; no
  environment, no clock, no randomness. Encoder and decoder share the code —
  literally: `encodeGoAMD64` builds its prediction by decoding the layout it
  just encoded and running the decoder's own functions on it, so the bytes
  the correction is measured against are the bytes the decoder will produce.
- Anything that could vary with the machine is a compile-time constant, not a
  core count: the worker counts in the parallel map build and the parallel
  relocation are fixed at 24, because a prediction that depends on `GOMAXPROCS`
  is a prediction two hosts can disagree about.
- The encoder puts the prediction's BLAKE3 in the patch and the decoder checks
  it before applying the correction. A mismatch is a clean, named failure —
  "encoder and decoder disagree" — rather than a wrong file caught only by
  the output hash.
- Enabling a Go release is gated on a byte-exact self-prediction: every corpus
  binary is run old → old through the whole pipeline and must come out
  identical, with a correction of at most 64 bytes (`TestSelfPrediction`).
  That gate is what caught `pcHeader.textStart`, which Go 1.27 leaves zero
  and the codec was filling in — 8 bytes per patch that no round-trip test
  would ever have shown, because the correction dutifully fixed them.
- Any decode failure of an instruction (1,616 undecodable bytes in
  prometheus's 89 MB of `.text`) leaves those bytes unrelocated; the
  correction fixes them.
- The result is BLAKE3-hashed and compared with the `to` in the patch header
  before it is handed back.

### 2.8 Plain codec (transform 0)

For non-Go, non-amd64, or otherwise unparseable inputs: a bsdiff-class
encoder over the whole file — an approximate match extended over the bytes
around it, so a shifted region costs a control triple and a difference
stream that is zero wherever the two agree — in the same container and
frames. Two substitutions against bsdiff proper:

- **No suffix array.** The anchor search uses the same position-ordered
  content index as the rest of the codec (`delta/internal/lz`). The suffix
  array is what makes bsdiff need 9–12× the input in RAM and minutes above
  100 MB; on executables the anchors this index finds are long enough that
  bsdiff's extension heuristic does the same work from them.
- **A sparse difference stream.** bsdiff's difference stream is one byte per
  matched byte and almost all zero — 383 K non-zero bytes in 30 MB on the
  reference pair — which bzip2's block sort handles beautifully and every LZ
  compressor handles badly. Carrying the zeros as run headers instead of
  bytes takes that pair from 315 KB to 170 KB, and drops the decoder's
  working set for the stream from the size of the file to nothing.

Measured on the one-line pair (29.6 MB): 169,929 B in 2.2 s and 278 MB RSS,
against bsdiff's 150,475 B in 39–43 s and 805 MB. 13 % more bytes for 18×
the speed and a third of the memory is the right trade for a fallback; the
Go-aware codec is what the bytes are supposed to come from.

It is capped at 256 MB old-file size; above that the encoder declines. (The
PIE build of the same pair used to take this path, at 72,681 B; the Go-aware
codec now reads ET_DYN, maps the `.data.rel.ro*` sections and carries the
GLOB_DAT head of Go's `.rela`, and patches it at 1,277 B.)

Not doing: zstd `--patch-from` as a codec (3.5× worse than bsdiff on
one-line and on kube-apiserver), HDiffPatch (C, cgo), xdelta3 (worst sizes,
8.9× RAM).

## 3. Testing strategy

Three tiers, split by what they need and how long they take.

- **Unit**, everything in `go test ./...`: round-trips over generated byte
  streams, and the corruption of every field of every header. 175 test
  functions across the repository, 3.7 s in total.
- **Corpus** (`BINSYNC_CORPUS=<dir> go test ./delta`): the two gates that
  need real binaries. `TestSelfPrediction` runs every corpus binary old → old
  through the whole pipeline and requires a byte-identical result and a
  correction under 64 B — this is what enables a Go release (§2.7).
  `TestCorpusRoundTrip` encodes and applies every *ordered pair* in the
  corpus, in both directions and across build flavours, and requires the
  result to be byte-identical to the target; 32 pairs, 170 s.
- **Benchmark** (manual, `bench/`): patch sizes and encoder time on the
  release corpus; must not regress against §2.2.

## 4. Decisions

| # | Decision | Reason (short) |
|---|---|---|
| D1 | Delta patches, not CDC/CAS | 100× smaller for shifted executables (`cdc-cas.md`) |
| D2 | Go-aware predict-then-correct as the primary codec; plain bsdiff-class fallback | 5.7–38× over bsdiff on the corpus; scales to 1 GB without a suffix array (§2) |
| D3 | Correction is positional after a layout-exact prediction | removes the encoder's memory/time cliff; the layout table makes prediction length-exact |
| D14 | Go-aware codec supports one Go release at a time (the current stable, 1.27); everything else takes the plain codec | pclntab/type layouts change per minor; one version + a self-prediction gate keeps the codec small and testable (§2.6) |
| D15 | Correction = positional regions, each written as literals or as a bounded local match, whichever is smaller | recovers the bsdiff-quality bytes inside changed functions that purely positional runs re-send, at O(region) memory, and lets the decoder apply in place (§2.4) |
| D16 | Every stream takes the smaller of zstd and brotli; brotli quality 11 up to 4 MiB, 10 above | pure-Go zstd is 6–14 % worse than the `zstd -19` the research numbers used, pure-Go brotli is better at both qualities; patch bytes are the product (§2.5) |
| D17 | Stages 1a and 1b use the plain codec, not the positional correction | their residual is *shifted*, not positional — one pc table that changed length moves every table after it. Positionally, stage 1b costs 66,372 B on prometheus against 17,441 B (§2.4) |
| D18 | The source window of a correction region is derived (`min(256, fileLen − end)` past its end), not transmitted | both sides know the region's end and the file's length; a transmitted window is 2–4 varints per region and there are tens of thousands of them (§2.4) |
| D19 | The patch body is one frame per 8 MiB, not one frame per stream | a frame costs a 32-byte hash and a table entry; the streams compressed separately came within 0.1 % of compressing them together (§2.5) |
| D20 | The encoder transmits the BLAKE3 of its prediction, and every prediction worker count is a compile-time constant | encoder/decoder divergence becomes a named failure and a blob fallback instead of a wrong file caught by the release hash; a prediction that varies with `GOMAXPROCS` is one two hosts can disagree about (§2.7) |
| D21 | The prediction fills the bytes no allocated section covers: gaps cleared, `.shstrtab` copied from the old file's tail | the base is a copy of the old file, so every section that moved left stale bytes behind in its gap — 3,780 mispredicted bytes on prometheus, 109 after (§2.2) |
| D22 | Frames are brotli-11, not zstd, and each carries its own codec tag | 18 % smaller than zstd and smaller than a single `zstd -19` stream, for 26 s of encode and 268 ms of decode on a 94 MB input (§2.5). Supersedes the "the largest payloads are always zstd" half of D16; the quality-10 tier it first shipped with was re-measured at 4 % larger and dropped |

D4–D13 were the release-distribution decisions of the project this codec
grew out of and are not presage's; the numbering is left as it was so the
citations elsewhere still land.

## 5. Open questions

The last question §2.3 asked — whether the local match inside changed regions
lands nearer the 94,470 B of a hdiffz stage 2 than the 111,552 B of purely
positional runs — is answered: v1 reached 95,366 B and presage now reaches
70,195 B (§2.2), below the hdiffz bound rather than beside it. What remains:

1. **`.go.type` residual.** 45,170 B of the prometheus/1.27 patch is type
   descriptors: ~31.6 KB are genuinely new descriptors (inherent), ~7.9 KB
   are wrong rewrites and ~4.5 KB names. Only the consensus pass has been
   applied; an anchor pass (descriptor start → symbol) may recover the wrong
   rewrites. The changed `.text` (63,179 B, 52 % of the mispredicted bytes)
   is the other large item and is real change.
2. **arm64** in the Go-aware codec: fixed-width instructions make operand
   relocation *simpler* (ADRP/ADD page+offset pairs, B/BL imm26); needs a Go
   arm64 corpus to validate. Until then arm64 targets get the plain codec.
3. **Memory — the one open question that is a constraint, not a saving.**
   Applying the correction in place (§2.4) removed one whole copy of the
   file, and the decoder still peaks at 714 MB of live heap for a 94 MB
   binary, 7.6×. The buffers are only 2× of that; the rest is the
   prediction's own working set, and three items account for most of it:
   the 110,147 `_func` records rebuilt as individual slices in
   `predictPcln` (arena-allocate them into one backing array), the
   position-ordered index over `pctab` and `go:func.*` at 8 bytes per
   position, and the encoder's habit of holding `old` and `pred` while it
   also holds `new`. A 1 GB binary at this ratio asks a target for 7.6 GB,
   which is not a thing to ask of a target; §2.3's ≈ 2× is the target
   and none of the three fixes changes the format.
4. **Encoder details.** `.text` is x86-decoded twice (relocation and
   shift-table derivation; caching saves ~0.3 of 2.4 s). The pointer-override
   table costs 5.5 KB of layout for a 26.5 KB net gain and its second
   consensus round gained nothing on prometheus. The remaining 109
   mispredicted bytes of ELF program and section headers could be predicted
   exactly from the layout, which already carries every section's address,
   offset and size; it is ~30 lines for ~0.1 % and has not been written.
