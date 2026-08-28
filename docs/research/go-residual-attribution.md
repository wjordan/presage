# Where the Go codec's remaining bytes go

Measured 2026-08-28 with `bench/goattr` on four pairs: the prometheus patch
release 3.13.1→3.13.2 (93.7 MB, patch 93,965 B), the minor release
3.13.2→3.14.0 (93.8 → 97.1 MB, patch 1,467,993 B), and the two synthetic
`bench/testsrv` pairs v1→v2c (1,264 B) and v1→v4 (1,785 B). All four are Go
1.27 linux/amd64.

The codec is `delta/` at the revision under test; `docs/DESIGN.md` §3.2–3.4
describes it. `bench/goattr` is report-only: it reuses `delta.Predict`,
`delta.EncodeCorrection` and the shipped compressor, so every price below is
a price the patch really pays. No patch byte changes.

**Headline.** The residual is not an operand-modelling problem, which is what
the Chrome ELF spike found on its corpus. It is an *alignment* problem: on the
minor pair 632,015 of the 1,081,452 `.text` bytes outside a relocated field
(58.4%) sit where the prediction is not even on an instruction boundary,
because the codec copies a matched old body positionally from the function
entry and one early insertion throws the whole tail out of phase. Six further
findings, three of them closed:

| finding | number | status |
|---|---:|---|
| `.text` residual that is mid-instruction (minor) | 632,015 B, 58.4% | probe A: net 70,013–86,463 B |
| wrong relocated fields that a better address map would fix (minor) | 11,661 of 79,754 (14.6%) | closed, worth 19,283 B |
| `.gopclntab` residual outside the `_func` records | 0 of 260,298 B | closed |
| `.gopclntab` `_func` offset fields with a systematic delta (minor) | ≈132,000 of 260,298 B | closed in part: the synthesised record, −3,161 B |
| `.go.type` residual in a field the walker rewrites | minor 27,592 of 766,010 B (3.6%); patch 8,914 of 45,170 (19.7%) | closed on a minor release only |
| `.go.type` new derived descriptors (minor) | 162,696 B, 52,991 marginal | probe C: net 35,803 B |
| unmatched-new code already present in the old image (minor) | 1.45× against a 42 MB dictionary, control 572× | closed |

## 1. Method

Every class is priced by **marginal cost**: the compressed correction the
patch loses when that class's wrong bytes are reverted to the prediction. A
class's standalone size overstates it — the spike saw 12× on Chrome
(`chrome-elf-whole-image.md` §9.3) — because one concatenated stream shares
context across classes.

A marginal can be *negative*. Reverting part of a correction region does not
remove the region; it hands the region's local matcher a short match to
encode, which moves bytes out of the well-compressing literal stream into the
varint control stream. Wherever a class does not own whole regions (ladders 3b
and 4b below) the negative numbers are that artefact, and the honest price for
such a class is the by-run table 3c, not the by-byte one.

The `fit` column is this codec's own yardstick, regressed on the section rows
of ladder 1 and then applied to every later class:

| pair | fitted marginal | residual RMS over rows |
|---|---|---:|
| minor 3.13.2→3.14.0 | 0.606 per run + 0.244 per wrong byte | 6,279 |
| patch 3.13.1→3.13.2 | 1.320 per run + 0.392 per wrong byte | 446 |
| synthetic v1→v2c | 2.052 per run + 0.609 per wrong byte | 10 |
| synthetic v1→v4 | 2.054 per run + 0.738 per wrong byte | 14 |
| Chrome ELF spike, `.text` | 1.061 per run + 0.624 per wrong byte | — |

The per-byte term falls as the pair grows, because a larger residual gives the
compressor more context; the per-run term rises as it shrinks, because a small
patch is closer to its floor. Against the spike's yardstick the measured
totals come in at ratio 0.41 (minor) and 0.74 (patch): a Go correction stream
is cheaper per wrong byte than Chrome's.

## 2. Ladder 1 — by section

Minor release 3.13.2→3.14.0. 3,667,914 wrong bytes in 149,056 runs, 3.7755%
of the file; correction 3,643,410 raw, 1,015,265 compressed.

| section | region B | wrong B | runs | density | marginal | raw |
|---|---:|---:|---:|---:|---:|---:|
| `.text` | 43114129 | 2447585 | 36719 | 5.68% | 619090 | 1886384 |
| `.go.type` | 12896568 | 766010 | 50147 | 5.94% | 216886 | 1060913 |
| `.gopclntab` | 36335623 | 260298 | 48282 | 0.72% | 89578 | 432773 |
| `.rodata` | 3783865 | 150007 | 10318 | 3.96% | 62427 | 210618 |
| `.noptrdata` | 608289 | 12377 | 795 | 2.03% | 8559 | 19760 |
| `.go.func` | 27696 | 8747 | 848 | 31.58% | 4781 | 13785 |
| `.data` | 343858 | 7208 | 1856 | 2.10% | 3029 | 13175 |
| `.go.buildinfo` | 23200 | 15391 | 41 | 66.34% | 971 | 5378 |
| (headers, padding, tail) | 0 | 195 | 35 | 0.00% | -172 | 439 |
| `.go.module` | 568 | 65 | 14 | 11.44% | -636 | 131 |
| `.go.fipsinfo` | 120 | 31 | 1 | 25.83% | -1297 | 34 |
| **TOTAL** | 97133916 | 3667914 | 149056 | | **1003216** | 3643390 |

