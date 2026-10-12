# OneCloud Hub — stage B evidence

This document records what was built in stage B, what was measured, and what was
**not** verified. It exists because the difference between those three categories
is the whole point of the project: an unmeasured claim about a 981 MiB machine is
how the previous deployment went wrong.

Everything below was produced on the development host (x86_64, Go 1.27.1) unless a
line says otherwise. **No measurement here was taken on the onecloud**, and the
target has not been touched.

---

## 1. What was built

Nine new packages plus a second binary. The album service (`albumctl`) is
unchanged apart from the configuration section it shares.

```
cmd/nasctl/                    the console binary
internal/domain/               modes, job states, principals, risk, redaction
internal/operations/           the registry: validation, parameters, audit summary
internal/hubops/               the permission table: 42 declared operations
internal/jobs/                 admission, mode gating, approvals, audit, execution
internal/files/                our own file manager, os.Root containment
internal/hubstore/             jobs, events, audit, modes, approvals (own SQLite)
internal/hubweb/               HTTP API + the unified single-page UI (embedded)
internal/suzu/                 read-only adapter for IWantWatchAnime
internal/nasctl/               the CLI over the same job layer
```

### 1.1 The separation that makes the console safe

The console is a **separate binary** from the album service, and its import graph
contains no decoder. That is not a convention; it is enforced by a test
(`internal/hubweb/boundary_test.go`) which walks the module's import graph and
fails if `hubweb` can reach `internal/thumbs`, `internal/scan`, `internal/probe`,
`internal/exif`, `internal/store`, `os/exec` or the `image` packages.

Reverse-verified: adding `_ "internal/probe"` to `hubweb` makes the test fail, and
it reports both the direct import and the transitive reach through `exif`.

The consequence is concrete: **an HTTP request cannot decode an image, launch
ffmpeg, walk a media tree or start a library scan**, because the code to do those
things is not reachable from the process that serves the request.

---

## 2. Path containment: measured behaviour, not assumption

The file manager resolves every path through `os.Root`, which uses `openat`/
`statat` under the kernel's control. There is no window between "check the path"
and "open the path" for a symlink to be swapped in.

The actual semantics were established by experiment, and two of them are
counter-intuitive enough to be worth stating plainly:

| Case | Result | Why |
|---|---|---|
| `../outside/secret.txt` | refused | escapes the root |
| `a\b.txt` (backslash in the name) | **accepted** | on Linux it is one filename, not a directory |
| absolute path `/etc/passwd` | refused | must be relative to a root |
| symlink → outside the root | refused | escapes the root |
| symlink → inside the root (relative) | **followed** | it does not escape |
| symlink → inside the root (absolute) | **refused** | `os.Root` refuses absolute links unconditionally |
| `sub/updir` where `updir → ".."` | **followed** | `..` from `sub/` is the root, which is inside |
| `/` and `""` and `"."` | the root itself | what the API renders and sends back |

The last four are the ones a reader is most likely to get wrong, so they are
asserted directly in `internal/files/files_test.go` rather than left implicit.

The backslash row is a **fixed bug**, not a design choice. The first version
refused any path containing a backslash, reasoning that a Windows-style traversal
should not be expressible. On Linux a backslash is an ordinary filename character,
so that rule made every file with one in its name unreachable: the browser listed
it, and every attempt to open or download it failed. Containment is enforced by
`os.Root`, which refuses a real escape whether or not the name contains a
backslash, so the refusal was protecting nothing and breaking a class of files.

**Reverse verification.** Replacing `os.Root` with the naive
`filepath.Join(root, rel)` makes exactly the symlink-escape tests fail. The tests
detect the difference between kernel-enforced containment and a string check.

### 2.1 A hang found by testing

Opening a FIFO for reading blocks until a writer appears. A request for a named
pipe would therefore have parked an HTTP handler, its goroutine and its file
descriptor indefinitely. The fix is `O_NONBLOCK` on the open plus the existing
regular-file check; the test that found it (`TestOpenRefusesNonRegularFiles`)
hung the suite before the fix.

