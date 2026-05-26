# Benchmark Log

| Time | Change | Rows | Seconds | Rows/sec | Command |
| --- | --- | ---: | ---: | ---: | --- |
| 2026-05-26T11:07:18.2757599+02:00 | Coalesce batch import delta frames | 1000000 | 16.1680245 | 61850.48 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-coalesced.tshd --max-rows 1000000` |
| 2026-05-26T11:11:42.4168812+02:00 | Reuse zstd encoders for snapshots and deltas | 1000000 | 6.3623265 | 157175.21 | `.\timeshadedb.exe import-csv --input 2022_place_canvas_history.csv.gzip --db sample1m-reuse-enc.tshd --max-rows 1000000` |
