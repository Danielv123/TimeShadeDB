# TimeShadeDB v3 HTTP API

Status: API version 1 (`catalog.version`). JSON shapes are the Go types in
`internal/v3/core/apitypes.go`, mirrored in `web/src/api/types.ts`. Example responses are in
`web/fixtures/`. Binary formats are in [FORMAT.md](FORMAT.md) section 7.

## 1. Basics

- Base path: `/api/v3/`. Every path below is relative to it. The static export (WP12)
  writes the same relative paths under its root, so one viewer `DataSource` works with
  both; section 6 lists what the static export contains.
- Path parameters:
  - `{ds}`: a datastore id (`gw-bc41437d~2E1.nauvis.player`), `{sfc}`: a surface id
    (`gw-bc41437d~2E1.nauvis`). Both must be canonical (FORMAT.md 1); others are 400.
  - `{z}`: level, `0` or negative (`-3`). `{x}`, `{y}`: decimal tile coordinates.
  - `{ver}`, `{from}`, `{to}`: decimal versions. `{start}`, `{t}`: decimal `TimeMs`.
    `{date}`: `YYYY-MM-DD` (UTC).
- Query `t`: a decimal `TimeMs`, or `latest`, which resolves to `max(now, LastT)` of the
  datastore (so live content is included).
- Default listen address is `127.0.0.1`. Binding anything else requires `--ingest-token`.
- Responses carry `X-TSDB-Now: <map clock ms>` on `catalog`, `view`, `movers/*/live.bin`
  and as the `now` field of live events. Viewers keep a data clock from it (never
  `Date.now()` alone), as illisis `movers.ts` does.
- Compression: binary and JSON bodies are sent with `Content-Encoding: zstd` when
  `Accept-Encoding` includes `zstd`, else `gzip`, else identity, with
  `Vary: Accept-Encoding`. The server caches encoded bytes per (resource, encoding).
- Binary bodies use `Content-Type: application/octet-stream`; JSON uses
  `application/json`. The format is identified by its magic.

### 1.1 Cache classes

| Class | `Cache-Control` | Used for |
|---|---|---|
| immutable | `public, max-age=31536000, immutable` | content addressed by version or closed time |
| short | `public, max-age=10` plus `ETag` | catalog, open day indexes and timelines, overlays |
| live | `public, max-age=2` | live mover payload, `view` at or after the horizon |
| none | `no-store` | live tiles, SSE, ingest |

`private` replaces `public` when a read token is configured. Errors are never cached
(`no-store`). Immutable headers are only sent for responses whose content can never change:
a version that exists, a closed window, a final day.

### 1.2 Errors

Status and body `{"error": "...", "code": "..."}` (`core.ErrorResponse`):

| Status | code | When |
|---|---|---|
| 400 | `bad_request` | malformed parameter, id or query |
| 400 | `bad_tile` | tile outside the grid or wrong level (`core.ErrBadTile`) |
| 401 | `unauthorized` | missing or wrong token |
| 404 | `not_found` | unknown datastore or surface, tile without data, version, window, keyframe or day that does not exist (`ErrUnknownDatastore`, `ErrNoTile`, `ErrNoVersion`) |
| 413 | `too_large` | ingest body over the cap |
| 503 | `overloaded` | ingest backpressure; `Retry-After` seconds given |
| 500 | `internal` | anything else |

**Hostile requests** (PLAN.md §4.3 G2): parameter validation, the grid bound check and the
datastore lookup happen before any per-tile allocation. A request for an invalid or unknown
tile allocates nothing that outlives it and answers in under 1 ms.

### 1.3 Authentication

- `--read-token`: when set, every GET needs `Authorization: Bearer <read token>` (or
  `?token=` for EventSource, which cannot set headers). When unset, reads are open.
- `--ingest-token`: `POST ingest` needs `Authorization: Bearer <ingest token>`. Required
  when listening on anything but loopback.
- Tokens are compared in constant time. Behind Clusterio, the plugin proxies
  `/api/timeshade/*` to the sidecar after its own auth and the `timeshade.view` permission
  (WP10); the sidecar itself listens on 127.0.0.1 with random tokens.

## 2. Terrain

### `GET catalog`

