# timeShadeDB design plan

## Goal

Build a Go database for continuously ingesting r/place pixel placements and
querying map tiles at a requested timestamp.

The core model is:

1. Keep the current canvas in memory as palette IDs.
2. Discard placements that do not change the current pixel color.
3. Append real changes to per-tile delta logs.
4. Periodically save full compressed snapshots per tile.
5. Answer historical queries by loading the latest snapshot at or before the
   requested timestamp, then replaying deltas forward.

The test data in this folder is:

```text
2022_place_canvas_history.csv.gzip
```

It is a gzip-compressed CSV with columns:

```text
timestamp,user_id,pixel_color,coordinate
```

The database ignores `user_id`.

## Assumptions

- Canvas size is `2000x2000`.
- Tile size is `512x512`.
- The canvas therefore has `4x4 = 16` logical tiles.
- Edge tiles store only their real size, not padded pixels:
  - full tile width or height: `512`
  - right/bottom edge width or height: `464`
- Pixel colors are palette encoded as `uint8`.
- Palette ID `0` is reserved for unset/no pixel.
- Actual colors start at palette ID `1`.
- Timestamps are stored as Unix seconds by default in `uint32`.

The timestamp recommendation is:

```text
uint32 timestamp_sec = unix_seconds
```

Unix seconds fit in 32 bits until February 7, 2106. The CSV contains
subsecond timestamps, but this database intentionally stores second precision.
For multiple placements in the same second, ingestion order remains the
tiebreaker.

## Public API

The first implementation should expose a library plus small CLIs.

```go
type DB struct {
    // internal fields
}

type OpenOptions struct {
    Path      string
    ReadOnly  bool
    CacheSize int64
}

type TileCoord struct {
    X int
    Y int
}

type TileAtOptions struct {
    Timestamp time.Time
    Tile      TileCoord
}

type TileResult struct {
    Tile        TileCoord
    Width       int
    Height      int
    Palette     []RGB
    Pixels      []uint8 // row-major palette IDs
    SnapshotSec uint32
    Replayed    int
}

func Open(opts OpenOptions) (*DB, error)
func (db *DB) IngestPlacement(ts time.Time, x, y int, rgb RGB) error
func (db *DB) TileAt(ctx context.Context, opts TileAtOptions) (*TileResult, error)
func (db *DB) Close() error
```

CLI commands:

```text
timeshadedb import-csv --input 2022_place_canvas_history.csv.gzip --db db.tshd
timeshadedb query-tile --db db.tshd --timestamp "2022-04-04T00:55:00Z" --tile 1,2 --out tile.raw
timeshadedb stats --db db.tshd
timeshadedb verify --input sample.csv.gzip --db db.tshd
```

## In-memory layout

Keep the full current map in memory as tile-local byte slices:

```text
tile[0].pixels []uint8
tile[1].pixels []uint8
...
tile[15].pixels []uint8
```

Total memory for the current canvas is about 4 MB:

```text
2000 * 2000 * 1 byte = 4,000,000 bytes
```

Per tile state:

```go
type TileState struct {
    TileX int
    TileY int
    W     int
    H     int

    Pixels []uint8

    LastSnapshotSec      uint32
    ChangesSinceSnapshot int
    DeltaBytesSinceSnap  int64
    LastSnapshotBytes    int64

    ActiveDeltaBlock DeltaBlockBuilder
}
```

A placement maps to a tile with:

```text
tile_x = x / 512
tile_y = y / 512
local_x = x % 512
local_y = y % 512
local_index = local_y * tile_width + local_x
```

For edge tiles, `tile_width` is the actual edge width.

## Ingestion pipeline

The ingestion path should be a bounded concurrent pipeline:

```text
source reader -> line batches -> parse workers -> ordered batch merge -> tile router -> tile workers -> compressed writers
```

### Source reader

The CSV import reader streams the gzip file. It should not decompress the full
file to disk.

For production ingestion, the same pipeline can read from an append stream or
message queue instead of gzip.

### Parser workers

Parser workers convert raw CSV rows into compact placement records:

```go
type Placement struct {
    Seq uint64
    Sec uint32
    X   uint16
    Y   uint16
    C   uint8
}
```

