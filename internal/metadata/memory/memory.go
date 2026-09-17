// Package memory is an in-memory metadata.Repository used as a test double. It
// mirrors the pgx implementation's observable semantics — live-only reads,
// soft delete, tag containment, keyset paging by id, reserved-key preservation
// on replace — so the layers above can be exercised without Postgres. It is
// not a deployment target.
package memory

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// Repo is a concurrency-safe in-memory repository. Every method honours
// context cancellation the way pgx does, so a caller that must run after its
// request context is gone can be tested for it.
type Repo struct {
	mu    sync.Mutex
	files map[string]*metadata.File
	keys  map[string]*claim
	jobs  map[string]*metadata.Job

	// FailCreate makes every Create and CreateForKey fail, to exercise the
	// orphan-cleanup paths above this seam.
	FailCreate bool
}

var (
	_ metadata.Repository    = (*Repo)(nil)
	_ metadata.JobRepository = (*Repo)(nil)
)

// New returns an empty repository.
func New() *Repo {
	return &Repo{files: map[string]*metadata.File{}, keys: map[string]*claim{}, jobs: map[string]*metadata.Job{}}
}

func (r *Repo) Create(ctx context.Context, f *metadata.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.insert(f)
}

// insert mirrors insertFile: it fills the DB-owned defaults on the caller's
// record and stores a private copy.
func (r *Repo) insert(f *metadata.File) error {
	if r.FailCreate {
		return errors.New("memory: forced create failure")
	}
	if _, dup := r.files[f.ID]; dup {
		return errors.New("memory: duplicate id")
	}
	if f.Metadata == nil {
		f.Metadata = map[string]any{}
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now()
	}
	r.files[f.ID] = clone(f)
	return nil
}

func (r *Repo) Get(ctx context.Context, id string) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[id]
	if !ok || f.DeletedAt != nil {
		return nil, metadata.ErrNotFound
	}
	return clone(f), nil
}

func (r *Repo) StorageKeys(context.Context) (map[string]struct{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make(map[string]struct{}, len(r.files))
	for _, f := range r.files {
		keys[f.StorageKey] = struct{}{}
	}
	return keys, nil
}

// clone returns a copy whose metadata map is not shared with the store.
func clone(f *metadata.File) *metadata.File {
	cp := *f
	cp.Metadata = maps.Clone(f.Metadata)
	return &cp
}
