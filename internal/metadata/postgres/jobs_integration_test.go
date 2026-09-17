//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// The three things only Postgres can prove: the partial unique index refuses
// a second active job, FOR UPDATE SKIP LOCKED hands one job to exactly one of
// several concurrent claimers, and the fenced writes really affect zero rows
// once a reap has moved the row on.

// createArchive inserts a files row for jobs to reference, purged after the
// test — which also cascades to any jobs left behind.
func createArchive(t *testing.T, r *Repo) *metadata.File {
	t.Helper()
	ctx := context.Background()
	f := newFile()
	if err := r.Create(ctx, f); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Delete(ctx, f.ID)
		_ = r.Purge(ctx, f.ID)
	})
	return f
}

func newJob(archiveID string, selection []int) *metadata.Job {
	return &metadata.Job{ID: uuid.Must(uuid.NewV7()).String(), ArchiveID: archiveID, Selection: selection}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return reflect.DeepEqual(va, vb)
}

// drainPending claims until the queue is empty, so a test's claims are not
// confused by rows another test in this package left pending.
func drainPending(t *testing.T, r *Repo) {
	t.Helper()
	for {
		j, err := r.ClaimJob(context.Background())
		if err != nil {
			t.Fatalf("ClaimJob: %v", err)
		}
		if j == nil {
			return
		}
		_ = r.FinishJob(context.Background(), j.ID, j.Attempt, metadata.JobResult{Status: metadata.JobCancelled})
	}
}

func TestRepoJobUniqueIndexAndSelection(t *testing.T) {
	ctx := context.Background()
	r := New(testPool)
	arch := createArchive(t, r)

	first := newJob(arch.ID, []int{})
	created, active, err := r.CreateJob(ctx, first)
	if err != nil || !created || active != "" {
		t.Fatalf("first: created=%v active=%q err=%v", created, active, err)
	}
	created, active, err = r.CreateJob(ctx, newJob(arch.ID, nil))
	if err != nil || created || active != first.ID {
		t.Fatalf("second active job admitted: created=%v active=%q err=%v", created, active, err)
	}
	// The selection's nil/empty distinction survives jsonb.
	got, err := r.GetJob(ctx, first.ID)
	if err != nil || got.Selection == nil || len(got.Selection) != 0 || got.Status != metadata.JobPending {
		t.Fatalf("empty selection: %+v err=%v", got, err)
	}
	if _, err := r.GetJob(ctx, uuid.NewString()); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("unknown job: got %v", err)
	}
	// Cancelled while pending: terminal at once, and the slot is free.
	c, err := r.RequestCancel(ctx, first.ID)
	if err != nil || c.Status != metadata.JobCancelled || c.FinishedAt == nil {
		t.Fatalf("cancel pending: %+v err=%v", c, err)
	}
	if created, _, err := r.CreateJob(ctx, newJob(arch.ID, nil)); err != nil || !created {
		t.Fatalf("after cancel: created=%v err=%v", created, err)
	}
	// The foreign key refuses an archive that does not exist.
	if _, _, err := r.CreateJob(ctx, newJob(uuid.NewString(), nil)); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("unknown archive: got %v want ErrNotFound", err)
	}
}

func TestRepoClaimJobConcurrentCallersOneWins(t *testing.T) {
	ctx := context.Background()
	r := New(testPool)
	drainPending(t, r)
	arch := createArchive(t, r)
	j := newJob(arch.ID, nil)
	if _, _, err := r.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}

	const callers = 8
	var wg sync.WaitGroup
	wins := make(chan *metadata.Job, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := r.ClaimJob(ctx)
			if err != nil {
				t.Errorf("ClaimJob: %v", err)
				return
			}
			if got != nil {
				wins <- got
			}
		}()
	}
	wg.Wait()
	close(wins)
	var won []*metadata.Job
	for w := range wins {
		won = append(won, w)
	}
	if len(won) != 1 || won[0].ID != j.ID || won[0].Attempt != 1 || won[0].Status != metadata.JobRunning {
		t.Fatalf("claims won: %d (%+v), want exactly one", len(won), won)
	}
}