This was found by running the tests, not by reasoning about them, which is the
argument for having them.

---

## 3. The permission model

`internal/hubops/ops.go` is the permission table. Reading it top to bottom tells
an operator what the console can be asked to do, to what, and what each thing
requires. Nothing about permission lives in a handler.

Rules enforced in code and covered by tests:

| Rule | Where enforced | Test |
|---|---|---|
| Reads need no approval | registry validation | `TestReadOperationsNeedNoApproval` |
| Every non-read needs approval | registry validation | `TestMutatingOperationsAllNeedApproval` |
| Scans/thumbnails require an explicit `limit` | declaration | `TestEveryBoundedBatchRequiresAnExplicitLimit` |
| Every file operation names a storage root | declaration | `TestStorageOperationsNameTheirRoot` |
| No operation offers a shell | declaration | `TestNoOperationOffersAShell` |
| The console never changes the backup | declaration | `TestTheConsoleNeverStartsOrStopsTheBackup` |
| An agent's identity comes from its credential | `hubweb.authenticate` | `TestAnAgentCredentialCannotBeEscapedByOmittingTheHeader` |
| An AI cannot grant an approval | `jobs.Grant` | `TestAnAICannotGrantItsOwnApproval` |
| A helper grants nothing | `jobs.Grant` | `TestAHelperGrantsNothing` |
| An approval is scoped and expires | `jobs.Grant` | `TestAnApprovalMustBeScopedAndExpire` |
| A changed parameter needs a new approval | `hubstore.ConsumeApproval` | `TestAnApprovalDoesNotCoverAChangedParameter` |
| A read-only console refuses every write | `jobs.Admit` | `TestAReadOnlyConsoleRefusesEveryWrite` |
| An AI cannot start heavy work by default | `jobs.Admit` | `TestAnAICannotStartHeavyWorkByDefault` |
| A manual pause outranks an automatic window | `domain.ModeSet.Effective` | `TestModePrecedencePutsSafetyFirst` |
| A resource trip outranks a maintenance window | `domain.ModeSet.Effective` | `TestAResourceTripOutranksAMaintenanceWindow` |

**Reverse verification.** Removing the AI check from `jobs.Grant` makes
`TestAnAICannotGrantItsOwnApproval` fail. Removing the path containment from
`suzu.playable` makes `TestPlaybackIsRefusedOutsideTheLibraryRoot` fail.

---

## 4. Job state honesty

The job vocabulary is closed, and the transitions that would let the console claim
work it did not do are refused:

* `queued → completed` is **refused**. A job must run before it can be recorded as
  done; otherwise the history says work happened that never started.
* A handler that returns without finishing is recorded as **failed**, not
  completed, with its stop reason preserved.
* `interrupted` is distinct from `failed`. After a restart, a job whose process is
  gone becomes `interrupted` - the outcome is unknown and the work must be re-run,
  which is different from "it ran and failed".
* `unknown` has its own colour in the UI and its own `needs_attention` flag. No
  branch renders it as healthy.
* A late progress write from a worker cannot resurrect a finished job.

**Restart safety.** `Reconcile` runs once at startup, before anything can be
admitted. A live process is reported as an **orphan** and left alone: the console
did not start it in this life, cannot supervise it, and killing a process group
whose provenance is uncertain could hit something that is not ours. A human
decides.

### 4.1 A self-deadlock found by testing

`Reconcile` originally fetched each job's full row through the connection pool
while holding a transaction. The pool holds a single connection by design, so the
query waited for a connection that could not be released until the transaction
ended. It presented as a **10-second** stall (the SQLite busy timeout), not as an
error - exactly the shape of bug that becomes "the console is slow sometimes" in
production. Reading the row inside the transaction fixed it, and the package's
test time went from 10.2 s to 0.22 s.

---

## 5. Measured resource use

### 5.1 Console RSS under request load

