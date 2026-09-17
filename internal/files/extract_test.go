package files_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mettjs/cairnmark/internal/files"
	metamem "github.com/mettjs/cairnmark/internal/metadata/memory"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

func TestExtractWritesChildrenWithTags(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	arch := uploadArchive(t, svc, buildZip(t, docs...))

	sum, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if sum.ArchiveID != arch.ID || sum.Entries != 4 || sum.Extracted != 3 || sum.Skipped != 1 ||
		sum.SkippedByReason["platform_metadata"] != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	if len(sum.SampleSkipped) != 1 || sum.SampleSkipped[0].Index != 2 ||
		sum.SampleSkipped[0].Name != "__MACOSX/reports/._q3.pdf" {
		t.Fatalf("sample_skipped: %+v", sum.SampleSkipped)
	}

	kids := children(t, svc, arch.ID, false)
	if len(kids) != 3 {
		t.Fatalf("children: got %d want 3", len(kids))
	}
	q3 := kids[0]
	if q3.Filename != "q3.pdf" || q3.ContentType != "application/pdf" || q3.SizeBytes != int64(len(docs[0].body)) {
		t.Fatalf("q3 record: %+v", q3)
	}
	if q3.Metadata["cm:archive_id"] != arch.ID || q3.Metadata["cm:archive_path"] != "reports/q3.pdf" {
		t.Fatalf("q3 tags: %v", q3.Metadata)
	}
	if kids[1].ContentType != "application/json" {
		t.Fatalf("content type from extension: %q", kids[1].ContentType)
	}
	// Content round-trips byte-identical, through the checksum-verifying reader.
	for idx, m := range map[int]member{0: docs[0], 1: docs[1], 3: docs[3]} {
		if got := readFile(t, svc, kids[idx].ID); got != m.body {
			t.Fatalf("entry %d: got %q want %q", idx, got, m.body)
		}
	}

	// The archive row carries the marker; the list scopes tell the three
	// populations apart.
	a, err := svc.Metadata(ctx, arch.ID)
	if err != nil || a.Metadata["cm:archive"] != "true" {
		t.Fatalf("archive marker: %v err=%v", a.Metadata, err)
	}
	only, _ := svc.List(ctx, files.ListFilter{Entries: files.EntriesOnly})
	exclude, _ := svc.List(ctx, files.ListFilter{Entries: files.EntriesExclude})
	archives, _ := svc.List(ctx, files.ListFilter{Tags: map[string]any{"cm:archive": "true"}})
	if len(only) != 3 || len(exclude) != 1 || exclude[0].ID != arch.ID || len(archives) != 1 {
		t.Fatalf("scopes: only=%d exclude=%d archives=%d", len(only), len(exclude), len(archives))
	}
}

func TestExtractRerunIsANoOp(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	arch := uploadArchive(t, svc, buildZip(t, docs...))
	if _, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{}); err != nil {
		t.Fatal(err)
	}
	first, _ := svc.Metadata(ctx, arch.ID)

	sum, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil {
		t.Fatalf("second Extract: %v", err)
	}
	if sum.Extracted != 0 || sum.SkippedByReason[files.SkipAlreadyExtracted] != 3 ||
		sum.SkippedByReason["platform_metadata"] != 1 {
		t.Fatalf("second run: %+v", sum)
	}
	if got := len(children(t, svc, arch.ID, false)); got != 3 {
		t.Fatalf("children after re-run: %d", got)
	}
	// A no-op re-run does not re-stamp the marker.
	second, _ := svc.Metadata(ctx, arch.ID)
	if first.UpdatedAt == nil || second.UpdatedAt == nil || !second.UpdatedAt.Equal(*first.UpdatedAt) {
		t.Fatalf("updated_at moved on a no-op re-run: %v -> %v", first.UpdatedAt, second.UpdatedAt)
	}
}

func TestExtractResumesAfterInterruption(t *testing.T) {
	ctx := context.Background()
	backend := &faultBackend{Backend: memory.New(), failAt: 3} // archive is Put #1; the 2nd child is #3
	svc := files.New(backend, metamem.New())
	arch := uploadArchive(t, svc, buildZip(t, docs...))

	if _, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{}); err == nil {
		t.Fatal("expected the injected store failure")
	}
	if got := len(children(t, svc, arch.ID, false)); got != 1 {
		t.Fatalf("after the interruption: %d children, want 1", got)
	}

	backend.failAt = 0
	sum, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if sum.Extracted != 2 || sum.SkippedByReason[files.SkipAlreadyExtracted] != 1 {
		t.Fatalf("resume summary: %+v", sum)
	}
	kids := children(t, svc, arch.ID, false)
	if len(kids) != 3 || kids[0] == nil || kids[1] == nil || kids[3] == nil {
		t.Fatalf("after resume: %d children", len(kids))
	}
}

