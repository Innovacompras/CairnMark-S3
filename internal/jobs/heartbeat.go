package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// Progress and liveness for one running job: the extraction's hook on one
// side, the flush loop on the other, meeting in a few atomics.

// progress is the extraction's hook: publish where the run is, and tell it
// whether to stop. Cheap on purpose — an atomic store and load — since it
// runs after every entry.
func (r *running) progress(done, total int) bool {
	r.done.Store(int64(done))
	r.total.Store(int64(total))
	if done == 0 {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	}
	return r.stop.Load()
}

// heartbeat flushes progress + liveness and reads the cancel flag back, one
// round trip per interval, until the run ends. Losing the row stops the run;
// so does failing to reach the database for half the TTL, since past that
// the worker can no longer prove it is alive before a reaper may act.
func (r *running) heartbeat(ctx context.Context) {
	t := time.NewTicker(r.w.opts.FlushInterval)
	defer t.Stop()
	lastOK := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.kick:
		}
		cancel, err := r.flush(ctx)
		switch {
		case errors.Is(err, metadata.ErrJobNotFound):
			r.w.log.Warn("job: lost ownership, stopping after the current entry",
				"job", r.job.ID, "attempt", r.job.Attempt)
			r.lost.Store(true)
			r.stop.Store(true)
			return
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			if time.Since(lastOK) > r.w.opts.HeartbeatTTL/2 {
				r.w.log.Error("job: heartbeat failing, stopping before the job can be reaped from under us",
					"job", r.job.ID, "err", err)
				r.lost.Store(true)
				r.stop.Store(true)
				return
			}
			r.w.log.Warn("job: heartbeat failed", "job", r.job.ID, "err", err)
			continue
		}
		lastOK = time.Now()
		if cancel {
			r.stop.Store(true)
		}
	}
}

func (r *running) flush(ctx context.Context) (bool, error) {
	fctx, cancel := context.WithTimeout(ctx, r.w.opts.FlushInterval)
	defer cancel()
	return r.w.repo.HeartbeatJob(fctx, r.job.ID, r.job.Attempt, int(r.done.Load()), int(r.total.Load()))
}
