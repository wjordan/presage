#!/usr/bin/env python3
"""mar.py ARCHIVE.mar [list|extract NAME [OUT]]

Reads Mozilla's MAR container -- the format Firefox ships both complete and
partial updates in. Every field is big-endian; every entry is compressed on
its own, XZ since Firefox 40. `list` prints the index largest first, which is
what makes a partial MAR legible: one entry usually carries most of it.
"""
import lzma, os, struct, sys


def parse(path):
    """(data, declared_size, [(section_type, blob)], [(name, off, size, flags)])."""
    d = open(path, "rb").read()
    if d[:4] != b"MAR1":
        raise ValueError(f"{path}: not a MAR ({d[:4]!r})")
    (index_off,) = struct.unpack(">I", d[4:8])
    (declared,) = struct.unpack(">Q", d[8:16])
    (sigs,) = struct.unpack(">I", d[16:20])
    p = 20
    for _ in range(sigs):
        _, siglen = struct.unpack(">II", d[p:p + 8])
        p += 8 + siglen
    (nsec,) = struct.unpack(">I", d[p:p + 4])
    p += 4
    sections = []
    for _ in range(nsec):
        blocklen, blocktype = struct.unpack(">II", d[p:p + 8])
        sections.append((blocktype, d[p + 8:p + blocklen]))
        p += blocklen
    (index_size,) = struct.unpack(">I", d[index_off:index_off + 4])
    p, end, entries = index_off + 4, index_off + 4 + index_size, []
    while p < end:
        off, size, flags = struct.unpack(">III", d[p:p + 12])
        p += 12
        z = d.index(b"\0", p)
        entries.append((d[p:z].decode(), off, size, flags))
        p = z + 1
    return d, declared, sections, entries


def compression(b):
    if b[:6] == b"\xfd7zXZ\x00":
        return "xz"
    if b[:3] == b"BZh":
        return "bz2"
    return b[:4].hex()


def extract(path, name):
    d, _, _, entries = parse(path)
    for n, off, size, _ in entries:
        if n == name:
            return lzma.decompress(d[off:off + size]), size
    raise KeyError(name)


def main(argv):
    path = argv[1]
    cmd = argv[2] if len(argv) > 2 else "list"
    if cmd == "extract":
        raw, stored = extract(path, argv[3])
        sys.stderr.write(f"{argv[3]}: stored {stored} -> raw {len(raw)}, magic {raw[:8]!r}\n")
        out = argv[4] if len(argv) > 4 else None
        (open(out, "wb") if out else sys.stdout.buffer).write(raw)
        return
    d, declared, sections, entries = parse(path)
    print(f"{os.path.basename(path)}: {len(d)} bytes (declared {declared}), {len(entries)} entries")
    for t, blob in sections:
        print(f"  section type {t}: {blob.rstrip(chr(0).encode()).decode(errors='replace')}")
    total = sum(e[2] for e in entries)
    print(f"  {'stored':>11} {'share':>6}  comp  name")
    for name, off, size, _ in sorted(entries, key=lambda e: -e[2]):
        print(f"  {size:>11} {100 * size / total:>5.1f}%  {compression(d[off:off + 8]):>4}  {name}")
    print(f"  {total:>11} 100.0%        TOTAL")


if __name__ == "__main__":
    main(sys.argv)
