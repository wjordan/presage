// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Copied from Go 1.26.3 compress/zlib and trimmed to the BestSpeed writer; frozen so its output never changes.

package flate126

import (
	"encoding/binary"
	stdhash "hash" // aliased: this package declares a function named hash
	"hash/adler32"
	"io"
)

// zlibHeader is the two-byte RFC 1950 header compress/zlib emits for
// BestSpeed: CINFO=7, CM=8 (0x78), then FLEVEL=0 with the mod-31 FCHECK
// bits that make the pair a multiple of 31 (0x01).
var zlibHeader = [2]byte{0x78, 0x01}

// zlibWriter frames a BestSpeed DEFLATE stream the way compress/zlib does:
// the header, the deflate stream, then the big-endian Adler-32 of the
// uncompressed data.
type zlibWriter struct {
	w           io.Writer
	compressor  *Writer
	digest      stdhash.Hash32
	err         error
	scratch     [4]byte
	wroteHeader bool
}

// NewZlibWriter returns a writer whose output is byte-for-byte identical to
// Go 1.26's zlib.NewWriterLevel(w, zlib.BestSpeed).
func NewZlibWriter(w io.Writer) io.WriteCloser {
	return &zlibWriter{w: w}
}

func (z *zlibWriter) writeHeader() error {
	z.wroteHeader = true
	if _, err := z.w.Write(zlibHeader[:]); err != nil {
		return err
	}
	z.compressor = NewWriter(z.w)
	z.digest = adler32.New()
	return nil
}

func (z *zlibWriter) Write(p []byte) (n int, err error) {
	if !z.wroteHeader {
		z.err = z.writeHeader()
	}
	if z.err != nil {
		return 0, z.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err = z.compressor.Write(p)
	if err != nil {
		z.err = err
		return
	}
	z.digest.Write(p)
	return
}

func (z *zlibWriter) Close() error {
	if !z.wroteHeader {
		z.err = z.writeHeader()
	}
	if z.err != nil {
		return z.err
	}
	z.err = z.compressor.Close()
	if z.err != nil {
		return z.err
	}
	// ZLIB (RFC 1950) is big-endian, unlike GZIP (RFC 1952).
	binary.BigEndian.PutUint32(z.scratch[:], z.digest.Sum32())
	_, z.err = z.w.Write(z.scratch[0:4])
	return z.err
}
