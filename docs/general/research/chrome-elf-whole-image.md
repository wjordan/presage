# Chrome ELF whole-image predictor: like-for-like baseline and extension

Measured 2026-08-27/28 on the Chrome 151.0.7922.169 → 151.0.7922.173 Linux
x86-64 release pair, continuing
[`chrome-elf-predictor-spike.md`](chrome-elf-predictor-spike.md) and
[`chrome-elf-zucchini.md`](chrome-elf-zucchini.md).

The spike ended with a `.text`-only hybrid at 4,599,840 XZ bytes and a claim
that this beat the 5,263,732-byte RELA-aware Zucchini patch by 12.61%. This
note does the two things that claim needed: it measures what Zucchini actually
spends on `.text` alone, and it extends the hybrid to the whole image.

**Headline: the 12.61% win was a scope artifact and is withdrawn — and the
corrected experiment wins anyway, by more.** On a like-for-like `.text`
comparison the hybrid as published was 30-41% *larger* than the incumbent, and
the whole-image extension of it was 46.8% larger.

Every gain since has been a choice of basis rather than a prediction change:
the same information, written down differently. With the encoding fixes of §3,
§7 and §9, the derived tables of §7 and §9 modelled, and the correction split
by region, the whole-image hybrid costs **2,678,488 XZ bytes against the
incumbent's 5,263,732 — 2,585,244 bytes, or 49.11%, smaller**, replaying
byte-exactly.

The prediction itself has barely moved: it has been within a quarter-point of
99% correct since §7.3, and every gain since has come from writing the same
information down differently. That is now the finding rather than the method —
see §10.

## 1. The like-for-like `.text` baseline

Zucchini emits one patch for the whole image, so its `.text` cost has to be
recovered indirectly. Three independent methods, none of which share a failure
mode, agree closely.

| method | `.text` cost (XZ) | what it measures |
|---|---:|---|
| stream attribution of the real patch | 3,534,348 | in-situ, by tagging each patch stream element with the region it targets |
| ablation by subtraction | 3,266,232 | in-situ marginal: 5,263,732 whole − 1,997,500 for a `.text`-stripped pair |
| synthetic `.text`-only pair, corrected | ~3,556,000 | standalone, adjusted for the missing cross-section context |
| synthetic `.text`-only pair, raw | 4,653,604 | standalone, unadjusted |

Stream attribution also serves as a self-check: 3,534,348 (`.text`) +
1,740,124 (non-`.text`) sums to within 0.20% of the 5,263,732 whole patch.

Against the in-situ figures the hybrid **as the spike published it**
(4,599,840) is 30.1% larger than 3,534,348 and 40.8% larger than 3,266,232.
After the §3.4 re-columnisation it is 3,816,288, which is 7.98% and 16.84%
larger respectively -- within noise of the corrected synthetic figure (+7.32%).
`.text` is the one region where the hybrid still loses. By subtraction from
the whole-image total, everything else together costs it about 693,000 XZ
against the incumbent's 1,740,124.

Against the standalone ablation the hybrid now wins outright: 3,816,288
against 4,653,604, 18.0% smaller.

The remaining in-situ gap is the interesting one, because it is the price of
being an isolated codec. In the whole image Zucchini gets 291 MB of source to
match `.text` against; the hybrid's `.text` model does not. Comparing an
isolated codec against an in-situ one is precisely the error the original
claim made, in the other direction, and it is worth about 8-17% here.

### Method trap: `-raw` silently disables disassembly

The first ablation run produced 2,787,936 (`.text` stripped) and 13,216,212
(`.text` only), which would have made the ablation useless. Cause: passing
`-raw` to `zucchini -gen` routes generation through `GenerateBufferRaw`
(`zucchini_integration.cc:75`) and skips the disassembler entirely.

The tell is in the patch header. Bytes 44..48 carry the element type: `Ex64`
means the ELF x86-64 disassembler ran, `NoOp` means the patch is a raw byte
diff. Both `-raw` patches were `NoOp`. Re-run without the flag they are
`Ex64`, and the numbers become 1,997,500 and 4,653,604 — a 2.8× difference on
the `.text`-only figure. Any measurement of Zucchini should assert on those
four bytes.

## 2. Whole-image extension

The decoder receives the stripped old image and the serialized plans, and
nothing else. Equivalences are applied across every byte of the image rather
than cropped to `.text`; structural retargeting and the per-function choice
then run inside the `.text` window as before. Each rung is verified by
replaying it and comparing against the target byte-for-byte.

| rung | correct | plan | correction | total XZ | vs incumbent |
|---|---:|---:|---:|---:|---:|
| equivalence only | 91.150% | 585,656 | 21,646,388 | 22,232,044 | 422.36% |
| + equivalence-derived retargeting | 95.814% | 585,856 | 7,491,892 | 8,077,748 | 153.46% |
| + structural retargeting | 96.196% | 795,212 | 5,609,936 | 6,405,148 | 121.68% |
| + per-function selection | 96.453% | 853,988 | 4,800,940 | 5,654,928 | 107.43% |
| + projected relocations | 98.901% | 950,828 | 2,297,736 | 3,248,564 | 61.72% |
| + modelled `.eh_frame` | 98.949% | 950,852 | 2,146,584 | 3,097,436 | 58.84% |
| + modelled `.rodata` | 99.248% | 952,412 | 1,906,520 | 2,858,932 | 54.31% |
| + corrected address fields | 99.377% | 1,244,060 | 1,434,428 | **2,678,488** | **50.89%** |

The correction column is the region-split measurement of §9.2 and §9.14; with
a single whole-image matcher the last three rungs read 3,256,164 / 3,028,228 /
2,787,848 (61.86% / 57.53% / 52.96%).

The plan column includes both fixes. Before §3.4's re-columnisation the
rungs read 8,597,256 / 7,724,648 / 6,348,156 (163.33% / 146.75% / 120.60%); in
situ that change is worth 783,552 XZ. Before §3.3's join the relocation rung
read 5,565,528 (105.73%); that change is worth a further 1,055,908 XZ. The
`.eh_frame` rung of §7.3 is worth 155,648 XZ and the `.rodata` rung of §9.1 a
further 63,724.

Before §3.4 the `.text` rungs inside this run reproduced the spike's published
numbers exactly, confirming that extending to the whole image did not perturb
the earlier measurements.

Note the shape of the ladder: the *prediction* was never the problem. The
hybrid's `.text` byte-correction stream is 1,700,412 XZ, already 268,836 XZ
**better** than Zucchini's equivalent (raw_delta 1,408,892 + extra_data
560,356 = 1,969,248). The whole of the original deficit was in the plan —
Zucchini encodes the same correspondence and layout in roughly 572,196 XZ
where the spike spent 2,733,696, a 4.8× overspend. §9.7 and §9.9–§9.13 close that gap
and more: the function map fell from 1,319,948 to 256,172 without a single
mapping being dropped. The plan is larger than that suggests only because
§9.15 moved address corrections *into* it, buying 472,092 out
of the correction.

## 3. Findings

### 3.1 Pointer targets must be resolved by identity, not by bytes

Resolving `.rela.dyn` addends through the symbol-derived function map (exact
address points first, then the function map) places **99.075%** of them
exactly. Resolving the same addends through the byte-level equivalence
projection manages only **78.760%**.

The cause is identical-code folding. When two functions have identical bodies
the linker keeps one copy, so a byte-level match is ambiguous about which
identity a pointer refers to — and for a pointer, identity is the whole
question. For a *displacement* the byte projection is fine, because a
displacement only has to land at the right place. The two questions look alike
and are not, and a codec needs separate oracles for them.

This one was measured offline first and held up end-to-end: with the join of
§3.3 in place, the production codec resolves 99.189% of 1,105,993 addends
exactly. It is the only headline number in this work that survived contact
with the full pipeline unchanged.

### 3.2 Derived tables of absolute addresses need a transformed domain

Chrome's `.rela.dyn` is 26 MB of pure address churn: of the 796,001 slots
present in both releases, **4** keep their addend. A byte-domain correction is
the wrong tool — inserting one entry shifts every later slot value, so the
correction pays for the whole tail of the table.

Split into columns, each has a natural domain. Slot addresses are a sorted
list whose *gaps* are a property of the data layout rather than of where that
data landed, so gap-coding turns a global shift into a handful of local edits.
Addends are pointers and resolve through the identity oracle of §3.1.

Measured in the production codec, moving the relocation table into the column
domain removes **2,528,520 XZ** from the whole-image correction and adds
**97,044 XZ** to the plan to carry the column corrections: a net saving of
2,431,476 XZ, a **26× improvement** on doing the same work in the byte domain.

The two columns behave very differently, and the split is informative:

| column | values exact | correction | compressed |
|---|---:|---:|---:|
| slot gaps | 80.755% | 646,192 B | 81,332 |
| addends | 99.189% | 29,498 B | 15,308 |

The addend column is essentially free once the pointers resolve by identity,
which is the §3.1 result showing up end-to-end. The slot-gap column is the
expensive half now, and it is expensive for a reason worth noting: a gap is
only 80.8% predictable because it is a *difference* between two projected
addresses, so it is wrong whenever either endpoint moves — the error rate of a
difference is roughly twice that of its terms.

A useful check on the codec choice: a plain numeric residual (new minus
projected, varint) costs 82,448 for gaps and 23,396 for addends, so the
byte-oriented correction encoder is already the better of the two on both
columns. The column *domain* was the win here, not a bespoke column codec.

### 3.3 Columns must be joined on the key, not on the position

Both columns above initially cost far more than this: the addend correction
was 3,216,275 B / 1,071,796 XZ, only 21.129% of values exact, against an
offline probe that had predicted about 4 KB. Two hypotheses, in order:

**Hypothesis 1: the projected entries need re-sorting.** The linker emits
`R_X86_64_RELATIVE` sorted by slot, so an entry whose slot moves past another
changes index, and the columns are built in old order. Sorting the projected
entries by projected slot should fix it.

Measured: **worse.** Gap 718,543 B and addend 3,469,642 B sorted, against
644,075 B and 3,216,275 B in old order. Instrumenting the order explained why:
of 1,105,974 entries only **809 are inversions** and only **2 slots fail to
project**, so the projected column is already 99.93% ordered. There was almost
nothing for a sort to gain, and an unstable sort loses on ties. Hypothesis
refuted.

**Hypothesis 2: the addends target data, which the function map cannot
resolve.** The map covers `.text`, so if most addends pointed into `.rodata`
or `.data` a 21% hit rate would be expected.

Measured: **refuted, and inverted.** 80.00% of the 1,105,993 addends point
into `.text`, 16.13% into `.rodata`, the rest into `.data.rel.ro`, `.data` and
`.bss`. The 21% exact rate is suspiciously close to the *non*-`.text` share,
meaning the code-targeted addends — the ones the map should be best at — were
the ones coming out wrong.

**The actual defect: positional placement.** Column entry *i* was being filled
from old entry *i*. When a relocation's slot moves past another it occupies a
different index in the linker-sorted new table, so from that point on every
addend is compared against the wrong entry. The offline probe had not hit this
because it paired the two tables *by slot*.

The fix is available to the decoder, which is the part that makes it a codec
change rather than an encoder trick. The decoder corrects the slot-gap column
first — 81,332 compressed bytes — and the exact new slots fall out of it. Those
slots are then the join key for the addend column: for each new slot, take the
projected addend of whatever old entry projects to that slot.

Joined instead of positioned, the addend column goes from 21.129% to
**99.189% exact** and its correction from 3,216,275 to **29,498 bytes**, a
109× reduction. That single change is worth 1,055,908 XZ on the whole-image
total, and it is what takes the hybrid from 105.73% of the incumbent to
85.67%.

The generalisable lesson is not about relocations. Any derived table that the
producer emits in sorted order has a key, and a predictive codec must join on
that key. Position is not the key, it is a consequence of the key, and the
difference only shows up once entries move relative to one another — which is
precisely the case a delta codec exists to handle. The same confound had
already produced three misleading diagnostics earlier in this work; it is
worth treating positional comparison of two sorted tables as suspect by
default.

