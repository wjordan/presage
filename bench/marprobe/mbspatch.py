#!/usr/bin/env python3
"""mbspatch.py OLD PATCH OUT [--crc]

Applies one MBDIFF10 patch -- the per-file entry of a Firefox partial MAR,
Mozilla's bsdiff variant. Exists so that a shipped patch can be used as a
denominator honestly: apply it to the base we measure against and check the
result is the target we measure against, byte for byte. Without that the
two endpoints are only assumed to be the ones Mozilla diffed.

Header is 32 bytes big-endian: tag, source length, source CRC, target length,
then the lengths of the control, diff and extra blocks. Control is triples of
(x, y, z): add x bytes of the diff block to x bytes of the source, copy y
bytes of the extra block, then seek the source forward by z. The blocks are
stored raw -- the MAR entry's XZ is the only compression.

The source CRC is CRC-32/BZIP2 (non-reflected, polynomial 0x04C11DB7), not the
reflected CRC-32 of zlib. Checking it is `--crc` rather than the default: the
loop is a byte at a time and libxul is 186 MB, and comparing the output
against the real target is the stronger check anyway.
"""
import struct, sys
import numpy as np

POLY = 0x04C11DB7


def crc32_bzip2(data):
    table = []
    for i in range(256):
        c = i << 24
        for _ in range(8):
            c = ((c << 1) ^ POLY) & 0xFFFFFFFF if c & 0x80000000 else (c << 1) & 0xFFFFFFFF
        table.append(c)
    crc = 0xFFFFFFFF
    for b in data:
        crc = ((crc << 8) & 0xFFFFFFFF) ^ table[((crc >> 24) ^ b) & 0xFF]
    return crc ^ 0xFFFFFFFF


def apply(old, patch):
    tag = patch[:8]
    if tag != b"MBDIFF10":
        raise ValueError(f"not an mbsdiff patch: {tag!r}")
    slen, scrc, dlen, cblen, difflen, extralen = struct.unpack(">IIIIII", patch[8:32])
    if len(old) != slen:
        raise ValueError(f"source is {len(old)} bytes, patch expects {slen}")
    ctrl, diff_at, extra_at = 32, 32 + cblen, 32 + cblen + difflen
    diff = np.frombuffer(patch, dtype=np.uint8, offset=diff_at, count=difflen)
    src = np.frombuffer(old, dtype=np.uint8)
    out = np.empty(dlen, dtype=np.uint8)
    o = op = dp = 0
    ep = extra_at
    while ctrl < diff_at:
        x, y, z = struct.unpack(">IIi", patch[ctrl:ctrl + 12])
        ctrl += 12
        if x:
            out[o:o + x] = diff[dp:dp + x] + src[op:op + x]
            o, dp, op = o + x, dp + x, op + x
        if y:
            out[o:o + y] = np.frombuffer(patch, dtype=np.uint8, offset=ep, count=y)
            o, ep = o + y, ep + y
        op += z
    if o != dlen:
        raise ValueError(f"wrote {o} bytes, header says {dlen}")
    return out.tobytes(), scrc


def main(argv):
    old = open(argv[1], "rb").read()
    result, scrc = apply(old, open(argv[2], "rb").read())
    if "--crc" in argv:
        got = crc32_bzip2(old)
        print(f"source crc {got:#010x} {'==' if got == scrc else '!='} header {scrc:#010x}")
    print(f"wrote {len(result)} bytes to {argv[3]}")
    open(argv[3], "wb").write(result)


if __name__ == "__main__":
    main(sys.argv)
