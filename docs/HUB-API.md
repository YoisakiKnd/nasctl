# nasctl (OneCloud Hub) HTTP API v1

The console's HTTP surface. File roots default to read-only; explicit operator file actions, native anime operations,
browser authentication and governance controls have explicit write endpoints,
and local album processing has explicit bounded task endpoints. The web query
package cannot import decoders, scanners, the album store or subprocess code;
startup injects the local read-only reader and the supervised worker capability.

Default bind: `127.0.0.1:8090`. The bind address is the access control (see
`CLI-ACCESS.md` §3); a public address is refused at startup.

Evidence and the list of unverified items: `HUB-STAGE-B.md`.

## Conventions

* All JSON is `application/json; charset=utf-8`. Responses carry `no-store`.
* **No request accepts a server filesystem path.** Files are addressed by a
  storage `root` ID plus a root-relative `path`. The ID is one the operator
  configured; the path is resolved through `os.Root`, so a symlink that leaves the
  root is refused by the kernel.
* Every listing is paged and bounded. Every cap is reported when it is hit
  (`truncated`, `note`), so a partial answer is never presented as a complete one.
* Strings are transported verbatim, including `<`, `>` and `&`. A filename is
  data: rewriting it in transport would corrupt the name displayed and downloaded.
  The renderer is responsible for never parsing it as markup, and the frontend
  uses `textContent` exclusively.
* Errors: `{"error":"<code>","message":"<text>"}`. The `message` is safe to show
  and never contains a server path; the detail is logged instead.

### Status codes

| Code | Meaning |
| --- | --- |
| 400 | the request is malformed (bad ID, missing parameter, bad body) |
| 401 | a token is configured and was not supplied or was wrong |
| 403 | **refused**: outside the root, not a regular file, or a state that a caller may not set |
| 404 | the root, entry or job does not exist |
| 413 | the image is above the inline-preview cap (download it instead) |
| 415 | no in-browser preview exists for this type (download it instead) |
| 503 | the root is unavailable, or too many transfers are in progress |

**403 and 404 are deliberately distinct for a configured root but identical for a
traversal.** A caller probing for paths cannot use the status code to tell "you
may not" from "it is not there", because a traversal is refused the same way
whether or not the target exists.

### Caller identity

Identity comes from the **credential**, not from a claim in the request.

| Credential presented | Principal |
| --- | --- |
| a configured agent token (`hub.agents[].token`) | `ai:<name>` |
| `server.token` | `user:operator` |
| a named account session cookie | `user:<username>#<stable-id>` with current role |
| a legacy operator session cookie | `user:operator` |
| nothing, when no credential or required setup is configured | `user:operator` |
| nothing, when a credential *is* configured | `401` |

Agent tokens are checked **before** the operator token, so an agent cannot be
mistaken for the operator, and the token cannot be spoofed by a header. This
ordering is the point: the first version identified an agent only by a header it
sent about itself, so an agent could escape every restriction by omitting it.

The public shell, `/api/v1/auth/status`, and `POST /api/v1/auth/setup|login|logout`
support initial setup and browser login. Private data still requires
authentication. Session behaviour and request bodies are documented in
[AUTH.md](AUTH.md); native anime endpoints are in [ANIME.md](ANIME.md).
Operator file writes, including background copy, cross-root move, trash purge
and cancellation, are documented in [FILES.md](FILES.md). Background endpoints
return 202 with `job_id`; read `/api/v1/jobs/{id}` for final state, progress and
events. `cancellable` is true only for operator requests while the current
process owns that native task. Cancellation requires `X-Nasctl-Confirm: 1` and
does not accept process IDs or stop external downloaders.

The distinction has consequences: an agent's **reads are recorded in the audit
log** (an operator's browsing is not, because page loads would bury the rows that
matter), an agent may not start heavy work unless `hub.agents[].allow_heavy` is
set for it, and an agent may not grant itself an approval.

`X-Nasctl-Agent: <name>` is honoured **only when no credential is configured at
all**, as a convenience for a single-operator setup. It is not an authorisation
mechanism, and configuring any credential disables it. The name is sanitised to
`[A-Za-z0-9._:-]` and truncated to 64 characters in either case.

---

## GET /api/v1/overview

