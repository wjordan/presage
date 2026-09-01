# The ELF module: `presage/elfmod`

*Spec, 2026-08-29. Implementation-ready. The decoder-side pipeline of
`bench/elfpredict` at its default `corrected-fields` rung, as one presage
module behind the `Module` seam of `presage/module.go`, so that
`presage diff old new -symbols old.sym,new.sym` and `presage patch` cover a
non-Go ELF x86-64 image end to end. Design authority: `SPEC.md` §4–6, §10
item 3; template: `presage/gomod` (`presage-core.md` §4, §7).*

Container v6 uses the compact CM model, balanced terminal compression, the
RELR slot layer, exact FDE CIE pointers and an exactly rebuilt
`.eh_frame_hdr`. Current end-to-end
sizes and resource measurements are in `baselines.md`; the status sections
below retain the measurements that motivated each layer.

## Status (2026-09-01): three derivable fields outside `.text`

Three fixes, all decoder-derivable, none costing more than a flag.
`presage.Version` is 6: the plan gains a ninth stream and the `eh_frame`
stream a flag.

1. **RELR relocation slots** (`relr.go`, stage 5b). An image whose linker
   packs its relative relocations into `.relr.dyn` has no `.rela.dyn` entry
   naming its `.data.rel.ro` pointers, so nothing told the module those
   words were addresses and the equivalence copy left every one of them
   pointing where its target used to be. The decoder now enumerates the
   **old** table's slots, projects each slot's file offset forward through
   the equivalence map and writes the pointer oracle's answer for the value
   that was there. libxul: 465,628 slots, all retargeted, `.data.rel.ro`
   1,109,576 → 45,929 mispredicted bytes. The plan pays 6 bytes — the old
   table's offset and size; the section maps the relocation plan already
   carries do the address arithmetic. Regenerating the new packed table the
   same way was measured and lost: its bitmap words are dense and the few
   bits the projection gets wrong cost more than the table's byte prediction
   saves. Chrome has no `.relr.dyn` and is untouched.
2. **`.eh_frame_hdr` through the `Finaliser` seam** (`ehframe.go`). The
   header is a pure function of the *finished* `.eh_frame`, and predicting
   it from the projected FDEs was a guess that a few unplaced entries
   shifted wholesale. The module now implements `presage.Finaliser`:
   `MaskResidual` hands the encoder a prediction that already holds the
   target's header, so the residual carries nothing for it, and `Finalise`
   rebuilds it over the corrected section after the residual is applied.
   The encoder ships the `HdrExact` flag only after checking that rebuilding
   the target's header from the target's own `.eh_frame` reproduces it byte
   for byte, so a linker the rule does not hold for falls back to shipping
   the section. `regenerateEhFrameHdr` and its `ehNewFDE` plumbing are gone.
3. **FDE CIE pointers** (`ehframe.go`, stage 7). `retargetEhFrame` wrote an
   FDE's `initial_location` and `address_range` but never its `cie_ptr` at
   entry+4, the one field of an FDE that states a *position* rather than an
   address: the distance back from itself to the CIE that governs it. Every
   FDE downstream of an insertion or a resize has moved relative to its CIE,
   so the byte prediction carried the old distance — libxul 513,723 of the
   section's 535,051 wrong bytes, across 272,545 FDEs. Both ends of the
   distance are projections the retarget already computes, so the fix reads
   the CIE offset out of the walk, projects each distinct CIE once, and
   writes the difference. Zero plan bytes. `.eh_frame` mispredictions:
   libxul 535,051 → 23,627, Chrome 70,961 → 29,973.

| pair | before | + RELR slots | + exact `.eh_frame_hdr` | + `cie_ptr` |
|---|---:|---:|---:|---:|
| libxul 154.0 → 154.0.1 | 2,866,328 | 2,609,318 (−257,010) | 2,598,190 (−11,128) | **2,337,304** (−260,886) |
| Chrome 151 .169 → .173 | 2,376,189 | 2,376,189 (no `.relr.dyn`) | 2,366,669 (−9,520) | **2,346,975** (−19,694) |

Applied and `cmp`-verified through the CLI at every step. Encode and apply
are unchanged within the noise of a shared machine, and peak RSS does not
move (encode: libxul 1,800,944 → 1,801,900 KiB, Chrome 2,811,884 →
2,811,380 KiB; apply: 399 → 407 MiB and 678 → 675 MiB). `MaskResidual`
clones the whole prediction, but the encoder's peak is the matcher's two
masked images, long freed by then.

The encoder's error counts -- `PredictErr` and the `mispredicted by section`
note -- are taken against the masked prediction the residual is actually
priced against, so `.eh_frame_hdr` drops out of the note rather than
appearing as a section wholly mispredicted at no cost.

## Status (2026-08-31c): the plan side

Two changes to the plan, `presage.Version` 4 (research/pgo-churn.md §8.2).

1. **The remap basis** (`fieldfix.go`). A remapped field's new target is
   stated as an index into the addresses the prediction already points at
   plus the function starts the map placed, escaping to the old shift where
   the target is not one of them. The encoder prices both bases per window
   and ships the smaller under a basis byte. Free at decode.
2. **The plan's columns under the CM coder** (`planpack.go`,
   `delta/cmplan.go`). The largest columns are carved out of the plan blob
   and coded on their own under varint contexts, each carrying its own
   codec and context byte; the rest stays in the body's joint blob. Which
   columns are coded is encoder policy only (`PRESAGE_PLAN_CM=off|reloc|<gain>`,
   default `2000`) — the decoder follows the table it is given.

