# Rust as a presage domain

*Research note, 2026-08-30. Grounds the "ELF C/C++/Rust PIE" row of
`SPEC.md` §9 and the "still open: a Rust server binary in the corpus" of
§10 item 3 in measurements instead of estimates. Extends
`domain-executables.md` §2.3, whose local experiment was a 350 KB toy.
Everything below was measured on this machine; §9 lists the commands.*

## Bottom line

1. **There is a Rust headline available today, with no new code.** `uv`
   0.12.6 → 0.12.7, a real two-day release pair, shipped **stripped** so
   presage runs with no function map at all: **2,883,460 B** against
   bsdiff's 6,025,014 and zstd `--patch-from`'s 6,047,749. 2.09×, round-trip
   verified, 20.7 % of a full compressed download versus bsdiff's 43.2 %.
   That row can go in `baselines.md` this week (§5).
2. **The Go headline does not transfer, and a "one-line Rust change" is the
   *worst* demo we could pick.** On a 6.4 MB Rust binary a one-line edit
   costs presage 25,325 B against bsdiff's 28,810 — 1.14×, versus 125× on
   the equivalent Go pair. A Rust/LLD link answers an insertion with a
   *translation*: bodies move, but rel32 between two functions that moved
   together is unchanged and absolute pointers live in `.rela.dyn` addends.
   bsdiff's suffix array eats that for breakfast. Go's linker instead
   rewrites addresses into `.gopclntab`, moduledata and type descriptors, so
   the shifted bytes are *not* identical — and that, not the shift, is what
   presage recovers.
3. **The Rust win is layout permutation, and it is worth 7×.** Relink the
   *same source* with the function order shuffled and nothing else changed:
   bsdiff 153,716, xdelta3 298,054, zstd `--patch-from` 216,740, presage
   **21,095**. presage gets *smaller* than on the one-line change (no new
   code, only a map); bsdiff gets 5.3× *bigger* (its long matches are gone).
   Not contrived — it is what a PGO or BOLT profile refresh does to every
   release of a large Rust binary, and it is the synthetic worth publishing.
4. **The biggest win on the biggest target was one section name, it needed
   no symbols, and it is now built.** `elfmod` canonicalised the section
   literally called `.text` and no other, so 78 MB of librustc_driver's code —
   `.bolt.org.text`, `.text.cold`, `.text.warm` — was matched with every
   PC-relative displacement intact. Modelling every code window instead:
   **12,001,139 → 6,358,307, −47.0 %**, round-trip verified, **2.38× bsdiff**
   where §4 measured 1.08×, and 57 % faster to encode. `.bolt.org.text`'s own
   mispredicted bytes fell 4,895,886 → 861,324. Chrome and libxul mispredict
   identically to the byte. §6.9.
5. **The map appeared to be a net negative, and the real cause was a
   retarget pass that skipped every unmapped byte.** With a function map,
   `retargetEquivalencePrediction` retargeted *only the mapped bodies* — under
   a comment claiming it matched the serial loop — so 41,263,056 bytes, **39 %
   of librustc_driver's code**, kept the old image's PC-relative
   displacements. `.bolt.org.text` was only 43.4 % covered, because the
   functions BOLT moves leave their originals behind unsymbolised. Covering
   the window with the mapped bodies *and the gaps between them*:
   **9,128,114 → 6,681,766, −26.8 %**, with chrome (2,581,677 → 2,537,338) and
   libxul (3,012,411 → 2,996,074) improving too and `TestNoSymbols` unchanged
   to the byte. Never Rust-specific; Rust made it 39 % instead of 10 %. §6.10.
6. **presage could not name-match a real Rust binary; now it can.** Between
   the two nightlies **6,093 of 101,837** function symbol names (6.0 %) were
   byte-identical; erase the v0 crate-disambiguator tokens and **94,970
   (93.3 %)** match. `symbols.CanonicalName` does that erasure, and with a
   nearest-position tie-break in the content pass it lifts `.text` from 1.7 %
   to 90.0 % name-matched and `.bolt.org.text` to 95.4 %, worth **−12.3 %**
   on the with-symbols path (14,011,895 → 12,283,694). The fix has to be
   mangling-aware, not a blanket strip: v0 spells the instantiation out
   structurally so erasing `Cs<disambiguator>_` is lossless, but legacy's
   `17h<hash>E` is the *only* thing separating two monomorphisations —
   stripping it collapsed 799 of 2,527 names onto twins. Keep it: item 5 says
   the map is not yet worth using here, but it is what every *other* pair in
   the corpus runs on.
7. **Where the 16.5 MB of mispredictions actually is**, now that `-v`
   reports it per section: `.bolt.org.text` 29.6 %, `.strtab` 24.0 %,
   `.text.cold` 7.2 %, `.symtab` 5.6 %, the four `.eh_frame`/`.eh_frame_hdr`
   sections 13.9 %. The string tables are loud and cheap — a standalone
   `.strtab` delta costs 639,760 B, so a perfect model of the noisiest
   quarter of the image is worth ~5 % of the patch. Do not start there. §6.4.
8. **Panic `Location` records are not a churn source.** They looked like the
   Rust-specific table worth a module: 26,047 in librustc_driver, 625 KB in
   `.data.rel.ro`, over 1,695 source files. Across a day of rustc commits,
   383 of 26,004 `(file, line, col)` triples changed and the `(file, col)`
   multiset was *identical*. Do not build it.

## 1. The measured corpus

| pair | what | size | provenance |
|---|---|---:|---|
| `rd-2026-08-27.so` → `rd-2026-08-28.so` | `librustc_driver`, two adjacent rustc nightlies one day apart | 162,016,800 / 162,133,984 | `static.rust-lang.org/dist/<date>/rustc-nightly-x86_64-unknown-linux-gnu.tar.xz`, 84 MB each |
| `uv-0.12.6` → `uv-0.12.7` | astral-sh/uv, released 2026-08-25 and 2026-08-27, stripped, PIE | 51,102,256 / 49,482,464 | GitHub release tarballs |
| `rsbig-v1` → `-v2-oneline` / `-v3-verbump` / `-v4-newmono` / `ord-a`→`ord-b` | synthetic crate (serde, serde_json, regex, clap, chrono, itertools), `opt-level=3`, `panic=abort`, unstripped | 6,443,920 | `§9` |

`librustc_driver` is the interesting artefact and not by accident: it ships
**unstripped**, with 124,255 `FUNC` symbols in `.symtab`, so a third party —
not just the publisher — can give presage the map it needs. rustup has no
delta mechanism; a nightly user downloads the whole 84 MB component every
day.

## 2. Rust's symbol problem

### 2.1 Two mangling schemes, opposite failure modes

rustc 1.93.0 stable still emits **legacy** mangling by default
(`_ZN...17h<16 hex>E`; 558 of 714 `FUNC` in a toy build, the rest C
runtime). rustc builds *itself* with **v0**: 91,307 of 100,082 distinct
`FUNC` names in librustc_driver are `_R...`.

The legacy hash is opaque and derived from the def-path plus the crate's
`-C metadata`, which cargo derives from the package id including its
version. The v0 name carries a `Cs<base62 disambiguator>_<cratename>` token
at every crate reference *and* spells the generic instantiation out in the
type grammar.

Measured consequences, same source, version bump only:

| mangling | binary | distinct names | churn on a version bump | churn on a one-line edit |
|---|---|---:|---:|---:|
| legacy | `rsbig` 6.4 MB | 2,527 | 99 (3.9 %) | 0 |
| legacy | `rsprobe` 3.9 MB | 714 | 16 (2.2 %) | 0 |
| v0 | librustc_driver 162 MB | 101,837 | **95,744 (94.0 %)** across one day | — |

The v0 number is not "rustc changed"; it is the disambiguator. rustc's
nightly version string carries the commit and date, so every crate's
`-C metadata` moves daily, and a v0 name mentions several crates.
Canonicalising the token recovers almost all of it:

```
raw name match        6,093 / 101,837   (6.0 %)
Cs<disambig>_ erased 94,970 / 101,550   (93.3 %)
```

