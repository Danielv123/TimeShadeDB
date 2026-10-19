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
| `bench.ts` | `createTrainRenderer()`: this module as a renderer for the WP11 train evaluator (`bench/trains`) |
| `eval/` | Train evaluator (leave-one-out, rendered distance to rails, jumps) and the illisis baseline port |
| `bench/` | Browser benchmark: 500 trains drawn per animation frame |

## How a train is drawn

1. **Delay behind live.** In live mode the drawn time is `now - delay`. The delay is the
   larger of the p99 of measured arrival lags plus 0.5 s (PLAN §4.8) and the p95 of
   *readiness* plus 0.25 s, capped at 6 s. Readiness is, for a moving mover, the time from a
   sample until the next one has arrived: past it the segment can be interpolated instead of
   extrapolated. The plan's rule alone extrapolates often on the test data, because MVW3
   stores no velocity, so the viewer cannot dead-reckon as well as the illisis capture's own
   emit rule assumes (see Evaluating). The delay is clamped to 0.5-30 s, drops only when the
   estimate falls 250 ms below it, grows by at most 20% of elapsed time (time slows to 0.8x)
   and shrinks by at most 5% (time runs at most 1.05x), so the clock never jumps and movers
   never visibly race. Static exports start from 2 x `live_ms`.
2. **Velocities.** MVW3 stores no velocity. A sample's speed is the derivative of the
   quadratic through its neighbours (time-weighted chord speeds). Its direction averages two
   estimates: the tangent of the circle through the neighbouring samples, and the captured
   orientation turned forward by curvature x half a pitch (the orientation is the chord from
   the second carriage to the first, which lags the head's tangent on curves).
3. **Segments.** Between linked samples the head follows a line then an arc (or an arc then
   a line) when the end points and tangents allow it, since rails are lines and arcs and a
   sample pair often spans a long straight that ends inside a curve; otherwise a geometric
   cubic Hermite: tangent lengths that make it a circular arc for a symmetric turn (the chord
   when straight), walked by arc length through a 16-step length table. Motion along the
   geometry is a 1D Hermite of arc length with the end speeds. Geometry much longer than its
   chord is rejected as a loop. (Tangents of v x dt put timing into the shape: a braking
   train's curve overshot its end and came back, and walking it by curve parameter drew the
   head at up to 4x its speed. That caused 56 of the 61 jumps the first version had in
   history mode.)
4. **Extrapolation.** Past the newest sample the head runs on along its velocity, turning
   at the curvature between the last two samples for at most 15 degrees
   (`motionConfig.extrapTurn`), then straight; the body follows the same arc back to the
   sample. Inside the data horizon (every sample older than `now - p99 lag` has arrived)
   silence means the mover is still on its predicted line, up to the capture heartbeat
   (11 s); past the horizon at most 1 s. Then it holds. Once the horizon is past the
   heartbeat with no newer sample, the data stopped (an instance went down without a gone,
   or the data ends there): the head goes back to the last sample instead of standing a
   heartbeat's run away from it until data resumes, minutes later. A train whose last two
   chord speeds show braking decelerates and stops (at most 40 tiles/s²) instead of running
   on at full speed, so a lost stop sample costs a few tiles, not heartbeat x speed. Never
   towards a gone; across a known gap it holds.
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
   been, the difference is blended out over 400 ms, or longer for a large difference so the
   blend adds at most 15 tiles/s to the mover's own motion (at most 2 s; corrections over 30
   tiles snap). An *overrun* (the drawn head is ahead of the new data within 60 degrees of
   the direction of travel, up to 150 tiles: it extrapolated through a stop or slowdown no
   sample announced) is instead used up by the mover's own forward progress: the drawn
   mover waits on its track until the true position catches up, then follows the data. It
   fades at 15 tiles/s at once when the true head stands still, or after 6 s.
