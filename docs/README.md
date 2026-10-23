# TimeShadeDB internals

This is the map of the repository for people working on it. The [README](../README.md) covers using it.

| Document | Contents |
|---|---|
| [FORMAT.md](FORMAT.md) | Grid, colours, terrain semantics, the on-disk layout and the wire formats (TSV3, TSD3, TSP3, MVW3) |
| [API.md](API.md) | The HTTP API under `/api/v3/`, cache rules, live events, ingest, metrics, and what the static export serves |
| [PROTOCOL.md](PROTOCOL.md) | Capture envelopes from the game to the server, transports, clock anchors, and how messages map to stored data |
| [RAIL-GEOMETRY.md](RAIL-GEOMETRY.md) | The rail piece model the capture, the server and the viewer share to place trains |

## Layout

| Path | Contents |
|---|---|
| `cmd/timeshadedb/` | The binary: one `cmd_<name>.go` per command, `stores.go` opens the file-backed stores for offline commands |
| `internal/core/` | Shared types, store interfaces, API JSON shapes, capture protocol types and metric names |
| `internal/terrain/`, `internal/series/` | The z0 terrain store: the net change per tile and window, keyframes and an append-only index per tile |
| `internal/pyramid/`, `internal/tileenc/` | Coarse zoom levels and the tile encodings with their response cache |
| `internal/movers/`, `internal/overlays/` | Trains, vehicles and players, and chart tags, recipes and rails |
| `internal/ingest/`, `internal/api/` | Capture ingest and the HTTP API |
| `internal/publish/` | Static export to a directory or S3/R2 |
| `internal/import/minimap/` | The illisis/minimap importer |
| `web/` | The viewer (TypeScript, WebGL2), embedded into the binary by `web_embed.go` |
| `capture/` | The capture Lua, the standalone mod and the Clusterio save-patch module |
| `clusterio/` | The Clusterio plugin |
| `bench/` | Benchmarks and the acceptance suite |

## Code contracts

- `internal/core`: the store interfaces (`TerrainStore`, `TileSource`, `MoverStore`, `OverlayStore`, `Series`, `SeriesStore`), API JSON shapes (`apitypes.go`), capture protocol types (`protocol.go`) and metric names (`metrics.go`).
- `internal/core/memstore`: in-memory reference implementations, used as test oracles.
- `internal/core/coretest`: contract tests that every store implementation runs (`coretest.TerrainStore`, `coretest.TerrainDifferential`, `coretest.MoverStore`, `coretest.OverlayStore`, `coretest.SeriesStore`, `coretest.PyramidLatest`).
- `internal/core/wire`: reference encoders and decoders for the wire formats.
- `web/src/api/types.ts`: the TypeScript mirror of the API JSON shapes, the `DataSource` interface and the perf hook types. `web/src/api/wire.ts` decodes the wire formats.
- `web/src/map/layer.ts`: the `MapLayer` interface every drawn layer implements. `web/src/layers/index.ts` lists the layers the viewer draws.
- `web/fixtures/`: generated example files (tiles, a delta, a palette, mover windows, catalog, view, timeline, overlays, capture envelopes) plus `expected.json` with the decoded values. Regenerate them with `go test ./internal/core/wire -run TestFixtures -update`. Without `-update` the Go test fails if the committed files drift. `npm --prefix web test` decodes them in TypeScript.

## Commands

`cmd/timeshadedb/commands.go` holds a registry. Each command lives in `cmd_<name>.go` and registers itself:

```go
func init() {
	register(Command{Name: "publish", Summary: "export the database as static files", Run: cmdPublish})
}
```

`Run` gets the arguments after the command name and parses them with its own `flag.FlagSet`. `timeshadedb help` lists every command.

Commands that open a database spell these flags the same way:

| Flag | Meaning |
|---|---|
| `--db <dir>` | Database root (FORMAT.md "On-disk layout") |
| `--listen <host:port>` | Listen address, default `127.0.0.1:8080`. Port 0 picks a free port and prints `listening on <addr>` as the first stdout line |
| `--ingest-token <t>` / `--read-token <t>` | Bearer tokens (API.md "Authentication") |
| `--mem-budget <bytes>` | Total cache and buffer budget, default 256 MiB |
| `--window <duration>` | Terrain window for new datastores, default 60s |

## Viewer hooks

The viewer exposes `window.__tsdbPerf` (type `TsdbPerf` in `web/src/api/types.ts`, helpers in `web/src/perf.ts`) for the benchmarks. Event names are fixed:

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

`firstFullPaint` is set at the first `view.full`, `lastFullPaint` and `lastFallbackPaint` at the latest. Times are `performance.now()` milliseconds.

`window.__tsdbControl` (type `TsdbControl`, implemented in `web/src/map/control.ts`) lets the benchmarks drive the viewer: `ready()`, `setView(x, y, s)`, `view()`, `datastore()`, `seek(t)`, `play(speed)`, `time()`, `range()` and `surfaceBounds()`.

The viewer keeps its state in the query string: `ds` (a catalog datastore id) or `map`, `surface` and `force`, then `x`, `y` and `s` (CSS pixels per tile) or `z` (a level, 2^z pixels per tile), `t` (absent means live) and `layers`. `ds` wins over `map`/`surface`/`force` and `s` over `z`.

The bundle is built with relative asset URLs (Vite `base: "./"`), so it works at `/`, under a sub-path and from a static export prefix. Open it at its directory (`/`, `/prefix/`), not at a deeper path. `serve` answers every missing path with 404 (`internal/api/web.go`).
