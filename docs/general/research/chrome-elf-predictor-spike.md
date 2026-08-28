# Chrome ELF structural predictor spike

Measured 2026-08-27 on the same Chrome 151.0.7922.169 → 151.0.7922.173
Linux x86-64 release pair as [`chrome-elf-zucchini.md`](chrome-elf-zucchini.md).
This experiment asks a more ambitious question than the RELA fix: can an
independent, publisher-symbol-assisted predictive codec obtain a much larger
gain than Zucchini on Chrome's own release corpus?

The answer is **not with function placement and reference prediction alone**,
but the follow-on combined experiment shows that it is useful when layered
over large mismatch-tolerant equivalences. The standalone structural predictor
costs 7,588,419 bytes with Zstd / 6,858,260 bytes with XZ. A replayable hybrid
that chooses between coarse-equivalence and symbol-derived predictions per
function costs **5,129,630 bytes with Zstd / 4,599,840 bytes with XZ** for
`.text`.

> **Correction (2026-08-27/28).** An earlier revision of this document claimed
> the hybrid was 663,892 bytes / 12.61% smaller than the 5,263,732-byte
> RELA-aware Zucchini patch. That comparison was invalid: it set a `.text`-only
> cost against a whole-binary one. Three independent measurements of what
> Zucchini actually spends on `.text` alone put it between 3.27 MB and 3.53 MB,
> so the 4,599,840-byte hybrid below is 30-41% *larger*, not smaller.
>
> The corrected experiment nonetheless wins, and by a wide margin. A series of
> plan-encoding changes — none of which changes the prediction, all of which
> either move a column onto a basis the decoder can already compute from the
> old image or move information into the layer that carries it more cheaply —
> take the whole-image extension to **2,678,488 bytes, 49.11% below the
> incumbent**, replaying byte-exactly. See
> [`chrome-elf-whole-image.md`](chrome-elf-whole-image.md). The ladder and the
> fairness boundary below stand as measured under the original plan format.

## Fairness boundary

The encoder reads the official old and new debug files, but the replay side
receives only:

1. the stripped old release binary;
2. a serialized function/address plan; and
3. an exact correction stream.

No symbol text or target code bytes are admitted to the replay side. Symbol
names are reduced to 128-bit fingerprints during matching. Every target-derived
function placement, copy choice, section translation, and exact reference
target is serialized and included in the size result. Replaying the plan and
correction reproduces the target `.text` byte-for-byte.

This is intentionally a `.text` feasibility probe rather than a complete ELF
patch. Comparing it with a whole-binary reference favors the spike; a win is a
feasibility result until the omitted regions are charged.

## Corpus inventory

| | old | new |
|---|---:|---:|
| `.text` bytes | 225,738,309 | 225,655,845 |
| `STT_FUNC` symbols | 1,251,397 | 1,251,581 |
| unique non-overlapping address units | 925,590 | 925,663 |
| bytes covered by units | 218,010,606 | 217,925,910 |

Exact-name matching maps 924,395 target units. Masked PC-relative content
matching recovers another 949. Of these, 892,354 units / 177,556,799 bytes are
identical after PC-relative operands are masked. Only 141,950 units /
10,537,513 bytes are byte-identical at their linked addresses. That 16.8× gap
confirms that address regeneration is doing real work.

## Decoder-faithful ladder

All rows below use the production positional correction format and pure-Go
best-compression Zstd. The plan cost is included in every total.

| predictor rung | compressed `.text` bytes | change from previous |
|---|---:|---:|
| normalized-equal functions + relocation | 25,892,132 | — |
| also use old same-name bodies to predict changed functions | 14,001,621 | −11,890,511 |
| translate non-code PC targets through matched ELF sections | 9,729,708 | −4,271,913 |
| add 259,469 exact reference-target correspondences | 7,614,976 | −2,114,732 |
| typed/columnar plan streams | **7,588,419** | −26,557 |

The final row predicts 205,900,434 bytes correctly (91.245%). It scans
57,263,065 instructions and 12,752,985 PC-relative operands. Only 13,061
references remain without a target and 3,259 relocated displacements do not
fit their original width.

