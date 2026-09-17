package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/mettjs/cairnmark/internal/archive"
	"github.com/mettjs/cairnmark/internal/metadata"
)

// extractEntry stores one entry as an ordinary upload. It returns a skip
// reason when the entry itself is at fault (its content failed the archive's
// checks) and an error when the run cannot continue (the store or the
// database failed).
//
// The entry's size is known from the directory, so the size-unknown Stat
// round-trip in stage is skipped; the content type comes from the entry's
// extension, with stage's sniff as the fallback — without it, a content-type
// search would be blind to exactly the documents just extracted.
func (s *Service) extractEntry(ctx context.Context, a *opened, e ArchiveEntry) (SkipReason, error) {
	rc, err := a.walker.Open(e.Index)
	if err != nil {
		switch {
		case a.store.fault() != nil:
			return "", fmt.Errorf("files: extract entry %d of %s: %w", e.Index, a.file.ID, a.store.fault())
		case errors.Is(err, archive.ErrNotExtractable):
			// Our own mistake, not a damaged archive: this entry was skipped or
			// does not exist, so something above chose it wrongly. Reporting it
			// as corruption would blame the caller's file and hide the bug.
			return "", fmt.Errorf("files: extract entry %d of %s: %w", e.Index, a.file.ID, err)
		}
		return SkipCorrupt, nil // a local header that does not match its directory record
	}
	defer rc.Close()

	src := &sourceReader{Reader: rc}
	_, err = s.Upload(ctx, UploadInput{
		Filename:    path.Base(e.Name), // the name it would be downloaded as; the full path lives in a tag
		ContentType: e.ContentType,
		Size:        e.Size,
		Body:        src,
		Tags: map[string]any{
			metadata.TagArchiveID:    a.file.ID,
			metadata.TagArchivePath:  e.Name,
			metadata.TagArchiveIndex: e.Index,
		},
	})
	switch {
	case err == nil:
		return "", nil
	case a.store.fault() != nil:
		return "", fmt.Errorf("files: extract entry %d of %s: %w", e.Index, a.file.ID, a.store.fault())
	case src.err != nil:
		// The archive's own verdict on this entry (CRC32, truncation, bad
		// deflate stream). Nothing was committed: the write failed before the
		// row, and any partial object is reclaimed by GC like any orphan.
		return SkipCorrupt, nil
	default:
		return "", fmt.Errorf("files: extract entry %d (%s) of %s: %w", e.Index, e.Name, a.file.ID, err)
	}
}

// sourceReader records whether the entry's content failed to read, so an
// Upload error can be attributed to the source rather than the sink no matter
// how the layers between wrap it — the same flag-carrying idea as the api
// layer's limitReader.
type sourceReader struct {
	io.Reader
	err error
}

func (r *sourceReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}
