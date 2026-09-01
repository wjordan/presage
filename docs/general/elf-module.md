# The ELF module: `presage/elfmod`

*Spec, 2026-08-29. Implementation-ready. The decoder-side pipeline of
`bench/elfpredict` at its default `corrected-fields` rung, as one presage
module behind the `Module` seam of `presage/module.go`, so that
`presage diff old new -symbols old.sym,new.sym` and `presage patch` cover a
non-Go ELF x86-64 image end to end. Design authority: `SPEC.md` §4–6, §10
item 3; template: `presage/gomod` (`presage-core.md` §4, §7).*

The container at this revision uses the compact CM model over bit-history states, balanced terminal compression, the
RELR slot layer, exact FDE CIE pointers, an exactly rebuilt
`.eh_frame_hdr`, cursor-placed `.rodata` switch tables, the operand-field
correction and plan columns coded as independent segments. Current end-to-end
sizes and resource measurements are in `baselines.md`; the status sections
below retain the measurements that motivated each layer.

## Status (2026-09-01f): the apply path's serial chain

Apply was 1.9× Zucchini's wall on libxul while spending 10.4 s of CPU to do
it: the cost was never arithmetic, it was a chain of stages that each waited
for the one before. An env-gated timeline (`internal/trace`, off unless
`PRESAGE_TIMING` is set, one boolean test when off) named the chain, and
seven changes shortened it. Six are byte-identical; the two segmentation
levers buy their time with plan bytes.

| stage | before | after |
|---|---:|---:|
| readBody | 0.016 | 0.015 |
| **Materialise** | **1.427** | **0.973** |
| unpackPlan | 0.485 | 0.205 |
| planMaps | 0.225 | 0.212 |
| decodeEquivalences | 0.006 | 0.007 |
| layImage | 0.105 | 0.084 |
| oracleParts | 0.023 | 0.024 |
| applyRelr | 0.027 | 0.027 |
| applyEhFrame + applyRoData | 0.120 | 0.096 ∥ |
| retarget | 0.061 | 0.061 |
| choices | 0.018 | 0.018 |
| textWalk | — | 0.082 |
| fieldFix | 0.178 | 0.096 |
| opField | 0.151 | 0.057 |
| predictionHash | 0.006 | 0.005 |
| **applyResidual** | **0.340** | **0.236** |
| Finalise | 0.022 | 0.023 |
| hashOf(out) | 0.040 | 0.040 |
| write | 0.051 | 0.060 |
| **total wall / CPU** | **1.903 / 10.40** | **1.354 / 10.28** |

Seconds, one traced run each on the libxul pair. CPU is process-wide, so a
concurrent stage's column counts everything that ran while it was open;
that the total barely moves is the point — almost none of this is less
work, it is the same work off the critical path.

The levers, each measured as the median of five warm applies of a freshly
encoded patch:

| lever | libxul apply | patch |
|---|---:|---:|
| base | 1.85 s | 2,092,567 |
| long plan columns coded as independent segments (128 KiB) | 1.67 s | +1,872 |
| one instruction walk per window for both field layers | 1.63 s | byte-identical |
| residual pieces applied concurrently | 1.58 s | byte-identical |
| plan segments cut at 64 KiB | 1.48 s | +2,427 |
| image and its disjoint layers laid concurrently | 1.44 s | byte-identical |
| old text's call targets walked in 1 MiB chunks | 1.44 s | byte-identical |
| window walk sharded by bytes, not by map count | 1.41 s | byte-identical |

1. **The plan decode is its longest column, not its total.** Twelve plan
   columns already decoded concurrently, so `unpackPlan` finished when the
   281 KB one did — 0.30 s solo, with eleven others idle around it. A worker
   sweep confirmed the ceiling is the chain, not bandwidth: 1 → 12 workers
   moved aggregate throughput 1.11 → 3.91 MB/s. A column over `planSegMax`
   is now coded as evenly sized segments cut forward to the next LEB128
   value boundary, each an independent chain, and a paired span names its
   index column's whole span range plus the record its own values start at,
   so index columns segment too. The cost is one adaptive-model restart per
   segment. 128 KiB paid 1,872 B for 0.18 s; 64 KiB paid a further 2,427 B
   for 0.10 s. 32 KiB was measured and dropped: it costs a further 3,385 B,
   putting the pair 0.367% over base and outside the size budget, and its
   apply median (1.36 s) is inside the noise of 64 KiB's. Segment sizes
   targeted per stream, giving the long-pole column
   more cuts and the rest fewer, were also measured and lost (1.72 s):
   `unpackPlan` splices the whole plan before any stream is read, so slack
   in a late stream is not exploitable.