### 3.4 The function map pays for the same correlation twice

The plan's four mapping streams cost 1,740,668 XZ:

| stream | XZ | content |
|---|---:|---|
| `dstGaps` | 377,696 | bytes between the previous function's end and this start |
| `dstSizes` | 789,156 | destination extent of each function |
| `srcDeltas` | 554,624 | source address, delta-coded against the previous source |
| `sizeDeltas` | 19,192 | source extent minus destination extent |

Both halves are redundant.

**Extents are derivable from starts.** A function runs up to the next start
less that start's leading gap, so `dstSizes` need only carry the *last*
function's size. 22.11% of the 925,344 mappings have no trailing island at
all.

**The source column is correlated with the destination column.** Consecutive
functions keep their relative spacing across a release, so the source address
advances by the same amount as the destination address. Delta-coding the
source column against the *destination* column rather than against itself
makes **894,063 of 925,344 residuals (96.62%) exactly zero**, collapsing
554,624 XZ to 85,324.

Re-columnised, the same information costs 972,440 XZ. Measured on the whole
serialized plan blob — so the comparison includes whatever cross-stream
redundancy XZ was already exploiting — the structural plan goes from
**2,107,780 to 1,339,528 XZ, a saving of 768,252 bytes (36.4%)**. That is
14.6% of the entire incumbent patch budget, recovered purely by choosing a
better basis for two columns. Implemented as plan format `EPP5`.

### 3.5 Ideas measured and killed

**`.eh_frame` as a free decoder-side function-boundary oracle.** Attractive
because it would let the decoder derive function extents without shipping
them. It does not work: the old `.eh_frame` covers only 31,662 function starts
(3.42% of 925,590), and extracting even those costs 2.9× more than shipping
them at the plan's own rate.

The inverse holds, though. `.eh_frame_hdr` is 100% byte-exact regenerable from
`.eh_frame` — the residual is exactly zero — and `.eh_frame` itself
reconstructs for 14,580 XZ against 272,018 today.

**Cross-region matching.** Worth only 2.5%, so a region-by-region hybrid
loses almost nothing architecturally and is much easier to reason about.

## 4. Region inventory

All 291,196,232 bytes of the new image are accounted for. Outside `.text`,
only `.rela.dyn`, `.rodata` and `.eh_frame*` carry material cost; the
irreducible floor for everything non-`.text` is about 355,000 XZ.

## 5. Headroom in the incumbent

Measured while attributing Zucchini's streams, and relevant to how much of the
remaining gap is actually defensible:

- Its x86-64 rel32 detector is a hardcoded five-pattern byte whitelist. It
  covers 64.4% of PC-relative operands present in `.text`.
- It misses 466,974 rel32 fields whose targets lie outside `.text`.
- It structurally cannot model `.text` → `.plt` calls; 131,311 are discarded.

So the 3.27-3.53 MB baseline is not a floor. A reference model that covered
the other 35.6% would lower it further, which raises rather than lowers the
bar for the structural approach.

## 6. Where this leaves the gate

The spike's gate was "extend and measure the hybrid end to end". Done, and the
whole-image hybrid now costs 4,354,816 XZ against the incumbent's 5,263,732 —
**17.27% smaller**, from a starting point of 46.8% larger. What the
measurements establish:

1. **Prediction quality was never the bottleneck.** The `.text` byte
   correction was already 5.2% better than Zucchini's equivalent streams
   before any of this. Every rung that improved the total did so by encoding
   the plan better or by moving a region into a better domain. The prediction
   model itself was not changed once.
2. **The bottleneck was encoding, and it was not information-theoretic.**
   §3.4 removed 36.4% of the structural plan and §3.3 removed 109× from one
   column, both with no loss of fidelity and no new information shipped.
3. **Region-specific transforms beat better byte matching, decisively.** §3.2
   is 26× on one region, §7.3 is 6.8× on another, against single-digit
   percentages for anything that makes the byte matcher smarter.
4. **Most confident hypotheses were wrong.** Of eight tested across both
   experiment sets, five were refuted — twice in §3.3, plus §7.1, §7.2, and
   the `.eh_frame` boundary oracle. Each cost about ten minutes to settle.
   That ratio is the argument for keeping the ladder cheap to re-run, for
   instrumenting a mechanism rather than reasoning about it, and for
   attributing cost before choosing a target (§7).

## 7. Second experiment set: aiming by attribution

The first set optimised whatever looked wrong, and three of four hypotheses
were refuted. The second set started by measuring where the remaining
2,462,424 XZ of correction actually was, before touching anything.

| region | size | wrong | correction | compressed |
|---|---:|---:|---:|---:|
| `.text` | 225,655,845 | 0.723% | 2,853,368 | 1,797,988 |
| `.rodata` | 23,981,900 | 4.873% | 1,282,198 | 409,800 |
| `.eh_frame` | 1,159,416 | 12.876% | 297,824 | 138,080 |
| `.eh_frame_hdr` | 253,308 | 86.312% | 253,315 | 105,656 |
| `.data.rel.ro` | 11,760,272 | 0.177% | 33,840 | 18,492 |
| `.plt` | 21,808 | 11.918% | 5,339 | 1,040 |
| `.rela.plt` | 32,688 | 12.509% | 6,827 | 1,004 |
| 12 further regions | | | | < 500 each |

Regions are corrected independently here, so the column does not sum to the
whole-image figure -- a per-region encoder cannot share matches across regions
-- but as an aiming device it is decisive. It immediately killed the target I
had ranked first.

### 7.1 The lever I had ranked first was worth 1.8%

§6 named the relocation slot-gap column as the leading candidate. The
attribution shows it at 81,332 XZ, 1.8% of the total, and already running at
0.59 bits per entry across 1,105,993 gaps. Its 80.755% hit rate looks poor
next to the addend column's 99.189%, but a low hit rate on a cheap column is
not a lever. Ranking by *badness* rather than by *cost* had put it first.

An attempt to improve it anyway confirmed this. Building the gap column from
the sorted projected slots -- rather than in old order -- looked strictly
better, because the new table is sorted and a gap is only meaningful between
neighbours in that order. Measured: 78.583% exact against 80.755%, and the
whole-image total moved the wrong way, 4,509,620 to 4,513,780. Reverted. The
projected slot column is already 99.93% ordered, so a sort corrects little and
displaces the indices in between, which is exactly what a positional
comparison charges for.

### 7.2 Points are not redundant

With a shift map available (§7.4), the 259,469 exact reference points looked
like they might be derivable: the lookup consults points before the map, so
any point that agrees with the map is dead weight.

Measured: only 1.257% agree, 5,062 disagree, and **251,146 -- 96.8% -- fall
outside the map's coverage entirely**. They are targets that are not inside
any mapped function: island bytes, and addresses in other sections. Dropping
the redundant ones saves 6,156 XZ of 348,080. Not a lever; the points stream
is carrying real information.

### 7.3 `.eh_frame` is `.rela.dyn` again

`.eh_frame` and its index cost 243,736 XZ between them, and both are derived
tables with exactly the structure §3.2 and §3.3 described.

Probing the two images directly:

- 31,611 FDEs old, 31,477 new, governed by 7 CIEs.
- Only **2 of 31,477** FDEs keep their target address. Every
  `initial_location` churns, exactly like a relocation addend.
- **98.211%** of new FDE bodies exist somewhere in the old table. Paired by
  position instead, that reads 31.674% -- the same threefold understatement
  positional pairing produced twice before.
- The table is in **link order, not target order**: only 74.520% of FDEs are
  in ascending target order. A sort-by-target model, which is what §3.3's fix
  would suggest by analogy, would have been wrong here. `.eh_frame_hdr` is the
  sorted index over it, which is why it exists.

So the model is not a re-ordering at all. The entries are matched as ordinary
bytes by the equivalence map; what needs a model is the 4-byte PC-relative
target field inside each one. The decoder recovers each field's old value from
where the equivalence map says those bytes came from, projects the old target
through the address oracle, and re-encodes it against the new field address --
the same operation `.text` already performs on PC-relative operands.

`.eh_frame_hdr` is then regenerated outright: it is a header plus one
(initial_location, fde_pointer) pair per FDE, section-relative and sorted by
address, all of which is recoverable from the projected FDEs.

Measured, all 31,611 FDEs retarget with none unresolved:

| region | wrong before | wrong after | XZ before | XZ after |
|---|---:|---:|---:|---:|
| `.eh_frame` | 12.876% | 6.321% | 138,080 | 79,944 |
| `.eh_frame_hdr` | 86.312% | 61.182% | 105,656 | 15,584 |

Whole-image total 4,510,464 to 4,354,816, a saving of 155,648 XZ. The header
is still 61% wrong byte-for-byte yet costs a seventh of what it did, because
what remains is systematic rather than arbitrary and compresses accordingly.

**A bug worth recording.** The first version walked the *predicted* `.eh_frame`
to find its FDEs, which is the natural reading of "the decoder has the
prediction". It reached 530 of 31,477 FDEs. The predicted section is 12.876%
wrong, so an entry's length field is eventually wrong, and a walker that stops
at the first implausible length stops there — after 1.7% of the table. Driving
from the *old* section, which is exact, and projecting each field forward
through the equivalence map fixed it. The general form: parse the input you
know is correct, not the one you are trying to produce.

### 7.4 The function map is a shift map with 31,281 breakpoints

The largest single item in the plan is the function map. After §3.4 it costs
972,440 XZ to describe where 925,344 functions are and where they came from.

§3.4's own result implies it is far more compressible than that. The source
residual it introduced is `shift_i - shift_(i-1)`, where `shift = src - dst`.
That residual is zero for 96.62% of functions, which does not merely say the
column compresses well -- it says **the old-to-new shift is piecewise constant
with only 31,281 breakpoints across 925,344 functions**, 3.38% of them. Every
address between two breakpoints maps by the same constant.

So the per-function map can be replaced by a source-keyed range map of 31,281
entries. Three preconditions were checked against the real plan before
committing to it:

| precondition | measured |
|---|---:|
| sources that move backwards vs the previous function | 13,275 (1.435%) |
| overlapping source extents once sorted | 412 (0.0445%) |
| bytes where source and destination sizes differ, so a uniform shift is wrong | 917,449 of 217,509,186 (0.4218%) |

All three are small enough to absorb. Within a run of constant shift the range
map and the per-function map agree *exactly* wherever the latter answers at
all, so this is not an approximation of the address oracle -- it is the same
function, encoded in its own terms.

That leaves function *extents*, which are still needed, but only for the
139,048 functions the selector actually chooses: those are the only ones whose
bodies get used. The other 786,296 mappings exist purely to answer address
queries, which the ranges now do.

One thing the ranges cannot do is bound the instruction walker, which
previously ran per function. The ranges serve instead, sorted and clipped in
destination order first -- retargeting rewrites a field in place, so walking
any byte twice would decode an already-rewritten displacement.

**A bug worth recording.** The first attempt aborted the whole run with
`address range 53 is empty or overlapping`. Identical-code folding lets several
destination functions share a single source address, so two shift breakpoints
can land on the same old address and produce a zero-extent range. Keeping the
first at each address is correct rather than merely expedient: the map is
source-keyed and can only answer once, which is precisely the ambiguity the
per-function map already had. The same failure also showed that fault
tolerance had been added to the rung *loop* but not to rung *construction*, so
one unmeasured experiment destroyed six measured ones.

### 7.5 The sparse map works, and is a net loss — which reframes the problem

Measured, the range map delivers exactly what §7.4 predicted and the total gets
worse:

| | plan | correction | total | correct |
|---|---:|---:|---:|---:|
| full per-function map | 2,048,040 | 2,462,424 | 4,510,464 | 98.901% |
| sparse map, 31,171 ranges + 139,048 extents | 1,463,120 | 4,734,800 | 6,197,920 | 98.275% |

