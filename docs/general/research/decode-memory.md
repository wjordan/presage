# Decoder memory: profile, reductions, and the next ceiling

Measurement date: 2026-08-31. Code baseline: `2c0bd70`, after the existing
parallel-apply work (mmap reference, in-place residual, direct file output,
parallel plan replay and residual streams). Machine: Ryzen 9 7900X, 24 logical
CPUs, Go 1.27.0, warm page cache.

## 1. Result

The main remaining problem was not the output-sized prediction itself. The x86
walks first retained every decoded reference as a 40-byte `x86.Reference`, then
converted the table to the 8-byte target domain or the 16-byte field-site
table the caller actually needed. On Chrome that temporary representation
accounted for 2.70 GiB of cumulative allocation.

Fusing the walk with the conversion and collecting into fixed-size chunks is
the effective first fix. It is wire-compatible and makes the apply faster as
well as smaller in memory. Building each residual piece's restricted
displacement context once removes a further 253 MB of cumulative allocation,
although that second change does not move peak RSS.

| pair | target | baseline RSS | fused/chunked RSS | change | baseline wall | new wall |
|---|---:|---:|---:|---:|---:|---:|
| Chrome 151.169 → .173 | 291.2 MB | 3,090 MB | 1,724 MB | **−44.2%** | 3.57 s | 3.25 s |
| libxul 154.0 → .1 | 185.6 MB | 1,776 MB | 945 MB | **−46.8%** | 3.70 s | 3.52 s |

Values are medians of five fresh processes for Chrome and three for libxul,
measured by `/usr/bin/time`. Every run applied the cached production patch and
compared byte-for-byte with the target. Patch bytes are unchanged.

A one-shot allocation benchmark (`BenchmarkApplyCorpus`) gives the same
explanation from another angle:

| Chrome apply | bytes allocated | allocation count |
|---|---:|---:|
| baseline | 6,688,877,344 | 9,705,543 |
| fused walk + one restricted context | 3,521,824,096 | 3,183,695 |
| change | **−47.3%** | **−67.2%** |

## 1.1 Second pass: remove lifetime overlap

A second profile started from the fused collector above and sampled the CLI
at its RSS high-water mark. The main goroutine was no longer in prediction: it
was waiting for residual streams. Prediction allocations that were dead by
then still occupied heap pages, while the output, displacement context, CM
sides and CM banks were live. The effective changes were therefore the ones
that either removed a whole prediction temporary or avoided earlier churn:

- replay structural choices directly into the final image instead of building
  a 215.2 MiB window-sized structural prediction and copying selected bodies;
- store field sites in one 64-bit word instead of two machine-width integers;
- sort small mapping indices and project x86 bodies lazily instead of cloning
  40-byte mappings and retaining separate body tables;
- transfer ownership of the displacement table and make piece restrictions
  zero-copy views;
- preflight columnar run geometry so CM sides have exact capacities, reducing
  `CMColumnarSides` allocation from 66.2 MiB to 12.6 MiB; and
- XOR CM probabilities with their neutral midpoint, leaving untouched model
  pages OS-zero while preserving the exact arithmetic-coder state.

All are patch-compatible. Cached patches produced before the changes applied
byte-for-byte on Chrome, libxul and Prometheus.

| pair | first-pass RSS | second-pass, no limit | CLI default | first-pass wall | CLI wall |
|---|---:|---:|---:|---:|---:|
| Chrome 151.169 → .173 | 1,724 MB | 1,245 MB | **934 MB** | 3.25 s | **3.06 s** |
| libxul 154.0 → .1 | 945 MB | 800 MB | **620 MB** | 3.52 s | **3.42 s** |
| Prometheus 3.13.1 → .2 | 600 MB | 600 MB | **400 MB** | 0.80 s | **0.79 s** |

At that pass the CLI column included an automatic soft limit of 2.2× target size, with
a 256 MiB minimum. An explicit `GOMEMLIMIT` takes precedence. The library does
not change a process-global runtime setting; without a limit, the code changes
alone reduce Chrome another 27.8% from the first pass.

