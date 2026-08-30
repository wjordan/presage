# Colin Percival's bsdiff: the released tool, the thesis algorithm ("bsdiff 6"), and what is claimed but unreleased

Research notes, 2026-08-29. Purpose: extract concrete, portable algorithmic ideas
from Percival's published work on executable delta compression.

**Evidence classes used throughout.** Every claim below is tagged:

- **[T]** — written in the D.Phil. thesis (*Matching with Mismatches and Assorted
  Applications*, Oxford, Hilary 2006), fetched as PDF and read directly.
- **[P]** — written in the 2003 paper *Naïve Differences of Executable Code*.
- **[C]** — read directly from source code (bsdiff 4.3 release, or the
  `github.com/cperciva/bsdiff` repo).
- **[W]** — public statement by Percival outside the papers (web page, HN comment).
- **[3P]** — a third party's published measurement or paraphrase, not Percival.\n- **[I]** — my own inference or arithmetic, explicitly labelled.

---

## 0. Executive summary

There are four distinct artefacts, routinely conflated:

| Artefact | Status | Matching | Encoding |
|---|---|---|---|
| **bsdiff 4.3** (2005, BSD licence) | released, ubiquitous | suffix array + greedy left-to-right scan, 8-mismatch trigger, 50% extension | 3 streams (ctrl/diff/extra), fixed 8-byte ints, bzip2 each |
| **bsdiff 6** (thesis Ch. 2, 2006) | **never released** | FFT "block alignment" ∪ suffix-array "local alignment" → pruned **shortest-path DP** over 64 candidate offsets per byte (in the prototype, a layered beam — see §8) | 4 selectable difference modes, **difference map split off and BWT+arithmetic coded**, base-128 varint ctrl, per-stream choice of zlib/bzip2 |
| **`github.com/cperciva/bsdiff`** (2012–2013) | released, BSD | refactored 4.3 alignment + optional block-parallel matching using the thesis *Epilogue*'s FFT similarity digest | still `BSDIFF40` format; **none** of the Ch. 2 encoding work |
| **`Julian-Rederle/bsdiff6`** (CISPA, Feb 2026) | released, Rust | independent reimplementation of bsdiff 6's block + local + **combination** alignment, from Percival's private C prototype | deliberately `BSDIFF40`-compatible; **none** of the Ch. 2 encoding work |

Two things that do **not** exist: **bsdiff 5** and **bsdiff 7**. See §6.1.

### The size claim has four different numbers, and they do not agree

| Source | Claim | Baseline | Status |
|---|---|---|---|
| daemonology.net, **2004-12 → 2005-06** (since deleted) **[W]** | **25% smaller** | bsdiff **4.2** | Percival, primary |
| daemonology.net, 2005-08 → today **[W]** | *"roughly 20% smaller"* | unstated | Percival, primary |
| My arithmetic across the 2003 and 2006 tables **[I]** | −22.5% aggregate | 2003 paper's BSDiff (≈ v4.0) | cross-table, uncontrolled |
| CISPA/SpaceSec 2026, from Percival's **actual C prototype** **[3P]** | **≈4.8% smaller** | bsdiff 4 (Endsley port) | controlled, but see below |

The 2026 measurement is the only independent, controlled one — and it is a factor of
four below the claim. Reconciling it is the single most important analytical result
in this document, because it determines which of Percival's ideas are actually worth
porting. See §4.7 and §9 for the full argument; the short version **[I]**:

- their reimplementation is **"compatible with the bsdiff4 patch format"**, so it
  implements bsdiff 6's *alignment* and **none of its encoding** (no Lilliputian /
  Blefuscuan differences, no difference-map split, no BWT, no varints). They say so
  themselves: *"The prototype of bsdiff6 available to us lacked certain packaging
  features."*
- their corpus is ~8 executables out of 19 pairs, the rest being PNG, JPEG, an xz
  archive, a disk image, YAML and Python — inputs on which bsdiff 6's entire
  second-order-address rationale does not apply.

So **≈4.8% is a lower bound measuring the alignment work alone, diluted across
non-executable inputs.** It does not refute the 20–25% claim for whole-tool,
executable-only comparison; it does mean the alignment changes alone are worth
single digits, and it relocates most of the expected gain into the encoding.

The gain is split across three separable levers:

1. **Better selection of matches** — a pruned shortest-path formulation with an
   explicit cost model, replacing the greedy scan. *Independently identified as the
   improving factor* by the 2026 analysis **[3P]**.
2. **Better recovery of long, heavily-mismatched regions** — the FFT block
   alignment finds address tables the suffix-array method provably cannot see.
3. **Better encoding of the residual** — carry-propagating subtraction instead of
   bytewise, plus splitting the diff stream into a *where* stream and a *what*
   stream. **Unmeasured by anyone**, and the residual explanation for the gap
   between 4.8% and 20–25%.

---

## 1. bsdiff 4.3 — what the released tool actually does

Source read: `bsdiff.c` / `bspatch.c` at tag `v4.3` **[C]**.

### 1.1 Patch container

```
0   8   "BSDIFF40"
8   8   length of bzip2ed ctrl block
16  8   length of bzip2ed diff block
24  8   length of new file
32  ..  bzip2(ctrl block)
..  ..  bzip2(diff block)
..  ..  bzip2(extra block)
```

Three streams, each independently bzip2-compressed at level 9. Integers are
**fixed 8-byte little-endian sign-magnitude** (`offtout`/`offtin`: low 63 bits
little-endian, bit 63 of the last byte is the sign). Note this is *not* a varint —
bsdiff 4 leans on bzip2 to squeeze the seven high zero bytes of every field. The
thesis explicitly moves to base-128 varints for bsdiff 6 (§4.6 below) **[T][C]**.

### 1.2 Control triples

`bspatch` loop **[C]**:

```c
while (newpos < newsize) {
    read ctrl[0], ctrl[1], ctrl[2];            /* three 8-byte ints */
    read ctrl[0] bytes from diff stream into new+newpos;
    for (i = 0; i < ctrl[0]; i++)              /* byte-wise ADD, mod 256 */
        if (oldpos+i >= 0 && oldpos+i < oldsize)
            new[newpos+i] += old[oldpos+i];
    newpos += ctrl[0]; oldpos += ctrl[0];
    read ctrl[1] bytes from extra stream into new+newpos;
    newpos += ctrl[1]; oldpos += ctrl[2];      /* signed seek in old */
}
```

So a triple is `(add_len, extra_len, seek)`:
- `add_len` bytes: `new[i] = diff[i] + old[i]` with **unsigned char wraparound**
  (no carry between bytes — this is the "bytewise subtraction" the tool is
  arguably named after **[W]**);
- `extra_len` bytes: literal insert from the extra stream;
- `seek`: signed adjustment to the old-file cursor.

There is no explicit COPY: a run that matches exactly is an ADD whose diff bytes
are all zero, which bzip2 flattens to nothing. This is the key trick — copies and
near-copies use the same opcode, so the differ never has to decide "is this a copy
or not".

### 1.3 Matching: qsufsort + greedy scan

`qsufsort` (Larsson–Sadakane) over the **old file alone**; `search()` is a plain
binary search over the suffix array using `memcmp`, returning the longest exact
match of `new[scan..]` and its position `pos`. Total `O((n+m) log n)` **[C][W]**.

The outer scan is where the "mismatch tolerance" lives **[C]**:

```c
for (scsc = scan += len; scan < newsize; scan++) {
    len = search(I, old, oldsize, new+scan, newsize-scan, 0, oldsize, &pos);
    for (; scsc < scan+len; scsc++)
        if (scsc+lastoffset < oldsize && old[scsc+lastoffset] == new[scsc])
            oldscore++;
    if ((len == oldscore && len != 0) || (len > oldscore + 8)) break;
    if (scan+lastoffset < oldsize && old[scan+lastoffset] == new[scan])
        oldscore--;
}
```

`oldscore` is maintained as *the number of bytes in `new[scan .. scan+len-1]` that
already match at the **previous** alignment offset* `lastoffset`. The scan advances
one byte at a time and stops when either:

- `len == oldscore && len != 0` — the incumbent offset explains the whole match, so
  keep going with the incumbent; or
- **`len > oldscore + 8`** — the new offset explains at least **8 bytes** the
  incumbent does not. This is the threshold quoted in the paper **[P]**:
  *"we only record regions which contain at least 8 bytes not matching the
  forward-extension of the previous match"*, and it is the reason approximate
  matches, not exact ones, come out of the process — *"This process of extending
  the matches is why we ignore any matches which are not 'better' than the previous
  match by 8 bytes."* **[P]**

### 1.4 The 50% extension rule

Once a new seed is accepted, the previous alignment is extended forward and the new
one backward **[C]**:

```c
s = 0; Sf = 0; lenf = 0;
for (i = 0; lastscan+i < scan && lastpos+i < oldsize; ) {
    if (old[lastpos+i] == new[lastscan+i]) s++;
    i++;
    if (s*2 - i > Sf*2 - lenf) { Sf = s; lenf = i; }
}
```

`s*2 - i` is exactly **(matches − mismatches)**. The loop keeps the prefix length
`lenf` maximising that quantity. **[I]** This is equivalent to the paper's stated
rule — *"subject to the requirement that every suffix of the forward-extension (and
every prefix of the backwards-extension) matches in at least 50% of its bytes"*
**[P]** — because a suffix of the extension is worth including iff it contains at
least as many matches as mismatches. Note the user-facing "extends while
(matches − mismatches) increases by 8" phrasing conflates two separate constants:
the **8** is the seed-acceptance trigger (§1.3); the extension rule has **no 8**, it
is a plain argmax of `2s − i`.

### 1.5 Overlap resolution

Forward extension of segment *k* and backward extension of segment *k+1* can
overlap. bsdiff picks the split point maximising the total match count **[C]**:

```c
overlap = (lastscan+lenf) - (scan-lenb);
s = 0; Ss = 0; lens = 0;
for (i = 0; i < overlap; i++) {
    if (new[lastscan+lenf-overlap+i] == old[lastpos+lenf-overlap+i]) s++;
    if (new[scan-lenb+i] == old[pos-lenb+i]) s--;
    if (s > Ss) { Ss = s; lens = i+1; }
}
lenf += lens - overlap;  lenb -= lens;
```

**[I]** A one-pass argmax over the overlap of (matches gained by giving byte *i* to
the left segment) − (matches gained by giving it to the right segment) — i.e. the
optimal split, computed exactly, in linear time.

