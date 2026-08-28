# Domain study: container images, OS/package updates and archive formats

Research notes for the generalised predictive codec (2026-08-27). Question:
is "container images / package updates / archives" a domain where a
structure-aware *predictor* beats general-purpose compression and delta by a
large margin, the way the Go-aware transform does for Go binaries
(`docs/DESIGN.md` §1: 24–68× over bsdiff)?

Sourcing rule: every number carries an inline URL to a primary source unless
marked *(secondary)* or *(unverified)*. Where our own earlier documents already
cover a system at the transport/metadata level (`docs/research/update-systems.md`
§2, `docs/research/binary-delta.md` §2), this document cross-references them and
adds only what matters for *prediction inside archives*. Search budget for this
session ran out part-way; a handful of items are flagged accordingly rather than
guessed.

---

## 0. Summary

1. **The domain is real but the win is split in two, and the two halves are
   different problems.** Half of the bytes in a container image that a delta
   tool sees are *opaque*: the layer is a gzip tarball, and 23 % of the bytes
   *inside* layers are themselves zip/gzip/tar/xz archives (Zhao et al.,
   CLUSTER'19, §4.1). No predictor sees through that without first solving
   *deterministic recompression*. The other half is the same executable-churn
   problem binsync already solves: executables/objects/libraries are 37 % of
   in-layer bytes, and redundant ELF files are 73 % of the redundant
   executable capacity.
2. **Deterministic recompression is a solved problem for zlib and an unsolved
   one for everything else.** preflate-rs reconstructs zlib output at every
   level with 0.01 % correction overhead, libdeflate/zlib-ng/miniz at ~1 %
   (§3.2). puffin sidesteps determinism entirely by shipping the Huffman
   tables (§3.1). But the encoder that produces *almost every OCI layer* is
   Go's `compress/gzip` (moby, buildkit and containerd all call
   `gzip.NewWriter` from the standard library, §3.4) and no published
   recompressor models Go's deflate. zstd explicitly does not guarantee
   identical output across versions (§3.3). This is the single most
   important finding: **a Go-deflate predictor is a gap nobody has filled,
   and binsync is a Go project.**
3. **Published ratios for archive-aware delta are 1.5–6× over bsdiff, not
   30×.** Google Play file-by-file (uncompress → bsdiff → recompress):
   Netflix 7.7 MB → 1.2 MB (6.4×), Kindle 19.1 → 8.4 MB (2.3×), Gmail
   7.6 → 7.3 MB (1.04×) (§2.9). Zucchini over bsdiff on Firefox partials:
   1.3–2.6× (§2.6). MSDelta's PE transforms: "50–70 % smaller" (§2.7). These
   are all *first-order-limited*: once the compression wrapper is peeled,
   what remains is genuine change plus executable second-order churn, and
   the latter is exactly what binsync's function-table alignment attacks.
4. **The fleet-level delta systems that ship today do not look inside
   anything.** Balena: rsync/librsync over the whole image, "10–70×"
   marketing claim, worst case a full pull (§1.6). Mender: xdelta3 on raw
   partition images. RAUC: 4 KiB SHA-256 block index, ~10 % of bundle for a
   one-package change. SWUpdate: zchunk. OSTree: bsdiff per same-named
   object ≤ 128 MB, xz parts. `containers/oci-delta` on Fedora bootc
   41 → 42: 555 MB delta for a 999 MB image (44 % saved). `tar-diff`: UBI
   8.0 → 8.1 deltas "~5–10 % of layer size". Everyone reports client CPU
   for recompression as the limiting cost (§2.1, §2.2, §2.9).
5. **Zeroth-order noise is bigger than the literature on deltas admits.**
   Only 2.7 % of 1,123 buildable GitHub Dockerfiles rebuild bit-identically;
   timestamps appear in 100 % of diffoscope reports, ordering 78 %, logs
   43 %, caches/DBs 37 % (§5.2). A predictor that *normalises* this (tar
   header mtimes from `SOURCE_DATE_EPOCH`, apt/dpkg logs, `.pyc` headers,
   rpmdb transaction ids) removes a large, cheap class of bytes that
   bsdiff-class tools pay for as raw inserts.
6. **Economics.** Pulling is 76 % of container start time and only 6.4 % of
   the data is read (Slacker, FAST'16); lazy-pull formats (eStargz,
   zstd:chunked, SOCI, Nydus) attack the *latency* with per-file/chunk TOCs
   and range requests, not the *bytes*; none of them are deltas against a
   previous version. Chunk-level dedup across image versions is bounded by
   the 64 KiB CDC chunk and the fact that a gzip'd layer dedups at ratio 1.0
   (DupHunter). The gap a predictive codec fills is "bytes on a bad link",
   which is Balena/Mender's market, not Kubernetes' (§6).
7. **Expected outcome (my estimate, §7.5):** on a realistic patch-release
   image rebuild (base unchanged, application binary rebuilt, a few
   packages bumped), a container module that (a) reconstructs Go-gzip
   layers, (b) normalises tar/timestamp noise, (c) recurses into zip/jar/gz
   with a zlib predictor, and (d) hands ELF/Go binaries to the existing
   binsync predictor should land at **5–20 % of what `zstd --patch-from` /
   bsdiff on the layer blobs produce**, i.e. 5–20× — with the ELF part at
   the 20–60× binsync already measures and the package/text part at the
   1–2× that Gmail-style content shows. Claims above 20× on whole images are
   not supported by anything published.

---

## 1. Container image distribution today

### 1.1 Layers are gzip tarballs, produced by Go's deflate