Patch release 3.13.1→3.13.2. 119,614 wrong bytes in 11,955 runs, 0.1276%;
correction 117,079 raw, 64,659 compressed.

| section | region B | wrong B | runs | density | marginal | raw |
|---|---:|---:|---:|---:|---:|---:|
| `.text` | 42116561 | 61941 | 2889 | 0.15% | 28393 | 43416 |
| `.go.type` | 11770480 | 45170 | 5092 | 0.38% | 23823 | 50870 |
| `.gopclntab` | 35205370 | 4549 | 1530 | 0.01% | 4524 | 8877 |
| `.rodata` | 3678233 | 3782 | 700 | 0.10% | 3353 | 5696 |
| `.go.func` | 27208 | 2568 | 866 | 9.44% | 2654 | 4643 |
| `.data` | 337650 | 744 | 573 | 0.22% | 961 | 1935 |
| `.noptrdata` | 593313 | 468 | 208 | 0.08% | 480 | 1008 |
| (headers, padding, tail) | 0 | 109 | 65 | 0.00% | 280 | 261 |
| `.go.module` | 568 | 36 | 21 | 6.34% | 190 | 79 |
| `.go.fipsinfo` | 120 | 32 | 1 | 26.67% | 109 | 35 |
| `.go.buildinfo` | 22800 | 215 | 10 | 0.94% | 42 | 244 |
| **TOTAL** | 93752303 | 119614 | 11955 | | **64809** | 117064 |

The two synthetic pairs are 797 and 1,028 wrong bytes over 30 MB. `.text`
holds 658 of 797 (v1→v2c, marginal 422 of 659) and 827 of 1,028 (v1→v4,
marginal 738 of 1,053); in both, one resized function holds 650 resp. 765 of
those bytes. Everything the real pairs stress at scale, the synthetics show in
miniature and in a single function.

`.go.buildinfo` is worth one line: 15,391 wrong bytes at 66% density for a
marginal of 971. It is the build id and module list, incompressible and
unpredictable, and it is already almost free.

## 3. Ladder 2 — `.text` by cause

Minor:

| class | region B | wrong B | runs | density | marginal | raw | fit |
|---|---:|---:|---:|---:|---:|---:|---:|
| unmatched-new function (6231 funcs) | 1254656 | 1136129 | 6012 | 90.55% | 271317 | 1139614 | 281016 |
| name-matched, same length (108826 funcs) | 40342113 | 158730 | 20249 | 0.39% | 69378 | 128116 | 51022 |
| name-matched, resized (721 funcs) | 1512992 | 1152688 | 10451 | 76.19% | 271298 | 614044 | 287748 |
| content-matched (renamed) (46 funcs) | 4352 | 38 | 7 | 0.87% | -117 | 61 | 14 |
| **TOTAL** | 43114129 | 2447585 | 36719 | | **611876** | 1881835 | 619800 |

Patch:

| class | region B | wrong B | runs | density | marginal | raw | fit |
|---|---:|---:|---:|---:|---:|---:|---:|
| unmatched-new function (52 funcs) | 13568 | 12573 | 48 | 92.67% | 6582 | 12439 | 4990 |
| name-matched, same length (110064 funcs) | 42018337 | 8852 | 2104 | 0.02% | 8263 | 10650 | 6247 |
| name-matched, resized (26 funcs) | 84480 | 40516 | 737 | 47.96% | 13325 | 20239 | 16849 |
| content-matched (renamed) (5 funcs) | 160 | 0 | 0 | 0.00% | 0 | 0 | 0 |
| **TOTAL** | 42116561 | 61941 | 2889 | | **28170** | 43328 | 28086 |

721 resized functions — 0.62% of the function count, 3.5% of `.text` — hold
47% of the `.text` residual and cost as much as all 6,231 genuinely new
functions. That ratio is what probe A is about. Their density is 76.19%: three
quarters of a resized body is mispredicted, which is not what a body that was
edited in one place looks like.

## 4. Ladder 3 — inside a relocated field

Minor: 2,087,336 relocated fields in matched functions, 79,754 of which hold a
wrong byte (3.8%). Patch: 2,102,776 fields, 4,426 wrong (0.2%).

| minor, 3a | wrong B | runs | marginal | fit |
|---|---:|---:|---:|---:|
| inside a relocated field | 230004 | 14630 | -149170 | 65018 |
| elsewhere in `.text` | 2217581 | 22089 | 421526 | 554782 |

| minor, 3b — could the address maps have got it right? | wrong B | marginal | fit |
|---|---:|---:|---:|
| map error: a per-symbol consensus shift fixes it (11661 fields) | 23924 | 19283 | 5841 |
| genuinely different target (62867 fields) | 193325 | -182262 | 47198 |
| single-site symbol, undecidable (5226 fields) | 12755 | 688 | 3114 |
| field the old body has no counterpart for | 0 | 0 | 0 |

| patch, 3b | wrong B | marginal | fit |
|---|---:|---:|---:|
| map error: a per-symbol consensus shift fixes it (1279 fields) | 2283 | 4012 | 895 |
| genuinely different target (2176 fields) | 6518 | -7260 | 2554 |
| single-site symbol, undecidable (971 fields) | 1615 | 1539 | 633 |

| minor, 3c — by run, the only clean price for a field-fix layer | wrong B | runs | marginal | fit |
|---|---:|---:|---:|---:|
| runs whose every wrong byte is in a field (14417) | 25471 | 14417 | 43481 | 14955 |
| mixed runs (1465), the in-field bytes only | 204533 | 0 | -193774 | 49934 |
| mixed runs (1465), all their wrong bytes | 1298139 | 1465 | 286414 | 317813 |
| runs with no field byte at all (20837) | 1123975 | 20837 | 279301 | 287032 |