2. **One instruction walk per window.** The field fix and the operand-field
   layer each walked every mapped body with the same length decoder, for two
   facts that fall out of one pass — the four-byte PC-relative sites, and how
   many operand fields of each class each body holds. Neither depends on the
   values in those fields, so a single walk taken before the field fix still
   describes the window the operand layer sees after it. `x86.WalkAll` emits
   both, and `TestWalkAllMatchesBothWalks` asserts it yields exactly the
   references `WalkReferences` yields and exactly the `(start, Fields, ok)`
   triples `WalkFields` yields, over random bytes and over real `.text`.
   The site list is 5,050,281 entries on libxul and is dropped as soon as
   the field fix has read it; holding it through the operand layer cost more
   in GC than the walk saved.
3. **Disjoint layers run together.** `.eh_frame` and `.rodata` write
   different byte ranges of the image, and `layImage`'s equivalence runs are
   disjoint by construction. Neither is assumed: `disjointRanges` tests the
   plan's ranges pairwise before `runLayers` starts anything, so a plan that
   overlaps them falls back to running them in order.
4. **Residual pieces run together.** A piece decodes and applies over its own
   span of the output and reads nothing another piece writes; ten small
   pieces were waiting behind the one that carries most of the correction.
   Errors are collected by index and reported in piece order, so a corrupt
   patch draws the same error every run.
5. **Shard by bytes.** Mapped bodies differ in size by orders of magnitude,
   and both the window walk and the equivalence copy were split by count, so
   the machine waited on whichever shard drew the big ones. The old text's
   call-target walk was chunked at 8 MiB, which on a 100 MB `.text` leaves
   twelve chunks for twenty-four cores; 1 MiB fills them.

End to end, with every apply `cmp`-verified against the target:

| pair | patch before | patch after | apply before | apply after |
|---|---:|---:|---:|---:|
| prometheus 3.13.1 → 3.13.2 | 74,636 | 74,636 | 0.81 s | 0.79 s |
| Chrome 151 .169 → .173 | 2,257,676 | 2,263,302 (+0.249%) | 3.22 s | 2.72 s |
| libxul 154.0 → 154.0.1 | 2,092,567 | 2,096,866 (+0.205%) | 1.86 s | 1.37 s |

Apply times are medians of three warm runs, the two builds measured back to
back on a shared host at load 10. Peak apply RSS is flat within noise:
libxul 377,724 → 383,960 KiB (+1.7%), Chrome 685,732 → 675,836 KiB,
prometheus 400,972 → 401,664 KiB. Encode is unchanged (libxul 23.9 →
23.1 s). The Go module's plan has no column long enough to segment, which
is why prometheus does not move at either end.

Two candidates were rejected. Parallelising the container hash of the
finished output (0.040 s, 3%) needs a BLAKE3 subtree merge the library does
not export, and overlapping it with the write would break the contract that
nothing reaches the writer until the output and the reference check have
both passed. Shrinking the coder's bank tables to cut the decode's cache
miss rate was dropped once the worker sweep showed the plan decode is
latency-bound on one chain rather than bandwidth-bound.

What still stands between 1.35 s and Zucchini's 0.97 s is the head of the
pipeline: `unpackPlan` (0.205) and `planMaps` (0.212) are strictly ordered,
because the plan is spliced whole before any stream is read and the section
maps are what every later stage addresses through. Overlapping them means
letting a stream be decoded and consumed before its neighbours arrive —
a streaming plan layout, not a splice — which is a wire-format change, not
a scheduling one.

## Status (2026-09-01e): two `.rodata` plan ideas, both measured and dropped

Two follow-ups to the cursor placement below were built and priced against
the corpus. Neither is in the tree; this section is the measurement, so the
next reader does not pay for them again.

**1. Re-basing a segmented span per segment.** A base-relative span is
re-based against one base word, the span's own. The hypothesis was that a
span segmented into per-function tables holds several bases -- one per table
-- so a single base reads every table after the first against the wrong
origin. The premise does not hold where it would have paid: of libxul's 11
segmented spans, 8 are self-relative and hold 99.97% of the segmented words
(the 43,128- and 70,165-entry spans among them), and a self-relative entry
never reads a base at all. The three base-relative ones are 16, 16 and 11
words. Chrome is the other way round -- 211 of 248 segmented spans are
base-relative -- but they are small.

