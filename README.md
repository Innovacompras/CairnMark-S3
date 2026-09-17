# CairnMark

[![CI](https://github.com/mettjs/cairnmark/actions/workflows/ci.yml/badge.svg)](https://github.com/mettjs/cairnmark/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> A cairn isn't a pile of stones. It's a pile that means something. Travelers
> stack stones to mark a path, record a place, point the way back. The stones are
> ordinary; the meaning lives in the marking. Strip that away and you have a heap.
> Keep it and every stone is placed, recorded, and findable.

A small, self-hostable file service: an HTTP API in front of **S3-compatible
object storage** with a **Postgres metadata layer**. More than a thin S3 proxy —
it owns a queryable metadata model, inline integrity checks, and tag search —
and deliberately less than a platform: **no auth, no tenancy, no UI** (put it
behind your own gateway).

## Features

- **Streaming upload/download** — multi-GB files flow through bounded memory;
  nothing is buffered whole.
- **Presigned-URL downloads** — `GET` 302-redirects to the object store by
  default, so large transfers never pass through the service.
- **Range requests** — `bytes=` partial downloads.
- **Inline SHA-256** — computed during upload, stored, and verified on read.
- **Queryable metadata** — arbitrary JSONB tags, searchable via a GIN index.
  This is the payoff a plain S3 proxy can't give you.
- **Soft delete + GC** — deletes are instant; objects are purged and orphaned
  objects reclaimed by a background reconciliation sweep.
- **Idempotent uploads** — an optional `Idempotency-Key` header makes retried
  uploads safe: a retry replays the original result instead of duplicating, with
  a crash-safe atomic commit.
- **Archive extraction** — upload a `.zip`, list what is inside, and store the
  entries you pick as ordinary, searchable files linked back to the archive.
  Runs as a job you poll or cancel; hard-capped, resumable, and safe across
  replicas.
- **One backend, many stores** — the storage layer speaks plain S3. RustFS,
  SeaweedFS, and MinIO each ship as a ready-to-run Compose setup; AWS S3 or any
  other S3-compatible store needs only config changes.
- **Prometheus metrics** — per-route request counts and latency, GC
  reclamation counters, and extraction-job outcomes, durations and queue
  depth, exposed at `/metrics`.

## Quickstart

Requires only Docker. From a clone:

```sh
docker compose up -d --build
```

That starts CairnMark on `:8080` plus Postgres and RustFS — no separate installs.
The service applies its migrations and creates its bucket on boot. Then:

```sh
# Upload a file with tags
curl -i -H "Content-Type: text/plain" \
  --data-binary @README.md \
  "http://localhost:8080/files?filename=README.md&tag.project=cairnmark"

# (grab the "id" from the response, then…)
curl "http://localhost:8080/files/<id>/metadata"
curl -L "http://localhost:8080/files/<id>" -o downloaded.md   # -L follows the presign redirect
curl "http://localhost:8080/files?tag.project=cairnmark"      # search by tag
```

Or run the scripted demo: [`examples/quickstart.sh`](examples/quickstart.sh).

The object store's web console is at
<http://localhost:9001/rustfs/console/> — note the path; the root of `:9001`
answers as the S3 API, not the console.

## Choosing a storage backend

CairnMark is not tied to any object store: it speaks plain S3. Three backends ship
as ready-to-run Compose setups; switching needs no edit to the base file and no
code change — only a different `-f` flag.

**Stop the running stack before switching**, though: Compose won't take the old
substrate down for you (a service parked in an inactive profile isn't an orphan,
so even `--remove-orphans` leaves it running), and it keeps port 9000 bound, so
the incoming store fails to start.

```sh
docker compose down     # add -v to discard the stored objects too
```

| | Default | How to run it | Why you'd pick it |
|---|---|---|---|
| **RustFS** `1.0.0-rc.6` | ✅ | `docker compose up -d --build` | Actively maintained, Apache-2.0, drop-in on port 9000. **Pre-1.0.** |
| **SeaweedFS** `4.46` | | `docker compose -f docker-compose.yml -f compose.seaweedfs.yml up -d --build` | The most mature — Apache-2.0, in production since 2012. Serves S3 on **8333**, not 9000. |
| **MinIO** *(pinned)* | | `docker compose -f docker-compose.yml -f compose.minio.yml up -d --build` | The most familiar, and where your existing data probably is. **Frozen** — see below. |

Each publishes a web UI: RustFS at <http://localhost:9001/rustfs/console/>,
MinIO at <http://localhost:9001>, SeaweedFS's filer browser at
<http://localhost:8888> (objects live under `/buckets/`).

> **Already have data in the old MinIO volume?** Use the MinIO row above. Earlier
> versions of this compose file stored objects in a `miniodata` volume;
> `compose.minio.yml` still declares it, so running that overlay reattaches your
> existing bucket untouched. The RustFS default uses a separate `rustfsdata`
> volume and therefore starts **empty** — your Postgres rows would survive while
> the objects they point at would not be there, giving you `404`s on download.
> Switch deliberately, and migrate the objects first if you want the default.

**The default is chosen for a clean first run, not as a production
recommendation.** All three pass every operation CairnMark performs — round-trip,
ranged reads, unknown-size multipart, presigned GET, listing past the 1000-key
page boundary, and both GC reclamation paths — so this is a choice about
maintenance posture, not capability. For production data, SeaweedFS is the
conservative pick: it is the only one that is neither pre-1.0 nor frozen.

**On MinIO.** It stopped publishing community binaries on 2025-10-23 and pulled
its Docker Hub images, so `minio/minio:latest` no longer resolves and the server
repo is archived. `compose.minio.yml` therefore pins the last community release
from the quay.io mirror, `RELEASE.2025-09-07T16-13-09Z` — a fixed tag on purpose,
since no newer community release is coming. That build still serves the full AGPL
console at the root of `:9001` (unlike RustFS, which serves its console under
`/rustfs/console/`). It also predates the fix for **CVE-2025-62506**,
which requires credentials for a *restricted service or STS account* to exploit;
this stack mints neither, so the precondition doesn't exist as shipped — but
don't issue scoped service-account keys from it. Full reasoning is in
[`compose.minio.yml`](compose.minio.yml).

Note that MinIO the *server* is not `minio-go` the *client library*: the latter is
a separate, actively maintained project, and it is what CairnMark links against
regardless of which store you run.

## Configuration

All configuration is via environment variables (see [`.env.example`](.env.example)).

| Variable | Default | Purpose |
|---|---|---|
| `CAIRNMARK_HTTP_ADDR` | `:8080` | HTTP listen address |
| `CAIRNMARK_SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown grace period |
| `CAIRNMARK_PRESIGN_TTL` | `15m` | Lifetime of presigned download URLs |
| `CAIRNMARK_MAX_UPLOAD_BYTES` | `0` (uncapped) | Max upload body size in bytes; larger uploads get `413`. Also caps each entry extracted from an archive |
| `CAIRNMARK_GC_INTERVAL` | `5m` | Reconciliation sweep cadence (`<=0` disables GC — and all the cleanup below) |
| `CAIRNMARK_GC_GRACE_PERIOD` | `1h` | Min age before an unreferenced object is reclaimed; **must exceed your longest upload** |
| `CAIRNMARK_IDEMPOTENCY_TTL` | `24h` | How long upload idempotency keys are kept before the GC sweep expires them (`<=0` disables expiry) |
| `CAIRNMARK_ARCHIVE_MAX_ENTRIES` | `1000` | Max entries in a zip the archive endpoints will open; larger archives get `413` (`0` = the 10,000-entry ceiling below, not unbounded) |
| `CAIRNMARK_ARCHIVE_MAX_TOTAL_BYTES` | `1073741824` (1 GiB) | Max uncompressed bytes one extraction may write; a larger selection gets `413` (`0` = uncapped) |
| `CAIRNMARK_ARCHIVE_MAX_RATIO` | `100` | Per-entry uncompressed÷compressed ratio above which an entry is skipped as a zip bomb (`0` = uncapped). Deflate reaches ~1032:1, so the default also skips genuinely repetitive documents — a CSV of repeated values, a log, an XML export; raise it if that is your corpus |
| `CAIRNMARK_ARCHIVE_EXTENSIONS` | — *(all)* | Comma-separated allowlist of extensions to extract, e.g. `pdf,docx`; other entries are skipped |
| `CAIRNMARK_ARCHIVE_MAX_DIRECTORY_BYTES` | `0` *(derived)* | Bytes the zip's central directory may span; larger archives get `413` **before** the directory is parsed, which is the only bound that can precede the allocation. `0` does not disable it — it derives the bound from `CAIRNMARK_ARCHIVE_MAX_ENTRIES` (512 bytes per entry), which is right unless your paths are extremely long |
| `CAIRNMARK_JOB_CONCURRENCY` | `1` | Extraction jobs one replica runs at once. `1` on purpose: extraction is IO-bound on the store, two workers never share one archive anyway, and each in-flight run pins a read window plus a multipart part of memory. Raise it with evidence; must be `>= 1` |
| `CAIRNMARK_JOB_HEARTBEAT_TTL` | `5m` | How long a running job may go without a heartbeat before its worker is presumed dead and the job is returned to the queue. Too long and a crashed job blocks its archive for that long; too short and a healthy run is stolen mid-flight. Must be `> 0`; `0` is refused rather than meaning "disabled" |
| `CAIRNMARK_JOB_RETENTION` | `24h` | How long a finished job stays readable at `GET /jobs/{id}` before the GC sweep deletes it (`<=0` disables the sweep). After that the job is `404`; the extracted files are permanent |
| `CAIRNMARK_POSTGRES_DSN` | — *(required)* | pgx connection string |
| `CAIRNMARK_S3_ENDPOINT` | — *(required)* | Object store host:port for service I/O (no scheme) |
| `CAIRNMARK_S3_PUBLIC_ENDPOINT` | = endpoint | Host baked into presigned URLs (see note below) |
| `CAIRNMARK_S3_REGION` | `us-east-1` | S3 region |
| `CAIRNMARK_S3_ACCESS_KEY` | — *(required)* | Access key |
| `CAIRNMARK_S3_SECRET_KEY` | — *(required)* | Secret key |
| `CAIRNMARK_S3_BUCKET` | — *(required)* | Bucket (auto-created if absent) |
| `CAIRNMARK_S3_USE_SSL` | `false` | TLS for the service-side endpoint |
| `CAIRNMARK_S3_PUBLIC_USE_SSL` | = `USE_SSL` | TLS scheme for presigned URLs |

**Port note:** SeaweedFS serves S3 on `8333`; RustFS and MinIO both use `9000`.
The overlay sets both endpoint vars accordingly — `validateEndpoint` accepts any
`host:port`, so nothing else changes.

**Public endpoint:** the service may reach the store by an internal name
(`rustfs:9000` in Compose) that external clients can't resolve. Set
`CAIRNMARK_S3_PUBLIC_ENDPOINT` to a client-reachable host; presigned URLs are
signed against it directly (the host is part of the SigV4 signature and can't be
rewritten afterward).

## API

| Method | Path | Description |
|---|---|---|
| `POST` | `/files` | Upload (raw body). Returns `201` + JSON metadata. Optional `Idempotency-Key` header (see below). |
| `GET` | `/files/{id}` | Download. Default `302` → presigned URL; `Range:` → `206`; `?download=stream` → `200` verified stream. |
| `GET` | `/files/{id}/metadata` | Metadata as JSON. |
| `PATCH` | `/files/{id}/metadata` | Merge tags (or `?mode=replace`). Body is a JSON object. |
| `DELETE` | `/files/{id}` | Soft delete (`204`). Object purged asynchronously. |
| `GET` | `/files/{id}/archive` | List a zip's entries — index, size, content type, and whether each is extractable (and why not). Writes nothing. |
| `POST` | `/files/{id}/extract` | Submit an extraction job: store a zip's entries as files. Optional body `{"entries":[0,4]}` selects by index. Returns `202` + the job, `Location: /jobs/{id}`. |
| `GET` | `/jobs/{id}` | An extraction job's state: status, progress, and the bounded summary once it is done. |
| `POST` | `/jobs/{id}/cancel` | Ask a job to stop after the entry it is on (`202`). Idempotent; a finished job is returned unchanged. |
| `GET` | `/files` | List/search: `?content_type=`, `?tag.<k>=<v>`, `?entries=include\|exclude\|only`, `?limit=`, `?cursor=`. |
| `GET` | `/healthz` · `/readyz` | Liveness / readiness. |
| `GET` | `/metrics` | Prometheus exposition. |

**Upload inputs:** filename from `?filename=` or a `Content-Disposition` header;
content type from `Content-Type` (sniffed when absent); tags from `?tag.<k>=<v>`
query params and/or an `X-Metadata` JSON-object header (for typed/nested values).

**Idempotent uploads:** send an `Idempotency-Key: <unique-string>` header to make
a retried `POST /files` safe. The first request for a key uploads and records the
result; a retry returns the original `201` (with `Idempotency-Replayed: true`)
instead of creating a duplicate. A retry while the first is still in flight gets
`409 Conflict` with a `Retry-After` header saying when to ask again; if the file
the key produced has since been deleted, the retry gets `410 Gone` — switch to a
new key. Keys expire
after `CAIRNMARK_IDEMPOTENCY_TTL` (default 24h). Note: the payload is not
fingerprinted, so reusing a key with different content returns the original
result — keys must be unique per upload.

Example responses:

```jsonc
// POST /files  → 201
{
  "id": "0f8d2312-bf28-42b0-a7a7-0b57a94eba1d",
  "filename": "README.md",
  "content_type": "text/plain",
  "size_bytes": 4096,
  "checksum_sha256": "f0f5…1708",
  "metadata": { "project": "cairnmark" },
  "created_at": "2026-06-24T12:54:54Z",
  "updated_at": null
}

// GET /files?tag.project=cairnmark → 200
{ "files": [ /* …file objects… */ ], "limit": 50, "count": 1 }
```

`updated_at` is `null` until the file's metadata is first changed via `PATCH` —
a quick way to tell an untouched original from an edited record. `limit` defaults
to 50 (max 500) and the response echoes the value actually applied.

**Pagination** is keyset-based: a full page carries a `next_cursor` — pass it
back as `?cursor=` to fetch the files older than it. A page without
`next_cursor` is the last one. Cursors stay accurate under concurrent
uploads/deletes and don't slow down on deep pages the way `OFFSET` does.

### Archives (`.zip`)

An archive uploads like any other file. Extraction is a second, explicit call
against the stored object, so nothing is unpacked until you ask:

```sh
# 1. Upload the zip
curl -s -H "Content-Type: application/zip" --data-binary @docs.zip \
  "http://localhost:8080/files?filename=docs.zip"            # note the "id" → <archive-id>

# 2. See what is inside — nothing is written
curl -s "http://localhost:8080/files/<archive-id>/archive"

# 3. Submit an extraction of every extractable entry (or a selection:
#    -d '{"entries":[0,4]}'). 202: the run happens on the server's worker.
curl -s -X POST "http://localhost:8080/files/<archive-id>/extract"   # note the job "id" → <job-id>

# 4. Poll the job until its status is succeeded, failed or cancelled
curl -s "http://localhost:8080/jobs/<job-id>"
#    (changed your mind? POST /jobs/<job-id>/cancel stops it after the current entry)

# 5. Find the extracted documents, and download one
curl -s "http://localhost:8080/files?tag.cm:archive_id=<archive-id>"
curl -s "http://localhost:8080/files?tag.cm:archive_id=<archive-id>&tag.cm:archive_path=reports/q3.pdf"
curl -L "http://localhost:8080/files/<entry-id>" -o q3.pdf
```

The listing names each entry by its **index** in the archive's directory —
names need not be unique inside a zip, indexes are — and says whether it is
extractable and, if not, why:

```jsonc
// GET /files/{id}/archive → 200
{ "archive_id": "…", "entries": [
  { "index": 0, "name": "reports/q3.pdf", "size": 184322,
    "content_type": "application/pdf", "crc32": "8f2a91c4", "selectable": true },
  { "index": 1, "name": "__MACOSX/reports/._q3.pdf", "size": 220,
    "crc32": "d1c0b3aa", "selectable": false, "reason": "platform_metadata" }
] }

// POST /files/{id}/extract → 202 — the job, with Location: /jobs/{id}
{ "id": "…", "archive_id": "…", "status": "pending",
  "progress": { "done": 0, "total": 0 }, "cancel_requested": false,
  "summary": null, "created_at": "…", "updated_at": "…", "finished_at": null }

// GET /jobs/{id} → 200 — once terminal, the bounded summary; never the list of results
{ "id": "…", "archive_id": "…", "status": "succeeded",
  "progress": { "done": 118, "total": 118 }, "cancel_requested": false,
  "summary": { "archive_id": "…", "entries": 132, "extracted": 118, "skipped": 14,
               "skipped_by_reason": { "platform_metadata": 12, "encrypted": 2 },
               "sample_skipped": [ { "index": 7, "name": "…", "reason": "encrypted" } ] },
  "created_at": "…", "updated_at": "…", "finished_at": "…" }
```

A job is `pending`, then `running`, then `succeeded`, `failed` (with an
`error`) or `cancelled`; only those last three are final. A running job can
go back to `pending` if its worker shuts down or stops reporting — it is
picked up again and resumes — so poll until a final state, not until the job
leaves `running`. `progress` counts the entries the current run intends to
write, not the archive's entry count, and a resumed run counts only what
remained. A job id is a bearer capability, exactly as a file id is: there is
no listing, and whoever holds the id can poll and cancel it.

Each extracted entry becomes an ordinary file — its own id, SHA-256, and
content type (from the entry's extension, sniffed otherwise) — carrying three
service-written tags: `cm:archive_id`, `cm:archive_path` (the full path inside
the archive) and `cm:archive_index`. The archive row itself is stamped
`cm:archive=true`. The `cm:` prefix is **reserved**: an upload or `PATCH` that
tries to write it gets `400`, and a `PATCH ?mode=replace` on an extracted file
leaves its `cm:` tags in place.

Skip reasons: `directory`, `non_regular`, `platform_metadata` (`__MACOSX/`,
`._*`, `.DS_Store`, `Thumbs.db`), `encrypted`, `unsupported_method` (anything
but Store or Deflate — Deflate64, bzip2, LZMA, zstd and AES all occur in the
wild, and skip the entry rather than fail the archive), `unsafe_name`,
`too_large`, `ratio_exceeded`, `extension_not_allowed`, `corrupt` (the entry
failed its own CRC32), `not_selected`, `already_extracted` and
`previously_deleted`. Zero-byte entries are kept: an empty file is a
legitimate file.

Extraction is **resumable by construction**: a re-run skips every entry that
already has a live file (`already_extracted`), so a job that failed, was
cancelled, or was interrupted by a restart is simply submitted again and
completes the remainder. An extracted file you deleted stays deleted
(`previously_deleted`) for as long as its tombstone exists — until the GC
sweep purges it — so delete wins over re-extract. One archive has at most one
job pending or running at a time: a second submission gets `409` with a
`Retry-After` and, in the body, the `job_id` of the active job — the thing to
poll. `Idempotency-Key` is refused on this endpoint (`400`): the job id is
the idempotency handle, and a re-submission resumes.

Cancelling is cooperative: the worker finishes the entry it is writing, then
stops and reports `cancelled` with the partial summary. Nothing already
written is undone; submit again and it continues. A worker that dies leaves
its job `running` with a heartbeat that ages; once it is older than
`CAIRNMARK_JOB_HEARTBEAT_TTL` (default 5m) the job goes back to the queue and
the next pickup resumes it — on any replica, with no coordination beyond the
database. A clean shutdown parks a running job the same way, immediately.
Finished jobs are readable for `CAIRNMARK_JOB_RETENTION` (default 24h), after
which `GET /jobs/{id}` is `404`; a client that polls slower than that loses
the summary, though the extracted files themselves are permanent.

What the submission itself can refuse, it refuses at once, with the same
statuses a synchronous call gave: not a zip (`415`), a selection out of range
(`400`), over a cap (`413`), unknown (`404`). A job only fails on what could
not be known up front — the store or the database failing mid-run.

After an extraction, `GET /files` returns ordinary files, archives and
extracted entries together. `?entries=exclude` hides the extracted entries,
`?entries=only` shows just them, and `?tag.cm:archive=true` lists the archives.

Other statuses: `415` when the file is not a zip (both endpoints); `413` when
the archive has more entries than `CAIRNMARK_ARCHIVE_MAX_ENTRIES` or the
selection would write more than `CAIRNMARK_ARCHIVE_MAX_TOTAL_BYTES`. The entry
count is also capped at 10,000 regardless of configuration: the listing is a
single unpaginated response, so `CAIRNMARK_ARCHIVE_MAX_ENTRIES=0` raises the
cap to that ceiling rather than removing it. A `413` also comes from
`CAIRNMARK_ARCHIVE_MAX_DIRECTORY_BYTES`, refused before the central directory is
parsed — the entry count cannot be checked any earlier, since it does not exist
until the parse is done. Each
extracted entry is an upload, so `CAIRNMARK_MAX_UPLOAD_BYTES` caps it too. Only
`.zip` is supported — RAR, 7z, tar and nested archives are not.

The SDKs wrap all of it: `ArchiveEntries` / `Extract` in Go, `archiveEntries`
/ `extract` in Node, `archive_entries` / `extract` in Python — where `Extract`
submits, polls and returns the summary, so a caller that does not care that
the server is asynchronous never sees it — plus the job surface
(`ExtractAsync`, `Job`, `WaitForJob`, `CancelJob` and their equivalents), the
`entries` list scope and constants for the `cm:` tag keys. The contract they
follow is [`docs/sdk-contract.md`](docs/sdk-contract.md).

## Official SDKs

Hand-written clients for three languages, all exposing the same surface:
streaming uploads/downloads, typed errors, safe retries with idempotency
keys, lazy pagination, and client-side checksum verification (the piece raw
HTTP can't give you — presigned downloads bypass the service, so only the
client can compare the stored SHA-256). Each SDK's README is a complete
integration guide. SDK v1.0.0 requires this server release or later (the
archive and job methods need the `extraction_jobs` migration); everything
else in them works against ≥ v1.1.0.

| Language | Repo | Install |
|---|---|---|
| Go | [`cairnmark-go`](https://github.com/mettjs/cairnmark-go) | `go get github.com/mettjs/cairnmark-go` |
| Python (sync + async) | [`cairnmark-python`](https://github.com/mettjs/cairnmark-python) | `pip install cairnmark` |
| Node.js (TypeScript) | [`cairnmark-node`](https://github.com/mettjs/cairnmark-node) | `npm install cairnmark` |

### Go

```go
package main

import (
	"context"
	"fmt"
	"strings"

	cairnmark "github.com/mettjs/cairnmark-go"
)

func main() {
	ctx := context.Background()
	c, err := cairnmark.New("http://localhost:8080")
	if err != nil {
		panic(err)
	}

	// Upload with a tag; AutoIdempotency makes retries duplicate-safe.
	f, err := c.Upload(ctx, cairnmark.UploadInput{
		Body:            strings.NewReader("hello from Go"),
		Filename:        "hello.txt",
		ContentType:     "text/plain",
		Metadata:        map[string]any{"env": "demo"},
		AutoIdempotency: true,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("uploaded:", f.ID)

	// Download by id — follows the presign redirect and verifies the SHA-256.
	if _, err := c.DownloadToFile(ctx, f.ID, "hello-copy.txt"); err != nil {
		panic(err)
	}

	// Search by tag, lazily across pages.
	for f, err := range c.ListAll(ctx, cairnmark.ListFilter{Tags: map[string]string{"env": "demo"}}) {
		if err != nil {
			panic(err)
		}
		fmt.Println("found:", f.ID, f.Filename)
	}
}
```

### Python

An `AsyncCairnMark` twin exposes the same surface with `await`.

```python
from cairnmark import CairnMark

with CairnMark("http://localhost:8080") as cm:
    # Upload with a tag; idempotency_key="auto" makes retries duplicate-safe.
    f = cm.upload(
        b"hello from Python",
        filename="hello.txt",
        content_type="text/plain",
        metadata={"env": "demo"},
        idempotency_key="auto",
    )
    print("uploaded:", f.id)

    # Download by id — follows the presign redirect and verifies the SHA-256.
    cm.download_to_file(f.id, "hello-copy.txt")

    # Search by tag, lazily across pages.
    for file in cm.iter_files(tags={"env": "demo"}):
        print("found:", file.id, file.filename)
```

### Node.js

Zero runtime dependencies (global `fetch` + Web Streams, Node 18+), ESM.

```js
import { CairnMark } from "cairnmark";

const cm = new CairnMark("http://localhost:8080");

// Upload with a tag; idempotencyKey: "auto" makes retries duplicate-safe.
const f = await cm.upload("hello from Node", {
  filename: "hello.txt",
  contentType: "text/plain",
  metadata: { env: "demo" },
  idempotencyKey: "auto",
});
console.log("uploaded:", f.id);

// Download by id — follows the presign redirect and verifies the SHA-256.
await cm.downloadToFile(f.id, "hello-copy.txt");

// Search by tag, lazily across pages.
for await (const file of cm.listAll({ tags: { env: "demo" } })) {
  console.log("found:", file.id, file.filename);
}
```

## Architecture

Dependencies point inward; the HTTP layer never touches storage directly.

```
cmd/server        composition root (DI) — the only place concretes are built
internal/api      HTTP handlers + routing (stdlib net/http)
internal/files    service layer: orchestrates storage + metadata, owns the write path
internal/archive  zip walking over an io.ReaderAt: entries, skip rules, per-entry content
internal/storage  Backend interface  ──  storage/s3 (S3 client, the only backend)
internal/metadata Repository interface ── metadata/postgres (pgx; the only SQL)
internal/gc       background reconciliation: purge soft-deletes, reclaim orphans
internal/metrics  Prometheus metric definitions + the /metrics handler
internal/config   env loading (imported only by cmd/server)
migrations        embedded, versioned SQL (goose)
```

## Development

```sh
go test ./...                    # unit tests (in-memory backend + fake repo)
go test -tags=integration ./...  # against real Postgres + object store (see CONTRIBUTING)
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for conventions and the integration setup.

## License

[MIT](LICENSE) © 2026 Michael Ramirez