The front page's single request. Leads with what is wrong.

```json
{
  "version": "0.1.0",
  "now": 1791780150,
  "data_as_of": "2026-10-12T06:00:00Z",
  "mode": {"mode": "manual_pause", "source": "user:op", "reason": "machine not verified",
           "set_at": "...", "expires_at": "0001-01-01T00:00:00Z"},
  "all_modes": [{"mode": "watching", "source": "user:op", "expires_at": "..."}],
  "warnings": ["heavy work is stopped: mode \"manual_pause\" is in force (user:op)"],
  "attention": [{"id": "j1a2...", "op": "album.thumbs", "state": "interrupted",
                 "needs_attention": true, "stop_reason": "console restarted while this job was open"}],
  "running": [],
  "job_counts": {"completed": 12, "failed": 1},
  "files": {"roots": [{"id": "media", "label": "Media", "path": "/mnt/data/media",
                        "available": true, "expose_hidden": false}],
            "writable": false, "note": "storage roots default to read-only"},
  "resources": {"mem_available_kb": 3040708, "mem_total_kb": 8131752,
                "swap_used_kb": 0, "verdict": "allow", "reason": "ok",
                "psi_memory_some_avg10": 0.12},
  "read_only": false,
  "allow_ai_heavy": false,
  "locks": {"scan": "j1a2..."}
}
```

Notes that matter for a client:

* `warnings` and `attention` are **arrays, never null**. Absence is an empty list,
  so a client cannot render "not reported" as "healthy".
* `all_modes` is an empty array when no mode is set. `mode.mode` is `normal` with
  `source: "default"` in that case - not a zero timestamp.
* `resources` is omitted when the machine's memory cannot be read, and
  `resource_error` explains why. It is never zeros.
* `resources.verdict` is `allow`, `drain` or `stop`.
* `files.writable` reports whether any available root permits operator writes;
  each root also reports its own writable flag. Writes and bounds are documented
  in [FILES.md](FILES.md). Agent credentials remain read-only for these routes.

---

## GET /api/v1/ops

The operation table. This is the permission model, published.

```json
{"operations": [
  {"id": "files.list", "summary": "List one directory level", "scope": ["storage"],
   "class": "read", "approval": "none", "timeout": "10s", "idempotent": true,
   "async": false, "read_only": true,
   "params": [{"name": "root", "kind": "string", "required": true, "max_len": 32}]}
]}
```

`class` is `read`, `bounded`, `mutating` or `privileged`. `approval` is `none`,
`preauthorised` or `per_call`. An operation declared here without an implementation
returns "not implemented in this build" rather than "unknown operation", so a
caller can tell the two apart.

---

## Local backup

The native backup module provides confirmed, bounded plan creation, execution,
manifest verification, version history, restore without replacement and exact-ID
cleanup. It does not control an external backup service. See [BACKUPS.md](BACKUPS.md)
for endpoints, retention, storage-scope binding and privacy. Private offline
service-state packages and crash recovery are described in [STATE-BACKUPS.md](STATE-BACKUPS.md).

## Unified settings and filesystem capacity

`GET /api/v1/settings` returns sanitized saved/active settings, secret-presence
flags, a revision digest and the restart requirement. `POST /api/v1/settings/validate`
validates without saving. `POST /api/v1/settings` additionally requires explicit
confirmation and records an audited operator job. Read-only consoles and AI
credentials cannot change these settings. See [SETTINGS.md](SETTINGS.md).

Storage roots and overview include real filesystem capacity and a volume ID;
shared filesystems must not be added up. Unknown capacity is omitted with an
explicit reason. A replaced root is unavailable for both reads and writes.

## GET /api/v1/files/roots

Also returns `limits`: the effective local `[hub.files]` policy for listing,
search, preview, upload, trash, copy and batch selection. Requests may lower
budgets but cannot raise local limits. See [FILES.md](FILES.md) for all write
endpoints and CLI commands. `POST /api/v1/files/batch` accepts an explicitly
confirmed bounded selection, returns a job ID and persists per-item outcomes
in a `kind=files.batch` job event, including partial completion and cancellation.

```json
{"roots": [{"id": "media", "label": "Media", "path": "/mnt/data/media",
            "available": true, "expose_hidden": false, "note": ""}],
 "writable": false, "note": "..."}
```

