# albumctl runbook

Operations, troubleshooting, index recovery and rollback.

Everything in this document assumes the deployment order from the design: nothing
runs on the target machine until the machine is confirmed healthy and a small
sample has been authorized. Nothing here reconfigures the operating system, the
existing services or the existing backup.

## 0. The five-minute version

```sh
# Is anything of mine running, and what is waiting?
albumctl status --config /etc/albumctl/config.toml

# Stop everything of mine, durably, including across a reboot.
albumctl pause --reason "operator stop"

# Look before touching: what is wrong, and what is my cache doing?
albumctl doctor       --config /etc/albumctl/config.toml
albumctl cache-report --config /etc/albumctl/config.toml

# Start a batch again, deliberately.
albumctl resume --config /etc/albumctl/config.toml
```

Rules to keep in mind while reading the rest:

* `albumctl pause` is the only "stop" that is durable. A resource trip is also
  durable, and `resume` is the only thing that clears either.
* No command in this project stops, signals or reconfigures anything outside it.
* Nothing ever writes to a media directory. Every write is a temp file plus a
  rename, inside the state or cache directory.
* No index row is ever deleted because a file disappeared. It is flagged, and the
  flag is reversible.

## 1. Diagnosing "the album is not responding"

Read the service's own view first — it is cheap and does not touch the media:

```sh
albumctl status --config /etc/albumctl/config.toml
```

Look at, in order:

1. **`reason`** — if a pause or a resource trip is recorded, the machine is not
   busy and the album is not broken: it is stopped, deliberately, and it will
   stay stopped. That is the answer a surprising number of times.
2. **`resource.mem_available_kb` against the configured thresholds.** If
   MemAvailable is under the stop threshold and something else on the machine is
   the reason, the correct action concerns *that* service, not this one.
3. **`running` / `last`** — a task that is still running when it should have
   finished means the per-file timeout or the supervisor needs a look.
4. **`totals.missing`** — a large count after a scan usually means a mount is
   absent, not that files were deleted. See §4.

Then, from the shell:

```sh
systemctl status albumctl-web
journalctl -u albumctl-web -u albumctl-thumbs --since "-1h" --no-pager
```

If the *machine* is slow but this service looks idle, the cause is elsewhere.
`albumctl` is a reader; it does not hold the machine when it is not running a
task. Confirm with:

```sh
albumctl status --config /etc/albumctl/config.toml --json
```

The JSON includes the cgroup's `memory.current` and `memory.events` when
accounting is available, which is how you tell "my worker is using memory" from
"the machine is short of memory for another reason". These are different
problems and they have different fixes.

## 2. Diagnosing "the web page loads but thumbnails are blank"

This is the designed behaviour when thumbnails have not been generated: a
placeholder is returned immediately and the original is still available. Nothing
on the web side can fix it, because the web side is not allowed to decode
anything.

```sh
albumctl doctor       --config /etc/albumctl/config.toml   # is heavy work allowed at all?
albumctl cache-report --config /etc/albumctl/config.toml   # how much cache is there?
albumctl thumbs --collection photos --limit 10 --max-runtime 10m
```

If `doctor` reports that cgroup isolation is unavailable, that is why nothing is
being generated: heavy tasks refuse to run when the limits they depend on cannot
be applied. Fix the delegation (see §6) rather than turning the requirement off.

A cache-report that shows the cache full is also a stop condition, not a bug: the
next step is `albumctl cache-gc`, which only ever removes albumctl's own
thumbnail files.

Two different "full" states are worth telling apart, because only one of them is
this application's own policy:

* **The cache ceiling was reached.** `cache.files` / `cache.bytes` at the ceiling
  in `cache-report`, and a run stops with `cache_full`, which `thumbs` reports as
  exit code **4** - the cache ceiling is grouped with the resource reasons, since
  the next run will hit it again until someone acts. This is designed behaviour,
  and the fix is a deliberate `cache-gc`, or a larger `cache.max_bytes` if the
  disk really has room.
* **The filesystem is full.** Then a write itself fails. The thumbnail is recorded
  as a failed file with a retry ceiling - it is not marked done, and no partial
  file is published, because every cache write goes to a temporary file that is
  renamed into place only after the bytes are on disk. Freeing space is the fix;
  raising `cache.max_bytes` is not, since the ceiling is what keeps this
  application from being the process that fills the SSD holding the index.

The second state has not been exercised on a device: the development host cannot
mount a size-limited filesystem. The write-interruption behaviour it depends on is
tested, but treat a real `ENOSPC` as unverified until it happens somewhere.

## 3. A resource trip happened. Now what?

