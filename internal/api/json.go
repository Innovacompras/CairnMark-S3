package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/mettjs/cairnmark/internal/files"
)

// retryAfterSeconds is the Retry-After hint sent with a 409 — long enough for
// most in-flight uploads to finish. For an extraction conflict the body also
// names the active job, which is the better thing to wait on.
const retryAfterSeconds = 30

// fileResponse is the public JSON shape of a file record. The internal storage
// key is deliberately omitted — callers address files by id only.
type fileResponse struct {
	ID          string         `json:"id"`
	Filename    string         `json:"filename"`
	ContentType string         `json:"content_type"`
	SizeBytes   int64          `json:"size_bytes"`
	Checksum    string         `json:"checksum_sha256,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   *time.Time     `json:"updated_at"` // null until first metadata update
}

func toResponse(f *files.File) fileResponse {
	return fileResponse{
		ID:          f.ID,
		Filename:    f.Filename,
		ContentType: f.ContentType,
		SizeBytes:   f.SizeBytes,
		Checksum:    f.ChecksumSHA256,
		Metadata:    f.Metadata,
		CreatedAt:   f.CreatedAt,
		UpdatedAt:   f.UpdatedAt,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError maps a service error to an HTTP status and JSON body. The known
// client-facing sentinels carry their (safe) message through; any other error
// is a 500 whose detail is logged server-side, never leaked to the client.
func (h *fileHandler) writeError(w http.ResponseWriter, err error) {
	var inProgress *files.ExtractInProgressError
	switch {
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrJobNotFound):
		writeJSON(w, http.StatusNotFound, errorBody(err.Error()))
	case errors.Is(err, files.ErrInvalidID), errors.Is(err, files.ErrInvalidSelection):
		writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
	case errors.As(err, &inProgress):
		// Another job holds the archive. Name it: the caller's right move is
		// to poll that job, which a bare 409 could not tell them. Retry-After
		// stays for a client that would rather resubmit.
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		body := errorBody(err.Error())
		if inProgress.JobID != "" {
			body["job_id"] = inProgress.JobID
		}
		writeJSON(w, http.StatusConflict, body)
	case errors.Is(err, files.ErrIdempotencyConflict):
		// The conflicting upload is still in flight; tell the client when to
		// ask again rather than leaving the backoff to guesswork.
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		writeJSON(w, http.StatusConflict, errorBody(err.Error()))
	case errors.Is(err, files.ErrIdempotencyResultGone):
		// Retrying under this key can never succeed — no Retry-After; the client
		// must switch to a fresh key.
		writeJSON(w, http.StatusGone,
			errorBody("the file created under this Idempotency-Key was deleted; use a new key"))
	case errors.Is(err, files.ErrNotArchive):
		// Named rather than a generic 400: pointing an archive endpoint at an
		// ordinary file is the one mistake a caller will make by accident.
		writeJSON(w, http.StatusUnsupportedMediaType, errorBody(err.Error()))
	case errors.Is(err, files.ErrArchiveTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody(err.Error()))
	default:
		h.log.Error("request failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("internal server error"))
	}
}

func writeClientError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody(msg))
}

func errorBody(msg string) map[string]string { return map[string]string{"error": msg} }
