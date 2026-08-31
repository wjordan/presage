# presage core, milestone 1: implementation spec

Status: resolved design for `SPEC.md` §10 milestone 1 ("core extraction").
This document is the authority for the `presage` package; `SPEC.md` is the
authority for what the core must grow into. Where this narrows the SPEC,
the narrowing is named.

## 1. Goal and exit

`presage` is the codec: it turns reference objects plus a target into a
patch and back, with modules supplying predictions and the core doing all
O(bytes) work. Everything above it — where a patch is stored, how it reaches
a target, what is done with the result — is a caller's problem.

Exit for this milestone, tested in `presage/corpus_test.go` against the
`BINSYNC_CORPUS` pairs: every pair round-trips; the self-prediction gate
(`SPEC.md` §4.3) is green for the Go module; every pair's patch is within
2 % of `delta.Encode`'s on the same pair.

## 2. Package layout and dependency direction

```
presage/            core: container, regions, plans, residual, verification
presage/gomod/      the Go linux/amd64 module, a façade over delta's predictor
cmd/presage/        diff / patch
```

`presage` imports `delta` only for byte engines that already exist there —
positional correction (`delta.EncodeCorrection`/`ApplyCorrection`), the
shifted delta (`delta.PlainDiff`/`PlainPatch`), the compressed-debug
transform (`delta.ExpandDebug`/`PackDebug`) — and `internal/cz` for the
terminal stage. The engines move into `presage` when `delta` becomes a
module only; the import is the seam, not the design. `presage/gomod`
imports `delta` for `GoAnalyse`/`GoPredict`, the two exported entry points
the Go transform is refactored into. Nothing in `delta` imports `presage`.

## 3. Container

```
magic "PSG1"  u8 version (1)  u8 flags (bit 0: debugz)
header  uvarint len, then:
        uvarint nrefs, per ref: b3[32] size
        b3 target[32], uvarint target_size
        uvarint nregions, per region: uvarint length, u8 module, uvarint plan_len
        uvarint nframes, per frame: uvarint off, len, zlen, u8 codec, b3[32]
        (the b3 omitted when nframes = 1: the frame is the whole body and
        the target hash checks it)
frames  independently decodable, ≤ 8 MiB each, concatenating to the body
body    when debugz: uvarint expanded_target_size
        per region, in order: plan bytes, b3 root of the prediction's chunk
        tree (leaves are 8 MiB chunks; 32 B whatever the size — shipping the
        leaves cost 96 B on a 30 MB file, 8 % of a one-line patch, so a
        divergence is named by region and module, not chunk), residual
        bytes (uvarint len + bytes)
```

Regions tile the target in order and are contiguous — the SPEC's `gap`
field is dropped: a stretch no module claims belongs to a core `lz` region.
Depth is 1; `child` is reserved. `from`/`to` sizes and hashes describe the
files as shipped; the debugz transform is the same as `delta`'s.

Header parsing is the same discipline as `delta/container.go`: every length
is bounded by what remains and by the declared target size before it
becomes an allocation.

## 4. Modules and plans

A module in this milestone is coarse: one op that materialises a whole
region from the references and its plan bytes.

```go
type Module interface {
    ID() byte                                 // wire id, stable
    Name() string
    // Analyse proposes a plan for the target region, or ErrUnsupported.
    Analyse(refs [][]byte, target []byte) (plan []byte, pred []byte, err error)
    // Materialise expands the plan on the decoder. Pure: refs and plan only.
    Materialise(refs [][]byte, plan []byte, length int) ([]byte, error)
    // Exact says the materialisation has the region's length (positional
    // correction) rather than a declared length (shifted delta, SPEC §6.2).
    Exact() bool
}
```

Core modules (ids 0–15 reserved): `0 lz` — plan is empty, the region's
residual is a `PlainDiff` stream against reference 0; not exact. `1 copy` —
plan is `(ref, off)`; exact. `2 go` (`presage/gomod`) — plan is the Go
transform's layout, stage-1a and stage-1b streams; `Materialise` runs
`delta.GoPredict`; exact. `3 eq` (`presage/eq.go` over `presage/eqmatch`,
the matcher of `research/matcher-spike.md`) — plan is the run columns, the
prediction is the runs copied into place; exact. It is *not* in the default
registry: standing alone it is the same fuzzy matching as `lz` without the
literal stream, and measures 2.5–2.8× larger than `lz` on every pair tried
(prometheus stripped 7,218,625 vs 2,604,181; synthetic 420,341 vs 169,914;
libxul 154.0 → 154.0.1 16,403,799 vs 10,594,355); as a declared region with
an `lz` residual it only ties `lz` at 3× the time. Its runs are the base
under a module that models part of a file — the layered DWARF plan — which
is where the matcher's measured gain came from. `4 elf` (`presage/elfmod`,
`elf-module.md`) — any other ELF x86-64 image: plan is the function map,
reference points, equivalence runs, per-function choices and the
regenerator plans (`.rela.dyn`, `.eh_frame`, jump tables, field fix, DWARF);
symbols are an encoder-only input (`Module.Symbols`, `presage/symbols`);
exact. Registered after `go`, so a Go binary `go` declines falls to it. Ids
above 15 are for admitted modules (SPEC §4.1), unused yet.

