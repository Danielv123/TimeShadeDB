# TimeShadeDB v3 capture protocol

Status: protocol version 1 (`v` field). Go types: `internal/v3/core/protocol.go`.
Example: `web/fixtures/envelopes.ndjson`. Producers: the capture Lua (WP9). Forwarders:
the Clusterio plugin (WP10) and `timeshadedb tail` (WP6). Consumer: `POST /api/v3/ingest`
(WP6, API.md 5).

## 1. Envelope

One envelope is one JSON object on one line (NDJSON). Lua writes the first group of fields;
the forwarder adds the second group before posting.

| Field | Set by | Type | Meaning |
|---|---|---|---|
| `v` | Lua | int | protocol version, 1 |
| `tick` | Lua | int | `game.tick` when the batch was flushed |
| `speed` | Lua | number, optional | `game.speed` when not 1 (ticks per second = 60 x speed) |
| `msgs` | Lua | array | messages (section 3), in the order they happened |
| `src` | forwarder | string | clock source id, unique per running game: `clusterio:<instance id>` or `standalone:<name>` |
| `map` | forwarder | string | map key (`gw-<world>` for gridworld, `i-<instance id>` otherwise, the `--map` flag standalone) |
| `instance` | forwarder | int, optional | Clusterio instance id (overlay keys, instance bounds) |
| `recv_ms` | forwarder | int | wall-clock ms when the forwarder read the line |
| `seq` | forwarder | int, optional | increasing per `src`; echoed by ingest for acknowledgements |

Envelopes without `src`, `map` or `recv_ms` are rejected by ingest (counted, not fatal).

## 2. Transports

- **Clusterio**: Lua prints `"\f$ipc:timeshade:out?j" .. json` lines, under 3,500 bytes
  each, built from one JSON encoding per message and cut between messages (port of
  illisis `tick.lua`; `send_json` is not used because it can desync clients). The capture
  encodes with `capture/lua/json.lua`, which gives the same JSON as `helpers.table_to_json`
  (key order aside) at about half the cost. The instance
  plugin parses the line, adds the forwarder fields and sends batches to the controller
  about once a second; the controller posts them to its sidecar.
- **Standalone**: the `timeshade_capture` mod appends envelope lines with
  `helpers.write_file("timeshade/<seq>-<tick>.ndjson", lines, true)`, starting a new file
  every 4 MiB or 60 s. `<seq>` is a counter kept in `storage` (raised every 3,600 ticks and
  at install and configuration change, on every peer alike), `<tick>` the game tick when the
  file was opened; both are zero-padded to 10 digits, so names sort in write order within a
  game. Lines are buffered and appended every 120 ticks or 256 KiB (one `write_file` call
  costs about 1 ms on Windows), so up to 2 s of lines are lost if the game exits. In
  multiplayer only the server writes (`for_player = 0`). Loading the same save twice can
  reuse a name and append to that file. `timeshadedb
  tail --dir <script-output>/timeshade --url ... --token ... --src standalone:<name> --map
  <key>` posts files in name order and deletes each file once ingest returned 200 for all
  of it. A file is complete only once a newer file exists: a paused game (a headless server
  pauses with no players) stops writing without closing its file and appends to it again
  on resume, so a pause in growth is not the end of a file. The tailer may post the newest
  file up to its last complete line (ending in `\n`), keeping the byte offset it posted to
  (`timeshadedb tail` keeps it in a state file, so a restart does not repost), but never
  deletes it.

## 3. Messages

Every message has `type`. Ingest ignores unknown types and unknown fields.

| type | Fields | Meaning |
|---|---|---|
| `chunk` | `s` surface, `f` force, `x`, `y` chunk coords, `d`?, `tick`? | `d` = `helpers.encode_string` of the 2,048-byte chart (`LuaForce.get_chunk_chart`: 32 x 32 RGB565 LE): zlib deflate, then standard base64. No `d`: the chunk was deleted. |
| `move` | `s`, `m`: array of samples or gones | mover samples on surface `s` |
| `adopt` | `j`, `trace` | a Universal Edges destination took journey `j` |
| `tag` | `s`, `f`, `n` tag number, `x`?, `y`?, `icon`?, `text`? | chart tag state; no `x`: removed. `icon` is `<signal type>/<name>` |
| `recipe` | `s`, `f`, `u` unit number, `x`?, `y`?, `r`?, `q`? | machine recipe and quality; no `r`: removed (no recipe or machine gone) |
| `rail` | `s`, `f`, `u`, `name`?, `x`?, `y`?, `dir`? | rail entity (optional layer); no `name`: removed |
| `surfaces` | `list`: `[{name, tile}]`, `created`? | the game's surfaces and whether each is in the capture clip |
| `resynced` | `trace`, `counts` | end of a resync |
| `trace` | `op`, `phase`, `level`? | log line; forwarders log it and may drop it |
| `map` | `info`: `core.MapInfo` | forwarder only: map title, instances and bounds, UE edges |