The plan fell by 584,920, within 3% of the offline estimate. The correction
rose by 2,272,376. Prediction accuracy fell 0.626 points, which across 291 MB
is about 1.8 MB of newly-wrong bytes — consistent with the correction growth.

The cause is the instruction walker. The full map gives it one span per
function, each starting at a known instruction boundary. The sparse map gives
it one span per shift range, averaging 7.2 MB, so linear x86 decoding runs
through padding and embedded data and loses sync. Re-supplying destination
function starts purely as walk boundaries costs 490,228 XZ, which consumes
five sixths of the 584,920 saved.

**The useful part is what this says about the function map.** It is not
expensive because old-to-new correspondence is expensive: that costs 102,076
XZ as a shift map, a fifth of what the map charges. It is expensive because
the disassembler needs to be told where instructions start. The plan's largest
item is, in substance, a table of instruction boundaries.

That reframes the next gate. Zucchini ships no boundaries at all, because its
rel32 detector is a five-pattern byte whitelist rather than a disassembler and
therefore needs no sync (§5). It pays for that in coverage — 64.4% of
PC-relative operands, and it cannot see `.text` to `.plt` calls at all. A
detector that needs no walk boundaries would let the function map be dropped
in favour of the shift map, worth roughly 1,075,000 XZ combined, at some cost
in reference coverage. That trade is now the open question, and it is a
different question from the one this work started with.

## 8. Scoreboard: twenty-seven predictions, nine right

§7 ended with three ranked candidates and a fourth implied by §7.5. Those,
and everything they led to, are the substance of §9. The scoreboard is worth
stating before the detail, because the ranking was mostly wrong and the
pattern in *how* it was wrong is the most useful thing here:

| prediction | expected | measured |
|---|---:|---:|
| `.rodata` switch tables are an address column (§9.1) | large | **−63,724** ✓ |
| the correction encoder is not one size fits all (§9.2) | untested | **−86,344** ✓ |
| the source column is worth 482,212 (§9.3) | −482,212 | −38,436 ✗ |
| function starts alone are enough for the walker (§9.4) | −416,032 | **+996,796** ✗ |
| short `.rodata` runs are unmodelled tables (§9.5) | moderate | **0** ✗ |
| verifying a table beats finding one (§9.6) | untested | **−165,288** ✓ |
| the gap column is padding the old image shows (§9.7) | moderate | **−363,848** ✓ |
| the equivalence stream has dead entries (§9.8) | moderate | −4,036 ✗ |
| sources and starts are derivable too (§9.9) | −750,540 | **−284,968** ✓ |
| a point should carry its shift, not its address (§9.10) | −171,624 | **−171,732** ✓ |
| an FDE's size can come from the function map (§9.11) | small gain | **+848** ✗ |
| a point's old address is derivable too (§9.12) | −157,600 | **−157,932** ✓ |
| a spurious boundary candidate is free (§9.13) | 0 | **−86,040** ✗ |
| the replacement bytes are one column (§9.14) | 0 | **−70,920** ✗ |
| transposing the four-byte bucket helps (§9.14) | moderate | +27,288 ✗ |
| a wrong field is bytes, not an address (§9.15) | — | **−180,360** ✗ |
| remapping an address needs no check (§9.15) | −220,244 | **+2,175,160** ✗ |
| the equivalence stream is already lean (§9.16) | 0 | **−46,464** ✗ |
| a finer choice than one bit per function pays (§9.17) | moderate | ~+220,000 ✗ |
| §9.15's columns want an index basis (§9.17) | moderate | +8,000 to +17,000 ✗ |
| the correction states its bytes outright (§9.17) | large | −21,672 ✗ |
| the source residual is like its neighbour (§9.18) | moderate | +25,288 ✗ |
| a run's length is derivable (§9.18) | moderate | +69,728 ✗ |
| the residual is misalignment or mismatching (§11) | large | 5,498 B of it ✗ |
| the residual duplicates other code, canonicalised (§11.5) | large | 6.5% ✗ |
| succinct structures beat xz on the sparse columns (§12) | moderate | +100,000 ✗ |
| packed bitfields beat varints (§12) | moderate | **−11,636** ✓ |

Nine of twenty-seven paid on the first try. But the scoreboard undersells the
failures, because five of the ten wrong ones were wrong in a way that was worth
more than being right: they were **claims that something was already finished**,
and refuting them is where 383,424 bytes came from.

**Every prediction about how a stream is written down paid; every prediction
about what could be dropped or was already settled failed.** §9.3, §9.4, §9.5,
§9.8, §9.11, §9.13 and §9.14 all say some version of "this is redundant" or
"this is done", and every one was wrong.

Three of them are claims this document itself made and then had to withdraw:

- §9.9 argued that a boundary detector's false positives are free because the
  plan only indexes the list. §9.13 measured them at **86,040**.
- §9.2 split the correction into three columns and stopped. §9.14 found a
  fourth split inside the third, worth **70,920**.
- §9.8 declared the equivalence stream "already lean" after finding one
  redundancy in it. §9.16 rewrote its source column against the function map
  for **46,464**.

§9.17 and §9.18 are the same pattern run deliberately rather than discovered
by accident: five claims about what remained, each turned into a probe that
only reports. All five held -- the first time in this document that "there is
nothing left here" survived contact with a measurement, and it survived it five
times. That is what makes it worth believing.

Each of the three was argued rather than measured at the time, and each
argument was one I found convincing when I wrote it. The pattern is sharp
enough to state as a rule: **in this work, "there is nothing left here" has
never once been true.** It is the claim that most needs a measurement and is
least likely to get one, because the reason for making it is to stop
measuring.

The one prediction that was quantitatively right rather than merely
directionally right (§9.10) was the one where the column ended up empty. That
is not a coincidence: standalone estimates are only trustworthy when the
column shares nothing with its neighbours, which is exactly the case when
there is nothing left in it.

## 9. What was measured

### 9.1 `.rodata` switch tables

The detector §8 proposed works exactly as described: runs of int32 values that
all land inside `.text` when added to the address of the run's first entry.
On this pair it finds **10,837 tables and 440,629 entries, retargets every one
of them, and leaves none unresolved or unplaced**. `.rodata`'s correction falls
from 409,800 to 345,716 XZ and the whole image from 4,354,816 to 4,291,092
(82.73% to 81.52%).

The entries are self-relative, so nothing in the binary marks them as
addresses — no relocation, no symbol, no section flag. They are found by their
signature and by nothing else. That is the same shape as `.eh_frame` in §7.3
and `.rela.dyn` in §3.3: a derived table of addresses that a byte matcher sees
as noise.

### 9.2 One correction codec is the wrong number

The production correction codec is a matcher: it searches the prediction for
the target's bytes. Against a 291 MB image that is 99.07% correct, what is
left is half a million short runs at unpredictable offsets, and a matcher
spends its budget describing where each run is in a format built for long
copies.

The alternative is to write the same edit list as three columns — the gap
since the last edit, the length, and the replacement bytes — and let the
compressor see three streams of like-valued data. Measured per region at the
best rung:

| region | matcher | columnar |
|---|---:|---:|
| `.text` | 1,797,988 | **1,700,412** |
| `.rodata` | **345,716** | 350,740 |
| `.eh_frame` | 76,288 | **74,600** |
| `.data.rel.ro` | **18,492** | 18,796 |

Neither wins everywhere, and over the whole image the majority swamps the
minority: whole-image columnar is 2,269,216 against the matcher's 2,243,000, a
loss. Splitting the image at the `.text` boundaries and letting each of the
three pieces pick its own codec gives **2,156,656**, and the pick is
matcher-columnar-matcher at every rung. That is 86,344 XZ for a one-byte tag
per piece, and it is the second-largest single gain in this document.

The general point: `.text` and everything else want different instruments, and
measuring the whole image at once hid that for six runs.

### 9.3 The source column costs 38,436, not 482,212

§3.4 priced the plan's four columns by compressing each on its own, and put
the source column at 482,212 XZ — a third of the structural plan. §7.4 then
showed the same information as a shift map with 31,281 breakpoints, and
predicted the difference as a gain.

Encoded and measured, replacing the per-function source column with an
index-keyed shift map — 31,171 breakpoints for 925,344 residuals, 4,910,073
raw plan bytes down to 3,121,300 — moved the compressed plan from 2,048,092 to
**2,009,656. A saving of 38,436, not 482,212.**

The standalone measurement was not wrong, it was answering a different
question. xz compresses the concatenated plan, so the source column's
*marginal* cost next to the three columns it sits beside is an eighth of its
*standalone* cost. Column pricing by isolation overstates by whatever the
columns share.

Removing that column also cost 231,380 references. The shift map gives an
exact source address but no source *extent*, so the walker's address lookup
loses the tail of every function whose old body is longer than its new one:
unknown references went from 13,685 to 245,065 and the correction rose 1.3 MB.
The mode was measured and removed.

### 9.4 Function starts alone: the islands are not padding

§7.5 concluded that the function map is in substance a table of instruction
boundaries. The cheapest test of that is to ship exactly that and nothing
else: every function's start, with each extent running to the next start and
each source coming from the shift map. It is a strict subset of §9.3, dropping
the leading-gap column too.

| | plan | correction | total | correct |
|---|---:|---:|---:|---:|
| full per-function map | 2,048,092 | 2,243,000 | 4,291,092 | 99.070% |
| starts only | 1,632,060 | 3,656,212 | 5,288,272 | 98.674% |

The plan fell by 416,032, as predicted. The correction rose by 1,412,828.

The cause is worth recording because it contradicts the name the gaps were
given. A function's leading gap was assumed to be alignment padding, which a
copy may as well overwrite because both images pad with the same byte. It is
not. The equivalence layer runs first and fills those bytes correctly; the
structural copy runs second and overwrites them. Extending each extent to the
next start therefore destroys correct content at 77.89% of function
boundaries. **The gap column is not describing padding, it is protecting the
layer underneath.**

### 9.5 Short `.rodata` runs are not the missing tables

After §9.1 the `.rodata` residual is 814,246 wrong bytes in 229,532 runs, of
which 196,369 — 85.5% — start on a four-byte boundary. Classifying the wrong
words by what they point at gives an unambiguous answer:

| interpretation | words | share |
|---|---:|---:|
| self-relative into `.text` | 256,547 | 86.3% |
| self-relative into `.rodata` | 36,959 | 12.4% |
| everything else | ~3,600 | 1.2% |

So essentially all of what is left in `.rodata` is still an address column.
The obvious hypothesis was that the run-length-4 threshold was missing short
tables: a two- or three-word switch is the common case, and four in a row
landing in `.text` by chance is only a one-in-160,000 event, so the threshold
is there to avoid false positives rather than because short tables do not
exist.

Enumerating the two- and three-word runs the long scan leaves over finds
**15,909 candidates, of which the encoder — which can check each against the
target — keeps zero**. Not one short run improves the prediction. Whatever the
256,547 words are, they are not short runs in the gaps between the tables
already found.

That leaves two places they can be, both testable: inside the long tables,
retargeted to the wrong value, or in runs the long scan is segmenting from the
wrong base. §10 takes both.

### 9.6 Verifying each `.rodata` table is worth more than finding it

§9.5 built the machinery for the encoder to check a candidate table against
the target and ship one bit. Pointing that machinery at the *long* runs as
well — the ones §9.1 took on their signature alone — changes the picture
completely:

| | spans applied | entries | `.rodata` wrong words | total XZ |
|---|---:|---:|---:|---:|
| signature only (§9.1) | 10,837 | 440,629 | 297,109 | 4,204,768 |
| encoder-verified | 3,555 | 279,794 | 124,614 | **4,040,540** |

**Applying two thirds fewer tables makes the prediction better**, by 165,288
XZ and 0.177 points of accuracy. Most of the 7,282 dropped runs are tables
whose targets did not move, where the equivalence layer already had the bytes
right and retargeting could only disturb them; the rest are runs that land in
`.text` by coincidence. The signature says "this could be a table"; only the
target says "and modelling it helps".

