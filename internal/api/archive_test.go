package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mettjs/cairnmark/internal/files"
	"github.com/mettjs/cairnmark/internal/jobs"
	metamem "github.com/mettjs/cairnmark/internal/metadata/memory"
	"github.com/mettjs/cairnmark/internal/storage"
	"github.com/mettjs/cairnmark/internal/storage/memory"
)

// newJobsRouter is newTestRouter plus the worker that runs what the router
// submits. Tests drive it with RunOnce between the submit and the poll, so
// nothing sleeps.
func newJobsRouter(t *testing.T) (http.Handler, *jobs.Worker) {
	t.Helper()
	repo := metamem.New()
	svc := files.New(memory.New(), repo)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := Router(Deps{Files: svc, Logger: discard})
	w := jobs.New(repo, svc, discard, jobs.Options{FlushInterval: time.Millisecond})
	return h, w
}

// jobBody is the JSON shape of GET /jobs/{id}, POST /files/{id}/extract and
// POST /jobs/{id}/cancel.
type jobBody struct {
	ID        string `json:"id"`
	ArchiveID string `json:"archive_id"`
	Status    string `json:"status"`
	Progress  struct {
		Done  int `json:"done"`
		Total int `json:"total"`
	} `json:"progress"`
	CancelRequested bool         `json:"cancel_requested"`
	Summary         *summaryBody `json:"summary"`
	Error           string       `json:"error"`
	FinishedAt      *string      `json:"finished_at"`
}

type summaryBody struct {
	ArchiveID       string         `json:"archive_id"`
	Entries         int            `json:"entries"`
	Extracted       int            `json:"extracted"`
	Skipped         int            `json:"skipped"`
	SkippedByReason map[string]int `json:"skipped_by_reason"`
	SampleSkipped   []struct {
		Index  int    `json:"index"`
		Reason string `json:"reason"`
	} `json:"sample_skipped"`
}

// extractAndRun submits an extraction, runs the worker once, and returns the
// job as GET /jobs/{id} then reports it.
func extractAndRun(t *testing.T, h http.Handler, w *jobs.Worker, archiveID string, body []byte) jobBody {
	t.Helper()
	rec := do(t, h, "POST", "/files/"+archiveID+"/extract", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d (%s)", rec.Code, rec.Body)
	}
	var submitted jobBody
	decode(t, rec, &submitted)
	if submitted.Status != "pending" || submitted.ArchiveID != archiveID || submitted.Summary != nil ||
		rec.Header().Get("Location") != "/jobs/"+submitted.ID {
		t.Fatalf("submitted job: %+v Location=%q", submitted, rec.Header().Get("Location"))
	}
	if ran, err := w.RunOnce(context.Background()); err != nil || !ran {
		t.Fatalf("RunOnce: ran=%v err=%v", ran, err)
	}
	rec = do(t, h, "GET", "/jobs/"+submitted.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET job: %d (%s)", rec.Code, rec.Body)
	}
	var job jobBody
	decode(t, rec, &job)
	return job
}

