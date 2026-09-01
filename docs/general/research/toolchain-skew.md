# Toolchain skew: the slow path, and what the one-release pin costs

2026-09-01, at `7140da8`. One run per number on a 24-core machine, Go 1.27.0.
Wall clock from the CLI's own report; sizes are the patch file.

A caller reported a 79 MB Go pair straddling a Go release taking 7 min 17 s
to encode where a matched pair takes seconds, and asked for two things: a
fail-fast out of that path, and a way to stop being on it. This is what the
pair actually costs, where the time goes, and what each of the two fixes is
worth.

## 0. Headline

Three separate things are true and only the first was known.

1. **The slow path is not a bad prediction, it is unpriced trial work.** On
   a pair of Go images from two toolchains the ELF module's prediction is
   20 % wrong, and the encoder spends 26 of its 29 seconds correcting it —
   but almost all of that is trying codings that do not pay. The same
   prediction, corrected in the one shape every decoder reads, is 5,191,642 B
   in 4.0 s, which is smaller *and* faster than the `lz` fallback's
   5,897,683 B in 7.0 s. The fallback was never the cheap option; frugality
   was.
2. **Detecting it is free.** `gobin.Parse` reads the section table, the
   build info, the pclntab and the moduledata of a 93.7 MB binary in 10 ms.
   A full self-prediction — the strongest statement available about whether
   the codec models a layout — is 1.6 s on the same file.
3. **The one-release pin is why real artifacts are on that path at all.**
   Every upstream Go release binary in the local corpus (cockroach,
   kube-apiserver, prometheus, terraform, vault; 88–326 MB each) is built
   with go1.25.5–go1.26.6. Not one is go1.27, the only release the Go module
   reads. On a real pair the pin costs **28.6× the bytes and 6.6× the
   time**: prometheus 3.13.1 → 3.13.2 is 74,636 B in 4.2 s when both sides
   are rebuilt with go1.27, and 2,137,152 B in 27.9 s as upstream ships it.

The cost model in §4 is implemented. The layout work in §6 is a proposal.

## 1. The pairs

`ts` is `bench/testsrv` from the sibling benchmark tree, built twice from
one source with two toolchains (`GOTOOLCHAIN=go1.26.4` and `go1.27.0`), the
cleanest available model of a release bump: the source is identical, so
every difference is the toolchain's. `prom-127` is the same prometheus pair
rebuilt with go1.27; `prom-126` is upstream's own release binaries.

| pair | target B | Go | module | encode | patch B |
| --- | ---: | --- | --- | ---: | ---: |
| prom-127, stripped | 93,769,955 | 1.27 → 1.27 | `go` | 4.2 s | 74,636 |
| prom-126, stripped | 97,577,304 | 1.26.5 → 1.26.5 | `elf` | 27.9 s | 2,137,152 |
| " | | | `lz` | 9.1 s | 3,049,082 |
| ts, stripped | 29,995,271 | 1.26.4 → 1.27.0 | `elf` | 29.3 s | 4,573,629 |
| " | | | `lz` | 6.1 s | 5,895,233 |
| ts, unstripped | 58,863,534 | 1.26.4 → 1.27.0 | `elf` | 61.0 s | 9,271,679 |
| " | | | `lz` | 14.2 s | 11,019,434 |

The unstripped target is the debugz-expanded length; the file is 43,144,701 B.
`lz` rows are `-modules lz`.

The reported 7 min 17 s is not reproducible at this commit — the closest
shape measured here, an unstripped cross-release pair, runs at 1.4 s/MB,
which puts a 79 MB pair near 110 s. The v5 encoder work landed after that
measurement. The ratio it illustrates is unchanged.

## 2. Where the time goes

CPU profile of the 30 MB cross-release pair (29.3 s wall, 46.6 CPU-s):

| | cum | flat |
| --- | ---: | ---: |
| `delta.encodeCorrectionSized` | 48.8 % | |
| `presage.codePiece` (split residual, 4 workers) | 45.5 % | |
| `lz.(*Index).Find` | 29.0 % | 15.1 % |
| `brotli.(*h6).FindLongestMatch` | 25.3 % | 24.1 % |
| `zstd.(*bestFastEncoder).Encode` | 13.1 % | 9.5 % |

Everything above the compressors is residual coding. Timed on its own,
`elfmod.Module.Analyse` is **3.3 s** of the 29.3 s (5.7 s of the 61 s on the
unstripped pair). The structural work is not what is slow; correcting a
failed prediction is.

