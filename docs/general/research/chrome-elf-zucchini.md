# Chrome ELF versus Zucchini: an end-to-end probe

Measured 2026-08-27 on linux/amd64. This note tests a narrow question from
`domain-executables.md`: can better ELF modelling improve Zucchini on a real,
public Chrome release pair, rather than only on a synthetic binary?

The answer is **yes**. A small prototype that recognizes x86-64 ELF RELA
addends reduced the final compressed patch by **625,620 bytes (10.62%)**
relative to Zucchini, while reproducing the release target binary exactly.

## Corpus

The pair is an adjacent Google Chrome stable patch release:

| | old | new |
|---|---:|---:|
| version | 151.0.7922.169-1 | 151.0.7922.173-1 |
| `opt/google/chrome/chrome` size | 291,290,440 B | 291,196,232 B |
| binary SHA-256 | `8d873378b45f2dfaf77a9f0801b25dee72a3d5619a2b4b8e84a453b24eecfee3` | `4b82a2d177699d305248705572656f2d8c47116f108b881b66e8b9fadef18fd0` |
| package SHA-256 | `6572478310553cb25fdcb4ba2fb5459b472c2c765f5f68e9837d4964e8a87f1e` | `878e5ab495b8a694980fca61bc09b37e651ccedce2291c73434d16e48a2646fd` |

The packages remain available from Google's public Debian repository:

- [151.0.7922.169-1](https://dl.google.com/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_151.0.7922.169-1_amd64.deb)
- [151.0.7922.173-1](https://dl.google.com/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_151.0.7922.173-1_amd64.deb)

These are stripped PIE ELF x86-64 binaries. The target contains a 26,546,616 B
`.rela.dyn` with 1,105,993 relative relocations, a 1,159,416 B `.eh_frame`,
and a 253,308 B `.eh_frame_hdr`.

This is not Chromium's checked-in `components/zucchini/testdata` Chrome pair.
Those two files are small Windows PE fixtures (798,208 B and 831,488 B), so
they cannot exercise an ELF change. They were still used as a regression
check below.

## The missed structure

Zucchini intends to derive absolute-pointer locations from
`R_X86_64_RELATIVE` relocations. The current ELF reader, however, has an
explicit `TODO` to process `r_addend` in RELA sections. It treats `Elf_Rela`
as though it were its `Elf_Rel` prefix:

1. It reads `r_offset`, correctly identifying the runtime relocation slot.
2. It then treats that slot as the body of an absolute-address reference.
3. That is correct for REL, where the addend lives in the relocated slot.
4. It is wrong for RELA, where the on-disk slot is zero and the address is in
   the relocation entry's `r_addend` field.

On both Chrome binaries Zucchini consequently logs that it removed every
candidate as untranslatable. Its absolute-reference pool contains zero usable
locations despite the binary carrying more than 1.1 million explicit
addresses. The relevant current source is
[`reloc_elf.cc`](https://chromium.googlesource.com/chromium/src/+/refs/heads/main/components/zucchini/reloc_elf.cc)
and the pool is populated in
[`disassembler_elf.cc`](https://chromium.googlesource.com/chromium/src/+/refs/heads/main/components/zucchini/disassembler_elf.cc).

The prototype scans each validated `SHT_RELA` section, selects relative
relocations, and adds the file offset of `r_addend` to the existing abs32/64
reference-location set. No patch-format change is needed: Zucchini's existing
absolute-reference reader, target association, delta stream, and writer do the
rest. On the old binary this changes the pool from:

```text
#locations=0, #targets=0
```

to:

```text
#locations=1,105,974, #targets=418,726
```

The prototype is deliberately minimal. It still visits the zero relocation
slots and emits the old warning before those invalid candidates are removed.
A production change should distinguish REL and RELA in the section metadata
and avoid adding the wrong candidates in the first place.

## Result

Zucchini patch format 2.0 was used for both runs. Raw patches were compressed
with XZ Utils 5.8.1 using `xz -9e -T0`, since Zucchini patches are designed for
an external terminal compressor.

| method | patch bytes | target | change versus Zucchini |
|---|---:|---:|---:|
| Zstd 1.5.7 `--patch-from`, level 3, long window | 81,525,078 | 28.00% | — |
| Zstd 1.5.7 `--patch-from`, level 19, long window | 45,538,524 | 15.64% | — |
| Zucchini, raw | 25,027,082 | 8.60% | baseline |
| **RELA-aware Zucchini, raw** | **20,102,147** | **6.90%** | **−4,924,935 (−19.68%)** |
| Zucchini + XZ | 5,889,352 | 2.02% | baseline |
| **RELA-aware Zucchini + XZ** | **5,263,732** | **1.81%** | **−625,620 (−10.62%)** |

The generic Zstd patches and both Zucchini patches were applied and matched
the target SHA-256 exactly.

The baseline Zucchini generator took 185.4 s and approximately 4.05 GiB peak
RSS. The prototype generator took 146.8 s and approximately 4.09 GiB, but the
two executables were compiled in different environments, so generation time
is **not** an attributable improvement. Patch application was 3.70 s / 691 MB
for baseline and 3.90 s / 696 MB for the prototype. The practical conclusion
is that this change buys size without materially changing decoder cost.

## Regression check

On Chromium's public `chrome64_1.exe` → `chrome64_2.exe` PE fixture, the
baseline and prototype each generated the same 108,233 B raw patch in 0.30 s.
The patch files were byte-identical, and the prototype reconstructed the target
exactly. This is the expected no-op result because the new path is ELF-only.

## What this validates

This is a useful public result for the general ELF project for three reasons:

1. It improves the incumbent algorithm on the incumbent's actual product, not
   a toy C program.
2. The improvement comes from *regenerating address movement*, the same
   predict-then-correct mechanism as the proposed framework, not from swapping
   the terminal compressor.
3. It is only the smallest missing ELF feature. It requires no symbol files,
   no function matcher, no new instruction decoder, and no wire-format change.

It also puts a bound on the first milestone. RELA support alone is a real
10.6% compressed win, but not a new order of magnitude. A substantially larger
result must combine it with function correspondence and derived-table
regeneration.

## Next experiments, in order

1. **Make RELA handling production-quality.** Carry REL versus RELA in
   `SectionDimensionsElf`; read only addends whose relative target maps into a
   loadable section; cover mixed REL/RELA, signed addends, 32/64-bit files,
   malformed sections, and exact patch application in unit tests.
2. **Regenerate `.eh_frame_hdr`.** It is a sorted table of `(function start,
   FDE)` relative pointers. On this pair a generic section-only patch costs
   213,963 B for the 253,308 B target section, making it the clearest next
   compact table operation.
3. **Normalize `.eh_frame`.** Its section-only patch costs 268,519 B for a
   1,159,416 B target. FDE boundaries also provide a reliable function-start
   map in the stripped client binary.
4. **Use publisher-side symbols to impose function correspondence.** Official
   Linux Chrome debug-info archives are public, so the encoder can use exact
   old/new symbols while the decoder remains symbol-free. This directly tests
   whether a shipped function permutation beats Zucchini's suffix-array
   discovery after Chrome's orderfile/PGO reordering.
5. **Replace the naive x86 opcode scan inside known function ranges.** This
   should reduce false reference candidates and expose RIP-relative data
   references more reliably; measure it only after the function map is in
   place.

The most defensible public target is now: **beat 5,263,732 B on this exact
pair, with byte-exact application, by composing RELA, unwind-table, and
publisher-symbol function modelling.**

Follow-up: [`chrome-elf-predictor-spike.md`](chrome-elf-predictor-spike.md)
first tests the publisher-symbol function model independently, then combines
it with the large equivalences from this patch. The independent result is
7.59 MB with Zstd; charged per-function selection between both predictors
reaches 4.60 MB with XZ for byte-exact `.text`. That is a promising 12.61%
advantage over this whole-image result under a favorable `.text`-only scope,
not yet a complete-patch win.
