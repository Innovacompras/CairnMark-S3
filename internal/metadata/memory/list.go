package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// The pgx implementation's page bounds, mirrored so a caller relying on the
// maximum (extraction pages its skip-set in 500s) behaves the same here.
const (
	listDefaultLimit = 50
	listMaxLimit     = 500
)

// List mirrors the pgx query: live rows unless IncludeDeleted, content-type
// equality, JSONB-style tag containment, the entries scope, and keyset paging
// by id descending — UUIDv7 ids sort by creation time, as in Postgres.
func (r *Repo) List(ctx context.Context, filter metadata.ListFilter) ([]*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	ids := make([]string, 0, len(r.files))
	for id, f := range r.files {
		if matches(f, filter) {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	limit := filter.Limit
	if limit <= 0 {
		limit = listDefaultLimit
	}
	if limit > listMaxLimit {
		limit = listMaxLimit
	}
	var out []*metadata.File
	for _, id := range ids {
		if filter.Cursor != "" && id >= filter.Cursor {
			continue
		}
		out = append(out, clone(r.files[id]))
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func matches(f *metadata.File, filter metadata.ListFilter) bool {
	if f.DeletedAt != nil && !filter.IncludeDeleted {
		return false
	}
	if filter.ContentType != "" && f.ContentType != filter.ContentType {
		return false
	}
	_, isEntry := f.Metadata[metadata.TagArchiveID]
	switch filter.Entries {
	case metadata.EntriesOnly:
		if !isEntry {
			return false
		}
	case metadata.EntriesExclude:
		if isEntry {
			return false
		}
	}
	for k, want := range filter.Tags {
		got, ok := f.Metadata[k]
		if !ok || !jsonEqual(got, want) {
			return false
		}
	}
	return true
}

// jsonEqual compares two tag values the way JSONB containment does — by JSON
// value — so an int and the float64 it decodes to are the same number.
func jsonEqual(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
}
