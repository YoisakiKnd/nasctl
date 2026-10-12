# OneCloud Hub — stage A design

Read `HUB-AUDIT.md` first: it establishes what actually exists. This document is the
design that follows from it. Stage A produces *design only* — no deployment, no
migration, no file writes, no scheduler.

---

## 1. Shape of the product

One console, one UI, one business layer — but **not** one process doing everything.
The failure this design is built around is the one that already happened: a heavy
task on a 981 MiB machine took the whole box down. So the unifying rule is:

> Unify the *product and the interfaces*. Never unify the *blast radius*.

Concretely:

| Process | Runs as | Can do | Cannot do |
| --- | --- | --- | --- |
| `nasctl serve` — console | `onecloud-hub`, no root | read-only queries, render pages, write its own job/mode/audit state, start *approved* bounded jobs | decode images, scan the library, upload, stream from disk in a request beyond a bounded copy |
| `nasctl scan` / `nasctl thumbs` — bounded jobs | same user, own cgroup | one bounded unit of work each | touch files, run unbounded |
| `nasctl exec-worker` — media worker | same user, own cgroup | decode one file, exit | anything else |
| `suzu` (existing) | `suzu` | its own subscriptions, downloads, archive | be driven by us beyond its own API |
| `rclone` (existing tool) | separate, supervised | transfer | be reimplemented |

The console can be restarted without touching a running job; a crashed worker cannot
take the console with it; nothing in the console holds a transaction across a
request.

---

## 2. Module map

```
cmd/
  nasctl/            the unified console: serve, status, files, media, album, backup, jobs, mode, ops
  albumctl/          unchanged compatibility entry point (same binary family)

internal/
  domain/            entities + invariants, no I/O: StorageRoot, Entry, Job, Mode,
                     Operation, Approval, MediaItem, WatchState
  operations/        the operation registry: schema, scope, permission, risk,
                     approval requirement, timeout, idempotency, async semantics,
                     mutex class, audit record. One registry, used by CLI, HTTP and
                     (later) MCP. No second permission path.
  jobs/              job records, state machine, reconciliation against reality,
                     scheduling decisions (idempotent coordinator), retry/backoff
  files/             self-built file manager: navigation, listing, search, stat,
                     download, preview, (stage E) upload/mkdir/rename/move/copy/trash
  album/             thin adapter over the existing albumctl packages
                     (store/scan/thumbs/resource/taskctl/source) — no fork
  media/             read-only adapter over suzu's database + its write API for
                     subscribe/toggle/backfill; library listing and playback
  backup/            adapter: status, plan, progress, verification. Wraps rclone and
                     whatever controls it; never reimplements the protocol
  mode/              normal | watching | maintenance | manual_pause | resource_guard:
                     source, expiry, recovery semantics, and the single policy gate
  hubweb/            unified pages, design tokens, embedded static assets, HTTP API
  hubconfig/         hub configuration on top of the existing config package
  cli/               nasctl verbs; reuses the same operations registry
  (existing)         config cachepath exif fsutil logx mimetype placeholder probe
                     resource scan server source store synth taskctl thumbs webstate
```