Offering each span under both conventions — entries relative to the table
base, and entries relative to their own address — adds 173 tables. Offering
each of the first four words as the origin, on the theory that a junk word
before a real table would give the whole table the wrong base, adds 4 more
tables and rebases 327; worth 1,364 XZ net of the larger bitmap, which is to
say the base was rarely the problem.

### 9.7 The gap column was paying for something the old image already says

The plan's leading-gap column costs 377,696 XZ to say where each function
ends. Two ways to stop paying for it were measured.

The first is arithmetic: 90.832% of gaps are exactly the padding that aligns
the next function to sixteen bytes, and the residual against that guess costs
10,484 XZ. It is also unusable. The guess needs the previous function's end,
which in a start-and-gap encoding is *derived from the gap*, so the equation
has no solution. Reparameterising to length-and-gap makes it invertible but
costs more than it saves: a length column compresses to 789,156 against a
start column's 490,228, because function starts are sixteen-byte aligned and
lengths are not.

The second works. **The decoder holds the old image, and a function's end is
visible in it**: the bytes after it are `0xcc` padding up to the next function,
so scanning back from the next source start finds the last real byte. That
guess is right for 82.290% of Chrome's 925,344 functions, and its residual
costs 12,668 XZ. The destination gap is then derived rather than shipped.

| | plan xz |
|---|---:|
| EPP6, gap column shipped | 1,319,948 |
| EPP7, source extent guessed from padding | **956,100** |

**363,848 bytes**, and unlike §9.3 the standalone estimate held: the gap column
shares nothing with its neighbours, so its marginal and isolated costs agree.

The general shape is worth naming, because it is the third time it has paid.
The decoder is not given a plan and nothing else — it is given a plan *and the
old image*. Anything the old image already says does not have to be in the
plan. `.rela.dyn`'s slots (§3.3), `.eh_frame`'s FDE structure (§7.3),
`.rodata`'s table spans (§9.1) and now function extents are all read out of the
old image rather than shipped.

### 9.8 The equivalence stream is already lean

The last unexamined plan item was the equivalence stream at 585,576 XZ. The
obvious redundancy is that a function the selector chose has its body
overwritten by the structural copy, so equivalence entries landing inside it
are dead weight.

Measured: of 158,544 entries, 154,645 are in `.text`, and **1,853 are fully
contained in a chosen function**. Dropping them takes the stream to 581,540 —
4,036 bytes. An equivalence run averages 1,822 bytes and a chosen function 399,
so a run almost never fits inside one function; it spans dozens.

At 3.7 compressed bytes per entry for 288 MB of copies, this stream is not
where the money is.

### 9.9 The source column is an index into a list the old image already carries

§9.7 left the function map at 956,100 XZ in two address columns: where each
function comes from in the old image, and where it goes in the new one. Both
are delta-coded against their predecessor, and both are paying for something
the decoder can already see.

**Sources.** The same padding signal as §9.7, read forwards instead of
backwards: a byte that is not `0xcc` and follows one that is begins a function.
Scanning old `.text` finds 1,352,889 such boundaries. It over-reads by half —
only 882,098 of them are real function starts, the rest are data islands and
padding inside function bodies — but that does not matter, because the column
does not have to *name* a boundary, only *index* one. 95.368% of the 925,344
mappings begin exactly on a detected boundary; the rest are named by the
boundary below them plus an offset.

An index delta is small and repetitive where an address delta is neither:

| source column | xz |
|---|---:|
| address, delta-coded against the previous source | 554,624 |
| boundary index delta + offset | **283,828** |

The offset also disposes of a hazard the earlier encoding had to work around.
Identical-code folding gives several destination functions the same source, so
a source delta of zero is legitimate and cannot double as a sentinel for "not
a boundary". Index-plus-offset needs no sentinel: an offset of zero simply
means the source *is* the boundary.

**Destinations.** §9.7 measured that 90.832% of destination gaps are exactly
the padding aligning the next function to sixteen bytes, and could not use it,
because in a start-and-gap encoding the previous function's end is itself
derived from the gap. §9.7 then removed the gap column — which makes the
previous end available, and the guess computable: the next function starts at
the previous end rounded up to sixteen. The column carries only the residual,
at 10,484 XZ against the start column's 490,228.

| | plan xz |
|---|---:|
| EPP7 | 956,100 |
| EPP8, boundary index + alignment residual | **671,132** |

**284,968 bytes.** The two standalone savings sum to 750,540, so the §9.3
discount is in force again and by now it is the expected result: these columns
sit in one xz stream and are strongly correlated, so entropy removed from one
was partly being paid for by the others.

### 9.10 A branch target's shift almost never changes

The plan carries 259,469 address points — the (old, new) pairs anchoring the
shift map of §7.2 — with the new address delta-coded against the previous new
address. That is §9.9's mistake once more: the *pair* is the datum, not either
address on its own.

Coding `new − old`, and then delta-coding that shift against the previous
point's shift, empties the column. 257,392 of 259,469 points (99.200%) carry
the same shift as their predecessor, so what ships is a run of zeros with
2,077 breaks in it:

| point column | xz |
|---|---:|
| new address, delta-coded | 176,096 |
| shift change | **4,472** |

In situ the plan falls from 1,415,128 to 1,243,396 — **171,732 bytes** — and
this time the standalone and marginal figures agree to within a hundred bytes.
A column of zeros shares nothing with its neighbours because it has nothing
left to share.

That takes the whole image to **3,220,836 XZ, 61.19% of the incumbent**.

### 9.11 A size may only be predicted where both sides agree what it sizes

Two smaller results, both about trusting a model only where it can be checked.

An FDE's `address_range` field is the size of the function it describes.
§7.3's model rewrote the FDE's start address and left the size alone, and the
function map has a destination extent for every function, so writing it in
looks free. Done unconditionally it **lost 848 XZ**. The two disagree about
what a function is: the map's extent comes from §9.7's padding scan and the
FDE's from the compiler, and wherever they differ the model overwrites a right
answer with a wrong one.

The test that fixes it is agreement about the *old* image, which both sides
hold. An FDE names its function by where that function used to be, so the map
can be keyed by old address; if the map's old extent equals the size the FDE
already carries, the two agree about the old function and the map's new extent
can be trusted. 497 of 31,611 FDEs are rewritten under that test, and it gains
instead of losing.

The second is §9.2's split threshold. That section cut the image at the
`.text` boundaries, three pieces, because `.text` was the only region big
enough to have its own character. Cutting at the boundaries of every section
of at least a megabyte gives thirteen pieces, and at the best rung five of
them choose the columnar codec where three-piece splitting had to choose one
codec for all of `.rodata`, `.eh_frame` and `.data.rel.ro` together. The two
changes together are worth 12,132 XZ, the larger part of it the split.

### 9.12 The point table names addresses the old image already points at

§9.10 emptied the point table's *shift* column and left its *old address*
column untouched at 171,984 XZ — by then the second-largest item in the whole
plan, behind only the source column of §9.9. 259,469 addresses at 0.66
compressed bytes each is not obviously wasteful, and that is what made it easy
to stop looking.

It is entirely wasteful, because of what a point is. A point exists to correct
the oracle's answer for some old address, and the oracle is only ever asked
about addresses that appear as branch or call targets in the old image. So
every point's old address is, by construction, a target of some instruction in
old `.text` — and the decoder holds old `.text`, and by the time it reads the
point table it has already decoded the function map that says which bodies to
walk. It can enumerate the entire domain for itself.

Walking the 924,932 distinct mapped bodies yields 6,093,998 distinct target
addresses, and **all 259,469 points are in that list** — not most of them, all
of them, which is what the argument predicts rather than merely hopes. The
column becomes an index:

| point old-address column | xz |
|---|---:|
| address, delta-coded | 171,984 |
| index into the walked target list | **14,404** |

The offset column that guards against a point falling between two enumerated
targets is 180 bytes, because it is never used.

| | plan xz |
|---|---:|
| EPP9 | 499,812 |
| EPP10, points indexed into the walked target list | **342,212** |

**157,600 bytes**, and standalone again equals marginal, for §9.10's reason.

The cost is decode time: the decoder now walks old `.text` twice, once to build
this domain and once to retarget. That took the plan's round trip from 2.7 to
54.5 seconds on this pair. For a spike measuring compressed size that is
irrelevant; for a shipping decoder it is a real trade, and it is the first time
in this document that a gain has cost anything but effort.

### 9.13 A spurious candidate is not free after all

§9.9 waved away the boundary detector's precision: it offered 1,352,889
candidates for 924,932 real function starts, and the note said the excess "does
not matter, because the column does not have to name a boundary, only index
one." That is wrong, and §9.12 is what made it obvious. An index is not free of
its list. Every false candidate sitting below a real start pushes that start's
index up by one, and the column ships *deltas* between consecutive indices, so
a stretch of dense false positives widens every delta crossing it.

Real function starts are aligned. Requiring a candidate to be eight-byte
aligned discards 414,381 of the 1,352,889 raw candidates and **six** of the
882,098 real starts:

| candidate filter | candidates | precision | sources exact | index + offset xz |
|---|---:|---:|---:|---:|
| non-padding after padding | 1,352,889 | 65.2% | 95.37% | 283,828 |
| + 4-byte aligned | 997,475 | 88.4% | 95.37% | 219,336 |
| + **8-byte aligned** | 938,508 | 94.0% | 95.37% | **197,684** |
| + 16-byte aligned | 828,488 | 96.6% | 86.49% | 197,956 |

Sixteen is what the compiler actually aligns to and it prunes hardest, but it
loses 82,114 real starts to the offset column and comes out level. Eight is the
alignment that costs nothing on this pair: precision rises half again while the
exact-hit rate does not move at all.

A filter on padding length instead of alignment was measured and is much worse
— requiring four padding bytes reaches 99.1% precision but drops the exact-hit
rate to 59.66% and the pair to 417,024. Precision is only worth having when it
is free of recall.

| | plan xz |
|---|---:|
| EPP10 | 342,212 |
| EPP11, candidates aligned to eight | **256,172** |

**86,040 bytes.** Across §9.7 and §9.9–§9.13 the function map has gone from
1,319,948 to 256,172 compressed bytes — a fifth of its original size — without
a single mapping, point or range being dropped from it.

### 9.14 The replacement bytes are not one column either

§9.2 split the correction into three columns — where the edit is, how long it
is, and what to put there — and stopped. The third column is still a mixture.
The shape diagnostic says .text's 520,393 wrong runs are 471,625 of four bytes
or fewer, 17,808 of five to eight, and a long tail; a one-byte run is a lone
changed opcode byte and a four-byte run is usually a displacement field whose
high bytes are nearly constant while its low bytes are noise. Concatenated,
they interleave structure with noise in the same stream.

The decoder reads a run's length out of the length column before it needs that
run's bytes, so it always knows which of several byte columns to draw from,
and the split needs no signalling at all. Bucketing by run length — one to
four bytes each in their own column, five and over sharing the last:

| .text byte column | xz |
|---|---:|
| one column | 1,151,588 |
| bucketed by run length | **1,091,100** |
| four-byte bucket also transposed into per-position planes | 1,118,388 |

Transposing the four-byte bucket, so that all the first bytes are adjacent and
then all the second bytes, is *worse*. That is worth stating because it is the
obvious next move and the reasoning behind it — the high byte of a
displacement is nearly constant, so gather the high bytes — is sound. It fails
because LZMA already models a four-byte period across a run of like-sized
records, and transposing destroys the match distances it was exploiting.

In situ the whole-image correction falls from 1,977,440 to **1,906,520**, so
70,920 rather than the 60,488 measured on `.text` alone: every region has a
length distribution, and the four that pick the columnar codec all gain.

### 9.15 A quarter of the correction was not bytes, it was addresses

Everything above this point moved information between columns. This is the
first change that moves it between *layers*: out of the byte correction and
into the plan, because the byte correction was the wrong domain for a quarter
of what it was carrying.