The parser ignores the `user_id` field.

The initial implementation can use `encoding/csv` for correctness. If import
speed is not high enough, replace it with a custom parser for this exact CSV
shape:

```text
timestamp,user_id,#RRGGBB,"x,y"
```

The parser must preserve sequence numbers so the merge stage can restore input
order before routing to tile workers.

### Ordered merge

If parsing is parallelized, output batches may complete out of order. The merge
stage emits batches in `Seq` order.

This keeps same-pixel updates deterministic, especially for multiple updates in
the same second.

### Tile workers

Each tile has a single owner goroutine. That goroutine:

1. Checks the in-memory pixel value.
2. Drops the placement if the color is unchanged.
3. Updates the in-memory pixel.
4. Appends a delta event to the tile's active delta frame builder.
5. Decides whether to flush a delta frame or create a snapshot.

This avoids lock contention on the hot in-memory canvas. Queries can use a
separate immutable snapshot cache, or briefly lock/copy a tile if live queries
against the latest state are required.

### Compression workers

Tile workers should not perform expensive zstd compression inline. They enqueue
completed snapshot or delta chunks to a compression pool.

Use `github.com/klauspost/compress/zstd`.

Recommended defaults:

```text
snapshot frames: zstd level 9
closed delta frames: zstd level 9 for best density
active WAL: uncompressed or low-level zstd until finalized
encoder concurrency: enabled
```

For sustained live ingestion, it may be useful to make delta frame level
configurable, for example level 3 during ingestion and level 9 during later
compaction.

## On-disk layout

Use an append-only directory database with one data file and one index sidecar
per tile:

```text
db.tshd/
  manifest.json
  palette.bin
  tiles/
    00_00.tdat
    00_00.tidx
    01_00.tdat
    01_00.tidx
    ...
```

This avoids many tiny files while keeping each tile independently queryable and
independently compactable. `*.tdat` is the append-only compressed data
container. `*.tidx` is the sidecar index container used to jump directly to the
right snapshot and delta frames in the data file.

### manifest.json

Human-readable database metadata:

```json
{
  "format": "timeShadeDB",
  "version": 1,
  "canvas_width": 2000,
  "canvas_height": 2000,
  "tile_size": 512,
  "timestamp_unit": "unix_second",
  "palette_file": "palette.bin",
  "codec": "zstd",
  "codec_level": 9
}
```

All stored timestamps are `uint32` Unix seconds. A future format version can add
epoch-relative millisecond timestamps if a dataset needs subsecond precision.

### palette.bin

Binary palette file:

```text
magic      4 bytes  "TPAL"
version    uint16
count      uint16
entries    count * RGB24
```

Palette ID `0` is implicit unset/no pixel and is not stored as a real color.
Stored entry `0` corresponds to palette ID `1`.

### Tile data file: `tiles/XX_YY.tdat`

Each tile data file is a sequence of independently compressed frames. A frame is
either a full snapshot or a delta frame. The sidecar index records every frame's
offset and length.

File header:

```text
magic       4 bytes "TDAT"
version     uint16
tile_x      uint16
tile_y      uint16
tile_width  uint16
tile_height uint16
```

Frame header, stored uncompressed before each zstd payload:

```text
frame_magic     4 bytes "TFRM"
frame_kind      uint8  // 1=snapshot, 2=delta
timestamp_sec   uint32 // snapshot time or first delta event time
max_time_sec    uint32 // same as timestamp_sec for snapshots
event_count     uint32 // 0 for snapshots
raw_len         uint32
compressed_len  uint32
checksum        uint32 // payload checksum after decompression
payload         compressed_len bytes, one complete zstd frame
```

Every payload is an independent zstd frame. This is the important seekability
property: a reader can seek to a snapshot frame offset, decompress only that
snapshot, then seek to and decompress only subsequent delta frames until the
requested timestamp is reached. It never needs to decompress data before the
chosen snapshot or after the first delta frame that exceeds the requested time.

Snapshot payload, before compression:

```text
pixels []uint8 // tile_width * tile_height, row-major palette IDs
```

Delta payload, before compression:

```text
magic       4 bytes "TDEL"
version     uint16
event_count uint32
base_sec    uint32
events      repeated event_count
```