### 1.6 Known limitation, stated by the author

> "The heuristic bsdiff uses for identifying 'mostly-matching' regions is just
> that — a heuristic — and the larger the amount of data you have the higher the
> chance that bsdiff will try to build a mostly-matching region out of a
> coincidental sequence of matching bytes." **[W]** (HN 13121344, 2016-12-07)

This is a direct consequence of the greedy scan: the 8-byte trigger is a *fixed*
threshold that does not scale with `n`, so the false-positive rate grows with file
size. The thesis's shortest-path formulation (§4.5) is the principled fix.

---

## 2. The 2003 paper — *Naïve Differences of Executable Code*

Full text read **[P]**. This is the canonical reference for bsdiff 4 **[W]**.

The two "important observations" motivating bytewise subtraction:

> "First, in the regions of an executable file not directly affected by a
> modification, the differences will generally be quite sparse. Not only will the
> modified addresses constitute only a small portion of the compiled code, but
> addresses are most likely to only change in their least significant one or two
> bytes. Second, data and code tends to be moved around in blocks; consequently,
> locality of reference will lead to a large number of different (nearby) addresses
> being adjusted by the same amount. These two observations lead to the important
> fact that if the regions in two versions of an executable program which correspond
> to the same lines of source code are matched against each other, the bytewise
> differences will be mostly zero, and even when non-zero will take certain values
> far more often than others — in short, the string of bytewise differences will be
> highly compressible."

On stream splitting:

> "While these three files together are slightly larger than the original target
> file, the control and difference files are highly compressible; in particular,
> bzip2 tends to perform remarkably well (probably due to the highly structured
> nature of these two files)."

Results (19 Alpha pairs, weighted by √filesize): bzip2 35.5%, Xdelta 19.4%,
.RTPatch 9.8%, Exediff 7.3%, **BSDiff 8.6%**. FreeBSD 4.7 security corpus (97
binaries, 36,397,575 B): Xdelta 3,288,540 (11.0×), .RTPatch 749,710 (47.7×),
**BSDiff 621,277 (58.3×)**.

Conclusion sentence, which the thesis later overturns:

> "While its performance does not quite match that of a platform-specific tool, we
> believe that BSDiff probably attains close to the best possible performance from a
> platform-independent tool."

---

## 3. The thesis's model of *why* executable diffs are hard

**[T]** §2.2 gives the taxonomy the whole design rests on:

- **Zeroth-order changes**: artefacts of compilation itself (timestamps, host
  stamps, PE header time field). Small; ignored.
- **First-order changes**: bytes directly attributable to changed source. *"Since
  the bytes of executable code affected by first-order changes belong to
  instructions which are essentially new, they are best compressed by
  well-understood (non-delta) compression algorithms."* → these become the **extra**
  stream.
- **Second-order changes**: *"those which are induced indirectly by first-order
  changes. Every time bytes of code are added or removed, the absolute addresses of
  everything thereafter, and any relative addresses which extend across the modified
  region, are changed. … a single line of modified source code can cause up to 5–10%
  of the bytes of executable code in a file to be modified."*

> "efficient delta compression of executable code can be largely considered to be
> the problem of locating and compactly encoding these second-order changes." **[T]**

And the design consequence:

> "Since first-order and second-order changes are fundamentally different in nature,
> it is important to distinguish between them, so that they can be handled
> differently." **[T]** §2.3

With a robustness note worth internalising (footnote 4, §2.3):

> "Providing that the encodings used for the first-order and second-order
> differences are reasonably 'stable' (i.e., not overly influenced by the presence of
> a small number of bytes which do not follow the same model as the majority), it is
> not necessary that this matching be exactly correct."

The thesis also quantifies why executable alignment differs from DNA alignment:

> "In DNA sequence alignment, the percentage of matched base pairs is usually quite
> high (often 90% or more) while the length of matched regions is quite low (tens of
> base pairs) …; when matching executable code, in contrast, the percentage of
> matched characters can be lower (often down to 50%) but the matched regions tend to
> be longer (hundreds or thousands of characters)." **[T]**

---

## 4. bsdiff 6 — the thesis algorithm, in detail

**[T]** §2.4–§2.7. Named in the text: *"We have implemented this method in a tool
named 'bsdiff 6'"*.

### 4.1 The DP baseline it is approximating

> "A trivial quadratic algorithm can be obtained by dynamic programming (or
> equivalently, by finding the shortest path through a directed graph); each byte
> within the new file can be aligned against any position in the old file, or left
> unmatched. Scores are computed based on the number of matched bytes …, the number
> of mismatched bytes …, the number of unmatched bytes, and the number of
> realignments (positions where two successive bytes in the new file are not aligned
> against successive bytes in the old file). Unfortunately, such a quadratic
> algorithm is far too slow to be useful in practice. This algorithm is closely
> related to the well-known Needleman-Wunsch algorithm…" **[T]**

**This is the objective function.** Four terms: matched, mismatched, unmatched,
realignments. Everything else in §2.4–2.6 is machinery to make it tractable.

### 4.2 Block alignment (FFT) — §2.4

> "Taking the old file S, we construct an index S̄ with k = 2 and L = 4√(n log n) in
> time linear in the file size. Next, we divide the new file T into blocks of length
> √(n log n), and using the index S̄ locate where in the old file each such block
> matches best." **[T]**

Footnote: *"The constant 4 is to some extent arbitrary; the FFT and block lengths
can be adjusted depending upon the amount of time available, the importance of
obtaining correct matchings, and the importance of identifying small matched
blocks."*

Then boundary refinement — **note the power-of-two snapping, which is a genuinely
executable-aware trick that costs nothing**:

> "We scan, first forwards, then backwards, through the new file, considering in turn
> each of the boundaries between aligned blocks. Based on the contents of the new and
> old files, we move these boundaries in order to reduce the number of mismatches;
> where a range of possible boundaries would result in the same number of mismatches,
> we place the boundary on a multiple of the largest power-of-two possible. We also
> remove blocks which, as a result of this process, shrink below some threshold; this
> is done because such blocks would be too small to have been found by our algorithm
> for matching with mismatches, so we have no reason to think that they are correctly
> aligned." **[T]**

Footnote 7 gives the reason: *"Due to issues involving instruction prefetching and
code caches, compilers usually attempt to align blocks of code on 8-, 16-, or
32-byte boundaries."*

Final filter (dropped when block alignment feeds the combined algorithm):

> "We now make a final pass through the list of blocks, counting the number of
> matching characters, and remove any blocks or parts of blocks which fail to match
> in at least 50% of their characters, on the assumption that if the best matching we
> can find for a region is that poor, then it probably doesn't match at all, but is
> instead a completely new region (i.e., a first-order change)." **[T]**

Cost: `O(m log n + n)` time, `O(n^(1/2+ε))` memory. Percival notes the constant is
better than it looks because the work is single-precision FFTs, which vendors
optimise hard (SSE/Altivec/3DNow are named).

**Why it exists** — the case the suffix-array method structurally cannot solve
(§2.5, footnote 11):

> "One case where this occurs is in tables of addresses, where there may be over a
> thousand bytes which mismatch in one or two out of every four bytes (the low order
> byte(s) of each 32-bit address). Since the largest perfect matches within such a
> block are only three bytes long, the block will remain entirely undetected by a
> method which relies only upon locally optimal alignments." **[T]**

### 4.3 Local alignment (suffix array) — §2.5

Differs from bsdiff 4.3 in one important respect: it suffix-sorts **`S#T#`** — the
concatenation of both files — computes the **LCP array** in linear time
(Kasai et al.), and scans the sorted suffix list in both directions to obtain, for
each position in T, the best-matching offset in S and its match length. **[T]**

(bsdiff 4.3 instead suffix-sorts only `old` and does a `memcmp` binary search per
position **[C]** — asymptotically the same but a larger constant and no LCP array.)

Footnote 10 records his verdict on the alternatives, which still reads correctly:

> "An alternative, and more commonly used approach, involves hashing fixed-length
> blocks, which is faster but often fails to find the longest matching substrings.
> Suffix trees are also commonly used, but in light of recent algorithmic
> improvements have no advantages over suffix sorting." **[T]**

Seed selection, then extension:

> "Starting at the beginning of the file, we set the 'current' alignment to be the
> offset … where the string starting at byte zero matches the furthest, and allow the
> 'next' alignment to iterate forwards through our array of locally optimal
> alignments. Any time that the 'current' alignment, extended forward to the end of
> the matching block associated with the 'next' alignment, contains enough mismatches
> (typically 8 mismatches is a reasonable threshold), the 'current' alignment is
> output and replaced by the 'next' alignment. This produces a list of
> non-overlapping alignments; starting from these 'seeds', we now extend the
> alignments forwards and backwards to the extent that they continue to match at
> least 50% of characters." **[T]**

**[I]** This is bsdiff 4.3's algorithm restated, confirming that "local alignment"
≈ released bsdiff.

### 4.4 Combining them: the pruned shortest path — §2.6

This is the part with no analogue in any released bsdiff, and the most directly
portable idea in the thesis.

> "In order to gain the advantages of both block alignment and local alignment, we
> return to dynamic programming, but avoid the O(nm) running time which would result
> from allowing arbitrary alignments by first pruning the graph using block and local
> alignment." **[T]**

Block alignment runs as in §2.4 **minus the final 50% filter**. Local alignment runs
only up to producing the longest-match-per-position array. Then:

> "Instead of including nm vertices corresponding to the n positions in the old file
> against which each of the m bytes in the new file could be aligned, we include only
> 64m. The **64 vertices** associated with each position `pos` in the new file
> comprise: **31** computed from the offsets ([position in old file] minus [position
> in new file]) associated with the longest matching substrings starting at positions
> immediately following `pos`; **31** from offsets which have the shortest distances
> from the origin to position `pos − 1`; **one** corresponding to the matching
> predicted by block alignment; and **one** which is the 'not matched' position." **[T]**

The cost model, verbatim:

> "At byte zero in the new file, all the vertices are initialized to distance zero;
> subsequently, the distance from (x, y) to (x + 1, y + 1) is **0** if the
> corresponding bytes from the old and new files match, and **2** if they mismatch;
> the distance from the 'not matched' position for `pos − 1` to the 'not matched'
> position for `pos` is **1**; and a distance of **20** is assigned to the remaining
> paths from one step to the next." **[T]**

