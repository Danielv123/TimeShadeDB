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
  (the live server sends it on every `/api/v3/` response) and as the `now` field of live
  events. Viewers keep a data clock from it (never
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
| 405 | `bad_request` | wrong method for the route |
| 408 | `timeout` | ingest body not received within the time limit |
| 413 | `too_large` | ingest body over the cap |
| 503 | `overloaded` | ingest bodies in flight over their budget, too many live clients; `Retry-After` seconds given (the stores never refuse ingest for memory: they spill to disk, FORMAT.md 5.5, 5.7) |
| 500 | `internal` | anything else |

**Hostile requests** (PLAN.md §4.3 G2): parameter validation, the grid bound check and the
datastore lookup happen before any per-tile allocation. A request for an invalid or unknown
tile allocates nothing that outlives it and answers in under 1 ms.

### 1.3 Authentication

- `--read-token`: when set, every GET under `/api/v3/` and `/metrics` needs
  `Authorization: Bearer <read token>` or `?token=<read token>` (for EventSource and
  `<img>` icon loads, which cannot set headers). When unset, reads are open. The viewer
  files (`/`, `/assets/*`) and `/healthz` are always open.
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

### `GET tile/{ds}/{z}/{x}/{y}/{ver}.png`

PNG of that version for external use (paletted at z0 when possible; uncharted pixels
transparent). Cache: immutable. Static: no.

### `GET icons/{file}`

The catalog icon files (`catalog.icons.png` and `.json` are `icons/<file>` relative to the
API base). The live server serves exactly the two files named in `<db>/icons/icons.json`
(FORMAT.md 5); their names are content-hashed. Cache: immutable. Static: yes.

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
had them; a moving train's rail row with the pieces under the train in front of its own,
FORMAT.md 7.4). Keyframes are at UTC hour boundaries. Cache: immutable. Static: yes.

### `GET movers/{sfc}/live.bin`

MVW3 kind 3 at `now`: every row of the last 60 s plus the last row of every live journey
older than that. Each journey's first row carries a moving train's body like a keyframe row
(FORMAT.md 7.4), so the payload alone places every carriage; later rows and `movers` events
are as stored. Cache: live (static export: rewritten every `live_ms`, `max-age=5`).
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

Subscribes to datastores (tile events) and surfaces (mover events). GET only (other
methods, HEAD included, get 405). Cache: none.
Static: no; static viewers poll `catalog`, `timeline` and `live.bin` every `live_ms`.

| event | data (JSON) |
|---|---|
| `hello` | `{"now": ms, "horizon": {"<ds or sfc id>": ms}}` sent first: the horizon of every subscribed datastore and surface that exists |
| `tile` | `{"ds", "z", "x", "y", "v", "live"?, "ls"?, "now"}`: a version or live content changed |
| `horizon` | `{"ds" or "sfc", "horizon", "now"}` after a commit |
| `movers` | `{"sfc", "now", "movers": [MoverMeta], "rows": [[entry, t, tick or null, x, y, o, flags, carriages or null]]}` new samples, in arrival order; `entry` indexes this event's `movers`; `carriages` is `[[x, y, o], ...]`; a rail row (flag bit 7, FORMAT.md 7.4) has three more elements, `speed, rail pieces as [[x, y, direction, kind], ...] or null, rail start`. Rows the store counted as invalid, or as late by the surface horizon of the last commit, are left out; a row the store rejected under another bound (its clock skew limits) can still appear |
| `catalog` | `{"now"}` the catalog changed; refetch it |

A comment line (`: ping`) is sent every 15 s. Each client has a bounded buffer (default
1 MiB); a client that falls behind is disconnected and must reconnect and resync from the
regular routes. `Last-Event-ID` is not supported. The number of clients is bounded
(default 64); beyond it the route answers 503 `overloaded`. Tile events for open-window
changes are coalesced and sent about once a second (the commit loop period); version
changes at every level are sent after the commit that made them. A client connected
before the server's first commit step after start is not sent the existing tiles.

### `POST ingest`

Body: NDJSON capture envelopes (PROTOCOL.md), `Content-Type: application/x-ndjson`,
optionally `Content-Encoding: gzip` or `zstd`. Auth: ingest token. Limits (flags, defaults):
body 32 MiB after decompression (413), line 4 MiB (the line is counted in `bad_lines`),
message 256 KiB and move message 4,096 entries (the message is counted as a bad message).

