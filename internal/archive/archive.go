// Package archive reads the members of an archive held behind an io.ReaderAt:
// entries out, content per entry on demand, without ever materialising the
// archive. It is pure — it imports neither storage nor metadata — so it is
// unit-testable against a bytes.Reader; the bridge to the object store lives
// one layer up, in files.
//
// Walker is the format seam: a caller holds a Walker and never names a format,
// so a second format is a new Walker plus a sniffing Open above the
// constructors — no caller changes. That dispatcher does not exist yet, and
// exactly one implementation does (zip), so the constructor is named for what
// it actually does: OpenZip. A general-purpose Open forwarding to the only
// format would claim a dispatch there is nothing to dispatch on.
package archive

import (
	"errors"
	"io"
)

// ErrNotArchive is returned by OpenZip when the bytes are not a zip. Callers
// map it to a client error: pointing the archive endpoints at an ordinary file
// is the one mistake a caller will make by accident.
var ErrNotArchive = errors.New("archive: not a supported archive")

// ErrDirectoryTooLarge is returned by OpenZip when the archive's central
// directory spans more bytes than Rules.MaxDirectoryBytes allows. It is
// refused before the directory is parsed, so the allocation it would have cost
// never happens. Callers map it to the same client error as the other caps.
var ErrDirectoryTooLarge = errors.New("archive: central directory exceeds the parse limit")

// ErrNotExtractable is returned by Walker.Open for an index that is out of
// range or classified as skipped. Both mean the caller asked for the wrong
// entry — a selection or classification bug above this package — so it is
// distinguishable from a damaged entry and must not be reported as one.
var ErrNotExtractable = errors.New("archive: entry is not extractable")

// SkipReason says why an entry is not extractable. The values are stable
// strings: they are reported to clients verbatim.
type SkipReason string

// Reasons decided from the archive's directory alone, before any content is
// read. Layers above add their own (already extracted, not selected, …).
const (
	SkipDirectory         SkipReason = "directory"
	SkipNonRegular        SkipReason = "non_regular"        // symlink, device, …
	SkipPlatformMetadata  SkipReason = "platform_metadata"  // __MACOSX/, ._*, .DS_Store, Thumbs.db
	SkipEncrypted         SkipReason = "encrypted"          // archive/zip cannot decrypt
	SkipUnsupportedMethod SkipReason = "unsupported_method" // anything but Store and Deflate
	SkipUnsafeName        SkipReason = "unsafe_name"        // absolute, escaping, or backslashed path
	SkipTooLarge          SkipReason = "too_large"          // over Rules.MaxEntryBytes
	SkipRatioExceeded     SkipReason = "ratio_exceeded"     // over Rules.MaxRatio
	SkipExtension         SkipReason = "extension_not_allowed"
)

// Entry describes one member of an archive as read from its directory, before
// any content is read. Index is its position in that directory and the handle
// callers select by: names need not be unique within an archive, indexes are.
type Entry struct {
	Index       int
	Name        string     // full path inside the archive, as stored
	Size        int64      // uncompressed length
	CRC32       uint32     // the archive's own checksum of the content
	ContentType string     // from the name's extension; empty when unknown
	Skip        SkipReason // why the entry cannot be extracted; empty when it can
}

// Selectable reports whether the entry may be opened and extracted.
func (e Entry) Selectable() bool { return e.Skip == "" }

// DefaultMaxDirectoryBytes is the backstop OpenZip applies when the caller
// supplies no bound. Deliberately loose — it admits a 65,535-entry zip64
// directory with long paths — because it is not meant to be the real cap: a
// caller that knows its entry limit should derive a much tighter span from it
// and pass that, as files.openArchive does. This only stops the unbounded case
// for a caller that forgot.
const DefaultMaxDirectoryBytes = 64 << 20

// Rules are the limits applied while opening and classifying an archive. Zero
// disables each per-entry cap and an empty Extensions list allows every
// extension — but MaxDirectoryBytes is not a per-entry cap and zero does not
// disable it, see the field.
type Rules struct {
	MaxEntryBytes int64
	MaxRatio      int64    // uncompressed ÷ compressed, per entry
	Extensions    []string // allowlist, lower-case with the leading dot

	// MaxDirectoryBytes bounds the central directory's byte span, and so what
	// zip.NewReader may allocate parsing it. Unlike the caps above, a
	// non-positive value falls back to DefaultMaxDirectoryBytes rather than
	// disabling the check: this is the only bound applied *before* the parse,
	// and switching it off restores the unbounded allocation it exists to
	// prevent. Raise it for an archive with a genuinely enormous directory.
	MaxDirectoryBytes int64
}

// Walker enumerates an archive's entries and opens their contents.
type Walker interface {
	// Entries returns every entry in directory order, already classified.
	// The slice is shared; callers must not modify it.
	Entries() []Entry

	// Open returns the uncompressed content of a selectable entry, or
	// ErrNotExtractable when the index is out of range or skipped. The reader
	// surfaces the archive's own integrity verdict (CRC32) before the final
	// bytes are delivered, so a consumer that stops reading at exactly Size
	// bytes still sees a corrupt entry fail. The caller must Close it.
	Open(index int) (io.ReadCloser, error)
}