That framing — a slow module against a fast fallback — is what the first
attempt at this document assumed, and it is wrong. §4 measures the same
prediction coded at several prices, and the cheapest coding of it beats the
fallback outright.

## 3. What each stage of the encoder is worth

The encoder's residual path is a cascade of optional codings, each tried in
full and kept if smaller: the shipped correction shape, then two more shapes
(near-miss and modal), then the split residual, which codes each piece of the
correction several ways again. Nothing prices any of them. Priced, on the
whole-region correction (`bench/costprobe`):

| pair | shipped shape | + two more shapes | they buy | for | exchange |
| --- | ---: | ---: | ---: | ---: | ---: |
| chrome, symbols | 3,910,631 B / 0.8 s | 3,910,631 B / 4.8 s | **0 B** | 4.0 s | 0 B/s |
| prom-126 | 4,372,626 B / 4.1 s | 2,588,664 B / 11.1 s | 1,783,962 B | 7.0 s | **254 KB/s** |
| ts, stripped | 5,191,642 B / 4.0 s | 5,185,897 B / 13.0 s | 5,745 B | 9.0 s | 638 B/s |
| ts, unstripped | 15,544,341 B / 5.7 s | 15,544,639 B / 18.7 s | **−298 B** | 13.0 s | — |

The same stage is worth a quarter of a megabyte per second on one pair and
nothing at all on three. No threshold on prediction quality can express that:
Chrome's prediction is the *best* on the corpus, 0.59 % wrong, and its extra
shapes are worth exactly nothing.

The fallback, priced the same way: chrome 18,874,585 B / 56.7 s, prom-126
3,062,435 B / 9.3 s, ts stripped 5,897,683 B / 7.0 s, ts unstripped
16,302,978 B / 14.7 s. Against those, the module's *cheapest* coding is
smaller on three of four pairs and faster on four of four, and its best
coding is smaller on all four. On this corpus the module never needs to be
abandoned for the fallback; it needs to stop over-coding.

### The estimator, and where sampling breaks

To price a stage before running it, its gain has to be estimated. A
deterministic sample of the region, coded and compressed, is the obvious
instrument. It works in one direction and fails in the other.

**Across two different streams it fails.** On prom-126, a 1/32 sample
overestimates the module's correction by 64.4 % and the fallback's by 16.1 %
— because losing the long-range context costs a structured correction far
more than it costs an lz stream — and the estimate therefore ranks the
fallback first when the truth ranks it second. Concatenating the windows and
compressing once, so both sides are treated identically, changes nothing
(+64.8 %). A sampled comparison of unlike codings is not sound.

**Within one stream it works.** The same sample comparing three codings of
one stream shares the bias, and the margin survives it:

| pair | sampled shapes/shipped | measured |
| --- | ---: | ---: |
| chrome | 0.992 | 1.000 |
| prom-126 | 0.860 | 0.592 |
| ts, stripped | 1.000 | 0.999 |
| ts, unstripped | 1.000 | 1.000 |

It understates the gain where there is one, which makes it conservative in
the direction of skipping a stage that would have paid, and it separates
"worth a quarter megabyte a second" from "worth nothing" on every pair.

### The work model

Seconds are modelled from byte counts at rates calibrated on this corpus
(`bench/workmodel`), never taken from a clock: a patch that depended on how
loaded the machine was would not be reproducible, and the corpus ratchet
could not be a test. Writing a correction shape runs at 5.9–9.8 MB/s of the
stream it produces; the shipping compressors price one at 4.4–5.8 MB/s of it.
The two extra shapes together are modelled at 1 MB/s of the already-written
stream, which predicts 6.5 s where 9.0 s was measured and 7.7 s where 7.0 s
was. A cost model good to a factor of two is enough when the gains it
arbitrates differ by 400×.

## 4. The price

`Options.Price` is what one second of encoding is worth in patch bytes. A
stage runs only if its estimated gain reaches `Price ×` the seconds it is
modelled to cost (`delta.WorthOf`). Two stages are priced today: the extra
correction shapes, against the sampled estimate above, and the split
residual, against the bound that it cannot save more than the whole
correction costs — a bound needs no sample and can only skip a stage that
could not have paid.

