package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrJobNotFound is returned when no extraction job matches — or, from the
// worker-side writes, when the row is no longer owned by the caller's attempt.
var ErrJobNotFound = errors.New("metadata: extraction job not found")

// JobStatus is the lifecycle state of an extraction job:
// pending → running → succeeded | failed | cancelled. A running job goes back
// to pending when its worker parks it on shutdown or a reaper takes it from a
// worker that stopped reporting.
type JobStatus string

// Job lifecycle states.
const (
	JobPending   JobStatus = "pending"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

// Terminal reports whether the job will not change state again.
func (s JobStatus) Terminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled
}

// Job is one extraction job. It mirrors the extraction_jobs table.
type Job struct {
	ID        string
	ArchiveID string
	Status    JobStatus
	// Selection is the caller's entry indexes. nil means every selectable
	// entry; an empty, non-nil slice selects nothing. The distinction is
	// preserved through storage (null vs []).
	Selection       []int
	CancelRequested bool
	// Attempt counts claims. It is the fencing token on every worker write.
	Attempt int
	// Progress of the current run: Done entries processed of Total intended.
	// A job resumed after an interruption counts only what remained.
	ProgressDone  int
	ProgressTotal int
	// Summary is the extraction summary as JSON, once terminal. nil when the
	// run never produced one (cancelled before it started, or failed before
	// the archive could be opened).
	Summary json.RawMessage
	// Error is the reason when Status is JobFailed.
	Error       string
	HeartbeatAt *time.Time // set while running; nil otherwise
	CreatedAt   time.Time
	UpdatedAt   time.Time
	FinishedAt  *time.Time // set once terminal
}

// JobResult is what a worker records when a run ends. It carries the final
// progress itself: a short run can end before its first heartbeat has landed,
// and the terminal row must still read done-of-total.
type JobResult struct {
	Status  JobStatus       // JobSucceeded, JobFailed or JobCancelled
	Done    int             // entries processed
	Total   int             // entries the run intended to write
	Summary json.RawMessage // nil when there is none
	Error   string          // the reason, for JobFailed
}

// JobRepository persists extraction jobs. It is a sibling of Repository — the
// pgx and in-memory implementations satisfy both — kept separate because jobs
// are a distinct bounded concern and Repository is already ten methods.
type JobRepository interface {
	// CreateJob inserts job as pending. created=false with activeID set means
	// another job for the same archive is still pending or running — the
	// unique index refused the insert. activeID may be empty if that job
	// finished between the conflict and the lookup; the caller simply retries.
	// ErrNotFound when the archive row no longer exists.
	CreateJob(ctx context.Context, job *Job) (created bool, activeID string, err error)

	// GetJob returns the job by id, or ErrJobNotFound.
	GetJob(ctx context.Context, id string) (*Job, error)

	// RequestCancel asks a job to stop. A pending job becomes cancelled at
	// once; a running one has its flag set for the worker to honour after
	// the current entry; a terminal one is left as it is. Returns the job's
	// state after the request, or ErrJobNotFound.
	RequestCancel(ctx context.Context, id string) (*Job, error)

	// ClaimJob moves the oldest pending job to running for this caller,
	// bumping Attempt, and returns it — or nil, nil when nothing is pending.
	// Safe across replicas: concurrent callers never claim the same row.
	ClaimJob(ctx context.Context) (*Job, error)

	// HeartbeatJob records progress and liveness for a running job in one
	// round trip and reports whether cancellation has been requested.
	// ErrJobNotFound when the row is not running under this attempt — the
	// caller has lost ownership and must stop.
	HeartbeatJob(ctx context.Context, id string, attempt, done, total int) (cancelRequested bool, err error)

	// FinishJob records a terminal result for a running job. ErrJobNotFound
	// when the row is not running under this attempt.
	FinishJob(ctx context.Context, id string, attempt int, result JobResult) error

	// ParkJob returns a running job to pending — a worker shutting down —
	// keeping its progress. ErrJobNotFound when the row is not running under
	// this attempt.
	ParkJob(ctx context.Context, id string, attempt, done, total int) error

	// ReapJobs returns running jobs that have gone longer than ttl without a
	// heartbeat to pending, so the next pickup resumes them, and reports how
	// many. Judged on the repository's own clock, which also stamps the
	// heartbeat: this is the one comparison where a skew between processes
	// would hand a live run to a second worker.
	ReapJobs(ctx context.Context, ttl time.Duration) (int, error)

	// CountPendingJobs is the queue depth.
	CountPendingJobs(ctx context.Context) (int, error)

	// PurgeFinishedJobs deletes terminal jobs that finished before the cutoff
	// (retention), returning how many. Called by GC.
	PurgeFinishedJobs(ctx context.Context, before time.Time) (int, error)
}
