# presage encoder profile — Chrome 151.169 → 151.173

Historical measurement at the pre-v5 encoder. The profiling interfaces it
motivated are now permanent CLI flags (`-cpuprofile`, `-allocprofile`, and
`-traceprofile`). Container v5 also revisited the preset across every terminal
compression site, not only the final frame: Brotli 9 plus the compact CM model
cuts the Firefox encode from 55.37 to about 30 seconds, for a 7.5% patch-size
increase.

A follow-up v5 heap profile separated cumulative allocation (19.6 GB) from the
peak live set. During whole-image matching, the two input images and their two
canonical copies occupy about 708 MiB; the dense source-seed positions, bucket
ends and hash scratch occupy another 981 MiB. This explains the approximately
1.7 GiB floor. A 1.5 GiB soft heap limit gives 1,801,300 KiB process RSS with
no time or output change, versus 3,521,840 KiB unconstrained. At 1 GiB the
collector cannot reclaim the live matcher and merely slows the encode. The CLI
therefore derives a default limit from the larger image with a 1.5 GiB floor;
an explicit `GOMEMLIMIT` overrides it. The original evidence follows.

> Profiled at commit 05518bf (pre derived-map/CM — the isolation worktree
> snapshotted the session-start HEAD). The trial-compression and matching
> machinery measured here is unchanged by those ports (both measured flat
> on encode), so the ranking stands; the derived-map sweep and CM coder
> simply do not appear.

The call-site counters in `internal/cz/cz.go` and the diagnostic print in
`presage/split.go` were scratch and did not land.

Machine: 24 cores (`nproc`). Go binary built from this worktree at `05518bf` + working-tree
changes. Raw artifacts under `prof/`: `diff.cpu`, `patch.cpu`, `diff.top-cum.txt`,
`diff.top-flat.txt`, `diff.peek-compress.txt`, `diff.peek-phases.txt`, `diff.peek-elfmod.txt`,
`patch.top-cum.txt`, `diff.time.txt`, `diff.counters.txt`, `diff.pieces.txt`, `patch.time.txt`.

## 0. Headline

`presage diff -symbols` on Chrome: **112.6 s wall, 173.1 CPU-s, 6.06 GB peak RSS**, for a
291,196,232 B target → 2,581,091 B patch (2.6 MB/s).

**Effective parallelism: 1.53× on 24 cores (6.4 % of the machine).** The encoder is
essentially a serial program with two small parallel bursts.

**Half of all CPU is brotli-11, and 83 % of the bytes fed to brotli-11 are thrown away.**
Exactly 48 MB total is handed to `cz.Compress` across the whole encode; only 8 MB of that is
the patch body that actually ships. The other 40 MB is trial compression used to *choose*
between correction shapes and piece kinds.

## (a) Phase breakdown

CPU-seconds are from the profile and are solid. Wall-seconds are derived (main-goroutine
on-CPU time plus its share of blocked-on-worker time) and are estimates ±3 s; they sum to
112.6 s by construction.

