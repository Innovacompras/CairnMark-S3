package files_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mettjs/cairnmark/internal/files"
	metamem "github.com/mettjs/cairnmark/internal/metadata/memory"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

func TestUploadGeneratesUUIDv7(t *testing.T) {
	svc := files.New(memory.New(), metamem.New())
	f, err := svc.Upload(context.Background(), files.UploadInput{
		Size: 3, Body: bytes.NewReader([]byte("abc")),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	for name, val := range map[string]string{"id": f.ID, "storage_key": f.StorageKey} {
		u, err := uuid.Parse(val)
		if err != nil {
			t.Fatalf("%s not a uuid: %v", name, err)
		}
		if u.Version() != 7 {
			t.Fatalf("%s should be UUIDv7, got version %d", name, u.Version())
		}
	}
}