> "Once the last byte of the new file is reached, we find the vertex from that final
> step with the minimum distance, and follow its path backwards; in so doing, we have
> constructed an alignment of the new file against the old file." **[T]**

Two footnotes matter:

- fn 12: *"Like many other numbers in this chapter, the values 64 and 31 are chosen
  simply because they work well without being excessively slow."*
- fn 13: *"These values are obtained by experimental observation, and will not be
  ideal for all input data."*

**[I] Reading the cost model as a cost model.** In bits-ish units where a matched
byte is free: a mismatched byte costs 2, an unmatched (literal) byte costs 1, and a
*realignment* (changing offset, i.e. emitting a new control triple) costs 20. So the
model says a control triple is worth about 20 mismatched-byte-halves ≈ 10
mismatches ≈ 20 literal bytes. That ratio is the interesting artefact: it prices the
seek/segment overhead against residual quality, which is exactly the trade bsdiff
4.3's hard-coded `+8` makes blindly. Note also that literals are *cheaper* than
mismatches (1 vs 2), which encodes the belief that an unmatched byte compresses
better in the bzip2'd extra stream than a nonzero diff byte does in the diff
stream.

**[I]** The candidate-set construction is the other half of the idea: the DP is over
*offsets*, not over old-file positions, and the offset candidates at each byte are
(a) offsets suggested by upcoming longest-matches (lookahead), (b) offsets that are
currently cheap (incumbents, carried forward — this is what makes long runs stable),
(c) the FFT block-alignment suggestion, (d) unmatched. That is a beam search over
offsets with a 64-wide beam, evaluated per byte.

### 4.5 Delta encoding — §2.7

**Control stream.** *"The matching is easily encoded by listing, in order of the new
file, the offsets and sizes of blocks which are copied from the old file and the
sizes of new blocks. We encode these integers in little-endian, base-128 format, and
use the most significant bit of each byte to identify the most significant byte"*
**[T]**. (bsdiff 4.3 uses fixed 8-byte fields **[C]** — a real regression relative
to the thesis.)

**Extra stream.** Unmatched regions concatenated, as in bsdiff 4.

**Four difference modes, one chosen per patch.** The problem with bytewise
subtraction, stated exactly:

> "The presence of multi-byte integers diminishes the potential compression somewhat,
> however, since a bytewise subtraction of `1200 - 11FC` yields `0104`, while
> `1210 - 120C` yields `0004`; to avoid this, we turn to multi-precision arithmetic
> subtraction, and write the difference in balanced notation (i.e., with digits in
> the range [-128 … 127]); thus `1200 - 11FC = 1210 - 120C = (00)(04)`, while
> `1280 - 11C8 = 12B8 - 1200 = (01)(-48)`." **[T]**

The four candidates:

1. **bytewise** differences — *"which perform reasonably well regardless of machine
   byte order"* (this is bsdiff 4's only mode);
2. **Lilliputian** multi-precision difference (one endianness);
3. **Blefuscuan** multi-precision difference (the other endianness);
4. **correction map** — *"simply containing the values from the new file in each
   place that the new and old files differ"*.

Footnote 15 on mode 4: *"We have yet to find any files (not specifically constructed
for this purpose) for which the 'correction map' results in smaller patch sizes; but
it seems a natural item to include."*

**[I]** Modes 2 and 3 are the substantive addition: carry-propagating subtraction
over the whole matched region means a 32-bit pointer that moved by a small delta
produces `(00)(00)(00)(δ)` instead of a borrow-corrupted 4-byte pattern. Balanced
digits in [−128,127] keep the result one byte per position (no carry-out to encode)
while making "moved by a small amount" map to a small-magnitude digit near the
low end and zeros elsewhere. This is essentially free to implement and is a strict
generalisation of what bsdiff 4 does.

**Splitting the difference string in two** — the highest-leverage encoding idea:

> "Now, we note that a difference string is in fact a union of two entirely different
> data sets. It specifies **where** in the executable file addresses have been
> modified, and it also specifies **by what amount** they have been modified. Just as
> combining similar data before compression tends to improve compression ratios,
> combining dissimilar data before compression tends to reduce compression ratios, so
> we split this string into two parts: First, a **'difference map'**, which is of the
> same length but contains bytes equal to zero or one, depending upon whether the
> corresponding byte is nonzero, and second, a **'non-zero difference string'**,
> containing all the non-zero bytes without the intervening zeroes. In this manner,
> we separate **locally repetitive** data (as noted before, in any region, the
> differences tend to take a small number of values repeatedly) from **globally
> structured** data (as a result of instruction encoding lengths and the positions
> within instructions where addresses are encoded, the locations where corrections
> must be made tend to form distinctive, and compressible, patterns)." **[T]**

Footnote 16, in full: *"Birds of a feather compress better together."*

**Per-stream compressor choice.**

> "The control string, non-zero difference string, and extra string are compressed
> independently, either with zlib (a Lempel-Ziv compressor) or libbzip2 (a block
> sorting compressor); in general, the control and non-zero difference strings
> compress most efficiently with zlib, since they contain local repetition, while the
> extra string compresses best with libbzip2, since executable code contains
> significant structure, which a block sorting compressor can utilize." **[T]**

**The difference map gets its own codec** — this is not a general-purpose
compressor:

> "Recognizing that it contains entirely structural redundancies, we first take the
> **Burrows-Wheeler transform** of the data, which serves to cluster related data
> together — in this case, causing the 1s to cluster together; next we **enumerate
> the positions of the 1s**, thus forming a strictly increasing sequence; finally, we
> **recursively divide this sequence in half, and encode the value at the midpoint
> using arithmetic compression**." **[T]**

**[I]** That last step is binary-interpolative coding (Moffat–Stuiver): given a
sorted list and known endpoints, the midpoint is bounded, so it costs
`log2(hi−lo+1)` bits at most, and clustered runs cost near zero. Applying BWT to a
0/1 map *first* is the unusual move — it turns positional structure into runs, and
then interpolative coding on the run positions is near-optimal. The patch header
records "the position of the EOF character after the Burrows-Wheeler transform has
been performed" so the inverse BWT can be run **[T]**.

**Patch file layout (bsdiff 6)** — five parts **[T]**:

- header: magic; a byte giving the **differencing mode** (correction / bytewise /
  Lilliputian / Blefuscuan) and the **compression method** (none / zlib / libbzip2)
  for each of ctrl, non-zero-diff and extra; sizes of new and old files; **MD5 of
  both files**; compressed and uncompressed sizes of ctrl, non-zero-diff, extra;
  compressed size of the difference map; **BWT EOF position**;
- compressed control string;
- compressed non-zero difference string;
- compressed difference map;
- compressed extra string.

Footnote 18 pre-empts the obvious objection about MD5 (post-Wang collisions):
*"we include these hashes simply as a safeguard against error, (whether human, in
the form of attempting to apply a patch to the wrong file, or computer, in the form
of incorrectly applying the patch)."*

### 4.6 Published bsdiff 6 results

**Upgrade corpus** (15 DEC Alpha pairs, mean of per-file ratios weighted by
√filesize) **[T]** Table 2.1:

| | bzip2 | Xdelta | Vcdiff | zdelta | .RTPatch | Exediff | **bsdiff 6** |
|---|---|---|---|---|---|---|---|
| avg | 36.22% | 20.83% | 19.66% | 19.52% | 10.88% | 8.41% | **7.67%** |

> "Exediff and bsdiff 6 being optimal in 7 and 8 cases respectively; overall bsdiff 6
> performs slightly better than Exediff." **[T]**

**Security corpus** (FreeBSD 4.7-RELEASE → RELENG_4_7, 82 files, 32,119,859 B)
**[T]** Table 2.2:

| | bzip2 | Xdelta | Vcdiff | zdelta | .RTPatch | **bsdiff 6** | bsdiff 6 block-align only |
|---|---|---|---|---|---|---|---|
| total | 12,368,169 | 2,979,432 | 2,075,361 | 1,283,736 | 608,064 | **409,522** | 442,981 |
| avg | 38.51% | 9.28% | 6.46% | 4.00% | 1.89% | **1.27%** | 1.38% |

Two observations from the text worth carrying:

> "the gap between best and worst performance is much larger for security patches;
> while the various tools produced 'upgrade' patches averaging from 7.67% to 20.83%
> … the 'security' patches averaged from 1.27% to 9.28% — a difference of more than
> a factor of seven. This reflects the greater relative importance of second-order
> changes in the files; where the source code changes … are small, the resulting
> patch sizes depend almost totally upon the efficiency of encoding second-order
> changes." **[T]**

> "In light of the vastly reduced memory consumption, it is useful to note the delta
> compression performance of bsdiff 6 when operating in 'block alignment only' mode:
> Given that the patches are on average only 8% larger, this may be a method worth
> considering further." **[T]**

**[I] The FFT block alignment alone, with no suffix array at all, gets within 8% of
the full algorithm on security updates** while using `O(√n)`-ish memory. On this
corpus the suffix array is nearly redundant; the win is in alignment quality and
encoding, not in exhaustive match finding.

### 4.7 Quantifying the gain — and why the numbers disagree

#### (a) Percival's own numbers, and that they changed

The **first** public claim, from the "Note to corporate users" that stood on
daemonology.net from late 2004 until bsdiff 4.3 shipped in mid-2005 and was then
deleted **[W]**:

> "In my doctoral thesis, I present **bsdiff 6.0**, which produces patches typically
> **25% smaller** than those produced by **bsdiff 4.2**."

The **current** claim, live since August 2005 **[W]**:

> "A far more sophisticated algorithm, which typically provides **roughly 20% smaller
> patches**, is described in my doctoral thesis."

**[I]** The 25% figure names a specific baseline (4.2); the 20% figure names none.
Both predate or coincide with 4.3. Note that **the thesis itself contains no
bsdiff-6-vs-bsdiff-4 comparison at all** — Tables 2.1 and 2.2 compare bsdiff 6 only
against Exediff, .RTPatch, zdelta, Vcdiff, Xdelta and bzip2. Whatever measurement
produced "25%" and "20%" was never published.

#### (b) My cross-table arithmetic: −22.5%

**[I]** The 2003 paper (Table 1, BSDiff ≈ v4.0) and the thesis (Table 2.1, bsdiff 6)
share 15 rows:

