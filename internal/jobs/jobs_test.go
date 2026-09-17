package jobs_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mettjs/cairnmark/internal/files"
	"github.com/mettjs/cairnmark/internal/jobs"
	"github.com/mettjs/cairnmark/internal/metadata"
	metamem "github.com/mettjs/cairnmark/internal/metadata/memory"
	"github.com/mettjs/cairnmark/internal/storage"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

// The standard fixture of the files suite: three documents and one piece of
// Finder litter, so indexes 0, 1 and 3 are extracted and 2 is skipped. The
// archive's own upload is Put #1, so the children are Puts #2, #3 and #4.
var docs = []struct{ name, body string }{
	{"reports/q3.pdf", "%PDF-1.7 quarterly report"},
	{"reports/data.json", `{"revenue": 42}`},
	{"__MACOSX/reports/._q3.pdf", "resource fork"},
	{"notes/readme.txt", "plain notes"},
}

func buildZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range docs {
		w, err := zw.Create(m.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, m.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gate is a Backend that blocks its holdAt-th Put (1-based) until released,
// so a test can hold a run at a chosen entry and act while it is there. It
// honours the context, as a real store would.
type gate struct {
	storage.Backend
	mu      sync.Mutex
	puts    int
	holdAt  int
	failAt  int
	entered chan struct{}
	release chan struct{}
}

func newGate(holdAt int) *gate {
	return &gate{Backend: memory.New(), holdAt: holdAt, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gate) Put(ctx context.Context, key string, r io.Reader, size int64, ct string) error {
	g.mu.Lock()
	g.puts++
	n := g.puts
	g.mu.Unlock()
	if n == g.failAt {
		return errors.New("injected store failure")
	}
	if n == g.holdAt {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.Backend.Put(ctx, key, r, size, ct)
}

// reapAll is a negative TTL: a cutoff in the future, so every running job
// counts as stranded.
const reapAll = -time.Hour

// outcomes records what the worker reports through OnJob.
type outcomes struct {
	mu   sync.Mutex
	seen []jobs.Outcome
}

func (o *outcomes) record(out jobs.Outcome) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, out)
}

func (o *outcomes) last(t *testing.T) jobs.Outcome {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.seen) == 0 {
		t.Fatal("no outcome reported")
	}
	return o.seen[len(o.seen)-1]
}

type stack struct {
	svc  *files.Service
	repo *metamem.Repo
	w    *jobs.Worker
	out  *outcomes
}

// newStack wires a service, repository and worker over backend. The flush
// and poll intervals are tiny so the tests run in milliseconds; the TTL is
// long so nothing is reaped unless a test asks for it.
func newStack(t *testing.T, backend storage.Backend) *stack {
	t.Helper()
	repo := metamem.New()
	svc := files.New(backend, repo)
	w := jobs.New(repo, svc, slog.New(slog.NewTextHandler(io.Discard, nil)), jobs.Options{
		HeartbeatTTL: time.Hour, PollInterval: 5 * time.Millisecond, FlushInterval: 2 * time.Millisecond,
	})
	out := &outcomes{}
	w.OnJob(out.record)
	return &stack{svc: svc, repo: repo, w: w, out: out}
}

func (s *stack) upload(t *testing.T) *files.File {
	t.Helper()
	data := buildZip(t)
	f, err := s.svc.Upload(context.Background(), files.UploadInput{
		Filename: "docs.zip", ContentType: "application/zip", Size: int64(len(data)), Body: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("upload archive: %v", err)
	}
	return f
}

func (s *stack) submit(t *testing.T, archiveID string) *files.Job {
	t.Helper()
	job, err := s.svc.SubmitExtraction(context.Background(), archiveID, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return job
}

func (s *stack) runOnce(t *testing.T, ctx context.Context) {
	t.Helper()
	ran, err := s.w.RunOnce(ctx)
	if err != nil || !ran {
		t.Fatalf("RunOnce: ran=%v err=%v", ran, err)
	}
}

func (s *stack) job(t *testing.T, id string) *metadata.Job {
	t.Helper()
	j, err := s.repo.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	return j
}

// children counts an archive's live children, failing if two rows claim one
// index — the duplicate a broken resume would produce.
func (s *stack) children(t *testing.T, archiveID string) int {
	t.Helper()
	page, err := s.svc.List(context.Background(), files.ListFilter{
		Tags: map[string]any{metadata.TagArchiveID: archiveID}, Limit: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[any]bool{}
	for _, f := range page {
		idx := f.Metadata[metadata.TagArchiveIndex]
		if seen[idx] {
			t.Fatalf("two children for index %v", idx)
		}
		seen[idx] = true
	}
	return len(page)
}

func summaryOf(t *testing.T, j *metadata.Job) *files.ExtractSummary {
	t.Helper()
	if j.Summary == nil {
		t.Fatalf("job %s has no summary: %+v", j.ID, j)
	}
	var sum files.ExtractSummary
	if err := json.Unmarshal(j.Summary, &sum); err != nil {
		t.Fatalf("summary %s: %v", j.Summary, err)
	}
	return &sum
}

func TestJobRunsToSuccessWithTheSynchronousSummary(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, memory.New())
	arch := s.upload(t)
	job := s.submit(t, arch.ID)

	s.runOnce(t, ctx)
	got := s.job(t, job.ID)
	if got.Status != metadata.JobSucceeded || got.Attempt != 1 || got.FinishedAt == nil || got.HeartbeatAt != nil {
		t.Fatalf("job after run: %+v", got)
	}
	if got.ProgressDone != 3 || got.ProgressTotal != 3 {
		t.Fatalf("progress: %d/%d want 3/3", got.ProgressDone, got.ProgressTotal)
	}

	// The persisted summary is exactly what a direct extraction returns.
	direct := newStack(t, memory.New())
	want, err := direct.svc.Extract(ctx, direct.upload(t).ID, files.ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want.ArchiveID = arch.ID
	if sum := summaryOf(t, got); !reflect.DeepEqual(sum, want) {
		t.Fatalf("summary:\n got %+v\nwant %+v", sum, want)
	}
	if n := s.children(t, arch.ID); n != 3 {
		t.Fatalf("children: %d", n)
	}
	if f, _ := s.svc.Metadata(ctx, arch.ID); f.Metadata[metadata.TagArchive] != metadata.TagArchiveMarker {
		t.Fatal("archive not marked")
	}
	if out := s.out.last(t); out.JobID != job.ID || out.Status != metadata.JobSucceeded || out.Elapsed <= 0 {
		t.Fatalf("outcome: %+v", out)
	}
	if ran, err := s.w.RunOnce(ctx); ran || err != nil {
		t.Fatalf("empty queue: ran=%v err=%v", ran, err)
	}
}

func TestShutdownParksTheJobAndTheNextPickupResumes(t *testing.T) {
	g := newGate(3) // hold the second child
	s := newStack(t, g)
	arch := s.upload(t)
	job := s.submit(t, arch.ID)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runOnce(t, ctx)
	}()
	<-g.entered
	cancel() // the shutdown signal, mid-entry
	<-done

	// The row went back to pending with its progress, not left running.
	got := s.job(t, job.ID)
	if got.Status != metadata.JobPending || got.HeartbeatAt != nil || got.ProgressDone != 1 || got.ProgressTotal != 3 || got.Attempt != 1 {
		t.Fatalf("parked job: %+v", got)
	}
	if out := s.out.last(t); out.Status != metadata.JobPending {
		t.Fatalf("outcome: %+v", out)
	}
	if n := s.children(t, arch.ID); n != 1 {
		t.Fatalf("children after the interruption: %d want 1", n)
	}

	// A fresh worker completes it, and nothing is written twice.
	g.holdAt = 0
	s.runOnce(t, context.Background())
	got = s.job(t, job.ID)
	if got.Status != metadata.JobSucceeded || got.Attempt != 2 {
		t.Fatalf("resumed job: %+v", got)
	}
	if sum := summaryOf(t, got); sum.Extracted != 2 || sum.SkippedByReason[files.SkipAlreadyExtracted] != 1 {
		t.Fatalf("resumed summary: %+v", sum)
	}
	if n := s.children(t, arch.ID); n != 3 {
		t.Fatalf("children after resume: %d want 3", n)
	}
}

func TestStrandedJobIsReapedAndResumed(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, memory.New())
	arch := s.upload(t)
	job := s.submit(t, arch.ID)

	// A worker claims it and dies.
	if _, err := s.repo.ClaimJob(ctx); err != nil {
		t.Fatal(err)
	}
	// Under a long TTL the row is honoured as live.
	if sw, err := s.w.ReapOnce(ctx); err != nil || sw.Reaped != 0 {
		t.Fatalf("live heartbeat reaped: %+v err=%v", sw, err)
	}
	// Under a TTL the heartbeat has outlived, it is returned to the queue.
	tight := jobs.New(s.repo, s.svc, slog.New(slog.NewTextHandler(io.Discard, nil)), jobs.Options{HeartbeatTTL: time.Nanosecond})
	time.Sleep(time.Millisecond)
	sw, err := tight.ReapOnce(ctx)
	if err != nil || sw.Reaped != 1 || sw.Pending != 1 {
		t.Fatalf("reap: %+v err=%v", sw, err)
	}
	if got := s.job(t, job.ID); got.Status != metadata.JobPending || got.HeartbeatAt != nil {
		t.Fatalf("reaped row: %+v", got)
	}

	s.runOnce(t, ctx)
	got := s.job(t, job.ID)
	if got.Status != metadata.JobSucceeded || got.Attempt != 2 {
		t.Fatalf("after pickup: %+v", got)
	}
	if n := s.children(t, arch.ID); n != 3 {
		t.Fatalf("children: %d", n)
	}
}

// awaitHeartbeatAfter waits until the worker has flushed since after — which
// is when it has read back whatever flag was set before after.
func (s *stack) awaitHeartbeatAfter(t *testing.T, id string, after time.Time) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j := s.job(t, id); j.HeartbeatAt != nil && j.HeartbeatAt.After(after) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the worker never flushed after the cancel request")
}

func TestCancelMidRunStopsAfterTheCurrentEntry(t *testing.T) {
	ctx := context.Background()
	g := newGate(3) // hold the second child
	s := newStack(t, g)
	arch := s.upload(t)
	job := s.submit(t, arch.ID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runOnce(t, ctx)
	}()
	<-g.entered
	if j, err := s.svc.CancelExtraction(ctx, job.ID); err != nil || j.Status != metadata.JobRunning || !j.CancelRequested {
		t.Fatalf("cancel running: %+v err=%v", j, err)
	}
	s.awaitHeartbeatAfter(t, job.ID, time.Now())
	close(g.release)
	<-done

	// It finished the entry it was on, then stopped: two children, not three.
	got := s.job(t, job.ID)
	if got.Status != metadata.JobCancelled || got.FinishedAt == nil || got.ProgressDone != 2 || got.ProgressTotal != 3 {
		t.Fatalf("cancelled job: %+v", got)
	}
	if sum := summaryOf(t, got); sum.Extracted != 2 {
		t.Fatalf("partial summary: %+v", sum)
	}
	if n := s.children(t, arch.ID); n != 2 {
		t.Fatalf("children after cancel: %d want 2", n)
	}
	if out := s.out.last(t); out.Status != metadata.JobCancelled {
		t.Fatalf("outcome: %+v", out)
	}

	// The children stay, and a new job resumes past them.
	again := s.submit(t, arch.ID)
	s.runOnce(t, ctx)
	got = s.job(t, again.ID)
	if sum := summaryOf(t, got); got.Status != metadata.JobSucceeded || sum.Extracted != 1 || sum.SkippedByReason[files.SkipAlreadyExtracted] != 2 {
		t.Fatalf("resubmitted job: %+v summary %+v", got, sum)
	}
	if n := s.children(t, arch.ID); n != 3 {
		t.Fatalf("children after resubmit: %d want 3", n)
	}
}

func TestJobFailsWhenTheStoreFails(t *testing.T) {
	ctx := context.Background()
	g := newGate(0)
	g.failAt = 3 // the second child
	s := newStack(t, g)
	arch := s.upload(t)
	job := s.submit(t, arch.ID)

	s.runOnce(t, ctx)
	got := s.job(t, job.ID)
	if got.Status != metadata.JobFailed || !strings.Contains(got.Error, "injected store failure") || got.Summary != nil {
		t.Fatalf("failed job: %+v", got)
	}
	if got.ProgressDone != 1 || got.FinishedAt == nil {
		t.Fatalf("failed job progress: %+v", got)
	}
	if n := s.children(t, arch.ID); n != 1 {
		t.Fatalf("children: %d", n)
	}
	if out := s.out.last(t); out.Status != metadata.JobFailed {
		t.Fatalf("outcome: %+v", out)
	}

	// The failure freed the archive; the next job resumes.
	g.failAt = 0
	s.submit(t, arch.ID)
	s.runOnce(t, ctx)
	if n := s.children(t, arch.ID); n != 3 {
		t.Fatalf("children after retry: %d", n)
	}
}

func TestJobCancelledBeforeItRanFinishesAsCancelled(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, memory.New())
	arch := s.upload(t)
	job := s.submit(t, arch.ID)

	// A worker claimed it and got part-way, the caller cancelled, the worker
	// died, the reaper handed the row back with the flag still set.
	if _, err := s.repo.ClaimJob(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.repo.HeartbeatJob(ctx, job.ID, 1, 2, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.CancelExtraction(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.repo.ReapJobs(ctx, reapAll); n != 1 {
		t.Fatalf("reaped %d", n)
	}

	s.runOnce(t, ctx)
	got := s.job(t, job.ID)
	if got.Status != metadata.JobCancelled || got.Summary != nil || got.FinishedAt == nil {
		t.Fatalf("job: %+v", got)
	}
	// The progress the earlier attempt recorded is kept, not zeroed.
	if got.ProgressDone != 2 || got.ProgressTotal != 5 {
		t.Fatalf("progress after a pre-cancelled pickup: %d/%d want 2/5", got.ProgressDone, got.ProgressTotal)
	}
	if n := s.children(t, arch.ID); n != 0 {
		t.Fatalf("a cancelled job extracted %d entries", n)
	}
}

func TestLostOwnershipStopsTheRunWithoutClobbering(t *testing.T) {
	ctx := context.Background()
	g := newGate(3) // hold the second child
	s := newStack(t, g)
	arch := s.upload(t)
	job := s.submit(t, arch.ID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runOnce(t, ctx)
	}()
	<-g.entered

	// The reaper — a TTL too tight for this run — hands the job to a second
	// worker while the first is still writing.
	if n, _ := s.repo.ReapJobs(ctx, reapAll); n != 1 {
		t.Fatalf("reaped %d", n)
	}
	stolen, err := s.repo.ClaimJob(ctx)
	if err != nil || stolen == nil || stolen.Attempt != 2 {
		t.Fatalf("second claim: %+v err=%v", stolen, err)
	}
	// Give the first worker's heartbeat several intervals to notice.
	time.Sleep(50 * time.Millisecond)
	close(g.release)
	<-done

	// The first worker finished the entry it was on and stopped, and its
	// fenced writes changed nothing: the row is still the second worker's.
	got := s.job(t, job.ID)
	if got.Status != metadata.JobRunning || got.Attempt != 2 || got.FinishedAt != nil {
		t.Fatalf("row after the lost run: %+v", got)
	}
	if out := s.out.last(t); out.Status != metadata.JobPending {
		t.Fatalf("outcome: %+v", out)
	}
	if n := s.children(t, arch.ID); n != 2 {
		t.Fatalf("children written by the lost run: %d want 2", n)
	}

	// The second worker's run resumes without duplicating.
	if err := s.repo.ParkJob(ctx, job.ID, 2, 0, 0); err != nil {
		t.Fatal(err)
	}
	s.runOnce(t, ctx)
	if got := s.job(t, job.ID); got.Status != metadata.JobSucceeded || got.Attempt != 3 {
		t.Fatalf("resumed: %+v", got)
	}
	if n := s.children(t, arch.ID); n != 3 {
		t.Fatalf("children: %d", n)
	}
}

func TestRunLoopPicksUpWorkAndStops(t *testing.T) {
	s := newStack(t, memory.New())
	arch := s.upload(t)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.w.Run(ctx)
	}()

	job := s.submit(t, arch.ID)
	deadline := time.Now().Add(5 * time.Second)
	for s.job(t, job.ID).Status != metadata.JobSucceeded {
		if time.Now().After(deadline) {
			t.Fatalf("job never ran: %+v", s.job(t, job.ID))
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if n := s.children(t, arch.ID); n != 3 {
		t.Fatalf("children: %d", n)
	}
}

// silentHeartbeat is a repository whose heartbeats never land — what a run
// that finishes before its first flush reaches the database looks like.
type silentHeartbeat struct{ *metamem.Repo }

func (silentHeartbeat) HeartbeatJob(context.Context, string, int, int, int) (bool, error) {
	return false, nil
}

func TestTerminalRowCarriesTheProgressWithoutAHeartbeat(t *testing.T) {
	// A short job can end before its first heartbeat flush lands. The
	// terminal write must carry both counters itself, or such a job reads
	// as three done of a total of nothing.
	ctx := context.Background()
	s := newStack(t, memory.New())
	w := jobs.New(silentHeartbeat{s.repo}, s.svc, slog.New(slog.NewTextHandler(io.Discard, nil)),
		jobs.Options{HeartbeatTTL: time.Hour, FlushInterval: time.Hour})
	arch := s.upload(t)
	job := s.submit(t, arch.ID)

	if ran, err := w.RunOnce(ctx); err != nil || !ran {
		t.Fatalf("RunOnce: ran=%v err=%v", ran, err)
	}
	got := s.job(t, job.ID)
	if got.Status != metadata.JobSucceeded || got.ProgressDone != 3 || got.ProgressTotal != 3 {
		t.Fatalf("terminal row: %s %d/%d, want succeeded 3/3", got.Status, got.ProgressDone, got.ProgressTotal)
	}
}