The 6,580 that still differ are a day of real commits.

### 2.2 Why the fix is not symmetric

Erasing the legacy `17h<hash>E` is *destructive*: it is the only field
separating two monomorphisations of the same path. In `rsbig`, 2,527
distinct names became 1,728 (158 collision groups, 799 names lost), and
`chooseNameCandidate` (`match.go:90`) then has to break the tie by size,
`x86.Equal` and position — exactly the failure `elf-module.md` §Status
records on libxul ("collapsed duplicate names … 9,007 FDEs on an identical
libxul"). Since legacy names churn only 3.9 %, there is nothing to buy.

v0 is the opposite: the instantiation is in the name, so two
monomorphisations of `smallvec::deallocate` differ structurally —

```
_RINvCs3IwsiA6RWiX_8smallvec10deallocateINtNtCs8Bf0E2LVbVF_22rustc_pattern_analysis3pat9PatOrWild…
_RINvCs3IwsiA6RWiX_8smallvec10deallocateNtNtCs71XozMx5lIs_9rustc_hir3hir10GenericArgE…
```

— and erasing every `Cs…_` costs 287 of 101,837 names (0.3 %).

**Implementation warning.** v0 uses back-references `B<base62>_` that name a
*byte offset into the mangled string*. Rewriting a disambiguator to a
different length shifts those offsets, so the two sides' names differ in the
back-reference digits too. A regex is enough to measure the opportunity (it
is what produced the numbers above); the module needs a real v0 parse that
re-emits canonically. In Go that is `ianlancetaylor/demangle`, or ~300 lines
of the v0 grammar, encoder-side only — the decoder never sees a name (SPEC
G7).

### 2.3 What that is worth

Unknown until built, and it should be measured before it is promised.
Measuring it needs one small thing that does not exist: `modules.Registry`
constructs `elfmod.Module{Symbols: s}` with no `Stats`, so the module's own
notes — "map: N functions (N by name, N by content, N canonical-equal)" —
are collected nowhere and `presage diff -v` cannot print them. The name-match
rate is the single number this whole section turns on and the CLI cannot
report it.

The mechanism is clear: with 6 % name coverage `constructPlan` falls back to
content hashing, which cannot match a function whose body changed at all,
and `srcPredictor` has no expected source to steer `eqmatch` with — the
lever `matcher-chrome.md` showed was worth 2,945,952 → 2,788,040 on Chrome.

## 3. Where the Rust win actually is

Same synthetic crate, five pairs, every row `presage diff` → `presage patch`
→ `cmp`:

| pair | presage | bsdiff | xdelta3 | zstd `--patch-from` | full `zstd -19` |
|---|---:|---:|---:|---:|---:|
| one line added | 25,325 | 28,810 | 197,626 | 127,170 | 1,425,477 |
| crate version bump only | 5,232 | **4,425** | 8,971 | 9,252 | 1,424,551 |
| one new monomorphisation | 61,664 | 63,820 | 316,111 | 202,966 | 1,425,708 |
| **layout permutation only** | **21,095** | 153,716 | 298,054 | 216,740 | 1,456,848 |
| layout permutation, no symbols | 67,694 | 153,716 | 298,054 | 216,740 | 1,456,848 |

The first three rows are the honest bad news: presage is level with bsdiff
on ordinary Rust source churn at this size. A one-line edit moves 75.6 % of
the file's byte positions, but it moves them by *one constant*, and the
moved bytes are identical because rel32 between two co-moving functions does
not change. presage's plan is 45,453 B and its residual 23,128 B for a
change bsdiff describes in one seek.

The fourth row is the case worth building for. `--symbol-ordering-file` with
a shuffled list, identical source, identical bodies: 72.6 % positional churn
and **not one byte of new program**. bsdiff must re-describe the layout as
2,527 out-of-order copies plus every rel32 that now spans a different
distance; presage sends a permutation and a relocation rule. 7.3×. Even
without symbols — deriving the correspondence from content alone — it is
2.3×.

### 3.1 What the publishable version of the permutation row looks like

The 6.4 MB shuffle above is a mechanism demo. Three changes make it a
benchmark:

1. **Scale it to uv's size** (~50 MB, ~40,000 functions). The permutation
   penalty bsdiff pays is per *reordered run*; presage's map is per
   function with delta-coded columns. The gap should widen with function
   count, and that is the claim to test rather than assume.
2. **Permute realistically, not uniformly.** A uniform `sort -R` is the
   worst case for everyone and easy to dismiss as contrived. The nightly
   pair measures what a real BOLT refresh does: 3.5 % adjacent inversions,
   6,379 distinct displacements over 76,050 functions, 98.5 % of bodies the
   same size. Generate an ordering file with that statistic — mostly local
   swaps, a long tail of movers — and quote both it and the uniform shuffle
   as the bound.
3. **Add the row a real release actually is: permutation *and* a source
   change.** Rows 1 and 4 in isolation each tell half a story; the honest
   release is both at once, and it is the only row that maps onto "what does
   my next update cost".

All three are one shell script over `--symbol-ordering-file`, no network,
and it belongs in `bench/` as R4 (§10). It is also the only Rust benchmark
here that does not depend on a third party's release schedule.

## 4. rustc nightly: the real specimen for that shape

BOLT re-runs on a fresh profile for every nightly. Matching functions by
canonicalised name between 2026-08-27 and 2026-08-28:

| | |
|---|---:|
| uniquely-named functions present in both | 76,050 |
| of those, **identical size** | 74,927 (98.5 %) |
| adjacent order inversions | 2,687 (3.5 %) |
| **distinct old→new displacements** | **6,379** |
| largest single displacement class | 4,045 functions (5.3 %) |

That is the permutation case at 162 MB: 98.5 % of the functions are the same
size a day later, and they are scattered across 6,379 different
displacements, so almost every cross-function rel32 in the image needs a new
value. Every tool, presage included, only halves the download:

| tool | patch | encode | vs full download |
|---|---:|---:|---:|
| **presage** `-symbols` (both images' own `.symtab`) | **14,011,895** | 12 m 28 s | 29.3 % |
| bsdiff 4.3 | 15,106,624 | 89.7 s | 31.6 % |
| `zstd -19 --long=31 --patch-from` | 23,336,349 | 37.1 s | 48.9 % |
| `xdelta3 -9 -B 268435456` | 26,225,821 | 21.6 s | 54.9 % |
| full `zstd -19` of the new image | 47,747,017 | | — |

Round-trip `cmp`-verified, and the apply is 1.1 s at 1.1 GB. Everything else
about the row is bad. Against the honest denominator — bsdiff, which beats
both stream tools here — presage is **1.08×**, for 8× the encode time
(12 m 28 s at 3.1 GB and 124 % CPU, against 111 s for a 291 MB Chrome). The
prediction leaves 18,359,763 mispredicted bytes, 11.3 % of the image, where
Chrome is 0.6 %. This is the weakest presage result on any pair in the
corpus, and §6 says why: the module can see 16 % of the code.

The right way to read this row is as the *floor*. The best target we have —
biggest, most frequent, symbols included, incumbent worst — is the one the
module is least equipped for, and both gaps are named and bounded.

### 4.1 The bigger prize is the toolchain, not the binary

`librustc_driver` is one file inside one component. A default
`rustup update nightly` fetches, for 2026-08-28 (`Content-Length` of each
`.tar.xz`):

| component | bytes |
|---|---:|
| `rustc` | 84,445,932 |
| `rust-std` | 31,635,576 |
| `rust-docs` | 24,291,008 |
| `cargo` | 10,940,012 |
| `clippy` | 5,300,176 |
| `rustfmt` | 2,490,480 |
| **default profile total** | **159,103,184** |
| (+ `rust-analyzer` 9,532,568, `llvm-tools` 40,970,820) | 209,606,572 |

**159 MB a day, in full, forever.** rustup's transport is per-component
`.tar.xz` with no delta mechanism; a nightly user re-downloads `rust-docs`
in its entirety to get a compiler fix. Reaching it needs the container
module of `SPEC.md` §10 item 4 — xz frame → tar → members dispatched, with
`rustc`'s members going to `elf` — plus §6's multi-`.text` work, because
`rust-std` is `.rlib` archives of ELF objects and `rustc` is the BOLT'd
dylib. That is the system-level version of this domain and the one worth
naming out loud; the single-file row above is the part that is measurable
today.

## 5. uv: a real release pair, no symbols, and it already works

`uv` is the counter-example to §3's pessimism, and the cheapest headline we
have. It ships **stripped**, so presage gets no map at all and runs the
`equivalence-derived` path — and still doubles bsdiff on a real two-day
release pair (round-trip `cmp`-verified, 102 s encode, 1.3 GB RSS):

| tool | patch | vs presage | share of a full `zstd -19` download |
|---|---:|---:|---:|
| **presage** (no symbols) | **2,883,460** | — | 20.7 % |
| bsdiff 4.3 | 6,025,014 | 2.09× | 43.2 % |
| zstd `-19 --long=31 --patch-from` | 6,047,749 | 2.10× | 43.4 % |
| xdelta3 `-9 -B` | 7,749,659 | 2.69× | 55.6 % |
| full `zstd -19` of the new binary | 13,948,701 | | — |

The pair is not a soft one: 93.4 % of byte positions differ, `.text` shrank
1.48 MB in two days, and `.eh_frame` and the 72,533-entry `.rela.dyn` moved
with it. presage sends 2.9 MB where the whole new binary compresses to 13.9.

Two things this proves. First, the §3 synthetic's pessimism is a *size*
effect, not a Rust effect: at 50 MB there is enough second-order churn
(`.rela.dyn` addends, `.eh_frame`, cross-function rel32) for the layers to
pay, and the win arrives without the correspondence work of §2 at all.
Second, "we need the publisher's symbols" is not a precondition for a Rust
headline — it is an upside. What §2 and §6 buy on top of 2.09× is unmeasured
and should stay unpromised until it is.

## 6. What blocks the module on a BOLT'd binary

*Measured 2026-08-30 against `rd-2026-08-27.so` → `rd-2026-08-28.so`.*

### 6.0 A bad map is worse than no map

The first thing to know about this pair is that **giving the encoder
symbols makes the patch bigger**:

| run | mapped window | plan | residual | mispredicted | patch |
|---|---|---:|---:|---:|---:|
| no symbols | — | 2,054,278 | 11,084,362 | 16,539,076 | **12,001,139** |
| `-symbols`, raw names | `.text` (26.9 MB) | 2,220,946 | 13,148,627 | 18,359,763 | 14,011,895 |
| `-symbols`, raw names, names swapped | `.bolt.org.text` (67.4 MB) | 2,629,235 | 12,565,580 | 18,039,792 | 13,596,870 |
| `-symbols`, R1+R1b | `.text` (26.9 MB) | 2,424,790 | 11,334,107 | 16,745,048 | 12,283,694 |
| `-symbols`, R1+R1b, names swapped | `.bolt.org.text` (67.4 MB) | 2,541,156 | 11,867,072 | 17,494,573 | 12,782,855 |

Row 2 is −17 % for handing the encoder a full 124,255-symbol table. That is
not a missing feature, it is a map that is actively wrong, and §6.1 says why.

Row 3 is the coverage control. It renames `.text` ↔ `.bolt.org.text` in both
images' section headers (bytes untouched) so the module maps the *large*
window instead of the small one. Two and a half times more code under the
map, and the patch is still 13 % worse than no symbols at all — coverage is
not the bottleneck while the map is wrong.

Row 4 is R1 (v0 disambiguator canonicalisation) plus R1b (nearest-position
tie-break in the content pass). It is worth **1,728,201 B, −12.3 %**, and it
predicts its window nearly perfectly: **710,481 mispredicted bytes out of
26,895,312**, 2.6 %. And it is *still* 2.4 % worse than shipping no symbols.

That last fact is the important one. With a correct 90 %-name-matched map,
both the plan **and** the residual came out worse than with no map at all
(+370,512 B of plan, +249,745 B of residual). The function map is not
failing on this pair; it is winning a contest that was already won. Between
two consecutive nightlies the whole-image equivalence matcher already
reconstructs `.text` from the old image, and the structural machinery layered
on top costs more than the ~300 KB of mispredictions it removes.

Row 5 confirms it on the target this section is named for. Map the 67.4 MB
`.bolt.org.text` with the *fixed* map — 61,967 units, 59,093 matched by name
(95.4 %), 58,682 canonical-equal — and the patch is **781,716 B worse** than
no symbols. Coverage and quality now both go the right way and the result
goes the wrong way, monotonically:

| correctly mapped code | patch vs no symbols |
|---:|---:|
| 0 B | — |
| 26,895,312 B | +282,555 |
| 67,399,817 B | +781,716 |

§6.5 attributes those extra bytes and finds no collateral at all: the map's
damage is entirely inside its own window, and it is a straight regression
against the matcher it replaces.

`librustc_driver` section layout, and where its `FUNC` symbols live:

| section | size | `FUNC` symbols |
|---|---:|---:|
| `.bolt.org.text` | 67,399,817 | 86,398 (69.5 %) |
| `.text` | 26,895,312 | 20,419 (16.4 %) |
| `.text.cold` | 10,096,650 | 12,206 (9.8 %) |
| `.text.warm` | 1,391,162 | 3,430 (2.8 %) |
| elsewhere | | 1,802 (1.5 %) |

BOLT's default `-lite` mode re-emits only profiled functions into a new
`.text` and renames the original section `.bolt.org.text`; both are mapped
and executable. `.bolt.org.rodata`, `.bolt.org.eh_frame`,
`.bolt.org.eh_frame_hdr` and `.bolt.org.gcc_except_table` are live too — the
panic `Location` string pool resolves into `.bolt.org.rodata`.

`elfmod` requires `.text` (`elf-module.md` §3 step 1), builds `codeUnits`
against `oi.Text` alone (`encode.go:34`), masks only `.text` for canonical
matching (`maskedText`, `encode.go:200`), and runs its retarget, choice and
field-fix stages over the `.text` window. On this image that is 16 % of the
functions. Everything in `.bolt.org.text` falls through to whole-image
equivalences — precisely the configuration §3's last row measures at 3.2×
worse.

### 6.1 Why the map is wrong, in one table

`constructPlan` pairs functions by name first, then by canonical content
hash for whatever is left. Both passes measured directly, per code section
(`presage/elfmod` probe, symbols from each image's own `.symtab`):

| section | new units | name-matched | content-matched | of those, **ambiguous** | **wrong twin taken** | unmapped |
|---|---:|---:|---:|---:|---:|---:|
| `.text` | 13,465 | 230 (1.7 %) | 9,343 | 1,233 (13.2 %) | 802 (8.6 %) | 3,892 |
| `.bolt.org.text` | 61,967 | 2,675 (4.3 %) | 56,278 | 32,408 (57.6 %) | **29,251 (52.0 %)** | 3,014 |

Two independent defects compound:

1. **The names don't match** (§2), so 97 % of the map comes from the
   content pass.
2. **The content pass had no tie-break.** It took the *first* entry of the
   `(size, canonical hash)` bucket and stopped, where the name pass
   deliberately picks the nearest twin by position ("Duplicate names
   (closures, monomorphisations, anonymous-namespace templates) tie on
   score; the twin at the nearest position wins", `match.go`). On a Rust
   image monomorphisation and ICF make that bucket enormous — 21,958 of
   `.bolt.org.text`'s units have **more than eight** canonical-equal
   candidates — so the arbitrary pick is wrong about half the time. A wrong
   `Src` then poisons three things at once: the structural prediction copies
   the wrong body, `retargetText` resolves references through it, and
   `srcPredictor` feeds `eqmatch.Params.Expect` a wrong expected source,
   where `MinFar = 96` *rejects* the correct far run for disagreeing with
   it. That is the −17 % of §6.0.

Both are now fixed. Erasing the v0 disambiguator
(`symbols.CanonicalName`, applied in `codeUnits`) and giving the content
pass the same nearest-position tie-break as the name pass:

| section | name-matched before | after | wrong twin before | after | unmapped before | after |
|---|---:|---:|---:|---:|---:|---:|
| `.text` | 230 (1.7 %) | **12,113 (90.0 %)** | 802 | 131 | 3,892 | 323 |
| `.bolt.org.text` | 2,675 (4.3 %) | **59,093 (95.4 %)** | 29,251 | 1,252 | 3,014 | 147 |

`.bolt.org.text` goes from a map that is half nonsense to one that names
95.4 % of its functions correctly — for a section the module cannot address
at all yet. That is the argument for doing §6.2 next, and the reason to do
it in this order: enabling the map on `.bolt.org.text` *before* the
canonicalisation would have made this pair dramatically worse.

### 6.2 The multi-window map

The change is bounded, and two measurements settle its shape.

**Functions do not migrate between code sections.** Of the 76,050
uniquely-named functions present in both nightlies, **75,968 (99.89 %) are
in the same section**; 82 move, 69 of them `.bolt.org.text` → `.text` as
BOLT's profile promotes them. So the map may be built *per section pair*,
keyed by name, and loses nothing. The alternative — one flat window over
all code — is unnecessary.

**There are exactly two code windows, not five.** Executable sections
group into maximal runs with a constant address−offset skew, which is a
property of the LOAD segment they sit in:

| window | sections | addr − off | size |
|---|---|---:|---:|
| 1 | `.bolt.org.text` `.init` `.fini` `.plt` | 0x1000 | 67,405,552 |
| 2 | `.text` `.text.warm` `.text.cold` | 0 | 38,383,124 |

A Go, Chrome or libxul image has exactly one such window, which is
`.text` — so a per-window design degenerates to today's behaviour on
every pair now in the gate, and the existing numbers are the regression
test.

Sketch: `image.Code []codeWindow` replaces `image.Text`; `elfmod` holds one
`predictionPlan` per window pair (paired by the first section's name, each
already carrying its own `OldAddr`/`NewAddr`/`TargetLen`); `Ranges` hoists
to a shared header and excludes every mapped window; `addressLookup`,
`funcSizeDeltas` and `equivalencePlan`'s text-projection dispatch on which
window an address falls in; `maskedText` masks all of them; `choices` and
`fields` become one stream per window. The wire format gains a count and
loses nothing else.

The prize, from the per-section attribution of §6.3: `.bolt.org.text`,
`.text.cold` and `.text.warm` are 78.9 MB of the image (48.7 %) and
8,541,345 B of a 20,521,908 B per-section delta proxy (42 %), all of it
currently unmodelled.

### 6.3 Where the bytes are, section by section

Every section present in both images, dumped and delta'd on its own with
`zstd -12 --long=27 --patch-from`. The proxy overstates in total (the sum is
20.5 MB against presage's 14.0 MB whole-image, because each section pays its
own dictionary), but it ranks the work correctly.

| section | new size | zstd delta | modelled by |
|---|---:|---:|---|
| `.bolt.org.text` | 67,399,817 | **6,637,049** | *nothing* |
| `.text` | 26,895,312 | 6,329,882 | map, retarget, choices, field fix |
| `.text.cold` | 10,096,650 | **1,666,653** | *nothing* |
| `.symtab` | 3,873,408 | 940,945 | *nothing* (lz) |
| `.eh_frame` | 6,192,580 | 741,970 | `.eh_frame` layer |
| `.strtab` | 18,797,170 | 639,760 | *nothing* (lz) |
| `.bolt.org.eh_frame` | 4,763,636 | **567,107** | *nothing* |
| `.eh_frame_hdr` | 825,020 | 461,714 | regenerated |
| `.bolt.org.rodata` | 4,600,382 | **395,013** | *nothing* |
| `.rela.dyn` | 2,597,832 | 382,352 | reloc layer |
| `.bolt.org.eh_frame_hdr` | 604,028 | **330,067** | *nothing* |
| `.gcc_except_table` | 1,428,609 | 320,438 | *nothing* |
| `.bolt.org.gcc_except_table` | 2,498,596 | **317,742** | *nothing* |
| `.text.warm` | 1,391,162 | **237,643** | *nothing* |
| `.dynsym` | 519,312 | 177,980 | *nothing* |
| `.rodata` | 274,624 | 132,151 | rodata layer |
| | | 20,521,908 | sum |

Three groups, and the module addresses one of them:

- **Unmodelled code — 8,541,345 (42 %)**: `.bolt.org.text`, `.text.cold`,
  `.text.warm`. §6.2.
- **BOLT's second copy of the metadata — 1,609,929 (8 %)**: the module's
  `.eh_frame`, `.eh_frame_hdr`, `.rodata` and `gcc_except_table` layers are
  keyed by *name*, and BOLT emits a complete second set for the functions it
  did not move. The `ehframe`/`rodata` layers need the same
  one-per-instance generalisation as the map — the same shape of change, in
  the same places.
- **Symbol tables — 1,840,885 (9 %)**: `.strtab` + `.symtab` + `.dynstr` +
  `.dynsym` are 25.8 MB of the shipped image (15.9 %) and have no model at
  all. Their churn is almost entirely the disambiguator substitution of §2 —
  a few hundred distinct `Cs<old>_` → `Cs<new>_` rewrites applied throughout
  an 18.8 MB string table. That is a *rule*, and no byte differ can state
  it. Unlike the other two groups this needs a new layer, not a
  generalisation; park it behind §6.2.

Note `.text` at 6,329,882 against `.bolt.org.text` at 6,637,049: the
modelled section costs 0.236 B per byte and the unmodelled one 0.098,
because `.bolt.org.text` is the *stable* half (BOLT left it where LLD put
it) while `.text` is the half BOLT re-orders every run. Per byte the map is
pointed at the harder section; in absolute bytes it is pointed at the
smaller one.

### 6.4 Where the prediction actually fails

§6.3 is a proxy. This is the measurement: `presage diff -v` now reports the
mispredicted bytes attributed to the new image's sections
(`encode.go:sectionErrNote`). No symbols, so this is the baseline every
symbol run has to beat.

| section | mispredicted | share | modelled by |
|---|---:|---:|---|
| `.bolt.org.text` | 4,895,886 | 29.6 % | *nothing* |
| `.strtab` | 3,967,293 | 24.0 % | *nothing* |
| `.text.cold` | 1,184,135 | 7.2 % | *nothing* |
| `.symtab` | 924,648 | 5.6 % | *nothing* |
| `.bolt.org.rodata` | 840,217 | 5.1 % | *nothing* |
| `.eh_frame_hdr` | 822,362 | 5.0 % | regenerated |
| `.bolt.org.eh_frame_hdr` | 597,735 | 3.6 % | *nothing* |
| `.bolt.org.eh_frame` | 509,221 | 3.1 % | *nothing* |
| `.text` | 502,732 | 3.0 % | map, retarget, choices, field fix |
| `.dynstr` | 408,384 | 2.5 % | *nothing* |
| `.eh_frame` | 375,352 | 2.3 % | `.eh_frame` layer |
| `.gcc_except_table` | 349,694 | 2.1 % | *nothing* |
| 18 more | 1,161,417 | 7.0 % | |
| | **16,539,076** | | |

Read it against §6.3's cost column, because a mispredicted byte in a string
table is not worth a mispredicted byte of machine code:

- **`.strtab` is loud and cheap.** 3,967,293 mispredicted bytes — a quarter
  of every failure in the image — because the v0 crate disambiguator changes
  every nightly and every mangled string with it. But §6.3 prices a
  standalone `.strtab` delta at 639,760 B: the residual's LZ already reads
  through a substitution that leaves the surrounding 18.8 MB untouched. A
  perfect string-table model is worth ~5 % of the patch, not 24 %. Park it.
- **`.bolt.org.text` is the real one.** Loudest *and* dearest: 29.6 % of the
  failures and §6.3's top cost line at 6,637,049.
- **The `.eh_frame` family is 2,304,670 (13.9 %), and half of that is one
  self-disqualifying gate.** See §6.8.

### 6.5 The function map loses to the matcher it is supposed to help

> **Superseded by §6.10.** Everything measured here is real and every
> hypothesis it rules out is correctly ruled out — but the cause was none of
> the candidates it considers. It was `retargetEquivalencePrediction`
> retargeting only the bodies the map named and leaving 39 % of the code with
> the old image's displacements. Read this section for the eliminations, not
> for its conclusion.

Attribute the same pair with and without symbols, section by section. Only
one line moves:

| section | no symbols | `.text` mapped (R1+R1b) | Δ |
|---|---:|---:|---:|
| `.text` | 502,732 | 710,481 | **+207,749** |
| `.eh_frame` | 375,352 | 373,447 | −1,905 |
| every other section | | | **0** |
| | 16,539,076 | 16,745,048 | +205,972 |

There is no ripple. `.bolt.org.text`, `.strtab`, `.symtab`, the whole
`.eh_frame` family and the eighteen-section tail all mispredict *exactly* the
same number of bytes either way. The function map does not perturb the image
outside the window it governs — and inside that window it is **41 % worse**
than the whole-image equivalence matcher it displaces, while adding 370,512 B
of plan and 249,745 B of residual.

That reframes §6.0. It was never "a good map is not worth its plan"; it is
"the map makes the prediction worse". The mechanism is in `eqmatch`:

```go
if p.Expect != nil {
    if e, ok := p.Expect(pos); ok && e >= 0 && e+k <= len(src) {
        expected = e                       // match.go:265
```

`Expect` **overwrites** the delta-continuation `expected`, and `expected` is
what `MinFar` measures against: `CodeDefaults.MinFar = 96` refuses to emit
any run of 12..95 bytes whose source is not near `expected`. With no map,
"near `expected`" means "continues the previous run" and the rule drops
isolated short idioms. With a map, it means "near where the map says this
function came from", so every short run the map does not vouch for is
censored. The map is not merely failing to help; it is vetoing the matcher's
own good answers.

It is being paid almost nothing for that veto. Adding a 13,142-function map
shrank the equivalence stream from 1,576,326 B to 1,571,487 B — **4,839 B**.
The whole point of `Expect` is that a run near the map's source codes its
source column as a cheap residual; here the matcher almost never lands on the
map's source, so the coding win never materialises and only the censorship
does.

`CodeDefaults` was tuned on Chrome and libxul *with* `Expect` set
(`eqmatch/match.go:74`), where it beats Zucchini's stream. Nothing here says
it is wrong there.

**Measured, and it is not the cause.** Nor are the reference points:

| variant | `.text` mispredicted | plan | patch |
|---|---:|---:|---:|
| no map | **502,732** | 2,054,278 | **12,001,139** |
| map | 710,481 | 2,424,790 | 12,283,694 |
| map, `Expect` suppressed | 720,894 | 2,436,932 | 12,301,431 |
| map, reference points dropped | 710,440 | 2,192,810 | 12,272,313 |

Suppressing the hint makes it marginally *worse*. Dropping all 89,504
reference points moves `.text` by 41 bytes. Both are exonerated, and what is
left is the mappings themselves: `newAddressLookup` projects an old `.text`
address through the map, and `retargetEquivalencePrediction` rewrites every
displacement it can answer for. With no map it can answer for none —
`sectionRanges` excludes `.text` by name — so the copied bytes are left alone,
and that is the configuration that wins.

Worth noting in passing: those reference points cost **274,690 B of plan**
(`structure` 361,360 → 86,670) to buy 41 bytes of prediction here. They are
measured valuable elsewhere; they are not free, and nothing checks.

A likely suspect for the projection itself, for whoever picks up R7:
`constructPlan` emits a mapping for every name match regardless of score, and
`allMapped` then marks all of them `Copy` (`match.go:261`). On this pair
12,113 units matched by name but only 9,522 were canonical-equal, so ~2,600
mappings assert a correspondence between bodies that genuinely differ, and the
oracle projects addresses through them anyway.

The choice cannot be the fix, and this is the structural point. It is a
strict per-function comparison —

```go
win[i] = wrongCount(structuralPred[...], targetBody) <
         wrongCount(equivalencePred[...], targetBody)   // retarget.go:81
```

— so it is monotone against `equivalencePred`. But `equivalencePred` is the
output of `retargetEquivalencePrediction(text, ep, structure, …)`, which has
*already* rewritten displacements using the map's oracle. Both options the
choice picks between are downstream of that oracle. If the oracle is worse
than leaving the bytes alone, no per-function choice can recover it, because
"leave it alone" is not on the ballot.

With no map the oracle knows nothing about `.text` (`sectionRanges` excludes
it by name) so intra-`.text` displacements are left as copied — and 502,732
bytes are wrong. With a map the oracle answers, and 710,481 are wrong. The
map's projection is losing to *doing nothing*, which is only possible if it
is confidently wrong: 89,504 inferred reference points and 13,142 mappings
asserting motion that did not happen.

The module has no guard against this anywhere: nothing measures whether the
map earned its place before committing the whole image to it.

### 6.6 The 67 MB window, measured

Attribute the swapped run the same way. Remember the names are exchanged, so
the section reported as `.text` holds the 67.4 MB body and the one reported
as `.bolt.org.text` holds the 26.9 MB one. In real terms:

| real section | no symbols | swapped + R1+R1b | Δ |
|---|---:|---:|---:|
| `.bolt.org.text` (67.4 MB) — modelled | 4,895,886 | **2,979,416** | **−1,916,470 (−39 %)** |
| `.text` (26.9 MB) — not modelled here | 502,732 | 3,324,041 | +2,821,309 |

The second row is an artifact of the experiment, not a result.
`maskedText` canonicalises the section *named* `.text` and nothing else
(`encode.go:maskedText`), so in the swapped image the real `.text` was matched
on raw bytes with every PC-relative displacement intact, and the equivalence
matcher lost it. One window can be modelled at a time; that is precisely the
limitation R2 exists to remove, and the swap cannot measure around it.

The first row is the result: **the module cuts the 67 MB window's
mispredictions by 39 % the moment it can reach it.** That is 1.9 MB, against
a whole-image total of 16.5 MB.

What the swap *cannot* say is which of three things bought it. Reaching a
window means masking it for the match, retargeting the copies afterwards, and
mapping its functions — and §6.5 has already shown the third of those to be a
net negative on `.text`. The control that separates them is the swapped pair
with **no symbols**: same mask, same retarget, no map.

### 6.7 The win is the mask, not the map

Run the swapped pair with no symbols at all. The 67 MB window gets masked,
retargeted and field-fixed; nothing gets a function map.

| real section | baseline, no symbols | 67 MB window modelled, no symbols | Δ |
|---|---:|---:|---:|
| `.bolt.org.text` (67.4 MB) | 4,895,886 | **846,589** | **−4,049,297 (−83 %)** |
| `.text` (26.9 MB) | 502,732 | 3,324,041 | +2,821,309 |
| `.text.cold` | 1,184,135 | 1,183,994 | −141 |
| whole image | 16,539,076 | 15,364,626 | −1,174,450 |
| plan | 2,054,278 | 2,264,736 | +210,458 |
| **patch** | **12,001,139** | **10,311,138** | **−1,690,001 (−14.1 %)** |

Round-trip verified: `presage patch` reconstructs all 162,133,984 bytes
byte-identically in 2.1 s. Encode took 8m59s, *faster* than the 11m35s
baseline.

**−83 % on the window, and not one symbol was used.** The whole `.bolt.org`
problem was never the missing function map. It is that `maskedText`
canonicalises the section literally named `.text` and nothing else, so 67 MB
of code is matched with its PC-relative displacements intact, and every run
the matcher does find is copied without retargeting.

And this is the *penalised* configuration: it pays +2,821,309 to abandon the
26.9 MB window, because the experiment can only reach one at a time. A real
implementation reaches both. Taking each window's measured best:

| window | best measured | source |
|---|---:|---|
| `.bolt.org.text` (67.4 MB) | 846,589 | §6.7 |
| `.text` (26.9 MB) | 502,732 | §6.4 |
| | **1,349,321** | against **5,398,618** today |

−4,049,297 mispredicted bytes, three times what the one-window experiment
could show. The patch consequence is not a straight scaling — this run bought
−1,690,001 B of patch for −1,174,450 B of mispredictions, but part of that
ratio is `.text.cold` and the field-fix stream — so take it as a lower bound:
**R2 puts this pair under 10 MB, against bsdiff's 15,106,624.** That is
≥1.5×, where §4 measured 1.08×.

This changes what R2 *is*. The expensive part of the design in §6.2 — one
function map per window, paired by name, `choices` and `fields` per window —
is not what earns the money, and §6.5 says the map is a net negative until
its oracle is fixed. What earns the money is much smaller and needs no
symbols:

- `image.Text section` becomes `image.Code []section`, the executable,
  allocated, file-backed sections.
- `maskedText` canonicalises every window, not the one named `.text`.
- `retargetEquivalencePrediction` and the field fix run per window.
- `sectionRanges` excludes every window rather than just `.text`.
- The wire format gains a window count; a mapless window's plan is ~300 B.

It works on stripped binaries, which is where the shipping corpus lives.
The per-window function map becomes an independent, later question — and one
that §6.5 says should not be answered until the oracle defect is understood.

### 6.8 `.eh_frame_hdr` regeneration never runs, and cannot

`.eh_frame_hdr` is 825,020 B and **822,362 of them are wrong** — 99.7 % — in a
section the module claims to regenerate from scratch. The layer reported
nothing at all until now; every other layer prints a `-v` note and this one
did not. Wired up (`predStats.EhFrame`, `elfmod.go`), it says:

```
eh_frame: 102966 FDEs, 99055 retargeted, 3911 unknown, 0 resized, 0 hdr entries
```

99,055 of 102,966 FDEs retargeted — the `.eh_frame` half works — and **zero
header entries written**. `regenerateEhFrameHdr` has three early returns;
`HdrSize` is 825,020 (≥ `ehHdrPrefix`) and `fdes` is 99,055 (non-empty), so by
elimination it is the third:

```go
hdr := out[p.HdrOff : p.HdrOff+p.HdrSize]        // ehframe.go:231 — the prediction
if hdr[0] != 1 || hdr[1] != ehPtrEncPCRelSData4 ||
   hdr[2] != ehEncUData4 || hdr[3] != ehTableEncDataRel4 {
	return 0                                     // ehframe.go:232
}
```

It validates the version and encoding bytes by reading **the prediction it is
about to overwrite**. Those four bytes are `01 1b 03 3b` in both nightlies and
in both the `.eh_frame_hdr` and `.bolt.org.eh_frame_hdr` of each — but the
*prediction* of that region is zero-filled, because nothing covers it. The
section's first 12 bytes are those four encodings followed by `eh_frame_ptr`
and `fde_count`, both of which change, so there is no 12-byte run for
`CodeDefaults.Min` to find, no equivalence, and `layImage` leaves zeros. The
gate fails on its own output.

The layer disqualifies itself precisely when it is needed: a header that
*is* well predicted passes the gate and gets regenerated redundantly; one
that is not, does not. Confirmed by switching the regeneration off
altogether — patch, plan and every per-section count came back byte-identical
to the baseline (12,001,139).

The evidence it needs is in the old image, which the decoder has. The fix is
to take those four bytes from the old `.eh_frame_hdr` rather than from the
prediction, which costs one uvarint of plan (`OldHdrOff`) and a wire-format
bump. §6.3 prices a standalone `.eh_frame_hdr` delta at 461,714 B, which
bounds the win; `.bolt.org.eh_frame_hdr` is a second 597,735 mispredicted
bytes that no layer reaches at all, and R2 does not reach it either — the
layers are keyed by section name.

### 6.9 R2, built and measured

Implemented as §6.7 specified: `image.Code` lists the executable sections
whose name has a dot-component equal to `text`, `pairCodeWindows` matches them
by name across the two images, and `maskedCode`, the retarget pass, the field
fix and the function map all run once per window. `equivalencePlan` carries a
window list instead of one `.text` pair, and the structure, choice and field
streams became one length-prefixed sub-stream per window.

Four windows found on librustc_driver: `.bolt.org.text` 67,366,601 B, `.text`
26,841,718 B, `.text.cold` 10,086,474 B, `.text.warm` 1,393,210 B.

| | baseline | **R2** | Δ |
|---|---:|---:|---:|
| patch | 12,001,139 | **6,358,307** | **−47.0 %** |
| mispredicted | 16,539,076 | 11,268,368 | −5,270,708 |
| plan | 2,054,278 | 2,713,097 | +658,819 |
| `.bolt.org.text` | 4,895,886 | **861,324** | **−82.4 %** |
| `.text.cold` | 1,184,135 | ≤ 330,866 | −72 % or better |
| `.text` | 502,732 | 491,982 | −10,750 |
| encode | 11m35s | **4m58s** | −57 % |

Round-trip verified: `presage patch` reconstructs all 162,133,984 bytes
byte-identically in 2.5 s.

**2.38× bsdiff** (15,106,624), where §4 measured 1.08×. The pair goes from the
weakest result in the corpus to a competitive one, with no symbols, no
function map, and no new layer — the module simply stopped being unable to
see 78 MB of the code it was given.

It is also *faster*. Canonical masking turns a window whose bodies all moved
into one the matcher can find long runs in, so eqmatch does less work, not
more, and the encode more than halved.

Note the plan grew 658,819 B, almost all of it the field fix (180,801 →
773,640) over four windows instead of one. That is a real cost and it is paid
several times over.

#### R2 follow-ups

Two loose ends, neither affecting any measured pair:

- **The encoder can emit a plan its own decoder rejects.**
  `parseEquivalencePlan` requires the windows to be sorted and
  non-overlapping in both images, which is the right check for untrusted
  input, but `pairCodeWindows` does not guarantee it: it sorts by *old*
  address and pairs by name, so a linker that reordered the sections in the
  new image would produce a plan that fails to parse. `Analyse` verifies by
  round-tripping, so this surfaces as an encode error rather than a corrupt
  patch — but `codec.go:117` treats a non-declined module error as fatal for
  the whole encode instead of declining to LZ. Make `pairCodeWindows` drop
  any window that would break monotonicity in either image.
- **`.rodata` switch tables still know one window.** `roDataPlan` carries a
  single `TextLo`/`TextHi`, set from `.text`, so a jump table pointing into
  `.bolt.org.text` is not a candidate. That is why the §6.6 swap run reported
  `rodata: 0 of 3192 spans kept`. On the real pair `.text` is window 0 and the
  layer behaves as before, so this costs nothing today and is not a
  regression — it is unclaimed ground.

#### R2 makes R7 urgent

R2 generalises the function map along with the geometry, so the map now
governs all four windows — 88,806 functions, 83,819 of them matched by name,
R1 working exactly as §6.1 measured. Run the same pair with symbols:

| section | R2, no symbols | R2, with symbols | Δ |
|---|---:|---:|---:|
| `.bolt.org.text` | **861,324** | 3,013,170 | **+2,151,846 (3.5×)** |
| `.text` | 491,982 | 713,378 | +221,396 |
| mispredicted | 11,268,368 | 13,680,225 | +2,411,857 |
| plan | 2,713,097 | 3,552,548 | +839,451 |
| **patch** | **6,358,307** | 9,128,114 | **+2,769,807 (+43.6 %)** |

§6.5 measured the map costing 282,555 B on one window. On four it costs
**2,769,807 B — a third of the patch.** The map is not broken in the sense of
being wrong; it is 93.6 % name-matched and its own `.bolt.org.text` figure of
3,013,170 agrees with the §6.6 swap measurement of 2,979,416. It is that a
correct map, driving `newAddressLookup`, predicts code worse than not
retargeting at all — and R2 has now scaled that from one window to four.

Until R7 lands, **the best result on this pair comes from withholding the
symbol table**, which is a strange thing for a structure-aware patcher to
have to say.

#### Single-window images are unchanged

The window rule was chosen so that Chrome, libxul and Go keep exactly one
window, and the corpus gate confirms the prediction is untouched:

| test | pre-R2 patch | R2 patch | mispredicted |
|---|---:|---:|---|
| `TestPairs` chrome 151.0.7922.169 → .173 | 2,582,404 | 2,581,677 | 1,757,125 → **identical** |
| `TestPairs` libxul 154.0 → 154.0.1 | 3,009,986 | 3,012,411 | 4,855,913 → **identical** |
| `TestNoSymbols` | 3,141,648 | 3,140,750 | 5,319,432 → **identical** |

Mispredicted bytes agree to the byte on all three, so the prediction is
bit-identical; the plan grew by 11, 10 and 6 bytes respectively (the window
count, and one length prefix on the structure stream), and the compressed
patch moves ±0.08 % around that framing change. `TestPairs`,
`TestSelfPrediction` and `TestNoSymbols` all pass, 504 s.

### 6.10 R7: the retarget pass skipped every unmapped byte

`retargetEquivalencePrediction` had two paths. With no function map it
retargeted the whole window. With a map it retargeted **only the mapped
bodies** —

```go
// One body per map, concurrently: bodies are disjoint and every lookup
// is a read, so the result matches the serial loop.
return parallelStats(len(structure.Maps), workers(), func(stats *x86.Stats, i int) {
	m := structure.Maps[i]
	retargetBody(stats, text[m.Dst:m.Dst+m.DstSize], m.Dst)
})
```

— and the comment is false. It matches the serial loop over *the maps*, not
over the window. Every byte no symbol described kept the **old** image's
PC-relative displacements.

On librustc_driver that is **41,263,056 bytes, 39 % of all code**. Worse, the
shortfall is concentrated exactly where it hurts: `.bolt.org.text` was only
43.4 % covered, because the functions BOLT moved into the new `.text` leave
their originals behind *without symbols*. The map is structurally incapable of
covering that section, and the code path read "unmapped" as "leave alone".

That is what §6.5 was measuring. Giving the encoder symbols switched the whole
window from "retarget everything" to "retarget the 61 % of it the symbols
name", and the unretargeted remainder cost more than the map ever won. The
per-function choice could not see it, the `Expect` hint had nothing to do with
it, and the reference points had nothing to do with it — all three of §6.5's
exonerations were correct, and all three were looking past the actual bug.

The fix is `windowSpans`: cover `[0, size)` with the mapped bodies *and the
gaps between them*, and retarget every span. Bodies keep their own start
offset so the map's alignment survives; gaps are retargeted from their own
base instead of skipped. `windowSpans(nil, size)` is a single span over the
whole window, so the no-map path is unchanged by construction.

| | before | after | Δ |
|---|---:|---:|---:|
| librustc_driver, with symbols | 9,128,114 | **6,681,766** | **−26.8 %** |
| ↳ `.bolt.org.text` mispredicted | 3,013,170 | 1,132,280 | −62.4 % |
| ↳ `.text` mispredicted | 713,378 | 561,877 | −21.2 % |
| chrome .169 → .173 | 2,581,677 | **2,537,338** | −44,339 |
| libxul 154.0 → 154.0.1 | 3,012,411 | **2,996,074** | −16,337 |
| `TestNoSymbols` | 3,140,750 | 3,140,750 | identical |

Round-trip verified; corpus gate PASS in 530 s. Chrome and libxul improve too:
their maps cover ~90 % of `.text` and the last tenth had the same hole. This
was never Rust-specific — Rust merely made it 39 % instead of 10 %.

#### Which oracle branch was actually lying

Scoring every rewritten displacement against the true new bytes, attributed to
the branch that answered:

| branch | helped | harmed |
|---|---:|---:|
| byte projection | 1,385,943 | 6,430 |
| exact point | 1,115,525 | 0 |
| section projection | 155,610 | 0 |
| ranges | 4,803 | 0 |
| **`mapTarget`** | **163** | **5,411** |
| total | 2,662,044 | 11,841 |

Retargeting is 225:1 beneficial and three of the five branches are never wrong
once. `mapTarget` is the only net-negative rule, 33:1 the wrong way — the
`allMapped` lead from §6.5 was right in kind: a name match whose bodies
genuinely differ still asserts a byte-for-byte correspondence the oracle
projects through. It is worth ~21 KB, so it is a real follow-up and a small
one.

#### The safety net was the wrong fix, and the field fix says why

The other candidate fix for §6.5 was a floor: give the per-function choice a
third option, the equivalence copy *as `layImage` left it*, so that a wrong
address oracle could never cost anything the choice was not free to refuse.
It was built and measured against the pre-R7 baseline — where its premise was
at its strongest, since 39 % of the code was going unretargeted — and it is a
negative result.

| pair | pre-R7 baseline | with the floor | Δ |
|---|---:|---:|---:|
| rustc, with symbols | 9,128,114 | 9,130,349 | **+2,235** |
| rustc, no symbols | 6,358,307 | 6,358,307 | identical |
| chrome | 2,581,677 | 2,584,141 | **+2,464** |
| libxul | 3,012,411 | 3,011,494 | −917 |

The decisive count: **57 of 88,806 bodies** preferred the unretargeted copy,
24,708 B in total. A dense second bitmap costs ~11 KB whether it carries 57
selections or none; even encoded sparsely the option does not pay.

The reason is that the floor already exists one level down. `applyFieldFix`
corrects at **4-byte granularity** — 2,910,980 sites on this pair — and
correcting a handful of broken displacements individually is strictly cheaper
than reverting a whole function body to get them. There is almost never a body
where wholesale reversion beats retail correction, and the 57 that exist are
not worth a stream.

Worth keeping as a principle: before adding a coarse escape hatch, check
whether a finer correction layer already covers the same failure. Here the
per-function choice and the per-field fix are two granularities of the same
idea, and the finer one dominates.

#### What is left of the map's deficit

With symbols is still behind no symbols, 6,681,766 against 6,358,307, but the
gap fell from 2,769,807 to **323,459** and it is now arithmetic rather than a
defect. The map costs **+839,451 B of plan** — a 1,401,484 B structure stream
for 88,806 mappings and 293,345 points — and returns about 516 K of residual.
It under-pays for itself by roughly the difference. That is a question about
`predictionPlan.marshal`'s encoding density, not about the oracle.

With the retarget fixed, the `Expect` hint also flips sign: suppressing it now
costs 54,384 B, where §6.5 measured it as a slight *gain* to suppress.

## 7. Negative results, so nobody re-derives them

- **Panic `Location` records.** 26,047 in librustc_driver (625 KB of
  `{ptr,len,line,col}` in `.data.rel.ro`, 1,695 distinct files); 224 in a
  3.9 MB toy. Across the nightly pair, 25,621 of 26,004 `(file,line,col)`
  triples are unchanged and the `(file,col)` multiset is *identical* — real
  Rust releases do not insert lines above panic sites often enough to
  matter. The pointer field is a `R_X86_64_RELATIVE` addend and is already
  the reloc layer's business. **No module.**
- **`.eh_frame` coverage.** 556 FDEs against 649 `.text` `FUNC` symbols in
  the toy (86 %), and Rust emits `.eh_frame` even under `panic=abort`. Good
  enough to *derive* boundaries decoder-side and stop shipping the boundary
  column — but `detectBoundaries` (`structure.go:34`, `0xcc` padding runs)
  already costs nothing and is not the bottleneck. Park it.
- **The rustc commit SHA in every std path string**
  (`/rustc-dev/<40 hex>/library/...`) changes wholesale between nightlies.
  It compresses to nothing (one literal, then matches) and the lengths are
  equal so no pointer moves. Not a lever.
- **Stripping the legacy mangling hash.** §2.2. Actively harmful.

## 8. Headline candidates, ranked

Score = size × cadence × whether symbols are obtainable × how badly the
incumbent does.

1. **`rustup update nightly` / `librustc_driver.so`.** 162 MB, rebuilt
   daily, **ships its own `.symtab`**, BOLT'd so byte tools only halve the
   download, and rustup ships no delta at all. The strongest technical case
   and the one the Rust audience feels personally — but presage is at 1.08×
   over bsdiff on it today (§4), and 12 m 28 s. Publish it after R1 and R2,
   not before.
2. **`uv`.** ~50 MB, released every few days, ~19.5 M downloads of the
   0.12.5 assets. Stripped — and **measured at 2.09× bsdiff anyway** (§5),
   with no symbols and no new code. Ready to publish today; the only thing
   the symbol work would add is a bigger number. Best reach, zero access
   cost, and the row that can go in `baselines.md` this week.
3. **A PGO/BOLT'd Rust service of our own building** — the §3 permutation
   row at realistic scale, where we control the symbols and can show both
   halves. This is the number that generalises: every large Rust deployment
   that turns on PGO gets it.
4. `deno` (42 MB, ~monthly), `vector` (56 MB, ~monthly) — same shape as uv,
   smaller reach.
5. Anything under ~10 MB (ripgrep, fd, bat). The §3 table says presage ties
   bsdiff there. Skip.

## 9. Reproducing

```
# nightly pair
for d in 2026-08-27 2026-08-28; do
  curl -sSO https://static.rust-lang.org/dist/$d/rustc-nightly-x86_64-unknown-linux-gnu.tar.xz
  tar xf … rustc/lib/librustc_driver-*.so
done
readelf -sW rd-<date>.so | awk '$4=="FUNC" && $8!=""{print $8}' | sort -u
# canonicalisation probe (measurement only; see §2.2 on back-references)
sed -e 's/Cs[0-9A-Za-z]\{1,\}_/Cs_/g' -e 's/17h[0-9a-f]\{16\}E/17hE/g'

# synthetic: layout permutation, identical source
readelf -sW a | awk '$4=="FUNC" && $8!=""{print $8}' | sort -u > order-a
sort -R order-a > order-b
RUSTFLAGS="-Clink-arg=-fuse-ld=lld \
  -Clink-arg=-Wl,--symbol-ordering-file=order-b \
  -Clink-arg=-Wl,--no-warn-symbol-ordering" cargo build --release
```

Baselines as `baselines.md`: `bsdiff` 4.3 no flags; `xdelta3 -9 -B` at file
size; `zstd -19 --long=31 --patch-from`. bsdiff is the strongest baseline on
every pair here except the permutation synthetic, so quote against it.

## 10. Work items, in dependency order

| # | item | where | gate |
|---|---|---|---|
| R0 | **done** — `elfmod.Stats` wired through `modules.RegistryStats`, so `-v` prints the map's composition | `presage/modules`, `cmd/presage` | `presage diff -v` reports name/content/canonical-equal counts |
| R1 | **done** — `symbols.CanonicalName` erases Rust v0 crate disambiguators, applied in `codeUnits`; legacy and Itanium untouched (a strict no-op unless the name starts `_R`, so Chrome, libxul and Go cannot move) | `presage/symbols/rust.go`, `elfmod/match.go` | `.text` 1.7 % → 90.0 % name-matched, `.bolt.org.text` 4.3 % → 95.4 % (§6.1) |
| R1b | **done** — the content pass takes the nearest twin by position, as the name pass does, instead of the first bucket entry | `elfmod/match.go` | wrong-twin picks on `.bolt.org.text` 29,251 → 1,252 (§6.1) |
| R2 | **done** — mask, retarget, field-fix and map every code window, not the one named `.text` | `elfmod/image.go` (`isCodeWindowName`, `pairCodeWindows`), `equiv.go` (`Windows`), `structure.go` (`codeLookup`), `oracle.go`, `retarget.go`, `elfmod.go`, `encode.go` | **6,358,307 (−47.0 %), 2.38× bsdiff**, round-trip verified; Chrome and libxul mispredict identically, corpus gate green (§6.9) |
| R2b | **done, with R7** — the per-window *function map*: `choices`, `fields` and a `predictionPlan` per window (88,806 functions across four windows on the nightly pair) | `elfmod/match.go`, `structure.go`, `encode.go` | unblocked once R7 named the §6.5 regression's real cause; whether the map pays at all is now R10 |
| R2c | the `.eh_frame`, `.eh_frame_hdr`, `.rodata` and `gcc_except_table` layers keyed per instance, not by name | `elfmod/ehframe.go`, `rodata.go` | BOLT's second metadata set: `.bolt.org.eh_frame` 509,221 + `.bolt.org.eh_frame_hdr` 597,735 + `.bolt.org.rodata` 840,217 mispredicted (§6.4) |
| R8 | **`.eh_frame_hdr` regeneration is gated on its own output** — take the four encoding bytes from the old image, not the prediction | `elfmod/ehframe.go`, one uvarint of plan | `hdr entries` non-zero on the nightly pair; `.eh_frame_hdr` mispredicts ≪ 822,362 (§6.8) |
| R7 | **done** — `retargetEquivalencePrediction` retargeted only the mapped bodies, leaving 39 % of the code with the old image's displacements; `windowSpans` covers the window with the bodies *and the gaps* | `elfmod/retarget.go` | with symbols 9,128,114 → **6,681,766 (−26.8 %)**; chrome and libxul also improve; `TestNoSymbols` byte-identical; gate PASS (§6.10) |
| R11 | **done** — cross-window moves resolve: the oracle accepts a run projection landing in *another* window, and the retarget pass accepts a source from another window (BOLT re-tiers functions across windows on every profile, so a body's old home and new home need not be the same section) | `elfmod/oracle.go`, `retarget.go` | no symbols 6,358,307 → **6,275,392**, with symbols 6,681,766 → **6,597,439** (−1.3 % each), round-trip verified; `TestWindowMigration` locks both halves |
| R9 | `mapTarget` is the one net-negative oracle branch (163 helped, 5,411 harmed): drop mappings that are not canonical-equal from the address lookup while keeping them for body selection | `elfmod/match.go` (`allMapped`), `structure.go` | ~21 KB on the nightly pair; Chrome/libxul unmoved |
| R10 | the function map under-pays: +839,451 B of plan for ~516 K of residual on the nightly pair | `elfmod/structure.go` `predictionPlan.marshal` | with symbols ≤ without symbols (today 6,597,439 vs 6,275,392, a 322,047 gap) |
| R3 | the uv pair in `baselines.md` | `docs/general/baselines.md`, `README.md` | already green: 2,883,460, `cmp`-verified, no symbols |
| R4 | the permutation synthetic as a reproducible fixture | `bench/` | the §3 row, buildable without a network |
| R5 | **done** — the nightly pair in the corpus gate, symbols from its own `.symtab`, parity a self-ratchet at 6,597,439 (no harness number exists; bsdiff is 15,106,624) | `elfmod/corpus_test.go` | round-trips byte-exactly; adds ~6 min to the gate |
| R6 | encode time on a mostly-unmapped image | `eqmatch` params, profile first | under 3 min on the nightly pair; 124 % CPU says most of the 12 m is single-threaded |

**R2 is built** (§6.9): 6,358,307 B, −47.0 % against the baseline and 2.38×
bsdiff, round-trip verified, corpus gate green, and 57 % faster to encode. It
needed no symbols, which is why it works on the stripped binaries the shipping
corpus is made of, and it was the smaller half of what §6.2 originally
proposed — geometry, not correspondence.

R2b was the original R2 and was held behind R7: §6.5 measured the map
costing 207,749 mispredicted bytes on the one window it governed, and
extending a regression to a 67 MB window would have applied it at two and a
half times the scale. R7 showed the regression was the retarget gap, not the
map, so R2b shipped with it. R1/R1b still stand: they make the map
*correct*, which is what made the regression legible in the first place, and
they are what every other pair in the corpus runs on. What remains of the
question is R10 — the map's plan bytes still outweigh the residual they
remove on this pair.

R3 is independent. R5 needs R2. R6 may fall out of R2 — `CodeDefaults` is
`Min 12`, tuned for a `.text` that is almost entirely mapped, and on this
image 84 % of the code is unmapped, so every 12-byte run is a candidate.
R2 touches the wire format (a window count) but not the residual coder or the
decoder contract.
