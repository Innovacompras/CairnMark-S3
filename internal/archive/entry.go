package archive

import (
	"errors"
	"io"
)

// errOverrun is returned when an entry yields more bytes than its directory
// record declares. archive/zip reports that itself; this is the fallback.
var errOverrun = errors.New("archive: entry longer than declared")

// entryReader delivers an entry's content and surfaces the archive's
// end-of-entry verdict early.
//
// archive/zip checks an entry's CRC32 only when the consumer reads past the
// last byte and observes io.EOF. A consumer that reads exactly Size bytes and
// stops — io.ReadFull, a Content-Length-bounded HTTP body, an S3 multipart
// uploader — never triggers the check, and a corrupt entry would be stored
// under a clean SHA-256 of the wrong bytes. So once the final byte is in hand,
// entryReader probes for EOF itself and, when the verdict is bad, returns it
// instead of the final chunk. The consumer then fails before anything is
// committed, whichever way it reads.
type entryReader struct {
	rc   io.ReadCloser
	left int64 // bytes not yet delivered
	err  error // sticky terminal state
}

func (r *entryReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	var n int
	if r.left > 0 {
		if int64(len(p)) > r.left {
			p = p[:r.left]
		}
		var err error
		n, err = r.rc.Read(p)
		r.left -= int64(n)
		switch {
		case err == io.EOF && r.left > 0:
			return r.fail(io.ErrUnexpectedEOF)
		case err == io.EOF:
			return r.done(n) // the underlying reader already rendered a clean verdict
		case err != nil:
			return r.fail(err)
		case r.left > 0:
			return n, nil
		}
	}

	// Every byte is in hand. Ask for the verdict now rather than leaving it to
	// a consumer that may never read again.
	var probe [1]byte
	switch m, err := r.rc.Read(probe[:]); {
	case m > 0:
		return r.fail(errOverrun)
	case err == io.EOF:
		return r.done(n)
	case err != nil:
		return r.fail(err)
	default:
		return n, nil // no answer yet; the next call probes again
	}
}

// done records a clean end of entry. The bytes read on this call are still
// delivered; the call after returns io.EOF.
func (r *entryReader) done(n int) (int, error) {
	r.err = io.EOF
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// fail records a terminal error and withholds the bytes read on this call, so
// no consumer can commit content whose verdict was bad.
func (r *entryReader) fail(err error) (int, error) {
	r.err = err
	return 0, err
}

func (r *entryReader) Close() error { return r.rc.Close() }
