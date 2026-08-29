# The Go-table module on presage: results

Measured 2026-08-29 with `bench/elfpredict` (harness) against the Go-aware
codec (`go-binsync diff`, transform 2) and Zucchini. Design: `SPEC.md` §4.4.
All sizes in bytes. "xz" is plan and correction as two `xz -9e` streams,
the harness's convention; "joint" is both as one stream, which is what a
patch would ship; the Go codec's patch is one brotli/zstd frame plus a
~120 B header, so the brotli joint is its like-for-like number.

## Go pairs

| pair | change | presage xz | joint xz | joint brotli | Go codec | Zucchini | bsdiff |
|---|---|---:|---:|---:|---:|---:|---:|
| prometheus 3.13.1 → 3.13.2 | patch release | **72,192** | 71,992 | 72,450 | 74,550 | 3,031,380 | 2,691,644 |
| prometheus 3.13.2 → 3.14.0 | minor release | **1,274,324** | 1,286,896 | 1,290,164 | 1,353,084 | 6,143,736 | 6,130,860 |
| synthetic v1 → v2c (exe) | one line | 1,844 | 1,748 | 1,662 | **1,334** | 173,060 | 150,475 |
| synthetic v1 → v2c (PIE) | one line | 1,828 | 1,728 | 1,706 | **1,277** | 66,420 | 67,564 |

bsdiff is `bsdiff` 4.3 (bzip2 patches). The NOBITS fix below since moved
the presage numbers to 71,192 (patch) and 1,828 (synthetic exe).

Flags per row: prometheus rows `-no-equivalences` (the Go code model is the
`.text` base); synthetic rows keep the equivalences. Best rung is
`modelled-rodata` everywhere; `corrected-fields` (tuned on Chrome) costs
+9 KB on the patch release and +200 KB on the minor. `-no-points` takes the
exe synthetic to 1,780 / 1,680 / 1,548 but costs 2.8 KB on the patch
release, so it is not the default.

Before the module, the generic predictor on the same pairs: 3,573,176
(patch), 5,553,876 (minor), 173,692 (synthetic) — it could only copy
`.gopclntab` and `.go.type`.

### Where the numbers come from

Each step on the synthetic exe pair (xz, plan + correction):

| step | total | what changed |
|---|---:|---|
| generic predictor | 173,692 | `.gopclntab` 2.18 MB and `.go.type` 1.23 MB corrected by hand |
| + Go tables layer | 19,992 | module writes the Go sections; plan is 18.5 KB of function map |
| + derived function map | 1,868 | map taken from the Go layout, structural plan carries none |
| + reloc head, adaptive correction shape, transform 2, derived geometry, no rodata layer | 1,844 | |

On the prometheus minor release the decisive steps were dropping the
equivalence stream (1,731,064 → 1,366,928: the stream was 492 KB and bought
107 KB of `.text`), writing the module at the codec's transform (its `.text`
prediction went from 2,447,518 wrong bytes to the codec's 1,778,100), and
using that prediction as the `.text` base with per-function selection
against the structural one (1,371,368 → 1,274,324).

### The synthetic gap, measured

The one-line pair is where presage's fixed costs show. A first reading
blamed ~880 B of plan streams (equivalences 404, structure 380, choices
100) — a guess. Diffing the predictions section by section against the
real file (`bench/secdiff`) gave the actual residual, in wrong bytes:

| residual | presage, no equivalences | Go codec | cause |
|---|---:|---:|---|
| ELF, program & section headers, `.shstrtab` | 746 | 44 | harness never laid the old headers down without equivalences (bug); codec: geometry fields |
| `.text`, the edited function | 367 | 372 | inserted code exists elsewhere in old `.text`; the per-function aligner cannot see it |
| build IDs, FIPS hash | 132 | 132 | random |
| everything else | 61 | 61 | |

Fixes, all in `delta` so the native codec gets them too (DESIGN.md
§3.2.1 far pieces, §3.2.2 headers): the module predicts holes and headers
(`predictHoles`, `predictHeaders`) in the harness path as the codec does;
headers are recomputed from the layout's section table; transform 3 lets a
resized function's segment map borrow code from anywhere in old `.text`,
scored by simulated relocation.

