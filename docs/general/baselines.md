# The README table: what each number is

Provenance for the headline comparison in `README.md`. presage column
measured at container v6: the C++ rows on 2026-09-01 (RELR relocation slots,
exact FDE `cie_ptr`, `.eh_frame_hdr` rebuilt after the residual, the CM
coder over bit-history states with an SSE chain, `.rodata` switch tables
placed by cursor), the Go rows
on 2026-08-31 (derived function map, displacement columns, compact CM coder
over correction and plan streams, parallel decode) and re-confirmed
unchanged at v6; baseline columns measured 2026-08-29. All on this machine
unless a source doc is named. All sizes in bytes. The resource table's
C++ rows predate v6; at v6 the stronger coder costs apply time (libxul
1.73 s and Chrome 3.06 s single runs under load 10, both under the 3.84 s
Zucchini bar).

## The pairs

| row | old | new | size |
|---|---|---|---:|
| one-line change | `bench/out/bin127/v1-F2` | `v2c-F2` | 29,995,271 |
| prometheus patch release | `corpus127/prometheus/3.13.1` | `3.13.2` | 93,741,283 |
| Chrome | `chrome-151.0.7922.169` | `.173` | 291,290,440 |
| libxul | `libxul-154.0.so` | `libxul-154.0.1.so` | 185,738,000 |

## The columns

**presage.** Every row is the shipped codec: `presage diff`, applied back
with `presage patch` and `cmp`-verified. The Go rows take no flags
(`go-module-results.md` §"Where the numbers come from": the research harness
measures the same rows at 1,828 / 71,192 because it lacks the far pieces and
the modal correction, and ships plan and correction as two separate `xz -9e`
streams). The C++ rows give the encoder symbols, which the patch does not
carry:

```
presage diff -symbols ~/.cache/presage-chrome-zucchini/symbols-151.0.7922.169/debug-info/chrome.debug,\
                      ~/.cache/presage-chrome-zucchini/symbols-151.0.7922.173/debug-info/chrome.debug \
    chrome-151.0.7922.169 chrome-151.0.7922.173 -o chrome.psg
presage diff -symbols ~/.cache/presage-pairs/libxul-154.0.funcs,~/.cache/presage-pairs/libxul-154.0.1.funcs \
    libxul-154.0.so libxul-154.0.1.so -o libxul.psg
```

Encoder trial pricing uses a zstd proxy above a 128 MiB target and the real
compressor below it (`PRESAGE_SIZE_PROXY` overrides); the prometheus row is
exact-priced by that default. The harness's own best on these pairs, with the
same matcher and two `xz -9e` streams, is
2,617,700 / 3,632,264; the Zucchini-stream runs the earlier drafts of this
table quoted were 2,634,264 / 4,063,404 (`elf-module.md` §0,
`research/matcher-chrome.md`).

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
| one-line change, 30 MB | 1,100 | 173,060 | 150,475 | 1,390,889 | 538,493 |
| prometheus 3.13.1 → 3.13.2 | 74,636 | 3,031,380 | 2,691,644 | 11,068,506 | 8,479,550 |
| Chrome 151.0.7922.169 → .173 | 2,305,394 | 5,263,732 | 18,599,806 | 40,102,887 | 45,538,524 |
| libxul 154.0 → 154.0.1 | 2,148,134 | 9,544,652 | 12,348,560 | 24,510,737 | 26,326,367 |

Two checks that the rows are scoped consistently: `bsdiff` on the one-line
pair reproduces `go-module-results.md`'s 150,475 exactly, and the ordering
bsdiff < xdelta3 < zstd `--patch-from` holds on all four rows.

## Resource comparison

Measured on 2026-08-31 on the same 24-core Linux 6.17 x86-64 host. Values are
medians of three serial warm-cache runs; elapsed time and maximum RSS come
from `/usr/bin/time`. Outputs went to tmpfs to minimise storage variance, and
every apply was compared byte-for-byte with the target.

| pair | tool | encode s | encode RSS KiB | apply s | apply RSS KiB |
|---|---|---:|---:|---:|---:|
| prometheus 3.13.1 → 3.13.2 | presage | 4.26 | 970,480 | 0.75 | 401,164 |
| prometheus 3.13.1 → 3.13.2 | Zucchini + XZ | 21.34 | 1,100,484 | 0.45 | 203,012 |
| prometheus 3.13.1 → 3.13.2 | bsdiff | 36.79 | 825,148 | 0.49 | 191,996 |
| prometheus 3.13.1 → 3.13.2 | xdelta3 | 8.49 | 705,716 | 0.38 | 115,244 |
| prometheus 3.13.1 → 3.13.2 | zstd `--patch-from` | 21.13 | 431,124 | 0.13 | 187,668 |
| libxul 154.0 → 154.0.1 | presage | 30.44 | 1,801,300 | 1.57 | 395,400 |
| libxul 154.0 → 154.0.1 | Zucchini + XZ | 70.00 | 2,256,308 | 0.97 | 394,180 |
| libxul 154.0 → 154.0.1 | bsdiff | 79.46 | 1,633,536 | 1.18 | 374,780 |
| libxul 154.0 → 154.0.1 | xdelta3 | 21.01 | 1,320,848 | 0.86 | 205,668 |
| libxul 154.0 → 154.0.1 | zstd `--patch-from` | 34.18 | 782,400 | 0.28 | 366,744 |

Tool versions were presage at this document's revision, Zucchini 2.0, bsdiff
4.3, xdelta3 3.2.0, zstd 1.5.7 and XZ Utils 5.8.1. Commands match the size
table above. Zucchini's figures include `xz -9e -T0`: time is the sum of the
two serial stages and RSS is the larger peak. xdelta3 apply uses the same
source-window size as encode. The C++ presage encoder alone reads the symbol
files; no apply command does.

## What the table does not say

For `libxul` the bsdiff column is a locally-run tool, not what Mozilla ships.
The shipped patch is an mbsdiff at 10,779,184, whose blocks the MAR container
compresses with XZ rather than bzip2, so it beats local bsdiff; presage is
3.8× smaller again. See `research/firefox-partial-mar.md`.

On the one-line row the shipped patch is 1,100 B, of which 134 B is the
container header and 100 B is random (the two build-ID notes); the FIPS sum
is recomputed by the decoder.
`research/one-liner-floor.md` has the full ledger.