| patch, 3c | wrong B | runs | marginal | fit |
|---|---:|---:|---:|---:|
| runs whose every wrong byte is in a field (2080) | 3152 | 2080 | 7086 | 3981 |
| mixed runs (59), the in-field bytes only | 7264 | 0 | -8179 | 2846 |
| mixed runs (59), all their wrong bytes | 45935 | 59 | 13777 | 19995 |
| runs with no field byte at all (750) | 12854 | 750 | 7507 | 6027 |

79% of the wrong fields on the minor pair (62,867 of 79,754) point at a target
that genuinely moved relative to every other reference to the same symbol, so
no map, vote or content hash recovers them. The 14.6% a consensus shift would
fix are worth 19,283 compressed bytes on a 1,467,993-byte patch. The address
domain is exhausted.

3c is the reason to believe that. A field-fix layer can only claim the runs it
owns outright — 43,481 on the minor pair, 7,086 on the patch pair. The 204,533
in-field bytes inside mixed runs price at −193,774 because removing them from
a longer wrong region costs more than it saves.

## 5. Ladder 4 — by instruction and operand class

Wrong bytes outside relocated fields, classified by comparing the prediction's
instruction with the target's at the same offset.

| relation to the target instruction | minor insts | minor wrong B | share | patch insts | patch wrong B | share |
|---|---:|---:|---:|---:|---:|---:|
| same op, same length (operand only) | 23834 | 38539 | 3.6% | 1010 | 1608 | 4.1% |
| same op, different length | 27829 | 128488 | 11.9% | 1052 | 4988 | 12.8% |
| different op, same length | 23607 | 59535 | 5.5% | 875 | 2232 | 5.7% |
| different op and length | 193666 | 819275 | 75.8% | 6723 | 28554 | 73.3% |
| prediction undecodable there | 9653 | 35615 | 3.3% | 399 | 1570 | 4.0% |
| — of which the prediction is on an insn boundary | 46517 | 187260 | 17.3% | 1644 | 6706 | 17.2% |
| — of which the prediction is mid-instruction | 147149 | 632015 | **58.4%** | 5079 | 21848 | **56.1%** |

| minor, 4b — by operand class | wrong B | runs | marginal | fit |
|---|---:|---:|---:|---:|
| operand shape or prefixes differ [221114 insns] | 903272 | 511 | 36860 | 220833 |
| several fields differ [27953 insns] | 114782 | 334 | -75645 | 28225 |
| prediction undecodable there [9653 insns] | 35615 | 0 | -37133 | 8695 |
| displacement only, rsp/rbp base (H2) [11726 insns] | 13244 | 11105 | 10254 | 9963 |
| operand width only (REX.W etc) [1079 insns] | 4913 | 2 | -640 | 1201 |
| displacement only, other reg base (H3) [4140 insns] | 4779 | 2966 | 3004 | 2964 |
| registers only (H1) [1425 insns] | 2484 | 331 | -4045 | 807 |
| immediate only [1093 insns] | 1255 | 991 | 4 | 907 |
| branch target only [165 insns] | 501 | 52 | -2038 | 154 |
| identical disassembly, different bytes [103 insns] | 431 | 0 | 368 | 105 |
| op only, operands identical [136 insns] | 170 | 0 | -1088 | 42 |
| displacement only, rip-relative [2 insns] | 6 | 0 | -256 | 1 |

| patch, 4b — by operand class | wrong B | runs | marginal | fit |
|---|---:|---:|---:|---:|
| operand shape or prefixes differ [7758 insns] | 31675 | 25 | 85 | 12445 |
| several fields differ [1089 insns] | 4641 | 12 | -6085 | 1834 |
| prediction undecodable there [399 insns] | 1570 | 0 | -2947 | 615 |
| displacement only, rsp/rbp base (H2) [676 insns] | 731 | 660 | 965 | 1158 |
| operand width only (REX.W etc) [42 insns] | 202 | 0 | -112 | 79 |
| registers only (H1) [29 insns] | 43 | 7 | 135 | -75 |
| immediate only [29 insns] | 36 | 29 | 89 | 85 |
| displacement only, other reg base (H3) [28 insns] | 31 | 25 | -8 | 84 |
| branch target only [7 insns] | 21 | 4 | 30 | 0 |
| op only, operands identical [2 insns] | 2 | 0 | 71 | -6 |

The single-field classes the Chrome spike modelled — a register renamed, a
frame displacement moved, an immediate changed — hold 22,269 wrong bytes of
1,081,452 on the minor pair (2.1%) and are worth 6,923 marginal between them.
Everything else is "operand shape differs" or "several fields differ", and
75.8% of it is an instruction of a different op *and* a different length,
which is the signature of comparing two unrelated byte streams rather than two
versions of one instruction. That is the inversion: Chrome's residual was
mostly on boundaries and mostly one field wide; this one is mostly not on a
boundary at all.

## 6. Ladder 5 — `.go.type` by descriptor field

