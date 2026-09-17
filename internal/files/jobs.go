package files

import (
	"context"
	"errors"
	"fmt"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// ExtractInProgressError is ErrExtractInProgress carrying the id of the job
// that holds the archive, so the api layer can tell the caller which job to
// poll rather than only when to try again.
type ExtractInProgressError struct {
	JobID string // may be empty if that job finished between the refusal and the lookup
}

func (e *ExtractInProgressError) Error() string { return ErrExtractInProgress.Error() }

// Unwrap makes errors.Is(err, ErrExtractInProgress) true.
func (e *ExtractInProgressError) Unwrap() error { return ErrExtractInProgress }

// SubmitExtraction enqueues an extraction of the archive id and returns the
// pending job. The run is validated first, exactly as a synchronous
// extraction would — the archive opens, the selection is in range, the total
// is under the cap — so every refusal a caller could act on is made now, with
// the same errors, rather than surfacing later as a failed job. A job for an
// archive that already has one pending or running is refused with an
// *ExtractInProgressError naming it: one active extraction per archive is a
// database invariant, and the caller's next move is to poll that job.
func (s *Service) SubmitExtraction(ctx context.Context, id string, selected []int) (*Job, error) {
	if !validID(id) {
		return nil, ErrInvalidID
	}
	if _, _, _, err := s.planExtraction(ctx, id, selected); err != nil {
		return nil, err
	}
	jobID, err := newID()
	if err != nil {
		return nil, fmt.Errorf("files: generate job id: %w", err)
	}
	job := &metadata.Job{ID: jobID, ArchiveID: id, Selection: selected}
	// Two inserts, not one: the first is refused only while another job is
	// active, and the lookup then names it — unless that job finished in
	// between, in which case the slot is free and one more insert takes it.
	// Without the second try the caller would get a 409 naming nobody, and
	// nothing to do about it but sleep out Retry-After on a free archive.
	for try := 0; ; try++ {
		created, active, err := s.repo.CreateJob(ctx, job)
		if err != nil {
			if errors.Is(err, metadata.ErrNotFound) {
				return nil, ErrNotFound // the archive was purged under us
			}
			return nil, fmt.Errorf("files: create extraction job: %w", err)
		}
		if created {
			return job, nil
		}
		if active != "" || try == 1 {
			return nil, &ExtractInProgressError{JobID: active}
		}
	}
}

// ExtractionJob returns the job by id, or ErrJobNotFound — which a job past
// retention also answers, since the sweep deletes its row.
func (s *Service) ExtractionJob(ctx context.Context, jobID string) (*Job, error) {
	if !validID(jobID) {
		return nil, ErrInvalidID
	}
	job, err := s.repo.GetJob(ctx, jobID)
	if err != nil {
		return nil, translateJobNotFound(err)
	}
	return job, nil
}

// CancelExtraction asks the job to stop and returns its state after the
// request. A pending job is cancelled at once; a running one stops after the
// entry it is writing, keeping the children written so far; a terminal one
// is unchanged — cancelling is idempotent and never an error on its own.
func (s *Service) CancelExtraction(ctx context.Context, jobID string) (*Job, error) {
	if !validID(jobID) {
		return nil, ErrInvalidID
	}
	job, err := s.repo.RequestCancel(ctx, jobID)
	if err != nil {
		return nil, translateJobNotFound(err)
	}
	return job, nil
}

func translateJobNotFound(err error) error {
	if errors.Is(err, metadata.ErrJobNotFound) {
		return ErrJobNotFound
	}
	return err
}
