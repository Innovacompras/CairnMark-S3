package files_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"testing"

	"github.com/mettjs/cairnmark/internal/files"
	"github.com/mettjs/cairnmark/internal/metadata"
	"github.com/mettjs/cairnmark/internal/storage"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

// member is one entry of a fixture archive.
type member struct {
	name, body string
}

// docs is the standard fixture: three documents and one piece of Finder
// litter, so indexes 0, 1 and 3 are selectable and 2 is platform_metadata.
var docs = []member{
	{"reports/q3.pdf", "%PDF-1.7 quarterly report"},
	{"reports/data.json", `{"revenue": 42}`},
	{"__MACOSX/reports/._q3.pdf", "resource fork"},
	{"notes/readme.txt", "plain notes"},
}

func buildZip(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		w, err := zw.Create(m.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, m.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildZipWithCorruptEntry returns an archive whose first entry carries a wrong
// CRC32 and whose second is sound.
func buildZipWithCorruptEntry(t *testing.T) []byte {
	t.Helper()
	const body = "looks fine, hashes wrong"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: "bad.txt", Method: zip.Store, CRC32: 0xDEADBEEF,
		CompressedSize64: uint64(len(body)), UncompressedSize64: uint64(len(body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	good := "sound entry"
	w, err = zw.CreateRaw(&zip.FileHeader{
		Name: "good.txt", Method: zip.Store, CRC32: crc32.ChecksumIEEE([]byte(good)),
		CompressedSize64: uint64(len(good)), UncompressedSize64: uint64(len(good)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, good); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadArchive(t *testing.T, svc *files.Service, data []byte) *files.File {
	t.Helper()
	f, err := svc.Upload(context.Background(), files.UploadInput{
		Filename: "docs.zip", ContentType: "application/zip",
		Size: int64(len(data)), Body: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("upload archive: %v", err)
	}
	return f
}

// children returns an archive's extracted entries keyed by directory index,
// failing if two rows claim the same index — the duplicate a broken resume
// would produce. Paged to exhaustion: the default page is 50, and a helper that
// silently returned the first page would make every count below meaningless
// above that.
func children(t *testing.T, svc *files.Service, archiveID string, includeDeleted bool) map[int]*files.File {
	t.Helper()
	filter := files.ListFilter{
		Tags:           map[string]any{metadata.TagArchiveID: archiveID},
		IncludeDeleted: includeDeleted,
		Limit:          500,
	}
	out := map[int]*files.File{}
	for {
		page, err := svc.List(context.Background(), filter)
		if err != nil {
			t.Fatalf("list children: %v", err)
		}
		if len(page) == 0 {
			return out
		}
		for _, f := range page {
			idx, ok := indexTag(f.Metadata[metadata.TagArchiveIndex])
			if !ok {
				t.Fatalf("child %s has no index tag: %v", f.ID, f.Metadata)
			}
			if _, dup := out[idx]; dup {
				t.Fatalf("two children for index %d", idx)
			}
			out[idx] = f
		}
		filter.Cursor = page[len(page)-1].ID
	}
}

// indexTag reads the archive-index tag through every representation a
// repository may hand back, as the service's own reader does. Asserting the Go
// int the in-memory double happens to store would leave every resume assertion
// below blind to what Postgres returns: JSONB numbers decode as float64, so
// that is the only shape production ever sees.
func indexTag(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), n == float64(int(n))
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func readFile(t *testing.T, svc *files.Service, id string) string {
	t.Helper()
	_, rc, err := svc.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open %s: %v", id, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return string(b)
}

// faultBackend fails the failAt-th Put (1-based; the archive's own upload is
// the first) to simulate a store outage mid-extraction.
type faultBackend struct {
	storage.Backend
	puts   int
	failAt int
	onFail func()
}

func (b *faultBackend) Put(ctx context.Context, key string, r io.Reader, size int64, ct string) error {
	b.puts++
	if b.puts == b.failAt {
		if b.onFail != nil {
			b.onFail()
		}
		return errors.New("injected store failure")
	}
	return b.Backend.Put(ctx, key, r, size, ct)
}

// countingBackend counts range requests, to prove opening an archive through
// the store is a bounded number of them.
type countingBackend struct {
	*memory.Backend
	ranges int
}

func (c *countingBackend) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	c.ranges++
	return c.Backend.GetRange(ctx, key, offset, length)
}

// buildZipDeclaringHugeEntries returns an archive whose directory claims two
// entries of 2^62 uncompressed bytes. The sizes are declared, not stored:
// CreateRaw writes the header verbatim, which is exactly what a hostile archive
// does.
func buildZipDeclaringHugeEntries(t *testing.T) []byte {
	t.Helper()
	const huge = uint64(1) << 62
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"a.bin", "b.bin"} {
		// Store, not Deflate: the ratio guard exempts Store, so the total cap
		// is the only thing standing between this archive and the run.
		w, err := zw.CreateRaw(&zip.FileHeader{
			Name: name, Method: zip.Store,
			CompressedSize64: huge, UncompressedSize64: huge,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildZipOfManyEntries returns an archive of n tiny entries.
func buildZipOfManyEntries(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := range n {
		w, err := zw.Create(fmt.Sprintf("docs/%06d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
