# Portable, deterministic predictor modules: prior art and options

Research notes for generalising the go-binsync predict-then-correct codec into a
framework with pluggable, shippable predictor modules. The question this
document tries to answer: *what should a predictor module be* — bytecode for a
tiny custom VM (ZPAQ), a restricted WebAssembly profile, a vendored
source-language artefact (Wuffs), or a declarative description (Kaitai) — given
that it must run bit-identically on encoder and decoder, on any host, possibly
without being built into the decoder ahead of time.

Everything with a URL was read (or its PDF converted to text) during this
research pass, August 2026. Numbers without a URL are my own extrapolations and
are labelled as such. Where I could not verify a claim I say so.

Contents

1. ZPAQ / ZPAQL — the closest prior art
2. WebAssembly as a deterministic codec substrate
3. Alternatives to Wasm
4. Precedents for "the stream carries its own decoder/model"
5. Security, resource limits, and verification
6. What this means for our design

---

## 1. ZPAQ / ZPAQL

Primary sources: the Level 2 specification v2.06 (Mahoney, 2016)
[zpaq206.pdf](https://mattmahoney.net/dc/zpaq206.pdf); "The ZPAQ Compression
Algorithm" (Mahoney, Dec 2015)
[zpaq_compression.pdf](https://mattmahoney.net/dc/zpaq_compression.pdf); the
libzpaq header [libzpaq.h](https://raw.githubusercontent.com/zpaq/zpaq/master/libzpaq.h);
the project page [zpaq.html](https://mattmahoney.net/dc/zpaq.html) and utilities
page [zpaqutil.html](https://mattmahoney.net/dc/zpaqutil.html).

### 1.1 What is in the stream

ZPAQ's defining property: "The ZPAQ standard does not specify a compression
algorithm. Rather, it specifies a format for representing the decompression
algorithm in the block headers." Every block header carries three things
(zpaq_compression.pdf §"Format"):

1. a *tree of context-model components* (up to 255) with their parameters;
2. a ZPAQL bytecode program `hcomp`, run once per decoded byte, that computes
   the context hashes feeding those components;
3. an optional ZPAQL program `pcomp` that post-processes the decoded byte stream
   (inverse E8E9, inverse BWT, LZ77 decode, ...).

Block grammar (spec §2, level 2):

```
block  ::= "zPQ" level(1..2) HPROG=1 hsize[0..1] header segment* EOB=255
header ::= hh hm ph pm n(0..255) comp[0..n-1] END=0 hcomp END=0
comp   ::= CONST=1 c | CM=2 sizebits limit | ICM=3 sizebits(0..26)
         | MATCH=4 sizebits bufbits | AVG=5 j k wt | MIX2=6 sizebits j k rate mask
         | MIX=7 sizebits j m rate mask | ISSE=8 sizebits(0..26) j
         | SSE=9 sizebits j start limit
```

`hh, hm, ph, pm` are log2 sizes of the two VMs' `H` (32-bit words) and `M`
(bytes) arrays. Level 2 (2012) added `n = 0`: no model, data stored raw (or
LZ-coded by `pcomp` alone), "to allow fast storage and retrieval of lightly
compressed or uncompressed data."

Decoding is bitwise: components emit stretched probabilities `ln(p1/p0)` in a
fixed chain, the last feeds a 32-bit binary arithmetic decoder; after each full
byte `hcomp` runs with the byte in `A` and writes new contexts into `H[i]`.
Every segment may end with a SHA-1 of the decompressed segment (`eos ::= 254 |
253 sha1[20]`).

### 1.2 The ZPAQL VM (spec §6)

State: `PC`; four 32-bit registers `A B C D`; a 1-bit flag `F`; `H[2^hbits]` of
u32; `M[2^mbits]` of u8; `R[256]` of u32. All zero-initialised. "The state of
COMP is retained between runs except for A and PC."

Instruction set: 1-byte opcodes, or 2 bytes with an immediate `N` (opcodes ≡ 7
mod 8), plus one 3-byte `LJ N M` (absolute jump to `N + 256*M`). Operand forms
are `A B C D *B *C *D N` where `*B, *C` = `M[B], M[C]` (mod 256) and `*D =
H[D]` (mod 2^32). Ops: `X++ X-- X! X=0 X<>A X=R N R=A N JT JF JMP LJ HALT OUT
HASH HASHD`, plus `A op= Y` for `+ - * / % & &~ | ^ << >>` and comparisons
`== < >` setting `F`. `HASH` is `A := (A + *B + 512) * 773`. There are 30
comparisons/ALU rows × 8 operands; the whole table fits on one page.

Determinism is total by construction:

* all arithmetic is unsigned 32-bit modulo 2^32, no floating point anywhere
  (the components' `squash`/`stretch` are integer table lookups; the training
  rules in §3 are all `floor()` of integer expressions);
* "`A/=Y` divides: if Y > 0 then A := A / Y else A := 0" — division by zero is
  defined, not trapped;
* `A<<=Y`: `A * 2^(Y mod 32)`; shifts are defined for all shift counts;
* array indexes are "modulo the array size. Thus if hh = 8, then H[256] =
  H[0]" — there are no out-of-bounds accesses to trap on;
* a program is padded with two `ERROR` bytes; "If PC not in (0...|prog|-3) then
  exit with an error"; "a stream where either program does not halt, or executes
  ERROR or any reserved instruction, or where the program counter goes outside
  the range of the program is not compliant."

Note what is *not* there: no call/stack ("There is no stack or CALL
instruction"), no I/O except `OUT` of one byte, no host imports, no dynamic
allocation, program limited to 64 KiB (`hsize` is 2 bytes). Non-termination is
"non-compliant" rather than prevented: there is no fuel counter. A decoder that
receives a looping `hcomp` hangs.

Memory is fully computable from the header before allocating anything (spec
§7): `4*2^hh + 2^hm + 4*2^ph + 2^pm` plus per component `CM: 4*SIZE`, `ICM:
64*SIZE+1024`, `MATCH: 4*SIZE + 2^bufbits`, `MIX2: 2*SIZE`, `MIX: 4*m*SIZE`,
`ISSE: 64*SIZE+2048`, `SSE: 128*SIZE`. "A decompresser is said to be compliant
up to its memory limit if it will accept all streams that require less memory."
This is the pattern to copy: *declare* resource requirements in the header;
let the decoder refuse before running.

Versioning: `level` byte (1, 2; 3..127 reserved, "Each level L shall support
reading all levels in the range (1...L)"; 128..255 private); `HPROG`/`PROG`
bytes "intended to indicate the language used in the (hcomp) and (pcomp)
sections" — i.e. Mahoney reserved room for a *second* bytecode language and
never needed it.

### 1.3 JIT

libzpaq.h: "By default, libzpaq uses just-in-time (JIT) acceleration by
translating ZPAQL code to x86-32 or x86-64 internally and executing it...
requires an x86 processor capable of executing SSE2 instructions." Compile with
`-DNOJIT` to interpret. The claimed gain is modest: "This approximately doubles
compression and decompression speed. On other hardware, the byte code is
interpreted." (zpaq_compression.pdf.) The encode.su JIT thread (t=1273) is
behind a 403 for fetchers; search snippets from it repeat the "about twice as
fast" figure. The reason the gain is only 2× is instructive: in a
context-mixing decoder the ZPAQL program runs once per *byte*, while the
component update/predict loop (hash-table probes, mixer dot products) runs once
per *bit* and dominates; ZPAQL is a small slice of the runtime, so JITting it
cannot help much. For a *predictor* that does most of its work in bytecode
this ratio inverts — see §6.

### 1.4 Performance numbers

From the project page [zpaq.html](https://mattmahoney.net/dc/zpaq.html):
method 1 ≈ 128 MB/s compress, ≈ 32 MB/s decompress (LZ77 with `pcomp`-decoded
output, no model). From the 10 GB benchmark
[10gb.html](https://mattmahoney.net/dc/10gb.html), dual Xeon E5-2620 2.0 GHz
(24 threads):

| method | size (bytes)   | compress s | extract s | ≈ extract MB/s |
|--------|----------------|------------|-----------|----------------|
| 1      | 3,833,498,676  | 177        | 67        | 150            |
| 2      | 3,701,584,921  | 187        | 67        | 150            |
| 3      | 3,399,589,497  | 241        | 115       | 87             |
| 4      | 3,066,519,365  | 476        | 251       | 40             |
| 5      | 2,917,361,916  | 1,306      | 852       | 12             |

Multi-threaded (blocks are independent). Single-thread CM decode at method 5 is
of the order of 1 MB/s; that is the cost of bitwise context mixing, not of the
VM.

### 1.5 libzpaq API

[libzpaq.h](https://raw.githubusercontent.com/zpaq/zpaq/master/libzpaq.h):
abstract `Reader { get(); read(buf,n) }` / `Writer { put(c); write(buf,n) }`;
one-shot `compress(Reader*, Writer*, const char* method, ...)` and
`decompress(Reader*, Writer*)`; streaming `Compressor` (`startBlock(level|config)`,
`startSegment`, `compress(n)`, `endSegment(sha1)`, `endBlock()`) and
`Decompresser` (`findBlock(&mem)` — *returns the memory requirement before
decoding*, `findFilename`, `readComment`, `decompress(n)`, `readSegmentEnd`).
The `method` string (`"x4.3ci1"` = 16 MiB blocks, BWT, order-0 ICM, order-1
ISSE chain) is compiled *by the library* into component list + ZPAQL; users
never write bytecode. There is also a source-level ZPAQL assembler (`.cfg`
files) for the `zpaq` CLI. Everything is public domain.

### 1.6 What worked and what did not

Worked:

* Forward compatibility actually held for 15+ years: a 2009 level-1 decoder
  reads only level-1 streams, but a 2012 level-2 decoder reads everything
  produced since, including models invented later (the method 5 model with 20+
  components, the LZ77/BWT `pcomp` decoders). The reference decoder
  `unzpaq206.cpp` is 80,077 bytes.
* Integer-only, wrap-around, all-ops-defined semantics made "same bytes on every
  host" a non-issue. No canonicalisation, no profiles, no CPU feature checks.
* Header-declared memory made resource admission trivial.

Did not:

* The ZPAQL VM has no fuel, so a hostile or buggy stream can hang the decoder.
  ZPAQ archives were always assumed trusted (backups you made yourself).
* ZPAQL is too small to express anything with real data structures — no
  stack, no calls, two flat arrays. `pcomp` LZ77/BWT decoders are written as
  straight-line state machines. Nobody outside Mahoney wrote significant
  ZPAQL; the ecosystem is `method` strings.
* Bit-granular CM dominates speed; the bytecode is a rounding error, so the JIT
  bought only 2×. No SIMD, no 64-bit.
* The design conflates *model* and *decoder*: the same block header must
  describe both the entropy model and the transform. For our purposes the
  interesting half is `pcomp` — a program that turns a small input stream into
  the output — which is exactly a "predictor plus correction" if the input is
  the old file.

---

## 2. WebAssembly as a deterministic codec substrate

### 2.1 Sources of non-determinism in core Wasm

The design doc [Nondeterminism.md](https://github.com/WebAssembly/design/blob/main/Nondeterminism.md)
lists: (a) feature-support variance between engines; (b) the host (imports,
call sequence, argument values); (c) `shared` memory and threads; (d) NaN bit
patterns — "when an arithmetic operator ... receives no NaN input values and
produces a NaN result value, the sign bit of the NaN result value is
nondeterministic", and payloads are constrained only to be a subset of input
payloads; (e) relaxed SIMD; (f) resource exhaustion — `memory.grow`,
`table.grow`, stack overflow.

That is the entire list. Integer arithmetic, memory access, control flow, and
plain SIMD (`simd128`) are fully deterministic in the spec.

### 2.2 The deterministic profile (Wasm 3.0)

Wasm 3.0 was finalised 2025-09-17
([announcement](https://webassembly.org/news/2025-09-17-wasm-3.0/)) and
includes the profile mechanism. Appendix "Profiles"
([spec](https://webassembly.github.io/spec/core/appendix/profiles.html)),
profile marker `DET`, excludes rules annotated `[!DET]`, which gives exactly
two changes: "All NaN values generated by floating-point instructions are
canonical and positive" and "All relaxed vector instructions have a fixed
behaviour that does not depend on the implementation." Explicitly *not*
covered: "memory.grow and table.grow operations retain non-deterministic
behavior ... to signal resource exhaustion in implementation-dependent ways."

So the profile fixes the *value* non-determinism; it says nothing about
threads (a decoder simply must not import/instantiate shared memory), host
imports (a decoder must provide only pure imports), or resource limits (must be
fixed by the embedder). For a codec module the practical recipe is: no
imports at all except a fixed memory or a pure `abort`, no `shared` memory,
`memory.grow` either forbidden or declared with `max` and pre-reserved, and the
DET profile for floats — or simply *ban floats* in the module validator, which
removes both NaN and relaxed-SIMD questions and is what most blockchains did
first.

### 2.3 How blockchains got determinism, metering and memory limits

| System | Value determinism | Time limit | Memory/stack | Notes |
|---|---|---|---|---|
| CosmWasm (Wasmer) | <1.5: reject any float opcode; ≥1.5: allow floats with Wasmer NaN canonicalisation (`allow_floats` config) | gas via Wasmer middleware, injected per basic block | static validation of imports/exports/memory | [CWIP #2](https://github.com/CosmWasm/CWIPs/issues/2), [1.5 release](https://medium.com/cosmwasm/cosmwasm-1-5-946fd3024f1d) |
| NEAR | floats allowed with NaN canonicalisation | gas: wasm→wasm instrumentation adding a mutable global "gas allowance", deducted per basic block ([nearcore #4410](https://github.com/near/nearcore/issues/4410), [PR #4688](https://github.com/near/nearcore/pull/4688)); [finite-wasm](https://github.com/near/finite-wasm) does the static analysis for instruction count and *stack height* "in a runtime-agnostic way" | explicit stack-height metering | engine-independent on purpose |
| Polkadot PVF (Wasmtime) | Cranelift; artifacts non-deterministic byte-wise but same results ([polkadot #1269](https://github.com/paritytech/polkadot/issues/1269)) | timeouts | *stack overflow points differ across Wasmtime versions/platforms* because of register allocation and spill slots — the motivating problem for a "deterministic PVF executor" ([forum](https://forum.polkadot.network/t/deterministic-pvf-executor/4204)) | led to PolkaVM, §3.2 |
| Arbitrum Stylus | floats via softfloat / restricted opcodes ("SIMD or other features" disallowed) | "ink" = gas/10,000, instrumented at activation | stack-depth check "for deterministic overflow behavior"; memory charged per 64 KiB page, 8 MB default (128 pages) | 24 KB brotli-compressed / 128 KB (256 KB at ArbOS 60) uncompressed program limit ([VALID_WASM](https://github.com/OffchainLabs/cargo-stylus/blob/main/main/VALID_WASM.md), [gas](https://docs.arbitrum.io/stylus/concepts/gas-metering)) |
| eWASM (Ethereum, 2016-2020) | floats banned | metering "insert metering instructions per branch after verification" as an optional layer ([rationale](https://ewasm.readthedocs.io/en/mkdocs/rationale/)) | — | abandoned; the reasons in the sources are political/roadmap (rollups) more than technical — I did not find a primary post-mortem |

Two lessons recur. First, everyone who cares about cross-engine determinism
does metering as a *wasm→wasm transform* (a global counter decremented per
basic block) rather than trusting an engine's built-in fuel, because then any
spec-compliant engine traps at the same instruction. Second, *stack depth* is
the nastiest residual: a native-compiled engine overflows the machine stack at
an engine/platform-dependent point unless the module is instrumented with a
logical depth counter (NEAR/finite-wasm, Stylus) or the engine is designed for
it (Polkadot's stack-machine executor; PolkaVM).

Wasmtime's own guidance
([Deterministic Wasm Execution](https://docs.wasmtime.dev/examples-deterministic-wasm-execution.html))
matches: enable `cranelift_nan_canonicalization`, enable
`relaxed_simd_deterministic` or disable `wasm_relaxed_simd`, limit
growth with a `ResourceLimiter`, disable threads, and use fuel rather than
epochs for interruption because fuel is deterministic.

### 2.4 Runtimes

Wasmtime ([Config docs](https://docs.wasmtime.dev/api/wasmtime/struct.Config.html)):
`consume_fuel` (default off; "Most WebAssembly instructions consume 1 unit of
fuel"; deterministic; "a Store starts with no fuel"), `epoch_interruption`
(cheap, non-deterministic, ~10 % in one cited example, incompatible with Winch),
`cranelift_nan_canonicalization` (off), `relaxed_simd_deterministic` (off),
`wasm_relaxed_simd` (on), `wasm_threads` (on when built with the feature),
`wasm_memory64` (off, "hasn't been exercised much"), `wasm_bulk_memory` (on),
`wasm_simd` (on; SSE2 baseline for Cranelift). Fuel overhead is described as
"rather significant" and a "slacked" fuel design was proposed
([#4109](https://github.com/bytecodealliance/wasmtime/issues/4109)); I found no
published percentage.

wazero ([README](https://github.com/tetratelabs/wazero),
[RATIONALE](https://github.com/wazero/wazero/blob/main/RATIONALE.md)): pure Go,
zero deps, no CGO; Core Spec 1.0 and 2.0 (so `simd128`, bulk memory, multi-value,
sign-ext, non-trapping float-to-int, reference types are in; SIMD supported in
both engines). Compiler on linux/amd64+arm64, macOS arm64, windows/freebsd/... amd64
only; interpreter everywhere (riscv64, netbsd, openbsd). "Compiler ... faster
than Interpreter, often by order of magnitude (10x) or more." No fuel: "the
deadline mechanism is `context.Context` cancellation" via
`WithCloseOnContextDone(true)`, checked at operation boundaries
([hardening article](https://www.systemshardening.com/articles/wasm/wazero-hardening/)),
so wazero cannot give a *deterministic* out-of-time trap unless the module is
pre-instrumented (NEAR style). Memory limit via `WithMemoryLimitPages`. memory64
not implemented ([wazero #2452](https://github.com/wazero/wazero/issues/2452),
opened 2025-11-30); Wasm 3.0 compliance tracked in
[#2426](https://github.com/wazero/wazero/issues/2426). Default fake clock:
"Both fake nanotime and walltime increase by 1ms on each read" — a nice
example of deterministic-by-default host imports.

wasmi ([1.0 post](https://wasmi-labs.github.io/blog/posts/wasmi-v1.0/),
[Config](https://docs.rs/wasmi/latest/wasmi/struct.Config.html)): Rust
interpreter, register-based IR since v0.32 (May 2024,
[post](https://wasmi-labs.github.io/blog/posts/wasmi-v0.32/)); all of Wasm 2.0
plus multi-memory, memory64, wide-arithmetic, custom-page-sizes, opt-in SIMD;
`consume_fuel()` with `TrapCode::OutOfFuel` and resumable calls; `floats()`
toggle to reject float instructions at validation; `no_std`; two runtime
dependencies; audited by Runtime Verification; OSS-Fuzz. Wasmi is the
interpreter that was *designed* for this use ("deterministic execution ...
smart contract execution, embedded devices").

wasm3: C interpreter, fastest classic interpreter; SIMD/bulk-memory/tail-call
only as experimental build flags
([Development.md](https://github.com/wasm3/wasm3/blob/main/docs/Development.md));
no metering that I could find; project in minimal-maintenance mode (maintainer
merges PRs, no new features). Not a base for new work.

wasm2c (WABT): Wasm→C transpile, used by Firefox RLBox since Firefox 95 for
portability across OS/CPU ([Mozilla](https://blog.mozilla.org/attack-and-defense/2021/12/06/webassembly-and-back-again-fine-grained-sandboxing-in-firefox-95/)).
Relevant as the *vendoring* path: a Wasm module can be turned into C (or Go)
and compiled in, so "portable module" and "built-in module" need not be
different artefacts.

### 2.5 Throughput

Frank Denis' libsodium suite, x86-64, slowdown vs native
([2026](https://00f.net/2026/06/23/webassembly-runtimes-2026/),
[2023](https://00f.net/2023/01/04/webassembly-benchmark-2023/)):

| runtime | baseline build | best build |
|---|---|---|
| Wasmer 7.1 (LLVM/Cranelift) | 2.08× | 1.33× (+wide_arithmetic) |
| Wasmtime 46 | 2.41× | 1.46× (+wide_arithmetic) |
| WAMR 2.4.4 AOT | 1.57× | 1.57× |
| WasmEdge 0.17 AOT | 1.74× | 1.74× |
| wazero 1.12 (compiler) | 4.72× | 4.72× ("basically flat: 4.84x, 4.70x, 4.72x ... over two years") |
| Bun (JSC) | 8.77× | — |

Interpreters (CoreMark and micro-benchmarks, different machines, so only
ratios are meaningful):

* wasm3 [Performance.md](https://github.com/wasm3/wasm3/blob/main/docs/Performance.md):
  CoreMark 1628 vs Wasmtime 6454 (4.0× faster) vs native 19145 (11.8×). fib(40)
  16.6× slower than native C.
* wasmi v0.32 CoreMark: 1457 (Epyc 7763), 2979 (i7-14700K), 1577 (M2) —
  same ballpark as wasm3.
* PolkaVM [BENCHMARKS.md](https://github.com/paritytech/polkavm/blob/master/BENCHMARKS.md):
  prime-sieve one-shot, interpreters "ranged from 21.8×–232.67×" vs native;
  wasmtime-cranelift 3.5–4.9× on the NES emulator.

Implied cost of running a byte-crunching predictor in an *interpreter*: 10–20×
native for the best C/Rust interpreters (wasm3, wasmi), and — combining
wazero's own "10× or more" interpreter/compiler gap with its 4.7× compiler —
somewhere around **40–50× native for wazero's interpreter**. I found no direct
MB/s measurements for wazero-interpreter on compression kernels; this is an
extrapolation.

For a concrete anchor: compression codecs in wasm inside browsers (JIT) run at
roughly native/2 — lz4 decompression "cracking 1 GB/s" in Firefox
([nickb.dev](https://nickb.dev/blog/wasm-compression-benchmarks-and-the-cost-of-missing-compression-apis/)),
a Chromium engineer's "native ... 10% faster than WASM" for Facebook's zstd —
and the wide-arithmetic proposal (Wasmtime, wasmi, Wasmer, SpiderMonkey, JSC,
LLVM, Rust support; [overview](https://github.com/WebAssembly/wide-arithmetic/blob/main/proposals/wide-arithmetic/Overview.md))
exists precisely because 128-bit multiply/add in Wasm was "2-7x slower than
native", which bites hash-heavy code.

What this means for a 1 GB input (my arithmetic, not a measurement): a
predictor that costs ~5 ns/byte natively (≈200 MB/s: a pass that parses a
function table and rewrites relocations is in this range) takes ~5 s native,
~12 s in wazero-compiler, ~50–100 s in wasm3/wasmi, and ~4–5 min in wazero's
interpreter. At 50 ns/byte natively (a per-byte modelling loop) multiply all of
those by ten. Interpreting a 1 GB predictor is viable only if the predictor is
O(structure) not O(bytes) — i.e. it emits a *layout* (copy ranges plus
rewrite rules) and the host applies it natively, which is what go-binsync
already does.

### 2.6 Module size

Toolchain-dependent, and the small end is tiny:

* Zig `wasm32-freestanding -O ReleaseSmall`: a Mandelbrot at 232 bytes; "a
  math library of twenty exported functions ... under 600 bytes"
  ([wazero Zig guide](https://wazero.io/languages/zig/), tutorials).
* Rust: a bare `no_std` + `panic=abort` module can be 152 bytes to ~1 KB; "27
  kilobytes when panic handling code is included"; std pulls in ~600 KB; the
  default allocator adds 50–100 KB; typical optimised (`-Oz`, LTO, `wasm-opt
  -Oz`) real modules 50–200 KB ([min-sized-rust](https://github.com/johnthagen/min-sized-rust),
  [rustwasm book](https://rustwasm.github.io/book/reference/code-size.html)).
* Stylus contracts prove the point at scale: whole Rust programs must fit in
  128 KB raw / 24 KB brotli, and "brotli compression ... reduces the footprint
  of common Rust WASMs by over 50%".
* AssemblyScript hello-world ≈ 3.5 KB + 1.2 KB runtime; MoonBit fib 253 bytes
  (vendor claim, [The New Stack](https://thenewstack.io/moonbit-wasm-optimized-language-creates-less-code-than-rust/)).

A predictor module comparable to go-binsync's Go-binary predictor (parse
pclntab, match functions, emit copy/rewrite plan) written in `no_std` Rust or
Zig should land in the 10–60 KB range raw, a few KB to ~20 KB compressed. That
is the same order as one *patch* today, so shipping the module inline is
plausible for large patches but not for the 2 KB case; it should be
content-addressed and cached (§5).

### 2.7 memory64, bulk memory, SIMD, component model

* memory64 is in Wasm 3.0 (phase 4 Nov 2024; Chrome/Firefox shipping 2025),
  Wasmtime supports it since v26 (with a caveat that >4 GB data segments hit
  u32 overflows, [#11816](https://github.com/bytecodealliance/wasmtime/issues/11816)),
  wasmi 1.0 supports it, **wazero does not**. Bounds checks in memory64 also
  cost more than the guard-page trick used for 32-bit memories. For a codec
  substrate, i32 memories with the *host* streaming windows through a bounded
  buffer are the safer bet; a 1 GB input does not need to be mapped whole.
* Bulk memory (`memory.copy/fill`) is universal (Wasm 2.0, `lime1` baseline:
  `bulk-memory-opt`, `call-indirect-overlong`, `extended-const`, `multivalue`,
  `mutable-globals`; Clang `-mcpu=lime1`, Zig default baseline
  [PR 22098](https://github.com/ziglang/zig/pull/22098)). Target `lime1` and
  every runtime in this document runs it.
* `simd128` is deterministic and in Wasm 2.0; wazero and Wasmtime support it,
  wasmi opt-in. Relaxed SIMD must be excluded or DET-profiled.
* Component model / WIT: irrelevant for this use. The interface is "here is a
  byte slice, give me a byte slice"; canonical-ABI marshalling is overhead with
  no benefit, and wazero has no component support. Use a core module with a
  hand-specified ABI (exported `alloc`, `predict`, one memory).

---

## 3. Alternatives to Wasm

### 3.1 eBPF — unsuitable

Kernel verifier ([docs](https://docs.kernel.org/bpf/verifier.html)): 512-byte
stack, no heap (only maps), the verifier's first step is a "DAG check to
disallow loops" (bounded loops exist since 5.3 but must be provably bounded by
the verifier's path exploration; instruction-complexity limit 1 M), no
function-pointer calls beyond tail calls. A predictor over a 30 MB file with a
hash table of functions cannot be expressed. eBPF's virtue — provable
termination — is achievable in Wasm by fuel with none of the expressiveness
cost. Userspace eBPF VMs (ubpf, rbpf) drop the verifier and then are just a
weak register VM with no memory model.

### 3.2 PolkaVM (RISC-V)

[Repo](https://github.com/paritytech/polkavm),
[announcement](https://forum.polkadot.network/t/announcing-polkavm-a-new-risc-v-based-vm-for-smart-contracts-and-possibly-more/3811).
RV32EM (later 64-bit) user-level VM; "no floating-point operations, SIMD, or
full 32-register RISC-V". Goals: determinism, cheap deterministic gas,
single-pass O(n) recompile (167–194 µs vs Wasmtime 62+ ms), sandbox in a
separate process, 128 KB baseline per instance. Benchmarks: NES emulator 1.68×
native (Wasmtime 3.5–4.9×), prime sieve 1.32×. Rationale for leaving Wasm:
Wasm's stack machine needs a heavyweight register allocator to be fast, and
that allocator is what makes stack usage and (indirectly) gas non-deterministic
across engines. Status: "unfinished and is a very heavy work-in-progress",
Rust-only host, linux-amd64/arm64 recompiler, interpreter fallback elsewhere,
still explicitly not for production. Interesting design, unusable dependency
for a Go library today, and no floats/SIMD.

### 3.3 Lua / LuaJIT

Unsuitable as a *portable deterministic* format: standard Lua uses doubles
(integers since 5.3 but `/` and math library are float); "Loading untrusted
bytecode is not safe, as it's trivial to crash the Lua or LuaJIT VM with
maliciously crafted bytecode" ([LuaJIT FAQ](https://luajit.org/faq.html));
LuaJIT "is not protected against infinite loops"; table iteration order varies
between runs. Sandboxing is done with `setfenv` and instruction-count hooks and
"it's very hard to get this right". Lua is a fine *authoring* language for
predictors if compiled to Wasm, but not a wire format.

### 3.4 A custom ZPAQL-style bytecode

Pros: trivially deterministic (see §1.2), spec fits on a page, interpreter is
a few hundred lines in any language, no toolchain dependency, resource needs
declared in the header, no NaN/threads/imports questions ever. Cons: you own
the compiler, the debugger, the optimiser and the assembler; ZPAQL history
shows that a tiny VM stays tiny — nobody writes big programs in it; a
predictor that walks ELF/PE/Mach-O structures wants structs, calls, and a
stack. A middle design — a small *register* VM with i32/i64, a call stack,
bounded loops via a fuel counter, one flat memory — is essentially
"Wasm without floats and without the spec baggage", and at that point the
argument for inventing it rather than restricting Wasm is only that the
interpreter is smaller. wasmi's `floats(false)` gives that in one line.

### 3.5 Wuffs

[README](https://github.com/google/wuffs/blob/main/README.md),
[hermeticity](https://github.com/google/wuffs/blob/main/doc/note/hermeticity.md),
[bounds-checking](https://github.com/google/wuffs/blob/main/doc/note/bounds-checking.md).
Wuffs is a language for exactly our problem class ("Wrangling Untrusted File
Formats Safely"): compile-time proofs against buffer overflow, integer
overflow, and null deref via refinement types (`base.u32[..= 100]`), facts from
prior `if` checks, and interval arithmetic ("for `x[m + n]`, the upper bound
for `m + n` is the sum of the upper bounds"), with explicit wrapping operators
where you mean it. Hermetic: "no mutable global variables, no mutable TLS ...
no FFI ... no user-supplied callbacks, no system calls", no allocation, no
clocks; coroutine-based sans-I/O so a fixed buffer can stream arbitrarily large
inputs. Transpiles to C; the generated C is what ships. Faster than the C
incumbents it replaces (GIF 2–6×, PNG 1.2–2.7×, deflate ≤1.4× vs zlib). In
Chrome since M93 (June 2021).

For our purposes Wuffs answers "how do I write a predictor that cannot crash"
but not "how do I ship it": its output is C, not a portable artefact, and there
is no Go backend. It could be *vendored* (cgo, or compiled to Wasm via clang
and then treated as a Wasm module — an odd but workable pipeline). Its real
lesson is that hermeticity plus static bounds gives safety *without* a
runtime sandbox, which is what you want for the *built-in* fast path.

### 3.6 Kaitai Struct — declarative descriptions

[kaitai.io](https://kaitai.io/), [user guide](https://doc.kaitai.io/user_guide.html),
[v0.11](https://kaitai.io/news/2025/09/07/kaitai-struct-v0.11-released.html).
`.ksy` YAML describes a format: sequences of typed fields, `repeat:
eos|expr|until`, `if:`, `switch-on`, lazy `instances` with `pos:`, an
expression language (arithmetic, bitwise, comparisons, ternary, string/array
methods), `process: xor|rol|zlib|custom`. Compiles to 12 languages including Go.
Serialisation (writing) arrived in v0.11 (Sept 2025) for Java and Python only.
Cannot express "loops with state ... arbitrary computation ... complex control
flow" — no accumulators, no hash tables, no matching. So a `.ksy` can locate
the pclntab and parse the function table, but it cannot compute the layout
prediction. A declarative *parse* description as a sub-module (feeding a
generic matcher) is conceivable but Kaitai's runtime model (parse into a full
object tree) is wrong for 1 GB inputs. Reject as the module format; keep as a
possible authoring aid.

### 3.7 Languages targeting Wasm

Any of Rust (`no_std`), Zig (`wasm32-freestanding`), C/C++ (clang
`-mcpu=lime1`), AssemblyScript, MoonBit, Grain, TinyGo produce core modules;
the *runtime* and *validator* are what enforce determinism, not the source
language. Practical ordering by output size and toolchain stability: Zig ≈ C <
Rust `no_std` < MoonBit/AssemblyScript (small but need their runtime) ≪ TinyGo
(GC, larger; and Go's own `GOARCH=wasm` is unsuitable: hundreds of KB, needs a
JS/WASI host). Since our host is Go, the tempting "write predictors in Go"
path does not lead to small modules; TinyGo is the only realistic Go→small-wasm
route and I did not measure it here.

---

## 4. Precedents: the stream carries its decoder/model

| Precedent | What is carried | Where | Verification | Take-away |
|---|---|---|---|---|
| ZPAQ | full decoder: model tree + two bytecode programs | every block header | SHA-1 of output per segment | proven forward compatibility; header-declared memory |
| Self-extracting archives / executable packers (UPX) | native decoder stub + payload | file | none | maximal portability = none (arch-specific); the anti-pattern |
| xz/7z BCJ filters, ZPAQ E8E9, Courgette/Zucchini | *parameter* only (which arch filter); the transform is built in | filter chain id | — | 0–15 % gain from a tiny fixed predictor; filters are versioned by *name*, decoder must have them |
| Courgette (Chrome) | "a prediction followed by a correction": server sends hint + diff against `original ‖ guess`; client rebuilds guess | patch | — | 10.4 MB full / 704 KB bsdiff / 78.8 KB Courgette — the design go-binsync generalises |
| Shared Brotli ([draft](https://datatracker.ietf.org/doc/html/draft-vandevenne-shared-brotli-format)) | dictionaries (LZ77 prefix or custom static) | referenced, not inlined | 256-bit HighwayHash id "within a trusted, known set of dictionaries" | dictionary-by-hash reference |
| RFC 9842 Compression Dictionary Transport ([rfc](https://datatracker.ietf.org/doc/html/rfc9842)) | dictionary = a previous response | negotiated via `Use-As-Dictionary` / `Available-Dictionary` | **SHA-256 of the dictionary embedded in the stream**: `dcb` = 4 magic bytes + 32-byte hash; `dcz` = 8 + 32 | Google Search HTML −23 % avg, ~−50 % best, LCP +1.7 % ([Chrome blog](https://developer.chrome.com/blog/search-compression-dictionaries)) |
| JPEG XL modular mode ([arXiv 2506.05987](https://arxiv.org/html/2506.05987v1)) | the *context model itself*: a meta-adaptive decision tree over local properties selecting predictor (Zero/Left/Top/Average/Select/Gradient/Weighted) and histogram; ANS/prefix histograms with clustering | in-stream (local or global MA tree) | — | a *bounded, declarative* program-as-model: trees, not bytecode; "jxl art" is people hand-writing them |
| NNCP ([paper](https://bellard.org/nncp/nncp_v2.1.pdf), [site](https://bellard.org/nncp/)) | nothing — model is *trained online* identically on both sides | — | "guaranteed only if the code is running with the exact same hardware and software versions" (PyTorch deterministic mode, fp16); 1–3 kB/s on an RTX 3090; enwik9 0.853 bpb | floating-point learning is not portable; the whole class is out |
| cmix ([repo](https://github.com/byronknoll/cmix)) | nothing — LSTM+paq8 models built in | — | float; 32 GB RAM recommended; ~1.7 kB/s | same |
| KoLMogorov Test ([arXiv 2503.13992](https://arxiv.org/abs/2503.13992)) | a generated *program* that reproduces the data | — | frontier LLMs "struggle" | "compression as a program" is the theoretical endpoint; practically a research benchmark |

Two patterns matter for us. (1) The successful shipping systems (RFC 9842,
Shared Brotli) *reference* the big shared thing by cryptographic hash and
inline only a fixed-size id; they do not inline the dictionary. Modules should
work the same way: `module_id = SHA-256(module bytes)` in the patch header,
module fetched/cached out of band, optionally inlined when the receiver
signals it lacks it. (2) JPEG XL shows that a *restricted, non-Turing-complete*
description (a decision tree with a fixed predictor menu) can carry most of
the adaptivity while staying trivially deterministic and bounded. For
go-binsync the analogue is the layout table: the *plan* is data, the
*interpreter of the plan* is fixed code.

---

## 5. Security, resource limits, verification

### 5.1 Threat model for an untrusted predictor module

A module is untrusted code that reads the old file and a small parameter blob
and emits a prediction. Failure modes: (a) memory unsafety in the host — only
if the sandbox leaks; (b) non-termination or superlinear time; (c) unbounded
memory; (d) *silent divergence* — encoder's and decoder's predictions differ,
so the correction produces garbage; (e) a module that behaves differently on
different hosts (the non-determinism list, §2.1); (f) supply-chain: the module
bytes are not what the encoder ran.

(a)–(c) are the sandbox's job: Wasm linear memory with a declared `max`, fuel
(Wasmtime `consume_fuel`, wasmi `consume_fuel`) or wasm→wasm instrumentation
(NEAR) that is engine-independent; and, on wazero, either instrumentation or a
context deadline (non-deterministic but adequate when the decoder's only goal
is not to hang). A `ResourceLimiter`/`WithMemoryLimitPages` cap at a size
declared *in the patch header* (ZPAQ `findBlock(&mem)` style) lets the decoder
refuse before instantiating.

(e) is the validator's job, at *module admission* not at run time: reject
modules with any import other than the fixed ABI; reject `shared` memories,
atomics, relaxed-SIMD; either reject float opcodes outright (CosmWasm <1.5,
wasmi `floats(false)`) or require DET-profile engines; require `memory.grow`
absent or bounded. This is a few hundred lines with `wasmparser` in Rust; in
Go, wazero exposes no public validator hook, so you write a small pass over
the binary (it is a simple format) or accept a Rust-side `wasm-tools` check at
*publish* time and trust the hash at *decode* time.

(f) is solved by content addressing: the patch carries `module_hash` and the
decoder either has that exact blob or fetches it; the module bytes are never
trusted by name or version.

### 5.2 Making divergence a named failure

Divergence (d) is the one that ZPAQ leaves implicit (a wrong `pcomp` yields
wrong bytes, caught only by the segment SHA-1 at the end) and that NNCP simply
cannot detect. The fix is cheap and should be mandatory:

1. The encoder records `H_pred = hash(prediction)` in the patch header,
   alongside `H_old` (input), `H_module` (predictor bytes), the parameter blob,
   the declared fuel and memory budgets, and `H_new` (final output).
2. The decoder checks `H_old` before running, runs the module under the
   declared budgets, hashes the prediction and compares with `H_pred` *before*
   applying the correction. A mismatch is a distinct error —
   `ErrPredictionDiverged{module, host_runtime_id}` — and is the signal that
   either the module is non-deterministic, the runtime is non-conformant, or
   the module bytes differ. Because `H_module` already matched, the first two
   are the only remaining explanations, which is exactly what an operator needs
   to know.
3. After correction, check `H_new` (already done today).

For a 1 GB prediction, hashing is ~0.5–2 s with BLAKE3/SHA-256 in Go — it is
not free but it is smaller than the prediction itself. Use a chunked or
tree hash (BLAKE3-style, or per-64 KiB SHA-256 leaves) so a divergence report
can say *where* the prediction first differs, which turns "the module is
non-deterministic somewhere" into a reproducible bug. It also allows the
decoder to stream: verify chunk i of the prediction, apply chunk i of the
correction.

Additionally record `runtime_id` (engine name + version + engine mode) in
diagnostics, and run a *self-test vector* per module at publish time on at
least two engines (e.g. wasmtime and wazero-interpreter). Cross-engine
agreement on a test corpus is the only practical evidence that the module sits
inside the deterministic subset; a validator catches the known sources, not
subtle ones like reading uninitialised memory — which in Wasm is defined (zero)
but in a source language may still be UB that the compiler exploited.

### 5.3 Fuel budgets as part of the format

Declare `max_fuel` per module invocation in the patch; the encoder measures
what it used and writes that with a margin. Cross-engine fuel counts are only
comparable if the *module* does the counting (NEAR instrumentation), so if
fuel is meant to be enforceable deterministically, instrument at publish time
and treat engine fuel as a backstop. If the budget is only a DoS guard,
engine fuel or a wall-clock deadline is fine and simpler.

---

## 6. What this means for our design

### 6.1 The choice is really three tiers, not one format

The prior art sorts into three roles that a single "module format" tries to
collapse:

1. **Built-in fast path** — the predictor compiled into the decoder (Go code
   today). Wuffs-style discipline (hermetic, bounds-proved, sans-I/O) is the
   right *design* for this code even if we do not use Wuffs; the Go predictor
   should already be a pure function `(old, params) -> plan` with no I/O.
2. **Portable module** — the same predictor as a shippable artefact for
   decoders that lack it. This is where Wasm belongs.
3. **Plan/data** — the thing the predictor emits. JPEG XL's MA trees and
   go-binsync's layout table are both "declarative programs": bounded,
   interpretable by fixed code, trivially deterministic. Keep this layer fat
   and the executable layer thin.

### 6.2 Restricted Wasm profile: yes, but narrower than "the DET profile"

A restricted Wasm profile is the right choice for tier 2, for reasons the
alternatives fail: ZPAQL-style VM cannot host a real parser; eBPF cannot loop
or allocate; Lua is unsafe as a wire format; PolkaVM is Rust-only and
unfinished; Kaitai cannot compute. Wasm is the only option with (a) a spec with
a named deterministic subset, (b) mature toolchains from C/Rust/Zig, (c) a
pure-Go runtime (wazero) *and* fast Rust runtimes, (d) fuel/limits in at least
two engines, (e) a decade of blockchain practice on exactly the
determinism+metering problem.

But the profile we need is tighter than Wasm's DET profile:

* Target `lime1` + `simd128` (opt-in, deterministic) — *no* floats at all.
  Predictors are integer code; banning `f32/f64` removes NaN canonicalisation
  as a runtime requirement, which matters because wazero has no canonicalisation
  switch (I found none) and would otherwise be out of the deterministic set.
* No imports except a single fixed memory (or a host-provided read-only
  window API) and an `abort`. No WASI. No `shared`, no atomics, no
  relaxed-SIMD, no memory64 (wazero lacks it, and predictors do not need a >4 GB
  address space if the host streams the input).
* `memory` declared with `min == max` or `max` ≤ header-declared budget.
* Fuel: instrument at publish time (NEAR/finite-wasm pattern) if
  cross-engine-deterministic termination is a goal; otherwise engine fuel /
  context deadline.
* Admission validator enforces all of the above on the raw binary before
  instantiation; the hash of the admitted binary is the module id.

### 6.3 Throughput is the real constraint, and it dictates the ABI

Running per-byte work in a Wasm interpreter on a 1 GB input is not acceptable:
~40–50× native in wazero-interpreter means minutes for something Go does in
seconds (§2.5, extrapolated). Even wazero's compiler at ~4.7× native turns a
5 s pass into ~25 s. Two consequences:

* The module ABI must be **plan-emitting**, not **byte-emitting**: the module
  parses structure (pclntab, section headers, symbol tables — O(#functions),
  not O(bytes)) and returns a compact plan (copy ranges, per-reference rewrite
  rules); the host materialises the prediction natively with
  `memory.copy`-class throughput and applies the correction. This is what
  go-binsync's layout table already is, so the generalisation is "make the
  layout-table interpreter the fixed core and make the thing that *produces*
  the table pluggable". If a plan language cannot express some predictor, the
  fix is to extend the plan language (a new opcode in the fixed core, with a
  version bump), not to let modules write bytes.
* Inputs should be exposed to the module through a bounded window (host
  callback `read(offset, len) -> ptr` into a 32-bit memory), not by mapping the
  whole old file, so a 1 GB file does not require memory64 or a 1 GB linear
  memory.

With that ABI, module execution is dominated by hashing/matching over
symbol tables — tens of MB of metadata at worst — and a 10–50× interpreter
penalty costs seconds, not minutes. Measure this before committing: port the
existing Go predictor's structure-walk to Rust/Zig→wasm, run under
wazero-interpreter, wazero-compiler, and wasmtime on the 30 MB and ~300 MB
binaries, and report ns/function and total seconds. That single benchmark
decides whether the interpreter fallback is a real product or a
correctness-only path.

### 6.4 Against a ZPAQL-style tiny VM

Tempting because the determinism proof is a page and the interpreter is an
afternoon. Rejected because: (1) no one will write an ELF/Mach-O/PE walker in
it, and the ZPAQ history shows the ecosystem collapses to "config strings";
(2) we would own a compiler and a debugger forever; (3) a tiny integer VM with
a call stack and fuel *is* a Wasm subset, so restricting Wasm buys the same
guarantees plus toolchains. The one thing worth stealing from ZPAQ is not the
VM but the *header discipline*: declared memory (`findBlock(&mem)`), level
byte with "level L reads all ≤ L", private-range escape codes, per-segment
output hash, and a reference decoder small enough to be the specification.

### 6.5 Against Wuffs-only vendoring

Vendoring compiled predictors (Wuffs→C or Rust→cgo) gives native speed and
static safety but abandons the requirement that a decoder can accept a
predictor it was not built with. It is the right tier-1 strategy and a
reasonable *publish-time* pipeline (Wuffs or Rust source → C for the built-in
path *and* → Wasm for the portable path, from one source), but not a module
format. If one source produces both, the Wasm build becomes the reference
semantics and the native build must match its prediction hash — which the
§5.2 mechanism verifies for free on every patch.

### 6.6 Runtime choice in a Go decoder

wazero is the only pure-Go option and is adequate for the plan-emitting ABI:
compiler on the mainstream linux/darwin targets, interpreter elsewhere, memory
caps, context deadlines, fake clocks by default, SIMD. Its gaps — no fuel, no
NaN canonicalisation, no memory64, ~4.7× native — are all either irrelevant
under the profile above or handled by publish-time instrumentation. Offer
wasmtime-go as an optional cgo backend for encoders/servers where speed
matters. Do not adopt wasm3 (unmaintained) or PolkaVM (not production, no Go).

### 6.7 Open questions I could not settle from sources

* Actual wazero-interpreter throughput on parsing-style code (hash tables,
  branches) — no published numbers; must be measured.
* Fuel overhead percentage in Wasmtime — described as "rather significant",
  no figure found; epoch ≈ 10 % from one example.
* Whether a Go→Wasm authoring path (TinyGo) yields acceptable module sizes and
  passes the no-float/no-import validator; not measured here.
* eWASM's technical post-mortem: sources describe restrictions (no floats,
  per-branch metering) but not a primary account of why it was dropped.