`tick` on a message overrides the envelope tick for that message (a chunk read on an
earlier tick than the flush, for example).

### 3.1 Move entries

Sample: `{j, k, f, name, x, y, h?, tick?, n?, o?, c?, stop?, es?, car?}`.
Gone: `{j, gone: true, tick?}`.

| Field | Meaning |
|---|---|
| `j` | journey id: `<instance>.<tick>.<train id>` for trains, `<instance>.<tick>.v<unit number>` for vehicles, `p:<player name>` for players. The instance and tick are where the id was minted; the id stays the same across joins, splits and instance crossings. |
| `k` | `t` train, `v` vehicle, `p` player |
| `f`, `name` | force; leading carriage or vehicle prototype, or player name |
| `x`, `y` | tiles; a train's leading carriage centre |
| `h` | orientation 0..1 (0 north, 0.25 east); trains: second carriage towards the first (a single carriage: its orientation in the direction of travel). Absent: unknown. |
| `n`, `o`, `c` | train carriage count; kinds from the leading end (`L` loco, `C` cargo, `F` fluid, `A` artillery, lower case faces back; vehicles `S` spidertron or `C` car); colour `rrggbb` |
| `tick` | the game tick of the read, when it is not the envelope's: a train's chord vertex is sent one read late |
| `stop` | at rest. A parked train is not read again until it moves: after its stop sample it sends only resync samples |
| `es` | the leading end switched at this sample (also when a stopped train starts off the other way); the sample before it is the old leading end's last read |
| `car` | every carriage from the leading end as `[x, y, orientation]` (x, y to 0.01 tile, orientation to 0.001; at most 100 carriages, so a sample fits a line); sent on first sight, on stop, on start, at end switches, after the train changed (join, split) and in resyncs |
| `vx`, `vy` | legacy (illisis), ignored |

## 4. Clock anchors

The map clock is wall-clock milliseconds (PLAN.md §4.1). Ingest keeps one anchor per `src`.
Every envelope is a point `(tick, recv_ms)`; its arrival lag is never negative, so the true
clock is a line on or under every point. The anchor fits that line, with a slope (ms per
tick) measured rather than assumed, because a busy server runs below 60 UPS:

```
nominal    = 1000 / (60 * (speed or 1))                        ms per tick
ms_per_tick = least-squares slope through the lowest-latency envelope of each of 12
              equal tick ranges of the last 3 minutes, once the window spans 5 s and
              4 envelopes and the slope is within [0.95, 30] * nominal; else nominal
offset     = min(recv_ms - tick * ms_per_tick) over the last 60 s of envelopes from src
t_ms(tick) = round(offset + tick * ms_per_tick)
```

At exactly 60 UPS this is the minimum of `obs = recv_ms - tick * 1000 / tps` over 60 s.

- On the first envelope of a source the offset is its `obs` at the nominal rate.
- If an envelope's distance above the line changes by more than 5 s from the previous
  envelope's (save reloaded, game paused, host clock stepped), if it is more than 30 s off
  the line, or if the tick goes backwards, the window resets and the line re-fits from the
  new observations.
- If the slope over the last 60 s differs from the 3-minute slope by more than 1%, the UPS
  changed: envelopes older than 60 s are dropped. If every envelope of the last 5 s (at
  least 3) is more than 500 ms above the line, the server slowed down faster than that:
  the window keeps only the last 10 s.
- A game speed change re-bases the anchor at the current tick (`tick - base` replaces
  `tick` above) and continues the clock where it is.
- Every message time is `t_ms(message tick or envelope tick)`. Mover samples also keep the
  raw tick, so velocity is exact below 60 UPS.