| minor | wrong B | runs | marginal | fit |
|---|---:|---:|---:|---:|
| not a field the walker rewrote, NEW descriptor | 683385 | 40838 | 165932 | 191587 |
| not a field the walker rewrote, changed descriptor | 55033 | 5365 | 28707 | 16687 |
| method-table entry, changed descriptor | 20209 | 2305 | 8529 | 6331 |
| nameOff, changed descriptor | 1591 | 446 | 2068 | 659 |
| method-table entry, NEW descriptor | 3837 | 510 | 1908 | 1246 |
| typeOff, NEW descriptor | 21 | 0 | 264 | 5 |
| typeOff, changed descriptor | 546 | 5 | 256 | 136 |
| nameOff, NEW descriptor | 104 | 29 | 202 | 43 |
| ptrToThis, NEW descriptor | 60 | 18 | -569 | 26 |
| ptrToThis, changed descriptor | 1224 | 631 | -821 | 681 |
| **TOTAL** | 766010 | 50147 | **206476** | 217400 |

| patch | wrong B | runs | marginal | fit |
|---|---:|---:|---:|---:|
| not a field the walker rewrote, changed descriptor | 30460 | 840 | 8678 | 13045 |
| not a field the walker rewrote, NEW descriptor | 5796 | 1819 | 6087 | 4673 |
| method-table entry, changed descriptor | 7170 | 1676 | 5555 | 10501 |
| ptrToThis, changed descriptor | 983 | 595 | 1894 | 2605 |
| method-table entry, NEW descriptor | 701 | 139 | 580 | 1014 |
| ptrToThis, NEW descriptor | 13 | 6 | 108 | 25 |
| nameOff, NEW descriptor | 3 | 1 | 90 | 5 |
| nameOff, changed descriptor | 39 | 12 | 44 | 69 |
| typeOff, changed descriptor | 5 | 4 | -45 | 15 |
| **TOTAL** | 45170 | 5092 | **22991** | 24423 |

Every field the walker rewrites — nameOff, typeOff, ptrToThis and the method
tables — holds 27,592 of the minor pair's 766,010 wrong bytes, 3.6%, worth
11,837 marginal. The walker is not the problem there: 683,385 bytes are in
descriptors the old image does not hold at all, which is what probe C is
about.

The patch pair does not say the same thing, and that is worth flagging rather
than filing away. Its walker-rewritten fields hold 8,914 of 45,170 wrong bytes
(19.7%) for 8,226 marginal, 7,871 of them method-table entries — 8.8% of that
93,965-byte patch. A field-fix layer is closed on a minor release and open on
a patch release. Nothing below probes it.

## 7. Ladder 6 — is the new code already in the old image?

Minor pair, 6,231 unmatched-new functions, 1,254,656 B, canonicalised (every
PC-relative field zeroed) on both sides:

| measurement | bytes |
|---|---:|
| the new code, `xz -9e -T1` alone | 123644 |
| the same, marginal after a 42,116,561 B dictionary of canonicalised old `.text` | 85264 |
| control: an equal-length slice of the dictionary itself, marginal | 216 |

1.45× against a dictionary that resolves a known-redundant control at 572×.
The new code is new. Renumbering is the same answer: 49 of the 6,231
unmatched-new functions share a closure/instantiation-normalised name with an
old one, 18 of those differ by under 10%, and those 18 hold 2,592 B. Both
avenues are closed.

## 8. Probe A — the sub-function alignment ceiling

Method. For every name-matched function, canonicalise the old body and the new
one (`bench/goattr/opclass.go`, every PC-relative field zeroed), hash 12-byte
windows at instruction boundaries in the old body, look each new boundary's
window up, keep the longest chain of anchors that is monotone in both bodies,
grow each anchor byte-wise, and merge neighbours that share an offset across
gaps of up to 16 mismatching bytes. The result is a segment list — (old
offset, new offset, length) — which is exactly what the candidate scheme would
transmit. Bodies average 2 KB; the alignment is O(n) hashing plus one
O(n log n) chain per function.

Because both sides are canonicalised, a segment's residual is what
re-relocation would *not* fix, and the relocated-field bytes that are wrong
today are charged to the scheme in full, unchanged.

| minor | funcs | new B | covered B | inserted B | residual B | segments | runs left |
|---|---:|---:|---:|---:|---:|---:|---:|
| name-matched, resized | 721 | 1512992 | 1018575 | 494417 | 12916 | 13709 | 39239 |
| name-matched, same length, with wrong bytes | 7921 | 8668160 | 8616525 | 51635 | 5196 | 10416 | 22279 |

| patch | funcs | new B | covered B | inserted B | residual B | segments | runs left |
|---|---:|---:|---:|---:|---:|---:|---:|
| name-matched, resized | 26 | 84480 | 70684 | 13796 | 837 | 434 | 1764 |
| name-matched, same length, with wrong bytes | 1151 | 2799744 | 2798162 | 1582 | 493 | 1260 | 2490 |

| what the current positional copy already gets right | resized | same length |
|---|---:|---:|
| minor | 438457 of 1512992 | 8540294 of 8668160 |
| patch | 46683 of 84480 | 2793667 of 2799744 |

| covered bytes, by whether the segment moved | at offset 0 | at a shifted offset |
|---|---:|---:|
| minor, resized | 244060 | 774515 |
| minor, same length | 8501811 | 114714 |
| patch, resized | 40076 | 30608 |
| patch, same length | 2792416 | 5746 |

| segments per function | minor resized | minor same-len | patch resized | patch same-len |
|---|---:|---:|---:|---:|
| 1 | 13 | 7534 | 1 | 1139 |
| 2 | 20 | 71 | 5 | 1 |
| 3–4 | 105 | 135 | 2 | 4 |
| 5–8 | 155 | 82 | 5 | 3 |
| 9–16 | 144 | 51 | 8 | 1 |
| 17+ | 284 | 48 | 5 | 3 |

