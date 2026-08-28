# Probe: cost of running a predictor kernel under a wasm runtime

Measured 2026-08-27 on an AMD Ryzen 9 7900X, Go 1.27.0, wazero v1.12.0
(pure-Go runtime; no cgo). One run each; numbers are indicative, not a
benchmark suite. Source in the session scratchpad (`wasmbench/`); the kernel
is 50 lines and reproduced below in outline.

## Kernel

A predictor-shaped inner loop: fill a buffer with xorshift bytes, then for
every 4-byte-aligned little-endian u32 look it up in a 4,096-run
piecewise-constant shift map (binary search) and add the run's shift — the
operand-relocation step of binsync's `.text` predictor — then FNV-hash the
result. Integer-only, no allocation inside the timed region. Built once
natively and once with `GOOS=wasip1 GOARCH=wasm` (Go's own wasm backend, which
is not a tight code generator; a Rust/C/Zig kernel would be closer to native).

## Results (16 MiB input)

| execution | throughput | vs native | output hash |
|---|---:|---:|---|
| native amd64 | 98 MB/s | 1× | `35a69e918da39cb1` |
| wazero compiler (AOT to amd64) | 37 MB/s | 2.6× slower | `35a69e918da39cb1` |
| wazero interpreter | 1 MB/s | ~100× slower | `35a69e918da39cb1` |

Module compile time under the compiler backend: 0.45 s for a 2.5 MB Go-built
`.wasm` (a Rust/C module for one format would be tens of KB and compile in
milliseconds; the size here is the Go runtime). The 64 MiB run under the
compiler completed in 1.9 s total including fill.

Two incidental observations:

- With wazero's default `ModuleConfig`, `time.Now()` inside the guest does not
  advance: the runtime substitutes a fake, deterministic clock unless
  `WithSysNanotime()`/`WithSysWalltime()` are set. Randomness likewise
  (`WithRandSource`). Determinism is the runtime's *default* posture, which is
  the posture a predictor module wants.
- `WithMemoryLimitPages(4096)` capped the guest at 256 MiB with no other
  change; a guest that grows past it gets a failed `memory.grow`, not the
  host's memory.

## What this settles

- A JIT/AOT wasm runtime costs a small constant factor (2–3× here, with a
  poor code generator on the guest side). On binsync's numbers — 94 MB
  encoded in 2.4 s, decoded in 0.9 s natively, of which x86 decoding is ~40 % —
  a wasm-hosted predictor would put the decoder around 2–3 s. Acceptable for a
  1 GB input on a CI box (≈ 30 s) and marginal but workable on a target.
- A pure interpreter is two orders of magnitude off and is not an option for
  the byte-crunching part of a predictor. Any design that says "the decoder
  just interprets the module" is wrong at GB scale; the runtime must compile,
  or the module must be small relative to the data it steers (e.g. it emits a
  *program* of copy/relocate operations that a native core executes — the
  Courgette "adjustment" shape).
- The determinism story is real in practice: identical hashes across native,
  AOT and interpreted execution for integer code with no imports beyond
  stdout/args. Floating point was not exercised; the deterministic-profile
  rules (NaN canonicalisation, no relaxed SIMD) are what would need
  enforcement there.

Not measured: wasmtime (Cranelift) which is typically faster than wazero's
compiler; SIMD; memory64; the cost of host↔guest copies for a streaming
interface (the kernel owned its buffer). Those are the next probes if the
design goes this way.