A trip is recorded when MemAvailable, swap, PSI or a cgroup threshold was crossed,
or when a worker was killed for exceeding its ceiling. It blocks the next run on
purpose. Nothing restarts automatically.

```sh
albumctl status --config /etc/albumctl/config.toml     # see the recorded reason
```

Ask three questions before resuming:

1. What else was running at that moment? A backup upload, a container restart,
   an index rebuild by another service. The trip is a fact about the machine at
   that time, not a property of the file.
2. Was the file unusual? A very large image, a corrupt header claiming a huge
   canvas, an animation with many frames. The per-file reasons in the journal say
   which.
3. Is the budget the right one? **Do not raise it to make a particular file
   succeed.** That is the specific mistake this design exists to prevent. A file
   that does not fit should stay a placeholder; if that is unacceptable, the
   decision is to change which formats are enabled, not to raise the ceiling.

Then, if the machine has recovered:

```sh
albumctl resume --config /etc/albumctl/config.toml
albumctl thumbs --collection photos --limit 5 --max-runtime 10m
```

`resume` clears the trip. It does not start work, and it does not enable a
schedule.

## 4. A share or the array is gone: files report as missing

The pattern to expect: a scan that "finds" nothing, or a large `totals.missing`,
after the machine came back without its RAID0 array assembled.

This is why the source check exists. Before a scan or a thumbnail pass touches
anything, it verifies that the declared `data_mount` is really a mount point and
that the collection's `root` really resides on it. When the array is absent,
`/mnt/data` is an ordinary empty directory on the system disk, and the task
**refuses to run** rather than indexing an empty tree:

```
source check failed: /mnt/data is not a mount point
```

If that fires, stop and fix the mount. Do not work around it.

If a scan somehow ran while the mount was absent (for example because
`allow_shared_volume` was set for a test), the rows are flagged missing, not
deleted, and the correct recovery is:

```sh
mount /mnt/data            # bring the real data back
albumctl doctor --config /etc/albumctl/config.toml     # confirm the mount and the FSID
albumctl scan --collection photos --limit 5000         # a complete walk clears the flags
```

The flags clear themselves on the next complete walk that sees the files again.
`include_missing=1` on the API, or the corresponding checkbox in the interface,
shows the flagged rows in the meantime.

If the filesystem identity changed (the array was rebuilt, or a disk replaced),
`doctor` reports an FSID mismatch. Confirm by hand that this is the array you
mean, then update `expect_fsid`. Never accept a mismatch blind: it is exactly the
case where a rebuild has replaced the data you thought you had.

## 5. Recovering the index

The index is a cache. Every fact in it can be rebuilt from the media tree, and
rebuilding costs time and I/O, nothing else.

Nothing about the index is authoritative for the originals: no original is moved,
renamed, deleted or rewritten by any recovery step here.

### It will not open

```sh
albumctl doctor --config /etc/albumctl/config.toml
```

`doctor` runs `PRAGMA integrity_check` and reports a schema mismatch separately
from corruption.

* **"schema is newer than this build understands"** — you are running an older
  binary against a newer index. Install the matching binary. Do not downgrade
  the index by hand.
* **Corrupt** — move it aside and rebuild. Keep the old file until you are
  satisfied; it costs disk, not correctness.

```sh
systemctl stop albumctl-web
cd /var/lib/albumctl
mv index.sqlite3 index.sqlite3.corrupt-$(date +%s)
# The WAL and SHM sidecars belong to the old file; move them too.
mv index.sqlite3-wal index.sqlite3-shm . 2>/dev/null || true
albumctl scan --collection photos --limit 5000     # rebuilds the rows
albumctl thumbnails --collection photos --limit 50 # optionally re-warm the cache
systemctl start albumctl-web
```

Thumbnails whose rows were lost become orphans in the cache directory;
`albumctl cache-gc` removes them after the configured grace period. Nothing else
is touched.

### It is there but wrong

* **Stale rows after a large reorganization** — run a complete scan. Changed files
  are detected by size and mtime, and their derived state is invalidated
  automatically.
* **Thumbnails that do not match their file** — the thumbnail is keyed to the
  media row and invalidated when size or mtime changes. If you suspect a mismatch,
  delete that one file under the cache directory; it will be regenerated.
* **A row count that keeps growing** — the same file appearing twice means the
  collection's root or `data_mount` changed between runs. Check `doctor`; two
  collection entries pointing at overlapping trees will index the same files under
  both codes.

### Sizing

The index is one SQLite file, WAL mode, a few hundred bytes per row. Storage is
not a concern at this scale; the concern is the *cache*, which holds image data,
and which has its own byte and file ceilings and its own report.

## 6. cgroup isolation is unavailable