| pair | BSDiff 4 | bsdiff 6 | Δ |
|---|---|---|---|
| agrep 4.0→4.1 | 6,066 | 4,265 | −29.7% |
| glimpse 4.0→4.1 | 31,720 | 24,642 | −22.3% |
| glimpseindex 4.0→4.1 | 21,559 | 16,240 | −24.7% |
| wgconvert 4.0→4.1 | 15,806 | 12,432 | −21.3% |
| agrep 3.6→4.0 | 53,490 | 44,327 | −17.1% |
| glimpse 3.6→4.0 | 130,210 | 109,680 | −15.8% |
| glimpseindex 3.6→4.0 | 97,782 | 80,447 | −17.7% |
| netscape 3.01→3.04 | 302,431 | 212,032 | −29.9% |
| gimp 0.99.19→1.00.00 | 284,278 | 219,684 | −22.7% |
| iconx 9.1→9.3 | 44,961 | 31,632 | −29.6% |
| gcc 2.8.0→2.8.1 | 121,371 | 88,022 | −27.5% |
| rcc 4.0→4.1 | 289 | 187 | −35.3% |
| apache 1.3.0→1.3.1 | 38,278 | 25,927 | −32.3% |
| apache 1.2.4→1.3.0 | 180,981 | 163,249 | −9.8% |
| rcc 3.2→3.6 | 33,136 | 22,691 | −31.5% |
| **total** | **1,362,358** | **1,055,457** | **−22.5%** |

Mean per-file reduction −24.5%.

**Caveats, and they are serious.** The two tables were produced at different times
and the Exediff column also moved between them (e.g. glimpse 3.6→4.0
104,350 → 106,154; apache 1.3.0→1.3.1 40,460 → 42,038), so the corpora or the bzip2
version were not byte-identical. More importantly the baseline is the **2003**
BSDiff — roughly v4.0 — so this figure absorbs every improvement made in 4.1, 4.2
and 4.3, which are all in the *released* tool. It is therefore an **upper bound** on
what bsdiff 6 adds over bsdiff 4.3, not an estimate of it.

#### (c) The independent 2026 measurement: ≈4.8%

Rederlechner, Planta and Abbasi (CISPA), *"One Small Patch for a File, One Giant Leap
for OTA Updates"*, SpaceSec 2026 **[3P]** — verified by downloading the paper:

> "For bsdiff6, only a theoretical description is publicly available. **We obtained
> access to Percival's original C prototype but were not permitted to publish it**,
> and therefore reimplemented bsdiff6 based on the prototype and its formal
> description."

> "Across all inputs, bsdiff6 reduces patch size by an average of **≈ 4,8%**,
> outperforming bsdiff4 in 18 of 19 cases."

> "HDiffPatch, compared to bsdiff4, achieves an average patch size reduction of
> **6,6%**, exceeding bsdiff6's gain."

And their own discussion of the discrepancy:

> "This result does not align with Colin Percival's original claim of a ≈ 20%
> improvement… our experiments yielded only one case approaching 20%, four around
> 10%, and the remainder below 5%. Consequently, we cannot confirm the previously
> stated ≈ 20% reduction. Although the exact reason for this discrepancy remains
> unclear, our analysis suggests that **differences in patch packaging
> implementations may be a contributing factor. The prototype of bsdiff6 available to
> us lacked certain packaging features**, indicating that the packaging logic may have
> evolved over time."

#### (d) Reconciling them

**[I]** Two structural facts about the 2026 experiment explain most of the gap, and
both are stated in the paper itself:

1. **Their bsdiff6 does not implement bsdiff 6's encoding.** They describe it as
   *"a drop-in replacement compatible with the bsdiff4 patch format"* requiring
   *"no changes to patch application logic"*. A `BSDIFF40` patch has exactly three
   bzip2 streams and bytewise ADD. That excludes, by construction, **every one of
   §4.5's encoding contributions**: the Lilliputian/Blefuscuan carry-propagating
   differences, the difference-map/values split, the BWT + interpolative coder, the
   varint control fields, and the per-stream codec choice. Their number measures
   **alignment only**.

2. **Their corpus is mostly not executables.** The 19 pairs are Linux kernel, gcc/g++,
   BusyBox, libcurl, glibc, SQLite, OpenSSL, OpenCV (8 executable pairs) plus a U-Boot
   config, a GitLab Docker-Compose YAML, NumPy's `polynomial.py`, a Raspberry Pi OS
   image in both `.xz` and raw `.img` form, a PNG, a JPEG, and APKs. bsdiff 6's entire
   rationale is second-order address relocation in compiled code; on a JPEG or an xz
   archive it has nothing to exploit. Averaging over those inputs dilutes the
   executable-case gain. They effectively concede this: *"within OSTree's commit-based
   update model, diffing primarily targets system binaries rather than complete OS
   images, **a scenario where bsdiff6 shows superior performance**."*

   Their own metric critique makes the point sharper: *"the bsdiff6 result for
   `polynomial.py` increases patch size by only 59 bytes, yet skews the average
   percentage increase by ≈11%."*

**Conclusion [I].** The honest reading is:

- **bsdiff 6's alignment work alone is worth low-to-mid single digits** on a mixed
  modern corpus — measured, credible, and *behind* HDiffPatch's 6.6%.
- **bsdiff 6's encoding work has never been measured by anyone**, including
  Percival, in a form that separates it from the alignment.
- The 20–25% claim, if it was ever a real measurement, was a whole-tool comparison
  on 1990s executables. It should not be quoted as the expected gain from porting
  any single piece.

This inverts the naive reading of the thesis. The chapter spends most of its pages on
matching theory, but the matching is worth ~5%; the two pages on delta encoding are
the part nobody has tested and the only place the remaining 15 points could be.

## 5. Chapter 1 — the matching-with-mismatches theory

### 5.1 What it is *not*

**Negative finding [T]:** the thesis contains **no** suffix-tree + LCA construction,
**no** "kangaroo jumping" (Landau–Vishkin), and no citation of Landau or Vishkin.
Grep of the full extracted text for `kangaroo|Landau|Vishkin|lowest common
ancestor|LCA` returns nothing. The kangaroo-jump technique is a different lineage;
Percival's approach is FFT/convolution based (in the tradition of Fischer–Paterson
[17] and Atallah–Chyzak–Dumas [3], both cited), with the novel step being
**projection onto prime-order cyclic groups**.

### 5.2 The core idea

Define the **match count vector** `V ∈ R^n`, `V_i = Σ_j δ(S_{i+j}, T_j)`. Good
offsets are spikes in `V`; `V` is computable by FFT correlation in `O(n log n)`.

The novelty: project `V` from `R^n` down onto `R^{Z_p}` for a prime `p ≪ n`.

> "The signal remains, although its locations (i.e., the values x_i) are reduced
> modulo P; and the level of 'background noise' increases. Providing that P is large
> enough … we can find the set X mod P … by taking the largest values of this
> projection." **[T]**

Do this for `k` distinct primes and reconstruct the true offsets by the **Chinese
Remainder Theorem**. Because each correlation is only length `p`, the total work is
sublinear in `n`.

**Algorithm 1.1** (verbatim structure) **[T]**:

1. `k = ⌈log(2n/ε) / (log 8n − log(mt log n))⌉`,
   `L = 8n·log(2kn/ε) / (mp² − 8·log(2kn/ε))`, `k̂ = ⌈log n / log L⌉`.
2. `P = {x prime : L ≤ x < L(1 + 2/log L)}`.
3. For `i = 1..k`: pick `p_i ∈ P` and `Σ_i ⊂ Σ` with `|Σ_i| = ½|Σ|` uniformly at
   random; define `φ_i : Σ → {−1, 1}` by `φ_i(x) = (−1)^{|Σ_i ∩ {x}|}`.
4. Fold: `A^{(i)}_j = Σ_λ φ_i(S_{j+λp_i})`, `B^{(i)}_j = Σ_λ φ_i(T_{j+λp_i})`.
5. `C^{(i)} = ` cyclic correlation of `A^{(i)}` and `B^{(i)}` via FFT;
   `X^{(i)} = {j : C^{(i)}_j > mp/2}`.
6. For every `k̂`-tuple in `X^{(1)} × … × X^{(k̂)}`, CRT-reconstruct `x`; keep it if
   `x < n` and `x mod p_i ∈ X^{(i)}` for all remaining `i`.

**Complexity [T]:** index construction `O(n)`; matching
`O(n log(nt/ε) log m / (m p²))`, i.e. **sublinear in n**.

> "To our knowledge, this is the first algorithm for any problem in the field of
> approximate string matching which is sublinear in n for constant error rate
> k/m ≈ 1 − p and m = Θ(n^β) for some β > 0." **[T]**

Algorithm 1.2 speeds up candidate generation (take the `βp_i + t` largest `C^{(i)}_j`
rather than thresholding). **Algorithm 1.3** is the practical one — a Bayesian
reformulation:

> "we compute k, p_i, C^{(i)} as in Algorithm 1.2, computing the sum
> `Σ_i (C^{(i)}_j − mp/2)/σ_{p_i}(n,m,j)` for all j and finding the t largest values."

with a clamp `D^{(i)}_j = max(C^{(i)}_j, δ)` to bound the error probability for
adversarial `X`, and a priority queue so you *"start by considering the largest
elements of each vector D^{(i)} and stop once it is clear that the remaining sums
will be less than all of the t largest values found so far."* **[T]**

### 5.3 §1.6 "Final notes" — the practical adaptations

These are the parts a practitioner needs; several apply to any FFT-based matcher.

- **Note 1**: since `m < L ≤ p_i`, you correlate a length-`L` vector against a
  length-`m` vector, not two length-`L` vectors — replaces `log L` with `log m`.
- **Note 2**: instead of `φ: Σ → {−1,1}`, count matching characters directly. Costs
  `|Σ|×` more, but reduces required `L` by `|Σ|/4` and *"allows for general
  goodness-of-match functions δ to be used, which is important in some contexts."*
- **Note 3**: `φ: Σ → {ω^i}` (roots of unity, per Atallah et al.) halves `L` but
  forces complex arithmetic.
- **Note 4 — the one that matters for binaries**:

  > "computer binaries often have large regions containing mostly zero bytes. To
  > resolve these problems, we can borrow a technique which is often used in the
  > field of DNA sequence alignment: If a sequence is repeated several times, the
  > repeating characters can be replaced by 'ignore' symbols which φ maps to 0.
  > Alternatively, and somewhat better in the context of computer binaries,
  > commonly-occurring characters (such as the '0' byte) can be **partially masked,
  > by weighting them … by some factor depending upon the character frequency**. In
  > practice, we find that **weighting by the inverse square root of character
  > frequency** seems to produce good results." **[T]**

  Footnote 13 floats an alternative: *"deflate the input strings, and weight each
  individual character according to the inverse of the length of the string
  represented by the symbol in which it is found; … the weights so obtained would
  most likely be a good estimate of the 'information' transmitted by each character,
  and thus of the importance of it correctly matching."*

