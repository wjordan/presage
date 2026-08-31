# PGO churn: the cause confirmed, its structure, and the levers left

2026-08-31. The Chrome patch's `.text` residual is attributed to "PGO drift"
(`chrome-elf-whole-image.md` §10) but the attribution had never been checked
against the build inputs, and no experiment could iterate on the phenomenon in
isolation. This note records both: the profile roll confirmed at the source,
an inventory of what a profile delta does to the binary, a synthetic
pure-profile-churn testbed with first numbers, and the ranked directions that
survive the existing negative results. Web-sourced facts were gathered
2026-08-31 and carry URLs; testbed numbers are gcc, not clang (§3 caveat).

## 1. The cause, confirmed at the source

Chromium pins its PGO profile per platform in `chrome/build/linux.pgo.txt`.
At the two corpus tags
(`https://chromium.googlesource.com/chromium/src/+/refs/tags/<tag>/chrome/build/linux.pgo.txt?format=TEXT`):

- 151.0.7922.169: `chrome-linux-7922-1786987994-9a05d427…-43ad97a8….profdata`
- 151.0.7922.173: `chrome-linux-7922-1787137391-84aa9e46…-ec4cb49a….profdata`

Different profiles, embedded timestamps 1.73 days apart; `win64.pgo.txt`
carries the same two timestamps and trailing hashes, so a roll is one
coordinated cross-platform event. The stable-branch roller
(`pgo-linux-chromium-stable`) landed ~100 rolls in 28 days on branch 7922 —
**essentially every stable point-release pair is a profile-churn pair**, and
this corpus pair is one.

Both files are publicly downloadable, no auth, from
`https://storage.googleapis.com/chromium-optimization-profiles/pgo_profiles/<name>`
(~88 MB gzipped, ~311 MB raw). Magic bytes say LLVM IndexedInstrProf v13:
**instrumented PGO, not AFDO** — checked by download, not inferred. Mozilla's
per-release profdata was *not* resolved (per-CI-push artifacts with expiry
only).

## 2. What a profile delta does to Linux x64 codegen

From `build/config/compiler/pgo/{BUILD.gn,pgo.gni}`: phase-2 official builds
use `-fprofile-use` plus `-Wl,-mllvm,-enable-ext-tsp-block-placement=1` under
ThinLTO. The mechanisms that consume the profile, ranked by expected diff
damage, each mapped onto the patch's 9-row cost table (§10 of the whole-image
doc):

| mechanism | effect of a count delta | lands in |
|---|---|---|
| ProfileSummary hot/cold cutoffs (990000/999999 permille) | *global* reclassification: a function's codegen flips with no change in its own counts, because the percentile moved | everywhere below |
| inlining (`-hot-callsite-threshold=3000` vs `-inlinecold-threshold=45`) | a callsite crossing a cutoff swings its budget 65×; resize cascades into regalloc | the 8,991 resized functions = 80 % of `.text` correction |
| ext-TSP block placement | intra-function block moves + branch polarity flips | mostly *absorbed by the matcher* as equivalence runs — part of the 539,300-byte stream, not the wrong bytes |
| regalloc spill weights (block frequency) | whole-body byte ripple at same semantics | the 66.3 % shape-change bucket |
| SelectOptimize / X86CmovConversion, indirect-call promotion | cmov↔branch flips, promotion stubs | small populations, same-size churn |
| lld `.llvm.call-graph-profile` sort | function reordering | observed mostly *stable*: the 31,281-breakpoint shift map says order survived this roll |

Confirmed **absent** on Linux x64: `-fsplit-machine-functions`, basic-block
sections, BOLT/Propeller, orderfiles (Android-only), `-pgo-cold-func-opt`
(Android/Fuchsia-only) — consistent with the shipped binary's single
monolithic `.text` and ~zero `.cold` symbols.

