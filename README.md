# presage

A binary patcher that predicts the new file instead of searching for it.

Two builds of the same program differ far less than their bytes do. A few
functions change; the linker then moves everything after them and rewrites
every reference that crosses the move. A byte-level differ has to encode all
of that displacement. presage reconstructs it instead: a *structure module*
reads the metadata the toolchain already left in the old file, predicts where
each piece landed in the new one, and the patch carries only a compact plan
plus the bytes the prediction got wrong.

The prediction is deterministic and the result is hash-verified, so a bad
prediction costs patch bytes and nothing else.

## What it costs

Go binaries, via the `go` module. Every row is encode → patch → decode →
byte-exact compare, in bytes:

| pair | presage | Zucchini | bsdiff |
|---|---:|---:|---:|
| prometheus 3.13.1 → 3.13.2, 94 MB (patch release) | **71,192** | 3,031,380 | 2,691,644 |
| prometheus 3.13.2 → 3.14.0 (minor release) | **1,274,324** | 6,143,736 | 6,130,860 |
| synthetic one-line change, 30 MB | **1,828** | 173,060 | 150,475 |

A patch release costs 38× less than the strongest general-purpose differ. A
minor release, where thousands of functions are genuinely new, converges
toward the cost of the new code — which is the expected result: prediction
removes the layout shift, and new content still has to be sent.

The approach is not Go-specific. On a whole-image C++ ELF pair — Chrome
151.0.7922.169 → .173, a 291 MB Linux x86-64 image — a predict-then-correct
codec costs **2,634,264 XZ bytes against a RELA-aware Zucchini's 5,263,732,
50.05% smaller**, replaying byte-exactly. The prediction there is 99.377%
byte-correct, and nearly all of the gain since the first working version came
from *encoding* the same information differently rather than from predicting
better. That result lives in the `bench/elfpredict` harness and is not yet a
shipped module; see `docs/general/research/chrome-elf-handoff.md`.

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

Each module emits a **plan** — a small description of the change — and the
core materialises the prediction natively from it, then codes the residual.
The encoder scores candidate models per region by measured size and keeps the
cheapest, so adding a module can only help. Because the predicted layout is
exact, the encoder never builds a whole-file suffix array, the step that makes
bsdiff need 9–12× the input in RAM.

## Status

Milestone 1 of `docs/general/SPEC.md` is built: the container, the residual
coder, the four modules above, and end-to-end verification. The rest of the
SPEC — the portable wasm module profile, cross-file priors, further domains —
is design.

The open weakness is decoder memory: it peaks at several times the size of
the binary, most of it the prediction's working set.

## Use

```
go install github.com/wjordan/presage/cmd/presage@latest

presage diff old new -o patch      # -v reports where the patch bytes went
presage patch old patch -o new
```

As a library:

```go
patch, err := presage.Encode([][]byte{old}, target, presage.Options{
    Registry: gomod.Registry(),
})
err = presage.Apply([][]byte{old}, patch, gomod.Registry(), w)
```

`Options.Modules` restricts the encoder to named modules, which is how the
benchmarks isolate one model's contribution.

## Documents

| | |
|---|---|
| [`docs/general/SPEC.md`](docs/general/SPEC.md) | the design: core, modules, plan language, residual coding, selection, verification, ranked domains |
| [`docs/general/presage-core.md`](docs/general/presage-core.md) | implementation spec for what is built |
| [`docs/general/go-module-results.md`](docs/general/go-module-results.md) | the Go module measured against Zucchini, bsdiff and the prior Go-aware codec |
| [`docs/general/research/`](docs/general/research/) | the measurements the design rests on, including the Chrome ELF spike |
| [`docs/research/`](docs/research/) | earlier research on Go binary layout and delta encoding |
