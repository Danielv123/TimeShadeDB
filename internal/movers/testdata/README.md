# Mover test traces

Excerpts of the journey traces in the research notes (`research/out/trace_*.txt`,
printed by `research/scripts/mover_trace.py` from the illisis/minimap S3 export of the
eternitycluster map). Each line is `t_ms +dt (x, y, vx, vy, heading)` or `t_ms +dt GONE`.

- `trace_train_660604.txt.gz`: train journey `248495586.660604.17` on
  `gw-bc41437d.1|nauvis`, lines 1-6000 (41 gones, all instance crossings) and lines
  12001-12400 (one gone that is a real end: the next sample is 35 minutes later).
- `trace_constructron.txt.gz`: a spidertron (constructron) journey, lines 1-4000
  (534 gones, all instance crossings; no heading).

The full traces have 417 and 16,983 gones; `TestTraceHandoffsFull` reads them when
`TSDB_TRACES` points at the directory holding the uncompressed files.
