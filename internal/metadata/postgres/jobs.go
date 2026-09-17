package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// Extraction jobs: submission, lookup, cancellation and claiming. The
// worker-side writes (heartbeat, finish, park) and the sweeps are in
// jobs_run.go.

// jobColumns is the column list and order scanJob expects.
const jobColumns = `id, archive_id, status, selection, cancel_requested, attempt,
	progress_done, progress_total, summary, coalesce(error, ''), heartbeat_at,
	created_at, updated_at, finished_at`

const selectJob = "select " + jobColumns + " from extraction_jobs"

// foreignKeyViolation is SQLSTATE 23503: the archive row the job references
// is gone.
const foreignKeyViolation = "23503"

// activeStatuses is the predicate of the one-active-per-archive index, which
// an ON CONFLICT clause must repeat verbatim to infer the index.
const activeStatuses = `status in ('pending', 'running')`

func scanJob(r row) (*metadata.Job, error) {
	var j metadata.Job
	var status string
	var selection, summary []byte
	if err := r.Scan(
		&j.ID, &j.ArchiveID, &status, &selection, &j.CancelRequested, &j.Attempt,
		&j.ProgressDone, &j.ProgressTotal, &summary, &j.Error, &j.HeartbeatAt,
		&j.CreatedAt, &j.UpdatedAt, &j.FinishedAt,
	); err != nil {
		return nil, err
	}
	j.Status = metadata.JobStatus(status)
	// A null selection stays nil (every entry); "[]" decodes to an empty,
	// non-nil slice (nothing) — the distinction the caller relies on.
	if selection != nil {
		if err := json.Unmarshal(selection, &j.Selection); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal job selection: %w", err)
		}
	}
	if summary != nil {
		j.Summary = json.RawMessage(summary)
	}
	return &j, nil
}

// jsonOrNull renders b for a jsonb parameter, keeping a nil slice as SQL null.
func jsonOrNull(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

// CreateJob inserts the job as pending. The partial unique index on
// (archive_id) where the status is active refuses a second live job for one
// archive; ON CONFLICT DO NOTHING turns that into "no row", and the active
// job's id is read back so the caller can point at it.
func (r *Repo) CreateJob(ctx context.Context, j *metadata.Job) (bool, string, error) {
	var selection any // nil → null → every selectable entry
	if j.Selection != nil {
		b, err := json.Marshal(j.Selection)
		if err != nil {
			return false, "", fmt.Errorf("postgres: marshal job selection: %w", err)
		}
		selection = string(b)
	}
	const ins = `insert into extraction_jobs (id, archive_id, status, selection)
		values ($1, $2, $3, $4::jsonb)
		on conflict (archive_id) where ` + activeStatuses + ` do nothing
		returning created_at, updated_at`
	err := r.pool.QueryRow(ctx, ins, j.ID, j.ArchiveID, string(metadata.JobPending), selection).
		Scan(&j.CreatedAt, &j.UpdatedAt)
	if err == nil {
		j.Status = metadata.JobPending
		return true, "", nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return false, "", metadata.ErrNotFound // the archive was purged under the caller
		}
		return false, "", fmt.Errorf("postgres: create job: %w", err)
	}

	var active string
	err = r.pool.QueryRow(ctx,
		`select id from extraction_jobs where archive_id = $1 and `+activeStatuses, j.ArchiveID,
	).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil // finished between the conflict and the read
	}
	if err != nil {
		return false, "", fmt.Errorf("postgres: read active job: %w", err)
	}
	return false, active, nil
}

// GetJob returns the job by id, or metadata.ErrJobNotFound.
func (r *Repo) GetJob(ctx context.Context, id string) (*metadata.Job, error) {
	j, err := scanJob(r.pool.QueryRow(ctx, selectJob+` where id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, metadata.ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get job: %w", err)
	}
	return j, nil
}

// RequestCancel flags a job. Every column reference on the right-hand side
// of a SET sees the row's old value, so one statement cancels a pending job
// outright, flags a running one, and leaves a terminal one untouched.
func (r *Repo) RequestCancel(ctx context.Context, id string) (*metadata.Job, error) {
	const q = `update extraction_jobs set
		cancel_requested = cancel_requested or ` + activeStatuses + `,
		status = case when status = 'pending' then 'cancelled' else status end,
		finished_at = case when status = 'pending' then now() else finished_at end,
		updated_at = case when ` + activeStatuses + ` then now() else updated_at end
		where id = $1 returning ` + jobColumns
	j, err := scanJob(r.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, metadata.ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: request cancel: %w", err)
	}
	return j, nil
}

// ClaimJob takes the oldest pending job. SKIP LOCKED is what makes N
// replicas safe with no coordination: a row another claim holds locked is
// passed over rather than waited on, so two workers never claim one job.
func (r *Repo) ClaimJob(ctx context.Context) (*metadata.Job, error) {
	const q = `update extraction_jobs set status = 'running', attempt = attempt + 1,
		heartbeat_at = now(), updated_at = now()
		where id = (
			select id from extraction_jobs where status = 'pending'
			order by created_at for update skip locked limit 1
		)
		returning ` + jobColumns
	j, err := scanJob(r.pool.QueryRow(ctx, q))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: claim job: %w", err)
	}
	return j, nil
}
