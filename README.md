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
