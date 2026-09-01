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
| one-line change, 30 MB Go binary | **1,129** | 173,060 | 150,475 | 1,390,889 | 538,493 |
| prometheus 3.13.1 → 3.13.2, Go, 94 MB | **69,933** | 3,031,380 | 2,691,644 | 11,068,506 | 8,479,550 |
| Chrome 151.0.7922.169 → .173, C++, 291 MB | **2,256,358** | 5,263,732 | 18,599,806 | 40,102,887 | 45,538,524 |
| Firefox libxul 154.0 → 154.0.1, C++, 186 MB | **2,665,810** | 9,544,652 | 12,348,560 | 24,510,737 | 26,326,367 |

The one-line change is the extreme case and the clearest picture of what
prediction buys: 133× smaller than bsdiff, because almost the whole patch is
displacement that presage recomputes rather than sends. The ratio falls as the
pair grows further apart, to 38× on a Go patch release, 8× on the Chrome
pair and 4.6× on libxul. That is expected: prediction removes displacement,
and genuinely new code still has to be sent. Applying the Chrome patch takes
3.6 s on a 24-core desktop — faster than Zucchini applies its own 2.3×
larger patch on the same machine.

On libxul the more honest denominator is what Mozilla actually ships, an
mbsdiff patch of 10,779,184 whose blocks the MAR container compresses with XZ;
presage is 4× smaller than that.

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
  runs leave wrong, and shares the Go module's DWARF layer.

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

Decoder memory remains the open weakness, but two profiling passes cut the
measured Chrome CLI peak from 3.1 GB to 934 MB (3.2× the target) while making
apply 14% faster. The CLI now derives a soft Go heap limit from the target;
`GOMEMLIMIT` overrides it. [`decode-memory.md`](docs/general/research/decode-memory.md)
profiles the working set, the retained changes, and the path toward
destination-backed materialisation.

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
