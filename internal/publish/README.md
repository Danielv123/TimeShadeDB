# Static export and S3 publisher (WP12)

`timeshadedb publish` writes the static export of a v3 database: the files of the HTTP API
subset in `docs/API.md` section 6, under the same relative paths, plus the viewer. The
viewer's `StaticSource` reads them from a directory, an S3 bucket or a CDN in front of one
(decision D6). The export is incremental: immutable objects are stored once, and mutable ones
are stored again only when their bytes change.

## Commands

```powershell
# From a database that no server has open (the stores are opened in this process):
timeshadedb publish --db v3db --dir C:\www\map

# From a running server (serve, or the Clusterio sidecar), once or every minute:
timeshadedb publish --from http://127.0.0.1:8080/api/v3/ --dir C:\www\map
timeshadedb publish --from http://127.0.0.1:8080/api/v3/ --every 1m `
    --s3-endpoint https://<account>.r2.cloudflarestorage.com --s3-bucket map --state C:\tsdb\publish-state

# Look at a directory export in a browser:
timeshadedb publish-serve --dir C:\www\map --listen 127.0.0.1:8090
```

- Source: `--db <dir>` opens the stores like `serve` does and reads the API in process.
  Two processes must not open one database (there is no lock file), so use `--from <API base
  URL>` (with `--read-token` if the server has one) while a server runs. `--every` needs
  `--from`.
- Target: `--dir <out>` writes files. `--s3-endpoint`, `--s3-bucket`, `--s3-prefix`,
  `--s3-region` (default `auto`, for R2; the bucket's region for AWS) and `--s3-virtual-host`
  (default: path-style `<endpoint>/<bucket>/<key>`, which R2 and MinIO accept) upload with
  signed PUTs (SigV4, no SDK). Credentials come from `--s3-access-key-id` and
  `--s3-secret-key-file`, else `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. The bucket (or
  the CDN) must allow public reads and, if the viewer is hosted elsewhere, CORS `GET`.
- `--encoding gzip` (default), `zstd` or `identity`. Objects are stored compressed with
  `Content-Encoding` set (images and bodies under 256 bytes are stored as they are). gzip at
  `--gzip-level 6` was smaller than zstd's default level on the test data (372 MB against
  392 MB for the whole export) and every browser accepts it.