Priced. `gross` is the bucket's measured marginal, re-measured in the same run;
`gross fit` is the yardstick's estimate of the same thing, so that the "before"
and "after" are computed the same way; `plan` is `xz -9e -T1` of the segment
list written as five contiguous varint columns (function-id gaps, segment
count, new-offset gaps, lengths, old-offset deltas), with `cz` the codec's own
compressor for comparison; `resid fit` is the yardstick applied to what the
scheme would leave wrong.

| minor | gross | gross fit | plan xz | plan cz | resid fit | net meas | net fit |
|---|---:|---:|---:|---:|---:|---:|---:|
| name-matched, resized | 271298 | 287748 | 23176 | 23869 | 178109 | **70013** | **86463** |
| name-matched, same length | 69378 | 51022 | 4748 | 4726 | 37174 | 27456 | **9100** |

| patch | gross | gross fit | plan xz | plan cz | resid fit | net meas | net fit |
|---|---:|---:|---:|---:|---:|---:|---:|
| name-matched, resized | 13325 | 16849 | 1252 | 1198 | 9986 | **2087** | **5611** |
| name-matched, same length | 8263 | 6246 | 440 | 373 | 5605 | 2218 | **201** |

| what the scheme is charged with, minor resized | bytes | runs |
|---|---:|---:|
| inserted (no old counterpart) | 494417 | |
| residual inside a segment | 12916 | |
| relocated-field bytes still wrong | 186826 | |
| total, deduplicated | 632149 | 39239 |
| what it replaces | 1152688 | 10451 |

| plan columns, xz | minor resized | minor same-len | patch resized | patch same-len |
|---|---:|---:|---:|---:|
| functions carrying a non-trivial map | 720 | 451 | 25 | 14 |
| function-id gaps | 632 | 488 | 92 | 76 |
| segment count | 552 | 296 | 84 | 72 |
| new-offset gaps | 7472 | 1344 | 456 | 156 |
| lengths | 8736 | 2024 | 552 | 248 |
| old-offset deltas | 6372 | 1044 | 396 | 136 |

Four things this settles.

**The ceiling is real but bounded.** Alignment lifts the correctly-predicted
bytes of the 721 resized bodies from 438,457 to 1,005,659 — a 2.3× improvement
— and the residual *inside* an aligned segment is 12,916 B, 1.3% of what the
segments cover. Operand-level change inside an aligned body is negligible,
confirming what `chrome-elf-whole-image.md` §15 found by a different route.

**The floor is the insertion.** 494,417 of the 1,512,992 bytes in resized
bodies (32.7%) have no old counterpart at any offset. No segment map reaches
them; they are the actual code change.

**Fragmentation is what the scheme pays with.** Wrong bytes fall from
1,152,688 to 632,149 but the correction runs rise from 10,451 to 39,239, and
at 0.606 per run that costs 23,779 of the 178,109 residual charge. 284 of the
721 functions need 17 or more segments.

**Do not extend it to same-length functions.** 8,501,811 of the 8,616,525
covered bytes there are at offset 0 — the body was edited in place, which is
what the codec already assumes. Only 114,714 B (1.3%), in 451 of 7,921
functions, are covered at a shifted offset. The fitted net is 9,100 on the
minor pair and 201 on the patch pair, against 86,463 and 5,611 for the resized
bucket.

Verdict: build it for the resized bucket only. 70,013–86,463 B on a
1,467,993-byte minor patch (4.8–5.9%) and 2,087–5,611 B on a 93,965-byte patch
release (2.2–6.0%). The two synthetic pairs are one resized function each, 650
and 765 wrong bytes, and would gain tens of bytes.

## 9. Probe B — `.gopclntab` by subtable

| minor | region B | wrong B | runs | density | marginal | raw | fit |
|---|---:|---:|---:|---:|---:|---:|---:|
| `_func` headers | 5096256 | 100537 | 32382 | 1.97% | 57170 | 196491 | 44167 |
| `_func` funcdata arrays | 3147072 | 108608 | 7483 | 3.45% | 17501 | 138268 | 31050 |
| `_func` pcdata arrays | 1795312 | 51153 | 8417 | 2.85% | 16398 | 94245 | 17589 |
| pcHeader | 72 | 0 | 0 | | 0 | 0 | 0 |
| funcnametab (stage 1a) | 9523912 | 0 | 0 | | 0 | 0 | 0 |
| cutab (stage 1b) | 359912 | 0 | 0 | | 0 | 0 | 0 |
| filetab (stage 1a) | 343984 | 0 | 0 | | 0 | 0 | 0 |
| pctab (stage 1b) | 7998136 | 0 | 0 | | 0 | 0 | 0 |
| functab index (entryOff, funcOff) | 926596 | 0 | 0 | | 0 | 0 | 0 |
| functab alignment padding | 89340 | 0 | 0 | | 0 | 0 | 0 |
| `go:func.*` (stage 1b) | 6844512 | 0 | 0 | | 0 | 0 | 0 |
| findfunctab (regenerated) | 210519 | 0 | 0 | | 0 | 0 | 0 |
| **TOTAL** | 36335623 | 260298 | 48282 | | **91069** | 429004 | 92806 |

