# Chrome ELF predictor — handoff

*2026-08-28. State of the spike, what is settled, what is open, and how to run
it. Read this first; it is a map, not a result.*

## Where it stands

A predict-then-correct codec for whole ELF images beats the incumbent by
**50.05%**: 2,634,264 XZ bytes (was 49.11% / 2,678,488 before §17 fixed the VEX/EVEX RIP-relative gate) against a RELA-aware Zucchini's 5,263,732, on
Chrome 151.0.7922.169 → .173 (Linux x86-64, 291 MB image, `.text` =
225,655,845 B). It replays byte-exactly. The decoder holds only the old image
and the patch.

The prediction is **99.377% byte-correct**, and essentially all of the gain
since the first working version came from *encoding* rather than from
predicting better. Split: plan 1,244,060 + correction 1,434,428.

Two principles paid for almost everything, eleven times between them:

1. *The decoder holds the old image, so a column should name what the old
   image already says rather than state it outright.* (8 wins)
2. *The encoder holds the target, so it can score each candidate model
   application and ship one bit.* (3 wins)

## What to read, in order

| document | why |
|---|---|
| [`chrome-elf-whole-image.md`](chrome-elf-whole-image.md) | **the main record.** §8 scoreboard (27 predictions, 9 right), §9 every measurement, §10 where it stops + the exact 9-row cost table, §11 what a better prediction would need, §12 data-structure sweep, §13 composition pressure test, §14 the displacement column measured |
| [`chrome-elf-predictor-spike.md`](chrome-elf-predictor-spike.md) | how the experiment was set up and why it is decoder-faithful; the reproduction invocation |
| [`chrome-elf-zucchini.md`](chrome-elf-zucchini.md) | the incumbent being beaten, and how the RELA-aware baseline was built |
| [`../SPEC.md`](../SPEC.md) | the authoritative design this spike feeds — §9 ranked domains, §10 milestones, §12 open questions |
| [`domain-executables.md`](domain-executables.md) | the wider domain: PE, Mach-O, per-ISA reference forms, linker determinism |
| [`environment-priors.md`](environment-priors.md) | negative result on ambient environment as a reference set — a *different* question from §13.3, and also closed |

## Settled — do not re-run these

Twenty-plus probes returned negative and are recorded so nobody pays for them
twice. The load-bearing ones:

- **The address domain is finished.** Before §9.15, 24.5% of `.text`'s wrong
  bytes lay inside relocated fields; after it, 1.9%. The remaining 1,232,241
  wrong bytes touch no field at all — they are instructions the compiler
  emitted differently (register allocation, inlining, PGO drift). §11's
  instruction diff: 66.3% of wrong bytes are *different op and different
  length*, and 99.4% of those sit on a real instruction boundary in the
  prediction. Only 4,256 bytes in the whole image are misalignment.
- **This code is not elsewhere in the image, in any form.** §11.5 re-ran the
  dictionary question canonicalised (PC-relative fields zeroed) after the raw
  test was judged the wrong instrument: 6.5% marginal gain, against a probe
  calibrated to show 6.1× where redundancy exists.
- **Data structures are a dead end.** §12: succinct/Elias-Fano/roaring/two-level
  indexes all lose, because every sparse column is already 16–47% *below*
  `log2 C(u,n)`. One survivor, byte-transposed varints, worth 11,636.
- **Layer ordering is correct.** §13.1: giving the choice layer a wider oracle
  makes chosen functions 10× worse (743,618 vs 72,430 wrong bytes), because a
  function is chosen structurally *precisely where the byte matcher went
  astray*. A structural-first variant moves the result by one byte.
- **Nothing in the plan is dead weight.** §13.2: the cheapest equivalence-run
  bucket earns 5× its plan cost; the matcher emits nothing shorter than 9
  bytes; 5 of 158,544 runs contribute no correct byte; dropping the 1,853 runs
  the choice layer overwrites changes the compressed stream by exactly 0.
- **Environment knowledge is spent.** §13.3: `.eh_frame_hdr` is already
  regenerated rather than corrected, clang switch tables already recovered by
  ABI signature (409,800 → 180,540), `.plt` + `.rela.plt` + `.got.plt` total
  2,308 XZ bytes. Separately, `environment-priors.md` closes the *other*
  environment question (ambient host binaries as a reference pool: 1.55×).

## Open — ranked

**1. A displacement column, measured at −20,248 and not yet implemented.**
`chrome-elf-whole-image.md` §14 (the prediction was §11.5). Zero the
PC-relative field of every instruction in the correction's long-run bucket,
ship the fields as their own columns, and let the decoder walk the repaired
bytes and fill them back in — zeroing a displacement never changes an
instruction's length, so both sides decode the same stream. Worth **−20,248 XZ
bytes, 0.76% of the patch**, against §11.5's estimate of 50,000–90,000.