The unit is the caller's own problem, which is the point. A patch fetched N
times over a link of W bytes per second makes a second of encoding worth
W/N patch bytes: 100 MB/s to a hundred hosts prices a second at 1 MB, to ten
thousand hosts at 10 KB. `-price` on the CLI; `-price -1` prices a second at
nothing and runs every stage.

The default, 8 KB/s, is not a tuned threshold but a floor: it skips what the
table in §3 measures as worthless (0 B/s, 638 B/s) and keeps everything
measured as worth having (254 KB/s for the shapes on prom-126, 127 KB/s for
the split residual on Chrome). It sits in a 400× gap, which is why it needs
no tuning. A caller who knows what its own second is worth should say so
rather than inherit it.

Measured end to end:

| pair | price | encode | patch |
| --- | --- | ---: | ---: |
| chrome | a second is free | 32.5 s | 2,376,189 B |
| " | **default** | **28.4 s** | **2,376,189 B** |
| ts, stripped | a second is free | 28.7 s | 4,573,629 B |
| " | **default** | **18.2 s** | **4,573,629 B** |
| " | 1 MB/s | **9.6 s** | 5,083,438 B |

The default is free: identical patches, 13 % off Chrome and 37 % off the
cross-toolchain pair. At a deploy path's price the pathological pair encodes
in 9.6 s, and the corpus ratchet is byte-identical (chrome 2,376,189 B,
libxul without symbols 3,100,352 B).

For comparison, the first version of this work gated on prediction quality
instead — discard a module whose prediction is more than 12 % wrong and code
with `lz`. That reached 9.6 s on the same pair for 5,895,233 B: the same
time, 811,795 bytes worse, and a threshold with no defensible value. It is
removed. The lesson is the one the frontier shows: the expensive thing was
never the module.

### What is not priced yet

The split residual's *pieces* are still each coded several ways
(`presage/split.go`), which is the largest remaining unpriced fork — on the
cross-toolchain pair it is about 8 s of wall clock for 528 KB, an exchange
rate of 66 KB/s that a deploy path would decline and a release pipeline
would take. Pricing it wants a per-piece estimate rather than the whole-region
bound used today. Module selection itself is also unpriced, and on this
corpus does not need to be (§3); if a module ever ships a plan large enough
to lose to the fallback, the comparison belongs here, and §3 says it cannot
be a sampled one.

## 5. What the pin costs

The Go module reads `go1.27` only (`gobin.SupportedGo`, D14). The measured
consequences:

- **prom-126 vs prom-127**: the same source change, the same two binaries
  modulo toolchain, is 2,137,152 B instead of 74,636 B — 28.6× — and 27.9 s
  instead of 4.2 s. Nothing about that pair is hard; it is 1.26.5 on both
  sides, a layout the codec simply does not read.
- The local corpus of upstream releases had to be *rebuilt* with go1.27
  (`corpus127/`) before the Go module could be measured on it at all. That
  is the tell: the pin is not a rare-input problem, it is the common case.
- A cross-release pair — the case the pin is actually designed for — is
  rarer than that, one deploy per bump, and is the one the encoder now
  handles cheaply rather than specially: at a deploy path's price it costs
  9.6 s and a patch a fifth better than the fallback's (§4).

The pin bought a codec small enough to test exhaustively, and the
self-prediction gate that goes with it caught a bug no round-trip test
would have (§2.7 of `go-module-design.md`). Both stay. What changes is
where the version knowledge lives.

## 6. Proposal: layouts as data, not as a pin

**What actually varies between releases.** From the prototype that carried
1.25, 1.26 and 1.27 at once (`bench/gotransform`, 5,725 lines, five
layout-conditional sites): where the moduledata lives (`.go.module` from
1.26; an ordinary `.noptrdata` symbol found by its pcHeader pointer in
1.25), its field offsets and field set (`typelinks`/`itablinks` slices
before 1.27, `typedesclen`/`itaboffset`/`itabsize` after), where the type
descriptors live (`.go.type` from 1.27, `.rodata` before), whether
`go:func.*` and `findfunctab` sit inside `.gopclntab` (1.26+) or in
`.rodata` (1.25), descriptor sizes (`MapType` grew 24 B in 1.27), and the
root rule for the descriptor walk (typelink-flagged roots before 1.27, the
sorted section after). Everything else — the pclntab magic and shape since
1.20, 32-byte function alignment, the matcher, the correction, the container
— is common.

**The shape.**

