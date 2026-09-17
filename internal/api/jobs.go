package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/mettjs/cairnmark/internal/files"
)

// A job id is a bearer capability, exactly as a file id is: whoever holds it
// can read and cancel the job. There is deliberately no listing — a job that
// knew who made it would be identity arriving through the back door.

func registerJobs(mux *http.ServeMux, h *fileHandler) {
	mux.HandleFunc("GET /jobs/{id}", h.job)
	mux.HandleFunc("POST /jobs/{id}/cancel", h.cancelJob)
}

func jobPath(id string) string { return "/jobs/" + id }

// jobResponse is the public JSON shape of an extraction job.
type jobResponse struct {
	ID              string          `json:"id"`
	ArchiveID       string          `json:"archive_id"`
	Status          files.JobStatus `json:"status"`
	Progress        progress        `json:"progress"`
	CancelRequested bool            `json:"cancel_requested"`
	// Summary is the extraction summary — the same shape a synchronous run
	// answered with — once the job is terminal; null until then, and null on
	// a job that never ran (cancelled while pending) or failed before the
	// archive could be opened.
	Summary json.RawMessage `json:"summary"`
	// Error is the reason when status is failed.
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at"` // null until terminal
}

// progress is where the current run is: Done entries processed of the Total
// it intends to write. A job resumed after an interruption counts only what
// remained, and Total is not the archive's entry count — reporting the larger
// number would read as a stall.
type progress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

func toJobResponse(j *files.Job) jobResponse {
	summary := j.Summary
	if summary == nil {
		summary = json.RawMessage("null")
	}
	return jobResponse{
		ID:              j.ID,
		ArchiveID:       j.ArchiveID,
		Status:          j.Status,
		Progress:        progress{Done: j.ProgressDone, Total: j.ProgressTotal},
		CancelRequested: j.CancelRequested,
		Summary:         summary,
		Error:           j.Error,
		CreatedAt:       j.CreatedAt,
		UpdatedAt:       j.UpdatedAt,
		FinishedAt:      j.FinishedAt,
	}
}

// job reports a job's state. A job past retention has been purged and is a
// 404: a client that polls slower than CAIRNMARK_JOB_RETENTION loses the
// result, though the extracted children themselves are permanent.
func (h *fileHandler) job(w http.ResponseWriter, r *http.Request) {
	j, err := h.svc.ExtractionJob(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toJobResponse(j))
}

// cancelJob asks a job to stop. Cooperative: a running job finishes the
// entry it is on, keeps the children written so far, and reports cancelled
// with the partial summary — resubmitting later resumes past them. Idempotent
// and 202 whatever the state; on a terminal job it is a no-op reporting the
// terminal state rather than an error.
func (h *fileHandler) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, err := h.svc.CancelExtraction(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toJobResponse(j))
}
