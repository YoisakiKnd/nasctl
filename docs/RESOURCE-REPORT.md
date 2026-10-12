# albumctl — measured resource report

Everything below is either a raw observation with the command that produced it, or
it is labelled as policy, as a guess, or as unverified. Nothing here is
extrapolated from a different machine, and no number here is an emulator's number
presented as the target's.

Read section 0 before quoting any figure from this file.

**This file covers `albumctl` only.** The OneCloud Hub console (`nasctl`) is a
separate binary with its own measurements, including an ARMv7-under-emulation
figure that is materially higher than the x86_64 one and a provisional memory
ceiling that follows from it: see `HUB-STAGE-B.md` §5. Do not apply the numbers
below to the console.

---

## 0. Status of each claim, in one table

| Plane | What it means | Measured where | Verified on the target? |
| --- | --- | --- | --- |
| Process RSS | `/proc/<pid>/status` `VmRSS` / `VmHWM`, summed over a process tree | Yes, x86_64 dev host | **No** |
| cgroup memory | `memory.current`, `memory.stat`, `memory.events`, `memory.peak`, `MemoryMax` enforcement | **No — not available on this host** | **No** |
| Whole machine | `/proc/meminfo` `MemAvailable`, swap, memory and I/O PSI | Yes, x86_64 dev host | **No** |
| ARMv7 binary behaviour | does the artefact run and produce the same results | qemu-arm-static only | **No** |

Three consequences that matter more than the numbers:

1. **The cgroup plane is unmeasured.** On this host `/sys/fs/cgroup` exposes the
   memory controller in `cgroup.controllers`, but `memory.current`, `memory.max`,
   `memory.events` and `memory.peak` do not exist in the namespace's view, and
   creating a child cgroup under `/sys/fs/cgroup/system.slice/dsh-web.service`
   returns `Permission denied`. So the isolation path was never exercised against
   a live, restricted worker here. What *was* verified is the refusal: with
   `guard.require_cgroup_isolation` at its default `true`, `albumctl thumbs`
   declines to run at all on this host. Enforcement itself is unverified.
2. **An RSS number is not a cgroup number.** A process can sit inside its RSS
   ceiling and still be killed by `MemoryMax` because a child, a mapped file or
   charged page cache used the rest. Nothing in this report claims otherwise.
3. **Nothing here says "it cannot hang".** A bounded, constant footprint measured
   for 60 files is not a statement about a 5,684-file library, an hour-long run,
   or a machine whose RAID0 is rebuilding. That is exactly what the per-file
   ceilings, the timeout, the trip state and the human-gated stages are for.

---

## 1. Environment actually used

```
$ uname -srmo
Linux 6.8.0-139-generic x86_64 GNU/Linux

$ nproc
4

$ awk '/MemTotal|MemAvailable|SwapTotal/{print $1,$2}' /proc/meminfo
MemTotal: 8131752 kB
MemAvailable: 3114132 kB
SwapTotal: 0 kB

$ go version
go version go1.27.1 linux/amd64

$ mount | grep cgroup
cgroup2 on /sys/fs/cgroup type cgroup2 (rw,nosuid,nodev,noexec,relatime,nsdelegate,memory_recursiveprot)
```

* **x86_64**, 4 CPUs, 7.76 GiB usable. This is a *development host*, roughly eight
  times the target's memory.
* **No swap is configured at all** (`SwapTotal: 0`). Any stop rule that depends on
  swap being used could not fire here even in principle; those paths are covered
  only by unit tests with an injected sampler.
* Memory and I/O PSI exist and read `0.00 at idle`; they were never driven up by
  this workload, so the PSI stop paths did not fire either.
* `GOARCH=amd64` and `GOARCH=arm` have different pointer widths, different
  allocator behaviour and different GC pacing. Absolute RSS does not transfer.

Two measurement rigs, both under `/tmp`, both built from `internal/synth/cmdgen`
(synthetic media written by this repository's own generator, so the fixtures are
reproducible from source):

| Rig | Media tree | Index |
| --- | --- | --- |
| A `/tmp/measure` | 39 files, 3,552,059 B | small, rebuilt per run |
| B `/tmp/curve` | 5,759 files, 3,609 of them media (the 39 above, hard-linked 715 times), plus one synthetic 314,573,824 B `IMG_9000.jpg` | 3,609 rows, 2,469,888 B |

