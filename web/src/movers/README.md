# Movers: trains, vehicles and players (WP8)

The movers layer draws every train, vehicle and player of the shown surface at the displayed
time. It implements PLAN.md §4.8 (trains on the rails) on top of the WP0 contracts: MVW3
windows, keyframes and live payloads (`docs/v3/FORMAT.md` 7.4) and live `movers` events
(`docs/v3/API.md` 5).

| File | Role |
|---|---|
| `track.ts` | Samples per journey; segment rules (gaps, gones, handoffs, end switches); derived velocities; head position; body path; carriage placement |
| `segment.ts` | Geometry between two samples: line+arc or arc+line where it fits, cubic Hermite otherwise; arc-length timing |
| `hermite.ts` | Hermite basis, orientation helpers, arc-length polyline |
| `view.ts` | `MoverView`: tracks fed from windows and events, per-frame evaluation, never-snap corrections, trimming |
| `clock.ts` | `DelayEstimator` (adaptive live delay and data horizon), `DataClock` |
| `feed.ts` | `MoverFeed`: live (SSE or polled `live.bin`) and history (day index, keyframe, windows) loading with a generation counter |
| `draw.ts` | Train, vehicle and player drawing (ported from illisis, Blue Oak 1.0.0) |
| `layer.ts` | `MoverLayer`, the `MapLayer` the engine (WP7) draws |
| `eval/` | Train evaluator (leave-one-out, rendered distance to rails, jumps) and the illisis baseline port |
| `bench/` | Browser benchmark: 500 trains drawn per animation frame |

## How a train is drawn

1. **Delay behind live.** In live mode the drawn time is `now - delay`, where the delay is
   the p99 of measured arrival lags plus 0.5 s (clamped to 0.5-30 s, changing by at most 20%
   of elapsed time so the clock never jumps). Static exports start from 2 x `live_ms`.
2. **Velocities.** MVW3 stores no velocity. A sample's speed is the derivative of the
   quadratic through its neighbours (time-weighted chord speeds). Its direction averages two
   estimates: the tangent of the circle through the neighbouring samples, and the captured
   orientation turned forward by curvature x half a pitch (the orientation is the chord from
   the second carriage to the first, which lags the head's tangent on curves).
3. **Segments.** Between linked samples the head follows a line then an arc (or an arc then
   a line) when the end points and tangents allow it, since rails are lines and arcs and a
   sample pair often spans a long straight that ends inside a curve; otherwise a cubic
   Hermite. Motion along the geometry is a 1D Hermite of arc length with the end speeds.
   Geometry much longer than its chord is rejected as a loop.
4. **Extrapolation.** Past the newest sample the head runs on along its velocity. Inside
   the data horizon (every sample older than `now - p99 lag` has arrived) silence means the
   mover is still on its predicted line, up to the capture heartbeat (11 s); past the
   horizon at most 1 s. Then it holds. Never towards a gone; across a known gap it holds.
5. **Body.** The path is sampled every 0.5 tile back from the head along the same segment
   geometry. Carriage *i*'s bogies sit 2 tiles either side of its arc position; its centre
   is their midpoint and its angle points from the rear bogie to the front one. Pitches come
   from captured carriage geometry when present, else 7 tiles. Where the history ends the
   path continues along the newest captured carriage geometry, else straight.
6. **Handoffs.** A gone followed by the same journey within 10 s is bridged with a straight
   blend in time, and the history is kept. In live mode an unresolved gone holds the mover
   until the handoff window has passed the data horizon.
7. **End switches.** Flagged (`es`), or inferred on imported data: the head jumped about
   the train's length and the orientation flipped, the step was faster than any train, or
   the new head lies behind the old one. The body behind the new head is the old body
   reversed, so carriages do not move.
8. **Never snap.** When new data changes where a drawn mover or any carriage would have
   been, the difference is blended out over 400 ms (corrections over 30 tiles snap).

## Evaluating

From `web/`, with the research outputs as data (never run from inside the data folder):

```
node src/movers/eval/train_eval.mjs <out/trains.jsonl> <rail_mask_nauvis.bin> [--surface nauvis] [--json report.json]
```

`rail_mask_nauvis.bin` comes from `research/scripts/rail_mask.py <data_root> <map> <surface> <out>`
(the final chart's rail-coloured pixels). The evaluator prints the leave-one-out error for
the illisis viewer, linear and Hermite-with-captured-velocity references and the v3 variants,
then draws every journey at 10 Hz in history mode (every row known) and in a live model (rows
arrive in 1 s batches plus 150 ms) for both the v3 viewer and the illisis baseline.

## Benchmark

```
NODE_PATH=<node_modules with playwright-core> node src/movers/bench/run.mjs --trains 500 --seconds 5
```

Prints frames per second and the per-frame JS time of evaluating and drawing all trains.
