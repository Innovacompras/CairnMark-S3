package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/mettjs/cairnmark/internal/files"
)

func registerFiles(mux *http.ServeMux, svc *files.Service, logger *slog.Logger, presignTTL time.Duration, maxUpload int64) {
	h := &fileHandler{svc: svc, log: logger, presignTTL: presignTTL, maxUpload: maxUpload}
	mux.HandleFunc("POST /files", h.upload)
	mux.HandleFunc("GET /files", h.list)
	mux.HandleFunc("GET /files/{id}", h.download)
	mux.HandleFunc("GET /files/{id}/metadata", h.metadata)
	mux.HandleFunc("PATCH /files/{id}/metadata", h.patchMetadata)
	mux.HandleFunc("DELETE /files/{id}", h.delete)
	mux.HandleFunc("GET /files/{id}/archive", h.archiveEntries)
	mux.HandleFunc("POST /files/{id}/extract", h.extract)
	registerJobs(mux, h)
}

type fileHandler struct {
	svc        *files.Service
	log        *slog.Logger
	presignTTL time.Duration
	maxUpload  int64 // reject upload bodies larger than this; 0 disables the cap
}

// maxIdempotencyKeyLen bounds the client-supplied key (it is a primary key).
const maxIdempotencyKeyLen = 255

// upload stores the raw request body. Filename comes from ?filename= or a
// Content-Disposition header; content type from the Content-Type header (the
// service sniffs it when absent); size from Content-Length (-1 when chunked).
// An optional Idempotency-Key header makes a retried upload return the original
// result instead of creating a duplicate. Bodies over the configured size cap
// are rejected with 413 — up front when Content-Length declares the breach,
// mid-stream otherwise (chunked or lying clients).
func (h *fileHandler) upload(w http.ResponseWriter, r *http.Request) {
	tags, err := uploadTags(r)
	if err != nil {
		writeClientError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > maxIdempotencyKeyLen {
		writeClientError(w, http.StatusBadRequest, "Idempotency-Key too long")
		return
	}

	body := io.Reader(r.Body)
	var lim *limitReader
	if h.maxUpload > 0 {
		if r.ContentLength > h.maxUpload {
			h.writeTooLarge(w)
			return
		}
		lim = &limitReader{r: r.Body, remaining: h.maxUpload}
		body = lim
	}

	in := files.UploadInput{
		Filename:    uploadFilename(r),
		ContentType: r.Header.Get("Content-Type"),
		Size:        r.ContentLength,
		Tags:        tags,
		Body:        body,
	}

	var f *files.File
	var replayed bool
	if key == "" {
		f, err = h.svc.Upload(r.Context(), in)
	} else {
		f, replayed, err = h.svc.UploadIdempotent(r.Context(), key, in)
	}
	if err != nil {
		if lim != nil && lim.exceeded {
			// The interrupted store write may leave a partial object; GC reclaims
			// it like any other orphan.
			h.writeTooLarge(w)
			return
		}
		h.writeError(w, err)
		return
	}

	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/files/"+f.ID)
	writeJSON(w, http.StatusCreated, toResponse(f))
}

func (h *fileHandler) writeTooLarge(w http.ResponseWriter) {
	writeClientError(w, http.StatusRequestEntityTooLarge,
		fmt.Sprintf("upload exceeds the %d-byte limit", h.maxUpload))
}

// download serves bytes one of three ways:
//   - a Range header → 206 partial, streamed through this process (GetRange);
//   - ?download=stream → 200 full body, streamed and checksum-verified;
//   - otherwise (default) → 302 redirect to a presigned URL (transfer offload).
func (h *fileHandler) download(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch {
	case r.Header.Get("Range") != "":
		h.downloadRange(w, r, id)
	case r.URL.Query().Get("download") == "stream":
		h.downloadStream(w, r, id)
	default:
		h.downloadRedirect(w, r, id)
	}
}

func (h *fileHandler) downloadRedirect(w http.ResponseWriter, r *http.Request, id string) {
	_, url, err := h.svc.Presign(r.Context(), id, h.presignTTL)
	if err != nil {
		h.writeError(w, err)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

func (h *fileHandler) downloadStream(w http.ResponseWriter, r *http.Request, id string) {
	f, rc, err := h.svc.Open(r.Context(), id)
	if err != nil {
		h.writeError(w, err)
		return
	}
	defer rc.Close()

	setDownloadHeaders(w, f)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", f.SizeBytes))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are already sent; the body may be partially written. Log the
		// detection — checksum mismatch is the integrity signal we care about.
		if errors.Is(err, files.ErrChecksumMismatch) {
			h.log.Error("download integrity check failed", "id", id, "err", err)
		} else {
			h.log.Warn("download stream interrupted", "id", id, "err", err)
		}
	}
}

func (h *fileHandler) downloadRange(w http.ResponseWriter, r *http.Request, id string) {
	f, err := h.svc.Metadata(r.Context(), id)
	if err != nil {
		h.writeError(w, err)
		return
	}

	offset, length, outcome := parseSingleRange(r.Header.Get("Range"), f.SizeBytes)
	switch outcome {
	case rangeIgnore:
		// RFC 9110 §14.2: a malformed Range header (or a unit we don't
		// understand) is ignored, not rejected — serve the response this request
		// would get without the header.
		if r.URL.Query().Get("download") == "stream" {
			h.downloadStream(w, r, id)
		} else {
			h.downloadRedirect(w, r, id)
		}
		return
	case rangeUnsatisfiable:
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", f.SizeBytes))
		writeClientError(w, http.StatusRequestedRangeNotSatisfiable, "unsatisfiable range")
		return
	}

	rc, err := h.svc.OpenRange(r.Context(), f, offset, length)
	if err != nil {
		h.writeError(w, err)
		return
	}
	defer rc.Close()

	setDownloadHeaders(w, f)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, f.SizeBytes))
	w.WriteHeader(http.StatusPartialContent)
	if _, err := io.Copy(w, rc); err != nil {
		h.log.Warn("range stream interrupted", "id", id, "err", err)
	}
}

func (h *fileHandler) metadata(w http.ResponseWriter, r *http.Request) {
	f, err := h.svc.Metadata(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(f))
}

func (h *fileHandler) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.Context(), r.PathValue("id")); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setDownloadHeaders writes the common content headers for a body response.
func setDownloadHeaders(w http.ResponseWriter, f *files.File) {
	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": f.Filename}))
	if f.ChecksumSHA256 != "" {
		w.Header().Set("X-Checksum-Sha256", f.ChecksumSHA256)
	}
}

// uploadFilename resolves the user-facing name: ?filename= wins, then a
// Content-Disposition filename, else empty (the service defaults it to the id).
func uploadFilename(r *http.Request) string {
	if q := r.URL.Query().Get("filename"); q != "" {
		return q
	}
	if cd := r.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			return params["filename"]
		}
	}
	return ""
}