| pair | Go codec before | + headers | + far pieces | presage module, joint brotli |
|---|---:|---:|---:|---:|
| synthetic exe | 1,334 | 1,275 | **1,184** | 1,314 (1,250 `-no-points`) |
| synthetic PIE | 1,277 | 1,229 | **1,140** | — |
| prometheus patch | 74,550 | 74,500 | 74,126 | **72,450** |
| prometheus minor | 1,353,084 | 1,352,085 | 1,350,486 | **1,290,164** |

Presage no-equivalence synthetic: 2,356 → 1,536 xz (headers) → joint
brotli 1,314 with the transform-3 module. The remaining 130 B against the
codec is the inferred reference points (260 B standalone xz, 64 B net) and
an empty choices stream. Far-piece threshold sweep (gain in priced
correction bytes; try 4 candidates of ≤ 256): 16 / 20 / 24 / 32 gave
synthetic 1,176 / 1,184 / 1,194 / 1,199, patch 74,255 / 74,126 / 74,320 /
74,374, minor 1,356,120 / 1,350,486 / 1,350,696 / 1,349,121; 20 shipped.

## DWARF builds (default `go build`)

Unstripped synthetic pair (`bench/out/bin127/v1-F1 → v2c-F1`, 43 MB):
13 MB of zlib-compressed DWARF (`SHF_COMPRESSED`), `.symtab` 950 KB,
`.strtab` 2 MB. Before the transform: Go codec 9,293,321; Zucchini
9,397,472; presage 9,371,156 — the compressed streams differ entirely.
Go's `compress/zlib` at level 1
(`BestSpeed`) reproduces every section byte for byte (`bench/dwarfprobe`),
on this build and on the prometheus one, so the patch is made on the
plaintext (SPEC §4.5 (a)): `bench/dwarfz plain` expands every
`SHF_COMPRESSED` section in place, keeping every inter-section gap, and
`dwarfz pack` inverts it with the set of compressed sections read from the
old file, so the transform costs 0 plan bytes. Verified on both pairs:
`pack(plain(f)) == f` for every file, and `pack(plain(new), old) == new`,
which is the decoder's path. The same transform is wired into the codec
(`delta/debugz.go`, header flag `debugz`): `go-binsync diff` on the shipped
files gives 1,408,107 on the synthetic pair (from 9,293,321) and 8,714,361
on prometheus, both applied back to the exact shipped file. The codec has
no DWARF field layer, so its debug sections go through the correction as
shifted bytes; the harness numbers below are what layer (b) adds on top.
The plaintext pairs (59 MB / 181 MB):

| plaintext pair (`dwarfz plain`) | bsdiff | Zucchini | presage, eq outside `.text` | presage, no eq |
|---|---:|---:|---:|---:|
| synthetic v1 → v2c, one line | 476,887 | 597,416 | **2,652** | 2,980 |
| prometheus 3.13.1 → 3.13.2, default build | 4,832,993 | 5,622,564 | **650,708** | 2,325,208 |

These are the end-to-end numbers for the files as `go build` ships them,
since the recompression adds nothing; the other tools on the shipped
compressed pair: bsdiff 29,004,245, Zucchini 28,963,240 (prometheus);
Zucchini 9,397,472 (synthetic, above). Earlier rows were
measured on `objcopy --decompress-debug-sections` output (650,040 /
2,325,468; Zucchini 5,596,196; bsdiff 4,837,754), which is not the true
plaintext: objcopy realigns the sections and rewrites `.strtab` (74 KB
smaller), so the shipped file is not recoverable from it.
The stripped prometheus patch is 71,192 (presage) / 74,126
(codec) / 3,031,380 (Zucchini) for comparison, so DWARF costs 9× the
stripped patch here; on the one-line synthetic it costs 800 B.
`-no-text-equivalences` is the mode that wins on the real pair: the Go code
model owns `.text` (the stream there cost more than it bought on the
stripped pairs) and the Zucchini stream owns the debug sections, where it
is a finer map than any table. Its plan is 186,556 B, of which the
equivalences are 160 KB; the no-eq plan is 41,420 B but its correction is
2.28 MB, most of it `.debug_info` (3.5 MB wrong: 218 of 1,263 units
unpaired and interiors of units that changed without changing length),
`.debug_line` (2.3 MB) and `.debug_loclists` (1.3 MB). Residual with the
stream: `.debug_info` 478 KB wrong in 293 K runs — `ref_addr` fields that
sit between two equivalences at an insertion, where nearest-equivalence
projection picks a side — then `.debug_addr` 45 KB. Steps on the synthetic (eq path):

