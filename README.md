# presage

A structure-aware binary patcher.

Add one line to a Go program and 13% of the bytes in the binary change. A real
patch release changes 82%. Almost none of that is new code: the linker shifted
everything after the edit and rewrote every address that crossed the shift,
all of it computed from rules.

General-purpose differs handle this well. bsdiff, xdelta3 and zstd's
`--patch-from` encode only what changed about the parts that survived, with no
knowledge of the file format, which is why they are the default everywhere.
But a patch may only say: copy these bytes, add these differences, seek
forward. Nothing in that vocabulary names a relocation or a jump target, so
the churn gets spelled out byte by byte.

Chrome's Courgette, and its successor Zucchini, put architecture-specific
structure into the patch itself: their format knows what a reference is, so a
moved target becomes a symbol the decoder resolves. presage generalises that
into a small core plus *structure modules*, each specific to one architecture
and toolchain: x86-64 RIP-relative references, ELF relocations, the tables the
Go linker leaves in a stripped binary, DWARF's compressed debug sections.

A presage patch is therefore built around a *plan* in the toolchain's own
terms, with a byte-level residual alongside it where the plan was wrong. On a
291 MB Chrome image the plan is 99.377% byte-correct, so nearly all the churn
is recomputed rather than sent.

## Benchmarks

Every row is `presage diff`, then `presage patch`, then a byte-exact compare.
`docs/general/baselines.md` records the exact pairs and flags.

| pair | presage | Zucchini | bsdiff | xdelta3 | zstd `--patch-from` |
|---|---:|---:|---:|---:|---:|
| one-line change, 30 MB Go binary | **1,100** | 173,060 | 150,475 | 1,390,889 | 538,493 |
| prometheus 3.13.1 → 3.13.2, Go, rebuilt with go1.27, 94 MB | **74,636** | 3,031,380 | 2,691,644 | 11,068,506 | 8,479,550 |
| prometheus 3.13.1 → 3.13.2 as upstream ships it (go1.26.5), 97 MB | **161,508** | 3,012,208 | 2,714,204 | 11,238,692 | 8,279,163 |
| Chrome 151.0.7922.169 → .173, C++, 291 MB | **2,257,676** | 5,263,732 | 18,599,806 | 40,102,887 | 45,538,524 |
| Firefox libxul 154.0 → 154.0.1, C++, 186 MB | **2,092,567** | 9,544,652 | 12,348,560 | 24,510,737 | 26,326,367 |

The one-line change is the extreme case and the clearest picture of what
prediction buys: 137× smaller than bsdiff, because almost the whole patch is
displacement that presage recomputes rather than sends. The ratio falls as the
pair grows further apart, to 36× on a Go patch release, 8.2× on the Chrome
pair and 5.9× on libxul. That is expected: prediction removes displacement,
and genuinely new code still has to be sent.

The two prometheus rows are the same release pair built two ways: the first
rebuilds both sides with go1.27, the toolchain the Go module was written
against; the second is upstream's own binaries, built with go1.26.5. The
module predicts both, but its layout prediction is less exact one release
back, so the as-shipped row costs 2.2× the patch bytes — 17× smaller than
bsdiff rather than 36×.

On libxul the more honest denominator is what Mozilla actually ships, an
mbsdiff patch of 10,779,184 whose blocks the MAR container compresses with XZ;
presage is 5.0× smaller than that.

Resource cost on the Go and C++ point releases (wall time / peak RSS):

