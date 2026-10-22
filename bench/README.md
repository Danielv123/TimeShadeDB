# TimeShadeDB v3 benchmarks and acceptance (WP11)

This directory holds the tools that check the numeric targets of the v3 plan
(`research/PLAN.md` section 5) and the baselines measured on today's systems:
TimeShadeDB `master` and the illisis/minimap viewer and export.

One command runs everything and writes `bench/REPORT.md`. The report and the result
files under `bench/results` and `bench/baselines` are local output, ignored by git:

```sh
# v3 acceptance (needs WP2-WP8 landed): imports the test data, then measures it.
TSDB_TESTDATA=/data/minimap bench/run_acceptance.sh

# Wave 1 baselines: TimeShadeDB master, the illisis export and the illisis viewer.
TSDB_MASTER_BIN=/tmp/tsdb-master TSDB_TESTDATA=/data/minimap bench/run_acceptance.sh -mode baseline
```

On Windows use `bench\run_acceptance.ps1` with the same arguments. Both wrappers run
`go run ./bench/cmd/acceptance`; its flags are listed by `-h`. Useful ones:
`-work <dir>` (scratch: binaries, databases, logs), `-only a,b` / `-skip a,b` (steps),
`-soak 6h`, `-attack-n`, `-web-runs`, `-capture-runs`, `-capture-affinity FF00` (pin the
capture UPS benchmark to cores 4-7 on Windows), `-strict` (exit 1 unless every target
passes). Candidate steps: `build-tools`, `build-js`, `stream`, `build-server`, `import`,
`disk`, `tiles`, `ingest-synth`, `ingest-testdata`, `synth10x`, `attack`, `soak`, `trains`,
`web`, `capture`, `report`.
A failing step is logged under `<work>/logs/` and the report shows its targets as
NOT RUN.

## Inputs

| Input | How to get it |
|---|---|
| `TSDB_TESTDATA` | The illisis S3 export (the directory holding `maps.json.gz`). It is untrusted data: the tools only parse it and never run its bundled viewer. |
| `TSDB_STREAM` | Optional. Chunk TSV files at 60 s windows (`<map>.chunks.tsv`), the same net changes the v3 format stores; used for ingest over HTTP, the v2 disk baseline and the 10x database. When unset, the runner's `stream` step makes them under `-work` with `go run ./bench/cmd/measure stream -data $TSDB_TESTDATA -out <dir> -coalesce 60000` (about 2 s). That command is a Go port of the chunk output of the research script `to_stream.py` and writes byte-identical files on the test data. |
| `TSDB_MASTER_BIN` | TimeShadeDB master built without its viewer: `git archive master \| tar -x -C src`, create `src/web/dist/index.html`, `go build ./cmd/timeshadedb`. |
| `ILLISIS_REPO` | The illisis-minimap checkout the baseline viewer and train renderer are built from (default: next to this repository or its worktree directory). |
| `TSDB_IMPORT_ARGS` | Arguments of the import step (default `import-minimap --data {data} --db {db}`; WP5 owns the real flags). |
| `TSDB_BENCH_CHROME` | Chromium for the client benchmark (default: the newest `ms-playwright/chromium-*`, else the installed Chrome). |
| `FACTORIO_EXE` | Factorio 2.x for the capture UPS benchmark (default: the Steam path; see `capture/README.md`). |

## Tools

All Go tools share `-target v2|v3`, `-label`, `-scenario`, `-out <result.json>`, and either
`-base <url>` (an already running server; no memory numbers) or `-server-bin <binary>`
with `-db`/`-work`. A started server always listens on `127.0.0.1` on a random port
(never 8880, 4443 or 8080) and is killed when the tool ends, also when it ends on an
error (`benchlib.Fatal` stops every server first) or is interrupted. Each child is also
tied to the process that started it (a kill-on-close Job Object on Windows, `Pdeathsig`
on Linux), so a crashed or killed tool, or acceptance runner, leaves nothing running.
v3 servers are started as
`serve --db {db} --listen {listen}` and must print `listening on <addr>` first;
v2 as `serve --db {db} --addr {addr}`. The tools that replay recorded or synthetic data
into v3 (`ingestbench`, `soak`) add `--clock data` (`-data-clock=false` turns it off): on
the wall clock serve counts every replayed row as late, and on the data clock it never
moves past the wall clock plus 10 s, so replays must lie in the past. `soak` therefore
places its data to end at the present, and the 10x database is built from replays
shifted into the past, oldest first. `attack` adds `--pprof` on a free loopback port
(`-gc`, default on) to force a garbage collection before each memory reading.

