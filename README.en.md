# nasctl — a lightweight NAS console

One program for resource-conscious Linux NAS devices, built around one rule: **the machine staying
up, and the original files staying intact, matter more than the features.**

* **`nasctl`** — a NAS console: file browsing, preview, album, native anime
  subscriptions, media library, local backups and task history. Anime uses RSS,
  qBittorrent and optional TMDB metadata.
* **`nasctl album`** — the album processing commands in the same binary. They
  scan directories and generate thumbnails, always as a bounded task with an
  explicit limit, a wall-clock budget and a resource guard that can stop it.

Start only `nasctl serve` for daily use: it reads the local album index directly.
Scans and thumbnails re-execute the same binary as `nasctl album ...` in a
separate process and cgroup; nothing else needs installing. Import-graph tests
keep decoders, scanners and subprocess implementations out of the web query
layer; explicit operator tasks pass admission and resource checks first.

> Upgrading from 0.2.x or earlier: read [`docs/UPGRADE.md`](docs/UPGRADE.md) first. The former `albumctl` binary is now `nasctl album`; the standalone
> `albumctl serve` viewer and the `hub.album.api_base` remote mode were removed.
> Config, index and cache paths (`/etc/albumctl`, `/var/lib/albumctl`) are
> unchanged; the systemd timer is now `nasctl-album-thumbs`.

Anime archiving preserves the downloaded originals and creates links or copies
inside a separate library. File roots default to read-only; explicit writable
roots support operator uploads, mkdir, bounded copy/cross-root move, rename,
recoverable trash and explicitly confirmed permanent cleanup. Background tasks
report progress and support cancellation without overwriting targets. See [file management](docs/FILES.md) and
[media playback](docs/MEDIA.md).

## Why it exists

This was written after a NAS was lost during a photo-indexing job: the machine
became unreachable while metadata and thumbnails were being generated, with
`MemAvailable` down to about 106 MiB. The project is the response to that, and its
design follows from it:

* no unbounded work, ever — every heavy operation takes an explicit count and time
  limit;
* one media worker at a time, in its own process, with a resource guard in front of
  every file and a stop that kills the whole process group;
* a placeholder image instead of a decode when a thumbnail is missing, so a page
  load never triggers work;
* a durable pause that survives a restart, and no automatic restart after a
  resource stop;
* nothing that stops SSH, Tailscale, Samba, the existing file browser, the backup
  or any container.

## Status

The native single-episode RSS → download → archive workflow and web UI are implemented.
Auto-download and polling default to off. See [`docs/ANIME.md`](docs/ANIME.md) for
configuration and current limits. AutoBangumi is the feature reference; no legacy
IWantWatchAnime source or Telegram integration is required. The web UI is split into
everyday user pages and an admin console (see docs/FRONTEND.md). Linux amd64, arm64 and ARMv7 static
builds pass; Linux/arm64 full race tests, native systemd/cgroup album processing,
protocol fixtures, compatible upgrade/rollback and browser video playback pass.

**Real NAS and qBittorrent integration remains unverified.** The historical list
of target checks — ARMv7 runtime behaviour, memory ceilings on the target, cgroup
enforcement there, the real media tree, the real watch database — and current
verification evidence are in [`docs/VERIFICATION.md`](docs/VERIFICATION.md).

## Build

Requires Go 1.27.1 or newer. Nothing else: no cgo, no cross-compiler, no Docker,
no Node, no Python for Go-only builds. The Svelte frontend's generated assets are
committed and embedded in Go. To edit the UI, use Node 22.12+ and run
`cd frontend && npm ci && npm run check && npm run build`.
See [`docs/FRONTEND.md`](docs/FRONTEND.md) for the visual system and build workflow.

```sh
go build ./...                       # both binaries
go test ./...                        # full suite requires Linux /proc and process control

# The ARMv7 artefacts, reproducibly
SOURCE_DATE_EPOCH=1760000000 ./scripts/build-armv7.sh
sha256sum -c dist/SHA256SUMS
```

The script produces static 32-bit ARM binaries with no shared-library dependency,
so the target needs nothing installed. Two builds from the same commit with the
same `SOURCE_DATE_EPOCH` produce identical bytes, which is what makes the published
checksum meaningful; CI checks that on every push.

For other architectures, use e.g. `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o nasctl ./cmd/nasctl`.
macOS supports console development; album resource protection and the full suite require Linux.