// zipOf builds an archive from members in the order given — order is the
// entry index, which every assertion below selects by, so this takes an
// ordered slice rather than a map.
func zipOf(t *testing.T, ms []member) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range ms {
		w, err := zw.Create(m.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, m.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type member struct{ name, body string }

// body returns the content zipOf stored for name.
func body(t *testing.T, name string) string {
	t.Helper()
	for _, m := range members {
		if m.name == name {
			return m.body
		}
	}
	t.Fatalf("no fixture member %q", name)
	return ""
}

// members is indexes 0 and 1 selectable, 2 platform_metadata.
var members = []member{
	{"reports/q3.pdf", "%PDF-1.7 quarterly"},
	{"reports/data.json", `{"revenue":42}`},
	{"__MACOSX/reports/._q3.pdf", "resource fork"},
}

func do(t *testing.T, h http.Handler, method, url string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, bytes.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
}

func uploadZip(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := do(t, h, "POST", "/files?filename=docs.zip&tag.project=demo", zipOf(t, members), "Content-Type", "application/zip")
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload zip: %d (%s)", rec.Code, rec.Body)
	}
	var f struct {
		ID string `json:"id"`
	}
	decode(t, rec, &f)
	return f.ID
}

type listPage struct {
	Files []struct {
		ID       string         `json:"id"`
		Metadata map[string]any `json:"metadata"`
	} `json:"files"`
	Count int `json:"count"`
}

func TestArchiveEndToEnd(t *testing.T) {
	h, w := newJobsRouter(t)
	id := uploadZip(t, h)

	// List the entries: nothing is written, and the junk is flagged.
	rec := do(t, h, "GET", "/files/"+id+"/archive", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive listing: %d (%s)", rec.Code, rec.Body)
	}
	var listing struct {
		ArchiveID string `json:"archive_id"`
		Entries   []struct {
			Index       int    `json:"index"`
			Name        string `json:"name"`
			Size        int64  `json:"size"`
			ContentType string `json:"content_type"`
			CRC32       string `json:"crc32"`
			Selectable  bool   `json:"selectable"`
			Reason      string `json:"reason"`
		} `json:"entries"`
	}
	decode(t, rec, &listing)
	if listing.ArchiveID != id || len(listing.Entries) != 3 {
		t.Fatalf("listing: %+v", listing)
	}
	if e := listing.Entries[0]; e.Index != 0 || e.Name != "reports/q3.pdf" || e.ContentType != "application/pdf" ||
		!e.Selectable || len(e.CRC32) != 8 || e.Size != int64(len(body(t, e.Name))) {
		t.Fatalf("entry 0: %+v", e)
	}
	if e := listing.Entries[2]; e.Selectable || e.Reason != "platform_metadata" {
		t.Fatalf("entry 2: %+v", e)
	}
	if rec := do(t, h, "GET", "/files?tag.cm:archive_id="+id, nil); !strings.Contains(rec.Body.String(), `"count":0`) {
		t.Fatalf("listing must not write: %s", rec.Body)
	}

	// Submit the extraction, run it, and read the bounded summary off the job.
	job := extractAndRun(t, h, w, id, nil)
	if job.Status != "succeeded" || job.Progress.Done != 2 || job.Progress.Total != 2 || job.FinishedAt == nil || job.Error != "" {
		t.Fatalf("job: %+v", job)
	}
	sum := job.Summary
	if sum == nil || sum.ArchiveID != id || sum.Entries != 3 || sum.Extracted != 2 || sum.Skipped != 1 ||
		sum.SkippedByReason["platform_metadata"] != 1 || len(sum.SampleSkipped) != 1 || sum.SampleSkipped[0].Index != 2 {
		t.Fatalf("summary: %+v", sum)
	}

	// The children are found by the archive tag, and one downloads byte-identical.
	rec = do(t, h, "GET", "/files?tag.cm:archive_id="+id, nil)
	var page listPage
	decode(t, rec, &page)
	if page.Count != 2 {
		t.Fatalf("children: %s", rec.Body)
	}
	var childID string
	for _, f := range page.Files {
		if f.Metadata["cm:archive_path"] == "reports/q3.pdf" {
			childID = f.ID
			if f.Metadata["cm:archive_index"] != float64(0) {
				t.Fatalf("index tag: %v", f.Metadata)
			}
		}
	}
	if childID == "" {
		t.Fatalf("q3.pdf child not found in %s", rec.Body)
	}
	rec = do(t, h, "GET", "/files/"+childID+"?download=stream", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != body(t, "reports/q3.pdf") {
		t.Fatalf("download: %d %q", rec.Code, rec.Body)
	}
	if rec.Header().Get("Content-Type") != "application/pdf" || rec.Header().Get("X-Checksum-Sha256") == "" {
		t.Fatalf("download headers: %v", rec.Header())
	}

	// The list scopes separate entries, everything else, and archives.
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 3}, {"&entries=include", 3}, {"&entries=only", 2}, {"&entries=exclude", 1}, {"&tag.cm:archive=true", 1},
	} {
		rec := do(t, h, "GET", "/files?limit=50"+tc.query, nil)
		decode(t, rec, &page)
		if rec.Code != http.StatusOK || page.Count != tc.want {
			t.Fatalf("GET /files?%s: %d count=%d want %d", tc.query, rec.Code, page.Count, tc.want)
		}
	}
	if rec := do(t, h, "GET", "/files?entries=bogus", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad entries param: %d", rec.Code)
	}

	// A replace-mode PATCH on a child keeps its reserved tags.
	rec = do(t, h, "PATCH", "/files/"+childID+"/metadata?mode=replace", []byte(`{"reviewed":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch replace: %d (%s)", rec.Code, rec.Body)
	}
	var patched struct {
		Metadata map[string]any `json:"metadata"`
	}
	decode(t, rec, &patched)
	if patched.Metadata["reviewed"] != true || patched.Metadata["cm:archive_id"] != id ||
		patched.Metadata["cm:archive_path"] != "reports/q3.pdf" || patched.Metadata["cm:archive_index"] == nil {
		t.Fatalf("replace stripped the reserved tags: %v", patched.Metadata)
	}

	// A re-run is a no-op that says so.
	again := extractAndRun(t, h, w, id, []byte(`{"entries":[0,1]}`))
	if again.Status != "succeeded" || again.Summary.Extracted != 0 || again.Summary.SkippedByReason["already_extracted"] != 2 ||
		again.Summary.SkippedByReason["platform_metadata"] != 1 {
		t.Fatalf("re-run: %+v", again)
	}
}

func TestArchiveEndpointsRejectNonArchives(t *testing.T) {
	h, _ := newTestRouter(0)
	up := doUpload(t, h, "just a text file", 16)
	loc := up.Header().Get("Location")

	if rec := do(t, h, "GET", loc+"/archive", nil); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("listing a non-archive: %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", loc+"/extract", nil); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("extracting a non-archive: %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/files/11111111-1111-1111-1111-111111111111/archive", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", rec.Code)
	}
}

func TestExtractRequestValidation(t *testing.T) {
	h, _ := newTestRouter(0)
	id := uploadZip(t, h)

	if rec := do(t, h, "POST", "/files/"+id+"/extract", nil, "Idempotency-Key", "k1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("Idempotency-Key on extract: %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/files/"+id+"/extract", []byte(`{"entries":[99]}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range selection: %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/files/"+id+"/extract", []byte(`{"entries":`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: %d (%s)", rec.Code, rec.Body)
	}
	// Both of these decode without error and leave Entries nil, which would
	// read as "no selection" and extract the whole archive.
	if rec := do(t, h, "POST", "/files/"+id+"/extract", []byte(`null`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("null body: %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/files/"+id+"/extract", []byte(`{"entries":[0]}{"entries":[1]}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing object: %d (%s)", rec.Code, rec.Body)
	}
	if kids := do(t, h, "GET", "/files?tag.cm:archive_id="+id, nil); !strings.Contains(kids.Body.String(), `"count":0`) {
		t.Fatalf("a rejected selection extracted something: %s", kids.Body)
	}

	// One active job per archive: a second submission is a 409 naming the
	// first, with a Retry-After for a client that would rather resubmit.
	rec := do(t, h, "POST", "/files/"+id+"/extract", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d (%s)", rec.Code, rec.Body)
	}
	var first jobBody
	decode(t, rec, &first)
	rec = do(t, h, "POST", "/files/"+id+"/extract", []byte(`{"entries":[0]}`))
	if rec.Code != http.StatusConflict || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("second submission: %d %v (%s)", rec.Code, rec.Header(), rec.Body)
	}
	var conflict struct {
		Error string `json:"error"`
		JobID string `json:"job_id"`
	}
	decode(t, rec, &conflict)
	if conflict.JobID != first.ID || conflict.Error == "" {
		t.Fatalf("conflict body: %+v want job %s", conflict, first.ID)
	}
}

func TestJobEndpoints(t *testing.T) {
	h, w := newJobsRouter(t)
	id := uploadZip(t, h)

	if rec := do(t, h, "GET", "/jobs/11111111-1111-1111-1111-111111111111", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job: %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/jobs/not-a-uuid", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed job id: %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/jobs/11111111-1111-1111-1111-111111111111/cancel", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown job: %d (%s)", rec.Code, rec.Body)
	}

	// Cancel a pending job: terminal at once, nothing extracted, and the
	// archive is free for the next submission.
	rec := do(t, h, "POST", "/files/"+id+"/extract", nil)
	var pending jobBody
	decode(t, rec, &pending)
	rec = do(t, h, "POST", "/jobs/"+pending.ID+"/cancel", nil)
	var cancelled jobBody
	decode(t, rec, &cancelled)
	if rec.Code != http.StatusAccepted || cancelled.Status != "cancelled" || !cancelled.CancelRequested ||
		cancelled.FinishedAt == nil || cancelled.Summary != nil {
		t.Fatalf("cancel pending: %d %+v", rec.Code, cancelled)
	}
	// Idempotent: cancelling a terminal job reports its state, not an error.
	rec = do(t, h, "POST", "/jobs/"+pending.ID+"/cancel", nil)
	decode(t, rec, &cancelled)
	if rec.Code != http.StatusAccepted || cancelled.Status != "cancelled" {
		t.Fatalf("cancel again: %d %+v", rec.Code, cancelled)
	}
	if ran, err := w.RunOnce(context.Background()); ran || err != nil {
		t.Fatalf("a cancelled job was still picked up: ran=%v err=%v", ran, err)
	}
	if kids := do(t, h, "GET", "/files?tag.cm:archive_id="+id, nil); !strings.Contains(kids.Body.String(), `"count":0`) {
		t.Fatalf("a cancelled job extracted something: %s", kids.Body)
	}

	// The slot is free: the next submission runs to completion, and its
	// terminal state reads the same on GET and on a late cancel.
	job := extractAndRun(t, h, w, id, nil)
	if job.Status != "succeeded" || job.Summary == nil || job.Summary.Extracted != 2 {
		t.Fatalf("job after cancel: %+v", job)
	}
	rec = do(t, h, "POST", "/jobs/"+job.ID+"/cancel", nil)
	var late jobBody
	decode(t, rec, &late)
	if rec.Code != http.StatusAccepted || late.Status != "succeeded" || late.Summary == nil || late.Summary.Extracted != 2 {
		t.Fatalf("cancel a finished job: %d %+v", rec.Code, late)
	}
}

func TestReservedKeysRejected(t *testing.T) {
	h, _ := newTestRouter(0)

	if rec := do(t, h, "POST", "/files?tag.cm:archive_id=x", []byte("abc")); rec.Code != http.StatusBadRequest {
		t.Fatalf("reserved tag param on upload: %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/files", []byte("abc"), "X-Metadata", `{"cm:archive":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("reserved tag header on upload: %d (%s)", rec.Code, rec.Body)
	}

	up := doUpload(t, h, "abc", 3)
	loc := up.Header().Get("Location")
	if rec := do(t, h, "PATCH", loc+"/metadata", []byte(`{"cm:archive_index":7}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("reserved tag on patch: %d (%s)", rec.Code, rec.Body)
	}
	// Reading by a reserved key is fine — it is how children are found.
	if rec := do(t, h, "GET", "/files?tag.cm:archive_id=x", nil); rec.Code != http.StatusOK {
		t.Fatalf("filter by reserved tag: %d (%s)", rec.Code, rec.Body)
	}
}

// outageBackend fails every range read once down is set: the store went away
// after the upload, as during an object-store outage.
type outageBackend struct {
	storage.Backend
	down bool
}

func (b *outageBackend) GetRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if b.down {
		return nil, errors.New("s3: connection refused")
	}
	return b.Backend.GetRange(ctx, key, off, n)
}

func TestArchiveEndpointsDuringAStoreOutageAreServerErrors(t *testing.T) {
	// Not 415: the file was never judged. The caller's zip is fine and the
	// store is not, and the status has to say which.
	b := &outageBackend{Backend: memory.New()}
	h := Router(Deps{Files: files.New(b, metamem.New()), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	id := uploadZip(t, h)
	b.down = true
	if rec := do(t, h, "GET", "/files/"+id+"/archive", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("listing during an outage: %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/files/"+id+"/extract", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("submission during an outage: %d (%s)", rec.Code, rec.Body)
	}
}