| pair | phase | presage | Zucchini | bsdiff | xdelta3 | zstd `--patch-from` |
|---|---|---:|---:|---:|---:|---:|
| prometheus, 94 MB | encode | **4.3 s** / 948 MiB | 21.3 s / 1.05 GiB | 36.8 s / 806 MiB | 8.5 s / 689 MiB | 21.1 s / **421 MiB** |
| prometheus, 94 MB | apply | 0.75 s / 392 MiB | 0.45 s / 198 MiB | 0.49 s / 187 MiB | 0.38 s / **113 MiB** | **0.13 s** / 183 MiB |
| Firefox libxul, 186 MB | encode | 30.4 s / 1.72 GiB | 70.0 s / 2.15 GiB | 79.5 s / 1.56 GiB | **21.0 s** / 1.26 GiB | 34.2 s / **764 MiB** |
| Firefox libxul, 186 MB | apply | 1.57 s / 386 MiB | 0.97 s / 385 MiB | 1.2 s / 366 MiB | 0.86 s / **201 MiB** | **0.28 s** / 358 MiB |

These are medians of three warm-cache runs on the same 24-core Linux host;
Zucchini includes its external XZ pass. See
[`baselines.md`](docs/general/baselines.md) for methodology and exact values.

## How it works

A patch is a sequence of **regions**, each claimed by one module:

- `copy` — the region is identical to the reference.
- `lz` — no structure model applies; general-purpose compression.
- `eq` — equivalence matching: runs of the reference that recur modulo
  relocation.
- `go` — the Go module: aligns functions between builds *by name* using the
  function table a stripped Go binary still carries, predicts the new layout,
  and models `.gopclntab`, type descriptors and `.rodata`. A DWARF layer
  handles unstripped builds, including compressed debug sections.
- `elf` — the ELF x86-64 module for everything else: aligns functions by
  name from symbols the encoder has (the decoder never sees them), finds
  equivalence runs modulo relocation with the native matcher, regenerates
  `.rela.dyn`, `.eh_frame` and jump tables, repairs the address fields the
  runs leave wrong, and uses the same shared DWARF layer as `go`.

Each module emits a **plan**, a small description of the change, and the
core materialises the prediction natively from it, then codes the residual.
The encoder scores candidate models per region by measured size and keeps the
cheapest, so adding a module can only help. Because the predicted layout is
exact, the encoder never builds a whole-file suffix array, the step that makes
bsdiff need 9–12× the input in RAM.

## Status

Milestone 1 of `docs/general/SPEC.md` is built: the container, the residual
coder, the five modules above, and end-to-end verification. The rest of the
SPEC (the portable wasm module profile, cross-file priors, further domains)
is design.

## Use

```
go install github.com/wjordan/presage/cmd/presage@latest

presage diff old new -o patch      # -v reports where the patch bytes went
presage patch old patch -o new

presage diff old new -o patch -symbols old.debug,new.debug
```

`-symbols` gives the encoder the unstripped builds or Breakpad `.sym` files
for a non-Go binary; the patch does not carry them and the decoder does not
need them.

As a library:

```go
patch, err := presage.Encode([][]byte{old}, target, presage.Options{
    Registry: modules.Registry(oldSyms, newSyms), // symbols optional, encoder-only
})
err = presage.Apply([][]byte{old}, patch, modules.Registry(), w)
```

`Options.Modules` restricts the encoder to named modules, which is how the
benchmarks isolate one model's contribution.

## Documents

| | |
|---|---|
| [`docs/general/SPEC.md`](docs/general/SPEC.md) | the design: core, modules, plan language, residual coding, selection, verification, ranked domains |
| [`docs/general/presage-core.md`](docs/general/presage-core.md) | implementation spec for what is built |
| [`docs/go-module-design.md`](docs/go-module-design.md) | the Go module's design: prediction pipeline, correction format, container, the supported Go release |
| [`docs/general/go-module-results.md`](docs/general/go-module-results.md) | the Go module measured against Zucchini, bsdiff and the prior Go-aware codec |
| [`docs/general/baselines.md`](docs/general/baselines.md) | the pairs, tools and flags behind the table above |
| [`docs/general/research/`](docs/general/research/) | the measurements the design rests on, including the Chrome ELF and Firefox MAR studies |
| [`docs/research/`](docs/research/) | earlier research on Go binary layout and delta encoding |

## License

Copyright 2026 Will Jordan. Licensed under the Apache License, Version 2.0;
see [`LICENSE`](LICENSE).
