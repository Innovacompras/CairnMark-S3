package api

import (
	"net/http"
	"strconv"

	"github.com/mettjs/cairnmark/internal/files"
)

// List pagination bounds applied at the edge so the response echoes the limit
// that was actually used (the repository enforces the same bounds defensively).
const (
	listDefaultLimit = 50
	listMaxLimit     = 500
)

// list returns files filtered by content_type, tag.<key> params and the
// extracted-entry scope, newest first, with keyset pagination: pass a page's
// next_cursor back as ?cursor= to fetch the files older than it.
func (h *fileHandler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := atoiDefault(q.Get("limit"), 0)
	if err != nil {
		writeClientError(w, http.StatusBadRequest, "limit must be an integer")
		return
	}
	limit = clampLimit(limit)
	scope, ok := entryScope(q.Get("entries"))
	if !ok {
		writeClientError(w, http.StatusBadRequest, "entries must be include, exclude or only")
		return
	}

	results, err := h.svc.List(r.Context(), files.ListFilter{
		ContentType: q.Get("content_type"),
		Tags:        tagParams(q),
		Limit:       limit,
		Cursor:      q.Get("cursor"),
		Entries:     scope,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}

	items := make([]fileResponse, 0, len(results))
	for _, f := range results {
		items = append(items, toResponse(f))
	}
	resp := listResponse{Files: items, Limit: limit, Count: len(items)}
	// A full page may have more behind it; a short page is definitely the last.
	// When the total is an exact multiple of limit, the final cursor yields one
	// empty page — the unambiguous end-of-list signal either way is no cursor.
	if len(items) == limit {
		resp.NextCursor = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

type listResponse struct {
	Files      []fileResponse `json:"files"`
	Limit      int            `json:"limit"`
	Count      int            `json:"count"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// entryScope parses ?entries=. The parameter is named for what it selects —
// *extracted entries* — because "archive=only" would read as "only archives",
// the opposite of what it does. The default keeps existing callers unchanged.
func entryScope(v string) (files.EntryScope, bool) {
	switch scope := files.EntryScope(v); scope {
	case "", files.EntriesInclude:
		return files.EntriesInclude, true
	case files.EntriesExclude, files.EntriesOnly:
		return scope, true
	}
	return "", false
}

func atoiDefault(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	return strconv.Atoi(s)
}

// clampLimit normalizes the page size so the value returned in the response is
// exactly what was applied to the query.
func clampLimit(limit int) int {
	if limit <= 0 {
		return listDefaultLimit
	}
	if limit > listMaxLimit {
		return listMaxLimit
	}
	return limit
}