Prior art (searched): BOLT's stale-profile matching
(arXiv 2401.17168, `--infer-stale-profile`) is the only formalism mapping
profile drift onto code drift, and it runs the opposite direction
(re-anchoring an old profile onto new code). No published work models
inlining/regalloc differences for delta compression, ships profile deltas as
patch side-information, or predicts compiler output externally. Zucchini has
no successor.

## 3. The synthetic testbed

`~/.cache/presage-synth-pgo/run.sh` — full pipeline 72 s, `run.sh measure`
re-measures. SQLite 3.54.0 amalgamation + shell (local checkout), gcc 15.2.0
`-O2 -g -ffunction-sections`, `-fprofile-generate`/`-fprofile-use`. Builds:
A = OLTP-workload profile, A0 = A rebuilt (determinism check, byte-identical),
A2 = same workload +10 % rows (small perturbation), B = analytics workload
(large perturbation), NOPGO. `churn/main.go` reports per-function churn;
patch baselines via bsdiff/xdelta3/zstd/xz.

Headline rows (2,646 functions, 920 KB `.text`; `.nodbg` = DWARF stripped):

| | A→A2 (small) | A→B (large) | NOPGO→A |
|---|---:|---:|---:|
| byte-identical functions | 17.7 % | 12.4 % | 6.1 % |
| same size, bytes differ | 81.0 % | 56.6 % | 5.3 % |
| resized | 1.3 % | 30.9 % | 88.6 % |
| order permutation distance | 0.1 % | 30.8 % | 67.2 % |
| hot/cold flips (of ~2,044 cold) | 0 | 142 | 1,971 |
| differing bytes in same-size fns | 3.39 % | 8.95 % | 30.9 % |
| …after crude E8/E9 canon | 1.65 % | 3.95 % | 24.6 % |
| bsdiff `.nodbg` | 59,561 | 364,564 | 638,373 |
| zstd --patch-from `.nodbg` | 88,874 | 345,611 | 556,986 |

Findings:

- **Small perturbation is almost pure same-size, displacement-dominated
  churn**: layout unchanged, hot/cold set unchanged, and zeroing call/jmp
  displacements alone halves the differing bytes and makes 698 more functions
  identical. This is the regime presage's retargeting already owns.
- **Large perturbation is bimodal**: a same-size-churn population *plus* 31 %
  resized (inlining) and a 31 % order permutation that defeats generic
  differs (bsdiff lands at only ~1.7× under solo xz).
- **Hot/cold assignment is remarkably stable between two real profiles**
  (142 of ~2,000 flips); PGO-on vs PGO-off is the cliff. Cold-splitting is
  not the churn driver; inlining and ordering are.
- **DWARF churns proportionally** (~4× the absolute bytes, same ratio), not
  as independent noise.
- The Chrome 1.73-day roll sits between A2 and B: layout stable (like A2),
  but the inlining/resize population real (like B, smaller share).
- Trap for anyone measuring PGO deltas: gcc's default
  `-grecord-gcc-switches` embeds the profile *path* in `DW_AT_producer`; one
  changed character cascaded to a 1.19 MB raw diff. `-gno-record-gcc-switches`
  restores byte-identical rebuilds.
- Fidelity caveat: gcc, not clang — no ext-TSP, different inliner. The
  phenomenology (bimodal churn, displacement dominance, ordering) is the
  claim, not the exact ratios. A clang variant needs a toolchain install.

## 4. Directions, ranked

1. **Use the testbed.** 72 s per cycle against Chrome's minutes, with
   cause-labelled ground truth. Next concrete steps: run `presage diff` on
   A→A2 and A→B `.nodbg` pairs against the table above (A→A2 should collapse
   to near the ~1.65 %-canon floor); consider promoting a pair to a corpus
   tier. Also measurable here and nowhere else: the bridge-build question —
   does old→(new source, old profile)→(new source, new profile) sum smaller
   than the direct patch (vendor-integrated pipelines only).