An unavailable root carries a `note` explaining why. **An absent mount is reported
as unavailable, never as an empty directory**, so a missing array does not look
like deleted files.

---

## GET /api/v1/files/list

| Parameter | Type | Default | Notes |
| --- | --- | --- | --- |
| `root` | string | required | storage ID |
| `path` | string | `/` | root-relative; `/`, `""` and `.` all mean the root |
| `page` | int | 1 | |
| `page_size` | int | 50 | clamped to 200 |
| `sort` | enum | `name` | `name`, `size`, `time`, `kind` |
| `order` | enum | `asc` | `asc`, `desc` |

```json
{"root": "media", "path": "/photos", "parent": "/",
 "entries": [{"name": "IMG_0001.jpg", "path": "photos/IMG_0001.jpg", "is_dir": false,
              "size": 3145728, "mod_time": "2024-05-01T12:00:00Z",
              "mode": "-rw-r--r--", "symlink": false, "kind": "image"}],
 "page": 1, "page_size": 50, "total": 128, "pages": 3,
 "truncated": false, "sort": "name", "order": "asc"}
```

* Directories sort first regardless of `order`, and ties fall back to name so
  paging is stable - an entry cannot be skipped or duplicated across pages.
* `truncated` is true when the directory holds more entries than the console
  examines at once (20 000). `total` and `pages` are then **lower bounds**, and
  `note` says so.
* `kind` is advisory (for icons): `dir`, `image`, `video`, `audio`, `archive`,
  `text`, `other`. It never decides permission.
* `symlink` marks an entry that is a symbolic link. Such an entry is listed, and
  opening it follows the link only if it stays inside the root.

---

## GET /api/v1/files/stat

| Parameter | Type | Notes |
| --- | --- | --- |
| `root` | string | required |
| `path` | string | required |

```json
{"entry": {"name": "notes.txt", "path": "/notes.txt", "kind": "text", "size": 6,
           "mod_time": "...", "mode": "-rw-r--r--", "symlink": false},
 "preview_kind": "none", "previewable": false}
```

`previewable` is false when the browser cannot display the type. It describes
whether a **preview** is worth attempting; it never gates a **download**.

---

## GET /api/v1/files/search

| Parameter | Type | Default | Notes |
| --- | --- | --- | --- |
| `root` | string | required | |
| `path` | string | `/` | subtree to search |
| `q` | string | required | matched case-insensitively as a substring |
| `depth` | int | 2 | clamped to 6 |

```json
{"root": "media", "entries": [...], "examined": 431, "truncated": false,
 "elapsed_ms": 12}
```

`q` is a substring, **never a glob or a regular expression**: a search box that
accepts patterns invites a caller to write a quadratic one. The query is capped at
128 characters. When a cap is reached, `truncated` is true and `note` names which
one (`result cap`, `examined cap`, `depth cap`, `time cap`).

---

## GET /api/v1/files/preview

Streams a file for display **in the page**. The console does not decode, resize or
transcode: it passes the bytes through with a bounded buffer.

| Parameter | Type | Notes |
| --- | --- | --- |
| `root` | string | required |
| `path` | string | required |

* `Content-Type` is derived from the extension; `X-Content-Type-Options: nosniff`
  is always set.
* **Range requests are supported** (`Accept-Ranges: bytes`), answered with `206`
  and a correct `Content-Range`. This is what makes seeking in a video cost a
  range rather than the whole file.
* **Images are capped** at `PreviewMaxBytes` (32 MiB) and refused with `413` and an
  explanation: a browser renders an image whole, so a huge one would be pulled
  through the page. **Video and audio are not capped**, because they are ranged -
  capping them would make playback impossible.
* Types with no in-browser preview return `415`; download them instead.
* Bounded by the concurrent-stream limit (3). When full: `503` with `Retry-After`.

---

## GET /api/v1/files/download

Streams a file as an attachment. **No cap and no type restriction**, because the
user asked for the file and the console streams it with a bounded buffer: the cost
to this process is the same for 4 KiB and 4 GiB.

| Parameter | Type | Notes |
| --- | --- | --- |
| `root` | string | required |
| `path` | string | required |

