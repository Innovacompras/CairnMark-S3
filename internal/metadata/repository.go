// Package metadata is the persistence seam for file records. The Repository
// interface is the only contract above this layer; the pgx implementation
// (Phase 1) is the sole place that issues SQL.
package metadata

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when no file record matches the query.
var ErrNotFound = errors.New("metadata: file not found")

// Idempotency-key lifecycle states.
const (
	IdempotencyPending   = "pending"
	IdempotencyCompleted = "completed"
)

// Reserved tag keys. The cm: prefix is owned by the service — written by
// archive extraction, never by a client — across the whole tag namespace. The
// api layer rejects it on upload and PATCH, and a replace-mode UpdateMetadata
// carries the keys across so a client cannot strip them by accident.
const (
	ReservedTagPrefix = "cm:"
	TagArchiveID      = ReservedTagPrefix + "archive_id"    // on an extracted entry: the archive it came from
	TagArchivePath    = ReservedTagPrefix + "archive_path"  // on an extracted entry: its full path inside the archive
	TagArchiveIndex   = ReservedTagPrefix + "archive_index" // on an extracted entry: its directory index (names are not unique)
	TagArchive        = ReservedTagPrefix + "archive"       // on an archive row: TagArchiveMarker once it has been extracted
)

// TagArchiveMarker is the value of TagArchive. A string, not a boolean, because
// the list endpoint's tag.<k>=<v> filter is string-typed: this is what makes
// GET /files?tag.cm:archive=true list the archives.
const TagArchiveMarker = "true"

// EntryScope selects whether extracted archive entries appear in a List. The
// parameter is about *entries*, not archives: the archives themselves are
// found by their TagArchive marker.
type EntryScope string

const (
	EntriesInclude EntryScope = "include" // the default: files, archives and entries together
	EntriesExclude EntryScope = "exclude" // everything but extracted entries
	EntriesOnly    EntryScope = "only"    // extracted entries alone
)

// IdempotencyRecord is the stored state of an upload idempotency key.
type IdempotencyRecord struct {
	Status string  // IdempotencyPending | IdempotencyCompleted
	FileID *string // the produced file id; nil while pending
}

// File is the metadata record for a stored object. It mirrors the files table.
type File struct {
	ID             string
	StorageKey     string
	Filename       string
	ContentType    string
	SizeBytes      int64
	ChecksumSHA256 string         // hex sha-256
	Metadata       map[string]any // JSONB tags
	CreatedAt      time.Time
	UpdatedAt      *time.Time // nil until the first metadata update — a pristine, unmodified file has none
	DeletedAt      *time.Time // soft delete; nil while live
}

// ListFilter narrows a List query. Zero values mean "no constraint".
//
// Pagination is keyset-based: Cursor is the id of the last file of the previous
// page (exclusive), and a page holds the next Limit files older than it. Ids
// are UUIDv7 — time-ordered — so paging by id descending walks newest-first
// without the deep-page cost of OFFSET.
type ListFilter struct {
	ContentType string
	Tags        map[string]any // matched against the JSONB metadata column
	Limit       int
	Cursor      string     // exclusive upper-bound file id; empty means first page
	Entries     EntryScope // "" behaves as EntriesInclude

	// IncludeDeleted also returns soft-deleted rows. Every read path stays
	// live-only except extraction's resume logic, which must see the tombstone
	// of an entry the user deleted so that it does not recreate it.
	IncludeDeleted bool
}

// Repository persists File records. Implementations own all SQL.
type Repository interface {
	// Create inserts a new record. The caller supplies the ID and StorageKey.
	Create(ctx context.Context, f *File) error

	// Get returns the record by ID, or ErrNotFound.
	Get(ctx context.Context, id string) (*File, error)

	// List returns records matching the filter, newest first.
	List(ctx context.Context, filter ListFilter) ([]*File, error)

	// UpdateMetadata writes tags into the record's JSONB metadata: merged into
	// the existing object when merge is true, replacing it wholesale otherwise.
	// A replace preserves keys under ReservedTagPrefix.
	UpdateMetadata(ctx context.Context, id string, tags map[string]any, merge bool) (*File, error)

	// Delete soft-deletes the record by ID (sets deleted_at). The object is
	// purged asynchronously by GC. Returns ErrNotFound if no live row matched.
	Delete(ctx context.Context, id string) error

	// ListDeleted returns up to limit soft-deleted records, oldest first, so GC
	// can purge their objects and then Purge the rows.
	ListDeleted(ctx context.Context, limit int) ([]*File, error)

	// Purge hard-deletes a soft-deleted record by ID, after its object is gone.
	Purge(ctx context.Context, id string) error

	// StorageKeys returns the set of every storage key referenced by the table
	// (any state), so GC can tell which stored objects are orphans.
	StorageKeys(ctx context.Context) (map[string]struct{}, error)

	// ClaimIdempotencyKey atomically claims key as pending. It returns
	// claimed=true if this caller now owns the key; otherwise claimed=false and
	// existing describes the row already present (which may be nil if it raced
	// away). Implemented as INSERT ... ON CONFLICT DO NOTHING.
	ClaimIdempotencyKey(ctx context.Context, key string) (claimed bool, existing *IdempotencyRecord, err error)

	// CreateForKey inserts the file row and marks the (already-claimed) key
	// completed in one transaction, so the two can never disagree after a crash.
	CreateForKey(ctx context.Context, f *File, key string) error

	// ReleaseIdempotencyKey deletes a still-pending claim so a failed upload can
	// be retried under the same key.
	ReleaseIdempotencyKey(ctx context.Context, key string) error

	// PurgeIdempotencyKeys deletes keys created before the cutoff (TTL), returning
	// how many were removed. Called by GC.
	PurgeIdempotencyKeys(ctx context.Context, before time.Time) (int, error)
}
