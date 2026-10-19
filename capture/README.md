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
node run_game.mjs --mode bench    # UPS benchmark (about a minute)
go test ./capture/...             # (from the repo root) real game output decodes into core types
```

Options: `--keep` leaves the temporary directory; `--write-fixture` (test mode) rewrites
`fixtures/game_envelopes.ndjson`, the Go test's input, from the run. The bench takes
`--runs N` (interleaved pairs, 3), `--host clusterio` (bench the Clusterio module instead of
the mod), `--compare <capture dir>` (run another version of `capture/`, for example an older
checkout's, interleaved on the same save), `--real-charts <illisis export>` (use 509 charts
cut from a real map instead of generated ones), `--profile` (per-step LuaProfiler totals),
`--micro` (per-operation timings), and `--trains`, `--charted`, `--chart-reads`.

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
and feeds 20 chart events a tick over a virtual map of 50,176 charted chunks: a quarter on
201 chunks that change every tick, the rest spread over the whole map. The event queue
fills to its cap and almost every read finds a chart the capture has not sent yet, which it
deflates and sends (88 KB of output per game second). It runs the same save with and
without the capture, interleaved, three times each; the run without loads a no-op stand-in
for the capture's test interface, so the driver's remote calls cost the same in both. It
compares Factorio's `--benchmark-verbose` timings over ticks 600 to 3,600, then a resync.
It fails when the Lua delta (`scriptUpdate + luaGarbageIncremental`) or the whole-tick delta
is over 0.3 ms/tick, and warns about the resync's cost, slow ticks and a baseline that moved
between pairs (a busy machine).

Timings move with the machine: on a shared one, pin the run to a few cores, for example
`cmd /c start "" /affinity FF00 /wait /b node run_game.mjs --mode bench` (the Factorio
processes inherit the mask), and use `--compare` to judge a change. Measured on 2026-10-19,
Factorio 2.1.19, Ryzen 9 9950X3D, Windows 11, defaults (one chart read a tick), with other
jobs running on the machine, mean of the steady state:

| | This version | v3 before (`--compare`) |
|---|---|---|
| Standalone mod, cores 4-7 (mask `FF00`), 5 pairs | 0.195 ms (pairs 0.189 to 0.201), whole tick 0.195 | 0.222 (0.210 to 0.257), whole tick 0.233 |
| The same, cores 12-15 (mask `FF000000`), 3 quiet pairs of 4 | 0.200 to 0.211 | 0.227 to 0.293 |
| Clusterio module (`--host clusterio`), cores 4-7, 3 pairs | 0.196 (0.191 to 0.204), whole tick 0.205 | 0.237, whole tick 0.256 |
| Standalone, `--real-charts`, cores 4-7, 3 pairs | 0.203 (0.198 to 0.206) | |

The median tick costs 0.157 ms and the 99th percentile 1.6 ms (the file write every 120
ticks, the mover scan every 30). The slowest tick, 6.6 to 7.2 ms, comes once, when the event
queue first reaches its cap (tick 1,429 at 20 events a tick, 2,875 at 10); its cause is not
pinned down (table growth or a garbage collection step). During a resync (four reads a tick, three of them forced,
each also listing the chunk's machines) the capture costs 0.39 to 0.41 ms/tick, for about
3.5 minutes per 50,000 chunks. Unpinned on the same busy machine the baseline tick itself
varied 2.3 times between pairs, and the capture cost 0.19 to 0.47 ms/tick (0.27 to 0.41
before); the budget is not verified on a quiet machine or on Linux.

Where a tick goes (`--profile`, which adds about 0.5 µs a wrapped call):

| Step | µs/tick |
|---|---|
| `helpers.encode_string` of the read chart (45 to 56 µs a call in the running game, 21 µs back to back) | 49 |
| the rest of the chart read: the queue, the cool-downs, the seam, the cache | 21 |
| train reads (11 a tick) and the mover scan | 52 |
| flush: JSON (21), the line cut (4), the file buffer (1) | 29 |
| 20 chart events, with the event tables the bench builds in Lua | 46 |
| `helpers.write_file`, once every 120 ticks | 13 |

`helpers.write_file` costs about 1.3 ms a call on Windows whatever the size (measured over
files from 0 to 4 MiB, and with the lines passed as a LocalisedString tree instead of one
string: slower), so it is a slow tick every 2 s and 11 µs/tick on average. Charts made of
real terrain deflate to fewer bytes than the generated ones but take longer to deflate.

What changed since the 0.330 ms/tick recorded here before: a queued chart read
is now the chunk's key alone, in a ring of 20,000 slots (it was a table per read in a table
whose indexes drifted into its hash part), which halves the capture's Lua heap in this bench
(19.8 to 9.4 MiB); chunk messages and move messages are encoded
without escaping the base64 or building a string per sample; the chord test avoids square
roots. The output is the same: the bench writes the same 7,209,102 bytes, and the game
test's envelopes match field for field (apart from entity unit numbers, all 7 lower in the
new run). Much of the earlier figure was the machine: the
earlier version measures 0.21 to 0.29 ms/tick pinned in the runs above.

## Licence

The files with an `SPDX-License-Identifier: BlueOak-1.0.0` header are ported from
[illisis/minimap](https://github.com/illisis/minimap) and stay under the Blue Oak Model
License 1.0.0 ([LICENSE-BlueOak-1.0.0.md](LICENSE-BlueOak-1.0.0.md)); both builds include
that file.
