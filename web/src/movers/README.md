# Movers: trains, vehicles and players (WP8)

The movers layer draws every train, vehicle and player of the shown surface at the displayed
time. It implements PLAN.md §4.8 (trains on the rails) on top of the WP0 contracts: MVW3
windows, keyframes and live payloads (`docs/v3/FORMAT.md` 7.4) and live `movers` events
(`docs/v3/API.md` 5).

| File | Role |
|---|---|
| `track.ts` | Samples per journey; rail runs of rail path samples; segment rules (gaps, gones, handoffs, end switches); derived velocities; head position; body path; carriage placement |
| `railpath.ts` | The rail path model (docs/v3/RAIL-GEOMETRY.md 8): piece shapes and ends, the chain of a run, projection, carriage placement, the timing model |
| `segment.ts` | Geometry between two samples without a rail path: line+arc or arc+line where it fits, cubic Hermite otherwise; arc-length timing |
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

0. **Rail path samples.** A train sample with a speed (`F_RAIL`; PROTOCOL.md 3.1 `v`, `rp`,
   `ru`) comes from the v3 capture's rail path model (docs/v3/RAIL-GEOMETRY.md 8). The track
   groups such samples into runs (`railRuns`): a run begins at a sample with `ru`, after a
   sample of another kind, at an end switch, or where a listed piece does not continue the
   run's path; its chain (`railpath.ts`) is the curved pieces and ramps of its samples joined
   by straight lines, the exact rail centre line. Each sample's leading joint (its centre plus
   2 tiles along its own orientation) is projected onto the chain, in order and no further
   than its speeds allow, so a train that comes back past a place is not put on its later
   pass. Between two samples of a run the leading joint's rail distance follows the timing
   model (stand-then-accelerate from a stop, brake-then-stand into one, else a cubic Hermite
   in time with the captured speeds), the head is the midpoint of the joints at that rail
   distance and 4 behind it, and every carriage sits on the chain at its joint distances
   (`placeRailCarriages`; pitches within a quarter tile of 7 are taken as 7). Past the newest
   sample the head runs on along the chain at its speed, braking to a stop when the last two
   speeds fell, within the caps of step 4; ahead of the newest piece the chain is the straight
   it ended on. A stop sample links to the start however long the train stood; it and the start
   list the pieces under the whole train (those already on the chain are resends), so a parked
   train drawn from its stop sample alone (a keyframe, the live payload) is on its rails. Carriages
   are placed by vanilla joint distances (4 and 3, RAIL-GEOMETRY.md 8.2). A new run whose
   pieces reach back to the sample before it (a first sight after a resync, a missed piece)
   links that segment on its chain. Each sample is placed no further along than its speeds
   allow and, among equally near points, nearest where they put it; a run's first sample no
   earlier than its last listed piece (on loops and straights used both ways the path passes
   a place more than once). Trimming (step 9) makes the first kept rail sample a run start
   with the pieces of its chain under the body behind it, and the one older sample kept for
   its carriage geometry stays out of the run. Everything below applies to other samples
   (imported data, older captures) and to segments a run does not cover.
1. **Delay behind live.** In live mode the drawn time is `now - delay`. The delay is the
   larger of the p99 of measured arrival lags plus 0.5 s (PLAN §4.8) and the p95 of
   *readiness* plus 0.25 s, capped at 6 s. With rail path samples the delay is at least a
   prior until 32 readiness lags were observed, set at once when the first such sample
   arrives (`DelayEstimator.railPrior`): the capture's longest wait between samples of a
   moving train in real time (120 ticks, 60 below game speed 0.5, at the game's tick rate)
   plus 1.5 s, 3.5 s at speed 1 and the 6 s cap at speed 0.2. Without it a viewer that had
   only arrival lags (and the readiness of samples a few ticks apart at a load) ran trains
   off the rails for their first seconds. Readiness is, for a moving mover, the time from a
   sample until the next one has arrived: past it the segment can be interpolated instead of
   extrapolated. A rail path stop sample is not counted (the rail path holds the train there
   until it starts; with a few samples a train-km, the station stops alone were more than 5%
   of the lags and raised the delay on the benchmark map from 3.2 s to 4.5 s at p50). The plan's rule alone extrapolates often on the test data, because MVW3
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
   history mode.) Trains get rail-shaped paths over gaps of 30 tiles or more (a sample
   gap at speed, often at an instance crossing), where a cubic cuts corners by several
   tiles. An end heading counts as a straight when it lies within 2 degrees of one of the
   16 rail directions; the captured orientation is used for it when it lies within 1
   degree of one (and 15 degrees of the derived tangent, which bends towards a neighbour
   across a corner). Both ends straight and turning by 10 degrees or more: a line, an arc
   of radius 13 and a line, which keeps both straights straight. End tangents that make
   an S (both at least 4 degrees to the same side of the chord), where no line+arc fits:
   a line, arc, straight in a rail direction, arc (or arc, line, arc, line when the mover
   turned harder before the gap than after it), radius 13, falling back to 9, 20 and 30
   tiles, at most 1.2 times the chord. Otherwise, when line+arc would need a curve tighter
   than 3 tiles (or has no solution) and one end is straight: a line from that end and an
   arc of radius 13 to the other end point, accepting a heading kink of up to 15 degrees
   there (the captured heading leads a curve). On the test data, with the importer's
   edge-crossing gones, this took the carriage-frames more than 2 tiles off the rails
   outside handoffs from 3,507 to 1,830 (one S at the y = 768 crossing on nauvis held
   1,869 of them), moving carriage-frames over 1 tile from 0.040% to 0.018%, and
   leave-one-out p90 from 0.574 to about 0.56. Allowed on chords of 15 tiles or more, the
   S shape made leave-one-out p90 worse (0.574 -> 0.579).
