# TimeShadeDB v3 capture

The Lua that records a Factorio game for the TimeShadeDB v3 cluster map: the map chart,
trains, vehicles, players, chart tags, machine recipes and (optionally) rail entities. It
writes the capture envelopes of [docs/v3/PROTOCOL.md](../docs/v3/PROTOCOL.md). One library
serves two hosts:

| Host | Directory | Output | Forwarder |
|---|---|---|---|
| Standalone mod `timeshade_capture` | `mod/timeshade_capture/` | `script-output/timeshade/<seq>-<tick>.ndjson` | `timeshadedb tail` (WP6) |
| Clusterio save-patch module `timeshade` | `clusterio_module/` | `\f$ipc:timeshade:out?j<envelope>` lines on stdout | the Clusterio plugin (WP10) |

It replaces `timeshadedb_exporter/`, which does not run on Factorio 2.0.

## Layout

| Path | Contents |
|---|---|
| `lua/core.lua` | Pure rules: clip, chunk keys, queues, chart debounce, sent-chart cache, train emission, dead reckoning, journeys, the line cut, storage migrations |
| `lua/outbox.lua` | Per-tick message queue, flushed as envelope lines under 3,500 bytes |
| `lua/json.lua` | The JSON encoder (cheaper than `helpers.table_to_json`) |
| `lua/terrain.lua` | `on_chunk_charted` through the debounce, chart reads and compare, surfaces, the resync walk |
| `lua/movers.lua` | Scan, read wheel, train/vehicle/player samples, joins and splits, Universal Edges hand-off |
| `lua/overlays.lua` | Chart tags, recipes, rails |
| `lua/capture.lua` | Entry points: `setup`, `config`, `resync`, `on_tick`, the event table |
| `mod/timeshade_capture/` | The standalone mod (settings, file rotation, `/timeshade` command, remote interface) |
| `clusterio_module/` | The module's `control.lua` and `globals.lua` (`ts_rt` RCON entry point) |
| `build.mjs` | Assembles both hosts with the library into `dist/` |
| `test/` | Unit tests (Lua 5.4 in WebAssembly), headless game tests and the UPS benchmark, a Go decode test |

## Build

```sh
node capture/build.mjs                         # dist/timeshade_capture_0.1.0{/,.zip} and dist/clusterio_module/
node capture/build.mjs --factorio-version 2.1  # Factorio 2.1 refuses mods that declare 2.0
```

The library runs unchanged on Factorio 2.0 and 2.1 (tested on 2.1.19, see below). `dist/` is
build output and is not committed.

## Standalone use

1. Put `dist/timeshade_capture_0.1.0.zip` (or the directory) in the game's `mods` folder.
2. Run `timeshadedb tail --dir <user data>/script-output/timeshade --url ... --token ...
   --src standalone:<name> --map <key>` next to it. It posts complete files and deletes them.

Map settings (runtime, per save): capture on or off, chart debounce (600 ticks), chart reads
per tick (1), sent-chart cache size (16,384 chunks), rail layer (off), extra forces to
capture (forces with a player are always captured). `/timeshade stats` prints counters,
`/timeshade resync` resends everything. Remote interface `timeshade_capture`: `resync`,
`stats`.

Files: one every 60 s of game time or 4 MiB, and a new one at every load; lines are
buffered and appended every 2 s, so up to 2 s are lost if the game exits. A paused game
(a headless server with no players) appends to its current file again on resume, so a file
is complete only once a newer one exists (PROTOCOL.md 2). In multiplayer only the server
writes. Run one game per user-data directory at a time: every save writes to the same
`script-output`.

## Clusterio use

The module directory is a plugin's `module/` directory (WP10 copies `dist/clusterio_module`).
At every instance start, on a `gridworld.tile` change and on reconnect, the instance plugin
runs over RCON:

```lua
/sc ts_rt.config('{"tile":{"world":"w","x":0,"y":0,"size":512,"surface":"nauvis"},"rails":false}')
/sc ts_rt.resync("start-1")
```

Nothing is captured before the config. Journey ids start with the Clusterio instance id.
With Universal Edges patched in, a crossing train or vehicle keeps its journey and the
destination sends `adopt`. `ts_rt.stats()` prints the counters to RCON; `ts_rt.seam.chart`
and `ts_rt.seam.charted` stand in for charting on headless servers.

## How it differs from illisis/minimap

- Envelope `{v, tick, speed?, msgs}`; message types without the `mm.` prefix.
- Chart debounce: a chunk is read at most once per 600 ticks; a chunk charted while
  cooling down is read when the cool-down ends. Measured in game: a chunk that changed every
  tick for 2,000 ticks was sent 6 times (2 of them forced by resyncs).
- The last sent charts are kept deflated in two generations of 8,192 (an approximate LRU,
  about 380 bytes each), not one 2 KiB string per chunk forever. The raw chart of a chunk
  that is cooling down is kept too, so an unchanged re-read skips the deflate. It is
  dropped when the cool-down ends with no new chart, or when the chunk is re-queued behind
  more than 256 reads, so at most max(chart reads, resync reads) x debounce + 257 = 2,657
  charts (5.2 MiB) are held.