The allocation benchmark now reports 2,574,672,864 B/op and 2,801,175
allocations/op. Against the original decoder that is 61.5% fewer allocated
bytes and 71.1% fewer allocations; against the first pass it is a further
26.9% reduction in bytes.

## 1.2 Third pass: remove the domain sort's scatter copy

Sampling the second-pass CLI at its new high-water mark found the main
goroutine in the field repair's address-domain sort. The domain already had
one exact contiguous entry per field, but the general shard sorter allocated
a second full-size scatter array and then a compact result while the output
image and field-site table were live.

The decoder now fills the exact domain concurrently and sorts it in place. A
sampled partition divides it into disjoint value ranges for parallel sorting;
badly skewed or duplicate-heavy partitions fall back to Go's serial sort. The
deduplicated result remains in the domain's own backing array. This preserves
the plan basis and patch format.

On Chrome, ordinary (non-allocator-outlier) CLI runs moved from about 934 MiB
to 916 MiB maximum RSS, with wall time unchanged around 3.0 seconds. Without a
memory limit, median maximum RSS moved from roughly 1,245 MiB to 1,214 MiB.
The one-shot allocation benchmark moved from 2,574,672,864 to 2,480,408,488
B/op, 94.3 MB less allocation. Cached patches still apply byte-for-byte on
Chrome, libxul and Prometheus. libxul remains near 620 MiB and Prometheus near
400 MiB because another phase already sets their high-water marks.

## 1.3 Fourth pass: bound clean reference residency by phase

The remaining large jump was outside the Go heap. The Linux CLI maps the old
file read-only, and a Chrome prediction eventually faults all 291 MB of it
resident. Later phases reread very different subsets, but the kernel kept all
of the earlier clean pages mapped while the destination and prediction tables
grew.

The ELF decoder now accepts an optional residency callback from the mapping
owner. The Linux CLI implements it with `MADV_DONTNEED`; byte-slice library
callers and non-Linux builds leave it nil. Whole-image layout evicts the clean
reference before it starts and after each 16 MiB of equivalence copies.
Structural replay works in 64K-map batches and evicts between them. A later
phase faults back only the file ranges it actually reads. The mapping is never
unmapped or modified, so patch validation and output are unchanged.

Decoder-only enumeration and target caches are also removed once their
finished structural plans own everything still needed. A collection before
destination allocation makes those pages reusable rather than carrying them
into the next phase. Entries are removed only for the current reference; this
does not clear an unrelated concurrent decoder's cache keys.

| pair | prior max RSS | phase-bounded max RSS | change | prior wall | new wall |
|---|---:|---:|---:|---:|---:|
| Chrome 151.169 → .173 | ~916,000 KiB | **722,064 KiB** median | **−21.2%** | ~3.0 s | **2.88 s** |
| libxul 154.0 → .1 | ~620,000 KiB | **535,768 KiB** median | **−13.6%** | ~3.4 s | **3.39 s** |
| Prometheus 3.13.1 → .2 | ~400,000 KiB | **~400,000 KiB** | none | ~0.79 s | **~0.79 s** |

Chrome is the median of seven fresh processes and libxul of five, all with a
warm page cache; every output compared byte-for-byte with the cached target.
Chrome runs ranged from 694,112 to 727,256 KiB. The result is deliberately a
CLI optimisation: applying from an ordinary heap buffer must not use
`MADV_DONTNEED`, which can discard anonymous data. Refaulting is cheap with a
warm cache and measured neutral here, but storage latency is the principal
risk on a cold or heavily pressured system.

## 1.4 Fifth pass: change the speed/size model

The Firefox profile exposed a different high-water mark. Context-mixing
tables accounted for 665 MiB of cumulative apply allocation and 160 MiB still
resident at the final allocation snapshot. On encode, Brotli's quality-10/11
optimal parser dominated CPU and its working set, while minimum-sized LZ
indices over many tiny correction regions accumulated 44 GiB of allocation.