Implemented as zero plan bytes (`segmentSpan` walks with the base moving to
each new table, `retargetTable` and `chooseCursors` read every table against
its own first word):

| pair | patch | `.rodata` wrong | `rodata` plan | segmented | corrections |
|---|---:|---:|---:|---:|---:|
| libxul, before | 2,148,134 | 193,964 | 153,675 | 11 | 9,544 |
| libxul, after | 2,148,134 (0) | 193,964 | 153,675 | 11 | 9,544 |
| Chrome, before | 2,305,394 | 218,045 | 29,671 | 248 | 2,592 |
| Chrome, after | 2,304,725 (−669) | 216,103 | 29,500 | 282 | 2,388 |

libxul does not move by a byte, which is below the 5 KB the change had to
earn, and Chrome's 669 is not a correct model paying off -- it is the
encoder finding a different local optimum. A round-trip test over a
base-relative fixture of four per-function tables shows why the model cannot
be right: **the segmentation and the base are circular.** A boundary is
found by watching the owning function change, which needs a base to read the
entries with; the base is the boundary. Reading the first entry of the next
table against the *previous* table's base still lands in that table's
function -- functions are far larger than the offset between the two bases
-- so the boundary is detected one or more entries late and every table
after it is re-based off its true start. Recovering the true start needs
either a search that mis-splits the genuine single-base spans (the common
case) or a shipped base per segment, and Chrome's whole ceiling here is
~700 bytes before paying a single varint. Dropped.

An ablation separated out the one clean part of the change -- not requiring
a span's base word to be placeable when the span is self-relative and never
reads it. Chrome is byte-identical with and without it, so the guard never
fires; it is not worth the line either.

**2. Sparse `Keep`.** The `Keep` bitmap is one bit per (span, variant) over
every candidate span, and almost all of it is zero: libxul sets 201 bits in
144,063 bytes, Chrome 3,562 in 26,746. It looks like 144 KB of waste. It is
not, because the plan is compressed, and a bitmap that is 99.98% zero is
what a terminal compressor is best at. Every sparse form was priced against
`cz` (min of raw/zstd/brotli, the same compressor the plan column gets):

| form | libxul raw | libxul cz | Chrome raw | Chrome cz |
|---|---:|---:|---:|---:|
| dense bitmap (shipped) | 144,063 | **190** | 26,746 | **1,384** |
| gap varints over set bits | 231 | 174 | 3,685 | 1,491 |
| gap varints over spans + variant bytes, two columns | 415 | 135 + 58 | 7,160 | 912 + 521 |
| the same two concatenated | 415 | 174 | 7,160 | 1,421 |
| one varint per kept span, gap and variant packed | 231 | 173 | 3,684 | 1,360 |
| gap varints over non-zero bytes, with the byte | 415 | 183 | 7,160 | 1,514 |

The whole column costs 190 shipped bytes on libxul and 1,384 on Chrome, so
no re-encoding of it can save 1 KB on libxul at all, and on Chrome the best
sparse form measured is 24 bytes better than the bitmap. There is no
decode-side case either: `bitSet` is an index into a slice, and 144 KB
against a 1.5 MB plan and a 1.8 GB encode is nothing. The only real cost the
dense form carries is encoder-side, where `packPlan` offers every column over
4 KB to the CM coder and spends about a tenth of a second coding 144 KB of
zeros it will never carve. Dropped; the format is unchanged and
`presage.Version` is unchanged.

## Status (2026-09-01d): the operand-field correction

The field fix below corrects four-byte PC-relative displacements, because
those are the fields the retargeter writes. Everything else inside an
instruction it hands to the byte correction, and §11.4 of
`chrome-elf-whole-image.md` measured what that costs: 31.1% of Chrome's
`.text` residual sits in instructions the prediction placed on the right
boundary, with the right opcode and the right length, and filled in with the
wrong value. §11.4's own probe priced the scalar half of that at −53,782.