- Forced reads (a resync, a deleted chunk) wait in their own queue, read first and not
  capped; while a resync runs one of its 4 reads a tick goes to chart events whenever any
  wait, so neither starves the other.
- Trains: every sample carries its read tick (the envelope's, or its own `tick` for a chord
  vertex sent one read late). A sample is sent when a read is off the chord from the last
  sample by more than 0.25 tile (or 1 tile off its time-interpolated place), when the
  heading turned more than 2 degrees, when 16 reads are buffered, or on the 600-tick
  heartbeat. `car` (every carriage's position and orientation) comes with first sight,
  stop, start, end switch (`es`), a changed train and resyncs. `h` points from the
  carriage behind the leading one towards it, as in illisis (a single carriage: its own
  orientation in the direction of travel). A parked train is not polled: after its stop
  sample it is sent again only by a resync or when it moves. No `vx`/`vy`.
- Vehicles keep their heading; players are sent by dead reckoning as before.
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
start empty, which only changes what that peer would write.

## Tests

```sh
cd capture/test && npm ci
npm test                          # unit tests: node run_unit.mjs (Lua 5.4 via wasmoon)
node run_game.mjs                 # headless Factorio: standalone mod and Clusterio module
node run_game.mjs --mode bench    # UPS benchmark (about 15 minutes)
go test ./capture/...             # (from the repo root) real game output decodes into core types
```

`run_game.mjs` needs a Factorio 2.x install (`--factorio <exe>` or `FACTORIO_EXE`; default:
the Steam path). It works in a fresh temporary directory with its own write-data, mods and
saves. Headless Factorio never charts, so a synthetic chart seam stands in for
`get_chunk_chart` and the harness recomputes every chart's bytes.

What the game tests check, on the standalone mod and on the Clusterio module (loaded as a
patched save loads it, with a stub Clusterio API):

- every line is a valid envelope under 3,500 bytes; every chart inflates to the 2,048 bytes
  the seam produced at that tick;
- the debounce bound; a resync sends all 400 charted chunks and ends with `resynced`;
- two trains, one on a straight track (east, stop, back west), one round a loop built with
  `LuaRailEnd.get_rail_extensions` (straight, `curved-rail-a`, `curved-rail-b`): every
  sample sits on the leading carriage at its tick, the polyline of samples stays within
  0.5 tiles of the true position at every tick (measured 0.20 to 0.30), `car` comes with first sight, stops and the
  end switch (and starts at the new leading carriage after it), kinds flip from `LCC` to
  `ccl`, a destroyed train sends one `gone`;
- a third train under a schedule (automatic mode, a temporary stop on a rail): its
  `LuaTrain.path` is a `LuaRailPath` (`size`, `current`, `total_distance`,
  `travelled_distance`, `rails`) while it drives, and its samples follow its line;
- a car's samples carry heading and kind; recipes open and close; the rail layer sends
  three built rails and one removal;
- the 2.0 API used: `LuaTrain.front_end`/`back_end` (`LuaRailEnd`), `path` (nil in manual
  mode), `get_rails`, `front_stock`/`back_stock`, `carriages`, carriage `orientation` and
  `is_headed_to_trains_front`, the rail prototype types, `helpers.write_file`,
  `helpers.encode_string`.

### UPS benchmark

`run_game.mjs --mode bench` builds 100 rail loops (24 straight rails a side, so about 30%
of the track is curve) with a 4-carriage train each, driven at 0.5 tiles/tick (108 km/h),
and feeds 20 chart events a tick over a virtual map of 50,176 charted chunks (a quarter of
them on 201 chunks that change every tick). It runs the same save with and without the mod,
interleaved, three times each, and compares Factorio's `--benchmark-verbose` timings
(`scriptUpdate + luaGarbageIncremental`) over ticks 600 to 3,600, then a resync.

Measured on 2026-10-19, Factorio 2.1.19, Ryzen 9 9950X3D, Windows 11, on a machine
shared with other build jobs (the baseline itself moved by about 25% between runs):

| | Capture cost (Lua ms/tick) |
|---|---|
| Steady state, mean | 0.287 and 0.302 (two runs of the final code; 0.29 to 0.36 with the earlier 1-second write) |
| Steady state, median | 0.237 and 0.247 |
| During a resync (4 forced reads a tick) | about 0.48 |
| Spikes | about 2 ms on the tick that writes the 2-second file buffer (`write_file`) |

These numbers include the test harness's own cost of feeding chart events through a remote
interface (estimated 20 to 40 µs/tick). The budget (D9) is 0.3 ms/tick, so this worst case
is at the budget, not clearly under it. Per operation (`run_game.mjs --mode bench --micro`):
a queued chart event 0.4 to 0.8 µs, a new one 1.6 to 2.4 µs, a train read 4 µs,
`helpers.encode_string` of a chart 22 µs, a 3-sample move message 13 µs to encode
(`helpers.table_to_json`: 24 to 33 µs), one `helpers.write_file` call about 1 ms.

## Licence

The files with an `SPDX-License-Identifier: BlueOak-1.0.0` header are ported from
[illisis/minimap](https://github.com/illisis/minimap) and stay under the Blue Oak Model
License 1.0.0 ([LICENSE-BlueOak-1.0.0.md](LICENSE-BlueOak-1.0.0.md)); both builds include
that file.