Container v5 adds a compact CM codec rather than silently changing the old
arithmetic model. Its hashed banks are quarter-sized; prediction-conditioned
streams omit two generic history banks that duplicated the prediction and
field contexts. Plan columns use the same compact codec, while codecs written
by the old model remain decodable. The encoder uses CM only when it saves at
least 2 KiB and 10% on a stream. Brotli quality 9 avoids the optimal parser;
quality 8 measured the same time with a slightly larger patch. Short LZ
regions use an exact bounded scan instead of allocating the minimum hash
table. The apply CLI heap target moves to 1.5× target size with a 256 MiB
floor. The encoder now also bounds heap growth at the measured whole-image
matcher knee, while leaving an explicit `GOMEMLIMIT` in control.

| pair | prior patch | v5 patch | prior encode | v5 encode | prior apply | v5 apply |
|---|---:|---:|---:|---:|---:|---:|
| Chrome 151.169 → .173 | 2,256,358 | 2,376,189 | — | **31.56 s / 2,811,180 KiB** | 2.88 s / 722,064 KiB | **2.52 s / 674,632 KiB** |
| libxul 154.0 → .1 | 2,665,810 | 2,866,328 | 55.37 s / 4,423,164 KiB | **30.44 s / 1,801,300 KiB** | 3.48 s / 525,904 KiB | **1.57 s / 395,400 KiB** |
| Prometheus 3.13.1 → .2 | 69,933 | 74,636 | 5.16 s / 1,049,492 KiB | **4.26 s / 970,480 KiB** | 0.81 s / 400,968 KiB | **0.75 s / 401,164 KiB** |

Times and RSS are three-run medians. Every apply was hash-verified internally;
Firefox and Prometheus were
also compared byte-for-byte with their targets. Firefox's patch grows 7.5%,
but remains 3.3× smaller than the next-smallest comparison. Its apply RSS is
now within 1.2 MiB of Zucchini + XZ, while encode is faster than zstd's patch
mode. The bounded small-LZ path alone saved about 117 MiB of Firefox encode
RSS but no wall time. Bounding retained heap at the matcher's live-set knee
then nearly halved encode RSS without changing the patch or wall time.

The CLI exposes `-cpuprofile`, `-allocprofile`, and `-traceprofile` on both
commands. They can be combined and are retained as normal performance-analysis
interfaces rather than one-off environment hooks.

## 2. Where the baseline went

The allocation profile's largest rows (`-memprofilerate=1`, one Chrome apply)
were:

| allocator | cumulative allocation | reason |
|---|---:|---|
| `x86.WalkBodies` reference append | 2,695.8 MiB | full 40-byte references, grown independently per body, across repeated walks |
| CM counter banks | 344.4 MiB | several independently decoded residual streams |
| parallel target sort/dedup | 307.9 MiB | raw target array, bucket scatter buffer, compact result |
| prediction output (`layImage`) | 277.7 MiB | the required output-sized buffer |
| restricted displacement contexts | 262.7 MiB | the same body subset built twice per piece |
| temporary structural prediction | 215.2 MiB | selected function bodies before copying into the whole image |
| field-site gather/final table | 362.5 MiB | worker shards plus a contiguous 16-byte/site table |

The GC trace is important to interpretation. Baseline Chrome reached about
1.32 GiB of live heap but 3.07 GiB RSS; after the fused walk it briefly reached
about 1.39 GiB allocated before a collection, only 456 MiB live after that
collection, and 1.72 GiB RSS. Dead backing arrays remain resident long enough
to determine the process peak. Reducing allocation churn therefore matters
more than tuning the final live heap alone.

The fused collector emits only the caller's value and uses 16K-element chunks.
Fixed chunks avoid append's geometric sequence of dead backing arrays; direct
target shards also feed the existing parallel sort without a concatenation.
The field-site path still concatenates once because its wire indices require
one stable order, but no longer retains a larger reference table alongside it.

## 3. Put a soft ceiling on the transient heap

The code change and a Go runtime memory limit compose well:

| pair / setting | peak RSS | wall | RSS / target |
|---|---:|---:|---:|
| Chrome, second-pass code only | 1,245 MB | 3.04 s | 4.3× |
| Chrome, `GOMEMLIMIT=600MiB` | **934 MB** | 3.03 s | **3.2×** |
| libxul, second-pass code only | 800 MB | 3.37 s | 4.3× |
| libxul, `GOMEMLIMIT=400MiB` | **620 MB** | 3.44 s | **3.3×** |
| Prometheus 3.13.1 → .2, code only | 600 MB | 0.79 s | 6.4× |
| Prometheus, `GOMEMLIMIT=256MiB` | **400 MB** | 0.80 s | **4.3×** |