`doctor` says so, and heavy tasks refuse to run:

```
[FAIL] cgroup v2 isolation is not available
[FAIL] guard.require_cgroup_isolation is true, so heavy tasks will refuse to run
```

This is deliberate. Without delegation, albumctl cannot attribute memory to a
decode child, cannot see its OOM count and cannot stop it as a group; it would be
back to watching one process's RSS, which is what the design says is not enough.

Fix it by running the unit with delegation, as the shipped unit does:

```ini
Delegate=yes
```

Verify that the service actually got its own subtree:

```sh
systemctl status albumctl-thumbs
cat /sys/fs/cgroup/system.slice/albumctl-thumbs.service/cgroup.controllers
cat /sys/fs/cgroup/system.slice/albumctl-thumbs.service/memory.max
```

The recovery is never `require_cgroup_isolation = false` on this machine. If you
set it for an experiment, set it back.

## 7. Pause, and what it does not cover

```sh
albumctl pause --reason "family using the NAS"     # durable, survives reboot
albumctl resume                                    # clears the pause, starts nothing
```

While paused:

* a running thumbnail batch stops at the next file boundary, not mid-file;
* a new batch refuses to start and says why;
* the web service keeps serving — browsing was never heavy work, and stopping it
  would only make the machine harder to inspect.

It does **not** stop: the web service, the existing Samba/FileBrowser/backup
services, or anything else on the machine. There is no command in this project
that could.

## 8. Uninstalling and rolling back

Rolling back is safe because the project owns only two directories and one binary.

```sh
# 1. Stop, and stay stopped.
albumctl pause --reason "rolling back"
systemctl disable --now albumctl-web.service
systemctl disable --now albumctl-thumbs.timer     # if it was ever enabled
systemctl stop albumctl-thumbs.service

# 2. Remove the pieces. The media tree is not mentioned here, on purpose.
rm -f /usr/local/bin/albumctl
rm -f /etc/systemd/system/albumctl-web.service
rm -f /etc/systemd/system/albumctl-thumbs.service
rm -f /etc/systemd/system/albumctl-thumbs.timer
systemctl daemon-reload

# 3. Keep the state, or remove it. Keeping it makes a reinstall cheap.
#    Deleting it loses only: the index, the task history and the thumbnails.
#    It does not touch one photo.
mv /var/lib/albumctl  /var/lib/albumctl.removed-$(date +%s)
mv /var/cache/albumctl /var/cache/albumctl.removed-$(date +%s)
```

To roll back a *version* rather than the whole thing:

```sh
# The previous binary and its checksum.
sha256sum -c albumctl-linux-armv7.sha256
install -m 0755 albumctl-linux-armv7 /usr/local/bin/albumctl
albumctl version
```

The index schema is versioned (`PRAGMA user_version`). A newer binary upgrades it
on open and reports what it did. An **older** binary refuses to open a newer index
rather than guessing, so a downgrade needs the state directory set aside or a
matching binary. That refusal is the intended behaviour, not an obstacle to work
around.

Nothing in the rollback path deletes, moves or edits an original file, and no
step requires stopping another service.

## 9. What to collect for a bug report

```sh
albumctl version
albumctl status   --config /etc/albumctl/config.toml --json
albumctl doctor   --config /etc/albumctl/config.toml
albumctl cache-report --config /etc/albumctl/config.toml --json
journalctl -u albumctl-web -u albumctl-thumbs --since "-2h" --no-pager
cat /proc/meminfo | head -5
cat /proc/pressure/memory 2>/dev/null
```

These are read-only and safe to run while the machine is loaded; none of them
touches a decoder.
---

# Part 2 — the console (`nasctl`)

The console is a separate service from `albumctl`. It reads and reports; it does
not scan, decode, transcode or write files. Its evidence and unverified items are
in `HUB-STAGE-B.md`.

## 9. The console says something is wrong

`nasctl status` prints what needs attention at the bottom. `GET /api/v1/overview`
returns the same list in `warnings`, and the front page shows it above everything
else. Work through it in this order.

### 9.1 "heavy work is stopped: mode X is in force"

```sh
nasctl mode show              # what is set, who set it, when it expires
nasctl mode clear watching    # clear a window you set
nasctl mode clear manual_pause
```

`resource_guard` cannot be cleared by hand, and that is deliberate: it is the
machine's own state. If it is in force, find the cause (§9.3) rather than looking
for a way to clear it.

### 9.2 "storage X is unavailable"

The configured root cannot be opened. This is reported as unavailable, **not** as
an empty directory, so an absent array does not look like deleted files.

```sh
findmnt /mnt/data            # is the array mounted?
nasctl doctor                # names the root and the reason
```