- Lines that are not valid envelopes are skipped and counted; valid messages in other lines
  are applied. Unknown message types are ignored (forward compatibility).
- 200 with `core.IngestResponse` (counts per kind, up to 10 error strings, highest `seq`).
- The body is decoded as a stream and applied in batches (at most 2,048 chunk updates or
  4 MiB of chart text, 12,288 mover samples and 16,384 overlay events each), in order.
  Messages are decoded a window at a time (2,048 messages or 256 KiB), however many a
  line packs, so memory per request is bounded whatever the body size or shape.
- The raw body is received in full before any of it is applied, under a time limit
  (60 s; 408 `timeout` after it) and a budget for the bodies being received at once
  (64 MiB; beyond it 503 `overloaded`), so a client that stalls mid-body never delays
  other ingest requests.
- The stores never refuse a batch for memory: past their share of `--mem-budget` they move
  whole open windows to disk (FORMAT.md 5.5, 5.7), so ingest runs as fast as CPU and disk
  allow. A store failure (a disk write error) answers 500: the batch may be partly applied
  (its chunk updates and overlay events can be stored before the mover store fails), the
  rest of the body was not, and earlier batches of the same body were. The forwarder
  retries the same body, which is safe (below). A 413 for a body that exceeds the cap only
  after decompression is handled by splitting.
- 415 for a `Content-Encoding` other than identity, gzip or zstd.
- A message time more than 10 s after the server clock (a broken game clock or anchor) is
  clamped to the clock and counted (`tsdb_ingest_rejected_total{reason="clamped"}`). A chunk
  update or mover sample before 2016 is impossible and counted as invalid; any time between
  is accepted while it is not behind its horizon, however old (a backlog). On
  `--clock data` the bound is the envelope's own `recv_ms` (at most the wall clock plus
  10 s), so a gap in replayed data does not move times.
- Retries are safe: an envelope whose `seq` and `recv_ms` are both at or below the last
  applied envelope of its `src` is skipped (`duplicates` in the response,
  `tsdb_ingest_rejected_total{reason="duplicate"}`), so it runs neither the journey rules
  nor the clock anchor again; the journey rules of a batch the mover store refused are
  rolled back; an envelope seen again but not applied is timed on its anchor's line (a
  replay, PROTOCOL.md 4); terrain updates are absolute, mover samples dedupe on
  (journey, t), and overlay events with unchanged content are no-ops.
- Envelopes stamped with their source's previous `recv_ms` although their tick is more
  than 2 s later are counted (`one_recv` in the response,
  `tsdb_ingest_rejected_total{reason="one_recv"}`) and logged: the forwarder lost the
  time (PROTOCOL.md 1), and their samples collide at one instant, where the mover store
  keeps one per journey (`tsdb_mover_store_rejected_total{reason="duplicate"}`).

### Horizon

The commit loop closes windows `--terrain-grace` and `--mover-grace` behind the clock,
and data older than a datastore's or surface's horizon is late (FORMAT.md 4). So that a
backlog is not late, each map's horizon is also held behind the data of the sources that
feed it (`ingest.Sources`, FORMAT.md 5.8): per source (`src`) and map the server keeps the
newest time of a chunk update or mover sample it applied (the data with a horizon; overlay,
map and trace messages hold nothing), and closes no window of the map past the lowest of those,
minus the grace, over the sources that had data (or a `caught_up` envelope) for the map
within `--source-hold` (default 1 hour). A source's first envelope holds its map at once.
A live source's newest time trails the clock by a few seconds, so live data is committed
as before (the committed horizon trails it by those seconds more than with the clock
alone; viewers see the open windows as live meanwhile). A source that is behind
(a forwarder catching up after it, the network or the server was down) holds the map at
its own data, which it sends in time order, so none of it is late. A `caught_up` envelope
(PROTOCOL.md 1) raises the source's time to its `recv_ms` (a forwarder that is idle sends
one every few seconds, so its source holds nothing back), and one marked `ended` (the game
stopped) releases the source's maps until it sends again. A source that goes quiet
without one holds its maps until `--source-hold` after it last had data for them; every
source in `sources.json` counts as heard from when the server starts, so after a restart
each map waits up to `--source-hold` for its sources' backlogs. A map with data that no
source in `sources.json` feeds (a database from before that file, or one whose file was
lost) is held at its newest data from the start in the same way, until a source sends data
or `caught_up` for it (its own hold then takes over) or `--source-hold` passes, so a
forwarder that reconnects some time after the restart still finds its backlog ahead of
the horizon. Data a source sends later than that, or out of order by more than the grace,
can still be late (counted as `late`): `--source-hold` is how long a forwarder may stay
away while the server runs (a server that is down holds nothing back, so its downtime has
no limit); a longer one costs only how late history closes for a source that stopped
without saying so. Closed windows are never rewritten. While a map is held, its windows
stay open (spilled to disk when memory runs short) and history in the held period is
served as live; the server logs when it holds a map more than a minute behind the clock
and when it releases it (`tsdb_horizon_held_maps`, `tsdb_horizon_hold_lag_ms`).
`--source-hold 0` turns the holds off.

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