After the second-pass changes, the ELF knee was near `2.2 * target size`.
Chrome at 500 MiB did not reduce RSS below the 600 MiB result and slowed from
3.03 to 3.62 seconds; 400 MiB took 4.22 seconds. The CLI applies the measured
formula automatically unless the environment supplies `GOMEMLIMIT`. The v5
model moves the measured default to 1.5× (§1.4). This is
admission control, not a hard promise: Go defines the limit as soft, and it
excludes the reference's `mmap`. The official GC guide specifically motivates
it with transient heap spikes and warns about thrashing below the live set:
<https://go.dev/doc/gc-guide#Memory_limit>.

`GOGC=50` and `GOGC=25` also lowered Chrome RSS, but with materially more
run-to-run variation. A forced phase-boundary GC saved only another 39 MiB and
would impose a process-wide pause, so it was not retained. `GOMEMLIMIT` is the
more predictable control and maps directly to a caller's actual memory budget.
The dedicated CLI can set it safely; library and service callers retain control
of their process-wide policy. Leave headroom for the mapped reference and
non-Go memory.

## 4. Probes that did not pay

### Pack the Go table window index

Prometheus's largest allocation is `buildWinIndex`: 286 MiB cumulatively for
an 8-byte `(hash, position)` entry at every byte position of several stage-1
tables. A prototype retained only 4-byte positions after construction and
recomputed the hash suffix during lookup. It preserved the prediction exactly
and changed RSS from about 600 to 542 MB (−10%), but changed median apply from
0.80 to 0.90 seconds (+12.5%). Under `GOMEMLIMIT=300MiB`, it saved only about
5% RSS for the same slowdown. Do not make it the default. It remains a
reasonable explicit low-memory mode if a 10–13% CPU trade is acceptable.

### Halve the residual context-model tables alone

Reducing every nontrivial CM bank by one address bit halves that model's
counter memory. A fresh Chrome encode produced a 2,259,357-byte patch, only
2,999 bytes (+0.133%) above the production patch, and apply became faster from
better cache locality. Peak RSS was unchanged at both the default and 750 MiB
limit: those pages were not the phase setting the high-water mark. It became
material only as a distinct wire codec used for plan and residual streams
together, with redundant contexts removed and a lower heap target (§1.4).

### Force the GC below the live-set knee

The then-current knee was 600 MiB managed memory. Below 500 MiB total RSS
stayed near 932 MB while wall time rose sharply. More GC could not remove live
metadata, the mapped reference, or the destination at that revision.

### Reduce concurrency or repeat walks

Serialising CM decoders left Chrome RSS unchanged near 1.30 GB and added about
0.2 seconds. Disabling residual prefetch saved only about 20 MB and regressed
libxul. A two-pass field-site collector removed worker chunks but repeated the
x86 decoder's own allocations; peak RSS rose by roughly 50 MB and apply added
about 0.1 seconds. None was retained.

A dense bitset for in-window reference targets removed most target-sort
allocation but did not change the later residual high-water mark. It also
penalises sparse code windows, so it was reverted rather than adding a more
complex representation with no measured peak benefit.

### Shrink the allocations around the new ceiling

The third pass also tested four smaller lifetime cuts: a 32-bit field-site
representation with a wide fallback, fixed-chunk call-target gathering,
reusing the compacted domain's spare capacity for the merged remap target set,
and starting residual prefetch only after materialisation. They removed up to
about 44 MiB from an individual live set or substantially reduced cumulative
allocation, but none moved maximum RSS on Chrome or libxul. Once one overlap
was removed, the next reached the same ceiling. Delaying prefetch also gave up
useful CPU overlap, so these changes were not retained solely as decoder-peak
optimisations.

