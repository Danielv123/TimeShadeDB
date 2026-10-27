# TimeShadeDB capture

The Lua that records a Factorio game for TimeShadeDB: the map chart, trains, vehicles,
players, chart tags, machine recipes and (optionally) rail entities. It writes the capture
envelopes of [docs/PROTOCOL.md](../docs/PROTOCOL.md). One library serves two hosts:

| Host | Directory | Output | Forwarder |
|---|---|---|---|
| Standalone mod `timeshadedb_exporter` | `mod/timeshadedb_exporter/` | `script-output/timeshade/<seq>-<tick>.ndjson` | `timeshadedb tail` |
| Clusterio save-patch module `timeshade` | `clusterio_module/` | `\f$ipc:timeshade:out?j<envelope>` lines on stdout | the Clusterio plugin ([clusterio/](../clusterio/README.md)) |

## Layout

| Path | Contents |
|---|---|
| `lua/core.lua` | Pure rules: clip, chunk keys, queues, chart debounce, sent-chart cache, train emission, dead reckoning, journeys, the line cut, storage migrations |
| `lua/outbox.lua` | Per-tick message queue, flushed as envelope lines under `core.LIMIT` bytes (each host sets it with `core.limits`: 64 KiB for the Clusterio module's IPC lines, 200,000 for the standalone mod's files) |
| `lua/json.lua` | The JSON encoder (cheaper than `helpers.table_to_json`) |
| `lua/terrain.lua` | `on_chunk_charted` through the debounce, chart reads and compare, surfaces, the resync walk |
| `lua/movers.lua` | Scan, read wheel, train/vehicle/player samples, joins and splits, Universal Edges hand-off |
| `lua/overlays.lua` | Chart tags, recipes, rails |
| `lua/capture.lua` | Entry points: `setup`, `config`, `resync`, `on_tick`, the event table |
| `mod/timeshadedb_exporter/` | The standalone mod (settings, file rotation, `/timeshade` command, remote interface). `mod/README.md` is its mod portal description |
| `clusterio_module/` | The module's `control.lua` and `globals.lua` (`ts_rt` RCON entry point) |
| `build.mjs` | Assembles both hosts with the library into `dist/` |
| `test/` | Unit tests (Lua 5.4 in WebAssembly), headless game tests and the UPS benchmark, a Go decode test |

## Build

```sh
node capture/build.mjs                         # dist/timeshadedb_exporter_<version>{/,.zip} and dist/clusterio_module/
node capture/build.mjs --factorio-version 2.1  # Factorio 2.1 refuses mods that declare 2.0
```

`scripts/build.ps1` also builds the mod. The library runs unchanged on Factorio 2.0 and
2.1. `dist/` is build output and is not committed.

## Standalone use

1. Put `dist/timeshadedb_exporter_<version>.zip` (or the directory) in the game's `mods` folder.
2. Run `timeshadedb tail --dir <user data>/script-output/timeshade --url ... --token ...
   --map <key>` next to it (`--src` defaults to `standalone:<map>`). It posts complete files
   and deletes them.

Map settings (runtime, per save): capture on or off, chart debounce (600 ticks), chart reads
per tick (1), sent-chart cache size (16,384 chunks), rail layer (off), extra forces to
capture (forces with a player are always captured). `/timeshade stats` prints counters,
`/timeshade resync` resends everything. Remote interface `timeshadedb_exporter`: `resync`,
`stats`.

Files: one every 60 s of game time or 4 MiB, and a new one at every load. Lines are
buffered and appended every 2 s, so up to 2 s are lost if the game exits. A paused game
(a headless server with no players) appends to its current file again on resume, so a file
is complete only once a newer one exists (PROTOCOL.md 2). In multiplayer only the server
writes. Run one game per user-data directory at a time: every save writes to the same
`script-output`.

## Clusterio use

The module directory becomes the plugin's `module/` directory (`npm run build` in
`clusterio/` builds it with `build.mjs` and copies it there). At every instance start, on a
`gridworld.tile` change and on reconnect, the instance plugin runs over RCON:

```lua
/sc ts_rt.config('{"tile":{"world":"w","x":0,"y":0,"size":512,"surface":"nauvis"},"rails":false}')
/sc ts_rt.resync("start-1")
```

Nothing is captured before the config. Journey ids start with the Clusterio instance id.
With Universal Edges patched in, a crossing train or vehicle keeps its journey and the
destination sends `adopt`. `ts_rt.stats()` prints the counters to RCON. `ts_rt.seam.chart`
and `ts_rt.seam.charted` stand in for charting on headless servers.

## How it captures

- Chart debounce: a chunk is read at most once per 600 ticks. A chunk charted while
  cooling down is read when the cool-down ends.
- The last sent charts are kept deflated in two generations of 8,192 (an approximate LRU,
  about 380 bytes each). The raw chart of a chunk that is cooling down is kept too, so an
  unchanged re-read skips the deflate. It is dropped when the cool-down ends with no new
  chart, or when the chunk is re-queued behind more than 256 reads, so at most
  max(chart reads, resync reads) x debounce + 257 = 2,657 charts (5.2 MiB) are held.
- Forced reads (a resync, a deleted chunk) wait in their own queue, are read first and are
  not capped. While a resync runs, one of its 4 reads a tick goes to chart events whenever
  any wait, so neither starves the other.