At this scale the whole index fits in a few megabytes of SQLite page cache, which
is why the curves below look flat. A real 5,684-file library with genuine EXIF
will have a larger index; see section 7.

---

## 2. Scanner (`albumctl scan`)

Every row started from an empty index (`rm -rf state cache`), so the column "rows
indexed" is the size of the index the scanner built, not the number of rows it
touched. Peak RSS is the whole process's `Maximum resident set size` from
`/usr/bin/time -v`.

```
$ for lim in 500 1000 2000 6000; do
    rm -rf state cache
    /usr/bin/time -v albumctl --config config.toml scan --collection photos --limit $lim
    # then: rows from `albumctl status`, index bytes from state/index.sqlite3
  done
```

| `--limit` | Rows indexed | Peak RSS | `index.sqlite3` | Wall |
| --- | --- | --- | --- | --- |
| 500 | 500 | 22,196 kB (21.7 MiB) | 442,368 B | 0.60 s |
| 1000 | 1000 | 23,256 kB (22.7 MiB) | 761,856 B | 1.30 s |
| 2000 | 2000 | 24,252 kB (23.7 MiB) | 1,421,312 B | 3.01 s |
| 6000 | 3,609 | 25,864 kB (25.3 MiB) | 2,469,888 B | 6.49 s |

* **7.2× the rows costs 3.7 MiB more.** Growth is sub-linear and dominated by the
  Go runtime plus a fixed SQLite page cache, not by per-row state.
* The last row stopped at 3,609 because the tree only *has* 3,609 media files —
  `--limit` caps work per run, it is not a row target. The 6,000 run therefore
  spent its budget re-verifying rows it already had: 3,609 indexed or unchanged
  out of 5,757 files seen.
* Throughput: 1.2 ms/file at 500 rows, 1.8 ms/file at 3,609 rows, per-file cost
  including `lstat`, the extension decision and one SQLite upsert.
* At 3,609 rows a subsequent no-op scan reports `0 indexed, 3608 unchanged` and
  takes a fraction of the first pass — the change test is `size` + `mtime`, so
  re-walks are cheap and no image is ever opened to decide.

What bounds it, and why the curve cannot run away: the directory walk reads one
batch of up to 1,024 entries at a time (`daemon.ReadDir`), the scanner holds one
`Result` structure and writes each file as it goes, `store.MaxWorkBatch` caps a
`NextProbe`/`NextThumb` batch at 1,000 rows, and the HTTP layer caps a page at
200 items. There is no code path that loads a library or a result set into RAM.

What the walk skipped, from the same rig, because a scan report is only
interpretable with its skips — verbatim, from a fresh index on the 5,759-file
tree:

```
directories:  7176 visited, 0 excluded by name, 0 past the depth limit, 0 on other filesystems
files seen:   5758 (3609 kept, 2149 not a media extension, 0 excluded by name)
              0 symlink(s), 2 hidden name(s) skipped, 0 unreadable directorie(s)
entries:      3609 indexed, 0 unchanged, 0 changed, 0 deferred
probed:       3599 of 3609 attempted
skipped:      10 (over budget or unsupported)
failed:       0
marked gone:  0 (a complete walk can do this)
```

The 2,149 non-media files are the `.tmp`/`.txt`/`.db`/extension-less sidecars the
rig creates on purpose; they are classified and dropped by extension, never
opened. The 2 hidden names start with `.` and are skipped before any `stat`. No
symlink is followed. 3,599 of 3,609 media files were probed and 10 were skipped
before decode (the 314 MB image among them) — nothing was handed to a decoder on
the basis of "let us see whether it fits".

---

## 3. Thumbnails (`albumctl thumbs`, concurrency 1)

Rig B, index pre-built with `scan --limit 6000`, then from a clean cache. Two
different measurements are in play and they are not interchangeable: `/usr/bin/time
-v` reports the peak of the **whole process tree** (supervisor + worker), while the
sampling loop reads the supervisor's and the worker's `/proc/<pid>/status`
separately.