4. **Extrapolation.** Past the newest sample the head runs on along its velocity. Inside
   the data horizon (every sample older than `now - p99 lag` has arrived) silence means the
   mover is still on its predicted line, up to the capture heartbeat (11 s), so it runs
   straight: the capture emits on about 2 degrees of turn. Past the horizon (at most 1 s)
   it turns at the curvature it was in when the last sample was taken (the end of the
   segment before it: a line+arc's arc, 0 after an arc+line) for at most 15 degrees
   (`motionConfig.extrapTurn`), then straight; the body follows the same path back to the
   sample. Then it holds. Once the horizon is past the
   heartbeat with no newer sample, the data stopped (an instance went down without a gone,
   or the data ends there): the head goes back to the last sample instead of standing a
   heartbeat's run away from it until data resumes, minutes later. A train whose last two
   chord speeds show braking decelerates and stops (at most 40 tiles/s²) instead of running
   on at full speed, so a lost stop sample costs a few tiles, not heartbeat x speed. Never
   towards a gone. Across a known gap (over 60 s) the mover holds when it is at the same
   place afterwards (a parked train: the imported data has no parked heartbeats); when it
   is somewhere else afterwards (an instance down, a save reloaded, without a gone), it is
   hidden from a heartbeat after the last sample until the next one, since where it was
   meanwhile is unknown, instead of standing still for minutes and then jumping.
5. **Body.** The path is sampled every 0.5 tile back from the head along the same segment
   geometry. Carriage *i*'s bogies sit 2 tiles either side of its arc position; its centre
   is their midpoint and its angle points from the rear bogie to the front one. Pitches come
   from captured carriage geometry when present, else 7 tiles. Where the history ends the
   path continues along the newest captured carriage geometry, else along the curvature of
   its last 6 tiles (a journey that starts in a curve; at most a quarter turn, only between
   about 3 degrees over the span and a 10-tile radius), else straight.
6. **Handoffs.** A gone followed by the same journey within 10 s is bridged with a straight
   blend in time, and the history is kept. In live mode an unresolved gone holds the mover
   until the handoff window has passed the data horizon.
7. **End switches.** Flagged (`es`), or inferred on imported data: the head jumped about
   the train's length and the orientation flipped, the step was faster than any train, or
   the new head lies behind the old one. The body behind the new head is the old body
   reversed, so carriages do not move. (A train that reappears behind its last position
   at a Universal Edge and drives on towards it is an instance crossing, not an end
   switch; the importer adds the gone the export lacks there, so it is drawn as a
   handoff: see `internal/import/minimap/doc.go`.)
8. **Never snap.** When new data changes where a drawn mover or any carriage would have
   been, the difference is blended out over 400 ms, or longer for a large difference so the
   blend adds at most 15 tiles/s to the mover's own motion (at most 2 s; corrections over 30
   tiles snap). An *overrun* is a drawn head ahead of the new data, up to 150 tiles: it
   extrapolated through a stop or a slowdown no sample announced. When it lies on the true
   head's line of travel (within 1 tile), the drawn mover and its carriages stand where they
   were drawn, on the track the true head is about to use, until the true head has run that
   far along its own track (measured along its direction of travel frame by frame, not
   along a fixed vector) or passes the point. The wait also ends when the true head turns
   away (2 tiles off its line: a curve or junction before the drawn point), stands still,
   or after 6 s; what is left then blends out at 15 tiles/s (back along the line, which is
   the track for an overrun on it), unless it is no more than the true head ran that frame.
   An overrun off the line (it ran on past a curve the true head took) does not wait, and
   blends out at 15 tiles/s however long that takes instead of snapping at 30 tiles.
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
4. **Overruns wait on the track instead of blending back** (step 8): an overrun on the
   true head's line freezes until the true head reaches it; one off the line blends out at
   15 tiles/s instead of snapping. Extrapolation past the data horizon follows the current
   curve for up to 15 degrees (step 4). Live jumps 93 -> 13.

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
  the delay statistics: their rows piled up while nobody was listening. While the event
  stream is down (or, without one, polls of `live.bin` fail) the data horizon stays where
  it broke, so the silence is not read as "still on its line" or as a missed heartbeat.