Event encoding:

```text
dt_sec uvarint // timestamp delta from previous event in this frame
pos    uvarint // local row-major pixel index
color  uint8   // palette ID
```

Events remain in original per-tile order. During replay, apply all events with
timestamp `<= requested_timestamp_sec`, stopping inside the final delta frame if
needed.

### Tile index sidecar: `tiles/XX_YY.tidx`

Each tile has a compact binary index sidecar loaded at startup.

Header:

```text
magic             4 bytes "TIDX"
version           uint16
tile_x            uint16
tile_y            uint16
tile_width         uint16
tile_height        uint16
data_file_size     uint64
snapshot_count     uint32
delta_frame_count  uint32
```

Snapshot index record:

```text
timestamp_sec          uint32
frame_offset           uint64
frame_header_len       uint16
compressed_len         uint32
raw_len                uint32
first_delta_frame_idx  uint32
event_seq              uint64
checksum               uint32
```

Delta frame index record:

```text
min_timestamp_sec uint32
max_timestamp_sec uint32
frame_offset      uint64
frame_header_len  uint16
compressed_len    uint32
raw_len           uint32
event_count       uint32
first_event_seq   uint64
last_event_seq    uint64
checksum          uint32
```

The query path binary-searches snapshot records, then scans delta frame records
until the requested timestamp. Offsets always point into the matching `.tdat`
file.

## Snapshot heuristic

Snapshots should be adaptive per tile. A quiet tile should not be snapshotted
as often as an active tile.

Create a snapshot at a delta frame boundary when any of these are true:

```text
changes_since_snapshot >= min(tile_pixels / 4, 65536)
delta_compressed_bytes_since_snapshot >= 0.75 * last_snapshot_compressed_bytes
time_since_snapshot >= 15 minutes and changes_since_snapshot > 0
time_since_snapshot >= 60 minutes even if activity is low
```

Rationale:

- `tile_pixels / 4` caps worst-case replay at about 25 percent of a full tile.
- `65536` keeps very large/full tiles from accumulating too many replay events.
- The compressed-size rule avoids keeping a long delta chain when storing a new
  compressed snapshot would be similarly cheap.
- The time rule gives predictable query latency for arbitrary timestamps.

For the 512x512 full tiles:

```text
tile_pixels = 262144
tile_pixels / 4 = 65536
```

For edge tiles:

```text
464 * 512 = 237568
237568 / 4 = 59392
```

So the replay target is roughly 59k to 65k changed pixels per tile.

The implementation should record these metrics per tile:

```text
snapshot_count
average_snapshot_compressed_bytes
delta_frame_count
average_delta_frame_compressed_bytes
max_replay_events_between_snapshots
unchanged_placements_discarded
changed_placements_stored
```

After importing the test dataset, tune these constants against real query
latency and compression ratio.

## Delta frame sizing

Flush an active delta frame when either condition is met:

```text
event_count >= 16384
time_span >= 60 seconds
```

This keeps query skip granularity reasonable while still giving zstd enough data
to compress efficiently.

For very active tiles, the event-count rule dominates. For quiet tiles, the
time-span rule avoids leaving long-lived unclosed frames.

## Query algorithm

For `TileAt(timestamp, tile)`:

1. Convert requested `time.Time` to `uint32 timestamp_sec`.
2. Load the tile index if it is not already cached.
3. Binary-search latest snapshot with `snapshot.timestamp_sec <= timestamp_sec`.
4. Seek to the snapshot frame offset in `XX_YY.tdat`.
5. Decompress that single snapshot frame into a caller-owned buffer.
6. Iterate delta frame index records from `snapshot.first_delta_frame_idx`.
7. Skip frames with `max_timestamp_sec <= snapshot.timestamp_sec`.
8. Stop before the first frame with `min_timestamp_sec > timestamp_sec`.
9. Seek to and decompress only the needed delta frames.
10. Apply events in order while `event.timestamp_sec <= timestamp_sec`.
11. Return row-major palette IDs plus the palette table.

Worst-case replay after tuning should be around 65k events per tile. For a
full map query, all 16 tiles can be queried in parallel.

## Durability model

Use append-only data files plus atomic index updates.

Writer flow:

