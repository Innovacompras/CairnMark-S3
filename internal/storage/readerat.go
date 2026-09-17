package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// DefaultReadWindow is the read-ahead length NewReaderAt uses when the caller
// passes a non-positive window. 1 MiB covers archive/zip's 65 KiB tail
// backscan and the whole central directory of any realistic document archive
// in a single range request.
const DefaultReadWindow = 1 << 20

// readerAt adapts a stored object to io.ReaderAt over Backend.GetRange with a
// windowed read-ahead. Without it, a consumer that issues many small reads —
// archive/zip walks a central directory 4 KiB at a time — would cost one HTTP
// range request per read.
type readerAt struct {
	ctx    context.Context
	b      Backend
	key    string
	size   int64
	window int64

	mu     sync.Mutex
	buf    []byte // the most recently fetched window
	bufOff int64  // object offset of buf[0]
}

// NewReaderAt returns an io.ReaderAt over the object at key. size must be the
// object's exact length — the caller has it from the metadata row, so no Stat
// round-trip is needed. window is the read-ahead length (DefaultReadWindow if
// non-positive): a cache miss fetches [off, off+window), or the object's final
// window bytes when off lies within one window of the end, so a tail-first
// reader like archive/zip is served from a single request.
//
// ctx is captured, against the usual convention, because io.ReaderAt has no
// context parameter. The reader is scoped to one request and dies with it.
//
// The returned reader honours the io.ReaderAt contract: ReadAt fills p or
// returns a non-nil error, with io.EOF exactly at the object's end. It is safe
// for concurrent use; calls are serialised.
func NewReaderAt(ctx context.Context, b Backend, key string, size, window int64) io.ReaderAt {
	if window <= 0 {
		window = DefaultReadWindow
	}
	return &readerAt{ctx: ctx, b: b, key: key, size: size, window: window}
}

func (r *readerAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("storage: negative read offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	var atEnd error
	if int64(len(p)) > r.size-off {
		p, atEnd = p[:r.size-off], io.EOF
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if !r.cached(pos) {
			if err := r.fill(pos); err != nil {
				return n, err
			}
		}
		n += copy(p[n:], r.buf[pos-r.bufOff:])
	}
	return n, atEnd
}

// cached reports whether the byte at object offset pos is in the window.
func (r *readerAt) cached(pos int64) bool {
	return pos >= r.bufOff && pos < r.bufOff+int64(len(r.buf))
}

// fill replaces the window with one window of bytes covering pos. The window is
// tail-aligned when pos lies within one window of the end, so archive/zip's
// backscan (last 1 KiB, then last 65 KiB) costs one request rather than two.
//
// One window, never more, however much the caller asked for: a read larger than
// the window is served by successive fills from ReadAt's loop. Sizing the window
// to the request instead would let one large ReadAt — a multipart upload part,
// say — pin a buffer that size for the reader's life, since the backing array is
// never released.
func (r *readerAt) fill(pos int64) error {
	length := r.window
	start := pos
	if start+length > r.size {
		start = max(0, r.size-length)
	}
	length = min(length, r.size-start)

	rc, err := r.b.GetRange(r.ctx, r.key, start, length)
	if err != nil {
		return fmt.Errorf("storage: read %q at %d: %w", r.key, start, err)
	}
	defer rc.Close()

	// Invalidated before the read, not after it: the read writes into buf's
	// backing array, so a partial failure would otherwise leave bufOff
	// describing a range those bytes no longer hold, and the next cached read
	// would return the wrong bytes with a nil error.
	if int64(cap(r.buf)) < length {
		r.buf = make([]byte, 0, length)
	}
	r.buf, r.bufOff = r.buf[:0], 0
	buf := r.buf[:length]
	if _, err := io.ReadFull(rc, buf); err != nil {
		// Deliberately not wrapped: a short object is a storage inconsistency
		// (the row's size disagrees with the store) and must not be mistaken
		// for the truncated-archive io.ErrUnexpectedEOF a zip parser reports.
		return fmt.Errorf("storage: read %q at %d: object shorter than its recorded size %d (%v)",
			r.key, start, r.size, err)
	}
	r.buf, r.bufOff = buf, start
	return nil
}
