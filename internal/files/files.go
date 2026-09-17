// Package files is the service / use-case layer. It owns the write path and the
// consistency logic between storage and metadata; layers above it (api) talk
// only to this package and never reach storage or metadata directly.
package files

import (
	"errors"

	"github.com/mettjs/cairnmark/internal/metadata"
	"github.com/mettjs/cairnmark/internal/storage"
)

// ErrNotFound is returned when no live file matches. It is the api layer's only
// signal for a 404, keeping storage/metadata sentinels below this seam.
var ErrNotFound = errors.New("files: not found")

// ErrJobNotFound is returned when no extraction job matches — including one
// the retention sweep has already purged. The api layer maps it to 404.
var ErrJobNotFound = errors.New("files: extraction job not found")

// ErrInvalidID is returned when an id is not a well-formed file identifier. The
// api layer maps it to 400 — a malformed id is a client error, not a 500.
var ErrInvalidID = errors.New("files: invalid id")

// ErrChecksumMismatch is surfaced by a verifying download reader when the
// stored bytes no longer hash to the recorded checksum (Phase 2 integrity).
var ErrChecksumMismatch = errors.New("files: checksum mismatch")

// ErrIdempotencyConflict means an upload with the same Idempotency-Key is still
// in progress. The api layer maps it to 409 — the client should retry shortly.
var ErrIdempotencyConflict = errors.New("files: idempotency key conflict")

// ErrIdempotencyResultGone means the file an Idempotency-Key produced has since
// been deleted. Retrying under the same key can never succeed until the key
// expires, so the api layer maps it to 410 — the client must pick a new key.
var ErrIdempotencyResultGone = errors.New("files: idempotent result deleted")

// ErrNotArchive means the file the archive endpoints were pointed at is not a
// supported archive. The api layer maps it to 415 — the one status a caller
// will hit by accident, so it is named rather than left to a generic 400.
var ErrNotArchive = errors.New("files: not a supported archive (zip)")

// ErrExtractInProgress means another extraction job for the same archive is
// still pending or running. The api layer maps it to 409; the value returned
// is an *ExtractInProgressError naming that job, which is the one to poll.
var ErrExtractInProgress = errors.New("files: an extraction of this archive is already in progress")

// ErrExtractCancelled is returned by Extract when its progress hook asked it
// to stop. The partial summary is returned alongside it: the children written
// so far remain, and a later run resumes past them.
var ErrExtractCancelled = errors.New("files: extraction cancelled")

// ErrArchiveTooLarge means the archive, or the selection to extract from it,
// exceeds a configured cap. The api layer maps it to 413; the wrapped message
// names the cap and the numbers.
var ErrArchiveTooLarge = errors.New("files: archive exceeds the extraction limits")

// ErrInvalidSelection means a requested entry index is outside the archive's
// directory. The api layer maps it to 400.
var ErrInvalidSelection = errors.New("files: entry selection out of range")

// File is the service-level view of a stored file, returned to the api layer.
type File = metadata.File

// Job is the service-level view of an extraction job.
type Job = metadata.Job

// JobStatus and its values, re-exported for the api layer.
type JobStatus = metadata.JobStatus

// Job lifecycle states.
const (
	JobPending   = metadata.JobPending
	JobRunning   = metadata.JobRunning
	JobSucceeded = metadata.JobSucceeded
	JobFailed    = metadata.JobFailed
	JobCancelled = metadata.JobCancelled
)

// ListFilter narrows a List query. Aliased so the api layer can build filters
// without importing the metadata package directly.
type ListFilter = metadata.ListFilter

// EntryScope and its values select extracted archive entries in a List.
type EntryScope = metadata.EntryScope

// List scopes, re-exported for the api layer.
const (
	EntriesInclude = metadata.EntriesInclude
	EntriesExclude = metadata.EntriesExclude
	EntriesOnly    = metadata.EntriesOnly
)

// Repository is everything the service persists through: file records and
// extraction jobs. The two are separate seams in the metadata package; one
// pgx repository — and one in-memory double — satisfies both.
type Repository interface {
	metadata.Repository
	metadata.JobRepository
}

// Service orchestrates the object store and the metadata repository.
type Service struct {
	backend storage.Backend
	repo    Repository
	limits  ArchiveLimits
}

// New wires the service to its collaborators.
func New(backend storage.Backend, repo Repository, opts ...Option) *Service {
	s := &Service{backend: backend, repo: repo}
	for _, o := range opts {
		o(s)
	}
	return s
}

func translateNotFound(err error) error {
	if errors.Is(err, metadata.ErrNotFound) || errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