| patch | region B | wrong B | runs | density | marginal | raw | fit |
|---|---:|---:|---:|---:|---:|---:|---:|
| `_func` headers | 4846468 | 2827 | 1288 | 0.06% | 3073 | 6104 | 2808 |
| `_func` funcdata arrays | 2990212 | 1056 | 119 | 0.04% | 645 | 1494 | 571 |
| `_func` pcdata arrays | 1706532 | 666 | 123 | 0.04% | 641 | 1168 | 423 |
| everything else (9 regions, 25,662,158 B) | | 0 | 0 | | 0 | 0 | 0 |
| **TOTAL** | 35205370 | 4549 | 1530 | | **4359** | 8766 | 3803 |

Every byte of the residual is inside a `_func` record. The regenerated functab
index, the regenerated findfunctab and all five variable-length blobs are
byte-exact on both pairs — the stage-1a/1b split of `docs/DESIGN.md` §3.4 and
the `findfunctab` regeneration are doing exactly what they claim.

By field, with the delta the codec would have had to add (`new − predicted`).
`values` is how many distinct deltas the field takes; `top 10` is the share of
changed fields the ten commonest deltas cover.

| minor, `_func` field | changed | wrong B | values | top 10 | most common new − pred |
|---|---:|---:|---:|---:|---|
| funcdata[] | 28581 | 108608 | 3137 | 72.8% | +6759688 (5508), +348721 (5296), +348709 (3233) |
| pcdata[] | 20306 | 51153 | 4020 | 55.6% | +53455 (1793), +275493 (1793), +65640 (1792) |
| pcfile | 8354 | 23512 | 2040 | 58.9% | +1529332 (1792), +2995521 (1280), +1529320 (512) |
| pcln | 8459 | 22837 | 6685 | 9.0% | −4 (188), +4 (129), −1001458 (96) |
| startLine | 11694 | 17632 | 5247 | 21.6% | +1 (714), +2 (456), +4 (216) |
| pcsp | 7057 | 16329 | 1320 | 68.2% | +65616 (1793), +20523 (1285), +472 (522) |
| cuOffset | 7435 | 12759 | 80 | 77.8% | +2 (1717), +1 (1175), +9605 (821) |
| args | 6567 | 6613 | 45 | 96.9% | +8 (3218), +32 (1403), +16 (774) |
| funcID | 468 | 468 | 2 | 100.0% | +23 (467), −23 (1) |
| deferreturn | 151 | 251 | 112 | 26.5% | −64 (6), +37 (6), −32 (6) |
| nameOff | 46 | 134 | 46 | 21.7% | +2275380 (1), +7471099 (1), +7471835 (1) |
| flag | 2 | 2 | 1 | 100.0% | +4 (2) |

| patch, `_func` field | changed | wrong B | values | top 10 | most common new − pred |
|---|---:|---:|---:|---:|---|
| funcdata[] | 327 | 1056 | 140 | 54.1% | +16 (47), +6690664 (36), +341449 (27) |
| cuOffset | 296 | 711 | 6 | 100.0% | −47485 (127), +154 (104), +3 (31) |
| pcln | 299 | 681 | 142 | 48.2% | −2624454 (40), −4 (28), +4 (16) |
| pcdata[] | 265 | 666 | 165 | 33.2% | −15736 (36), +22572 (12), +39460 (7) |
| pcfile | 264 | 617 | 137 | 35.2% | −4 (18), +4 (12), +22559 (12) |
| startLine | 491 | 509 | 54 | 77.6% | +6 (100), +11 (72), +1 (44) |
| pcsp | 97 | 208 | 58 | 49.5% | +22559 (12), +35561 (11), −8 (6) |
| args | 45 | 45 | 9 | 100.0% | +8 (22), +16 (7), +24 (6) |
| funcID | 30 | 30 | 2 | 100.0% | +23 (29), −23 (1) |
| nameOff | 5 | 15 | 5 | 100.0% | +1704389 (1), +1704457 (1), +1704528 (1) |
| deferreturn | 7 | 11 | 7 | 100.0% | −11 (1), +399 (1), +272 (1) |

Three readings.

**The five offset fields were the record the codec synthesises for a function
the release added, not a mapping failure.** `pcsp`, `pcfile`, `pcln`,
`pcdata[]` and `funcdata[]` hold 222,439 of the 260,298 wrong bytes (85%), and
four of them are dominated by a handful of large constant deltas. The repeated
counts 1,793 / 1,792 / 1,285 / 1,280 looked like `buildDataMap`
(`gfBlock = 16`, `gfTol = 4`) putting whole runs of functions through the
wrong shift. It was not. Dumping every wrong field together with the map hop
that produced it shows the large deltas belong, without exception, to
*unmatched-new* functions, whose `_func` record `delta/pcln.go` synthesised as
all zeroes with `^0` in every funcdata slot — so `new − predicted` was simply
the true field value. The +6,759,688 group is 5,508 records, 5,507 of them
unmatched-new and every one with `funcdata[6] = 0x672507`; +65,616 is 1,793
unmatched-new records with `pcsp = 0x10050`. The blocks are one library's
generated method wrappers (`github.com/felixge/httpsnoop.(*rwN).M`): the 6,231
unmatched-new functions cover only 1,402 distinct record tuples, and the
largest tuple repeats 1,792 times. The content maps are exonerated — over the
matched functions they misplace 2,202 `pcln`, 2,597 `pcdata[]` and 2,700
`funcdata[]` values, all by small local deltas.