1. `gobin.Layout` is a value: section names, moduledata field offsets, a
   descriptor-size table id, and the two or three behavioural flags above.
   `gobin.Parse` picks one from `debug/buildinfo`'s version string and then
   *validates* it against the image's own invariants — `pcHeader` equals the
   `.gopclntab` address, `epclntab` equals its end, `text` lies inside
   `.text` — which are the checks already in `Parse`. A descriptor that does
   not fit the image is a decline, not a misparse.
2. **Read with the source's descriptor, write with the target's.** The
   predictors take both. The plan records the target's layout id, so a
   decoder that does not know it refuses by name (the prototype's layout
   already carried `GoLayout`; §2.6's transform byte is the same mechanism
   one level up).
3. **Same-layout pairs first.** A pair whose two sides share a layout is the
   whole common case, and needs N readers and N writers, not N². A
   cross-layout pair declines in the 10 ms it takes to read both build
   strings — which is §4's missing tier-1 rule, delivered as a side effect.
   Cross-layout prediction is a later question, and only worth asking if
   someone is bumping toolchains often enough to care.
4. **A release is supported when the self-prediction gate says so.** SPEC
   §4.3 already requires `analyse(obj, obj)` to be byte-exact on the corpus;
   make that per release, with a corpus directory per release. That is the
   acceptance test, and it is what keeps a widened matrix honest.
5. **Unknown releases: probe rather than guess.** The gate is cheap enough
   to run at encode time — 1.62 s for 93.7 MB, 0.44 s for 30.0 MB, against
   the 4.2 s the whole encode takes. An encoder meeting a version it does
   not have a descriptor for can try the newest one, self-predict the
   reference image, and accept the layout only if the result is byte-exact.
   A new Go minor that moved nothing then keeps working; one that moved
   something declines in under two seconds instead of mispredicting. This is
   safe in the direction that matters: the correction fixes whatever the
   prediction gets wrong and the output is hash-verified either way, so the
   worst case of a bad guess is a bigger patch, never a wrong one.

**Order of work.** 1.26 first: it is what the corpus ships and what the
current stable release will be preceded by forever. The parse side is
mechanical (the prototype's `elfbin.go` is the reference); the predictor
side is the descriptor-size table and the descriptor walk's root rule. Then
1.25, if the `.rodata` placement of `go:func.*`/`findfunctab` turns out to
be cheap; the prototype declined to encode 1.25 for exactly that reason.

## 7. Side finding: exact trial pricing earns nothing here

`cz.PreferExactUnder` prices encoder trials with the real compressors for
targets under 128 MiB. Re-run with `PRESAGE_SIZE_PROXY=zstd`, all three Go
pairs produce **byte-identical patches** and save 16–25 % of the encode
(61.0 → 46.6 s, 29.3 → 22.0 s, 27.9 → 23.3 s); the `go`-module pair is a
wash both ways (4.17 → 4.24 s, same 74,636 B). The threshold's evidence came
from the browser pairs, which are not re-measured here, so this is not an
argument to move it — but it suggests the axis is wrong. What makes exact
pricing expensive is the size of the streams being priced, which is the
*correction*, not the target. Keying it on the correction would want a
measurement on chrome and libxul first.

It is the same finding as §3 from another angle: the encoder spends on
precision it cannot use. Pricing the trials, rather than the compressor they
use, was the larger lever, and is what §4 does.

## 8. Reproducing

```sh
go build -o /tmp/presage ./cmd/presage
# two builds of one source with two toolchains
( cd <bench>/testsrv
  GOTOOLCHAIN=go1.26.4 go build -trimpath -ldflags="-s -w" -o /tmp/ts-126 .
  GOTOOLCHAIN=go1.27.0 go build -trimpath -ldflags="-s -w" -o /tmp/ts-127 . )
/tmp/presage diff /tmp/ts-126 /tmp/ts-127 -o /tmp/skew.psg -v
/tmp/presage diff -price -1 /tmp/ts-126 /tmp/ts-127 -o /tmp/full.psg -v
/tmp/presage diff -price 1048576 /tmp/ts-126 /tmp/ts-127 -o /tmp/fast.psg -v
go run ./bench/costprobe /tmp/ts-126 /tmp/ts-127     # the frontier of §3
go run ./bench/workmodel /tmp/ts-126 /tmp/ts-127 ts  # the rates of §3
```

The second prices a second at nothing, which is what the encoder did before
this work.
