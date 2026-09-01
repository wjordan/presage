# presage — generalising the Go codec

Design and measurements for the general codec: predictive encoding with
pluggable, portable structure modules, of which the Go predictor
(`docs/go-module-design.md`) was the first. `SPEC.md` is the authoritative
design; `research/` holds what it rests on. Started 2026-08-27.

| document | what it is |
|---|---|
| [`SPEC.md`](SPEC.md) | the design: core / modules / distribution, plan language, residual coding, selection, verification, wasm profile, ranked domains, milestones, decisions |
| [`presage-core.md`](presage-core.md) | **implementation spec for milestone 1**, the `presage` package as built: container, coarse modules, residual, what is deferred |
| [`elf-module.md`](elf-module.md) | **implementation spec for the ELF module** (`presage/elfmod`), as built: decoder data flow, wire format, symbols, matcher, gate; current measurements are in [`baselines.md`](baselines.md) |
| [`go-module-results.md`](go-module-results.md) | measured: the Go module on presage against the Go codec, Zucchini and bsdiff; DWARF builds end to end |
| [`research/binsync-lessons.md`](research/binsync-lessons.md) | the ten measured insights from the Go codec the general design must keep, and what it got wrong |
| [`research/percival-thesis.md`](research/percival-thesis.md) | Percival 2006: the three-orders taxonomy, FFT block alignment, difference-string modes, universal (syndrome) deltas |
| [`research/wasm-throughput-probe.md`](research/wasm-throughput-probe.md) | measured: native 98 MB/s vs wazero AOT 37 MB/s vs interpreter 1 MB/s on a relocation kernel; identical hashes |
| [`research/portable-predictors.md`](research/portable-predictors.md) | ZPAQ/ZPAQL, wasm determinism and runtimes, eBPF/PolkaVM/Wuffs/Kaitai alternatives, verification story |
| [`research/adaptive-predictive-coding.md`](research/adaptive-predictive-coding.md) | predict-then-correct lineage, selection vs mixing theory, Roaring/BtrBlocks/xz/ClickHouse selection in practice, MDL framing, cost model |
| [`research/domain-executables.md`](research/domain-executables.md) | Courgette/Zucchini, what survives stripping in ELF/PE/Mach-O/DEX/wasm/firmware, per-ISA reference forms, linker determinism, ranking |
| [`research/chrome-elf-zucchini.md`](research/chrome-elf-zucchini.md) | measured Chrome 151 ELF release pair: a RELA-aware Zucchini prototype cuts the compressed patch 10.62% and round-trips exactly |
| [`research/chrome-elf-handoff.md`](research/chrome-elf-handoff.md) | **start here for the Chrome ELF work**: state of the spike at 49.11%, what is settled and must not be re-run, the last lead now measured (§14's displacement column, −20,248 = 0.76%, unimplemented), how to run the harness fast |
| [`research/chrome-elf-predictor-spike.md`](research/chrome-elf-predictor-spike.md) | decoder-faithful Chrome experiment: standalone symbol predictor at 7.59 MB, then charged equivalence/structure selection at 4.60 MB XZ for `.text`; fairness limits and oracle bounds on a 10× result |
| [`research/decode-memory.md`](research/decode-memory.md) | decoder allocation/RSS profile and successive reductions; current Firefox apply is 1.57 s / 395,400 KiB, with rejected probes and the path to destination-backed materialisation |
| [`research/domain-containers-packages.md`](research/domain-containers-packages.md) | OCI layers, deltarpm/OSTree/Balena/Mender, the recompression trick (puffin, archive-patcher, preflate), rebuild noise, economics |
| [`research/domain-ai-weights.md`](research/domain-ai-weights.md) | lossless weight compression floors, fine-tune/checkpoint deltas, Xet/safetensors/GGUF, a local bf16 measurement |
| [`research/priors-and-regeneration.md`](research/priors-and-regeneration.md) | thinking note + measurements: a prior is worth its *aligned* overlap (byte-exact reuse is 30× smaller than reloc-modulo); hello-world halves gofmt; **delink**: 66 % of function text across 30 unrelated Go projects is shared modulo relocation (1.8 % byte-exact), 3.4× pool dedup |
| [`research/environment-priors.md`](research/environment-priors.md) | **negative result**: the ambient environment as reference set is worth 1.55× on a cold blob, a perfect pool 2.9–3.2×; rodata is 26 % of the artefact and delink does nothing for it. Keeps: pc-value streams are 85 % byte-exactly reusable. The cold path is a distribution problem, not a codec one |
| [`research/matcher-spike.md`](research/matcher-spike.md) | **measured**: a native Go equivalence matcher (seed index + X-drop extension) replaces the Zucchini stream: prometheus DWARF pair 323,744 vs 650,708 XZ (−50 %) at 3.9 s vs 46 s, synthetic +0.8 %; the gain is a retuned drop cut-off that Zucchini's patch format cannot express, not better anchors |
| [`research/matcher-chrome.md`](research/matcher-chrome.md) | **measured**: the native matcher on Chrome's 225 MB `.text`, 4,823,576 → 2,617,700 in six rungs (canonical-byte masking, expected-source probing, near search, far-run minimum), at parity with Zucchini's stream; no suffix array or global selection needed |
| [`research/openzl.md`](research/openzl.md) | **landscape**: Meta's OpenZL assessed against this design. No codec can read a reference (`nbRegens == nbInputs`), its SDDL format-model VM is encoder-only, and its generic graph is zstd on executable bytes (measured, 5–6 % behind brotli/xz). Corroborates G1/G4, reprices G6, concedes the zero-reference half of rank 6; `sparse_num` is worth a measured −27,108 XZ (1.01%) on the Chrome plan, and the equivalence stream is immune to the whole vocabulary |

The short version of what the research says:

- The win is *regeneration* of second-order structure from a small
  description of the first-order change; matching alone is at the floor.
- Modules must emit *plans*, not bytes: a wasm interpreter is 100× off
  native on byte work, AOT 2.6×. The core materialises natively.
- A restricted wasm profile (no floats, no imports, declared memory + fuel)
  is the right portable format; ZPAQL, eBPF, Lua and Kaitai are not.
- Selection is hard, per structural region, encoder-side, by measured size;
  one shared residual coder with typed sub-streams.
- Domains, in order: Go (done), ELF C/C++/Rust, PE, container layers (the
  Go result "in a container costume", plus an unfilled Go-deflate
  recompression niche), arm64, then weights (1.5–2× ceilings, throughput
  is the game) and the rest.
