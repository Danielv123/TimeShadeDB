# TimeShadeDB v3: contracts and layout

TimeShadeDB v3 is the cluster map for Factorio and Clusterio: one Go binary that stores
chart history, trains, vehicles, players and overlays, serves them over HTTP, and embeds a
WebGL viewer. These documents are the contract every v3 work package (WP) codes against.
The plan behind them is `research/PLAN.md` in the v3 work notes (sections 4 to 6).

| Document | Contents |
|---|---|
| [FORMAT.md](FORMAT.md) | Grid, colours, terrain semantics, the on-disk layout and the wire formats (TSV3, TSD3, TSP3, MVW3) |
| [API.md](API.md) | The HTTP API under `/api/v3/`, cache rules, live events, ingest, metrics, and what the static export serves |
| [PROTOCOL.md](PROTOCOL.md) | Capture envelopes from the game to the server, transports, clock anchors, and how messages map to stored data |

Code contracts:

- `internal/v3/core`: shared types and the store interfaces (`TerrainStore`, `TileSource`,
  `MoverStore`, `OverlayStore`, `Series`, `SeriesStore`), API JSON shapes (`apitypes.go`),
  capture protocol types (`protocol.go`) and metric names (`metrics.go`).
- `internal/v3/core/memstore`: in-memory reference implementations, used as test oracles.
- `internal/v3/core/coretest`: contract tests. Every store implementation runs them
  (`coretest.TerrainStore`, `coretest.TerrainDifferential`, `coretest.MoverStore`,
  `coretest.OverlayStore`, `coretest.SeriesStore`, `coretest.PyramidLatest`).
- `internal/v3/core/wire`: reference encoders and decoders for the wire formats.
- `web/src/api/types.ts`: TypeScript mirror of the API JSON shapes, the `DataSource`
  interface and the perf hook contract. `web/src/api/wire.ts` decodes the wire formats.
- `web/src/map/layer.ts`: the `MapLayer` interface every drawn layer implements.
- `web/src/layers/index.ts`: the list of layers the viewer draws.
- `web/fixtures/`: generated example files (tiles, a delta, a palette, mover windows,
  catalog, view, timeline, overlays, capture envelopes) plus `expected.json` with the
  decoded values. Regenerate with
  `go test ./internal/v3/core/wire -run TestFixtures -update`; without `-update` the Go test
  fails if the committed files drift. `npm --prefix web test` decodes them in TypeScript.

## Directory ownership

Each WP works in its own branch and worktree off `v3` and only edits the paths it owns.
"Reads" means it codes against a contract and does not edit it. If a contract is wrong or
incomplete, the WP makes the smallest additive change and lists it in its report.

| WP | Owns | Notes |
|---|---|---|
| WP0 contracts | `docs/v3/`, `internal/v3/core/**`, `cmd/timeshadedb/main.go`, `cmd/timeshadedb/commands.go`, `cmd/timeshadedb/cmd_legacy.go`, `go.mod`, `web/src/api/`, `web/src/map/layer.ts`, `web/src/perf.ts`, `web/fixtures/`, `web/dist/.keep`, `web_embed.go`, `web_placeholder/`, `web/vite.config.ts`, `web/tsconfig.json` (test exclusion), `.gitattributes`, the web test step in `.github/workflows/ci.yml` | Only WP0 touches `go.mod` and `main.go`. Later WPs add dependencies in their own branch; `go.sum` conflicts are resolved at merge. |
| WP1 v2 stopgaps and migrator | `chunk.go`, `storage.go`, `cmd/timeshadedb/serve.go` (v2 only), `internal/v3/migrate/`, `cmd/timeshadedb/cmd_migrate.go` | |
| WP2 terrain engine | `internal/v3/series/`, `internal/v3/terrain/` | Implements `core.SeriesStore`, `core.Series`, `core.TerrainStore`. |
| WP3 pyramid and tile encoding | `internal/v3/pyramid/`, `internal/v3/tileenc/` | Implements `core.TileSource` for z <= 0 on top of a `TerrainStore` and a `SeriesStore`; TSV3/TSD3 encoding and the encoded-response cache. |
| WP4 movers, overlays, rails | `internal/v3/movers/`, `internal/v3/overlays/` | Implements `core.MoverStore`, `core.OverlayStore`. |
| WP5 illisis importer | `internal/import/minimap/`, `cmd/timeshadedb/cmd_import_minimap.go` | Produces `core.ChunkUpdate`, `core.MoverSample`, `core.OverlayEvent`. |
| WP6 HTTP API, ingest, auth | `internal/v3/api/`, `internal/v3/ingest/`, `cmd/timeshadedb/cmd_serve_v3.go`, `cmd/timeshadedb/cmd_tail.go` | Implements API.md and PROTOCOL.md decoding. |
| WP7 viewer terrain engine | `web/src/map/*` except `layer.ts`, `web/src/App.tsx`, `web/src/main.tsx`, `web/src/styles.css` | Renderer, camera, scheduler, workers, caches, server and static `DataSource`s. Only WP7 touches `App.tsx`. |
| WP8 viewer movers and overlays | `web/src/movers/`, `web/src/overlays/`, `web/src/layers/index.ts` | Only WP8 touches the layer list. |
| WP9 capture | `capture/` (`lua/`, `mod/timeshade_capture/`, `clusterio_module/`, `test/`, `build.mjs`, `README.md`, the Blue Oak licence text), `timeshadedb_exporter/README.md` (deprecation note) | Writes PROTOCOL.md envelopes. `node capture/build.mjs` assembles the mod and the Clusterio module into `capture/dist/` (WP10 copies the module). |
| WP10 Clusterio plugin | `clusterio/` | Forwarder, sidecar, auth proxy, web page. |
| WP11 benchmarks and acceptance | `bench/` (`bench/cmd/{ingestbench,soak,tilebench,attack}/`, `bench/web/`, `bench/trains/`, `bench/run_acceptance.{ps1,sh}`, `bench/REPORT.md`) | Reads `window.__tsdbPerf` and `/metrics`. |
| WP12 static export and S3 | `internal/v3/publish/`, `cmd/timeshadedb/cmd_publish.go` | Writes the API.md static subset. |

