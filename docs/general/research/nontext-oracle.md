# The residual outside `.text`, bounded by oracle

Status: measurements of 2026-09-01 on container v5 (`8e7dbbd`); the
fixes they led to shipped in v6 (`elf-module.md` status ledger).

## 1. The instrument

`presage diff -v` counts mispredicted bytes by section. A count is not a
cost: a uniformly shifted table costs the coder almost nothing per wrong
byte and new content costs nearly a byte each. So each section was priced
by *oracle*: the encoder overwrote the prediction of one section with the
target's bytes before coding the residual, and the patch shrank by exactly
what that section's mispredictions were costing. The patch cannot apply —
it is a bound, not a codec — and the plan is unchanged, so the delta is the
residual's alone. A range-restricted form of the same hook (a file of
`lo hi` offsets) priced one *cause* inside a section.

## 2. The bounds

libxul 154.0 → 154.0.1, 2,866,328 B, residual 1,950,500 of which `.text`
mispredicts 1,383,378 bytes:

| section revealed | wrong bytes | patch saved | per wrong byte |
|---|---:|---:|---:|
| `.eh_frame_hdr` | 1,308,676 | 11,151 | 0.009 |
| `.data.rel.ro` | 1,109,576 | 267,990 | 0.24 |
| `.eh_frame` | 535,051 | 293,551 | 0.55 |
| `.rodata` | 390,097 | 298,904 | 0.77 |
| all four | 3,343,400 | 871,526 | |
| `.relr.dyn` | 39,800 | 26,295 | 0.66 |

Chrome 151.0.7922.169 → .173, 2,376,189 B:

| section revealed | wrong bytes | patch saved |
|---|---:|---:|
| `.eh_frame_hdr` | 134,305 | 9,556 |
| `.rodata` | 248,444 | 151,023 |
| `.eh_frame` | 70,961 | 41,339 |
| all three | 453,710 | 201,934 |

The bounds are additive to within 70 bytes. The second-largest error on
libxul was worth 11 KB and the fourth-largest 299 KB: byte counts rank the
work backwards.

## 3. The causes, and what each was worth

**`.data.rel.ro`, libxul.** Firefox links with packed RELR relocations, so
the slots hold pre-relocated absolute addresses and every one that names a
moved function changes. The module regenerated `.rela.dyn` only. 97.7% of
the wrong words are RELR slots, and 97.6% of those are fixed exactly by
projecting the old slot through the section map and writing the pointer
oracle's answer for the old value. Regenerating the new `.relr.dyn` from
the same projection is a loss (39,800 → 78,967 wrong bytes): the byte
prediction keeps that section. Built: `relr.go`, six plan bytes,
**−257,010 B**.

**`.eh_frame`, both.** 96% of libxul's wrong bytes (513,723 of 535,051,
272,545 FDEs) are one field: `cie_ptr`, the distance back from an FDE to
its CIE, which is position-dependent and which `retargetEhFrame` never
wrote, so every FDE that moved carried the old distance. Rewritten from
the projected entry and CIE: zero plan bytes, **−260,874 B libxul,
−19,698 B Chrome**, 99.2% and 97.0% of the bound. The rest is retarget
error on `initial_location` (Chrome 22,598 B over 6,055 FDEs) and
recompiled CFA programs.

**`.eh_frame_hdr`, both.** The regenerated header was built from the old
FDE set projected forward, so an FDE added or removed upstream, and 617
pairs of old FDEs projecting to one address, put its count and its
pointers off by whole FDE widths, and a packed sorted table then shifts
everything after the first error. Rebuilding it from the *finished*
`.eh_frame` is byte-identical on every image tried, so the section is now
masked out of the residual and rebuilt on the decoder after the
correction (`presage.Finaliser`, its first implementer). **−11,151 B
libxul, −9,520 B Chrome**, the whole bound.

**`.rodata`, libxul.** Not the 143,862 dropped jump-table candidates
(144 wrong bytes among them: they are 16-bit ICU and font data that pass
the `.text`-band signature, and their `Keep` bits cost ~700 B) but the 201
kept spans: 335,856 wrong bytes, 250,581 B. Two spans of 43,128 and
70,165 entries sit in a 453 KB hole the byte matcher cannot match —
every entry is `target − entry_addr`, so the whole region churns when
`.text` moves — and the retargeter places each entry by projecting its
old offset, which extrapolates a flat shift across a hole whose true shift
walks +16 → +528 → +316. 61 of 113,293 entries landed in the right slot.
The region is per-function switch tables in link order (7,775 → 7,805
tables, owner sequence preserved); placing each table by a cursor that
advances table by table, with a near-all-zero correction column, puts
78,134 of 98,455 aligned entries exactly and the restricted oracle prices
it at **140,013 B**, headroom 250,581 across all kept spans. Chrome has the
same mechanism with tables a sixth the size, so projection stays close and
the model already recovers 88% (kept-span bound 72,410 B). The other half
of Chrome's `.rodata` (72,603 B) is content churn — ICU and skia tables,
pixel data; 2,024 of the wrong bytes are strings.

## 4. The rule this leaves

Price a section by oracle before modelling it. The four fixes above cost
about 200 lines and moved libxul 18.4%; the shelf in
`chrome-elf-whole-image.md` §11.4, priced by the same discipline, moves
Chrome 2.4% for a far larger port. A count of wrong bytes would have sent
the work to `.eh_frame_hdr` first.