| pair | v3 | + basis | + plan CM (gain≥2000) | (gain≥1000) |
|---|---:|---:|---:|---:|
| Chrome 151 .169 → .173 | 2,291,929 | 2,278,289 | **2,256,358** | 2,248,931 |
| libxul 154.0 → 154.0.1 | 2,691,589 | 2,679,613 | **2,665,810** | 2,663,297 |

Encode is flat across tiers, 57.9–60.7 s (the columns are coded in parallel
and the encoder reuses the plan it just packed rather than decoding it
back). Apply pays the coder over the carved columns — 1.27 MB of Chrome's
3.64 MB plan at the default tier, 1.69 MB at gain≥1000 — at 1.8 MB/s for a
varint column and 4 MB/s for an opaque one, median of five interleaved
runs: Chrome 6.20 → 6.89 s (7.12 at gain≥1000), libxul 5.01 → 5.87 (6.08).
That is why the tier is a knob and not a constant; the full frontier is in
research/pgo-churn.md §8.2.

## Status (2026-08-31b): the prediction-conditioned coder

The residual's terminal stage now offers a context-mixing arithmetic coder
conditioned on the prediction byte under each correction byte
(`delta/cmcoder.go`, `SPEC.md` §6.3 amending G6). It is a per-stream
candidate: the encoder codes every piece stream above 4 KiB both ways and
ships whichever is smaller under a codec id in the piece's stream table.
Measured through the CLI, applied and `cmp`-verified:

| pair | before | + CM coder |
|---|---:|---:|
| Chrome 151 .169 → .173 | 2,412,635 | **2,303,210** (−109,425) |
| libxul 154.0 → 154.0.1 | 2,942,865 | **2,705,570** (−237,295) |
| librustc_driver 08-27 → 08-28 | 6,564,155 | **6,052,023** (−512,132) |

Where it goes on Chrome: the `.text` piece's five byte buckets are 1,219,717
raw and 640,616 under brotli; the coder takes them to 533,643 (−106,973), and
with the piece's other columns the whole `.text` piece falls 1,064,564 →
952,890. That is the harness's rung-4 result arriving intact through a
different correction shape. The encoder chose CM for 28 of 34 offered streams
on Chrome and 41 of 46 on libxul, including several with no positional
conditioning at all — the lens column, the displacement tags — where the
match model alone beats brotli. It correctly declined Chrome's 377,872-byte
`gaps` column (272,617 brotli, 274,220 coded), which §8 of `pgo-churn.md`
called CM-proof. libxul gains more than Chrome because more of its correction
is long runs of replacement bytes, which is where a prediction byte under
each one is worth most.

Encode is unchanged within noise (Chrome 108.1 s → 106.9 s, libxul 169.9 s →
171.8 s): the CM attempt is a couple of megabytes against a two-minute
encode. Apply pays the coder's speed, roughly 1.5 MB/s of residual: measured
against the same build with the coder switched off, Chrome goes 5.1 s →
6.1 s and libxul 2.4 s → 4.4 s. Both no-coder builds reproduce the previous
column's numbers to the byte, so nothing else in this change moved bytes.
`presage.Version` is 3; an older build refuses every patch by name.

Concatenating a piece's byte buckets into one coded stream, worth −2,960
against xz in the harness, loses here (+10,862 on Chrome): five separate
streams already give the coder five separate model banks.

## Status (2026-08-31)

The derived function map (§2.1) and the residual's displacement columns
(§2.2) are ported from the harness. Measured through the CLI on this tree,
applied and `cmp`-verified:

| pair | before | + derived map | + displacement columns |
|---|---:|---:|---:|
| Chrome 151 .169 → .173 | 2,537,338 | 2,427,719 (−109,619) | **2,412,635** (−15,084) |
| libxul 154.0 → 154.0.1 | 2,996,042 | 2,959,645 (−36,397) | **2,942,865** (−16,780) |

Encode is unchanged within noise (Chrome 106.0 s → 107.7 s, libxul 168.4 s →
171.5 s); apply pays the enumeration sweep and the residual re-walk (Chrome
4.5 s → 6.4 s, libxul 2.4 s → 2.7 s). The corpus gate's third pair,
librustc_driver (four code windows, Rust v0 symbols), moves 6,597,439 →
6,564,155. Both changes alter the wire format, so
`presage.Version` is 2 and every patch an older build wrote is refused by
name.

## Status (2026-08-30)

Built and shipped: `presage/elfmod` (tracks T1–T4 of §8), `presage/symbols`,
`-symbols` on `presage diff`, the matcher work of §4 in `presage/eqmatch`,
and the split residual in the core (`SPEC.md` §6.1). Measured through the
CLI, applied and `cmp`-verified:

| pair | `presage diff -symbols` | harness, native matcher | harness, Zucchini stream |
|---|---:|---:|---:|
| Chrome 151 .169 → .173 | **2,581,091** (encode 111 s / 6.0 GB, apply 4.4 s / 3.8 GB) | 2,617,700 | 2,634,264 |
| libxul 154.0 → 154.0.1 | **3,010,960** (encode 164 s, apply 2.1 s) | 3,632,264 | 4,063,404 |
| libxul, no symbols | about 3.5 MB | — | — |

The Go rows are unchanged (1,202 / 70,195); the prometheus DWARF pair moved
332,414 → 330,678 from the matcher's near search. The port's self-prediction
gate found three bugs the harness numbers had carried, so the module beats
the harness on identical plans: a CIE with a personality routine (`zPLR`)
was declined and its FDEs (198 libxul / 221 Chrome) left out of the
regenerated `.eh_frame_hdr`, shifting every later entry; the function map
collapsed duplicate names (Rust closures, monomorphisations,
anonymous-namespace templates) onto the last old twin, 9,007 FDEs on an
identical libxul; and ICF's several FDEs per initial location were all
indexed where the linker indexes the first. The remaining gap to the
harness-native number was the correction's shape, not a stage, and is
closed by the split residual (`SPEC.md` G14).

