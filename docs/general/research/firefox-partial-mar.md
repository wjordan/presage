# presage against a shipped Firefox patch

Status: measured, 2026-08-29. Companion to `chrome-elf-whole-image.md`, which
measures the same codec against Zucchini on Chrome. This one supplies what
`bsdiff6-spike.md` said `libxul` did not have: **a real shipped patch as the
denominator**, downloaded from Mozilla's release archive rather than
reconstructed by running a diff tool locally.

Everything below decodes. Both official patches were applied to the base we
measure against, and the result checked byte for byte against the target we
measure against, so the endpoints are not merely assumed to be the ones
Mozilla diffed.

## Bottom line

`libxul.so`, Linux x86-64 en-US, against what Mozilla actually shipped:

| | 154.0 → 154.0.1 (dot release) | 153.0.4 → 154.0.1 (release cycle) |
|---|---:|---:|
| Mozilla shipped (mbsdiff, XZ in the MAR) | 10,779,184 | 19,777,036 |
| Zucchini + `xz -9e` | 9,544,652 (88.5 %) | 17,554,636 (88.8 %) |
| presage, own matcher | 4,780,572 (44.4 %) | 13,484,956 (68.2 %) |
| **presage** | **4,063,404 (37.7 %)** | **11,689,952 (59.1 %)** |

2.65× and 1.69× smaller than the shipped patch. The gap closes as the pair
gets further apart, which is the same shape the Chrome pairs showed: what
presage predicts is *displacement*, and a full release cycle recompiles enough
bodies that the residual stops being displacement and starts being new code.

## 1. What Firefox ships, as measured

Read off the shipped artefacts with `bench/marprobe/mar.py`, not from docs.

**The container.** MAR (`MAR1`), all fields big-endian: a 4-byte offset to the
index, an 8-byte total size, a signature block, then typed additional sections
— type 1 carries `firefox-mozilla-release\0154.0.1` — then content, then an
index of `(offset, size, flags, name)`. Every entry is compressed on its own;
all 54 entries of the 154.0.1 complete MAR and all 31 of the 154.0 → 154.0.1
partial are XZ.

**The complete MAR** is the whole installation: 89,503,671 B for linux-x86_64
en-US 154.0.1, of which `libxul.so` is 55,674,596 (185,617,424 B raw).

**The partial MAR** is `updatev3.manifest` plus one `<file>.patch` entry per
changed file. The manifest is a line per action — `add`, `patch`, `remove` —
so a partial is a per-file operation list, not a whole-image diff. Mozilla
publishes partials from four prior versions to 154.0.1 (154.0, 153.0.4,
153.0.3, 153.0.1); Balrog serves one only on an exact buildID match, and falls
back to the complete MAR otherwise.

**The per-file patch** is `MBDIFF10` — mbsdiff, Mozilla's variant of Percival's
bsdiff. 32-byte big-endian header (tag, source length, source CRC, target
length, then control/diff/extra block lengths), then control triples
`(x, y, z)`: add `x` bytes of the diff block to `x` bytes of the source, copy
`y` bytes of the extra block, seek the source forward by `z`. The blocks are
stored raw — the MAR entry's XZ is the only compression, which is why
`libxul.so.patch` is 187,808,908 B uncompressed and 10,779,184 B stored.

The header's source CRC is **CRC-32/BZIP2** (non-reflected, polynomial
0x04C11DB7), not the reflected CRC-32 of zlib. Identified on
`libmozsandbox.so` (0xb4b31051) after the zlib value disagreed on `libxul.so`
and briefly looked like a corpus mismatch.

**Zucchini is in-tree but not on this channel.** The shipped 154.0.1 partials
are `MBDIFF10`. `taskcluster/docs/partials.rst` explains why: the
`zucchini_partial_rollout` transform keys off a `LEGACY_PARTIALS_PROJECTS`
set, "mozilla-central, mozilla-beta (nightly): uses `partials-zucchini`;
mozilla-release, ESR channels: uses legacy `partials`", and "as zucchini
partials are validated on nightly, the rollout will expand to other channels
by removing entries". So the Zucchini row above is the baseline Firefox is
moving *to*, and the mbsdiff row is what users get today.

## 2. Why the comparison is valid

The failure mode this guards against is the one recorded for Chrome: binaries
that look like the shipped ones and are not.

1. `libxul.so` extracted from the 154.0 complete MAR is SHA-256 identical to
   the corpus copy taken from the release tarball.
2. Both official patches were applied with `bench/marprobe/mbspatch.py` and
   both reproduce `libxul-154.0.1.so` byte for byte — 182,621 control triples
   from 154.0, 608,620 from 153.0.4.
3. Symbols come from Mozilla's own `crashreporter-symbols.zip` for each
   release, selected by converting the ELF build ID to a Breakpad debug ID and
   matching the directory name, so the symbol file provably belongs to the
   binary being patched.