`web/package.json` and `web/package-lock.json` are shared: WP7 and WP8 may add
dependencies; merge conflicts there are resolved at merge time. WP0 added only the `test`
script.

## Commands

`cmd/timeshadedb/commands.go` holds a registry. Each command lives in `cmd_<name>.go` and
registers itself:

```go
func init() {
	register(Command{Name: "publish", Summary: "export the v3 database as static files", Group: GroupV3, Run: cmdPublish})
}
```

`Run` receives the arguments after the command name and parses them with its own
`flag.FlagSet`. `timeshadedb help` lists every command. Planned v3 commands:

| Command | File | WP |
|---|---|---|
| `serve-v3` (becomes `serve` when v2 is removed) | `cmd_serve_v3.go` | WP6 |
| `tail` | `cmd_tail.go` | WP6 |
| `import-minimap` | `cmd_import_minimap.go` | WP5 |
| `migrate-v3` | `cmd_migrate.go` | WP1 |
| `publish` | `cmd_publish.go` | WP12 |

The legacy commands (`serve`, `import-csv`, `tail-chunk-tsv`, ...) stay in
`cmd_legacy.go` until v3 replaces them (decision D11).

## Server options shared by v3 commands

These flag names are reserved so every command spells them the same way:

| Flag | Meaning |
|---|---|
| `--db <dir>` | database root (FORMAT.md "On-disk layout") |
| `--listen <host:port>` | listen address; default `127.0.0.1:8080`; port 0 picks a free port and prints `listening on <addr>` as the first stdout line |
| `--ingest-token <t>` / `--read-token <t>` | bearer tokens (API.md "Authentication") |
| `--mem-budget <bytes>` | total cache and buffer budget, default 256 MiB (PLAN.md §4.3) |
| `--window <duration>` | terrain window for new datastores, default 60s |

## Perf hooks

The viewer exposes `window.__tsdbPerf` (type `TsdbPerf` in `web/src/api/types.ts`,
helpers in `web/src/perf.ts`). WP7 and WP8 record into it; WP11's Playwright suite reads
it. Event names are fixed:

| Event | Recorded when | detail |
|---|---|---|
| `tile.request` | a tile, delta or palette fetch starts | `{key, z, prio}` |
| `tile.loaded` | its body is decoded and ready to upload | `{key, bytes, ms}` |
| `tile.drawn` | its texture is first drawn with its own data | `{key}` |
| `tile.abort` | a request is cancelled because the tile left the view | `{key}` |
| `view.fallback` | every visible tile shows at least an ancestor or descendant | `{}` |
| `view.full` | every visible tile at the target level shows its own data | `{tiles}` |
| `time.change` | the displayed time changes (scrub, play, live step) | `{t}` |
| `timelapse.frame` | a timelapse frame is drawn | `{t, ms}` |
| `movers.frame` | the mover layer drew a frame | `{movers, ms}` |
| `longtask` | a main-thread task over 50 ms was observed | `{ms}` |

`firstFullPaint` is set once, at the first `view.full`; `lastFullPaint` and
`lastFallbackPaint` at the latest. Times are `performance.now()` milliseconds.

The viewer also exposes `window.__tsdbControl` (type `TsdbControl` in
`web/src/api/types.ts`): `ready()`, `setView(x, y, s)`, `view()`, `datastore()`,
`seek(t)`, `play(speed)`, `time()`, `range()` and `surfaceBounds()`. WP11 drives zoom-out,
scrub and timelapse through it, and checks `view()` and `datastore()` before measuring;
without it only the first-paint numbers can be measured. The suite opens the viewer with
the query parameters `ds`, `x`, `y`, `z` and `t` (datastore id, centre in world tiles,
level, map-clock time). WP7 implements `window.__tsdbControl` and these parameters.
