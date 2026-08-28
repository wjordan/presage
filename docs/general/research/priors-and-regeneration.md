# Priors and regeneration: the codec without an "old"

*2026-08-27. Thinking note, with three small measurements. Question posed:
does "predict second-order change from first-order change" say anything
useful when there is no previous version — for plain compression, for
compression against a synthetic starting point such as a hello-world
binary, or as a front-end that strips structure so a standard coder does
better?*

## 1. Reframing: delta coding is conditional coding, and "old" is one oracle

Everything in go-binsync is `code(new | old)`. Percival's taxonomy names the
parts: the first-order change (what the author did) is small; the
second-order change (what the toolchain did in response) is large but a
deterministic function of the first-order change plus the old artefact.
The codec ships the first-order change and regenerates the rest.

Drop the "change" and the same shape is still there inside a single file.
A binary is **primary content** (function bodies' semantics, string
literals, line numbers) plus **derived content** (relocations, symbol and
hash tables, pclntab layout, type descriptors' cross-references, section
headers, padding, checksums). Derived content is a deterministic function
of primary content and toolchain rules. A statistical coder pays for it at
roughly its conditional entropy given a few kilobytes of context; a
structural model pays nothing beyond the description of the rule.

So the reference-free version of the thesis is: **regenerate derived
content from primary content**. And the reference-set version generalises
"old" to any object the decoder can obtain or construct — a previous
release, a shared library of fragments, a toolchain output, or the earlier
part of the same file. The spec already allows a reference set of
0..n objects; this note is about what to put in it and what changes when
n = 0.

## 2. Measurement: a prior is worth exactly what the codec can align

Three targets, all go1.27.0 linux/amd64, against a 2.3 MB hello-world
built by the same toolchain (`fmt.Println`). Function bodies compared via
`x86.ContentHash` (PC-relative fields zeroed).

| target | text | funcs | byte-exact in hello | reloc-modulo in hello | reloc-modulo self-dup |
|---|---|---|---|---|---|
| gofmt (3.1 MB) | 1.35 MB | 3 228 | 21 KB (1.6 %) | **626 KB (46.5 %)** | 34 KB (2.5 %) |
| go-binsync (20 MB) | 6.5 MB | 11 913 | 25 KB (0.4 %) | **648 KB (10.0 %)** | 481 KB (7.4 %) |
| cmd/compile (28 MB) | 13.0 MB | 20 267 | 24 KB (0.2 %) | **538 KB (4.1 %)** | 438 KB (3.4 %) |

Two things to read off:

1. Byte-exact reuse of the prior — all an LZ dictionary or a
   content-addressed chunk store can see — is **~30× smaller** than reuse
   modulo relocation. That is the same 30× that separates bsdiff from
   go-binsync in the delta setting, and for the same reason: every
   function that moves is rewritten every ~10 bytes. A prior is useless to
   a coder that cannot align it and near-free to one that can.
2. The **entire** hello-world text (626 KB) is present modulo relocation
   in every target. The prior's value is bounded by its size, not by
   anything about the target. Hello-world is simply a small prior.

End to end, feeding hello-world as `old` to the existing codec:

| target | zstd -19 | zstd -19 + hello as dictionary | xz -9 | xz -9 + BCJ | hello-world patch |
|---|---|---|---|---|---|
| gofmt | 1 152 642 | 708 935 | 1 066 652 | 1 021 880 | **602 064** (−48 % vs zstd, −41 % vs xz+BCJ) |
| go-binsync | 9 640 802 | 9 258 394 | 9 214 484 | 9 071 336 | 9 214 472 (loses to xz+BCJ by 1.6 %) |
| cmd/compile | 7 428 641 | 7 266 937 | 6 661 576 | 6 312 244 | 7 063 077 (loses by 12 %) |

Also to read off:

- Where the prior covers half the text (gofmt), the answer to the
  question "is hello-world a better starting point than a blank slate" is
  **yes, 2×** — and note the zstd dictionary gets a real 38 % too, from
  exact-matching rodata, names and unicode tables plus short matches
  between displacements. Structure buys the other 15 %.
- Where the prior covers 4–10 %, the patch is decided by the residual
  coder on the unmatched 90 %, and there go-binsync's residual coder is a
  zstd-class coder with no instruction-aware transform. xz's BCJ filter is
  worth 1.5–5 % on these files (`xz` vs `xz --x86`), and its larger window
  the rest. This is a gap in the current codec, not in the idea: the
  relocation canonicaliser already exists and is applied only to *matched*
  functions. Applied to unmatched code as a plain relative→canonical
  filter it is a BCJ, for free. See §5.

## 3. Scaling the prior: the dependency set is the generative description

If a prior is worth its size, make it bigger — but not by picking a
larger arbitrary binary. Go tells you exactly which bigger prior to use.
Text bytes by origin in the 20 MB go-binsync binary:

| origin | text | share |
|---|---|---|
| standard library (incl. runtime, net/http, crypto) | 3.77 MB | 58.0 % |
| github.com/aws/aws-sdk-go-v2 | 1.39 MB | 21.4 % |
| golang.org/x/crypto, klauspost/compress, smithy-go, x/arch, blake3, … | ~0.6 MB | 9–10 % |
| **the main module + `main`** | **0.30 MB** | **4.7 %** |

Go compiles each package to an archive independent of its importer; a
dependency's function bodies are bit-identical modulo relocation across
every program that links them with the same toolchain and build flags
(exceptions: generic instantiations, which are per-shape and land in the
importer; inlined bodies; PGO, `-race`, `GOAMD64`, `-gcflags`, which
change codegen and must be part of the prior's identity). So a decoder
holding — or fetching by hash — the package archives named by
`go.sum` + toolchain version could regenerate ~95 % of this binary's text
and, through the same regenerators go-binsync already has, most of its
pclntab and type metadata. The first-order description of the whole
binary is `go.mod`/`go.sum`/toolchain/flags (a few KB) plus the main
module's 300 KB of code plus a layout.

This is the point where the LLM analogy inverts. Generative text
compression fails on determinism and inference cost; a compiler is a
deterministic generator with a tiny, exact, already-published prompt
(`go.sum`), and its output is cached everywhere (`GOCACHE`, module proxies,
CI). Kolmogorov's "shortest program" is, for a Go binary, literally the
build inputs; the codec's job is to sit at whatever point on the
source→binary line the decoder can cheaply reach. Three points on that
line, cheapest decoder first:

| decoder holds | patch carries | realistic size, 20 MB service binary |
|---|---|---|
| nothing (plain compression) | everything, structurally filtered | ~8.5–9 MB |
| the previous release | first-order change | 0.1–0.5 MB (measured, go-binsync) |
| package archives for the dependency set | main-module code + layout + residual | ~0.5–1 MB *(estimate: 5 % of text plus rodata/pcln residue; not measured)* |
| toolchain + module cache | `go.sum` + source of main module | tens of KB, but the decoder is a compiler |

The third row is new and has a name in another field: CRAM compresses
DNA reads against a reference genome the decoder is assumed to have and
fetches by hash, and gets 3–5× over gzip that way. The Go equivalent is a
**link-plan patch**: the decoder is linker-shaped (place these canonical
functions at these addresses, relocate, regenerate tables), and the
reference set is content-addressed at package or function granularity.

What it needs from the design, beyond the spec as written:

- **Reference discovery.** The delta case names one reference by hash. A
  prior-library case names a set, and the decoder may hold only part of
  it. Cheapest layered scheme: name the set by one hash; inside, name
  packages by hash; only for packages the decoder lacks fall back to
  per-function canonical hashes (8 B × 12k functions ≈ 100 KB, too much to
  ship unconditionally, fine as a negotiated fallback).
- **Prior tolerance.** Decoders will hold a *near* prior — the same
  package compiled by go1.27.1 instead of go1.27.0. This is exactly where
  Percival's universal (syndrome) deltas fit: a small correction that
  fixes a bounded number of differences in whatever near-prior the decoder
  has, without the encoder knowing which one. The spec's reserved
  syndrome mode (§7.5) should be motivated by this case, not by the
  one-way-delta case.
- **Canonical form = the object file.** See §7 for the full description
  (`delink`) and the cross-project measurement that motivates it.

## 4. Self-similarity modulo relocation

The last column of the first table: 2.5–7.4 % of text is functions whose
canonical form has already appeared *earlier in the same binary*.
Mostly generic instantiations (`slices.pdqsortCmpFunc[go.shape…]` families
dominate the compiler's list), method wrappers, and generated code. LZ
sees none of it, because each copy is relocated. A `copy` op whose source
is an earlier target region *with relocation applied* captures it; the
cost is one entry in the plan per duplicate. This is the n = 0 special
case of the reference set: the reference is the target's own prefix. It
also gives the encoder a cheap signal (canonical-hash collisions) rather
than requiring a search.

Worth ~5 % of text, i.e. ~2 % of a compressed binary. Small, but it is
the same op as §3, so it comes for free once that exists.

## 5. Predict nothing, type everything: the standalone product

The user's second framing — "use whatever structure we have to reduce
latent entropy so the stream is more compressible by standard methods" —
is a known and productive technique with a low ceiling: 7-zip's BCJ2
(separate the call/jump target stream, convert relative to absolute so
repeated targets repeat), LZX's E8 transform, xz's BCJ, and at the far end
paq8's x86 models. Gains over plain LZ on executables: BCJ 2–5 % (measured
above), BCJ2 ~5–10 %, context-mixing 15–25 % at 100–1000× the cost.

Where presage adds something is not a better statistical model — a
context-mixing coder is off the table for a codec that must decode at
hundreds of MB/s — but a **format-driven, pluggable, multi-stream filter**:
the spec's four typed sub-streams (control, literals, pointer diffs, table
payloads) produced by a module with an empty reference set. Every plan op
has a reference-free dual:

| delta op | reference-free dual |
|---|---|
| `relocate(isa)` against old | relative→canonical filter (BCJ), targets to a separate stream |
| `table` regenerated from old's table + change | `table` regenerated from primary content (pclntab layout from function sizes; `.gnu.hash` from `.dynsym`; zip central directory from local headers; sqlite indexes from tables) |
| `map` / pointer diff against old | pointer fields → delta-from-previous-pointer stream |
| `copy` from old | `copy` from own prefix, modulo relocation (§4) |
| `recompress` (deflate) | the same — it never needed a reference |

So the standalone codec is not a new design; it is the delta codec with
`old = ∅`, and the same modules serve both. Expected result on Go
binaries: 5–10 % better than xz+BCJ at zstd decode speeds, mostly from
pclntab and type-metadata regeneration, which no generic filter attempts.
That alone would not justify the project; it is a by-product.

The one general rule worth stating: **anything a tool "builds" or
"indexes" is derived content.** Section headers, symbol hash tables,
pclntab, zip/PDF/tar directories and checksums, sqlite index b-trees and
freelists, parquet dictionaries, git packfile indices, container manifest
digests. Each has a canonical primary form and a deterministic
regenerator that already exists as code somewhere (the tool that built
it). preflate/precomp are this idea for one derived form (deflate
streams); a module system makes it one op per derived form. The sqlite
case is worth a specific look given this machine's other repositories:
index pages are pure functions of table pages plus schema, and a
`VACUUM`-canonical database with indexes dropped is a small fraction of a
working file — a "primary-only" form that the decoder rebuilds.

## 6. Where this leads, ranked by interest

