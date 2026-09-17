package files_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/mettjs/cairnmark/internal/files"
	metamem "github.com/mettjs/cairnmark/internal/metadata/memory"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

func TestUploadComputesChecksum(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	body := []byte("integrity matters")
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	f, err := svc.Upload(ctx, files.UploadInput{
		ContentType: "text/plain",
		Size:        int64(len(body)),
		Body:        bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if f.ChecksumSHA256 != want {
		t.Fatalf("checksum: got %q want %q", f.ChecksumSHA256, want)
	}
}

func TestUploadSniffsContentType(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	// Minimal PNG signature → http.DetectContentType reports image/png.
	body := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

	f, err := svc.Upload(ctx, files.UploadInput{Size: int64(len(body)), Body: bytes.NewReader(body)})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if f.ContentType != "image/png" {
		t.Fatalf("sniffed content type: got %q want image/png", f.ContentType)
	}
}

func TestChecksumMismatchDetectedOnRead(t *testing.T) {
	ctx := context.Background()
	backend := memory.New()
	svc := files.New(backend, metamem.New())

	f, err := svc.Upload(ctx, files.UploadInput{
		Size: 5, Body: bytes.NewReader([]byte("hello")),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Corrupt the stored object behind the service's back.
	if err := backend.Put(ctx, f.StorageKey, bytes.NewReader([]byte("HELLO")), 5, "text/plain"); err != nil {
		t.Fatalf("corrupt object: %v", err)
	}

	_, rc, err := svc.Open(ctx, f.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); !errors.Is(err, files.ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch reading corrupt object, got %v", err)
	}
}

func TestOpenRangeReturnsPartialBytes(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	body := []byte("0123456789")

	f, err := svc.Upload(ctx, files.UploadInput{Size: int64(len(body)), Body: bytes.NewReader(body)})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	rc, err := svc.OpenRange(ctx, f, 2, 4)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "2345" {
		t.Fatalf("range bytes: got %q want %q", got, "2345")
	}
}
