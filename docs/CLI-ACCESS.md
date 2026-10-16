# nasctl album — CLI, album API and permissions

This is the operator-facing description of what exists, what it is allowed to
touch, and what it deliberately cannot do. Routes and payloads are specified in
`API.md`; operating procedures are in `RUNBOOK.md`; measurements are in
`RESOURCE-REPORT.md`.

---

## 1. Verbs

Every verb is bounded, reversible and manual. There is no verb that deletes,
renames, moves or rewrites an original, and none that starts an unbounded job.

| Command | What it does | Writes | Exit codes |
| --- | --- | --- | --- |
| `nasctl album status [--json]` | web service state, index totals, pending counts, current lock, pause and trip state, machine memory | nothing | 0; 1 on a broken config or unreadable index |
| `nasctl album scan --collection NAME --limit N [--dry-run] [--json]` | bounded directory + metadata pass | index only (`--dry-run`: nothing) | 0; 1 on config/usage error |
| `nasctl album thumbs --collection NAME --limit N --max-runtime D` | up to N thumbnail files within D | index + cache | 0 finished; 3 stopped by policy (pause, trip, backup busy, lock, operator, no isolation); 4 stopped by a resource reason |
| `nasctl album pause [--reason TEXT]` | stop heavy work durably | pause flag | 0 |
| `nasctl album resume [--force]` | clear the human pause (and, with `--force`, a pressure trip) | pause flag | 0 |
| `nasctl album doctor [--json] [--probe-writes]` | read-only health check: config, mounts, permissions, cache space, resource limits | nothing (unless `--probe-writes`) | 0 clean; 1 with problems |
| `nasctl album cache-report [--json]` | cache bytes, file count, ceiling and percentage | nothing | 0 |
| `nasctl album cache-gc [--max-age D] [--limit N] [--dry-run]` | remove *this application's own* cache files older than the age | cache only | 0 |
| `nasctl album collections [--json]` | the configured collections and their verification state | nothing | 0 |
| `nasctl album version` | version, target and runtime | nothing | 0 |
| `nasctl exec-worker …` | internal: the isolated decode child. Not an operator interface | cache | internal |

The three that matter for safety are the ones with a cap: `scan` and `thumbs`
always take a `--limit`, `thumbs` also takes `--max-runtime`, and neither has a
"do the whole library" mode. The album API that `nasctl serve` mounts never starts
work of its own — it has no code path to a decoder, a scanner or a child process,
and that is enforced by a test that walks the package graph.

`thumbs`'s exit code is meant to be read by a scheduler: **0** for a run that did
its job (including "the queue was empty"), **3** for a policy stop that a human
should decide about, **4** for a resource stop that must not be retried in a loop.
See `RUNBOOK.md` §3 for what to do with a 4.

---

## 2. Files and permissions

| Path | Owner | Mode | Who writes |
| --- | --- | --- | --- |
| media roots (`/mnt/data/photos`, …) | your data | your choice | **nobody, ever** |
| `state_dir` (`/var/lib/albumctl`) | `albumctl` | `0750` | scan, thumbs |
| `cache_dir` (`/var/cache/albumctl`) | `albumctl` | `0750` | thumbs, cache-gc |
| `/etc/albumctl/albumctl.toml` | `root` | `0640` | you |
| `/etc/systemd/system/nasctl-*.service` | `root` | `0644` | you |

