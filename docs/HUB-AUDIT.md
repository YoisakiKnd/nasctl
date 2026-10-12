# OneCloud Hub — stage A audit

What actually exists, what is only design, and what could not be checked from this
machine. Every claim below was produced by reading the code in this session, with
the file and line that carries it. Nothing here is inferred from a README's
"planned" section.

Date of audit: 2026-10-12. Dev host: the x86_64 machine this session runs on.
The target onecloud was **not** contacted, and nothing on it was read or changed.

---

## 0. The two findings that change the plan

**Finding 1 — `iwantwatchanime` has no watching features to migrate.**

The task description says the anime project brings "媒体列表、播放、观看进度、收藏、
历史及分类" and asks that "播放记录、收藏、配置等数据" be preserved. The code says
otherwise. `IWantWatchAnime` (binary name `suzu`) is a *subscription → download →
archive* pipeline:

* its schema (`internal/store/schema.sql`) has exactly seven tables —
  `subscriptions`, `items`, `tasks`, `library`, `events`, `bangumi`, `meta` — and
  none of them records a position in a video, a favourite, a watch session or a
  category;
* there is no `<video>` element, no player and no progress endpoint anywhere in
  `internal/httpx/` (grep for `<video`, `play(`, `player`: no hits);
* `tasks.progress` / `tasks.done_bytes` are *download* progress, not viewing
  progress.

So there is no existing watch data to preserve, and no playback code to lift. The
"看番" module has to be assembled from two different things:

| Part | Where it comes from | Status |
| --- | --- | --- |
| 订阅、RSS 匹配、下载、入库归档、刮削、封面、活动流 | suzu, already working | reuse as-is |
| 媒体列表（已归档集数）、直接播放、观看进度、收藏、历史 | **does not exist anywhere yet** | new work in the hub |

This is worth a decision from you, because it changes the size of stage D: the
"融合" is mostly an *adapter plus a library browser*, and the watch-state features
are new code with no predecessor. It does **not** mean suzu is being rewritten —
its downloader, matcher and pipeline stay untouched (see §1.4).

**Finding 2 — `backupctl` and the production services cannot be audited from here.**

There is no `backupctl` source on this machine (whole-filesystem search: no hits),
and no SSH host entry for the onecloud (`~/.ssh/config` contains only `github.com`).
Samba, FileBrowser, Homepage, qBittorrent, nginx and the backup tooling all live on
the target. Their purpose is therefore *unverified*, and the "先审计，再决定修复或
替换控制逻辑" step cannot be completed until either read-only access is authorized
or you run the recon commands in §5 and send the output. Nothing in this design
assumes what they do.

---

## 1. IWantWatchAnime / suzu

### 1.1 Identity and licence

| Fact | Value | Evidence |
| --- | --- | --- |
| Location | `/home/ubuntu/Codes/IWantWatchAnime` | `ls` |
| Origin | `git@github.com:YoisakiKnd/IWantWatchAnime.git` | `git remote -v` |
| Licence | **MIT** | `README.md:299` (`## License` → `MIT`) |
| Licence file | **absent** — no `LICENSE` in the tree | `ls LICENSE* COPYING*` → exit 2 |
| Language / version | Go, `go 1.26.0` | `go.mod` |

The licence is permissive and the repository is your own, so merging code is
allowed. Two things to do anyway: add a `LICENSE` file to that repository (MIT
requires the notice to travel with the source), and keep an attribution note in
whatever hub file derives from it. No third-party code of unclear provenance was
found — its dependencies are the four in `go.mod` plus indirect ones, all
permissive.

### 1.2 Technology stack

Static Go binary, **no CGO**, `-s -w`, ~15 MB. Dependencies are small and — in two
cases — the *same* ones albumctl already pins:

```
github.com/BurntSushi/toml v1.6.0      (albumctl pins the same version)
github.com/mmcdole/gofeed v1.5.0       (RSS parsing)
golang.org/x/net v0.58.0
modernc.org/sqlite v1.60.1             (albumctl pins the same version)
```

Same SQLite driver at the same version means one storage technology, one set of
ARMv7 build properties to verify, and no second C library. That is a real
integration advantage — see §1.4.

### 1.3 Interface, data and build

**HTTP surface** (`internal/httpx/server.go`): server-rendered HTML plus partials
(`/partials/schedule|queue|activity|library|verdict`), a subscribe browser
(`/subscribe`), a small JSON API (`/api/v1/state`, `/api/v1/subscriptions*`,
`/api/v1/tasks/{id}/{action}`, `/api/v1/bangumi/search|subscribe`,
`/api/v1/covers/{name}`), and `GET /healthz` → `ok <version> uptime=<dur>`.