1. Append the frame header and compressed payload to `XX_YY.tdat`.
2. Flush the tile data file.
3. Append or rewrite `XX_YY.tidx.tmp`.
4. Atomically rename `XX_YY.tidx.tmp` to `XX_YY.tidx`.

For continuous ingestion, add an active write-ahead log:

```text
wal/
  tile_00_00.active
  tile_01_00.active
```

The WAL stores unfinalized changed placements. On clean shutdown, active WAL
events are compressed into delta frames. On startup, replay WAL after loading
the latest indexes.

The first batch importer can skip the WAL and write finalized delta frames
directly.

## Palette handling

Maintain a process-wide map:

```go
map[uint32]uint8 // RGB24 -> palette ID
```

When a new color appears:

1. Assign the next palette ID.
2. Append it to `palette.bin`.
3. Publish the updated palette to query readers.

If more than 255 real colors appear, fail the import with a clear error. The
format reserves ID `0`, so there are 255 real color slots.

The known r/place color set is small enough for this.

## Multithreading plan

Recommended goroutines:

```text
1 source reader
N parse workers, default runtime.NumCPU()
1 ordered merge/router
16 tile workers, one per tile
M compression workers, default max(1, runtime.NumCPU()/2)
1 manifest/index writer coordinator
```

The parser and compressor are CPU-heavy. Tile workers are mostly memory and
append-buffer work.

Backpressure should be explicit:

```go
rawBatches := make(chan RawBatch, 2*numCPU)
parsedBatches := make(chan ParsedBatch, 2*numCPU)
tileQueues := make([]chan Placement, tileCount)
compressionJobs := make(chan CompressionJob, 2*numCPU)
```

Bounded channels prevent an import from buffering a large fraction of the CSV
in memory.

## Validation strategy

Use the CSV test data as the main benchmark.

Correctness tests:

1. Import a small prefix of the CSV.
2. Replay the same prefix naively into a `2000x2000` byte array.
3. Query random tiles at random timestamps.
4. Compare database output to naive replay.
5. Include repeated writes to the same pixel in the same second.
6. Include unchanged writes and verify they are discarded.

Full import validation:

```text
total_rows_seen
changed_rows_stored
unchanged_rows_discarded
palette_size
snapshot_count_by_tile
compressed_bytes_by_tile
query_p50_ms
query_p95_ms
query_p99_ms
```

Benchmark queries:

```text
single random tile at random timestamp
all 16 tiles at random timestamp
hot tile near peak activity
latest tile query
oldest tile query
```

## Initial implementation phases

### Phase 1: Batch importer and query reader

- Create database directory and manifest.
- Stream gzip CSV.
- Parse timestamp, color, and coordinate.
- Ignore user ID.
- Maintain palette.
- Maintain current in-memory tiles.
- Drop unchanged placements.
- Write per-tile delta frames.
- Write per-tile snapshots with the adaptive heuristic.
- Implement `TileAt`.
- Add verification against naive replay for small samples.

### Phase 2: Performance tuning

- Add parser worker pool.
- Add compression worker pool.
- Add tile worker goroutines.
- Load indexes into memory on open.
- Add query snapshot cache.
- Benchmark zstd levels 3, 6, and 9 on snapshots and deltas.
- Tune snapshot thresholds from full-dataset metrics.

### Phase 3: Continuous ingestion

- Add WAL for active unfinalized writes.
- Add crash recovery.
- Add live query behavior for newest in-memory state.
- Add background compaction from low-level active blocks to level 9 blocks.

### Phase 4: Tooling

- Add `stats`, `inspect-index`, and `export-tile` commands.
- Add optional PNG export for debugging.
- Add import progress metrics.
- Add pprof endpoints for CPU and memory profiling.

## Open decisions

These are not blockers for the design, but should be decided before locking the
public API:

1. Query output format: raw palette IDs, RGBA bytes, PNG, or all three.
2. Whether timestamps should be inclusive (`<= timestamp`) or exact frame
   boundaries. This design assumes inclusive.
3. Whether continuous ingestion must be crash-safe from day one, or whether the
   first implementation can be an offline batch builder.
4. Whether palette ID `0` should mean unset/transparent or a chosen background
   color when rendering.
