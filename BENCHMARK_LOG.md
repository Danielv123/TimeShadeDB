# Benchmark Log

| Time | Change | Rows | Seconds | Rows/sec | Command |
| --- | --- | ---: | ---: | ---: | --- |
| 2026-05-26T11:07:18.2757599+02:00 | Coalesce batch import delta frames | 1000000 | 16.1680245 | 61850.48 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-coalesced.tshd --max-rows 1000000` |
| 2026-05-26T11:11:42.4168812+02:00 | Reuse zstd encoders for snapshots and deltas | 1000000 | 6.3623265 | 157175.21 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-reuse-enc.tshd --max-rows 1000000` |
| 2026-05-26T11:15:34.5737074+02:00 | Simplify import pipeline to ordered parse plus tile workers | 1000000 | 4.5258482 | 220953.06 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-simple-pipeline.tshd --max-rows 1000000` |
| 2026-05-26T11:18:47.9342128+02:00 | Use fastest zstd level for batch import frames | 1000000 | 2.5005987 | 399904.23 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-fast-batch-zstd.tshd --max-rows 1000000` |
| 2026-05-26T11:21:34.0252306+02:00 | Add fixed UTC timestamp parse fast path | 1000000 | 2.3194512 | 431136.47 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-fast-ts.tshd --max-rows 1000000` |
| 2026-05-26T11:43:31.6560201+02:00 | Add fixed coordinate and RGB parse fast paths | 5000000 | 9.7053860 | 515177.86 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample5m-fast-coord-2.tshd --max-rows 5000000` |
| 2026-05-26T11:49:48.5684279+02:00 | Use pgzip reader for faster gzip decompression | 5000000 | 4.2533366 | 1175547.69 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample5m-pgzip.tshd --max-rows 5000000` |
| 2026-05-26T11:53:38.5645338+02:00 | Increase pgzip read-ahead block size to 1 MiB | 5000000 | 3.9621918 | 1261927.80 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample5m-pgzip-1mb-2.tshd --max-rows 5000000` |
| 2026-05-26T11:58:15.5413137+02:00 | Current sustained import throughput check | 20000000 | 13.4056739 | 1491905.60 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample20m-current.tshd --max-rows 20000000` |

## Factorio chunk ingest benchmark

Measured `serve` plus `tail-chunk-tsv` against the local `chunk-charted.tsv` Factorio mod output. Runs reset the local `factorio*` benchmark database directories first. Capped runs used `WaitForExit(...)` and killed the tail client and server after the timeout; rows are metadata-confirmed when possible, otherwise they are the tail client's last completed progress report.

| Time | Change | Batch rows | Timeout sec | Rows accepted | Chunks/min | Command |
| --- | --- | ---: | ---: | ---: | ---: | --- |
| 2026-05-26T20:45:00+02:00 | User-observed pre-optimization baseline | default | n/a | n/a | 1400.0 | `.\timeshadedb.exe serve --dev --db factorio` + `.\timeshadedb.exe tail-chunk-tsv --input .\chunk-charted.tsv --savefile speedrun --once` |
| 2026-05-26T21:02:00+02:00 | Batched HTTP ingest and grouped chunk-file writes | 4096 | 60 | 69632 | 69610.1 | `.\timeshadedb.exe tail-chunk-tsv --input .\chunk-charted.tsv --savefile speedrun --base-url http://127.0.0.1:18082 --once --progress-interval 5s --batch-rows 4096` |
| 2026-05-26T21:09:00+02:00 | Remove chunk hot-path fsync and cache datastore metadata writes | 4096 | 60 | 94208 | 94185.7 | same as above |
| 2026-05-26T21:23:00+02:00 | Skip no-op repeat frames while persisting per-datastore ingest progress | 16384 | 60 | 114688 | 119566.3 | `.\timeshadedb.exe tail-chunk-tsv --input .\chunk-charted.tsv --savefile speedrun --base-url http://127.0.0.1:18082 --once --progress-interval 5s` |
| 2026-05-26T21:32:00+02:00 | Tune default batch size and avoid client-side RGB565 decode on fresh sends | 10240 | 60 | 122880 | 122867.2 | same as above |
| 2026-05-26T21:40:00+02:00 | Final exact consecutive-repeat shortcut check | 10240 | 60 | 122880 | 122847.4 | same as above |
| 2026-05-26T21:50:00+02:00 | Cache exact encoded payloads per datastore/chunk within each batch | 10240 | 60 | 143360 | 143324.3 | same as above |

Best sustained 60-second result so far is 143,324 chunks/min, about 102.4x the 1,400 chunks/min baseline. Shorter 30-second tuning runs reached 131,042 chunks/min at 8,192-row batches and 143,307 chunks/min at 10,240-row batches before the final per-batch exact payload cache.

## Tile API benchmark

Measured `serve` over the actual HTTP tile API against `full.tshd` with a 64 MiB decoded snapshot cache. The request generator sampled tile coordinates weighted by each tile's `changed_placements_stored` in `full.tshd/stats.json`, then sampled timestamps uniformly across the `/api/meta` dataset time range. Each run used 1,000 requests, concurrency 16, seed 12345, and read the full PNG response body.

| Time | Server | Commit | Requests | Concurrency | Seconds | Tiles/sec | Mean bytes/tile | P50 ms | P95 ms | P99 ms | Max ms | Command |
| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 2026-05-26T13:04:04+02:00 | old tileserver | `2a974eae` | 1000 | 16 | 21.0053497 | 47.61 | 136498.06 | 236.78 | 945.74 | 1282.95 | 1707.89 | `go run .\cmd\tileapibench -base-url http://127.0.0.1:18080 -stats full.tshd\stats.json -requests 1000 -concurrency 16 -seed 12345` |
| 2026-05-26T13:04:04+02:00 | optimized tileserver | `e601eac` | 1000 | 16 | 0.3332554 | 3000.70 | 65096.66 | 5.64 | 9.59 | 12.31 | 14.84 | `go run .\cmd\tileapibench -base-url http://127.0.0.1:18081 -stats full.tshd\stats.json -requests 1000 -concurrency 16 -seed 12345` |

Optimizations in `e601eac`:

- Stop delta replay once the next indexed delta frame starts after the requested timestamp.
- Apply delta payloads directly into the tile buffer instead of allocating decoded `deltaEvent` slices.
- Encode API tiles as indexed-color PNGs, copying palette IDs directly into `image.Paletted` rows instead of expanding every pixel to RGBA.
- Use `png.BestSpeed` for served API tiles. This keeps compression enabled while avoiding the original default-compression CPU cost.
- Mark timestamped tile URLs as immutable-cacheable with `Cache-Control: public, max-age=31536000, immutable`.

Result: the weighted random tile API workload improved from 47.61 to 3000.70 tiles/sec, a 63.03x throughput increase, while mean response size dropped from 136.5 KiB to 65.1 KiB.
