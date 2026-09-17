package memory

import (
	"context"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// Metadata edits and the soft-delete lifecycle, mirroring query.go, postgres.go
// and gc.go of the pgx implementation.

func (r *Repo) UpdateMetadata(ctx context.Context, id string, tags map[string]any, merge bool) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[id]
	if !ok || f.DeletedAt != nil {
		return nil, metadata.ErrNotFound
	}
	next := map[string]any{}
	for k, v := range f.Metadata {
		// Merge keeps every existing key; replace keeps only the reserved ones.
		if merge || strings.HasPrefix(k, metadata.ReservedTagPrefix) {
			next[k] = v
		}
	}
	maps.Copy(next, tags)
	f.Metadata = next
	now := time.Now()
	f.UpdatedAt = &now
	return clone(f), nil
}

func (r *Repo) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[id]
	if !ok || f.DeletedAt != nil {
		return metadata.ErrNotFound
	}
	now := time.Now()
	f.DeletedAt = &now
	return nil
}

func (r *Repo) ListDeleted(_ context.Context, limit int) ([]*metadata.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*metadata.File
	for _, f := range r.files {
		if f.DeletedAt != nil {
			out = append(out, clone(f))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeletedAt.Before(*out[j].DeletedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *Repo) Purge(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.files[id]; ok && f.DeletedAt != nil {
		delete(r.files, id)
		// extraction_jobs.archive_id is `on delete cascade`.
		for jid, j := range r.jobs {
			if j.ArchiveID == id {
				delete(r.jobs, jid)
			}
		}
	}
	return nil
}
