package files_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/mettjs/cairnmark/internal/files"
	metamem "github.com/mettjs/cairnmark/internal/metadata/memory"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

func uploadWithTags(t *testing.T, svc *files.Service, tags map[string]any) *files.File {
	t.Helper()
	f, err := svc.Upload(context.Background(), files.UploadInput{
		Size: 3, Body: bytes.NewReader([]byte("abc")), Tags: tags,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	return f
}

func TestUpdateMetadataMerge(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	f := uploadWithTags(t, svc, map[string]any{"env": "prod"})

	got, err := svc.UpdateMetadata(ctx, f.ID, map[string]any{"team": "search"}, true)
	if err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}
	if got.Metadata["env"] != "prod" || got.Metadata["team"] != "search" {
		t.Fatalf("merge: expected both keys, got %v", got.Metadata)
	}
}

func TestUpdateMetadataReplace(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	f := uploadWithTags(t, svc, map[string]any{"env": "prod", "team": "search"})

	got, err := svc.UpdateMetadata(ctx, f.ID, map[string]any{"env": "staging"}, false)
	if err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}
	if got.Metadata["env"] != "staging" {
		t.Fatalf("replace: env not updated, got %v", got.Metadata)
	}
	if _, ok := got.Metadata["team"]; ok {
		t.Fatalf("replace: old key should be gone, got %v", got.Metadata)
	}
}

func TestUpdateMetadataInvalidAndMissing(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())

	if _, err := svc.UpdateMetadata(ctx, "bad-id", nil, true); err != files.ErrInvalidID {
		t.Fatalf("expected ErrInvalidID, got %v", err)
	}
	if _, err := svc.UpdateMetadata(ctx, "11111111-1111-1111-1111-111111111111", nil, true); err != files.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