Inside `state_dir`: `index.sqlite3` plus its WAL/SHM sidecars, `web.json` (the web
service's own record of itself: pid, bind address, start time, version, schema),
the task lock, and the pause/trip flags. Everything the application needs to
survive a restart lives there in a file, not in RAM.

Practical consequences:

* The service account needs **read+execute on the media roots** and **write on
  `state_dir` and `cache_dir`**, and nothing else. If the media tree is mounted
  read-only, that is an improvement, not a problem.
* `doctor` checks this without writing anything: with `--probe-writes` it does a
  create/delete probe, but the default uses permission bits and `mountinfo`, so it
  is safe to run against a live library. `doctor` reports a media root that is
  inside `state_dir` (or vice versa) as a problem rather than proceeding.
* The web service runs unprivileged and holds the index **read-only**: its handle
  refuses writes at the SQLite level, so a bug in the service cannot corrupt the
  index, and the index is not blocked for a running scan.
* The cache is pruned only by `nasctl album cache-gc`, only inside `cache_dir`, only
  files matching this application's own hash-shaped names. There is no route, verb
  or flag anywhere that removes an original.

---

## 3. The album API

The standalone `albumctl serve` viewer was removed. The read-only album API
(`internal/albumapi`, routes in `API.md`) is mounted in process by `nasctl serve`
and reached by the browser under `/api/v1/album/...`, behind the console's own
authentication, roles and audit. `[server]` limits such as
`max_concurrent_requests`, `max_concurrent_streams` and `page_max` still bound it.

## 4. What the service account must not be

* Not `root`. The units run as an unprivileged user with `ProtectSystem=strict`,
  `NoNewPrivileges`, a private `/tmp` and explicit `ReadWritePaths` for the state
  and cache directories only.
* Not a member of a group that can write the media tree, if you can avoid it.
* Not given a shell interface of any kind: there is no endpoint that executes a
  command, reads an arbitrary path, or lists a directory outside the index.

## 5. Quick reference

```sh
nasctl album doctor                      # before anything else: is this host safe to run on?
nasctl album status                      # what is running, what is pending, what is blocked
nasctl album scan --collection photos --limit 200 --dry-run
nasctl album scan --collection photos --limit 200
nasctl album thumbs --collection photos --limit 10 --max-runtime 5m
nasctl album pause --reason "array maintenance"
nasctl album resume                      # clears only the human pause
nasctl album cache-report                # never deletes
```

Stage D (the first run on the target machine) is 10 files, then 50, one variable at
a time, and it starts only on explicit authorisation. It is described in
`RESOURCE-REPORT.md` §7 and `RUNBOOK.md`.
---

# Part 2 — `nasctl`, the OneCloud Hub console

`nasctl serve` is the console. Album scanning and decoding run only in a separate
`nasctl album` process that the console starts for an explicit, bounded task.
The web handlers' import graph contains no decoder, which is what makes "an HTTP
request cannot start a full-library scan" a property of the build rather than a
promise.

Full evidence, measurements and the list of unverified items are in
`HUB-STAGE-B.md`.

## 1. Verbs

| Command | What it does | Writes | Exit codes |
| --- | --- | --- | --- |
| `nasctl status [--json]` | mode, memory, guard verdict, job counts, storage roots, anything needing attention | nothing | 0; 1 on a broken config |
| `nasctl doctor [--json]` | read-only check of config, storage, the console database, suzu, memory and the bind address | nothing | 0 clean; 1 with problems |
| `nasctl serve` | run the web console in the foreground | its own state directory | 0; 1 if the bind address is refused |
| `nasctl ops [--json]` | the operation table: class, approval, async, mutex, timeout | nothing | 0 |
| `nasctl files roots \| list \| stat \| search` | browse the configured storage roots | nothing | 0; 1 on a refusal or a missing path |
| `nasctl jobs list [--open] [--state S] \| jobs show ID` | job history and one job's event trail | nothing | 0; 1 if the job is unknown |
| `nasctl mode [show \| watch D \| maintenance D \| pause [reason] \| clear MODE]` | read or change the operating mode | mode table | 0 |
| `nasctl approvals [grant … \| revoke ID]` | list, grant or revoke approvals | approval table | 0; 1 if refused |
| `nasctl media [status \| library]` | the IWantWatchAnime integration, read-only | nothing | 0; 1 if the database cannot be read |
| `nasctl audit [--json]` | recent audited actions | nothing | 0 |
| `nasctl version` | version and schema | nothing | 0 |

Global flags: `--config PATH`, `--hub-db PATH`, `--json`, `--agent NAME`.

`--agent NAME` is not a label. It makes the caller an **AI principal**, which is
subject to the approval rules, may not start heavy work unless the operator has
enabled it, and has its reads recorded in the audit log. A local operator acts on
their own authority and their browsing is not recorded.

## 2. The console cannot do these things

* **Write a file.** Upload, mkdir, rename, move, copy, trash and restore are
  declared and unimplemented. `hub.storage[].writable = true` is **refused by the
  configuration validator**, so the flag cannot promise a capability the binary
  does not have.
* **Scan or decode.** `album.scan` and `album.thumbs` are declared with the right
  bounds; they run in a separate `nasctl album` process, which owns the
  resource guard and the worker isolation.
* **Run a command.** There is no operation that executes a shell, and a test fails
  if one is added.
* **Touch the backup.** Every `backup.*` operation is read-only, by a test.
* **Write suzu's state.** The database is opened `mode=ro` with `_query_only`.
* **Schedule anything.** `schedule.enable` is declared and unimplemented.
* **Delete an original.** There is no such operation at any risk class.

## 3. Access control

The console has **no per-user authentication**. Its access control is the bind
address, and that is checked rather than documented: `serve` refuses to start on a
public address unless `server.allow_non_private_bind` is set for an offline test.

```
bind = "100.x.y.z:8090"      # Tailscale: reachable only by your tailnet
bind = "127.0.0.1:8090"      # loopback, reach it through an SSH tunnel
```

An optional shared token (`server.token`) adds a bearer requirement. It is not
per-user either: it distinguishes "has the token" from "does not", and every
holder is the same principal.

Identity comes from the **credential**, not from a claim:

| Presented | Principal |
| --- | --- |
| a configured agent token (`hub.agents[].token`) | `ai:<name>` |
| `server.token` | `user:operator` |
| nothing, with no credential configured | `user:operator` |
| nothing, with a credential configured | refused |

Agent tokens are checked before the operator token, so an agent cannot be mistaken
for the operator. `X-Nasctl-Agent` (HTTP) and `--agent` (CLI) are honoured **only
when no credential is configured at all** - a convenience for a single-operator
setup, never an authorisation mechanism. The name is sanitised to
`[A-Za-z0-9._:-]` before it reaches a log, a JSON field or the audit table.

Each agent may have `allow_heavy` set individually, so one trusted agent can be
permitted to start bounded heavy work without permitting all of them. It is off by
default.

## 4. Modes

| Mode | Set by | Expires | Effect |
| --- | --- | --- | --- |
| `normal` | — | — | heavy work permitted, subject to resources |
| `maintenance` | a person | always (≤ 24 h) | permits work a schedule would otherwise gate; never bypasses a resource check |
| `watching` | a person | always (≤ 24 h) | stops heavy work while somebody is streaming |
| `manual_pause` | a person | **never** (cleared by hand) | stops heavy work; survives a restart |
| `resource_guard` | **the machine** | when the cause clears | stops heavy work |

Precedence, highest first: `resource_guard` → `manual_pause` → `watching` →
`maintenance` → `normal`. The two restricting modes outrank the permissive one on
purpose: a maintenance window that overlapped a watching window must not license
heavy work while somebody is streaming.

`resource_guard` **cannot be set or cleared through the API or the CLI.** It is
the machine's own state, and a caller who could clear it could silence a safety
state and restart decoders.

## 5. Approvals

An approval is bound to one operation, one principal, one target and **the exact
parameter string**. Changing a parameter needs a new approval: approving "scan 10
files" is not approving "scan".

* granted by a human or the local operator; **an AI cannot grant one, including to
  itself**, and a privileged helper grants nothing;
* must name a target when the operation acts on one;
* must expire, and may not outlive 12 h (bounded work) or 24 h (other);
* one-shot by default, so one approval cannot authorise a loop.

A local operator does not need an approval for bounded work - requiring somebody to
approve their own keystroke would make the console unusable without making it
safer - and a human through the API may do bounded work directly. Anything that
changes files or the machine needs an explicit approval from either.

## 6. Verifying the page

The frontend is plain JavaScript embedded in the binary: no build step, no
framework, no runtime dependency on the NAS. The cost of that is that nothing
compiles it, so a mistake in it is not caught by `go build`.

`scripts/uitest/render.mjs` renders the real page in a real DOM against a running
server and checks that all seven views populate, that breadcrumbs navigate and
return, that search filters and clears, that download links carry filenames, and
that the theme toggle works. It exits non-zero on failure. It runs on the
development machine only, and it does not check layout or appearance - jsdom has no
layout engine.

## 7. Configuration

The `[hub]` section of the same `config.toml`. It defaults to an **empty console**:
no storage roots, no integrations. Copying the example without editing it gives a
console that can report on the machine and nothing more.

```toml
[hub]
bind = "127.0.0.1:8090"

[[hub.storage]]
id = "media"                # lowercase, digits, - and _; used in URLs
label = "Media"
path = "/mnt/data/media"    # absolute, must not overlap state or another root
expose_hidden = false       # dot-prefixed entries are hidden by default

[hub.jobs]
max_open_jobs = 32
max_history = 5000
audit_retention_days = 90

[hub.suzu]
db_path = ""                # empty disables the integration entirely
api_base = "http://127.0.0.1:8637"
library_root = ""           # empty disables playback rather than guessing
```

Unknown keys are refused by name. A storage root inside the state or cache
directory is refused. A storage ID that is not lowercase-and-safe is refused,
because it appears in URL paths and must never look like a path.
