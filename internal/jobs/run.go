package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mettjs/cairnmark/internal/files"
	"github.com/mettjs/cairnmark/internal/metadata"
)

// writeTimeout bounds the terminal write once the run's own context is gone —
// a shutdown must still park the row, and a finished run must still record
// its result.
const writeTimeout = 5 * time.Second

// running is one claimed job while this worker drives it. The extraction
// runs on the caller's goroutine; a heartbeat goroutine flushes progress and
// liveness on its own clock, because a single large entry can take minutes
// and a heartbeat tied to entry boundaries would go stale mid-entry.
type running struct {
	w     *Worker
	job   *metadata.Job
	done  atomic.Int64
	total atomic.Int64
	// stop is read by the extraction's progress hook. Once set — by a cancel
	// request, or by losing the row — the run ends after the current entry.
	stop atomic.Bool
	// lost means the row is no longer this attempt's to finish.
	lost atomic.Bool
	// kick asks the heartbeat loop for a prompt flush, so progress_total is
	// visible as soon as the run knows it rather than one interval later.
	kick chan struct{}
}

func (w *Worker) run(ctx context.Context, job *metadata.Job) {
	start := time.Now()
	r := &running{w: w, job: job, kick: make(chan struct{}, 1)}
	status := r.execute(ctx)
	elapsed := time.Since(start)
	w.log.Info("job run ended", "job", job.ID, "archive", job.ArchiveID, "attempt", job.Attempt,
		"status", status, "elapsed", elapsed)
	if w.onJob != nil {
		w.onJob(Outcome{JobID: job.ID, ArchiveID: job.ArchiveID, Status: status, Elapsed: elapsed})
	}
}

func (r *running) execute(ctx context.Context) metadata.JobStatus {
	if r.job.CancelRequested {
		// Cancelled before it ran — a reaped or parked row can carry the flag
		// — so there is nothing to do but say so, keeping whatever progress
		// the previous attempt recorded.
		return r.finish(ctx, metadata.JobResult{
			Status: metadata.JobCancelled, Done: r.job.ProgressDone, Total: r.job.ProgressTotal,
		})
	}
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	var hb sync.WaitGroup
	hb.Add(1)
	go func() {
		defer hb.Done()
		r.heartbeat(hbCtx)
	}()

	sum, err := r.w.svc.Extract(ctx, r.job.ArchiveID, files.ExtractOptions{
		Entries: r.job.Selection, OnProgress: r.progress,
	})
	stopHeartbeat()
	hb.Wait()

	done, total := int(r.done.Load()), int(r.total.Load())
	switch {
	case r.lost.Load():
		// Not ours to finish. The park is attempted anyway — fenced by the
		// attempt, it lands only if the row was merely unreachable rather
		// than re-claimed, and then the next pickup resumes at once.
		return r.park(ctx, done, total)
	case errors.Is(err, files.ErrExtractCancelled):
		return r.finish(ctx, metadata.JobResult{Status: metadata.JobCancelled, Done: done, Total: total, Summary: marshal(sum)})
	case err != nil && ctx.Err() != nil:
		// Shutdown. Abandoning the run mid-entry is safe: an upload commits
		// its object before its row, so an interrupted entry leaves no row,
		// and the object is an orphan GC reclaims. What must not be left
		// behind is a row still marked running — it goes back to pending
		// with its progress, and the next pickup resumes past the children
		// already written.
		return r.park(ctx, done, total)
	case err != nil:
		r.w.log.Error("job failed", "job", r.job.ID, "archive", r.job.ArchiveID, "err", err)
		return r.finish(ctx, metadata.JobResult{Status: metadata.JobFailed, Done: done, Total: total, Error: err.Error()})
	default:
		return r.finish(ctx, metadata.JobResult{Status: metadata.JobSucceeded, Done: done, Total: total, Summary: marshal(sum)})
	}
}

// finish records the terminal result on a context that survives the run's.
// If the write fails the row is left running for the reaper, which is the
// same recovery a crash gets — so the reported outcome is pending.
func (r *running) finish(ctx context.Context, res metadata.JobResult) metadata.JobStatus {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := r.w.repo.FinishJob(wctx, r.job.ID, r.job.Attempt, res); err != nil {
		r.w.log.Error("job: record result", "job", r.job.ID, "status", res.Status, "err", err)
		return metadata.JobPending
	}
	return res.Status
}

// park returns the row to pending with its progress, for the next pickup.
func (r *running) park(ctx context.Context, done, total int) metadata.JobStatus {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := r.w.repo.ParkJob(wctx, r.job.ID, r.job.Attempt, done, total); err != nil {
		if errors.Is(err, metadata.ErrJobNotFound) {
			r.w.log.Info("job: moved on under us, nothing to park", "job", r.job.ID, "attempt", r.job.Attempt)
		} else {
			r.w.log.Error("job: park failed, the reaper will recover it", "job", r.job.ID, "err", err)
		}
	}
	return metadata.JobPending
}

// marshal renders the summary for storage; nil when there is none.
func marshal(sum *files.ExtractSummary) json.RawMessage {
	if sum == nil {
		return nil
	}
	b, err := json.Marshal(sum)
	if err != nil {
		return nil // unreachable for this struct; a nil summary is the honest fallback
	}
	return b
}