## 0. Targets, and what the harness numbers actually are

| pair | table | harness, Zucchini stream | harness, native matcher | note |
|---|---:|---:|---:|---|
| Chrome 151.0.7922.169 → .173 (291 MB) | 2,634,264 | 2,634,264 (plan 1,244,060 + corr 1,434,428; 99.377 % correct) | **4,823,576** (plan 2,285,048 + corr 2,538,528; 98.389 %) — measured 2026-08-29, this tree | Zucchini 5,263,732 |
| libxul 154.0 → 154.0.1 (186 MB) | 4,063,404 | 4,063,404 (plan 1,178,352 + corr 2,885,052; 96.968 %) | 4,780,572 | with `delta/modal.go` on the Zucchini-stream prediction: 3,977,931 |

Three facts the table hides, all load-bearing for this spec:

1. **Both table numbers were produced with Zucchini's equivalence stream**
   (`-equivalence-patch`, `research/firefox-partial-mar.md` §"bottom line":
   the bold row is the Zucchini-stream row; `chrome-elf-handoff.md` §"How to
   run"). The module must use `presage/eqmatch` and never Zucchini, and the
   native matcher is +17.6 % on libxul and **+83 % on Chrome**. Matching or
   beating the table is therefore a matcher problem first and a port second
   (§4, track T3). The Chrome gap is not the equivalence stream alone
   (337,956 xz) — it is a worse prediction everywhere the layers project
   *through* the runs: `.rela.dyn` addends (reloc plan 942,096 vs 97,124),
   `.text` (2,226,012 vs 1,167,832).
2. The harness's correction number is `min(lz-shape xz, columnar xz,
   per-section split)` (`bench/elfpredict/main.go:145`, `correction.go:150`)
   and its plan is a separate `xz -9e` stream. The product ships one body —
   plan, prediction hash, residual — in `internal/cz` frames (raw / zstd /
   brotli-9, 16 MiB window, 8 MiB `FrameSize`), with
   `delta.EncodeCorrectionAdaptive` (plain, near, **modal**) as the residual.
   On the Chrome-native run the harness's split picked the lz shape on every
   cut and joint xz was +1.2 % over split; modal is worth −9.3 % on libxul.
   Net: the product's terminal stage is expected within ±3 % of the harness on
   the same prediction, slightly ahead on libxul.
3. Every stage the harness measures at `corrected-fields` is decoder-faithful
   (replayed from serialized plans and the old image only —
   `image.go:17`), **except** that every plan carries target-derived section
   geometry and counts it; that convention is kept (§2.8).

## 1. Decoder data flow at `corrected-fields`

`Materialise(refs, plan, length)` is `predictImage` (`image.go:17–189`) with
the Go-tables branch removed. Inputs: `old = refs[0]`, the plan. Output:
`length` bytes, length-exact. Stages, in order; each names what it reads from
the old image and what from the plan.

| # | stage | harness | reads | writes |
|---|---|---|---|---|
| 0 | parse the plan container; parse the equivalence header (`OldLen, NewLen, OldText, NewText`) | `image.go:18–30`, `equivalence.go:373` | plan | — |
| 1 | decode the structure: dense function map from boundary indices + extents guessed from old padding, then points and ranges | `plan.go:388, 428, 482`; `detectBoundaries :134`, `sourceExtents :164`, `referenceTargets :192` | old `.text` | `predictionPlan` |
| 2 | decode equivalences; sources of runs that start inside a mapped function come from the residual column against `srcPredictor.at` | `equivalence.go:269`, `:48` | structure | `[]equivalence` |
| 3 | lay: `out` zero-filled, `0xcc` inside new `.text`; copy every run whole-image | `image.go:50–58` | old, runs | all sections |
| 4 | build the oracles once: `addressLookup` over the structure (points → map → ranges), `sourceEquivalenceMapper` (runs de-overlapped by source, `project` extrapolates to nearest run) | `equivalence.go:500–575`, `:114–193`, `plan.go:562–646` | — | — |
| 5 | `.rela.dyn`: predict slot-gap column from the old table through the **pointer oracle**, apply gap correction (exact), predict addends by slot join, apply addend correction, predict tail, apply; `assembleRela` | `reloc.go:488, 299, 380, 391, 464` | old table, plan corrections | `.rela.dyn` |
| 5b | `.relr.dyn`: expand the **old** packed table, project each slot's file offset through the runs, write the pointer oracle's answer for the old value; the new packed table is left to the byte prediction |  new (`relr.go:77`) | old table, old section map | `.data.rel.ro` slots |
| 6 | DWARF field layer (unstripped ELF only): `dwarf.Apply` with the pointer oracle as `ptr` and `funcSizeDeltas(structure)` | `image.go:103–113`, `dwarf.go:75`, `presage/dwarf` | old debug secs, runs clipped per section | debug sections |
| 7 | `.eh_frame`: walk the **old** section's FDEs, project each `initial_location` field through the runs, retarget via the pointer oracle, fix `address_range` where old sizes agree, and rewrite each `cie_ptr` as the distance between the projected entry and its projected CIE |  `ehframe.go:155, 212` | old `.eh_frame`, section maps | `.eh_frame` |
| 8 | `.rodata` switch tables: enumerate candidate spans in the old section by signature (`roDataSpans`), apply only the variants whose `Keep` bit is set, retarget entries through the pointer oracle | `rodata.go:167, 199, 232` | old `.rodata`, `Keep` bits | `.rodata` |
| 9 | retarget `.text`: walk references in every mapped body of the laid prediction, resolve each field's old target through the **image oracle** (projection first inside `.text`), rewrite the displacement | `equivalence.go:616` | — | `.text` fields |
| 10 | per-function choice: structural prediction of old `.text` relocated through `addressLookup.target`, copy chosen bodies over the retargeted ones | `image.go:158–175`, `plan.go:669` | old `.text`, choice bits | chosen bodies |
| 11 | field fix, last: enumerate 4-byte displacement sites by walking the finished `.text`; apply address remaps (index into the sorted distinct-target domain + shift), then per-field deltas | `fieldfix.go:245, 42, 119` | plan columns | `.text` fields |

Then the core: `applyResidual` → `delta.ApplyFlaggedCorrection` (positional,
`Exact() == true`), prediction hash check, `Finalise` — rebuild
`.eh_frame_hdr` over the corrected `.eh_frame` where the plan's `HdrExact`
flag says the encoder checked that it is derivable — then the target hash
check (`presage/codec.go:157`).

Stages 5–8 are each conditional on their sub-plan being non-empty, exactly as
the harness treats every sub-plan as optional (`image.go:63, 103, 114, 143`).
Stage 10 is conditional on a non-empty choice stream; stage 11 on a non-empty
field stream. Stage 1 with zero mappings (no symbols, §3.4) leaves `Maps`
empty: stages 2, 9, 10, 11 take their no-map branches (`equivalence.go:269`
`pred == nil`, `:616` whole-section walk, `fieldfix.go:42` `len(maps)==0`).

**Layer order is not negotiable.** §13.1 of `chrome-elf-whole-image.md`
measured the alternatives; and stage 11 names fields by position in a walk
of the finished prediction, so every stage that can move a `.text` byte runs
before it.

## 2. Wire format

The module's plan is the harness's combined plan (`equivalence.go:686`)
re-cut as a fixed sequence of nine uvarint-length-prefixed streams, no
magic (the container's region header names the module), no "optional
trailing streams" convention (that existed to keep old measurements
byte-comparable; an absent layer is an empty stream):

```
plan := eq structure choices reloc ehframe rodata fields dwarf relr
        each: uvarint(len) bytes
