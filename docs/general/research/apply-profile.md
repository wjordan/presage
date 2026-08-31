# `presage patch` apply profile — Chrome 151.169 → 151.173

Measurement worktree at `e680ade`. Scratch instrumentation only (`internal/ptime`,
timers in `codec.go`/`split.go`/`elfmod.go`/`structure.go`/`derived.go`, a
`-cpuprofile` flag on `patch`, and `delta/chunkprobe_scratch_test.go`). Nothing
committed.

Corpus: `chrome-151.0.7922.169` (291,290,440 B) → `chrome-151.0.7922.173`
(291,196,232 B). Patch built once with `-symbols`: **2,291,929 B in 58.5 s**
(89.6 user + 5.3 sys CPU-s, 4.9 GB peak RSS). Machine: 24 cores, 91 GB RAM,
page cache warm for every timed run.

---

## 1. Headline numbers

Three timed apply runs (unmodified binary, `/usr/bin/time -v`):

| run | apply wall | user CPU | sys CPU | %CPU | peak RSS |
|---|---|---|---|---|---|
| 1 | 6.52 s | 15.67 | 1.78 | 253% | 4.19 GB |
| 2 | 6.39 s | 15.69 | 1.84 | 259% | 4.22 GB |
| 3 | 7.19 s | 16.43 | 1.89 | 243% | 4.14 GB |

