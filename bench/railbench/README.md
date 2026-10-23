# railbench: trains on real maps against the game's own positions

railbench runs a real save headless with the capture and a test-only recorder in the same
game, feeds the capture's envelopes through `serve`, and compares what the viewer
(`web/src/movers`, run headless in Node) draws from the stored rows with where the game
really put every carriage on every tick. It also checks the rail geometry model of
[docs/RAIL-GEOMETRY.md](../../docs/RAIL-GEOMETRY.md) against the recording and replays
sampling rules on it.

Nothing here is part of the shipped capture. The recorder mod is for benchmarks only.

## Requirements

- Factorio 2.x (`FACTORIO_EXE`, default the Steam path). Every run gets
  its own write-data, mods directory and mod list under `<work>/runs/<name>`, so the real
  Factorio config, mods and saves are never touched. Loads use `--benchmark` (as fast as the
  machine allows, no network port).
- Node 22 or newer (TypeScript is loaded with the type stripping `web/src/movers/eval` uses).
- `timeshadedb`, `measure` (`bench/cmd/measure`) and `raildisk` (`bench/cmd/raildisk`) built
  into `<work>/bin`:

```sh
go build -o $WORK/bin/timeshadedb.exe ./cmd/timeshadedb
go build -o $WORK/bin/measure.exe ./bench/cmd/measure
go build -o $WORK/bin/raildisk.exe ./bench/cmd/raildisk
```

`<work>` is `--work <dir>` or `RAILBENCH_WORK`. Set one of them: the default,
`G:/project_files/timeshade-v3-work/railbench`, is a path on the author's machine. Saves are
read from where they are given and never written, but use copies anyway.

## Steps

```sh
R=bench/railbench
node $R/railbench.mjs survey <save.zip>... [--ticks 18000]      # trains, moving trains, carriages, rails by type
node $R/railbench.mjs record <save.zip> --name <run> [--ticks 36000] [--refuel] [--no-capture]
node $R/railbench.mjs feed --name <run> [--speed 1] [--db <name>] # capture envelopes -> serve -> <run>/db
$WORK/bin/raildisk.exe -db $WORK/runs/<run>/db -out $WORK/runs/<run>/disk.json
node $R/eval.mjs --run <run> [--db <name>] [--modes history,live,seek,open] [--stopstart-ticks 120]
    [--live-estimator shared|journey] [--stopped-every 10] [--session-ms 60000] [--body server|none]
$WORK/bin/measure.exe seekcheck -db $WORK/runs/<run>/db -surface <surface dir> [-every 60000] [-out <file>]
node $R/simulate.mjs --run <run> [--reads capture|accel|shipped] [--sed 1] [--rules ...] [--thin] [--max-gap N]
node $R/geometry.mjs --run <run> [--run <run2>...] [--every 10] [--train-every 30] [--out <file>]
node $R/railbench.mjs ups <save.zip> --name ups-<run> [--pairs 3] [--ticks 7200] [--affinity FF00] [--compare <capture dir>]
node $R/compare.mjs --row "<label>=<run dir>[,eval=<file>]"... [--out <file.md>]  # before/after tables
node $R/report.mjs --run <run>[:ups-<run>]... [--out <file.md>]                  # tables of one or more runs
node $R/gen-vectors.mjs --run <run>... [--out docs/rail-geometry-vectors.json]
node --test $R/railgeom.test.mjs
```