## Evaluating

From `web/`, with the research outputs as data (never run from inside the data folder):

```
node src/movers/eval/train_eval.mjs <out/trains.jsonl> <rail_mask_nauvis.bin> [--surface nauvis] [--modes history,live] [--extrap-turn rad] [--extrap-turn-inside] [--json report.json]
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
| Moving carriage-frames > 1 tile | 3.46% | 2.23% | 3.09% (3.07%) |
| Same where the mask covers the track (95.4%) | 0.66% | 0.18% | 0.18% (0.19%) |
| p99 distance, all / where covered | 4.01 / 0.01 | 3.01 / 0.01 | 3.01 / 0.01 |
| Carriage-frames > 2 tiles outside handoffs, where covered | 2,979,777 | 1,115,738 | 1,145,185 (1,179,076) |
| Moving carriage-frames > 2 tiles outside handoffs | 636,541 | 667,961 | 592,086 (590,223) |
| Frame-to-frame jumps > v dt + 2 tiles (head / carriage) | 1730 / 1760 | 5 / 5 | 13 / 13 (93 / 93) |

- The remaining history-mode jumps are rows stamped 9-19 ms apart by arrival time (implied
  speeds over 200 tiles/s). Captured ticks (PLAN §4.1) remove them.
- Of the 93 wave-1 live jumps, 80 were snaps back after the head extrapolated through a
  stop that no sample announced (typically 1.5 s at 30-50 tiles/s, then the next sample
  3 s later only about 12 tiles on: station entries, often on a curve), 6 were missed
  heartbeats (the head ran on 11 s and stood 250-500 tiles away for minutes until data
  resumed) and 4 were same-millisecond rows. Overruns now wait on the track or blend out
  (step 8). Left: 6 returns to the last sample after a missed heartbeat (now 11 s after the
  data stopped instead of minutes later; not blended), 3 small (3-5 tiles) stops just
  before a gone and 4 same-millisecond rows.
- How the overrun and extrapolation rules were chosen (WP8 live model / WP11 evaluator at
  lookahead 0 on the first 10 journeys: jumps outside handoffs, moving carriage-frames off
  the rails):

  | Variant | WP8 live jumps | WP8 live moving > 1 tile | WP11 jumps | WP11 moving off-rail (p99) |
  |---|---:|---:|---:|---:|
  | wave 1 (blend back, straight extrapolation) | 93 | 3.07% | 1202 | 5.13% (6.44) |
  | first fix round: wait as an offset that progress along a fixed vector used up, curve inside the horizon | 15 | 3.14% | 1098 | 9.91% (9.26) |
  | the same without the overrun wait | not run | not run | 1200 | 9.79% (8.95) |
  | the same with straight extrapolation | 22 | 3.10% | 1105 | 5.24% (6.59) |
  | curve only past the horizon, from the segment's end curvature | 22 | 3.10% | 1105 | 5.24% (6.59) |
  | + frozen wait on the line (within 1 tile), off-line overruns snap | 65 | 3.07% | 1159 | 5.15% (6.45) |
  | + off-line overruns blend at 15 tiles/s (now) | 13 | 3.09% | 1071 | 5.28% (6.68) |

  The first fix round's off-rail regression came from turning inside the horizon (trains
  turned on past a curve's exit), not from the wait. Widening the line tolerance to 3 tiles
  changed nothing measurable. Moving off the rails at lookahead 0 stays above illisis: on
  all 32 journeys v3 has 5.88% (p99 8.61), wave 1 5.70% (7.90), the first fix round 10.58%
  (11.50), illisis 3.44% (7.51); jumps outside handoffs 3,511, 4,004, 3,566 and 40,694.
  Lookahead 0 extrapolates every frame; the viewer's live delay makes that rare.
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
  for illisis, moving carriage-frames off the rails 3.13% against 3.37%; leave-one-out p90
  0.59 against 1.42 (curves 0.57 against 1.53). Lookahead 0 is in the table above.
- Before wave 1's last round (first WP8 version) the same runs gave: leave-one-out p90
  0.60, history jumps 61, live jumps 1041, live carriage-frames > 1 tile 3.50% (0.55% where
  covered).

## Benchmark

```
NODE_PATH=<node_modules with playwright-core> node src/movers/bench/run.mjs --trains 500 --seconds 5
```

Prints frames per second and the per-frame JS time of evaluating and drawing all trains.