| `--limit` | Files | Tree peak (GNU time) | Worker's own `VmHWM` (max/file) | Cache after | Wall |
| --- | --- | --- | --- | --- | --- |
| 1 | 1 | 29,568 kB (28.9 MiB) | 29.1 MiB | 8.8 KiB | 0.14 s |
| 5 | 5 | 29,568 kB (28.9 MiB) | 29.1 MiB | 34.9 KiB | 0.23 s |
| 20 | 20 | 29,568 kB (28.9 MiB) | 29.2 MiB | 139.5 KiB | 0.89 s |
| 60 | 60 | — | 28.0 MiB (sampled live) | 418.6 KiB | — |

The 60-file run was measured independently by polling both the supervisor and its
worker every 50 ms:

```
supervisor own max RSS: 24512 kB | worker(s) max RSS observed: 28652 kB | MemAvailable min: 3072860 kB
```

* **15× the file count, the same footprint.** The tree peak is byte-identical at
  1, 5 and 20 files because it is the same fixture dominating each time; the
  supervisor's own footprint sampled at 60 files is 24,512 kB (23.9 MiB), i.e.
  15× the work did not raise the process that drives it. Per-file state really is
  released; the loop does not accumulate.
* **The per-file peak is set by the image, not by the batch.** The worker's own
  `VmHWM` was 10.2 MiB on the small fixtures and 29.1 MiB on the 4000×3000
  fixtures in the same batch. A pixel budget is what keeps this bounded — the
  ceiling has to hold for one file, and that is why the ceiling is per *file*.
* **The cache stays small on purpose**: 60 thumbnails, 418.6 KiB, ≈ 7 KiB each.
  One size, longest edge 512 px, JPEG; no pyramid, no precomputed large image.
* **The supervisor's sampling is honest, and sometimes it misses.** On the
  1-file run the live sampler caught a peak of 11.8 MiB while the worker's own
  `VmHWM` said 29.1 MiB, because a fast file can finish between two samples. The
  report prints exactly that rather than presenting the sampled number as the
  truth, and prints the worker's self-reported `VmHWM` alongside it.

Fail-closed behaviour on an over-budget file, quoted from the API for the
synthetic 314,573,824 B image with `decode.max_file_bytes = 67108864`:

```
"probe_state": "skipped",
"note": "file_too_large:314573824>67108864",
"placeholder": true
```

It is indexed, listed with a placeholder, and never handed to a decoder. That is
the intended trade: show the file, skip the work.

---

## 4. Web service (`albumctl serve`)

Same index (3,609 rows, 2,469,888 B), 20 thumbnails in cache, service on
`127.0.0.1:8101`:

| Moment | RSS |
| --- | --- |
| Idle, just after start | 13,136 kB (12.8 MiB) |
| After walking 3,800 items in pages of 200 | 22,232 kB (21.7 MiB) |
| After 20 thumbnails | 23,744 kB (23.2 MiB) |
| After 600 thumbnails | 24,796 kB (24.2 MiB) |
| After 3 more full paging passes | 24,624 / 24,456 / 24,676 kB |
| During and after a full 314,573,824 B original download | +16 kB (23,908 → 23,924 kB) |
| After a 1 MiB range slice | unchanged (23,924 kB) |

```
$ curl -s -r 1048576-2097151 .../api/v1/media/3609/raw -D headers -o /dev/null
range http=206 bytes=1048576
Content-Range: bytes 1048576-2097151/314573824
$ curl -s .../api/v1/media/3609/raw -o /dev/null -w '%{http_code} %{size_download} %{time_total}\n'
200 314573824 0.171019
```

* **~24.7 MiB is a plateau, not a slope.** Three paging passes over the whole
  library moved the number by ±200 kB, and 600 served thumbnails moved it by
  50 kB. Growth is the Go runtime and SQLite's page cache reaching a steady state.
* **Streaming a 300 MiB original is memory-neutral**: +16 kB, i.e. a copy buffer
  rather than a decode. 314 MB in 0.171 s from page cache is a `copy` path; no
  image is decoded and no child process is started, which `internal/server`'s
  import-boundary test enforces structurally.
