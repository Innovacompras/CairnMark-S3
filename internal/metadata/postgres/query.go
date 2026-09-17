package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mettjs/cairnmark/internal/metadata"
)

// fileColumns is the single source of truth for the column list and order that
// scanFile expects; selectColumns prefixes it for SELECT, and RETURNING clauses
// reuse it directly.
const fileColumns = `id, storage_key, filename, content_type, size_bytes,
	coalesce(checksum_sha256, ''), metadata, created_at, updated_at, deleted_at`

const selectColumns = "select " + fileColumns

// listDefaultLimit / listMaxLimit bound a List page when the caller's limit is
// unset or unreasonable. The api layer applies the same bounds for an accurate
// response; these are defense-in-depth for any other caller.
const (
	listDefaultLimit = 50
	listMaxLimit     = 500
)

// row abstracts pgx.Row / a single result row so scanFile serves Get and List.
type row interface {
	Scan(dest ...any) error
}

func scanFile(r row) (*metadata.File, error) {
	var f metadata.File
	var meta []byte
	if err := r.Scan(
		&f.ID, &f.StorageKey, &f.Filename, &f.ContentType, &f.SizeBytes,
		&f.ChecksumSHA256, &meta, &f.CreatedAt, &f.UpdatedAt, &f.DeletedAt,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(meta, &f.Metadata); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal metadata: %w", err)
	}
	return &f, nil
}

// List returns records matching the filter, newest first — live ones unless
// the filter asks for tombstones too — filtered by content type, JSONB tag
// containment and the extracted-entry scope, with keyset pagination. Ordering
// is by id descending: ids are UUIDv7, so id order is creation order and the
// primary-key index serves both the sort and the cursor bound.
func (r *Repo) List(ctx context.Context, filter metadata.ListFilter) ([]*metadata.File, error) {
	var conds []string
	var args []any
	if !filter.IncludeDeleted {
		conds = append(conds, "deleted_at is null")
	}

	if filter.ContentType != "" {
		args = append(args, filter.ContentType)
		conds = append(conds, fmt.Sprintf("content_type = $%d", len(args)))
	}

	if filter.Cursor != "" {
		args = append(args, filter.Cursor)
		conds = append(conds, fmt.Sprintf("id < $%d::uuid", len(args)))
	}

	// JSONB containment (@>) is GIN-indexed (files_metadata_gin): the row's
	// metadata must contain every requested key/value pair.
	if len(filter.Tags) > 0 {
		tags, err := json.Marshal(filter.Tags)
		if err != nil {
			return nil, fmt.Errorf("postgres: marshal tag filter: %w", err)
		}
		args = append(args, string(tags))
		conds = append(conds, fmt.Sprintf("metadata @> $%d::jsonb", len(args)))
	}

	// Presence (?) of the archive-id key is what makes a row an extracted
	// entry. `only` is served by the GIN index; `exclude` is a negation and
	// therefore a filter, not an index seek — fine at this project's scale,
	// and a partial index is the additive upgrade if it ever shows up in slow
	// queries.
	switch filter.Entries {
	case metadata.EntriesOnly:
		args = append(args, metadata.TagArchiveID)
		conds = append(conds, fmt.Sprintf("metadata ? $%d", len(args)))
	case metadata.EntriesExclude:
		args = append(args, metadata.TagArchiveID)
		conds = append(conds, fmt.Sprintf("not (metadata ? $%d)", len(args)))
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = listDefaultLimit
	}
	if limit > listMaxLimit {
		limit = listMaxLimit
	}
	args = append(args, limit)
	limitClause := fmt.Sprintf(" limit $%d", len(args))

	where := ""
	if len(conds) > 0 {
		where = " where " + strings.Join(conds, " and ")
	}
	q := selectColumns + " from files" + where + " order by id desc" + limitClause

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list files: %w", err)
	}
	defer rows.Close()

	var files []*metadata.File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan list row: %w", err)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list rows: %w", err)
	}
	return files, nil
}

// UpdateMetadata writes tags into the record's JSONB column: merged (||) when
// merge is true, replaced otherwise.
func (r *Repo) UpdateMetadata(ctx context.Context, id string, tags map[string]any, merge bool) (*metadata.File, error) {
	patch, err := json.Marshal(tags)
	if err != nil {
		return nil, fmt.Errorf("postgres: marshal tags: %w", err)
	}
	args := []any{id, string(patch)}
	assignment := "metadata = metadata || $2::jsonb"
	if !merge {
		// A replace rewrites the whole column, which would silently strip the
		// service-owned cm:* keys and orphan an extracted entry from its
		// archive. Rejecting reserved keys at the api layer cannot prevent
		// that — the client never names them — so they are carried across
		// here, in the only place that issues SQL.
		assignment = `metadata = coalesce(
			(select jsonb_object_agg(key, value) from jsonb_each(metadata) where key like $3),
			'{}'::jsonb) || $2::jsonb`
		args = append(args, metadata.ReservedTagPrefix+"%")
	}
	q := `update files set ` + assignment + `, updated_at = now()
		where id = $1 and deleted_at is null returning ` + fileColumns
	f, err := scanFile(r.pool.QueryRow(ctx, q, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, metadata.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: update metadata: %w", err)
	}
	return f, nil
}