```

Each stream keeps the harness serialization verbatim — these columns were
tuned against xz through eleven EPP revisions (`plan.go:15–41`) and the
`chrome-elf-whole-image.md` §12 sweep found nothing better — with the
changes listed.

| stream | format | transmitted | derived on both sides | change from harness |
|---|---|---|---|---|
| `eq` | `EQP2`: `OldLen NewLen OldText{Addr,Off,Size} NewText{…} predicted:u8` then streams `SrcSkip SrcResidual DstSkip CopyLen` (`equivalence.go:332`) | run columns, geometry | which source column each run uses (from the map) | drop the magic; keep the rest |
| `structure` | `EPPB`: `OldAddr NewAddr TargetLen mode:u8 n` + columns `srcIndexDelta srcOffset extentResidual sizeDelta startResidual copyBits`, then points `n idxDelta offset shiftDelta`, then ranges `n (oldΔ newΔ size)` (`plan.go:237`) | map columns, points, ranges | boundary list, extents, reference-target list (`plan.go:134,164,192`) | drop the magic; `mode` is `planDense` (0) or `planDerived` (1, §2.1) — `planSparse`/`planGoDerived` are not written and are rejected on read |
| `choices` | bitmap, one bit per mapping in destination order (`equivalence.go:856`) | | | none |
| `reloc` | `OldOff OldSize NewOff NewSize RelCount TailCount Anchor`, old and new section maps (`(addrΔ offΔ size)*`), streams `gap addend tail` (each a `delta.EncodeCorrection` stream), optional flags word (`reloc.go:184`) | geometry, three column corrections | predicted columns | `PairByRow`/`NoAddends`/`DerivedGeometry` flags are not written (experiment rungs); `HeadCount` kept behind its flag (Go-linker PIE tables) |
| `ehframe` | nine uvarints of geometry then the `HdrExact` byte (`ehframe.go:49`) | geometry, one flag | FDE list, `.eh_frame_hdr` contents | the flag is new: it says `Finalise` rebuilds the header after the residual |
| `rodata` | eight uvarints of geometry, `n Keep[n]` (`rodata.go:48`) | geometry, one bit per (span, variant) | candidate spans | none |
| `fields` | streams `RemapIndex RemapShift FieldIndex FieldDelta` (`fieldfix.go:89`) | | site list, remap domain | none |
| `relr` | two uvarints: the old `.relr.dyn`'s offset and size (`relr.go:30`) | geometry | the slot list, where each slot lands, what it points at | new |
| `dwarf` | `presage/dwarf` `Plan.Marshal()`; the runs are *not* carried (`Plan.MarshalRuns`) — the decoder clips the whole-image equivalences per section (`dwarf.go:48`) exactly as the harness does | | | none |

### 2.1 The derived function map (`mode = planDerived`)

`presage/elfmod/derived.go`, ported from the harness's `derived-map` rung
(`bench/elfpredict/derivedrung.go`, `derivedmap.go`;
`research/pgo-churn.md` §5.1c). The five map columns go out **empty** and a
delta stream sits immediately after the copy bitmap, where they were —
everything downstream is byte for byte what the dense form emits, because
the decoder reconstructs the same map and so derives the same
reference-target basis.

```
derived := Derived:u  Boundary Suppress SizeFixIdx SizeFixVal
           NewUnits:u Align:u Maps:u
           DropRuns OrderIndex OrderSrc SizeIndex SizeDelta
           InsertPos InsertSize FixIndex FixDelta Raw
           each column: uvarint(len) bytes