1. **Link-plan patches against a package-archive prior** (§3). Biggest
   number in this note, an unmeasured 95 % coverage ceiling, and it is
   Go-specific in the same way the 28–67× result was: the toolchain is
   simple and its cache is universal. Measurable in a day: build the
   dependency archives, canonicalise, count what the decoder would need.
   Depends on `delink` and reference discovery, which are new spec items.
2. **Relocation filter on unmatched code** (§2, §5). Missing today;
   closes a measured 2–12 % gap against xz+BCJ in the current codec at
   near-zero cost. Should go into the residual coder regardless of the
   general project.
3. **Prior tolerance via syndromes** (§3). Gives §7.5 of the spec a real
   customer.
4. **Self-copy modulo relocation** (§4). ~2 % of compressed size, free
   once 1 exists.
5. **Derived-content regeneration as a standalone filter** (§5). Broad,
   shallow, 5–10 %; sqlite is the interesting non-executable instance.

What I would not pursue: an LLM-style learned prior for binaries. The
measurement in §2 says the value of a prior is its aligned overlap with
the target, and a compiler-produced prior has 100 % aligned overlap on the
bytes it covers at zero inference cost. There is no version of a learned
model that competes with the actual generator on its own output.

## 7. Delink, and the cross-project measurement

### 7.1 The idea

A linked function body is an object-file function with the linker's
answers written into it: every `call rel32`, `lea rip+disp`, jump to
another function, and every absolute pointer in data was a symbolic
reference before linking, and became a number that depends on where
everything else landed. That is why the same code hashes differently in
every program and why byte-exact dedup sees ~0 % across binaries. Delink
undoes exactly that step and nothing else.

Per binary:

1. Split into *units* using structure the file already carries:
   functions from pclntab (boundaries and names survive stripping in Go),
   rodata symbols and strings, type descriptors, itabs. Later ELF:
   `.eh_frame_hdr` plus relocations; PE: `.pdata`.
2. Decode each code unit; for every PC-relative or absolute field resolve
   the target to (unit, offset) and zero the field. Same for pointer
   fields in data units. Output per unit: canonical bytes plus a
   relocation list `[(field offset, width, kind, target unit, addend)]` —
   the object-file representation. `x86.ContentHash` is already the hash
   of the canonical bytes.