**Auth**: two mechanisms, both off by default because the default bind is
`127.0.0.1:8637`.
* HTTP Basic (`server.go:103`) when `server.user`/`server.password` are set, with a
  startup warning when they are not (`server.go:91`);
* `X-Api-Token: <token>` for machine clients when `server.token` is set and
  `server.accept_token` is true (`server.go:99`).

That second one is the door the hub should use for writes, so that suzu stays the
only writer of its own database.

**Data layout**: everything under `storage.data_dir` — `suzu.db` (+ WAL/SHM),
`suzu.snapshot.db`, `covers/`. Config examples put it at `/mnt/usb/suzu/data`, with
`downloads` and `library` alongside. The unit grants write access to exactly three
paths.

**Schema** (Unix seconds, no BLOBs, indexed list queries — deliberately ARMv7
friendly): `subscriptions` (feed_url, filters, interval, mikan_id, etag/last_mod),
`items` (per-feed entries with episode/size/status/reason), `tasks` (downloader, gid,
state, progress, bytes, save_path, error), `library` (series_id, task_id, title,
path, linked, nfo_path, episode, added_at), `events`, `bangumi` (scrape cache:
cover_path, air_text, episodes, subgroups JSON, summary, score), `meta`.

**Retention is already bounded**: `keep_events_rows = 2000`, `snapshot_min = 360`.
Good — the hub does not need to invent a policy for suzu's log.

**Build**: `Makefile` with `build` / `all` / `dist` (amd64, armv7, armv6, arm64),
`SHA256SUMS`, `vet`, `fmt-check`, `size`. Prebuilt ARMv7 binary present at
`bin/suzu-linux-armv7`. `VERSION` comes from `git describe`, injected via ldflags.

**Deployment**: `deploy/suzu.service` — `User=suzu`, `WorkingDirectory=/opt/suzu`,
`MemoryMax=192M`, `MemoryHigh=144M`, `CPUWeight=20`, `Nice=5`,
`IOSchedulingClass=best-effort`, `ProtectSystem=strict`, `PrivateTmp`,
`NoNewPrivileges`, `ReadWritePaths=/mnt/usb/suzu /mnt/usb/downloads /mnt/usb/library`,
logs to journald only. `deploy/README-armv7.md` documents the "never write the root
filesystem" install path.

### 1.4 What this means for integration

Three options were considered.

| Option | What it costs | Verdict |
| --- | --- | --- |
| **A. Read-only adapter + write-through suzu's API** | Hub opens `suzu.db` read-only (`mode=ro`, WAL allows concurrent readers) for lists and status; sends subscribe/toggle/backfill/poll through suzu's existing HTTP API with `X-Api-Token`. suzu remains the only writer. | **Recommended.** No fork, no second writer, no risk to a pipeline that is proven in production, and full rollback: stopping the hub leaves suzu exactly as it is. |
| B. Merge suzu's packages into the hub module | Requires moving `internal/*` out of its module or copying it; the scheduler loop becomes hub code; two release cycles entangled. | Rejected for now. It is a fork of a working system with no functional gain for stage B. |
| C. Proxy or embed suzu's pages | Explicitly forbidden by the task. | Rejected. |

Option A also matches the architecture rule "运行时分开 … 控制台重启不能默认中断托管
任务": suzu keeps running and downloading whether or not the hub is up.

One caveat to design around: suzu's `/api/v1/state` returns unpaged slices
(`pipeline.Overview` carries `Subs`, `Tasks`, `Items`, `Library`, `Events`). The hub
will **not** use it for lists — it will read the database with its own bounded,
paginated queries, and use `/healthz` only as a liveness ping. Details in
`HUB-DESIGN.md` §5.

---

## 2. albumctl — implemented vs design-only

This is the module the hub reuses for 相册, and the base the hub's own code is added
to. The distinction that matters: albumctl is a *read-only album* with a *heavy-work
scheduler*; it has **no file-write capability at all**, which is why stage E exists.

### 2.1 Implemented and verified (in this repository, with passing tests)

