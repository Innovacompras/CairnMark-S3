package memory

import (
	"context"
	"slices"
	"time"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// The worker side of extraction jobs, mirroring jobs_run.go of the pgx
// implementation: every write is fenced by the attempt handed out at claim
// time, and the reap, count and retention sweeps.

// owned returns the row if it is running under attempt, else nil — the
// fencing check every worker write goes through.
func (r *Repo) owned(id string, attempt int) *metadata.Job {
	j, ok := r.jobs[id]
	if !ok || j.Status != metadata.JobRunning || j.Attempt != attempt {
		return nil
	}
	return j
}

func (r *Repo) HeartbeatJob(ctx context.Context, id string, attempt, done, total int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.owned(id, attempt)
	if j == nil {
		return false, metadata.ErrJobNotFound
	}
	now := time.Now()
	j.ProgressDone, j.ProgressTotal = done, total
	j.HeartbeatAt = &now
	j.UpdatedAt = now
	return j.CancelRequested, nil
}

func (r *Repo) FinishJob(ctx context.Context, id string, attempt int, res metadata.JobResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.owned(id, attempt)
	if j == nil {
		return metadata.ErrJobNotFound
	}
	now := time.Now()
	j.Status = res.Status
	j.ProgressDone, j.ProgressTotal = res.Done, res.Total
	j.Summary = slices.Clone(res.Summary)
	j.Error = res.Error
	j.HeartbeatAt = nil
	j.FinishedAt = &now
	j.UpdatedAt = now
	return nil
}

func (r *Repo) ParkJob(ctx context.Context, id string, attempt, done, total int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.owned(id, attempt)
	if j == nil {
		return metadata.ErrJobNotFound
	}
	j.Status = metadata.JobPending
	j.ProgressDone, j.ProgressTotal = done, total
	j.HeartbeatAt = nil
	j.UpdatedAt = time.Now()
	return nil
}

func (r *Repo) ReapJobs(_ context.Context, ttl time.Duration) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-ttl)
	n := 0
	for _, j := range r.jobs {
		if j.Status == metadata.JobRunning && j.HeartbeatAt != nil && j.HeartbeatAt.Before(cutoff) {
			j.Status = metadata.JobPending
			j.HeartbeatAt = nil
			j.UpdatedAt = time.Now()
			n++
		}
	}
	return n, nil
}

func (r *Repo) CountPendingJobs(context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, j := range r.jobs {
		if j.Status == metadata.JobPending {
			n++
		}
	}
	return n, nil
}

func (r *Repo) PurgeFinishedJobs(_ context.Context, before time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id, j := range r.jobs {
		if j.FinishedAt != nil && j.FinishedAt.Before(before) {
			delete(r.jobs, id)
			n++
		}
	}
	return n, nil
}
