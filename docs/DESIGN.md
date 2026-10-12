# albumctl — design

Phase A deliverable. This describes what was built, why the boundaries are where
they are, and what each number is and is not a guarantee of.

The order of priorities, which every decision below follows, is:

1. the machine stays reachable,
2. the original files are not put at risk,
3. the existing backup keeps working,
4. *then* the photo library.

When those conflict, 1–3 win and the album degrades on purpose: a file is
skipped, a task stops, a placeholder is shown.

## 1. What this is

A read-only browser for photos and videos that already exist in directories on
this machine. It builds its own index and its own thumbnail cache and touches
nothing else. It is not a replacement for FileBrowser, Samba or the cloud backup,
and it deliberately has no file management, no upload, and no authentication
beyond the bind address.

## 2. Module boundaries

Three capabilities are separated into three processes with no shared memory and,
in normal operation, no shared permissions:

| Capability | Process | Writes | Reads |
| --- | --- | --- | --- |
| Browsing | `albumctl serve` (long-lived) | nothing | index, thumbnail cache, originals |
| Directory / metadata scan | `albumctl scan` (bounded) | index only | media tree |
| Thumbnail generation | `albumctl thumbs` (bounded) | index + cache | media tree |

The web service is the one process that faces the network, so it is given the
smallest set of abilities. It holds a write-refusing handle on the index and
cannot decode an image, start a subprocess, or walk the media tree at all. That
is not a convention that a later change could quietly break: `internal/server/arch_test.go`
walks the package graph and fails the build if the web service can reach
`image/*`, `os/exec`, the scanner, the thumbnail generator, or the EXIF parser,
and it also fails if the *source* of `internal/server` mentions
`filepath.Join`, `os.OpenFile`, `os.Rename`, `exec.Command`, `scan.Walk` or
`thumbs.Generate` anywhere. The rule is enforced by a test that fails, and the
test was checked against a deliberate violation to confirm it can fail.

Package layout:

```
cmd/albumctl            entry point
internal/cli            command line, exit codes, human JSON output
internal/server         read-only HTTP service, embedded frontend, route handlers
  web/                  index.html, app.css, app.js — served from the binary
internal/store          SQLite index: media rows, work queues, tasks, pause, pressure
internal/scan           bounded directory walk, metadata probe pass
internal/probe          format identification from content and extension
internal/exif           JPEG/PNG/TIFF metadata parser
internal/thumbs         thumbnail generation, worker child process, supervisor loop
internal/resource       /proc + cgroup v2 + PSI sampling, the guard, kill policy
internal/backupgate     asks the existing backup whether it is busy
internal/taskctl        lock file, worker re-exec path
internal/fsutil         path containment, atomic write, temp cleanup, device identity
internal/source         verifies a collection's mount and filesystem identity
internal/cachepath      thumbnail cache key → sharded path
internal/placeholder    placeholder SVG
internal/mimetype       closed extension → MIME whitelist
internal/config         TOML parsing, defaults, validation; refuses unknown keys
internal/webstate       the running web service's own record file (pid, bind, start)
internal/synth          synthetic media fixtures for tests (test-only)
internal/e2e            end-to-end tests driving the real binary
```

Two boundaries matter most:

**The web service cannot generate anything.** There is no code path from an HTTP
request to a decode. When a thumbnail is absent the response is a placeholder SVG
produced from the file name and kind, in one pass, with no I/O beyond a `stat`.
This is the difference between browsing a large library and DDoSing your own NAS
by scrolling.

**Decoding happens in a child process that is killed as a group.** The thumbnail
generator is the only code that touches a decoder, and it only ever runs inside
`albumctl exec-worker`, a child process placed in its own cgroup. If it
misbehaves, is OOM-killed, hangs, or forks, none of that reaches the supervisor,
and stopping it means killing the cgroup rather than one pid. The supervisor
polls the entire *machine* while the child works, not just the child.

## 3. Data flow

### Scan (bounded)

```
config → source.Verify (mount present? right filesystem? FSID pins match?)
       → walk (readdir batches, depth cap, dot-prefixed names skipped,
               no symlink following, one directory entry at a time)
       → per file: relative path, size, mtime_ns → index upsert
       → metadata probe: open, read a bounded header, identify format,
         compute width/height/pixels/frames, resolve capture time
       → work queues: probe pending, thumbnail pending
```

An upsert compares size and mtime only. Nothing hashes an original, and nothing
reads a video to discover duplicates. A row whose size or mtime changed has its
derived state invalidated — probe result, thumbnail state, metadata — so a
stale cache cannot outlive the file it described.

### Browse (read-only)