- **Note 5**: precompute the index at geometric lengths `L_i = 2^{-i} n` in
  `O(n log n)` producing an `O(n)` index, then pick the smallest `L_i ≥ L` at query
  time. Warns: *"the most convenient values 2^i are likely poor choices if the source
  data is computer-generated."*
- **Note 6**: `σ_{p_i}(n,m,j) ≈ nm/p_i` — use that.
- **Note 8**: apply several `φ` per prime and sum the `C^{(i)}` to avoid an unlucky
  mapping.
- **Note 9**: the practical acceptance test is whether the `C^{(i)}_j` are
  approximately normally distributed apart from the outliers; if so the method works
  regardless of how the inputs were generated.

### 5.4 Epilogue — the rapid string similarity metric

A late-breaking spin-off, and the only Chapter-1 machinery that *did* get released
as code (§7 below).

> "Consider how this vector [V] behaves when the two strings S, T differ by a small
> number of indels (and up to a constant proportion of substitutions). The vector V
> will have a few very large values, at positions corresponding to the offsets of the
> indel-free blocks which match between the two strings, and will otherwise have
> values which cluster around μm … For pairs of strings which are similar …, the
> positions in V which have unusually large values will translate directly into a
> large **variance**, whereas for strings which are dissimilar, the variance will be
> comparatively small.
>
> But wait! The vector V can be estimated as the cyclic correlation of two vectors
> A = φ(S) and B = φ(T). The cyclic correlation is computed as the inverse Fourier
> transform of the pointwise product of the Fourier transformed inputs. And **the
> variance of an inverse Fourier transform is the sum of the squared norms of the
> non-DC components**." **[T]**

Algorithm (verbatim) **[T]**: fix `φ: Σ → {−1,1}` and prime `p`. Given `S`:

1. `A_j = Σ_k φ(S_{j+kp})` for `j = 0..p−1` (fold);
2. `Ā_j = Σ_k A_j exp(2πijk/p)` (DFT);
3. `S̄_j = Ā_j² / sqrt(Σ_{j=1}^{(p−1)/2} Ā_j⁴)` for `j = 1..(p−1)/2` (energy
   spectrum, L2-normalised).

> "Then the dot product S̄ · T̄ of the 'digests' of two random strings S, T will be
> approximately equal to 1/2, the dot product S̄ · S̄ of the digest of a string with
> itself is equal to 1, and other pairs of strings will lie in between, in accordance
> with their similarity." **[T]**

**[I]** This is a fixed-size, shift-invariant, comparable-by-dot-product similarity
sketch. Shift invariance falls out of taking magnitudes of Fourier coefficients, so
two blocks that match at *any* alignment score high. That is precisely what you want
for a coarse "which old block does this new block correspond to" index, and it is
*not* what a shingling/minhash sketch gives you.

---

## 6. Version numbering, licensing, and what was actually claimed publicly

### 6.1 The numbering — settled by the thesis itself

Footnote 19 to §2.7, verbatim **[T]**:

> "bsdiff versions 0.8 through 4.2 were previously published by the author, and
> ranged from the traditional copy-and-insert to a method similar to what we have
> described here, but using only local alignment, and using a more primitive delta
> encoding. **Version 5 never existed except as a descriptor for experimental work
> which later became version 6.**"

So: **bsdiff 5 never existed.** 4.3 is the last public release of the 4 series.
bsdiff 6 is the thesis tool, and Percival's own deleted 2004 web copy calls it
**"bsdiff 6.0"** (§6.2).

**bsdiff 7 does not exist.** Searched exhaustively and found zero occurrences in: the
thesis; all 12 items on his publications page; all Wayback captures of the bsdiff page
2003–2019; all 133 monthly pages of his blog archive; the 51-commit GitHub repo;
HN Algolia full-text; and GitHub-wide code and issue search. **The highest version
number Percival has ever used publicly is 6.**

### 6.2 The deleted "Note to corporate users" — bsdiff 6 *was* commercially claimed

This is the most important primary source on the question, and it is not on the live
web. Between roughly December 2004 and June 2005, `www.daemonology.net/bsdiff/`
carried a section headed **"Note to corporate users:"** **[W]**:

> "In my doctoral thesis, I present **bsdiff 6.0**, which produces patches typically
> **25% smaller** than those produced by bsdiff 4.2. **Pursuant to its statutes, this
> software has been claimed by Oxford University.** For licensing details, please
> [contact me] and I will put you in contact with the appropriate people within the
> University."

The mailto target was **`bsdiff6@daemonology.net`** — a dedicated licensing alias.

I verified this myself against the Wayback capture at
`https://web.archive.org/web/20050304055057/http://www.daemonology.net/bsdiff/`
(archived 2005-03-04 05:50:57 GMT). It also appears at captures `20041204114048`,
`20050310083416`, `20050404102940`, `20050612005901` and `20050617233453`, and is
**absent** from `20040811000227` (before) and `20050819152418` (after — the first
4.3 capture, which already carries the "roughly 20% smaller patches" wording that is
still live today).

**So the answer to "is bsdiff 6 proprietary?" is: it was, for a period.** Oxford
claimed it under Statute XVI, Percival advertised commercial licensing through the
University, and there is a version number — **6.0** — attached to it in his own
words. This is the only place he has ever written a full version number for it.

**[I]** Note the direct chronological contradiction with the thesis, which was
submitted *after* the note was taken down:

| Date | Source | Says |
|---|---|---|
| 2004-12 → 2005-06 | website **[W]** | *"has been claimed by Oxford University"* |
| 2006 | thesis Appendix A **[T]** | *"it is not yet clear if the University will so elect"* |
| 2021-06 | HN **[W]** | *"They eventually did give me permission"* |

I have no source that resolves the 2004→2006 reversal.

### 6.3 Licensing status in the thesis, and its resolution

Appendix A, verbatim **[T]**:

> "An earlier version of the software described in Chapter 2, namely bsdiff 4.2, has
> been published under an open source license … Under the statutes of Oxford
> University, the remaining software may be claimed by the University if it 'may
> reasonably be considered to possess commercial potential'. At present, it is not
> yet clear if the University will so elect. If the University does not claim
> ownership of the remaining software, it will also be released under an open source
> license after it has been 'cleaned up' somewhat and prepared for widespread usage."

Resolution, 15 years later, on HN (2021-06-15, thread on university IP policy)
**[W]**:

> **hlandau**: "His website notes he implemented a superior version of this algorithm
> for his 2006 Oxford PhD thesis. I seem to recall him mentioning years ago that the
> IP for this superior version belonged to Oxford, and how he hoped they would at
> some point give him permission to release the code for it. As far as I'm aware
> nothing has been heard since…"
>
> **cperciva**: "**They eventually did give me permission, but I never got around to
> cleaning up the code for release.**"

So bsdiff 6 is **not proprietary and never was made proprietary** — Oxford granted
permission at some unstated date; it simply was never released. There is no
"commercial bsdiff"; the "commercial" tool in the comparison tables is Pocket Soft's
.RTPatch, a third-party product.

**Verified 2026-08-29 [C]:** enumerating all 33 public repositories under
`github.com/cperciva` finds exactly one delta-compression repo — `bsdiff`, described
as *"Automatically exported from code.google.com/p/bsdiff"*, last pushed
2015-09-08 — and it is the `BSDIFF40` tree described in §7. **No bsdiff 6 code has
been published anywhere**, five years after permission was granted.

**[I]** Corollary: there is no bsdiff 6 source to read. Everything in §4 above is
from the thesis prose. The design is fully specified enough to reimplement; the
constants (64/31, 0/2/1/20, `4√(n log n)`) are given but explicitly described as
empirical.

### 6.4 Percival's own public claims about the unreleased version

All **[W]**, primary sources (his own words):