The residual split of §7 said 24.5% of `.text`'s wrong bytes fall inside
relocated fields — fields the retargeter wrote and got wrong. Shipping four
literal bytes to fix a displacement is shipping the answer when only the
difference was needed, and the decoder can find those fields on its own: it
holds the prediction, and walking the prediction's instructions is what
produced the fields in the first place. Instruction lengths do not depend on
displacement values, so the field list is stable under any rewrite of it.

Chrome's `.text` has **8,666,697** four-byte displacement fields. Two layers
were measured over them.

**Correct the field.** One signed difference per wrong field, indexed into the
field list. 164,998 fields are wrong, holding 375,588 wrong bytes; the layer
costs 352,068 and takes `.text`'s correction from 1,700,412 to 1,194,868. Net
**+153,476**.

**Correct the address.** A field holds a displacement to an address, so a
wrong field means the oracle returned the wrong address — and where several
fields chose the same wrong address, one entry fixes all of them. The domain
is the 2,975,069 distinct addresses the prediction points at, which the
decoder can also enumerate, so an entry is an index and a shift, exactly as in
§9.10 and §9.12.

Applied on that reasoning alone it is a **−2,175,160 catastrophe**: it fixes
116,857 of the 164,998 wrong fields and takes the correction from 1,700,412 to
3,655,328. A wrong address is frequently the *right* address somewhere else —
identical-code folding put several old functions at one address and the new
build pulled them apart — so remapping it repairs one call site and breaks
three.

This is §9.6 for the third time. The encoder holds the target, so it can score
each candidate: how many fields the remap fixes, less how many it breaks. On
that test **66,637 remaps are kept and 25,689 rejected**, and what the encoder
keeps rewrites 128,762 fields correctly. The 54,579 fields left over — the
minority destinations, and the groups that were rejected — fall through to the
per-field layer, which then makes every one of the 8,666,697 fields exactly
right.

| | plan | correction | total | correct |
|---|---:|---:|---:|---:|
| modelled `.rodata` | 998,792 | 1,906,520 | 2,905,312 | 99.248% |
| + per-field deltas only | — | — | ~2,751,836 | — |
| + verified remap, then deltas | 1,290,524 | 1,434,428 | **2,724,952** | **99.377%** |

**180,360 bytes**, of which grouping the addresses is worth 26,884 over
correcting each field on its own. The plan grows by 291,732 and the correction
falls by 472,092: the first time in this document that making the plan bigger
was the right move.

Two refinements of the delta column were measured and rejected. Coding each
delta as the change from the previous one costs 122,576 against 112,028 —
consecutive misses do not share a magnitude, because the fields that miss are
scattered across the image rather than clustered in one moved region. And
declining to ship a delta wider than the bytes it fixes saves 35,516 in the
plan while pushing 12,950 fields back into the byte correction.

### 9.16 The equivalence stream can read the function map too

§9.8 closed the equivalence stream as "already lean" at 585,576 XZ for 158,544
entries, having found only one redundancy in it and that one worth 4,036. The
question it did not ask is the one §9.9 and §9.12 turned out to hinge on: not
*is this stream redundant* but *is it written against the right thing*.

Its source column is written against itself — each run's source as the signed
distance from where the previous run ended, 330,432 XZ of the plan. But the
plan already carries a function map that says, for 217 MB of `.text`, exactly
where the new bytes came from. Where an equivalence run starts inside a mapped
function, the map has already been paid for and can predict the source.

Which column an entry uses needs no flag: the destination decides it, and the
destination column is decoded first. Coverage is 133,768 of 158,544 entries,
and only 16.68% of covered sources are predicted exactly — much weaker than
the 95.37% of §9.9, because the byte matcher segments on content and the map
segments on functions, so a run rarely begins where a function does. It does
not need to be exact. It needs to be *close*, and the residual is small even
when it is not zero:

| source column | xz |
|---|---:|
| distance from the previous run's end | 330,432 |
| residual against the map, plus that distance for the rest | **284,624** |

This is the first change that requires the decoder to reorder its work. The
function map has to be decoded before the equivalences, which is possible
because the map needs only the old image's `.text` and the equivalence header
names that before any equivalence is read.

In situ the equivalence plan falls from 585,644 to **539,300** and the whole
image to **2,678,488 XZ, 50.89% of the incumbent**.

The coupling this introduces is real and cost a failed rung to find. The source
column is written against *a* map, so a plan shipping a different one cannot
read it — which is exactly what the sparse rung did, having been handed the
dense rung's equivalence stream. `predictImage` builds its predictor out of the
plan's own structure, so a decoder cannot get this wrong; only an encoder
assembling a plan out of mismatched parts can.

### 9.17 Three claims, measured cheaply

§10 of an earlier revision of this document listed three things it believed
about the remaining cost. This document's own record says a belief of that
shape is the one most worth measuring, so all three were turned into encoder
probes that only report. The probes also forced a change in how the benchmark
is run, which is written up first because it is what made asking affordable.

**The harness was the bottleneck, not the question.** Answering any of these
meant a 25-minute run, because the driver replays all eleven rungs and each one
compresses a multi-megabyte correction with `xz -9e`. None of that work bears
on a probe. The driver now takes `-rungs` to name which rungs to measure --
which also skips *constructing* the ones it skips, and with them the relocation,
`eh_frame`, `rodata` and field plans and their two extra whole-image
predictions -- and `-probes` to report against a rung's prediction without
measuring it at all. About a dozen reporting compressions over ~1 MB streams
are gated off in probe mode, and §9.12's walk of the old image, the single
largest fixed cost at roughly fifty seconds, is now memoised on disk under a
key covering the old text and the mappings that walk it. Two questions that
would have taken 50 minutes serially were answered in **2.5 minutes, in
parallel**.

One trap is worth recording. Multithreaded `xz` splits a large input into
independently coded blocks, so `xz(dictionary ++ data) - xz(dictionary)` under
`-T0` would have measured a stream that could not see its own dictionary. The
dictionary probe compresses `-T1` for that reason. The probe was calibrated
first on 440,666 bytes of 5-40 byte fragments drawn from an 8 MB random
dictionary: 437,492 alone, 71,144 marginal, a 6.1x effect. A probe that cannot
show a large effect on a case that has one is not evidence of anything.

**Claim one: a choice finer than one bit per function would pay.** It does not,
by a factor of about twenty. Splitting each mapping into chunks and letting an
oracle pick the better prediction per chunk:

| granularity | wrong bytes | vs per function | selection bits |
|---|---:|---:|---:|
| per function | 1,564,767 | — | 925,344 |
| 16 bytes | 1,520,636 | −44,131 | 14,028,609 |
| 64 bytes | 1,538,299 | −26,468 | 3,932,888 |
| 256 bytes | 1,550,022 | −14,745 | 1,474,730 |

The existing bitmap costs 59,152 XZ for 925,344 bits. At that rate 64-byte
choices would spend about 250,000 to save 26,468, and the saving is an oracle
bound that ignores the selection entirely. The counts are taken at the
retargeting rung, before the relocation, `rodata` and field models close their
own share; downstream the gap can only be smaller. Where a function is
half-recompiled, both predictions are wrong in the same half.

**Claim three: the correction layer states its bytes outright.** This is the
principle that paid seven times, asked for the first time of the byte
correction rather than the plan. The decoder holds the entire predicted image
while it applies the correction, and 470,956 XZ -- 40% of `.text`'s correction
and the largest single sub-item in the patch -- is the bucket of runs five
bytes and longer, 47,078 of them holding 796,298 bytes of replacement. Those
are new instruction sequences, and `.text` is 225,655,845 bytes of
near-identical template-instantiated code. A coder able to reference the
prediction might not have to spell them out.

| | xz for the 796,298 bytes |
|---|---:|
| stated alone, as now | 470,956 |
| marginal after a 16 MiB prediction dictionary | 454,588 |
| marginal after a 64 MiB prediction dictionary | **449,284** |

**4.6%**, and quadrupling the dictionary bought 1.1 points of it. (This probe
was later re-run with both sides canonicalised, which is the form the question
should have been asked in; §11.5 has the result, and the conclusion holds.) The obvious
objection is coverage — 64 MiB is 30% of `.text` — but that is what the two
rows are for: the curve from 16 to 64 MiB is nearly flat, so extending it over
the remaining 70% buys a point or so more. On the calibration case the same
probe showed 6.1x. The bytes are not there to be found: what a recompile emits
differently is genuinely new, not old code relocated, and no dictionary drawn
from the prediction reaches it. This is the direct evidence for the claim §10
could previously only infer from the residual's shape.

**Claim two: §9.15's two value columns are on the wrong basis.** §9.10 and
§9.12 both paid by replacing a value with an index into a set the decoder can
enumerate, and neither of §9.15's columns does that -- a remap ships a signed
shift, a field delta ships a signed difference. The domain to index is the
2,975,069 addresses the prediction points at, plus the function starts it
placed: 3,198,339 entries.

| column | as written | index where possible, escape otherwise | floor index + residual |
|---|---:|---:|---:|
| remap shift | **112,492** | 32,760 + 60,004 = 92,764 | 75,248 + 45,696 = 120,944 |
| field delta | **112,028** | 38,668 + 69,512 = 108,180 | 89,660 + 39,564 = 129,224 |

Both alternatives lose. The middle column needs a tag per entry saying which of
the two encodings it used -- 121,216 bits, unpriced above and worth more than
the margin -- because only 42% and 38% of destinations land on a domain
address. The right-hand column needs no tag, taking the floor entry and stating
the remainder as §9.12 does for points, and it is the worst of the three.

The reason is a distinction the earlier sections did not have to make. Indexing
pays when a value spans the image, because the domain is far sparser than the
address space it covers. A field that missed usually missed by a *little*, so
its signed difference is already small, while its index difference has to count
every one of the 3.2 million domain entries lying between the two addresses.
**Local values want the byte basis; image-spanning values want the index
basis.** §9.12's points were the second kind. §9.15's misses are the first.

### 9.18 The equivalence stream, asked twice more

The per-column attribution added in §9.17 prices the equivalence stream for the
first time: destinations 68,204, lengths 186,992, sources 57,836 + 226,788. The
source residual is the largest column in the largest plan stream, and §9.16
left it in a state that invites one more question -- only 16.68% of covered
sources are predicted exactly, so the column is almost all non-zero.

**Are the residuals like their neighbours?** Consecutive runs inside one
function share that function's internal shift, so their residuals should be
nearly equal and the column should be differenced rather than absolute. The
same applies to lengths, which repeat heavily.

| column | absolute | delta | delta, reset per function |
|---|---:|---:|---:|
| source residual | **226,788** | 252,076 | 247,452 |
| copy length | **186,992** | 221,360 | — |

Both lose, by 25,288 and 34,368. This is §9.15's rejected delta-of-delta a
second time, and the reason is now clear enough to state as a rule: **equal
consecutive values are literal repetition, which is what LZ is for.**
Differencing them converts a match xz would have found for nearly nothing into
a run of zeros, while scattering the unequal values into high entropy. A basis
change only pays against a coder that is not already modelling the structure.

**Is a run's length something the plan already says?** Two candidates the
decoder can compute: the rest of the mapped function the run starts in, and the
distance to where the next run begins. Only 6,440 of 158,544 runs end with
their function, and that residual costs 256,720 against the 186,992 it would
replace.

The second candidate reported 82,900 of 158,544 exact and a residual of 68,932
against 186,992 -- an apparent 118,060, and a tautology. A run's length plus
the gap after it *is* the distance to the next run, so "length as a residual
against that distance" is the negated gap column, which the plan already ships
at 68,204. The 728-byte difference between the two figures is zigzag against
unsigned varint encoding and nothing else. The 52.3% abutment rate is precisely
*why* the destination column costs only 68,204; it is already spent.

That near-miss is the methodological point of this section. A probe that
re-expresses the same pair of values in the other order will report a large
win, because it is measuring one of the two columns while the other is off the
books. The check is whether the decoder could actually run the arithmetic in
the order proposed -- here it could not, since the next destination is not
known until the current length is.

The equivalence stream costs 1.18 bytes per run for lengths and 1.70 for
covered sources. Nothing in it is derivable that is not already derived.

