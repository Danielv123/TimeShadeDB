# TimeShadeDB v3 benchmarks and acceptance (WP11)

This directory holds the tools that check the numeric targets of the v3 plan
(`research/PLAN.md` section 5) and the baselines measured on today's systems:
TimeShadeDB `master` and the illisis/minimap viewer and export.

One command runs everything and writes [REPORT.md](REPORT.md):

```sh
# v3 acceptance (needs WP2-WP8 landed): imports the test data, then measures it.
TSDB_TESTDATA=/data/minimap TSDB_STREAM=/data/stream60 bench/run_acceptance.sh

# Wave 1 baselines: TimeShadeDB master, the illisis export and the illisis viewer.
TSDB_MASTER_BIN=/tmp/tsdb-master TSDB_TESTDATA=... TSDB_STREAM=... bench/run_acceptance.sh -mode baseline
```

On Windows use `bench\run_acceptance.ps1` with the same arguments. Both wrappers run
`go run ./bench/cmd/acceptance`; its flags are listed by `-h`. Useful ones:
`-work <dir>` (scratch: binaries, databases, logs), `-only a,b` / `-skip a,b` (steps),
`-soak 6h`, `-attack-n`, `-web-runs`, `-strict` (exit 1 unless every target passes).
A failing step is logged under `<work>/logs/` and the report shows its targets as
NOT RUN.

## Inputs

| Input | How to get it |
|---|---|
| `TSDB_TESTDATA` | The illisis S3 export (the directory holding `maps.json.gz`). It is untrusted data: the tools only parse it and never run its bundled viewer. |
| `TSDB_STREAM` | `python -I research/scripts/to_stream.py $TSDB_TESTDATA <dir> --coalesce 60000 --no-jsonl`: chunk TSV files at 60 s windows, the same net changes the v3 format stores. Used for v2 ingest, the disk baseline and the 10x database. |
| `TSDB_MASTER_BIN` | TimeShadeDB master built without its viewer: `git archive master \| tar -x -C src`, create `src/web/dist/index.html`, `go build ./cmd/timeshadedb`. |
| `ILLISIS_REPO` | The illisis-minimap checkout the baseline viewer and train renderer are built from (default: next to this repository or its worktree directory). |
| `TSDB_IMPORT_ARGS` | Arguments of the import step (default `import-minimap --data {data} --db {db}`; WP5 owns the real flags). |
| `TSDB_BENCH_CHROME` | Chromium for the client benchmark (default: the newest `ms-playwright/chromium-*`, else the installed Chrome). |

## Tools

All Go tools share `-target v2|v3`, `-label`, `-scenario`, `-out <result.json>`, and either
`-base <url>` (an already running server; no memory numbers) or `-server-bin <binary>`
with `-db`/`-work`. A started server always listens on `127.0.0.1` on a random port
(never 8880, 4443 or 8080) and is killed when the tool ends. v3 servers are started as
`serve-v3 --db {db} --listen {listen}` and must print `listening on <addr>` first;
v2 as `serve --db {db} --addr {addr}`.