The final byte accounting is:

| stream | raw | Zstd | XZ `-9e -T0` |
|---|---:|---:|---:|
| function/reference plan | 5,435,617 | 2,364,194 | 2,088,516 |
| exact correction | 9,034,062 | 5,224,225 | 4,769,744 |
| total | 14,469,679 | **7,588,419** | **6,858,260** |

Compressing both raw streams together in a small archive produces 6,859,760
bytes, so separate XZ streams are not hiding a useful cross-stream gain.

Relative to the 5,263,732-byte RELA-aware Zucchini whole-binary patch, the
best like-terminal-compressor result is 1,594,528 bytes / 30.29% larger. The
Zstd result is 2,324,687 bytes / 44.16% larger.

## Combined coarse-equivalence and structural predictor

The combined test consumes the 158,544 whole-image equivalences from the raw
reference patch but none of its literal, raw-difference, or reference-delta
streams. The equivalence streams are serialized into the new plan and fully
charged. Replay then proceeds through four measured rungs:

1. copy the equivalences into target `.text`;
2. project old reference targets through those same equivalences and retarget
   copied x86 operands;
3. repeat within shipped function boundaries and with section fallbacks;
4. ship one entropy-coded choice bit per mapped function and use the standalone
   symbol-derived prediction only when it has fewer wrong bytes.

The encoder uses target bytes to choose rung 4, but replay does not. It receives
the old stripped binary, equivalence and structural plans, choice bits, and an
exact correction. Applying that correction reproduces target `.text`
byte-for-byte. The choice stream selects the structural prediction for 139,048
functions / 55,406,539 bytes.

| combined rung | correct target bytes | plan Zstd | correction Zstd | total Zstd |
|---|---:|---:|---:|---:|
| equivalences only | 92.420% | 640,382 | 23,506,939 | 24,147,321 |
| equivalence-derived retargeting, no function map | 98.439% | 640,623 | 5,290,055 | 5,930,678 |
| function-boundary structural retargeting | 98.931% | 3,006,346 | 3,180,707 | 6,187,053 |
| per-function choice of both predictors | **99.263%** | **3,071,059** | **2,058,571** | **5,129,630** |

The same final streams with XZ `-9e -T0` are:

| stream | raw | XZ |
|---|---:|---:|
| equivalence + structural + choice plan | 6,321,226 | 2,733,704 |
| exact correction | 2,938,575 | 1,866,136 |
| total | 9,259,801 | **4,599,840** |

This explains the earlier gap more precisely. Large equivalences provide cheap
layout and instruction-boundary continuity; structural target projection
removes relocation churn inside those regions; and function-level selection
recovers cases where the equivalence source is a worse semantic ancestor than
the symbol match. Neither predictor is competitive by itself. The choice bits
add only 59,328 XZ bytes over the retarget plan while reducing the correction
by 923,908 XZ bytes.

It also sharpens the next gate. The hybrid remains 8.74× larger than the
526,373-byte 10× target, and 59.4% of its XZ total is now plan metadata. The
highest-value next experiment is a whole-image hybrid with a like-for-like
`.text` baseline, followed by deriving function boundaries/order from unwind
or equivalence structure so most of the symbol layout does not need to ship.

## Oracle bounds

These rows deliberately copy selected target regions into the encoder's
prediction and therefore are **not patch results**. They measure the exact
correction left if a proposed structural model were perfect and free.

| free perfect prediction | wrong bytes left | correction Zstd | correction XZ |
|---|---:|---:|---:|
| all normalized-equal units | 19,749,241 | 5,214,990 | — |
| every mapped unit | 2,383,833 | 707,642 | 588,496 |
| every target function unit | 1,967,919 | 479,117 | 387,212 |

Perfecting the 892,354 stable units reduces the real 5,224,225-byte correction
by only 9,235 bytes. The relocator and exact edge table have effectively
finished that part of the problem. The remaining useful work is:

- 32,990 mapped-but-changed units, occupying 39,952,387 target bytes;
- 319 unmatched function units; and
- about 1.97 MB of code-section islands outside symbol-sized function bodies.

