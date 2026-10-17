# Album HTTP API v1 (read-only)

This is the internal album API (`internal/albumapi`). nasctl mounts it in
process and exposes it to the browser under `/api/v1/album/...` behind its own
authentication; it no longer has a listener of its own. The paths below are the
handler's own paths.

The API is **read-only**. It never scans, never decodes an original image, never
runs FFmpeg/ExifTool, never loads the whole library into memory and never queues cache
work. Missing thumbnails return a placeholder immediately.


## Conventions

* All JSON is `application/json; charset=utf-8`.
* Every string that reaches the DOM must be inserted with `textContent` /
  `createElement`, **never** `innerHTML`. The embedded frontend does exactly this and
  contains no `innerHTML`, `outerHTML`, `insertAdjacentHTML` or `document.write`
  anywhere; `internal/e2e` asserts it.
  The server does **not** HTML-escape `<`, `>` or `&` inside JSON strings, and must
  not: a file name is data, and rewriting it in transport would corrupt the name that
  is displayed and downloaded. A name such as `<img src=x onerror=alert(1)>.jpg` is
  transported byte for byte as a JSON string, which is inert until something puts it
  into markup. Escaping is the renderer's job, and the renderer never parses HTML.
* `limit` default `60`, hard max `200`. `cursor` is opaque; pass back `next_cursor`
  verbatim. Never build cursors by hand.
* Errors: `{"error":{"code":"...","message":"..."}}` with an appropriate status.
* Nothing in the API accepts a server filesystem path. Media are addressed only by
  numeric `id`; collections only by configured `code`.

## GET /api/v1/status

Fast (must not wait for a running decode). Used by the header/status panel.

```json
{
  "version": "0.1.0",
  "schema": 1,
  "now": 1760000000,
  "paused": true,
  "pause_reason": "manual",
  "pause_since": 1759900000,
  "collections": [
    {"code":"photos","name":"Photos","enabled":true,"media_count":5684,"mount_ok":true,"mount_note":""}
  ],
  "totals": {
    "media": 5684, "images": 5400, "videos": 284, "other": 0,
    "probed": 5684, "thumb_ok": 1200, "thumb_pending": 4484, "thumb_failed": 3,
    "unsupported": 12, "with_gps": 91, "undated": 14, "missing": 2
  },
  "running": null,
  "last": {"kind":"thumbs","state":"stopped","reason":"mem_below_stop","ended_at":1760000000,
            "processed":37,"skipped":4,"failed":1,"detail":"MemAvailable fell to 198 MiB"},
  "cache": {"files":1200,"bytes":98765432,"max_bytes":2147483648,"used_pct":4.6},
  "resource": {
    "mem_available_kb": 812000,
    "mem_total_kb": 991232,
    "swap_free_kb": 0,
    "start_threshold_kb": 409600,
    "warn_threshold_kb": 256000,
    "stop_threshold_kb": 204800,
    "pressure_tripped": false,
    "pressure_tripped_at": 0,
    "read_only": true,
    "web_service_rss_bytes": 14680064
  },
  "warnings": ["read-only service; no authentication, access is limited by bind address"]
}
```

Field notes:

* `running` and `last` are top-level, not nested under a `tasks` object. `running` is
  `null` when no task holds the lock.
* `totals` counts: `media` is every indexed row; `images`/`videos`/`other` split it by
  kind; `other` is a kind that was classified but is neither (an unsupported format is
  counted in `media` and in `unsupported`, not in `images`). `unsupported` counts rows
  whose format is deliberately not enabled. `with_gps` counts rows with coordinates.
  `undated` counts rows with no usable timestamp at all — they are *not* placed on the
  timeline, because a date is never invented for them. `missing` counts rows whose
  original was absent at the last **complete** scan.
* `cache` here is index-derived and cheap; `GET /api/v1/cache` reports the cache
  directory itself.
* `resource.read_only` is always `true` for this service and is present so an operator
  can confirm it from outside.
* `paused` is the persistent manual pause flag, and it survives a restart. The frontend
  shows a banner and never offers an action that would start work. The API has no
  endpoint that starts work at all.

## GET /api/v1/collections

```json
{"items":[{"code":"photos","name":"Photos","enabled":true,"media_count":5684,
           "mount_ok":true,"mount_note":"","first_seen":1760000000,"last_scan":1760000000}],
 "paused":true}
```

## GET /api/v1/media

Timeline / album / search listing.

Query parameters:

| name | values | notes |
|---|---|---|
| `collection` | collection code | omit = all collections |
| `limit` | 1..200, default 60 | |
| `cursor` | opaque | keyset pagination, stable under inserts |
| `sort` | `time_desc` (default), `time_asc`, `name_asc` | |
| `q` | utf-8 text | simple substring search over name/model/make/path |
| `from` / `to` | `YYYY-MM-DD` inclusive | filters on `taken_at` (UTC day) |
| `year` | `YYYY` | shortcut for from/to |
| `month` | `YYYY-MM` | shortcut for from/to |
| `kind` | `image` \| `video` \| `other` | |
| `gps` | `1` | only media that has coordinates |
| `fallback` | `1` | only media whose shown time is a *fallback* rather than a real capture time |
| `unflagged` | `1` | accepted as an alias of `fallback`; prefer `fallback` |
| `include_missing` | `1` | include media whose original was absent at the last complete scan |

```json
{
  "items": [ MediaItem, ... ],
  "next_cursor": null,
  "total_estimate": 5684,
  "paused": true,
  "degraded": false,
  "warnings": []
}
```

`total_estimate` is a cheap cached count over the same filter, capped at `100000`.