## 10. Where this stops

The whole image costs **2,678,488 XZ bytes against the incumbent's 5,263,732 —
49.11% smaller**, replaying byte-exactly. Every byte of that came from
encoding: the prediction is 99.377% correct and was 98.9% correct sixteen
experiments ago.

Here is everything left, measured at the best rung:

Each region's figure is the codec the splitter actually ships for it, not the
matcher's price on it — `.text`, `.rodata` and `.eh_frame` all go out
columnar, and quoting the matcher overstates `.text` alone by 98,188. The nine
rows are exact and sum to the total.

| | xz | share |
|---|---:|---:|
| `.text` byte correction | 1,167,832 | 43.6% |
| equivalence stream | 539,300 | 20.1% |
| address-field corrections (§9.15) | 290,608 | 10.8% |
| function map (§9.13) | 256,172 | 9.6% |
| `.rodata` byte correction | 165,360 | 6.2% |
| `.rela.dyn` plan | 97,124 | 3.6% |
| `.eh_frame` byte correction | 67,576 | 2.5% |
| per-function choice bitmap | 59,152 | 2.2% |
| everything else | 35,364 | 1.3% |

**The address domain is finished.** That is the substantive finding of this
last set, and it is measurable rather than asserted. Before §9.15, 24.5% of
`.text`'s wrong bytes lay inside relocated fields; after it, **1.9%** do — and
those 23,569 bytes are the one- and two-byte displacement fields the layer does
not cover, worth perhaps 10,000. The remaining 1,232,241 wrong bytes are spread
over 341,778 runs that touch no field at all. They are not addresses the
predictor got wrong. They are instructions the compiler emitted differently:
different register allocation, different inlining, a different PGO profile
between 151.0.7922.169 and .173. No transform of the address domain reaches
them, because they are not in it.

Eight items were probed and returned negative, and are recorded here so they
are not probed again. Five are in §9.17 and §9.18; the other three are:

- **`.rodata`'s 180,540** is not mismodelled tables. Of its wrong aligned
  words, only 26,214 are covered by an equivalence at all, and only 6,353 of
  those are a self-relative pointer whose old target the map can project
  forward. The rest is `.rodata` the matcher found no source for — new data,
  which has to be shipped.
- **The choice bitmap's 59,152** is 925,344 bits at 15.03% density. An
  independent coder would spend 70,557 bytes on that; xz spends 59,152, so it
  is already exploiting correlation the bits have. Run-length coding them costs
  64,628.
- **Gating the field deltas** — declining to ship a delta wider than the bytes
  it fixes — saves 35,516 in the plan and costs 42,756 in the correction, for a
  net loss of 7,176. Measured as its own rung rather than estimated.

What is left that has not been refuted is the same architectural item §7.5
raised and §9 never took: the function map is in substance a table of
instruction start positions for the disassembler, and a reference detector that
needed no walk boundaries would let a 256,172-byte map be replaced by a
102,076-byte shift map. That was worth ~1,075,000 when the map cost 1,319,948.
It is worth at most 154,000 now, and the sparse-plan rung of §7.5 says taking
it costs more in correction than it saves in plan. §9.7 through §9.16 have
made that lever small by making the map cheap instead.

Five further claims this section wanted to make were turned into probes first,
because this document's record says "there is nothing left here" is exactly the
claim that gets asserted instead of measured. All five came back confirming it.
A choice finer than one bit per function loses by a factor of about twenty
(§9.17). Both of §9.15's value columns are already on the right basis, because
their values are local and indexing only pays for values that span the image
(§9.17). The long runs in `.text`'s residual are 4.6% cheaper after a 64 MiB
dictionary drawn from the predicted image, on a probe calibrated to show 6.1x
where the redundancy exists (§9.17). And the equivalence stream's two largest
columns are both worse when differenced, and its lengths are not derivable from
anything the plan already carries (§9.18).

That last one is the load-bearing measurement. It says the residual is not old
code the predictor failed to locate — it is code that does not exist anywhere
in the old build. So the honest statement of the gate is: **the encoding work
is done, and the next gain has to come from a better prediction, not a better
plan.** Sixteen consecutive experiments took the same prediction from
4,599,840 to 2,678,488 without changing a single modelled byte. The
seventeenth cannot; 47% of what remains is compiler output that differs, and
the only lever on that is predicting the new compiler output better.

One methodological note belongs with that verdict. Every question above used
to cost a 25-minute run, because the driver replayed eleven rungs and
compressed a multi-megabyte correction for each. With `-rungs` to measure one
rung instead of eleven, `-probes` to report against a prediction without
encoding a correction at all, and a disk memo of §9.12's walk of the old image,
the same questions cost **3m30s for the ones needing a whole-image prediction
and 1.5 seconds for the ones that do not**. The full ladder still reproduces
2,678,488; a single-rung confirmation of it takes 1m52s. Cheap probes are what
made it affordable to attack this section's own conclusion five times instead
of asserting it once. §9.17 has the detail.

One cost was incurred along the way and should not be lost. §9.12 made the
decoder walk the old image's `.text` an extra time to build its index domain,
and the plan's round trip on this pair went from 2.7 seconds to 54.5; §9.15
walks the *predicted* `.text` on top of that. Compressed size was the only
thing being optimised here, and it is the only thing that improved.

## 11. What a better prediction would have to do

§10 ends by saying the next gain has to come from a better prediction. That is
a verdict without a target, which is the same failure mode §8 catalogues: a
claim shaped like a conclusion that nobody has to measure. This section
measures it. Every probe here is report-only and runs in about ninety seconds
on the harness §9.17 built.

### 11.1 The yardstick

`.text` ships 1,167,832 for 1,255,810 wrong bytes in 362,032 runs -- positions
295,492, lengths 88,700, bytes 783,640. That is **1.061 patch bytes per run
plus 0.624 per wrong byte**, so an average run of 3.47 bytes costs 3.23. Any
proposal has to be priced this way and not by byte count, because the residual
is mostly short runs and their positions cost nearly as much as their contents.

### 11.2 The residual by cause

Classifying each mapped function by how its new body relates to its old one:

| | functions | region | wrong bytes | runs | density |
|---|---:|---:|---:|---:|---:|
| differs only in PC-relative fields | 29 | 13 KB | **51** | 51 | 0.39% |
| same length, content changed | 22,701 | 16.8 MB | 157,042 | 65,321 | 0.93% |
| resized | 8,991 | 21.7 MB | **1,000,045** | 265,557 | 4.60% |
| content matches a *different* old function | 282 | 57 KB | 1,242 | — | 2.19% |
| outside any mapping | — | 8.1 MB | 98,672 | — | 1.21% |

Three things close here. **Retargeting is wrong on 51 bytes**, which is the
strongest possible confirmation that §9.15 finished the address domain.
**Better content-matching is worth 1,242 bytes** -- there is no meaningful set
of functions whose body already exists in the old image under a different name.
And **extending map coverage is worth at most ~91,000**: the uncovered 3.6% of
`.text` is 2.3x denser in errors but far too small to matter.

What is left is a concentration. **8,991 functions -- 0.97% of them -- hold 80%
of the residual**, and they are not functions that were replaced: 21,829,012
bytes of them became 21,731,933, a 0.44% net change. They are 2.4 KB on
average, 4.6% wrong, in 29.5 runs each. The strategic consequence is that a
diffuse improvement is worthless, because 99% of functions are already perfect,
while an expensive per-function model aimed at these has a budget of about 100
bytes per function before it stops paying.

### 11.3 The residual by instruction

Decoding the target's instruction stream and comparing each instruction against
whatever the prediction holds at the same offset -- both are in new-image
coordinates, so no alignment is assumed -- splits the 1,157,088 wrong bytes
inside mapped functions:

| | instructions | wrong bytes | share |
|---|---:|---:|---:|
| same operation, same length -- operand differs | 252,229 | 360,392 | 31.1% |
| same operation, different length | 5,995 | 16,928 | 1.5% |
| different operation, same length | 5,337 | 7,998 | 0.7% |
| **different operation and length** | 194,916 | **767,244** | **66.3%** |
| prediction does not decode there | 1,392 | 4,526 | 0.4% |

The two-thirds share has two readings that point opposite ways. Either the
prediction holds genuinely different code, or it is *misaligned* -- a byte-level
match placed off an instruction boundary decodes into garbage that looks like a
different instruction. The second reading would make an instruction-aware
matcher worth most of 767,244 bytes plus the 194,916 runs they cost.

It is the first. Splitting the bucket by whether the target's instruction
boundary is also a boundary in the prediction's own instruction stream:

| | instructions | wrong bytes |
|---|---:|---:|
| prediction is on an instruction boundary | 193,636 | **762,988** |
| prediction is mid-instruction (misaligned) | 1,280 | 4,256 |

**99.4% on-boundary.** The matcher is placing its predictions correctly and the
instruction there is genuinely different. Two-thirds of the residual is
recompiled code, and §10's verdict is confirmed rather than merely asserted.

### 11.4 What that leaves, priced