| Capability | Where | Evidence |
| --- | --- | --- |
| Bounded directory walk (depth, exclusions, symlink policy, hidden names, counter reconciliation) | `internal/scan/walk.go` | `walk_test.go`, e2e "files seen adds up" |
| Metadata probe + EXIF/GPS, extension-vs-content reconciliation | `internal/probe`, `internal/exif` | package tests pass |
| Missing-file detection that cannot mark a deferred/unreached row absent | `internal/store/media.go` (`seen_run`) | `scan/missing_test.go`, reverse-verified |
| SQLite store, WAL, pagination, read-only open that refuses writes | `internal/store` | `store_test.go` (incl. `TestWaitForWriteIdleReturnsPromptly`, read-only refusal) |
| Single-worker thumbnail generation with child-process isolation, per-file timeout, group kill, retry ceiling, backoff | `internal/thumbs/{run,worker,gen}.go` | `thumbs_test.go`, `resource/proc_test.go` |
| Atomic cache writes (temp + rename, no partial file) | `internal/fsutil.AtomicWrite` | `fsutil_test.go` |
| Cache ceiling stop (bytes and file count) with `cache_full` | `internal/thumbs/run.go` | e2e `TestTheRunStopsAtTheCacheCeiling` |
| Resource guard: sustained-start admission, warn/stop thresholds, swap, PSI, cgroup peaks, persistent trip + cooldown | `internal/resource/guard.go` | `guard_test.go`, `meminfo_test.go` |
| Persistent pause that survives restart and is honoured by the read-only web service | `internal/store`, `internal/server` | store tests, e2e pause cases |
| Mutual exclusion locks that survive a killed holder | `internal/taskctl/lock.go` | `lock_test.go` |
| Source identity check (mount present, FSID pinned, root inside the data mount, shared-volume opt-in) | `internal/source` | `source_test.go` (10 refusals) |
| Read-only HTTP API with bounded pagination, placeholder SVG, bounded streaming and correct Range on originals | `internal/server` | e2e `http_test.go` (incl. 300 MB range case) |
| Optional bearer-token / cookie auth, constant-time compare, warning when unset | `internal/server/server.go:152-189` | code; **off by default** |
| CLI: `status scan thumbs pause resume doctor cache-report cache-gc serve collections version` | `internal/cli/cli.go` | e2e drives these as a real binary |
| Backup *gate*: defers album work while a backup is busy, via a check command or systemd unit query, with unknown ≠ idle | `internal/backupgate` | `backupgate_test.go` (incl. real-subprocess exit codes) |
| ARMv7 static artefact + reproducible build + checksum | `scripts/build-armv7.sh`, `dist/` | `sha256sum -c` OK, byte-identical rebuild |

Scale: 59 Go files, 23 test files, 263 test functions, `go test ./...` exit 0.

### 2.2 Not implemented (do not assume these exist)

| Missing | Consequence for the hub |
| --- | --- |
| Any file **write**: upload, mkdir, rename, move, copy, trash, restore | stage E is genuinely new code; no partial implementation to extend |
| Trash / recycle bin with bounds and restore | new; the "no permanent delete without confirmation" rule has nothing to build on |
| Per-user identity, quotas, per-user permissions | new; today there is one implicit operator and an optional shared token |
| AI operation registry, approval records, audit log | new; today the CLI is the only interface and it has no notion of a caller |
| A job table with the hub's state names (`queued…unknown`) and reconciliation | albumctl has `tasks` for scan/thumb work only; the hub needs its own |
| Backup execution (rclone orchestration, progress, verification) | albumctl only *yields* to a backup; it never starts or inspects one |
| Scheduler / timers | deliberately none; `systemd/albumctl-thumbs.timer` ships disabled |
| Media-library browsing of arbitrary video, playback, watch state | new |
| MCP / agent transport | new |

### 2.3 Resource behaviour worth carrying over unchanged

The thresholds, the single-worker rule, the sustained start hold, the persistent
trip, the refusal to run heavy work when cgroup isolation is unavailable, and the
"unknown is not healthy" reporting are the parts of albumctl that exist *because* a
machine was lost. The hub inherits them rather than re-deriving them, and the 40 MiB
idle target for the console (task §八) sits below albumctl's measured 12.8 MiB idle
web service, so the budget is plausible but **must be re-measured** with the hub's
own pages, and measured as a process tree, not as one process.

---

## 3. Repository placement decision

`internal/` packages cannot be imported across Go modules — a second module cannot
import `github.com/YoisakiKnd/nasctl/internal/...`. That rules out "new hub repo that
imports albumctl" without first renaming albumctl's `internal/` to something
importable, which would be a mechanical but real change to tested code.

Decision taken (and open to your reversal): **the hub is built in this repository.**