The estimate missed because its premise was wrong, which is the part worth
carrying forward. §11.5 called these fields image-spanning and applied §9.17's
indexing rule to them; in fact **79.4% are jumps inside their own function**
and only 13.1% are calls to a known function start. Pulling out just the
genuinely image-spanning fields captures −17,200 of the −20,248, and the
47,603 local jumps are worth about 3,000 net — §9.17's other half ("local
values want a byte basis") holding, weakly.

Implementing it means touching the shipped correction format in
`correction.go`, adding a decode-side instruction walk, and re-proving
byte-exactness end to end. Judge whether 0.76% earns that.

**2. The general project.** The spike's question ("is ELF C/C++ domain rank 2
real?") is answered yes. `SPEC.md` §10 milestones 1–6 are the actual road:
core extraction, the portable wasm path, then the ELF module proper (matcher +
`.eh_frame_hdr`/RELR/`.dynsym` regenerators, corpus = a Rust and a C++ binary
across three point releases each, measured against bsdiff/hdiffz/Zucchini).
`SPEC.md` §12 lists six open design questions, of which #2 (op granularity)
and #4 (the 2× memory ceiling, against binsync's current 7.6×) are the ones
this spike touched without resolving.

**3. Operand relabelling, measured at ≤80,000 before costs.** §15: register renaming (H1) and stack-frame shifts (H2) are real and per-function consistent; struct-offset relabels are refuted; the 66.3% bucket is genuinely different instructions. Immediate-only (88 KB wrong) and absolute-disp-only (43 KB) are unexamined.

**4. Marginal, probably not worth it.** §13.2: re-deciding the per-function
choice bits in the whole-image context rather than inheriting the `.text`-only
pass's bits is worth −0.51%; making the criterion price wrong *runs* as well
as wrong bytes adds −0.04% on top. Roughly 6 KB, measured before the field-fix
layer repairs some of the same bytes, so the shipped figure is smaller.

## How to run it

Inputs (note: the two release binaries **moved** on 2026-08-28 — they had been
living only in `/tmp`, which is tmpfs here, one reboot from gone):

```
C=~/.cache/presage-chrome-zucchini
$C/chrome-151.0.7922.169                          # old release ELF
$C/chrome-151.0.7922.173                          # new release ELF
$C/symbols-151.0.7922.{169,173}/debug-info/chrome.debug   # encoder-side symbols
$C/chrome-151.169-to-173.zuc                      # supplies whole-file equivalences
$C/elfpredict-wi33/                               # cached plans for -resume
$C/memo/                                          # the stage memo, ~164 MB
```

**The default run is the headline and nothing else**, and takes **2m34s cold /
44s warm** on the 24-core box. It regenerates the five `*-plan.bin` artifacts,
so it is also what refreshes `-resume`:

```sh
go run ./bench/elfpredict \
  -old $C/chrome-151.0.7922.169 -new $C/chrome-151.0.7922.173 \
  -old-debug $C/symbols-151.0.7922.169/debug-info/chrome.debug \
  -new-debug $C/symbols-151.0.7922.173/debug-info/chrome.debug \
  -equivalence-patch $C/chrome-151.169-to-173.zuc \
  -out $C/elfpredict-wi33 -reference 5263732
```

`-rungs` defaults to `corrected-fields`. The scoreboard rungs below it —
`stable-raw`, `stable-relocated`, `all-mapped-raw`, `all-mapped-relocated`,
`text-ladder`, `changed-units`, `oracles` — xz between 30 and 64 MB of
correction each purely to reprint numbers already in
[`chrome-elf-whole-image.md`](chrome-elf-whole-image.md) §9, and nothing
downstream reads them. `-rungs all` restores every one of them and reproduces
the full §8 scoreboard byte-identically, in **10m10s** — down from 16 min,
because every xz call in the harness is single-threaded (`-T0` only splits
above 192 MiB, and the largest stream here is 64 MB) and they now overlap
under `-xz-jobs`. On the default single-rung run that overlap is worth 6 s of
50; on `-rungs all`, 1,507 calls and 547 s of xz CPU finish in 610 s of wall.

### Where a run spends its time

Measured on the cold default run, 2026-08-28, one line per stage on stderr
(`stage <name> <seconds> t+<elapsed>`, with the xz share broken out):