- **www.daemonology.net/bsdiff/**: *"The algorithm used by BSDiff 4 is described in
  my (unpublished) paper Naive differences of executable code … A far more
  sophisticated algorithm, which typically provides roughly 20% smaller patches, is
  described in my doctoral thesis."*
- **HN 706496, 2009-07-15** (Courgette announcement): *"In my thesis I show that
  bsdiff (well, a slightly improved version of bsdiff which I never got around to
  releasing publicly) performed on par with or slightly better than Exediff; so I'm
  surprised that Courgette is getting this far ahead of bsdiff. Based on the numbers
  they've published (a 10 line source code patch to a 10MB executable resulting in a
  700kB bsdiff patch), it sounds like there's something weird going on which is
  breaking bsdiff — the FreeBSD kernel is roughly the same size as Chrome, but
  normally for a small (e.g., 10 line) source code patch I would see a bsdiff patch
  of between 50kB and 100kB."*
- **HN 2578873, 2011-05-24** (Courgette): *"I'm surprised they were able to beat it
  by such a margin. Technology marches on. I have ideas for improving bsdiff, too."*
- **HN 12962177, 2016-11-15**: *"Some of that, and some further improvements, made
  their way into https://github.com/cperciva/bsdiff. But I don't think anyone is
  using that new code; the company which was paying for me to work on it decided not
  to continue with that project before I had a chance to finish polishing the work."*
- **HN 13121192 / 13121344, 2016-12-07**: bsdiff is designed to run **file by file**
  (*"running file-by-file is how bsdiff was designed to operate … I wrote it for
  FreeBSD Update, which operates on a file-by-file basis"*), and on concatenated
  input its region heuristic degrades (quoted in §1.6 above).
- **HN 27521415, 2021-06-15**: *"The second chapter of my thesis describes the
  version of bsdiff I wrote as part of my doctorate."*
- **Debian bug #632585, 2011-07-04** — posted by Percival directly to the public bug
  log: *"I'm not going to commit them to the central bsdiff repo … because there's
  bigger issues I need to work on and I don't want to muck around with minor changes
  like this when **I have a complete rewrite pending**."*
- **Debian bug #632585, 2011-09-23** (private reply, forwarded into the public bug by
  the maintainer two days later — so archived, but not posted publicly by Percival):
  asked whether anything newer than 4.3 existed, he answered
  *"**That's the latest code. Everything new than that is ideas floating around in my
  head waiting to be translated into code.**"* This is the most explicit dated
  statement that nothing past 4.3 was ever released, and it *predates* the 2012
  GitHub work by a year.
- **GitHub `cperciva/bsdiff` issue #2, 2018-06-03** — asked directly whether he had
  released the thesis algorithm: *"**Sort of. I took bits and pieces from my thesis
  work and made some further improvements.**"* (Issue still open.) This is the
  definitive statement on what the 2012 repo is: fragments, not bsdiff 6.
- **GitHub `cperciva/bsdiff` issue #4, 2021-01-07** — on the 403'd tarball, and *not*
  about versions: *"Yes. There are security issues in that code; I've been pointing
  people at the code in FreeBSD instead (which has the fixes)."*
- **BSDCan '07 Portsnap slides** (not listed on his publications page) — the only
  algorithmic remark in any of his talks: *"Side note: Part of the reason bsdiff is so
  efficient is that it is the first delta compressor designed with an awareness of
  byte substitutions."*
- **A direct question he did not answer.** HN 24539084, 2020-09-21, hlandau: *"did you
  ever succeed in freeing the enhanced version of bsdiff from Oxford University?"* —
  zero replies **[3P]**.

**No conference talk on bsdiff internals exists.** His publications page lists
BSDCon '03 (*An Automated Binary Security Update System for FreeBSD* — the
freebsd-update system, not bsdiff internals), BSDCan '05 (cache timing), BSDCan '06
(coding by contract), BSDCan '09 (scrypt), AsiaBSDCon '18 (kernel boot profiling).
The thesis and the 2003 paper are the only algorithmic write-ups.

---

## 7. What Percival *did* release after 4.3: `github.com/cperciva/bsdiff`

Cloned and read **[C]**. Copyright headers say 2003–2005, 2012. `git log` dates the
whole tree to **July–November 2012** (plus a 2013-08 library sync): a stylistic
refactor of 4.3 in July 2012 (*"Stylize the code. Was my space bar broken back in
2003?"*), then the block-matching/parallel/random-access work in
15–21 November 2012. 51 commits, **no tags, no releases, no README**.

The client is named in the tree: commit `02d55cb255` (2012-11-21) ends with
`Submitted by:  Sony Computer Entertainment Worldwide Studios` **[C]** — identifying
*"the company which was paying for me to work on it"* from HN 12962177. And in 2018
Percival stated plainly what this code is relative to the thesis **[W]**:
*"Sort of. I took bits and pieces from my thesis work and made some further
improvements."*

Layout:

```
bsdiff/bsdiff.c            classic driver
bsdiff-big/main.c          -B blocksize (default 1 MiB), -L diglen (8000), -P ncores
bsdiff-ra/, bspatch-ra/    random-access ("seekable") patch format, experimental
lib/sufsort/               qsufsort
lib/bsdiff/                bsdiff_align.c, bsdiff_align_multi.c, bsdiff_writepatch.c
lib/blockmatch/            blockmatch_psimm.c  <-- the thesis Epilogue metric
                           blockmatch_index.c
lib/fft/                   fft_fft.c, fft_fftn.c, fft_fftconv.c, fft_roots.c
lib/parallel/              parallel_iter.c (pthreads)
```

**Critically: this is still `BSDIFF40` output.** `bsdiff_writepatch.c` writes the
same magic and the same three bzip2 streams, and `encval` is still the fixed 8-byte
sign-magnitude encoding. **None** of Chapter 2's encoding work (Lilliputian /
Blefuscuan differences, difference map + BWT + interpolative coding, varint control,
per-stream codec choice) is present **[C]**.

What *is* new:

**(a) Refactored alignment** (`bsdiff_align.c`). Same algorithm as 4.3 but
restructured into explicit phases: build non-overlapping perfectly-matched seeds →
extend forward → extend backward → resolve overlaps → drop empty segments.

The extension rule is *not* byte-identical to 4.3, and the difference is worth
noting **[I]**. 4.3 takes a global argmax of `2s − i` over all prefixes of the
extension. The refactor instead commits greedily and resets:

```c
s = 0;
for (i = asegp->alen; i < alenmax; ) {
    if (old[asegp->opos + i] == new[asegp->npos + i]) s++;
    i++;
    if (s * 2 > i - asegp->alen) { s = 0; asegp->alen = i; }
}
```

i.e. whenever the *pending tail since the last commit* is more than 50% matching,
absorb it and reset the counter. Same 50% principle, but a local ratchet rather than
a global optimum; it cannot backtrack past a committed point.
Commit messages document intent, e.g. *"Split alignment-generation loop into two
loops: 1. Construct list of non-overlapping perfectly-matched segments. 2. Extend
those segments forwards and backwards as long as at least 50% of the bytes match,
finding an optimal point to split between two segments if this would make them
overlap."*

There is a **disabled** block (`#if 0`) implementing a symmetric backwards version
of the 8-mismatch filter, with the commit message *"Comment out the 'Delete
alignments which aren't much better than their successors' logic for now since it's
producing worse patches — something's not working quite right in that code. I'll
return to this and fix it properly later."* **[C]** — i.e. an attempt at bidirectional
seed pruning that was abandoned.

**(b) Block-parallel alignment for large files** (`bsdiff_align_multi.c` +
`lib/blockmatch`). Commit message: *"Add code for estimating the 'similarity' of two
blocks of data, **as described in the Epilogue to my thesis**; and for using this to
index a file and rapidly find the most-similar portion to a provided new block of
data."* **[C]**

Mechanism: index `old` as 1 MiB blocks, each reduced to a length-8000 `double`
digest; for each 1 MiB block of `new`, find the best-matching old block by dot
product, then run the *ordinary* suffix-array `bsdiff_align` on that block against
a window of old extended by a **1.5× fudge factor** on both sides; concatenate the
sub-alignments. Parallel over blocks.

`blockmatch_psimm.c` implements the Epilogue metric with three refinements over the
thesis text **[C]**:

- **three independent sub-digests** with randomly perturbed lengths
  (`L0, L1 ∈ [L/4, L/4 + L/8)`, `L2` the remainder) — the "several different
  mappings" advice of §1.6 note 8;
- the byte→±1 map is seeded from 256 bits of real entropy per sub-digest;
- and the **byte-frequency weighting from §1.6 note 4 is implemented literally**:

```c
for (T = S = 0, i = 0; i < 256; i++) { S += ctx->map[i]*sqrt(bfreq[i]); T += sqrt(bfreq[i]); }
S = S / T;                                     /* zero-point adjustment */
for (i = 0; i < 256; i++)
    map[i] = (bfreq[i] == 0) ? 0 : (ctx->map[i] - S) / sqrt(bfreq[i]);
