# Environment priors: negative result

*2026-08-27. Question: can a decoder's ambient environment — other binaries
already on the host, a module/build cache — serve as the reference set, so a
cold target (one with no previous release) receives a small patch instead of a
20 MB blob? Measured, then dropped. Don't re-run this.*

## The answer

**No. Ambient environment mining is worth 1.55× on the cold blob; a perfect
pool is worth 2.9–3.2×, or ~2.5× once the link plan is charged for.** The
`delink` result (66 % cross-project reuse of function text,
`priors-and-regeneration.md` §7) does not carry to a whole binary, because
text is only 39 % of the compressed artefact and the other regions behave
differently.

Cold blob of a stripped 102 MB prometheus (go1.26.6), `zstd -19`, decomposed:

| region | raw | compressed | share | vs pool, raw | vs pool, **delinked** |
|---|---:|---:|---:|---:|---:|
| `.text` | 43.4 MB | 9.51 MB | 39 % | 1.85× | **6.96×** |
| `.gopclntab` | 37.3 MB | 7.89 MB | 32 % | 2.17× | regenerated, see below |
| `.rodata` | 20.6 MB | 6.38 MB | 26 % | 1.21× | **1.28×** |
| everything else | | 0.40 MB | 2 % | | |

Pool = `promtool`: same repo, same commit, same toolchain, same dependency
closure — more generous than any real environment. Both sides canonicalised
(PC-relative fields zeroed for text, in-image pointers for rodata), matched by
`zstd -19 --long=27` so no chunk boundary or unit assumption is involved.

**Canonicalisation is worth 3.8× on code and nothing on data.** `.rodata` is
26.6 % printable strings and 73 % type descriptors, gcbits, funcdata and itabs;
the strings are program-specific (metric names, error text, k8s/OpenAPI type
names) and absent from any other binary. Neutralising the 32-bit
module-relative offsets in type descriptors as well moves rodata coverage from
25 % to 53 % and the end-to-end figure from 2.85× to 3.21× — the honest
bracket, and the ceiling.

End to end, covered units zeroed plus the regenerable pclntab:

| pool | cold blob | gain | text | pcln | fname | rodata |
|---|---:|---:|---:|---:|---:|---:|
| promtool (same build), conservative | 8.48 MB | 2.85× | 87.1 % | 84.4 % | 68.5 % | 25.3 % |
| promtool, permissive rodata canon | 7.52 MB | 3.21× | 87.1 % | 84.4 % | 68.5 % | 52.6 % |
| alertmanager (other project, same go1.26.6) | 15.87 MB | 1.52× | 32.6 % | 27.0 % | 17.8 % | 6.4 % |
| 22 Go binaries actually installed on a dev host | 15.61 MB | 1.55× | 33.9 % | 28.3 % | 24.5 % | 8.8 % |
| none | 24.14 MB | 1.00× | | | | |

Not charged above: the link plan. 8 B × 119 537 functions ≈ 1 MB before
run-encoding, against an 8.48 MB best case.

## Two things worth keeping

- **pc-value streams are 85.1 % *byte-exactly* reusable across binaries**, and
  `funcnametab` 68.5 %. No canonicalisation needed: pc tables are varint deltas
  from function entry, so they are position-independent by construction. The
  rest of `.gopclntab` (ftab, `_func`, cutab, filetab — 18 MB raw) is derived
  content the codec already regenerates. pclntab was never the obstacle.
- **Ambient coverage saturates at four binaries.** Cumulative reloc-modulo text
  coverage of prometheus as a dev host's binaries are added: 19.9 % (1),
  29.4 % (3), 33.3 % (4), 33.9 % (21). The last 17 binaries — 118 MB of text —
  bought 0.6 points. Coverage is close to all-or-nothing per module (`x/net`
  99.6 %, protobuf 100 %, `aws-sdk-go-v2` 42 %, then a cliff to 2–8 %), keyed by
  toolchain version, not by project. Scanning a filesystem is the wrong
  mechanism; naming `(module, version, toolchain)` is the right one — and it
  still only reaches the 2.9–3.2× row.

## What isn't the blocker, and what to do instead

Reference negotiation is free in this store model: the environment id goes in
the object path (`patches/<from>-<to>-<envid>`, falling back to
`patches/<from>-<to>`), the publisher pre-generates for the few env ids in a
fleet, and targets already poll. No extra round trip, no server logic, works on
s3/https/file/ssh unchanged. The idea dies on the ratio, not the protocol.

The cold path is expensive only because the target lacks *the previous release
of this program* — the one artefact that shares rodata, and worth 250×. Solve
it by distribution, not by codec: have a scaling-out host fetch the previous
release from a neighbour on the LAN, then take the ordinary patch from the
origin. Same codec, no pool format, no index, no side-channel surface.

Also settled by the above: a plan may never name arbitrary local paths. Making
decode depend on host contents turns every target into a file-existence oracle
(the cloud-dedup side channel), and `ErrPredictionDiverged`'s chunk index would
make it a byte-level one. References come only from explicitly enrolled
material.

Measurement tool: `~/.cache/presage-corpus/regions/`, over the corpus in
`~/.cache/presage-corpus/bin/`, outside the repository.