Point 3 is also the answer to "does presage need something Mozilla doesn't
have". The shipped `libxul.so` is stripped; presage's function map comes from
the Breakpad `FUNC` records Mozilla already publishes for every release
(275,358 functions for 154.0, 273,916 for 153.0.4). Nothing here needs an
unstripped build that only Mozilla's build system sees.

## 3. What the patch contains

The plan is self-contained — it carries its own equivalence map, so applying a
presage patch needs the old file and the patch, nothing else:

| stream (standalone xz) | 154.0 → 154.0.1 | 153.0.4 → 154.0.1 |
|---|---:|---:|
| equivalences | 527,328 | 1,834,388 |
| structure | 202,836 | 283,384 |
| choices | 9,424 | 15,772 |
| relocations / eh_frame / rodata | 828 | 1,044 |
| **plan total** | **1,178,352** | **3,271,724** |
| correction | 2,885,052 | 8,418,228 |
| prediction correct | 96.968 % | 92.890 % |

The two presage rows in the bottom line differ only in where the equivalence
runs come from. The lower row runs Zucchini first and reads the runs out of
its patch; the "own matcher" row uses `-native-equivalences` and depends on no
external tool. The matcher is worth 17.6 % of the patch on the dot release and
15.4 % on the release cycle — a real gap, and the largest single item on the
list of things worth improving, but not the difference between winning and
losing.

## 4. What it would mean for the update

Re-coding only `libxul.so` and leaving all 30 other entries as Mozilla built
them:

| | shipped | with presage | |
|---|---:|---:|---:|
| 154.0 → 154.0.1 | 12,491,548 | 5,775,768 | −53.8 % |
| 153.0.4 → 154.0.1 | 24,424,672 | 16,337,588 | −33.1 % |

`libxul.so` is 86.3 % and 81.0 % of those two partials, so it is the only
entry worth attacking. The next-largest is `browser/omni.ja` at ~10 % — a zip
archive, a different domain, and not something this codec addresses.

## 5. Cost

Generation, single host, 186 MB pair: Zucchini 88.8 s and 2.2 GB resident;
presage 131 s wall (71 s of it in xz) for the release-cycle pair, 90 s for the
dot release. Both are well inside what a per-release task already spends —
Mozilla's own partials task generates four of these in parallel per platform
and locale. Decode cost is not measured end to end by this harness; the
correction decoder alone is a byte loop at 24.9 ms on a file this size
(`bsdiff6-spike.md` §2), but the prediction replay is the larger half and is
unmeasured.

## 6. Reproducing

```sh
B=https://archive.mozilla.org/pub/firefox
curl -O $B/releases/154.0.1/update/linux-x86_64/en-US/firefox-154.0-154.0.1.partial.mar
curl -O $B/releases/154.0/update/linux-x86_64/en-US/firefox-154.0.complete.mar
curl -O $B/candidates/154.0-candidates/build1/linux-x86_64/en-US/firefox-154.0.crashreporter-symbols.zip

python3 bench/marprobe/mar.py firefox-154.0-154.0.1.partial.mar        # index, largest first
python3 bench/marprobe/mar.py firefox-154.0.complete.mar extract libxul.so libxul-154.0.so
python3 bench/marprobe/mar.py firefox-154.0-154.0.1.partial.mar extract libxul.so.patch p
python3 bench/marprobe/mbspatch.py libxul-154.0.so p applied.so        # must equal the target

go run ./bench/elfpredict -old libxul-154.0.so -new libxul-154.0.1.so \
  -old-debug libxul-154.0.funcs -new-debug libxul-154.0.1.funcs \
  -native-equivalences -reference 10779184 -rungs corrected-fields
```

`*.funcs` is the `FUNC` lines of the `libxul.so.sym` in the symbols zip whose
directory name is the binary's build ID in Breakpad form (uint32 LE, uint16
LE, uint16 LE, then 8 bytes verbatim, then the age digit).

Measured with codec identity `src-8c582cfc31650c02`, harness
`src-c6b0534e337dd5f6` — the tree with `delta/modal.go` in it.

## 7. What this changes

Nothing about the codec. It replaces a projected denominator with a shipped
one and, in doing so, moves the `libxul` claim from "3.09× bsdiff 4.3, a tool
nobody ships unmodified" to "2.65× the patch Firefox served for this pair last
week". It also says which baseline matters next: Zucchini is only 11 % ahead
of mbsdiff on these two pairs, so the interesting comparison stays Zucchini
even after Mozilla's rollout reaches the release channel.

The release-cycle number is the one to watch. 59.1 % against 37.7 % on a dot
release is the same falloff seen on Chrome, and points at the same residual —
recompiled bodies, not displacement. Any further work aimed at real update
traffic should be measured on the 153.0.4 pair, not the 154.0 one.