`presage/elfmod/opfield.go` ships it. The domain is every instruction the
length decoder finds in a mapped body of the prediction, split into one list
per **field class** -- immediate, branch displacement, and a displacement based
on rsp/rbp, on another register, on rip, or on nothing -- and an entry is a
gap in one of those lists plus one signed varint. The decoder holds the
prediction and the map before any of this is applied, so it enumerates the
same lists; field values do not change instruction lengths, so the lists are
the same before and after the layer rewrites one. Two columns per class, an
index and a value, carved into the plan packer like every other column.
A stream count is not self-announcing, so a build from before this change
reads the tenth stream as trailing data; the format is pre-release and
`presage.Version` bumps only per release (container.go).

Three things decide what it is worth.

**The field layout comes from the length decoder, not from a disassembler.**
`delta/x86/lendec.go` already located the displacement and read the immediate
width on its way to the length; `Fields`, `FieldsAt` and `WalkFields` return
what it passed over instead of discarding it, and the walk costs one decode
per instruction rather than a walk plus a lookup. The probe used
`x86asm`, whose apply cost §11.4 measured at 1.74 s against the length
decoder's 0.14 s. The price of that is coverage: where the tables defer --
vector prefixes above all -- the instruction has no locatable field and joins
no domain, which is why this finds 63,684 rsp/rbp corrections where the probe
found 69,083.

**An entry exists only where the fields explain the whole instruction.** The
encoder writes the target's field bytes into the prediction's instruction and
keeps the entry only if the result *is* the target, byte for byte. An
instruction that differs anywhere else is a different instruction, not a
different value, and belongs to the residual. Nothing on the decode side reads
the target or re-decodes anything: it adds the value to the field and writes it
back.

**Each class prices itself, and most of them lose.** A class ships only where
its two columns, compressed, cost less than the wrong bytes they take out of
the residual -- `.rodata`'s rule for a cursor correction, with the column
priced the way it ships. On the corpus:

| class | Chrome entries | column | wrong bytes | libxul entries | column | wrong bytes |
|---|---:|---:|---:|---:|---:|---:|
| immediate | 48,919 | 86,335 | **93,560** ✓ | 12,145 | 29,710 | 16,167 ✗ |
| rsp/rbp displacement | 63,684 | 48,830 | **67,806** ✓ | 127,589 | 91,786 | **141,221** ✓ |
| branch displacement | 25,217 | 44,872 | 25,217 ✗ | 8,287 | 15,974 | 8,287 ✗ |
| other-register displacement | 4,802 | 8,435 | 5,614 ✗ | 2,264 | 5,450 | 2,682 ✗ |
| baseless displacement | 32 | 87 | 34 ✗ | 11 | 44 | 11 ✗ |
| rip displacement | 0 | — | — | 0 | — | — |

The rip class is empty by construction: the field fix has already made every
four-byte PC-relative displacement in a mapped body exact, so what reaches
this layer is the fields it does not name. The branch class is what is left of
them -- one-byte `rel8` displacements -- and every one of its entries fixes
exactly one byte, which no column can carry for less. The immediate class
carries the new *value* and not the difference, because an immediate that
changed is a constant and not an offset: on Chrome that is worth 15,490 over
the difference basis, and on libxul it is still not enough. Forcing every
class on regardless costs Chrome 692 bytes over letting them price
themselves, so the rule is not merely conservative.

| pair | patch before | after | mispredicted `.text` | opfield stream | apply |
|---|---:|---:|---:|---:|---:|
| Chrome 151 .169 → .173 | 2,305,394 | **2,257,676** (−47,718) | 1,219,717 → 1,058,351 | 325,279 B raw | 3.06 → 3.29 s |
| libxul 154.0 → 154.0.1 | 2,148,134 | **2,092,567** (−55,567) | −141,221 | 272,804 B raw | 1.80 → 1.83 s |
| prometheus 3.13.1 → 3.13.2 | 74,636 | 74,636 (0) | — | — | — |

Applied and `cmp`-verified through the CLI on all three. The plan grows by
208,424 on Chrome and the residual falls by 173,018; the terminal compressor
turns that into −47,718, 2.1% of the patch. Encode is 29.2 → 32.2 s on Chrome
and unchanged on libxul; apply pays about 0.3 s on Chrome for the walk that
enumerates the domain -- one pass over 225 MB of `.text` to count the
per-class lists, and a second that touches only the bodies holding an entry,
112,603 of 925,344 -- at no cost in peak RSS (673,656 → 660,468 KiB).

The register and structural classes the same domain could carry are not here
and should not be: §11.4 measured them as a net loss, because a modrm byte
three bits from the prediction's is exactly what the residual coder's
prediction context codes cheaply, while a column replacing it would have no
prediction under it.

