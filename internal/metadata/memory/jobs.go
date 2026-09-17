package memory

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// Extraction jobs — submission, lookup, cancellation and claiming — mirroring
// jobs.go of the pgx implementation: the one-active-per-archive invariant and
// oldest-first claiming. The mutex stands in for row locks. The worker-side
// writes are in jobs_run.go.

func (r *Repo) CreateJob(ctx context.Context, j *metadata.Job) (bool, string, error) {
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.files[j.ArchiveID]; !ok {
		return false, "", metadata.ErrNotFound // the foreign key
	}
	for _, other := range r.jobs {
		if other.ArchiveID == j.ArchiveID && !other.Status.Terminal() {
			return false, other.ID, nil // the unique index
		}
	}
	now := time.Now()
	j.Status = metadata.JobPending
	j.CreatedAt, j.UpdatedAt = now, now
	r.jobs[j.ID] = cloneJob(j)
	return true, "", nil
}

func (r *Repo) GetJob(ctx context.Context, id string) (*metadata.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return nil, metadata.ErrJobNotFound
	}
	return cloneJob(j), nil
}

func (r *Repo) RequestCancel(ctx context.Context, id string) (*metadata.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return nil, metadata.ErrJobNotFound
	}
	now := time.Now()
	switch j.Status {
	case metadata.JobPending:
		j.Status = metadata.JobCancelled
		j.CancelRequested = true
		j.FinishedAt = &now
		j.UpdatedAt = now
	case metadata.JobRunning:
		j.CancelRequested = true
		j.UpdatedAt = now
	}
	return cloneJob(j), nil
}

func (r *Repo) ClaimJob(ctx context.Context) (*metadata.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var pending []*metadata.Job
	for _, j := range r.jobs {
		if j.Status == metadata.JobPending {
			pending = append(pending, j)
		}
	}
	if len(pending) == 0 {
		return nil, nil
	}
	sort.Slice(pending, func(a, b int) bool {
		if !pending[a].CreatedAt.Equal(pending[b].CreatedAt) {
			return pending[a].CreatedAt.Before(pending[b].CreatedAt)
		}
		return pending[a].ID < pending[b].ID // ids are UUIDv7, so this is creation order too
	})
	j := pending[0]
	now := time.Now()
	j.Status = metadata.JobRunning
	j.Attempt++
	j.HeartbeatAt = &now
	j.UpdatedAt = now
	return cloneJob(j), nil
}

// cloneJob returns a copy sharing no mutable state with the store, keeping a
// nil Selection nil and an empty one empty.
func cloneJob(j *metadata.Job) *metadata.Job {
	cp := *j
	cp.Selection = slices.Clone(j.Selection)
	cp.Summary = slices.Clone(j.Summary)
	if j.HeartbeatAt != nil {
		t := *j.HeartbeatAt
		cp.HeartbeatAt = &t
	}
	if j.FinishedAt != nil {
		t := *j.FinishedAt
		cp.FinishedAt = &t
	}
	return &cp
}