Measured on the development host with `/proc/<pid>/status`. These are **x86_64
numbers, not ARMv7 numbers**, and the memory figures must not be presented as
target measurements.

| Phase | VmRSS |
|---|---|
| Idle, just started | 12.8 MiB |
| After 200 directory listings | 16.9 MiB |
| After 200 overview requests | 19.3 MiB |
| After 200 small-image previews | 19.4 MiB |
| After **100 streams of a 300 MB file** | 19.5 MiB |
| After 30 full 300 MB video streams + 200 ranged seeks (9.3 GB total) | 18.5 MiB |

The last two rows are the ones that matter. Streaming 30 GB through the console
moved resident memory by **4.5 MiB with no upward trend**: the transfer uses a
64 KiB bounded buffer, so a 300 MB video costs the same as a 300 KB one. The
console does not decode, so its memory does not grow with the size of any file it
serves.

These x86_64 figures sit well inside any sane ceiling. **They are not the target's
numbers**, and §5.2 shows why that distinction matters here more than usual: the
same binary under emulation reaches roughly two and a half times this. The
provisional ceiling in the unit is set from §5.2, not from this table.

### 5.2 The ARMv7 binary under emulation — and why its number is not the target's

The ARM binary was measured the same way, under `qemu-arm-static`. The result is
**higher and must not be read as a target measurement**:

| Phase | Total RSS | `rwxp` (qemu JIT) | remainder |
|---|---|---|---|
| Startup | 34.8 MB | 14.3 MB | 20.5 MB |
| 400 requests | 42.1 MB | 16.4 MB | 25.7 MB |
| 2000 requests | 42.9 MB | 16.4 MB | 26.5 MB |
| 2800 requests | 47.6 MB | 18.4 MB | 29.2 MB |

The memory **plateaus**; it does not grow without bound. Over 2800 requests it
rose from 34.8 MB to 47.6 MB and flattened, and repeating the *same* request
repeatedly did not keep growing it.

**Where the number comes from.** A 14 MB anonymous `rwxp` mapping appears in the
console's address space that a bare ARM HTTP server under the same emulator does
not have at all. It grows in 2 MB steps as more distinct code is executed and then
stops - that is qemu's translation-block cache, not the console. Controls run to
establish this:

| Program under `qemu-arm-static` | RSS |
|---|---|
| minimal Go program | 3.5 MB |
| SQLite-linked ARM binary (12 MB) | 3.5 MB |
| bare `net/http` server | 3.5 MB |
| bare server doing JSON + hashing work | 3.5 MB |
| **the console** | **34.8 → 47.6 MB** |

So the gap is not binary size, not the SQLite dependency, and not merely "an ARM
binary under qemu" - it is specific to executing this program's code. That is
consistent with a JIT cache sized by executed code, but **it is not proof**, and
the honest reading is: under emulation the console reaches ~47.6 MB and plateaus.

**What this means for the systemd unit.** The first draft set `MemoryMax=64M`,
chosen from the x86_64 figure of 19 MB. The emulated figure is within 16 MB of it.
Two readings are possible and this environment cannot distinguish them:

1. the extra memory is emulation, and the target will resemble the x86_64 number;
2. some of it is real allocation the x86_64 run did not exercise.

**The consequence is a decision, not a claim here.** The unit now ships
`MemoryHigh=72M` / `MemoryMax=96M`: large enough to contain the emulated figure
plus margin, still far below the machine's 981 MiB. The reasoning is that a
ceiling which is too tight OOM-kills a console that is merely doing its job - a
visible but annoying failure - whereas one that is looser than necessary still
bounds the process. That is the right way to be wrong.

**`MemoryMax` remains unvalidated.** Re-measure on the target and set it to the
measured peak plus margin. The number in the unit is a considered provisional
value, not a measurement of the onecloud.

### 5.3 The three memory figures, kept apart

The objective requires RSS, cgroup memory and whole-machine resources to be
reported separately, because adding them together or reading one as another is how
a budget goes wrong. Measured on the development host:

| Figure | Value | Source | What it means |
|---|---|---|---|
| Console RSS | 12.9 MiB idle | `/proc/<pid>/status` `VmRSS` | the pages this process is charged for |
| Console cgroup `anon` delta | ~21.0 MB | `/sys/fs/cgroup/.../memory.stat` | the slice's anonymous memory, measured as a delta across start/stop |
| Whole-machine `MemAvailable` | 3.15 GB of 7.75 GB | `/proc/meminfo` | what is left for everything, not for this console |

**These are three different quantities and none is a substitute for another.**

* **RSS is not the cgroup.** RSS is what this process is charged for; the cgroup
  figure covers the whole slice, which on this host also holds the session that
  started the console. The `anon` delta of ~21 MB against a 12.9 MiB RSS is the
  clearest illustration: reading either one as "the console's memory" is wrong, and
  the difference is other processes, not the console.
* **`MemAvailable` is not a budget for us.** 3.15 GB available does not mean the
  console may take 3.15 GB. The thresholds in the guard exist precisely because
  the machine's spare memory is shared with SSH, Samba, the backup and the
  containers, none of which this project may stop.
* **`memory.current` was not readable here.** The service slice does not delegate
  it (`cat /sys/fs/cgroup/memory.current` → *No such file or directory*), and
  creating a child cgroup was denied, which is why `doctor` reports cgroup
  isolation as unavailable on this host. `memory.stat` *is* readable, which is
  what the delta above uses. On the target, systemd with `Delegate=yes` should
  make `memory.current` available, and the ceiling in the unit is what would be
  enforced against it - **that has not been verified on the onecloud.**

`memory.events` for the slice shows `oom_kill 0` and `max 0`, so the console did
not trip a limit during these runs. Whole-machine memory PSI was `avg10=0.00`,
i.e. no measurable memory stall pressure at the time. Both are recorded as
observations of one moment, not as properties of the console.

### 5.4 Range support

Verified against a 300 MB file:

```
full request   → 200 OK, Accept-Ranges: bytes, Content-Length: 314572800
range 1000-2999 → 206 Partial Content, Content-Range: bytes 1000-2999/314572800
```

Video is streamed with ranges; images are capped at 32 MiB and refused with an
explanation, because a browser renders an image whole and cannot be served a
range of one. Capping video at the same limit would have made playback impossible,
which is the feature the cap would have been breaking.

### 5.5 Query and listing bounds

| Bound | Value | Enforced in |
|---|---|---|
| Directory entries examined per listing | 20 000 | `files.Limits.MaxEntries` |
| Page size (clamped, never trusted) | 200 | `files.Limits.MaxPageSize` |
| Search results | 200 | `files.Limits.SearchMaxResults` |
| Search entries examined | 20 000 | `files.Limits.SearchMaxExamined` |
| Search wall clock | 2 s | `files.Search` |
| Search depth | 6 | `files.Search` |
| Job rows returned | 500 | `hubstore.Jobs` |
| Audit rows returned | 500 | `hubstore.Audit` |
| Request body | 64 KiB | `hubweb.decodeBody` |
| Concurrent requests | 8 | `hubweb` middleware |
| Concurrent streams | 3 | `hubweb` middleware |

Every listing is one directory level, paged, and reports `truncated` with a
reason when it hits a cap - so a partial answer is never presented as a complete
one.

---

## 6. Tests

Counts are of top-level `func Test` declarations; several contain subtests, so the
number of assertions is higher.

| Package | Tests | What they cover |
|---|---|---|
| `internal/domain` | 7 | mode precedence, state transitions, redaction |
| `internal/files` | 20 | traversal, symlink semantics, paging, caps, FIFO refusal, backslash names |
| `internal/hubops` | 10 | the permission table's own invariants |
| `internal/hubstore` | 26 | job lifecycle, cap under concurrency, reconcile, approvals |
| `internal/jobs` | 24 | every gate, approvals, mutex, audit, guard refusal |
| `internal/hubweb` | 34 | HTTP end to end: containment, XSS, modes, Range, bounds, download, header escaping, credentials, suzu wiring |
| `internal/suzu` | 14 | read-only open, schema drift, path containment |
| **Total new** | **135** | |
| `scripts/uitest/render.mjs` | 28 checks | the page in a real DOM: every view populates, navigation, search, download links, theme |