Median **6.5 s apply / 6.6 s process, ~17.5 CPU-s, 2.5× parallelism, 4.2 GB RSS.**
(The task brief's "3.9×" is not what this build does; measured fan-out is 2.5×.)

### Parallelism curve — it saturates at 12 cores for 1.75× total

| GOMAXPROCS | apply wall | user CPU | sys | %CPU |
|---|---|---|---|---|
| 1 | 10.88 s | 9.65 | 1.60 | 100% |
| 2 | 8.77 s | 10.44 | 1.71 | 133% |
| 4 | 7.12 s | 10.27 | 1.73 | 160% |
| 8 | 6.45 s | 11.50 | 1.59 | 193% |
| 12 | 6.22 s | 12.49 | 1.56 | 216% |
| 24 | 6.23 s | 15.71 | 1.72 | 265% |

Speedup ceiling **1.75×**. Solving Amdahl at P=12 gives a **serial fraction of
s ≈ 0.51**: about **5.7 s of the 6.4 s wall is a strictly serial critical path**,
and cores 13–24 buy nothing but 3 extra CPU-seconds of contention.

**This is the whole story of the profile.** Apply is not compute-bound in the
sense that would reward more cores; it is a long chain of single-threaded
sections with four well-parallelized bursts embedded in it.

### GC is not a factor

`GODEBUG=gctrace=1`: **9 GC cycles**, each 0.15–10 ms clock, every one reported
at **0%** of runtime; total GC clock ≈ 35 ms out of 6.4 s. Live heap peaks at
2.95 GB. `GOGC=off` makes things *worse* (8.31 s wall, 8.66 GB RSS) because
first-touch page faults on fresh memory cost more than the collector does;
`GOGC=400` is also worse (8.28 s, 5.64 GB). The extra CPU at high GOMAXPROCS is
parallel-walk overhead, not GC. **Do not spend effort on GC tuning.**

---

## 2. Definitive phase breakdown

Wall-clock, from `PRESAGE_TIME=1` timers. Indented rows are contained in their
parent. P=1 column is the same instrumented binary under `GOMAXPROCS=1`; the
speedup column is what identifies the serial sections.

| phase | P=24 wall | P=1 wall | speedup | verdict |
|---|---|---|---|---|
| **process total** | **6.62 s** | **11.75 s** | 1.78× | |
| `io.readOld` (ReadFile, 291 MB) | 0.100 | 0.095 | 1.0× | serial |
| **`Apply` total** | **6.42** | **11.55** | 1.80× | |
| `decomp.readBody` (brotli/zstd, 4.79 MB body) | 0.026 | 0.025 | 1.0× | negligible |
| `hash.refBlake3` (291 MB) | 0.072 | 0.061 | 1.0× | serial |
| **1. `Materialise` (prediction rebuild)** | **3.85** | **8.80** | **2.29×** | |
| &nbsp;&nbsp;`1a.unmarshalPlan` | 1.82 | 3.62 | 1.99× | **largest phase** |
| &nbsp;&nbsp;&nbsp;&nbsp;`P1.readPointsAndRanges` | 1.16–1.30 | 2.34 | 1.8× | |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;`W1.WalkBodies` (217.6 MB old bodies) | 0.151 | 1.122 | **7.4×** | parallel |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;`W1b.gatherRefs` (12.80 M targets) | 0.170 | — | 1.0× | **serial** |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;`W1c.sortTargets` (12.80 M → 6.10 M uniq) | 0.455 | — | 1.0× | **serial** |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;`K1.cacheKeyHash` (blake3 over 217 MB .text) | 0.062 | 0.062 | 1.0× | **serial, pure waste** |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;point/range replay remainder | ~0.32 | ~0.33 | 1.0× | **serial** |
| &nbsp;&nbsp;&nbsp;&nbsp;`P2.readDerivedMaps` | 0.79–0.86 | 1.54 | 1.8× | |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;`E0.deriveEnumeration` | 0.416 | 1.152 | 2.8× | |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;`W2.callTargets` sweep (225.7 MB) | 0.254 | 0.984 | 3.9× | parallel (27×8 MB chunks) |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;`E2.relaWindowTargets` (.rela.dyn parse) | 0.088 | 0.093 | 1.0× | **serial** |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;`E3.detectBoundaries` | 0.033 | 0.034 | 1.0× | **serial** |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;`E4.unionSorted` | 0.041 | 0.042 | 1.0× | **serial** |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;`K2.cacheKeyHash` (blake3 over 291 MB file) | 0.062 | 0.062 | 1.0× | **serial, pure waste** |
| &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;`derivedUnits`/`reconstructMaps` remainder | ~0.32 | ~0.27 | 1.0× | **serial** |
| &nbsp;&nbsp;`1i.applyFieldFix` | 0.78 | 1.81 | 2.3× | walk parallel, `remapDomain` serial |
| &nbsp;&nbsp;`1e.applyReloc` | 0.35 | 0.30 | 1.0× | **serial** |
| &nbsp;&nbsp;`1d.newOracleParts` | 0.242 | 0.243 | 1.0× | **serial** |
| &nbsp;&nbsp;`1h.predictDecoded` (→ `x86.Relocate`) | 0.208 | 1.280 | **6.2×** | parallel |
| &nbsp;&nbsp;`1c.layImage` (291 MB fill + run copies) | 0.157 | 0.169 | 1.1× | **serial** |
| &nbsp;&nbsp;`1g.retargetEquivalencePrediction` | 0.120 | 1.222 | **10.2×** | parallel |
| &nbsp;&nbsp;`1b.decodeEquivalences` | 0.008 | 0.015 | — | negligible |
| `hash.predBlake3` (291 MB) | 0.061 | 0.066 | 1.0× | serial |
| **2. `applyResidual`** | **2.22** | **2.39** | **1.08×** | **essentially all serial** |
| &nbsp;&nbsp;`3a.CMDecode` (6 streams, 1.50 MB) | 1.33–1.63 | 1.60 | 1.2× | **serial** |
| &nbsp;&nbsp;`4.dispContextBuild` (2nd full plan parse) | 0.33–0.42 | 0.28 | 1.0× | **serial, duplicated work** |
| &nbsp;&nbsp;`5.CMColumnarSides` (classify, 225.7 MB span) | 0.184 | 0.186 | 1.0× | **serial** |
| &nbsp;&nbsp;`6c.ApplyColumnarDisp` (refill walk) | 0.153 | 0.170 | 1.0× | **serial** |
| &nbsp;&nbsp;`pred → out` copy (`module.go:204`) | ~0.11 | ~0.11 | 1.0× | **serial** |
| &nbsp;&nbsp;`4b.dispRestrict` | 0.018 | 0.010 | — | |
| &nbsp;&nbsp;`3b.czDecompress` (23 streams, 768 KB) | 0.004 | 0.005 | — | negligible |
| &nbsp;&nbsp;`6a.ApplyFlaggedCorrection` (11 pieces, 39 MB) | 0.005 | 0.005 | — | negligible |
| `copy.outAppend` (291 MB) | 0.108 | 0.114 | 1.0× | **serial** |
| `hash.outBlake3` (291 MB) | 0.061 | 0.064 | 1.0× | serial |
| `copy.writeToBuf` (291 MB into `bytes.Buffer`) | 0.024 | 0.026 | 1.0× | serial |
| `io.writeOut` (291 MB) | 0.100 | 0.108 | 1.0× | serial |

Answering the brief's numbered items directly:

1. **Prediction rebuild — 3.85 s, 60% of wall.** Of this, `retargetEquivalencePrediction`
   (0.12 s) and `predictDecoded`/`x86.Relocate` (0.21 s) are the *cheapest* parts —
   they parallelize 10.2× and 6.2×. The cost is `unmarshalPlan` (1.82 s), which is
   plan replay, sorting and cache-key hashing, not prediction.
2. **Derived-map enumeration sweep + `.rela.dyn` parse — 0.42 s** (`E0`), of which
   the parallel `callTargets` sweep is 0.254 s and the serial `.rela.dyn` parse is
   0.088 s. `.rela.dyn` is parsed up to **3× per `Materialise`** (twice inside
   `applyReloc` via `relocGapColumn`/`relocTailColumn`, once as E2).
3. **CM decode — 1.33 s serial for 1,504,258 decoded bytes = 1.13 MB/s.** 6 streams,
   all in the one big `pieceColumnarDisp` piece, decoded one after another in
   `split.go:279-296`. Per-stream (measured):

   | stream j | raw bytes | decode | MB/s |
   |---|---|---|---|
   | 06 | 830,232 | **1.079 s** | 0.73 |
   | 02 | 218,511 | 0.383 s | 0.54 |
   | 01 | 322,392 | 0.098 s | 3.15 |
   | 03 | 63,356 | 0.042 s | 1.44 |
   | 04 | 26,742 | 0.015 s | 1.69 |
   | 07 | 43,025 | 0.010 s | 3.94 |

   Extremely skewed: one stream is 66% of CM time.
4. **Field-context `classify` walk — 0.184 s** (`CMColumnarSides` → `DispContext.classify`,
   `dispfield.go:167`). Cheaper than expected because the body-skip test means it
   only decodes bodies a changed run touches; only 0.16 CPU-s in the profile.
5. **Displacement-column refill walk — 0.153 s** (`ApplyColumnarDisp` tail, which
   re-runs `WalkReferences` over the repaired buffer via `DispContext.sites`).
6. **brotli/zstd — 0.026 s.** Body is 4.79 MB uncompressed. Irrelevant.
7. **BLAKE3 — 0.194 s** in three full-image passes (ref 0.072, pred 0.061, out 0.061)
   at ~4.3 GB/s single-threaded, plus **0.124 s of memoisation cache-key hashing**
   (`K1`+`K2`) that exists only for the encoder. Total 0.32 s of hashing.
8. **I/O — 0.20 s.** The CLI uses `os.ReadFile` (`main.go:181`), **not mmap**;
   with a warm page cache the 291 MB read is 0.100 s at 2.8 GB/s and the write
   0.100 s. Not a bottleneck, but see L6 — it forces the copy chain.
9. **GC/alloc — negligible for GC (35 ms), significant for copies.** Five live
   291 MB buffers on the apply path:
   1. `old` — `os.ReadFile` (`main.go:181`)
   2. `pred` — `layImage`'s `out := make([]byte, ep.NewLen)` (`elfmod.go:320`)
   3. `applyResidual`'s `out := append([]byte(nil), pred...)` (`module.go:204`) — a
      defensive full copy
   4. `Apply`'s `out := make([]byte, 0, size)` + `out = append(out, bytes...)`
      (`codec.go:210`, `:236`) — a second full copy
   5. `main.go`'s `var buf bytes.Buffer` + `w.Write(out)` (`main.go:190-194`) — a
      third full copy

   That is 1.46 GB of full-image buffers and **three redundant full-image copies**
   (0.11 + 0.11 + 0.024 = 0.24 s). Measured `runtime.memmove` = 1.40 CPU-s (8.2% of
   all CPU). The rest of the 4.2 GB RSS is walk output: the reference-target list
   alone is 12.80 M × 8 B = 102 MB before dedup, plus per-body reference slices
   (`WalkBodies.func1.1` → `growslice` is 1.58 CPU-s).

### CPU profile cross-check (17.02 CPU-s of samples over 6.35 s)

| function | flat | cum | % of CPU |
|---|---|---|---|
| `x86.fastStep` | 3.73 | 3.77 | 22% |
| `x86.WalkReferences` | 0.99 | **7.98** | **47%** |
| `x86.step` | 0.87 | 5.51 | 32% |
| `runtime.memmove` | 1.40 | 1.40 | 8% |
| `x86.Relocate` | 0.32 | 2.14 | 13% |
| `runtime.growslice` | 0.06 | 2.46 | 14% |
| `cmCoder.code` + `cmBank.update` | 0.96 | 1.16 | 7% |
| `slices.partitionOrdered[uint64]` (target sort) | 0.71 | 0.84 | 5% |
| `blake3 hash_avx2.HashF` | 0.38 | 0.38 | 2% |

`WalkReferences` callers: `WalkBodies.func1` 4.20 (53%), `retargetEquivalencePrediction`
2.25 (28%), `callTargets` 1.42 (18%), `DispContext.sites` 0.11 (1%). Adding
`Relocate`'s own decoding gives **~10.1 of 17.0 CPU-s (59%) spent decoding x86
instruction lengths.**

---

## 3. How many times apply walks the same bytes

Six x86 walks per `Materialise`, over only **two distinct byte images**:

| walk | over | bytes | wall @P=24 | CPU |
|---|---|---|---|---|
| `referenceTargets` → `WalkBodies` | **old** .text mapped bodies | 217.6 MB | 0.151 | 4.20* |
| `callTargets` (E1 sweep) | **old** .text, whole window, linear | 225.7 MB | 0.254 | 1.42 |
| `predictDecoded` → `x86.Relocate` | **old** .text mapped `Copy` bodies | ~200 MB | 0.208 | 2.14 |
| `retargetEquivalencePrediction` | **new** predicted window (bodies + gaps) | 225.7 MB | 0.120 | 2.25 |
| `applyFieldFix` → `fieldSites` | **new** predicted .text mapped bodies | ~217 MB | (in 0.78) | * |
| `DispContext.classify` + `sites` | **new** repaired buffer, touched bodies | subset | 0.34 | 0.27 |

\* `WalkBodies`'s 4.20 CPU-s is shared between `referenceTargets` and `fieldSites`.

**~880 MB of instruction-length decoding over ~450 MB of distinct bytes — roughly
2× redundant, and 4 of the 6 walks recover the same instruction boundaries.**
`retargetEquivalencePrediction` and `applyFieldFix` only rewrite displacement
*bytes*; they never change instruction *lengths*. So the boundary map of the new
.text is invariant across the whole tail of the pipeline and is being recomputed
three times. Same for the old .text across `referenceTargets` and `Relocate`.

---

## 4. Chunked CM — measured, not estimated

The 6 real CM streams were dumped from a live apply and re-encoded whole vs. in
K equal chunks with a full model reset per chunk (`delta/chunkprobe_scratch_test.go`),
then decoded with all chunks concurrent.

| stream | n | whole | bpb | serial decode | K=4 size | K=8 size | K=8 par decode |
|---|---|---|---|---|---|---|---|
| 06 | 830,232 | 341,077 | 3.29 | 1.155 s | +14,918 (+4.4%) | +24,349 (+7.1%) | **169 ms (6.8×)** |
| 02 | 218,511 | 86,945 | 3.18 | 0.488 s | +4,032 (+4.6%) | +6,494 (+7.5%) | 26 ms (18.7×) |
| 01 | 322,392 | 77,774 | 1.93 | 0.102 s | +222 (+0.3%) | +438 (+0.6%) | 22 ms (4.6×) |
| 03 | 63,356 | 32,705 | 4.13 | 0.042 s | +1,792 (+5.5%) | +2,883 (+8.8%) | 10 ms |
| 04 | 26,742 | 17,250 | 5.16 | 0.016 s | +894 (+5.2%) | — | 7 ms |
| 07 | 43,025 | 4,700 | 0.87 | 0.010 s | +34 (+0.7%) | +69 (+1.5%) | 3 ms |
| **total** | 1,504,258 | **560,451** | | **1.81 s** | **+21,892 (+0.96% of patch)** | **+35,127 (+1.53% of patch)** | |

Model-restart cost is roughly linear in chunk count and scales with the stream's
entropy: ~2.4 KB per restart on stream 06 (3.29 bpb), ~13 B per restart on stream
07 (0.87 bpb). High-entropy streams pay; low-entropy ones are nearly free.

**The tuned mix**: stream 06 at K=8 (169 ms, +24.3 KB), stream 02 at K=4 (69 ms,
+4.0 KB), everything else whole, all decoded concurrently. Wall = the slowest
chunk ≈ **170 ms**, cost **+28.3 KB = +1.23% of the 2.29 MB patch**.

Just running the 6 streams concurrently *without* chunking costs zero bytes but
gives only 1.33 s → 1.08 s, because stream 06 alone is 1.08 s. **Chunking is the
only way CM fits a 1 s budget.**

---

## 5. Ranked lever list

Wall savings are against the 6.62 s process baseline and are non-overlapping.

| # | lever | measured evidence | est. wall saving | size cost | format change? |
|---|---|---|---|---|---|
| **L1** | **Chunked + concurrent CM decode** | 6 streams, 1.33 s serial, 66% in one stream; chunk probe above | **−1.16 s** (1.33 → 0.17) | **+28.3 KB, +1.23%** | **yes** (chunk table per stream) |
| L1′ | concurrent streams only, no chunking | max stream 1.08 s | −0.25 s | none | no |
| **L2** | **Parallelize the serial elfmod stragglers**: `applyReloc` (0.35), `newOracleParts` (0.24), `remapDomain` in `applyFieldFix` (~0.35), `layImage` (0.157), `relaWindowTargets` (0.088), `detectBoundaries` (0.033), `unionSorted` (0.041) | all measured at speedup 1.0×; all are per-entry independent loops over `.rela.dyn` / equivalence runs / a stride-8 scan | **−0.90 s** | none | no |
| **L3** | **Parallel sort of the reference-target domain** | 12.80 M uint64 gathered 0.170 s + sorted 0.455 s, both serial; `slices.partitionOrdered` is 0.84 CPU-s | **−0.45 s** | none | no |
| **L4** | **Drop the duplicate plan parse for `DispContext`** | `4.dispContextBuild` = 0.33–0.42 s; it re-runs `parsePlanStreams`+`planMaps`+`unmarshalPlan` (n=2 on every `P1`/`P2`/`K1`/`K2` timer) purely to rebuild `bodies`/`starts` that `Materialise` already computed | **−0.35 s** | none | no (plumbing: return `structures` from `Materialise`) |
| **L5** | **Shared instruction-boundary index per image** | 6 walks over 2 images, ~880 MB decoded for ~450 MB distinct; 10.1 of 17.0 CPU-s; retarget/fieldfix/classify/refill never change instruction lengths | **−0.35 s wall, −5 CPU-s** | none (28 MB in-memory bitmap) | no, except E1 (see note) |
| **L6** | **Buffer discipline**: mmap `old`, materialise into the final output buffer, mutate in place, write via `os.File` not `bytes.Buffer` | 5 live 291 MB buffers; 3 redundant full copies measured at 0.108 + ~0.11 + 0.024; `memmove` = 1.40 CPU-s; `readOld` 0.100 s | **−0.34 s, −900 MB RSS** | none | no |
| **L7** | **Drop the encoder-only memoisation cache-key hashes at decode** | `K1` 0.123 s + `K2` 0.123 s across 2 calls; these blake3 passes over 217 MB and 291 MB exist so the *encoder*'s many predictions share a result — the decoder predicts once and never hits the cache | **−0.12 s** (−0.25 s without L4) | none | no |
| **L8** | **Parallel/tree-mode BLAKE3 for the three verification hashes** | 0.194 s at 4.3 GB/s single-threaded over 3×291 MB | **−0.16 s** | none | no |
| **L9** | **Parallelize `classify` + `sites` per body** | `CMColumnarSides` 0.184 + `ApplyColumnarDisp` 0.153, both 1.0× speedup; each body writes disjoint byte ranges of `sides[]`, `d.bodies`/`d.starts` read-only — no shared mutable state | **−0.25 s** | none | no |
| **L10** | **Shard the plan replay** (`readPointsAndRanges` remainder ~0.32, `derivedUnits`/`reconstructMaps` ~0.32) | both 1.0× speedup; they are serial cursor decodes of a single delta stream | **−0.54 s** | ~+0.5–1% (lost cross-shard context, by analogy with L1) | **yes** (K independent plan shards with self-contained bases) |
| L11 | prealloc walk output slices | `growslice` 2.46 CPU-s, 1.58 in `WalkBodies.func1.1`; preallocating the gather slice alone already moved `P1` from 1.30 → 1.16 s in this session | −0.14 s (already counted in L3's baseline) | none | no |
| — | GC tuning | 9 cycles, 35 ms, 0%; `GOGC=off` is 1.9 s *worse* | **0** | — | — |
| — | brotli/zstd body | 0.026 s | 0 | — | — |

**Note on L5 and E1**: `callTargets` is defined as a *linear* 8 MiB-chunked sweep,
deliberately, so both sides desynchronise identically inside data islands
(`derived.go:56-60`). A body-anchored boundary index decodes different bytes, so
E1 cannot share it without redefining the enumeration — a format change. The other
three walks (retarget, fieldfix, classify/refill) share the same anchoring and can
reuse the index freely.

### On the parallelism ceiling (brief item a)

What bounds apply at its measured 2.5×/1.75× is **not** fan-out constants, **not**
GC, and **not** memory bandwidth. It is that the four phases which *are* parallel
(`retargetEquivalencePrediction` 10.2×, `predictDecoded` 6.2×, `WalkBodies` 7.4×,
`callTargets` 3.9×) already cost only **0.73 s of the 6.4 s wall combined**.
Everything else — 5.7 s — runs on one core. Amdahl on the current shape gives an
asymptotic floor of 5.7 s no matter how many cores you add, which is exactly what
the P=12 → P=24 flatline shows.

The x86 walks are 59% of *CPU* and 11% of *wall*. Optimizing the walker's constant
factor is therefore the wrong target; the walks are already the healthiest part of
the pipeline. `callTargets` at 3.9× is the one under-parallelized walk — its 8 MiB
chunking gives 27 chunks over 24 cores, so the tail chunk dominates; a finer chunk
size would recover ~0.15 s, but that changes E1's decode alignment, hence the
format-change flag.

---

## 6. Composed best case

| step | wall |
|---|---|
| baseline (process) | **6.62 s** |
| − L1 chunked+concurrent CM | −1.16 |
| − L2 parallelize serial elfmod stragglers | −0.90 |
| − L3 parallel target sort/gather | −0.45 |
| − L4 kill duplicate plan parse | −0.35 |
| − L5 shared boundary index | −0.35 |
| − L6 buffer discipline + mmap | −0.34 |
| − L9 parallel classify/refill | −0.25 |
| − L8 parallel BLAKE3 | −0.16 |
| − L7 drop decode-side cache-key hashes | −0.12 |
| **subtotal — no format change, no patch bytes** | **≈ 2.6 s** |
| − L10 sharded plan replay (**+0.5–1% patch**) | −0.54 |
| − finer E1 chunking (**format change**) | −0.15 |
| **with both format changes** | **≈ 1.9 s** |

Composition risk is real: L2/L3/L9 all assume near-linear scaling of work that has
never been run in parallel, and the 24-core flatline plus the `GOGC=off` result say
this machine punishes wide first-touch allocation. Haircut the parallel levers by
30% and the honest landing zone is **≈ 3.0 s without format changes, ≈ 2.3 s with
them**.

## 7. Verdict on <1 s

**Not reachable on this codebase's decode architecture, and not reachable by
tuning.** The honest floor is **≈ 2 s**, and getting even there costs two format
changes and roughly +2% patch size.

The arithmetic that settles it: total work at P=1 is **11.25 CPU-s**. Removing the
redundant work the profile identifies (2× walk redundancy, three full-image copies,
the duplicate plan parse, the encoder-only cache hashes) takes that to roughly
**8 CPU-s**. A sub-1-second wall therefore requires an *average* parallel
efficiency of **>8×** across **every** phase of apply — including phases that are
today serial single-cursor stream decodes (`reconstructMaps`, `readPointsAndRanges`,
`applyCorrection`) and one phase, CM decode, whose parallelism can only be bought
with patch bytes.

What dominates the floor, in order:

1. **CM decode, ~0.17 s even fully chunked.** At 1.13 MB/s measured (≈1000 cycles
   per byte: ~376 multiplies per byte across 8 binary-coder steps with 5–9 mixer
   inputs each, plus hashed-bank random access), the coder cannot be made fast; it
   can only be made wide, and width costs bytes. Pushing below 0.17 s means K=16 or
   K=32 on the big stream, which the probe prices at +10.8% and +14.9% of that
   stream — the size curve turns bad exactly where the speed curve flattens
   (K=16 → K=32 buys 13 ms for another 14 KB).
2. **Serial plan replay, ~0.64 s.** `reconstructMaps` and the point/range decode
   are single-cursor delta replays. Parallelizing them is a format change (L10),
   and the shard boundaries cost bytes.
3. **x86 instruction decoding, ~0.35 s even fused.** 450 MB of distinct code must
   be length-decoded at least twice (old and new image). This is the one cost that
   is genuinely irreducible short of shipping a boundary map in the patch — which
   would cost far more bytes than it saves.
4. **Whole-image memory traffic, ~0.25 s.** Even with every redundant copy removed,
   apply must touch 291 MB for `layImage`, once for the correction, once for the
   output hash, and once for the write. At the measured ~2.5–4.5 GB/s per pass
   that is a hard ~0.25 s, and it is the part that stays serial-ish because it is
   bandwidth-bound, not compute-bound.

If <1 s is a hard product requirement, the lever that is missing from this list is
architectural rather than optimizational: **make the patch decodable in independent
shards end to end** — region-parallel `Materialise`, per-shard plan streams, per-shard
CM streams — so apply becomes `n_shards` independent single-threaded pipelines with
a final concatenation. `codec.go:211` already loops over regions; it just does so
serially, and elfmod's `Cuts` already emits a region per ≥1 MiB section. Making that
loop concurrent, and cutting far more aggressively, is the only route to a wall that
scales with cores. The price is the sum of all the cross-shard context the coder
currently exploits — on this evidence, somewhere between 2% and 5% of patch size.

---

## Appendix: reproduction

```
go build -o presage ./cmd/presage
presage diff -symbols $C/symbols-151.0.7922.169/debug-info/chrome.debug,\
  $C/symbols-151.0.7922.173/debug-info/chrome.debug \
  $C/chrome-151.0.7922.169 $C/chrome-151.0.7922.173 -o p.presage
PRESAGE_TIME=1 presage patch $C/chrome-151.0.7922.169 p.presage -o out.bin -cpuprofile apply.cpu
PRESAGE_DUMPCM=./cm PRESAGE_TIME=1 presage patch ... # dump CM streams
go test ./delta -run TestChunkedCMProbe -v                # chunking size/speed probe
```

Logs: `ap/base.log` (timings + GOMAXPROCS sweep), `ap/ptime24.log` (P=24 and P=1
phase tables), `ap/gc.log` (gctrace, GOGC=off/400), `ap/streams.log` (per-CM-stream),
`ap/chunk.log` (chunking probe), `ap/apply.cpu` / `ap/apply1.cpu` (CPU profiles).
