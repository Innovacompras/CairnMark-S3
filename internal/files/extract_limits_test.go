package files_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mettjs/cairnmark/internal/files"
	"github.com/mettjs/cairnmark/internal/metadata"
	metamem "github.com/mettjs/cairnmark/internal/metadata/memory"
	"github.com/mettjs/cairnmark/internal/storage"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

func newLimited(t *testing.T, l files.ArchiveLimits) (*files.Service, *files.File) {
	t.Helper()
	svc := files.New(memory.New(), metamem.New(), files.WithArchiveLimits(l))
	return svc, uploadArchive(t, svc, buildZip(t, docs...))
}

func TestExtractSelection(t *testing.T) {
	ctx := context.Background()
	svc, arch := newLimited(t, files.ArchiveLimits{})

	sum, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{Entries: []int{1}})
	if err != nil {
		t.Fatalf("Extract [1]: %v", err)
	}
	if sum.Extracted != 1 || sum.SkippedByReason[files.SkipNotSelected] != 2 ||
		sum.SkippedByReason["platform_metadata"] != 1 {
		t.Fatalf("select one: %+v", sum)
	}
	if kids := children(t, svc, arch.ID, false); len(kids) != 1 || kids[1] == nil {
		t.Fatalf("children: %v", kids)
	}

	if _, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{Entries: []int{4}}); !errors.Is(err, files.ErrInvalidSelection) {
		t.Fatalf("out of range: got %v want ErrInvalidSelection", err)
	}
	if _, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{Entries: []int{-1}}); !errors.Is(err, files.ErrInvalidSelection) {
		t.Fatalf("negative: got %v want ErrInvalidSelection", err)
	}

	// An explicit empty selection selects nothing; a rule-skipped entry keeps
	// its rule's reason even when named.
	sum, err = svc.Extract(ctx, arch.ID, files.ExtractOptions{Entries: []int{2}})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Extracted != 0 || sum.SkippedByReason["platform_metadata"] != 1 ||
		sum.SkippedByReason[files.SkipNotSelected] != 3 {
		t.Fatalf("select a skipped entry: %+v", sum)
	}
}

func TestExtractEntryCountCap(t *testing.T) {
	ctx := context.Background()
	svc, arch := newLimited(t, files.ArchiveLimits{MaxEntries: 3})

	if _, _, err := svc.ArchiveEntries(ctx, arch.ID); !errors.Is(err, files.ErrArchiveTooLarge) {
		t.Fatalf("ArchiveEntries: got %v want ErrArchiveTooLarge", err)
	}
	if _, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{}); !errors.Is(err, files.ErrArchiveTooLarge) {
		t.Fatalf("Extract: got %v want ErrArchiveTooLarge", err)
	}
}

func TestExtractTotalBytesCap(t *testing.T) {
	ctx := context.Background()
	svc, arch := newLimited(t, files.ArchiveLimits{MaxTotalBytes: 30})

	_, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if !errors.Is(err, files.ErrArchiveTooLarge) {
		t.Fatalf("whole archive: got %v want ErrArchiveTooLarge", err)
	}
	if got := len(children(t, svc, arch.ID, false)); got != 0 {
		t.Fatalf("a refused run wrote %d children", got)
	}
	// A selection under the cap goes through: the cap applies to what would be
	// written, so list-then-extract in batches works.
	sum, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{Entries: []int{0}})
	if err != nil || sum.Extracted != 1 {
		t.Fatalf("selection under the cap: %+v err=%v", sum, err)
	}
}

func TestExtractPerEntryRules(t *testing.T) {
	ctx := context.Background()

	// Per-entry size reuses the upload cap: the two long documents are skipped.
	svc, arch := newLimited(t, files.ArchiveLimits{MaxEntryBytes: 12})
	sum, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Extracted != 1 || sum.SkippedByReason["too_large"] != 2 {
		t.Fatalf("size rule: %+v", sum)
	}

	// The extension allowlist covers "only documents" literally.
	svc, arch = newLimited(t, files.ArchiveLimits{Extensions: []string{".pdf"}})
	sum, err = svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Extracted != 1 || sum.SkippedByReason["extension_not_allowed"] != 2 ||
		sum.SkippedByReason["platform_metadata"] != 1 {
		t.Fatalf("extension rule: %+v", sum)
	}
	if kids := children(t, svc, arch.ID, false); len(kids) != 1 || kids[0].Filename != "q3.pdf" {
		t.Fatalf("children: %v", kids)
	}
}

