# Benchmark Log

| Time | Change | Rows | Seconds | Rows/sec | Command |
| --- | --- | ---: | ---: | ---: | --- |
| 2026-05-26T11:07:18.2757599+02:00 | Coalesce batch import delta frames | 1000000 | 16.1680245 | 61850.48 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-coalesced.tshd --max-rows 1000000` |
| 2026-05-26T11:11:42.4168812+02:00 | Reuse zstd encoders for snapshots and deltas | 1000000 | 6.3623265 | 157175.21 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-reuse-enc.tshd --max-rows 1000000` |
| 2026-05-26T11:15:34.5737074+02:00 | Simplify import pipeline to ordered parse plus tile workers | 1000000 | 4.5258482 | 220953.06 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-simple-pipeline.tshd --max-rows 1000000` |
| 2026-05-26T11:18:47.9342128+02:00 | Use fastest zstd level for batch import frames | 1000000 | 2.5005987 | 399904.23 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-fast-batch-zstd.tshd --max-rows 1000000` |
| 2026-05-26T11:21:34.0252306+02:00 | Add fixed UTC timestamp parse fast path | 1000000 | 2.3194512 | 431136.47 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-fast-ts.tshd --max-rows 1000000` |
| 2026-05-26T11:43:31.6560201+02:00 | Add fixed coordinate and RGB parse fast paths | 5000000 | 9.7053860 | 515177.86 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample5m-fast-coord-2.tshd --max-rows 5000000` |