The 135 are Go test functions; the 28 are DOM checks in the separate UI harness.
Both are run manually on the development machine - see `RUNBOOK.md` §13.

Whole repository: `go build ./...` and `go vet ./...` clean; `go test -count=1
./...` passes across **25 packages**.

These were found by tests, by probing the running server, by rendering the page, and
by auditing the objective against the delivered code:

1. the FIFO hang (§2.1);
2. the `Reconcile` self-deadlock (§4.1);
3. `cleanRel` rejecting `/`, which would have made the file manager's **first**
   listing fail in the browser while every other path worked - the frontend sends
   back the `/` that the API renders.

Another was found by probing the running server rather than by a test: an unknown
`/api/` path returned the HTML shell with status 200, which would have told a
caller that an endpoint exists. It is now a 404, and the response body is JSON.

The rest were found by rendering the page and by auditing the objective against the
delivered code - which is why both are worth doing even when the tests are green:

5. **The page's pluralisation read "1 entries".** Found by rendering the page
   rather than by reading the code: a directory with one file is common, and the
   first fix produced "5 entrys" - which is why the plural form is now passed
   explicitly at the call site rather than derived by appending "s".
6. **There was no download endpoint.** The UI offered a "Download" link that
   pointed at the *preview* endpoint, which refuses types a browser cannot render
   (`415`) and images above the cap (`413`) - so the button failed for exactly the
   files somebody would want to download. Downloads and previews are now separate
   endpoints with different policies: a preview is for looking at something in the
   page and is capped for images; a download is for taking a file and streams
   anything with a bounded buffer.
7. **A file with a backslash in its name could not be opened** (see the table
   above).
8. **The suzu client was opened but never wired into the web layer.** The serving
   command opened the database, then passed everything to the server *except* the
   client. The CLI reported the library correctly while the page said "not
   configured" - the two views of the same fact disagreed, and only comparing them
   exposed it. `hubweb.New` now takes the client as a **parameter** rather than
   accepting it as a settable field, so a caller cannot forget it and the compiler
   catches the omission. Reverse-verified: forcing `Suzu: nil` makes
   `TestAConfiguredSuzuIsReportedByTheWebLayer` fail.
9. **The AI distinction was advisory, not enforced.** The design says an agent
   authenticates "with its own token, distinct from the user's", but the first
   implementation identified an agent only by an `X-Nasctl-Agent` header it sent
   about itself. An agent could therefore escape *every* agent restriction - the
   audit of its reads, the prohibition on starting heavy work, the approval rules -
   by simply not sending the header. Agents now have real credentials
   (`hub.agents[]`), the credential decides the principal, and agent tokens are
   checked before the operator token so the two cannot be confused. Reverse-
   verified: restoring the header-trusting version makes
   `TestAnAgentCredentialCannotBeEscapedByOmittingTheHeader` fail on both the audit
   and the 401.

---

## 7. Not verified — read this before trusting anything above

**Nothing here has been run on the onecloud.** The following are explicitly
unverified, and must not be described as working:

1. **ARMv7 runtime behaviour.** The binaries are cross-compiled for
   `linux/arm/GOARM=7`, are static (`CGO_ENABLED=0`, no `NEEDED` entries) and run
   under `qemu-arm-static` for `version`, `doctor`, `files list`, `mode` and the
   containment refusals. **Emulation is not the target.** Memory figures under
   qemu are not ARM measurements and are not reported as such.
2. **Memory ceilings.** The unit ships `MemoryHigh=72M` / `MemoryMax=96M` as a
   considered **provisional** value, chosen because §5.2 shows the emulated ARM
   figure reaching ~47.6 MB while this environment cannot separate emulation
   overhead from real allocation. Neither number is a measurement of the onecloud.
   Re-measure on the target and set the ceiling to the measured peak plus margin.