```

Both sides derive an ordered enumeration of old function starts from the old
image alone — call `rel32` targets, relocation addends landing in the window,
and `detectBoundaries`' padding rule — suppress the spurious entries with the
shipped bitmap, add the starts the derivation missed (`Boundary`), size each
entry by the padding rule with `SizeFix*` where the rule is wrong, then
replay the delta stream: drop runs, a positional walk with order exceptions,
size deltas, inserts and a layout replay at the shipped alignment. `Derived`
is the enumeration's length, so a divergent derivation refuses the plan
rather than decoding a shifted map. Nothing is carried between patches.

Two harness layers are **absent** by construction, not dropped: there is no
carried symbol table, so no insert carries a name hash, and no hash join, so
the correspondence-exception columns have nothing to say — the encoder codes
the shipped map's own correspondence directly against the positional cursor.
A window with no symbols, or one the unit model cannot express, falls back to
`planDense`; per-window, so a multi-window image may mix the two.

### 2.2 The displacement columns of the residual

`delta/dispfield.go` and `presage/split.go`, ported from
`bench/elfpredict/dispfield.go` and its `correction.go` changes. A columnar
piece of the split residual may carry four more streams — `Tags Idx Loc Far`,
piece kind 2 — holding every PC-relative field that lies wholly inside one of
the piece's long (5+ byte) wrong runs. The encoder zeroes those fields in the
byte column; the decoder places the replacement bytes, re-walks the repaired
buffer through `x86.WalkReferences` over the function map's bodies, and
refills each field from its class: an index into the new function-start
domain, a local displacement, or an absolute address. The module supplies the
context through `presage.FieldRefiner`, built from the old image and the
plan alone. A piece with no such field ships the seven columns it always did.

Section geometry appears in `eq` (text), `reloc` (every allocated section
with file bytes, both images), `ehframe`, `rodata` and `dwarf`. Old-side
geometry is derivable from the old image's section headers; it is ~1 KB
against a 2.6 MB patch and is kept as shipped so that the port is a copy,
not a redesign. Listed as a later saving, not part of this milestone.

Everything else the harness serialized — `plan.bin`, the derived/retarget
/selected artefacts, memo keys — is encoder-internal scaffolding and has no
wire form here.

## 3. Encoder

`Analyse(refs, target)` runs the harness's construction (`main.go:1140–1210`)
and `buildRungPlans` (`main.go:412–720`) restricted to the `corrected-fields`
path, then materialises its own plan and returns that prediction — never the
encoder's working copy — so `Materialise` is proven on every encode (as
`gomod.Analyse` does with `layer`).

Order (each step names its harness source):

1. Sections of both images (`elf.go:34`): allocated, non-TLS, non-zero,
   `Addr != 0`; `.text` required, else `ErrDeclined`. A Go binary that
   `gomod` accepted never reaches here (registry order, §6); one it declined
   is fine here.
2. Code units from the symbol tables (§3.4; `elf.go:122`) — both images.
3. `constructPlan` (`match.go:54`): name match with `chooseNameCandidate`
   (`:27`; size equal → `x86.Equal` canonical → byte-equal), then content
   match by `x86.ContentHash` verified with `x86.Equal`; `Ranges =
   sectionRanges` (`elf.go:73`). `inferReferencePoints` (`match.go:141`).
4. Equivalences: `eqmatch.Match(old, new, params)` whole image
   (`nativeeq.go:23`), `encodeColumns` (`equivalence.go:258`), marshalled
   against the map's `srcPredictor` (`equivalence.go:332`; `main.go:426`).
5. Two `.text` predictions: structural (`predictDecoded`, relocate, plan
   lookup) and retargeted (lay runs in `.text`, stage 9) →
   `chooseStructuralFunctions` with the `bytes` score (`equivalence.go:856`;
   `main.go:344`). The harness's `corr`/`fields` scores are dropped.
6. `buildRelocPlan` (`reloc.go:519`) with the pointer oracle over the map,
   when both images have `.rela.dyn`/`.rela` with ≥1 `R_X86_64_RELATIVE`
   entry (`elf.go:247`, `main.go:452–485`); otherwise a geometry-only reloc
   plan (`main.go:487`) — the later layers read the section maps from it.
7. DWARF plan (`dwarf.go:63`, `main.go:566–592`): `dwarf.Build` over the
   unallocated sections present in both images, `withRecords` as the
   harness (`main.go:578`), `addrMap` = pointer oracle. Absent when the pair
   has no `.debug_info`. Compressed debug sections are already expanded by
   the core (`delta.ExpandPair`, `codec.go:96`) before the module sees them.
8. `.eh_frame` geometry plan when both images have `.eh_frame` and the new
   one `.eh_frame_hdr` (`main.go:596–608`).
9. `.rodata`: predict once with everything above (stages 0–9 of §1 —
   the harness predicts the `modelled-eh-frame` rung, `main.go:628`), then
   `selectRoDataTables` (`rodata.go:260`) → `Keep`.
10. Fields: predict again with the rodata plan (`main.go:653`), then
    `encodeFieldFix(gate=false)` on the `.text` window (`fieldfix.go:132`).
11. Final: `Materialise` of the assembled plan → `pred`; return.

Three full predictions per encode (steps 9, 10, 11): ~2.2 s each on Chrome
(`chrome-elf-handoff.md` §"cost table"). The harness's stage timings, xz
probes, `planComponents`, memo and `-out` artefacts are not ported.

### 3.4 Symbols: encoder-only input

SPEC G7: correspondence is shipped, not recovered; the encoder holds the
unstripped artefacts. Package `presage/symbols`:

```go
// Funcs lists the function symbols of an image: address (virtual), size,
// name. Names are fingerprinted by the caller; the reader keeps none.
type Func struct{ Addr, Size uint64; Name string }
type Reader interface{ Funcs(visit func(Func)) error }