Residual: exact regions use the positional correction (`SPEC.md` §6.1: the
adaptive shapes and the modal correction), or, where the module implements
`presage.Cutter`, the split residual — one piece per cut, each in the
smaller of the adaptive shapes or the columnar form, flagged
`FlagSplitResidual` (`presage/split.go`, `delta/columnar.go`); declared
regions use the `lz` stream, and for `lz` regions the residual *is* the
plan.

Selection (SPEC §6.4) in this milestone is tier 1 only: the Go module claims
the whole file when both binaries parse, else `lz` does. Lowering (§5.4) is
an option — `Options.Modules` names the module ids the deployed decoder has;
a region whose module is absent is coded as `lz`.

## 5. Encode and Apply

Encode(refs, target):
1. If any reference or the target has compressed debug sections and the
   target rebuilds exactly from its expansion, set debugz and code the
   expanded objects (`delta.expandPair`, generalised to one reference — the
   only reference the modules use today).
2. Regions: one, module by selection above.
3. Per region: `Analyse` → plan, prediction; residual against the target
   slice; the prediction hash.
4. Body, frames, header.

Apply(refs, patch):
1. Parse header; check every reference's size and hash.
2. Expand debug sections of the references if debugz.
3. Per region: `Materialise`; check the prediction hash and name the region
   on mismatch (`ErrPredictionDiverged`); apply the residual; append.
4. Pack debug sections if debugz; check target size and hash; write.

The decoder never allocates beyond the declared target size plus one
region's prediction; the references are read-only.

## 6. What this milestone does not do

Fine-grained plan ops (`map`, `relocate`, `table`) — the Go module is one
op, which is SPEC open question 2 answered "coarse first". Multi-region
selection with measurement. Nested regions. Wasm tier. The DWARF module —
it is the first thing to build on this core (harness layer (b),
`bench/elfpredict/dwarf.go`), and it is what forces multi-region and the
input DAG (its `.text` map comes from the Go region), so those come with it.

## 7. Milestone 2: the layered Go region

The unstripped Go binary is the case M1 leaves at 8.7 MB on prometheus
(`go-module-results.md`): the transform predicts `.text`, the tables and
the mapped data sections, and copies every other section positionally —
the debug sections, `.symtab`, `.strtab` — so an inserted byte in
`.debug_info` costs the rest of the section. The harness's layered plan
(eq → Go tables → DWARF fields) has that pair at 323,744.

M2 ports the layering *inside the `go` region*: one region, one module id,
a plan with three parts. The container's region DAG (SPEC §5.2) stays
deferred — nothing else needs an input edge yet, and a plan-internal order
is verified by the same region hash. The three parts, in the order the
decoder runs them, each writing only what it owns:

1. **Go prediction** (`delta.GoExpand`): the M1 op. Besides the predicted
   file it exports what the next parts need — which sections the transform
   *modelled* and which it merely copied, the old→new address map, and the
   size change of every matched function.
2. **Equivalence runs** (`presage/eqmatch`), per unmodelled section: the
   old section against the new one, runs in section-relative offsets, laid
   over the prediction. This is where `eq` earns its keep (§4): under a
   layer that then projects the fields the runs stopped at.
3. **DWARF fields** (`presage/dwarf`, the harness layer moved): for every
   reference field of the old `.debug_info`, `.debug_addr`, `.debug_frame`,
   `.debug_line` and `.symtab`, project its position and its value into the
   new image and write it. Positions project through the run that copied
   them (or a record table where a section has one — `.debug_info`'s
   units always, and the fixed-row tables `.symtab`, `.strtab`,
   `.debug_addr`, `.debug_frame`, which byte matching aligns by chance);
   values through the nearest run; addresses through the Go map.

Plan: `[go plan][uvarint len][dwarf plan][uvarint len][runs]`, the last two
absent (length 0) when the file has no `.debug_info`, so a stripped
binary's plan is M1's. The wire form of the two is `presage/dwarf`'s own
(`Plan.Marshal`, `Plan.MarshalRuns`), which the harness shares.

Exit: the DWARF prometheus pair (`prom-3.13.{1,2}-Dz`, applied to the
shipped compressed file) within 10 % of the harness's 323,744, and the
stripped pair unchanged; corpus gate green.

*Status: built and met.* prometheus DWARF pair 332,414 (from 8,480,958),
synthetic 2,065 (harness 2,652), stripped prometheus 70,195, corpus gate
green. Two things the measurement forced:
the unallocated sections are not in the transform's layout, so the DWARF
plan carries their geometry; and the layer is priced against the bare
prediction where the bare misprediction is under 1 MiB, because
near-identical builds (the corpus) pay more for the record tables than for
the positional copy's correction.
