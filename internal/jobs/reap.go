package jobs

import (
	"context"
	"time"
)

// The reaper runs on the worker's own ticker, not in gc: gc reconciles the
// object store against the metadata table, and whether a job's worker is
// alive is job-domain logic. (gc does own the retention sweep of finished
// rows — that is row cleanup, which is exactly its job.)

func (w *Worker) reapLoop(ctx context.Context) {
	t := time.NewTicker(w.opts.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s, err := w.ReapOnce(ctx)
		if w.onSweep != nil {
			w.onSweep(s, err)
		}
		if err != nil {
			if ctx.Err() == nil {
				w.log.Error("jobs: reap failed", "err", err)
			}
			continue
		}
		if s.Reaped > 0 {
			// Worth a warning, not just a counter: a non-zero rate means
			// workers are dying or the heartbeat TTL is too tight, and this
			// is the only signal of either.
			w.log.Warn("jobs: returned stranded jobs to the queue", "reaped", s.Reaped, "pending", s.Pending)
		}
	}
}

// ReapOnce returns every running job whose heartbeat is older than the TTL to
// pending — not failed, because extraction resumes and the next pickup
// continues past the children already written — and samples the queue depth.
func (w *Worker) ReapOnce(ctx context.Context) (Sweep, error) {
	reaped, err := w.repo.ReapJobs(ctx, w.opts.HeartbeatTTL)
	if err != nil {
		return Sweep{}, err
	}
	pending, err := w.repo.CountPendingJobs(ctx)
	if err != nil {
		return Sweep{Reaped: reaped}, err
	}
	return Sweep{Reaped: reaped, Pending: pending}, nil
}