| Step | Writes | What it does |
|---|---|---|
| `survey` | `<work>/survey-<time>.json` | Loads each save with the recorder in survey mode: trains, automatic trains, trains that moved more than 100 tiles, train-km, carriages per train, train states, rail pieces by surface and type, ms per tick. |
| `record` | `<run>/run.json`, `write/script-output/railbench/gt-*.txt`, `write/script-output/timeshade/*.ndjson` | One game run with the shipped standalone capture mod (built by `capture/build.mjs`, patched only so it writes files in benchmark mode) and the recorder. `--refuel` tops up empty locomotives every 600 ticks. |
| `feed` | `<run>/db`, `feed.json` | Starts `serve --clock data` on a free loopback port, posts the envelopes in file order as `timeshadedb tail` does (gzip NDJSON, about one post per game second), with `recv_ms` = game time of the envelope's tick + 250 ms, then a trace 7 minutes later so every mover window closes and an `ended` envelope (the game stopped, so the server does not hold the map for the source), waits for the mover horizon and stops the server. The server runs with the default `--mem-budget` (`--mem-budget` overrides it); open windows beyond it are spilled to disk. Game time follows the envelope's `speed` (`--speed x` replays as if the game ran at x). |
| `raildisk` | `disk.json` | Mover window and keyframe files on disk (zstd), every file of each surface's mover directory (`disk_bytes`: windows, keyframes, day indexes, meta), rows, `car` rows, and the same windows re-encoded with train rows only and without the carriage column or the rail pieces. |
| `eval` | `eval.json`, `worst-*.json` | The viewer's `MoverView` fed with the stored rows (read back with `measure movers`, rail path rows with their speed and pieces), drawn against the recording in four modes: `history` (every row of the journey known, every tick), `live` (rows arrive in 1 s batches + 150 ms and are added with the viewer's own lag bookkeeping, `feed.ts` `LiveLags`; `DelayEstimator` delay; one view and one estimator for all trains of a surface, as in the viewer, or one per journey with `--live-estimator journey`), `seek` (every `--session-ms` of map time a seek to a keyframe there: each journey starts from its last row at or before it, as a keyframe holds it, then the rows after it; scored until the next session) and `open` (every `--session-ms` the live map is opened: the live payload, the last 60 s of rows and each live journey's last row, then live rows as they arrive). In `seek` and `open` the first row of each journey carries its body, the rail pieces under the train that the server puts in front of a keyframe row's or the live payload's first row's own (FORMAT.md 7.4, read with `measure movers -bodies`); `--body none` leaves them out. Moving frames are scored at every tick; standing trains, from their last row to the end of the recording, every `--stopped-every` ticks in a bucket of their own (`stopped`). Samples per train-hour and train-km by track kind; head and carriage errors; spacing; jumps; the live display delay (now minus the drawn time, once a second). |
| `seekcheck` | `-out` file | Checks the bodies the store serves for one surface against those `measure movers -bodies` computes from the closed windows: the stored keyframes, the live payload rebuilt from disk every `-every` ms, and the live payload from memory of a fresh store fed the surface's train rows again in time order (1 s batches, committing 15 s behind). Also the bytes of a keyframe at every `-every` ms with and without the bodies (zstd, as stored). |
| `simulate` | `simulate-*.json` | Replays sampling rules on the recording (the header of `simulate.mjs` lists them): the earlier chord rule (`current`), pose-only rail shapes (also timed with speeds, `shape-v`), the known rail path with and without speeds (`railpath-t`: the timing model of RAIL-GEOMETRY.md 8.3, `--thin` keeps every other read of a full buffer, `--max-gap` samples at least every N ticks), and map matching on the rail network. `--reads capture` (default) reads every 4 tiles rounded up, at most 120 ticks apart. `--reads shipped` is the capture's own schedule (5 tiles rounded down, sooner while accelerating, at most 30 ticks, and with `--max-gap` a read when the gap ends). Routes (the pieces under each recorded train) are built by `routes.mjs`. |
| `geometry` | `geometry.json` | Checks the rail model against the recording: joint residuals per rail type, the carriage centre's offset from the rail, the game's rail distance against speed, carriage placement from the leading joint and the rail path, headings. |
| `ups` | `ups-<run>/ups.json` | The save with and without the capture mod (recorder off), interleaved pairs, Factorio's `--benchmark-verbose` timings; the capture's Lua cost (`scriptUpdate + luaGarbageIncremental`) and whole-tick cost after the load resync, and during it. `--compare <dir>` also runs the capture built from another `capture/` directory (an older checkout's, e.g. `git archive <rev> capture`) in each pair. |
| `compare` | stdout or `--out` | Markdown tables of samples, storage, history and live accuracy for several runs, to compare a change. |
| `report` | stdout or `--out` | Markdown tables of every output of the given runs (`run`, `feed`, `disk`, `eval`, `simulate`, `geometry`, and `ups` after the colon). |
| `gen-vectors` | `docs/rail-geometry-vectors.json` | Test vectors from the model and from recorded carriages. |

## Ground truth format

The recorder writes text lines (`mods/railbench_recorder/control.lua` documents every
record): the rail pieces once (`RAIL`: unit, type, position, direction, layer, length, both
ends), then per tick a line per train that moves, stopped this tick or changed state: speed,
state, the rail under the end it moves towards, and every carriage's position (1/256 tile,
Factorio's own fixed point, so exact) and orientation (1/65536 turn), as differences from
the train's previous tick when it had one. 10 minutes of a map with 77 trains (10.5 M
carriage-ticks) take about 123 MB.

## Track kinds

Each recorded line is classified by the pieces near the leading carriage's centre whose
tangent lies within 20 degrees of its axis: `junction` when one of them has an end shared by
three or more pieces, else `curve`, `diagonal` (diagonal or half-diagonal straight) or
`straight`; `stopstart` when the train stood within `--stopstart-ticks` (default 120, 2 s);
`--stopstart-ticks 0` splits by the track alone.

## Notes

- Samples are matched to the recording by the journey's train id and the head position at the
  sample's tick (within 0.2 tile). A journey keeps its id across joins and splits; rows that
  match no train are counted as unmatched.
- The live model is the viewer's own clock code and lag bookkeeping (`clock.ts`,
  `feed.ts` `LiveLags`) with arrival in 1 s batches; it is not a browser, and it gets the
  rows from `measure movers`, not through the TypeScript wire decoder (which a shared fixture
  test covers).
- `survey` and `record` load 2.x saves with the Space Age mods that ship with the game enabled
  and 1.1 saves with base only; a save's own extra mods are not loaded (vanilla trains
  survive).