func TestExtractSkipsCorruptEntry(t *testing.T) {
	ctx := context.Background()
	backend := memory.New()
	svc := files.New(backend, metamem.New())
	arch := uploadArchive(t, svc, buildZipWithCorruptEntry(t))

	sum, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil {
		t.Fatalf("a corrupt entry must not fail the archive: %v", err)
	}
	if sum.Extracted != 1 || sum.SkippedByReason[files.SkipCorrupt] != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	kids := children(t, svc, arch.ID, false)
	if len(kids) != 1 || kids[1] == nil {
		t.Fatalf("children: %v", kids)
	}
	if got := readFile(t, svc, kids[1].ID); got != "sound entry" {
		t.Fatalf("sound entry: %q", got)
	}
	// Nothing was stored for the corrupt entry — the write failed before the row.
	objects := 0
	_ = backend.List(ctx, func(storage.StoredObject) error { objects++; return nil })
	if objects != 2 {
		t.Fatalf("stored objects: got %d want 2 (archive + one child)", objects)
	}

	// It is reported the same way on every run: deterministic, not a one-off.
	sum, err = svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil || sum.SkippedByReason[files.SkipCorrupt] != 1 || sum.SkippedByReason[files.SkipAlreadyExtracted] != 1 {
		t.Fatalf("re-run: %+v err=%v", sum, err)
	}
}

func TestExtractTotalBytesCapSurvivesDeclaredSizeOverflow(t *testing.T) {
	// Entry sizes come from the directory and nothing validates them against
	// the object, so an archive can declare two entries of 2^62 bytes. Summed
	// into an int64 those wrap negative — under every cap — and the run would
	// proceed instead of answering 413.
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New(),
		files.WithArchiveLimits(files.ArchiveLimits{MaxTotalBytes: 1 << 30}))
	arch := uploadArchive(t, svc, buildZipDeclaringHugeEntries(t))

	if _, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{}); !errors.Is(err, files.ErrArchiveTooLarge) {
		t.Fatalf("got %v want ErrArchiveTooLarge", err)
	}
	if got := len(children(t, svc, arch.ID, false)); got != 0 {
		t.Fatalf("a refused run wrote %d children", got)
	}
}

func TestExtractNothingLeavesTheArchiveUnmarked(t *testing.T) {
	// The marker is what GET /files?tag.cm:archive=true lists, so an archive
	// nothing was extracted from must not carry it: there are no children to
	// find behind it.
	ctx := context.Background()
	svc, arch := newLimited(t, files.ArchiveLimits{})

	sum, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{Entries: []int{}})
	if err != nil || sum.Extracted != 0 {
		t.Fatalf("empty selection: %+v err=%v", sum, err)
	}
	f, err := svc.Metadata(ctx, arch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := f.Metadata[metadata.TagArchive]; ok {
		t.Fatalf("marked after extracting nothing: %v", v)
	}
	before := f.UpdatedAt

	// A run that writes a child does mark it.
	if _, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{Entries: []int{0}}); err != nil {
		t.Fatal(err)
	}
	if f, err = svc.Metadata(ctx, arch.ID); err != nil {
		t.Fatal(err)
	}
	if f.Metadata[metadata.TagArchive] != metadata.TagArchiveMarker {
		t.Fatalf("not marked after extracting: %v", f.Metadata)
	}
	if before != nil && f.UpdatedAt != nil && !f.UpdatedAt.After(*before) {
		t.Fatal("marking an archive did not bump updated_at")
	}
}

func TestArchiveListingHasAHardCeiling(t *testing.T) {
	// MaxEntries=0 is documented as uncapped, which the extraction summary can
	// afford and the unpaginated listing cannot.
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New(),
		files.WithArchiveLimits(files.ArchiveLimits{MaxEntries: 0}))
	arch := uploadArchive(t, svc, buildZipOfManyEntries(t, 10_001))

	if _, _, err := svc.ArchiveEntries(ctx, arch.ID); !errors.Is(err, files.ErrArchiveTooLarge) {
		t.Fatalf("ArchiveEntries: got %v want ErrArchiveTooLarge", err)
	}
}

func TestArchiveDirectoryIsBoundedBeforeItIsParsed(t *testing.T) {
	// MaxEntries cannot be checked until the directory is parsed, and parsing
	// it allocates one record per entry — so a crafted directory is refused on
	// its byte span first. The span bound is derived from the entry ceiling, so
	// a cap of 1 admits only a very small directory.
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New(),
		files.WithArchiveLimits(files.ArchiveLimits{MaxEntries: 1}))
	arch := uploadArchive(t, svc, buildZipOfManyEntries(t, 40))

	_, _, err := svc.ArchiveEntries(ctx, arch.ID)
	if !errors.Is(err, files.ErrArchiveTooLarge) {
		t.Fatalf("got %v want ErrArchiveTooLarge", err)
	}
	// Prove it was the pre-parse guard and not the entry count afterwards:
	// only the span check names the central directory.
	if !strings.Contains(err.Error(), "central directory") {
		t.Fatalf("expected the directory-span guard to fire, got %v", err)
	}
	if _, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{}); !errors.Is(err, files.ErrArchiveTooLarge) {
		t.Fatalf("Extract: got %v want ErrArchiveTooLarge", err)
	}
}