* `cmd/nasctl` — the unified console and the primary binary.
* `cmd/albumctl` — kept, unchanged, as the compatibility entry point.
* New packages: `internal/files`, `internal/jobs`, `internal/operations`,
  `internal/hubweb`, `internal/mediasrc`, `internal/backupsrc`, `internal/mode`,
  plus `internal/hubconfig` (or an extension of the existing `config`).
* Everything already in `internal/` is reused as-is; the safety-critical parts
  (resource guard, store, fsutil, taskctl, source, thumbs) are not forked.

Cost: the directory is still called `albumctl`. Renaming it (and the module path) is
a mechanical follow-up that changes no behaviour — say the word and it is a
`git mv` plus a find-and-replace, verified by the same test suite.

---

## 4. What the hub must not do (carried from the task, restated as audit findings)

* No iframe, proxy, re-implementation-by-configuration or re-install of FileBrowser;
  it stays as the fallback entry point until a new file module passes acceptance.
* No takeover of qBittorrent or nginx. suzu's `qbittorrent` adapter talks to a
  qBittorrent that already exists; whether that instance is also used for other
  things is **unverified** (§5).
* No Docker on the target, no new resident runtime, no large build on the target.
* No touching RAID, partitions, mounts, SSH or Tailscale; no recursive chown of the
  data volume; no stopping of existing services to free memory.
* No production deployment, data migration, file write, upload or scheduler
  enablement without a separate approval — none of those happened in this stage.

---

## 5. Unverified: production components, and the exact commands to verify them

Not checked, and deliberately not guessed: the purpose and configuration of Samba,
FileBrowser, Homepage, qBittorrent, nginx, the backup tooling, and how the RAID0 is
mounted. The dev host has no route to the onecloud (`~/.ssh/config` lists only
`github.com`), and read-only access to the target was not authorized.

To close this, either authorize a read-only session, or run the following on the
target and send back the output. Every command is read-only; none of them is run by
this repository, and none of them is executed automatically.

```sh
# identity and resources
uname -a; cat /etc/os-release | head -3
free -m; cat /proc/meminfo | grep -E 'MemTotal|MemAvailable|SwapTotal|SwapFree'
cat /proc/pressure/{cpu,io,memory} 2>/dev/null
ls /sys/fs/cgroup/ | head; cat /proc/self/cgroup

# mounts and storage (identity only, no writes)
findmnt -o TARGET,SOURCE,FSTYPE,OPTIONS
cat /proc/mdstat; lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT

# what is actually running, and under which unit
systemctl list-units --type=service --state=running --no-pager
systemctl list-timers --all --no-pager
systemctl cat filebrowser 2>/dev/null | head -40
systemctl cat smbd 2>/dev/null | head -20
systemctl cat nginx 2>/dev/null | head -20
systemctl cat homepage 2>/dev/null | head -20
systemctl cat qbittorrent-nox 2>/dev/null | head -20
systemctl cat aria2 2>/dev/null | head -20
systemctl cat suzu 2>/dev/null | head -40

# listeners: who owns which port, and is anything on a public interface
ss -lntp

# backup tooling: existence, not modification
command -v backupctl rclone
ls -la /etc/backupctl* /etc/cron.d/*backup* /etc/systemd/system/*backup* 2>/dev/null
backupctl --help 2>&1 | head -40
rclone listremotes 2>/dev/null          # names only; no upload, no listing of contents
rclone about <remote>: 2>/dev/null      # quota only, read-only

# media and library roots (paths and sizes only)
df -h; du -sh /mnt/* 2>/dev/null | sort -h | tail -20

# suzu's own state, read-only
systemctl status suzu --no-pager | head -20
curl -s http://127.0.0.1:8637/healthz

# filebrowser's real role: is it a separate binary, and what root does it serve
ps -o pid,user,args -C filebrowser 2>/dev/null | head -5
```

What each gap blocks:

| Gap | Blocks |
| --- | --- |
| `backupctl`'s real commands/exit codes | whether the hub wraps it, fixes it or replaces its control logic (§七 of the task) |
| rclone remotes and current flags | any statement about resume, bandwidth or verification |
| Which units exist and their memory ceilings | the machine-wide budget the hub's ceilings must fit inside |
| What FileBrowser serves and as whom | the storage-root table and the "don't expose the whole data disk" default |
| Whether nginx sits in front of anything | whether a hub port is reachable from Tailscale as designed |

Until then, the hub's own configuration example will contain **no production paths
and no credentials**, and the backup module will report `unknown` rather than a
guess.