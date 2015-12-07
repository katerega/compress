// Copyright 2015, Joe Tsai. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package prefix

import (
	"bufio"
	"bytes"
	"io"
	"strings"

	"github.com/dsnet/compress"
	"github.com/dsnet/compress/internal"
)

// Reader implements a prefix decoder. If the input io.Reader satisfies the
// compress.ByteReader or compress.BufferedReader interface, then it also
// guarantees that it will never read more bytes than is necessary.
//
// For high performance, provide an io.Reader that satisfies the
// compress.BufferedReader interface. If the input does not satisfy either
// compress.ByteReader or compress.BufferedReader, then it will be internally
// wrapped with a bufio.Reader.
type Reader struct {
	rd        io.Reader
	byteRd    compress.ByteReader
	bufRd     compress.BufferedReader
	bufBits   uint64 // Buffer to hold some bits
	numBits   uint   // Number of valid bits in bufBits
	offset    int64  // Number of bytes read from the underlying io.Reader
	bigEndian bool   // Are bits read in big-endian order?

	// These fields are only used if rd is a compress.BufferedReader.
	bufPeek     []byte // Buffer for the Peek data
	discardBits int    // Number of bits to discard from reader
	fedBits     uint   // Number of bits fed in last call to pullBits
}

// Init initializes the bit Reader to read from r. If bigEndian is true, then
// bits will be read starting from the most-significant bits of a byte
// (as done in bzip2), otherwise it will read starting from the
// least-significant bits of a byte (such as for deflate and brotli).
func (pr *Reader) Init(r io.Reader, bigEndian bool) {
	*pr = Reader{rd: r, bigEndian: bigEndian}
	switch rr := r.(type) {
	case *bytes.Buffer:
		pr.bufRd = &buffer{Buffer: rr}
	case *bytes.Reader:
		pr.bufRd = &bytesReader{Reader: rr}
	case *strings.Reader:
		pr.bufRd = &stringReader{Reader: rr}
	case compress.BufferedReader:
		pr.bufRd = rr
	case compress.ByteReader:
		pr.byteRd = rr
	default:
		br := bufio.NewReader(r)
		pr.rd, pr.bufRd = br, br
	}
}

// ReadPads reads 0-7 bits from the bit buffer to achieve byte-alignment.
func (pr *Reader) ReadPads() uint {
	nb := pr.numBits % 8
	val := uint(pr.bufBits & uint64(1<<nb-1))
	pr.bufBits >>= nb
	pr.numBits -= nb
	return val
}

// Read reads bytes into buf.
// The bit-ordering mode does not affect this method.
func (pr *Reader) Read(buf []byte) (cnt int, err error) {
	if pr.numBits > 0 {
		if pr.numBits%8 != 0 {
			return 0, internal.Error{"non-aligned bit buffer"}
		}
		for cnt = 0; len(buf) > cnt && pr.numBits > 0; cnt++ {
			c := byte(pr.bufBits)
			if pr.bigEndian {
				c = internal.ReverseLUT[c]
			}
			buf[cnt] = c
			pr.bufBits >>= 8
			pr.numBits -= 8
		}
		return cnt, nil
	}
	if _, err := pr.Flush(); err != nil {
		return 0, err
	}
	cnt, err = pr.rd.Read(buf)
	pr.offset += int64(cnt)
	return cnt, err
}

// ReadOffset reads an offset value using the provided RangeCodes indexed by
// the symbol read.
func (pr *Reader) ReadOffset(pd *Decoder, rcs RangeCodes) uint {
	rc := rcs[pr.ReadSymbol(pd)]
	return uint(rc.Base) + pr.ReadBits(uint(rc.Len))
}

// TryReadBits attempts to read nb bits using the contents of the bit buffer
// alone. It returns the value and whether it succeeded.
//
// This method is designed to be inlined for performance reasons.
func (pr *Reader) TryReadBits(nb uint) (uint, bool) {
	if pr.numBits < nb {
		return 0, false
	}
	val := uint(pr.bufBits & uint64(1<<nb-1))
	pr.bufBits >>= nb
	pr.numBits -= nb
	return val, true
}

// ReadBits reads nb bits in from the underlying reader.
func (pr *Reader) ReadBits(nb uint) uint {
	if err := pr.pullBits(nb); err != nil {
		panic(err)
	}
	val := uint(pr.bufBits & uint64(1<<nb-1))
	pr.bufBits >>= nb
	pr.numBits -= nb
	return val
}

