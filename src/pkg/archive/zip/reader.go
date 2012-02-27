// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package zip

import (
	"bufio"
	"compress/flate"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"io/ioutil"
	"os"
)

var (
	ErrFormat    = errors.New("zip: not a valid zip file")
	ErrAlgorithm = errors.New("zip: unsupported compression algorithm")
	ErrChecksum  = errors.New("zip: checksum error")
)

type Reader struct {
	r       io.ReaderAt
	File    []*File
	Comment string
}

type ReadCloser struct {
	f *os.File
	Reader
}

type File struct {
	FileHeader
	zipr         io.ReaderAt
	zipsize      int64
	headerOffset int64
}

func (f *File) hasDataDescriptor() bool {
	return f.Flags&0x8 != 0
}

// OpenReader will open the Zip file specified by name and return a ReadCloser.
func OpenReader(name string) (*ReadCloser, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r := new(ReadCloser)
	if err := r.init(f, fi.Size()); err != nil {
		f.Close()
		return nil, err
	}
	r.f = f
	return r, nil
}

// NewReader returns a new Reader reading from r, which is assumed to
// have the given size in bytes.
func NewReader(r io.ReaderAt, size int64) (*Reader, error) {
	zr := new(Reader)
	if err := zr.init(r, size); err != nil {
		return nil, err
	}
	return zr, nil
}

func (z *Reader) init(r io.ReaderAt, size int64) error {
	end, err := readDirectoryEnd(r, size)
	if err != nil {
		return err
	}
	z.r = r
	z.File = make([]*File, 0, end.directoryRecords)
	z.Comment = end.comment
	rs := io.NewSectionReader(r, 0, size)
	if _, err = rs.Seek(int64(end.directoryOffset), os.SEEK_SET); err != nil {
		return err
	}
	buf := bufio.NewReader(rs)

	// The count of files inside a zip is truncated to fit in a uint16.
	// Gloss over this by reading headers until we encounter
	// a bad one, and then only report a ErrFormat or UnexpectedEOF if
	// the file count modulo 65536 is incorrect.
	for {
		f := &File{zipr: r, zipsize: size}
		err = readDirectoryHeader(f, buf)
		if err == ErrFormat || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
		z.File = append(z.File, f)
	}
	if uint16(len(z.File)) != end.directoryRecords {
		// Return the readDirectoryHeader error if we read
		// the wrong number of directory entries.
		return err
	}
	return nil
}

// Close closes the Zip file, rendering it unusable for I/O.
func (rc *ReadCloser) Close() error {
	return rc.f.Close()
}

// Open returns a ReadCloser that provides access to the File's contents.
// Multiple files may be read concurrently.
func (f *File) Open() (rc io.ReadCloser, err error) {
	bodyOffset, err := f.findBodyOffset()
	if err != nil {
		return
	}
	size := int64(f.CompressedSize)
	if size == 0 && f.hasDataDescriptor() {
		// permit SectionReader to see the rest of the file
		size = f.zipsize - (f.headerOffset + bodyOffset)
	}
	r := io.NewSectionReader(f.zipr, f.headerOffset+bodyOffset, size)
	switch f.Method {
	case Store: // (no compression)
		rc = ioutil.NopCloser(r)
	case Deflate:
		rc = flate.NewReader(r)
	default:
		err = ErrAlgorithm
	}
	if rc != nil {
		rc = &checksumReader{rc, crc32.NewIEEE(), f, r}
	}
	return
}

type checksumReader struct {
	rc   io.ReadCloser
	hash hash.Hash32
	f    *File
	zipr io.Reader // for reading the data descriptor
}

func (r *checksumReader) Read(b []byte) (n int, err error) {
	n, err = r.rc.Read(b)
	r.hash.Write(b[:n])
	if err != io.EOF {
		return
	}
	if r.f.hasDataDescriptor() {
		if err = readDataDescriptor(r.zipr, r.f); err != nil {
			return
		}
	}
	if r.hash.Sum32() != r.f.CRC32 {
		err = ErrChecksum
	}
	return
}

func (r *checksumReader) Close() error { return r.rc.Close() }

func readFileHeader(f *File, r io.Reader) error {
	var b [fileHeaderLen]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return err
	}
	if sig := toUint32(b[:]); sig != fileHeaderSignature {
		return ErrFormat
	}
	f.ReaderVersion = toUint16(b[4:])
	f.Flags = toUint16(b[6:])
	f.Method = toUint16(b[8:])
	f.ModifiedTime = toUint16(b[10:])
	f.ModifiedDate = toUint16(b[12:])
	f.CRC32 = toUint32(b[14:])
	f.CompressedSize = toUint32(b[18:])
	f.UncompressedSize = toUint32(b[22:])
	filenameLen := int(toUint16(b[26:]))
	extraLen := int(toUint16(b[28:]))
	d := make([]byte, filenameLen+extraLen)
	if _, err := io.ReadFull(r, d); err != nil {
		return err
	}
	f.Name = string(d[:filenameLen])
	f.Extra = d[filenameLen:]
	return nil
}

