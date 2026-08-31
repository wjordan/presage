# The one-line change, driven toward its floor

2026-08-30. The one-line pair (`v1-F2 → v2c-F2`, 29.6 MB) is the extreme
case in the README table, and the question was how much of its 1,202-byte
patch is information and how much is encoding. Ledger first, then the four
levers that shipped, then what remains and why it stays.

## Where the 1,202 bytes went

Measured with `presage diff -v` and probes over `gomod.Module.Analyse`
directly (plan and correction compressed jointly, as the body ships):

| part | bytes | content |
|---|---:|---|
| container header | 166 | three 32-byte BLAKE3 hashes (ref, target, frame), sizes, one region |
| plan, in the joint stream | ~505 | funcs op stream 65,330 raw (32,652 × "unchanged"); section table 159; stage-1b pctab diff 266; segment map 86; data maps 106 |
| prediction root hash | 32 | `predictionHash`, plaintext in the body |
| correction | ~536 | 372 mispredicted bytes in 65 runs |

The 372 wrong bytes: 132 random (Go build ID note 80, GNU build-id 20, FIPS
sum 32), 179 the edited function's `.text`, 13 `.gopclntab`, and ~45 across
`.rodata`/`.go.type`/`.go.module`/`.noptrdata` — of which the `.go.module`
10 were derivable moduledata fields, and the rest is genuinely changed data
(runtime bitmaps whose old and new values differ by one bit position, which
no shift rule reaches).

## What shipped (branch `one-liner-floor`)

1. **Moduledata from the layout** (`delta/moduledata.go`). The pointer
   rewrite predicted `.go.module` positionally; the pclntab table pointers,
   slice lengths (cutab in elements, ftab `NFunc+1`), section bounds and the
   transmitted modValues are all fixed by the skeleton. Each field is
   written only when the old binary's field matches the same derivation on
   the old skeleton, so a build the rule does not describe (coverage
   counters, say) falls back to the positional prediction and pays
   correction bytes, never correctness. −24 B synthetic, −172 B prometheus.

2. **Run ops in the layout** (`delta/layout.go`). The funcs op stream and
   the section table gain op 3, "n entries matched in order, unchanged" —
   sizes coded against the previous entry's delta, offsets against the
   address delta. The one-line plan drops 66,002 → 595 B raw. Compressed
   gain is small (brotli already ate the zeros): −28 B synthetic, −11 B
   prometheus. The real win is that the plan is now legible: what remains
   in it is content, not ceremony. Layout format change; patches from
   before this branch do not decode.

3. **Single-frame patches drop the frame hash** (`presage/container.go`).
   A one-frame body is all-or-nothing and the target hash already checks
   it, so the per-frame BLAKE3 is pure duplication there: omitted when
   `nframes = 1`, −32 B on every small patch. Multi-frame patches keep
   per-frame verification for ranged fetch.

4. **The FIPS sum recomputed, not shipped** (`presage/gomod/fips.go`,
   `presage.Finaliser`). The linker's `.go.fipsinfo` integrity sum is
   HMAC-SHA256(zero key) over `"go fips object v1\n"` plus each of the four
   module ranges, length-framed — a pure function of the rest of the
   binary, verified reproducible on every corpus build including PIE. The
   encoder checks the rule holds on the target, sets one plan flag, and
   prices the residual against a prediction whose sum bytes are already
   right (`MaskResidual`); the decoder recomputes the sum after the
   residual (`Finalise`), before the container's target hash checks the
   result. The recorded prediction root is still of the decoder-reproducible
   prediction. −37 B on the `-buildid=` pair, −47 B prometheus; on the F2
   pair the 36-byte raw saving compresses 10 B *worse* (brotli-11 block
   splitting; verified deterministic), net +11 there.

Result, the full measured set, all applied and `cmp`-verified:

| pair | before | after |
|---|---:|---:|
| one-line (`v1→v2c`, F2) | 1,202 | **1,129** |
| one-line, F3 build (`-trimpath -s -w -buildid=`) | 1,080 | **970** |
| +3-byte string (`v1→v2l`) | 545 | 459 |
| multi-package (`v1→v4`) | 1,705 | 1,635 |
| `v3→v4` | 580 | 479 |
| PIE one-line | 1,157 | 1,071 |
| one-line with DWARF (59 MB) | 2,065 | 1,950 |
| prometheus 3.13.1 → 3.13.2 | 70,195 | 69,933 |
| prometheus, default DWARF build (181 MB) | 332,414 | 330,557 |
| prometheus 3.13.1 → 3.14.0 (minor) | 1,398,749 | 1,398,675 |

## Build flags: the randomness is optional

Of the F2 pair's 132 random bytes, 100 are build IDs that exist only in
default builds. `-ldflags=-buildid=` drops the Go build ID note;
`-trimpath`/`-buildvcs=false` keep paths and VCS stamps out; the F3 build
has no note sections at all, and its only remaining "random" bytes were the
FIPS sum, now recomputed. Simulated on the F2 pair: pinning both notes is
worth −153 B. For a fleet shipping its own binaries, the F3 flag set is the
answer; the codec cannot remove what the linker was asked to randomise.

## What remains at 970, and why

| part | ~bytes | status |
|---|---:|---|
| container header | 134 | ref, target and prediction-root hashes; a "minimal" profile could truncate, integrity says don't |
| `.text` edit + segment map | ~230 | the change itself: 17 pieces + 179 corrected bytes |
| stage-1b pctab diff | ~198 compressed | the edited function's pc tables, ~80–100 B of it true content; the rest is `plainDiff` triple overhead. The next real lever, and it is pcln-level work |
| section table, data maps, mod values | ~100 | measured near entropy |
| residual run addressing | ~120 | 3–4 B per run × 30-odd runs |

The floor for this pair is therefore roughly 700–800 B without changing
what the container promises. Everything cheaper than that is either the
change itself or a hash.

## The pc-table replay, tried and rejected

The stage-1b lever looked like offsets: the prediction replays the linker's
pctab dedup walk with the *old* tables, so a resized function's tables land
with old run boundaries. A spike replayed each such table through the
function's transmitted piece list (`mapSegOff`), carrying values — decoder-
derivable, zero plan bytes. Measured end to end: one-line 1,129 → 1,155,
prometheus 69,933 → 69,919. Rejected, twice over:

* A transformed-but-inexact table is *novel* content. The untransformed old
  table exists verbatim in the prediction, so `plainDiff` copies it for a
  triple; the replayed one exists nowhere and becomes extra bytes. The
  replay only pays where it is nearly byte-exact, and it is not: the real
  new tables interleave runs the old function never had (the inserted
  call's inlined pcfile/pcln spans), and the piece list misses sub-piece
  shifts (the prologue grew 3 bytes when the frame crossed imm8).
* The prometheus stage-1b diff is 17,441 B compressed — 25 % of that patch,
  the largest single component — but it is the pc tables of *recompiled*
  functions: genuinely new content, the same flat spot the sub-function
  selection work hit. A model that wins there has to predict what the
  compiler emitted, not where the linker put it.

The measured breakdown that says the rest of s1b is honest: 5 control
triples (27 B), 120 B of diff (77 nonzero — mostly ±1 line-delta varints of
same-file functions after the edit, already 1 byte each), 115 B of extra —
the edited function's regenerated tables.