| Tool | Measures |
|---|---|
| `cmd/ingestbench` | Chunk updates/s (TSV rows for v2, PROTOCOL.md envelopes for v3) and mover samples/s, the rate per tenth of the stream (`chunk_flatness` = last tenth / second tenth), server RSS while ingesting, database size and B/chunk-hour, startup on the result. `-input` replays to_stream TSV files, otherwise the synthetic workload (`-synth N`, `-movers N`). |
| `cmd/tilebench` | Server tile latency: z0 latest cold (after a restart) and warm, z0 at random historical times (one new time per round, so caches cannot help), coarse levels `-levels`. v3 tiles are listed with the `view` route and fetched by version. Also startup. |
| `cmd/attack` | Random hostile tile requests (random levels and coordinates, unknown datastores, missing versions): RSS growth per million and latency. Stops at `-max-rss-mib` or `-max-duration` and extrapolates (noted in the result). |
| `cmd/soak` | Replays a workload at `-speed` (100x) for `-duration` while sampling RSS: maximum, growth from the end of the first sixth to the end ("hour 1 to hour 6"), slope. Reads a few tiles a second like a viewer. |
| `cmd/measure` | `run`: wall time and peak RSS of a command (the import). `v3disk`: v3 database size by category, B/chunk-hour (z0, and terrain with the pyramid) and mover B/sample. `illisis`: the same numbers for the illisis export. |
| `cmd/report` | Builds REPORT.md from `targets.json` and every result file under `bench/baselines` and `bench/results`. |
| `cmd/acceptance` | Runs all of the above in order (the wrappers call it). |
| `trains/` | Train evaluator (TypeScript, bundled with esbuild): leave-one-out error, carriage distance to rail pixels at 10 Hz over all train time, frame-to-frame jumps. Renderers: `illisis` (the illisis viewer's own `Tracks` class, bundled from its repo), `linear` and `hermite` (head-only references), `v3` (WP8). |
| `web/` | Client benchmark (Playwright, headless Chromium, 1920x1080, 40 ms RTT, 50 Mbit/s): cold and warm first full paint, zoom out to the whole surface, -5 min scrub steps, 10 s timelapse at 1 h/s, memory. Viewers: `illisis` (built from its repo source with a small bench entry) and `v3`. |

Results are JSON files (`bench/internal/benchlib/result.go`): tool, label, target,
scenario, machine (CPU, OS), parameters, metrics, notes. `targets.json` maps each target
to a metric, a tool and a scenario.

## Workloads

- **testdata**: the illisis export. Disk and tile numbers use nauvis on the gridworld map
  (7,483 charted chunks over 169.1 h is the chunk-hour denominator). **testdata-all** is
  every map and surface.
- **synth**: `benchlib.Synth`, modelled on the test data: 8,000 charted chunks, 97% of the
  updates in 0.4% of the chunks (Zipf inside that hot set; small groups of pixels toggling
  to enemy red and back), full chunk states as a capture sends them, and 25 trains on
  closed loops sampled every 300 ms.
- **synth10x**: ten time-shifted replays of the testdata stream in one database (startup on
  a 10x history).
- **soak**: the synthetic workload at 100x (v3: with trains).
- **attack**: random tile URLs against the testdata database.

## Interfaces the v3 packages must provide

- **WP6** `serve-v3`: `--db`, `--listen 127.0.0.1:0` printing `listening on <addr>`,
  `/healthz`, the API.md routes, `--ingest-token` (the tools send `-token` as a bearer
  token).
- **WP5** the import command (see `TSDB_IMPORT_ARGS`).
- **WP7** `window.__tsdbPerf` (docs/v3/README.md) and `window.__tsdbControl`
  (`TsdbControl` in `web/src/api/types.ts`); the query parameters `ds`, `x`, `y`, `z`, `t`.
- **WP8** `web/src/movers/bench.ts` exporting `createTrainRenderer(): TrainRenderer`
  (`bench/trains/src/types.ts`): a session gets samples in time order (`feed`) and returns
  the drawn head and carriage centres at a time (`pose`).
- **WP9** a result file with `capture.ms_per_tick` (tool `capture`, scenario `ups`) from
  the headless benchmark save, placed under `bench/results/v3/`.

## Caveats

- The illisis viewer and train code are bundled from the illisis-minimap repository at
  build time (`bench/web/dist`, `bench/trains/dist`, not committed); that code is under
  the Blue Oak Model License 1.0.0. Nothing from it is copied into this directory.

- Latency is measured with QueryPerformanceCounter on Windows: the Go clock there moves in
  0.5 ms steps, too coarse for the sub-millisecond targets.
- The client benchmark serves files over HTTP/1.1 from a local server with CDP network
  emulation. Production illisis is served over h2 by a CDN, so the illisis numbers here
  are for 6 connections per host, not for multiplexed requests.
- Headless Chromium does not cap `requestAnimationFrame` at the display rate, so the
  timelapse fps can exceed 60.
- The train evaluator reads movers from the illisis export. Once WP4 and WP6 serve v3
  movers, it should read the imported (re-timed) data instead; until then the v3 renderer
  is measured on the illisis samples.