* **Range works and does not fetch the whole file**: `206`, exact
  `Content-Range`, 1 MiB transferred for a 300 MiB object.
* **Against the stated idle target of ≤ 32 MiB: met here at 12.8 MiB idle and
  24.7 MiB busy, on amd64, on an idle host.** It is not a verified ARM number.
  The 48/64 MiB `MemoryHigh`/`MemoryMax` candidates still have to be tested on
  the target, and the busy plateau plus Go's GC headroom is the reason to keep
  the ceiling above 32 MiB rather than at it.

---

## 5. Whole-machine view during a batch

Sampled every 50 ms by an external loop while 60 thumbnails were generated
(`thumbs --limit 60 --max-runtime 10m`), reading `/proc/meminfo`:

| Signal | Observation |
| --- | --- |
| `MemAvailable`, lowest reading in the whole measurement session | 3,072,860 kB (2.93 GiB of 7.76 GiB) |
| `MemAvailable` at the time the environment was captured | 3,114,132 kB (2.97 GiB) |
| Swap in use | 0 — the host has no swap configured at all |
| PSI memory / I/O `some avg10` | 0.00 before and after the batches — read at idle, never driven up |

The 60-file batch ran on a machine with roughly 3 GiB available out of 7.76 GiB.
On the target there is 981 MiB of physical memory in total, so **this is the least
transferable number in the report**: the thresholds (400 start / 250 warn / 200
stop MiB) are never approached here, and the low-memory stop paths were therefore
never observed firing for real on any host. They are exercised by unit tests
against injected samplers (`internal/resource/guard_test.go`,
`meminfo_test.go`) and by the admission-refusal measurements, not by a starved
machine. Whether the *policy* numbers are right for a 981 MiB machine is still a
guess — see section 7.

---

## 6. ARMv7 artefact: what was exercised, and what an emulator cannot tell you

```
$ file dist/albumctl-linux-armv7
ELF 32-bit LSB executable, ARM, EABI5 version 1 (SYSV), statically linked, stripped
$ sha256sum dist/albumctl-linux-armv7
53b87503cbf3937329532f33c1afbe6f3735b3b39635c6794154d774cee1ea90  (12,648,608 B)
```

Rebuilt after the last source change to the shipped binary (the runnable-process
test for a worker group in `internal/resource/proc.go`, which the supervisor uses
to decide whether a stop was obeyed); two runs from the same source with the same
`SOURCE_DATE_EPOCH` produce byte-identical output (`cmp` reports no difference),
and the earlier value for the preceding revision was
`797b1007bc94662456eb2f55bb66d212ac89087832074b103feda918a2828644`.

Run under `qemu-arm-static` on the x86_64 host (rig: 39-file synthetic library,
`start_hold = "3s"`, `require_cgroup_isolation = false`):

| Command | Result |
| --- | --- |
| `version` | `albumctl 0.1.0 (target linux/arm/GOARM7, runtime linux/arm)` |
| `scan --limit 200` | 33 indexed, 24 probed, 9 skipped, 0 failed, 0 deferred, 330 ms |
| `scan --limit 200` again | 0 indexed, 33 unchanged, 0 deferred, complete walk, 0 marked gone |
| `thumbs --limit 5 --max-runtime 5m` | 5 processed, 0 failed, 5.2 KiB cache, 4.387 s wall, stop `limit_reached` |
| `thumbs --limit 3 --max-runtime 2m` (rebuilt artefact) | 3 processed, 0 skipped, 0 failed, stop `limit_reached`; the 20.8 MiB worker peak printed here is an emulator number and is deliberately not carried into any claim above |

Three things this does show, and one it does not:

* The artefact **runs**: pure-Go SQLite works on 32-bit ARM, the worker joins its
  cgroup-less path, writes thumbnails, and reports the same counters as on amd64.
* The **3 s sustained start hold is really applied**: 4.387 s wall for work that
  takes 0.23 s natively is the hold, not slowness (0.6 s of it is the run itself
  under emulation).
* The binary is **self-sufficient**: no interpreter, no shared library, so there
  is no libc or runtime version to get wrong on Debian 12.
