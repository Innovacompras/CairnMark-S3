package files_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/mettjs/cairnmark/internal/files"
	metamem "github.com/mettjs/cairnmark/internal/metadata/memory"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

func TestUploadIdempotentReplaysSameFile(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	body := func() *bytes.Reader { return bytes.NewReader([]byte("payload")) }

	first, replayed, err := svc.UploadIdempotent(ctx, "key-1",
		files.UploadInput{Size: 7, Body: body()})
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if replayed {
		t.Fatal("first upload should not be a replay")
	}

	second, replayed, err := svc.UploadIdempotent(ctx, "key-1",
		files.UploadInput{Size: 7, Body: body()})
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if !replayed {
		t.Fatal("second upload with same key should be a replay")
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned a different file: %s vs %s", second.ID, first.ID)
	}
}

func TestUploadIdempotentInProgressConflicts(t *testing.T) {
	ctx := context.Background()
	repo := metamem.New()
	svc := files.New(memory.New(), repo)

	// Simulate another request that has claimed the key but not completed.
	if _, _, err := repo.ClaimIdempotencyKey(ctx, "busy"); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	_, _, err := svc.UploadIdempotent(ctx, "busy",
		files.UploadInput{Size: 3, Body: bytes.NewReader([]byte("abc"))})
	if !errors.Is(err, files.ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict for in-progress key, got %v", err)
	}
}

func TestUploadIdempotentDeletedResultIsGone(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())

	first, _, err := svc.UploadIdempotent(ctx, "key-gone",
		files.UploadInput{Size: 7, Body: bytes.NewReader([]byte("payload"))})
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if err := svc.Delete(ctx, first.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, _, err = svc.UploadIdempotent(ctx, "key-gone",
		files.UploadInput{Size: 7, Body: bytes.NewReader([]byte("payload"))})
	if !errors.Is(err, files.ErrIdempotencyResultGone) {
		t.Fatalf("expected ErrIdempotencyResultGone after result deleted, got %v", err)
	}
}

func TestUploadIdempotentReleasesOnFailure(t *testing.T) {
	ctx := context.Background()
	repo := metamem.New()
	repo.FailCreate = true // force the upload's metadata commit to fail
	svc := files.New(memory.New(), repo)

	if _, _, err := svc.UploadIdempotent(ctx, "retry-me",
		files.UploadInput{Size: 3, Body: bytes.NewReader([]byte("abc"))}); err == nil {
		t.Fatal("expected upload failure")
	}
	// The claim must be released so a retry can re-claim (not stuck pending).
	claimed, _, err := repo.ClaimIdempotencyKey(ctx, "retry-me")
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if !claimed {
		t.Fatal("key should be re-claimable after a failed upload was released")
	}
}