| stage | cold s | warm s | memoised on |
|---|---|---|---|
| load old / new image | 0.3 | 0.3 | — |
| symbols old / new (`chrome.debug`, 1.45 GB each) | 2.2 | memo | harness sources |
| structural matching | 14.4 | memo | codec sources |
| reference points | 14.7 | memo | codec sources |
| reference-targets (§9.12 domain, 6,096,339 targets) | 7.9 | memo | codec sources |
| mapped plan serialisation | 8.3 | memo | codec sources |
| predict all-mapped-relocated | 9.2 | memo | codec sources |
| equivalence parse + predict text-retargeted + selection | 9.6 | memo | codec sources |
| decode function map | 0.4 | 0.4 | — |
| equivalence sources + relocation plan | 1.9 | memo | codec sources |
| rodata selection (a whole-image prediction + the sweep) | 20.6 | memo | codec sources |
| predict for field fix (a second whole-image prediction) | 20.1 | memo | codec sources |
| field fix, 8,681,458 sites | 9.0 | memo | codec sources |
| plan column diagnostics | 0.8 | 0.8 | — |
| **predict corrected-fields** | **28.2** | **28.7** | never |
| measure corrected-fields (128 xz calls, 33.1 MB) | 4.7 | 4.7 | never |
| attribute corrected-fields (59 xz calls) | 9.3 | 9.2 | never |
| **total** | **154** | **44** | |

The warm floor is the prediction, its measurement and its attribution — 43 s.
That is deliberate: the memo stores *serialized plans*, the same artifacts
`-resume` already caches, and stops at the decoder. Memoising the prediction
itself would mean the run no longer proves the decoder produces it, and a
corrupted entry would still yield a valid-but-larger correction that nothing
would catch.

After a `delta/x86` edit only the 2.2 s symbol parse is reused: every other
stage decodes instructions. The default-rungs change, not the memo, is what
makes that case 2m34s instead of 16 min.

### Fast iteration

`-resume` replays from cached plans and skips construction entirely; `-probes`
reports without encoding a correction. A full probe pass is **1m30s**, of which
28 s is the one prediction the probe reads:

```sh
go run ./bench/elfpredict -old ... -new ... \
  -resume $C/elfpredict-wi33 -rungs corrected-fields \
  -probes order,eqvalue,eqcanon -reference 5263732
```

Probes available: `chunks`, `cond`, `cause`, `insts`, `cdict`, `dict`,
`packing`, `eqsrc`, `order`, `eqvalue`, `eqcanon`, `dispcol`, `operands`,
`immprobe`, `instpos`, `rnfdict`.

Other flags: `-memo DIR` (empty disables the memo), `-xz-jobs N` (concurrent
xz processes, default `min(GOMAXPROCS, 8)`).

## Traps

- **`xz -T1` is mandatory for any dictionary or marginal-cost probe.**
  Multithreaded xz splits input into independently-coded blocks and hides
  exactly the cross-boundary matches such a probe asks about. `xzSizeContiguous`
  exists for this.
- **A column's standalone xz size wildly overstates its marginal cost** inside
  a concatenated stream — §9.3 saw 12×. The two agree only when the column ends
  up empty. Always measure marginal.
- **Check the decoder could actually run the arithmetic in the order proposed.**
  More than one apparent win has been a value the decoder cannot compute yet at
  the point it needs it.
- **Watch for tautologies.** One probe reported a 118,060-byte win that was the
  negated `DstSkip` column, already shipped; the real gap was 728 bytes of
  zigzag-vs-unsigned varint.
- **The stage memo is keyed on a hash of the source tree, not on git state.**
  `bench/elfpredict/memo.go`, living in `$C/memo` (not `/tmp`, which is tmpfs
  here). Two identities: `codeHarness` = the `.go` files under
  `bench/elfpredict` plus `go.mod`/`go.sum`, `codeCodec` = those plus `delta/`.
  **Editing anything under `delta/` or `bench/elfpredict` invalidates every
  stage that decodes an instruction; editing under `delta/` alone still keeps
  the symbol parse.** Both are printed on the first line of every run, so
  `code identity:` changing is the signal that the next run rebuilds. What it
  does *not* see: an input file rewritten with an identical size and mtime
  (inputs are identified by path/size/mtime, because content-hashing 3.5 GB of
  frozen release binaries and DWARF costs more than the stages it protects),
  and changes to packages outside `bench/elfpredict` and `delta/` — `internal/cz`
  moves the zstd columns, which no memoised stage stores. When in doubt,
  `rm -rf $C/memo`; a cold run is 2m34s.
- **The memo deliberately stops short of the prediction.** Everything it stores
  is serialized plan bytes. A stale prediction would produce a *valid* but
  larger correction, and `measure`'s byte-exact replay check would still pass —
  the same silent-fiction failure mode as a stale reference-target domain,
  which is why entries carry a checksum and the code identity.
- **Cost yardstick for `.text`:** 1,167,832 shipped for 1,255,810 wrong bytes
  in 362,032 runs = **1.061 patch bytes per run + 0.624 per wrong byte**. An
  average 3.47-byte run costs 3.23. Use this to price any proposed change
  before building it.
