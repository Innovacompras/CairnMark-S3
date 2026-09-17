package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// The worker side of extraction jobs. Every write here is fenced by the
// attempt the worker was handed at claim time: a worker that stalled, was
// reaped, and woke up affects zero rows and learns it lost the job.

// HeartbeatJob records progress and liveness together and reads the cancel
// flag back, so the three cost one round trip.
func (r *Repo) HeartbeatJob(ctx context.Context, id string, attempt, done, total int) (bool, error) {
	const q = `update extraction_jobs
		set progress_done = $3, progress_total = $4, heartbeat_at = now(), updated_at = now()
		where id = $1 and attempt = $2 and status = 'running'
		returning cancel_requested`
	var cancel bool
	err := r.pool.QueryRow(ctx, q, id, attempt, done, total).Scan(&cancel)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, metadata.ErrJobNotFound
		}
		return false, fmt.Errorf("postgres: heartbeat job: %w", err)
	}
	return cancel, nil
}

// FinishJob records the terminal result.
func (r *Repo) FinishJob(ctx context.Context, id string, attempt int, res metadata.JobResult) error {
	const q = `update extraction_jobs
		set status = $3, progress_done = $4, progress_total = $5, summary = $6::jsonb,
		    error = nullif($7, ''), heartbeat_at = null, finished_at = now(), updated_at = now()
		where id = $1 and attempt = $2 and status = 'running'`
	tag, err := r.pool.Exec(ctx, q, id, attempt, string(res.Status), res.Done, res.Total, jsonOrNull(res.Summary), res.Error)
	if err != nil {
		return fmt.Errorf("postgres: finish job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return metadata.ErrJobNotFound
	}
	return nil
}

// ParkJob returns a running job to pending with its progress kept, for a
// worker that is shutting down. The next claim resumes it.
func (r *Repo) ParkJob(ctx context.Context, id string, attempt, done, total int) error {
	const q = `update extraction_jobs
		set status = 'pending', progress_done = $3, progress_total = $4, heartbeat_at = null, updated_at = now()
		where id = $1 and attempt = $2 and status = 'running'`
	tag, err := r.pool.Exec(ctx, q, id, attempt, done, total)
	if err != nil {
		return fmt.Errorf("postgres: park job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return metadata.ErrJobNotFound
	}
	return nil
}

// ReapJobs returns stranded running rows — no heartbeat within ttl — to
// pending. Not to failed: extraction resumes, so the next pickup continues
// past the children already written. The cutoff is now() minus ttl on the
// database's clock, the clock that stamped the heartbeat, so no skew between
// this process and the database can make a live run look dead.
func (r *Repo) ReapJobs(ctx context.Context, ttl time.Duration) (int, error) {
	tag, err := r.pool.Exec(ctx, `update extraction_jobs
		set status = 'pending', heartbeat_at = null, updated_at = now()
		where status = 'running' and heartbeat_at < now() - $1::interval`, ttl)
	if err != nil {
		return 0, fmt.Errorf("postgres: reap jobs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// CountPendingJobs is the queue depth, served by the pickup index.
func (r *Repo) CountPendingJobs(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx,
		`select count(*) from extraction_jobs where status = 'pending'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgres: count pending jobs: %w", err)
	}
	return n, nil
}

// PurgeFinishedJobs deletes terminal rows past retention, returning the count.
func (r *Repo) PurgeFinishedJobs(ctx context.Context, before time.Time) (int, error) {
	tag, err := r.pool.Exec(ctx, `delete from extraction_jobs where finished_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("postgres: purge finished jobs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
