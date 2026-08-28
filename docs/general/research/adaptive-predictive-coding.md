# Adaptive, multi-model predictive coding: theory and prior art

Research notes for generalising go-binsync's predict-then-correct codec into a
framework of pluggable domain predictors with per-region selection. Written
2026-08-27. Every number below is taken from the cited primary source unless
marked **[unverified]**; those are recollections that the fetched sources did
not confirm and should be checked before being relied on.

Contents

1. Prediction + residual as an architecture
2. Model selection and mixing: the theory
3. Region-wise container/encoding selection in practice
4. Cascading and composable transforms
5. Shipping the predictor: dictionaries, grammars, programs, LLMs
6. Rate/throughput economics of trial encoding
7. Comparison table: how selection is chosen and signalled
8. The MDL framing in one page
9. Assessment for a predictive codec

---

## 1. Prediction + residual as an architecture

Every system in this section has the same shape: a deterministic predictor
that both sides can run, a residual (actual minus predicted) that the encoder
transmits, and some amount of side information telling the decoder which
predictor to run where. They differ on three axes: how the predictor is chosen
(fixed, encoder trial, adaptive at the decoder), the granularity of the choice,
and how the residual is coded.

### 1.1 DPCM, lossless JPEG, LOCO-I / JPEG-LS

Lossless JPEG (1993) offers seven fixed spatial predictors, one chosen per
scan, so the choice is coarse and signalled once. LOCO-I, the algorithm behind
JPEG-LS, deliberately went the other way: one fixed nonlinear predictor and all
of the adaptivity moved into the *residual* model.

- Predictor: MED (median edge detector) — `median(W, N, W+N−NW)`, which picks
  W or N when it detects an edge, otherwise the planar estimate.
- Contexts: gradients `NE−N, N−NW, NW−W` each quantised to 9 levels, merged by
  sign symmetry to **365 contexts**; the paper says this number "balances
  storage" against context dilution.
- Bias cancellation: per context, an accumulated error `B[q]` and count `N[q]`
  give an integer correction `C[q]` added to the prediction. This is
  "adaptive prediction" without adaptive predictor *selection*: the predictor
  is fixed, the correction learns the per-context systematic error.
- Residual coding: Golomb-Rice with parameter `k` chosen per context from the
  running mean `A[q]/N[q]` of absolute residuals; plus a run mode for flat
  regions.
- Framing: LOCO-I is explicitly described as a "low complexity projection" of
  universal context modelling (CALIC being the high-complexity end). Speed on
  a Pentium II: about 1.5 MB/s on natural images.

Source: Weinberger, Seroussi, Sapiro, "The LOCO-I lossless image compression
algorithm: principles and standardization into JPEG-LS", IEEE TIP 2000,
https://www.sfu.ca/~jiel/courses/861/ref/LOCOI.pdf (also HP Labs HPL-98-193).

The design lesson: when the predictor family is small and the data is
stationary at the scale of a context, **fixed predictor + context-conditioned
residual correction** is nearly as good as choosing among predictors, and
needs zero signalling. This is the alternative go-binsync should measure
against before adding a per-region selector.

### 1.2 PNG filters: per-scanline selection by heuristic

