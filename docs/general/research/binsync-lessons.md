# What go-binsync taught — the insights the general codec must keep

Distilled from `docs/DESIGN.md`, `docs/research/go-aware-transform.md` and the
`delta` package (5,301 lines: ≈ 1,200 generic — container, correction,
exact-match engine, plain codec — and ≈ 4,100 Go/x86-specific). Each item
below is a measured fact from that work, followed by what it implies for a
codec that is not Go-specific.

## 1. The win is in *regeneration*, not in better matching

bsdiff-class approximate matching already recovers the first-order change
cheaply; Percival showed a disassembler buys ~10 % beyond it. binsync got
28–67× because it does something neither does: it **rebuilds** the new file's
second-order structure — function addresses, every rel32/rip-relative
operand, `functab`, `findfunctab`, `pctab` offsets, type-descriptor
`nameOff/typeOff/textOff`, absolute pointers in data — from a small
description of the *first-order* change (the layout table: 10 KB compressed
for a 94 MB file). Prediction removes the cost of the shift; matching only
re-finds it.

Implication: a predictor module is a *format model with a regenerate step*,
not a smarter differ. The API must let a module say "given old bytes and this
side information, here is the whole predicted new object", and the framework
measures the residual.

## 2. Length-exact prediction makes the residual positional

Because the layout table carries every function's and every table's new size,
the prediction has *exactly* the new file's length and structure; the
correction is then a lockstep walk (`gap, span, bytes`) with a bounded local
match window for changed function bodies. That is what removes the suffix
array: encoder 2.4 s / decoder 0.9 s on 94 MB, versus bsdiff at 40 s. And it
lets the decoder apply the correction **in place over the prediction buffer**
(the source window for a region runs forward into untouched bytes).

Implication: a module's contract should distinguish *length-exact*
predictions (residual = positional correction, cheapest) from
*length-changing* ones (residual = a shifted delta, D17: stage 1b cost 66 KB
positionally vs 17 KB as an LZ delta). Both are needed; the framework picks
the residual coder per region from the module's declared property.

## 3. Prediction is a compression context, never a correctness mechanism

The decoder hashes the prediction before applying the correction (the hash
rides in the patch), the output is hashed against the release, and a wrong
prediction costs bytes only. That single property is what let the Go
transform be shipped with "1,616 undecodable bytes of `.text` left
unrelocated" and 0.13 % of the file mispredicted: it did not have to be
right, only deterministic.

Implication: every module output is hash-checked; encoder/decoder divergence
is a *named* failure that falls back (in binsync, to the blob). This is also
the property that makes untrusted or third-party modules tolerable: a
malicious or buggy predictor can only produce a hash mismatch, provided the
framework bounds its resources (§7).

## 4. Determinism is a discipline, and it is fragile

Concrete traps that bit: worker counts derived from `GOMAXPROCS` (fixed at a
compile-time constant 24 — D20); the encoder building its prediction from its
*own* structures rather than by decoding the layout it just emitted (fixed by
literally running the decoder's functions inside the encoder); a self-check
that caught `pcHeader.textStart` being filled in when Go 1.27 leaves it zero —
8 bytes per patch that no round-trip test would ever show, because the
correction silently fixed them (`TestSelfPrediction`: old → old through the
whole pipeline must be byte-identical with a correction ≤ 64 B).

Implication: the module runtime must make non-determinism *impossible*, not
discouraged — no threads, no clock, no floats with platform-dependent
results, no uninitialised memory, no host-dependent limits — and the
framework must ship the self-prediction gate as a first-class test for every
module. This is the strongest argument for a sandboxed bytecode over
"vendor a native library per format".

## 5. Per-format-version work is real and unbounded

Go 1.25 → 1.26 changed `entryOff` handling; 1.26 → 1.27 changed `moduledata`,
removed `.typelink`/`.itablink`, added `.go.type`/`.go.func`, grew `MapType`
by 24 B and made `go:func.*` alignment data-dependent. The response was D14:
support exactly one Go release, gate it on the self-prediction corpus, and
fall back to the plain codec for everything else.

Implication: modules will be *many* and *versioned*, most of them written by
the people who know the format, and the deployed decoder cannot possibly have
them all built in. That is the case for a portable module format: the patch
(or the store) names the module by hash; a decoder that lacks it fetches it
once. It is also the case for keeping each module small: the Go one is 4,100
lines *because* it does everything itself (ELF, pclntab, x86 decode, data
maps, type descriptors). Shared engines (ISA operand relocation, piecewise
shift maps, offset-table regeneration) should live in the core so a format
module is mostly parsing.

## 6. Consensus and majority-vote maps beat point estimates

Pointer shifts chosen by majority vote among all pointers into the same
symbol fixed a 5-byte-off shift into `runtime.gcbits` that held one pair at
4.7× (it went to 69×). The data maps are piecewise-constant shift runs over
16-byte blocks, with pointer-containing blocks constrained to multiples of 8.

Implication: "piecewise-constant shift map with voting" is a generic
primitive (Percival's block alignment produces the same object from an FFT
index). Put it in the core.

## 7. Memory is the unsolved constraint

Decoder peak 7.6× the input (714 MB live heap for 94 MB): not the buffers
(2×) but the prediction's working set — 110 K `_func` records as individual
slices, a position-ordered index at 8 B per position, the encoder holding
old + pred + new. A 1 GB input at that ratio is 7.6 GB on a target.

Implication: the module runtime must have a hard memory ceiling, prediction
must be *streamable in regions* where the format allows (predict a function,
emit it, drop it), and the core must let a module declare its working-set
needs so the encoder can choose a cheaper module when the target cannot
afford the best one. Also: a decoder that runs a predictor in an interpreter
pays CPU, but the memory story can be *better* than native if the sandbox
enforces a ceiling.

## 8. Compression tier choices are measurable, so measure them

Every stream takes the smaller of zstd and brotli (D16); brotli-10 in
independent 8 MiB frames beat a single `zstd -19` stream for the blob (D22);
splitting streams by statistics (ctrl vs literals) mattered, splitting frames
by stream did not (0.1 %, D19). Percival's "four difference strings, keep the
smallest" is the same instinct.

Implication: the encoder's job includes *trying alternatives and keeping the
smallest* at every level — module choice per region, residual mode per
region, entropy coder per stream — with the choice recorded in the stream.
That is the Roaring container-selection pattern applied recursively, and it
belongs in the core so modules never reimplement it.

## 9. Small, hash-verified, independently fetchable frames

8 MiB frames, each with a BLAKE3 hash and its own codec tag, cut at size not
at stream boundaries; parallel ranged fetch recovers a lossy link (57 s →
7.4 s at 1 % loss); resume at a frame boundary; a per-frame hash in the
pointer. Bounds-checked varint headers rather than CBOR: "60 lines of varint
reader are less surface than a codec dependency".

Implication: keep the container. It is format-agnostic already.

## 10. What binsync got wrong or left open, that the general design should fix

- **Correction mode is literal-only**: mispredicted rel32 operands cost their
  full 4 bytes; a multiprecision difference (thesis §2.7) would cost one.
- **The `.go.type` residual** (45 KB of 95 KB) is partly wrong rewrites that an
  anchor pass could recover: i.e. the *predictor* has headroom, not the coder.
- **arm64 untried**; fixed-width instructions should be easier. A generic ISA
  relocation engine is the natural home.
- **One module, hard-wired**: transform id is a byte (0 plain, 1 go-amd64-v1);
  there is no way to ship a new transform to an old decoder except the blob.
- **No composition**: a Go binary inside a tarball inside a gzip layer gets
  the plain codec.