| step | total | `.debug_info` wrong bytes |
|---|---:|---:|
| before | 1,458,732 | 10,905,441 |
| Go module tail copy limited to `.shstrtab` (was shifting the whole debug tail) | 1,083,208 | 928,129 |
| DWARF layer: `ref_addr`, `sec_offset`, `addr` fields, `.debug_addr` | 81,704 | 38 |
| NOBITS sections out of the offset maps (`.bss`/`.noptrbss` share an offset; 118 KB of `.text` was wrong) | 7,188 | |
| `.debug_frame`, `.debug_line`, `.symtab` values and sizes | 2,668 | |

The no-equivalence path places every debug section from record tables
(units, line programs, lists, address groups, FDEs, symbols, strings; DIE
records inside a unit that changed length): 6,842,640 → 583,464 → 3,036 on
the synthetic as the tables went from units to lists and DIEs, and
7,987,992 → 2,325,468 on prometheus as unit pairing moved from in-order
name matching (383 of 1,263 units) to an anchored diff (1,045) and
`.strtab`/`.symtab` got tables (8.6 MB → 10 KB wrong). The synthetic plan is
1,300 B xz (679 KB raw: one entry per list). The fixed-row tables
(`.symtab`, `.debug_addr`, `.strtab`, `.debug_frame`) ship beside
equivalences too and own their sections: Zucchini's byte matches align
symbol rows by chance once every value changes, and trusting them cost
647 KB of `.symtab`.
Both fixes in the first two rows landed in `delta`, so the codec has them;
its sizes did not move (1,184 / 1,140 / 74,126) except the compressed-DWARF
synthetic (9,285,641 → 9,293,321: the shifted tail copy had matched a
little by chance).

Per-section residual on the synthetic, eq path: `.debug_loclists` 120,
`.debug_rnglists` 107, `.text` 91, build IDs 99, `.debug_line` 49,
`.debug_info` 38 wrong bytes. `.debug_info` forms (DWARF 5, 475 CUs,
544,875 DIEs): `ref_addr` 480,828, `sec_offset` 305,740, `addrx` 32,610,
`addr` 17,902; 477,785 `ref_addr` fields shift by exactly the 61 bytes the
edited CU grew.

Harness flags: `-no-dwarf` prices the layer; `-no-text-equivalences` keeps
the stream out of `.text` only (the Go code model owns `.text`, the stream
owns the debug sections).

Design: SPEC §4.5; research: `dwarf-research.md`.

## Non-Go pairs (context)

| pair | presage xz | Zucchini | ratio |
|---|---:|---:|---:|
| Chrome 151.0.7922.169 → .173 (stable patch) | 2,634,264 | 5,889,352 | 44.7 % |
| Chrome 151.0.7922.173 → 152.0.7977.64 (cross-major) | 14,155,296 | 16,592,664 | 85.3 % |
| Firefox libxul 154.0 → 154.0.1 | 4,064,180 | 9,544,652 | 42.6 % |

Two Chrome pairs measured earlier today (152.0.7977.54 → .64, 154.0.8029.0
→ .8030.0) used Chrome-for-Testing binaries with google-chrome debug-info
symbols; those are different builds (Build IDs differ), so the map was built
from the wrong symbols and the results (111 % and 96 % of Zucchini) are not
presage's. CfT publishes no symbols; only the current version of each
google-chrome channel is downloadable as a .deb, which is why the valid
cross-version pair is 151.173 → 152.64. There the field layer is worth
7 % (15,211,888 → 14,155,296) and `.text` is 3.9 % wrong: a major jump
recompiles most bodies, and the advantage over Zucchini shrinks from 55 %
to 15 %.

## Reproducing

Binaries, symbols and Zucchini patches are cached in
`~/.cache/presage-pairs/`; scripts `gd*.sh` there hold the exact commands.
The harness flags added for this work: `-no-equivalences`, `-own-map`,
`-no-go-tables`, `-no-points`, `-no-go-text`.
