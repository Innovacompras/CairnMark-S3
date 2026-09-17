// Package jobs runs extraction jobs. A Worker claims pending jobs from the
// metadata repository, drives the files service's extraction with progress
// reporting and cancellation, and records each outcome. It is a sibling
// use-case to files and gc — it orchestrates files + metadata, and nothing
// imports it but the composition root.
//
// Its shape mirrors gc: narrow interfaces for what it needs, a Run that
// returns on cancellation, hooks so the package stays instrumentation-free,
// construction in the composition root only.
package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mettjs/cairnmark/internal/files"
	"github.com/mettjs/cairnmark/internal/metadata"
)

// Repo is the slice of metadata.JobRepository the worker needs.
type Repo interface {
	ClaimJob(ctx context.Context) (*metadata.Job, error)
	HeartbeatJob(ctx context.Context, id string, attempt, done, total int) (cancelRequested bool, err error)
	FinishJob(ctx context.Context, id string, attempt int, result metadata.JobResult) error
	ParkJob(ctx context.Context, id string, attempt, done, total int) error
	ReapJobs(ctx context.Context, ttl time.Duration) (int, error)
	CountPendingJobs(ctx context.Context) (int, error)
}

// Extractor is the slice of *files.Service the worker drives.
type Extractor interface {
	Extract(ctx context.Context, id string, o files.ExtractOptions) (*files.ExtractSummary, error)
}

// Outcome reports one run's resting state: a terminal status, or JobPending
// when the run was parked (shutdown) or lost to a reaper and will be resumed.
type Outcome struct {
	JobID     string
	ArchiveID string
	Status    metadata.JobStatus
	Elapsed   time.Duration
}

// Sweep reports one reaper pass.
type Sweep struct {
	Reaped  int // running jobs returned to pending because their worker stopped reporting
	Pending int // queue depth after the pass
}

// Worker claims and runs extraction jobs.
type Worker struct {
	repo    Repo
	svc     Extractor
	log     *slog.Logger
	opts    Options
	onJob   func(Outcome)
	onSweep func(Sweep, error)
}

// New constructs a Worker.
func New(repo Repo, svc Extractor, log *slog.Logger, o Options) *Worker {
	if o.Concurrency < 1 {
		o.Concurrency = 1
	}
	if o.HeartbeatTTL <= 0 {
		o.HeartbeatTTL = DefaultHeartbeatTTL
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = defaultFlushInterval
	}
	if o.ReapInterval <= 0 {
		o.ReapInterval = max(o.PollInterval, o.HeartbeatTTL/10)
	}
	return &Worker{repo: repo, svc: svc, log: log, opts: o}
}

// OnJob registers fn to observe every run's outcome. The composition root
// wires metrics through it, so this package stays instrumentation-free. Call
// it before Run; it is not safe to change while the worker is running.
func (w *Worker) OnJob(fn func(Outcome)) { w.onJob = fn }

// OnSweep registers fn to observe every reaper pass, likewise.
func (w *Worker) OnSweep(fn func(Sweep, error)) { w.onSweep = fn }

// Run claims and runs jobs until ctx is cancelled, then returns once every
// runner has parked or finished its job — the composition root waits on it
// within the shutdown timeout, so a running job's row goes back to pending
// rather than being left running for the reaper to find.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("jobs worker started",
		"concurrency", w.opts.Concurrency, "heartbeatTTL", w.opts.HeartbeatTTL,
		"poll", w.opts.PollInterval, "reap", w.opts.ReapInterval)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.reapLoop(ctx)
	}()
	for range w.opts.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runLoop(ctx)
		}()
	}
	wg.Wait()
	w.log.Info("jobs worker stopped")
}

func (w *Worker) runLoop(ctx context.Context) {
	for {
		ran, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.log.Error("jobs: claim failed", "err", err)
		}
		if ran && ctx.Err() == nil {
			continue // there may be more queued behind it
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.opts.PollInterval):
		}
	}
}

// RunOnce claims one pending job and runs it to its next resting state,
// reporting whether there was one. Exposed so tests can drive the worker
// synchronously.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	job, err := w.repo.ClaimJob(ctx)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}
	w.run(ctx, job)
	return true, nil
}