What the exporter writes (`internal/v3/publish/README.md` has the details and sizes):

- Tile versions: every version of every tile at every level as a full tile (version 0
  included), plus `tiledelta` for every consecutive z0 pair from version 1. The viewer can
  then show any time from a cold start. `--keyframe-every N` stores fewer full z0 tiles,
  but the viewer does not support that yet.
- Timelines: every level, every UTC day from the datastore's first to its last update, and
  today's (the static viewer asks for today's timeline for "latest").
- Palette: the catalog's `palette_version`; versions exported earlier are kept.
- Overlays: only tiles with intervals (a 404 means none).
- Final timelines and day indexes are stored after the tiles, deltas, windows and keyframes
  they name, and the catalog after everything else, so a reader never finds a name whose
  file is not there yet. Nothing is deleted.
- `timeshadedb publish-serve --dir <out>` serves a directory export with these headers
  (`Content-Encoding` taken from the stored bytes).

## 7. Metrics and health

- `GET /metrics` (outside `/api/v3/`): Prometheus text. Names are constants in
  `internal/v3/core/metrics.go` (prefix `tsdb_`), with labels `ds`, `sfc`, `route`, `code`,
  `result`, `kind`, `cache`, `reason`, `src` as documented there. Per-entity series are not
  allowed (bounded cardinality, PLAN.md §4.3 G10).
- Besides the names in `metrics.go`, the live server exports
  `tsdb_ingest_rejected_total` with the reasons `bad_line`, `bad_message`, `overloaded`,
  `clamped`, `owner`, `unknown_gone`, `quarantined`, `duplicate` (retried envelopes
  skipped) and `one_recv` (section 5), the cache gauges and counters with
  `cache="terrain"`, `"pyramid"`, `"decoded"` and `"encoded"`, `tsdb_mover_open_rows`,
  `tsdb_mover_open_bytes`, `tsdb_spilled_windows{store}` and `tsdb_spills_total{store}`
  (`store` is `terrain` or `movers`), `tsdb_mover_store_rejected_total{reason}` (`ahead`,
  `behind`: before 2016, `duplicate`: rows a commit removed for an equal (journey, t),
  `corrupt`, `wal_skipped`), `tsdb_ingest_caught_up_total`, `tsdb_horizon_held_maps`,
  `tsdb_horizon_hold_lag_ms`, `tsdb_ingest_resolved_gones_total` (gones of journeys the
  journey rules had forgotten, after a restart, completed from the mover store's last row),
  `tsdb_mover_live_evicted_total` (live journeys forgotten past the store's live-journey
  bound), `tsdb_overlay_open_intervals`,
  `tsdb_overlay_torn_bytes_total` and the Go runtime gauges `go_memstats_heap_inuse_bytes`,
  `go_memstats_heap_alloc_bytes`, `go_memstats_sys_bytes` and `go_goroutines`.
- `GET /healthz`: `200 ok` when the stores are open.
- `--pprof <addr>` (optional) serves `net/http/pprof` on a separate listener.

## 8. Running the server

`timeshadedb serve-v3 --db <dir>` opens (or creates) the database and serves this API, the
live events, ingest, `/metrics`, `/healthz` and the embedded viewer at `/`. It prints
`listening on <addr>` as its first stdout line. Flags beyond the shared ones (README.md):

- `--commit-every` (commit loop period, default 1 s);
- `--terrain-grace` (10 s) and `--mover-grace` (15 s, at least the handoff window): how
  far behind the clock windows close;
- `--source-hold` (1 h): how long a quiet source holds its maps' horizon at its newest
  data (section 5, "Horizon"; 0 turns it off);
- `--web-dir <dir>` (serve a viewer build from disk) or `--no-viewer`;
- `--pprof <addr>`, `--slow-log <duration>`, `--quiet`;
- `--salvage-overlays`: open an overlay log damaged before its end, keeping it aside;
- `--exit-on-stdin-close`: stop when the parent process goes away (the Clusterio sidecar).
- `--clock data`: the map clock is the newest ingested `recv_ms` (seeded from the newest
  data in the database) instead of the wall clock, for replaying recorded captures (a
  backlog, a benchmark stream). The graces then default to 2 minutes, because replayed
  bodies, often posted several at once, arrive out of order. One envelope moves the
  clock at most 1 hour forward and never past the wall clock plus 10 s (such envelopes
  count as `clamped`), so one bad `recv_ms` cannot push the map clock ahead for good;
  after a longer gap in the data the clock catches up over a few envelopes. Message times
  are clamped against their envelope's own `recv_ms`, so the data after a gap keeps its
  times.

The process sets a soft memory limit of `--mem-budget` (Go's `GOMEMLIMIT`, which takes
precedence when set), so garbage collection works harder before the process
grows past its budget and RSS stays within the budget plus 64 MiB (PLAN.md §5). The
caches get about half of the budget (terrain 3/32 decoded tiles, 1/8 open windows and
3/64 indexes, encoded tiles 1/8, coarse images 1/16, overlays 1/32; the mover open
windows another eighth, counted per row); the rest is working memory
and the collector's headroom. Open windows past their share move to disk (FORMAT.md 5.5,
5.7) and are read back by the commit that closes them, so memory stays within the budget
however fast data arrives or however long a horizon is held.
After 10 s without requests the server returns freed memory to the operating system.

