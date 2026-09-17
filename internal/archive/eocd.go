package archive

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Signatures and fixed record lengths from the zip specification. They mirror
// archive/zip's own unexported constants (struct.go) because the guard below
// must read the end-of-central-directory record itself, before handing the
// archive to zip.NewReader.
const (
	directoryEndSignature   = 0x06054b50
	directory64LocSignature = 0x07064b50
	directory64EndSignature = 0x06064b50

	directoryEndLen   = 22 // + comment
	directory64LocLen = 20
	directory64EndLen = 56 // + extra

	// directoryHeaderLen is the smallest a central directory record can be —
	// 46 bytes before its name, extra field and comment. It is the divisor that
	// turns a byte span into the most entries that span could possibly hold.
	directoryHeaderLen = 46

	// maxCommentLen is the largest trailing comment a zip may carry, and so how
	// far back the end record may sit from the end of the object.
	maxCommentLen = 1 << 16
)

// errNoDirectoryEnd means the bytes carry no usable end-of-central-directory
// record. Callers map it to ErrNotArchive: it is the same verdict zip.NewReader
// reaches as zip.ErrFormat, only sooner.
var errNoDirectoryEnd = errors.New("archive: no end-of-central-directory record")

// directorySpan returns an upper bound on the bytes zip.NewReader may parse as
// central directory records: from the directory's declared start to the end of
// the object.
//
// A span, and not the entry count the archive declares, because that count is
// load-bearing on nothing. archive/zip uses it as a preallocation hint and then
// reads headers until one is malformed, comparing the total only modulo 65536
// ("The count of files inside a zip is truncated to fit in a uint16"). So an
// archive may declare 10 entries, contain 500,000, be parsed in full — one
// *zip.File allocated per record — and only then be rejected, with the memory
// already spent. An attacker who declares 500000 mod 65536 is not rejected at
// all. Go's own comment on the hint says it plainly: "the number of directory
// records is not validated".
//
// The span cannot lie in the direction that matters. size is the object's true
// length from the metadata row. A directoryOffset that understates leaves a
// larger span and so a more conservative refusal; one that overstates leaves a
// smaller span, but then sends zip.NewReader to bytes that are not a directory
// record, which fails as ErrNotArchive before anything is allocated.
func directorySpan(ra io.ReaderAt, size int64) (int64, error) {
	off, err := directoryOffset(ra, size)
	if err != nil {
		return 0, err
	}
	if off < 0 || off >= size {
		// A directory starting at or past the end of the object is not one.
		return 0, errNoDirectoryEnd
	}
	return size - off, nil
}

// maxEntriesIn reports the most central directory records a span of n bytes
// could hold. Deliberately generous: every real record carries a filename, so
// this over-counts, which is why it bounds the parse rather than standing in
// for the exact entry cap applied after the directory is read.
func maxEntriesIn(span int64) int64 {
	return span / directoryHeaderLen
}

// directoryOffset locates the end-of-central-directory record and returns where
// it says the central directory begins.
func directoryOffset(ra io.ReaderAt, size int64) (int64, error) {
	n := min(int64(directoryEndLen+maxCommentLen), size)
	if n < directoryEndLen {
		return 0, errNoDirectoryEnd
	}
	tail := make([]byte, n)
	if read, err := ra.ReadAt(tail, size-n); (err != nil && err != io.EOF) || read < len(tail) {
		if err != nil && err != io.EOF {
			return 0, fmt.Errorf("archive: read directory end: %w", err)
		}
		return 0, errNoDirectoryEnd
	}

	i := findDirectoryEnd(tail)
	if i < 0 {
		return 0, errNoDirectoryEnd
	}
	rec := tail[i:]
	records := binary.LittleEndian.Uint16(rec[10:])
	dirSize := binary.LittleEndian.Uint32(rec[12:])
	off := binary.LittleEndian.Uint32(rec[16:])

	// Any saturated field *may* mean zip64. The 0xffff test on directorySize
	// follows archive/zip rather than the specification's 0xffffffff: this
	// guard has to agree with the parser it is guarding, not with the spec.
	if records == 0xffff || dirSize == 0xffff || off == 0xffffffff {
		off64, found, err := directory64Offset(ra, size-n+int64(i))
		if err != nil {
			return 0, err
		}
		if found {
			return off64, nil
		}
		// No locator, so not zip64 after all — a zip holding exactly 65,535
		// entries saturates the 16-bit count without needing zip64 at all.
		// archive/zip falls back to the 32-bit fields here; so do we, or a
		// perfectly valid archive would be refused.
	}
	return int64(off), nil
}

// directory64Offset reads the zip64 central-directory offset. found is false
// when no zip64 locator precedes the end record, which is a normal archive and
// not an error.
func directory64Offset(ra io.ReaderAt, endOffset int64) (off int64, found bool, err error) {
	locOff := endOffset - directory64LocLen
	if locOff < 0 {
		return 0, false, nil
	}
	loc := make([]byte, directory64LocLen)
	if read, err := ra.ReadAt(loc, locOff); (err != nil && err != io.EOF) || read < len(loc) {
		if err != nil && err != io.EOF {
			return 0, false, fmt.Errorf("archive: read zip64 locator: %w", err)
		}
		return 0, false, nil
	}
	if binary.LittleEndian.Uint32(loc) != directory64LocSignature {
		return 0, false, nil
	}

	// Past here the archive claims to be zip64, so a record that does not
	// parse is malformed rather than absent.
	end64 := binary.LittleEndian.Uint64(loc[8:])
	if end64 > math.MaxInt64 {
		return 0, false, errNoDirectoryEnd
	}
	rec := make([]byte, directory64EndLen)
	if read, err := ra.ReadAt(rec, int64(end64)); (err != nil && err != io.EOF) || read < len(rec) {
		return 0, false, errNoDirectoryEnd
	}
	if binary.LittleEndian.Uint32(rec) != directory64EndSignature {
		return 0, false, errNoDirectoryEnd
	}
	o := binary.LittleEndian.Uint64(rec[48:])
	if o > math.MaxInt64 {
		return 0, false, errNoDirectoryEnd
	}
	return int64(o), true, nil
}

// findDirectoryEnd returns the index of the end-of-central-directory record in
// buf, or -1. The record's comment length must account for exactly the bytes
// that follow it — the same disambiguation archive/zip performs, and what keeps
// a signature that merely appears inside stored data or inside a comment from
// being mistaken for the real record.
func findDirectoryEnd(buf []byte) int {
	for i := len(buf) - directoryEndLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(buf[i:]) != directoryEndSignature {
			continue
		}
		commentLen := int(binary.LittleEndian.Uint16(buf[i+directoryEndLen-2:]))
		if i+directoryEndLen+commentLen == len(buf) {
			return i
		}
	}
	return -1
}