- Trains are described by their rail path and the timing along it. A train is read every 5
  tiles of travel (sooner while accelerating, at most 30 ticks apart). The curved pieces and
  ramps it enters go with the next sample (`rp`), and a sample is sent when a read lies more
  than 0.1 tile of rail distance off the timing model, at least every 120 ticks while moving,
  on the 600-tick heartbeat, and on first sight, stop, start, end switch and surface change.
  The last read since the last sample goes out before a gone, or when Universal Edges
  captures the train for a crossing, so the way into a border box is not lost.
  A parked train is not polled. [RAIL-GEOMETRY.md](../docs/RAIL-GEOMETRY.md) 8 has the
  full rules. Deploy the server and viewer before a capture that sends rail path samples
  (PROTOCOL.md 3.1).
- Vehicles carry their heading. Vehicles and players are sent by dead reckoning, when they
  are more than 1 tile off the prediction.
- Recipe and rail removals come from the mined and died events, so nothing new is registered
  for them.
- The resync never writes `storage`, so a single peer may run it (the standalone mod resyncs
  after every load). The walk of charted chunks keeps pace with the reads.

## Desync rules

Storage holds what must be the same on every peer: journeys and their
`on_object_destroyed` registrations, the train read wheel, each mover's last sample, the
forces, the config and the standalone file counter. It changes only in events, `on_tick`
steps and `capture.config` (RCON, `on_init`, `on_configuration_changed`, a settings change),
which run on every peer. File-local tables hold what only decides what is written: the
debounce and chart cache, the outbox, the resync's progress, the file buffer. On load they
start empty, which only changes what that peer would write. Storage carries a schema number,
and `core.MIGRATIONS` brings an older one up to date.

## Tests

```sh
cd capture/test && npm ci
npm test                          # unit tests: node run_unit.mjs (Lua 5.4 via wasmoon)
node run_game.mjs                 # headless Factorio: standalone mod and Clusterio module
node run_game.mjs --mode bench    # UPS benchmark (about a minute)
go test ./capture/...             # (from the repo root) real game output decodes into core types
```

`run_game.mjs` needs a Factorio 2.x install (`--factorio <exe>` or `FACTORIO_EXE`, default
the Steam path). It works in a fresh temporary directory with its own write-data, mods and
saves. Headless Factorio never charts, so a synthetic chart seam stands in for
`get_chunk_chart` and the harness recomputes every chart's bytes. `--keep` leaves the
temporary directory. `--write-fixture` (test mode) rewrites
`fixtures/game_envelopes.ndjson`, the Go test's input, from the run.

The game tests run the standalone mod and the Clusterio module (loaded as a patched save
loads it, with a stub Clusterio API) and check:

- every line is a valid envelope under its host's limit, and every chart inflates to the
  2,048 bytes the seam produced at that tick;
- the debounce bound, and that a resync sends all 400 charted chunks and ends with
  `resynced`;
- two trains driven by hand, one on a straight track and one round a loop with both kinds of
  curved rail: what the viewer draws from the samples (`web/src/movers/track.ts`, history
  mode) stays within 0.5 tiles of the leading carriage at every tick it moves, plus stops,
  end switches and `gone`;
- a third train under a schedule, whose samples follow its `LuaTrain.path`;
- a car's heading and kind, recipes opening and closing, and three built rails and one
  removal on the rail layer;
- the 2.0 API the capture uses (`LuaTrain.front_end`/`back_end`, `path`, `get_rails`,
  carriage `orientation`, the rail prototype types, `helpers.write_file`,
  `helpers.encode_string`).

### UPS benchmark

`run_game.mjs --mode bench` builds 100 rail loops with a 4-carriage train each, driven at
0.5 tiles/tick, and feeds 20 chart events a tick over a virtual map of 50,176 charted
chunks, so almost every read finds a chart the capture has not sent yet. It runs the same
save with and without the capture, interleaved, and compares Factorio's
`--benchmark-verbose` timings over ticks 600 to 3,600, then a resync. It fails when the Lua
cost (`scriptUpdate + luaGarbageIncremental`) or the whole-tick cost of the capture is over
0.3 ms/tick, and warns about the resync's cost, slow ticks and a baseline that moved between
pairs (a busy machine).

Options: `--runs N` (interleaved pairs, 3), `--host clusterio` (bench the Clusterio module
instead of the mod), `--compare <capture dir>` (run another version of `capture/`, for
example `git archive <rev> capture`, interleaved on the same save), `--real-charts <illisis
export>` (use 509 charts cut from a real map instead of generated ones, standalone mod
only), `--profile` (per-step LuaProfiler totals), `--micro` (per-operation timings), and
`--trains`, `--charted`, `--chart-reads`.

Timings move with the load on the machine. On a shared one, pin the run to a few cores, for
example `cmd /c start "" /affinity FF00 /wait /b node run_game.mjs --mode bench` (the
Factorio processes inherit the mask), and use `--compare` to judge a change.
`bench/railbench` has the same benchmark on a real save (`railbench.mjs ups`).

## Licence

The files with an `SPDX-License-Identifier: BlueOak-1.0.0` header are ported from
[illisis/minimap](https://github.com/illisis/minimap) and stay under the Blue Oak Model
License 1.0.0 ([LICENSE-BlueOak-1.0.0.md](LICENSE-BlueOak-1.0.0.md)). Both builds include
that file.
