package memory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mettjs/cairnmark/internal/metadata"
)

func newJob(archiveID string, selection []int) *metadata.Job {
	return &metadata.Job{ID: uuid.Must(uuid.NewV7()).String(), ArchiveID: archiveID, Selection: selection}
}

func TestCreateJobRefusesASecondActiveJobPerArchive(t *testing.T) {
	ctx := context.Background()
	r := New()
	arch := create(t, r, nil)

	first := newJob(arch.ID, nil)
	if created, active, err := r.CreateJob(ctx, first); err != nil || !created || active != "" {
		t.Fatalf("first: created=%v active=%q err=%v", created, active, err)
	}
	if first.Status != metadata.JobPending || first.CreatedAt.IsZero() {
		t.Fatalf("first job not filled in: %+v", first)
	}
	// Pending and running both hold the slot.
	if created, active, err := r.CreateJob(ctx, newJob(arch.ID, nil)); err != nil || created || active != first.ID {
		t.Fatalf("while pending: created=%v active=%q err=%v", created, active, err)
	}
	if _, err := r.ClaimJob(ctx); err != nil {
		t.Fatal(err)
	}
	if created, active, _ := r.CreateJob(ctx, newJob(arch.ID, nil)); created || active != first.ID {
		t.Fatalf("while running: created=%v active=%q", created, active)
	}
	// A terminal job frees it.
	if err := r.FinishJob(ctx, first.ID, 1, metadata.JobResult{Status: metadata.JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	if created, _, err := r.CreateJob(ctx, newJob(arch.ID, nil)); err != nil || !created {
		t.Fatalf("after success: created=%v err=%v", created, err)
	}
	// The archive must exist: the foreign key.
	if _, _, err := r.CreateJob(ctx, newJob(uuid.NewString(), nil)); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("unknown archive: got %v want ErrNotFound", err)
	}
}

func TestSelectionRoundTripsNilAndEmpty(t *testing.T) {
	ctx := context.Background()
	r := New()
	a, b := create(t, r, nil), create(t, r, nil)
	all, none := newJob(a.ID, nil), newJob(b.ID, []int{})
	for _, j := range []*metadata.Job{all, none} {
		if _, _, err := r.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	gotAll, _ := r.GetJob(ctx, all.ID)
	gotNone, _ := r.GetJob(ctx, none.ID)
	if gotAll.Selection != nil {
		t.Fatalf("nil selection came back %v", gotAll.Selection)
	}
	if gotNone.Selection == nil || len(gotNone.Selection) != 0 {
		t.Fatalf("empty selection came back %v", gotNone.Selection)
	}
}

func TestClaimIsOldestFirstAndBumpsAttempt(t *testing.T) {
	ctx := context.Background()
	r := New()
	older := newJob(create(t, r, nil).ID, nil)
	newer := newJob(create(t, r, nil).ID, nil)
	for _, j := range []*metadata.Job{older, newer} {
		if _, _, err := r.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	got, err := r.ClaimJob(ctx)
	if err != nil || got == nil || got.ID != older.ID {
		t.Fatalf("claim: %+v err=%v", got, err)
	}
	if got.Status != metadata.JobRunning || got.Attempt != 1 || got.HeartbeatAt == nil {
		t.Fatalf("claimed job: %+v", got)
	}
	if n, _ := r.CountPendingJobs(ctx); n != 1 {
		t.Fatalf("pending after one claim: %d", n)
	}
	if got, _ := r.ClaimJob(ctx); got == nil || got.ID != newer.ID {
		t.Fatalf("second claim: %+v", got)
	}
	if got, err := r.ClaimJob(ctx); err != nil || got != nil {
		t.Fatalf("empty queue: %+v err=%v", got, err)
	}
}

func TestWorkerWritesAreFencedByAttempt(t *testing.T) {
	ctx := context.Background()
	r := New()
	j := newJob(create(t, r, nil).ID, nil)
	if _, _, err := r.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	first, _ := r.ClaimJob(ctx)

	cancel, err := r.HeartbeatJob(ctx, j.ID, first.Attempt, 3, 10)
	if err != nil || cancel {
		t.Fatalf("heartbeat: cancel=%v err=%v", cancel, err)
	}
	if got, _ := r.GetJob(ctx, j.ID); got.ProgressDone != 3 || got.ProgressTotal != 10 {
		t.Fatalf("progress not recorded: %+v", got)
	}

	// The worker stops reporting; the reaper hands the job back, and a
	// second claim owns it under the next attempt.
	if n, _ := r.ReapJobs(ctx, time.Hour); n != 0 {
		t.Fatalf("a live heartbeat was reaped")
	}
	n, err := r.ReapJobs(ctx, -time.Hour) // a cutoff in the future: everything is stale
	if err != nil || n != 1 {
		t.Fatalf("reap: n=%d err=%v", n, err)
	}
	if got, _ := r.GetJob(ctx, j.ID); got.Status != metadata.JobPending || got.HeartbeatAt != nil {
		t.Fatalf("reaped row: %+v", got)
	}
	second, _ := r.ClaimJob(ctx)
	if second == nil || second.Attempt != 2 {
		t.Fatalf("re-claim: %+v", second)
	}

	// Everything the first worker tries now affects nothing.
	if _, err := r.HeartbeatJob(ctx, j.ID, first.Attempt, 4, 10); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("stale heartbeat: got %v", err)
	}
	if err := r.FinishJob(ctx, j.ID, first.Attempt, metadata.JobResult{Status: metadata.JobSucceeded}); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("stale finish: got %v", err)
	}
	if err := r.ParkJob(ctx, j.ID, first.Attempt, 4, 10); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("stale park: got %v", err)
	}
	if got, _ := r.GetJob(ctx, j.ID); got.Status != metadata.JobRunning || got.Attempt != 2 {
		t.Fatalf("a fenced write changed the row: %+v", got)
	}

	// The owner's writes land.
	sum := json.RawMessage(`{"extracted":10}`)
	if err := r.FinishJob(ctx, j.ID, second.Attempt, metadata.JobResult{Status: metadata.JobSucceeded, Done: 10, Total: 12, Summary: sum}); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetJob(ctx, j.ID)
	if got.Status != metadata.JobSucceeded || string(got.Summary) != string(sum) || got.FinishedAt == nil || got.HeartbeatAt != nil {
		t.Fatalf("finished row: %+v", got)
	}
	if got.ProgressDone != 10 || got.ProgressTotal != 12 {
		t.Fatalf("the terminal write must carry both counters: %d/%d", got.ProgressDone, got.ProgressTotal)
	}
}