The console keeps running with the root unavailable, and it does not delete any
index or cache because a source went away.

### 9.3 "new heavy work would not start: <reason>"

The resource guard refused. The reason is a stable string:

| Reason | Meaning |
| --- | --- |
| `mem_below_start` | MemAvailable is below the start threshold (400 MiB by default) |
| `mem_below_warn` | below the warning threshold (250 MiB): no new file starts |
| `mem_below_stop` | below the stop threshold (200 MiB): a run in flight is stopped |
| `swap_in_use` | swap is being consumed |
| `psi_memory_pressure` / `psi_io_pressure` | the machine is stalling, not just short |
| `no_resource_signal` | `/proc/meminfo` could not be read at all |

`no_resource_signal` is the one to treat as a hard failure: the console cannot
tell whether the machine has room, so it must assume it does not.

Do not respond by raising the thresholds. The thresholds are what this machine was
measured against; raising them to make one large file succeed is the mistake the
project exists to avoid.

### 9.4 "job X is interrupted"

The console stopped while that job was open. `interrupted` means **the outcome is
unknown** - not that it failed and not that it finished.

```sh
nasctl jobs show <id>        # the event trail, with the reason
```

### 9.5 "job X still has a live process"

An orphan: a process the console recorded but did not start in its current life.
It is reported and **not** adopted or killed, because the console cannot prove the
process is its own and killing a process group whose provenance is uncertain could
hit something that is not ours.

```sh
ps -o pid,pgid,etime,cmd -p <pid>
```

Decide by hand. If it is a leftover decoder, `kill` it. If it is something else,
leave it.

## 10. Starting and stopping the console

```sh
# Foreground, which is what you want while trying it out
nasctl serve --config /etc/albumctl/config.toml

# Under systemd
systemctl start nasctl-web
systemctl status nasctl-web
journalctl -u nasctl-web -n 50
```

The unit sets `Restart=no` **on purpose**. A console that restarts itself in a
loop while the machine is short of memory is the failure mode this project exists
to avoid, and its startup reconciliation would rewrite job history repeatedly.

`serve` reconciles at startup: jobs that were open and whose process is gone
become `interrupted`. It prunes finished jobs beyond the history cap and audit
rows beyond the retention window. All of that is printed.

## 11. The console database

Its own SQLite file, separate from the album index:

```
<paths.state_dir>/hub.db      jobs, events, audit, modes, approvals
```

Separate on purpose: the album index's schema moves with the scanner, and a
console that cannot open its job database should still start and say so.

```sh
nasctl doctor --hub-db /var/lib/albumctl/hub.db    # integrity check
```

If it is corrupt, the loss is bounded: job history, the audit log and the mode
table. **No photo, no index row and no thumbnail is in it.** Move it aside and the
console recreates it on the next start:

```sh
systemctl stop nasctl-web
mv /var/lib/albumctl/hub.db{,.broken}
systemctl start nasctl-web
```

The console comes up with an empty history. Note that **a manual pause is stored
there**, so moving the file aside also clears a pause; check `nasctl mode show`
afterwards and re-apply it if it was deliberate.

## 12. Checking the console's page still works

The frontend has no build step: it is plain JavaScript embedded in the binary. That
removes a whole class of toolchain problems, but it also means nothing compiles the
page, so a mistake in it is not caught by `go build`.

There is a harness for exactly that. It renders the real page in a real DOM against
a running server and checks that every panel populates:

```sh
# One-off, on the development machine. Nothing here runs on the NAS.
cd scripts/uitest
npm install jsdom                     # the only dependency
node render.mjs http://127.0.0.1:8090 <token>
```

If `npm install` cannot create `node_modules` here (some build environments strip
it from the checkout), install elsewhere and point the harness at it:

```sh
JSDOM_PATH=/path/to/node_modules node render.mjs http://127.0.0.1:8090 <token>
```

It exits non-zero when a view fails to populate. It does **not** check layout or
appearance: jsdom has no layout engine, so a broken grid or an overflowing column
is invisible to it. It prints that limitation in its own output so nobody mistakes
a pass for "the page looks right".

## 13. Rolling back the console

Nothing the console does is hard to undo, because it does not write to your media.

```sh
systemctl stop nasctl-web
systemctl disable nasctl-web
rm -f /etc/systemd/system/nasctl-web.service
rm -f /usr/local/bin/nasctl
# Optional: the state. This loses job history, the audit log and any pause.
rm -rf /var/lib/albumctl/hub.db*
```

FileBrowser and Samba are untouched by any of this. The console is an additional
read-only view; removing it leaves the machine exactly as it was, apart from the
files listed above.