| Phase | CPU-s | % CPU | ~wall-s | notes |
|---|---|---|---|---|
| **Compression (`cz.Compress`) — total** | **86.97** | **50.4** | **~64** | brotli-11 84.32 s, zstd 2.65 s |
| &nbsp;&nbsp;`delta.czLen` — 3-way shape trial | 45.27 | 26.2 | ~26 | thrown away |
| &nbsp;&nbsp;`delta.modalPack` — modal sub-streams | 15.13 | 8.8 | ~15 | kept only if modal wins |
| &nbsp;&nbsp;`presage.compressAll` — LZ-vs-columnar trial, per piece | 11.26 | 6.5 | ~11 | ≥half thrown away |
| &nbsp;&nbsp;`presage.frameBody` — **final shipping bytes** | 10.42 | 6.0 | ~10 | productive |
| &nbsp;&nbsp;`presage.frameCost` — split-vs-whole decision | 4.89 | 2.8 | ~5 | exact duplicate of a `czLen` already done |
| **elfmod analysis (match / plan / predict)** | **67.94** | **39.4** | **~38** | 36.13 s serial + 31.81 s in `parallelStats` workers |
| &nbsp;&nbsp;`eqmatch.Match` (+`buildSeedIndex`) | 16.48 | 9.5 | ~16 | **serial** |
| &nbsp;&nbsp;`retargetEquivalencePrediction` → `x86.WalkReferences` | 16.59 | 9.6 | ~3 | parallel |
| &nbsp;&nbsp;`predictDecoded` → `x86.Relocate` | 13.88 | 8.0 | ~3 | parallel |
| &nbsp;&nbsp;`predictImage` / `inferReferencePoints` / `constructPlan` / `codeUnits` / `maskedText` / marshal / fieldfix / reloc / oracle | ~16.9 | 9.8 | ~16 | mostly serial |
| **Correction-stream construction (non-compress `delta`)** | ~7.5 | 4.3 | ~7 | `corrShape.write` 3.61, `modalWrite` 3.54, `modalRegions` 0.27, `EncodeColumnar` 0.12 |
| **Symbol parsing (`symbols.elfReader.Funcs`)** | 1.76 | 1.0 | ~1 | ELF `.symtab`; **no DWARF cost is visible at all** |
| **BLAKE3 hashing** | 0.24 | 0.14 | ~0 | noise |
| **GC / allocator** (`mallocgc` 7.38 cum, `memclrNoHeapPointers` 5.77 flat, bg mark ~1) | ~8–10 | ~5 | — | overlaps the above |

Cross-check: `main.diff` cum = 98.25 s (the main goroutine); the remaining 74.4 s of samples
are in worker-goroutine roots `delta.encodeCorrection.func1` (32.17 s, the 3-way czLen fan-out)
and `elfmod.parallelStats.func1` (31.81 s), plus GC. The main goroutine splits
`residual` 51.36 / `elfmod.Module.Analyse` 36.13 / `frameBody` 10.43.

### Compression call-site census (instrumented counters, `prof/diff.counters.txt`)

| call site | calls | MB in |
|---|---|---|
| `czLen` (shape trials) | 70 | 27 |
| `modalPack` | 140 | 6 |
| `compressAll` (split piece, LZ *and* columnar) | 104 | 5 |
| `frameBody` (**FINAL**) | 2 | 8 |
| `frameCost` (split-vs-whole) | 1 | 2 |
| **split pieces** | **13** | 277 |

Brotli-11 throughput measured here: 48 MB / 84.3 s = **0.57 MB/s**. zstd-`SpeedBestCompression`
on the same 48 MB: 2.65 s = **18 MB/s**, i.e. **32× faster**.

### Piece census (`prof/diff.pieces.txt`) — important for the parallelism plan

13 pieces, and they are extremely skewed:

| piece | span B | corr B | cols B | kind |
|---|---|---|---|---|
| 8 | **225,655,845** | 2,040,948 | 2,002,705 | columnar |
| 2 | 26,546,616 | 7 | 0 | columnar |
| 4 | 23,981,900 | 360,346 | 439,832 | lz |
| 10 | 11,760,272 | 33,449 | 39,036 | lz |
| 12 | 1,416,704 | 735 | 754 | lz |
| 6 | 1,159,416 | 38,012 | 148,361 | lz |
| 13, 5, 1, 3, 9, 11, 7 | ≤ 266,056 | ≤ 26,686 | — | mixed |

One piece carries 2.04 MB of the ~2.5 MB of correction. **Parallelizing across pieces is
worth ~1.2–1.3×, not 13×.**

## (b) Effective parallelism

```
User 166.75 s + Sys 6.35 s = 173.10 CPU-s   /   112.72 s wall   =   1.54×
nproc = 24                                  →   6.4 % machine utilisation
```