At that revision's field-fix/correction boundary, GC traces showed about 607 MiB of
managed memory live. Lowering `GOMEMLIMIT` below that knee leaves RSS unchanged
and increases wall time. A further material reduction therefore needs to
remove an output- or reference-sized resident object, or change when global
metadata is needed; another local table packing is unlikely to show up in the
process peak.

## 5. What prior art says about the next architecture

The original `bspatch` bound is `old + new + O(1)` memory. That is a useful
lower bound for an in-memory API, not merely a competitor's implementation
detail: <https://www.daemonology.net/bsdiff/>.

Zucchini likewise applies into one preallocated destination buffer, and its
apply sources expose `GetNext()` cursors rather than first materialising every
equivalence and delta:

- <https://chromium.googlesource.com/chromium/src/+/08f6e3a26c7b4e37924ffc5fae4c57c0cc17dc44/components/zucchini/zucchini.h#68>
- <https://chromium.googlesource.com/chromium/src/+/37af22b8100dacbcbd9468100e7f2359149e5f6f/chrome/installer/zucchini/zucchini_apply.cc#75>

HDiffPatch makes the more aggressive endpoint explicit: its streaming patch
forms bound memory by block/cache size, and its single-compressed format needs
one decompression buffer. It trades encoder memory and sometimes patch size to
get there: <https://github.com/sisong/HDiffPatch#readme>.

Presage cannot blindly stream prediction bytes in file order: address domains,
function maps, relocation tables and field repair contain genuine global
dependencies. The useful split is global *metadata* followed by local byte
materialisation, not pretending the global dependencies do not exist.

## 6. Ranked next steps

1. **Keep both passes' allocation and lifetime changes.** The fused collector,
   direct structural replay, compact field sites, lazy bodies, displacement
   views and exact CM-side sizing take the code-only Chrome peak from 3.09 GB
   to 1.25 GB with no format change or patch cost, while improving wall time.

2. **Keep apply-memory admission and phase-aware page eviction at the process
   boundary.** The Linux CLI combines `max(256 MiB, 1.5 × target)` managed-memory
   admission with eviction of clean reference pages at reconstruction phase
   boundaries, bringing Chrome to about 675,000 KiB. Library/service callers
   should set their own process policy. Longer term, put a conservative module
   working-set declaration in the patch header and reject work that cannot fit
   before allocating it.

3. **Finish making decode memoisation apply-scoped.** Reconstruction now drops
   the current reference's target, enumeration, and relocation cache entries
   as soon as their phases finish. Parsed maps still live through displacement
   context construction, and package-global identity caches remain the wrong
   lifetime for a long-running decoder. A decoder session should own every
   entry and release it after the residual has consumed its field context.

4. **Add a destination-backed materialiser.** An optional `MaterialiseInto`
   path can build directly into a writable output mapping or transactional
   temporary file, apply the correction there, incrementally hash finalized
   chunks, then atomically publish or copy only after verification. This
   removes the output-sized Go heap object. With chunk-aligned prediction
   hashing and `madvise` after finalized spans, clean file-backed pages need
   not remain resident. Preserve the current `io.Writer` safety contract with
   a temporary file; do not emit unverified bytes to an arbitrary writer.

5. **Cursorize the remaining large derived tables selectively.** The compact
   field-site table is now 66 MiB; a tested two-pass iterator made peak worse,
   so the next attempt should preserve the x86 walk and consume chunks by
   cursor instead. The reference-target sort still allocates about 309 MiB
   cumulatively, although it no longer owns the peak. Equivalence runs and
   patch frames are also naturally cursor-shaped.

6. **For the Go module, shorten blob lifetimes before changing its index.**
   Build stage-1 maps one at a time, release each window index before the next,
   and write `.gopclntab` directly into its final output span. The packed-index
   probe shows that recomputing hashes is a worse first trade than eliminating
   simultaneously live `pctab`, `gofunc`, concatenations and the final pcln
   copy.

The practical milestone is met: the code changes, CLI runtime budget, and
Linux reference-page eviction put Chrome near 2.4× and libxul near 2.2× target
RSS. Destination-backed materialisation and fully scoped metadata are the
route from there toward the `old + new + metadata` bound; more GC tuning and
smaller entropy tables in isolation are not.