PNG has five filters (None, Sub, Up, Average, Paeth), one filter-type byte per
scanline, chosen freely by the encoder
(https://www.w3.org/TR/png-3/). libpng's adaptive strategy tries all five
on each row and keeps the one with the **minimum sum of absolute filtered
values** (bytes treated as signed), a heuristic proposed by Lee Daniel Crocker
in 1995 (http://www.libpng.org/pub/png/book/chapter09.html). The heuristic is a
proxy for the residual's entropy under the downstream DEFLATE stage, not the
real cost.

How good is the proxy? OptiPNG's experiment
(https://optipng.sourceforge.net/pngtech/better-filtering.html): brute-force
search over filter/zlib parameters vs minsum gave a negligible difference on a
photograph (475,442 → 475,430 bytes) but 30–33 % on synthetic images with few
colours (377,522 → 251,865; 152,546 → 106,765). The proxy fails exactly where
the residual is far from an i.i.d. Laplacian — where downstream LZ matching,
not per-symbol entropy, dominates. Relevant to us: a byte-level correction
stream that is mostly "match against prediction" is the second kind of data.

### 1.3 FLAC / ALAC: per-block predictor order chosen by trial

FLAC (RFC 9639, https://www.rfc-editor.org/rfc/rfc9639.html) codes each
channel block (16–65535 samples; streamable subset ≤16384, ≤4608 at ≤48 kHz)
as one subframe of type CONSTANT, VERBATIM, FIXED (order 0–4, fixed integer
predictors) or LPC (order 1–32, coefficients transmitted). The type is a
6-bit field in the subframe header; LPC additionally sends precision, shift
and quantised coefficients. The residual is Rice-coded in 2^k partitions
(k is a 4-bit field), each with its own 4- or 5-bit Rice parameter, with an
escape to fixed-width residuals.

The spec says nothing about how to choose; the reference encoder estimates
the best LPC order from residual energy by default, and `-e` ("exhaustive
model search") "forces the encoder to evaluate all order models and select
the best", which the docs call expensive
(https://xiph.org/flac/documentation_tools_flac.html). The same encoder tries
several apodisation windows (up to 32) and, with `-p`, neighbouring
coefficient precisions. This is textbook encoder-side trial with per-block
signalling; decoder cost is identical whichever order wins.

### 1.4 Floats: fpzip, SZ, ZFP

- fpzip (Lindstrom & Isenburg, IEEE TVCG 2006,
  https://computing.llnl.gov/projects/fpzip) uses a fixed Lorenzo predictor
  over the n-dimensional neighbourhood, maps predicted and actual floats to
  integers, and range-codes the residual with a quasi-static model. No
  selection at all.
- SZ (Di & Cappello, IPDPS 2016) predicts each point with the best of three
  curve-fitting models (preceding-neighbour, linear, quadratic) and codes
  which one, falling back to a quantised "unpredictable" representation.
  **[unverified: I recall the per-point selector is a 2-bit code; the fetched
  abstracts confirm the three-model best-fit design but not the bit
  width.]** Note the granularity: per *value*, with the selector itself then
  entropy-coded — a much finer grain than any block scheme.
- ZFP is a transform coder (block orthogonal transform + embedded bit-plane
  coding), not predictive; it is the counterexample that a transform can
  beat prediction when the data is smooth.

### 1.5 Video: intra/inter mode decision by RDO

HEVC has 35 intra modes per prediction unit; the reference encoder uses a
three-step rough-mode-decision: rank all 35 by a cheap Hadamard (SATD) cost,
keep N candidates, then run full rate-distortion optimisation on those. In
*lossless* HEVC there is no transform, so the SATD proxy is replaced by SAD
against the prediction ("Lossless Intra Coding in HEVC with Adaptive 3-tap
Filters", https://arxiv.org/pdf/1604.07051; and RDO-cost-prediction papers
such as https://ieeexplore.ieee.org/document/8401532/). The pattern —
**cheap proxy to prune, real cost to decide** — recurs in BtrBlocks (§3.3)
and is the right structure for our encoder (§9e).

### 1.6 JPEG XL modular mode: the selector is a decision tree in the stream

JPEG XL's modular mode (Sneyers, "JPEG XL's Modular Mode Explained",
https://cloudinary.com/blog/jpeg-xls-modular-mode-explained) generalises PNG
filters in three ways:

1. ~14 predictors (Zero, W, N, NW, NE, averages, Paeth-like "Select",
   Gradient, and the *weighted / self-correcting* predictor).
2. The weighted predictor runs four parameterised sub-predictors, tracks
   each one's error on the previous row, and outputs an error-weighted
   average — LOCO-I-style bias cancellation generalised to online mixing
   of predictors, with no signalling.
3. An **MA (meta-adaptive) tree** is signalled per image. Interior nodes
   test properties of the causal neighbourhood (sample values, differences,
   position, channel, and `max_error` of the weighted predictor); each leaf
   names a predictor, an entropy-coding histogram (shareable between
   leaves), and a multiplier/offset for the residual. So the predictor is
   chosen *per context*, not per block, and the choice is a function the
   decoder evaluates rather than a table it reads.

Tree overhead is small — JXL-art images with a hand-written tree come in at
23–124 bytes total. The encoder learns the tree from the image (higher
effort levels explore more predictor/property combinations; low effort uses
a fixed small tree). The design paper (arXiv:2506.05987, 73 pp.) has more on
the tree learner; it is too large to fetch here, so the learner's algorithm
is **[unverified]** beyond the blog's description.

### 1.7 WebP lossless: per-tile predictor mode chosen by residual entropy

Spec: https://developers.google.com/speed/webp/docs/webp_lossless_bitstream_specification.
The predictor transform has 14 modes. Tile size is `1 << size_bits`,
`size_bits = ReadBits(3) + 2`, so tiles are 4×4 up to 512×512 pixels. The per-tile mode is stored in the green channel of a
subresolution image, which is itself entropy-coded — signalling cost is
therefore data-dependent and small when neighbouring tiles agree. Other
transforms (colour transform, subtract-green, colour-indexing) each may be
used once, and are undone in reverse order of reading.

The encoder (libwebp `src/enc/predictor_enc.c`,
https://github.com/webmproject/libwebp/blob/main/src/enc/predictor_enc.c)
tries all 14 modes per tile and minimises
`PredictionCostSpatialHistogram` = combined Shannon entropy of the tile's
residual histogram *added to the accumulated histogram so far* (so the cost
is the marginal cost under the shared entropy coder, "favor low entropy,
locally and globally"), plus a bias toward small values, minus a fixed
`kSpatialPredictorBias` if the mode equals the left or above tile's mode.
Low-effort encoding skips the search and uses mode 11 everywhere. Two
things to steal: (i) the cost is *marginal* against the shared coder's
state, not standalone; (ii) an explicit hysteresis term makes the selector
prefer not to switch, which cheapens the mode map.

---

## 2. Model selection and mixing: the theory

### 2.1 Two-part codes and stochastic complexity

Rissanen's MDL (1978): choose the model minimising `L(M) + L(D | M)`, the bits
to describe the model plus the bits to describe the data given the model.
That is a *two-part code*, and it is exactly what a predictive codec does when
it ships a selector table plus residuals. Stochastic complexity (Rissanen 1986)
and the normalised maximum likelihood (NML/Shtarkov) code remove the
redundancy of the two-part split by coding data and parameters jointly; the
resulting `parametric complexity` term is `(k/2) log n + O(1)` for a
k-parameter smooth family — the unavoidable price of not knowing the
parameters. Practical consequence for us: the cost of *one* selector choice
among |M| modules is `log2 |M|` bits if uniform, less if the choice is
predictable from context, and this must be compared against the residual
saving it buys. There is no free lunch in the number of modules: each extra
candidate raises `L(M)` for every region that could have used it, even
regions that don't.

Reference: Grünwald, *The Minimum Description Length Principle* (MIT Press
2007); Rissanen, "Modeling by shortest data description" (Automatica 1978);
the Wallace & Dowe comparison of MDL and MML
(https://users.monash.edu/~dld/Publications/1999/WallaceDowe1999bRefinementsOfMDL_AndMML_Coding_ComputerJVol42No4pp330-337.pdf).

### 2.2 Bayesian mixing vs switching; the catch-up phenomenon

Mixing (Bayesian model averaging, CTW) codes with the weighted mixture
`ξ = Σ w_i ρ_i`. The redundancy against the single best model is at most
`−log2 w_i` bits — e.g. `log2 |M|` for a uniform prior — for the *whole
sequence*. This is the cheapest possible way to "choose" one model, but only
one: if the best model changes partway, a static mixture's posterior has to
catch up, and it does so slowly.

Van Erven, Grünwald & de Rooij, "Catching up faster by switching sooner"
(JRSS-B 2012; arXiv:0807.1005, https://arxiv.org/abs/0807.1005) name this the
*catch-up phenomenon* and show that Bayesian model averaging can converge
at rate `n^{-2/3}(log n)^{2/3}` where the minimax rate is `n^{-1}` in some
nonparametric settings, i.e. BMA is genuinely suboptimal when the best
model changes with sample size. Their **switch distribution** is a mixture
over sequences of models with a prior on switch points; its regret against
any switching sequence with `m` switches is `O(m · log n)` (their eq. 29–31),
it is consistent, and it is computable in time linear in the number of
models per step via a hidden-Markov formulation (their Section 7). They
relate it to Herbster & Warmuth's Fixed Share (1998) and Volf & Willems's
switching method (DCC 1998).

Veness, Ng, Hutter & Bowling, "Context Tree Switching" (DCC 2012,
https://arxiv.org/abs/1111.3182) give the clean bound for a decaying switch
rate `α_t = 1/t` (their Theorem 1):

    −log2 τ(x_1:n) ≤ min over i_1:n of
        (m(i_1:n) + 1)·(log2 |M| + log2 n) − log2 ρ_{i_1:n}(x_1:n)

That is: **each switch costs about `log2 |M| + log2 n` bits**, over the cost
of the models themselves. Replacing CTW's weighting with switching gave
consistent small gains on Calgary (up to 7–8 % smaller files, never more
than 1 % worse); weighted-average bits/byte: PPM* 2.09, CTW 1.99, PPMZ 1.93,
CTS* 1.93, DEPLUMP 1.89. Koolen & de Rooij, "Universal codes from switching
strategies" (IEEE TIT 2013, https://arxiv.org/abs/1311.6536) give a general
HMM language for such expert-tracking codes, including priors where switches
cluster; Herbster & Warmuth's Fixed Share bound is `≈ n·H(α*, α) + m·log|Ξ|`
where `α*` is the true switch rate.

### 2.3 What this means for hard per-region selection

A hard selector that names one module per region is a *degenerate two-part
code*: an explicit switching sequence, with `L(M)` = the bits to write the
sequence. Compare:

| scheme | cost above the best per-region model |
|---|---|
| explicit selector, uniform code | `R · log2 |M|` bits for R regions, regardless of data |
| explicit selector, context-coded | `≈ Σ −log2 P(choice | previous choices)`, ≪ `R log2 |M|` when choices are sticky |
| switch distribution (decoder-side, no signalling) | `(m+1)(log2 |M| + log2 n)` for the best sequence with m switches, plus per-symbol mixing cost |
| static Bayesian mixture | `log2 |M|` once, but cannot track changes |

So hard selection is *nearly as good as* adaptive switching when (a) regions
are long enough that `log2 |M|` per region is negligible against the
residual, and (b) the encoder can actually find the best module (it has the
data; the decoder-side switch distribution has to learn it from the past).
Hard selection is *better* than any decoder-side scheme when the module cost
is dominated by a one-off structural fact (this region is a pclntab, this
block is a run) rather than by gradually drifting statistics — because the
switch distribution pays `log n` per switch to *discover* what the encoder
could simply *state* for `log2 |M|`. Mixing wins when several models are each
partially right at the same position (PAQ's regime), and that is exactly what
a positional byte-diff prediction is not: the prediction is either right or
wrong at each byte.

### 2.4 Context mixing (PAQ, cmix) and online expert advice

PAQ mixes many bit-predictors in the logistic domain,
`p = squash(Σ w_i · stretch(p_i))`, updates weights by gradient on coding loss,
and *selects the weight set by a small context* (Mahoney, "Adaptive weighing
of context models" 2005; overview at https://mattmahoney.net/dc/dce.html and
https://en.wikipedia.org/wiki/Context_mixing). The result is refined by an
adaptive probability map (SSE/APM). Veness et al.'s Gated Linear Networks
(https://arxiv.org/abs/1712.01897) recast this: hashing picks one neuron per
layer, which is a hard gate on *which mixer* runs — mixing inside, selection
outside. cmix v21 stacks 2,077 models with an LSTM byte mixer, reaches about
1.17 bits/byte on enwik8, needs ≥32 GB RAM and runs at roughly 0.5–5 KB/s
(https://www.byronknoll.com/cmix.html; LTCB
https://www.mattmahoney.net/dc/text.html). This is the cost of mixing at
scale: 3–5 orders of magnitude slower than zstd, on both sides.

Cover's universal portfolios and exponential-weights/Hedge are the same
mathematics (log loss ↔ wealth); the compression literature's "expert
advice" results (Koolen & de Rooij above) are the ones with explicit
switching priors, which is the part that matters here.

---

## 3. Region-wise container/encoding selection in practice

### 3.1 Roaring bitmaps

Chambi, Lemire, Kaser, Godin, "Better bitmap performance with Roaring
bitmaps" (SP&E 2016, https://arxiv.org/abs/1402.6407) and Lemire et al.,
"Consistently faster and smaller compressed bitmaps with Roaring"
(SP&E 2016, https://arxiv.org/abs/1603.06549). The 32-bit space is split
into 2^16 chunks of 2^16 values; each chunk is a container:

- array: sorted uint16s, `2·card` bytes, used when `card ≤ 4096`;
- bitmap: 1024 × 64-bit words, always 8192 bytes, used when `card > 4096`
  (the threshold is where the two sizes cross: 4096 × 2 B = 8 KB);
- run: `(start, length−1)` pairs, `2 + 4r` bytes for r runs, introduced in
  the second paper, created only by an explicit `runOptimize()` pass, and
  "only allowed to exist if it is smaller than either the array container or
  the bitmap container" — for `card > 4096` that means `r ≤ ⌈(8192−2)/4⌉ =
  2048` runs.

Signalling (https://github.com/RoaringBitmap/RoaringFormatSpec): a cookie
says whether run containers may occur; if so, a bitset of **one bit per
container** flags the run ones; a descriptive header of 4 bytes per
container carries key and `cardinality − 1` (which, with the run bit,
determines array vs bitmap); an offset table is omitted for small bitmaps.
Cost model: pure size, closed-form, evaluated per container, no sampling,
with the run type gated behind an explicit optimisation call because
run containers are slower for some operations. The comparison the author
draws with go-binsync is apt with one caveat: Roaring's three encodings have
*exact* size formulas from two statistics (cardinality, run count), so
"choose by measured size" is free. Ours will not have closed forms; the
selection cost is a real design variable (§6, §9e).

### 3.2 Parquet / ORC / Arrow writers

Parquet (https://github.com/apache/parquet-format/blob/master/Encodings.md)
signals the encoding per *page* in the page header. Encodings: PLAIN,
RLE_DICTIONARY, RLE, DELTA_BINARY_PACKED, DELTA_LENGTH_BYTE_ARRAY,
DELTA_BYTE_ARRAY, BYTE_STREAM_SPLIT. Writers do not search; they apply
rules. The one that matters is the dictionary fallback: the writer starts
every column chunk dictionary-encoded and, "if the dictionary grows too
big, whether in size or number of distinct values, the encoding will fall
back to the plain encoding" for the rest of the chunk. arrow-rs exposes this
as `dictionary_page_size_limit` and calls the alternative the "fallback
encoding" (https://docs.rs/parquet/latest/parquet/file/properties/struct.WriterProperties.html);
the default limit is 1 MiB **[unverified: the fetched doc page did not
state the default]**. BtrBlocks' paper characterises Parquet's approach as
"hard-coded, implementation-specific rules" with a small scheme set, and
measures the consequence (§3.3). ClickHouse likewise leaves the choice to
the schema: `CODEC(Delta, ZSTD)` per column, with specialised codecs
(Delta, DoubleDelta, GCD, Gorilla, FPC, T64, ALP, SZ3) meant to be followed
by a general-purpose one (LZ4, ZSTD)
(https://clickhouse.com/docs/sql-reference/statements/create/table). It does
not select automatically.

### 3.3 BtrBlocks: sample, try everything, cascade

Kuschewski, Sauerwein, Alhomssi, Leis, "BtrBlocks: Efficient Columnar
Compression for Data Lakes" (SIGMOD 2023; PDF at
https://www.cs.cit.tum.de/fileadmin/w00cfj/dis/papers/btrblocks.pdf; code
https://github.com/maxi-k/btrblocks). This is the closest published analogue
to "pick the best of N encodings per block".

- Blocks of 64,000 values per column. Scheme pool: RLE, One Value,
  Dictionary, Frequency, FOR + bit-packing, PFOR, FSST (strings),
  Pseudodecimal (doubles), with recursion into the outputs.
- Selection: (1) one pass of statistics (min, max, unique count, average
  run length); (2) rule-based exclusion (RLE if average run < 2; Frequency
  if ≥ 50 % of values unique; Pseudodecimal if > 50 % exceptions or < 10 %
  unique values, the latter because dictionaries decompress much faster);
  (3) compress a **sample** with every surviving scheme and (4) take the
  highest estimated ratio.
- Sample: **10 runs of 64 consecutive values = 1 % of the block**, spread
  over non-overlapping parts. Random single tuples or one contiguous range
  performed worst; multiple small runs across the block best (their Fig. 5,
  N = 640). This costs **1.2 % of compression time**, picks the optimal
  scheme (or one within 2 % of it) **77 % of the time**, and the resulting
  files are **3.3 % larger than the oracle choice on average** (their §6.3).
- Cascade: after a scheme runs, each of its outputs (e.g. RLE's values and
  run-lengths) is itself put through the selector, to a **maximum
  recursion depth of 3**; beyond that, raw. Each scheme records which scheme
  it cascaded into, and decompression applies them in reverse.
- Trade-off statement, verbatim in spirit: more schemes → slower
  compression (more samples evaluated) and better ratio; heavyweight
  schemes → better ratio, slower decompression. The pool was grown
  empirically: find columns that compress badly, look at them, add a scheme,
  prune.
- Results (Public BI): compression factor 7.06× vs Parquet+Snappy 6.88× and
  Parquet+Zstd 8.24×; compression from in-memory 75.3 MB/s vs 41.0 MB/s for
  Parquet+Zstd; decompression 8.9 GB/s (doubles) and 11.8 GB/s (integers)
  per core, dictionary strings 19.6 GB/s. It beats every proprietary column
  store tested and everything open except Parquet+Zstd on ratio, while
  decompressing an order of magnitude faster.

### 3.4 zstd's own block-level choices

zstd (https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md)
makes Roaring-style choices at two levels with a 2-bit field each: block
type Raw / RLE / Compressed (3-byte header; blocks ≤ 128 KiB), and inside a
compressed block the literals section is Raw / RLE / Huffman-with-tree /
Huffman-reusing-previous-tree. The encoder computes both candidate sizes and
takes the smaller; the "treeless" option is a *shared-state* choice — reuse
the previous model — which is the cheap way to make consecutive regions
share a residual coder.

### 3.5 Archive filter chains: xz, 7-zip, RAR5, Precomp

- xz (https://tukaani.org/xz/xz-file-format.txt): a block's filter chain is
  1–4 filters (2-bit count in Block Flags), the last must be LZMA2 (or
  another non-BCJ filter), and among the up-to-3 non-last filters at most
  two may change data size; a size-changing non-last filter "SHOULD produce
  at least n bytes of output when given 2n bytes of input". The stated
  rationale is denial of service: without a bound, one filter could expand
  massively while the next consumes it and outputs little. Filters: Delta
  (0x03), BCJ x86/PowerPC/IA64/ARM/ARM-Thumb/SPARC/ARM64/RISC-V
  (0x04–0x0B), LZMA2 (0x21). Selection is by the user or by file-type
  guess; xz does not trial.
- 7-zip BCJ2 splits x86 into four streams (main, call targets, jump targets,
  and a range-coded selector stream) so the branch-target stream gets its
  own LZMA with a small dictionary (512 KB suffices per the manual); 7-Zip
  uses BCJ2 in Ultra mode and BCJ otherwise for executables; the section
  size was raised from 64 to 240 MiB to help large executables
  (https://sevenzip.osdn.jp/chm/cmdline/switches/method.htm). No published
  percentage gain in the sources fetched; **[unverified]** folk figure is
  5–10 % on x86 code.
- RAR5 (unrar `unpack.hpp`,
  https://github.com/aawc/unrar/blob/master/unpack.hpp): filters are
  per-block records `{Type, Channels, BlockStart, BlockLength}` embedded in
  the compressed stream, `MAX_UNPACK_FILTERS = 8192` per window and
  `MAX_FILTER_BLOCK_SIZE = 0x400000` (4 MiB). Types Delta, E8, E8E9, ARM
  **[unverified: from memory of `unpack50.cpp`; the header fetched only
  shows the struct and limits]**. RAR3 shipped filters as VM programs
  (`VM_PreparedProgram`), i.e. the predictor *was* a program in the stream;
  RAR5 dropped the VM for a fixed set — worth remembering when tempted by
  "ship the predictor as code".
- Precomp/preflate (https://github.com/schnaader/precomp-cpp,
  https://github.com/deus-libri/preflate): detect embedded deflate streams,
  inflate them, and store the *reconstruction information* needed to
  re-deflate bit-exactly. For zlib-produced streams that is three
  parameters; preflate generalises to arbitrary deflate by storing a diff
  against its own re-encode, tested on ~10,000 streams. This is
  predict-then-correct applied to a *compressor*: the prediction is "zlib
  at level L would have produced these bits", the correction is the diff.
  Detection uses a cheap probe (zlib on the first bytes) before the
  expensive path — the proxy-then-commit pattern again.

### 3.6 Damme et al.: no single best lightweight scheme

"Lightweight Data Compression Algorithms: An Experimental Survey" (EDBT 2017)
and the TODS 2019 follow-up "From a Comprehensive Experimental Survey to a
Cost-based Selection Strategy for Lightweight Integer Compression Algorithms"
(https://dl.acm.org/doi/10.1145/3323991) evaluate cascades of delta,
frame-of-reference, null-suppression and dictionary techniques and conclude
"there is no single-best algorithm"; the winner depends on distribution,
sort order, distinct count and hardware. Their selection strategy is a
*grey-box cost model*: explicit knowledge of each algorithm plus a small
number of calibration measurements per algorithm per machine to capture
data/hardware effects. That is the alternative to BtrBlocks' black-box
sampling: model the cost instead of measuring it. It works when the
algorithms' behaviour is a smooth function of a few statistics; it is
fragile for anything LZ-like.

---

## 4. Cascading and composable transforms

Systems where one module's output feeds another: xz filter chains, 7-zip
method chains, ClickHouse codec chains, BtrBlocks recursive cascades, zpaq's
model stack (a ZPAQL program in the archive header specifies the whole
preprocess + context-model pipeline), WebP's transform list, Precomp's
recursion into streams inside streams.

Constraints these systems impose, and why:

1. **Acyclic, bounded depth.** BtrBlocks depth ≤ 3 (then raw); xz ≤ 4
   filters; WebP each transform at most once. Depth bounds cap decoder work
   and make the grammar of the stream finite.
2. **Expansion bounds on every intermediate.** xz's 2n→n rule and the "at
   most two size-changing filters" rule exist specifically to prevent
   decompression bombs across a chain. RAR5 caps a filter block at 4 MiB and
   the number of filters per window.
3. **Terminal stage is a fixed general-purpose coder.** xz requires LZMA2
   last; ClickHouse documents specialised codecs as things you put *before*
   LZ4/ZSTD; BtrBlocks' leaves are bit-packing/raw. The general coder is
   where cross-module redundancy gets picked up.
4. **Explicit reverse-order application, recorded in the stream.** Every
   system stores the chain and the decoder undoes it back to front. Nobody
   infers the chain.
5. **Length-exactness or explicit lengths at every stage.** Positional
   correction (go-binsync §3.3–3.4 of DESIGN.md) depends on the prediction
   having the exact output length; xz block headers carry compressed and
   uncompressed sizes; RAR filter records carry `BlockLength`. A module
   whose output length is not known before it runs breaks positional
   composition and must declare it.
6. **Canonical serialisation of the choice.** Roaring's `runOptimize` is
   deterministic given the container; zstd picks the smaller of two exact
   sizes. Where the choice is a sampled estimate (BtrBlocks) the *result*
   is still canonical because the choice is written down; only
   reproducibility of the encoder is lost, which nobody promises.

---

## 5. Shipping the predictor: dictionaries, grammars, programs, LLMs

The Kolmogorov view: the best code for `x` is the shortest program that
prints it, uncomputable in general (Mahoney,
https://mattmahoney.net/dc/dce.html, §1). Every practical scheme is a
restricted program family plus a data part. Ordered by how much "program"
they ship:

- **Preset dictionaries** (DEFLATE `setDictionary`, zstd `--train`, Shared
  Brotli RFC 9841). A dictionary is a predictor of the form "the file
  begins by resembling this blob": pure LZ77 prefix plus, for zstd,
  pre-trained entropy tables. zstd's default trained dictionary is 112,640
  bytes (https://github.com/facebook/zstd/blob/dev/programs/zstd.1.md);
  on 1,000 GitHub-user JSON records (~850 KB) a 65,599-byte dictionary
  took the ratio from 2.8× to 6.9× (122 KB vs ~300 KB)
  (https://engineering.fb.com/2016/08/31/core-infra/smaller-and-faster-data-compression-with-zstandard/),
  and the gain "is mostly effective in the first few KB" of each input.
  Shared Brotli allows multiple LZ77 dictionaries up to the window size, up
  to 64 custom word lists and 64 transform lists, referenced by a 256-bit
  HighwayHash (https://datatracker.ietf.org/doc/html/rfc9841). Dictionaries
  are the weakest form of the idea: they predict *content*, not *structure*,
  and they cannot express "this table is a function of that one".
- **Grammar-based codes** (Sequitur, Re-Pair, Kieffer & Yang's universal
  grammar codes, https://www.mit.edu/~6.454/www_fall_2002/emin/kieffer_and_yang.pdf):
  the "program" is a straight-line context-free grammar; universal for
  finite-state sources. Structurally identical to LZ78-style dictionaries
  with hierarchy; useful for repeated substrings, blind to arithmetic
  relations (offsets, relocations), which is what dominates binaries.
- **Executable transforms as fixed programs**: BCJ/E8E9/ARM filters and
  Courgette. These *are* domain predictors — "a call operand is an absolute
  address; predict it will be the same after relocation". They ship no code
  because the transform is standardised; the price is one filter ID per
  block. go-binsync's Go-aware prediction is the same species with a much
  larger side table (function layout) and a much better prediction.
- **Programs in the stream**: RAR3 VM filters, zpaq's ZPAQL. Both prove it
  is implementable; RAR abandoned it (RAR5 has fixed filters) and zpaq
  stayed niche. The recurring reasons are attack surface, determinism
  across implementations, and that the useful program set is small enough
  to standardise. **[unverified: reasons are inference, not a cited
  post-mortem.]**
- **Learned models as predictors**: Delétang et al., "Language Modeling Is
  Compression" (DeepMind 2023, https://arxiv.org/abs/2309.10668). With
  arithmetic coding over 2048-byte chunks, raw compressed sizes as a
  percentage of input: enwik9 — gzip 32.3, LZMA2 23.0, Chinchilla 70B
  **8.3**; ImageNet patches — PNG 58.5, LZMA2 57.9, Chinchilla 70B
  **48.0**; LibriSpeech — FLAC 30.3, Chinchilla 70B **21.0**. But the
  two-part *adjusted* rate that counts the parameters is 14,008 % for
  Chinchilla 70B on a 1 GB dataset; only the tiny trained Transformers
  (200K–3.2M params, adjusted 17.7–30.9 % on enwik9) are net wins. The
  paper does not report throughput; a 70B forward pass per 2048-byte chunk
  is many orders of magnitude slower than any codec here **[speed figure
  unverified; the qualitative point is not in doubt — cf. cmix at 0.5–5
  KB/s with a far smaller model]**. Learned image compression is the
  lossy cousin and is out of scope except as the reminder that "learned
  predictor" and "shippable predictor" are different things.

The honest reading: the whole gain of a domain predictor comes from
*structure the compiler/linker left behind that a byte-string model cannot
see* (go-binsync: 67× vs bsdiff on a one-line change). Dictionaries and
grammars see strings; LLMs see everything but cost 10^5× more; fixed
structural transforms are the sweet spot, and a pluggable module system is
a way to have many of them without a VM.

---

## 6. Rate/throughput economics of trial encoding

- Encoder-side trial is the norm (PNG, FLAC `-e`, WebP, HEVC RDO,
  BtrBlocks); the decoder always runs exactly one path per region. The
  asymmetry is already extreme in plain LZ codecs: lzbench (Silesia, one
  core, https://github.com/inikep/lzbench) — zstd -1: 422 MB/s compress,
  1347 MB/s decompress, 34.6 % of input; zstd -5: 125 / 1197 MB/s, 29.7 %;
  zstd -22: **2.08** / 1073 MB/s, 24.7 %; xz -9: 2.57 / 123 MB/s, 23.0 %;
  brotli -11: 0.58 / 389 MB/s, 23.8 %. Encoders routinely spend 200× more
  than decoders for the last 10 % of size. go-binsync's own measurement is
  in the same shape (brotli-10 13 s encode / 0.27 s decode on 94 MB).
- Cost of trying N candidates per region is `N × (predict + estimate)`. The
  three ways systems cut it: (1) sample — BtrBlocks' 1 % sample at 1.2 %
  overhead, losing 3.3 % of size; (2) proxy cost — PNG minsum, HEVC SAD,
  WebP histogram entropy, all O(region) and cheaper than a real encode;
  (3) rule-based pruning from cheap statistics before any trial — BtrBlocks
  excludes schemes on average-run-length and unique-ratio, and Precomp
  probes the first bytes before inflating.
- When the candidate predictors have wildly different costs (a Go-pclntab
  regenerator vs "copy old bytes"), the trial order should be cheapest
  first with early exit when the cheap one is already at the floor — a
  branch-and-bound on size, which none of the block systems need because
  their schemes are all cheap.
- Sampling is unreliable for schemes whose gain is non-local (LZ matching,
  anything with a dictionary): a 64-value run cannot show that a value
  repeats 10,000 values later. BtrBlocks gets away with it because its
  schemes are all local statistics; ours are not, so sampling should be
  used to *prune*, and the real residual size to *decide* (§9e).

---

## 7. Comparison: how predictor selection is chosen and signalled

| system | candidates | region / granularity | chosen by | signalling | residual coder |
|---|---|---|---|---|---|
| Lossless JPEG | 7 fixed predictors | per scan | encoder, once | header field | Huffman |
| JPEG-LS / LOCO-I | 1 predictor (MED) + 365-context bias correction | per sample, implicit | decoder-side adaptive (no choice) | none | per-context Golomb-Rice + run mode |
| PNG | 5 filters | scanline | encoder trial, min-sum-abs proxy | 1 byte/row (then DEFLATE'd) | DEFLATE over all rows |
| FLAC | CONSTANT/VERBATIM/FIXED 0–4/LPC 1–32 | block (16–65535 samples) | residual-energy estimate; `-e` exhaustive trial | 6-bit type + LPC params | Rice, 2^k partitions each with own parameter |
| fpzip | 1 (Lorenzo) | none | none | none | range coder |
| SZ | 3 curve fits + fallback | per value | best fit per value | per-value code, entropy-coded | quantised / bit-analysed |
| HEVC (lossless) | 35 intra modes + inter | prediction unit | SAD prune → RDO on N | CABAC-coded mode, predicted from neighbours | CABAC residual |
| WebP lossless | 14 predictors | tile 4×4…512×512 (3-bit size) | exhaustive, marginal residual-histogram entropy + neighbour hysteresis | subresolution mode image, entropy-coded | shared Huffman groups |
| JPEG XL modular | ~14 predictors + weighted self-correcting | per context (leaf of MA tree) | encoder learns tree | MA tree per image (tens of bytes upward) | per-leaf histogram, shareable |
| Roaring | array / bitmap / run | 2^16-value container | closed-form size; run only via `runOptimize` | 1 bit/container (run) + cardinality (array vs bitmap) | n/a |
| Parquet | 7 encodings | page / column chunk | writer rules; dictionary → plain fallback on size | page header enum | page-level general codec |
| ClickHouse | codec chain | column (schema) | human | schema | LZ4/ZSTD last in chain |
| BtrBlocks | ~8 schemes, recursive | 64k-value block, cascade depth ≤ 3 | stats-based pruning, then 1 % sample trial, best estimated ratio | scheme code per (sub)block | bit-packing/raw leaves |
| zstd | raw / RLE / compressed; literals raw/RLE/Huffman/treeless | block ≤ 128 KiB | exact size comparison | 2-bit fields | FSE/Huffman |
| xz | ≤ 4 filters, LZMA2 last | block | user / file-type | filter list in block header | LZMA2 |
| RAR5 | delta/E8/E8E9/ARM filters | filter block ≤ 4 MiB | encoder heuristics (undocumented) | filter records in-stream | RAR LZ + PPM |
| Precomp/preflate | deflate parameter sets | detected stream | probe then reconstruct | params or diff | LZMA2 |
| CTW / CTS / switch distribution | all models, weighted or switched | per symbol | decoder-side Bayes, no signalling | none | arithmetic |
| PAQ / cmix | hundreds–thousands of models | per bit | logistic mixing, mixer weights selected by context | none | arithmetic |
| go-binsync v1 | Go-aware predictor; zstd / brotli / raw per blob | whole binary; 8 MiB blobs | fixed; smaller-of for blobs | codec byte per blob | positional regions → brotli-10 |

---

## 8. The MDL framing in one page

A predictive codec sends `L(patch) = L(selector) + Σ_regions L(side_r) +
L(residual)`, where `side_r` is the module-specific side table for region
`r` (go-binsync's function layout table is one big `side`) and the residual
is coded by a shared coder conditioned on everything the decoder has
reconstructed so far. Three facts from §2 shape the design.

1. **Selection is a two-part code; its price is the selector's entropy,
   not `log2 |M|` per region.** A context-coded selector whose choices
   are sticky (the same module for consecutive regions, or determined by
   the container's own metadata such as ELF section type) costs a fraction
   of a bit per region. WebP's subresolution mode image and JPEG XL's tree
   are both ways to make the selector cheap; HEVC predicts the mode from
   neighbours. What must be avoided is `R · log2 |M|` for small `R`-sized
   regions — the regime where the switch distribution's `log n`-per-switch
   bound would actually be competitive.

2. **The regret bound is the budget for a region.** Switching theory says a
   perfect adaptive decoder pays about `log2 |M| + log2 n` bits per change
   of model. An explicit selector pays about `log2 |M|` (or less) per change
   and needs no learning. Therefore an explicit region is worth creating
   only if the residual saved exceeds `−log2 P(choice) + L(side_r) +
   L(region boundary)`. A boundary costs a varint or two; a side table can
   cost anything. This is the inequality the encoder's cost model (§9e)
   should literally evaluate.

3. **Mixing is for simultaneous partial correctness; selection is for
   structural certainty.** BMA's `log2 |M|` once-only price is unbeatable
   *when one model is best throughout*; the catch-up phenomenon is the
   proof that it is beatable otherwise. A byte-diff prediction at a given
   offset is right or wrong, and which module is right is a function of
   file structure known to the encoder, so selection dominates. The one
   place mixing-like behaviour pays is *inside* a module — LOCO-I/JPEG XL
   style bias correction of a predictor from its own past errors, which
   needs no signalling and is the cheapest adaptivity there is.

Corollary on module count: adding a module never raises the residual but
always raises the selector's alphabet; with a context-coded selector the
cost is `≈ −log2 P(unused module) ≈ 0` per region for modules that are
never chosen, so the alphabet can be large *provided the prior is learned or
skewed*. With a uniform fixed-width code it cannot. Use a prior.

---

## 9. Assessment for a predictive codec

### (a) Hard per-region selection vs mixing

Hard selection. The evidence: every high-throughput system in §3 selects;
the systems that mix (PAQ, cmix, CTW) are 10^2–10^5× slower on *both* sides
and gain mostly on data where several weak models are simultaneously
informative. Our modules are strong, structural, and mutually exclusive
(a region is a pclntab or it is not). Switching theory (§2.2) quantifies the
gap: an oracle selector written into the stream costs ≈ `log2 |M|` per
switch; the best decoder-side scheme costs `log2 |M| + log2 n` per switch
and still has to be *right* about the past. Hard selection with a
context-coded selector is strictly cheaper here.

Two places to keep mixing-like adaptivity, both signalling-free: (1) inside
a predictor, bias-cancel from its own residual history (LOCO-I C[q], JPEG
XL weighted predictor) — e.g. a data-pointer rewriter that learns the
constant slide of a section from the first corrected pointers; (2) in the
shared residual coder, which is already an adaptive model.

Fall back to a *per-region fallback module* ("raw / copy-from-old /
generic LZ") rather than to mixing when no domain module fits; that is the
Parquet dictionary→plain and BtrBlocks depth-limit pattern.

### (b) Granularity of a region, and cheap signalling

Regions should be **structural, not fixed-size**: ELF/Mach-O/PE sections
and the sub-objects a module can name (function bodies, pclntab, type
descriptors, an embedded file inside a container). Fixed 64k blocks are
right for BtrBlocks because column values are exchangeable; bytes of a
binary are not, and a boundary that cuts a function in half is a boundary
both modules will mispredict across. WebP's tile-size field and JPEG XL's
tree both exist because fixed tiles were too coarse or too fine somewhere.

Signal regions as a **sorted list of (start, length, module id, side-table
ref)** with varint gaps and a module id coded under a per-container prior
(or simply grouped: all regions of module X together, so the id is
implicit). Cost: a handful of bytes per region, which is negligible unless
regions number in the tens of thousands — and if they do, that is the
signal the module boundary is wrong (make the module own the sub-structure
internally, as go-binsync's layout table owns thousands of functions under
one "Go text" region). Rule of thumb from §8: no region smaller than the
bytes its selector entry costs times the expected fractional saving; for
a 4-byte entry and a 10 % saving that is ~40 bytes, so a floor of a few
hundred bytes is safe and anything under ~64 bytes should be absorbed into
the neighbour.

Nested regions (container → inner file → sub-region) are fine and are what
Precomp and BtrBlocks' cascade do; keep the nesting explicit and bounded
(§9d), never inferred.

### (c) One shared residual coder or per-module?

**One shared, positional residual stage, with per-module *hints* rather
than per-module coders.** Reasons:

- Cross-module redundancy: a mispredicted string constant in `.rodata` and
  its reference in `.text` are related; a shared LZ/entropy stage sees
  both. Per-module coders would each learn the same statistics from less
  data — the small-file problem that zstd dictionaries exist to fix (2.8× →
  6.9× when statistics are shared), which is direct evidence that
  splitting a residual into small independent streams costs a lot.
- Every fast system in §3 ends in one general-purpose coder (xz LZMA2 last,
  ClickHouse LZ4/ZSTD last, BtrBlocks' bit-packing leaves, WebP shared
  Huffman groups, PNG's single DEFLATE).
- go-binsync's own measurement: brotli-10 on the whole correction beat a
  single `zstd -19` of the file and the per-stream choice between zstd and
  brotli was worth 6–7 %; that choice is a *codec* choice for one stream,
  not a per-module split.

What modules *should* be allowed to do to the residual: (1) transform it
before the shared stage — deltas of pointers, xor-against-prediction,
byte-plane split for fixed-width tables (Parquet's BYTE_STREAM_SPLIT,
ClickHouse T64) — because those are cheap, deterministic, and change what
the shared coder sees from "random-looking bytes" to "mostly zeros"; (2)
emit residuals into a small number of **typed sub-streams** (code bytes,
pointer deltas, table entries, literals) that are concatenated and coded
with one coder each — the BCJ2 four-stream design and FLAC's partitioned
Rice parameters are this: a few streams with distinct statistics, not one
per module. Two to four sub-streams, fixed by the framework, not by
modules.

The residual coder's *state* should be shared across regions (zstd's
treeless-literals trick; WebP's accumulated-histogram cost) so region
boundaries do not reset it.

### (d) Constraining a composable module graph

Container → inner file → per-format predictor → shared residual coder is a
tree, and should be declared as one in the stream, not discovered by the
decoder. Constraints, each with the precedent that motivates it:

1. **Fixed depth limit** (BtrBlocks 3, xz 4). Three levels — container,
   file, format — plus the terminal coder is enough; deeper nesting
   (archive in archive) is a recursion of the same three, capped at, say,
   4 nestings total.
2. **Every module declares its output length before running** (xz sizes in
   the block header; RAR `BlockLength`; go-binsync's length-exact
   prediction). Positional correction is impossible otherwise. Modules
   whose prediction is not length-exact must wrap it as "predict length L,
   then pad/truncate", and the framework verifies.
3. **Expansion bound per stage** (xz's 2n→n). A module may not emit more
   than `k ×` its input plus a constant; the decoder enforces it and aborts
   — this is what makes a malicious patch bounded in memory and time.
4. **Terminal stage is the framework's shared coder**; modules cannot
   substitute their own entropy coder. This is the ClickHouse/xz rule and
   what keeps (c) true.
5. **Modules are pure functions of (old bytes they are given, their side
   table)** with no access to other regions' outputs except through an
   explicit, declared dependency (e.g. "text layout table" is an input to
   both the text predictor and the pclntab predictor). Declared inputs make
   the graph a DAG the decoder can topologically order and verify for
   cycles; undeclared reads make it nondeterministic across
   implementations.
6. **Determinism gate**: the encoder runs the decoder's exact reconstruction
   and hashes the prediction (go-binsync already does this, DESIGN.md §3.2);
   the hash goes in the stream. This is cheaper than proving every module
   deterministic and catches the Go-version-drift class of bug.
7. **Canonical serialisation**: one encoding for each selector value, no
   optional fields whose presence carries information, so two encoders
   given the same choices produce the same bytes and a hash of the patch is
   meaningful.
8. **No code in the stream.** RAR dropped its VM; zpaq kept it and stayed
   niche. Ship module *versions* and *parameters*, not programs; if a
   predictor needs data (a dictionary, a table), it is a side table with a
   size bound.

### (e) A cost model for the encoder's selection

Per candidate region `r` and module `m`, the true objective is

    cost(r, m) = L(selector entry) + L(side_m(r)) + L_shared(residual_m(r) | state)

with the last term the *marginal* size under the shared coder given what
has been coded already (WebP's accumulated-histogram trick; not the
standalone compressed size, which overstates small regions). Evaluate it in
three tiers, cheapest first, and stop as soon as one tier decides:

1. **Rules from metadata** (BtrBlocks stats pruning; Precomp probe): section
   type, magic bytes, presence of a pclntab, ELF flags. Most modules are
   ruled out for most regions here at zero cost, and the prior over modules
   for the selector code comes from the same metadata.
2. **Proxy on a sample** (BtrBlocks 1 %; PNG minsum; HEVC SAD): run the
   surviving modules' predictors on a few scattered sub-ranges and measure
   the *mispredicted byte count* — for a positional codec that is the
   natural analogue of SAD and it is what the residual cost is roughly
   proportional to. Use it to rank and to drop candidates that are >2×
   worse than the leader. Keep the sample as multiple small runs across the
   region, not one contiguous slice (BtrBlocks' finding). Do not use it to
   *decide* when a candidate's gain is non-local (LZ-style modules).
3. **Full trial for the top one or two**: run the predictor over the whole
   region, produce the residual sub-streams, and measure the true marginal
   size. This is what FLAC `-e` and libwebp do unconditionally; we can
   afford it for ≤2 candidates because regions are large and the decoder
   is the thing that must be fast.

Add WebP's hysteresis: a candidate that differs from the neighbouring
region's module must beat it by more than the selector-entry cost, so
the module map stays sticky and cheap. Add a cheap-first ordering with
branch-and-bound: if "copy from old" already gives zero residual for a
region, no other module is tried. And record the tier-2 estimate vs tier-3
actual on a corpus, as BtrBlocks did (77 % correct, 3.3 % loss); the point
at which the sample proxy becomes reliable enough to skip tier 3 is an
empirical question, and the number to publish.

What not to do: a learned or calibrated cost model (Damme's grey-box
approach). It works for lightweight integer schemes whose cost is a smooth
function of two statistics; our modules' costs depend on whether a linker
moved something, which no summary statistic predicts. Measuring is
cheaper than modelling here, and the encoder is allowed to be slow.

---

## Sources

- LOCO-I / JPEG-LS: https://www.sfu.ca/~jiel/courses/861/ref/LOCOI.pdf
- PNG spec: https://www.w3.org/TR/png-3/ ; filter heuristic: http://www.libpng.org/pub/png/book/chapter09.html ; brute-force study: https://optipng.sourceforge.net/pngtech/better-filtering.html
- FLAC RFC 9639: https://www.rfc-editor.org/rfc/rfc9639.html ; encoder options: https://xiph.org/flac/documentation_tools_flac.html
- fpzip: https://computing.llnl.gov/projects/fpzip
- HEVC lossless intra: https://arxiv.org/pdf/1604.07051
- JPEG XL modular: https://cloudinary.com/blog/jpeg-xls-modular-mode-explained ; design paper https://arxiv.org/abs/2506.05987
- WebP lossless spec: https://developers.google.com/speed/webp/docs/webp_lossless_bitstream_specification ; encoder: https://github.com/webmproject/libwebp/blob/main/src/enc/predictor_enc.c
- Switch distribution: https://arxiv.org/abs/0807.1005 ; Context Tree Switching: https://arxiv.org/abs/1111.3182 ; Koolen & de Rooij: https://arxiv.org/abs/1311.6536 ; Volf & Willems DCC'98: https://ieeexplore.ieee.org/document/672217/
- Context mixing: https://mattmahoney.net/dc/dce.html ; GLN: https://arxiv.org/abs/1712.01897 ; cmix: https://www.byronknoll.com/cmix.html ; LTCB: https://www.mattmahoney.net/dc/text.html
- MDL: Wallace & Dowe 1999 https://users.monash.edu/~dld/Publications/1999/WallaceDowe1999bRefinementsOfMDL_AndMML_Coding_ComputerJVol42No4pp330-337.pdf
- Roaring: https://arxiv.org/abs/1402.6407 ; https://arxiv.org/abs/1603.06549 ; format: https://github.com/RoaringBitmap/RoaringFormatSpec
- Parquet encodings: https://github.com/apache/parquet-format/blob/master/Encodings.md ; arrow-rs writer: https://docs.rs/parquet/latest/parquet/file/properties/struct.WriterProperties.html
- ClickHouse codecs: https://clickhouse.com/docs/sql-reference/statements/create/table
- BtrBlocks (SIGMOD 2023): https://www.cs.cit.tum.de/fileadmin/w00cfj/dis/papers/btrblocks.pdf ; https://github.com/maxi-k/btrblocks
- Damme et al. TODS 2019: https://dl.acm.org/doi/10.1145/3323991
- zstd format: https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md ; dictionaries: https://github.com/facebook/zstd/blob/dev/programs/zstd.1.md , https://engineering.fb.com/2016/08/31/core-infra/smaller-and-faster-data-compression-with-zstandard/
- Shared Brotli RFC 9841: https://datatracker.ietf.org/doc/html/rfc9841
- xz format: https://tukaani.org/xz/xz-file-format.txt ; 7-zip methods: https://sevenzip.osdn.jp/chm/cmdline/switches/method.htm ; unrar: https://github.com/aawc/unrar/blob/master/unpack.hpp
- Precomp / preflate: https://github.com/schnaader/precomp-cpp ; https://github.com/deus-libri/preflate
- Grammar codes: https://www.mit.edu/~6.454/www_fall_2002/emin/kieffer_and_yang.pdf
- Language Modeling Is Compression: https://arxiv.org/abs/2309.10668
- lzbench: https://github.com/inikep/lzbench