The fix is to synthesise the record from the *modal* record of the matched
functions instead of from zeroes: what a release adds is nearly always a
compiler-generated wrapper, and a wrapper's record repeats. Both sides compute
the mode from the same re-targeted records, so it stays symmetric and costs no
patch bytes. It removes 82,225 of the 260,298 wrong `.gopclntab` bytes on the
minor pair — `funcdata[]` from 28,581 wrong fields to 12,044, `args` from
6,567 to 3,933 — and on its own is worth 3,161 bytes of the 1,467,993-byte
minor patch and 211 of the 93,965-byte patch release. The correction stream
was already squashing those repeated tuples nearly for free, which is why
82,225 wrong bytes were worth 0.2% of the patch and why the ≈45,000 estimated
above was an order too high: a top-10 share of *fields* applied to bytes
overstates what a compressor charges for the same tuple 1,792 times.

What is left in these fields is `pcsp`, `pcfile`, `pcln` and `pcdata[]` for
new functions. Those are per-function positions in the new pctab, which the
decoder does hold by the time the records are built — stage 1b has already
been applied — so recovering them means replaying the linker's first-use
emission order over the true pctab rather than taking a mode. That is the open
item.

**`pcln` and `startLine` are real change.** `pcln` takes 6,685 distinct deltas
with a top-10 share of 9.0%, `startLine` 5,247 with 21.6% and deltas of +1, +2,
+4 — source lines moved. 40,469 wrong bytes between them that no regenerator
recovers.

**`args` and `funcID` came from the same place.** `args` takes 45 values,
96.9% of them in the top ten and every one a multiple of 8, because the
synthesised record guessed 0; the modal record fixes 2,634 of them. `funcID`
was +23 for 467 of 468 changed records on the minor pair and 29 of 30 on the
patch pair — the wrapper id, where the synthesised record guessed 0. A mode
cannot carry that one, since the mode over all functions *is* 0, so the codec
learns the modal `funcID` per name key from the old binary: `nameKey` is the
receiver form plus the last dot-separated component with its trailing digits
stripped, or `fm` for a method value, which is what a compiler-generated
wrapper shares with the wrappers already in the old image. Changed records go
from 468 to 297, and it is the larger half of the 3,161-byte win — a correct
byte 40 ends a wrong run early, and the correction stream is charged per run.

## 10. Probe C — `.go.type` new descriptors by kind

Method. `delta.WalkDescriptors` walks a *single* binary's descriptors with
`delta/typedesc.go`'s own layout constants and reachability rule, which the
encoder's walk cannot do (it walks the old image and maps into the new, so it
never enumerates a descriptor the old image does not hold). A new descriptor
counts as NEW when fewer than half its bytes are covered by a descriptor the
encoder placed there.

| minor, kind | descs | bytes | wrong B | new | new B | new wrong | derivable B | new name |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| struct | 19027 | 3880304 | 207274 | 2154 | 512832 | 178934 | 0 | 1818 |
| ptr | 22148 | 2835712 | 159234 | 1576 | 228496 | 112848 | 46624 | 1274 |
| map | 1510 | 209680 | 22718 | 562 | 76432 | 22195 | 2720 | 542 |
| array | 2248 | 163744 | 14989 | 577 | 41544 | 14605 | 1872 | 555 |
| string | 1458 | 103424 | 15638 | 555 | 35616 | 15450 | 0 | 550 |
| slice | 5296 | 315552 | 12436 | 583 | 33576 | 11626 | 2728 | 551 |
| func | 13934 | 1049768 | 4697 | 131 | 10912 | 1420 | 9080 | 40 |
| interface | 1633 | 201952 | 3378 | 43 | 6200 | 2014 | 0 | 23 |
| chan | 153 | 9888 | 65 | 2 | 128 | 2 | 128 | 0 |
| 17 basic kinds | 665 | 59120 | 439 | 7 | 608 | 323 | 0 | 7 |
| **TOTAL** | 68072 | 8829144 | 440868 | 6190 | 946344 | 359417 | 63152 | 5360 |

| patch, kind | descs | bytes | wrong B | new | new B | new wrong | derivable B |
|---|---:|---:|---:|---:|---:|---:|---:|
| ptr | 20820 | 2649600 | 37495 | 206 | 35440 | 1313 | 35232 |
| struct | 17206 | 3455360 | 2653 | 36 | 10632 | 956 | 0 |
| interface | 1602 | 198176 | 157 | 2 | 224 | 4 | 0 |
| slice | 4717 | 283080 | 319 | 1 | 184 | 1 | 184 |
| string | 903 | 67808 | 71 | 2 | 160 | 26 | 0 |
| func | 13815 | 1039992 | 94 | 2 | 160 | 3 | 160 |
| map | 959 | 134744 | 20 | 1 | 152 | 2 | 152 |
| 19 other kinds | 2505 | 192264 | 586 | 0 | 0 | 0 | 0 |
| **TOTAL** | 62527 | 8021024 | 41395 | 250 | 46952 | 2305 | 35728 |

| new descriptors of a derived kind, by where their element types live | minor new | all in old | some new | patch new | all in old |
|---|---:|---:|---:|---:|---:|
| ptr | 1576 | 302 | 1274 | 206 | 204 |
| map | 562 | 20 | 542 | 1 | 1 |
| array | 577 | 26 | 551 | 0 | 0 |
| slice | 583 | 33 | 550 | 1 | 1 |
| func | 131 | 110 | 21 | 2 | 2 |
| chan | 2 | 2 | 0 | 0 | 0 |
| **TOTAL** | 3431 | **493** | 2938 | 210 | 208 |

