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
`--dry-run` migrates into memory and writes nothing. Writing with `--db` needs a
binary that includes the v3 terrain engine.

## Factorio export

The `timeshadedb_exporter` mod writes Factorio export files under
`script-output/timeshadedb/`:

```text
chunk-charted.tsv
entity-positions.tsv
```

Import entity movement into VictoriaMetrics with:

```powershell
timeshadedb tail-entity-tsv --input entity-positions.tsv --savefile SAVE_UUID --victoriametrics-url http://127.0.0.1:8428
```

The chunk tailer can watch both files together:

```powershell
timeshadedb tail-chunk-tsv --input chunk-charted.tsv --entity-input entity-positions.tsv --savefile SAVE_UUID --base-url http://127.0.0.1:8080 --victoriametrics-url http://127.0.0.1:8428
```

Entity coordinates are stored as `timeshadedb_entity_x` and
`timeshadedb_entity_y` series labelled by savefile, surface, force, entity type,
entity id, and path segment. No tile label is written.

## Factorio mod deployment

GitHub Actions publishes the Factorio mod to the Factorio Mod Portal when a tag
named `factorio-v<version>` is pushed, where `<version>` matches
`timeshadedb_exporter/info.json`.

Configure the repository secret `FACTORIO_MOD_PORTAL_API_KEY` with a Factorio
API key that has `ModPortal: Publish Mods` permission. The deploy workflow can
also be run manually from GitHub Actions.
