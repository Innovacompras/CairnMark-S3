package archive

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
)

// zipWalker is the zip implementation of Walker, over archive/zip.
type zipWalker struct {
	r       *zip.Reader
	entries []Entry
}

// OpenZip parses the central directory of the zip held in ra — eagerly, into
// r.File — classifies every entry, and returns a Walker over them. No content
// is touched. ErrNotArchive when the bytes are not a zip.
//
// It returns the Walker interface rather than the concrete type so callers stay
// format-agnostic: when a second format arrives it gets its own constructor and
// a sniffing Open above both, and nothing above this package changes.
func OpenZip(ra io.ReaderAt, size int64, rules Rules) (Walker, error) {
	// Before zip.NewReader, not after: it parses the whole central directory
	// eagerly into r.File, so any cap read from the parsed result is applied
	// once the allocation has already happened. See directorySpan for why the
	// declared entry count cannot be used for this.
	maxDir := rules.MaxDirectoryBytes
	if maxDir <= 0 {
		maxDir = DefaultMaxDirectoryBytes
	}
	span, err := directorySpan(ra, size)
	if errors.Is(err, errNoDirectoryEnd) {
		return nil, ErrNotArchive
	}
	if err != nil {
		// The store failed under the guard. That says nothing about the
		// archive, and calling it one would turn an outage into a 415.
		return nil, err
	}
	if span > maxDir {
		return nil, fmt.Errorf("%w: the central directory spans %d bytes (up to %d entries), the cap is %d",
			ErrDirectoryTooLarge, span, maxEntriesIn(span), maxDir)
	}

	r, err := zip.NewReader(ra, size)
	switch {
	case errors.Is(err, zip.ErrFormat), errors.Is(err, io.ErrUnexpectedEOF):
		// No end-of-central-directory record, or a directory that ends early:
		// not a zip, or not a whole one. A short *object* is reported by the
		// storage ReaderAt as a distinct error and never reaches this branch.
		return nil, ErrNotArchive
	case errors.Is(err, zip.ErrInsecurePath):
		// Raised only under GODEBUG=zipinsecurepath=0, and the reader is still
		// usable. classify applies the same name check itself, so behaviour
		// does not depend on the environment.
	case err != nil:
		return nil, fmt.Errorf("archive: open zip: %w", err)
	}

	w := &zipWalker{r: r, entries: make([]Entry, len(r.File))}
	for i, f := range r.File {
		w.entries[i] = classify(i, f, rules)
	}
	return w, nil
}

func (w *zipWalker) Entries() []Entry { return w.entries }

func (w *zipWalker) Open(index int) (io.ReadCloser, error) {
	if index < 0 || index >= len(w.entries) {
		return nil, fmt.Errorf("%w: entry %d out of range (%d entries)", ErrNotExtractable, index, len(w.entries))
	}
	e := w.entries[index]
	if !e.Selectable() {
		return nil, fmt.Errorf("%w: entry %d (%s): %s", ErrNotExtractable, index, e.Name, e.Skip)
	}
	// f.Open, not DataOffset plus a hand-rolled decompressor: Open dispatches
	// Store vs Deflate and verifies the CRC32 at EOF. entryReader makes that
	// verdict reach a consumer that never reads past Size bytes.
	rc, err := w.r.File[index].Open()
	if err != nil {
		return nil, fmt.Errorf("archive: open entry %d (%s): %w", index, e.Name, err)
	}
	return &entryReader{rc: rc, left: e.Size}, nil
}