```
HTTP → validate (id is digits only, collection is a configured code, query is bounded)
     → one indexed query with LIMIT/OFFSET or a keyset cursor
     → JSON of one page
```

No handler loads the library, builds a queue, or waits for work. `GET /api/v1/status`
reads counters and the pause flag and returns; it never waits for a running task.

### Thumbnails (bounded, one at a time, interruptible)

```
loop:
  check stop conditions (pause, deadline, limit, resource thresholds, backup gate)
  pick one due file from the index, claim it
  sample MemAvailable — gates the start
  re-exec self as a worker child, put it in its own cgroup, handshake
  poll every second: MemAvailable, swap, cgroup memory.current, memory.events,
    PSI, child RSS (from /proc, so the child cannot lie)
  on threshold breach, timeout, or handshake failure: kill the process group,
    record the reason, keep truncation-safe leftovers out of the cache
  write the thumbnail via a temp file in the cache dir and rename it into place
  record the outcome; check resources again before the next file
```

## 4. Resource budget

These are the initial numbers, and they are *starting points to be measured*, not
assertions of safety. Nothing here has been validated on the target machine yet;
see `RESOURCE-REPORT.md` for what was actually measured and where.

| Item | Value | Nature |
| --- | --- | --- |
| Web service RSS, idle | ≤ 32 MiB target | target |
| Web service MemoryHigh / MemoryMax | 48 / 64 MiB | cgroup ceiling |
| Worker MemoryHigh / MemoryMax | 96 / 128 MiB | cgroup ceiling |
| Start a worker only while MemAvailable ≥ | 400 MiB, held for 3 s | admission gate |
| Do not start a new file below | 250 MiB | admission gate |
| Stop heavy work immediately below | 200 MiB | abort |
| Concurrent media workers | 1 | fixed |
| Thumbnail size | one, longest edge 512 px | fixed |
| Resource sample interval | 1 s (≤ 10 s, enforced) | abort latency |
| Post-trip cooldown | 30 min | persistent state |
| PSI memory `some avg10` stop | 50 | abort |
| PSI I/O `some avg10` stop | 80 | abort |
| Swap used stop | 64 MiB | abort |
| `MemoryMax` on the CPU-heavy worker | 128 MiB | ceiling |

What these are and are not:

* `MemoryMax` bounds the cgroup **as a whole** — the worker plus everything it
  forks, plus page cache charged to it. It is not "the process's RSS". A process
  can be inside its RSS ceiling and still be killed by the cgroup limit because a
  child or page cache used the rest.
* The media worker ceiling (96/128 MiB) is deliberately *below* the 250 MiB
  warning threshold, because the whole point is to stop before the machine is in
  trouble, not to finish the file.
* `MemAvailable` is read from `/proc/meminfo`, which accounts for page cache that
  can be reclaimed. Free memory and process RSS are never added together to
  build a budget, and zram is never counted as extra physical memory.
* Nothing here assumes "30 % of available is safe". The thresholds are absolute
  and are meant to be re-derived from measurements on the target.
* "≥ 400 MiB sustained" is a real condition, not a single reading:
  `guard.start_hold` (default 3 s) makes the first file wait until `MemAvailable`
  has stayed inside the budget continuously, and a run whose `--max-runtime` is
  not longer than that hold is refused rather than silently truncated. A machine
  that is briefly free and then busy again does not get a worker.
* The sampling interval is bounded at both ends: slower than 10 s is rejected at
  configuration time, because a poll that cannot catch a runaway decoder is not a
  stop mechanism. Sampling happens at every file boundary, on every interval while
  a worker is alive, and once after it exits.
* If the machine cannot enforce the cgroup limits (`require_cgroup_isolation`,
  default true), heavy tasks refuse to run. They do not fall back to running
  unbounded because limits could not be applied. `--allow-no-cgroup` exists to
  override that deliberately, and it is not used by the shipped units.

## 5. Stop mechanism

There are five independent ways heavy work stops, and they are ranked, because
"stop" has to be reliable when something else has already gone wrong:

1. **A hard limit inside the child.** The worker enforces its own pixel, file
   size, frame and wall-clock budgets and exits with a skip/fail code. This is
   the fastest and needs no cooperation.
2. **The parent's per-file timeout.** If the child does not finish in
   `per_file_timeout`, the parent kills the cgroup, records the reason, and moves
   on. A hung decoder cannot hang the machine.
3. **The parent's resource poll.** Every second the parent samples the machine.
   Crossing a stop threshold, seeing swap being used, PSI pressure, or a rising
   cgroup peak aborts the *current file* and ends the run.
4. **The persistent pause.** `albumctl pause` writes a durable flag. Every loop
   iteration re-reads it, so a pause lands within one file. The web service reads
   it too and never offers to start work.
