package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/mettjs/cairnmark/internal/archive"
	"github.com/mettjs/cairnmark/internal/metadata"
	"github.com/mettjs/cairnmark/internal/storage"
)

// ArchiveEntry and SkipReason are aliased so the api layer can describe an
// archive without importing the archive package directly.
type (
	ArchiveEntry = archive.Entry
	SkipReason   = archive.SkipReason
)

// Skip reasons decided by the service rather than by the archive's directory.
const (
	SkipAlreadyExtracted  SkipReason = "already_extracted"  // a live child row exists for this index
	SkipPreviouslyDeleted SkipReason = "previously_deleted" // the child was soft-deleted: delete wins over re-extract
	SkipNotSelected       SkipReason = "not_selected"       // absent from the caller's entries list
	SkipCorrupt           SkipReason = "corrupt"            // the content failed the archive's own checks
)

// ReservedTagPrefix marks metadata keys the service writes and clients may
// not: the api layer rejects them on upload and PATCH, and the repository
// carries them across a replace.
const ReservedTagPrefix = metadata.ReservedTagPrefix

// ArchiveLimits bound an extraction. Zero disables each cap.
type ArchiveLimits struct {
	MaxEntryBytes int64    // per extracted entry — an entry is an upload, so this reuses the upload cap
	MaxEntries    int      // entries in the archive's directory; both endpoints refuse larger archives
	MaxTotalBytes int64    // uncompressed bytes one extraction may write
	MaxRatio      int64    // per-entry uncompressed ÷ compressed
	Extensions    []string // allowlist, lower-case with the leading dot; empty allows all

	// MaxDirectoryBytes bounds the central directory's span, refusing an
	// archive before its directory is parsed. It is the only cap applied
	// pre-parse — MaxEntries cannot be, since the count does not exist until
	// the parse is done. Zero does not disable it: it derives the bound from
	// the entry ceiling instead (see maxRecordBudget).
	MaxDirectoryBytes int64
}

// Option configures a Service beyond its two collaborators.
type Option func(*Service)

// WithArchiveLimits sets the extraction caps. Without it extraction is uncapped.
func WithArchiveLimits(l ArchiveLimits) Option {
	return func(s *Service) { s.limits = l }
}

// readWindow is the read-ahead over the stored archive. One window serves the
// whole open — tail backscan plus central directory — for any realistic
// document archive, and then the bodies of entries laid out sequentially.
const readWindow = storage.DefaultReadWindow

// maxListedEntries floors ArchiveLimits.MaxEntries. Zero is documented as
// "uncapped", which extraction can afford — its response is a bounded summary —
// but the listing cannot: GET /files/{id}/archive serialises one element per
// entry with no cursor, so an uncapped deployment pointed at a 65,535-entry zip
// would answer with tens of megabytes. "Uncapped" therefore means this, not
// unbounded.
const maxListedEntries = 10_000

// maxRecordBudget is a generous per-record allowance used to turn the entry
// ceiling into a directory-span bound: 46 fixed bytes plus room for a ~460
// character path, extra field and comment. An archive that would pass the entry
// cap essentially cannot exceed the product, so the span guard refuses nothing
// the count check would have allowed — while still refusing a directory crafted
// to be parsed before any count exists.
//
// The trade, stated: an archive whose paths really do average more than ~460
// characters is refused even under the entry cap. Raise
// CAIRNMARK_ARCHIVE_MAX_DIRECTORY_BYTES for it; the error names the cap.
const maxRecordBudget = 512

// ArchiveEntries lists the entries of the archive stored as id, each classified
// as selectable or skipped with a reason. It writes nothing, and it is what
// lets a caller answer "which file exactly?" before anything is stored.
func (s *Service) ArchiveEntries(ctx context.Context, id string) (*File, []ArchiveEntry, error) {
	a, err := s.openArchive(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return a.file, a.walker.Entries(), nil
}

// opened is an archive resolved to its record and parsed directory.
type opened struct {
	file   *File
	walker archive.Walker
	store  *faultReaderAt
}

// openArchive resolves the record and opens a Walker over the stored object
// through a windowed ReaderAt, so the archive is never pulled into memory and
// no Backend method beyond GetRange is needed.
func (s *Service) openArchive(ctx context.Context, id string) (*opened, error) {
	f, err := s.Metadata(ctx, id)
	if err != nil {
		return nil, err
	}
	// The entry ceiling is decided once, here, because two caps depend on it:
	// the exact count check after the directory is parsed, and the byte span
	// allowed before it is.
	limit := s.limits.MaxEntries
	if limit <= 0 || limit > maxListedEntries {
		limit = maxListedEntries
	}
	dirBytes := s.limits.MaxDirectoryBytes
	if dirBytes <= 0 {
		dirBytes = int64(limit) * maxRecordBudget
	}

	store := &faultReaderAt{ReaderAt: storage.NewReaderAt(ctx, s.backend, f.StorageKey, f.SizeBytes, readWindow)}
	w, err := archive.OpenZip(store, f.SizeBytes, archive.Rules{
		MaxEntryBytes:     s.limits.MaxEntryBytes,
		MaxRatio:          s.limits.MaxRatio,
		Extensions:        s.limits.Extensions,
		MaxDirectoryBytes: dirBytes,
	})
	if fault := store.fault(); err != nil && fault != nil {
		// The store failed while the parser was reading. Whatever the parser
		// made of the bytes it did get, the answer is the store's — an internal
		// error — never a verdict on the archive.
		return nil, fmt.Errorf("files: open archive %s: %w", id, fault)
	}
	if errors.Is(err, archive.ErrNotArchive) {
		return nil, ErrNotArchive
	}
	// A directory too large to parse is the same class of answer as too many
	// entries or too many bytes: the archive exceeds a configured cap.
	if errors.Is(err, archive.ErrDirectoryTooLarge) {
		return nil, fmt.Errorf("%w: %w", ErrArchiveTooLarge, err)
	}
	if err != nil {
		return nil, fmt.Errorf("files: open archive %s: %w", id, err)
	}
	// Applied after the directory has been parsed — the count does not exist
	// before then — so this bounds what is listed and extracted, not the parse
	// itself. The archive's own upload cap bounds that.
	if n := len(w.Entries()); n > limit {
		return nil, fmt.Errorf("%w: %d entries, the cap is %d", ErrArchiveTooLarge, n, limit)
	}
	return &opened{file: f, walker: w, store: store}, nil
}

// faultReaderAt records the first error the object store returns, so the
// extraction loop can tell a storage failure (abort the run) from a corrupt
// entry (skip it and carry on) no matter how archive/zip wraps the error.
//
// The flag is atomic because the reader underneath is safe for concurrent use
// (storage.NewReaderAt serialises its own state) and a wrapper that read and
// wrote a plain field would quietly take that guarantee away from everything
// composed on top of it.
type faultReaderAt struct {
	io.ReaderAt
	err atomic.Pointer[error]
}

func (f *faultReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.ReaderAt.ReadAt(p, off)
	if err != nil && err != io.EOF {
		f.err.CompareAndSwap(nil, &err)
	}
	return n, err
}

// fault returns the first store error recorded, or nil if the store is healthy.
func (f *faultReaderAt) fault() error {
	if p := f.err.Load(); p != nil {
		return *p
	}
	return nil
}
