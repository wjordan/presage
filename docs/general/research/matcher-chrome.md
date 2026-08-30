# The native matcher on Chrome: closing the gap to Zucchini's stream

`matcher-spike.md` measured `presage/eqmatch` against Zucchini's equivalence
stream on prometheus pairs with `.text` runs discarded, and named three
deficits it could not see there: hash-chain anchors, greedy selection, and no
reference-aware comparison. Chrome 151.0.7922.169 → .173 is 225 MB of `.text`
in a 291 MB image, and there the native matcher started 83 % behind. This
note is the ladder that closed it, every rung measured with

```
bench/elfpredict -old chrome-…169 -new chrome-…173 -old-debug … -new-debug …
  -native-equivalences -native-eq-drop D -native-eq-min M [-native-eq-minfar F]
  -reference 5263732 -rungs corrected-fields
```

(2026-08-29). The number is the `whole image corrected-fields … = N XZ bytes`
line: plan + correction as separate `xz -9e` streams. The Zucchini-stream
run (`-equivalence-patch chrome-151.169-to-173.zuc`) re-measured on the same
tree is **2,621,664** (plan 1,244,736 + correction 1,376,928; the handoff
document's 2,634,264 is the same run on an earlier tree).

## Ladder

| step | D / M / F | runs | plan | correction | total |
|---|---|---:|---:|---:|---:|
| as shipped (`Defaults`) | 4096 / 32 | 96,269 | 2,285,048 | 2,538,528 | 4,823,576 |
| tuned only (sweep of 9 points) | 24 / 24 | — | 1,315,788 | 2,146,628 | 3,462,416 |
| + `.text` matched on `x86.Canonical` bytes | 24 / 16 | 214,747 | 1,789,388 | 1,330,984 | 3,120,372 |
| + function map as `Expect`, tie-break towards it | 24 / 16 | 193,780 | 1,628,120 | 1,317,832 | 2,945,952 |
| + chain searched past its cap near the expected source | 24 / 16 | 183,262 | 1,487,168 | 1,300,872 | 2,788,040 |
| + `MinFar`: a run far from the expected source must be ≥ F | 24 / 16 / 96 | 152,780 | 1,181,228 | 1,465,256 | 2,646,484 |
| shorter seeds under the same far floor | 24 / 12 / 96 | — | 1,251,912 | 1,365,788 | **2,617,700** |
| Zucchini's stream | — | 158,544 | 1,244,736 | 1,376,928 | 2,621,664 |

libxul 154.0 → 154.0.1 at the final setting: **3,632,264** (plan 1,068,672 +
correction 2,563,592) against 4,063,404 with Zucchini's stream, 4,780,572
native as shipped. The prometheus DWARF pair through `presage diff` (the Go
module's per-section use of the matcher, `Defaults` unchanged) moved 332,414
→ 330,678; the synthetic DWARF pair stayed 2,065.

## What each rung is

**Masking (−450 K).** The harness adapter matches `.text` on
`x86.Canonical` — every PC-relative field zeroed — in both images, so two
copies of code whose targets moved agree byte for byte, and the relocation
stage retargets the fields from the function map afterwards. The correction
fell below Zucchini's at once (1,330,984 vs 1,376,928): masking is Zucchini's
label comparison in its cheapest form, and everything after this rung is
about the plan, not the prediction. Zeroed operands make the seed hash worse
(every `call rel32` hashes alike), which the next two rungs pay for.

**Expect (−175 K).** `eqmatch.Params.Expect` hands the matcher the function
map's answer for a destination offset (`srcPredictor.at`). The source column
is coded as a residual against that answer, so a run that starts where the
map says costs nothing in the source column; the matcher probes the expected
source as a candidate and breaks near-ties (`Slack`, 16 bytes of exact
prefix) towards it. Raising `Slack` to 64 changed nothing: the residual was
not coming from ties.

**Near search (−160 K).** A function that gained a few bytes puts the rest
of its old body a little away from where the map says, and the seed there is
what the residual column wants; a hash chain capped at 32 most-recent
offsets rarely reaches it. When the best of the first 32 is more than 64 KB
from the expected source, the chain is walked on (up to 4,096 entries) but
only candidates within 64 KB of the expected source are scored. Matching
time 9 s → 17 s on the pair.

**MinFar (−140 K).** A residual histogram of the `.text` runs (`EQSTATS=1`)
showed 43 K runs whose source was more than 64 KB from the expected one,
each paying a 4–5 byte residual for a 16–40 byte masked idiom (prologues,
`call; test; je` shapes) that xz would have coded for less. `MinFar` is the
length floor for such runs; at 96 the far runs fell to 2.7 K and the
equivalence stream from 594 K to 431 K, at a 165 K cost in correction, net
−140 K. 48 / 64 / 128 / 192 are all within 8 K of 96.

**Min 12 (−29 K).** With the far floor in place, the near runs can be
shorter: min 12 recovers coverage (correction −100 K) for +70 K of plan.
Min 8 overshoots (2,668,312).

## What did not move it

- `Drop` above 24 with masking on: 48 → +1 K, 128 → +430 K. The
  `matcher-spike.md` argument for a large drop (the correction absorbs a
  disagreeing stretch) holds for DWARF under `Defaults`; on code the
  disagreeing stretch is a changed function, and a run that spans it
  mis-places every reference the layers project through it (reloc stream
  942 K → 100 K from drop 4096 → 24).
- `Slack` 64.
- Min 8, 24; MinFar 48–192 (all within 8 K).

## Settings

`eqmatch.CodeDefaults = {Min 12, Drop 24, MinFar 96, Slack 16}`; the harness
`-native-equivalences` defaults to them with `-native-eq-mask` and
`-native-eq-expect` on. `eqmatch.Defaults` (Min 32, Drop 4096, MinFar =
Min) is what `presage/gomod` still uses per DWARF section, and is
untouched except that the near search now applies there too (the 332,414 →
330,678 above). The ELF module (`elf-module.md` §4) takes `CodeDefaults`,
masks `.text` with `x86.Canonical`, and passes its function map as `Expect`.

## What remains

The stream is 4 K under Zucchini's on Chrome and 430 K under on libxul, with
158 K vs 153 K runs; per-column, the source residual is the last place the
two differ (Zucchini 226,756, native 174,036 at min 16, higher at min 12).
Not taken: a suffix array (the near search bought what exact anchors would
have, at 4 B/old byte less), and a global best-first selection pass, whose
gain the spike could not isolate and which this ladder never needed.
