package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"github.com/mettjs/cairnmark/internal/files"
)

// tagPrefix marks query parameters that become metadata tags / filters, e.g.
// ?tag.env=prod → {"env": "prod"}.
const tagPrefix = "tag."

// uploadTags builds the metadata to attach on upload from two sources, merged
// with query params taking precedence:
//   - an X-Metadata header carrying a JSON object (typed/nested values);
//   - tag.<key>=<value> query params (string values, curl-friendly).
//
// Keys under the service-owned prefix are refused from either source.
func uploadTags(r *http.Request) (map[string]any, error) {
	tags := map[string]any{}
	if h := r.Header.Get("X-Metadata"); h != "" {
		if err := json.Unmarshal([]byte(h), &tags); err != nil {
			return nil, fmt.Errorf("X-Metadata must be a JSON object: %v", err)
		}
	}
	maps.Copy(tags, tagParams(r.URL.Query()))
	if k := reservedTag(tags); k != "" {
		return nil, fmt.Errorf("tag %q is reserved: keys under %q are written by the service", k, files.ReservedTagPrefix)
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}

// tagParams extracts tag.<key>=<value> pairs as string-valued tags.
func tagParams(q url.Values) map[string]any {
	tags := map[string]any{}
	for key, vals := range q {
		if name, ok := strings.CutPrefix(key, tagPrefix); ok && name != "" && len(vals) > 0 {
			tags[name] = vals[0]
		}
	}
	return tags
}

// reservedTag returns a key under the service-owned prefix, or "" if none.
// Reading such keys (as list filters) is fine; writing them is not.
func reservedTag(tags map[string]any) string {
	for k := range tags {
		if strings.HasPrefix(k, files.ReservedTagPrefix) {
			return k
		}
	}
	return ""
}

// maxMetadataBodyBytes caps a PATCH metadata body. Tags are small; without a
// cap the decoded map grows with whatever the client streams.
const maxMetadataBodyBytes = 1 << 20 // 1 MiB

// patchMetadata merges (default) or replaces (?mode=replace) the JSONB tags of
// a file. The body is a JSON object. Reserved keys are refused here, and a
// replace leaves the existing reserved keys in place — enforced in the
// repository, since a client replacing the tags never names them.
func (h *fileHandler) patchMetadata(w http.ResponseWriter, r *http.Request) {
	var tags map[string]any
	body := http.MaxBytesReader(w, r.Body, maxMetadataBodyBytes)
	if err := json.NewDecoder(body).Decode(&tags); err != nil {
		if tooLarge, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeClientError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("metadata body exceeds the %d-byte limit", tooLarge.Limit))
			return
		}
		writeClientError(w, http.StatusBadRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if tags == nil { // body was literal `null` — not a usable tag set
		writeClientError(w, http.StatusBadRequest, "body must be a JSON object, not null")
		return
	}
	if k := reservedTag(tags); k != "" {
		writeClientError(w, http.StatusBadRequest,
			fmt.Sprintf("tag %q is reserved: keys under %q are written by the service", k, files.ReservedTagPrefix))
		return
	}
	merge := r.URL.Query().Get("mode") != "replace"

	f, err := h.svc.UpdateMetadata(r.Context(), r.PathValue("id"), tags, merge)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(f))
}