| Tool | Measures |
|---|---|
| `cmd/ingestbench` | Chunk updates/s (TSV rows for v2, PROTOCOL.md envelopes for v3) and mover samples/s, the rate per tenth of the stream (`chunk_flatness` = last tenth / second tenth), server RSS while ingesting, database size and B/chunk-hour, startup on the result. `-input` replays chunk TSV files, otherwise the synthetic workload (`-synth N`, `-movers N`). A v3 server answers 200 even when it skips lines or rejects rows (API.md 5), so every answer's `IngestResponse` is checked: a body whose rows were not all stored stops the run (`-allow-rejected` records them instead, and rates then count stored rows). Envelopes are kept under 1 MiB per line and bodies under 16 MiB (the server caps are 4 MiB and 32 MiB). 503 answers are retried for at most `-max-retry-wait` (2 min) per request. |
| `cmd/tilebench` | Server tile latency: z0 latest cold (after a restart) and warm, z0 at random historical times (a new time per round; each tile URL, that is each (tile, version), is fetched once so caches cannot help), coarse levels `-levels` (the cold pass is repeated after restarts until it has `-min-cold` samples, 20 by default, because few tiles cover a coarse level). v3 tiles are listed with the `view` route and fetched by version. Also startup. |
| `cmd/attack` | Random hostile tile requests (random levels and coordinates, unknown datastores, missing versions): memory growth per million and latency. Memory is the median of 5 readings before and after, as private bytes, RSS, and the Go heap from `/metrics` when the server exports it. For v3 each reading follows a forced garbage collection (pprof `heap?gc=1`) and `-gc-settle` (30 s), and the live heap after it (`attack.live_heap_growth_mib_per_1m`, the primary memory target) is what the requests retained; without the forced collection the readings mostly show how far the collector let garbage grow. Stops at `-max-rss-mib` or `-max-duration`; the growth is extrapolated per million only from at least 100,000 requests, or when it is already far past the budget (both noted in the result). Before the baseline reading, `-warm-hostile` (200,000) hostile requests at the measured concurrency grow the connection goroutine stacks and runtime caches once (a one-time step of about 8 MiB of private bytes on Windows that further millions of requests do not repeat). |
| `cmd/soak` | Replays a workload at `-speed` (100x) for `-duration` while sampling RSS: maximum, growth from the end of the first sixth to the end ("hour 1 to hour 6"), slope. Reads a few tiles a second like a viewer. |
| `cmd/measure` | `run`: wall time and peak RSS of a command (the import). `v3disk`: v3 database size by category, B/chunk-hour (z0, and terrain with the pyramid) and mover B/sample, with the stored samples counted by decoding the closed mover windows; `disk.store_mib` leaves out `icons/` (catalog icons, not map history), as the illisis figure leaves out its icons. `illisis`: the same numbers for the illisis export. `stream`: the chunk TSV stream from the illisis export. `capture`: runs `capture/test/run_game.mjs --mode bench` (headless Factorio, WP9) and records the capture's steady-state cost per tick (Lua and whole tick, mean over the interleaved pairs). `movers`: the train journeys stored in a v3 database as JSON, for the train evaluator's `--journeys-json`. |
| `cmd/report` | Builds REPORT.md from `targets.json` and every result file under `bench/baselines` and `bench/results`. |
| `cmd/acceptance` | Runs all of the above in order (the wrappers call it). |
| `trains/` | Train evaluator (TypeScript, bundled with esbuild): leave-one-out error, carriage distance to rail pixels at 10 Hz over all train time, frame-to-frame jumps. Renderers: `illisis` (the illisis viewer's own `Tracks` class, bundled from its repo), `linear` and `hermite` (head-only references), `v3` (WP8). Input: the illisis movement windows (`--data`), or with `--journeys-json` the rows of a v3 database exported by `measure movers`; the candidate is graded on the imported database's rows (scenario `testdata`) and also run on the illisis windows (scenario `testdata-illisis-input`). Rail mask (`--mask`): `history` (default) takes every track colour (rail, elevated rail, rail ramp) in every snapshot and change record of the z0 chart history, `final` the final chart only. Renderers get the data horizon the evaluator has fed (pose time plus `--lookahead-ms`). A jump also compares the drawn body with the previous body reversed (an end switch drawn in place is not a jump). Diagnostics: `--jumps-out`, `--offrail-out` (JSON lines), `--probe <journey> --probe-from --probe-to` (frames printed), `--shard k/n`. |
| `web/` | Client benchmark (Playwright, headless Chromium, 1920x1080, 40 ms RTT, 50 Mbit/s): cold and warm first full paint, zoom out to the whole surface, -5 min scrub steps, 10 s timelapse at 1 h/s, memory. For v3 the scrub also checks "bytes <= the changed tiles": tile URLs carry the tile version, so a step that downloads a tile URL the page has already downloaded fetched an unchanged tile again (`web.scrub_unchanged_tile_bytes`, target 0). The timelapse also records the page's own `requestAnimationFrame` rate (`web.raf_hz`) and the share of those frames the viewer drew (`web.timelapse_frame_share`). Viewers: `illisis` (built from its repo source with a small bench entry) and `v3`. |

Results are JSON files (`bench/internal/benchlib/result.go`): tool, label, target,
scenario, machine (CPU, OS), parameters, metrics, notes. Local paths in parameters and
notes are written relative to `$TSDB_TESTDATA`, `$TSDB_STREAM`, `$TSDB_WORK`, `$TMP` or
`~`, or as their last element, so result files can be shared between machines.

`targets.json` maps each target to a metric, a tool and a scenario. `tool` may list
alternatives, such as `"measure-v3disk|measure-illisis|ingestbench"`: the first listed
tool that has the metric for a label wins (the newest result within one tool). That is
how the baselines read the disk numbers from their own tools while v3 is graded on the
imported database. `scenario` may list alternatives the same way. The v3 run that
re-ingests the stream over HTTP uses its own scenario, `testdata-stream`, so its database
never stands in for the imported one.

## Workloads

- **testdata**: the illisis export. Disk and tile numbers use nauvis on the gridworld map
  (7,483 charted chunks over 169.1 h is the chunk-hour denominator). **testdata-all** is
  every map and surface. For v2 these numbers come from ingesting the 60 s stream; for v3
  they come from the imported database.
- **testdata-stream** (v3): the 60 s nauvis stream ingested over HTTP into a fresh
  database (ingest memory).
- **synth**: `benchlib.Synth`, modelled on the test data: 8,000 charted chunks, 97% of the
  updates in 0.4% of the chunks (Zipf inside that hot set; small groups of pixels toggling
  to enemy red and back), full chunk states as a capture sends them, and 25 trains on
  closed loops sampled every 300 ms.
- **synth10x**: for v3, ten copies of the imported database under suffixed map keys
  (`measure replicate`: ten times its datastores, tiles, movers, overlays and bytes,
  184 MiB), started three times (`measure startup`). For v2 (baseline), ten time-shifted
  replays of the testdata stream in one database.
- **soak**: the synthetic workload at 100x (v3: with trains).
- **attack**: random tile URLs against the testdata database.

## Interfaces the v3 packages must provide

- **WP6** `serve`: `--db`, `--listen 127.0.0.1:0` printing `listening on <addr>`,
  `/healthz`, the API.md routes, `--ingest-token` (the tools send `-token` as a bearer
  token).
- **WP5** the import command (see `TSDB_IMPORT_ARGS`).
- **WP7** `window.__tsdbPerf` (docs/v3/README.md) and `window.__tsdbControl`
  (`TsdbControl` in `web/src/api/types.ts`); the query parameters `ds`, `x`, `y`, `z`, `t`.
  The client benchmark opens the v3 viewer at `ds=gw-bc41437d~2E1.nauvis.player`, x 0,
  y 0, z 0, the same view as the illisis run, and checks `view()` and `datastore()` before
  measuring (an error when they differ, a note when the viewer cannot report them).
- **WP8** `web/src/movers/bench.ts` exporting `createTrainRenderer(): TrainRenderer`
  (`bench/trains/src/types.ts`): a session gets samples in time order (`feed`) and returns
  the drawn head and carriage centres at a time (`pose`).
- **WP9** `capture/test/run_game.mjs --mode bench` printing its summary lines (`bench pair
  N: capture Lua cost ...`, `bench: capture costs ... (..., mean), ... of the whole tick`);
  the `capture` step parses them into `capture.ms_per_tick` and
  `capture.whole_ms_per_tick` (tool `capture`, scenario `ups`).

## Caveats

- The illisis viewer and train code are bundled from the illisis-minimap repository at
  build time (`bench/web/dist`, `bench/trains/dist`, not committed); that code is under
  the Blue Oak Model License 1.0.0. Nothing from it is copied into this directory.

- Latency is measured with QueryPerformanceCounter on Windows: the Go clock there moves in
  0.5 ms steps, too coarse for the sub-millisecond targets.
- The client benchmark serves files over HTTP/1.1 from a local server with CDP network
  emulation. Production illisis is served over h2 by a CDN, so the illisis numbers here
  are for 6 connections per host, not for multiplexed requests.
- Headless Chromium paces `requestAnimationFrame` differently per page: the illisis page
  (no WebGL) ran it at about 130 Hz, the v3 page (a WebGL canvas, presented through
  ANGLE/D3D11) at about 55.4 Hz on the owner's machine, which bounds the v3 timelapse fps
  (`web.raf_hz`).
- The train evaluator grades v3 on the imported database's rows, while the baselines
  ran on the illisis windows, which still hold the illisis store's clamp re-timing. The
  rail mask is the same for all (`--mask history`).
