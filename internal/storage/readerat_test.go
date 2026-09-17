package storage_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/mettjs/cairnmark/internal/storage"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

// countingBackend counts range requests so tests can prove the read-ahead
// collapses many small reads into few requests.
type countingBackend struct {
	*memory.Backend
	ranges int
}

func (c *countingBackend) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	c.ranges++
	return c.Backend.GetRange(ctx, key, offset, length)
}

func seed(t *testing.T, n int) (*countingBackend, []byte) {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	b := &countingBackend{Backend: memory.New()}
	if err := b.Put(context.Background(), "obj", bytes.NewReader(data), int64(n), ""); err != nil {
		t.Fatal(err)
	}
	return b, data
}

func TestReadAtContract(t *testing.T) {
	const size, window = 100, 16
	b, data := seed(t, size)
	ra := storage.NewReaderAt(context.Background(), b, "obj", size, window)

	tests := []struct {
		name    string
		off     int64
		n       int
		wantN   int
		wantEOF bool // io.EOF required; otherwise nil (or io.EOF exactly at the end)
		wantBad bool // a non-EOF error
	}{
		{"inside one window", 10, 5, 5, false, false},
		{"straddles two windows", 14, 20, 20, false, false},
		{"larger than the window", 0, 50, 50, false, false},
		{"larger than the window at the tail", 60, 40, 40, false, false},
		{"ends exactly at the end", 90, 10, 10, false, false},
		{"runs past the end", 95, 10, 5, true, false},
		{"starts at the end", 100, 5, 0, true, false},
		{"starts beyond the end", 150, 5, 0, true, false},
		{"zero-length read", 30, 0, 0, false, false},
		{"negative offset", -1, 5, 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := make([]byte, tt.n)
			n, err := ra.ReadAt(p, tt.off)
			if tt.wantBad {
				if err == nil || errors.Is(err, io.EOF) {
					t.Fatalf("want a non-EOF error, got n=%d err=%v", n, err)
				}
				return
			}
			if n != tt.wantN {
				t.Fatalf("n: got %d want %d (err=%v)", n, tt.wantN, err)
			}
			switch {
			case tt.wantEOF && !errors.Is(err, io.EOF):
				t.Fatalf("err: got %v want io.EOF", err)
			case !tt.wantEOF && err != nil && !(errors.Is(err, io.EOF) && tt.off+int64(n) == size):
				t.Fatalf("unexpected error: %v", err)
			}
			if n > 0 && !bytes.Equal(p[:n], data[tt.off:tt.off+int64(n)]) {
				t.Fatal("bytes differ from the object")
			}
		})
	}
}

func TestReadAtWholeObjectThroughUnalignedWindow(t *testing.T) {
	const size, window = 10_007, 1_000 // prime length: no window boundary meets the end
	b, data := seed(t, size)
	ra := storage.NewReaderAt(context.Background(), b, "obj", size, window)

	got, err := io.ReadAll(io.NewSectionReader(ra, 0, size))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("section read differs from the object")
	}
	if want := (size + window - 1) / window; b.ranges > want {
		t.Fatalf("range requests: got %d want <= %d", b.ranges, want)
	}
}

func TestReadAheadServesSmallSequentialReads(t *testing.T) {
	const size, window = 200 << 10, 64 << 10
	b, _ := seed(t, size)
	ra := storage.NewReaderAt(context.Background(), b, "obj", size, window)

	p := make([]byte, 4<<10)
	for off := int64(0); off < size; off += int64(len(p)) {
		if _, err := ra.ReadAt(p, off); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
	}
	// Fifty 4 KiB reads over 200 KiB with a 64 KiB window: four windows, not
	// fifty requests.
	if b.ranges != 4 {
		t.Fatalf("range requests: got %d want 4", b.ranges)
	}
}

