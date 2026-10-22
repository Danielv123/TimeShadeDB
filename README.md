<picture>
  <source media="(prefers-color-scheme: dark)" srcset="./web/images/logo_dark_transparent.png">
  <img alt="TimeShadeDB" src="./web/images/logo_light_transparent.png">
</picture>

# TimeShadeDB

TimeShadeDB stores timestamped canvas history and serves an embedded web interface for querying and replaying the canvas over time.

## Build

```powershell
powershell.exe -ExecutionPolicy Bypass -File scripts\build.ps1
```

The build script rebuilds the web bundle, compiles the Go binary with the current embedded assets, and writes a package archive under `build/packages/`.

## Datastore upgrades

`manifest.json` contains a datastore-wide `datastore_version`, separate from the
storage-family `version` and the versions embedded in binary files. Writable
opens automatically run every registered `N -> N+1` migration. Read-only opens
reject outdated datastores, and newer versions are always rejected.

Migrations copy the active datastore, validate the copy, and then atomically
replace a small root manifest that selects the new generation. The source stays
active if any step fails. Upgrades require space for another full copy and all
other writers must be stopped. Previous and interrupted generations remain
unselected for manual cleanup.

## Moving a v2 chunk database to v3

`timeshadedb migrate-v3` copies every datastore of a v2 chunk database into a v3
terrain store (see `docs/v3/`), then compares both pixel by pixel at random ticks:

```powershell
timeshadedb migrate-v3 --from factorio.tshd --db v3db
timeshadedb migrate-v3 --from factorio.tshd --dry-run --verify 20
```

v2 stores game ticks and v3 stores map-clock milliseconds. `--epoch auto` (the
default) puts each savefile's latest tick at the time its `progress.json` was last
written; pass `--epoch` as RFC 3339 or Unix milliseconds to choose it, and `--ups`
if the game did not run at 60 UPS. The savefile UUID becomes the v3 map name unless
`--map` is given. v3 keeps the net change per chunk and terrain window (`--window`,
default 60s), so states that lasted less than a window are not carried over; at every
window boundary the copy equals the v2 database. The source is opened read-only.
`--dry-run` migrates into memory and writes nothing. `--db` writes the v3 terrain
store and then builds its coarse zoom levels (the pyramid).

## Importing an illisis/minimap export

`timeshadedb import-minimap` reads an illisis/minimap S3 export (the directory that
holds `maps.json.gz`) and writes terrain, movers, tags and recipes into a v3 database,
then builds the pyramid and stores the map metadata in `maps.json`:

```powershell
timeshadedb import-minimap --db v3db --verify D:\minimap-export
```

Without `--db` it imports into in-memory stores, which only checks the export.
`--verify` compares the imported state with the export and reads the movers and
overlays back. An import cannot resume or add to a database: `--db` must be empty
(or not exist yet), and the command fails if a store dropped any update as late.
The catalog icons of the export are copied into `v3db/icons`.

## Serving a v3 database

`timeshadedb serve-v3` serves the v3 HTTP API (`docs/v3/API.md`), live events, ingest
and the embedded viewer (build it first with `npm --prefix web run build`):

```powershell
timeshadedb serve-v3 --db v3db --listen 127.0.0.1:8080
```

It listens on loopback by default; any other address needs `--ingest-token`. A standalone
game with the `timeshade_capture` mod is forwarded with
`timeshadedb tail --dir <factorio>/script-output/timeshade --map <key> --url http://127.0.0.1:8080`.
Ingest is not throttled by memory: open windows past their share of `--mem-budget` are
spilled to disk. A backlog (a recorded capture posted with `tail --once`, or the lines a
forwarder kept while it, the network or the server was down) keeps the times it was
captured at and is stored in full, as long as each source sends its data in time order:
the server holds a map's horizon behind the data of a source that is still catching up
(docs/v3/API.md section 5). A forwarder may stay away for up to `--source-hold` (default
1 hour) while the server keeps running; past that its source no longer holds the map and
the rest of its backlog is late. A server that is down holds nothing back, so the length
of its downtime is not limited.

## Publishing a static map

`timeshadedb publish` exports a v3 database as static files (the API subset of
`docs/v3/API.md` section 6 plus the viewer) to a directory or an S3/R2 bucket, storing each
immutable file once and mutable files only when they change:

```powershell
timeshadedb publish --db v3db --dir C:\www\map
timeshadedb publish --from http://127.0.0.1:8080/api/v3/ --every 1m --s3-endpoint https://<account>.r2.cloudflarestorage.com --s3-bucket map --state publish-state
```

See `internal/v3/publish/README.md` for the flags, the tile versions it exports and the
durable upload queue.

## Factorio export

The `timeshadedb_exporter` mod writes charted chunks to
`script-output/timeshadedb/chunk-charted.tsv`. Send them to `serve` with:

```powershell
timeshadedb tail-chunk-tsv --input chunk-charted.tsv --savefile SAVE_UUID --base-url http://127.0.0.1:8080
```

## Factorio mod deployment

GitHub Actions publishes the Factorio mod to the Factorio Mod Portal when a tag
named `factorio-v<version>` is pushed, where `<version>` matches
`timeshadedb_exporter/info.json`.

Configure the repository secret `FACTORIO_MOD_PORTAL_API_KEY` with a Factorio
API key that has `ModPortal: Publish Mods` permission. The deploy workflow can
also be run manually from GitHub Actions.
