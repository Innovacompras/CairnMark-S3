package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// archiveEntryResponse is one entry of GET /files/{id}/archive.
type archiveEntryResponse struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type,omitempty"`
	CRC32       string `json:"crc32"`
	Selectable  bool   `json:"selectable"`
	Reason      string `json:"reason,omitempty"`
}

type archiveResponse struct {
	ArchiveID string                 `json:"archive_id"`
	Entries   []archiveEntryResponse `json:"entries"`
}

// archiveEntries lists an archive's entries without writing anything, so a
// caller can decide which to extract — by index, since names need not be
// unique — before anything is stored. The index is stable: the archive is an
// immutable object, so an index handed out here is still valid at extract time.
func (h *fileHandler) archiveEntries(w http.ResponseWriter, r *http.Request) {
	f, entries, err := h.svc.ArchiveEntries(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	resp := archiveResponse{ArchiveID: f.ID, Entries: make([]archiveEntryResponse, 0, len(entries))}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, archiveEntryResponse{
			Index: e.Index, Name: e.Name, Size: e.Size, ContentType: e.ContentType,
			CRC32: fmt.Sprintf("%08x", e.CRC32), Selectable: e.Selectable(), Reason: string(e.Skip),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// extractRequest is the optional body of POST /files/{id}/extract. Entries
// selects by directory index; an absent body, or one without the field, means
// every selectable entry.
//
// errNullBody and errTrailingBody are the two malformed bodies encoding/json
// accepts in silence: `null` unmarshals into the struct as a no-op, and the
// decoder reads one value and ignores whatever follows it. Both would arrive as
// "no selection given" and extract the whole archive — the opposite of what
// such a body asks for.
type extractRequest struct {
	Entries []int `json:"entries"`
}

var (
	errNullBody     = errors.New("body must be a JSON object, not null")
	errTrailingBody = errors.New("body must be a single JSON object")
)

// extract submits an extraction job and answers 202 with the job, whose
// Location is where to poll. The run itself happens on the worker: an
// extraction moves the whole archive through the service, which is minutes,
// not seconds, and a reverse proxy's idle timeout is not the caller's to
// configure. What can be refused now is refused now, with the same statuses
// a synchronous run gave — not a zip (415), a selection out of range (400),
// over a cap (413), unknown (404) — so a job only ever fails on what could not
// be known up front.
//
// Idempotency-Key is refused: the job id already is the idempotency handle,
// and a second mechanism for the same guarantee is the kind of surface this
// project avoids. Retry safety comes from the run itself, which resumes.
func (h *fileHandler) extract(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Idempotency-Key") != "" {
		writeClientError(w, http.StatusBadRequest,
			"Idempotency-Key is not supported on extraction; the job id is the handle, and a re-run resumes")
		return
	}
	selected, err := decodeSelection(w, r)
	if err != nil {
		if tooLarge, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeClientError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds the %d-byte limit", tooLarge.Limit))
			return
		}
		if errors.Is(err, errNullBody) || errors.Is(err, errTrailingBody) {
			writeClientError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeClientError(w, http.StatusBadRequest, "body must be a JSON object: "+err.Error())
		return
	}

	job, err := h.svc.SubmitExtraction(r.Context(), r.PathValue("id"), selected)
	if err != nil {
		h.writeError(w, err)
		return
	}
	w.Header().Set("Location", jobPath(job.ID))
	writeJSON(w, http.StatusAccepted, toJobResponse(job))
}

// decodeSelection reads the optional selection. No body, or a body without
// entries, means all; an explicit empty list means none. Decoded through a
// pointer so a literal `null` stays distinguishable from an absent body, which
// a struct target would silently conflate.
func decodeSelection(w http.ResponseWriter, r *http.Request) ([]int, error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetadataBodyBytes))
	var req *extractRequest
	err := dec.Decode(&req)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errNullBody
	}
	if dec.More() {
		return nil, errTrailingBody
	}
	return req.Entries, nil
}