Rules the layering enforces (and a test will check, the way `server/arch_test.go`
already checks albumctl's):

* `domain` imports nothing from this module.
* `operations` and `jobs` may import `domain` and `store`; they may not import `hubweb`.
* `files`, `album`, `media`, `backup` are adapters: they may import `domain`,
  `operations`, `jobs`; they may not import each other.
* `hubweb` and `cli` call the registry; they never implement permission or
  scheduling themselves.
* Nothing under `hubweb` may import an image decoder, `os/exec`, or the scanner.

---

## 3. Data flow

Read paths (stage B):

```
browser ──HTTP──▶ hubweb ──▶ operations.Lookup(op) ──▶ permission check ──▶ files/album/media/backup
                                   │                                              │
                                   └──▶ audit record ◀───────────────────────────┘
                                                                                  │
                                    files: storage root + bounded walk of ONE directory level
                                    album: existing albumctl store (paged SQL)
                                    media: suzu.db opened read-only (paged SQL)
                                    backup: backupctl/rclone status (or `unknown`)
```

Write paths (stage E and later), all asynchronous and all approved:

```
caller ──▶ operations.Lookup ──▶ approval check ──▶ jobs.Create (queued, persistent)
                                                        │
                          policy gate (mode + resources + backup + mutex) ──▶ runner
                                                        │
                                             progress/result persisted
```

The console never performs a heavy action inline. A request either (a) returns data
it can answer from a bounded query, or (b) creates a job record and returns its ID.
There is no third option.

---

## 4. Resource budget (targets, to be measured, never asserted)

| Process | Idle target | Ceiling candidate | Notes |
| --- | --- | --- | --- |
| console (`nasctl serve`) | ≤ 40 MiB RSS | MemoryHigh 72 MiB / MemoryMax 96 MiB (provisional) | task's target is 40 MiB. Measured: 12.8 MiB idle on amd64, 19.5 MiB after streaming 30 GB; but ~47.6 MiB under qemu-arm-static, dominated by an executable mapping that is almost certainly qemu's JIT cache. The ceiling is provisional until measured on the target — see `HUB-STAGE-B.md` §5.2 |
| one media worker | — | MemoryHigh 96 MiB / MemoryMax 128 MiB | unchanged from albumctl |
| scan / thumbs job | measured in `RESOURCE-REPORT.md` | inherits albumctl ceilings | one worker, concurrency 1 |
| suzu | unchanged | MemoryMax 192 MiB (existing unit) | not ours to change |
| rclone | per-run, not resident | operator-set | not ours to change |

Admission is a conjunction, not a percentage: `MemAvailable` sustained above the
start threshold, no swap activity, PSI below its limit, the job's own cgroup below
its ceiling, mode allows the work, no conflicting job holds the mutex class, and no
backup is busy. Unknown on any input means *refuse*, not *proceed*. The machine-wide
reservation (981 MiB minus OS minus Samba/FileBrowser/suzu/containers) is what the
hub's ceilings must fit inside; the numbers above are candidates until measured.

Memory accounting is per process tree and per cgroup, never "the console's RSS".
The 40 MiB target is a measurement goal and will be reported as measured-or-not.

---

## 5. Interfaces

### 5.1 Operation registry (the one permission and audit path)

Every action — CLI, HTTP, later MCP — is an entry:

```go
type Operation struct {
    ID          string        // "files.list", "media.library", "backup.status"
    Summary     string
    Params      ParamSpec     // declared schema; unknown params are rejected
    Scope       Scope         // storage IDs / collection IDs / job IDs it may touch
    Class       Risk          // read | bounded | mutating | privileged
    Approval    ApprovalRule  // none | preauthorised | per-call | two-person
    Timeout     time.Duration
    Idempotent  bool
    Async       bool          // true => returns a job ID, never blocks
    Mutex       MutexClass    // e.g. "scan:<collection>", "backup", "media:<root>"
    Audit       AuditLevel
}
```

Stage B registers only `read` operations. The registry is still the only path, so
stage E adds entries rather than a second mechanism.

### 5.2 Job record and state machine

```
queued ──▶ running ──▶ completed
   │          │  ▲
   │          │  └── waiting ──▶ running      (waiting for a gate, not for a human)
   │          ├──▶ paused ──▶ running          (pause is a state, not a flag)
   │          ├──▶ failed
   │          ├──▶ cancelled
   │          └──▶ interrupted ──▶ (reconciled on startup)
   └──▶ cancelled
unknown  — reported when the record cannot be reconciled with reality
```

`interrupted` is what a job becomes when the console died while it was running.
`unknown` is what a job becomes when reconciliation cannot decide. **`unknown` must
never render as normal** (task §七: "未知不能显示正常"). Reconciliation at startup
compares the record against the real process/cgroup, not against a PID file alone.

Recorded per job: operation, caller, target, parameter summary, times, the real
executing instance (pid/cgroup), phase, progress, exit reason, error, verification
result.

### 5.3 Mode

```
normal ⇄ watching (expires)      watching ⇄ resource_guard (automatic, takes precedence)
normal ⇄ maintenance (expires)   manual_pause (human only, overrides everything but safety)
```

Each mode carries `source` (who set it), `expires_at`, and `recovery`: what happens
when it ends. Precedence: safety > manual_pause > mode expiry > schedule. A mode
never kills a process and never edits a timer — it changes what *the hub* is willing
to start, plus, for `watching`, an rclone bandwidth limit passed as a flag on the
next run (never a change to existing schedules).

### 5.4 HTTP API (stage B subset)

```
GET  /api/v1/hub/status          console, jobs, modes, resources, unknown flags
GET  /api/v1/hub/operations      the registry, with risk and approval class
GET  /api/v1/files/roots         configured storage roots (IDs, labels)
GET  /api/v1/files/list?root&path&page&per_page&sort&order&q
GET  /api/v1/files/stat?root&path
GET  /api/v1/files/raw?root&path          bounded stream, Range supported
GET  /api/v1/files/preview?root&path      image passthrough with a byte cap
GET  /api/v1/media/library?page&per_page
GET  /api/v1/media/play/{id}              Range stream of one archived episode
GET  /api/v1/album/collections | /api/v1/album/media (delegates to albumctl's store)
GET  /api/v1/backup/status                service/job/coverage, or unknown
GET  /api/v1/jobs?state&page&per_page
GET  /api/v1/modes
```

All responses are bounded: `per_page` has a hard maximum, search has a result and
time cap, and every list endpoint is paginated in SQL, not in memory.

### 5.5 CLI (`nasctl`)

```
nasctl serve                     the console
nasctl status                    console + jobs + modes + resources
nasctl doctor                    read-only configuration/mount/permission check
nasctl files roots|ls|stat       read-only navigation
nasctl media library|show        read-only library view
nasctl album collections|media   read-only album view (delegates)
nasctl backup status             status or unknown
nasctl jobs list|show            job records
nasctl mode show|set             mode state
nasctl ops list                  the registry
nasctl version
# compatibility, unchanged:
nasctl albumctl <verb> …         or keep invoking the albumctl binary
```

---

## 6. Permission model

Four principals, deliberately separated (task §十):

| Principal | Authenticates with | May do |
| --- | --- | --- |
| human user | session cookie (console) or bearer token (API) | everything their role allows, including approvals |
| AI agent | its own token, distinct from the user's | read operations, plus explicitly pre-authorised bounded ones |
| console process | local only | what the user asked for; never approves its own requests |
| privileged helper | fixed operation list + validated fixed targets | exactly those operations; no arbitrary command, path or unit name |

Default for the AI principal: **read-only**. Pre-authorisable: time-boxed pause,
time-boxed watching mode, bounded scan/thumbnail batches with explicit count and
runtime, cancelling cancellable hub jobs. Needs its own approval: uploads, enabling
or changing schedules, service restarts, widening a library, moving/overwriting/
deleting files, cache pruning.

An approval is bound to (operation, target, parameter hash, caller, expiry,
single-use ID). Any parameter change invalidates it. `approved=true` in a request,
or an agent asserting the user consented, is **not** an approval. An AI credential
cannot approve its own request. Secrets never appear in pages, logs or AI responses.

---

## 7. Data structures (hub-owned, in the existing state directory)

```sql
-- hub jobs: the record of every asynchronous request
CREATE TABLE IF NOT EXISTS jobs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  op_id         TEXT    NOT NULL,
  caller        TEXT    NOT NULL,          -- "user:alice", "ai:hermes", "cli:local"
  target        TEXT    NOT NULL DEFAULT '',
  params_hash   TEXT    NOT NULL DEFAULT '',
  params_json   TEXT    NOT NULL DEFAULT '{}',
  state         TEXT    NOT NULL DEFAULT 'queued',
  phase         TEXT    NOT NULL DEFAULT '',
  progress      REAL    NOT NULL DEFAULT 0,
  instance_pid  INTEGER NOT NULL DEFAULT 0,
  instance_cgroup TEXT  NOT NULL DEFAULT '',
  exit_reason   TEXT    NOT NULL DEFAULT '',
  error         TEXT    NOT NULL DEFAULT '',
  verification  TEXT    NOT NULL DEFAULT '',
  approval_id   INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL DEFAULT 0,
  started_at    INTEGER NOT NULL DEFAULT 0,
  updated_at    INTEGER NOT NULL DEFAULT 0,
  ended_at      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_jobs_state ON jobs(state, id DESC);
CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at DESC);

CREATE TABLE IF NOT EXISTS job_events (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  at     INTEGER NOT NULL DEFAULT 0,
  kind   TEXT    NOT NULL DEFAULT '',
  detail TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_job_events ON job_events(job_id, id);

-- approvals: bound, expiring, single-use
CREATE TABLE IF NOT EXISTS approvals (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  op_id        TEXT    NOT NULL,
  target       TEXT    NOT NULL DEFAULT '',
  params_hash  TEXT    NOT NULL DEFAULT '',
  granted_by   TEXT    NOT NULL,
  granted_at   INTEGER NOT NULL DEFAULT 0,
  expires_at   INTEGER NOT NULL DEFAULT 0,
  used_at      INTEGER NOT NULL DEFAULT 0,
  single_use   INTEGER NOT NULL DEFAULT 1
);

-- audit: append-only, one row per operation attempt, allowed or refused
CREATE TABLE IF NOT EXISTS audit (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  at         INTEGER NOT NULL DEFAULT 0,
  caller     TEXT    NOT NULL,
  op_id      TEXT    NOT NULL,
  target     TEXT    NOT NULL DEFAULT '',
  params_sum TEXT    NOT NULL DEFAULT '',   -- redacted summary, never secrets
  decision   TEXT    NOT NULL,              -- allowed | refused | error
  reason     TEXT    NOT NULL DEFAULT '',
  job_id     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit(at DESC);

-- modes: current state plus history
CREATE TABLE IF NOT EXISTS modes (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT    NOT NULL,
  source     TEXT    NOT NULL,
  reason     TEXT    NOT NULL DEFAULT '',
  set_at     INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL DEFAULT 0,
  ended_at   INTEGER NOT NULL DEFAULT 0
);

-- watch state: NEW. suzu has none, so nothing is migrated into these tables.
CREATE TABLE IF NOT EXISTS watch_state (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  media_id   TEXT    NOT NULL UNIQUE,       -- suzu library row id, namespaced
  position_s REAL    NOT NULL DEFAULT 0,
  duration_s REAL    NOT NULL DEFAULT 0,
  watched    INTEGER NOT NULL DEFAULT 0,
  favorite   INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);

-- storage roots: IDs the user configures; paths never come from a request
CREATE TABLE IF NOT EXISTS storage_roots (
  id         TEXT PRIMARY KEY,
  label      TEXT NOT NULL,
  path       TEXT NOT NULL,
  fsid       TEXT NOT NULL DEFAULT '',
  enabled    INTEGER NOT NULL DEFAULT 1,
  writable   INTEGER NOT NULL DEFAULT 0,   -- stage E only; false in B
  created_at INTEGER NOT NULL DEFAULT 0
);
```

Same storage technology as albumctl and suzu (SQLite via `modernc.org/sqlite`), WAL,
integer/text only, every list query indexed and paged. The hub's tables live in the
hub's own file; suzu's database is opened read-only and never written by the hub.

---

## 8. Unified UI

Navigation (task §九), one level, identical on mobile and desktop:

```
概览  |  文件  |  看番  |  相册  |  备份  |  任务  |  设置
```

**概览** answers, in this order: what is at risk (unbacked-up data), what is running
now, why work is stopped, whether resources are abnormal, and when each figure was
last updated. Status vocabulary is fixed and never collapsed: `未知`, `已接受`,
`运行中`, `等待`, `已暂停`, `完成`, `失败`. A single green banner is forbidden: the
overview shows per-area state, and one failed area must be visible while others are
fine.

**Design tokens** (`hubweb/static/tokens.css`), one source for both themes:

```css
:root {
  --bg, --surface, --surface-2, --border,
  --text, --text-dim, --accent, --ok, --warn, --danger, --unknown,
  --radius-s/m/l, --space-1..8, --font-ui, --font-mono,
  --shadow-1, --focus-ring
}
[data-theme="light"] { … }
[data-theme="dark"]  { … }   /* default: dark */
```

No framework, no remote font, no icon CDN, no analytics, no map, no chart library.
Icons are inline SVG. The shell is server-rendered HTML with a small amount of
progressive enhancement, so a phone on Tailscale gets a usable page in one round
trip. Lists paginate; images lazy-load; nothing preloads a whole directory.

Layout sketch (desktop / mobile):

```
┌─────────────────────────────────────────────┐
│ OneCloud Hub      [mode] [theme] [health]   │
├──────────┬──────────────────────────────────┤
│ 概览      │  breadcrumb: 数据盘 / 照片 / 2019 │
│ 文件      │  ┌──────────────────────────────┐ │
│ 看番      │  │ name        size   mtime  ▾  │ │
│ 相册      │  │ …rows (paged)…               │ │
│ 备份      │  └──────────────────────────────┘ │
│ 任务      │  page 1/N   sort   search         │
│ 设置      │                                   │
└──────────┴──────────────────────────────────┘
mobile: header + tab bar, same order, same words
```

---

## 9. Integration and migration plan, with rollback

### 9.1 suzu (看番)

**Plan**: option A from the audit.

1. Hub reads `suzu.db` read-only (`mode=ro`, WAL permits concurrent readers) for
   library/subscription/task/event lists, with its own paged queries.
2. Hub uses `GET /healthz` as the liveness signal for "suzu is up".
3. Any *write* (subscribe, toggle, backfill, poll, task control) goes through suzu's
   own HTTP API with `X-Api-Token`. The hub never writes that database.
4. Watch state (progress, favourites, history) is new hub data in `watch_state`,
   keyed by suzu's library row id. **Nothing is migrated**, because nothing exists.
5. suzu keeps its own config, unit and data directory. No file is moved, no schema
   is changed, no unit is edited.

**Rollback**: stop the hub. suzu is untouched and keeps working; the hub's own
`watch_state` is additive and can be dropped without touching suzu.

**Not in this plan**: forking suzu's code, merging its scheduler, or having the hub
write its database. If you later want a single process, that is a separate,
explicitly approved refactor.

### 9.2 albumctl (相册)

**Plan**: reuse in place. The hub's album module calls the existing packages; the
existing `albumctl` binary keeps working; the existing index and cache directories
are read (and, for scan/thumbs, written only by the existing, tested code paths).

**Rollback**: nothing to roll back — no data is moved or converted. `nasctl album`
can be dropped without affecting `albumctl`.

### 9.3 backup

**Plan**: adapter only. `nasctl backup status` reports what the tooling can prove:
service state, job phase, live progress, last completed, last verified, pending
range, and when each figure was measured. Until `backupctl` is audited (§5 of the
audit), everything it would report is `unknown`, and the overview says `未知` rather
than green. No upload is started, no flag is changed, no schedule is touched.

**Rollback**: not applicable — stage B performs no backup action at all.

### 9.4 FileBrowser

**Plan**: untouched and running. It remains the fallback entry point. The hub's own
file module is separate code with its own roots and its own permission checks. No
proxy, no iframe, no shared session, no config import.

**Rollback**: nothing to do — FileBrowser was never modified.

---

## 10. Stage B scope (what gets built next)

In scope:

* `cmd/nasctl` with `serve`, `status`, `doctor`, `files`, `media`, `album`, `backup`,
  `jobs`, `mode`, `ops`, `version`;
* `domain`, `operations` (read-only entries), `jobs` (records + reconciliation, no
  new heavy jobs), `files` (read-only: roots, list, stat, search, download, preview
  with Range), `album` (read-only delegate), `media` (read-only suzu adapter),
  `backup` (status or unknown), `mode` (state + display), `hubweb` (shell, tokens,
  nav, pages), `hubconfig`;
* the unified UI with the seven sections, dark/light tokens, responsive;
* tests for: path traversal, symlink escape, permission bypass, secret leakage,
  unknown-not-normal, pagination bounds, Range correctness, search caps;
* ARMv7 artefact, checksums, reproducible build, resource report for the console.

Explicitly **not** in stage B: upload, mkdir, rename, move, copy, trash, restore,
any file write; starting scans or thumbnail batches; backup actions; scheduler
enablement; MCP transport; watch-state writes beyond the schema.

### 10.1 What stage B actually delivered

Built and verified (see `docs/HUB-STAGE-B.md` for the evidence):

| Area | Package | State |
|---|---|---|
| Vocabulary: modes, job states, principals, risk | `internal/domain` | done |
| Operation registry and validation | `internal/operations` | done |
| The permission table (42 operations) | `internal/hubops` | done |
| Admission, modes, approvals, audit | `internal/jobs` | done |
| Own file manager, `os.Root` containment | `internal/files` | done |
| Own job/audit/mode/approval database | `internal/hubstore` | done |
| Unified API + UI | `internal/hubweb` | done |
| Read-only suzu adapter | `internal/suzu` | done |
| CLI | `internal/nasctl` | done |
| systemd unit | `systemd/nasctl-web.service` | done |

Deliberately left for later stages, and declared so that a caller gets "not
implemented in this build" rather than "unknown operation":

* every file write (`files.upload`, `mkdir`, `rename`, `copy`, `trash`, `restore`);
* `schedule.enable` / `schedule.disable`;
* starting a scan or a thumbnail batch from the console (`album.scan`,
  `album.thumbs` are declared with the right bounds but have no handler here: they
  belong to `albumctl`, which is the process with the decoder and the guard);
* watch-state writes (`media.progress`, `media.favourite`) - the adapter is
  read-only, and suzu remains the only writer of its own state;
* the MCP transport. The registry it would need is already the single source of
  policy, so the transport adds no new permission logic.

Two decisions from §11 were resolved by building rather than by asking, because
neither changes what runs on the machine:

1. **Repository placement**: the hub is built inside this repo, with `cmd/nasctl`
   added and `albumctl` untouched. Both binaries come from one `go build`, so the
   shared packages cannot drift apart.
2. **suzu integration**: the read-only adapter was built. The write-through API is
   declared (`media.progress`, `media.favourite`) but not implemented, and the
   database is opened with `mode=ro` plus `_query_only`, so this build cannot write
   suzu's state even by mistake.

---

## 11. Open decisions for you

1. **Repository placement** (§3): hub built inside this repo with `cmd/nasctl` added
   and `albumctl` kept. Confirm, or ask for a rename to `onecloud-hub`.
2. **suzu integration** (§9.1): read-only adapter + write-through API. Confirm, or
   ask for the code-merge variant.
3. **看番 feature set**: playback, watch progress, favourites and history do not
   exist upstream. Confirm they are wanted as *new* hub features (they are the bulk
   of stage D), or drop them and keep 看番 as "browse + subscribe + download status".
4. **Stage E file writes**: the design keeps `writable = 0` for every root until you
   approve a specific root and a specific operation set.
5. **Target access** (§5 of the audit): authorize a read-only session, or run the
   recon commands and send the output, before any statement about the production
   environment is treated as fact.