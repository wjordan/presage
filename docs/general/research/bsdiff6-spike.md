# bsdiff 6, measured: what of Percival's encoder is worth porting

Status: spike, 2026-08-29. Companion to `percival-thesis.md`, which reads the
thesis; this measures its encoder's ideas inside presage's own coders and
records which survive. Every number below decodes: each shape is replayed and
compared byte for byte against the target before it is reported.

Harness: `delta/bs6spike_test.go` (the correction), `delta/bs6lz_spike_test.go`
and `delta/bs6dstr_spike_test.go` (the `lz` module), `bench/bs6probe` (a
standalone run-shape probe). Run with `BS6_PRED`/`BS6_TARGET` or
`BS6_OLD`/`BS6_NEW`; `BS6_DUMPDIR` writes the winning shape's streams and
`BS6_PRICE` prices a directory of them.

§2.4 and §2.5 are later pressure tests of §2.1–2.3 and supersede several of
their numbers; where they disagree, the later section is the measurement.

## Bottom line

One change is worth having, and it is worth about a tenth of the patch on a
non-Go binary. Everything below is one measurement of the correction stream;
this table converts to whole patches, holding everything but the correction
constant.

| | `p2` prometheus 3.13.1→2 | `libxul` 154.0→154.0.1 |
|---|---:|---:|
| xdelta3 -9 | 15,194,011 | 31,118,515 |
| `zstd --patch-from -19 --long=31` | 8,479,550 | 26,326,367 |
| bsdiff 4.3 | 2,691,644 | 12,348,560 |
| presage before this work | 74,177 (36.3× bsdiff) | ~4,387,547 (2.81×) |
| **presage with `delta/modal.go`** | **70,195 (−5.4 %)**, 38.3× | **3,977,931 (−9.3 %)**, 3.10× |

Built and shipped 2026-08-29, not projected: the correction goes 43,936 →
39,673 on `p2` and 3,208,624 → 2,799,008 on `libxul`, both replayed byte for
byte. See `SPEC.md` §6.1 for the format and G13 for the decision. (Both rows
are re-measured against the build that also made `internal/cz` use brotli
quality 11 on every stream rather than dropping to 10 above 4 MiB. That change
is worth 2.5 % of `libxul`'s correction on its own and shows up here as a
better *baseline*, which is why the modal gain reads narrower than the
−14.4 %/−10.6 % first measured against the quality-10 tier.)

Note how differently the same change lands. On a Go binary the Go module has
already predicted the pointer tables structurally, so the residual is thin and
the gain is 5 %; on a shared object with no such module the tables are the
residual, and it is 9.3 %. The general codec is where this pays.

`libxul` has no shipped patch number, so its denominator is
`bench/elfpredict`'s `corrected-fields` rung: the plan compresses to 1,178,923
and the correction to 3,208,624. `p2`'s 74,177 is the real patch with the
modal candidate ablated out of the encoder, of which the correction is 43,936;
with it the patch is 70,195.

The change is: write a differing region's bytes as balanced base-256 digits of
`want-pred` instead of as literals, let each region name which transform and
width it used, and give each of those a sub-stream. About 200 lines. The
encoder is *cheaper* than the one that ships (one compression where the shape
picker does two); the decoder is a byte loop, 24.9 ms on a 186 MB file.
Everything else in Percival's encoder measured neutral or negative — details
in §6.

## 0. The claim

Percival's released bsdiff is 4.3; his thesis (ch. 2) describes a successor
he calls bsdiff 6 and reports it well ahead of the field — 1.27 % of the target
on the 82-file FreeBSD security corpus against .RTPatch's 1.89 % and Xdelta's
9.28 %. The public claim attached to it is that it is roughly 20 % better than
the open-source 4.3. Four things distinguish it from 4.3:

1. **Four difference-string modes** — bytewise, little-endian multiprecision
   balanced, big-endian multiprecision, and a correction map — tried per file,
   smallest kept (§2.7).
2. **The difference map / non-zero split** — one bit per matched byte saying
   *where* the changes are, coded apart from the values saying *by how much*
   (§2.7).
3. **Block alignment with mismatches** (ch. 1) feeding a **pruned dynamic
   program** over 64 candidate alignments per byte, in place of 4.3's greedy
   scan (§2.4–2.6).
4. Sub-linear-memory alignment, which is a memory result, not a size one.

presage has none of 1–3. This spike prices all three.

**The claim's provenance matters, because it decides what is worth porting.**
`docs/research/bsdiff-percival.md` establishes four things that reframe it:

- Percival named the tool **bsdiff 6.0**, in a "Note to corporate users" that
  stood on daemonology.net from 2004-12 to 2005-06 and was then deleted. It
  claimed **25 % smaller than bsdiff 4.2** and that Oxford had claimed the
  software. The surviving "roughly 20 %" wording names no baseline. Neither
  number was ever backed by a published measurement.
- The **only independent, controlled measurement** is CISPA's SpaceSec 2026
  reimplementation, built from Percival's actual C prototype: **≈4.8 %**.
- But that reimplementation is deliberately `BSDIFF40`-format-compatible, so it
  has bsdiff 6's *alignment* and **none of its encoding** — no multiprecision
  differences, no map split, no BWT, no varints. Its corpus is also ~8
  executables out of 19 pairs, the rest PNG/JPEG/xz/disk images.
- So **bsdiff 6's encoding work has never been measured by anyone**, and the
  4.8 % result relocates most of the expected gain into exactly the part
  nobody has tested. That is what §2 measures, and it is why the finding here
  is an encoding one.

## 1. Where each idea lands in presage

| bsdiff 6 idea | presage's coder | today |
|---|---|---|
| difference-string modes | positional correction, `delta/correct.go` | literals, or `want^pred` |
| difference map split | `lz` difference runs, `delta/plain.go` | `(gap, len, values)` interleaved |
| DP match selection | `lz` greedy scan, `delta/plain.go` | bsdiff 4.3's scan verbatim |

Three pairs carry the measurements. `syn` is the 30 MB one-line-change pair
(`bench/build.sh` v1 → v2c, Go 1.27); `p2` is prometheus 3.13.1 → 3.13.2
stripped, 94 MB, the patch release; `p14` is prometheus 3.13.2 → 3.14.0,
97 MB, the minor release. Reference points: bsdiff 4.3 gets 150,475 on `syn`
and 2,691,644 on `p2`; presage ships 1,202 and 70,195.

## 2. Difference-string modes in the correction — **the find**

The positional correction writes a differing region either as an lz stream
over the prediction's own bytes or as literals, and the literals are the
target's bytes (the shipped shape) or `want^pred` (the near-miss shape,
research 14.5). bsdiff has written `want-pred` since 4.3, and bsdiff 6 adds
the two multiprecision modes; presage has neither.
`delta/bs6spike_test.go` adds both to the real encoder, so the lz-region
option that carries the long runs is still in play, and replays every shape
before reporting it.

