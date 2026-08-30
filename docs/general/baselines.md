# The README table: what each number is

Provenance for the headline comparison in `README.md`. Measured 2026-08-29 on
this machine unless a source doc is named. All sizes in bytes.

## The pairs

| row | old | new | size |
|---|---|---|---:|
| one-line change | `bench/out/bin127/v1-F2` | `v2c-F2` | 29,995,271 |
| prometheus patch release | `corpus127/prometheus/3.13.1` | `3.13.2` | 93,741,283 |
| Chrome | `chrome-151.0.7922.169` | `.173` | 291,290,440 |
| libxul | `libxul-154.0.so` | `libxul-154.0.1.so` | 185,738,000 |

## The columns

**presage.** Go rows are the shipped codec, `presage diff` applied back and
`cmp`-verified (`go-module-results.md` §"Where the numbers come from": the
harness measures the same rows at 1,828 / 71,192 because it lacks the far
pieces and the modal correction, and ships plan and correction as two
separate `xz -9e` streams). Chrome and libxul have no shipped module yet, so
those rows are the harness's `xz -9e` streams: Chrome from
`research/chrome-elf-handoff.md`; libxul from
`research/firefox-partial-mar.md`.

**Zucchini.** Patch format 2.0, compressed `xz -9e -T0`, since Zucchini
expects an external terminal compressor. The Chrome figure is the RELA-aware
variant, which is the stronger baseline; stock Zucchini on that pair is
5,889,352. Go and libxul figures from the documents above.

**bsdiff.** `bsdiff 4.3`, no flags, bzip2 blocks internal to the patch.

**xdelta3.** `xdelta3 -9 -B 268435456 -s old new out` (`-B 536870912` for
Chrome). The window matters: the default 64 MB source window does not cover
any of these files, and leaving it at the default overstates the patch by
around 40% on the smaller pairs. Earlier drafts of the README quoted
`xdelta3 -9` at defaults, which flattered presage; these numbers do not.

**zstd `--patch-from`.** `zstd -19 --long=31 --patch-from=old new`.

## Results

| pair | presage | Zucchini | bsdiff | xdelta3 | zstd `--patch-from` |
|---|---:|---:|---:|---:|---:|
| one-line change, 30 MB | 1,202 | 173,060 | 150,475 | 1,390,889 | 538,493 |
| prometheus 3.13.1 → 3.13.2 | 70,195 | 3,031,380 | 2,691,644 | 11,068,506 | 8,479,550 |
| Chrome 151.0.7922.169 → .173 | 2,634,264 | 5,263,732 | 18,599,806 | 40,102,887 | 45,538,524 |
| libxul 154.0 → 154.0.1 | 4,063,404 | 9,544,652 | 12,348,560 | 24,510,737 | 26,326,367 |

Two checks that the rows are scoped consistently: `bsdiff` on the one-line
pair reproduces `go-module-results.md`'s 150,475 exactly, and the ordering
bsdiff < xdelta3 < zstd `--patch-from` holds on all four rows.

## What the table does not say

For `libxul` the bsdiff column is a locally-run tool, not what Mozilla ships.
The shipped patch is an mbsdiff at 10,779,184, whose blocks the MAR container
compresses with XZ rather than bzip2, so it beats local bsdiff; presage is
2.65× smaller again. See `research/firefox-partial-mar.md`.

On the one-line row the shipped patch is 1,202 B, of which 166 B is the
container header (three 32-byte hashes) and 132 B is random (two build IDs and
the FIPS hash); `go-module-results.md` §"Where the numbers come from" has the
rest of the ledger.
