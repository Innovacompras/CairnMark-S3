package memory

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mettjs/cairnmark/internal/metadata"
)

func create(t *testing.T, r *Repo, tags map[string]any) *metadata.File {
	t.Helper()
	f := &metadata.File{ID: uuid.Must(uuid.NewV7()).String(), StorageKey: uuid.NewString(), Metadata: tags}
	if err := r.Create(context.Background(), f); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return f
}

func TestReplacePreservesReservedKeys(t *testing.T) {
	ctx := context.Background()
	r := New()
	f := create(t, r, map[string]any{"env": "it", metadata.TagArchiveID: "arch"})

	got, err := r.UpdateMetadata(ctx, f.ID, map[string]any{"only": "this"}, false)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, ok := got.Metadata["env"]; ok || got.Metadata["only"] != "this" || got.Metadata[metadata.TagArchiveID] != "arch" {
		t.Fatalf("replace: %v", got.Metadata)
	}
}

func TestListScopesAndTombstones(t *testing.T) {
	ctx := context.Background()
	r := New()
	plain := create(t, r, map[string]any{"run": "x"})
	entry := create(t, r, map[string]any{"run": "x", metadata.TagArchiveID: "arch", metadata.TagArchiveIndex: 3})
	gone := create(t, r, map[string]any{"run": "x", metadata.TagArchiveID: "arch", metadata.TagArchiveIndex: 4})
	if err := r.Delete(ctx, gone.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	ids := func(filter metadata.ListFilter) map[string]bool {
		t.Helper()
		filter.Tags = map[string]any{"run": "x"}
		page, err := r.List(ctx, filter)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		out := map[string]bool{}
		for _, f := range page {
			out[f.ID] = true
		}
		return out
	}

	if got := ids(metadata.ListFilter{}); len(got) != 2 || !got[plain.ID] || !got[entry.ID] {
		t.Fatalf("default scope: %v", got)
	}
	if got := ids(metadata.ListFilter{Entries: metadata.EntriesOnly}); len(got) != 1 || !got[entry.ID] {
		t.Fatalf("only: %v", got)
	}
	if got := ids(metadata.ListFilter{Entries: metadata.EntriesExclude}); len(got) != 1 || !got[plain.ID] {
		t.Fatalf("exclude: %v", got)
	}
	if got := ids(metadata.ListFilter{IncludeDeleted: true}); len(got) != 3 || !got[gone.ID] {
		t.Fatalf("include deleted: %v", got)
	}
	// A number tag matches whether the filter carries an int or the float64 a
	// JSON round-trip would produce.
	page, err := r.List(ctx, metadata.ListFilter{Tags: map[string]any{metadata.TagArchiveIndex: float64(3)}})
	if err != nil || len(page) != 1 || page[0].ID != entry.ID {
		t.Fatalf("numeric tag match: %v err=%v", page, err)
	}
}