### 2.1 On Go binaries, `sub` wins and multiprecision barely shows

`p2`, prediction 93,769,955 B with 54,520 wrong (0.058 %), columnar at merge 32:

| shape | cz | vs shipped |
|---|---:|---:|
| shipped, adaptive pick (near-miss) | 43,836 | baseline |
| literals | 45,620 | +4.07 % |
| `want^pred` | 44,022 | +0.42 % |
| **`want-pred`** | **41,955** | **−4.29 %** |
| multiprecision, 4-byte balanced digits | 43,068 | −1.75 % |
| multiprecision, 8-byte balanced digits | 44,368 | +1.21 % |

Merge sweep for `sub`: +1.08 % at 6, −2.48 % at 16, **−4.29 % at 32**, −2.60 %
at 48 — the same merge the near-miss shape already picks. The win survives
every terminal stage: 42,016 under xz -9e, 41,727 under xz with `lc=0 pb=0`,
41,960 under brotli-11, against the shipped 43,836 / 43,684 / 43,878 / 43,836.

`sub` is pair-adaptive exactly as `xor` is. On `p14`, where the residual is
new content rather than near-misses, the shipped plain shape wins at 868,532
and `sub` is +2.28 % (merge 32) to +8.56 % (merge 6) — so it belongs as a
third candidate in the shape picker `EncodeCorrectionAdaptive` already runs,
not as a new default.

Why it beats `xor` where `xor` beats literals: both destroy the literal
stream's self-matching, and both are only worth it when the prediction is
near-miss rather than absent. But a mispredicted address that moved by a small
amount is *numerically* close, not bitwise close: `0x1000` against `0x0FFF`
xors to `0x1FFF` and subtracts to `-1`. research 14.5 found the near-miss
regime; `sub` is the right transform for it.