- **Operand-field correction**, the §9.15 layer generalised from PC-relative
  displacements to modrm, SIB and immediate fields. Addresses the 31.1%:
  eliminating those 252,229 instructions saves 492,500. But §9.15's own layer
  cost 2.72 bytes per entry (0.67 index, 2.05 value), and 252,229 entries at
  that rate cost 686,063. **The sign is genuinely unknown: between +114,000 and
  −194,000**, turning entirely on whether an operand value is cheaper than a
  four-byte displacement delta. Worth one more column-pricing probe before any
  build.

  *Priced 2026-09-01* (`bench/elfpredict -probes opfield`, the layer run
  as a decoder would: the domain is every instruction the length decoder
  finds in a mapped body of the prediction, an entry is (index delta, field
  class, value), every row round-trips). Against the production residual
  coder — columnar shape, `DispContext`, the prediction-conditioned CM —
  the `.text` piece is 1,064,218 and the classes price as:

  | class | entries | column | correction | net |
  |---|---:|---:|---:|---:|
  | immediate only | 48,720 | +80,782 | −97,414 | **−16,632** |
  | registers only | 80,628 | +145,878 | −103,024 | **+42,854** |
  | rsp/rbp displacement | 69,083 | +46,034 | −83,393 | **−37,359** |
  | all scalar fields (imm, disp, rip, rel, baseless) | 146,238 | +174,785 | −228,567 | **−53,782** |
  | every repairable class | 235,478 | +346,224 | −357,023 | −10,799 |

  Two things decide it. Indexing each class over its own field domain
  (§9.15's shape) rather than over all instructions halves the index and
  flips rsp/rbp from −9,209 to −37,359. And the register class must be left
  alone: a modrm byte three bits from the prediction's is exactly what
  `CMSide.Pred` codes cheaply (xz saving 144,064, production 103,024), while
  the column that would replace it has no prediction under it. The cost is
  1.47 B/entry, not the 2.72 §9.15's four-byte deltas carried. Net for the
  scalar classes: **−53,782, 2.4% of the patch**, the largest item on this
  shelf; a port needs `delta/x86/lendec.go` to report the immediate and
  displacement field layout it already computes (the `x86asm` walk this
  probe used is 1.74 s of apply, the length decoder's 0.14 s). Unbuilt.
- **Extending map coverage**: ≤91,000, and the map extension is itself plan.
- **A prediction-conditioned correction coder**: the predicted byte is worth
  0.41 bits/byte beyond ordinary literal context (out[i−1] alone costs 916,518;
  pred[i] with it costs 853,026), which against xz's effective 4.99 bits/byte
  bounds the gain near 55,000 -- and it costs the ability to compare against the
  incumbent through the same terminal compressor.
- **Instruction-granular alignment**: dead. There is 4,256 bytes of
  misalignment in the whole image.

### 11.5 The dictionary question, asked properly

§9.17 concluded that the residual's long runs are not duplicated elsewhere in
the predicted image, having measured it with raw-byte LZ. That was the wrong
instrument: machine code does not self-match raw, because two instances of
identically-compiled code diverge at every call site. Canonicalising both sides
first -- zeroing every PC-relative field, the transform `x86.ContentHash`
already applies -- is the only form in which the answer means anything.

| | long runs | bytes | alone xz | marginal after 64 MiB |
|---|---:|---:|---:|---:|
| raw (§9.17) | 47,078 | 796,298 | 470,956 | 449,284 (4.6%) |
| canonicalised | 45,702 | 758,235 | 319,992 | **299,220 (6.5%)** |

6.5% against a probe calibrated to show 6.1x where the redundancy exists. The
conclusion survives its own corrected instrument: **this code is not elsewhere
in the image, in any form.**

The by-product is the more useful result. Canonicalising drops the long-run
content from 470,956 to 319,992, and that **150,964 is displacement bytes
inside newly-emitted instructions**. §9.15's field layer cannot reach them: it
names fields by walking the *prediction*, and where the prediction holds a
different instruction there is no field to name. So they are shipped as literal
bytes today.

They are call and jump targets -- image-spanning values -- and §9.17's own rule
says image-spanning values want an index into the enumerable address domain
while local values want a byte basis. The 3.2 million-entry domain is already
built and already indexed by two other layers. Splitting the long-run bucket
into canonical content plus a domain-indexed displacement column is therefore
the one direction this document ends with that follows from a rule it
established rather than from a hunch, and the first estimate is **50,000 to
90,000**. *(Measured in §14: it is worth −20,248, and the premise that these
fields are image-spanning is wrong for 79.4% of them.)*

## 12. Data structures, swept

A separate question from §11's: not "what should the codec predict" but "is any
part of the payload written in the wrong *kind* of structure". Packed
bitfields, roaring bitmaps, succinct sparse sets, reduced-precision numbers.

Two constraints decide most of it before any measurement. **The patch must
reconstruct byte-exactly**, so every lossy technique -- reduced float
precision, quantisation proper -- is inapplicable by construction, and there
are no floats in the payload in any case: every column is integers or literal
bytes. And **every column already passes through `xz -9e`**, so a structure
does not have to be good, it has to beat a strong entropy coder with a match
model.

### 12.1 Succinct structures target a bound this payload is already under

The sparse-set columns, priced against `log2 C(universe, n)` -- the cost of an
*unstructured* set of that cardinality, which is what Elias-Fano, roaring and
bit-packed index arrays are designed to approach:

| column | entries | universe | iid bound | xz | |
|---|---:|---:|---:|---:|---:|
| `.text` correction gaps | 362,032 | 225,655,845 | 485,364 | **295,492** | −39.1% |
| field-index | 54,579 | 8,666,697 | 59,690 | **36,584** | −38.7% |
| remap-index | 66,637 | 2,975,069 | 57,532 | **30,624** | −46.8% |
| per-function choices | 139,078 | 925,344 | 70,625 | **59,152** | −16.2% |

Every one is *below* the bound those structures aim at, by 16 to 47%. Concretely:
Elias-Fano on the gaps costs `n(2 + ceil(log2(u/n)))` bits = **543,048**, and a
roaring bitmap of the choices picks bitmap containers at 15% density for
**~115,500**. Both roughly double what xz spends. They lose for the same
reason: they are indexed on cardinality, and this payload's structure is
*clustering* -- the wrong runs all live inside 31,692 dirty functions.

The structure that should exploit clustering is a two-level index: name the
dirty function, then the offsets within it. Priced the same way it costs
**394,643** (24,900 for the functions, 369,743 for the offsets) -- better than
the flat unstructured bound, still 33% worse than xz. Restricting to dirty
functions makes each per-function subset denser, and a dense subset costs more
per element, so the hierarchy is paid for twice. xz captures the clustering
without an explicit index at all.
### 12.2 Packed bitfields: the win is real, and mostly unreachable

The one idea in this family with a track record here is §9.14's byte bucketing,
which won 70,920 by giving xz homogeneous streams instead of one mixed one. Its
analogue for the plan's ten numeric columns -- all LEB128 varint streams --
is to partition each by value width. Standalone, every column improves:

| column | values | as-is | width-bucketed | fixed-width packed |
|---|---:|---:|---:|---:|
| eq src-residual | 133,768 | 226,788 | **203,476** | 205,340 |
| eq copy-len | 158,544 | 186,992 | **169,572** | 172,824 |
| `.text` correction gaps | 362,032 | 295,492 | **274,192** | 277,084 |
| field remap-shift | 66,637 | 112,492 | **100,008** | 100,960 |
| field field-delta | 54,579 | 112,028 | **102,356** | 102,732 |
| eq src-skip | 24,776 | 57,836 | 54,688 | 55,028 |
| field field-index | 54,579 | 36,584 | 33,908 | 34,288 |
| field remap-index | 66,637 | 30,624 | 27,952 | 28,480 |
| eq dst-skip | 158,544 | 68,204 | 67,824 | 67,992 |
| `.text` correction lens | 362,032 | 88,700 | 87,848 | 87,856 |
| **total** | | **1,215,740** | **1,121,824** | 1,132,584 |

Two things fall out. **Fixed-width packing is worse than bucketing in all ten
cases** -- stripping LEB128's continuation bits gives back part of what the
partition won, which is the byte-alignment rule §9.14 found, holding again.
And the gain survives a shared compression, which §9.3 warned it might not:
concatenated into one stream and compressed once, 1,213,432 becomes
**1,117,636, −95,796**.

**It is not decodable.** Partitioning by width requires the decoder to know
each value's width before it can pick a bucket, and that width is exactly what
LEB128 states inline. Recovering it means shipping a width column over
1,442,128 entries, which costs several times what the arrangement saves.
§9.14's bucketing worked only because the bucket key -- the run length -- was
*already* in a column the decoder reads first.

The maximal self-describing rearrangement is transposition by byte position:
stream *k* holds the *k*-th byte of every value long enough to have one, and
the continuation bits in stream *k* say which values contribute to stream
*k+1*. That is decodable, and it is worth **11,636, 1.0%**:

| one stream, compressed once | xz |
|---|---:|
| as written | 1,213,432 |
| width-bucketed (not decodable) | 1,117,636 |
| **byte-transposed (decodable)** | **1,201,796** |

So the sweep returns one real item worth 11,636 -- 0.43% of the patch, free,
needing no extra data -- and one mirage worth 84,000 more that is the price of
information LEB128 already gives away. Note that this transposition *wins*
where §9.14's lost by 27,288: that one transposed fixed-width bytes, where
column *k* has no relationship to column *k+1*. On varints the columns are
tied by the continuation bit, which is what makes the arrangement both
self-describing and compressible.

### 12.3 Verdict

| technique | verdict |
|---|---|
| reduced-precision floats, quantisation | inapplicable: byte-exact output, and no floats in the payload |
| roaring bitmaps | ~115,500 against 59,152 -- optimises access, not size |
| Elias-Fano, succinct sparse sets | 543,048 against 295,492 -- targets a bound the payload is 39% under |
| Golomb/Rice gap coding | same failure: parameterised on density, blind to clustering |
| two-level (function, offset) index | 394,643 against 295,492 |
| sort-then-permute | log2(54,579!) ≈ 100 KB of permutation to save less |
| struct-of-arrays column splitting | already the architecture; §9.14 extended it for −70,920 |
| fixed-width packing of varints | worse than bucketing in all ten columns |
| width-bucketed varints | −95,796, and not decodable |
| **byte-transposed varints** | **−11,636, decodable, the sweep's only survivor** |


## 13. The composition, pressure-tested

§10 through §12 asked whether the *payload* could be smaller. This section
asks a different question: whether the seven layers are assembled correctly.
Three suspicions, all of them plausible from reading the decoder, and all
three measured. Two are refuted outright, and the third is real but worth
about half a percent.

### 13.1 Does an earlier layer blind a later one?

The decode order is: equivalence copies, relocation table, `.eh_frame`,
`.rodata`, retarget, per-function choice, field fix. The choice layer is the
suspicious one. It overwrites a chosen function with `predict()`'s output, and
`predict()` retargets through `newAddressLookup`, which is built from the
structural map alone. That map describes `.text` and nothing else, so
`addressLookup.target` returns `Known: false` for every address outside it --
which reads like a blind spot, because the equivalence bytes it just replaced
had those same references resolved through `newImageOracle` and the section
geometry the relocation plan carries.

It is not a blind spot. Handing the structural prediction the wider oracle
makes the chosen functions **ten times worse**:

| oracle given to the chosen functions | wrong runs | wrong bytes |
|---|---:|---:|
| structural map only (as shipped) | 31,539 | 72,430 |
| whole-image oracle | 243,414 | 743,618 |
| structural first, sections for what leaves `.text` | 31,539 | 72,431 |

The reason is worth stating, because it is the same principle as §9.17 seen
from the other side. `newImageOracle` answers an in-`.text` target from the
equivalence projection first, and prefers it *because* byte-level evidence
beats a symbol map -- but a function is selected structurally precisely where
the byte matcher went astray. The projection is at its least trustworthy
exactly where this layer would consult it. The narrow oracle is not a smaller
oracle, it is the correct one for the bytes that reach it.

The third row settles what remains: give the structural lookup priority and
fall back to the section geometry only where it has nothing to say, and the
result moves by **one byte**, in the wrong direction. References that leave
`.text` from a chosen function are not an unexploited residual; there are
almost none, and the ones there are already survive with their old
displacement.

The second ordering suspicion fails the same way. The equivalence runs are
decoded and copied before the choice layer overwrites some of them, so runs
falling wholly inside a chosen function are dead weight the decoder copies and
discards. There are 1,853 of them covering 627,123 bytes -- and dropping them
from the plan changes the compressed equivalence stream by **exactly zero
bytes**. xz was already pricing them at nothing.

### 13.2 Cost-modelling the selection

`chooseStructuralFunctions` compares wrong-byte counts. The measured yardstick
is 1.061 patch bytes per wrong *run* plus 0.624 per wrong *byte*, so the rule
optimises one term and ignores the other; a prediction that scatters fewer
wrong bytes across more runs can and should lose. Separately, the bits are
decided in the `.text`-only pass, where the equivalence side is built with no
relocation plan, and then applied in a whole-image decode where that side is
stronger -- the selection is made against a weaker alternative than the one
that runs.

Both are real. Neither is worth much.

| selection rule | modelled correction | delta |
|---|---:|---:|
| as shipped | 1,475,536 | — |
| reselect on whole-image wrong bytes | 1,468,000 | −0.51% |
| reselect on run-aware cost | 1,467,375 | −0.55% |

So re-deciding the bits in the context they are actually used in is worth
−0.51%, and making the criterion run-aware on top of that adds a further
−0.04% -- around 500 bytes. The cost model, the part that looked like the
principled fix, is the part that does nothing. (These are measured before the
field-fix layer, which repairs some of the same bytes, so the shipped gain
would be smaller still.)

The other half of the question -- whether a sparser plan that left more to the
correction would pay -- is refuted more firmly than anything else here. Every
length of equivalence run earns several times its plan cost:

| run length | runs | plan bytes (est.) | correction saved (est.) |
|---:|---:|---:|---:|
| 9–16 | 11,219 | 21,967 | 110,115 |
| 17–32 | 28,584 | 55,969 | 436,791 |
| 33–64 | 29,792 | 58,320 | 865,037 |
| 65–256 | 44,685 | 92,757 | 3,588,637 |
| 257+ | 44,264 | 99,711 | 175,053,681 |

The cheapest bucket pays five to one, and there are no runs shorter than nine
bytes -- Zucchini's matcher already filtered them. Of 158,544 runs, exactly
five contribute no correct byte to the finished prediction. There is nothing
in the largest plan stream that is not carrying its weight, and §7.5's sparse
rung already showed what happens when the structural map is thinned wholesale:
4,558,796 against 2,678,488.

### 13.3 What the environment could still supply

Anything both sides can agree on without transmission is fair game, and the
list is shorter than it looks, because most of it is already spent:

| assumed knowledge | status |
|---|---|
| x86-64 instruction encodings | used throughout, via `x86asm` |
| ELF section and RELA semantics | used: §9.9's relocation projection |
| `.eh_frame` FDE structure | used: §9.10 |
| `.eh_frame_hdr` is a pure function of `.eh_frame` | already regenerated, not corrected (`regenerateEhFrameHdr`) |
| Itanium C++ ABI vtable/RTTI layout | vtables live in `.data.rel.ro`, already named by `.rela.dyn`; that section costs 18,492 |
| clang switch-table layout | used: §9.11 recovered it by signature, 409,800 → 180,540 |
| PLT stub shape, `.got.plt` initial contents | `.plt` + `.rela.plt` + `.got.plt` total 2,308 XZ bytes |
| a static code dictionary shipped with the codec | §11.5 measured the *best possible* version -- the prediction itself, canonicalised -- at 6.5% |

One genuinely new modelling idea survived the list, and it is not external at
all. The largest equivalence column is the source residual: 226,756 bytes
recording where a run's bytes came from, against where the function map
predicts they came from. Where two places in the old image hold the same
bytes, the matcher's choice between them carries no information the decoder
needs, and the encoder could ship a zero instead. Of 111,455 nonzero
residuals, **98** have byte-identical alternatives, 86 of them free of
references. The column moves from 226,756 to 226,460. The residual is not
recording an arbitrary choice; it is recording that the source genuinely
differs.

### 13.4 Verdict

| question | answer |
|---|---|
| does an earlier layer degrade a later one? | no -- both alternative oracles are flat or 10× worse, and the dead equivalence runs compress to nothing |
| would a run-aware cost model select better? | yes, by 0.04%; re-deciding in the right context adds 0.51% |
| would a sparser plan, leaving more to correction, pay? | no -- the cheapest run bucket earns 5× its plan cost |
| is there unexploited environment knowledge? | no -- every derivable structure named above is already derived |

## 14. The displacement column, measured

§11.5 ended by naming one direction and estimating it at 50,000 to 90,000
bytes. It has now been run. **It is worth −20,248**, and the reasoning that
produced the estimate was wrong in a way worth recording.

The transform: the columnar correction's last bucket holds the replacement
bytes for every run of five or more, which is where newly emitted instructions
land. Zero the PC-relative field of each instruction in that bucket, ship the
fields as their own columns, and let the decoder walk the repaired bytes and
fill them back in. Zeroing a displacement never changes an instruction's
length, so the decoder decodes the same stream the encoder did. `columnarXZ`
already prices every stream with its own xz call, so this is measured exactly
rather than estimated.

| cut | xz | vs shipped |
|---|---:|---:|
| bucket as shipped | 447,024 | — |
| pull out every field | **426,776** | **−20,248** |
| pull out non-local only | 429,824 | −17,200 |
| pull out function-start calls only | 435,316 | −11,708 |

**§11.5's premise was wrong.** It called these fields "call and jump targets --
image-spanning values", and on that basis applied §9.17's rule that
image-spanning values want an index into an enumerable domain. Of the 59,935
displacement sites in the bucket, **47,603 (79.4%) are jumps inside their own
function**. Only 7,875 (13.1%) are calls to a known new function start, and
4,457 (7.4%) point anywhere else. The rule was applied to a population that
mostly does not belong to it.

The rule is not what failed. Pulling out only the genuinely image-spanning
fields -- 20.6% of the sites -- captures −17,200 of the −20,248, at a column
cost of 14,152 for the function-start indices and 8,804 for the rest. The
47,603 local jumps are worth about 3,000 net: they cost 71,924 as a column and
save 81,248 out of the content stream. §9.17 says local values want a byte
basis, and leaving them in the content stream *is* that byte basis; the
measurement says the choice barely matters either way, which is the weakest
form of agreement but not a contradiction.

Two caveats on the figure. The probe covers the 756,074 bytes of the bucket
that lie inside mapped functions; the format ships 796,264, and the remaining
40,190 have no function context to walk from, so an implementation would leave
them raw. And an implementation has to touch the shipped correction format and
add a decode-side instruction walk, then re-prove byte-exactness end to end --
real work for 0.76% of the patch.

**What this closes.** §11.5 was the last direction this document had that
followed from an established rule rather than a hunch. It is now measured, at
a quarter of its estimate. Nothing else on the list is untested.

## 15. The operand question, asked by field

§11.3 classified wrong instructions by `(Op, Len)` and §11.5 canonicalised
only PC-relative fields, so neither could tell recompiled code from
*relabelled* code — register renaming, stack-frame shifts, struct-offset
changes — which the two winning principles would reach cheaply. The `operands`
probe (`bench/elfpredict/operands.go`, ~80 s on `-resume`) compares
operand-by-operand over `x86asm.Inst.Args`, and reproduces §11.3's buckets
byte for byte.

| class (whole `.text` residual) | instructions | wrong bytes | share |
|---|---:|---:|---:|
| operand shape or prefixes differ | 205,508 | 790,926 | 68.4% |
| registers only (H1) | 80,568 | 99,886 | 8.6% |
| immediate only | 48,149 | 88,294 | 7.6% |
| displacement only, rsp/rbp base (H2) | 69,593 | 74,833 | 6.5% |
| displacement only, no base (absolute) | 14,426 | 43,421 | 3.8% |
| branch target only | 25,088 | 27,530 | 2.4% |
| several fields differ | 7,067 | 17,630 | 1.5% |
| displacement only, other base (H3) | 5,543 | 6,209 | 0.5% |
| everything else | 3,928 | 8,322 | 0.7% |

- **The 66.3% "different op and length" bucket is 99.9% shape changes** —
  a register operand became a memory operand or vice versa, a prefix appeared.
  Register renaming across the REX boundary accounts for 97 instructions,
  281 bytes, in the whole image. §10's verdict stands: it is recompiled code.
- **H1** (register renaming): real, 4,816 functions; a per-function best-fit
  bijection explains 67.3% of sites (61.5% in functions with ≥5 sites), worth
  ≤41,000 before the 13,896 mapping entries and per-site addressing are paid.
- **H2** (frame shift): the cleanest. One delta per function explains 55.6%,
  the top three 84.7%; all sites length-preserving. ≤40,000 before costs.
- **H3** (struct-offset relabel, image-spanning): refuted. 0.5% of the
  residual, and the top 10 `(old,new)` pairs cover 7.7% of it.
- **Immediate-only** (88,294 wrong bytes) and **absolute-displacement-only**
  (43,421) are the two pockets no probe has looked inside.

Ceiling for H1+H2 together is ~80,000 (3%) before addressing costs, so a
structured operand column is a marginal item on the §10 shelf beside the
displacement column, not a new direction.

## 16. Three more threads, pulled

Run 2026-08-28 after §15, all report-only, all on `-resume` cached plans.

### 16.1 Positions in instruction units — negative

`.text`'s position column ships byte gaps. Re-based to gaps over the
prediction's own instruction stream (`instpos` probe), the gap column alone
falls 295,524 → 236,096 (−59,428). But only **19.36%** of wrong-run *starts*
sit on an instruction boundary — a run begins at the first differing byte
inside an otherwise-correct instruction, not at its opcode — so 291,933 runs
need an escape, costing 87,092 across flag and offset columns. Net **+27,664**;
the flagless variant (offset for every run) is +23,080. Not a contradiction of
§11.3's 99.4%: that counted instruction starts, this counts run starts.

### 16.2 Immediates and "absolute" displacements — one negative, one bug

`immprobe` opens the two classes §15 left unexamined.

**Immediate-only (48,149 sites, 88,294 wrong bytes, ~106,000 patch) closes.**
47.5% are constants under 256; `test` alone is 16,558 one-byte sites where an
index costs what the byte costs; only 3.8% look like addresses and the oracle
projects **0 of 1,833** of them (a constant that lands in a section range is
still a constant); permutation within a function explains 8.1%; half the
wrong bytes are magic numbers and hashes. Per-function delta explains 56.4%
of sites in functions with ≥5 sites. Best case ≲0.4% for a format change.

**"Displacement only, no base register" is a bug, not a residual.** 99.8% of
the class (14,392 sites, 43,385 wrong bytes) is RIP-relative addressing in
VEX/EVEX encodings — `vbroadcastss xmm, [rip+d]` and kin, every one a
`.rodata` constant-pool load — which `x86asm` reports as `PCRel=0,
Base=Reg(0)`. Every `inst.PCRel > 0` gate in `delta/x86/x86.go` (Relocate,
WalkReferences, ContentHash) therefore skips them: never retargeted at
prediction time, never nameable by the field-fix layer. The existing
whole-image oracle already projects **88.3%** of them correctly (structural
lookup alone 65.4%); the 11.5% residue has the shape the field-fix remap
handles (96.7% of predicted addresses want one correct value, top-10 shifts
cover 74.6%). Estimated **−37,000 (1.39%)** at zero plan cost — fix measured
in §17.

### 16.3 Register normal form dictionary — negative, §11.5 stands

`rnfdict` reruns §11.5 with stronger canonicalisation, against a control that
shows 6.3× where redundancy exists:

| canonicalisation | long runs | bytes | alone xz | marginal | gain |
|---|---:|---:|---:|---:|---:|
| raw | 45,588 | 756,074 | 447,016 | 426,280 | 4.6% |
| PC-relative zeroed (§11.5) | 45,701 | 758,201 | 320,004 | 299,212 | 6.5% |
| all disp+imm zeroed | 46,228 | 764,010 | 255,888 | 237,808 | 7.1% |
| register normal form (bank) | 48,906 | 796,686 | 281,348 | 262,776 | 6.6% |
| register normal form (full) | 48,759 | 799,696 | 286,592 | 267,940 | 6.5% |

Erasing every constant lifts the dictionary gain from 6.5% to 7.1%. Register
normal form is *worse*: renumbering by first use is context-dependent, so the
first divergent instruction re-ranks every register downstream on one side
only and manufactures new wrong runs. A canonicalisation that is not a local
function of the bytes cannot serve as a diff basis. The residual is not
inlined callee bodies under a different allocation either. By-product: the
all-fields row's standalone drop (320,004 → 255,888) bounds the immediate and
absolute-displacement mass inside the long-run bucket at 64,116 standalone;
§14 turned 150,964 of the same shape into −20,248 net, so expect well under
that.

## 17. The VEX/EVEX gate, fixed and measured

`delta/x86/x86.go` now routes every PC-relative gate through `pcrelField`,
which falls back to locating the modrm byte of a `c5`/`c4`/`62`-prefixed
instruction and treating `mod=00 rm=101` as RIP+disp32. Ten hand-checked
encodings pin it, including two with an imm8 after the disp32 (the target is
`pc + Len + disp`, not disp-end). Full ladder, clean memo:

| | before | after | delta |
|---|---:|---:|---:|
| whole image, corrected-fields | 2,678,488 | **2,634,264** | **−44,224 (−1.65%)** |
| vs Zucchini 5,263,732 | 49.11% | **50.05%** | |
| prediction | 99.377% | 99.392% | |
| `.text` wrong bytes | 1,255,810 | 1,211,283 | −44,527 |
| `.text` wrong runs outside fields | 341,778 | 327,400 | −14,378 |
| exact reference points | 259,469 | 261,104 | +1,635 |
| normalized-equal units | 892,354 | 893,739 | +1,385 |

The win landed above §16.2's 37,000 estimate because the fix reaches plan
construction too: `ContentHash`/`Equal` now canonicalise VEX loads, so
content matching and the reference-point table both improve, and the
reference-target domain gained 2,341 distinct `.rodata` addresses. Plan
1,244,060 → 1,245,112 (+1,052); correction 1,434,428 → 1,389,152.

**Trap, now closed in the harness:** `targetcache`'s memo key hashed only
inputs, not the decoder, and would have served the pre-fix domain unchanged.
The memo is keyed on the binary's identity from here on.

This was the first modelled-byte gain since §9.15; it came from a decoder
bug, not a new model. The residual is otherwise as §10–§16 describe.