Only two things are parallel today: `elfmod.parallelStats` (fan-out over
`retargetEquivalencePrediction` and `predictDecoded`, plus `x86.WalkBodies` at GOMAXPROCS),
and `delta.encodeCorrection`'s 3-way `czLen` fan-out (bounded at 3). `splitResidual`'s
13-piece loop, `compressAll`, `frameBody`, `frameCost`, `eqmatch.Match` and every brotli
call are strictly serial; `zstd.NewWriter` is built with `WithEncoderConcurrency(1)`.

## (c) Top 10 cumulative

```
      flat  flat%   sum%        cum   cum%
         0     0%     0%     98.25s 56.91%  main.diff
         0     0%     0%     98.23s 56.90%  presage.Encode
         0     0%     0%     86.97s 50.37%  internal/cz.Compress
         0     0%     0%     84.32s 48.84%  brotli.encoderCompressStream
         0     0%     0%     84.24s 48.79%  brotli.encodeData
         0     0%     0%     51.36s 29.75%  presage.residual
         0     0%     0%     45.27s 26.22%  delta.czLen
         0     0%     0%     43.94s 25.45%  brotli.(*Writer).Close
     0.23s  0.13%  0.13%     42.56s 24.65%  brotli.createHqZopfliBackwardReferences
         0     0%  0.13%     40.38s 23.39%  brotli.(*Writer).Write
```

Top flat: `brotli.findBlocksLiteral` 21.48 s (12.4 %), `brotli.updateNodes` 18.73 s (10.9 %),
`x86.fastStep` 11.32 s (6.6 %), `eqmatch.Match` 9.97 s (5.8 %),
`slices.BinarySearchFunc[equivalence]` 6.32 s (3.7 %), `runtime.memclrNoHeapPointers` 5.77 s.

## (d) Ranked optimization passes

### 1. Use a cheap size *proxy* for trial compressions — brotli-11 only for shipped bytes
**Free-ish (encoder-side choice; may perturb size by a hair).**
Evidence: 40 of the 48 MB fed to `cz.Compress` is thrown away after being used only as a
number. brotli-11 costs 84.3 CPU-s of 172.6; zstd-best on the same bytes would cost ~2.2 s.
The trial sites are `delta.czLen` (45.27 s), `presage.frameCost` (4.89 s) and the losing half
of `presage.compressAll` (~5 s of 11.26 s).
Fix: thread a `trial bool` (or a `cz.SizeProxy(b) int` entry point) into `czLen`, `frameCost`
and the `compressAll` comparison so they call `CompressZstd` only.
**Estimated win: ~70 CPU-s, ~55–60 s wall — 112 s → ~50–55 s.**
**Risk/effort: small (one plumbing change, ~30 lines). Low risk.** *Caveat:* the shape/kind a
zstd proxy picks can differ from what brotli-11 would pick, so patch size can move slightly.
This does **not** change the format or the shipped compressor; verify on the corpus and expect
well under 1 %. If even that is unacceptable, use brotli quality 5 as the proxy instead
(~10× faster than q11, ranks shapes far more like q11 does) for a smaller but still large win.

### 2. Stop double-compressing the modal candidate
**Free — the shipped bytes are identical.**
Evidence: `encodeModal` → `modalPack(split=true)` brotli-11s each modal sub-stream (15.13 CPU-s,
6 MB) to *build* the candidate, and then `czLen` brotli-11s that already-compressed result again
purely to size it (part of the 45.27 s; the modal candidate is one of three). Compressing
compressed bytes is the slowest possible way to learn `len(stream)`.
Fix: for the split-modal shape, size the candidate as `sum(len(z)) + header` instead of
re-running `czLen` on it.
**Estimated win: ~10–15 CPU-s / wall.** Composes with #1 (after #1 the outer pass is cheap, but
`modalPack`'s own 15.13 s remains — see #3).
**Risk/effort: small. Low risk.**

### 3. Memoize `frameCost(whole)` — it recomputes a `czLen` that was just computed
**Free — bit-identical output.**
Evidence: `presage/split.go:frameCost` and `delta/correct.go:czLen` are the same loop, same
`FrameSize`, same compressor. `residual()` calls `EncodeCorrectionAdaptive` (which computes
`czLen` of the winning candidate and discards it) and then calls `frameCost(whole)` on that
same winner. 4.89 CPU-s, 2 MB, pure duplicate.
Fix: have `EncodeCorrectionAdaptive` return the winner's compressed size alongside the stream.
**Estimated win: ~5 s. Risk/effort: trivial, ~10 lines.**