Multiprecision, on Go binaries, looks like a dud — −1.75 % against `sub`'s
−4.29 %, because bytewise subtraction already carries a borrow into the next
byte *as a value* (`(0x18, 0xFC)` is `-1000` read as a two's-complement pair),
and trimming the high zero digits needs a significant-digit count per word
that costs more than the borrows save. Per section it is more interesting —
`sub` wins `.text` and multiprecision wins every section holding aligned
machine words, at the width those words have:

| `p2` section | wrong | of | lit | xor | sub | mp4 | mp8 |
|---|---:|---:|---:|---:|---:|---:|---:|
| `.text` | 31,895 | 0.08 % | 23,540 | 23,197 | **22,356** | 24,088 | 25,290 |
| `.rodata` | 3,572 | 0.10 % | 2,871 | 2,888 | 2,853 | 2,740 | **2,575** |
| `.gopclntab` | 3,567 | 0.01 % | 4,640 | 4,569 | 4,422 | **4,262** | 4,562 |
| `.go.type` | 11,421 | 0.10 % | 13,454 | 12,323 | 11,248 | **11,241** | 11,299 |
| `.noptrdata` | 470 | 0.08 % | 610 | 591 | 588 | 528 | **449** |
| `.data` | 744 | 0.22 % | 1,116 | 1,105 | 1,094 | 887 | **751** |

— but the margins are small, and `.gopclntab`'s and `.go.type`'s are small
*because the Go module has already predicted those tables structurally*. What
is left in them is the predictor's misses, not the tables themselves. That is
the clue.

### 2.2 On a binary with no such module, multiprecision is worth 16 %

`libxul` 154.0 → 154.0.1, 185,617,424 B, is the general codec's real case: a
non-Go shared object, predicted by `bench/elfpredict`'s `corrected-fields`
rung (replayed from `ep-ff`'s cached plans with `-probes dump`), 5,627,305
bytes wrong — 3.03 %. Nothing here predicts its pointer tables; they are
residual.

| shape | cz | vs shipped |
|---|---:|---:|
| shipped, adaptive pick (plain) | 3,302,134 | baseline |
| literals | 3,686,524 | +11.64 % |
| `want^pred` | 3,709,478 | +12.34 % |
| `want-pred` | 3,626,585 | +9.83 % |
| multiprecision, 4-byte | 3,233,744 | −2.07 % |
| per-region best-of-four, length buckets | 3,138,441 | −4.96 % |
| **multiprecision, 8-byte, whole file** | **3,114,729** | **−5.68 %** |
| per-section modes, one shared literal stream | 2,979,566 | −9.77 % |
| **per-section modes, a sub-stream per mode** | **2,865,705** | **−13.22 %** |
| per-section modes, each section coded alone | 2,777,592 | −15.88 % |

The row that matters for implementation is the third from the bottom:
**whole-file 8-byte multiprecision, with no selection machinery at all, beats
per-region best-of-four selection** (−5.68 % against −4.96 %). Choosing badly
per region is worse than choosing one good width for the file, because a mode
column and four interleaved statistics cost more than they recover. It wins
under every terminal stage too — 3,017,556 xz -9e against the shipped
3,166,132, 3,055,268 brotli-11 against 3,208,450, 3,480,998 bzip2 against
3,772,883 — so it is not an artefact of `cz`'s codec pick. On `p2` the same
shape is +1.21 %, which is what the adaptive picker is for.

| `libxul` section | wrong | of | lit | xor | sub | mp4 | mp8 |
|---|---:|---:|---:|---:|---:|---:|---:|
| `.text` | 1,773,680 | 1.42 % | **1,700,382** | 1,736,798 | 1,736,066 | 1,863,898 | 1,937,978 |
| `.data.rel.ro` | 1,104,032 | 19.77 % | 904,969 | 904,117 | 902,458 | 446,664 | **249,733** |
| `.eh_frame` | 546,592 | 3.93 % | 580,990 | 565,222 | 491,273 | **446,190** | 462,739 |
| `.rodata` | 662,324 | 1.78 % | 300,054 | 300,295 | 298,748 | **292,294** | 296,531 |
| `.data` | 48,823 | 5.53 % | 41,759 | 38,736 | 34,272 | 28,765 | **17,295** |
| `.eh_frame_hdr` | 1,427,797 | 64.79 % | 44,418 | 44,441 | **44,338** | 44,438 | 44,355 |
| `.relr.dyn` | 39,215 | 38.19 % | **27,345** | 27,371 | 27,421 | 27,601 | 27,694 |

`.data.rel.ro` is an array of 8-byte pointers — the relative-relocation table —
and 8-byte balanced-digit differences take it from 904,969 to **249,733, a
72 % cut on that one section**, 655,236 bytes of a 3.3 MB correction. `.data`
loses 59 %, `.eh_frame` 23 %. This is precisely the case Percival names, and
presage has never been able to see it on a Go binary because the Go module
predicts `.gopclntab` and `.go.type` structurally instead of leaving them in
the residual.

The reading: **a multiprecision difference is a relocation predictor that
costs forty lines instead of a module.** Wherever there is a table of machine
words whose values all moved by related amounts and no module models it, this
recovers most of what a model would. That is the general codec's whole
domain — `SPEC.md` §9's ranked list is mostly formats with no module yet.

Note also that `sub` *loses* on `libxul`'s `.text` (1,736,066 against literals'
1,700,382) while winning on `p2`'s. The transform is not a property of the
codec; it is a property of the region.

### 2.3 The per-section win survives without cutting the file into sections

"Coded per section" lets every section compress in isolation. That is a real
format presage could emit, but it needs the section table and it is not priced
like-for-like — see §2.5, which re-measures it and decomposes what it is
buying. Two shapes that keep one correction stream over the whole file and name
the transform per region — the mode column `SPEC.md` §5's region record would
carry — were measured on the same regions:

- **one shared literal stream**, regions in file order: 2,979,566, −9.77 %.
- **a sub-stream per transform** (`SPEC.md` §6.3's typed sub-streams — a
  region's literals go to the stream of its *mode*, not of its length):
  **2,865,705, −13.22 %.**

So four fifths of the −15.88 % bound is reachable inside one stream, and the
last fifth is context dilution that typed sub-streams mostly recover. The
routing matters more than the selection here: the same mode choices lose 3.5
points when the four transforms share a value stream, because `mp8` residue on
a pointer table and literals from `.text` have nothing to say to each other.

The same shape is a *loss* on `p2`: 42,934 shared / 42,583 per-mode against
whole-file `sub` at 41,955. On a Go binary the mode is uniform, so paying a
mode column and splitting the streams only fragments a 42 KB payload. Both
facts point the same way — this is an adaptive choice, priced like the existing
`shipped`/`nearmiss` pick, not a new default.
### 2.4 Pressure test: a selection bitmap, done properly

The obvious question about §2.3 is whether the *rejected* pieces become
interesting as optional candidates chosen per unit, with a column naming the
winner. Pushing on that found two things wrong with the numbers above.

**The per-region selector was not measuring per-region selection.** It scored
candidates by `len(b)`. `lit`, `xor` and `sub` all emit exactly one byte per
byte, so length cannot separate them at all: the "best-of-four" row was really
"multiprecision or literals", and it never once picked `sub`. Replacing the
score with the order-0 cost of a candidate's bytes under a model of the
sub-stream they would join — three passes, each training on what the last one
picked — roughly doubles the win.

**A redundant varint was being paid on every multiprecision region.** Each one
carried its digit-stream length, which the decoder can derive by walking the
significance counts. On libxul that varint is written 237,887 times. Removing
it is worth 3.8 points on the whole-file shape and 3.5 on the per-region one,
and it was inflating every multiprecision number in §2.1–2.3.

With both fixed, on `libxul`:

| shape | cz | vs shipped |
|---|---:|---:|
| shipped, adaptive pick | 3,302,134 | baseline |
| mp8 whole file, as measured in §2.2 | 3,114,729 | −5.68 % |
| **mp8 whole file, length dropped** | **2,989,024** | **−9.48 %** |
| per-region, length-scored (the broken row) | 3,138,441 | −4.96 % |
| per-region, cost-scored | 2,965,009 | −10.21 % |
| per-region, cost-scored, length dropped | 2,851,015 | −13.66 % |
| **per-region, + a stream per mode code** | **2,828,392** | **−14.35 %** |
| per-section, each section coded alone (the bound) | 2,777,592 | −15.88 % |

and on `p2`, where §2.1 concluded selection was pointless:

| shape | cz | vs shipped |
|---|---:|---:|
| whole-file `sub`, the §2.1 winner | 41,955 | −4.29 % |
| mp4 whole file, length dropped | 40,860 | −6.79 % |
| **per-region, cost-scored, per mode code** | **40,125** | **−8.47 %** |

So the answer is yes, and it is not marginal: a properly scored per-region
selector reaches −14.35 % on `libxul` against a per-section *bound* of
−15.88 %, and it needs no section table, no ELF parsing, and nothing that
would not work on a Mach-O, a PE or a firmware image. On `p2` it now beats
every fixed shape as well, so the same code is the right answer on both pairs
rather than one of two adaptive candidates.

**What closed the gap was routing, not stickiness.** The natural hypothesis for
why sections beat regions is that a section is a long sticky run of one mode,
so a selector that pays to change mode would recover it. Measured as a Viterbi
over the region sequence — transforms as states, the trained cost as the
emission, a penalty λ per mode change — it is worth 0.55 points at λ=8 on
libxul and 0.15 on `p2`, and the curve is flat and then monotonically worse:

| λ | 0 | 8 | 32 | 128 | 512 | 2048 |
|---|---:|---:|---:|---:|---:|---:|
| `libxul` | −10.21 % | **−10.76 %** | −10.51 % | −10.23 % | −10.10 % | −10.00 % |

Giving each *mode code* its own sub-stream is worth more than that on its own
(−13.66 % → −14.35 %), and the two do not compose: with per-mode streams,
λ=0 is the best row. Sections were never about stickiness; they were about not
mixing a 4-byte significance column with an 8-byte one in the same stream.

**Percival's map/value split, measured at the one granularity where it is not
absurd, is a wash.** Inside a multiprecision region the significance counts
already are a map — one small count per word saying how many digits follow.
Peeling that column into its own sub-stream is his idea at the granularity
where the map is dense and small rather than a bitmap over 185 MB. It is
−13.03 % against −14.35 % for simply dropping the redundant length, and −7.40 %
against −8.47 % on `p2`. Closer than anything in §3, and still a loss.

**A codec selection bitmap is already what `cz` is, and it is near-optimal.**
Priced on the nine streams of the winning shape, against `cz`'s own per-stream
pick:

| stream | `cz` | pure-Go LZMA | bzip2 | CLI `xz -9e` |
|---|---:|---:|---:|---:|
| `lit0` (literals) | **1,463,648** | 1,558,017 | 1,806,967 | 1,449,820 |
| `ops` (lz control) | 408,021 | 436,229 | **394,583** | 393,984 |
| `lit5` (mp8 digits) | **338,595** | 401,188 | 376,601 | 337,964 |
| `lit4` (mp4 digits) | **214,420** | 248,465 | 227,484 | 210,196 |
| `gaps` | **144,034** | 158,219 | 156,943 | 140,696 |
| `spans` | **110,689** | 127,199 | 118,294 | 107,872 |
| `modes` | **58,436** | 67,416 | 62,894 | 57,712 |
| total | **2,828,392** | 3,091,935 | 3,245,420 | 2,787,684 |

bzip2 wins exactly one stream, `ops`, by 3.3 % — 13,438 B, or 0.48 % of the
correction. That is the whole value of recommendation 3 on this pair, and it
is real but small.

**Re-measured against the shipped streams, the bzip2 candidate is closed.** The
row above prices the *C* `bzip2 -9`. Re-priced on the nine sub-streams the
shipped encoder actually writes for `libxul` and the nine for `p2`, `bzip2 -9`
still wins exactly one of the eighteen — `libxul`'s `ops`, by 13,438 B, 0.34 %
of that patch, and nothing at all on `p2`, which ships the packed shape. But
the only pure-Go bzip2 writer, `dsnet/compress/bzip2` at level 9 (the same
900 KB block), is 6.8 % worse than the C encoder on that stream: 421,481 B
against 394,583. The one win becomes a 3.30 % loss, so under D15's actual
constraint the candidate does not exist. Buying it means shelling out to a C
encoder — the liblzma trade, already declined below, for a third of the gain,
and `xz -9e` codes the same stream to 393,992 B, so bzip2 is not even the best
version of that trade. Encode 77 ms, stdlib `compress/bzip2` decode 29 ms
against brotli's 5 ms, for reference.

The pure-Go alternative that targets the same stream fails too. `ops` is
interleaved `(nlit, ncopy, seek)` varint triples, so the obvious move is to cut
it into three columns and let `cz` model each. It is **+5.82 % on `libxul`**
(431,776 against 408,021) and −2.31 % on `p2` — 100 bytes. Re-encoding the
columns as fixed-width `uint32` is worse again, +26.7 % and +27.4 %. The triple
is the repeated object; the columns lose the mutual information between a
literal run and the seek that follows it. Same shape of result as the map/value
split in §3, for the same reason.

The LZMA row needs care, because the encoder and the decoder are not subject to
the same constraint. `ulikunitz/xz`'s **encoder** is 9.3 % worse than `cz` on
these streams and 10.9 % behind liblzma — it wins one 1.9 KB stream by nine
bytes — so a pure-Go LZMA codec would cost size. But `DESIGN` D15 only binds
the agent: *"agents must cross-compile and run in `FROM scratch` containers;
the encoder runs on a build box where a few seconds are acceptable"*. Nothing
stops the encoder shelling out to `xz`, so the question is really whether a
pure-Go **decoder** can read liblzma's output, and what it costs to.

Measured on the nine streams of the winning shape, decoding each three times
and keeping the best, with every result compared byte for byte against the
original:

| codec | bytes | pure-Go decode |
|---|---:|---:|
| zstd (`cz` candidate) | 3,295,590 | 7 ms |
| brotli (`cz` candidate) | 2,828,392 | 39 ms |
| **liblzma `xz -9e`** | **2,787,684** | **203 ms** |

`ulikunitz/xz` decoded liblzma's output correctly on all nine. So it works, and
203 ms on an 8.2 MB correction is not disqualifying next to the rest of patch
application. The reason to rank it low is size, not feasibility: 40,708 bytes,
**1.0 % of the whole patch** — an order of magnitude under the multiprecision
change and only three times the bzip2 candidate. The gap looked like 3–10 % in
the earlier tables because those priced a *worse* correction; once the shape
is good, brotli-11 is within 1.4 % of `xz -9e` and there is much less left to
win. Against that 1 % stands a cgo or subprocess dependency in the encoder,
which makes patch bytes depend on the installed liblzma version — awkward for a
system that identifies releases by hash, though the codec tag does record the
choice and the decoder verifies the result either way.

**The unmatched edge stays rejected, and now the curve proves it.** §4's grid
stopped at a minimum stretch of 256 bytes, which left open whether a stricter
threshold crosses zero. Extended to 1024, 4096 and 16384 on `p2`, every column
converges monotonically down to the baseline from above and none crosses it:
at g=1/min 4096 the excision simply stops firing and reproduces the baseline
byte for byte. There is no subset on which the idea pays, so no selector can
rescue it.

### 2.5 What the per-section row actually is, and five cheap follow-ups

**The per-section bound is more viable than §2.3 said, and less comparable.**
Calling it "a bookkeeping bound, not a format" was wrong: presage could emit
one correction per section, each with its own mode, each compressed as its own
object. Nothing in the container forbids it. Three things do argue against it,
and one is a measurement error in the row itself:

- it is **not like-for-like**. `perSection` skips every section under 64 KiB,
  so the bound covers 5,602,469 of libxul's 5,627,305 wrong bytes — 24,836
  bytes, 0.44 %, are simply not priced. At the file's average that is roughly
  12 KB unaccounted, about 0.4 of the points it claims. It is also the only
  row in this document that is never replayed.
- the mode is an **oracle**: five full encodes and five full compressions per
  section, chosen on the answer. The per-region selector is *cheaper* than the
  encoder presage already ships (below).
- it needs an **ELF section table**, which the general codec's target list
  (`SPEC.md` §9) is mostly not.

Re-measured with the redundant length gone, and decomposed:

| libxul shape | cz | vs shipped |
|---|---:|---:|
| per-section modes, one shared stream | 2,988,304 | −9.50 % |
| per-section modes, a stream per mode | 2,876,211 | −12.90 % |
| **per-region modes, a stream per mode code** | **2,825,416** | **−14.44 %** |
| per-section, each section coded alone | 2,721,271 | −17.59 % |

The third row is the interesting one: **a per-region selector beats per-section
*oracle* mode choice** once both share the same streams. Sections are not a
better way to choose the mode. Everything the bottom row still has over it is
context isolation — compressing each section's streams by themselves — worth
3.15 points, minus the ~0.4 it owes to the sections it never priced.

**Isolation does not come for free by cutting on position.** Regions are
emitted in file order, so every sub-stream is in file order too and a fixed-size
cut approximates a section cut without any format knowledge. Priced on the nine
streams of the winning shape, it costs, monotonically:

| cut | whole | 4 MiB | 2 MiB | 1 MiB | 512 K | 256 K | 128 K | 64 K |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| total | **2,828,392** | 2,828,392 | 2,837,338 | 2,846,786 | 2,865,794 | 2,895,058 | 2,922,654 | 2,959,001 |

So the last three points of the per-section row are not "isolation helps" in a
form anything else can borrow. Whatever produces them is specific to coding a
section as its own correction — restarted gaps, section-local lz windows, no
mode column — and buying them costs the section table.

**The selector is cheaper than the encoder presage already ships.** Three
training passes plus the final write, against `EncodeCorrectionAdaptive`'s two
writes and *two* compressions of the largest stream in the patch:

| pair | 3 training passes | final write | compress ×1 | decode | shipped: 2 writes |
|---|---:|---:|---:|---:|---:|
| `p2`, 94 MB | 177 ms | 80 ms | 302 ms | 0.72 ms | 177 ms + 2 compressions |
| `libxul`, 186 MB | 3.17 s | 1.02 s | 13.9 s | 24.9 ms | 2.54 s + 2 compressions |

Compression dominates, and the selector needs one where the shape picker needs
two. On libxul that is roughly 18 s against 30 s. Decoding is a byte loop:
24.9 ms for a 186 MB file.

**The "the model undervalues `sub`" caveat is closed, and it was wrong.** §2.4
noted the order-0 selector picks `sub` once in 6,995 regions on `p2` while
whole-file `sub` beats every other fixed shape, and guessed the model could not
see that the same delta recurs across call sites. Pricing a region payload the
previous pass already emitted as a match instead of as its bytes tests that
directly, and it does nothing: `p2` 40,239 against 40,125 at a 8-bit match cost,
libxul 2,825,416 against 2,828,392 — 0.09 points, inside the noise. The
selector is not mispricing `sub`; `sub` simply loses per region once
multiprecision can pick its width there.

**The map/value split wins somewhere, so it is a candidate rather than a
rejection.** Chrome 152.0.7977.54 → .64's `.data.rel.ro` is 11,783,520 B with
the previous version's own table as the prediction — 811,329 wrong, 6.885 %.
There, `mpSplit` beats dropping the length: 107,042 against 107,635 whole-file
at 8 bytes, and 107,322 against 107,440 per region. It loses on both other
pairs (§2.4). The split wins exactly where the significance column is most
skewed — most words unchanged, so the map is nearly all zeros and the values
are few. Cheap enough to write both and keep the smaller.

That Chrome measurement is **not** the test §7 asks for, and it should not be
read as one. Using the old table directly as the prediction is a different and
much harsher regime than libxul's, where the predictor had already relocated
the pointers approximately and left them near-miss across 19.77 % of the
section. Balanced digits pay when a value *moved by a little*; here most
changed entries moved arbitrarily, `xor` at merge 32 already gets 109,463, and
every mode lands within ±2 % of it. It does confirm the transform is never a
loss, on a third binary and a third regime.

Section sizes also temper §2.2's expectation for Chrome: `.data.rel.ro` is
11.8 MB against `.text`'s 225 MB, a ratio of 5.2 %, where libxul's is 5.6 MB
against 125 MB, 4.5 %. Chrome's table is 2.1× larger in absolute terms but the
file is 1.6× larger too, so the prize is proportionally similar rather than
"far larger".

## 3. The difference map / non-zero split — **reject, it is a loss**

`percival-thesis.md` §"Three ideas to lift directly" lists the map/value split
as idea 2, "separate *where* from *how much*". Measured, it costs.
`delta/bs6dstr_spike_test.go` reruns the shipped scan and prices the same
matched bytes four ways: the dense difference string bsdiff 4.3 ships, the
`(gap, len, values)` runs presage ships, those runs with positions and values
in separate streams, and Percival's bitmap — one bit per matched byte — with
the non-zero values apart.

`syn`: 80 triples, 29,994,838 matched bytes, 371,748 non-zero (1.24 %).

| difference | cz | xz -9e | xz lc0 pb0 | bzip2 -9 | brotli -q11 | zstd -22 | raw |
|---|---:|---:|---:|---:|---:|---:|---:|
| dense | 237,291 | 168,608 | 168,234 | **149,222** | 199,018 | 191,924 | 29,994,838 |
| runs (shipped) | **169,040** | 166,996 | 166,968 | 156,266 | 169,040 | 174,202 | 1,015,112 |
| runs, pos \| val | 191,926 | 184,084 | 187,277 | 196,980 | 191,926 | 194,536 | 1,015,112 |
| bitmap \| val | 190,920 | 182,344 | 180,512 | 200,855 | 190,920 | 196,629 | 4,121,103 |

`p2`: 8,831 triples, 92,717,498 matched bytes, 7,676,712 non-zero (8.28 %).

| difference | cz | xz -9e | xz lc0 pb0 | bzip2 -9 | brotli -q11 | zstd -22 | raw |
|---|---:|---:|---:|---:|---:|---:|---:|
| dense | 2,355,656 | **1,895,456** | 1,910,186 | 2,035,631 | 2,122,079 | 2,247,026 | 92,717,498 |
| runs (shipped) | 2,180,115 | 2,009,300 | 2,039,671 | 2,125,887 | 2,120,873 | 2,247,851 | 15,696,788 |
| runs, pos \| val | 2,257,194 | 2,126,756 | 2,143,272 | 2,316,643 | 2,223,297 | 2,310,022 | |
| bitmap \| val | 2,230,810 | 2,078,144 | 2,092,084 | 2,380,331 | 2,161,368 | 2,263,566 | 19,266,400 |
| — bitmap alone | 876,546 | 825,004 | 831,807 | 972,963 | 851,491 | 891,816 | 11,589,688 |
| — values alone | 1,354,264 | 1,253,140 | 1,260,277 | 1,407,368 | 1,309,877 | 1,371,750 | 7,676,712 |

Both splits lose to the interleaved runs on both pairs under all six
compressors: comparing each shape at its own best compressor, the bitmap split
is +8.1 % on `syn` (180,512 against 166,968) and +3.4 % on `p2` (2,078,144
against 2,009,300); under `cz`, the stage that ships, +12.9 % and +2.3 %. The
reason is the one the
thesis states and then reasons past: "birds of a feather compress better
together" is true of *values*, and the split assumes positions and values are
independent birds. In a relocated binary they are not. A shifted call site
produces the same `(gap, len, values)` record thousands of times over, and an
LZ compressor matches the whole record; splitting the record into two streams
destroys the match and leaves each half individually less predictable. Percival
codes his map with a BWT and an arithmetic coder built for position sets,
which recovers some of it, and even then he needs the map to be
"distinctive" — this measurement says the record is more distinctive than the
map.

The same holds for splitting `(gap, len)` from the values: 184,084 against
166,996 on `syn` at each one's best. Both directions of the idea are closed.

## 4. Better matching and the pruned dynamic program — **no headroom here**

bsdiff 6 replaces 4.3's greedy scan with block alignment feeding a dynamic
program over 64 candidate alignments per byte, with edge costs 0/2 for
match/mismatch, 1 for unmatched and 20 for a realignment (§2.6). `plain.go`
runs 4.3's scan verbatim, over a position-ordered hash index rather than a
suffix array. Three measurements bound what any of that could buy.

Two facts from the prototype are worth having first. CISPA's reimplementation
reports that the "shortest path" is not one: each node in a layer back-links
to the single best-scoring node of the previous layer, so the graph degenerates
and the DP collapses to a running argmin — **a beam of width 64 on the node
set and width 1 on the back-link**, linear in file size and an approximation,
not an optimum. And the constants are, in their words, *"tuned for bzip2
compression"*, with adaptive penalty models listed as future work for LZMA-class
back ends. Both cut the same way: there is less algorithmic content in the DP
than the thesis's framing suggests, and its numbers do not transfer to a
zstd/brotli terminal stage unretuned.

**The index is not the limit.** `delta/bs6lz_spike_test.go` sweeps
`Index.SetProbe` — how many candidates around the hint a lookup tries — over
5, 16 and 64 on `syn`. All three produce byte-identical streams: 80 triples,
169,887 cz. The anchors an executable offers are long enough that a wider
search finds nothing a narrow one missed, which is the claim `lz.go`'s
package comment makes and this confirms.

**The cost model is flat.** Sweeping the scan's two constants — the margin at
which a fresh match displaces the running one (bsdiff: 8) and the extension
ratio (bsdiff: two matched bytes per byte of difference stream) — over
cutoff ∈ {2, 4, 8, 16, 32} × ratio ∈ {2, 3, 4} moves the total by 0.4 % on
`syn`, and every value below 8 is worse. The shipped constants are at a local
optimum.

**The matchers are already equivalent.** On `syn`, presage's scan produces a
difference string that bzip2s to 149,222 B. bsdiff 4.3's *entire patch* on
that pair — its own suffix-array matching, its own control and extra streams,
its own bzip2 — is 150,475 B. Whatever the suffix array finds that the hash
index does not is worth less than 1 % here.

**The unmatched edge loses.** The DP's one decision the greedy scan cannot
express is "these bytes match badly enough to send as literals instead". The
spike adds it as an excision pass: inside a matched run, Kadane's algorithm
finds stretches scoring +g per mismatched byte and −1 per matched byte above
a threshold, and moves them to the extra stream, splitting the triple. On
`p2`, against a 2,517,180 cz baseline:

| g | min 8 | min 24 | min 64 | min 256 |
|---:|---:|---:|---:|---:|
| 1 | 2,697,655 | 2,621,328 | 2,578,313 | 2,548,523 |
| 2 | 4,369,408 | 3,092,951 | 2,971,451 | 2,927,259 |
| 3 | 6,369,661 | 4,177,801 | 4,019,807 | 3,875,708 |
| 6 | 7,926,809 | 7,169,364 | 6,054,801 | 5,459,449 |

Every setting loses, monotonically in how much it excises. The hypothesis was
that the extra stream compresses like code (~2–3 bits/byte) while the
difference stream's non-zero values compress like noise, so moving a dense
mismatch stretch should pay. The data says the opposite: at g=2/min 256 the
difference stream drops 158,716 B while extra grows 567,072 — a byte moved to
extra costs 3.6× what it saves. The difference values are *systematic* — the
same relocation delta recurs across thousands of call sites — and the extra
bytes are genuinely new code. The greedy scan is keeping the right bytes in
the match.

That result is what the bzip2-tuning warning predicts. bzip2 codes a dense
mismatch stretch inside the difference stream badly — its block sort has no
window to relate one stretch to another — so under bzip2 there is a real gain
in moving it out. `cz` (zstd-19 / brotli-11 over an 8 MiB window) does not have
that weakness: it matches the recurring relocation deltas across the whole
stream, and excising them destroys exactly the matches it was finding. An
unmatched edge priced for bzip2 is a pessimisation on a modern terminal stage,
and 4.8 % of the claimed gain is all the alignment work bought even where it
was priced correctly.

### 4.1 Why the split fails here and worked for Percival

Percival's reasoning for the map/value split is explicit: the values are
"locally repetitive" while the positions are "globally structured", because
"as a result of instruction encoding lengths and the positions within
instructions where addresses are encoded, the locations where corrections must
be made tend to form distinctive, and compressible, patterns". His corpus was
DEC Alpha. Alpha instructions are four bytes wide and four-byte aligned, so
every correction position is congruent to a constant mod 4 and the map really
is nearly free.

x86-64 is not. On `syn` the 371,748 correction positions fall mod 4 as
81,850 / 110,836 / 45,543 / 133,519 — skewed, but nothing like a single class,
and mod 2 as 127,393 / 244,355. On `p2` they are all but uniform:
2,119,942 / 1,712,553 / 1,855,048 / 1,989,169. Priced apart, on `syn`:

| stream | best | coder |
|---|---:|---|
| difference map, general compressor | 136,263 | xz, lc=0 pb=0 |
| difference map, BWT + Huffman | 149,379 | bzip2 -9 |
| difference map, binary-interpolative code on the positions | 336,430 | exact, 7.24 bits/position |
| non-zero values | 44,249 | xz, lc=0 pb=0 |
| **map + values apart** | **180,512** | |
| **positions and values coded together as runs** | **166,996** | xz -9e |

Two things fall out. The map is not cheap — 136 KB is four fifths of the whole
shipped difference stream — and Percival's own coder for it, BWT then position
enumeration then interpolative arithmetic coding, is not what makes it cheap:
the interpolative code on the raw positions costs 7.24 bits each against the
7.87 bits a uniformly random subset of that density would need, so as a
*position set* the corrections are barely structured at all. What a general
compressor finds in the bitmap is run structure, not positional structure.

And coding position with value beats coding them apart by 7–8 % on `syn` and
3.4 % on `p2` (825,004 + 1,253,140 = 2,078,144 against 2,009,300), because the
repeated object in a relocated x86-64 binary is the whole `(gap, length,
values)` record — the same call-site shift recurring across thousands of
sites — not the position alone and not the value alone. The birds were in the
right flock already.

## 5. Where presage actually still loses to bsdiff — and it is not a bsdiff 6 idea

The one place the shipped `lz` module is behind bsdiff is the representation
`plain.go` deliberately replaced: bsdiff's *dense* difference string, one byte
per matched byte, almost all zero. Against the shipped sparse runs:

| pair | non-zero density | runs, shipped stage | dense, best stage | gap |
|---|---:|---:|---:|---:|
| `syn` | 1.24 % | 169,040 (cz) | 149,222 (bzip2 -9) | −11.7 % |
| `p2` | 8.28 % | 2,180,115 (cz) | 1,895,456 (xz -9e) | −13.1 % |

and the best terminal stage *for the dense string* flips with density: a block
sorter when the string is sparse, an LZ with a big dictionary when it is not.
`plain.go`'s package comment already says the first half of this; the second
half is that the trade is 30–90× the bytes through the compressor and the
whole file resident, which is what presage bought its way out of.

There is a cheaper middle. The shipped sparse-run stream also prefers a block
sorter when it is sparse: on `syn` it is 156,266 under bzip2 against 169,040
under `cz` — **−7.6 % for a codec tag**, on a stream 30× smaller than the
dense one, and `cz` was already giving it brotli-11, so this is a real
ranking and not a quality artefact. On `p2`, where the difference is dense,
bzip2 loses (2,125,887) and `cz` is right. `internal/cz` already picks the
smallest of its candidates per frame and already carries a codec tag per
frame; adding a block sorter to the candidate set is the whole change, and the
decoder side is `compress/bzip2` in the standard library.

### 5.1 Three streams or one buffer

bsdiff compresses control, difference and extra separately; `packPlain`
concatenates them and the container compresses 8 MiB frames of the
concatenation. On `p2` that is worth something, but not for the reason
Percival gives:

| packing | cz | xz -9e | bzip2 -9 | brotli -q11 |
|---|---:|---:|---:|---:|
| one buffer (shipped) | 2,603,345 | **2,346,840** | 2,812,674 | 2,463,156 |
| ctrl \| diff \| extra | **2,518,941** | 2,355,028 | 2,800,574 | 2,459,699 |
| one buffer, streams frame-aligned | 2,601,593 | 2,359,264 | 2,821,620 | 2,467,082 |

Under `cz` the split is worth 3.24 %; under `xz`, which has no size-dependent
quality rule and a 64 MiB dictionary, it is a 0.35 % *loss*. And padding the
streams onto frame boundaries inside one buffer recovers almost none of it
(0.07 %). So the gain is not "no frame straddles two streams" and it is not
"birds of a feather" — it is `cz`'s own `BrotliMax` cliff: a stream under
4 MiB gets brotli-11, and `extra` (1,052,457 B) only gets that when it is
compressed as its own object rather than inside an 8 MiB frame. Worth knowing
about `cz`; not evidence for bsdiff's three-stream layout.

## 6. Verdict

Percival's public claim for the unreleased bsdiff 6 is "roughly 20 % smaller
patches" (25 % against 4.2 in the deleted 2004 wording); the only independent,
controlled measurement — CISPA's 2026 reimplementation from his own prototype —
is ≈4.8 %, but that reimplementation is `BSDIFF40`-compatible and therefore
carries his *alignment* and none of his *encoding*. The gain is three levers:
better match selection, a coarse matcher that survives heavy mismatch, and
better residual encoding. The independent number bounds the first two at
single digits and leaves the third untested by anyone. That is where this
spike found everything it found, and it is the useful takeaway from the whole
exercise: the part of bsdiff 6 that has been reimplemented twice is the part
worth the least.

| bsdiff 6 lever | expected rank | measured here | verdict |
|---|---|---|---|
| multiprecision balanced difference | tier 1 | **−9.48 % whole-file at 8 bytes** on `libxul`, **−14.35 %** chosen per region into per-mode streams; `.data.rel.ro` −72 % | **port — the largest number in this spike** |
| per-unit selection with a mode column | not in the thesis | −14.35 % on `libxul`, −8.47 % on `p2`; beats every fixed shape on both | **port — it is what makes the above pay on both pairs** |
| bytewise `want-pred` | not novel (4.3 has it, presage does not) | −4.29 % on `p2`; +9.8 % on `libxul` | **port, as another adaptive shape** |
| map / value split | tier 1, "likely the single largest encoding win" | +3.4 % to +13.5 % on the difference string; as an mp significance column it loses 1.3 points on `libxul` and `p2` and *wins* 0.5 on Chrome's relocation table | **reject on the difference string; keep as an adaptive candidate on the significance column** |
| BWT + interpolative map coder | tier 3 | 2.5× worse than xz on the same bitmap | **reject** |
| shortest-path match selection | tier 2 | its one new decision (the unmatched edge) loses at every setting | **no headroom on this corpus** |
| suffix array over the hint index | implied | probe 5 ≡ probe 64; within 1 % of bsdiff's whole patch | **no headroom** |
| coarse matcher for address tables | tier 2 | already built as `presage/eqmatch`, measured 2.5–2.8× worse alone | **already answered** |
| three streams, one codec each | tier 1 | −3.2 % under `cz`, +0.35 % under `xz`; it is `cz`'s brotli-11 cliff | **not the reason claimed** |
| per-stream codec choice | implied | `cz` already is this; the only pure-Go bzip2 writer loses 3.3 % on the one stream C bzip2 wins; liblzma wins eight, worth 1.0 % of the patch, but only with a non-Go encoder | **reject bzip2; framework already exists, candidates are thin** |

### What to do

Decided 2026-08-29: recommendations 1 and 2 are in, and are now written up as
`SPEC.md` §6.1/§6.3/§6.4 and decision G13. The LZMA candidate is out — 1.0 % of
the patch does not pay for a non-Go encoder in a project whose releases are
identified by hash. The bzip2 candidate is out for the same reason once
re-measured against the streams that shipped: it is worth 0.34 % and only with
a C encoder, because the pure-Go writer loses on the single stream C bzip2 wins
(§2.4). `internal/cz` keeps its three tags.

1. **Implement SPEC §6.1 mode 2 as a whole-file shape first.** The reserved
   modes are right and were under-rated because the only residuals presage had
   measured were Go binaries, whose pointer tables the Go module already
   predicts. On the first non-Go pair measured, 8-byte balanced digits applied
   to the whole file are worth **−9.48 %** with no new machinery — no mode
   column, no per-region decision, no section table. `mpDigits`/`mpApply` in
   `delta/bs6spike_test.go` are the whole encoder and decoder, about forty
   lines each, and the shape slots into `EncodeCorrectionAdaptive`'s existing
   write-both-keep-smaller loop, which is what keeps `p2` from regressing.

   Do not transmit the per-region digit-stream length; the decoder derives it
   by walking the significance counts. It is 3.8 points of that −9.48 %.

   *Then*, as a second step: **choose the mode per region and give each mode
   its own sub-stream** (SPEC §6.3), worth a further −4.9 points on `libxul`
   (−14.35 % total) and turning `p2` from −6.79 % into −8.47 %. Three things
   matter and one does not:

   - the selector must score candidates by what their bytes *cost*, not by how
     many there are. Length cannot separate `lit`, `xor` and `sub`. An order-0
     model per destination sub-stream, trained over three passes on what the
     previous pass picked, is enough (`byteModel` in the spike);
   - route by the full mode code, so mp4 digits and mp8 digits do not share a
     stream: −0.7 points;
   - the mode column costs 58,436 B compressed of a 2.83 MB correction, which
     the routing pays for many times over;
   - a switching penalty does **not** matter. A Viterbi over the region
     sequence buys 0.55 points at best and nothing once the streams are routed
     per mode.

   This is a format change (a mode column in the region record) where the first
   step is not, but it now wins on all three pairs measured, so it need not be
   adaptive. It is also *cheaper* than what ships today: three training passes
   plus one write cost less than `EncodeCorrectionAdaptive`'s two writes, and
   it compresses once where the shape picker compresses twice (§2.5). Decoding
   is 24.9 ms on a 186 MB file.

   Do **not** buy the per-section bound's remaining 3 points. It needs an ELF
   section table, it prices five oracle encodes per section, it silently omits
   every section under 64 KiB, and the format-independent way to get what it
   is actually buying — cutting each stream on position — costs rather than
   saves at every size from 4 MiB down to 64 KiB (§2.5).

2. **Add `want-pred` as a third correction shape.** `corrShape` grows a `sub`
   field and a sibling at merge 32; the adaptive encoder already writes two
   shapes and keeps the smaller, so this is one more `czLen` on a stream it
   already compresses twice. Worth −4.29 % of the correction on `p2` — about
   1,900 B of a 74,112 B patch, whose residual is 76,827 B raw.

   The wire cost is the awkward part: the shape lives in the low bit of the
   region count under transform 2, and a third shape needs two bits, which a
   deployed transform-2 decoder would misparse. Either spend a transform id
   or reserve the second bit now. A format decision, not a codec one.

3. ~~**Give `internal/cz` a block-sorting candidate.**~~ **Withdrawn
   2026-08-29.** The −7.6 % on a sparse difference stream (`syn`, 1.2 %
   non-zero) was measured against a correction shape that no longer exists.
   Against the shipped modal streams C `bzip2 -9` wins one sub-stream of
   eighteen, worth 0.34 % of `libxul`'s patch and nothing on `p2` — and the
   pure-Go writer is 6.8 % behind the C one there, turning that win into a
   loss. `compress/bzip2` on the decode side was never the problem; the
   encoder is (§2.4).

   An LZMA candidate is a separate, smaller question: liblzma beats `cz` by
   1.0 % of the patch, a pure-Go decoder reads its output correctly at 203 ms
   per 8.2 MB, and D15 lets the encoder shell out — but the pure-Go *encoder*
   loses 9.3 %, so taking it means a non-Go encoder dependency (§2.4).

### What not to do

- Do not split the difference string into a map and values, in either
  direction, and do not build the BWT/interpolative map coder. Both are
  measured losses, for a legible reason: Percival's premise — that correction
  positions "form distinctive, and compressible, patterns" — holds for
  fixed-width aligned Alpha instructions and fails for x86-64, where the
  positions are nearly uniform mod 4 (`p2`: 2,119,942 / 1,712,553 /
  1,855,048 / 1,989,169) and the repeated object is the whole
  `(gap, length, values)` record rather than either half of it.
- Do not replace the greedy scan with a shortest path on the strength of the
  thesis. It may still pay on a corpus with real function reordering — nothing
  here rules that out, and the `lz` module was only measured on `syn` and
  `p2` — but on those the scan is at a local optimum in both its constants and
  its one structural limitation.
- Do not port the suffix array. The hint index finds the same matches.
- Think twice before adding an LZMA candidate to `cz`, but not because it
  cannot be done. A pure-Go decoder reads liblzma's output correctly, at 203 ms
  per 8.2 MB against brotli's 39 ms, and D15 leaves the encoder free to shell
  out. It is worth 1.0 % of the patch, against a cgo/subprocess encoder
  dependency (§2.4). Rank it below recommendation 1; do not port the pure-Go
  *encoder*, which loses 9.3 %.
- Do not add a bzip2 candidate to `cz`, and do not cut the lz op stream into
  columns. Both were measured against the shipped streams and both lose
  (§2.4).

### A methodological note, because it changed two conclusions

Two numbers in §2.1–2.3 were measuring the harness rather than the idea: a
per-region selector that scored candidates by byte count (which cannot
separate three of its four candidates), and a redundant length varint charged
to every multiprecision region. Both made a good idea look mediocre, and the
first made "selection does not pay here" look like a finding when it was an
artefact. §4's rejection of the unmatched edge rests on the same kind of fixed
cost model, which is why its grid was extended until the curve was shown to be
one-sided rather than merely unfavourable at the settings tried. Where a
measurement says an idea loses, it is worth checking whether the thing being
priced is the idea or the scaffolding.

## 7. What this spike did not measure

- The `lz` module on a pair with real function reordering (`libxul`, Chrome).
  Every `lz` measurement here is a Go binary, where the file is one shift and
  the greedy scan has almost nothing to decide — 80 triples on `syn`. The DP
  question is open there, not closed.
- Multiprecision on a Chrome pair *under a real prediction*. §2.5 prices the
  transforms on Chrome's `.data.rel.ro` with the previous version's own table
  standing in for a prediction, which is a harsher regime and lands within
  ±2 %; it does not answer §2.2's question. `elfpredict` on the Chrome pair
  stops after the first whole-image rung and never reaches `corrected-fields`,
  so no prediction to test against exists yet. The section is also only
  proportionally as large as libxul's (5.2 % of `.text` against 4.5 %), so
  §2.2's "far larger" was about absolute size only.
- Only one real Go release pair exists at Go 1.27 (prometheus). `cockroach`,
  `terraform`, `kube-apiserver` and `vault` in `bench/out/corpus` are Go 1.25
  and 1.26, which `delta.Predict` declines.
- Anything a decoder pays. Every number here is encoder-side stream size;
  `mpApply` is a byte loop and should be free, but the per-mode-sub-stream
  shape adds a routing table the decoder has to walk.
- Encoder time for the tuned selector. It writes the correction three times to
  train its models before the write that ships, on top of the two the adaptive
  shape picker already does. On libxul that is seconds, not minutes, but it is
  not free and no one has priced it against `EncodeCorrectionAdaptive`'s
  existing budget.
- What produces the per-section row's last three points. §2.5 rules out the
  obvious candidate (context isolation reachable by cutting on position) and
  shows the mode choice is not it either, which leaves restarted gap streams,
  section-local lz windows and the absent mode column. Nobody has separated
  them, and until somebody does it is not known whether any of it is portable.
- bsdiff 6's own combination step, on presage's own matcher. §4 bounds what
  its *one novel decision* (the unmatched edge) is worth here and finds it
  negative under `cz`, but that is not the same as porting the block
  alignment and the beam and measuring the pair.
