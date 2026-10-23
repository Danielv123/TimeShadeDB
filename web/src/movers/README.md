# Movers: trains, vehicles and players

The movers layer draws every train, vehicle and player of the shown surface at the displayed
time. It reads MVW3 windows, keyframes and live payloads
([docs/FORMAT.md](../../../docs/FORMAT.md) 7.4) and live `movers` events
([docs/API.md](../../../docs/API.md) 5).

| File | Role |
|---|---|
| `track.ts` | Samples per journey, rail runs of rail path samples, segment rules (gaps, gones, handoffs, end switches), derived velocities, head position, body path, carriage placement |
| `railpath.ts` | The rail path model ([docs/RAIL-GEOMETRY.md](../../../docs/RAIL-GEOMETRY.md) 8): piece shapes and ends, the chain of a run, projection, carriage placement, the timing model |
| `segment.ts` | Geometry between two samples without a rail path: line+arc or arc+line where it fits, cubic Hermite otherwise, arc-length timing |
| `hermite.ts` | Hermite basis, orientation helpers, arc-length polyline |
| `view.ts` | `MoverView`: tracks fed from windows and events, per-frame evaluation, never-snap corrections, trimming |
| `clock.ts` | `DelayEstimator` (adaptive live delay and data horizon), `DataClock` |
| `feed.ts` | `MoverFeed`: live (SSE or polled `live.bin`) and history (day index, keyframe, windows) loading with a generation counter |
| `draw.ts` | Train, vehicle and player drawing (ported from illisis, Blue Oak 1.0.0) |
| `layer.ts` | `MoverLayer`, the `MapLayer` the terrain engine ([../map](../map/README.md)) draws |
| `bench.ts` | `createTrainRenderer()`: this module as a renderer for the train evaluator in `bench/trains` |
| `eval/` | Train evaluator (leave-one-out, rendered distance to rails, jumps) and the illisis baseline port |
| `bench/` | Browser benchmark: 500 trains drawn per animation frame |

## Rail path samples

A train sample with a speed (`F_RAIL`, PROTOCOL.md 3.1 `v`, `rp`, `ru`) comes from the
capture's rail path model (RAIL-GEOMETRY.md 8). The track groups such samples into runs
(`railRuns`). A run begins at a sample with `ru`, after a sample of another kind, at an end
switch, or where a listed piece does not continue the run's path. Its chain (`railpath.ts`)
is the curved pieces and ramps of its samples joined by straight lines, the exact rail
centre line.

Each sample's leading joint (its centre plus 2 tiles along its own orientation) is projected
onto the chain in order, no further along than its speeds allow, so a train that comes back
past a place is not put on its later pass. A run's first sample is placed no earlier than its
last listed piece. Between two samples of a run the leading joint's rail distance follows the
timing model (stand-then-accelerate from a stop, brake-then-stand into one, else a cubic
Hermite in time with the captured speeds) over the game ticks between them. Two samples whose
ticks cannot be of one game (going back, at a rate no game runs at, or more than 4 times off
the neighbouring samples' rate, as at an instance crossing whose gone row was lost) are timed
by the time between them at the neighbours' rate instead (`railTicks`, and `tickRate` keeps
such pairs out of every other rate). The head is the midpoint of the joints at that rail
distance and 4 behind it, and every carriage sits on the chain at its joint distances
(`placeRailCarriages`, vanilla joint distances 4 and 3, pitches within a quarter tile of 7
taken as 7).

Past the newest sample the head runs on along the chain at its speed, braking to a stop when
the last two speeds fell, within the caps of step 4 below. Ahead of the newest piece the chain
is the straight it ended on. A stop sample links to the start however long the train stood.
Stop and start samples list the pieces under the whole train, so a parked train drawn from its
stop sample alone (a keyframe, the live payload) is on its rails. A new run whose pieces reach
back to the sample before it (a first sight after a resync, a missed piece) links that
segment on its chain. Trimming (step 9) makes the first kept rail sample a run start with the
pieces of its chain under the body behind it.

## Other samples, and rules for all movers

Steps 2, 3 and 5 apply to samples without a rail path (imported data, vehicles, players) and
to segments a run does not cover. The others apply to every mover.

