# A native equivalence matcher: how far short of Zucchini's does it fall?

Spike run 2026-08-29 against `bench/elfpredict` (presage). The harness has until
now taken its whole-image equivalence stream — the list of (old offset, new
offset, length) runs that the prediction copies — out of an external Zucchini
patch file supplied by `-equivalence-patch`. The question was what it costs to
drop that dependency and match natively in Go.

Answer, in one line: on the prometheus pair the native matcher does not fall
short at all. It ships **312,760–323,744 XZ bytes against Zucchini's 650,708**,
a 50–52 % reduction, and finds its runs in 3.9 s against Zucchini's 46.2 s. On
the synthetic pair it lands within 0.8 % of Zucchini (2,672 against 2,652). But
that outcome depends entirely on one parameter being retuned away from
Zucchini's setting; at Zucchini-like settings the native matcher is 43 % *worse*
(929,548). Section 5 says why.

Contents

1. What was built
2. The algorithm
3. Headline measurements
4. The parameter sweep
5. Assessment: where the difference actually comes from
6. Reproducing

---

## 1. What was built

| File | Lines | Contents |
| --- | ---: | --- |
| `bench/elfpredict/nativeeq.go` | 360 | the matcher, the plan wrapper, the flag checks |
| `bench/elfpredict/nativeeq_test.go` | 197 | 8 tests, 0.06 s |
| `bench/elfpredict/main.go` | +8, −2 | flag registration, one call site, one condition |

Flags:

- `-native-equivalences` — match in Go instead of reading `-equivalence-patch`.
  The two are alternatives and passing both is an error. `-reference` is
  unaffected; it is only a number. Everything downstream is unchanged:
  `-no-text-equivalences`, `-no-equivalences`, `encodeColumns`, the source
  rewrite against the function map, every rung.
- `-native-eq-min` (default 32) — the shortest run the matcher will emit, and
  its seed length up to 16.
- `-native-eq-drop` (default 4096) — how far a run's running score may fall
  below its peak before the run is cut.

`main.go`'s only substantive edit is that `runCombined` calls
`buildEquivalencePlan` instead of `parseExternalEquivalence`; that wrapper
dispatches to the matcher or the Zucchini parser and prints the run count and
coverage for both, which is how the counts below were obtained for the Zucchini
side too.