Interrupt or SIGTERM shuts down cleanly: live streams are ended at once, requests in
flight get up to 5 s (then their connections are closed), ingest requests in flight are
waited for, the pyramid is flushed at the horizon and every store is closed. The
`--slow-log` line names the request path without its query (which may hold the read
token). Coarse levels load lazily (in parallel, in the background), so the server
listens within tens of milliseconds whatever the history length; a read of a datastore
that is not loaded yet waits only for that datastore.

The commit loop commits terrain at `now - terrain-grace`, runs the pyramid build, and
commits movers at `now - mover-grace`, each map at most up to the newest data of the
sources holding it minus the grace (section 5, "Horizon"). Updates older than the
horizon are late (FORMAT.md 4); with the holds that only happens to data a source sends
out of order by more than the grace, or after it was quiet for longer than
`--source-hold`.

`timeshadedb tail --dir <script-output>/timeshade --map <key> [--url ...] [--token ...]
[--src standalone:<name>]` forwards a standalone game's files (PROTOCOL.md 2). It adds
`src`, `map`, `instance`, `recv_ms` and an increasing `seq` to every line, posts gzip
bodies of up to `--batch-bytes` (1 MiB), deletes a file once all of it was acknowledged
and a newer file exists, and posts the newest file up to its last complete line without
deleting it. `recv_ms` is the time the game wrote the line, from the file (PROTOCOL.md 1:
the file's modification time for its last line, counted back by ticks for the lines
before it), so a backlog (a tailer that was stopped, a server that was down, a recorded
capture posted with `--once`) keeps its times and is posted as fast as the server takes
it. Once it has posted everything it sends a `caught_up` envelope, and again every
`--heartbeat` (5 s) while nothing new arrives, and one when it stops (at the end of
`--once`, or on interrupt), marked `ended` when the newest file has not changed for 10 s
(the game is not running). So `--once` on the files of a game that is still running
leaves the map held at its last line for `--source-hold`: the next run (or a tailer that
follows the game) continues from there without its lines being late. Network
errors, 5xx, 429 and 408 are retried with backoff (honouring `Retry-After`); a 400 or 413
splits the batch, and a single line that is still refused is skipped and logged; any other
status (401, 403, 404, 415) stops the tailer with an error. The acknowledged offsets are
kept in a state file (`--state`, default `<dir>/timeshadedb-tail.state.json`, `-` for
none) with a hash of each file's first KiB, so a restarted tailer does not post lines
again (the server would skip them as duplicates only while it still knows the source's
last `seq`). `--once` forwards what is there and exits.