func TestReadAheadServesTailBackscan(t *testing.T) {
	// archive/zip locates the end-of-central-directory record by reading the
	// last 1 KiB, then the last 65 KiB. A tail-aligned window serves both from
	// a single request.
	const size, window = 1 << 20, 128 << 10
	b, _ := seed(t, size)
	ra := storage.NewReaderAt(context.Background(), b, "obj", size, window)

	for _, n := range []int64{1024, 65 * 1024} {
		if _, err := ra.ReadAt(make([]byte, n), size-n); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("tail read of %d: %v", n, err)
		}
	}
	if b.ranges != 1 {
		t.Fatalf("range requests: got %d want 1", b.ranges)
	}
}

func TestShortObjectIsAStorageError(t *testing.T) {
	// The row claims 100 bytes; the store holds 60. That must surface as an
	// error — never a silent short read — and must not look like the
	// truncated-archive error a zip parser reports.
	b, _ := seed(t, 60)
	ra := storage.NewReaderAt(context.Background(), b, "obj", 100, 16)

	_, err := ra.ReadAt(make([]byte, 10), 55)
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want a distinct storage error, got %v", err)
	}
}

func TestZipOpenCostsBoundedRequests(t *testing.T) {
	// The point of the read-ahead: opening a zip through the store and walking
	// every entry costs a handful of range requests, not one per 4 KiB of
	// central directory plus one per local header.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for i := range 200 {
		w, err := zw.Create(fmt.Sprintf("docs/report-%03d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "report %d body", i)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := zb.Bytes()

	b := &countingBackend{Backend: memory.New()}
	if err := b.Put(context.Background(), "z", bytes.NewReader(data), int64(len(data)), ""); err != nil {
		t.Fatal(err)
	}
	ra := storage.NewReaderAt(context.Background(), b, "z", int64(len(data)), storage.DefaultReadWindow)

	zr, err := zip.NewReader(ra, int64(len(data)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	if len(zr.File) != 200 {
		t.Fatalf("entries: got %d want 200", len(zr.File))
	}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		rc.Close()
	}
	// The archive is a few tens of KiB; one window holds all of it.
	if b.ranges != 1 {
		t.Fatalf("range requests: got %d want 1", b.ranges)
	}
}

// truncatingBackend serves only the first cut bytes of a range starting at or
// past failFrom, then fails — the shape of a connection reset mid-body.
type truncatingBackend struct {
	*memory.Backend
	failFrom int64
	cut      int64
}

func (b *truncatingBackend) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	rc, err := b.Backend.GetRange(ctx, key, offset, length)
	if err != nil || offset < b.failFrom {
		return rc, err
	}
	return readCloser{Reader: io.MultiReader(io.LimitReader(rc, b.cut), errReader{}), Closer: rc}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestFailedFillLeavesNoStaleWindow(t *testing.T) {
	// A fill that fails part-way has already overwritten the cached window's
	// backing array. If the window still claimed its old range, the next read
	// inside that range would be served from the buffer and return the *new*
	// range's bytes with a nil error — a silent wrong read.
	const size, window = 100, 16
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}
	mem := memory.New()
	if err := mem.Put(context.Background(), "obj", bytes.NewReader(data), size, ""); err != nil {
		t.Fatal(err)
	}
	b := &truncatingBackend{Backend: mem, failFrom: window, cut: window / 2}
	ra := storage.NewReaderAt(context.Background(), b, "obj", size, window)

	if _, err := ra.ReadAt(make([]byte, 4), 0); err != nil {
		t.Fatalf("warm the first window: %v", err)
	}
	if _, err := ra.ReadAt(make([]byte, 4), 64); err == nil {
		t.Fatal("want the injected mid-body failure, got nil")
	}
	p := make([]byte, 4)
	if _, err := ra.ReadAt(p, 0); err != nil {
		t.Fatalf("read after the failed fill: %v", err)
	}
	if !bytes.Equal(p, data[:4]) {
		t.Fatalf("stale window served: got %v want %v", p, data[:4])
	}
}