- `tsdb_clock_anchor_offset_ms{src}` exports the current offset.
- Measured with 20-400 ms arrival lag and 2% stalls of +3 s (TestAnchorSlowServer): after the
  first minute the error stays under 150 ms at 1 envelope/s (mostly 60-90 ms) and 45 ms at 4/s, at any
  rate from 3 to 60 UPS. In the first 10 s of a source (or after a reset) the nominal rate is
  used, so a slow server is off by up to `(1 - ups/60) * 10 s`; an abrupt UPS drop from 60 to
  40 is off by up to about 3 s for the following minute.

## 5. Mapping to stored data

The ingest decoder (WP6) produces core values:

| Message | Becomes |
|---|---|
| `chunk` with `d` | `ChunkUpdate{DS: {map, s, f}, Chunk: {x, y}, T, Pixels: DecodeChart(d)}`; a chart that does not inflate to 2,048 bytes is a bad message |
| `chunk` without `d` | `ChunkUpdate{..., Deleted: true}` |
| `move` sample | `MoverSample{Surface: {map, s}, Meta: {j, k, f, name, n, o, c}, T, Tick, X, Y, Orientation: h, Flags, Carriages: car}` with `FlagStop` for `stop`, `FlagEndSwitch` for `es`, `FlagNoHeading` when `h` is absent |
| `move` gone | `MoverSample{..., Flags: FlagGone}` with the journey's last known meta (ingest keeps the last meta per journey in a bounded map; unknown: kind from the id prefix, empty names) |
| `tag` | `OverlayEvent{DS: {map, s, f}, T, Interval: {Kind: tag, Key: "<instance>/<n>", Instance, X, Y, Icon, Text}}`; no `x`: `Close` |
| `recipe` | `OverlayEvent{Kind: recipe, Key: "<instance>/<u>", Recipe: r, Quality: q}`; no `r`: `Close` |
| `rail` | `OverlayEvent{Kind: rail, Key: "<instance>/<u>", Name, Direction: dir}`; no `name`: `Close` |
| `map` | stored in `<db>/maps.json`, shown in the catalog |
| `adopt` | moves the journey to the adopting source in ingest's owner map (`movers.Journeys`); not stored. The Clusterio controller may keep its own copy (WP10) |
| `surfaces` without `created` | starts a resync of the source (a resync begins by listing the surfaces): journeys it does not re-send before `resynced` are ended there |
| `resynced` | ends the resync: every journey the source owns and did not re-send gets a gone at the message time; logged |
| `trace` | logged |

Standalone sources use instance 0 in overlay keys.

Ordering: messages of one source are applied in envelope order. Across sources, stores
order by T (terrain: newest T wins per window; movers: sorted per journey). Duplicates from
forwarder retries are harmless (API.md 5).

## 6. Capture requirements (WP9)

These are protocol-level requirements; the Lua is WP9's.

- Never write game state from capture-only data (desync safety). What only decides what is
  printed (the chart cache, the debounce, the outbox, a resync's progress) lives in
  file-local Lua tables. Journeys, their `on_object_destroyed` registrations, the train read
  schedule and each mover's last sample live in `storage` and change only in events, in
  `on_tick` steps and in the config call, which run identically on every peer (as in
  illisis). A resync only reads, so one peer may run it alone: the standalone mod resyncs
  after every load.
- Chart: on `on_chunk_charted` and on a debounce of N ticks (default 600) per changed chunk,
  send a `chunk` only when the chart differs from the last one sent. Keep the last sent
  charts deflated in a capped cache (bounded memory on any surface size; the capture uses
  two generations of 8,192 charts, an approximate LRU).
- Resync on start and reconnect: every charted chunk in the clip, every live mover with
  `car`, every tag and recipe. The walk of charted chunks keeps pace with the chart reads
  (4 a tick while a resync runs, one of them kept for chart events while any wait), so
  50,000 chunks take 3.5 to 4.6 minutes.
- Trains: emit when the chord error since the last sample exceeds 0.25 tile, when the
  orientation changed by more than about 2 degrees, or on a 600-tick heartbeat while
  moving; always send the tick; send `car` on stop, start, end switch and resync; set `es` on end switches.
- Vehicles and players: emit when off the dead-reckoned position by more than 1 tile or on
  the heartbeat; send `h` for vehicles too.
- Gone: on leaving the surface, the instance (crossing) or the game; on instance stop for
  every player.
- Budget: at most 0.3 ms per tick with 100 moving trains and 50,000 charted chunks (D9).