* `Content-Type: application/octet-stream`, `Content-Disposition: attachment`
  with both an ASCII `filename=` and an RFC 5987 `filename*=UTF-8''...` form. The
  name comes from the filesystem, so it is escaped: a quote, backslash, control
  character or non-ASCII byte cannot inject a header.
* Range requests are supported, so an interrupted transfer can be resumed.
* `HEAD` is supported.
* Bounded by the concurrent-stream limit.
* The write deadline moves forward with each write: a transfer is dropped only
  after 30 s without progress, not after the server's total `write_timeout`.
  The same applies to previews, media playback and vault originals.

---

## GET /api/v1/files/archive

Streams a folder as a ZIP attachment (`<folder>.zip`; the root itself is named
after the root's label). Entries are stored, not compressed.

| Parameter | Type | Notes |
| --- | --- | --- |
| `root` | string | required |
| `path` | string | the folder; empty or `/` is the root |

* The folder is planned before any byte is sent. More than `copy_max_entries`
  entries, `copy_max_bytes` bytes or 32 levels is a `413 too_large`, never a
  truncated archive.
* Links and special files are skipped; hidden files follow the root's
  `expose_hidden`; internal names (trash, backup namespaces) are never included.
* No write lock is taken. A file that changes during the transfer drops the
  connection, so the browser reports a failed download instead of saving a
  mixed archive.
* Shares the concurrent-stream limit and the 30 s idle deadline.

---

## GET /api/v1/jobs

| Parameter | Type | Default | Notes |
| --- | --- | --- | --- |
| `state` | enum | — | one of the nine job states; an invented one is `400` |
| `op` | string | — | filter by operation |
| `open` | bool | false | only jobs that still hold a slot |
| `limit` | int | 50 | clamped to 200 |
| `offset` | int | 0 | |

```json
{"jobs": [{"id": "j1a2...", "op": "album.scan", "state": "running", "principal": "user:op",
           "target": "photos", "params": "collection=photos limit=10",
           "created_at": "...", "progress": 3, "total": 10, "unit": "files",
           "verified": false, "needs_attention": false}], "limit": 50, "offset": 0}
```

`needs_attention` is true for `failed`, `interrupted` and `unknown` - the states
where the console does not know what happened. It is computed server-side so a
client cannot accidentally render one of them as healthy.

---

## GET /api/v1/jobs/{id}

```json
{"job": {...}, "events": [{"at": "...", "from": "queued", "to": "running", "note": "admitted"}]}
```

The event trail is the job's history. A job that never ran cannot be recorded as
completed, so the trail is the evidence for what actually happened.

---

## GET /api/v1/media/status

The IWantWatchAnime integration. The console opens suzu's database **read-only**
(`mode=ro`, `_query_only`) and never writes it.

| Parameter | Type | Default |
| --- | --- | --- |
| `q` | string | — |
| `limit` | int | 50 |
| `offset` | int | 0 |

```json
{"configured": true, "db_path": "/opt/suzu/suzu.db", "tables": ["library", "..."],
 "library_count": 128, "task_count": 1,
 "library": [{"id": "ep01", "title": "Episode 01", "path": "/library/ep01.mkv",
              "size": 1234567, "playable": true, "note": ""}],
 "note": "read-only: the console reads suzu's database and never writes it"}
```

* `configured: false` means the integration is off, and `library` is absent. The
  console reports nothing rather than guessing at a library.
* `playable` is false when no `library_root` is configured, when the recorded path
  resolves outside it, or when the file is absent. `note` says which. **A path
  recorded in suzu's database does not get to name what the console opens.**
* An unreadable count is omitted, not reported as `0`.

---

## GET /api/v1/modes

```json
{"states": [{"mode": "watching", "source": "user:op", "reason": "a film",
             "set_at": "...", "expires_at": "..."}],
 "effective": {"mode": "watching", "source": "user:op"}}
```

`states` includes expired windows so the page can show a lapsed one rather than
pretending it never existed. `effective` is the mode that governs new work.

---

## POST /api/v1/modes

Sets a mode. Body (unknown fields are refused):

```json
{"mode": "watching", "duration": "2h", "reason": "a film"}
```

| `mode` | `duration` | Result |
| --- | --- | --- |
| `watching` | required, positive, ≤ 24h | a bounded window |
| `maintenance` | required, positive, ≤ 24h | a bounded window |
| `manual_pause` | **must be omitted** | durable until cleared |
| `resource_guard` | — | **`403`**: the machine's own state is not settable by a caller |

A window without a duration is `400`, because a forgotten window would stop
maintenance forever. A pause *with* one is `400`, because a pause that expires on
its own is not a pause.

Returns the same shape as `GET /api/v1/modes`.

---

## DELETE /api/v1/modes/{mode}

Clears `watching`, `maintenance` or `manual_pause`. `resource_guard` returns
`403`: it clears when the resource cause is resolved, not by request. Returns the
same shape as `GET /api/v1/modes`.

---

## GET /api/v1/approvals

```json
{"approvals": [{"id": "a1b2...", "op": "album.scan", "principal": "ai:hermes",
                "target": "photos", "params": "collection=photos limit=10",
                "granted_by": "user:op", "granted_at": "...", "expires_at": "...",
                "used_at": "0001-01-01T00:00:00Z", "one_shot": true}]}
```

An approval is bound to one operation, principal, target and **exact parameter
string**. Changing a parameter needs a new approval.

---

## GET /api/v1/audit

| Parameter | Type | Default | Notes |
| --- | --- | --- | --- |
| `limit` | int | 100 | clamped to 500 |
| `offset` | int | 0 | |

```json
{"entries": [{"at": "...", "principal": "ai:hermes", "action": "read",
              "op": "GET /api/v1/files/list", "target": "/api/v1/files/list",
              "params": "root=media", "outcome": "ok", "note": ""}]}
```

`action` is `call`, `read`, `mode.set`, `mode.clear`, `approval.grant` or
`approval.revoke`. `outcome` is `ok`, `refused` or `failed`.

**A refusal is recorded.** A refusal with no trace is indistinguishable from a call
that never happened, which is exactly what an investigation needs to tell apart.

---

## Routes that do not exist

There is no operation, and no route, that:

* executes a shell or any program;
* writes, moves, renames, copies or deletes a file;
* starts a scan, a thumbnail batch or any decode;
* changes the backup, the scheduler or another service;
* accepts an arbitrary server path.

A request to an unknown `/api/` path returns **`404` with a JSON body** - not the
HTML shell, so a client cannot mistake a missing endpoint for a working one.

## Native album browser

The authenticated `/api/v1/album` reader reads the in-process local index. It offers status (with
mode/index_ready), collections, bounded cursor listings,
media details, cached thumbnails and original-file Range/HEAD/attachment streams.
Configuration, query parameters, credentials and transfer limits: [ALBUM.md](ALBUM.md).
Browsing never launches indexing or thumbnail work.

Local operator management: `GET /settings`, `POST /directories`,
`DELETE /directories/{code}`, `POST /tasks/{scan,thumbs,pause,resume}`.
Write requests require `X-Nasctl-Confirm: 1`; scan/thumbs require collection,
limit (1–10000), and max_runtime (1s–2h). Tasks return job_id and use the shared
job monitor/cancel endpoints. Worker/AI/read-only constraints and configuration
fields are documented in [ALBUM.md](ALBUM.md).

### Private directories

An album directory with `"private": true` is left where it is and hidden until
the vault is unlocked. The ordinary routes behave as if it did not exist; its
media is served only under `/api/v1/album/private/` (`collections`, `media`,
`media/{id}`, `media/{id}/{thumb|raw|download}`), which answer a device manager
whose browser carries an unlocked vault cookie (`nasctl_photo_vault`, path
`/api/v1`). Every private read moves the 15-minute idle lock forward.

The file API applies the same rule to the storage location of each private
directory, judging both the requested path and the path it resolves to through
links. While locked, listings, searches and the trash leave the directory out,
and requests that name a path inside it return `403 vault_locked`. Moving,
renaming, trashing, copying or zipping a private directory or any folder that
contains one returns `409 private_folder`, locked or not. Setting, changing or
removing a private directory through `POST /directories` or
`DELETE /directories/{code}` also requires the unlocked vault.
`GET /api/v1/album/vault/status` reports `private_folders`, the number of
private directories. Details: [PHOTO-VAULT.md](PHOTO-VAULT.md#私密目录).