- `--no-viewer`, `--web-dir <dir>` (default: the bundle embedded in the binary; a binary built
  without one exports data only), `--no-movers`, `--no-overlays`, `--workers` (8),
  `--max-attempts` (6), `--live-ms` (the catalog's poll interval; default `--every`).
- `--state <dir>`: where the publish state lives (below). Default `<out>/.tsdb-publish` for
  `--dir`, `<db>/publish/s3-<hash of the target>` for S3 with `--db`; required for S3 with
  `--from`. `--reset` forgets it, so everything is stored again.
- Exit status: non-zero when a round failed (the queue is kept), or when objects could not be
  read or were refused. Each round prints one line: objects planned, stored (immutable), bytes,
  already stored, unchanged, missing, refused, failed, retries and the duration.

`publish-serve` serves a directory export with the headers the S3 target stores: a file whose
bytes start with the gzip or zstd magic is sent with that `Content-Encoding` (no export file
starts with those bytes otherwise), and `Content-Type` and `Cache-Control` follow the key
(timelines and day indexes always get `max-age=10`, since the file does not say whether it is
final). It does not serve dot files. Any other web server works if it does the same; with
`--encoding identity` any static server works, at about 8 times the bytes.

## What is exported

| Key | Class | Cache-Control |
|---|---|---|
| `catalog.json` (the server's catalog with `"static": true`) | mutable | `public, max-age=10` |
| `palette/{ds}/{n}.bin` (the catalog's palette version; older ones stay) | immutable | `public, max-age=31536000, immutable` |
| `timeline/{ds}/{z}/{date}.json` for every level, every day from the first to the last update, and today | immutable once final, else mutable | final: immutable, else `public, max-age=10` |
| `tile/{ds}/{z}/{x}/{y}/{v}.bin` | immutable | immutable |
| `tiledelta/{ds}/{x}/{y}/{v}-{v+1}.bin` (z0, every consecutive pair from 1) | immutable | immutable |
| `movers/{sfc}/day/{date}.json` | immutable once final, else mutable | as timelines |
| `movers/{sfc}/window/{start}.bin`, `movers/{sfc}/keyframe/{t}.bin` | immutable | immutable |
| `movers/{sfc}/live.bin` | mutable | `public, max-age=5` |
| `overlays/{ds}/{x}/{y}.json` (tiles with intervals) | mutable | `public, max-age=60` |
| `icons/{file}` (the catalog's spritesheet and metadata) | immutable | immutable |
| `assets/*` (content-hashed viewer files) | immutable | immutable |
| `index.html` (with `<meta name="tsdb-source" content="static">`) | mutable | `public, max-age=300` |

Today's timeline is written on every round, so a viewer starting on "latest" finds its first
version request (`web/src/map/boot.ts` asks for today's timeline on a static export) and it
answers every time after the newest data. Overlays: with `--db` the overlay store lists the
tiles that have intervals; with `--from` every z0 tile with terrain is asked and files without
intervals are not stored (the viewer treats a 404 as "no intervals").

`index.html`'s root-relative `/assets/` URLs are made relative. URLs inside the bundle's
scripts are not rewritten: a bundle built with Vite's default base `/` loads its worker and
images from `/assets/`, so the export works at the root of a site. For an export under a
sub-path, build the viewer with `npx vite build --base ./` and pass `--web-dir`.

## Tile versions

The live server renders any version on request; a static export has to store every file a
viewer may ask for. The viewer (WP7) resolves the version of every visible tile from the
day's timeline, then loads it as a full tile, or, when it already holds an older version of
the same z0 tile at most 4 versions back, as a chain of consecutive tile deltas.

**Default: every version of every tile as a full tile (`tile/.../{v}.bin`, version 0
included), at every level, plus every consecutive z0 delta (`{v}-{v+1}`).** This is the only
choice with which the current viewer can show any time from a cold start: scrubbing backwards,
opening a history link, or jumping more than 4 versions ahead all load full tiles. Deltas make
timelapse and small scrubs cheap (about 0.7 KB per delta against 26 KB per z0 tile).

On the imported test data (one week of a 57-instance gridworld, 22 MB database) the export is
24,668 objects and 372 MB with gzip: z0 tiles 8,898 files and 232 MB, z-1 1,502 files and
90 MB, z-2 523 files and 25 MB, coarser levels 6 MB, deltas 8,766 files and 6 MB, mover
files 10.5 MB, timelines 0.13 MB. The full export takes 13 s from `--db` on the owner's
32-core machine; an unchanged second round plans 936 objects (open timelines, overlays, live
movers, catalog) and stores 7 of them in 43 ms.

`--keyframe-every N` (N > 1) exports a z0 version as a full tile only when it is 0, 1, the
newest at the time of the round, or a multiple of N; deltas and coarse levels are unchanged.
With N = 16 the test export shrinks to 156 MB (coarse levels then dominate). **The current
viewer does not support it**: a cold load of a version without a full tile gets a 404 and
the viewer keeps drawing the older texture it has (measured with `browsercheck.mjs`: the
history step showed the latest image and logged 25 tile 404s). Using it needs a viewer change
(load the newest full version at or below the wanted one, then chain deltas up to N - 1) and
a way to tell the viewer N (for example a catalog field); until then, keep the default.

## Durable queue and state

The state directory holds:

- `target.json`: the target's id (directory path, or endpoint, bucket and prefix; never a
  secret). A state is never used for another target.
- `done.log`: append-only, one line per stored object (key, hash of the stored bytes and
  headers, size, class). It is fsynced every 256 records and at the end of each phase, and
  compacted at open when it has grown to more than twice its live records. A torn last line
  is cut off at open.
- `queue.json`: the objects of the round in progress, written before the round stores
  anything and removed when it ends.

A round:

1. finishes the queue a stopped round left (in its order), then
2. plans: reads the catalog, the timelines and the mover day indexes; a final timeline or day
   index the state records as stored is not read again (it was stored after everything it
   names, below), so later rounds only read the open days;
3. writes the queue, then stores in phases, each one only after the previous one is complete:
   immutable data; final timelines and day indexes (skipped for this round if an object of the
   round was refused or unreadable, so they are planned again); mutable data; the catalog;
   `index.html`. A viewer therefore never finds a catalog, timeline or day index naming an
   object that is not stored yet.

An immutable object the state records is neither read nor stored again. A mutable object is
read, and stored only when its hash changed (the catalog and `live.bin` change on every round,
because they carry the clock). The directory target also checks that the file still exists
with the recorded size, and writes it again if not.

Failures: network errors, 5xx and 429 are retried with backoff (500 ms doubling to 30 s) up to
`--max-attempts`; then the round stops before the catalog, and the queue is kept. 401 and 403
stop the round at once (`target refused the credentials`). Other 4xx skip the object for this
round. An object the source no longer has (404) is skipped.

After a crash, objects stored but not yet in the fsynced part of `done.log` are stored again
with the same bytes (harmless). Measured on the test data against a fake S3 endpoint: killed
after 4,205 PUTs with 4,096 records durable, the restarted publisher resumed the queue and
stored the 109 others a second time, everything else once; a further round stored only the
catalog and the six `live.bin` files.

Objects are never deleted. Publishing a different database to the same target needs
`--reset` and a CDN purge, because tile versions of two databases share names.

## Known limits

- Coarse levels on a running server lag z0 by the pyramid schedule (up to 5 minutes at z-1, an
  hour at z <= -3; WP3), and a static export has no live coarse content, so with `--from` the
  zoomed-out map can be that far behind. An offline database (`--db` after an import) is built
  to its horizon.
- The state holds one record per stored object in memory (about 100 bytes each): about 2.5 MB
  for the test week, about 130 MB for a year of the same map. Tracking the highest stored
  version per tile instead would make it O(tiles).
- `--db` opens the stores for writing (WAL replay, torn-tail repair). Do not point it at a
  database a server has open.

## Files

| File | Role |
|---|---|
| `source.go` | `Source`: `HandlerSource` (API handler in process), `HTTPSource` (running server) |
| `plan.go` | The round's objects from the catalog, timelines and day indexes; tile-version policy; catalog and `index.html` rewrites |
| `publisher.go` | Rounds, phases, workers, retries, change detection |
| `state.go` | `done.log`, `queue.json`, `target.json` |
| `target.go`, `s3.go`, `sigv4.go` | `DirTarget`, `S3Target`, SigV4 (ported from illisis-minimap `s3/sigv4.ts` and `s3/publisher.ts`, Blue Oak Model License 1.0.0) |
| `serve.go` | The `publish-serve` handler |
| `browsercheck.mjs` | Headless check of an export in Chromium (below) |

## Tests

`go test ./internal/publish ./cmd/timeshadedb -run 'Publish|SigV4|S3|State|Keyframe|Dir|Handler|Static|Valid'`:
SigV4 against AWS's published PUT and GET examples; a fake S3 endpoint that verifies every
signature; change detection over rounds (unchanged, new version, day becoming final); a queue
resumed after a failing endpoint and after a cancelled round; auth and refused objects; final
indexes deferred while an object is missing; the keyframe policy; the directory target and
`publish-serve`; a torn state log; and, in `cmd/timeshadedb`, an imported database exported
to a directory (every version a timeline names is there), to a fake S3 endpoint, and from a
running `serve`.

Browser check (needs a built viewer, Chromium and `playwright-core`):

```sh
timeshadedb publish --db v3db --dir out
timeshadedb publish-serve --dir out --listen 127.0.0.1:0
node internal/publish/browsercheck.mjs --pw <dir>/node_modules/playwright-core --chrome <chrome.exe> --url http://127.0.0.1:<port>/
```

It waits for the first full paint, checks that terrain is drawn, zooms to z0, scrubs to the
middle of the history (cold historical versions), plays a timelapse (deltas), zooms out, and
fails on any request to `/api/v3/` or any failed tile, delta, palette, catalog or viewer
request. On the test-data export (WebGL2, headless Chromium 1223): first full paint 190-197 ms
on loopback, all checks passed, 48 tile and 24 delta requests, no failed request but one
overlay 404 for a tile without intervals. With `--renderer canvas2d` the viewer's worker
loader resolves the relative static base `./` against the worker script (`/assets/`) and every
tile request 404s; with an absolute base (`?data=http://host/`) all checks pass. That is a
viewer bug (WP7), not an export problem.
