// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Copied from Go 1.26.3 compress/flate and trimmed to the BestSpeed writer; frozen so its output never changes.

package flate126

import (
	"errors"
	"io"
)

const (
	// The LZ77 step produces a sequence of literal tokens and <length, offset>
	// pair tokens. The offset is also known as distance. The underlying wire
	// format limits the range of lengths and offsets. For example, there are
	// 256 legitimate lengths: those in the range [3, 258]. This package's
	// compressor uses a higher minimum match length, enabling optimizations
	// such as finding matches via 32-bit loads and compares.
	baseMatchLength = 3       // The smallest match length per the RFC section 3.2.5
	maxMatchLength  = 258     // The largest match length
	baseMatchOffset = 1       // The smallest match offset
	maxMatchOffset  = 1 << 15 // The largest match offset

	maxStoreBlockSize = 65535
)

type compressor struct {
	w         *huffmanBitWriter
	bestSpeed *deflateFast // Encoder for BestSpeed

	// input window: unprocessed data is window[:windowEnd]
	window    []byte
	windowEnd int

	sync bool // requesting flush

	// queued output tokens
	tokens []token

	err error
}

func (d *compressor) writeStoredBlock(buf []byte) error {
	if d.w.writeStoredHeader(len(buf), false); d.w.err != nil {
		return d.w.err
	}
	d.w.writeBytes(buf)
	return d.w.err
}

// encSpeed will compress and store the currently added data,
// if enough has been accumulated or we at the end of the stream.
// Any error that occurred will be in d.err
func (d *compressor) encSpeed() {
	// We only compress if we have maxStoreBlockSize.
	if d.windowEnd < maxStoreBlockSize {
		if !d.sync {
			return
		}

		// Handle small sizes.
		if d.windowEnd < 128 {
			switch {
			case d.windowEnd == 0:
				return
			case d.windowEnd <= 16:
				d.err = d.writeStoredBlock(d.window[:d.windowEnd])
			default:
				d.w.writeBlockHuff(false, d.window[:d.windowEnd])
				d.err = d.w.err
			}
			d.windowEnd = 0
			d.bestSpeed.reset()
			return
		}

	}
	// Encode the block.
	d.tokens = d.bestSpeed.encode(d.tokens[:0], d.window[:d.windowEnd])

	// If we removed less than 1/16th, Huffman compress the block.
	if len(d.tokens) > d.windowEnd-(d.windowEnd>>4) {
		d.w.writeBlockHuff(false, d.window[:d.windowEnd])
	} else {
		d.w.writeBlockDynamic(d.tokens, false, d.window[:d.windowEnd])
	}
	d.err = d.w.err
	d.windowEnd = 0
}

func (d *compressor) fillStore(b []byte) int {
	n := copy(d.window[d.windowEnd:], b)
	d.windowEnd += n
	return n
}

func (d *compressor) write(b []byte) (n int, err error) {
	if d.err != nil {
		return 0, d.err
	}
	n = len(b)
	for len(b) > 0 {
		d.encSpeed()
		b = b[d.fillStore(b):]
		if d.err != nil {
			return 0, d.err
		}
	}
	return n, nil
}

func (d *compressor) init(w io.Writer) {
	d.w = newHuffmanBitWriter(w)
	d.window = make([]byte, maxStoreBlockSize)
	d.bestSpeed = newDeflateFast()
	d.tokens = make([]token, maxStoreBlockSize)
}

func (d *compressor) close() error {
	if d.err == errWriterClosed {
		return nil
	}
	if d.err != nil {
		return d.err
	}
	d.sync = true
	d.encSpeed()
	if d.err != nil {
		return d.err
	}
	if d.w.writeStoredHeader(0, true); d.w.err != nil {
		return d.w.err
	}
	d.w.flush()
	if d.w.err != nil {
		return d.w.err
	}
	d.err = errWriterClosed
	return nil
}

// NewWriter returns a new [Writer] compressing data at BestSpeed (level 1),
// producing a raw DEFLATE stream byte-for-byte identical to Go 1.26's
// flate.NewWriter(w, flate.BestSpeed).
func NewWriter(w io.Writer) *Writer {
	var dw Writer
	dw.d.init(w)
	return &dw
}

var errWriterClosed = errors.New("flate: closed writer")

// A Writer takes data written to it and writes the compressed
// form of that data to an underlying writer (see [NewWriter]).
type Writer struct {
	d compressor
}

// Write writes data to w, which will eventually write the
// compressed form of data to its underlying writer.
func (w *Writer) Write(data []byte) (n int, err error) {
	return w.d.write(data)
}

// Close flushes and closes the writer.
func (w *Writer) Close() error {
	return w.d.close()
}
