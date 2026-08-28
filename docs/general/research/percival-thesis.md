# Percival, *Matching with Mismatches and Assorted Applications* (D.Phil. thesis, Oxford 2006)

Source: <https://www.daemonology.net/papers/thesis.pdf>. Read 2026-08-27 for
the generalised predictive codec. The 2003 bsdiff paper was already in the
corpus (`docs/research/binary-delta.md` §1.3); this is the fuller treatment,
and two of its three chapters describe machinery binsync does not have.

## What the thesis contains

| Chapter | Content | Relevance |
|---|---|---|
| 1. Matching with mismatches | An FFT-based randomised algorithm that, for a pattern of length *m* and a text of length *n*, finds the offsets where the pattern matches the text *apart from substitutions* — up to ~50 % of the characters may differ. Projects the alphabet to ±1 via random maps φᵢ, folds the text modulo random primes pᵢ ≈ L, and reads the best offsets off cyclic correlations. Index size O(n log n / m) floats; O(L log L) per query. | A *symbol-free* approximate aligner. It is what a predictor module falls back to when a format offers no function table. |
| 2. Delta compression of executable code (bsdiff 6) | The three-orders taxonomy of executable change; block alignment (ch. 1) + local alignment (suffix array) combined by pruned dynamic programming; four difference-string modes; the difference map/non-zero split; results vs Exediff. | The theory of *why* binsync works, and the encoder it would need where no metadata exists. |
| 3. Universal delta compression | One patch that applies to *any* old file within edit radius *R*: transmit the block-matching index of ch. 1 plus a Reed–Solomon syndrome; the receiver aligns what it has, marks the rest as erasures, and decodes. | Drifted targets, fleets with heterogeneous builds, and — importantly — a codec whose *prediction* is only approximately right. |

## Chapter 2: the taxonomy (verbatim in spirit)

- **Zeroth-order** changes: innate to compilation — timestamps, build ids, host
  stamps. Small, but they defeat "is it modified" checks.
- **First-order** changes: directly attributable to source edits. Localised,
  proportional to the source delta, "essentially new" bytes; best handled by
  ordinary (non-delta) compression.
- **Second-order** changes: induced by first-order ones — every absolute
  address after an insertion and every relative address spanning it. "A single
  line of modified source code can cause up to 5–10 % of the bytes ... to be
  modified"; "efficient delta compression of executable code can be largely
  considered to be the problem of locating and compactly encoding these
  second-order changes."

binsync's measurements sharpen the numbers for Go (13 % for one line, 70 %
for a multi-package edit, because `.gopclntab` is one big offset table) but
the taxonomy is exactly right, and it generalises beyond executables: in any
*serialised structure with internal offsets* — a zip central directory, a
safetensors header, a SQLite page, a wasm module's LEB128 section sizes — a
first-order edit induces second-order churn. The generalised codec's job is
"predict the second-order change from the first-order change"; that is the
one-sentence statement of the project.

## Chapter 2: the encoder

1. **Block alignment** (§2.4). Index S with k = 2, L = 4√(n log n); split T
   into √(n log n)-byte blocks; find each block's best offset with mismatches;
   "tweak" boundaries forwards then backwards to reduce mismatches, snapping to
   the largest power-of-two boundary on ties (compilers align code on 8/16/32);
   drop blocks below a threshold or under 50 % matching.
   Cost O(m log n + n), memory O(n^{1/2+ε}). *This alone* gives patches only
   8 % larger than full bsdiff 6 on the security corpus (Table 2.2:
   1.27 % vs 1.38 %) — with sub-linear memory. That is a result binsync's plain
   codec (§3.8 of DESIGN.md, hash-indexed) should be measured against.
2. **Local alignment** (§2.5): suffix-sort S#T#, LCP, longest match per
   position in T; seeds extended while ≥ 50 % match. Needed for small matched
   regions; dominated by suffix sort time and O(√n) more memory than block
   alignment. Footnote 11 is the key limitation: an address table that
   mismatches in 1–2 of every 4 bytes has no exact match longer than 3 bytes
   and is invisible to a suffix array — exactly the pclntab/`.go.type` case.
3. **Combined** (§2.6): dynamic programming over a pruned graph — 64 candidate
   alignments per byte (31 from following longest matches, 31 from the
   shortest-path frontier, one from block alignment, one "unmatched"), edge
   costs 0/2 match/mismatch, 1 unmatched, 20 realignment.
4. **Delta encoding** (§2.7). Control string (varints); extra string
   (unmatched bytes, bzip2); *four* candidate difference strings — bytewise,
   little-endian multi-precision balanced, big-endian multi-precision, and a
   correction map — each tried and the smallest chosen; then the difference
   string is split into a **difference map** (0/1 per byte: *where* changes
   are; BWT + position enumeration + recursive midpoint arithmetic coding,
   because instruction encodings make change positions "distinctive and
   compressible") and a **non-zero difference string** (*by how much*; zlib,
   locally repetitive). "Birds of a feather compress better together."

Results (Table 2.1, Alpha "upgrade" corpus, 15 pairs): bsdiff 6 7.67 % vs
Exediff (platform-specific disassembler) 8.41 %, .RTPatch 10.88 %, zdelta
19.52 %, Vcdiff 19.66 %, Xdelta 20.83 %, bzip2 36.22 %. Security corpus
(Table 2.2, 82 FreeBSD files): bsdiff 6 1.27 %, .RTPatch 1.89 %, zdelta 4.00 %,
Xdelta 9.28 %. The spread widens from 3× to 7× when changes are small,
"reflecting the greater relative importance of second-order changes".