`core.Catalog`: maps with title, instance bounds and per-surface Universal Edges segments,
surfaces with their datastores (window, horizon, first/last update, palette version, z0
bounds, levels that have tiles) and mover summary (horizon, first, last, days), icons, and
`now`. Cache: short. Static: yes (`catalog.json`; see section 6 for the file name rule).

### `GET palette/{ds}/{ver}.bin`

TSP3 with exactly the first `ver` entries. 404 if `ver` is greater than the current
palette length. Cache: immutable. Static: yes (the exporter writes the versions tiles
reference, at least the latest).

### `GET view/{ds}/{z}?bbox=x0,y0,x1,y1&t=`

`core.ViewResponse`: the state of every tile holding data in the inclusive tile rectangle at
level z, at time t. Each tile has `v` (version effective at t), and `live` and `ls`
(live sequence) when live content applies (z0: open-window updates; z < 0: z0 changes not
yet in a coarse version, FORMAT.md 4). At most 4,096 tiles per request (400 otherwise).
Tiles without data are omitted. Cache: immutable when t < horizon, else live. At z < 0 the
bound is min(horizon, `Pyramid.BuiltUpTo`): coarse versions are built after the commit
that moves the horizon, so between a commit and the next build (or after a failed build)
answers for t in [BuiltUpTo, horizon) can still change and are served as live.
Static: no (use `timeline`).

### `GET timeline/{ds}/{z}/{date}.json`

`core.TimelineResponse`: for every tile at level z, `v0` (version at the start of the UTC
day) and `points` `[[t, v], ...]` (versions that became effective that day). `final` is
true once the horizon (at z < 0: min(horizon, `Pyramid.BuiltUpTo`)) passed the end of the
day. Cache: immutable when final, else short.
Static: yes.

### `GET tile/{ds}/{z}/{x}/{y}/{ver}.bin`

TSV3 for that exact version (version 0 is all uncharted). Cache: immutable. 404 for a
version above the latest. Static: yes.

### `GET tile/{ds}/{z}/{x}/{y}/live.bin`

TSV3 with the live flag; `version` is the committed version underneath. At z0: committed
content plus the newest open-window update per chunk (FORMAT.md 4, rule 6). At z < 0: the
coarse live latest, the average of the committed z0 content not yet in a coarse version
(FORMAT.md 4). If the tile has nothing pending, it equals the latest version with the live
flag set. Cache: none. Static: no.

### `GET tiledelta/{ds}/{x}/{y}/{from}-{to}.bin`

TSD3 between two versions of a z0 tile (`from < to`). Cache: immutable. Static: yes for
consecutive versions (`{v}-{v+1}`); the exporter may add longer spans.

## 3. Movers

### `GET movers/{sfc}/day/{date}.json`

`core.DayIndex`: the day's closed windows (`start`, `first`, `last`, `rows`), keyframe
times and `final`. Cache: immutable when final, else short. Static: yes.

### `GET movers/{sfc}/window/{start}.bin`

MVW3 kind 1 for the closed 5-minute window starting at `start`. Cache: immutable.
Static: yes.

### `GET movers/{sfc}/keyframe/{t}.bin`

MVW3 kind 2: the last row before `t` of every live journey (with carriages, if that row
had them). Keyframes are at UTC hour boundaries. Cache: immutable. Static: yes.

### `GET movers/{sfc}/live.bin`

MVW3 kind 3 at `now`: every row of the last 60 s plus the last row of every live journey
older than that. Cache: live (static export: rewritten every `live_ms`, `max-age=5`).
Static: yes.

Seeking to time t: fetch the day index, load the newest keyframe at or before t and the
windows from the keyframe to t. Windows close 5 minutes after their start plus the commit
grace; the open part of the last 60 s comes from `live.bin` or live events.

## 4. Overlays

### `GET overlays/{ds}/{x}/{y}.json?t0=&t1=&kinds=`

`core.OverlayResponse`: intervals (tags, recipes, rails) whose position is in z0 tile
(x, y) and which overlap `[t0, t1)`. `kinds` is a comma list of `tag`, `recipe`, `rail`
(default all). Without `t0`/`t1`, all intervals ever. `version` changes when the tile's
intervals change. Cache: short, with `ETag: "<version>"`. Static: yes, without query
parameters (the whole history of the tile; `max-age=60`).