5. **The operator.** `SIGTERM` to the supervisor ends the loop at the next file
   boundary, after a short grace period; a second signal kills the cgroup
   immediately.

A stop caused by resource pressure is recorded as a **persistent trip**, and a
trip blocks the next run. That is the mechanism behind "no immediate automatic
restart after a resource stop": it is not a sleep, it is a state change, and it
survives a reboot. `albumctl resume` clears it, deliberately, by hand.

Whether a worker is really gone is answered by looking for a process that is still
*running*, not merely for a pid that still exists. A process that has exited but
has not been reaped occupies its slot in the process table, so the cheap signal-0
test still succeeds for it; taking that as "alive" made the supervisor escalate
and then report that a worker had ignored `SIGTERM` and been `SIGKILL`ed when it
had in fact exited promptly. The diagnostic is part of the deliverable, so the
group is scanned for a runnable process and only a group with one is treated as
alive.

Stopping only ever means *this application's* worker cgroup being killed. No unit
or command in this repository sends a signal to SSH, Tailscale, Samba,
FileBrowser, rclone or any container, and there is no code that could: the only
pids the supervisor signals are its own children, and the only cgroup it removes
is the one it created.

## 6. Format support matrix

Identification is by content first (`exif.DetectFormat`), then reconciled with the
extension. A file whose extension and content disagree is indexed with a note
rather than trusted.

| Format | Index | Metadata | Thumbnail | Notes |
| --- | --- | --- | --- | --- |
| JPEG | yes | full EXIF incl. GPS | yes | primary format |
| PNG | yes | pixel size, some text chunks | yes | |
| GIF | yes | size, frame count | first frame only | `max_frames = 1` |
| WebP | yes | size | yes | |
| MP4 / MOV | yes | duration, container creation time | no (poster off) | played directly by the browser |
| TIFF | yes | dimensions, EXIF | no | not enabled in v1 |
| HEIC / HEIF | indexed, marked unsupported | none | no | placeholders; needs an external decoder |
| CR2 / RAW | indexed, marked unsupported | none | no | placeholders |
| Anything else | indexed, marked unsupported | none | no | placeholder + original still fetchable |

Indexed-but-unsupported is deliberate: the file appears in the timeline with a
placeholder and its original stays downloadable, so nothing is invisible. It is
not queued for decoding. Video posters are a separate, off-by-default capability
that requires an external `ffmpeg`; without it, the format is skipped and the
run continues rather than failing.

## 7. Index and cache

**Index:** SQLite in WAL mode via `modernc.org/sqlite`, which is pure Go. That
matters on ARMv7: no cgo, so `CGO_ENABLED=0` produces a static binary with no
shared-library or cross-compilation trapdoors. Verified on an armv7 build under
emulation (see `RESOURCE-REPORT.md`).

Bounded memory: every query is paginated and every batch has a hard maximum
(`store.MaxWorkBatch`, 1000 rows; API pages are capped at 200). Records are
never loaded wholesale. A cursor-based page uses the last row's key rather than
`OFFSET` where it matters, so page 4000 costs the same as page 1.

Schema version 2 adds a `seen_run` column. Each scan takes a token
(`MAX(seen_run)+1`) at its start and stamps every row it touches with it; the
"missing" flag is then set for rows whose token does not match a *completed* walk
of that collection. This is a change from comparing `last_seen` against a
timestamp, and it was made after a real defect: a file deleted and re-scanned
within the same second was still reported present, because the two events landed
in the same clock tick. A row is only flagged missing when a walk that covered the
whole collection did not see it — a truncated or dead-ended walk never marks
anything gone, and nothing is ever deleted from the index automatically. A missing
row is hidden from the default listing (`include_missing=1` shows it) and its
original is not served.

A file the walk saw but could not re-index is stamped as seen for that run even
though nothing else about its row changes. This distinction is deliberate:
"missing" has to mean *absent from the directory*, not "not refreshed". Without
it, the same walk that deferred a file being copied in would also flag that file
absent — hiding a picture that is merely still being written, and taking its
original out of service until the next scan. The row keeps its size, mtime and
thumbnail state, so no partial result is published either way; the deferral is
reported in the run's notes and counted separately.

Recovery: SQLite's own WAL recovery applies on open. Nothing here is stored only
in memory, so a crash mid-batch loses at most the current file's progress, and
that file is still marked pending. A row is only marked complete after its
thumbnail is renamed into place; an interrupted write leaves a temp file in the
cache directory, not a broken entry.

