package memory

import (
	"context"
	"errors"
	"time"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// claim is one idempotency_keys row.
type claim struct {
	rec     metadata.IdempotencyRecord
	created time.Time
}

func (r *Repo) ClaimIdempotencyKey(ctx context.Context, key string) (bool, *metadata.IdempotencyRecord, error) {
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.keys[key]; ok {
		rec := c.rec
		return false, &rec, nil
	}
	r.keys[key] = &claim{
		rec:     metadata.IdempotencyRecord{Status: metadata.IdempotencyPending},
		created: time.Now(),
	}
	return true, nil, nil
}

func (r *Repo) CreateForKey(ctx context.Context, f *metadata.File, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.keys[key]
	if !ok {
		return errors.New("memory: complete idempotency key: claim no longer exists")
	}
	if err := r.insert(f); err != nil {
		return err
	}
	id := f.ID
	c.rec = metadata.IdempotencyRecord{Status: metadata.IdempotencyCompleted, FileID: &id}
	return nil
}

func (r *Repo) ReleaseIdempotencyKey(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.keys[key]; ok && c.rec.Status == metadata.IdempotencyPending {
		delete(r.keys, key)
	}
	return nil
}

func (r *Repo) PurgeIdempotencyKeys(_ context.Context, before time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for key, c := range r.keys {
		if c.created.Before(before) {
			delete(r.keys, key)
			n++
		}
	}
	return n, nil
}