```

i.e. **inverse-square-root frequency weighting plus a mean-removal term** so the
mapping is zero-mean under the observed byte distribution. Then fold to `2L+1`
lanes, FFT, take `DIG[i] = |Ā_{i+1}|²` for the first `L` AC bins, and normalise by
`sqrt(L)/‖DIG‖₂` — so a digest's self dot-product is `L`, not `1` as in the thesis
statement; scores are therefore on a `[~L/2, L]` scale rather than `[~0.5, 1]`
**[C][I]**. Score = plain dot product of two digests.

**(c) A random-access patch format** (`bsdiff-ra/FORMAT`), magic `BSDIFFSX`,
explicitly *"experimental and subject to change! It will obtain a version number
when it is fixed."* The new file is cut into fixed-length segments; a bzip2'd header
block holds per-segment `(old offset, old length, patch length)` triples; each
segment's patch data is an independent `(ctrl, diff, extra)` bzip2 trio with 4-byte
lengths, and the ctrl tuple is `(seek, add_len, extra_len)` in 4-byte fields. This
lets a patcher materialise any segment of the new file without decompressing the
whole patch. **[C]**

---

## 8. The 2026 independent reimplementation (CISPA) — and how it makes the DP linear

Rederlechner, Planta & Abbasi, *"One Small Patch for a File, One Giant Leap for OTA
Updates"*, SpaceSec 2026 (NDSS workshop), 23 Feb 2026. Code:
`github.com/Julian-Rederle/bsdiff6` (Rust). Paper downloaded and read **[3P]**.

They had **Percival's actual bsdiff 6 C prototype** — the first confirmation the
artefact still exists — and were not allowed to publish it, so they reimplemented
from it plus the thesis. The acknowledgements thank Percival *"for kindly providing
access to his prototype implementation of bsdiff6."*

Their headline number and its caveats are in §4.7. What matters here is their
reverse-engineering of the **combination step**, which fills in exactly the
implementation detail the thesis leaves out. They attribute bsdiff 6's whole gain to
it: *"our results suggest that the combination step is the improving factor"* and
*"the combination step alone can dramatically improve alignment quality."*

### How the shortest path is made linear-time

The thesis says "find the shortest path" over a 64m-vertex graph and stops. The
prototype, per their analysis, uses **three** optimisations **[3P]**:

> **a) Reduction of Node Count**: "Instead of constructing a graph with all possible
> alignments, bsdiff6 restricts the node set to the outputs of the block and local
> alignment steps, representing a pool of likely optimal alignments."
>
> **b) Layer-by-Layer Graph Construction**: "Since edges exist only between nodes of
> adjacent indices, the graph can be constructed incrementally, one layer at a time,
> for each index of the new file. Each layer connects only to the previous one,
> eliminating the need to keep the full graph in memory."
>
> **c) Selective Edge Connections**: "With the layer-by-layer design, bsdiff6 can save
> on edges by **only connecting each node of the current layer to the best-scoring
> node of the previous layer**. This effectively prunes the search space, leaving a
> single valid path from the last to the first index of the new file. As a result,
> **the need for an explicit shortest path computation is removed entirely**, reducing
> computational cost to linear complexity with respect to file size."

**[I]** (c) is the load-bearing trick and it is worth stating plainly: because every
node in layer *i* back-links to the *same* node in layer *i−1* (the cheapest one),
the "graph" degenerates to a forest with one path per terminal, so the DP collapses
to a running argmin plus a single back-pointer per layer. You never build a graph,
never run Dijkstra, and use O(64) working state. It is a **greedy beam of width 1 on
the back-link, width 64 on the node set** — which is why it is an *approximation* of
the shortest path rather than the shortest path, and the thesis's "shortest path"
framing somewhat oversells it.

They also report two features of the prototype absent from the thesis text **[3P]**:

> "the combination step introduces mechanisms such as **score penalties** and the
> option to **allow regions not to be aligned**, which further improve patch size
> reduction. These details, including the description of the penalty mechanism, fall
> beyond the scope of this paper but are documented alongside the released code."

And the observation that most directly affects anyone porting this **[3P]**:

> "the current **penalty system in bsdiff6 is tuned for bzip2 compression**. To reach
> optimal performance with modern compression tools such as LZMA, future studies
> should investigate the relationship between penalties and compression efficiency.
> Such analyses could yield **adaptive penalty models** that dynamically optimize
> patch generation based on the characteristics of the inputs and chosen
> compression."

**[I]** That is a direct warning that the `0/2/1/20` constants are calibrated to
bzip2's cost curve. Porting them unchanged onto an xz/zstd/LZMA back end is very
likely to mis-price the realignment penalty.

### Their other results worth knowing

- **`bsdiff-ra` is not an improvement.** *"bsdiff-ra does not provide any improvement
  over bsdiff4, neither in patch compactness nor robustness."* **[3P]** Percival's own
  random-access variant applies block alignment *first* and then refines with local
  alignment — the opposite composition order to bsdiff 6 — and it does not pay off.
  **[I]** That is evidence the *combination* is what matters, not merely having a
  block matcher.
- **HDiffPatch (6.6%) beats their bsdiff6 (4.8%)** on the mixed corpus, driven almost
  entirely by already-compressed inputs (xz, PNG, JPEG); excluding the single xz pair,
  bsdiff6 wins. HDiffPatch's own wins come from DivSufSort + RLE in the diff block.
- **CVE-2014-9862 and CVE-2020-14315** affect bsdiff 4 — worth knowing if any bsdiff-4
  derived code is in your tree.

---

## 9. Portability assessment — what is worth stealing

Ranked by (expected gain) / (implementation cost), for a differ that already has
suffix-array matching and a bsdiff-4-shaped three-stream encoder.

**Revised in light of the 2026 measurement.** The evidence now says: the alignment
work (Tier 2 below) is worth **~5% measured**, and the encoding work (Tier 1) is
worth **an unknown amount that has never been isolated** — but which is the only
remaining candidate to explain the gap between 4.8% and Percival's 20–25% claim.
Tier 1 was already the better cost/benefit trade; that is now doubly true. **[I]**

### Tier 1 — cheap, self-contained, likely wins

1. **Multi-precision (carry-propagating) subtraction with balanced digits.**
   §4.5. Replace `new[i] - old[i] (mod 256)` with a borrow-propagating subtraction
   over the matched run, digits in `[−128, 127]`. Try both endiannesses; pick the
   mode that compresses smallest and record it in the header. Cost: ~40 lines plus a
   header byte. Directly targets the dominant residual class (relocated pointers).

2. **Split the diff stream into a *map* and a *values* stream.** §4.5. The map is
   `nonzero(diff[i]) ? 1 : 0`; the values stream is the nonzero bytes with zeros
   removed. Compress separately. Rationale is stated as a general principle —
   *"combining dissimilar data before compression tends to reduce compression
   ratios"* — and the two halves genuinely have different statistics (positions of
   corrections are instruction-encoding-structured; values are locally repetitive).
   Cost: trivial. This is likely the single largest encoding win — and note that
   **no published measurement of bsdiff 6 has ever included it**, since both the 2012
   repo and the 2026 reimplementation keep the `BSDIFF40` three-stream format.

3. **Per-stream codec selection, decided empirically per patch.** §4.5. Try each
   candidate codec on each stream, keep the best, record the choice in a header
   byte. Percival's finding: LZ for control and nonzero-diff (local repetition), BWT
   for extra (global structure). With a modern codec set the ranking may differ, but
   the *mechanism* — measure, don't assume — is what to copy.

4. **Varint control fields.** §4.5. bsdiff 4's fixed 8-byte fields are pure waste;
   the thesis uses base-128. If your control stream is already varint, skip.

### Tier 2 — real work, real payoff

5. **The combination step: a layered beam over candidate offsets, replacing the
   greedy scan.** §4.4 + §8 — and per the 2026 analysis, *the* source of bsdiff 6's
   measured gain **[3P]**. The concrete recipe, now with the implementation detail
   the thesis omits:

   - candidate node set per new-file byte ≈ 64 *offsets*: ~31 implied by
     longest-matches starting just ahead, ~31 currently-cheapest incumbents, one from
     a coarse block matcher, one "unmatched";
   - edge costs `match 0 / mismatch 2 / literal 1 / realign 20`, plus score penalties
     and an explicit allow-not-to-align option present in the prototype;
   - **build it layer by layer and back-link every node in layer *i* only to the
     single best-scoring node of layer *i−1*.** That collapses the search to a
     running argmin with one back-pointer per layer: **linear time, O(64) working
     state, no Dijkstra, no graph in memory**.

   This replaces bsdiff's fixed `+8` with an explicit, tunable cost model, and it is
   the principled cure for the scale-dependent false-match problem Percival himself
   flags **[W]**.

   **[I] Do not port `0/2/1/20` unchanged.** They are empirical (*"will not be ideal
   for all input data"* — thesis fn. 13) and, per **[3P]**, *"tuned for bzip2
   compression"*. Replace them with measured bit costs from your own encoder: the
   estimated compressed cost of a nonzero diff byte, of a literal byte, and of a
   control triple. The realignment cost in particular is the price of a control
   triple, which varies by an order of magnitude between a fixed-8-byte format and a
   varint one.

   Expected value: **low-to-mid single-digit percent**, on the only measurement that
   exists. Worth doing, but do not expect 20%.

6. **A coarse block-level matcher that survives heavy mismatch.** §4.2 + Epilogue.
   Either the full projective matching-with-mismatches, or — far more practical —
   the released `psimm` digest: fold a block into `2L+1` lanes under an
   inverse-√frequency-weighted, mean-removed ±1 map, FFT, keep `|Ā_j|²` for the
   first `L` AC bins, normalise, compare by dot product (three independent
   sub-digests with randomised lengths, summed, to avoid an unlucky mapping). Shift-invariant, fixed-size, comparable.
   Use it to propose an offset for regions where exact matching finds nothing —
   specifically address/pointer tables, which by construction contain no exact match
   longer than 3 bytes.

   The evidence this is worth it: **block-alignment-only bsdiff 6 lands within 8% of
   full bsdiff 6 on the security corpus** (1.38% vs 1.27%) at `O(√n)` memory
   **[T]**.

   **Counter-evidence [3P]:** Percival's own `bsdiff-ra`, which applies block
   alignment first and then refines with local alignment, *"does not provide any
   improvement over bsdiff4."* **[I]** So the block matcher is only worth having if it
   feeds the combination step (item 5) as one candidate among many — not if it drives
   the alignment. Order of composition matters more than the matcher itself.

7. **Power-of-two snapping of segment boundaries when the mismatch count is tied.**
   §4.2. Free, and exploits compiler code alignment. Applies to any boundary-choosing
   step, including bsdiff-style overlap resolution.

### Tier 3 — specialist

8. **BWT + interpolative coding for the sparse-correction bitmap.** §4.5. Only pays
   if the map stream is large and highly structured; a modern range coder with a
   context model over recent bits may match it with less machinery. Worth
   benchmarking against, not necessarily worth implementing.

9. **Random-access patch format.** §7(c) — the `bsdiff-ra` format. Orthogonal to compression; matters if
   patches are applied lazily or over a network.

### Explicit non-recommendations

- **Do not** port the Chapter 1 sublinear matching wholesale. It is the theoretical
  contribution, its constants are tuned for asymptotics, and Percival's own code
  uses only the Epilogue simplification. **[I]**
- **The "correction map" difference mode** is included in bsdiff 6 for completeness
  but Percival reports never having found a real file where it wins **[T]**.
- **Suffix-sorting `S#T#` jointly** (thesis) rather than `old` alone (bsdiff 4.3)
  buys an LCP array and a cleaner per-position best-match array, but the same
  information is obtainable other ways; it is an implementation choice, not an
  algorithmic idea. **[I]**

---

## 10. What the thesis does *not* address

Recorded so the gaps are explicit rather than assumed **[T]** (absence verified by
reading Ch. 2 in full):

- **No cost model tied to the actual encoder.** The shortest-path weights
  `0/2/1/20` are hand-tuned integers, not estimated bit costs. There is no feedback
  loop between the chosen alignment and the resulting compressed size.
- **No treatment of relocations, symbol tables, or any container structure.** The
  method is deliberately "naïve" — that is the thesis's central claim. Section-aware
  or reloc-aware preprocessing (Courgette, Zucchini, and any format-specific
  transform) is out of scope by construction.
- **No multi-reference or cross-file matching.** One old file, one new file.
  Percival separately confirms bsdiff was designed file-by-file **[W]**.
- **No streaming or bounded-memory patch application.** `bspatch` materialises both
  files. The random-access format in the 2012 repo is the only work in that
  direction, and it is marked experimental **[C]**.
- **No modern entropy coding.** zlib and bzip2 only; the difference map's
  BWT + interpolative coder is the sole custom codec.
- **No handling of insertions/deletions inside a matched region.** Block and local
  alignment both produce indel-free segments; indels are only expressible by ending
  a segment and starting another (paying the realignment cost). The matching-with-
  mismatches framing is substitutions-only by definition (Ch. 1 §1.1).

---

## 11. Exhaustive negatives — places checked that contain nothing

Recorded so this ground is not re-covered. Each was searched, not assumed.

- **His blog is a dead end.** All 133 monthly archive pages of
  `daemonology.net/blog/` (~3.4 MB) downloaded and grepped. Only six posts mention
  bsdiff at all — 2006-07, 2006-09, 2008-02 (the gcc 3.4 → 4.2 post about patches
  degrading), 2009-07, 2014-09, 2017-05 — and **none discusses a version number or
  the unreleased algorithm.** Disqus threads on those posts likewise.
- **FreeBSD mailing lists contain nothing.** 1,323 pipermail monthly archives
  (~1.1 GB) grepped, 761 Percival messages extracted; ~30 marc.info lists searched.
  **Zero** matches for bsdiff 5/6/7, "improved bsdiff", "20% smaller", or release
  plans. His 2003–2005 posts discuss bsdiff internals (suffix sort vs rsync sampling,
  the "bs" naming) but never a successor. `tarsnap-users` (all 1,809 messages) has
  zero occurrences of "bsdiff".