// findBodyOffset does the minimum work to verify the file has a header
// and returns the file body offset.
func (f *File) findBodyOffset() (int64, error) {
	r := io.NewSectionReader(f.zipr, f.headerOffset, f.zipsize-f.headerOffset)
	var b [fileHeaderLen]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	if sig := toUint32(b[:4]); sig != fileHeaderSignature {
		return 0, ErrFormat
	}
	filenameLen := int(toUint16(b[26:28]))
	extraLen := int(toUint16(b[28:30]))
	return int64(fileHeaderLen + filenameLen + extraLen), nil
}

// readDirectoryHeader attempts to read a directory header from r.
// It returns io.ErrUnexpectedEOF if it cannot read a complete header,
// and ErrFormat if it doesn't find a valid header signature.
func readDirectoryHeader(f *File, r io.Reader) error {
	var b [directoryHeaderLen]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return err
	}
	if sig := toUint32(b[:]); sig != directoryHeaderSignature {
		return ErrFormat
	}
	f.CreatorVersion = toUint16(b[4:])
	f.ReaderVersion = toUint16(b[6:])
	f.Flags = toUint16(b[8:])
	f.Method = toUint16(b[10:])
	f.ModifiedTime = toUint16(b[12:])
	f.ModifiedDate = toUint16(b[14:])
	f.CRC32 = toUint32(b[16:])
	f.CompressedSize = toUint32(b[20:])
	f.UncompressedSize = toUint32(b[24:])
	filenameLen := int(toUint16(b[28:]))
	extraLen := int(toUint16(b[30:32]))
	commentLen := int(toUint16(b[32:]))
	// skipped start disk number and internal attributes (2x uint16)
	f.ExternalAttrs = toUint32(b[38:])
	f.headerOffset = int64(toUint32(b[42:]))
	d := make([]byte, filenameLen+extraLen+commentLen)
	if _, err := io.ReadFull(r, d); err != nil {
		return err
	}
	f.Name = string(d[:filenameLen])
	f.Extra = d[filenameLen : filenameLen+extraLen]
	f.Comment = string(d[filenameLen+extraLen:])
	return nil
}

func readDataDescriptor(r io.Reader, f *File) error {
	var b [dataDescriptorLen]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return err
	}
	f.CRC32 = toUint32(b[:4])
	f.CompressedSize = toUint32(b[4:8])
	f.UncompressedSize = toUint32(b[8:12])
	return nil
}

func readDirectoryEnd(r io.ReaderAt, size int64) (dir *directoryEnd, err error) {
	// look for directoryEndSignature in the last 1k, then in the last 65k
	var b []byte
	for i, bLen := range []int64{1024, 65 * 1024} {
		if bLen > size {
			bLen = size
		}
		b = make([]byte, int(bLen))
		if _, err := r.ReadAt(b, size-bLen); err != nil && err != io.EOF {
			return nil, err
		}
		if p := findSignatureInBlock(b); p >= 0 {
			b = b[p:]
			break
		}
		if i == 1 || bLen == size {
			return nil, ErrFormat
		}
	}

	// read header into struct
	d := new(directoryEnd)
	d.diskNbr = toUint16(b[4:])
	d.dirDiskNbr = toUint16(b[6:])
	d.dirRecordsThisDisk = toUint16(b[8:])
	d.directoryRecords = toUint16(b[10:])
	d.directorySize = toUint32(b[12:])
	d.directoryOffset = toUint32(b[16:])
	d.commentLen = toUint16(b[20:])
	d.comment = string(b[22 : 22+int(d.commentLen)])
	return d, nil
}

func findSignatureInBlock(b []byte) int {
	for i := len(b) - directoryEndLen; i >= 0; i-- {
		// defined from directoryEndSignature in struct.go
		if b[i] == 'P' && b[i+1] == 'K' && b[i+2] == 0x05 && b[i+3] == 0x06 {
			// n is length of comment
			n := int(b[i+directoryEndLen-2]) | int(b[i+directoryEndLen-1])<<8
			if n+directoryEndLen+i == len(b) {
				return i
			}
		}
	}
	return -1
}

func toUint16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }

func toUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
