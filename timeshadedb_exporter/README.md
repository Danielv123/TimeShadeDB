# TimeShadeDB Exporter

> **Deprecated.** Use the v3 capture in [`capture/`](../capture/README.md) instead: the
> `timeshade_capture` mod with `timeshadedb tail`, or the Clusterio module. This mod does
> not run on Factorio 2.0: it calls `game.write_file` (`control.lua:145`), which 2.0 moved
> to `helpers.write_file`. It also stores a 4 KB hex string per chunk in `storage` and draws
> only bare terrain (`get_tile`). Its entity export to VictoriaMetrics has been removed. The
> chunk export is kept until the v2 code is removed (decision D11).

This Factorio mod writes charted chunk tile snapshots to:

```text
script-output/timeshadedb/chunk-charted.tsv
```

The chunk TSV format is:

```text
game_tick surface chunk_x chunk_y force rgb565_hex_payload
```
