package files_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/mettjs/cairnmark/internal/files"
	metamem "github.com/mettjs/cairnmark/internal/metadata/memory"
	"github.com/mettjs/cairnmark/internal/storage"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

// outageBackend fails every range read once down is set — the store went away
// after the upload, as during an object-store outage. Writes still succeed, so
// the archive can be stored first.
type outageBackend struct {
	storage.Backend
	down bool
}

var errStoreDown = errors.New("s3: connection refused")

func (b *outageBackend) GetRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if b.down {
		return nil, errStoreDown
	}
	return b.Backend.GetRange(ctx, key, off, n)
}

func TestArchiveOpenReportsAStoreOutageAsItself(t *testing.T) {
	// The tail read that locates the directory is the first thing an open
	// does. When it fails the file has not been judged at all: reporting
	// "not an archive" would turn an outage into a client error, and a job
	// that ran into it would fail as though the caller's zip were bad.
	ctx := context.Background()
	b := &outageBackend{Backend: memory.New()}
	svc := files.New(b, metamem.New())
	arch := uploadArchive(t, svc, buildZip(t, docs...))
	b.down = true

	_, _, err := svc.ArchiveEntries(ctx, arch.ID)
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("listing during an outage: got %v want the store's error", err)
	}
	if errors.Is(err, files.ErrNotArchive) {
		t.Fatal("an outage was reported as not-an-archive")
	}
	if _, err := svc.SubmitExtraction(ctx, arch.ID, nil); !errors.Is(err, errStoreDown) || errors.Is(err, files.ErrNotArchive) {
		t.Fatalf("submission during an outage: got %v want the store's error", err)
	}
}
