package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/mettjs/cairnmark/internal/files"
	metamem "github.com/mettjs/cairnmark/internal/metadata/memory"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

func TestUploadDownloadRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	want := []byte("cairn marks the path")

	f, err := svc.Upload(ctx, files.UploadInput{
		Filename:    "note.txt",
		ContentType: "text/plain",
		Size:        int64(len(want)),
		Body:        bytes.NewReader(want),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if f.ID == "" || f.StorageKey == "" || f.ID == f.StorageKey {
		t.Fatalf("expected distinct non-empty id/key, got id=%q key=%q", f.ID, f.StorageKey)
	}

	got, rc, err := svc.Open(ctx, f.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if !bytes.Equal(body, want) {
		t.Fatalf("round-trip mismatch: got %q want %q", body, want)
	}
	if got.Filename != "note.txt" || got.SizeBytes != int64(len(want)) {
		t.Fatalf("metadata mismatch: %+v", got)
	}
}

func TestUploadUnknownSizeResolvedFromStore(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	want := []byte("size unknown at upload")

	f, err := svc.Upload(ctx, files.UploadInput{Size: -1, Body: bytes.NewReader(want)})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if f.SizeBytes != int64(len(want)) {
		t.Fatalf("size: got %d want %d", f.SizeBytes, len(want))
	}
}

func TestUploadOrphanCleanupOnMetadataFailure(t *testing.T) {
	ctx := context.Background()
	backend := memory.New()
	repo := metamem.New()
	repo.FailCreate = true
	svc := files.New(backend, repo)

	_, err := svc.Upload(ctx, files.UploadInput{Size: 3, Body: bytes.NewReader([]byte("abc"))})
	if err == nil {
		t.Fatal("expected error when metadata commit fails")
	}
	// The object written before the failed commit must have been cleaned up.
	if _, err := backend.Stat(ctx, "abc-key"); err == nil {
		t.Fatal("unexpected: stat should not find a cleaned-up object")
	}
}

func TestNotFoundTranslation(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	const absent = "11111111-1111-1111-1111-111111111111" // valid uuid, no record

	if _, err := svc.Metadata(ctx, absent); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("Metadata: expected files.ErrNotFound, got %v", err)
	}
	if _, _, err := svc.Open(ctx, absent); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("Open: expected files.ErrNotFound, got %v", err)
	}
	if err := svc.Delete(ctx, absent); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("Delete: expected files.ErrNotFound, got %v", err)
	}
}

func TestListRejectsMalformedCursor(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())

	if _, err := svc.List(ctx, files.ListFilter{Cursor: "not-a-uuid"}); !errors.Is(err, files.ErrInvalidID) {
		t.Fatalf("List: expected files.ErrInvalidID for bad cursor, got %v", err)
	}
	if _, err := svc.List(ctx, files.ListFilter{}); err != nil {
		t.Fatalf("List without cursor: %v", err)
	}
}

func TestInvalidID(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())

	if _, err := svc.Metadata(ctx, "not-a-uuid"); !errors.Is(err, files.ErrInvalidID) {
		t.Fatalf("Metadata: expected files.ErrInvalidID, got %v", err)
	}
	if _, _, err := svc.Open(ctx, "not-a-uuid"); !errors.Is(err, files.ErrInvalidID) {
		t.Fatalf("Open: expected files.ErrInvalidID, got %v", err)
	}
	if err := svc.Delete(ctx, "not-a-uuid"); !errors.Is(err, files.ErrInvalidID) {
		t.Fatalf("Delete: expected files.ErrInvalidID, got %v", err)
	}
}