- **He never commented in Chromium or Mozilla.** The Courgette design doc mentions
  bsdiff repeatedly but never names Percival, his thesis, or a version. **The Zucchini
  README does not mention bsdiff or Percival at all.** Mozilla bugs 504624, 1632374,
  296295 and 1434513 have no Percival comments (296295 refers only to *"a modified
  version of bsdiff 4.2"*).
- **No conference talk covers bsdiff internals.** All 12 linked publication PDFs were
  grepped: only `bsdiff.pdf` (2003), `binup.pdf` (BSDCon '03) and `thesis.pdf` contain
  "bsdiff"/"bspatch" at all. BSDCan '05, '06, '09 and AsiaBSDCon '18: **zero
  occurrences**. The BSDCan '07 Portsnap talk (not on his publications page) has one
  algorithmic aside and no version claims.
- **No `bsdiff6`/`bsdiff-5`/`bsdiff-6` page or tarball ever existed on
  daemonology.net** (full Wayback URL index for the domain checked).
- **No bsdiff 6 in any of his repos.** All 33 public repos under `github.com/cperciva`
  enumerated; `bsdiff` is the only delta-compression one, last pushed 2015-09-08.
- **Twitter/X could not be checked** (login wall; Nitter mirrors dead). His Mastodon
  (`mastodon.social/@cperciva`, all 22 posts pulled) has zero bsdiff mentions.
  Reddit was bot-blocked from this environment. **Anything said on those platforms is
  outside what was verified** — this is a gap, not a negative.

---

## Sources

Reproduction (all read-only, 2026-08-29):

```sh
curl -sL -o thesis.pdf http://www.daemonology.net/papers/thesis.pdf   # 442,320 B, 82 pp
curl -sL -o bsdiff.pdf https://www.daemonology.net/papers/bsdiff.pdf  #  35,919 B,  3 pp
pdftotext -layout thesis.pdf thesis.txt && pdftotext -layout bsdiff.pdf bsdiff.txt
curl -sL https://www.daemonology.net/bsdiff/            # raw HTML; WebFetch drops the key line
curl -sL https://raw.githubusercontent.com/mendsley/bsdiff/v4.3/bsdiff.c
git clone https://github.com/cperciva/bsdiff            # master @ 093b35e
curl -s "https://hn.algolia.com/api/v1/search?query=bsdiff&tags=comment,author_cperciva"
```

Primary documents fetched and read in full:

- **http://www.daemonology.net/papers/thesis.pdf** — Colin Percival, *Matching with
  Mismatches and Assorted Applications*, D.Phil. thesis, Oxford, Hilary 2006. 82 pp.
  Downloaded and converted with `pdftotext -layout`. Source of everything tagged
  **[T]**: Preface, Ch. 0, Ch. 1 (Algorithms 1.1–1.3, §1.6 final notes), Ch. 2
  (§2.1–2.9 in full, incl. Tables 2.1 and 2.2 and all footnotes), Appendix A,
  Epilogue, Bibliography.
- **https://www.daemonology.net/papers/bsdiff.pdf** — Colin Percival, *Naïve
  Differences of Executable Code*, 2003, 3 pp. Read in full; source of everything
  tagged **[P]** including Table 1 and the 50%-extension and 8-byte-trigger prose.
- **https://www.daemonology.net/bsdiff/** — the bsdiff home page (fetched raw HTML
  via curl, since WebFetch's summary dropped the key sentence). Source of: the
  50–80%-vs-Xdelta and 15%-vs-.RTPatch claims, memory/time bounds, the
  "bs" = "binary software" / "bytewise subtraction" note, version 4.3 availability
  and MD5, and the decisive sentence *"A far more sophisticated algorithm, which
  typically provides roughly 20% smaller patches, is described in my doctoral
  thesis."*
- **https://www.daemonology.net/papers/** — Percival's publications list. Used to
  confirm no bsdiff-internals conference talk exists and that the 2003 paper is
  designated *"the canonical reference for BSDiff 4"*.

Source code read:

- **https://raw.githubusercontent.com/mendsley/bsdiff/v4.3/bsdiff.c** and
  **.../bspatch.c** — bsdiff 4.3 release source. Source of everything in §1:
  the `BSDIFF40` container, `offtin`/`offtout`, the control-triple semantics, the
  greedy scan with `oldscore` and the `len > oldscore + 8` trigger, the `s*2 - i`
  extension argmax, and the overlap-splitting loop. (The canonical tarball at
  `https://www.daemonology.net/bsdiff/bsdiff-4.3.tar.gz` returns HTTP 403 — see
  `github.com/cperciva/bsdiff` issue #4 — so the `mendsley` mirror at tag `v4.3` was
  used instead.)
- **https://github.com/cperciva/bsdiff** (git clone, master @ 093b35e) — Percival's
  own post-4.3 tree. Read: `lib/bsdiff/bsdiff_align.c`,
  `lib/bsdiff/bsdiff_align_multi.c`, `lib/bsdiff/bsdiff_writepatch.c`,
  `lib/blockmatch/blockmatch_psimm.{c,h}`, `lib/blockmatch/blockmatch_index.{c,h}`,
  `bsdiff-big/main.c`, `bsdiff-ra/FORMAT`, plus `git log` commit messages. Source of
  §7 in its entirety, including the confirmation that the released tree still emits
  `BSDIFF40` and contains none of the Chapter-2 encoding work.
- **https://api.github.com/users/cperciva/repos** — enumeration of all 33 public
  repositories, confirming `bsdiff` is the only delta-compression repo and that no
  bsdiff 6 has been published.
- **https://www.daemonology.net/freebsd-update/binup.pdf** — *An Automated Binary
  Security Update System for FreeBSD*, BSDCon '03. Checked for bsdiff internals;
  contains none — it cites the 2003 paper and uses BSDiff as a black box.

Third-party primary research:

- **https://www.ndss-symposium.org/wp-content/uploads/spacesec26-70.pdf** — Julian
  Rederlechner, Ulysse Planta, Ali Abbasi (CISPA), *"One Small Patch for a File, One
  Giant Leap for OTA Updates"*, SpaceSec 2026 (NDSS workshop), 23 Feb 2026,
  doi:10.14722/spacesec.2026.23070. Downloaded and read. **The only independent
  measurement of bsdiff 6 in existence**, made from Percival's private C prototype.
  Source of §7a and §4.7(c)–(d): the ≈4.8% figure, the three combination-step
  optimisations, the "penalties tuned for bzip2" warning, the bsdiff-ra negative
  result, and the corpus composition. Their Rust reimplementation:
  `https://github.com/Julian-Rederle/bsdiff6`.

Archived primary source (not on the live web):

- **https://web.archive.org/web/20050304055057/http://www.daemonology.net/bsdiff/** —
  the deleted **"Note to corporate users"** naming *"bsdiff 6.0"*, claiming *"25%
  smaller than … bsdiff 4.2"*, stating Oxford *"has claimed"* the software, and giving
  `bsdiff6@daemonology.net` as the licensing contact. Verified directly by me; also
  present at captures `20041204114048`, `20050310083416`, `20050404102940`,
  `20050612005901`, `20050617233453`; absent at `20040811000227` and `20050819152418`.

Bug trackers:

- **https://bugs.debian.org/cgi-bin/bugreport.cgi?bug=632585** — Debian #632585.
  Percival's 2011-07-04 *"complete rewrite pending"* message (#17) and the 2011-09-23
  *"That's the latest code"* reply forwarded by the maintainer (#32). Mirrored at
  `mail-archive.com/debian-bugs-dist@lists.debian.org/msg918430.html` and
  `msg944739.html`.
- **https://github.com/cperciva/bsdiff/issues/2** — 2018-06-03, *"Sort of. I took bits
  and pieces from my thesis work and made some further improvements."*
- **https://github.com/cperciva/bsdiff/issues/4** — 2021-01-07, on the 403'd tarball
  and the security fixes in FreeBSD's copy.
- **https://www.bsdcan.org/2007/schedule/attachments/11-Portsnap_Colin_Percival.pdf**
  — BSDCan '07 Portsnap slides; the "first delta compressor designed with an awareness
  of byte substitutions" remark.

Public statements (Hacker News, retrieved via the Algolia HN API —
`https://hn.algolia.com/api/v1/search?...author_cperciva` and
`https://hn.algolia.com/api/v1/items/<id>`; the API reported 220 comments
site-wide matching "bsdiff" and 29 cperciva comments each for the queries "bsdiff"
and "thesis" — all were retrieved and read):

- **https://news.ycombinator.com/item?id=706496** (2009-07-15) — the thesis version
  vs Exediff and Courgette; expected bsdiff patch sizes for kernel-scale binaries.
- **https://news.ycombinator.com/item?id=2578873** (2011-05-24) — *"I have ideas for
  improving bsdiff, too."*
- **https://news.ycombinator.com/item?id=12962177** (2016-11-15) — the post-4.3 code
  on GitHub and the sponsoring company cancelling the project.
- **https://news.ycombinator.com/item?id=13121192** and
  **https://news.ycombinator.com/item?id=13121344** (2016-12-07) — bsdiff is
  file-by-file by design; the mostly-matching-region heuristic degrades with input
  size.
- **https://news.ycombinator.com/item?id=27513380** thread (2021-06-15) — hlandau's
  question about Oxford owning the IP and cperciva's reply *"They eventually did give
  me permission, but I never got around to cleaning up the code for release."*;
  child comment **27521415** pointing to Chapter 2.
- Various biographical comments (939365, 4725594, 3034834, 33665765) confirming the
  provenance narrative; not load-bearing for the algorithms.

Negative results are catalogued in §11 above rather than here. The one worth
repeating: grep of the full thesis text for
`kangaroo|Landau|Vishkin|LCA|lowest common ancestor` returns **0 hits** — the
suffix-tree/LCA lineage is not what Percival used.

**Division of labour and verification.** The thesis, the 2003 paper, the bsdiff 4.3
source, the `cperciva/bsdiff` clone, the GitHub repo enumeration and the HN comment
corpus were retrieved and read directly. A parallel background agent swept the
Wayback Machine, the blog archive, FreeBSD/Debian mailing lists, bug trackers and the
2026 literature; **its two load-bearing finds — the deleted "Note to corporate users"
and the SpaceSec 2026 measurement — were then independently re-fetched and verified
by me** (`curl` on the 2005-03-04 Wayback capture; download and `pdftotext` of
`spacesec26-70.pdf`) before being written up here. Claims from that sweep which I did
not personally re-verify are the mailing-list and blog negatives in §11, the Debian
bug quotes, and the GitHub issue quotes.