The plan the matcher produces satisfies what `decodeEquivalences` requires:
runs in destination order, non-overlapping in the destination (`dstSkip` is an
unsigned gap from the previous run's destination end), non-empty, and inside
both images. Sources may overlap and may move backwards; the source column is a
signed skip.

## 2. The algorithm

**An equivalence run is not a common substring.** This was the spike's first
and most expensive discovery. Zucchini's runs tolerate mismatching bytes inside
them. On the synthetic pair its 77 runs cover 59,258,528 of 59,259,479 bytes,
and its first three runs alone cover 14.6 MB at delta 0 — across a pair whose
byte-by-byte comparison at delta 0 differs in 38,047,102 places. A matcher that
emitted only exact runs was measured first, and on the synthetic pair it
produced 414,668 runs where Zucchini produced 77, costing 1,138,136 XZ bytes
against Zucchini's 2,652.

So runs are grown by the extension BLAST calls X-drop and Zucchini calls
`ExtendEquivalenceForward`: walk outwards from an anchor adding +2 per agreeing
byte and −3 per disagreeing one (Zucchini's own weights are +1.0 and −1.5),
remember the length at which the running score peaked, and stop once the score
falls `-native-eq-drop` below that peak. The run is cut at the peak, so a run's
agreeing bytes always outweigh its disagreeing ones — a run is never worse than
leaving those bytes uncovered, which matters because uncovered destination bytes
are predicted as zero (0xcc inside `.text`).

Anchors come from a chained hash table on fixed-length seeds rather than
Zucchini's suffix array:

- `head[1<<b]` int32, `b` chosen so `1<<b >= len(old)/4`, capped at 26;
  `next[len(old)]` int32, one entry per old offset, chains most-recent-first.
  Computed allocation, not a measurement: 993,550,240 B for the 181 MB
  prometheus old image, 304,146,208 B for the 59 MB synthetic one. A 32-bit
  suffix array over the same prometheus image would be 725,114,784 B for the
  array plus whatever SA-IS needs to build it.
- Seed length is `min(max(-native-eq-min, 8), 16)`; the hash mixes the seed's
  first and last eight bytes.

The scan of the new image is greedy and left to right. At each offset:

1. If a delta is already in force, the offset `prevSrcEnd + (pos - prevDstEnd)`
   is tried first. An exact prefix of 512 bytes there is taken immediately,
   without touching the hash chain — this is what keeps a block that moved
   wholesale linear.
2. Otherwise up to 32 chain candidates are scored by their exact prefix, capped
   at 4,096 bytes. Degenerate seeds — zero runs, padding, repeated relocation
   shapes — occur hundreds of thousands of times, and the cap is what stops them
   consuming the run.
3. The winner is extended fuzzily forwards without a limit and backwards as far
   as the previous run's destination end.
4. Runs abutting in both images at the same delta are merged.

Ties and near-ties go to the candidate closest to where the source column
expects the next run rather than to the longer match: a candidate must be 16
bytes longer to displace one that continues the delta already in force. The
column pays for the difference, not for the run.

## 3. Headline measurements

Both pairs, rung `modelled-rodata`, whole image. Every harness run below used a
private, empty stage memo, so all of them did full plan construction; wall and
RSS are for the whole harness process, `/usr/bin/time -v`. The Zucchini rows'
harness time excludes the cost of producing the `.zuc` file, which is given
separately from Zucchini's own log.

### Synthetic pair

`syn-v1-F1z` → `syn-v2c-F1z`, 59,259,336 → 59,259,479 B, reference 597,416.

| | Zucchini | native (defaults) |
| --- | ---: | ---: |
| total XZ | **2,652** | **2,672** (+0.8 %) |
| plan XZ | 1,468 | 1,224 |
| equivalence stream XZ | 492 | 232 |
| correction XZ | 1,184 | 1,448 |
| runs | 77 | 23 |
| bytes covered | 59,258,528 (99.998 %) | 59,258,706 (99.999 %) |
| prediction correct | 99.999 % | 99.998 % |
| matching time | 11.13 s | 1.09 s |
| matching peak memory | 725,376 KiB | — |
| harness wall | 13.33 s | 8.40 s |
| harness peak RSS | 562,832 kB | 894,848 kB |

Zucchini's matching time and peak working set are from `z-synf1z.log`
(`Zucchini.TotalTime`, `Zucchini.PeakWorkingSetSize`); its equivalence count and
coverage in that log agree exactly with what the harness decodes. The native
matcher's own peak memory was not measured separately — its index allocation is
the computed 304 MB in §2, and the harness RSS column includes both images and
every downstream buffer.

### Prometheus pair

`prom-3.13.1-Dz` → `prom-3.13.2-Dz`, 181,278,696 → 181,329,375 B, reference
5,622,564, `-no-text-equivalences`.

| | Zucchini | native (defaults) | native (`-native-eq-min 128`) |
| --- | ---: | ---: | ---: |
| total XZ | **650,708** | **323,744** (−50.2 %) | **312,760** (−51.9 %) |
| plan XZ | 186,524 | 60,824 | 48,148 |
| equivalence stream XZ | 150,420 | 24,760 | 12,136 |
| — destination column | 32,152 | 4,140 | 1,868 |
| — length column | 48,820 | 7,080 | 4,892 |
| — source-skip column | 69,504 | 13,760 | 5,576 |
| correction XZ | 464,184 | 262,920 | 264,612 |
| runs (all / after dropping `.text`) | 47,222 / 44,099 | 6,616 / 6,271 | 2,724 / 2,615 |
| bytes covered | 177,221,523 (97.73 %) | 179,705,994 (99.11 %) | 179,695,568 (99.10 %) |
| prediction correct | 99.659 % | 99.833 % | 99.828 % |
| matching time | 46.17 s | 3.85 s | 4.05 s |
| matching peak memory | 2,187,824 KiB | — | — |
| harness wall | 26.22 s | 27.29 s | 27.32 s |
| harness peak RSS | 2,000,672 kB | 2,056,888 kB | 1,998,272 kB |

Zucchini figures from `z-p2Dz.log`. The 650,708 and 2,652 baselines reproduce
the previously recorded values for these pairs exactly.

## 4. The parameter sweep

Total XZ bytes. The minimum-length column is `-native-eq-min`, the drop column
`-native-eq-drop`; 24 is Zucchini's own drop expressed in these weights.

Synthetic pair (Zucchini 2,652):

| min | drop | runs | eq stream XZ | plan XZ | correction XZ | total XZ |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 8 | 24 | 173 | 836 | 1,820 | 1,100 | 2,920 |
| 12 | 24 | 105 | 592 | 1,576 | 1,212 | 2,788 |
| 16 | 24 | 78 | 484 | 1,464 | 1,308 | 2,772 |
| 24 | 24 | 53 | 364 | 1,352 | 1,336 | 2,688 |
| 32 | 24 | 45 | 324 | 1,320 | 1,320 | 2,640 |
| 32 | 48 | 37 | 288 | 1,284 | 1,308 | **2,592** |
| 32 | 4096 | 23 | 232 | 1,224 | 1,448 | 2,672 |
| 64 | 4096 | 21 | 224 | 1,220 | 1,500 | 2,720 |
| 128 | 4096 | 19 | 220 | 1,212 | 1,552 | 2,764 |

Prometheus pair (Zucchini 650,708):

| min | drop | runs | eq stream XZ | plan XZ | correction XZ | total XZ |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 12 | 24 | 80,893 | 258,140 | 294,272 | 635,276 | 929,548 |
| 32 | 48 | 24,178 | 84,440 | 120,628 | 414,892 | 535,520 |
| 32 | 96 | 16,098 | 58,160 | 94,384 | 349,104 | 443,488 |
| 32 | 256 | 11,166 | 40,788 | 76,964 | 293,696 | 370,660 |
| 32 | 1024 | 8,223 | 30,160 | 66,256 | 265,824 | 332,080 |
| 12 | 4096 | 14,049 | 47,904 | 83,968 | 263,072 | 347,040 |
| 32 | 4096 | 6,616 | 24,760 | 60,824 | 262,920 | 323,744 |
| 64 | 4096 | 5,229 | 19,492 | 55,508 | 262,296 | 317,804 |
| 128 | 4096 | 2,724 | 12,136 | 48,148 | 264,612 | **312,760** |
| 512 | 4096 | 1,940 | 9,484 | 45,484 | 293,092 | 338,576 |
| 32 | 16384 | 5,954 | 22,720 | 58,732 | 266,400 | 325,132 |

The prometheus optimum is a shallow basin between drop 1024 and 16384 and
between min 64 and 128; the synthetic optimum is at drop 48 and min 32, where
the whole spread across the table is 328 bytes. Defaults were set to 32/4096,
which is 0.8 % above the synthetic best and 3.5 % above the prometheus best.

## 5. Assessment: where the difference actually comes from

**The gain is not better matching. It is a different cut-off, and it is
specific to this codec.** At Zucchini-like settings — minimum 12, drop 24 — the
native matcher is 43 % worse than Zucchini on prometheus (929,548 against
650,708) and 5 % worse on the synthetic pair. Three things separate the two at
those settings, and the source-skip column measures all of them at once:
154,160 bytes native against Zucchini's 69,504.

- **Anchor quality.** Zucchini's suffix array answers "the longest old
  substring starting here" exactly. A hash chain capped at 32 candidates
  answers "the best of up to 32 old offsets sharing these 12 bytes", and its
  chains are ordered by old offset, not by usefulness. Where the right
  candidate is the 33rd, the scan takes a worse one, and a worse one usually
  means a different delta.
- **Selection order.** Zucchini scores candidate equivalences globally and
  takes them best-first with a pruning pass. The scan here is left-to-right
  greedy, so an early mediocre run can occupy destination bytes a later, better
  run would have wanted.
- **Reference-aware matching.** Zucchini compares references by target label
  rather than by the bytes of the operand, so two calls to the same moved
  function count as agreeing even though their rel32 displacements differ. That
  advantage is worth nothing in the prometheus measurement, which runs under
  `-no-text-equivalences` and therefore discards every run touching `.text`,
  and it is not what the native matcher is losing on.

What flips the result is the drop parameter, and the reason it can be flipped
is structural. Zucchini's runs are consumed by a patch format whose per-byte
correction is expensive, so cutting a run at the first sustained disagreement is
right for it. Presage's correction is an xz'd LZ stream over the whole image;
absorbing a disagreeing stretch inside a run costs it almost nothing, while an
extra run costs three varints in a plan that is only 60 KB. The prometheus
sweep is that trade-off drawn out: going from drop 24 to drop 4096 cuts the
equivalence stream by 90 % (258,140 → 24,760) *and* the correction by 59 %
(635,276 → 262,920), because longer runs also raise coverage from 96.9 % to
99.1 % and every uncovered byte is predicted as zero.

That is a lever Zucchini's patch file cannot pull, whatever Zucchini's matcher
is worth: the file is produced by Zucchini's binary at Zucchini's setting. The
honest statement of the result is therefore not "the native matcher beats
Zucchini's" but "a matcher inside the harness can be tuned to the harness's own
correction layer, and that is worth more than the matching quality given up".

Two things would close the remaining matching-quality gap if it ever mattered:

- Replacing the capped hash chain with a real longest-match structure. The
  cheapest version is not a suffix array but a second, longer seed table
  consulted first; the full version is 4 bytes per old byte of suffix array on
  top of the 4 already spent, and SA-IS construction time that would dwarf the
  3.9 s the whole matcher currently takes.
- A pruning pass over candidate runs before selection, so that a long run is
  not blocked by a short one that the left-to-right scan happened to reach
  first.

Neither is worth doing on this evidence. The measured cost of dropping the
Zucchini dependency is negative on both pairs.

## 6. Reproducing

Inputs are in `/home/will/.cache/presage-pairs`. Build with
`go build -o ep2 ./bench/elfpredict`, then:

```
ep2 -old syn-v1-F1z -new syn-v2c-F1z -old-debug syn-v1-F1z -new-debug syn-v2c-F1z \
    -native-equivalences -reference 597416 -rungs modelled-rodata -out <dir>

ep2 -old prom-3.13.1-Dz -new prom-3.13.2-Dz -old-debug prom-3.13.1-Dz -new-debug prom-3.13.2-Dz \
    -native-equivalences -no-text-equivalences -reference 5622564 -rungs modelled-rodata -out <dir>
```

Swap `-native-equivalences` for `-equivalence-patch z-synf1z.zuc` or
`-equivalence-patch z-p2Dz.zuc` for the Zucchini rows. The numbers to read are
the `whole image modelled-rodata ... = N XZ bytes` line, the
`plan streams (standalone xz)` line, the `equivalence columns (standalone xz)`
line, and the `equivalences: N runs` line.