The 10× target relative to 5,263,732 bytes is 526,373 bytes. Even granting all
function bytes for free, the XZ correction for the remaining `.text` islands
is already 387,212 bytes. That leaves 139,161 bytes for the entire function
layout, all changed code, and every non-code ELF section. This does not prove
an information-theoretic impossibility, but it rules out an order-of-magnitude
result from this function-map model.

## Changed-function and delink probes

The changed units were concatenated in target function order, with the
relocated old bodies as the reference. Strong generic deltas give:

| changed-unit coder | bytes |
|---|---:|
| HDiffPatch, suffix matcher + Zstd 22 | **3,348,826** |
| bsdiff | 3,781,515 |
| xdelta3 | 6,881,712 |
| xdelta3 + XZ | 5,780,464 |

This is a meaningful improvement over the current correction, but an
optimistic composite is still about 6.03 MB for `.text`: 2,088,516 bytes of
XZ plan + 3,348,826 bytes of changed-unit delta + 588,496 bytes of oracle
unmapped correction. It remains 14.5% above the whole-binary reference before
paying for other sections.

A deeper delink experiment zeros PC-relative fields and deltas canonical code
and operand streams separately:

| stream | HDiffPatch bytes |
|---|---:|
| canonical changed-function code | 1,511,442 |
| 2.3 million changed-function reference operands | 1,952,078 |
| total | **3,463,520** |

This is 114,694 bytes worse than delta-coding the linked bodies together.
Separating references exposes reusable canonical code, but correspondence and
operand entropy give the saving back.

## Interpretation and next gates

The experiment rejects two tempting claims:

1. An oracle function permutation plus relocation does not automatically beat
   Zucchini. A million-entry shipped layout is expensive even when compactly
   delta-coded.
2. The remaining result is not dominated by mis-relocated unchanged code.
   It is dominated by genuine or compiler-amplified churn inside the 32,990
   changed units.

Further work should proceed only through gates that attack those measured
costs:

1. **Whole-image hybrid and fair section accounting.** Extend the successful
   per-function combination to the rest of the ELF and generate a like-for-like
   `.text` incumbent baseline. The current 12.61% result is a feasibility win,
   not yet an end-to-end patch win.
2. **Implicit layout derivation.** Replace most of the 2.73 MB XZ combined plan
   with boundaries/order derived from equivalences, unwind data, or large
   unchanged runs. The map-free rung proves equivalence-derived projection
   works, but its residual still needs function boundaries.
3. **Changed-function basic-block graph coding.** Reconstruct canonical target
   blocks and symbolic edges, then measure code and edge streams together.
   The 3.35 MB strong-delta result is the baseline this model must beat by a
   large margin.
4. **Model `.text` islands explicitly.** Jump tables, constant pools, thunks,
   and alignment outside symbol sizes consume 387–479 KB even in the
   all-functions-free oracle. A tenfold claim cannot ignore them.
5. **Add derived ELF tables in the whole-image path.** `.eh_frame_hdr`,
   `.eh_frame`, and other sections can produce useful incremental savings, but
   they cannot repair a multi-megabyte code/layout deficit.

For this Chrome pair, the `.text` hybrid has reached a modest measured win over
5.26 MB under favorable scope. The credible near-term goal is now to retain it
after adding every omitted ELF region. An order-of-magnitude public result
still needs a much smaller first-order release change or a model that derives
changed code and layout rather than shipping them.

## Reproduction

The disposable benchmark is `bench/elfpredict`. A representative invocation
is:

```sh
go run ./bench/elfpredict \
  -old /path/to/151.0.7922.169/chrome \
  -new /path/to/151.0.7922.173/chrome \
  -old-debug /path/to/151.0.7922.169/chrome.debug \
  -new-debug /path/to/151.0.7922.173/chrome.debug \
  -equivalence-patch /path/to/raw-whole-image.patch \
  -out /tmp/elfpredict \
  -reference 5263732
```

The combined run takes about 134 seconds on the measurement host. Unit tests
remain separate and complete in about 3 ms.