1. Delay behind live. In live mode the drawn time is `now - delay`. The delay is the larger
   of the p99 of measured arrival lags plus 0.5 s and the p95 of readiness plus 0.25 s,
   capped at 6 s. Readiness is, for a moving mover, the time from a sample until the next one
   has arrived: past it the segment can be interpolated instead of extrapolated. MVW3 stores
   no velocity, so the viewer extrapolates less well than the arrival lag alone assumes. With
   rail path samples the delay is at least a prior until 32 readiness lags were observed,
   set when the first such sample arrives (`DelayEstimator.railPrior`): the capture's longest
   wait between samples of a moving train in real time (120 ticks, 60 below game speed 0.5)
   plus 1.5 s, so 3.5 s at speed 1. A rail path stop sample is not counted, since the rail
   path holds the train there until it starts. The delay is clamped to 0.5-30 s, drops only
   when the estimate falls 250 ms below it, grows by at most 20% of elapsed time (time slows
   to 0.8x) and shrinks by at most 5% (time runs at most 1.05x), so the clock never jumps and
   movers never visibly race. Static exports start from 2 x `live_ms`.
2. Velocities. A sample's speed is the derivative of the quadratic through its neighbours
   (time-weighted chord speeds). Its direction averages two estimates: the tangent of the
   circle through the neighbouring samples, and the captured orientation turned forward by
   curvature x half a pitch (the orientation is the chord from the second carriage to the
   first, which lags the head's tangent on curves).
3. Segments. Between linked samples the head follows a line then an arc (or an arc then a
   line) when the end points and tangents allow it, since rails are lines and arcs. Otherwise
   it follows a geometric cubic Hermite: tangent lengths that make it a circular arc for a
   symmetric turn (the chord when straight), walked by arc length through a 16-step length
   table. Motion along the geometry is a 1D Hermite of arc length with the end speeds.
   Geometry much longer than its chord is rejected as a loop. Over gaps of 30 tiles or more
   (a sample gap at speed, often at an instance crossing) trains get rail-shaped paths, where
   a cubic would cut corners by several tiles. An end heading counts as straight when it
   lies within 2 degrees of one of the 16 rail directions. The captured orientation is used
   for it when it lies within 1 degree of one (and 15 degrees of the derived tangent). Both
   ends straight and turning by 10 degrees or more: a line, an arc of radius 13 and a line.
   End tangents that make an S (both at least 4 degrees to the same side of the chord): a
   line, arc, straight in a rail direction, arc (or arc, line, arc, line when the mover
   turned harder before the gap than after it), radius 13, falling back to 9, 20 and 30
   tiles, at most 1.2 times the chord. Otherwise, when line+arc would need a curve tighter
   than 3 tiles (or has no solution) and one end is straight: a line from that end and an arc
   of radius 13 to the other end point, accepting a heading kink of up to 15 degrees there.
4. Extrapolation. Past the newest sample the head runs on along its velocity. Inside the data
   horizon (every sample older than `now - p99 lag` has arrived) silence means the mover is
   still on its predicted line, up to the capture heartbeat (`HEARTBEAT_MS` in `track.ts`, 11
   s for the 600-tick heartbeat), so it runs straight. If the capture's sampling rule
   changes, `HEARTBEAT_MS` must follow it. Past the horizon (at most 1 s) it turns at the
   curvature it was in when the last sample was taken (the end of the segment before it: a
   line+arc's arc, 0 after an arc+line) for at most 15 degrees (`motionConfig.extrapTurn`),
   then straight. The body follows the same path back to the sample. Then it holds. Once the
   horizon is past the heartbeat with no newer sample, the data stopped (an instance went
   down without a gone, or the data ends there), and the head goes back to the last sample. A
   train whose last two chord speeds show braking decelerates and stops (at most 40
   tiles/s²), so a lost stop sample costs a few tiles, not heartbeat x speed. Never towards a
   gone. Across a known gap (over 60 s) the mover holds when it is at the same place
   afterwards (a parked train: imported data has no parked heartbeats). When it is somewhere
   else afterwards (an instance down, a save reloaded, without a gone), it is hidden from a
   heartbeat after the last sample until the next one.
5. Body. The path is sampled every 0.5 tile back from the head along the same segment
   geometry. Carriage *i*'s bogies sit 2 tiles either side of its arc position. Its centre is
   their midpoint and its angle points from the rear bogie to the front one. Pitches come from
   captured carriage geometry when present, else 7 tiles. Where the history ends the path
   continues along the newest captured carriage geometry, else along the curvature of its last
   6 tiles (a journey that starts in a curve, at most a quarter turn, only between about 3
   degrees over the span and a 10-tile radius), else straight.
6. Handoffs. A gone followed by the same journey within 10 s is bridged with a straight blend
   in time, and the history is kept. In live mode an unresolved gone holds the mover until the
   handoff window has passed the data horizon.
7. End switches. Flagged (`es`), or inferred on imported data: the head jumped about the
   train's length and the orientation flipped, the step was faster than any train, or the new
   head lies behind the old one. The body behind the new head is the old body reversed, so
   carriages do not move. A train that reappears behind its last position at a Universal Edge
   and drives on towards it is an instance crossing, not an end switch. The importer adds the
   gone the export lacks there, so it is drawn as a handoff (see
   `internal/import/minimap/doc.go`).
8. Never snap. When new data changes where a drawn mover or any carriage would have been, the
   difference is blended out over 400 ms, or longer for a large difference so the blend adds
   at most 15 tiles/s to the mover's own motion (at most 2 s, corrections over 30 tiles snap).
   An overrun is a drawn head ahead of the new data, up to 150 tiles: it extrapolated through
   a stop or a slowdown no sample announced. When it lies on the true head's line of travel
   (within 1 tile), the drawn mover and its carriages stand where they were drawn until the
   true head has run that far along its own track or passes the point. The wait also ends
   when the true head turns away (2 tiles off its line), stands still, or after 6 s. What is
   left then blends out at 15 tiles/s. An overrun off the line does not wait, and blends out
   at 15 tiles/s however long that takes instead of snapping at 30 tiles.
9. Trimming. Live and history views drop samples older than 2 minutes behind the drawn time,
   but keep enough path for the body (train length + 40 tiles), across handoff bridges, plus
   one sample more so the oldest kept segment keeps its shape.
10. Redraws. The layer asks the engine for another frame only while a shown mover's drawn
    position still changes and time runs (live, or a history time that changed since the
    last frame). Held movers are not moving, so a paused history view draws once and stops.
    In live mode, when nothing moves, a timer wakes the engine when the delayed display time
    reaches the next sample that has already arrived (`MoverView.nextT`). The engine's live
    display time is now minus the largest `MapLayer.delay()` (this layer's delay).

## Loading (feed.ts)

- History: a seek loads the day index, the newest keyframe at or before the time and every
  window from it to the time plus a lookahead (API.md 3). Windows between the keyframe and
  the time cannot be skipped: a gone row in a skipped window would leave the mover drawn from
  its keyframe row. The lookahead is 60 s, or 2 s of real time at the playback speed (at
  most 30 min). Moving forward by less than an hour past the loaded range extends it instead
  of seeking again, so fast playback falls behind at worst but never clears the view and
  refetches. The range ends on window boundaries, so an extension does not fetch the last
  window again. Once the range reaches past the closed horizon, `live.bin` is refetched at
  most every `live_ms`.
- Trimming in history moves the start of the loaded range up to the kept history (drawn time
  minus 2 minutes), so seeking back past it is a real seek.
- Day indexes are cached across seeks: final days for good, open days for 60 s, failures for
  10 s. They are fetched with their own abort signal, which only `stop()` uses, so a
  superseded seek shares the in-flight index instead of caching it as missing.
- Live: the first payload and the resync after a reconnect (`hello`) do not count for the
  delay statistics, because their rows piled up while nobody was listening. While the event
  stream is down (or, without one, polls of `live.bin` fail) the data horizon stays where it
  broke, so the silence is not read as "still on its line" or as a missed heartbeat.

## Evaluating

`bench/trains` ([bench/README.md](../../../bench/README.md)) grades this module through
`bench.ts` on the illisis export or an imported database, and `bench/railbench` grades it
against the game's own carriage positions.

`eval/train_eval.mjs` is an older evaluator that reads research outputs not kept in this
repository (a `trains.jsonl` dump and a rail mask of the final chart). From `web/`:

```
node src/movers/eval/train_eval.mjs <trains.jsonl> <rail_mask.bin> [--surface nauvis] [--modes history,live] [--extrap-turn rad] [--extrap-turn-inside] [--json report.json]
```

It prints the leave-one-out error for the illisis viewer, linear and Hermite references and
this viewer, then draws every journey at 10 Hz in history mode (every row known) and in a live
model (rows arrive in 1 s batches plus 150 ms) for this viewer and the illisis baseline
(`eval/illisis.ts`, with a fixed 2 s delay in live mode).

## Benchmark

```
NODE_PATH=<node_modules with playwright-core> node src/movers/bench/run.mjs --trains 500 --seconds 5
```

Prints frames per second and the per-frame JS time of evaluating and drawing all trains.