- OCI media types: `application/vnd.oci.image.layer.v1.tar`, `+gzip`, `+zstd`;
  `+gzip` is "interchangeable and fully compatible" with Docker's
  `application/vnd.docker.image.rootfs.diff.tar.gzip`
  (https://github.com/opencontainers/image-spec/blob/main/media-types.md).
- **Who compresses with what** (verified in source, all three default to gzip):
  - moby (`go-archive`): `return gzip.NewWriter(dest), nil` from
    `compress/gzip` at default level; decompression prefers external `unpigz`
    (`MOBY_DISABLE_PIGZ` opts out), falling back to `compress/gzip`
    (https://github.com/moby/go-archive/blob/main/compression/compression.go).
  - buildkit: `compress/gzip` via `gzip.NewWriterLevel(dest, level)`,
    `gzip.DefaultCompression` unless configured
    (https://github.com/moby/buildkit/blob/master/util/compression/gzip.go).
  - containerd: gzip *compression* is `compress/gzip` `gzip.NewWriter(dest)`;
    decompression tries `igzip`, then `unpigz`, then `klauspost/compress/gzip`;
    zstd is `klauspost/compress/zstd` both ways
    (https://github.com/containerd/containerd/blob/main/pkg/archive/compression/compression.go).
- Consequence: the deflate encoder behind essentially every layer pushed by
  Docker/BuildKit/containerd/nerdctl is **Go's `compress/flate`**, not zlib.
  Go's encoder is a different algorithm from zlib's (Go 1.7 replaced
  `BestSpeed` with "an algorithm similar to Snappy", 5–10 % larger output,
  and made the default-level compressor 2× faster, i.e. changed its match
  search: https://go.dev/doc/go1.7). Go does not promise identical output
  across releases; it promises RFC 1951 compliance. See §3.4.
- **gzip vs zstd share in the wild: no published measurement found.**
  Structural facts: zstd layers require containerd ≥ 1.5 / Docker 23 to
  pull, Docker Hub's own official images are gzip, and all three builders
  default to gzip. zstd:chunked (§1.3) is the main producer of zstd layers
  and is opt-in in podman/buildah. Treat "≫ 90 % gzip" as a safe
  qualitative assumption, *(unverified as a number)*.

### 1.2 What is inside a layer (Docker Hub, 2019)

Zhao et al., "Large-scale analysis of the Docker Hub dataset", IEEE CLUSTER
2019, accepted manuscript https://par.nsf.gov/servlets/purl/10167826.
Dataset: 457,627 repos, 1,792,609 compressed layers, 5.28 G files, 47 TB
compressed / 167 TB uncompressed.

| Metric | Value |
|---|---|
| Median layer | < 4 MB (50 %); 90 % < 177 MB uncompressed / 63 MB compressed |
| Median image | 94 MB uncompressed / 17 MB compressed; 1,090 files |
| Median layer compression ratio | 2.6 |
| Executables/objects/libraries | 37 % of bytes, 11 % of files |
| Archives (zip/gzip/tar/xz) inside layers | 23 % of bytes; zip/gzip = 96 % of archive files, 70 % of archive bytes |
| Documents (mostly ASCII text) | 14 % of bytes, 44 % of files |
| Scripts | 9 % of files; Python = 53.5 % of script files, 66 % of script bytes |
| Databases | SQLite = 7 % of DB files, > 57 % of DB bytes |
| Unique files | ~3 %; file-level dedup ratio 85.69 % overall, ELF ~87 % |
| Redundant ELF | 73.4 % of redundant executable capacity |

DupHunter (Zhao et al., USENIX ATC'20,
https://people.cs.vt.edu/~butta/docs/atc2020-duphunter.pdf) on the same data:
"97 % of files have more than one duplicate, dedup ratio 2×"; **compressed
layer tarballs dedup at ratio 1.0** (gzip scrambles), decompressed layers
2.1–2.3× with jdupes/btrfs/zfs; DupHunter's storage reduction up to 6.9×.
Both papers are about *cross-image* redundancy in a registry, not
*cross-version* redundancy of one image, but the composition table is the
best public estimate of what a per-format predictor would meet.

### 1.3 Lazy-pull formats: eStargz, zstd:chunked, SOCI, Nydus

Covered at the transport level in `update-systems.md` §2.11; the facts that
matter here:

| Format | Unit | Index | Dedup claim | Source |
|---|---|---|---|---|
| CRFS/stargz | gzip member per file, 4 MiB splits | TOC via 47-byte footer | none (no per-chunk digests) | https://github.com/google/crfs |
| eStargz | per-file gzip members, chunked | TOC + 51-byte footer, per-chunk `chunkDigest`, prefetch landmark | not a delta; "pull is 76 % of startup" is the motivation | https://github.com/containerd/stargz-snapshotter/blob/main/docs/estargz.md |
| zstd:chunked | rollsum CDC, `RollsumBits=16` (~64 KiB), one zstd frame per file | TOC + tar-split in zstd skippable frames; TOC offset in manifest annotation | local reflink dedup by chunk digest; missing chunks in **one multi-range GET** (≤ 1024 ranges) | https://github.com/containers/storage/blob/main/docs/containers-storage-zstd-chunked.md |
| SOCI | 4 MiB spans; layers < 10 MiB not indexed | separate zTOC artifact | none | https://github.com/awslabs/soci-snapshotter |
| Nydus/RAFS | fixed 1 MB chunks, sha256/blake3 | bootstrap + blob | chunk-level; Ant Group pull 4 m 15 s → 560 ms *(vendor)* | https://www.cncf.io/blog/2023/05/01/ant-group-security-technologys-nydus-and-dragonfly-image-acceleration-practices/ |

- zstd:chunked is the only mainstream format that keeps the *uncompressed*
  DiffID unchanged while adding a TOC, because the metadata lives in zstd
  skippable frames: "the zstd decompressor ignores the additional metadata so
  that the digest for the uncompressed file doesn't change"
  (https://www.redhat.com/en/blog/faster-container-image-pulls). Red Hat's
  post gives **no measured dedup ratio**; the only number is the motivating
  20 s Fedora pull. I found no published "pulled X % of the layer"
  statistic from Red Hat *(gap)*.
- Registry reality check from our earlier notes: S3/MinIO-backed registries
  answer multi-range with 200, making c/image "download the entire layer blob
  ... discarding ~99.9 %" (https://github.com/containers/image/issues/2792).
- eStargz README: "Docker still does not support lazy pulling of eStargz"; the
  README's benchmark is a histogram with no numbers
  (https://github.com/containerd/stargz-snapshotter/blob/main/README.md).

None of these is a *delta between two versions*. Their unit of reuse is the
file (eStargz) or the ~64 KiB chunk (zstd:chunked); a rebuilt Go binary,
which changes 13–70 % of its bytes in ~2 M short runs (`DESIGN.md` §1),
matches zero chunks.

### 1.4 The layer-invalidation problem

A layer is a tar of a filesystem diff, so any change to one file in a
`RUN`/`COPY` step re-emits the whole layer, and everything after it in the
Dockerfile. There is no OCI-level sub-layer delta type. The published
attempts:

| System | Granularity | Result | Status |
|---|---|---|---|
| `containers/tar-diff` (Larsson, 2020) | per-file ops `Data/Open/Copy/AddData/Seek`, bsdiff-style `AddData` inside files; output byte-identical so DiffID verifies | UBI 8.0 → 8.1 deltas "~5–10 % of layer size" | Flatpak ≥ 1.8 consumes it; podman/skopeo PR closed unmerged (https://github.com/containers/tar-diff, https://blogs.gnome.org/alexl/2020/05/13/putting-container-updates-on-a-diet/, https://github.com/containers/image/pull/902) |
| `containers/oci-delta` (bootc) | whole-image | Fedora 41 → 42, 972 M → 999 M image, **555 M delta (44 % saved)** | experimental (https://github.com/containers/oci-delta) |
| Balena deltas v3 | whole image rootfs, librsync | "10–70× smaller than pulling layers" (marketing); worst case equals full pull | production (§1.6) |

tar-diff's 5–10 % on a *minor RHEL rebase* is the best published
container-native number, and it is bsdiff-class inside files. oci-delta's
44 % on a *major* Fedora bump shows what generic delta gets when most
packages change: not much.

### 1.5 Delta systems for OS images on devices

| System | Algorithm | Recompression? | Published ratio | Source |
|---|---|---|---|---|
| Mender `mender-binary-delta` | xdelta3 on raw partition images (`-B` 64 MB source window default) | no | none published | https://docs.mender.io/artifact-creation/create-a-delta-update-artifact |
| RAUC adaptive updates | SHA-256 per 4 KiB block, index ~0.8 % of image; fetch missing blocks | no | "around 10 % of the bundle size" for a single-package change in ext4 | https://rauc.readthedocs.io/en/latest/advanced.html |
| SWUpdate delta | zchunk (CDC + zstd), header ~1 %, HTTP ranges | no | none published; librsync rejected because "the resulting delta image can even become larger as the original one"; xdelta "does not scale well" | https://sbabic.github.io/swupdate/delta-update.html |
| OSTree static deltas | per same-basename object within 30 % size: bupsplit rollsum pass, then bsdiff ≤ 128 MB; objects > 4 MB become fallbacks; parts xz | operates on uncompressed objects, client stores in its own format — no recompression needed | Foundries: 750 files / 1.5 GB OTA → "38 files ~30 M" each (request count, not bytes) | https://github.com/ostreedev/ostree/blob/main/src/libostree/ostree-repo-static-delta-compilation.c, https://foundries.io/insights/blog/ostree-static-deltas/ |
| Chrome OS / Android `update_engine` | per-block ops; best-of {SOURCE_BSDIFF, BROTLI_BSDIFF, PUFFDIFF, ZUCCHINI, LZ4DIFF} chosen by size | PUFFDIFF (deflate), LZ4DIFF (EROFS; requires *the same liblz4* on both sides) | Pixel 6 Pro incremental 376 MB vs full 2.2 GB *(secondary, unverified)* | §2.6 |
| Toradex Torizon / Foundries | OSTree | as OSTree | as OSTree | — |
| Google COS | update_engine A/B | as above | not published | — |

The RAUC number is instructive: a *block-hash* scheme with 4 KiB granularity
on an ext4 image gets 10 % for a one-package change. That is the floor a
predictive scheme must beat, and it is already far better than a
layer-invalidating pull.

### 1.6 Balena

- Docs: deltas are "a binary delta" computed "on the full image level,
  comparing the actual content, regardless of the layers used"; worst case
  "the binary delta is equal size to the Docker pull"; build-time deltas
  between last successful and new release, on-demand otherwise
  (https://docs.balena.io/learn/deploy/delta/). v2 = rsync, v3 = balenaEngine
  native (https://github.com/dcaputo-harmoni/open-balena-delta).
- Engine: "True container deltas: Bandwidth-efficient updates with binary
  diffs, 10-70x smaller than pulling layers"
  (https://github.com/balena-os/balena-engine). Implementation is
  `github.com/balena-os/librsync-go` (https://pkg.go.dev/github.com/balena-os/librsync-go).
- Staff on the forum: "Our deltas are currently based on rsync"; rsync tests
  on ~120 MB files with inserted bytes gave "tiny deltas, at most in the
  order of a few KB"; a user's modified image produced a ~700 KB delta
  (https://forums.balena.io/t/delta-update-process-on-large-files/2647).
- No Balena-published measured distribution of delta sizes was found; the
  10–70× is a marketing range *(unverified)*. Since librsync is block
  matching (`update-systems.md` §1.9: wharf's own data, 33 MiB rsync vs
  174 KiB bsdiff on two 82 MiB builds), a Balena delta of a rebuilt Go/Rust
  service is close to a full copy of that binary.

---

## 2. OS and package deltas: how each reconstructs the payload

### 2.1 Fedora deltarpm

- Reconstruction: the compressor is *read from the rpm*, not guessed.
  `makedeltarpm.c` detects the payload compressor from the stream
  (`cfile_detect_rsync`, flagging gzip `--rsyncable`) and takes the level from
  the header tag `PAYLOADFLAGS` (`'1'..'9'`; `'T'` = threaded zstd);
  `cfile.h` enumerates `CFILE_COMP_{UN,GZ,BZ_20,GZ_RSYNC,BZ_17,LZMA,XZ,ZSTD,ZSTD_THREADED}`
  (https://github.com/rpm-software-management/deltarpm/blob/master/makedeltarpm.c,
  https://github.com/rpm-software-management/deltarpm/blob/master/cfile.h).
- Apply: regenerates the cpio payload block-by-block from the old rpm or the
  installed files, pipes it through the same compressor, and **MD5-verifies
  both the payload and the whole rpm**, failing with "md5 mismatch of result"
  (https://github.com/rpm-software-management/deltarpm/blob/master/applydeltarpm.c).
  Bit-identical recompression is required and checked, not assumed.
- Known hazard: when Fedora moved to xz payloads, xz on PPC vs x86 produced
  different bytes, breaking signatures *(secondary, unverified: search
  summary of a Fedora-devel thread; the XZRpmPayloads page only says to check
  reassembly)* (https://fedoraproject.org/wiki/Features/XZRpmPayloads).
- Ratios: Presto (F11) claimed "reduce the download size of updates by
  60 %–80 %" (https://fedoraproject.org/wiki/Features/Presto). By F40/41,
  with zstd-19 payloads: available for "25 out of 42 upgrades", "saved 7.5 MB
  / 8 % of downloads on average", "wasted 52.7 MB of download on average (if
  there were failures)", and repodata costing 86.8 + 33.2 + 13.3 MB for
  everyone → dropped
  (https://fedoraproject.org/wiki/Changes/Drop_Delta_RPMs). Memory: "three to
  four times the size of the rpm's uncompressed payload"
  (https://manpages.opensuse.org/Tumbleweed/deltarpm/makedeltarpm.8.en.html).
- Lesson: a K=1 patch matrix plus expensive recompression plus a fast link
  killed it, *not* the delta ratio. The 60–80 % → 8 % collapse is mostly the
  availability/policy problem (`update-systems.md` §2.5), but zstd-19 payloads
  also mean the full rpm was already small.

### 2.2 debdelta and Debian pdiffs

- debdelta decompresses `data.tar.{gz,xz,bz2}`, diffs with xdelta/xdelta3/bsdiff
  ("a memory hog", chunked), recompresses so the `.deb` is "byte-by-byte
  identical to the original" for apt's hash checks; deltas GnuPG-signed
  (https://manpages.debian.org/testing/debdelta/debdelta.1.en.html).
- Inner gzipped files (docs, etc.) are also expanded and recompressed;
  recompression is "exactly identical" about 90 % of the time; 2006 speeds
  ~900 KB/s create, ~600 KB/s patch; "people that have a fast ADSL Internet
  connection usually are better downloading all the debs"
  (https://debdelta.debian.net/html/x190.html). The dpkg team calls it
  "unnecessarily slow" because it re-tars/re-compresses
  (https://wiki.debian.org/Teams/Dpkg/Spec/DeltaDebs).
- Site stats (Oct 2012): ~17,860 MB/month served, ~35 % of requested deltas
  unavailable; "gcc 4.4→4.5" made binaries stop delta-ing well
  (https://debdelta.debian.net/). **No published average savings percentage**
  was found; the "80–90 %" figure sometimes quoted is *unverified*.
- pdiffs: ed-script patches on `Packages` indexes, applied by apt `rred`
  (https://wiki.debian.org/DebianRepository/Format). Text; irrelevant to
  prediction beyond "line-oriented diffs are fine for text".

### 2.3 zsync

- Look-inside gzip: the `.zsync` file records "the offset of each block
  header in the deflated stream, and the offset in the uncompressed data that
  this corresponds to" (bit offsets); the client rsyncs against uncompressed
  content and fetches compressed byte ranges, inductively holding the 32 KB
  window (https://zsync.moria.org.uk/paper200501/ch03s02.html).
- Server-side fix: `zsyncmake -z/-Z` compresses "1024 bytes ... at a time,
  telling zlib to start a new block after each input block" so one zsync
  block = one deflate block (https://zsync.moria.org.uk/paper/ch03s04.html).
- Rebuilding the original `.gz`: brute force — "decompress the file and then
  recompress it with a variety of options, until a set of options is found
  that produces a file identical to the original", relying on "almost all
  gzip files are either compressed with the defaults, or with gzip --best";
  "not guaranteed to work" (https://zsync.moria.org.uk/paper/ch03s06.html).
- Ubuntu ISO savings ~80 % on dailies are community numbers *(secondary)*
  (https://help.ubuntu.com/community/ZsyncCdImage).

### 2.4 OSTree static deltas, rpm-ostree, Nix, Guix

- OSTree format and compiler: §1.5 table; opcodes `OPEN_SPLICE_AND_CLOSE`,
  `OPEN`, `WRITE`, `SET_READ_SOURCE`, `UNSET_READ_SOURCE`, `CLOSE`, `BSPATCH`;
  parts "Hardcode xz for now"; candidate pairs = same filename within 30 %
  size; rollsum pre-pass "only proceed if the file contains ... more than
  50 % of the previous chunks"
  (https://github.com/ostreedev/ostree/blob/main/src/libostree/ostree-repo-static-delta-private.h,
  https://ostreedev.github.io/ostree/formats/). No published byte ratio from
  Endless/Silverblue was found *(gap)*.
- Nix: no upstream deltas. nix-sandwich (`zstd --patch-from` L3 default or
  xdelta3 on *uncompressed* NARs; expands compressed kernel modules first):
  pipewire 315,060 B → 670 B; systemd 251.15 → 251.16 5.8 MB → 1.3 MB; a
  NixOS update 13.6× less download (https://github.com/dnr/nix-sandwich).
  casync-chunked NARs: 48.47 % saved on a mass rebuild, 1.07 % on a Firefox
  bump (https://alternativebit.fr/posts/nixos/future-of-nix-substitution/).
  That last pair is the cleanest public demonstration that CDC dedup
  collapses on a rebuilt binary while an LZ/bsdiff-class delta does not.
- Guix: whole NARs, lzip/zstd, no deltas
  (https://guix.gnu.org/en/blog/2021/getting-bytes-to-disk-more-quickly/).

### 2.5 Chrome OS / Android `update_engine`: puffin, zucchini, lz4diff

- Ops and selection: `BestDiffGenerator` tries candidates and keeps the
  smallest; limits `kMaxBsdiffDestinationSize` 200 MiB, `kMaxPuffdiffDestinationSize`
  150 MiB, `kMaxZucchiniDestinationSize` 150 MiB; ZUCCHINI restricted to
  `.ko .so .art .odex .vdex`, kernel, modem, "skipping zip files where puffin
  performs better"; deflate streams located by extension (`.apk .zip .jar
  .gz`) and by scanning squashfs images ≥ 1 MB, with bit extents shifted to
  partition offsets
  (https://android.googlesource.com/platform/system/update_engine/+/HEAD/payload_generator/delta_diff_utils.cc,
  https://android.googlesource.com/platform/system/update_engine/+/HEAD/payload_generator/deflate_utils.cc).
- **puffin** ("A deterministic deflate re-compressor (for patching purposes)"):
  motivation "deflate has a bit-aligned format, hence, changing one byte in
  the raw data can cause the entire deflate stream to change drastically".
  `puff` "decompresses only the Huffman part of the deflate stream and keeps
  the structure of the LZ77 coding unchanged" — "decompressing half way". The
  puff stream keeps block headers (type, dynamic code-length arrays), literal
  runs and (length, distance) pairs as byte-aligned records. `huff` inverts
  it deterministically because "there is no need to perform LZ77 algorithm"
  and "the dynamic Huffman tables can be recreated uniquely from the code
  length array stored inside the puff stream". `puffdiff` = puff(src),
  puff(dst), bsdiff (or any diff) + `BitExtent` stream locations;
  `puffpatch` = puff(src), bspatch, huff
  (https://chromium.googlesource.com/chromium/src/+/main/third_party/puffin/README.md).
  The README does not quantify puff overhead or patch-size gains *(gap)*.
  Design consequence: puffin needs **no assumption about which encoder made
  the stream**, but the patch must carry the *new* stream's LZ77 choices and
  Huffman tables, so it only shrinks when old and new streams share block
  structure; it does not remove the encoder's second-order churn, it only
  makes it byte-aligned so bsdiff can align it.
  Chromium's **"whole CRX differential update"** is this operation applied to
  the cached signed package as one object: previous full CRX + `.puff` → next
  full CRX, followed by normal verification/install. It replaced the component
  updater's old Courgette-based, per-file differential path behind a feature
  flag in 2022
  (https://chromium-review.googlesource.com/c/chromium/src/+/3885395). That is a
  product-pipeline replacement for Courgette, not an algorithmic successor to
  Zucchini: Puffin models DEFLATE streams and then uses bsdiff, while Zucchini
  models executable references. Chromium's current updater protocol retains
  separate `puff` and `zucc` operations, so a server can select the appropriate
  transform for a cached payload
  (https://chromium.googlesource.com/chromium/src/+/main/docs/updater/protocol_4.md).
- **Zucchini** (Courgette successor): disassemblers extract references for
  x86/x64/ARM/AArch64/DEX, targets are relabelled so old/new share labels, an
  "encoded image" masks the pointer noise, and an equivalence map is built
  with a suffix array (https://chromium.googlesource.com/chromium/src/+/HEAD/components/zucchini/README.md).
  Mozilla's measurements on Firefox partials (MB, mbsdiff / Courgette /
  Zucchini): 68.0.2→69.0 27.33 / 23.83 / 21.18; 69.0→69.0.1 5.17 / 2.19 /
  1.94; 70.0→70.0.1 5.54 / 2.46 / 2.15; expected gains Windows ~33 %, Linux
  ~10 %, macOS ~9.7 % (https://bugzilla.mozilla.org/show_bug.cgi?id=1632374).
  Courgette's own number: Chrome 190.1→190.4 full 10,385,920 B, bsdiff
  704,512 B, Courgette 78,848 B (https://www.chromium.org/developers/design-documents/software-updates-courgette/).
  Note the Linux figure: on ELF, Zucchini only buys ~10 % over bsdiff for
  Firefox, versus binsync's 24–68× on Go binaries. The difference is that Go
  exposes a complete function table (`.gopclntab`) so alignment is *by name*
  rather than by heuristic reference relabelling; that is the argument for
  per-format predictors rather than one generic disassembler.
- **LZ4DIFF** (EROFS): decompresses clusters, diffs, recompresses; "the
  delta_generator uses a copy of liblz4.so ... important that this copy is
  the same as the one on the source build"
  (https://android.googlesource.com/platform/system/update_engine/+/refs/heads/master/lz4diff/).
  I.e. Google solved LZ4 determinism by *pinning the library binary*, not by
  prediction.

### 2.6 Windows MSDelta / UUP

- Delta Compression API: MSDelta (PA30) "When the source and target files are
  similar PE files, the size of the delta can typically be made 50–70 %
  smaller than without this special treatment"; flags `DELTA_FLAG_E8`
  ("Transform E8 instructions (relative calls) ... MSDelta will create the
  delta twice ... The smaller of the two deltas will be returned"),
  `_I386_JMPS`, `_I386_CALLS`, `_AMD64_DISASM`, `_AMD64_PDATA`, `_ARM_DISASM`,
  `_CLI_DISASM`, `_CLI_METADATA`, `_IMPORTS/_EXPORTS/_RESOURCES/_RELOCS`,
  `_HEADERS`; PE normalisation rebases and strips bind/timestamps so one
  delta applies to several source variants
  (https://learn.microsoft.com/en-us/previous-versions/bb417345(v=msdn.10)).
- Reverse engineering of PA30: LZX-delta with three Huffman trees, a "rift
  table" tracking block movement, transforms such as
  `RiftTransformRelativeJmpsI386`, processing graphs built at runtime
  (https://github.com/smilingthax/msdelta-pa30-format,
  https://www.cobalt.io/blog/decoding-windows-cbs-manifests-reversing-the-dcm/pa30-delta-format).
  MSDelta is thus Courgette-shaped (normalise pointers, then LZ-delta), with
  the interesting twist that *normalisation is applied to the source too*, so
  the delta is against a canonical form — the same idea as binsync's
  prediction being a deterministic function of (old, side table).
- Sizes and UUP forward/reverse differentials: `update-systems.md` §2.8
  (Full ~1 GB, Delta 300–500 MB, Express 150–200 MB, UUP pair ~250 MB).
  Windows 11 24H2 checkpoint cumulative updates rebase file-level
  differentials on a checkpoint instead of RTM; no sizes given
  (https://learn.microsoft.com/en-us/windows/whats-new/whats-new-windows-11-version-24h2).

### 2.7 Apple

- iOS OTA: `payloadv2/` custom archive in a pbzx (chunked XZ) container,
  `payload.bom` with SHA-1s; patches are BXDIFF (bsdiff-derived)
  (https://newosxbook.com/articles/OTA.html). Cryptex patches apply "a binary
  diff to the relevant cryptex image" via private `RawImagePatch`; a March
  2026 Background Security Improvement was 26.5 MiB against 3–17 GB full
  OTAs (https://blog.calif.io/p/reverse-engineering-apples-silent). Delta
  vs full size examples (iPhone 13 17.3→17.4 1.2 GB vs 5.1 GB) are
  *(secondary, unverified)*. Nothing public on structure-awareness.

### 2.8 Steam, Wharf

- Steam: ~1 MB chunks, compressed and encrypted; reuse by chunk match; the
  docs warn absolute-offset pack files force "over half the entire file"
  (https://partner.steamgames.com/doc/sdk/uploading). No official percentage.
- Wharf/butler: rsync-style push, server re-diffs with bsdiff + Brotli;
  Mewnbase 82 MiB builds: rsync patch 33 MiB → bsdiff 174 KiB; Aven Colony
  11 GiB: 532 MiB → 125 MiB (https://fasterthanli.me/articles/efficient-game-updates).
  No look-inside.

### 2.9 Google Play file-by-file / archive-patcher

- Mechanism (source): `DefaultDeflateCompressionDiviner` inflates each changed
  entry and re-deflates with candidates in popularity order — nowrap ∈
  {true,false} × strategy 0 levels {6,9,1,4,2,3,5,7,8}, strategy 1 levels
  {6,9,4,5,7,8}, strategy 2 level 1; a `MatchingOutputStream` throws on the
  first divergent byte. Recompression record per entry: level, strategy,
  wrap (1 B each) + compatibility-window id; `DefaultDeflateCompatibilityWindow`
  fingerprints the local zlib against a ~9000-byte corpus and "all zlib
  versions since 1.2.0.4 (2003) have identical fingerprints"; entries whose
  params cannot be found stay compressed; only `java.util.zip` (zlib) is
  supported; 32 KiB window hard-coded; no zip64
  (https://github.com/google/archive-patcher/blob/master/generator/src/main/java/com/google/archivepatcher/generator/DefaultDeflateCompressionDiviner.java,
  https://github.com/google/archive-patcher/blob/master/README.md).
- Published table (Dec 2016 blog, original / bsdiff / file-by-file):

| App | APK | bsdiff | file-by-file | f-b-f vs bsdiff |
|---|---|---|---|---|
| Farm Heroes Super Saga | 71.1 MB | 13.4 MB (−81 %) | 8.0 MB (−89 %) | 1.7× |
| Google Maps | 32.7 MB | 17.5 MB (−46 %) | 9.6 MB (−71 %) | 1.8× |
| Gmail | 17.8 MB | 7.6 MB (−57 %) | 7.3 MB (−59 %) | 1.04× |
| Google TTS | 18.9 MB | 17.2 MB (−9 %) | 13.1 MB (−31 %) | 1.3× |
| Kindle | 52.4 MB | 19.1 MB (−64 %) | 8.4 MB (−84 %) | 2.3× |
| Netflix | 16.2 MB | 7.7 MB (−52 %) | 1.2 MB (−92 %) | 6.4× |

  "recompression can take a little over a second per megabyte" on 2015
  devices; "if the patch size is halved then the time spent applying the
  patch ... is doubled"; restricted to background auto-updates
  (https://android-developers.googleblog.com/2016/12/saving-data-reducing-the-size-of-app-updates-by-65-percent.html).
  Earlier: bsdiff itself gave Chrome M46→M47 22.8 → 12.9 MB, ~98 % of
  updates are deltas (https://android-developers.googleblog.com/2016/07/improvements-for-smaller-app-downloads.html).
- Reading the table honestly: peeling deflate is worth 1.0–6.4× *over
  bsdiff*, median ~1.7×. The residual (Gmail 7.3 MB of 17.8) is mostly DEX
  code and resources that changed for real, plus the executable-churn bsdiff
  cannot fix. Play never stacked Zucchini on top of file-by-file for DEX in
  this pipeline; update_engine does (ZUCCHINI on `.odex/.vdex`, PUFFDIFF on
  zips), which is the closest published analogue to the modular design in §7.

---

## 3. The recompression trick

### 3.1 Two strategies

| Strategy | Examples | What the patch carries | Encoder assumption |
|---|---|---|---|
| **Recover parameters, re-run the encoder** | deltarpm (reads `PAYLOADFLAGS`), archive-patcher (brute-force level×strategy×wrap, verified), zsync `-Z`, debdelta, LZ4DIFF (pinned liblz4) | a few bytes per stream | the *same encoder implementation* is available at decode time and is deterministic |
| **Predict the encoder, store corrections** | preflate, preflate-rs, precomp ≥ 0.4.8 (uses preflate), Reflate | corrections: 0.01 %–3 % of uncompressed size | a *model* of the encoder; unknown encoders degrade gracefully |
| **Carry the entropy-coder state** | puffin | LZ77 token stream + Huffman code lengths of the *new* stream | none, but the patch pays for the new stream's tables and token choices |

The middle row is literally binsync's predict-then-correct pattern applied
to a compressor: the "prediction" is a re-implementation of zlib's match
finder, the "correction" is an arithmetic-coded diff of where the real
encoder chose differently.

### 3.2 preflate / preflate-rs / precomp

- preflate (Steinke): splits deflate into "uncompressed data and
  reconstruction information"; for zlib streams "only a few bytes of
  reconstruction information"; at low zlib levels "20–30 %" of streams need
  more than 3 bytes, "usually only a few tens of bytes"; supports "ZLIB at any
  compression level, 7zip, kzip, any deflate stream"; slower than
  precomp/reflate on small streams (50–500 %); tested on "some ten thousand
  valid deflate streams" (https://github.com/deus-libri/preflate/blob/master/README.md).
- preflate-rs (Microsoft, Roomp): correction overhead as % of uncompressed
  data, per encoder and level:

| Encoder | L1 | L2 | L3 | L4 | L5 | L6 | L7 | L8 | L9 |
|---|---|---|---|---|---|---|---|---|---|
| zlib | 0.01 | 0.01 | 0.01 | 0.01 | 0.01 | 0.01 | 0.08 | 0.03 | 0.01 |
| zlib-ng | 0.01 | 0.01 | 0.01 | 0.97 | 1.07 | 0.90 | 0.01 | 0.01 | n/a |
| libdeflate | 0.25 | 1.04 | 0.91 | 1.51 | 1.04 | 0.96 | 0.87 | 1.04 | 1.03 |
| miniz_oxide | 0.06 | 2.70 | 1.78 | 0.53 | 0.30 | 0.09 | 0.06 | 0.08 | 0.07 |

  "Unrecognized compressors still round-trip correctly — the corrections
  overhead is simply higher"; "used in production cloud storage systems"
  (product unnamed) (https://github.com/microsoft/preflate-rs/blob/main/README.md).
  **Go's `compress/flate`, klauspost/compress, zopfli and .NET are not
  listed.**
- precomp: zlib/deflate (PDF, PNG, ZIP...), bzip2, GIF, JPG, MP3; since
  0.4.8 "preflate v0.3.5 ... is used to create and use reconstruction
  information of deflate streams"; silesia.zip 67,633,896 B → 47,122,779 B
  (69.7 %) vs 7-Zip LZMA2 ultra 99.7 %
  (https://github.com/schnaader/precomp-cpp/blob/master/README.md).
- Reflate (Shelwien), AntiZ, xtool: not reached with primary sources this
  session *(gap)*; preflate's README positions its reconstruction data as
  smaller than reflate's.

### 3.3 Is zstd / brotli / xz / gzip re-encoding reproducible?

| Codec | Same version, same params | Across versions | Source |
|---|---|---|---|
| zstd | "fully deterministic"; "compressed outcome is always the same for the same set of compression parameters" regardless of thread count — but `--single-thread` and `-T#` are *different* parameter sets with different output; CLI defaults to the MT path | "The compressed output may (and does often) change between zstd versions" (terrelln); only decode compatibility is guaranteed | https://github.com/facebook/zstd/issues/2079 |
| gzip/deflate | deterministic per implementation and level | GNU gzip, zlib, zlib-ng, libdeflate, pigz, Go, klauspost all differ; Go changed its `BestSpeed` and default-level encoder in 1.7 (https://go.dev/doc/go1.7); klauspost's README documents rewrites and a changed default level (https://github.com/klauspost/compress) | — |
| xz/lzma | deterministic | Debian relies on it for `.deb` reproducibility; deltarpm's PPC/x86 mismatch is the cautionary tale *(unverified)* | §2.1 |
| bzip2 | deterministic | stable for decades | — |
| brotli | deterministic per version/params | **not verified this session**; Google does not, to my knowledge, promise cross-version stability *(unverified)* | — |
| lz4 | deterministic | Android pins the library binary for LZ4DIFF | §2.5 |

Conclusion: parameter recovery only works where the decoder host has *the
same encoder build* (deltarpm, LZ4DIFF) or an encoder whose output has been
frozen for 20 years (zlib — archive-patcher's "identical fingerprints since
1.2.0.4"). For zstd layers and Go-gzip layers, the only robust route is
prediction with corrections (preflate-style) or carrying coder state
(puffin-style).

### 3.4 The Go-deflate gap

Every OCI layer produced by Docker/BuildKit/containerd is Go `compress/gzip`
at `DefaultCompression` (§1.1). Consequences:

- archive-patcher's diviner would fail on all of them (it only models zlib).
- preflate-rs would round-trip them but at the "unrecognised compressor"
  overhead, i.e. likely ≥ 1 % of uncompressed size — for a 177 MB layer
  that is ≥ 1.8 MB of correction just to *see* the content, before any
  delta. *(Not measured; needs an experiment.)*
- puffin would work (it needs no encoder model) but the patch then carries
  the new layer's full LZ77 token stream where content changed.
- **A binsync deflate module has an advantage nobody else has: it *is* Go,
  can link the exact `compress/flate` encoder (version-pinned via
  `go.mod`/toolchain), and can therefore recover Go-gzip layers with
  parameter recovery (level only) rather than prediction — and, for layers
  built by a different Go version, fall back to a Go-flate-specific
  predictor whose corrections should be near zero because the algorithm
  differences between Go releases are small and known.**
  Concretely: Go's default-level encoder is a hash-chain matcher with
  fixed-size blocks; predicting its token stream from the plaintext is a
  much smaller job than modelling zlib's lazy matching with 9 levels.
- Go's `archive/tar` writer is equally deterministic given the header
  fields (USTAR → PAX → GNU selection rules, PAX records for sub-second
  mtimes, long names, big uid/gid: https://pkg.go.dev/archive/tar), so the
  tar framing is fully predictable from a file list plus one epoch.

---

## 4. Structure inside images a predictor could exploit

### 4.1 Bytes by type (from §1.2)

| Class | Share of in-layer bytes | Predictor available? | Published delta-friendliness |
|---|---|---|---|
| ELF / objects / libraries | 37 % | Courgette/Zucchini/MSDelta (generic); binsync (Go) | Zucchini 1.1–2.6× over bsdiff on Firefox; binsync 24–68× on Go |
| Archives (zip/gzip/tar/xz) | 23 % | deflate: preflate/puffin/archive-patcher; xz: parameter recovery; zstd: none published | file-by-file 1.0–6.4× over bsdiff (APKs) |
| Text/docs | 14 % | line diff / LZ with old as dictionary | text is where `zstd --patch-from` is already near-optimal |
| Scripts (Python 66 % of script bytes) | ~5 % *(est.)* | source is text; `.pyc` see §4.2 | none published |
| SQLite DBs | > 57 % of DB bytes | page-aware diff (sqldiff / page map) | none published for rpmdb |
| Locale / tz | image-dependent; Debian `locales-all` 236 MB installed, `tzdata` 1.4 MB | rarely changes between versions; predictor = "copy" | none |

### 4.2 Python `.pyc`

- Header (PEP 552): magic, flags, then (mtime, size) or a 64-bit SipHash of
  the source; `SOURCE_DATE_EPOCH` set → CHECKED_HASH default in
  `py_compile` since 3.7.2 (https://peps.python.org/pep-0552/,
  https://docs.python.org/3/library/py_compile.html).
- Body is **not** deterministic even with hash-based headers: marshal's
  `FLAG_REF` is emitted only when an object's refcount > 1, so identical
  objects can serialise differently; fix proposals cost ~41 % marshal
  overhead and stalled ("closed as not planned")
  (https://github.com/python/cpython/issues/78274). PEP 552 also names
  frozenset ordering and interning.
- Predictor shape: recompile the source (available in the same layer as
  `.py`) with the same interpreter, then correct. Expect small residual
  corrections, not identity. No published byte-change-per-edit numbers.

### 4.3 Java `.jar` / `.class`

- javac output is reproducible; packaging is the problem (zip timestamps,
  entry order): Maven `project.build.outputTimestamp` since maven-jar-plugin
  3.2.0 (https://maven.apache.org/guides/mini/guide-reproducible-builds.html).
  Ecosystem-scale: 35.8 % of 35,956 Reproducible-Central artifacts
  unreproducible; 936 with bytecode diffs, canonicalisation fixes 29.4 % of
  those (arXiv 2504.21679).
- `java.util.zip` = zlib → archive-patcher/preflate reproduce jar entries
  exactly; jars are the one archive class where the *zlib* predictor is the
  right one, unlike OCI layers.

### 4.4 node_modules

Text; npm normalises tarball mtimes to a fixed 1985-10-26 date
(https://github.com/npm/npm/issues/20439). Byte-determinism of `npm pack`
beyond that is *(unverified)*. A predictor adds nothing over LZ-with-old-dict
here; source maps are large JSON with position arrays that shift on edits
(second-order churn in VLQ form) — a plausible but unmeasured target.

### 4.5 `.deb`/`.rpm` caches and package databases

- dpkg `status` is text. The rebuild noise is in `/var/log/apt/*.log`,
  `dpkg.log`, `alternatives.log` and `/var/cache/ldconfig/aux-cache`
  (wall-clock timestamps); removing them plus snapshot.debian.org gave
  bit-identical images (https://dangerzone.rocks/news/2026-03-02-repro-build/).
- rpmdb is SQLite since RPM 4.16 / Fedora 33
  (https://fedoraproject.org/wiki/Changes/Sqlite_Rpmdb); `rpmdb.sqlite-shm/-wal`
  are "the very last thing blocking reproducible rpm-ostree container
  images" (https://github.com/rpm-software-management/rpm/issues/2219);
  rpmoci pins install times/transaction ids via `SOURCE_DATE_EPOCH`
  (https://github.com/microsoft/rpmoci/issues/82). No byte-fraction
  numbers; SQLite pages are mostly stable across identical installs but not
  identical without effort.

### 4.6 tar headers

Fully predictable given the file listing: Go `archive/tar` chooses
USTAR/PAX/GNU deterministically, rounds mtime to seconds unless PAX/GNU
(https://pkg.go.dev/archive/tar); BuildKit `SOURCE_DATE_EPOCH` (0.11+) sets
config/history timestamps, `rewrite-timestamp=true` (0.13+) rewrites in-layer
mtimes at export (https://github.com/moby/buildkit/blob/v0.12/docs/build-repro.md;
caveat that cross-stage COPY does not: https://github.com/moby/buildkit/issues/6348).
A container module should *predict* tar headers from (path, size, mode, uid,
gid, epoch) and correct, rather than diff them — 512-byte headers with an
octal checksum are a textbook second-order artefact.

### 4.7 ELF from Go/Rust/C

Go ≥ 1.21 toolchains are "perfectly reproducible" (https://go.dev/blog/rebuild);
Rust's tracking issue #34902 is open since 2016. So for Go the *only*
difference between two builds of the same source is nothing, and between
two versions it is first-order edits plus the layout churn binsync
already models. For C/C++ packages inside a distro base layer, the
second-order churn is the Courgette/Zucchini case (pointer relabelling); no
function-name table is guaranteed (stripped), so the predictor falls back
to reference relabelling — expect Zucchini-class 1.1–2.6×, not binsync-class.

---

## 5. Reproducible builds and zeroth-order noise

### 5.1 Packages

- Debian trixie/amd64, 2026-08-27: 36,417 reproducible (96.9 %), 828 FTBR
  (2.2 %), 260 FTBFS (https://tests.reproducible-builds.org/debian/trixie/index_suite_amd64_stats.html);
  unstable/amd64 38,693 / 41,140 = 94.1 %
  (https://tests.reproducible-builds.org/debian/reproducible.html).
- Cause catalogue: timestamps, timezones, locales, randomness, build path,
  archive metadata, volatile inputs, file order, uid/gid, system images,
  value initialisation, version information
  (https://reproducible-builds.org/docs/).
- Debian notes-tagged issue counts: gcc captures build path 1,841;
  randomness in R rdb/rds 1,334; records build flags 442; cmake rpath build
  path 437; **build-id differences only 327**; build path via assert 217;
  sphinx randomness 207 (https://tests.reproducible-builds.org/debian/index_issues.html).
- RepLoc (Ren et al., ICSE'18, arXiv 1803.06766), 671 fixed packages:
  timestamps 462 (69 %), file ordering 118 (18 %), randomness 50 (7 %),
  locale 41 (6 %). Lamb & Zacchiroli (arXiv 2104.06020): "Timestamps are,
  by far, the biggest source".
- **No paper quantifies "% of bytes differing" on a nondeterministic
  rebuild**; diffoscope reports are hunk-level. This is a measurable gap
  we can fill ourselves cheaply (rebuild a few images twice, run our codec).

### 5.2 Images

- "It's Not Just Timestamps: A Study on Docker Reproducibility" (arXiv
  2602.17678): 2,000 GitHub repos, 1,123 buildable; **2.7 % (30) bitwise
  reproducible as-is, 18.6 % (209) after hardening** (`SOURCE_DATE_EPOCH`,
  `rewrite-timestamp`, pinning), 78.7 % still not. Of 954 diffoscope
  reports: timestamps/metadata 100 %, file formatting/ordering 78.1 %,
  system logs 43.3 %, caches & DBs 36.8 %, compiled artifacts 20.0 %,
  app-specific 13.0 %, random data 9.4 %, package-manager state 5.6 %.
- apko/Chainguard: a locked config reproduces the same digest; nightly
  digest churn comes from floating package versions, not the builder
  (https://www.chainguard.dev/unchained/reproducing-chainguards-reproducible-image-builds).
  No churn number published.
- Interpretation for the codec: on a *rebuild with unchanged sources*, the
  differing bytes are almost entirely in classes a predictor can normalise
  (tar mtimes, log lines with timestamps, `.pyc` headers, rpmdb ids,
  build-ids). On a *rebuild with changed sources*, those same bytes are
  still there, on top of the real change, and bsdiff-class tools pay for
  them as inserts. The size of that tax is unmeasured (§5.1 last bullet).

---

## 6. Fleet economics

| Fact | Number | Source |
|---|---|---|
| Container start dominated by pull | pulling = 76 % of start time; 6.4 % of data read | Slacker, FAST'16 https://www.usenix.org/conference/fast16/technical-sessions/presentation/harter |
| Images in the registry | median 17 MB compressed / 94 MB uncompressed; 90th pct 0.48 GB / 1.3 GB | Zhao CLUSTER'19 (§1.2) |
| Compressed layers dedup | ratio 1.0 (gzip); 2.1–2.3× decompressed | DupHunter (§1.2) |
| Registry egress | S3/GCS class egress $0.05–0.09/GB *(general cloud pricing, not re-verified this session)*; a 1,000-device fleet pulling a 100 MB image daily = 3 TB/month | — |
| Edge/IoT | Balena's premise: cellular/satellite links where a full pull is unaffordable; deltas "10–70×" *(marketing)* | §1.6 |
| Nydus at scale | Ant Group pull 4 m 15 s → 560 ms *(vendor)* | §1.3 |
| P2P fan-out | Kraken 3 GB to 2,600 hosts p50 10 s; Spegel 17 GB on 56 nodes 348 s → 307 s | `update-systems.md` §2.12 |

Reading: in datacentre Kubernetes the pain is *latency*, and lazy pulling
plus P2P already address it without deltas; in edge/IoT and CI-to-production
over the public internet the pain is *bytes*, and there the shipping
answers are rsync/xdelta/block-hash at 10 %–50 % of full. That is the
market for a predictive codec, and it is Balena/Mender/RAUC/OSTree's market,
not Docker Hub's.

---

## 7. Assessment for a predictive codec

### 7.1 Where the big wins are, ranked

1. **Executables inside layers (37 % of bytes).** This is binsync's existing
   result transplanted: for Go services (the common case in the images this
   project cares about) 24–68× over bsdiff is already measured. For distro
   C/C++ libraries in the base layer, expect Zucchini-class 1.1–2.6× — and
   note the base layer usually does not change in a patch release, so its
   contribution to the patch is zero either way.
2. **The layer wrapper itself.** Without deterministic reconstruction of the
   Go-gzip layer, none of the above is reachable: the patch target is the
   compressed blob whose digest the registry/runtime verifies. This is a
   prerequisite, not a win, but it is also where binsync has a unique
   position (§3.4). Puffin-style coder-state carrying is the safe fallback
   for layers produced by unknown encoders.
3. **Zeroth-order noise.** Tar headers, timestamps in logs, `.pyc` headers,
   rpmdb transaction ids, build-ids. Cheap to predict, present in ~100 % of
   rebuilds (§5.2), and paid for as raw bytes by every shipping tool. Size
   unmeasured; my guess is tens of KB to a few MB per image, which matters
   when the executable patch is 100 KB.
4. **Nested archives (23 % of bytes).** jars/wheels/`.gz` docs. zlib-made,
   so preflate-class 0.01 % overhead applies and the inner content becomes
   visible; the win then depends on what is inside (DEX/class: Zucchini-ish;
   text: ~1×). Play's table says 1.7× median over bsdiff.
5. **Text, SQLite, locale data.** Marginal. `zstd --patch-from` with the old
   file as dictionary is near-optimal for text; SQLite page-level copy is
   easy but the DB rarely dominates.

### 7.2 Prediction vs deterministic recompression

They are the same mechanism at different layers, and the framework should
treat them identically:

- A *deflate predictor* takes the plaintext (which the container module has,
  because it also has the old layer) and predicts the token stream; the side
  table is the encoder id + level; the correction is preflate's diff. That is
  exactly `predict(old, side) → prediction; correct(prediction, patch)`.
- A *tar predictor* takes (file list, epoch, uid/gid policy) and predicts
  headers.
- A *Go-ELF predictor* takes (old binary, new function table) and predicts
  layout.

So the answer to "prediction or recompression?" is: recompression is
prediction of a compressor. The distinction that *does* matter is whether
the predictor needs the *exact encoder build* (deltarpm, LZ4DIFF — brittle)
or a *model plus corrections* (preflate — robust). The framework should
require the robust form: every module must round-trip *any* valid input,
with correction size, not success, as the quality metric. preflate-rs's
"unrecognised compressors still round-trip correctly" is the contract to
copy.

### 7.3 What the modular architecture needs

```
container module (OCI manifest/config/layers)
  └─ gzip/zstd frame module       — recover/predict encoder; expose plaintext
       └─ tar module              — predict headers; expose entries by path
            ├─ elf/go module      — binsync predictor (by function table)
            ├─ elf/generic module — reference relabelling (Zucchini-class)
            ├─ zip/jar module     — central directory + per-entry deflate (zlib model)
            │    └─ class/dex module (optional)
            ├─ pyc module         — recompile-from-source predictor, marshal correction
            ├─ sqlite module      — page-map copy
            ├─ text module        — LZ with old entry as dictionary (zstd --patch-from)
            └─ default            — bsdiff-class approximate matching
```

Requirements that fall out of the research:

- **Pairing, not diffing, is the module contract.** OSTree pairs by basename
  within 30 % size; update_engine pairs by partition block map; Play pairs
  by zip entry name. The tar module must expose `(path → old entry, new
  entry)` pairs, including renames (Debian's `*-1.2.3.so` versioning), and
  hand *pairs* to inner modules. Unpaired new files go to plain compression.
- **Every module is a deterministic function of (old, side table)** and must
  produce a byte-exact result after correction; digests (layer DiffID, blob
  sha256) are verified at every level, as deltarpm and update_engine do.
- **Corrections are arithmetic-coded, not byte-diffed.** preflate's diff
  sizes come from modelling *which* decision failed, not from bsdiff-ing
  the token stream; puffin's larger patches come from doing the latter.
- **Depth budget and CPU budget are first-class.** Play: "if the patch size
  is halved then the time spent applying the patch ... is doubled";
  debdelta: better to download on fast links; Fedora: dropped. A module must
  report predicted decode cost so the publisher can choose to stop
  recursing (e.g. do not recompress a 200 MB locale archive to save 3 KB).
- **Encoder identification is part of the side table.** Layer → "Go
  compress/gzip, level −1, Go 1.27"; jar entry → "zlib level 6 strategy 0
  nowrap". The container module can often *read* this (rpm `PAYLOADFLAGS`,
  gzip header OS/XFL bytes, zip entry flags) and only falls back to
  diviner-style search when it cannot.
- **Fallback objects**, as in OSTree: anything the predictor cannot improve
  over `zstd --patch-from` ships that way, per file, so one hostile input
  never blows up the whole patch.

### 7.4 What is unverified and should be measured first

1. Correction overhead of preflate-rs on Go `compress/gzip` output (no
   published number; determines whether a Go-specific predictor is needed
   or merely nice).
2. Bytes of zeroth-order noise on a same-source Docker rebuild, by class
   (tar headers / logs / pyc / rpmdb), on three representative images
   (Go static binary on distroless; Python on debian-slim; Java on a JRE
   base).
3. Fraction of a realistic patch-release image diff that is the application
   binary vs everything else — this decides whether the container module is
   worth building beyond "unwrap gzip+tar, hand the binary to binsync".
4. zstd:chunked / eStargz measured dedup on cross-version pulls (Red Hat
   published no number).
5. Brotli cross-version output stability.

### 7.5 Expected ratio on a realistic image rebuild

Scenario: Go service image, patch release; base layer unchanged (0 bytes),
one application layer of a 30–90 MB stripped Go binary plus a few config
files, rebuilt with BuildKit.

| Tool | Patch (est.) | Basis |
|---|---|---|
| Registry pull (layer invalidated) | 100 % of the app layer, ~10–30 MB gz | layer digest changes |
| zstd:chunked / eStargz dedup | ~90–100 % of the layer | no 64 KiB chunk survives a Go rebuild (`DESIGN.md` §1) |
| Balena librsync / RAUC blocks | ~50–100 % of the binary | block matching on shifted code |
| bsdiff / zstd --patch-from on the *uncompressed* layer | 2–10 MB | `benchmark-scale.md`: kube-apiserver 2.06 MB, terraform 5.4 MB |
| bsdiff on the *compressed* layer blob | ≈ full | deflate scrambles; this is what naive "delta the OCI blob" gives |
| Container module + binsync predictor | **0.1–0.5 MB** | Go-aware 24–68× over bsdiff, plus recovered gzip/tar wrapper at ~0 cost, plus noise normalisation |

So 20–60× over the best generic tool *on the uncompressed content*, and
effectively unbounded over tools operating on compressed blobs — but that
headline is entirely the Go-binary result wearing a container costume. For
a Python or Java image the honest expectation is Play-like: 1.5–6× over
bsdiff on the uncompressed content, dominated by first-order change, with
the extra gains coming from noise normalisation rather than prediction.

The domain is worth a *container module* because it is the delivery
vehicle for the binaries binsync already wins on, and because the Go-deflate
gap is unfilled. It is not, on the evidence, a domain where a predictor
wins 30× on the *non-executable* content, and the design brief should not
promise that.

---

## References

Primary sources are linked inline. Cross-references: `docs/DESIGN.md` §1,
`docs/research/update-systems.md` §2.4–2.12, `docs/research/binary-delta.md`
§2, `docs/research/benchmark-scale.md`, `docs/general/research/percival-thesis.md`.