* It does **not** show memory behaviour. Under `qemu-arm-static` the guest process
  *is* a host process, and `/proc/<pid>/status` for it describes the emulator,
  including its translated-code and memory-translation overhead. The `21.0 MiB`
  the worker self-reported under emulation is an emulator artefact. **No ARMv7
  memory number is claimed anywhere in this report**, and none of the figures in
  sections 2–4 were taken under emulation.

---

## 7. Policy, measurement, and guesses — kept apart

| Value | Status |
| --- | --- |
| 22–26 MiB scanner peak for a 3,609-row index | measured (amd64) |
| 24.5 MiB supervisor / 28–29 MiB worker peak per file | measured (amd64) |
| Constant footprint from 1 to 60 files | measured (amd64) |
| 12.8 MiB idle / 24.7 MiB busy web service | measured (amd64) |
| +16 kB to stream a 300 MiB original; range is a slice | measured (amd64) |
| 3.7 MiB growth for 7.2× the rows | measured (amd64) |
| 48/64 MiB web `MemoryHigh`/`MemoryMax` | policy candidate, untested on target |
| 96/128 MiB worker ceilings | policy candidate, untested on target |
| 400 / 250 / 200 MiB `MemAvailable` gates, 3 s sustained start hold | policy candidate, untested under real memory pressure |
| 512 px single thumbnail, concurrency 1 | fixed by design |
| "These thresholds are right for a 981 MiB machine" | **guess.** Derive it in stage D from the first authorised 10-file batch |

An honest expectation to carry into stage D: the synthetic library is *small and
unusually well-behaved*. Real photos bring EXIF strings of unknown length, real
JPEGs vary more in decode cost, and a 5,684-file index will be larger than 2.4 MB.
The shapes measured above (constant per batch, sub-linear in rows, neutral while
streaming) are the parts that should hold; the absolute numbers are the parts that
should be re-measured, on the target, one variable at a time.

---

## 8. Reproducing this

```sh
# build the dev-host binary and the fixture generator
go build -o /tmp/albumctl ./cmd/albumctl
go build -o /tmp/cmdgen  ./internal/synth/cmdgen

# a small library
/tmp/cmdgen /tmp/measure/media

# the large library: 39 files hard-linked into ~715 directories
cp -al /tmp/measure/media/photos /tmp/measure/media/photos-001 ... # repeated

# scanner curve, fresh index per row
rm -rf state cache && /usr/bin/time -v /tmp/albumctl --config config.toml \
    scan --collection photos --limit 2000

# thumbnail batch, with both supervisor and worker RSS sampled
/tmp/albumctl --config config.toml thumbs --collection photos --limit 60 --max-runtime 10m

# web service footprint
/tmp/albumctl --config config.toml serve &
curl -s "http://127.0.0.1:8101/api/v1/media?collection=photos&per_page=200&page=1" -o /dev/null
awk '/VmRSS/{print $2}' /proc/$(pgrep -f 'albumctl.*serve')/status
```

Two rigs were configured with `require_cgroup_isolation = false` and
`start_hold = "0s"` to make them measurable on this host. Those are *not* the
shipped defaults, and neither is an acceptable setting for the target machine.

## 9. What these measurements do not prove

* They do not prove the album cannot hang, cannot be OOM-killed, or cannot hurt
  the machine. They bound the parts that were observed, on an idle development
  host with 8 GB of memory.
* They say nothing verified about the target: not its RSS, not its cgroup
  behaviour, not its I/O. `qemu-arm-static` numbers are excluded from every memory
  claim above.
* They do not prove the original files are intact, that the RAID0 is healthy, or
  that any backup is current. Nothing in this repository inspects those.
* They do not show the stop paths firing. Every low-memory, swap and PSI stop was
  exercised by tests with injected readings, not by a starved machine.
* They do not show a full filesystem. The cache *policy* ceiling was exercised by
  test and stops a run; a real `ENOSPC` needs a size-limited filesystem, which
  cannot be mounted here, so the recovery path for a genuinely full SSD - a failed
  write recorded as a failed file, nothing partial published - rests on the
  atomic-write tests rather than on an observed disk-full event.
* A resource trip on the target is possible with these numbers set too high.
  The thresholds are candidates to be tightened after stage D, never widened to
  make one oversized image succeed.