2. **Profile-delta attribution on Chrome** (encoder-side, one-off). Both
   profdata files are public; `llvm-profdata` (an LLVM release tarball
   unpacked under `~/.cache`, no root) can diff per-function counts, joined
   against the 8,991 resized functions via the symbols the encoder already
   loads. The question it decides: is churn *count-driven* (the function's
   own profile moved) or *cutoff-driven* (global percentile shift)? If
   cutoff-driven, even fleets that re-train profiles carefully will churn,
   and only exact profdata pinning helps.
3. **The vendor lever: profile pinning is a patch-size policy.** The C++
   analogue of the one-liner-floor build-flags finding ("the codec cannot
   remove what the linker was asked to randomise"). Measured on the testbed:
   pure profile churn costs 25 % of the image as a patch; a pinned profile
   costs ~0. Chrome can't be pinned by us, but for the fleet product story,
   rolling PGO on point releases is deliberate entropy injection with a
   quantifiable delivery cost. Belongs in the product docs.
4. **One cheap Chrome probe: equivalence-run locality.** §14 found 79.4 % of
   displacement fixes are jumps within their own function; if a comparable
   share of equivalence runs have source and destination inside the same
   function (ext-TSP block moves), a function-relative source basis could
   shrink part of the 539,300-byte stream. Price against the 1.061 B/run
   yardstick before building.
5. **The one >100 KB door left: a field-aware terminal coder.** §11.4
   bounded prediction-conditioning at ~55 K, but a structured x86 coder over
   the correction's literal column (783,640 raw bytes) is unexplored;
   exe-specific CM coders typically take 15–25 % over LZMA on code. Costs:
   decode CPU, a custom coder to maintain, and the like-for-like xz
   comparison against the incumbent. A G6 question for the SPEC, not a spike.
6. **The research program: a learned recompilation model.** Harvest
   (old, new) bodies of churned functions across dozens of public Chrome
   release pairs; learn "how clang re-emits a function when hotness shifts";
   encoder verifies candidates and ships selection bits (principle 2).
   Confirmed novel (§2 prior-art gaps). Ceiling on this pair ≈ the resized
   bucket's ~1.0 M wrong bytes at whatever hit-rate materialises. The
   testbed is the substrate for a v0; do not start before 1–4.

## 5. Build integration without touching the tuning: the sidecar channel

2026-08-31, follow-up question: if the patch pipeline is tightly integrated
with the vendor's PGO build, what *metadata* could be captured — with zero
codegen impact, since a Chrome-class vendor will not alter PGO tuning for
patch size — to make future patches cheaper?

The discriminator: the encoder already holds both binaries and full debug
info, so build integration adds nothing encoder-side. The new channel is
metadata **carried forward on the client** — each patch installs a sidecar
alongside the binary, delta-coded release to release, that the *next* patch's
decoder uses as a second reference input. The container's multi-reference
design (SPEC §5, open question 5) is the natural seam. Every candidate is
therefore priced against the plan-side ledger rows, because that is all a
sidecar can reach.

Ranked:

1. **Symbol-identity sidecar** (function boundaries + names for the stripped
   binary — retained `nm` output, no build change at all). The 256 K
   function map exists because the decoder cannot know new boundaries or
   correspondence, even though the encoder derived the map by name-joining
   the two symtabs (G7). With the old symtab carried, correspondence is name
   equality and the patch ships only the symtab *delta*. **Measured** (§5.1):
   −98,460 xz, −3.76 % of the patch. Portable across PE/Mach-O.
2. **Basic-block address map — measured dead (§5.2).** Verified first:
   clang `-fbasic-block-address-map` (x86 ELF since LLVM 18; the
   `-fbasic-block-sections=labels` spelling is deprecated in clang 20)
   emits `.llvm_bb_addr_map`, type `SHT_LLVM_BB_ADDR_MAP`, **not
   SHF_ALLOC** — never loaded at runtime, strippable, pure metadata with no
   codegen or layout effect; ~11 MB on an optimized PGO clang binary. The
   hoped-for target was the equivalence stream (ext-TSP moves as
   intra-function block permutations). The probe killed it: blocks do not
   explain the moves (29 % whole-block coverage, and a 25× gap to
   break-even).
3. **PGO-specific captures rank low**: per-callsite hotness classifications
   could prior the 59 K choice bitmap; ext-TSP layout intent is subsumed by
   #2; inlining decision logs describe a resize without supplying its bytes
   (§16.3 already refuted decoder-side splicing).

**The boundary, stated as an argument**: no sidecar reaches the 1.17 MB of
recompiled bytes. Suppose the vendor also built each release with a frozen
canonical profile and the client carried that build: old→new factors into
(source change under frozen profile — near-Go-class small) + (frozen→fresh
profile on new source — precisely the pure-profile-churn patch, ~25 % of the
image on the testbed). The fresh profile's effect on codegen must cross the
wire because the shipped binary embodies it; the only artifact containing
those bytes is compiler output. Metadata changes how cheaply the patch
*addresses and describes*; it cannot supply what the compiler re-emitted.
The plan-side pool is ~1.1 MB and the first estimate for realistic capture
was 300–500 K; §5.1–§5.2 measured both candidates and §5.1b built the
survivor: **−135,868, −5.18 % of the patch** (the block half of the
estimate died).

### 5.1 The symbol-sidecar probe, measured

`bench/elfpredict -probes sidecar` (`sidecar.go`), run 2026-08-31 on
`-resume $C/elfpredict-wi33`, 12 s; reproduced twice byte-identically.
It prices the hypothesis directly: the decoder holds the old symtab for
free, and the patch ships a new-side symtab delta instead of the map
columns.

**Calibration.** The ledger's "function map" row is the plan's structure
stream (256,292 standalone xz here; the doc's 256,172 is pre-§17 drift).
It is *not* pure correspondence + layout: it bundles the 261,104
reference points and 31 ranges (~18,700), which a sidecar does not touch.
The replaceable part — the five map columns less the copy bitmap — costs
**237,044, priced marginally**: re-marshalling with the maps merely removed
explodes the point table (its index basis derives *from* the map), so the
probe blanks the map columns inside an otherwise identical stream, asserted
byte-identical when nothing is blanked. The coupling is also why the win
survives: the decoder reconstructs the same map from the symtabs, so
`referenceTargets` and the point columns are unchanged.

**Fidelity.** Name-join (duplicates disambiguated by relative order — only
245 duplicated names covering 523 of 925,663 units) agrees with the shipped
structural map on **99.854 % of mapped units, 99.668 % of mapped bytes**.
1,352 exceptions (265 different-source, 1,087 map-only: renames and ICF
folds) cost 4,872 xz to ship.

**The replacement stream**, priced as one contiguous `xz -T1` (the
standalone trap biases *against* the sidecar here, so this is a floor):

| | count | xz |
|---|---:|---:|
| drop runs (1,333 dropped units) | 1,209 | 2,204 |
| order exceptions | 26,798 | 56,460 |
| size deltas (resizes) | 9,518 | 15,604 |
| inserts: positions + sizes + names | 1,406 | 52,828 |
| layout fixups (align-16 replay, 90.87 % exact) | 84,549 | 8,636 |
| correspondence exceptions | 1,352 | 4,940 |
| **whole stream, contiguous** | | **138,584** |

**Verdict: 138,584 vs 237,044 = −98,460 xz, −3.76 % of the 2,621,664
patch** — the top of the §5 estimate, and conservative twice over: the
1,406 inserted mangled names are 49,036 of the stream (35 %) and a real
sidecar would dictionary them against the carried table (the same stream
without names is 89,512); and the pricing is standalone-contiguous.
Reorder exceptions, not layout, are the expensive part (41 % of the
stream); layout replay is nearly free because a 16-byte-aligned residual
column is almost all small values. What the sidecar does *not* replace:
the points/ranges residue, the equivalence stream, the choice bitmap, the
field corrections, and every byte correction — the structure stream goes
256 K → ~157 K, not to zero.

Design refinement for any real build-out: name-join needs *equality*, not
names — the carried table can be 8-byte name hashes (~8 MB installed
instead of tens of MB of mangled C++), and inserted functions ship hashes
too, shrinking the 49 K insert-names column to ~11 K and the win toward
−135 K (*estimate*). Hash-pinned to the binary, bootstrapped by full
install or a sidecar-less first patch (capability-gated variant per G4).

### 5.1b Built for real: the sidecar rung

`bench/elfpredict -rungs sidecar-map` (`symsidecar.go`), 2026-08-31: a full
encoder + decoder, not a probe. The decoder holds only the old image, the
carried table and the patch; it reconstructs the `[]mapping` from the
symtab-delta stream (asserted exactly equal to the shipped map), parses
points/ranges against the reconstructed bases, and predicts
**byte-identically** to `corrected-fields`; the correction replays
byte-exactly. Normal-path plan bytes are pinned unchanged (`planSidecar`
is a new mode value).

| | corrected-fields | sidecar-map |
|---|---:|---:|
| plan xz | 1,244,736 | 1,108,868 |
| correction xz | 1,376,928 | 1,376,928 |
| **total** | **2,621,664** | **2,485,796 (−135,868, −5.18 %)** |
| vs Zucchini 5,263,732 | 49.81 % | **47.22 %** |

The built number beats the probe's −98,460 for the reasons the probe
flagged against itself: joint compression (the stream costs 99,428 inside
the plan vs 138,584 standalone) and hashed inserts (11,572 vs 49,036 for
names). The carried table is 925,590 units, 13.56 MB raw / 12.4 MB xz —
alias hashes dominate (ICF folding gives inserted units 16.8 names on
average), and that average is also the build's sharpest lesson:

* **One hash per insert is load-bearing.** Shipping every alias hash per
  inserted unit costs 190,184 xz — 64-bit hashes are incompressible — and
  flips the rung to a **+42,744 loss**. §5.1's "~11 K hashed names" holds
  only at one hash per unit.
* **Reconstruction order is load-bearing**: drops → insert positions →
  order+size walk → layout replay → correspondence exceptions *last* (a
  rename may point at a dropped unit the walk rejects by construction).
  The delta stream must sit exactly where the map columns were, because
  `referenceTargets` derives from the maps.
* The probe missed an exception class — the join proposing a source the
  map lacks — covered by a shared `s−j` sentinel column; 0 extra cost here.
* 0 distinct-name hash collisions across 1.85 M names; 1,352
  correspondence exceptions; 0 unrepresentable mappings.
* **Roll-forward caveat**: an inserted unit's rolled-forward entry carries
  one of its aliases, and a renamed unit a stale hash — correctness is
  untouched (the next encoder replays the client table byte-exactly), but
  join fidelity can degrade over a chain of patches. −135,868 is a
  first-patch number; the steady state is unmeasured.

### 5.2 The block-map probe, measured dead

`bench/elfpredict -probes blocksidecar` (`blocksidecar.go`), run
2026-08-31, reproduced twice identically. Three-phase design with stop
rules; it stopped at phase 2.

**Anatomy first** (phase 1): the 539,504-xz equivalence stream (158,544
runs), classified by the destination function's mapped source, priced
marginally (rows deleted, contiguous `xz -T1`; marginals are sub-additive
— the classes share LZ context):

| class | runs | bytes covered | marginal xz | xz/run |
|---|---:|---:|---:|---:|
| (a) source at the map-predicted offset | 22,313 | 121.0 MB | 24,460 | 1.10 |
| (b) intra-function move | 80,786 | 16.9 MB | **206,668** | 2.56 |
| (c) source in another function / outside old `.text` | 30,669 | 3.1 MB | 145,528 | 4.75 |
| (d) destination outside a mapped function | 24,776 | 147.9 MB | 81,076 | 3.27 |

Class (a)'s 1.10 bytes/run for 121 MB confirms §9.16 is already banked.
Class (b) — the ext-TSP signature, 51 % of runs, 38 % of the stream — is
the block model's target, so phase 1 passed.

**The kill** (phase 2): block boundaries derived by disassembly on both
sides (branch targets + post-terminator starts, calls don't split —
matching the BB-addr-map definition). 57.09 % of class-(b) bytes start on
a block boundary on both sides, but only **29.15 % are whole-block
explained** — the byte matcher segments where bytes stop being *equal*,
which is mid-block, and a partly-carried run still ships its row. This is
§16.1's failure one level up: run boundaries are content boundaries, not
structural ones. Worse, the alphabet is inverted: blocks average 19.6
bytes in the affected functions (137.6 per function) against class-(b)
runs averaging 209 — the replacement alphabet is 10× denser than what it
replaces. Deleting exactly the 23,989 whole-block-explained rows saves
48,592 xz; the block stream replacing them spans 1,138,200 new blocks,
a break-even of **0.043 bytes per shipped block**, an order of magnitude
below the cheapest conceivable per-block record — before the carried
map's own upkeep for churned functions the stream never names. No
optimistic ID-matching variant crosses a 25× gap; phases 3–4 are moot.

**Consequence for §5's ceiling**: the 12–19 % estimate is dead; the
sidecar direction's measured ceiling is the symbol sidecar's −98,460
(≈ −135 K with hashed names, *estimate*) — **about 4–5 % of the patch,
from updater plumbing alone, with no build change**. The BB address map
earns nothing here and should not be asked of a vendor.

## 6. Damping knobs, verified and recorded (self-shipped fleets only)

Scoped out for Chrome/Firefox-class vendors (they will not change PGO
settings for patch size), but live for the fleet product story, and all four
verified against LLVM/GCC sources 2026-08-31:

- **Inline replay**: `-mllvm -cgscc-inline-replay=<file>` with
  `-scope=<Function|Module>`, `-fallback=<Original|AlwaysInline|NeverInline>`
  (LLVM 13+; sample-profile variants exist). Consumes the `-Rpass=inline`
  *text* remark log, not `-fsave-optimization-record` YAML. `Original`
  fallback = replay known callsites, let the advisor decide new ones — the
  hysteresis mode.
- **Threshold pinning**: `-mllvm -profile-summary-hot-count=` /
  `-profile-summary-cold-count=` override the 990000/999999 permille
  percentile computation with absolute counts
  (`ProfileSummaryBuilder.cpp`) — removes the global-reclassification
  mechanism while profiles stay fresh.
- **Order pinning**: lld's `--symbol-ordering-file` takes strict precedence
  over call-graph-profile sort; CG sort still places symbols absent from the
  file (`lld/ELF/Writer.cpp`).
- **Testbed perturbation is scriptable for GCC**: `gcov-tool merge -w w1,w2`
  and `gcov-tool rewrite -s <scale|frac>` — enables profile-quantization
  experiments on the §3 harness.

## 7. Dead, and why (recorded so nobody pays twice)

- **Decoder-side use of the public profiles.** 88 MB of reference to improve
  a 2.6 MB patch, uncacheable across pairs (every pair has fresh profiles).
  Replaying lld's ordering from it is also moot: order survived this roll.
- **Inlining-splice reconstruction** (predict a resized body by splicing the
  callee into the caller): the §16.3 canonicalised-dictionary and RNF probes
  already showed the residual is not callee bodies under a different
  allocation, in any normal form. A constructive splice makes the same
  content claim those probes refuted.
- **Block-reorder grammars for the wrong bytes**: reordered blocks that
  survive intact are already found by the matcher (they cost equivalence
  runs, not wrong bytes); the wrong bytes are re-emitted instructions.
- **A carried block map against the equivalence runs**: measured dead in
  §5.2 — whole blocks explain 29 % of intra-function move bytes, and the
  block alphabet is 10× denser than the runs it would replace. This also
  answers §4's direction 4 (equivalence-run locality): the locality is
  real (51 % of runs) and already cheap (2.56 xz bytes/run), and no
  structural basis beats it.
