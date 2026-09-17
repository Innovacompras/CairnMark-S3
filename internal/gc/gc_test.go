package gc_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mettjs/cairnmark/internal/gc"
	"github.com/mettjs/cairnmark/internal/metadata"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

// gcFakeRepo is a minimal gc.Repo for collector tests.
type gcFakeRepo struct {
	deleted    []*metadata.File
	keys       map[string]struct{}
	purged     []string
	purgedKeys int
	purgedJobs int
}

func (r *gcFakeRepo) ListDeleted(context.Context, int) ([]*metadata.File, error) {
	return r.deleted, nil
}
func (r *gcFakeRepo) Purge(_ context.Context, id string) error {
	r.purged = append(r.purged, id)
	return nil
}
func (r *gcFakeRepo) StorageKeys(context.Context) (map[string]struct{}, error) {
	return r.keys, nil
}
func (r *gcFakeRepo) PurgeIdempotencyKeys(_ context.Context, _ time.Time) (int, error) {
	r.purgedKeys++
	return r.purgedKeys, nil
}
func (r *gcFakeRepo) PurgeFinishedJobs(_ context.Context, _ time.Time) (int, error) {
	r.purgedJobs++
	return r.purgedJobs, nil
}

// unlimited is the collector configuration the sweep-logic tests share:
// key expiry and job retention are off, so those counters stay at zero.
var unlimited = gc.Options{Interval: time.Minute, GracePeriod: time.Hour}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func exists(t *testing.T, b *memory.Backend, key string) bool {
	t.Helper()
	_, err := b.Stat(context.Background(), key)
	return err == nil
}

func TestReclaimOrphanPastGraceButNotFresh(t *testing.T) {
	ctx := context.Background()
	b := memory.New()
	b.PutAged("old-orphan", []byte("x"), time.Now().Add(-2*time.Hour))
	b.PutAged("fresh-orphan", []byte("y"), time.Now())

	repo := &gcFakeRepo{keys: map[string]struct{}{}} // neither key referenced
	c := gc.New(b, repo, discardLogger(), unlimited)

	stats, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Orphans != 1 {
		t.Fatalf("orphans reclaimed: got %d want 1", stats.Orphans)
	}
	if exists(t, b, "old-orphan") {
		t.Fatal("old orphan should have been reclaimed")
	}
	if !exists(t, b, "fresh-orphan") {
		t.Fatal("fresh orphan within grace must be preserved (could be in-flight upload)")
	}
}

func TestReferencedObjectNotReclaimed(t *testing.T) {
	ctx := context.Background()
	b := memory.New()
	b.PutAged("live-key", []byte("data"), time.Now().Add(-2*time.Hour))

	repo := &gcFakeRepo{keys: map[string]struct{}{"live-key": {}}}
	c := gc.New(b, repo, discardLogger(), unlimited)

	stats, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Orphans != 0 || !exists(t, b, "live-key") {
		t.Fatal("a referenced object must never be reclaimed")
	}
}

func TestPurgeSoftDeleted(t *testing.T) {
	ctx := context.Background()
	b := memory.New()
	b.PutAged("obj-1", []byte("bye"), time.Now())

	repo := &gcFakeRepo{
		deleted: []*metadata.File{{ID: "id-1", StorageKey: "obj-1"}},
		keys:    map[string]struct{}{"obj-1": {}}, // still referenced until purged
	}
	c := gc.New(b, repo, discardLogger(), unlimited)

	stats, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Purged != 1 {
		t.Fatalf("purged: got %d want 1", stats.Purged)
	}
	if exists(t, b, "obj-1") {
		t.Fatal("soft-deleted object should have been purged from storage")
	}
	if len(repo.purged) != 1 || repo.purged[0] != "id-1" {
		t.Fatalf("expected row id-1 purged, got %v", repo.purged)
	}
}

func TestExpireIdempotencyKeys(t *testing.T) {
	ctx := context.Background()
	repo := &gcFakeRepo{keys: map[string]struct{}{}}

	// Disabled when TTL <= 0.
	c := gc.New(memory.New(), repo, discardLogger(), unlimited)
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if repo.purgedKeys != 0 {
		t.Fatalf("TTL<=0 should not purge keys, got %d calls", repo.purgedKeys)
	}

	// Enabled when TTL > 0.
	c = gc.New(memory.New(), repo, discardLogger(), gc.Options{Interval: time.Minute, GracePeriod: time.Hour, IdempotencyTTL: 24 * time.Hour})
	stats, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.ExpiredKeys != 1 {
		t.Fatalf("expected ExpiredKeys reported, got %d", stats.ExpiredKeys)
	}
}

func TestExpireFinishedJobs(t *testing.T) {
	ctx := context.Background()
	repo := &gcFakeRepo{keys: map[string]struct{}{}}

	// Retention <= 0 disables the purge, like key expiry.
	c := gc.New(memory.New(), repo, discardLogger(), unlimited)
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if repo.purgedJobs != 0 {
		t.Fatalf("retention<=0 should not purge jobs, got %d calls", repo.purgedJobs)
	}

	c = gc.New(memory.New(), repo, discardLogger(), gc.Options{Interval: time.Minute, GracePeriod: time.Hour, JobRetention: 24 * time.Hour})
	stats, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.ExpiredJobs != 1 {
		t.Fatalf("expected ExpiredJobs reported, got %d", stats.ExpiredJobs)
	}
}
