# Domain study: AI model weight files

Research notes for the generalised predictive codec (2026-08-27). Question:
are model weight files (safetensors / GGUF / checkpoints) a domain where a
structure-aware *predictor* beats general-purpose compression and delta by a
large margin, the way the Go-aware transform does for Go binaries (28-67x over
bsdiff)?

Sourcing rule: every number carries an inline URL to a primary source unless
marked *(secondary)* or *(unverified)*. Numbers marked *(measured here)* come
from a small experiment run while writing this document (§6) on
Qwen2.5-0.5B vs Qwen2.5-0.5B-Instruct, bf16 safetensors, 494 M parameters;
the script is reproducible from the description in §6.

---

## 0. Summary

Short answer: **no, not by a large margin — and the reason is a hard
information-theoretic floor, not a lack of cleverness.**

- Full-file lossless compression of bf16 weights has a well-established floor
  of **~10.5-11 bits/weight** (1.45-1.5x). The 7 mantissa bits and the sign bit
  are essentially incompressible; all the gain is in the 8-bit exponent, whose
  order-0 entropy is ~2.6 bits. Every published system (ZipNN, DFloat11,
  NeuZip, Huff-LLM, Unweight, the "Shannon bound" paper) lands in the same
  30-33% window because they all hit the same wall. Context modelling on top
  of order-0 exponent coding is worth **<0.1 bit/weight** *(measured here,
  §6)*; there is no "Go-binary moment" hiding in the weights.
- Version-to-version delta between a base model and a **full fine-tune** is
  also floor-limited: the low 3-4 mantissa bits of every weight flip like coins
  after any real amount of training. Published lossless deltas are **~50-65%
  of the fine-tune's size** (FM-Delta, ZipNN, ZipLLM/BitX), versus ~67-71%
  for standalone compression. *(measured here)*: only 2% of bf16 values are
  bit-identical between Qwen2.5-0.5B and its Instruct variant; the XOR stream
  has 8.1 bits/weight of order-0 entropy. A "1% patch" for a full fine-tune is
  physically impossible losslessly.
- The one regime with a genuine 100x is **successive checkpoints of the same
  run at small learning rates** (RL post-training, late pre-training) where
  ~99% of bf16 values are bit-identical per step because Adam updates fall
  below the bf16 rounding cell (PULSESync, 14 GB -> ~140 MB, lossless). That
  is a sparse-patch problem, not a prediction problem, and it is already
  solved by index+value+zstd.
- What a predictor module *can* legitimately do: (a) reproduce the
  safetensors/GGUF header and tensor layout from the previous version (100%
  predictable, but headers are 35 KB-1 MB of a 1-1000 GB file, so it matters
  for *correctness of alignment*, not size); (b) align tensors by name so the
  residual is the element-wise training delta even when the file layout
  changes (this is where CDC dedup fails: ZipLLM shows chunk dedup recovers
  only 14.8% across 3,048 HF LLMs while tensor-aligned XOR + entropy coding
  recovers 54%); (c) implement the exponent/sign-mantissa split with a
  near-optimal entropy coder. Together that yields roughly **1.3-1.5x over
  zstd** on full files and **1.25-1.35x over zstd on deltas**, not 28-67x.
- Quantized formats: the evidence conflicts. Hershcovitch 2025 finds FP4 /
  NVFP4 payloads "statistically uniform" with the block scales (~10% of the
  file) the only compressible part, ~5% total; the "Shannon bound" paper
  reports int4/fp4 symbol entropy of only 0.6-1.0 bits per 4-bit symbol for
  its own symmetric quantizers. Which applies to GGUF K-quants is unverified
  (§1.3). Xet's own numbers on a 29-quantization GGUF repo show 49% dedup,
  which is almost entirely shared tokenizer/embedding/metadata chunks, not
  cross-quant prediction.

Recommendation: treat weights as a **thin, well-understood domain**: a
`safetensors`/`GGUF` structure-aware predictor (header + tensor alignment +
exponent split) is cheap to build, gives a reliable 1.3-1.5x over zstd and
about 2x over Xet-style CDC dedup on fine-tune families, and is throughput-
bound rather than ratio-bound. Do not pitch it as the headline domain for the
framework; the headline claim (predictor >> generic) is falsified here by
entropy, and anyone in the field will know it.

---

## 1. Lossless compression of full weight files

### 1.1 The bf16 entropy floor

bf16 = 1 sign + 8 exponent + 7 mantissa. Trained weights are concentrated in
[-1, 1] with a heavy-tailed magnitude distribution, so only ~40 of 256
exponent codes ever appear and a handful cover almost everything; the mantissa
is effectively uniform.

