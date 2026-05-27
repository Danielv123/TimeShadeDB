# TimeShadeDB Exporter

This Factorio mod writes charted chunk tile snapshots and adaptive moving-entity
samples to:

```text
script-output/timeshadedb/chunk-charted.tsv
script-output/timeshadedb/entity-positions.tsv
```

The chunk TSV format is:

```text
game_tick surface chunk_x chunk_y force rgb565_hex_payload
```

The entity TSV format is:

```text
game_tick surface force entity_type entity_id segment x y orientation speed
```

The TimeShadeDB reader imports this file into VictoriaMetrics as two canonical
coordinate metrics:

```text
timeshadedb_entity_x{savefile,surface,force,entity_type,entity_id,segment}
timeshadedb_entity_y{savefile,surface,force,entity_type,entity_id,segment}
```

The importer also adds `segment` when the TSV contains it. A segment is advanced
when an entity disappears, a player disconnects or loses a valid character or
vehicle, or the observed surface or force changes. This splits paths without
depending on VictoriaMetrics' internal stale marker encoding.

There is intentionally no `tile_xy` label. View filtering is expected to query
by save, surface, force, entity type, and time range, then clip paths client-side.
