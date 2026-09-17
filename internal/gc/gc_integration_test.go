//go:build integration

// Integration tests for the collector against a real object store. Run with:
//
//	docker compose up -d rustfs
//	CAIRNMARK_S3_ENDPOINT=localhost:9000 CAIRNMARK_S3_ACCESS_KEY=cairnmark \
//	CAIRNMARK_S3_SECRET_KEY=cairnmark-secret CAIRNMARK_S3_BUCKET=cairnmark-it \
//	go test -tags=integration ./internal/gc/
//
// The unit tests cover the collector's logic against an in-memory double, where
// LastModified is whatever the double says it is. These cover the half a double
// cannot: how the collector behaves on timestamps a real store actually
// produces. TestFreshOrphanSurvivesGrace is the one that matters — it is the
// only test anywhere that would catch a backend whose ListObjects reports a zero
// or skewed LastModified, which makes the sweep delete live uploads.
package gc_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mettjs/cairnmark/internal/gc"
	"github.com/mettjs/cairnmark/internal/metadata"
	"github.com/mettjs/cairnmark/internal/storage"
	"github.com/mettjs/cairnmark/internal/storage/s3"
)

// reclaimEverythingGrace is deliberately negative: it puts the cutoff in the
// future so a just-written object is already past it, letting the reclaim and
// purge tests below run deterministically without sleeping.
//
// Note what this setting CANNOT test: with the cutoff in the future, no
// timestamp is ever After it, so the comparison passes for any value including
// the zero time. Tests using it prove the collector deletes what it should, not
// that it reads timestamps correctly. TestFreshOrphanSurvivesGrace covers that.
const reclaimEverythingGrace = -time.Minute

// realisticGrace is a plausible production grace period — long enough that a
// just-written object must fall inside it.
const realisticGrace = time.Hour

// newStore opens a backend on a bucket dedicated to this package.
//
// A sweep is bucket-wide by construction: reclaimOrphans walks every object and
// deletes whatever the repository does not claim. Sharing a bucket with the
// storage tests is therefore not merely untidy — `go test ./...` runs packages
// concurrently, so objects those tests create mid-sweep are unreferenced from
// this package's point of view and get deleted, failing both suites at random.
// The bucket is created on first use by ensureBucket and reused thereafter.
func newStore(t *testing.T) *s3.Backend {
	t.Helper()
	endpoint := os.Getenv("CAIRNMARK_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("CAIRNMARK_S3_ENDPOINT not set; skipping gc integration test")
	}
	b, err := s3.New(context.Background(), s3.Options{
		Endpoint:  endpoint,
		Region:    envOrDefault("CAIRNMARK_S3_REGION", "us-east-1"),
		AccessKey: os.Getenv("CAIRNMARK_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("CAIRNMARK_S3_SECRET_KEY"),
		Bucket:    envOrDefault("CAIRNMARK_S3_BUCKET", "cairnmark-it") + "-gc",
	})
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	return b
}

func envOrDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// protectExisting snapshots every key already in the bucket and reports them as
// referenced, so each test's orphan count reflects only what that test planted.
// The dedicated bucket keeps other packages out; this keeps debris from an
// earlier interrupted run from inflating the count.
func protectExisting(t *testing.T, store *s3.Backend) map[string]struct{} {
	t.Helper()
	keys := map[string]struct{}{}
	if err := store.List(context.Background(), func(o storage.StoredObject) error {
		keys[o.Key] = struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("snapshot bucket: %v", err)
	}
	return keys
}

func objectExists(t *testing.T, store *s3.Backend, key string) bool {
	t.Helper()
	_, err := store.Stat(context.Background(), key)
	return err == nil
}

// TestReclaimOrphanAgainstRealStore is the end of the LastModified chain: the
// storage suite proves the store reports a sane timestamp, and this proves the
// collector's cutoff comparison then reclaims a genuinely unreferenced object.
func TestReclaimOrphanAgainstRealStore(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	protected := protectExisting(t, store)

	orphan := "it/gc-orphan-" + uuid.NewString()
	if err := store.Put(ctx, orphan, bytes.NewReader([]byte("orphan")), 6, "text/plain"); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, orphan) })

	repo := &gcFakeRepo{keys: protected} // everything but the orphan is referenced
	c := gc.New(store, repo, discardLogger(), gc.Options{Interval: time.Minute, GracePeriod: reclaimEverythingGrace})

	stats, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Orphans != 1 {
		t.Fatalf("reclaimed %d orphans, want exactly 1 (the planted key)", stats.Orphans)
	}
	if objectExists(t, store, orphan) {
		t.Fatal("planted orphan survived the sweep")
	}
}

// TestFreshOrphanSurvivesGrace is the data-loss guard, and the reason these
// tests run against a real store at all.
//
// An unreferenced object that was just written is indistinguishable from an
// upload whose metadata commit has not landed yet. The grace period is what
// protects it, and that protection is only as good as the LastModified the store
// reports. If a backend returns the zero time — or a badly skewed clock —
// `o.LastModified.After(cutoff)` is false, the sweep treats a live upload as an
// old orphan, and deletes it. Silently: no error, no log, just a missing object
// and a metadata row pointing at nothing.
//
// The in-memory double cannot catch this, because it returns whatever timestamp
// the test handed it. Only a real store can.
func TestFreshOrphanSurvivesGrace(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	protected := protectExisting(t, store)

	fresh := "it/gc-fresh-" + uuid.NewString()
	if err := store.Put(ctx, fresh, bytes.NewReader([]byte("in flight")), 9, "text/plain"); err != nil {
		t.Fatalf("plant fresh object: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, fresh) })

	// Unreferenced, exactly like an upload mid-commit — the grace period is the
	// only thing standing between it and deletion.
	repo := &gcFakeRepo{keys: protected}
	c := gc.New(store, repo, discardLogger(), gc.Options{Interval: time.Minute, GracePeriod: realisticGrace})

	stats, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Orphans != 0 {
		t.Errorf("sweep reclaimed %d objects inside the grace period, want 0", stats.Orphans)
	}
	if !objectExists(t, store, fresh) {
		t.Fatal("a just-written object was deleted inside a 1h grace period: " +
			"this store's ListObjects LastModified cannot be trusted, and running " +
			"CairnMark against it will destroy in-flight uploads")
	}
}

// TestPurgeDeletedAgainstRealStore covers the other half of a sweep: a
// soft-deleted row's object is removed from the store, then the row is purged.
func TestPurgeDeletedAgainstRealStore(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	protected := protectExisting(t, store)

	key := "it/gc-purge-" + uuid.NewString()
	if err := store.Put(ctx, key, bytes.NewReader([]byte("bye")), 3, "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, key) })
	protected[key] = struct{}{} // still row-backed until purged; not an orphan

	id := uuid.NewString()
	repo := &gcFakeRepo{
		keys:    protected,
		deleted: []*metadata.File{{ID: id, StorageKey: key}},
	}
	c := gc.New(store, repo, discardLogger(), gc.Options{Interval: time.Minute, GracePeriod: reclaimEverythingGrace})

	stats, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Purged != 1 {
		t.Fatalf("purged %d rows, want 1", stats.Purged)
	}
	if stats.Orphans != 0 {
		t.Fatalf("reclaimed %d orphans, want 0: the sweep touched objects it should not have", stats.Orphans)
	}
	if objectExists(t, store, key) {
		t.Fatal("purged object survived the sweep")
	}
	if len(repo.purged) != 1 || repo.purged[0] != id {
		t.Fatalf("row purges = %v, want exactly [%s]", repo.purged, id)
	}
}
