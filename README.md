# nasctl — a low-resource console for an ARM NAS

Two programs for a small ARMv7 NAS, built around one rule: **the machine staying
up, and the original files staying intact, matter more than the features.**

* **`albumctl`** — a photo and video index with a read-only web viewer. It scans
  directories and generates thumbnails, always as a bounded task with an explicit
  limit, a wall-clock budget and a resource guard that can stop it.
* **`nasctl`** — a read-only console over the same machine: file browsing,
  download and preview, the album index, the watch library, backup status, and the
  console's own tasks.

They are separate binaries on purpose. `nasctl` cannot reach a decoder, the
scanner or a child process — that is enforced by an import-graph test, not by
convention — so an HTTP request to the console cannot start a full-library scan.

Nothing here moves, renames, deletes or rewrites an original file. File writes are
declared and refused, and the configuration validator rejects the flag that would
claim otherwise.

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

Development and offline testing are complete for the album service and for the
console's read-only stage. **Nothing has been run on the target machine yet.**
That is a deliberate boundary, not an oversight, and the list of what is therefore
unverified — ARMv7 runtime behaviour, memory ceilings on the target, cgroup
enforcement there, the real media tree, the real watch database — is in
[`docs/HUB-STAGE-B.md`](docs/HUB-STAGE-B.md) §7.

## Build

Requires Go 1.27.1 or newer. Nothing else: no cgo, no cross-compiler, no Docker,
no Node, no Python. The frontend is plain JavaScript embedded in the binary, with
no build step.

```sh
go build ./...                       # both binaries
go test ./...                        # the test suite

# The ARMv7 artefacts, reproducibly
SOURCE_DATE_EPOCH=1760000000 ./scripts/build-armv7.sh
sha256sum -c dist/SHA256SUMS
```

The build produces static 32-bit ARM binaries with no shared-library dependency,
so the target needs nothing installed. Two builds from the same commit with the
same `SOURCE_DATE_EPOCH` produce identical bytes, which is what makes the published
checksum meaningful; CI checks that on every push.

## Running it

`albumctl` and `nasctl` read the same configuration file. Copy the example and
edit the paths:

```sh
cp configs/albumctl.example.toml /etc/albumctl/config.toml
$EDITOR /etc/albumctl/config.toml

albumctl doctor --config /etc/albumctl/config.toml    # read-only health check
nasctl doctor --config /etc/albumctl/config.toml
```

Every policy value in the example is the built-in default, so a line can be
deleted to get the default back. Only `[paths]` and `[[collections]]` are local to
a machine. A misspelled key is refused by name rather than ignored — most numbers
in the file are ceilings, and a silently dropped one would leave you believing a
limit is in force when it is not.

Then, deliberately and by hand:

```sh
# The console, read-only. It never starts work of its own.
nasctl serve --config /etc/albumctl/config.toml

# Album work, bounded, one collection at a time.
albumctl scan   --collection photos --limit 500 --dry-run   # preview first
albumctl scan   --collection photos --limit 500
albumctl thumbs --collection photos --limit 200 --max-runtime 20m
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

The console has **no per-user authentication**. Its access control is the bind
address, and that is checked at startup rather than documented after the fact: a
public address is refused unless the operator explicitly opts in for an offline
test. Bind it to a Tailscale address or to loopback behind an SSH tunnel. An
optional shared token adds a bearer requirement; it distinguishes "has the token"
from "does not", and every holder is the same principal.

AI callers can be given their own credentials (`[[hub.agents]]`). The credential
decides the principal, so an agent cannot escape its restrictions by not
announcing itself: its reads are audited, it may not start heavy work unless
enabled per agent, and it cannot grant itself an approval.

## What it does not do

No AI classification, face recognition or OCR. No video transcoding. No
continuous library watching, no background loop. No moving, renaming or deleting
originals, and no EXIF write-back. No cloud upload, sync or backup logic — the
existing tools keep doing that, and this project yields to them rather than
replacing them. No arbitrary shell, and no operation that accepts a server path.

## License

MIT. See [`LICENSE`](LICENSE).