### MediaItem

```json
{
  "id": 123,
  "collection": "photos",
  "name": "IMG_1234.JPG",
  "rel_dir": "2023/05",
  "kind": "image",
  "mime": "image/jpeg",
  "bytes": 3145728,

  "taken_at": "2023-05-01T12:33:04",
  "taken_at_unix": 1682944384,
  "taken_src": "exif",
  "taken_label": "EXIF DateTimeOriginal",
  "taken_is_fallback": false,
  "taken_tz": "offset",
  "tz_offset_min": 480,

  "width": 4032,
  "height": 3024,
  "orientation": 6,
  "make": "Apple",
  "model": "iPhone 12",
  "gps": {"lat": 31.2014, "lon": 121.4123},
  "duration_ms": null,

  "probe_state": "ok",
  "thumb_state": "ok",
  "thumb_url": "/api/v1/media/123/thumb",
  "raw_url": "/api/v1/media/123/raw",
  "playable": false,
  "placeholder": false,
  "note": null
}
```

Field rules:

* `taken_src` is one of `exif`, `filetime`, `filename`, `none`.
  `taken_is_fallback` is `true` for everything except `exif`.
  `taken_label` is a human string the UI may display next to fallback values.
* `taken_tz` is `offset` when EXIF carried `OffsetTime*`, otherwise `naive`.
  With `naive` the displayed time is the local wall clock as recorded by the camera and
  is **not** converted to the viewer's timezone.
* `taken_at` is `null` when no time could be determined.
* `gps` is `null` unless the file really carried GPS EXIF. GPS is never guessed.
* `orientation` is the EXIF orientation of the original (1–8, 0 when absent). Thumbnails
  are already turned upright from it, so a client must **not** rotate `thumb_url` again.
  Browsers apply it to `raw_url` themselves. `width` and `height` are the stored,
  unrotated dimensions. (Before 0.6.0 thumbnails were stored unrotated.)
* `placeholder` is `true` when this item cannot be thumbnailed (unsupported format or
  over budget) so the UI can render it as a tile with a glyph instead of an image.
* `note` carries a short machine reason such as `unsupported_format` or
  `over_pixel_budget`.

## GET /api/v1/media/{id}

Same object plus a bounded `exif` object:

```json
{"item": { ...MediaItem... },
 "exif": {"TakenAt":"2023-05-01T12:33:04","Make":"Apple","Software":"16.4",
          "ExposureTime":"1/120","FNumber":"1.8","ISO":"64",
          "FocalLength":"4.2 mm","LensModel":"..."},
 "paths": {"display":"2023/05/IMG_1234.JPG"},
 "warnings": []}
```

`exif` values are strings, already HTML-escaped, and truncated to 512 bytes each; at
most 24 keys are returned.

## GET /api/v1/media/{id}/thumb

* `200 image/jpeg` for a cached thumbnail (`Cache-Control: public, max-age=604800`,
  `ETag`, supports `If-None-Match` → `304`).
* `200 image/svg+xml` placeholder when no thumbnail exists, with header
  `X-Thumb: placeholder` and `Cache-Control: public, max-age=60`.
  This is generated from a fixed template — no decode, no queueing.
* `404` when the media id is unknown.
* Optional `size`, only ever `512` in v1 (the configured single size). Any other value
  is clamped, never used to scale up to original resolution.

## GET /api/v1/media/{id}/raw

Streams the original file with bounded buffers (`io.ReadSeeker` + `http.ServeContent`).

* `Accept-Ranges: bytes`, correct `Content-Range` / `416` handling, `HEAD` supported.
* `Content-Type` from the extension allowlist; anything unknown is
  `application/octet-stream` (never sniffed into something executable).
* `Content-Disposition: inline; filename*=UTF-8''...` with the stored base name.
* `ETag: "<size>-<mtime_ns>"`, `Last-Modified` from mtime.
* The file is opened only after re-verifying that its real path is still inside the
  configured collection root. Symlinks that leave the root are refused with `403`.

## GET /api/v1/media/{id}/download

Same as `/raw` but `Content-Disposition: attachment`.

## GET /api/v1/timeline

Aggregate counts for the year/month scrubber. Bounded (one row per month present).

```json
{"years":[{"year":2023,"count":812,"months":[{"month":5,"count":120}]}],
 "undated": 14,
 "paused": true}
```

## GET /api/v1/cache

Read-only cache report; never deletes.

```json
{"files":1200,"bytes":98765432,"max_bytes":2147483648,"max_files":200000,
 "used_pct":4.6,"shard_depth":3,"indexed_thumbnails":1200,
 "source":"cache directory",
 "notes":["cache pruning is manual only: `nasctl album cache-gc`",
          "the web service never adds to the cache and never removes from it"]}
```

The file and byte counts come from the cache **directory**, so this report and
`nasctl album cache-report` cannot disagree about the space actually in use: `bytes` is
what the files occupy on disk, not what the index believes. A note is added
when the index records thumbnails for a different number of files than the directory
holds, which is what a temporary file left by an interrupted write or a hand-deleted
cache entry looks like. Nothing is removed, ever, by this endpoint.

## Static assets

`GET /` serves the embedded single-page app. `GET /app.js`, `GET /app.css` are served
from `go:embed` with long cache headers and a content hash query the page appends.
No external CDN, no web fonts, no analytics, no map tiles.

## Explicitly absent

* No write endpoints. No delete. No move/rename. No EXIF write-back.
* No upload endpoints.
* No "start scan" / "start thumbnails" endpoint that bypasses `pause`.
* No shell, no exec, no server-path parameter.