Conclusion the author draws (§2.9): a naïve method equals a platform-specific
one, so prefer naïve for simplicity, safety and portability. **binsync's
result is the counter-example**: with exact metadata (a function table), the
platform-aware predictor is 28–67× better than the naïve one, not 10 %. The
difference is that Exediff *disassembled*, while binsync *predicts the layout
from a table the toolchain shipped*. Percival's argument stands wherever no
such table exists, which is why the general codec needs both.

## Chapter 3: universal deltas — the idea worth carrying forward

Problem statement (§3.1): given S, radius R, failure probability ε, produce S′
smaller than S such that *any* T with d(S,T) < R reconstructs S from {T, S′}
with probability ≥ 1−ε. One patch for every old version; no per-target
negotiation; works for locally modified/rebuilt files.

Mechanisms:

- **Hamming distance**: send the syndrome S mod g(x) of a cyclic code (RS over
  GF(2⁸) or larger); the receiver decodes T − S′ to the nearest codeword.
  Cost for Nₛ substitutions of total length Lₛ: ≈ min(2Lₛ·⌈log₂₅₆|S|⌉,
  2Lₛ + 4Nₛ(⌈log₂₅₆|S|⌉−1)) bytes. Two-phase randomised variant (§3.3):
  random 255-byte subsequences with per-subsequence syndromes reduce the error
  count before one big-field code finishes; worked example 475,000 → 310,929 B
  for 10⁵ isolated errors in a 1 MB file.
- **Indels** (§3.4, "one-way rsync"): send block hashes + an RS parity block;
  receiver places matched blocks, marks the rest erasures, decodes. Patch
  ≈ Lₛ + Lᵢ + √(2|S|N·log₂₅₆(|T|²|S|N/ε²)).
- **Both** (§3.5–3.6): the FFT index of ch. 1 (about 10× the size of an rsync
  checksum block, but it matches blocks *with substitutions*), boundary
  adjustment against the transmitted projections, erasure marking, then the
  two-phase RS code. Patch ≈ 2Lₛ + 2Lᵢ + 2·log₂₅₆|S|·(Nₛ + 2√(|S|(N_d+Nᵢ)·log256)).

Results (Table 3.1, upgrade corpus): rsync −z 33.5 %, one-way rsync 78 %,
block-matching universal delta 52.5 % — against 7.67 % for a pairwise bsdiff 6.
So a universal patch costs ~7× a pairwise one on that corpus, and the author
notes it is "of little value where indels dominate".

### Why this matters for a *predictive* codec

The pairwise patch and the universal patch are the two ends of one axis, and
prediction moves a file along it. Once a predictor has rebuilt a length-exact
prediction P of the new file from the old one, the correction is
`new ⊖ P`, and P differs from `new` almost only by *substitutions* — that is
exactly what binsync measured (120,852 differing bytes in 93.8 MB, in runs a
few bytes long). Substitution-only residuals are the regime where the
error-correcting-code approach is strong and indel-tolerant matching is not
needed. Two concrete consequences:

1. **A syndrome instead of a positional correction** is a legitimate
   correction format when the residual is sparse and the decoder's prediction
   is *not guaranteed* to equal the encoder's — e.g. a predictor that is
   allowed to be non-deterministic across hosts (floating-point, thread
   scheduling), or targets whose old file is one of several near-identical
   builds. The encoder would measure the residual, choose R with margin, and
   the decoder could still verify by hash. Cost is ~2× the positional
   correction per differing byte (RS needs 2 parity symbols per error) plus
   the log factor; the gain is that the patch no longer names *which* old
   file it applies to. This is a module-level option, not the default.
2. **The block-alignment index is a predictor input.** For formats with no
   function table (stripped C/Rust binaries without `.eh_frame`, arbitrary
   blobs), the ch. 1 aligner produces a piecewise-constant shift map from a
   *sub-linear* index — the same object binsync's data maps are (per-16-byte
   block shift runs), obtained without metadata and without a suffix array.
   Memory O(n^{1/2+ε}) is the property binsync's decoder (§11.3 of DESIGN.md,
   7.6× the input) lacks.

## Three ideas to lift directly

1. **Try several difference encodings and keep the smallest** (§2.7): the
   correction stage should offer bytewise, LE-multiprecision, BE-multiprecision
   and correction-map modes per region, because the right one depends on the
   operand width and endianness the region holds. binsync uses only the
   correction map (positional literal runs). On mispredicted rel32 operands a
   multiprecision difference would be one or two small bytes instead of four.
2. **Separate *where* from *how much*** (§2.7): a positions stream and a values
   stream with different coders. binsync already splits ctrl/literal streams
   (DESIGN.md §3.4) for the same reason.
3. **Snap boundaries to power-of-two alignment on ties** (§2.4): a cheap prior
   that binsync's function-level alignment gets for free (32 B) and a
   metadata-free aligner must reconstruct.

## What the thesis does not do, that the general codec must

- No notion of *regenerating* a structure (a table, a header) from a model of
  the format; everything is alignment + correction on bytes. That is the
  Courgette/binsync step and the source of the 10× beyond bsdiff.
- No composition: one file, one aligner. Nothing about nested containers
  (archive → member), compressed members, or per-region choice among
  different models. That is the Roaring-style selection layer.
- No portable model: the encoder and decoder are the same program. The thesis
  argues for naïve methods *because* platform-specific ones are not portable —
  which is precisely the problem a portable predictor module format removes.