**Cache:** separate files, sharded by a hash of the media key, with a configured
byte and file ceiling and a percentage at which `doctor` warns. Thumbnails are
never blobs inside the database, because a single file that grows without bound
is exactly what cannot be pruned safely. Pruning (`albumctl cache-gc`) only ever
removes files under the configured cache directory, only ones matching this
application's own naming, and never touches an original or an index row.

The file-name heuristic for guessing a capture date from a name is off by
default; a guessed date is worse than an honest "no capture time".

## 8. Time handling

Capture time is resolved in a fixed order, and the chosen source travels with the
row to the interface, which labels it:

1. EXIF `DateTimeOriginal` (with `OffsetTime` when present),
2. video container creation time,
3. a file-name heuristic — **off by default**,
4. the file's own mtime, always shown as a fallback.

Zone rules: an EXIF time with an explicit offset is stored with that offset and
displayed with it. A time with no offset is stored as a naive local time and
labelled as such, because inventing a zone would silently move someone's photos
by hours. The system timezone is never changed, and no original is ever
rewritten. A row with no usable date at all is not placed on the timeline; it is
counted as `undated` and shown separately, rather than being given today's date.

## 9. Explicitly not implemented

Not "not yet", but designed out of v1, and visible as such in the interface:

* AI classification, face recognition, OCR.
* Video transcoding, and video thumbnails by default.
* Continuous watching, filesystem notification, background loops, automatic
  rescanning, scheduled runs (the timer unit ships disabled and is not installed
  by the deployment steps).
* Any modification of an original: move, rename, delete, EXIF write-back.
* Upload, cloud sync, backup logic of any kind. The existing backup is left alone
  and is never signalled; albumctl yields to it instead.
* Maps, remote fonts, analytics, external APIs, public sharing, Funnel.
* Authentication. The service binds to a private address and that is the whole
  of the access control; this is stated in the interface itself so it is not
  mistaken for an authenticated service.

## 10. What is verified, and what is not

Verified on a development machine, by automated tests and by running the real
binary: path traversal and symlink escape, crafted file names, malformed and
hostile EXIF, pixel bombs and decode-bomb files, truncated and empty files, files
replaced mid-processing, Range requests, pause honoured by both the worker and the
service, concurrent request bounding, the web service's structural inability to
reach a decoder (`internal/server/arch_test.go`, itself checked against a
deliberate violation), the index opening read-only, the scanner's bounded memory
curve, the thumbnail supervisor's constant footprint across 1–60 files, and the
web service's plateau and streaming behaviour, the scanner's file-count
bookkeeping and its refusal to mark a deferred or unreached file absent, the task
lock's behaviour when a previous holder was killed, the web service's own record
file, the cache ceiling stopping a run (by bytes and by file count), the whole
process group of a worker being terminated with its children, the source
identity check refusing each way it can refuse, and the backup gate's verdicts
including the real subprocess boundary. Exact commands and raw output:
`RESOURCE-REPORT.md`. CLI, routes and permissions: `CLI-ACCESS.md`. Operating and
recovering: `RUNBOOK.md`.

**Not verified, and not claimed:**

* Anything on the target `onecloud`. The ARMv7 artefact was cross-compiled and run
  under `qemu-arm-static`, which shows that the binary *starts and produces the
  same results* on a 32-bit ARM ABI. An emulated process's RSS is the emulator's,
  so no ARMv7 memory number is claimed anywhere.
* cgroup accounting and `MemoryMax` enforcement. The development host could not
  delegate a cgroup, so the isolation path has only been exercised as a *refusal*.
  On the target, `doctor` must be read before any heavy task is authorised.
* Any stop path that depends on real memory pressure: no swap threshold, no PSI
  threshold and no `MemAvailable` threshold has been observed firing against a
  starved machine. They are covered by unit tests with injected readings only.
* Video posters (the adapters are not compiled in and the capability is off),
  HEIC and RAW (not implemented at all).
* A genuinely full filesystem. The cache *policy* ceiling is tested and does stop
  a run; an actual `ENOSPC` from the kernel is not, because a size-limited
  filesystem cannot be mounted on the development host. What is relied on there
  is the write discipline: every write is temp-file-then-rename with the temp file
  removed on any error, which is tested at the `fsutil` level, and a failed
  thumbnail is recorded as a failed file with a retry ceiling rather than as a
  success.
* Behaviour over a multi-hour run, or against a RAID0 array that is rebuilding or
  being written to by another service. The longest continuous batch measured is 60
  files in under a second; the tooling is deliberately built around short, bounded,
  manual runs rather than around surviving a long unattended one.
* The health of the originals, the array, or the backup. Nothing in this repository
  reads a file to verify it, and no measurement here implies that any file or
  backup is intact.