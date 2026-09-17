package files

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// childPage is the page size for walking an archive's existing children. The
// repository silently clamps a larger request to its own maximum, so this is a
// request, not a guarantee — priorChildren pages until a page comes back empty
// rather than treating a short page as the last one.
const childPage = 500

// priorChildren maps the directory indexes that already have a child row to
// the reason a re-run must skip them. Soft-deleted children are included on
// purpose: delete wins over re-extract for as long as the tombstone exists.
// Once GC purges it the system has no memory the child ever existed and a
// later run will recreate it — inherent to hard deletion, not a defect.
func (s *Service) priorChildren(ctx context.Context, id string) (map[int]SkipReason, error) {
	prior := map[int]SkipReason{}
	filter := ListFilter{
		Tags:           map[string]any{metadata.TagArchiveID: id},
		Limit:          childPage,
		IncludeDeleted: true,
	}
	for {
		page, err := s.repo.List(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("files: list extracted entries of %s: %w", id, err)
		}
		for _, c := range page {
			i, ok := tagIndex(c.Metadata[metadata.TagArchiveIndex])
			if !ok {
				continue
			}
			if c.DeletedAt == nil {
				prior[i] = SkipAlreadyExtracted // a live child outranks a tombstone
			} else if prior[i] == "" {
				prior[i] = SkipPreviouslyDeleted
			}
		}
		// Until a page comes back empty, not until one comes back short: the
		// repository clamps an over-large limit silently, so a short page is
		// not evidence of the last page. Treating it as one would truncate the
		// skip-set and re-extract the remainder as duplicate children.
		if len(page) == 0 {
			return prior, nil
		}
		filter.Cursor = page[len(page)-1].ID
	}
}

// tagIndex reads the archive-index tag back. It is written as a Go int, but
// JSONB hands numbers back as float64.
func tagIndex(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), n == float64(int(n))
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// markArchive stamps TagArchive on the archive row so "list my archives" is
// answerable: children are findable by TagArchiveID, but without a marker the
// archives themselves are indistinguishable from ordinary files. A merge, so
// the row's own tags survive; skipped when already present so a no-op re-run
// does not bump updated_at. Extract calls this only when the run wrote a
// child, so an archive nothing was extracted from stays unmarked.
func (s *Service) markArchive(ctx context.Context, f *File) error {
	if f.Metadata[metadata.TagArchive] == metadata.TagArchiveMarker {
		return nil
	}
	if _, err := s.repo.UpdateMetadata(ctx, f.ID, map[string]any{metadata.TagArchive: metadata.TagArchiveMarker}, true); err != nil {
		return fmt.Errorf("files: mark archive %s: %w", f.ID, translateNotFound(err))
	}
	return nil
}