func TestRequestCancelByState(t *testing.T) {
	ctx := context.Background()
	r := New()
	if _, err := r.RequestCancel(ctx, uuid.NewString()); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("unknown job: got %v", err)
	}

	// Pending: cancelled outright, and the archive's slot is free again.
	pending := newJob(create(t, r, nil).ID, nil)
	if _, _, err := r.CreateJob(ctx, pending); err != nil {
		t.Fatal(err)
	}
	got, err := r.RequestCancel(ctx, pending.ID)
	if err != nil || got.Status != metadata.JobCancelled || !got.CancelRequested || got.FinishedAt == nil {
		t.Fatalf("cancel pending: %+v err=%v", got, err)
	}
	if created, _, _ := r.CreateJob(ctx, newJob(pending.ArchiveID, nil)); !created {
		t.Fatal("a cancelled job still holds the archive")
	}

	// Running: flagged, and the flag comes back through the heartbeat.
	running := newJob(create(t, r, nil).ID, nil)
	if _, _, err := r.CreateJob(ctx, running); err != nil {
		t.Fatal(err)
	}
	// Two pending jobs now; claim until we hold the running one.
	var claimed *metadata.Job
	for claimed == nil || claimed.ID != running.ID {
		if claimed, _ = r.ClaimJob(ctx); claimed == nil {
			t.Fatal("running job never claimed")
		}
	}
	got, _ = r.RequestCancel(ctx, running.ID)
	if got.Status != metadata.JobRunning || !got.CancelRequested {
		t.Fatalf("cancel running: %+v", got)
	}
	if cancel, _ := r.HeartbeatJob(ctx, running.ID, claimed.Attempt, 1, 2); !cancel {
		t.Fatal("heartbeat did not report the cancel request")
	}
	// Terminal: a no-op reporting the terminal state.
	_ = r.FinishJob(ctx, running.ID, claimed.Attempt, metadata.JobResult{Status: metadata.JobFailed, Error: "boom"})
	got, _ = r.RequestCancel(ctx, running.ID)
	if got.Status != metadata.JobFailed || got.Error != "boom" {
		t.Fatalf("cancel terminal: %+v", got)
	}
}

func TestPurgeFinishedJobsAndCascade(t *testing.T) {
	ctx := context.Background()
	r := New()
	arch := create(t, r, nil)
	done := newJob(arch.ID, nil)
	if _, _, err := r.CreateJob(ctx, done); err != nil {
		t.Fatal(err)
	}
	c, _ := r.ClaimJob(ctx)
	_ = r.FinishJob(ctx, done.ID, c.Attempt, metadata.JobResult{Status: metadata.JobSucceeded})
	live := newJob(arch.ID, nil)
	if _, _, err := r.CreateJob(ctx, live); err != nil {
		t.Fatal(err)
	}

	// Retention removes only terminal rows past the cutoff.
	if n, _ := r.PurgeFinishedJobs(ctx, time.Now().Add(-time.Hour)); n != 0 {
		t.Fatalf("purged a fresh row: %d", n)
	}
	if n, _ := r.PurgeFinishedJobs(ctx, time.Now().Add(time.Hour)); n != 1 {
		t.Fatalf("purge: %d want 1", n)
	}
	if _, err := r.GetJob(ctx, done.ID); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("purged job still readable: %v", err)
	}
	if _, err := r.GetJob(ctx, live.ID); err != nil {
		t.Fatalf("pending job purged: %v", err)
	}

	// Purging the archive row cascades to its jobs, whatever their state.
	if err := r.Delete(ctx, arch.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Purge(ctx, arch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetJob(ctx, live.ID); !errors.Is(err, metadata.ErrJobNotFound) {
		t.Fatalf("job survived its archive's purge: %v", err)
	}
}