A new derived descriptor against a same-kind, same-`Size_` old template
(3,429 of the 3,431 minor descriptors found one; 210 of 210 on the patch pair):

| field of `abi.Type` | minor differs | share | patch differs | share |
|---|---:|---:|---:|---:|
| `Size_` | 0 | 0.0% | 0 | 0.0% |
| `PtrBytes` | 555 | 16.2% | 0 | 0.0% |
| `Hash` | 3428 | 100.0% | 210 | 100.0% |
| `TFlag`/`Align`/`Kind` | 1375 | 40.1% | 159 | 75.7% |
| `Equal` fn | 2143 | 62.5% | 206 | 98.1% |
| `GCData` | 3429 | 100.0% | 210 | 100.0% |
| `Str` (nameOff) | 3429 | 100.0% | 210 | 100.0% |
| `PtrToThis` | 144 | 4.2% | 4 | 1.9% |
| element pointer | 3429 | 100.0% | 210 | 100.0% |

Priced:

| minor | wrong B | marginal | raw | fit |
|---|---:|---:|---:|---:|
| new descriptors of a derived kind | 162696 | 52991 | 258175 | 39720 |
| new descriptors of any other kind | 196721 | 46072 | 370164 | 48027 |

| patch | wrong B | marginal | raw | fit |
|---|---:|---:|---:|---:|
| new descriptors of a derived kind | 1319 | 962 | 1869 | 517 |
| new descriptors of any other kind | 986 | 874 | 1748 | 386 |

| what a regenerator would ship instead, xz | minor | patch |
|---|---:|---:|
| offsets, kind, nameOff, element delta | 16696 | 1844 |
| — net against the derived row | **36295** | **−882** |
| plus `GCData`, `Equal`, `TFlag`/`Align`/`Kind`, `PtrBytes` | 17188 | 1952 |
| — net against the derived row | **35803** | **−990** |
| columns: offsets / kinds / nameOffs / element deltas | 1528 / 108 / 8852 / 6144 | 496 / 76 / 700 / 820 |
| columns: GCData / Equal / TFlag / PtrBytes | 200 / 148 / 336 / 140 | 80 / 80 / 148 / 72 |

Four readings.

**Derived kinds are 45% of the new-descriptor residual, not all of it.** The
six derived kinds hold 162,696 of the 359,417 wrong bytes in new descriptors.
`struct` alone holds 178,934, and a struct descriptor is a field array of
names, offsets and types — nothing regenerates it.

**A regenerator cannot be closed over the old image.** Only 493 of the 3,431
new derived descriptors are built entirely from types the old image already
holds; 2,938 name at least one type that is also new. The plan therefore has
to carry the element reference against the *new* type set, which is what the
element-delta column above measures (6,144 B for 3,431 references, 1.8 B
each).

**The template is nearly free, and the four fields it misses are nearly
free too.** `Size_` is always right and `PtrToThis` is right 95.8% of the
time; adding `GCData`, `Equal`, the flag word and `PtrBytes` as their own
columns costs 492 more xz bytes on the minor pair, because those values come
from a small alphabet shared across thousands of descriptors.

**It pays on the minor pair and not on the patch pair.** 35,803 B net on a
1,467,993-byte patch is 2.4%. On the patch release the 210 new derived
descriptors cost 962 compressed bytes and the plan costs 1,952: the plan's own
floor exceeds what it replaces. A descriptor regenerator has to be optional,
priced per pair, or not built.

## 11. Verdict

| candidate | minor pair | patch pair | build? |
|---|---:|---:|---|
| A. sub-function segment map, resized functions only | +70,013 to +86,463 (4.8–5.9%) | +2,087 to +5,611 (2.2–6.0%) | yes |
| A′. the same, extended to same-length functions | +9,100 fit | +201 fit | no |
| B. fix the `pctab`/`go:func.*` block mapping for `_func` offset fields | ≈+45,000 (≈3.1%) | ≈+1,065 (≈1.1%) | yes, and it is a bug fix |
| B′. `funcID` for unmatched-new records | +468 B raw | +30 B raw | yes, one line |
| C. derived type-descriptor regenerator | +35,803 (2.4%) | −990 | only with a per-pair guard |
| closed: better address maps / consensus shift | +19,283 max | +4,012 max | no |
| closed: dictionary or renumbering for new code | 1.45× vs a 572× control | — | no |
| a field-fix layer for `.go.type` | closed: 27,592 B of 766,010, 11,837 marginal | open: 8,914 B of 45,170, 8,226 marginal | not probed |

Three of the four remaining sources are worth building and one is a bug. Their
sum on the minor pair, if each is independent, is 150,816 to 167,266 of the
1,467,993-byte patch — 10.3% to 11.4%. None of them touches the 494,417 bytes of
genuinely new code inside resized functions, the 1,254,656 bytes of genuinely
new functions, or the 178,934 bytes of genuinely new struct descriptors, which
together are the change the release actually made.

## Reproducing

```
go build -o goattr ./bench/goattr
./goattr -old OLD -new NEW -label NAME -cache DIR -levels 123456789 -jobs 6
```

Levels 7–9 are the probes of §8–§10; level 7 prices itself with the yardstick
level 1 fits, so run it together with 1. The prediction is memoised on the two
input files *and* on the contents of `delta/`, because a prediction memoised
on its inputs alone is the spike's silent-fiction failure mode: a stale entry
still yields a valid, merely larger, correction that nothing checks.