3. **cgroup isolation on the target.** The development host cannot create a child
   cgroup under its service slice, so `doctor` reports the isolation as
   unavailable. Whether the target's systemd can enforce `MemoryMax` on this unit
   has not been checked, and the project's own rule is that heavy work stays
   disabled when a limit cannot be enforced.
4. **suzu's real schema.** The adapter was written against the audited schema and
   tested against a reconstruction of it. It adapts to renamed columns and refuses
   a schema it does not recognise, but it has not read the real
   `suzu.db` on the machine.
5. **The real media tree.** Every filesystem test uses a synthetic tree. Behaviour
   against the RAID0 array - its mount identity, its file count, its symlink
   habits, its filenames - is unverified.
6. **Concurrent use by two people.** The concurrency tests exercise the request
   slot limit and the job cap, but not a browser and an agent working at once
   against a real library.
7. **The UI's *appearance*.** The page is now rendered in a real DOM
   (`scripts/uitest/render.mjs`, jsdom) against a live server: 28 checks confirm
   that the shell loads, all seven views populate their panels, the breadcrumb
   navigates and returns, search filters and clears, download links carry
   filenames, and the theme toggle switches. Reverse-verified: making the listing
   return no entries fails four checks and exits 1. The harness was also run against
   the **ARMv7 binary under qemu-arm-static**, where it passes the same 28 checks -
   so the page is served correctly by the artefact that would ship, not only by the
   development build.
   **What that does not cover: layout, CSS, responsive breakpoints, and anything
   visual.** jsdom has no layout engine, so it cannot see a broken grid or an
   overflowing column. Those still need a real browser, and this environment has
   none - so the *appearance* remains unverified, and the harness says so in its
   own output.
8. **Backup coordination.** The console reports backup state read-only and the
   album service yields to it through the existing `backupgate`. The console has
   no backup adapter yet, and no status command has been run against a real backup
   tool.

---

## 8. What this stage deliberately does not do

* **No file writes.** Upload, mkdir, rename, move, copy, trash and restore are
  declared and refused. Reading - listing, searching, previewing and downloading -
  is implemented. `hub.storage[].writable = true` is rejected by the
  configuration validator rather than accepted and ignored, so the flag cannot
  promise a capability the binary does not have.
* **No scanning or decoding from the console.** `album.scan` and `album.thumbs`
  are declared with the right bounds but have no handler in this binary. They
  belong to `albumctl`, which owns the resource guard and the worker isolation.
* **No scheduler.** `schedule.enable` is declared and unimplemented. Automatic
  scheduling remains an operator decision that requires a separate approval.
* **No writes to suzu.** The database is opened `mode=ro` and `_query_only`.
* **No MCP transport.** The registry is already the single source of policy, so a
  transport adds no permission logic - but it is not built.

---

## 9. Artefacts

```
dist/albumctl-linux-armv7     ELF 32-bit ARM EABI5, statically linked
dist/nasctl-linux-armv7       ELF 32-bit ARM EABI5, statically linked
dist/SHA256SUMS               checksums for both
dist/README.txt               build record: toolchain, sizes, readelf output
dist/deps.txt                 runtime dependencies (none beyond the kernel)
```

One build command produces both binaries (`scripts/build-armv7.sh`), so the two
cannot drift apart in how they were produced. Both are `CGO_ENABLED=0` and
statically linked: no libc, no dynamic linker, and nothing to install on the
target beyond the kernel and a writable state directory.

Built reproducibly: two runs from the same tree with `SOURCE_DATE_EPOCH` set
produce byte-identical output for both binaries.

Verify:

```sh
sha256sum -c dist/SHA256SUMS      # from the repository root
```

The build record's checksums are the authority; they are not repeated here,
because a hash copied into prose goes stale the moment the code changes and then
looks like evidence while being wrong.