## Status (2026-09-01c): `.rodata` tables placed by cursor

(Measured on the branch before the CM engine below; the two land together.)

`.rodata`'s residual was not the spans the encoder dropped -- it was the ones
it kept. Of libxul's 201 kept spans, 335,856 bytes were still wrong, and two
of them account for most of it: spans of 43,128 and 70,165 entries lying in a
453,499-byte hole the byte matcher cannot match, because every entry is a
target minus its own address and the whole region churns when `.text` moves.
`retargetTable` placed each entry at `mapper.project(entry)`, which across a
hole extrapolates a flat shift from the nearest run; the true shift walks
+16 … +528 across 512-word blocks, so 61 of 98,455 entries landed in the
right slot. The span stayed "kept" only because wrong slots share high bytes.

The region is a concatenation of per-function clang switch tables in link
order, and that order survives. So the layer now **segments a kept span from
the old image alone** -- consecutive entries whose targets lie in the same old
function are one table (`segmentSpan`, `rodata.go:387`, over the new
`codeLookup.unitAt`) -- and **places by cursor**: table *t* is predicted at
the end of table *t−1*, plus one signed correction the plan carries
(`placeTables`, `rodata.go:361`). The cursor is right wherever the table
sequence is unchanged, which is nearly all of it, so a table inserted or
deleted in the new image costs one varint and not one per table after it.
Both the correction and the cursor are clamped to the new section, so no plan
can make a write leave it. The encoder chooses each correction against the
target (`chooseCursors`, `rodata.go:542`): the cursor itself, the byte
matcher's opinion, four words either side, and -- for a long self-relative
span -- an index of what each new-side word resolves to, which finds a table
that moved further than the local scan reaches. One bit per span says which
arm is used, and the encoder sets it only on measured gain, so a span the
projection already places is untouched and the byte count that keeps a span
now reflects the placement it will actually get.

| pair | patch before | after | `.rodata` wrong bytes | spans kept | segmented | corrections |
|---|---:|---:|---:|---:|---:|---:|
| libxul 154.0 → 154.0.1 | 2,337,304 | **2,179,781** (−157,523) | 390,097 → 193,964 | 201 → 201 | 11 | 9,544 (9,559 B) |
| Chrome 151 .169 → .173 | 2,346,975 | **2,331,752** (−15,223) | 248,444 → 218,045 | 3,553 → 3,562 | 248 | 2,592 |

Chrome has the same mechanism much milder -- its largest span is 10,995 words
and the projection already recovered most of it -- and nine spans that were
worth nothing under the projection are now worth keeping. Applied and
`cmp`-verified through the CLI. Encode is within noise on a loaded box
(libxul 23.1 → 23.5 s, Chrome 31.5 → 29.1 s) and peak RSS does not move
(1,801,132 → 1,801,068 KiB; 2,810,268 → 2,811,880 KiB); apply is 1.46 → 1.47 s
and 2.85 → 2.81 s. The plan pays 9,581 bytes on libxul and 2,889 on Chrome:
the corrections, plus the list of segmented span indexes, which is a list and
not a bitmap because eleven spans out of 144,063 are.
## Status (2026-09-01b): the CM engine, second pass