// TryReadSymbol attempts to decode the next symbol using the contents of the
// bit buffer alone. It returns the decoded symbol and whether it succeeded.
//
// This method is designed to be inlined for performance reasons.
func (pr *Reader) TryReadSymbol(pd *Decoder) (uint, bool) {
	if pr.numBits < uint(pd.minBits) || len(pd.chunks) == 0 {
		return 0, false
	}
	chunk := pd.chunks[uint32(pr.bufBits)&pd.chunkMask]
	nb := uint(chunk & countMask)
	if nb > pr.numBits || nb > uint(pd.chunkBits) {
		return 0, false
	}
	pr.bufBits >>= nb
	pr.numBits -= nb
	return uint(chunk >> countBits), true
}

// ReadSymbol reads the next symbol using the provided prefix Decoder.
func (pr *Reader) ReadSymbol(pd *Decoder) uint {
	if len(pd.chunks) == 0 {
		panic(internal.ErrInvalid) // Decode with empty tree
	}

	nb := uint(pd.minBits)
	for {
		if err := pr.pullBits(nb); err != nil {
			panic(err)
		}
		chunk := pd.chunks[uint32(pr.bufBits)&pd.chunkMask]
		nb = uint(chunk & countMask)
		if nb > uint(pd.chunkBits) {
			linkIdx := chunk >> countBits
			chunk = pd.links[linkIdx][uint32(pr.bufBits>>pd.chunkBits)&pd.linkMask]
			nb = uint(chunk & countMask)
		}
		if nb <= pr.numBits {
			pr.bufBits >>= nb
			pr.numBits -= nb
			return uint(chunk >> countBits)
		}
	}
}

// Flush updates the read offset of the underlying ByteReader.
// If reader is a compress.BufferedReader, then this calls Discard to update
// the read offset.
func (pr *Reader) Flush() (int64, error) {
	if pr.bufRd == nil {
		return pr.offset, nil
	}

	// Update the number of total bits to discard.
	pr.discardBits += int(pr.fedBits - pr.numBits)
	pr.fedBits = pr.numBits

	// Discard some bytes to update read offset.
	var err error
	nd := (pr.discardBits + 7) / 8 // Round up to nearest byte
	nd, err = pr.bufRd.Discard(nd)
	pr.discardBits -= nd * 8 // -7..0
	pr.offset += int64(nd)

	// These are invalid after Discard.
	pr.bufPeek = nil
	return pr.offset, err
}

// pullBits ensures that at least nb bits exist in the bit buffer.
// If the underlying reader is a compress.BufferedReader, then this will fill
// the bit buffer with as many bits as possible, relying on Peek and Discard to
// properly advance the read offset. Otherwise, it will use ReadByte to fill the
// buffer with just the right number of bits.
func (pr *Reader) pullBits(nb uint) error {
	if pr.bufRd != nil {
		pr.discardBits += int(pr.fedBits - pr.numBits)
		for {
			if len(pr.bufPeek) == 0 {
				pr.fedBits = pr.numBits // Don't discard bits just added
				if _, err := pr.Flush(); err != nil {
					return err
				}

				var err error
				cntPeek := 8 // Minimum Peek amount to make progress
				if pr.bufRd.Buffered() > cntPeek {
					cntPeek = pr.bufRd.Buffered()
				}
				pr.bufPeek, err = pr.bufRd.Peek(cntPeek)
				pr.bufPeek = pr.bufPeek[int(pr.numBits/8):] // Skip buffered bits
				if len(pr.bufPeek) == 0 {
					if pr.numBits >= nb {
						break
					}
					if err == io.EOF {
						err = io.ErrUnexpectedEOF
					}
					return err
				}
			}
			cnt := int(64-pr.numBits) / 8
			if cnt > len(pr.bufPeek) {
				cnt = len(pr.bufPeek)
			}
			for _, c := range pr.bufPeek[:cnt] {
				if pr.bigEndian {
					c = internal.ReverseLUT[c]
				}
				pr.bufBits |= uint64(c) << pr.numBits
				pr.numBits += 8
			}
			pr.bufPeek = pr.bufPeek[cnt:]
			if pr.numBits > 56 {
				break
			}
		}
		pr.fedBits = pr.numBits
	} else {
		for pr.numBits < nb {
			c, err := pr.byteRd.ReadByte()
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return err
			}
			if pr.bigEndian {
				c = internal.ReverseLUT[c]
			}
			pr.bufBits |= uint64(c) << pr.numBits
			pr.numBits += 8
			pr.offset++
		}
	}
	return nil
}