func Open(path string) (Reader, error)   // sniffs: Breakpad text ("MODULE "/"FUNC "), else ELF
func FromELF(f *elf.File) Reader         // .symtab STT_FUNC; falls back to .gopclntab (readPclntabFuncs) when there is no symtab
func FromBreakpad(r io.Reader) Reader    // "FUNC [m ]addr size paramsize name", hex, module-relative == vaddr for PIE
```

`elfmod.CodeUnits(r Reader, text Section) ([]CodeUnit, Stats, error)` is
`loadCodeUnits` (`elf.go:122`): group by address, max size, 16-byte name
fingerprints, clip to the next start / end of `.text`.

Module configuration, mirroring `gomod.Module{Stats}`:

```go
type Module struct {
    Symbols [2]symbols.Reader // old, new; nil = no symbols
    Params  eqmatch.Params    // zero = eqmatch.Defaults
    Stats   *Stats
}
```

CLI: `presage diff -symbols OLD,NEW old new -o patch`. Both paths sniffed
by `symbols.Open`; Chrome's are `symbols-<ver>/debug-info/chrome.debug`
(ELF, 1.45 GB each — the reader streams `f.Symbols()` and releases it, as
`elf.go:171–176`), Firefox's are the `FUNC` lines of `libxul.so.sym`
(`research/firefox-partial-mar.md` §6). `-symbols` with one path applies to
both images only if the user says so explicitly (`-symbols A,A`); with the
flag absent the module runs **without a map** (§1, zero mappings): whole-
image equivalences, section-range retargeting, the reloc/eh_frame/rodata
layers and the field fix still apply. That is the harness's
`equivalence-derived` rung plus the late layers, and it is the honest
symbol-less number, not a decline. `Analyse` records in `Stats.Notes`
whether symbols were used.

## 4. Equivalence source: `presage/eqmatch`, and the gap it must close

The module calls `eqmatch.Match` with `Module.Params` (default
`eqmatch.Defaults`, `Min 32 / Drop 4096` — the libxul harness row's
`-native-equivalences` with default min/drop, `firefox-partial-mar.md`
§6). No Zucchini anywhere in the encode path; `parseExternalEquivalence`
(`equivalence.go:209`) is not ported.

Measured cost of that decision, same tree, same plans otherwise:

| pair | Zucchini stream | native | gap |
|---|---:|---:|---:|
| Chrome 151 .169→.173 | 2,634,264 | 4,823,576 | +83.1 % |
| libxul 154.0→154.0.1 | 4,063,404 | 4,780,572 | +17.6 % |

`research/matcher-spike.md` §5 named the three things Zucchini's matcher
has that this one lacks and judged them "not worth doing on this
evidence" — the evidence being two Go pairs measured under
`-no-text-equivalences`, where the runs never touch `.text`. On a C++ image
the runs *are* the `.text` base and the projection every data layer
resolves pointers through, and the judgement inverts. Track T3 (§8) owns
closing it; the levers, in the order the spike ranked them:

1. **Reference-aware matching** in `.text`: compare and extend on
   `x86.Canonical` bytes (PC-relative fields zeroed) so two calls to the same
   moved function agree, then let stage 9 retarget the fields. Seeds hashed
   on canonical bytes; the extension scores canonical agreement.
2. **Anchor quality**: a second, longer seed table consulted first, or a
   suffix array over old (`+4 B/old byte`).
3. **Selection**: a pruning pass over candidate runs before the greedy scan
   commits.
4. Per-section `Drop`: the data sections are projected through
   (`project` extrapolates to the *nearest* run, `equivalence.go:176`), so an
   over-extended run in `.data.rel.ro` mis-places every pointer after the
   divergence; a smaller drop there is the cheap experiment.

Exit for T3 is measured with the harness, not the module:
`go run ./bench/elfpredict … -native-equivalences -rungs corrected-fields`
on both pairs, ≤ the Zucchini-stream number.

**Done** (`research/matcher-chrome.md`): Chrome 4,823,576 → 2,617,700
(Zucchini-stream re-run on the same tree 2,621,664), libxul 4,780,572 →
3,632,264. The ladder: tuning alone floors at 3,462,416 (drop 24 / min 24);
masking `.text` on `x86.Canonical` 3,120,372; `Params.Expect`, the
function map's predicted source, probed and near-tie-broken toward
2,945,952; a near search past the 32-candidate chain cap within 64 KB of
the expected source 2,788,040; `Params.MinFar` (a run far from the
expected source must be ≥ 96 B) 2,646,484; min 12 under that 2,617,700.
Lever 2 (suffix array) and lever 3 (global selection) were not needed.
`eqmatch.CodeDefaults` = `{Min 12, Drop 24, MinFar 96, Slack 16}` is what
the module uses; the Go DWARF path keeps `Defaults`.

## 5. Inventory: harness → product

| harness | lines | disposition | target |
|---|---:|---|---|
| `plan.go` types, `marshal`/`unmarshalPlan`, `readDenseMaps`, `readPointsAndRanges`, `detectBoundaries`, `sourceExtents`, `referenceTargets` + cache, `addressLookup`, `predictDecoded` | ~600 | move; drop `planSparse`, `planGoDerived`, `sparseStructure`, `derivedMap`, `Prior` | `presage/elfmod/structure.go` |
| `equivalence.go` `equivalencePlan`, `srcPredictor`, `sourceEquivalenceMapper`, `encodeColumns`, `decodeEquivalences`, `marshal`/`parse`, `sourceAt`, `read/writeDisplacement`, `oracleParts`, `retargetEquivalencePrediction`, `chooseStructuralFunctions` (`bytes` only), `wrongCount` | ~550 | move; drop `parseExternalEquivalence`, `readFixedStream`, `sparseWalkOffsets`, `predictEquivalences`, `predictCombined`, `withEquivalences`, all `probe*` | `presage/elfmod/equiv.go`, `oracle.go`, `retarget.go` |
| `match.go` | 223 | move | `presage/elfmod/match.go` |
| `elf.go` `section`, `image`, `loadImage` (from bytes, not path), `sectionRanges`, `codeUnit`, `loadCodeUnits`, `relaSection`; `isBreakpad`, `readBreakpadFuncs`, `readPclntabFuncs` | 271 | move / split | `presage/elfmod/image.go`; `presage/symbols/` |
| `image.go` `predictImage`, `funcSizeDeltas` | ~200 | adapt: becomes `Materialise`; drop `goMapDeriver`, `goGeometry`, `ApplyGoTables` branch, `noGoText`/`noDwarf` flags | `presage/elfmod/elfmod.go` |
| `reloc.go` | 561 | move; drop `PairByRow`/`NoAddends`/`DerivedGeometry`, `relocDiag`, `relocCodecStats` | `presage/elfmod/reloc.go` |
| `ehframe.go` | 245 | move; drop `reportEhFrame` | `presage/elfmod/ehframe.go` |
| `rodata.go` | 307 | move; drop `reportRoData` | `presage/elfmod/rodata.go` |
| `fieldfix.go` | ~310 | move; drop `probeFieldBases`, `gate` parameter (always false) | `presage/elfmod/fieldfix.go` |
| `dwarf.go` | 104 | move (adapter over `presage/dwarf`) | `presage/elfmod/dwarf.go` |
| `nativeeq.go` | 89 | fold into `Analyse` (three lines) | — |
| `parallel.go` | 59 | move | `presage/elfmod/parallel.go` |
| `main.go` construction (`:1140–1210`), `runCombined` (`:250–410`), `buildRungPlans` (`:412–720`) | ~500 | adapt: the `corrected-fields` path only, as `Analyse`; drop rungs, memo, `-resume`, `measure`, `attribute`, `planColumnCost`, reports | `presage/elfmod/encode.go` |
| `correction.go`, `packing.go`, `xz.go`, `timing.go`, `memo.go`, `attribute.go`, `instpos/instdiff/dispcol/immprobe/condprobe/valueprobe/orderprobe/rnfdict/operands.go` | ~3,500 | drop (measurement, probes, memo) | — |
| `*_test.go` `plan_test` round-trips, `reloc_test`, `ehframe_test`, `rodata_test`, `fieldfix_test`, `correction_test`, `breakpad_test` | ~1,100 | move the round-trip/replay tests; drop harness-only ones | `presage/elfmod/*_test.go`, `presage/symbols/*_test.go` |
| `delta/x86` (`Relocate`, `References`, `WalkBodies`, `ContentHash`, `Equal`, `Canonical`) | — | reuse as is | — |
| `presage/dwarf`, `presage/eqmatch` | — | reuse; T3 changes `eqmatch` internals, not its API | — |

Net: ≈ 3,300 harness lines become ≈ 2,600 product lines plus tests. The
harness keeps working throughout (it is not modified by the port); once the
gate is green the harness's whole-image rungs may be re-pointed at
`elfmod` in a follow-up, not now.

## 6. Decoder contract

`Materialise(refs, plan, length)`:

- Pure function of `refs[0]` and `plan` (SPEC §4.3); `length` must equal
  the equivalence header's `NewLen`, else `ErrCorrupt`. `Exact() == true`:
  the prediction is length-exact and the residual is the positional
  correction (SPEC §6.1, G9) — every layer writes into a pre-sized `out`.
  A region of this module is never length-declared.
- Every offset and length in the plan is bounds-checked before use, as the
  harness does (`image.go:70, 119, 147`; `plan.go:428–480`); a plan that
  fails a check returns `presage.ErrCorrupt`. The prediction hash catches
  what the checks let through.
- Parallelism: stages 9, 10, 11 walk one body per goroutine
  (`parallel.go`); output regions are disjoint and every lookup is a read,
  so results are order-independent. `runtime.GOMAXPROCS(0)` workers.
- Memory bound: `old` + `out` + structural `.text` prediction (`Text.Size`)
  + reference-target list (8 B × targets: 49 MB on Chrome) + the run and
  map tables. ≈ 2.9× the image on Chrome (≈ 850 MB); the structural
  prediction is freed after stage 10. The encoder additionally holds the
  target, the matcher's hash chain (4 B × `1<<26` + 4 B × old bytes ≈ 1.4 GB
  on Chrome) and two extra predictions: ≈ 6× the image.
- Time: decode ≈ 2.5 s on Chrome (harness `predict corrected-fields`
  2.24 s) plus the residual apply; encode ≈ 60 s cold (symbols 2 s, match
  2.3 s, points 3.5 s, `eqmatch` 9 s, reloc plan, three predictions, field
  fix 2.3 s, residual).

## 7. Gate

`presage/elfmod/corpus_test.go`, modelled on `presage/gomod/corpus_test.go`:

- `TestPairs`: for each of Chrome (`~/.cache/presage-chrome-zucchini/
  chrome-151.0.7922.{169,173}` with `symbols-…/debug-info/chrome.debug`) and
  libxul (`~/.cache/presage-pairs/libxul-154.0{,.1}.so` with
  `libxul-154.0{,.1}.funcs`): skip when any file is absent; `presage.Encode`
  with the ELF module and its symbols; `presage.Apply`; `bytes.Equal`.
  Assert the region's module is `elf`, and the patch size against two
  budgets: **parity** — ≤ 1.03 × the harness `-native-equivalences`
  `corrected-fields` number of the same tree (proves the port; today
  4,823,576 / 4,780,572), and **product** — ≤ the table (2,634,264 /
  4,063,404), which depends on T3 and is `t.Skip`-ped behind
  `PRESAGE_ELF_PRODUCT_GATE=1` until T3 lands, then made unconditional.
  Manual-run class (minutes); tagged like the Go corpus gate.
- `TestSelfPrediction`: each image against itself with its own symbols:
  `PredictErr == 0`, residual ≤ 64 B (as the Go gate).
- `TestNoSymbols`: libxul pair without `Symbols`; must round-trip; size
  recorded, not asserted.

Fast unit tests (<1 s each, `t.Parallel`), from the harness's own where they
exist: structure plan round-trip and truncation rejection
(`plan_test.go:15, 44`), equivalence plan round-trip, reloc column replay on
a synthetic table (`reloc_test.go`), `.eh_frame` walk + header rebuild + `cie_ptr` replay +
the `Finaliser` round trip (`ehframe_test.go`), RELR pack/parse round trip and slot replay
(`relr_test.go`), rodata span detection and `Keep` selection
(`rodata_test.go`), field-fix encode/apply round-trip (`fieldfix_test.go`),
Breakpad and ELF symbol readers on small fixtures, and a synthetic whole
module test: a hand-built two-function ELF-shaped byte image (no real
linker) through `Analyse` → `Materialise` → equality with the returned
prediction.

## 8. Work breakdown

The shared type file is written **first**, by the integrator, before the
tracks start, so that T1/T2 compile independently:
`presage/elfmod/types.go` = `section`, `image` (bytes-based), `mapping`,
`addressRange`, `addressPoint`, `equivalence`, `sectionMap` +
`offsetOf`/`addrOf`, `planReader`/`appendU`/`appendS`/`appendStream`,
`cmpU`, and the two oracle signatures `func(uint64) x86.Target` named
`ImageOracle`/`PointerOracle`. Straight copies from `plan.go`, `elf.go`,
`reloc.go:40–127`.

**T1 — structure, equivalences, oracles, `.text` (core).**
Files: `structure.go`, `equiv.go`, `oracle.go`, `retarget.go`, `match.go`,
`image.go`, `parallel.go`, `elfmod.go` (`Module`, `Materialise` stages
0–4, 9–10 with 5–8, 11 as calls into functions T2 provides — stubbed as
no-ops returning `nil` until T2 lands), `encode.go` steps 1–5, 11.
Interfaces out: `decodeStructure`, `decodeEquivalences`, `newOracleParts`
(`.image(rp)`, `.pointer(rp)`, `.sm`, `.lk`), `retargetText`,
`chooseStructuralFunctions`. Done-check: unit tests above for its files;
libxul pair without symbols round-trips through `presage.Encode/Apply`
(T4's CLI not needed — a test can call the API); with symbols via the API,
`Stats.PredictErr` on `.text` within 1 % of the harness's
`per-function-selection` rung (`-rungs text-ladder`).

**T2 — regenerators and the field fix.**
Files: `reloc.go`, `ehframe.go`, `rodata.go`, `fieldfix.go`, `dwarf.go`.
Depends only on `types.go` and `presage/dwarf`, `delta`, `delta/x86`.
Interfaces out: `buildRelocPlan/applyReloc`, `applyEhFrame`,
`selectRoDataTables/applyRoData`, `encodeFieldFix/applyFieldFix`,
`buildDwarfPlan/applyDwarf`, each taking `(out, old []byte, plan, oracle
funcs, mapper)` exactly as the harness signatures do. Done-check: the
harness's own unit tests pass moved over; a replay test per layer on a
synthetic image (encode → apply → equal).

**T3 — matcher parity (`presage/eqmatch`).**
Files: `presage/eqmatch/*` only; API unchanged (`Match`, `Params`,
`Encode`, `Decode`). Levers in §4 order; measure each with the harness on
both pairs (`-native-equivalences -rungs corrected-fields`, Chrome memo is
warm). Done-check: Chrome ≤ 2,634,264 and libxul ≤ 4,063,404 at the
harness, no regression on the Go DWARF pair (`prom-3.13.1-D` 323,744 ±
3 %) — the layered Go region uses the same matcher. Record the ladder in
`research/matcher-spike.md` §7.

**T4 — symbols, CLI, gate, docs.**
Files: `presage/symbols/*`, `cmd/presage/main.go` (`-symbols`, module
registration order §6, `-v` prints the ELF module's notes),
`presage/elfmod/corpus_test.go`, `docs/general/baselines.md` (Chrome and
libxul rows return only when the product gate is green, sourced from
`presage diff`), `README.md` table. Done-check: `symbols` unit tests on
Breakpad and ELF fixtures; `presage diff -symbols` on both pairs produces a
patch that `presage patch` applies byte-exactly.

**Integration.** Registry: `gomod.Registry()` becomes a `presage/modules`
(or `cmd`-side) registry that adds `gomod.Module{}` then `elfmod.Module{}`;
`ModuleELF = 4`. Candidate order is id order, so a Go binary is taken by
`go` first and `elf` sees only what `go` declined. Wire T2's functions
into T1's stages 5–8 and 11; run the parity gate; then T3's matcher under
the product gate; regenerate the two table rows from `presage diff`;
update `SPEC.md` §10 item 3 status.

## 9. Not in scope

Deriving old-side geometry (§2.8); re-pointing the harness at `elfmod`;
non-x86-64 ELF (`delta/x86` is the only relocator); PE/Mach-O (SPEC §10
item 6); the region DAG (SPEC §5.2) — the module is one region, one plan,
as `gomod` is; symbol-less matching by content only beyond what
`constructPlan`'s hash pass already does.