9. **Trimming.** Live and history views drop samples older than 2 minutes behind the drawn
   time, but keep enough path for the body (train length + 40 tiles), across handoff
   bridges, plus one sample more so the oldest kept segment keeps its shape. (Stopping at
   the handoff turned parked trains' bodies straight off the rails after a minute.)
10. **Redraws.** The layer asks the engine for another frame only while a shown mover's
   drawn position still changes and time runs (live, or a history time that changed since
   the last frame). Held movers (parked across a gap, held before a gone, past the
   extrapolation cap) are not moving, so a paused history view draws once and stops. In
   live mode, when nothing moves, a timer wakes the engine when the delayed display time
   reaches the next sample that has already arrived (`MoverView.nextT`). The engine's live
   display time is now minus the largest `MapLayer.delay()` (this layer's delay).

## Departures from PLAN §4.8

Measured on the test data (see Results) and kept on purpose; they still need recording in
PLAN.md / STATUS.md, and WP9 should confirm the capture side of the first one.

1. **Extrapolation inside the data horizon runs to the heartbeat (11 s), not 1 s.** Past
   the horizon the cap is still 1 s; once the heartbeat is known missed the head goes back
   to the last sample. This assumes the capture's emit rule (PROTOCOL.md:
   chord error 0.25 tile, orientation about 2 degrees, 600-tick heartbeat) means silence
   implies the mover is on its predicted straight line. If WP9's rule predicts anything
   else (dead reckoning along a curve, or a longer heartbeat), `HEARTBEAT_MS` in
   `track.ts` must follow it. Braking trains decelerate to a stop instead.
2. **The live delay adds a readiness term**: max(p99 lag + 0.5 s, p95 readiness + 0.25 s
   capped at 6 s), about 2.4 s on the test data where the plan's rule gives about 1.7 s.
   It cuts live jumps from 233 to 93.
3. **Segments are line+arc (or arc+line) with a geometric Hermite fallback walked by arc
   length**, not a Hermite with v x dt tangents. Leave-one-out p90 0.57 against 0.60, and
   history jumps 5 against 61.
4. **Overruns wait instead of blending back** (step 8), and extrapolation follows the
   current curve for up to 15 degrees (step 4). Live jumps 93 -> 15.

## Loading (feed.ts)

- **History.** A seek loads the day index, the newest keyframe at or before the time and
  every window from it to the time plus a lookahead (API.md 3). Windows between the
  keyframe and the time cannot be skipped: a gone row in a skipped window would leave the
  mover drawn from its keyframe row. The lookahead is 60 s, or 2 s of real time at the
  playback speed (at most 30 min). Moving forward by less than an hour past the loaded
  range extends it contiguously instead of seeking again (a seek would load as much from
  its keyframe), so fast playback falls behind at worst but never clears the view and
  refetches. The range ends on window boundaries, so an extension does not fetch the last
  window again. Once the range reaches past the closed horizon, `live.bin` is refetched
  at most every `live_ms`.
- **Trimming in history** moves the start of the loaded range up to the kept history
  (drawn time minus 2 minutes), so seeking back past it is a real seek.
- **Day indexes** are cached across seeks: final days for good, open days for 60 s,
  failures for 10 s. They are fetched with their own abort signal, which only `stop()`
  uses, so a superseded seek shares the in-flight index instead of caching it as missing.
  There are no timers.
- **Live.** The first payload and the resync after a reconnect (`hello`) do not count for
  the delay statistics: their rows piled up while nobody was listening.

## Evaluating

From `web/`, with the research outputs as data (never run from inside the data folder):

```
node src/movers/eval/train_eval.mjs <out/trains.jsonl> <rail_mask_nauvis.bin> [--surface nauvis] [--modes history,live] [--extrap-turn rad] [--json report.json]
```

`rail_mask_nauvis.bin` comes from `research/scripts/rail_mask.py <data_root> <map> <surface> <out>`
(the final chart's rail-coloured pixels). The evaluator prints the leave-one-out error for
the illisis viewer, linear and Hermite-with-captured-velocity references and the v3 variants,
then draws every journey at 10 Hz in history mode (every row known) and in a live model (rows
arrive in 1 s batches plus 150 ms) for both the v3 viewer and the illisis baseline.

### Results on the test data

nauvis, 32 journeys, 1,126,800 rows (clamped rows dropped), measured 2026-10-19 after the
overrun and extrapolation changes (the wave-1 figures in brackets). The illisis baseline is
its viewer's placement ported to `eval/illisis.ts`, with a fixed 2 s delay in live mode.
Distances are to the nearest rail pixel of the final chart, in tiles.

| Metric | illisis | v3 history | v3 live |
|---|---:|---:|---:|
| Leave-one-out p50 / p90 / p99, all | 0.60 / 1.36 / 6.99 | 0.15 / 0.57 / 2.92 | |
| Leave-one-out p50 / p90 / p99, curves | 0.62 / 1.46 / 4.40 | 0.19 / 0.56 / 1.35 | |
| Carriage-frames > 1 tile (raw head samples: 3.46%) | 3.60% | 2.98% | 3.15% (3.15%) |
| Same where the mask covers the track (95.4%) | 0.66% | 0.18% | 0.18% (0.19%) |
| p99 distance, all / where covered | 4.01 / 0.01 | 3.01 / 0.01 | 3.01 / 0.01 |
| Carriage-frames > 2 tiles outside handoffs, where covered | 2,979,777 | 1,115,738 | 1,154,590 (1,179,076) |
| Frame-to-frame jumps > v dt + 2 tiles (head / carriage) | 1730 / 1760 | 5 / 5 | 15 / 15 (93 / 93) |

- The remaining history-mode jumps are rows stamped 9-19 ms apart by arrival time (implied
  speeds over 200 tiles/s). Captured ticks (PLAN §4.1) remove them.
- Of the 93 wave-1 live jumps, 80 were snaps back after the head extrapolated through a
  stop that no sample announced (typically 1.5 s at 30-50 tiles/s, then the next sample
  3 s later only about 12 tiles on: station entries, often on a curve), 6 were missed
  heartbeats (the head ran on 11 s and stood 250-500 tiles away for minutes until data
  resumed) and 4 were same-millisecond rows. Overruns now wait on the track (step 8) and
  extrapolation follows the curve (step 4). Left: 2 snaps after an overrun that was not
  along the track, 6 returns to the last sample after a missed heartbeat (now 11 s after
  the data stopped instead of minutes later), 3 small (3-5 tiles) stops just before a gone
  and 4 same-millisecond rows.
- The delay trades latency for snaps: in wave 1, without the readiness term (about 1.7 s
  delay) there were 233, with p90 (about 2.15 s) 140, with p95 (about 2.4 s, the default) 93.
- Most remaining "> 2 tiles where covered" carriage-frames are parked trains, weighted by
  the hours they stand. In the cases checked the body lies on the path the head itself drove
  (for example journey 0 at (-346.7, 8.1), parked for hours: the carriages lie on the
  x = -345 straight it arrived on), but the final chart has no rail pixels there. So the
  mask, not the drawing, is wrong at those spots, and this metric cannot reach zero; nor
  can p99 reach the plan's 1.5 tiles (the raw samples themselves have p99 4.01).
- The WP11 evaluator (`bench/trains`, `--renderer v3` through `bench.ts`) counts jumps
  without dropping clamped rows or excluding end switches and data gaps, so its numbers
  are higher: history (`--lookahead-ms 61000`) 1,451 jumps outside handoffs against 2,931
  for illisis; leave-one-out p90 0.59 against 1.42 (curves 0.57 against 1.53).
- Before wave 1's last round (first WP8 version) the same runs gave: leave-one-out p90
  0.60, history jumps 61, live jumps 1041, live carriage-frames > 1 tile 3.50% (0.55% where
  covered).

## Benchmark

```
NODE_PATH=<node_modules with playwright-core> node src/movers/bench/run.mjs --trains 500 --seconds 5
```

Prints frames per second and the per-frame JS time of evaluating and drawing all trains.