The coder's slots hold a zpaq-family bit history under a per-bank state map
instead of a 16-bit counter, and an lpaq SSE chain refines the mixer's
output (`delta/cmcoder.go`; `research/pgo-churn.md` §8, "The engine,
revisited"). Same contexts, same side information, same codec ids — the
engine behind them is stronger. Chrome 2,346,975 → **2,320,568**
(−26,407), libxul 2,337,304 → **2,282,523** (−54,781), applied and
`cmp`-verified. Apply pays for it: Chrome 2.68 → 3.05 s, libxul
1.36 → 1.71 s (median of 3 under load 14–16). Encode and peak RSS are flat
at both ends; decoder table memory per large stream falls, since a slot is
one byte rather than three. `presage.Version` is unchanged: a
pre-release format is rewritten in place.

## Status (2026-09-01): three derivable fields outside `.text`

Three fixes, all decoder-derivable, none costing more than a flag.
The plan gains a ninth stream and the `eh_frame`
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

Two changes to the plan (research/pgo-churn.md §8.2).

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
6,564,155. Both changes alter the wire format in place.

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
| 8 | `.rodata` switch tables: enumerate candidate spans in the old section by signature (`roDataSpans`), apply only the variants whose `Keep` bit is set, retarget entries through the pointer oracle; a span the plan lists in `Seg` is segmented into its per-function tables from the old image and placed by running cursor plus one correction each, instead of by projection | `rodata.go:175, 208, 241, 361, 387` | old `.rodata`, `Keep` bits, `Seg`/`Cursor` | `.rodata` |
| 9 | retarget `.text`: walk references in every mapped body of the laid prediction, resolve each field's old target through the **image oracle** (projection first inside `.text`), rewrite the displacement | `equivalence.go:616` | — | `.text` fields |
| 10 | per-function choice: structural prediction of old `.text` relocated through `addressLookup.target`, copy chosen bodies over the retargeted ones | `image.go:158–175`, `plan.go:669` | old `.text`, choice bits | chosen bodies |
| 11 | field fix: enumerate 4-byte displacement sites by walking the finished `.text`; apply address remaps (index into the sorted distinct-target domain + shift), then per-field deltas | `fieldfix.go:245, 42, 119` | plan columns | `.text` fields |
| 12 | operand-field correction, last: walk the same bodies again, count each field class's domain, and add the plan's signed value to the field an entry names | `opfield.go:108, 306`, `lendec.go:69` | plan columns | `.text` immediate and displacement fields |

Then the core: `applyResidual` → `delta.ApplyFlaggedCorrection` (positional,
`Exact() == true`), prediction hash check, `Finalise` — rebuild
`.eh_frame_hdr` over the corrected `.eh_frame` where the plan's `HdrExact`
flag says the encoder checked that it is derivable — then the target hash
check (`presage/codec.go:157`).

Stages 5–8 are each conditional on their sub-plan being non-empty, exactly as
the harness treats every sub-plan as optional (`image.go:63, 103, 114, 143`).
Stage 10 is conditional on a non-empty choice stream; stage 11 on a non-empty
field stream; stage 12 on a non-empty operand-field stream, which is what a
pair whose classes all price out ships. Stage 1 with zero mappings (no
symbols, §3.4) leaves `Maps` empty: stages 2, 9, 10, 11 take their no-map
branches (`equivalence.go:269` `pred == nil`, `:616` whole-section walk,
`fieldfix.go:42` `len(maps)==0`), and stage 12 has no bodies to enumerate.

**Layer order is not negotiable.** §13.1 of `chrome-elf-whole-image.md`
measured the alternatives; and stages 11 and 12 name fields by position in a
walk of the finished prediction, so every stage that can move a `.text` byte
runs before them. Stage 12 runs after stage 11 and enumerates what stage 11
wrote: the encoder replays the field fix through the decoder's own apply
before it looks for operand fields, so both sides read the same bytes.

## 2. Wire format

The module's plan is the harness's combined plan (`equivalence.go:686`)
re-cut as a fixed sequence of ten uvarint-length-prefixed streams, no
magic (the container's region header names the module), no "optional
trailing streams" convention (that existed to keep old measurements
byte-comparable; an absent layer is an empty stream):

```
plan := eq structure choices reloc ehframe rodata fields dwarf relr opfield
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
| `rodata` | eight uvarints of geometry then three streams, `Keep Seg Cursor` (`rodata.go:53`) | geometry, one bit per (span, variant); the segmented spans as ascending index deltas; one signed varint per table of those spans | candidate spans, their segmentation | `Seg`/`Cursor` are new. `Keep` stays a dense bitmap: it is 99.98% zero and compresses to 190 B on libxul and 1,384 on Chrome, which every sparse form measured ties or loses to (2026-09-01e) |
| `fields` | streams `RemapIndex RemapShift FieldIndex FieldDelta` (`fieldfix.go:89`) | | site list, remap domain | none |
| `opfield` | one stream per code window: a class mask byte then, per class the mask names, an index column of gaps in that class's field domain and a column of signed values (`opfield.go:289`) | which classes ship, and their two columns | the per-class field domains, from a walk of the prediction | new |
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
   `selectRoDataTables` (`rodata.go:419`) → `Keep`, `Seg`, `Cursor`. Each
   span is scored under the projected placement in every variant, then the
   long ones under the cursor placement as well; the arm with the larger
   measured gain, net of what its corrections cost the plan, wins.
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
(`relr_test.go`), rodata span detection, `Keep` selection, span
segmentation and cursor placement across an inserted and a deleted table
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
