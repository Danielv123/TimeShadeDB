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
  each, built from `helpers.table_to_json` per message and cut between messages (port of
  illisis `tick.lua`; `send_json` is not used because it can desync clients). The instance
  plugin parses the line, adds the forwarder fields and sends batches to the controller
  about once a second; the controller posts them to its sidecar.
- **Standalone**: the `timeshade_capture` mod appends envelope lines with
  `helpers.write_file("timeshade/<seq>.ndjson", line .. "\n", true)`, starting a new file
  every 4 MiB or 60 s (`<seq>` is a zero-padded counter kept in `storage`). `timeshadedb
  tail --dir <script-output>/timeshade --url ... --token ... --src standalone:<name> --map
  <key>` posts complete files in order and deletes each file once ingest returned 200
  (files are complete once a newer file exists or after 10 s without growth).

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
| `h` | orientation 0..1 (0 north, 0.25 east); trains: second carriage towards the first. Absent: unknown. |
| `n`, `o`, `c` | train carriage count; kinds from the leading end (`L` loco, `C` cargo, `F` fluid, `A` artillery, lower case faces back; vehicles `S` spidertron or `C` car); colour `rrggbb` |
| `stop` | at rest |
| `es` | the leading end switched at this sample |
| `car` | every carriage from the leading end as `[x, y, orientation]`; sent on stop, on start, at end switches and in resyncs |
| `vx`, `vy` | legacy (illisis), ignored |

## 4. Clock anchors

The map clock is wall-clock milliseconds (PLAN.md §4.1). Ingest keeps one anchor per `src`:

```
tps        = 60 * (speed or 1)
obs        = recv_ms - tick * 1000 / tps          (one value per envelope)
offset     = min(obs) over the last 60 s of envelopes from src (the lowest-latency observation)
t_ms(tick) = round(offset + tick * 1000 / tps)
```

- On the first envelope of a source the offset is its `obs`.
- If `obs` jumps by more than 5 s (save reloaded, game paused, host clock stepped) or the
  tick goes backwards, the window resets and the offset re-fits from the new observations.
- Every message time is `t_ms(message tick or envelope tick)`. Mover samples also keep the
  raw tick, so velocity is exact below 60 UPS.
- `tsdb_clock_anchor_offset_ms{src}` exports the current offset.

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
| `adopt`, `surfaces`, `resynced`, `trace` | logged; `adopt` is used by the Clusterio controller's owner map (WP10), not stored |

Standalone sources use instance 0 in overlay keys.

Ordering: messages of one source are applied in envelope order. Across sources, stores
order by T (terrain: newest T wins per window; movers: sorted per journey). Duplicates from
forwarder retries are harmless (API.md 5).

## 6. Capture requirements (WP9)

These are protocol-level requirements; the Lua is WP9's.

- Never write game state from capture-only data (desync safety). Keep capture state in
  file-local Lua tables, not `storage`, except the standalone file counter.
- Chart: on `on_chunk_charted` and on a debounce of N ticks (default 600) per changed chunk,
  send a `chunk` only when the chart differs from the last one sent. Keep the last sent
  charts deflated in a capped LRU (bounded memory on any surface size).
- Resync on start and reconnect: every charted chunk in the clip, every live mover with
  `car`, every tag and recipe.
- Trains: emit when the chord error since the last sample exceeds 0.25 tile, when the
  orientation changed by more than about 2 degrees, or on a 600-tick heartbeat; always send
  the tick; send `car` on stop, start, end switch and resync; set `es` on end switches.
- Vehicles and players: emit when off the dead-reckoned position by more than 1 tile or on
  the heartbeat; send `h` for vehicles too.
- Gone: on leaving the surface, the instance (crossing) or the game; on instance stop for
  every player.
- Budget: at most 0.3 ms per tick with 100 moving trains and 50,000 charted chunks (D9).
