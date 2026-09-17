-- +goose Up
-- One row per POST /files/{id}/extract. The row is two things at once: the
-- admission control for its archive (the partial unique index below) and the
-- unit of work a worker claims — so the advisory lease that used to borrow the
-- idempotency_keys table is gone.
create table extraction_jobs (
    -- uuid, not text: files.id is uuid and a foreign key must match its type.
    -- Generated in Go as a UUIDv7, like files.id.
    id               uuid primary key,
    archive_id       uuid not null references files(id) on delete cascade,
    status           text not null,               -- pending | running | succeeded | failed | cancelled
    selection        jsonb,                       -- the caller's entry indexes; null = every selectable entry
    cancel_requested boolean not null default false,
    -- Bumped on every claim. Every write a worker makes is guarded by it, so a
    -- worker that stalled, was reaped, and woke up cannot overwrite the row a
    -- second worker now owns.
    attempt          integer not null default 0,
    progress_done    integer not null default 0,  -- entries the current run has processed
    progress_total   integer not null default 0,  -- entries the current run intends to write
    summary          jsonb,                       -- the extraction summary, once terminal
    error            text,                        -- the reason, when status = 'failed'
    heartbeat_at     timestamptz,                 -- refreshed while running; null otherwise
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now(),
    finished_at      timestamptz
);

-- Admission control: at most one non-terminal job per archive. This is the
-- lease, as an invariant rather than a convention — an insert either wins or
-- conflicts, and Postgres decides.
create unique index extraction_jobs_one_active_per_archive
    on extraction_jobs (archive_id)
    where status in ('pending', 'running');

-- Pickup: oldest pending first.
create index extraction_jobs_pickup
    on extraction_jobs (created_at) where status = 'pending';

-- Reaping stranded running rows, and purging terminal rows past retention.
create index extraction_jobs_heartbeat
    on extraction_jobs (heartbeat_at) where status = 'running';
create index extraction_jobs_finished
    on extraction_jobs (finished_at) where finished_at is not null;

-- +goose Down
drop table extraction_jobs;
