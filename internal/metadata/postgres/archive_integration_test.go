//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// createTagged inserts a row with the given tags and removes it after the test
// (a soft delete so Purge, which only hard-deletes tombstones, can take it).
func createTagged(t *testing.T, r *Repo, tags map[string]any) *metadata.File {
	t.Helper()
	ctx := context.Background()
	f := newFile()
	f.Metadata = tags
	if err := r.Create(ctx, f); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Delete(ctx, f.ID)
		_ = r.Purge(ctx, f.ID)
	})
	return f
}

func TestRepoReplacePreservesReservedTags(t *testing.T) {
	ctx := context.Background()
	r := New(testPool)
	f := createTagged(t, r, map[string]any{
		"env": "it", metadata.TagArchiveID: "arch", metadata.TagArchiveIndex: 3,
	})

	// A replace rewrites the whole column — except the service-owned keys,
	// which the SQL carries across.
	replaced, err := r.UpdateMetadata(ctx, f.ID, map[string]any{"only": "this"}, false)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	m := replaced.Metadata
	if _, ok := m["env"]; ok || m["only"] != "this" {
		t.Fatalf("replace did not overwrite the client keys: %v", m)
	}
	if m[metadata.TagArchiveID] != "arch" || m[metadata.TagArchiveIndex] != float64(3) {
		t.Fatalf("replace stripped the reserved keys: %v", m)
	}

	// A merge naturally keeps them too.
	merged, err := r.UpdateMetadata(ctx, f.ID, map[string]any{"env": "x"}, true)
	if err != nil || merged.Metadata[metadata.TagArchiveID] != "arch" || merged.Metadata["only"] != "this" {
		t.Fatalf("merge: %v err=%v", merged.Metadata, err)
	}
}

func TestRepoListIncludeDeleted(t *testing.T) {
	ctx := context.Background()
	r := New(testPool)
	tags := map[string]any{"deltest": uuid.NewString()}
	f := createTagged(t, r, tags)
	if err := r.Delete(ctx, f.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	live, err := r.List(ctx, metadata.ListFilter{Tags: tags})
	if err != nil || len(live) != 0 {
		t.Fatalf("live list should hide the tombstone: %d err=%v", len(live), err)
	}
	all, err := r.List(ctx, metadata.ListFilter{Tags: tags, IncludeDeleted: true})
	if err != nil || len(all) != 1 || all[0].DeletedAt == nil {
		t.Fatalf("IncludeDeleted should return the tombstone with deleted_at set: %+v err=%v", all, err)
	}
}

func TestRepoListEntryScope(t *testing.T) {
	ctx := context.Background()
	r := New(testPool)
	run := uuid.NewString()
	plain := createTagged(t, r, map[string]any{"scopetest": run})
	entry := createTagged(t, r, map[string]any{"scopetest": run, metadata.TagArchiveID: "arch"})
	archive := createTagged(t, r, map[string]any{"scopetest": run, metadata.TagArchive: metadata.TagArchiveMarker})

	ids := func(filter metadata.ListFilter) map[string]bool {
		t.Helper()
		if filter.Tags == nil {
			filter.Tags = map[string]any{}
		}
		filter.Tags["scopetest"] = run
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

	if got := ids(metadata.ListFilter{}); len(got) != 3 {
		t.Fatalf("default scope: %v", got)
	}
	if got := ids(metadata.ListFilter{Entries: metadata.EntriesOnly}); len(got) != 1 || !got[entry.ID] {
		t.Fatalf("only: %v", got)
	}
	if got := ids(metadata.ListFilter{Entries: metadata.EntriesExclude}); len(got) != 2 || !got[plain.ID] || !got[archive.ID] {
		t.Fatalf("exclude: %v", got)
	}
	if got := ids(metadata.ListFilter{Tags: map[string]any{metadata.TagArchive: metadata.TagArchiveMarker}}); len(got) != 1 || !got[archive.ID] {
		t.Fatalf("archives by marker: %v", got)
	}
}