### 4. Skip the whole-region correction trial when the module supplies cuts and split wins
**Free-ish; changes only which of two equally-valid encodings is chosen in edge cases.**
Evidence: `residual()` unconditionally builds `whole = EncodeCorrectionAdaptive(pred, target)`
over the full 277 MB region — three candidates, each brotli-11'd — and then builds the pieces
and (here) discards `whole` entirely. The whole-region correction is ~2.5 MB; three candidates
plus `frameCost` is roughly 10–12 MB of the 27 MB `czLen` total.
Fix: compute `whole` lazily/proxied, or gate it: build the piecewise form first and only build
the whole-region form if the piece count is small or the split total looks marginal.
**Estimated win: ~15–20 CPU-s if the whole-region trial is skipped outright; less if merely
proxied (subsumed by #1). Risk/effort: small–medium; needs a rule that doesn't regress the
cases where `whole` actually wins.**

### 5. Parallelize `splitResidual`'s piece loop — but expect only ~1.2×, not 13×
**Free — bit-identical output.**
Evidence: the loop in `presage/split.go` is serial across 13 pieces, and each iteration does
`EncodeCorrectionAdaptive` + `EncodeColumnar` + two `compressAll`s. `splitResidual` is
29.62 CPU-s. Fix is an `errgroup` over the pieces (outputs are already independent buffers,
assembled by index).
**Honest caveat: piece 8 alone holds 2.04 MB of the ~2.5 MB of correction, so Amdahl caps this
at roughly 1.2–1.3× on the split path (~5 s wall), not the 13× the piece count suggests.**
**Risk/effort: small. Low risk.** Worth doing, but rank it *below* #1–#4; it is the intuitive
fix and the profile says it is the least valuable of the structural ones.

### 6. Replace the sorted-span binary searches with an ordered cursor / flat index
**Free; helps `presage patch` too.**
Evidence, encode: `slices.BinarySearchFunc[equivalence]` 7.08 s + `[mapping]` 5.64 s +
`[addressPoint]` 3.25 s ≈ **16 CPU-s (9.3 %)**, reached through `equivalencePlan.sourceAt`
(7.59 s) and `addressLookup.target` (8.31 s). Evidence, apply: the same two searches are
1.95 s + 1.15 s of 17.5 CPU-s (**18 % of apply**). References are walked in ascending address
order, so a monotone cursor or a page-indexed side table replaces most of the `log n`.
**Estimated win: ~10 CPU-s on encode, ~2.5 CPU-s (≈0.6 s wall, 13 %) on apply.**
**Risk/effort: medium (touches `elfmod/equiv.go`, `retarget.go`, `oracle.go`). Low correctness
risk — it's a pure lookup change, guarded by the existing round-trip tests.**

### 7. Parallelize / speed up `eqmatch.Match`
Evidence: 16.48 CPU-s (9.5 %), entirely serial on the main goroutine —
`Match` flat 9.97 s, `buildSeedIndex` flat 4.83 s. After #1 this becomes one of the two
largest remaining serial blocks.
**Estimated win: up to ~14 s wall if it parallelizes cleanly. Risk/effort: medium — matching
is order-sensitive and the output must stay deterministic.** Needs a look at the algorithm
before committing to an estimate; I did not analyse it.

### 8. Run zstd and brotli concurrently inside `cz.Compress`
Evidence: `cz.Compress` runs `CompressZstd` then brotli-11 sequentially and keeps the smaller;
they are independent. Trivially parallel.
**Estimated win: ~2.5 CPU-s only (zstd is 3 % of the pair) — near-worthless once #1 lands.
Listed for completeness; do not bother.**

### 9. Memory / allocator
Evidence: 6.06 GB peak RSS for a 291 MB target (21×); `mallocgc` 7.38 s cum,
`memclrNoHeapPointers` 5.77 s flat, `growslice` visible in both profiles.
**Estimated win: a few percent from buffer reuse, or from a higher `GOGC` at the cost of even
more RSS. Low priority, but the 6 GB itself may be a product constraint worth a separate look.**

### Superseded probe: lowering only the *final* preset
Dropping `frameBody`'s brotli-11 to q10, or shipping zstd only, is the obvious-looking knob and
it is a trap: **`frameBody` is only 10.42 CPU-s (6 %)**, so the ceiling is ~10 s wall, and every
byte of it is paid for in patch size. The package comment in `internal/cz/cz.go` already records
that brotli q10 was 4–5 % larger. Passes #1–#5 are 8× larger wins and cost nothing.

## (e) `presage patch` (apply)

**4.59 s wall, 17.77 CPU-s, 3.87× parallelism, 3.55 GB peak RSS**, for the same 291 MB output.
Utilisation is 16 % of 24 cores — better than encode, still low.

Apply is **not** decompression-bound. Decompression does not reach the top-30 at all. It is
dominated by rebuilding the prediction, the same work the encoder does:

| | cum s | % |
|---|---|---|
| `x86.WalkReferences` | 8.60 | 49.3 |
| `elfmod.parallelStats` fan-out | 7.85 | 45.0 |
| `x86.WalkBodies.func1` | 5.71 | 32.7 |
| `x86.step` / `x86.fastStep` | 4.64 / 3.32 | 26.6 / 19.0 |
| `elfmod.Module.Materialise` → `predictImage` | 3.00 | 17.2 |
| `elfmod.retargetEquivalencePrediction` | 4.09 | 23.4 |
| `slices.BinarySearchFunc` (both shapes) | 3.10 | 17.8 |
| `elfmod.unmarshalPlan` | 1.42 | 8.1 |

So pass **#6** is the one optimization that pays on both sides. Beyond that, apply's headroom is
in the `x86` walkers themselves (`fastStep` 3.26 s flat, 19 %) and in raising the fan-out's
efficiency from 3.9× toward the core count.

## (f) What I could not attribute

- **DWARF parsing does not appear in the profile at all** (< 0.5 s, below the drop threshold).
  Only `symbols.elfReader.Funcs` shows, at 1.76 s. Either the `-symbols` path uses `.symtab`
  rather than DWARF on these inputs, or the DWARF reader is lazy and nothing forced it. Worth
  confirming in code before anyone concludes "symbol parsing is free" in general.
- **The new derived-map / dispcol code does not show up** under any name I searched for. Either
  it is not on this path or it is below 0.86 s.
- The ~8–10 CPU-s of GC/allocator work is spread across every phase and is double-counted
  against them in the table rather than being carved out.
- The wall-second column is a derivation, not a measurement. The CPU-second and % columns, the
  byte counters, and the piece census are direct measurements.
- I did one profiled run of each command. No repeats, no variance estimate. The three encode
  runs I did make (112.4 s, 114.9 s, 119.6 s wall — the latter two carry instrumentation and
  stderr I/O) suggest run-to-run noise of a few percent.

## Outcome (2026-08-31, commit 48de052)

Passes #1/#2/#3/#5 landed: Chrome encode 116 → 71 s (1.64×), patch
+1,944 B (+0.084 %; `PRESAGE_SIZE_PROXY=exact` restores exact ranking at
~100 s and is 1,432 B *smaller* than the old baseline — #2 fixed a real
overpricing). zstd chosen over brotli-q5 as proxy: identical patches on
Chrome, but q5 mis-ranks a modal-vs-exact margin in TestCorrectionShapes.
#4 measured (whole-region trial = 10.2 s post-#1, split wins by 26 %) and
left for a rule; remaining serial floor is elfmod analysis + eqmatch
(#6/#7). One unlisted lever found: modalPack(split) still brotli-11s
sub-streams to build usually-discarded candidates, inside that 10.2 s.