func TestRepoReapAndFencing(t *testing.T) {
	ctx := context.Background()
	r := New(testPool)
	drainPending(t, r)
	arch := createArchive(t, r)
	j := newJob(arch.ID, nil)
	if _, _, err := r.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	first, err := r.ClaimJob(ctx)
	if err != nil || first == nil {
		t.Fatalf("claim: %+v err=%v", first, err)
	}
	if cancel, err := r.HeartbeatJob(ctx, j.ID, first.Attempt, 2, 5); err != nil || cancel {
		t.Fatalf("heartbeat: cancel=%v err=%v", cancel, err)
	}
	if got, _ := r.GetJob(ctx, j.ID); got.ProgressDone != 2 || got.ProgressTotal != 5 || got.HeartbeatAt == nil {
		t.Fatalf("progress: %+v", got)
	}

	// A live heartbeat is left alone; a stale one is returned to pending.
	if n, err := r.ReapJobs(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("reaped a live job: n=%d err=%v", n, err)
	}
	if n, err := r.ReapJobs(ctx, -time.Hour); err != nil || n != 1 { // a cutoff in the future: everything is stale
		t.Fatalf("reap: n=%d err=%v", n, err)
	}
	if got, _ := r.GetJob(ctx, j.ID); got.Status != metadata.JobPending || got.HeartbeatAt != nil {
		t.Fatalf("reaped row: %+v", got)
	}
	second, err := r.ClaimJob(ctx)
	if err != nil || second == nil || second.ID != j.ID || second.Attempt != 2 {
		t.Fatalf("re-claim: %+v err=%v", second, err)
	}

	// The first worker's writes are fenced out.
	if _, err := r.HeartbeatJob(ctx, j.ID, first.Attempt, 3, 5); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("stale heartbeat: got %v", err)
	}
	if err := r.FinishJob(ctx, j.ID, first.Attempt, metadata.JobResult{Status: metadata.JobSucceeded}); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("stale finish: got %v", err)
	}
	if err := r.ParkJob(ctx, j.ID, first.Attempt, 3, 5); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("stale park: got %v", err)
	}

	// Cancel a running job: flagged, seen through the owner's heartbeat.
	if c, err := r.RequestCancel(ctx, j.ID); err != nil || c.Status != metadata.JobRunning || !c.CancelRequested {
		t.Fatalf("cancel running: %+v err=%v", c, err)
	}
	if cancel, err := r.HeartbeatJob(ctx, j.ID, second.Attempt, 4, 5); err != nil || !cancel {
		t.Fatalf("owner heartbeat: cancel=%v err=%v", cancel, err)
	}

	// Park, re-claim, and finish under the new attempt.
	if err := r.ParkJob(ctx, j.ID, second.Attempt, 4, 6); err != nil {
		t.Fatalf("park: %v", err)
	}
	third, _ := r.ClaimJob(ctx)
	if third == nil || third.Attempt != 3 || third.ProgressDone != 4 || third.ProgressTotal != 6 || !third.CancelRequested {
		t.Fatalf("claim after park: %+v", third)
	}
	sum := json.RawMessage(`{"archive_id":"x","extracted":4}`)
	if err := r.FinishJob(ctx, j.ID, third.Attempt, metadata.JobResult{Status: metadata.JobCancelled, Done: 4, Total: 6, Summary: sum}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ := r.GetJob(ctx, j.ID)
	if got.Status != metadata.JobCancelled || got.FinishedAt == nil || got.HeartbeatAt != nil {
		t.Fatalf("terminal row: %+v", got)
	}
	if got.ProgressDone != 4 || got.ProgressTotal != 6 {
		t.Fatalf("the terminal write must carry both counters: %d/%d", got.ProgressDone, got.ProgressTotal)
	}
	// jsonb keeps the value, not the bytes: key order and whitespace are
	// normalised, so the summary is compared as JSON, not as text.
	if !jsonEqual(t, got.Summary, sum) {
		t.Fatalf("summary: got %s want %s", got.Summary, sum)
	}
	// Cancel on a terminal job is a no-op reporting the terminal state.
	if c, _ := r.RequestCancel(ctx, j.ID); c.Status != metadata.JobCancelled {
		t.Fatalf("cancel terminal: %+v", c)
	}

	// Retention: only terminal rows past the cutoff go.
	if n, _ := r.PurgeFinishedJobs(ctx, time.Now().Add(-time.Hour)); n != 0 {
		t.Fatalf("purged a fresh row: %d", n)
	}
	if n, err := r.PurgeFinishedJobs(ctx, time.Now().Add(time.Hour)); err != nil || n < 1 {
		t.Fatalf("purge: n=%d err=%v", n, err)
	}
	if _, err := r.GetJob(ctx, j.ID); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("purged job still readable: %v", err)
	}
}

func TestRepoJobCascadesWithArchive(t *testing.T) {
	ctx := context.Background()
	r := New(testPool)
	arch := createArchive(t, r)
	j := newJob(arch.ID, []int{1, 2})
	if _, _, err := r.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, arch.ID); err != nil {
		t.Fatal(err)
	}
	// A soft delete leaves the job; the purge takes it.
	if got, err := r.GetJob(ctx, j.ID); err != nil || got.Selection == nil || len(got.Selection) != 2 {
		t.Fatalf("after soft delete: %+v err=%v", got, err)
	}
	if err := r.Purge(ctx, arch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetJob(ctx, j.ID); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("job survived its archive's purge: %v", err)
	}
}