## Running it

The console and the album commands read the same configuration file. Copy the example and
edit the paths:

```sh
cp configs/nasctl.example.toml /etc/albumctl/config.toml
$EDITOR /etc/albumctl/config.toml

nasctl album doctor --config /etc/albumctl/config.toml    # read-only health check
nasctl doctor --config /etc/albumctl/config.toml
```

Every policy value in the example is the built-in default, so a line can be
deleted to get the default back. Only `[paths]` and `[[collections]]` are local to
a machine; native anime also requires explicit download/library directories.
Photo collections can be omitted for a files-only or native-anime console. A misspelled key is refused by name rather than ignored — most numbers
in the file are ceilings, and a silently dropped one would leave you believing a
limit is in force when it is not.

Then, deliberately and by hand:

```sh
# The console; anime polling is controlled by hub.anime.auto_poll.
nasctl serve --config /etc/albumctl/config.toml

# Album work, bounded, one collection at a time.
nasctl album scan   --collection photos --limit 500 --dry-run   # preview first
nasctl album scan   --collection photos --limit 500
nasctl album thumbs --collection photos --limit 200 --max-runtime 20m
```

Nothing is scheduled by default. The nightly timer ships disabled and must be
enabled as a separate decision.

## Documentation

| Document | What it covers |
| --- | --- |
| [`docs/DESIGN.md`](docs/DESIGN.md) | the album service: module boundaries, data flow, resource budget, format matrix, what is deliberately not implemented |
| [`docs/HUB-AUDIT.md`](docs/HUB-AUDIT.md) | the audit of the existing code the console was built on |
| [`docs/HUB-DESIGN.md`](docs/HUB-DESIGN.md) | the console: module map, UI sketch, permission table, state machine, data structures, migration and rollback |
| [`docs/HUB-STAGE-B.md`](docs/HUB-STAGE-B.md) | what was built, the defects found and how, measured resources, and **everything unverified** |
| [`docs/HUB-API.md`](docs/HUB-API.md) | the console's HTTP API: every route, parameter, response and status code |
| [`docs/API.md`](docs/API.md) | the album service's HTTP API |
| [`docs/CLI-ACCESS.md`](docs/CLI-ACCESS.md) | both CLIs, the permission model and the access control |
| [`docs/RUNBOOK.md`](docs/RUNBOOK.md) | operating procedures, troubleshooting, index recovery, rollback |
| [`docs/RESOURCE-REPORT.md`](docs/RESOURCE-REPORT.md) | measured resource use for the album service |

## Access control

See [deployment, upgrades and compatible rollback](docs/DEPLOYMENT.md) for the current setup and resource policy.

The console supports up to 32 local administrators, operators and viewers.
Members share the configured files and media; server-side roles control writes
and administration. Initialize a new deployment with `nasctl init`, then use its
private local challenge to set up the first administrator. Keep `hub.setup=true`
in the base configuration so loss of the state volume cannot enable anonymous
access. Public binds are still refused unless explicitly allowed locally; use
HTTPS, Tailscale or an SSH tunnel for remote access. HttpOnly sessions expire
after 12 hours and are revoked on logout, restart or account changes.
`server.token` remains a compatible device-administrator Bearer credential.
Local password resets and offline account/settings repair preserve recovery
evidence. See [browser authentication](docs/AUTH.md) and the
[remaining feature checklist](docs/ROADMAP.md).

AI callers can be given their own credentials (`[[hub.agents]]`). The credential
decides the principal, so an agent cannot escape its restrictions by not
announcing itself: its reads are audited, it may not start heavy work unless
enabled per agent, and it cannot grant itself an approval.

## What it does not do

No AI classification, face recognition or OCR. No video transcoding. No
continuous photo-library watching; anime RSS polling is optional. Album indexing
and anime archiving preserve originals; file management runs only explicit
operator actions. No EXIF write-back. Cloud upload and cloud synchronization remain deferred. Local file backups and verified
no-overwrite restores are documented in [`docs/BACKUPS.md`](docs/BACKUPS.md). Private
service-state packages, offline restore and crash recovery are documented in
[`docs/STATE-BACKUPS.md`](docs/STATE-BACKUPS.md). No arbitrary shell; HTTP file operations
accept configured storage IDs and relative paths.

## License

MIT. See [`LICENSE`](LICENSE).

Chinese: [`README.md`](README.md)