func TestExtractDeletedChildStaysDeleted(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	arch := uploadArchive(t, svc, buildZip(t, docs...))
	if _, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{}); err != nil {
		t.Fatal(err)
	}
	kids := children(t, svc, arch.ID, false)
	if err := svc.Delete(ctx, kids[1].ID); err != nil {
		t.Fatal(err)
	}

	sum, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if sum.Extracted != 0 || sum.SkippedByReason[files.SkipPreviouslyDeleted] != 1 ||
		sum.SkippedByReason[files.SkipAlreadyExtracted] != 2 {
		t.Fatalf("delete must win over re-extract: %+v", sum)
	}
	if live := children(t, svc, arch.ID, false); len(live) != 2 || live[1] != nil {
		t.Fatalf("the deleted child came back: %v", live)
	}
	if all := children(t, svc, arch.ID, true); len(all) != 3 || all[1].DeletedAt == nil {
		t.Fatalf("tombstone missing: %v", all)
	}
}

func TestExtractRejectsNonArchives(t *testing.T) {
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	plain := uploadArchive(t, svc, []byte("%PDF-1.7 just a document"))

	if _, _, err := svc.ArchiveEntries(ctx, plain.ID); !errors.Is(err, files.ErrNotArchive) {
		t.Fatalf("ArchiveEntries: got %v want ErrNotArchive", err)
	}
	if _, err := svc.Extract(ctx, plain.ID, files.ExtractOptions{}); !errors.Is(err, files.ErrNotArchive) {
		t.Fatalf("Extract: got %v want ErrNotArchive", err)
	}
	if _, err := svc.Extract(ctx, "not-a-uuid", files.ExtractOptions{}); !errors.Is(err, files.ErrInvalidID) {
		t.Fatalf("bad id: got %v want ErrInvalidID", err)
	}
}

func TestArchiveEntriesListsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	backend := &countingBackend{Backend: memory.New()}
	svc := files.New(backend, metamem.New())
	arch := uploadArchive(t, svc, buildZip(t, docs...))

	f, entries, err := svc.ArchiveEntries(ctx, arch.ID)
	if err != nil {
		t.Fatalf("ArchiveEntries: %v", err)
	}
	if f.ID != arch.ID || len(entries) != 4 {
		t.Fatalf("got %d entries for %s", len(entries), f.ID)
	}
	if entries[2].Skip != "platform_metadata" || !entries[0].Selectable() || entries[1].ContentType != "application/json" {
		t.Fatalf("classification: %+v", entries)
	}
	if backend.ranges != 1 {
		t.Fatalf("opening the archive cost %d range requests, want 1", backend.ranges)
	}
	if got := len(children(t, svc, arch.ID, true)); got != 0 {
		t.Fatalf("listing wrote %d children", got)
	}
}

func TestExtractResumeSkipSetPagesPastOnePage(t *testing.T) {
	// The skip-set is built by paging the archive's existing children, and one
	// page is 500. An archive with more children than that is the only case
	// that exercises the loop at all; a loop that mistook a short page for the
	// last one would leave the remainder out of the skip-set and re-extract
	// them as duplicates sharing a cm:archive_index.
	ctx := context.Background()
	svc := files.New(memory.New(), metamem.New())
	arch := uploadArchive(t, svc, buildZipOfManyEntries(t, 600))

	first, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil || first.Extracted != 600 {
		t.Fatalf("first run: %+v err=%v", first, err)
	}
	// children fails the test if two rows ever claim one index.
	if got := len(children(t, svc, arch.ID, false)); got != 600 {
		t.Fatalf("children after the first run: %d want 600", got)
	}

	second, err := svc.Extract(ctx, arch.ID, files.ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Extracted != 0 || second.SkippedByReason[files.SkipAlreadyExtracted] != 600 {
		t.Fatalf("re-run must be a whole no-op: %+v", second)
	}
	if got := len(children(t, svc, arch.ID, false)); got != 600 {
		t.Fatalf("children after the re-run: %d want 600", got)
	}
}