3. Two identities: `h_code = hash(canonical bytes)` ("same instructions,
   whatever the callees") and optionally a Merkle
   `h_unit = hash(h_code, [(offset, kind, target h_unit)])` ("same
   function and same callees", true object-level identity; needs SCC
   handling for call cycles, so name-keyed edges are the practical form).

The system on top is a **linker-shaped decoder**:

- **Pool**: content-addressed store of units keyed by `h_code`, holding
  the canonical bytes *and the symbolic relocation list*. Because the
  edges live in the pool, a binary's plan does not have to ship them.
- **Link plan** per binary: the ordered unit list (the layout) plus
  residual for whatever is not regenerable (line tables, new code,
  padding). Unit lists compress well: dependencies are address-contiguous
  in package order, so a plan is mostly "package P at version V" runs,
  not 12k hashes. Upper bound 8 B per function; kubectl (83k functions)
  ≤ 660 KB against 27 MB of text before run-encoding.
- **Decode = link**: place units, resolve addresses, apply relocations
  (`x86.Relocate` exists), regenerate derived tables (pclntab layout,
  findfunctab, typelinks, moduledata — regenerators exist), apply
  residual, verify `H_target`.
- **Tolerance**: a unit absent from the pool but *near* one (same name,
  adjacent dependency or toolchain version) is delta-coded against it with
  the existing correction codec.
- **One format, three uses**: version-to-version delta is a pool holding
  the old binary's units; fleet/registry dedup is a pool holding
  everyone's units; standalone compression is an empty pool with the
  typed streams still applied.

### 7.2 The measurement

Does cross-*program* reuse exist in practice, or does every project pin
different dependency versions and different toolchains? Corpus: the
current release binaries of 30 unrelated Go projects (43 binaries,
1.22 GB of function text, 12 distinct toolchain versions from go1.24.0
to go1.27.0): kubernetes ×6, prometheus/alertmanager/node_exporter,
etcd ×3, helm, argocd, terraform, vault, consul, traefik, caddy, gitea,
loki, trivy, cilium, k9s, kind, kustomize, compose, minio, rclone,
restic, syncthing, hugo, gh, fzf, age ×4, plus four local go1.27 builds.
Function boundaries from `debug/gosym` (with `runtime.text` read from
moduledata, which matters for externally-linked cgo binaries where
`.text` starts with crt code); canonical form = PC-relative fields
zeroed. Reuse is counted **strictly across projects** — kubectl reusing
kubelet's code does not count.

| | share of all function text |
|---|---|
| byte-exact present in another project | **1.8 %** |
| relocation-modulo present in another project | **65.8 %** |
| — of which stdlib/runtime (16 % of corpus) | 86.1 % |
| — of which third-party modules (84 % of corpus) | 64.1 % |
| same-named function elsewhere, ≥95 % similar canonical bytes | 1.3 % |
| same-named, 50–95 % similar | 3.3 % |
| pool size, byte-exact dedup | 1.02× |
| pool size, relocation-modulo dedup | **3.39×** |

The same 30× exact-vs-aligned gap as the delta result and the hello-world
probe, now across unrelated programs. It is not a stdlib artefact:
aws-sdk-go-v2 (88 MB across the corpus) is 73 % shared, k8s.io/api 87 %,
protobuf 99 %, go-redis 85 %, arrow 96 %, envoy control-plane 83 %.
What is not shared is what one would expect: each project's own code
(`k8s.io/kubernetes/pkg` 18 %, `hashicorp/vault` 11 %) and rare
dependencies (msgraph-sdk 3.5 %, only in trivy).

Per binary, cross-project coverage runs 55–94 % for anything built with
a toolchain that at least one other project also used, and collapses for
lone toolchains: minio (go1.24.6, the only one) 18 %, the go1.27 local
builds 4–19 %. Toolchain version is the dominant key — not because
dependencies differ but because codegen differs: the same-named functions
that fail to match are mostly *not* close (similarity below 80 % for the
majority), so cross-version tolerance recovers a few percent, not the
gap. In practice this is fine: release cadences cluster on the latest
toolchain (23 of 43 binaries here are go1.26.5/6), and a pool keyed by
toolchain is still one pool per version, not one per project.

Fleet case (the six kubernetes components, one toolchain): pairwise
coverage 50–96 %; kube-proxy is 94–96 % present in any of the others.
Unrelated projects on the same toolchain: kubectl→trivy 81 %,
alertmanager→prometheus 76 %, compose→trivy 64 %, prometheus→trivy 58 %.

### 7.3 Reading

- The decisive question was whether cross-program relocation-modulo
  overlap is >50 % or ~10 %. It is 66 %, and 3.4× on the pool. The
  registry / fleet / binary-cache product is real: store canonical units
  once per toolchain version, ship link plans.
- What the number does *not* say: the link plan and residual are not
  free (unit lists, line tables, rodata, type metadata are not in this
  measurement, which covers function text only — 45–55 % of file bytes).
  The end-to-end number needs the decoder built; the honest expectation
  from the go-binsync stage breakdown is that pclntab and rodata follow
  text at similar ratios once function correspondence is known, since
  that is exactly what the existing regenerators do.
- Text sharing being keyed by toolchain version, not project, is the
  useful design constraint: pools are per-toolchain, tolerance is a
  minor feature, and the encoder should name the toolchain first.
- Measurement tool and corpus: `~/.cache/presage-corpus/` (`pooltool/`,
  `fetch.sh`, `extract.sh`, `result3.txt`), outside the repository.