Rail intervals (`kind: "rail"`) carry the rail prototype `name` and `dir`; the viewer
map-matches trains against them when present (PLAN.md §4.8 step 6).

## 5. Live events and ingest

### `GET live?ds=<id>&ds=<id>&sfc=<id>` (Server-Sent Events)

Subscribes to datastores (tile events) and surfaces (mover events). Cache: none.
Static: no; static viewers poll `catalog`, `timeline` and `live.bin` every `live_ms`.

| event | data (JSON) |
|---|---|
| `hello` | `{"now": ms, "horizon": {"<ds>": ms}}` sent first |
| `tile` | `{"ds", "z", "x", "y", "v", "live"?, "ls"?, "now"}`: a version or live content changed |
| `horizon` | `{"ds" or "sfc", "horizon", "now"}` after a commit |
| `movers` | `{"sfc", "now", "movers": [MoverMeta], "rows": [[entry, t, tick or null, x, y, o, flags, carriages or null]]}` new samples, in arrival order; `entry` indexes this event's `movers`; `carriages` is `[[x, y, o], ...]` |
| `catalog` | `{"now"}` the catalog changed; refetch it |

A comment line (`: ping`) is sent every 15 s. Each client has a bounded buffer (default
1 MiB); a client that falls behind is disconnected and must reconnect and resync from the
regular routes. `Last-Event-ID` is not supported.

### `POST ingest`

Body: NDJSON capture envelopes (PROTOCOL.md), `Content-Type: application/x-ndjson`,
optionally `Content-Encoding: gzip` or `zstd`. Auth: ingest token. Limits (flags, defaults):
body 32 MiB after decompression (413), line 4 MiB (the line is counted in `bad_lines`).

- Lines that are not valid envelopes are skipped and counted; valid messages in other lines
  are applied. Unknown message types are ignored (forward compatibility).
- 200 with `core.IngestResponse` (counts per kind, up to 10 error strings, highest `seq`).
- 503 `overloaded` with `Retry-After` when the open-window buffers or the ingest queue are
  over budget. Nothing from that body was applied; the forwarder retries the same body.
- Ingest is at-least-once safe: terrain updates are absolute, mover samples dedupe on
  (journey, t), and overlay events with unchanged content are no-ops.

## 6. Static export (WP12)

`timeshadedb publish --dir <out> | --s3 ...` writes the immutable files once and rewrites
the mutable ones. Static file names drop query strings; the viewer's static `DataSource`
maps calls to these paths:

| Path | Mutable | Cache-Control |
|---|---|---|
| `catalog.json` (the `catalog` route, with `"static": true`) | yes | `max-age=10` |
| `palette/{ds}/{ver}.bin` | no | immutable |
| `timeline/{ds}/{z}/{date}.json` | until final | final: immutable, else `max-age=10` |
| `tile/{ds}/{z}/{x}/{y}/{ver}.bin` | no | immutable |
| `tiledelta/{ds}/{x}/{y}/{from}-{to}.bin` | no | immutable |
| `movers/{sfc}/day/{date}.json` | until final | as timeline |
| `movers/{sfc}/window/{start}.bin`, `movers/{sfc}/keyframe/{t}.bin` | no | immutable |
| `movers/{sfc}/live.bin` | yes | `max-age=5` |
| `overlays/{ds}/{x}/{y}.json` | yes | `max-age=60` |
| `index.html`, `assets/*` (viewer bundle, content-hashed) | index only | index `max-age=300`, assets immutable |

On the live server the catalog route is `catalog` and also answers `catalog.json`. Uploads
set `Content-Encoding` (gzip by default) and `Content-Type` per object.

## 7. Metrics and health

- `GET /metrics` (outside `/api/v3/`): Prometheus text. Names are constants in
  `internal/v3/core/metrics.go` (prefix `tsdb_`), with labels `ds`, `sfc`, `route`, `code`,
  `result`, `kind`, `cache`, `reason`, `src` as documented there. Per-entity series are not
  allowed (bounded cardinality, PLAN.md §4.3 G10).
- `GET /healthz`: `200 ok` when the stores are open.
- `--pprof <addr>` (optional) serves `net/http/pprof` on a separate listener.