| Source | Metric | Value |
|---|---|---|
| DFloat11 (Zhang et al. 2025) [arXiv:2504.11651](https://arxiv.org/pdf/2504.11651) | exponent Shannon entropy, linear layers of Llama-3/Qwen/Mistral | ~2.6 bits of 8; sign and mantissa "close to their bit widths" |
| DFloat11 Table 1 | Llama 3.1 8B / 70B / 405B Instruct, Qwen3 14B, Mistral Small 3, FLUX.1 | 67.6-69.5% of original, **10.81-11.12 bits/weight** |
| ZipNN (Hershcovitch et al. 2024) [arXiv:2411.05239](https://arxiv.org/pdf/2411.05239) | distinct exponent values appearing | ~40 (50 in ResNet) of 256 |
| ZipNN | random permutation of all parameters then zstd on exponents | ratio changes by <=0.05% => **no exploitable ordering/locality in exponents** |
| ZipServ [arXiv:2603.17435](https://arxiv.org/pdf/2603.17435) §3.1 | top-7 exponents cover 95-97.4% (Llama-3 96.4%, Mistral-24B 97.4%); exponent entropy 2.74 bits | theoretical ratio 1.51x (16/10.6) |
| Huff-LLM [arXiv:2502.00922](https://arxiv.org/pdf/2502.00922) Table 1 | Llama-3-8B FP16: whole-16-bit-symbol entropy vs 1-5-5-5 split vs 8-8 split | 10.54 / 10.61 / 10.57 bits/param — splitting costs <0.1 bit |
| Huff-LLM Table 5 | BF16 Llama/Qwen/OPT/Vicuna | 11.59-11.68 bits/param (1.37-1.38x); FP16 Llama/Qwen 10.96 (1.46x), FP16 OPT/Vicuna 13.68-13.78 (1.16-1.17x) |
| "Approaching Shannon Bound" (Tan et al. 2026) [arXiv:2606.15789](https://arxiv.org/pdf/2606.15789) | per-tensor order-0 entropy, Qwen-1.5B..Llama-405B | bf16 effective **10-12 bits**, ~1.5x; tile-level ANS lands within 0.01-0.05 bits of that bound |
| Cloudflare Unweight (2026) [blog](https://blog.cloudflare.com/unweight-tensor-compression/) | Llama 3.1 8B MLP weights, per-tensor 16-entry exponent palette + Huffman | top-16 exponents cover >99%; ~30% on exponent stream; 22% whole-model (MLP only) |
| "Split12" writeup [brianbell-x](https://brianbell-x.github.io/weight-compression/Split12/) | GLM-5.2 753B bf16, 59,509 tensors | 4-bit sign+exp code into 15-entry LUT: **11.17 bits/weight**; exact byte-split path 12.0 bits |
| *(measured here, §6)* Qwen2.5-0.5B bf16 | H(exp)=2.639, H(mant)=6.957, H(16-bit symbol)=10.554 | ZipNN-style H(exp)+8 = 10.64 bits/weight; 38 distinct exponents; top-16 cover 99.97% |

Everything converges on **~10.5-11 bits/weight for bf16**. The mantissa's 7
bits carry 6.9-7.0 bits of entropy; there is nothing to take from them
without going lossy.

FP32 and FP16 are different:

- FP32: exponent is 8 of 32 bits, so the same trick saves only ~17%
  ([ZipNN](https://arxiv.org/pdf/2411.05239) §I). "Clean" FP32 models that
  were rounded after training (RoBERTa, BGE, Whisper, CLIP, xlm-RoBERTa) have
  their low mantissa bytes zeroed and compress to **42-50%** with byte
  grouping (ZipNN Table I: Bge 42.1%, Whisper 42.7%, xlm-RoBERTa 42.3%,
  Clip 49.7%; vs Bert 83.9%, Mpnet 82.9%). The IBM predecessor paper
  [arXiv:2404.15198](https://arxiv.org/html/2404.15198) reports RoBERTa-base
  FP32 mantissa bytes compressing to 99.9% / 44.7% / **0.005%** — the lowest
  byte is all zeros. That is a real structure a predictor can detect (but so
  can zstd after byte grouping).
- FP16 (5-bit exponent): Llama FP16 compresses better per-weight than BF16
  (10.96 vs 11.68 bits, Huff-LLM Table 5) because FP16 weights were cast from
  BF16 and carry padded mantissa bits; OPT FP16 does not (13.7 bits).

### 1.2 Published full-model systems

| System | Format | Ratio / bits per weight | Throughput | Notes |
|---|---|---|---|---|
| ZipNN [paper](https://arxiv.org/pdf/2411.05239), [repo](https://github.com/zipnn/zipnn) | bf16/fp32/fp8 | bf16 ~66-67% (Llama 3.1 67.2%, Qwen 66.9%); 17% better than zstd on bf16 | single-thread M1: ZipNN 1.15 GB/s comp / 1.65 GB/s decomp vs zstd 0.71/1.02 (Llama-3.1 bf16, Table III); multi-thread 2x NUMA Xeon: up to 13 GB/s comp, 80 GB/s decomp | exponent extraction + Huffman; byte grouping for fp32; auto-select zstd vs Huffman per chunk |
| DFloat11 [arXiv:2504.11651](https://arxiv.org/pdf/2504.11651) | bf16 | ~70% (10.8-11.1 b/w), 12 models incl. Llama 3.1 405B 811.7->551.2 GB | GPU LUT Huffman decode; up to 20.97x faster than nvCOMP; ratio 68% vs nvCOMP 79% | inference-time decompression in the kernel |
| NeuZip [arXiv:2410.20650](https://arxiv.org/abs/2410.20650) | bf16 | Llama-3 8B training memory 31 GB -> <16 GB (lossless variant) | GPU ANS on exponents | same exponent/ANS idea |
| "Shannon bound" [arXiv:2606.15789](https://arxiv.org/pdf/2606.15789) | bf16/fp8/int8/sq8/fp4/int4/awq4 | bf16 ~1.5x; int8 ~2.0-2.1x (4-5 bits); fp4/int4 6-10x *symbol* redundancy | tile-level ANS fused into GEMM; Qwen-14B weights 27.5->18.1 GB, Mixtral-176B 261.9->163.7 GB | "Shannon bound" = per-tensor order-0 entropy |
| Huff-LLM [arXiv:2502.00922](https://arxiv.org/pdf/2502.00922) | fp16/bf16 | 15-32% size reduction | hardware Huffman decoders, <6% area | |
| Unweight [blog](https://blog.cloudflare.com/unweight-tensor-compression/), [paper](https://research.cloudflare.com/papers/unweight-2026.pdf) | bf16 MLP | 22% (distribution bundle), 13% (inference bundle) | Hopper kernels, 30-41% throughput overhead | palette of 16 exponents per tensor |
| nvCOMP gANS [NVIDIA blog](https://developer.nvidia.com/blog/cut-checkpoint-costs-with-about-30-lines-of-python-and-nvidia-nvcomp/) | bf16 checkpoint components | bf16 1.46-1.48x (gANS) vs 1.27-1.28x (GPU zstd); whole dense checkpoint ~1.18x (fp32 Adam states 1.06-1.11x) | gANS ~530 GB/s on bf16 on Blackwell; zstd ~16 GB/s | fp32 optimizer state is nearly incompressible |
| DietGPU [repo](https://github.com/facebookresearch/dietgpu) | float mode | comparable | ~265 GB/s (secondary) | |
| QStore [arXiv:2505.04081](https://arxiv.org/pdf/2505.04081) | bf16 + int8 pair | stores W and Q(W) jointly; 2.2x vs uncompressed pair, 1.6x vs ZipNN+zstd; H(W\|Q(W)) is 39% of pair | 1.5 GB/s SSD tests | the only "predict high precision from low precision" system found |

Byte grouping vs exponent extraction: zstd on raw bf16 gets 77-78% (ZipNN
Table III Llama-3.1: 77.7%; *(measured here)* 77.8%); zstd on byte-grouped
bf16 gets ~71% *(measured here)*: 71.1%, high byte 3.40 b/w, low byte 7.97
b/w); Huffman on extracted exponents gets 66-68%. The remaining gap between
zstd-on-hi-byte (3.40 bits for sign+7 exponent bits) and the entropy (2.64 +
~1 sign) is zstd's FSE modelling loss on a 1-byte alphabet with no
repetition, which is exactly what an order-0 Huffman/ANS on the split stream
recovers.

### 1.3 Quantized formats

- GGUF Q4_K super-block layout: 256 weights = 2x fp16 (d, dmin) + 12 bytes of
  6-bit sub-scales/mins + 128 bytes of nibbles = 144 bytes, 4.5 bits/weight
  ([GGUF spec](https://github.com/ggml-org/ggml/blob/master/docs/gguf.md);
  struct layout *(secondary)* from
  [zeroentropy.dev](https://zeroentropy.dev/concepts/gguf/)). Q8_0: 32 weights
  = fp16 scale + 32 int8 = 34 bytes, 8.5 bits/weight.
- "Lossless Compression of Neural Network Components ... in Low-Precision
  Formats" (Hershcovitch et al. 2025) [arXiv:2508.19263](https://arxiv.org/abs/2508.19263):
  FP8 E4M3 exponent compresses to 0.20-0.30, mantissa >0.80, overall
  **55-70%**; FP4 quantized values "appeared statistically uniform"; NVFP4
  scale factors are ~10% of the file and the only compressible part, giving
  **5%** overall. K/V cache in fp8: 0.25-0.45.
- The "Shannon bound" paper measures int4/fp4 *symbol* entropy at 0.6-1.0
  bits per 4-bit symbol (6-10x "gap"). This is real for their symmetric int4
  quantizers where most codes are near zero, but it contradicts the FP8/FP4
  paper above for the block-scaled formats; the two papers quantize
  differently and I could not reconcile which applies to GGUF K-quants
  *(unverified)*. Practical prior from llama.cpp users: GGUF files do not
  compress meaningfully under gzip/zstd (Docker's model-artifact rationale:
  "model weights are largely uncompressible", so DMR ships layers
  uncompressed — [Docker blog](https://www.docker.com/blog/oci-artifacts-for-ai-model-packaging/) *(secondary quote)*).
- Cross-quantization dedup: Xet's gemma-2-9b-it-GGUF study, 29 quantizations,
  191 GB -> 97 GB stored (49%) ([From Chunks to Blocks](https://huggingface.co/blog/from-chunks-to-blocks)).
  ZipLLM observes that "many LLM repositories include multiple GGUF files
  that differ only by quantization method" and proposes storing base +
  quantization config and re-quantizing on demand
  ([arXiv:2505.06252](https://arxiv.org/pdf/2505.06252) §6) — i.e. a
  *predictor that recomputes the quantization*, which is exactly the
  predict-then-correct shape; nobody has published it done losslessly for
  GGUF. QStore does it for bf16->int8 pairs (H(W|Q(W)) = 39% of pair size).

---

## 2. Delta between model versions

### 2.1 Why fine-tune deltas are not sparse in bf16

Two mechanisms, pulling in opposite directions:

1. **Update absorption.** bf16 has 7 mantissa bits, so a weight of magnitude
   |w| only changes if the accumulated update exceeds ~|w|/256. At RL
   post-training learning rates (1e-6 .. 1e-5) a single Adam step almost
   never crosses that threshold: "approximately 99% of per-step weight updates
   are invisible after the BF16 cast", stable across 400 steps (std 0.2-0.4%),
   gradients themselves ~99% dense
   ([Miahi & Belilovsky 2026, arXiv:2602.03839](https://arxiv.org/pdf/2602.03839) §3).
   "The Silent Freeze" measures the same thing in fp8 training: 41-60% of
   attention/MLP coordinates never move under round-to-nearest
   ([arXiv:2607.09800](https://arxiv.org/html/2607.09800)).
2. **But any real amount of training crosses it.** Over thousands of SFT
   steps at 1e-5..2e-5 the FP32 master weights drift by more than one ulp for
   nearly every parameter, and once a bf16 value changes its low mantissa
   bits are effectively re-randomised.

*(measured here)* Qwen2.5-0.5B -> Qwen2.5-0.5B-Instruct (full SFT+RL):

| statistic | value |
|---|---|
| bf16 values bit-identical | **2.07%** overall (layernorm weights 20-36%, all projections and embeddings 1.7-3.0%) |
| XOR bit-flip fraction by bit (sign, e7..e0, m6..m0) | sign 2.9%; e7-e5 0.0%; e4 0.3%; e3 5.3%; e2 8.4%; e1 13.4%; e0 21.3%; m6 30.8%; m5 40.9%; m4 47.7%; m3 49.8%; m2-m0 **50.0%** |
| order-0 entropy of 16-bit XOR symbol | 8.10 bits/weight |
| order-0 entropy of monotone integer delta (FM-Delta mapping) | 7.71 bits/weight |
| zstd -3 on raw XOR stream | 62.3% of fine-tune size |
| zstd -3 on byte-grouped XOR | 56.3% |
| zstd -3 on byte-grouped int delta | 57.0% |
| zstd -19 on byte-grouped XOR | 53.2% |
| for comparison: zstd -3 on the fine-tune alone, byte-grouped | 71.1% |

The low three mantissa bits are exact coin flips (0.500) — that is 3
bits/weight of irreducible residual before you count the rest of the
distribution. Delta coding buys ~2.5 bits/weight over standalone coding (10.6
-> 7.7-8.1), i.e. the delta is **~0.75x the size of the standalone
compressed file**, never 0.01x.

ZipLLM's bit-position breakdown across 311 Llama-3/3.1/Mistral/Qwen2.5
fine-tunes shows the same shape: "most differences are concentrated in the
lower mantissa bits, with the upper mantissa and exponent bits contributing
far less, and the sign bit almost never flipping"
([arXiv:2505.06252](https://arxiv.org/pdf/2505.06252) Fig. 5). ZipNN's
ResNet-18 checkpoint series: "while all parameters in the model change in
each epoch, when broken down to bytes, more and more bytes remain unchanged
as the training converges ... the exponent byte has the least changes,
whereas the least bits in the fraction have the most change" ([ZipNN](https://arxiv.org/pdf/2411.05239) §IV-B).

### 2.2 Published lossless delta numbers

| Source | Pair | Lossless delta size | Standalone compressed | Method |
|---|---|---|---|---|
| FM-Delta (NeurIPS 2024) [pdf](https://proceedings.nips.cc/paper_files/paper/2024/file/7b75a7339dfb256ee4b4bec028a6890b-Paper-Conference.pdf) Table 4 | Falcon-40B x10 fine-tunes | 56% of total family storage (846 -> 474 GB) | LZMA 73%, gzip 79% | monotone float->uint map, integer subtract, range-code (sign, MSB position) + raw low bits |
| FM-Delta | GPT-NeoX-20B x10 | 48% (423 -> 205 GB) | LZMA 71% | |
| FM-Delta | GPT-J-6B x10, GPT-2 x100, BERT-large x100, SD x10, ResNet50 x20 | 59%, 60%, 65%, 67%, 66% | 84-93% | |
| FM-Delta Table 5 | BERT-large: float delta vs uint delta under LZMA | float params 92%, float subtraction 78%, int delta 74%, **uint delta 72%**; FM-Delta 68% | | fp subtraction is worse than XOR/integer delta |
| FM-Delta Table 6 | per-dtype on BERT-large | bf16 < fp16 < fp32 (bf16 compresses best; exact values in paper) | | |
| FM-Delta appendix | per fine-tune, Falcon-40B variants | 43-53%; GPT-NeoX 28-53%; GPT-J 52-60%; GPT-2 62-78% | | |
| FM-Delta throughput | | comp 109.7 MB/s, decomp 100.9 MB/s (single core, range coder) | LZMA 4.9 MB/s comp | |
| ZipNN §IV-B [pdf](https://arxiv.org/pdf/2411.05239) | 3 Twitter-RoBERTa fine-tunes of same base | **56%** avg XOR delta | 83.7% standalone | XOR + exponent-extract + Huffman/zstd auto-select |
| IBM 2404.15198 [html](https://arxiv.org/html/2404.15198) | RoBERTa epoch 10 vs 9 (fp32) | 55% lossless (37% with 2^23 tunable-lossy) | 83% | byte-grouped zstd |
| IBM 2404.15198 | RoBERTa epoch 10 vs base | 65% lossless | | |
| ZipNN Fig 8 | ResNet-18 consecutive epochs (fp32) | 54.9% consecutive; base-every-5/10 "still far better than standalone" | 66.9% | |
| Hershcovitch 2025 [arXiv:2508.19263](https://arxiv.org/html/2508.19263) | bf16 checkpoint deltas | exponent stream as low as 0.07; overall **38% of delta size** | 62% standalone | |
| ZipLLM/BitX [arXiv:2505.06252](https://arxiv.org/pdf/2505.06252) | 3,048 HF LLMs, family-clustered | total storage **-54.1%** (BitX+TensorDedup); "many model sizes reduced by over 50%" | ZipNN -33%; zstd less | XOR vs family base + zstd; BitX kernel ~7.9 GB/s (192 threads), ZipNN 9.7 GB/s, zstd 1.05 GB/s single-thread |
| PULSESync [arXiv:2602.03839](https://arxiv.org/pdf/2602.03839) | RL per-step bf16 patch, 7B (14 GB) | **140 MB, >100x, bit-identical** (108 MB mean upload; 32B model 62 GB sync took 14 min dense) | | index+value sparse patch; delta-varint indices give 23% more; then lz4/zstd-1 |
| Check-N-Run (NSDI'22) [pdf](https://www.usenix.org/system/files/nsdi22-paper-eisenman.pdf) | DLRM embedding tables | 17x bandwidth / 8x storage | | differential (only touched rows) + *lossy* quantization |

Lossy delta systems, for orientation only (not comparable to a lossless codec):
BitDelta (1-bit sign + per-tensor scale, >10x, NeurIPS 2024
[pdf](https://proceedings.neurips.cc/paper_files/paper/2024/file/187d94b3c93343f0e925b5cf729eadd5-Paper-Conference.pdf));
DeltaZip (2-bit + 50% sparsity, 9.86x on Vicuna-7B, then GDeflate,
[repo](https://github.com/eth-easl/deltazip)); Delta-CoMe (mixed-precision
SVD, [arXiv:2406.08903](https://arxiv.org/pdf/2406.08903)); Per-Axis Weight
Deltas (sign mask + per-row/col fp16 scales, 5.24x on Llama-3.1-8B,
explicitly lossy, [arXiv:2512.19720](https://arxiv.org/html/2512.19720));
ExCP (residual + weight-momentum joint pruning + non-uniform quantization,
25-70x, "nearly lossless", [arXiv:2406.11257](https://arxiv.org/html/2406.11257v1));
DynaQuant/Inshrinkerator (26-39x, SoCC'24, [arXiv:2306.11800](https://arxiv.org/pdf/2306.11800)):
its *lossless* delta stage is run-length coding of unchanged
quantization-bucket indices, "3-4x higher than existing delta compression
schemes" only because the input has already been quantized; Delta-DNN
(ICPP'20, error-bounded lossy, 2-10x over prior,
[DOI](https://dl.acm.org/doi/10.1145/3404397.3404408)); QD-Compressor and
LC-Checkpoint (quantization-based delta, IEEE TPDS 2023,
[IEEE](https://ieeexplore.ieee.org/document/10018182/)). Every "20x+"
checkpoint number in the literature is lossy; the lossless numbers are 1.5-2.6x.

### 2.3 Structure of the delta stream, for predictor design

- Sign flips: ~3% *(measured here)*; effectively never in ZipLLM's families.
- Exponent changes: top 3 bits never, e0 in ~21% of weights *(measured)*.
  Hershcovitch 2025 reports the exponent *delta* stream compressing to 0.07.
- Mantissa: m6 flips 31%, m4+ ~50%. In a monotone integer representation the
  delta magnitude is roughly |w| x (relative change), so the MSB position of
  the integer delta clusters (FM-Delta: "most significant bits of most delta
  elements are around 19" of 32 for SD fp32).
- Because the delta magnitude scales with |w|, coding the *integer delta
  conditioned on the base exponent* is the natural context. FM-Delta does
  something adjacent (range-coding the MSB position). *(unverified)* nobody
  has published H(delta | base exponent); I expect it to be worth ~0.2-0.5
  bit/weight over unconditioned XOR, which is the largest remaining lever and
  still small.
- XOR vs subtraction: FM-Delta Table 5 — LZMA on float subtraction 78%, on
  uint delta 72%; ZipLLM chose XOR for "sparse outputs". *(measured here)*:
  XOR 8.10 vs int-delta 7.71 bits/weight order-0, but zstd byte-grouped
  56.3% vs 57.0% — zstd can't exploit the int-delta's entropy advantage.

---

## 3. Distribution and storage systems

| System | Unit / dedup | Compression | Published ratios | Delta product? |
|---|---|---|---|---|
| Hugging Face Xet ([dedup spec](https://huggingface.co/docs/xet/en/deduplication), [xorb spec](https://huggingface.co/docs/xet/xorb), [00f.net intro](https://00f.net/2026/01/19/xet-intro-1/)) | GearHash CDC, target 64 KiB (min 8 / max 128 KiB); chunks grouped into <=64 MiB xorbs (<=8,192 chunks); global chunk dedup | per-chunk LZ4F, optional **BG4** (4-way byte grouping then LZ4), selected by a KL-divergence predictor on per-byte popcounts ([xet-core](https://github.com/huggingface/xet-core)) | gpt2 two versions 1.2 GB -> 645 MB (53%), "compression adds ~10%"; fine-tune/checkpoint repos "30-85%" dedup; CORD-19 dataset 8.9 -> 3.52 GB ([From Files to Chunks](https://huggingface.co/blog/from-files-to-chunks)); 29-quant GGUF repo 191 -> 97 GB ([From Chunks to Blocks](https://huggingface.co/blog/from-chunks-to-blocks)); marketing: "62% deduplicated: uploading 912 GB (saved 1.5 TB)" ([hf.co/storage](https://huggingface.co/storage)) | No. Dedup only — a full fine-tune shares almost no 64 KiB chunks with its base (see §2.1; ZipLLM: CDC finds 14.8% across 3,048 LLMs, "largely due to repeated tensors rather than generic byte-level similarity") |
| Git LFS | whole file | none | 0 | No |
| safetensors ([repo](https://github.com/safetensors/safetensors), [tensor.rs](https://github.com/safetensors/safetensors/blob/main/safetensors/src/tensor.rs)) | 8-byte LE header length + JSON `{name: {dtype, shape, data_offsets}}` padded with spaces to 8-byte alignment + raw contiguous tensors | none | Header for Qwen2.5-0.5B: 34,770 bytes, byte-identical between base and Instruct *(measured)*. Rust serializer orders tensors by dtype size desc then name asc; Python/torch writers may differ, ZipLLM notes "many models ... reorder tensors alphabetically by name" | — |
| GGUF ([spec](https://github.com/ggml-org/ggml/blob/master/docs/gguf.md)) | magic, version 3, tensor count, KV metadata (tokenizer vocab, hyperparams), tensor infos (name <=64 B, dims, ggml_type, offset), padding to `general.alignment` (default 32), tensor data | none | tokenizer + metadata typically 1-10 MB and identical across quants of a model | — |
| Ollama ([DeepWiki](https://deepwiki.com/ollama/ollama/4.2-model-registry-and-layers) *(secondary)*) | OCI-style manifests + sha256 blobs; GGUF as one blob layer; pulls only missing blobs, resumable | none | blob-level dedup only | No |
| Docker Model Runner / CNCF ModelPack ([Docker](https://www.docker.com/blog/oci-artifacts-for-ai-model-packaging/), [CNCF 2026](https://www.cncf.io/blog/2026/08/12/advancing-ai-model-interoperability-with-docker-and-modelpack/)) | OCI artifact, media types `application/vnd.docker.ai.gguf.v3`, `application/vnd.cncf.model.manifest.v1+json`; one layer per file | **uncompressed layers by design** ("model weights are largely uncompressible") | layer dedup across tags | No |
| NVIDIA NIM ([profiles](https://docs.nvidia.com/nim/large-language-models/latest/deployment/model-profiles-and-selection.html), [cache](https://docs.nvidia.com/nim-operator/latest/cache-llm.html)) | per-profile (backend x GPU x precision x TP/PP) pre-built TensorRT-LLM engines + weights, hash-identified, cached on PVC (e.g. 50Gi) | none | — | No; engines are per-GPU-SKU so the artifact fan-out is *larger* than raw weights |
| vLLM / Run:ai Model Streamer ([NVIDIA blog](https://developer.nvidia.com/blog/reducing-cold-start-latency-for-llm-inference-with-nvidia-runai-model-streamer), [Azure](https://devblogs.microsoft.com/azure-sdk/eliminate-llm-cold-starts-load-models-up-to-6x-faster-with-azure-blob-storage-and-runai-model-streamer/)) | streams safetensors tensors concurrently from S3/Blob/GCS into GPU | none | Llama-3-8B 15 GB: S3 -> GPU 4.88 s (32 conc.) vs Tensorizer 37 s; vLLM ready 23 s vs 65 s; GP3 SSD ~1 GiB/s, IO2 ~2 GiB/s | No |
| vLLM RL weight sync ([docs](https://docs.vllm.ai/en/latest/training/weight_transfer/nccl/), [RFC 31848](https://github.com/vllm-project/vllm/issues/31848)) | NCCL broadcast / CUDA IPC trainer -> inference workers each step | none (dense) | PULSESync shows 100x is available here | Research only |
| JFrog Artifactory (native Xet, 2026) ([JFrog](https://jfrog.com/blog/native-xet-support-in-jfrog-artifactory/)) | Xet chunks | as Xet | — | No |

Bottom line for §3: **there is no shipping "delta between model versions"
product.** The closest are Xet (CDC dedup, which structurally cannot see a
fine-tune delta) and research systems (FM-Delta, ZipLLM/BitX, PULSESync).
Docker/OCI and NIM ship raw bytes. That is a genuine gap, but the prize is
~2x on families and ~100x only on per-step RL sync.

---

## 4. Where a predictor module could help

### 4a. Header + layout prediction

- safetensors: the header is JSON with deterministic key order for a given
  writer; between two versions from the same pipeline it is byte-identical
  (Qwen2.5-0.5B base vs Instruct: 34,770 B identical *(measured)*). A
  predictor that emits `header(prev)` and expects a small textual correction
  (changed `__metadata__`, a renamed tensor, a re-sharded `model.safetensors.index.json`)
  is trivial and gives a perfectly aligned tensor table for free. Size win:
  nil (35 KB of 1 GB; up to ~1 MB for 405B multi-shard indexes).
- Alignment *is* the point. When a fine-tune is re-sharded (different
  `model-0000N-of-0000M` boundaries, alphabetical vs dtype-major order,
  `lm_head` tied vs untied), byte-offset delta (bsdiff/xdelta) and CDC lose
  the correspondence. ZipLLM: "tensor-level deduplication is better aligned
  ... In practice, many models use custom naming conventions or reorder
  tensors alphabetically by name, which can complicate BitX matching."
  A name-keyed tensor map solves this exactly and cheaply.
- GGUF: header + KV metadata (including the whole tokenizer vocab) is
  identical across quantizations and across fine-tunes of the same base; the
  tensor-info table differs only in `type` and `offset`. Predicting the
  metadata from any sibling file is exact; predicting offsets from
  (type, dims, alignment) is a pure function.

### 4b. Tensor-from-previous-tensor prediction

- Prediction = `prev[name]` element-wise, same dtype and shape; correction =
  XOR or integer delta, entropy-coded per §2.3. Expected result on full
  fine-tunes: **50-60% of the new file** (vs ~67-71% standalone). On
  successive checkpoints of one run: 38-55%. On RL per-step syncs: <1%.
- Shape-changed tensors (vocab extension, added LoRA-merged heads): predict
  the overlapping slice, code the rest standalone. Cheap.
- Dtype-changed (bf16 -> fp16, fp32 -> bf16 casts): predict by exact
  re-cast; correction is zero for bf16->fp32 upcasts and a rounding residual
  for downcasts (QStore's H(W|Q(W)) = 39% figure is the analogous number for
  bf16 -> int8).
- Merged-LoRA fine-tunes *(unverified, but worth a test)*: if the delta is
  `bf16(W + B A)` with rank r, then given B and A (r x (d_in + d_out)
  values) the predictor can recompute the merge bit-exactly *if* it uses the
  same accumulation order and rounding; the residual would then be ~0 for
  target matrices. Any deviation in kernel/accumulation order would put you
  back at the 3-coin-flip-bits floor. High reward (the side table would be
  ~1% of the file for r=16 on a 7B), high fragility.
- Quantization recompute (GGUF Qx from bf16 source): predictor runs
  `llama-quantize`'s block quantizer on the previous full-precision tensor.
  Bit-exactness depends on the exact llama.cpp version and (for imatrix
  quants) the importance matrix; if exact, the correction is empty and a 4 GB
  Q4_K_M is a few KB of side table on top of the bf16 source. Nobody has
  shipped this; ZipLLM proposes it as future work.

### 4c. Context modelling of exponents — what the evidence says

Direct tests of whether neighbourhood or per-channel statistics carry
information beyond the order-0 exponent histogram:

| test | result |
|---|---|
| ZipNN: random-permute all parameters, zstd on exponents | ratio change <= 0.05% => sequence order carries nothing LZ can see |
| Huff-LLM Table 1: entropy of the whole 16-bit symbol vs sum of split-field entropies | 10.54 vs 10.57-10.61 => fields are nearly independent |
| *(measured here)* H(exp \| previous exp) | 2.632 vs H(exp) 2.639: **0.007 bit** |
| *(measured here)* H(mant \| exp) | 6.916 vs 6.957: 0.04 bit |
| *(measured here)* H(exp \| row) over all 2-D tensors (per-output-channel histograms) | 2.568 vs 2.639: **0.07 bit** — and that is before paying for per-row tables |
| "Shannon bound" paper | per-tensor order-0 ANS is within 0.01-0.05 bit of per-tensor entropy; they call *that* the Shannon bound, i.e. they assume no higher-order structure |
| Unweight | per-tensor 16-entry palette captures >99%; no context modelling attempted |

Total realistic context gain: **~0.1 bit/weight on 10.6, i.e. <1%.** Weight
matrices are, to a lossless coder, i.i.d. draws from a per-tensor
heavy-tailed distribution. Contrast with images/audio/binaries where context
modelling is worth 20-50%. This is the single most important finding for the
framework: the predictor's value in this domain is *structural alignment*,
not *statistical prediction*.

Two small things a per-tensor model should still do: (i) separate exponent
tables per tensor (embeddings, layernorm and MLP have visibly different
histograms; the 0.07-bit row gain is mostly this), (ii) handle "clean" FP32
tensors whose low mantissa bytes are zero (ZipNN's 42-50% class) by
detecting the zero byte-plane and coding it as such.

### 4d. Quantized formats: predicting block scales

- Q4_K: 16 bytes of scale/min per 256 weights (11% of the block). Scales are
  fp16 and correlated across neighbouring blocks in the same row (same
  output channel, similar magnitude); sub-scales are 6-bit. Hershcovitch
  2025 measured NVFP4 scalers as the only compressible component, worth 5%
  of the file. *(unverified)* predicting a block's fp16 scale from the
  previous block's and coding the residual might halve the scale bytes: 5%
  of the file -> 2-3%. Not worth a module on its own.
- The nibbles are uniform *given the scale*. Any lossless gain there would
  have to come from cross-version prediction (§4b), not intra-file modelling.

### 4e. Float-aware predictive coding prior art

| System | Domain | Predictor | Residual coding | Lossless ratio |
|---|---|---|---|---|
| fpzip (Lindstrom & Isenburg 2006) [LLNL](https://computing.llnl.gov/projects/fpzip), [repo](https://github.com/LLNL/fpzip) | n-D scientific grids | Lorenzo predictor from encoded neighbours | map to sign-magnitude ints, subtract, range-code sign + leading-zero count, raw remaining bits | 1.5-4x on smooth fields |
| ndzip (Knorr et al. 2021) | HPC grids | integer Lorenzo in 4096-element hypercubes | bit transpose + zero-word bitmap | FCBench: Lorenzo-family median 1.22x |
| Gorilla (Facebook 2015) | time series doubles | previous value | XOR, leading/trailing-zero header | depends on smoothness |
| Chimp / Chimp128 (VLDB 2022) [pdf](https://www.vldb.org/pvldb/vol15/p3058-liakos.pdf) | time series | best of last 128 values | XOR; observed that XORs "rarely have long trailing zeros" so trailing-zero fields waste bits | ~50% of Gorilla's space |
| Elf (VLDB 2023) [pdf](https://www.vldb.org/pvldb/vol16/p1763-li.pdf) | time series | erases mantissa bits below decimal precision before XOR | | |
| SZ lossless mode / "Floating-Point Data Transformation" [arXiv:2506.18062](https://arxiv.org/pdf/2506.18062) | HPC | | | |
| FCBench (VLDB 2024) [arXiv:2312.10301](https://arxiv.org/pdf/2312.10301) | 33 datasets, 12 compressors | | | **median CR 1.16**, fp32 1.225, fp64 1.202; dictionary+bitshuffle 1.31 > Lorenzo 1.22 > delta 1.12; "floating-point data is difficult to compress" |

The lesson from scientific-data compression transfers directly: prediction
helps only when the field is *smooth* along the prediction axis, and then
only in the exponent + top mantissa bits; the low mantissa bits are noise
everywhere. Weight matrices are not smooth along any axis (ZipNN's shuffle
test), so the only "smooth" axis is *time* (version-to-version), which is §2.
FM-Delta is literally fpzip's residual coder (monotone int map, MSB-position
symbols + raw bits) with `prev_version` as the predictor, and it gets 50-65%.

---

## 5. Deployment shape

| Fact | Value | Source |
|---|---|---|
| Artifact sizes | 7B bf16 ~15 GB (Llama-3-8B: 15 GB / 16.06 GB); 70B bf16 141 GB; 405B 812 GB; Mistral-7B "over 14.5 GB" | DFloat11 Table 1; NVIDIA streamer blog; ZipNN §I |
| Hub egress | Mistral-7B alone: 2.77 M downloads/month x 14.5 GB = **~40 PB/month** | [ZipNN](https://arxiv.org/pdf/2411.05239) §I |
| Hub storage | 45 PB across 2 M repos; PyTorch checkpoints ~200 TB (2024); GGUF 3.5 PB; "over 17 PB of models in 2024"; fine-tunes are 99.2% of LLM storage (3,243 of 3,269 TB in ZipLLM's sample) and 99.6% of model count | Xet blogs; ZipLLM §3 |
| Inactive fine-tunes | FM-Delta: 89% of sampled fine-tuned models on the hub are inactive; LLaMA-7B had 5,112 fine-tunes, 91% inactive | FM-Delta Fig 1 |
| Cloud egress price | AWS $0.09/GB, Azure $0.087, GCP $0.12 entry tier; R2 $0 | [egresscost.com](https://egresscost.com/), [Spheron](https://www.spheron.network/blog/gpu-cloud-egress-data-transfer-costs-ai-workloads-2026/) *(secondary)* |
| Worked example | 200 GB checkpoint loaded 20x/month (restarts, scaling, node failures) = 4 TB = $200-480/month egress per model | Spheron *(secondary)* |
| Cold start | 232.8 GiB model: 3-5 min GPU-allocated with default vLLM loader; Llama-3-8B from S3: 4.9 s with 32-way streaming vs 37 s Tensorizer; vLLM ready 23 s vs 65 s; naive HF load of 70B "up to 10 minutes"; Modal GPU snapshot 460 s -> 70 s | Azure/AKS blogs; NVIDIA blog; [dreaming.press](https://dreaming.press/posts/2026-06-27-scale-to-zero-llm-inference-gpu-cold-starts.html) *(secondary)* |
| Idle GPU cost during load | H100 $2-3+/h; a 5-minute cold start on an 8-GPU node is ~$2 of wasted GPU per replica per scale-up | dreaming.press *(secondary)* |
| RL sync | 32B bf16 policy (62 GB) sync to inference workers: 14 min per step dense; 7B: 14 GB/worker dense vs 140 MB PULSESync | [arXiv:2602.03839](https://arxiv.org/pdf/2602.03839) |
| Edge / on-device | Ollama, Docker Model Runner pull whole GGUF blobs; no delta path | §3 |

Has anyone quantified "a fine-tune update should be a 1% patch"? No — and the
data says it isn't one. The closest quantified claims are:

- Xet's marketing example: "fine-tuning a 50 GB model and changing 2% of its
  weights ... only the changed chunks move" ([JFrog/HF](https://jfrog.com/blog/native-xet-support-in-jfrog-artifactory/)).
  Fine-tunes do not change 2% of the weights; they change 98% of the bf16
  values (§2.1). Xet's own measured range for fine-tune/checkpoint repos is
  30-85% dedup, and most of that is tokenizer/embedding/unchanged-shard reuse.
- FM-Delta: family storage -40..-52%, "total cost savings of the cloud can be
  at least 40%".
- ZipLLM: -54% over 3,048 LLMs, "$2.2M/yr at S3 pricing on 17 PB".
- PULSESync: >100x, but only for the trainer -> inference worker per-step
  path, and it needs the previous step's weights resident on the worker.

The economically real pain is (i) cold-start wall clock, which is a
bandwidth/parallelism problem already attacked by streaming loaders and is
*hurt* by any decompression slower than ~2-10 GB/s per node, and (ii) hub
storage/egress, where 1.5x lossless is worth petabytes and is exactly what
ZipNN/Xet-BG4 are deploying.

---

## 6. Local measurement (for reproducibility)

Setup: `Qwen/Qwen2.5-0.5B/model.safetensors` and
`Qwen/Qwen2.5-0.5B-Instruct/model.safetensors` (988,097,824 bytes each, 290
tensors, all bf16, 494,032,768 parameters, headers 34,770 bytes and
identical). Python 3 + numpy 2.2.4, zstd CLI 1.5.x. Per-tensor bincounts of
exponent (8 bit), mantissa (7 bit), 16-bit symbol, joint (exp_i, exp_{i-1}),
joint (exp, mant), per-row exponent histograms for 2-D tensors; XOR and
monotone-integer delta between the two files; zstd on raw, byte-grouped
(hi|lo) and delta streams. Entropies are symbol-weighted across tensors.
Runtime ~50 s single-threaded for the histograms.

Output (verbatim):

```
header bytes base/inst 34770 34770 identical layout: True
tensors 290 dtypes {'BF16'}
params 494032768
--- base model, order-0 entropies (bits/weight) ---
H(exp)=2.639 H(mant)=6.957 H(sym16)=10.554  ZipNN-like(H(exp)+8)=10.639
distinct exponents used: 38 top-16 exp coverage: 0.9997
H(exp|prev exp)=2.632  H(mant|exp)=6.916  H(exp|row) [2D tensors]=2.568
zstd-3 whole bf16 stream: 77.83%
zstd-3 byte-grouped (hi|lo): hi=3.403 b/w lo=7.971 b/w total=71.09%
--- delta base->instruct ---
identical bf16 values: 2.07%
xor bit-flip fraction by bit (15=sign,14..7=exp,6..0=mant):
  [0.029, 0.0, 0.0, 0.0, 0.0029, 0.0532, 0.0837, 0.1337,
   0.2128, 0.3075, 0.4088, 0.4765, 0.498, 0.5, 0.5, 0.5]
H(xor sym16)=8.101 b/w  H(int delta)=7.710 b/w  (vs standalone H(sym16)=10.554)
zstd-3 xor raw: 62.29%  xor byte-grouped: 56.32%
zstd-3 intdelta byte-grouped: 57.04%
zstd-19 xor byte-grouped: 53.18%
identical fraction by tensor class: embed_tokens 0.017, down_proj 0.022,
  gate_proj 0.022, up_proj 0.022, q_proj 0.02, o_proj 0.024, k_proj 0.03,
  v_proj 0.03, input_layernorm 0.356, post_attention_layernorm 0.204,
  model.norm 0.006
```

Caveats: one model pair, 0.5B scale, one fine-tune recipe (SFT + RL, many
thousand steps). The literature numbers in §1-2 at 7B-405B agree with every
figure here to within a few percent, so I am treating it as representative
of *full* fine-tunes. It says nothing about LoRA-merged or few-hundred-step
fine-tunes, which should be closer to the 38% (checkpoint-delta) end.

---

## 7. Assessment for a predictive codec

### 7.1 Realistic gain over zstd / ZipNN

**(i) Full-file compression.**

| baseline | bf16 bits/weight | vs. |
|---|---|---|
| raw | 16.00 | |
| zstd -3 on raw bytes | 12.45 (77.8%) | |
| zstd on byte-grouped (Xet BG4 with zstd instead of LZ4 would be here) | 11.37 (71.1%) | |
| ZipNN / DFloat11 / NeuZip (order-0 exponent Huffman/ANS + raw sign/mantissa) | 10.6-11.1 (66-69%) | |
| order-0 per-tensor entropy ("Shannon bound" paper's bound) | 10.55 | |
| + every context trick measured in §4c | ~10.45 | |
| **best-case predictor codec** | **~10.5 (65.5%)** | **1.19x over zstd-raw, 1.08x over BG4+zstd, ~1.01-1.03x over ZipNN** |

fp32 "clean" models: byte-grouped zstd already gets 42-50%; a predictor that
detects zero byte-planes adds nothing over that. fp8: 55-70%. int4/fp4
nibbles: ~1.0x; scales: 5% of file.

Verdict: full-file is a solved 1.5x. A framework predictor buys at most a
few percent over ZipNN and would be judged on throughput, not ratio.

**(ii) Version-to-version delta.**

| scenario | best published / measured lossless | what a tensor-aligned predictor + entropy coder should get | vs zstd on aligned XOR | vs Xet CDC |
|---|---|---|---|---|
| full fine-tune vs base (same layout) | 48-65% of new file (FM-Delta, ZipNN, BitX); 53-56% *(measured)* | **~48-52%** (int-delta conditioned on base exponent, per-tensor tables; unverified 0.2-0.5 bit gain) | 1.1-1.2x | ~2x (Xet ~0% chunk reuse on the weight tensors, so it stores the whole fine-tune at ~65-70% after BG4+LZ4) |
| fine-tune vs base, re-sharded / reordered | same, once aligned | same | **large** — byte-offset delta and CDC get nothing; the alignment is the whole win | large |
| consecutive checkpoints, mid-training | 38-55% (Hershcovitch 2025, IBM, ZipNN) | ~35-45% | 1.1-1.2x | ~2x |
| consecutive checkpoints, late / low-LR / RL per-step | <1% (PULSESync 140 MB / 14 GB) | <1% | ~1x (sparse index+value+zstd is already near-optimal) | 100x |
| GGUF quant from bf16 source, exact requantize | not published | **~0.1%** if bit-exact, else ~100% | n/a | n/a |
| bf16 -> fp32 upcast | 0% | 0% | | |
| bf16 -> int8/fp8 quant (QStore-style) | 39% of pair | similar | | |

Verdict: for the common case (a full fine-tune) the honest number is "the
patch is about half the file", **1.3-1.4x better than compressing the new
file standalone and ~2x better than what Hugging Face actually stores**. The
100x cases exist but are either already solved trivially (RL per-step sparse
patches) or depend on bit-exact recomputation of a lossy pipeline (LoRA
merge, quantization) that nobody has demonstrated.

### 7.2 Hard floor vs exploitable

Hard floor (do not spend design effort here):
- Low 3-4 mantissa bits of every changed weight: 3+ bits/weight, uniform.
- Mantissa of standalone weights: 6.9-7.0 of 7 bits.
- Quantized nibbles given their scale: uniform.
- Neighbourhood/row context on exponents: <0.1 bit.
- fp32 Adam moment tensors in checkpoints: 1.06-1.11x (nvCOMP). If the
  checkpoint includes optimizer state, it dominates and is incompressible.

Exploitable:
- Exponent order-0 (2.6 of 8 bits): 5.4 bits/weight, already taken by ZipNN.
- Version alignment by tensor name/shape/dtype regardless of file layout:
  turns a 0% CDC hit into a 45-55% delta. This is the *predictor* content.
- Sign + high exponent bits + top mantissa bit across versions: ~2.5
  bits/weight (10.6 -> 8.1).
- Conditioning the delta on the base exponent: est. 0.2-0.5 bit/weight
  *(unverified)*.
- Zero byte-planes in "clean" fp32 (already in zstd's reach).
- Headers, tokenizer, metadata: 100% predictable, negligible bytes.
- Bit-exact recompute of deterministic derivations (upcasts trivially;
  LoRA-merge and quantization only if the pipeline is pinned).

### 7.3 What the predictor module would compute

1. Parse the container: safetensors (8-byte len, JSON, offsets) or GGUF
   (magic, KV table, tensor infos, alignment). Emit a normalised tensor map
   `{name -> (dtype, shape, byte range)}` for both old and new. Handle
   multi-shard indexes so the *set of files* is the unit, not one file.
2. Predict the new header from the old one + the new tensor map (the JSON
   text is a pure function of (writer, map)); code textual residual.
3. For each new tensor: pick the prediction source — same-name tensor in
   old (exact shape), overlapping slice (shape change), re-cast (dtype
   change), or none. Optional exact-recompute predictors (quantize, upcast,
   LoRA merge) behind a "verify hash, fall back" gate.
4. Residual transform: per element, `XOR(new, pred)` or monotone-int delta;
   split into (sign+exponent-delta | mantissa-delta) streams; per-tensor
   adaptive frequency tables; ANS/FSE code the exponent-side stream and the
   top mantissa bits, emit the low k mantissa bits raw where measured entropy
   > 0.95 bit/bit (k is 3 for a full fine-tune, 0 for a per-step RL patch,
   7 for an unrelated model).
5. For unpredicted tensors: ZipNN-equivalent exponent-split order-0 coding.
6. Sparse fast path: if the identical-fraction is >90%, switch to
   index+value patches (PULSESync) — the entropy coder path would also get
   there but slower.

The decoder is embarrassingly parallel per tensor and needs the old file
resident (mmap) — same shape as binsync's old-file-resident prediction.

### 7.4 Throughput requirements

Inputs are 1-800 GB. Reference points:
- zstd single-thread ~1 GB/s decompress on bf16; ZipNN 1.65 GB/s
  single-thread, 80 GB/s on a 2-socket Xeon; BitX ~7.9 GB/s with 192
  threads; FM-Delta's range coder 0.1 GB/s single-thread (too slow — a
  16 GB model takes 160 s on one core).
- Streaming loaders already move 1-2 GiB/s from local NVMe and ~1 GB/s from
  S3 per node (NVIDIA blog) and 2.1 GB/s upload on Xet. A decoder slower than
  the link (~1-3 GB/s per node today, 10+ GB/s on 100 GbE + NVMe) turns a
  bandwidth win into a wall-clock loss; DFloat11/Unweight pay 30-40%
  inference throughput to decode on the GPU, which is acceptable for memory
  but not for a distribution codec.
- Target: **>=2 GB/s per core on the residual path** (order-0 tANS/FSE at
  1-2 GB/s/core is standard; Huffman with a 16-entry table is faster), linear
  scaling to 20-50 GB/s across a socket, memory-bound not compute-bound.
  Because the coding is order-0 per tensor, the encoder only needs one
  histogram pass + one coding pass; no expensive prediction (no Lorenzo, no
  neural context) is justified by the <0.1-bit gains in §4c.
- Go implementation risk: a scalar bf16 XOR + histogram + FSE in Go will sit
  around 0.5-1 GB/s/core; getting to the 2+ GB/s/core that ZipNN's C reaches
  will need assembly or SIMD-friendly byte-plane layouts. Since the ratio
  ceiling is fixed, throughput is the *only* axis on which this module can
  compete with ZipNN/Xet-BG4.

### 7.5 Bottom line

Model weights are a domain where the predictor's contribution is
*book-keeping* (find the matching tensor, split the fields, pick sparse vs
dense) and the entropy coder does the rest; the achievable ratios are
~1.5x standalone and ~2x delta, bounded by coin-flip mantissa bits that no
predictor can see. That makes it a good, cheap **second-tier plug-in** for the
framework — it validates the "structure-aware alignment + generic residual
coder" architecture and beats what hubs ship today — but it is not evidence
for the framework's headline thesis, and it should not be presented as such.
If you want a weights-adjacent claim that *is* big, it is "bit-exact
recompute of derived artifacts" (upcasts, quantizations, LoRA merges): 100x+
when it works, unproven, and entirely dependent on pinning the producer.
