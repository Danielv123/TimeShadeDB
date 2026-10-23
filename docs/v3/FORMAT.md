# TimeShadeDB v3 formats

Status: format version 1. Normative unless a section says "tunable" or "recommended".
The executable definitions are `internal/v3/core` (types and semantics),
`internal/v3/core/memstore` (semantics as code) and `internal/v3/core/wire` (wire codecs).

## 1. Conventions

- All integers are little-endian. `i64`/`u32`/... are fixed width.
- `uvarint` is unsigned LEB128 (Go `binary.AppendUvarint`), at most 10 bytes.
  `varint` is zigzag (`(n << 1) ^ (n >> 63)`) followed by uvarint.
- Times are `TimeMs`: int64 Unix milliseconds on the map clock (PLAN.md §4.1, D2).
  Windows are aligned to the epoch: window of length `W` containing `t` is
  `[floor(t/W)*W, floor(t/W)*W + W)`.
- Names in ids: `EscapeName` keeps bytes in `[a-z0-9_-]` and writes every other byte of the
  UTF-8 encoding, upper-case letters included, as `~XX` (upper-case hex). Upper case is
  escaped so ids never collide on case-insensitive file systems; `.` is escaped so it can
  separate parts.
  - Datastore id: `Escape(map) "." Escape(surface) "." Escape(force)`,
    e.g. `gw-bc41437d~2E1.nauvis.player` for map `gw-bc41437d.1`.
  - Surface id: `Escape(map) "." Escape(surface)`, e.g. `gw-bc41437d~2E1.nauvis`.
  - Ids are canonical: a parser rejects lower-case hex, unescaped bytes outside the set,
    empty parts and the wrong number of parts. Raw names are at most 200 bytes.
- A datastore is one (map, surface, force). Terrain, the palette and overlays are per
  datastore. Movers are per surface (all forces; each sample carries its force).

## 2. Grid

| Constant | Value |
|---|---|
| chunk | 32 x 32 tiles; chunk image 32 x 32 pixels, row-major |
| z0 tile | 16 x 16 chunks = 512 x 512 pixels, 1 pixel per world tile |
| level z | z <= 0; one pixel covers 2^-z x 2^-z world tiles; every tile is 512 x 512 pixels |
| grid origin | world tile -256 on both axes, at every level |
| levels | `MinZ` = -10 to 0 |
| bounds | z0 tile x, y in [-2048, 2048]; at level z, [-(2048 >> -z), 2048 >> -z] (at least +-1) |

- Tile (z, x, y) covers world tiles `[-256 + x*512*2^-z, -256 + (x+1)*512*2^-z)` per axis.
  The -256 origin makes each z0 tile one 512-tile gridworld instance (instance (0,0)
  covers [-256, 256)), as in illisis `keys.ts`.
- Chunk (cx, cy) is in z0 tile `(floor((cx+8)/16), floor((cy+8)/16))` at block
  `b = ((cx+8) mod 16) + 16*((cy+8) mod 16)` (0..255, row-major). Inverse:
  `cx = 16*x - 8 + (b & 15)`, `cy = 16*y - 8 + (b >> 4)`.
- Pixel `p` (0..1023, row-major) of block `b` is tile pixel
  `(32*(b & 15) + (p & 31), 32*(b >> 4) + (p >> 5))`, which is world tile
  `(32*cx + (p & 31), 32*cy + (p >> 5))`.
- The parent of tile (z, x, y) is (z-1, floor(x/2), floor(y/2)); its children are
  (z+1, 2x+i, 2y+j). Child (i, j) fills the parent quadrant starting at pixel (256i, 256j).

## 3. Colours, palette and coarse pixels

- Colours are RGB565 exactly as `LuaForce.get_chunk_chart` returns them:
  `r = v>>11 & 31, g = v>>5 & 63, b = v & 31`. 0 (black) means uncharted.
  Viewers expand channels by bit replication (`r8 = r5<<3 | r5>>2`, `g8 = g6<<2 | g6>>4`).
- Each datastore has an append-only palette of RGB565 colours. Entry 0 is always 0. Entries
  are never changed or removed, so the first `n` entries (palette version `n`) are
  immutable. The order is store-defined: two stores fed the same data may number colours
  differently, so tests compare RGB565, never indices. At most 65,536 entries.
- Coarse level pixels (z < 0) are the per-channel average of the 2 x 2 pixels below, rounded
  half up: `((a+b+c+d+2) >> 2)` per channel. This is bit-identical to illisis
  `shared/levels.ts`, so illisis z1-z4 data cross-checks the pyramid. Uncharted children
  count as 0.

## 4. Terrain semantics

The full rules are on `core.TerrainStore`. In short:

1. A `ChunkUpdate` (datastore, chunk, T, full pixels or deleted) goes into the open window
   containing T. Per chunk and window the greatest T wins; on equal T the later arrival
   wins. Deleted sets the chunk to all 0.
2. Every datastore has a **horizon**. Updates with T < horizon are late and dropped
   (terrain is absolute; the next update corrects it).
3. `Commit(upTo)` closes every window ending at or before `upTo` and raises the horizon to
   `floor(upTo/W)*W`. For each tile and closed window, chunks whose final content differs
   from the committed content form a block. A non-empty block creates the tile's next
   **version**, effective at the window end. A window that nets out creates nothing.
4. Version 0 is all uncharted. The content of (datastore, tile, version) never changes.
   The version effective at t is the newest with effective time <= t. Versions at times
   before the horizon never change.
5. Early flush (tunable): under memory pressure a store may write a window in several
   blocks, all effective at the window end. Pixels at every t are unchanged; only version
   numbers differ.
6. **Live**: when a tile has open-window updates and t >= max(horizon, LastT) (LastT = the
   newest accepted update time of the datastore), a live read also applies the newest open
   update of each chunk. Live content has no version; it is served without caching.

Coarse levels (z < 0, WP3) have their own versions per coarse tile: each record the
pyramid writes is the next version. Recommended schedule (tunable): write a version of a
coarse tile when a descendant changed, at most once per aligned interval of 15 min (z -1),
60 min (z -2) or 8 h (z <= -3), and always keep the latest state reachable. The
reference oracle (`memstore.Pyramid`) writes a version at every change. (The plan's 5/15/60
min schedule stored about 2.3 B per chunk-hour over the illisis test map, which with z0
exceeds the PLAN.md §5 disk target; see 5.4.)

As implemented in `internal/v3/pyramid` (WP3): a version is built for time T and its
content is exactly the 2 x 2 average of the committed z0 content at T, so it is
effective at T. The live server builds at T = the datastore horizon after each commit;
a level is due when a multiple of its interval lies between the previous successful build
pass and T (with a commit every window: when T is a multiple). Each interval is a multiple
of the finer one, so when a tile is built every finer level is current at T. A tile
without any version is built at the first build after a z0 descendant gets data, whatever
the schedule, so a new surface or area appears at every level within one commit. A flush
(shutdown, import) builds every pending tile at the horizon. An offline rebuild of an
imported history produces the same versions as a live server that committed every window.
A version is synced to disk before it is visible to readers.

Between versions, a coarse tile whose z0 descendants changed has a **live latest**: the
2 x 2 average of the committed z0 content as of the last build pass (not of open windows).
It is not stored and has no version; at t >= the horizon of the last pass the tile reports
`live` and a live sequence (like z0 rule 6) and `live.bin` serves it (API.md).

Immutability rule for every level: once the server has served anything, a new version may
only become effective at or after the datastore horizon, so `VersionAt(t)` for t below the
horizon never changes (API.md caches those `view` answers as immutable). Coarse versions
with older effective times may only be written by offline builds (import, rebuild) before
the database is served.

## 5. On-disk layout

```
<db>/
  timeshadedb-v3.json                    {"format":"timeshadedb-v3","version":1}
  maps.json                              map metadata from "map" messages (WP6)
  datastores/<datastore id>/
    meta.json                            {"key":{...},"window_ms":60000,"grid_origin":-256,"format_version":1}
    palette.bin                          append-only palette (5.2)
    state.json                           committed horizon and update time range (5.7, WP2)
    tiles/<x>_<y>.blk, .idx              z0 terrain series, one per tile (5.1, 5.3)
    pyramid/<z>/<x>_<y>.blk, .idx        coarse snapshots, z = -1, -2, ... (5.4, WP3)
    wal/                                 write-ahead log of open windows (WP2, tunable)
    open/<start>.spill                   open windows spilled out of memory, a cache of wal/ (5.7)
  movers/<surface id>/
    meta.json                            {"key":{...},"format_version":1}
    windows/<YYYY-MM-DD>/<start>.mvw     closed 5-minute windows (5.5)
    keyframes/<YYYY-MM-DD>/<t>.mvw       hourly keyframes (5.5)
    days/<YYYY-MM-DD>.json               day index, core.DayIndex as JSON
    open/<start>.wal                     samples of one open window (5.5)
  movers/wal/wal.log                     open-window samples of older stores (5.5)
  sources.json                           per-source ingest state: horizon holds and retry dedupe (5.8)
  overlays/<datastore id>/               interval log and per-tile index (WP4, 5.6)
  icons/                                 catalog icon spritesheet and metadata (optional)
    icons.json                           core.CatalogIcons: {"png":"icons/<file>","json":"icons/<file>"}
    <file>                               the two files icons.json names ([A-Za-z0-9._-], content-hashed)
```

`icons/` is written by `import-minimap --db` from the illisis export's icons, and by the
Clusterio plugin from the default mod pack's data export (clusterio/README.md, "Icons"). The
paths in `icons.json` are relative to the API base, so the catalog repeats them unchanged and
`GET icons/<file>` serves them (API.md 2). A writer puts the two files in place first and
`icons.json` last, each through a temporary file and a rename. `serve-v3` reads `icons.json`
again when its modification time or size changed, checked when the catalog is rebuilt (at
most every 2 s) and on each icon request, so replaced icons need no restart.

`<x>_<y>` uses decimal integers with a leading `-` for negatives, e.g. `tiles/-3_2.blk`.
Every JSON file is written as tmp + rename. Unknown files are ignored.

### 5.1 Series files (`.blk` + `.idx`)

A series is an append-only record log (`core.Series`). Terrain keeps one per z0 tile, the
pyramid one per coarse tile.

`.blk`: a sequence of records, each a 32-byte header followed by its payload.

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `TSB3` |
| 4 | 1 | kind: 1 delta, 2 keyframe, 3 snapshot |
| 5 | 1 | flags (bit 0 = wide: u16 palette indices) |
| 6 | 2 | reserved, 0 |
| 8 | 8 | `t_start` i64 |
| 16 | 4 | `t_len` u32 (effective time `t_end = t_start + t_len`) |
| 20 | 4 | payload length u32 |
| 24 | 4 | CRC-32C (Castagnoli) of the payload |
| 28 | 4 | CRC-32C of bytes 0..27 |

`.idx`: fixed 24-byte records, one per `.blk` record, in the same order. The record number
is the record's `Seq`.

| Offset | Size | Field |
|---|---|---|
| 0 | 8 | `t_start` i64 |
| 8 | 4 | `t_len` u32 |
| 12 | 6 | offset of the record header in `.blk`, u48 |
| 18 | 3 | payload length u24 (payloads are under 16 MiB) |
| 21 | 1 | kind |
| 22 | 1 | flags |
| 23 | 1 | reserved, 0 |

The PLAN sketched a 16-byte `.blk` header; it is 32 bytes here so the index can be rebuilt
from `.blk` alone (it needs `t_start` and `t_len`) and torn headers are detectable.

Rules:

- Records are appended with non-decreasing `t_end`.
- Write order: append the `.blk` record, fsync (per the fsync policy, tunable: group commit
  about once a second), then append the `.idx` record.
- Recovery on open: read the last `.idx` record; if it points past the end of `.blk` or its
  header CRC fails, drop index records from the end until one is valid, then truncate `.blk`
  after it. Then scan `.blk` from the last indexed record and append index records for any
  complete, CRC-valid records found (index rebuilt from `.blk`). A missing `.idx` is rebuilt
  entirely. A corrupt record in one tile never prevents opening the database.
- Reading the state at t (`Series.Chain`): binary-search `.idx` for the last record with
  `t_end <= t`, walk back to the newest keyframe or snapshot at or before it, and read the
  whole range in one contiguous `.blk` read.

### 5.2 `palette.bin`

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `TPL3` |
| 4 | 4 | reserved, 0 |
| 8 | 2n | entries, u16 RGB565 each; entry 0 is 0 |

Append-only. The entry count is `(size - 8) / 2`; an odd trailing byte is a torn write and
is truncated on open. A new colour is appended (and fsynced) before any block that uses its
index.

### 5.3 Terrain payloads

Payloads are zstd frames (one frame per record). Inside, after decompression:

**Delta block** (kind 1), for one tile and one window (or part of one, on early flush): the
chunks that changed, in increasing block order. For each:

```
uvarint block            0..255
uvarint n                number of pixels given, 1..1024
positions                n == 1024: none (every pixel, in order)
                         n <= 128:  n uvarints: first position, then (position - previous - 1)
                         otherwise: a 128-byte bitmap, bit p (byte p>>3, bit p&7) set for each given pixel
n indices                u8 each, or u16 LE each when the record's wide flag is set
```

Pixels not given keep their value. A deleted chunk is written as n = 1024 zeros. Writers
choose positions for the pixels that differ from the committed content; when more than
512 differ they should write the whole chunk.

**Keyframe** (kind 2): the tile's full state after every earlier record with the same or
earlier `t_end`: 262,144 indices (512 x 512, row-major), u8 or u16 LE when wide. It carries
the same `t_start`/`t_len` as the delta it follows.

Keyframe rule (tunable, WP2): write a keyframe when the delta bytes since the last keyframe
reach 2x the size of that keyframe, and at most every 256 records of chain. The prototype
measured keyframes as 55% of the bytes at a 256-record cap, so this rule should be tuned
against the §5 disk target.
WP2 measured the rule on the test-data replay (60 s windows, zstd default level): factor 2
gives 3.31 B per chunk-hour over the whole map with a cold read p95 of 1.07 ms, factor 4
3.08 B and 1.09 ms, factor 8 2.79 B and 1.30 ms. The store's default is factor 4 with the
256-record cap. Before the first keyframe of a tile, the size of record 0 (the first delta,
which holds the whole content) stands in for the keyframe size.

Indices refer to the datastore palette (5.2). A record uses the wide flag if any index in it
is above 255.

### 5.4 Coarse snapshots (WP3)

Series `pyramid/<z>/<x>_<y>` (series name `<datastore id>/pyramid/<z>/<x>_<y>`, e.g.
`gw-bc41437d~2E1.nauvis.player/pyramid/-2/0_-1`). Every record is one version: version n
of a coarse tile is record n-1, `t_start` is its effective time and `t_len` is 0. Each
record changes content (a build whose average equals the previous version writes
nothing). Two kinds:

- **Snapshot** (kind 3, flags 0): a zstd frame of 512 x 512 u16 RGB565 LE, row-major.
  Record 0 is always a snapshot.
- **Coarse delta** (kind 1, wide flag set): a zstd frame holding the pixels that differ
  from the previous version, with the same layout as the terrain delta block (5.3) over
  the coarse tile's 256 blocks of 32 x 32 pixels (block b covers pixels
  `(32*(b & 15) + (p & 31), 32*(b >> 4) + (p >> 5))`), except that the n values are
  u16 LE RGB565 colours, not palette indices.

Snapshot rule (tunable; defaults measured on the illisis test map): write a snapshot
instead of a delta when more than half the pixels changed, after 512 deltas, or when the
delta bytes since the last snapshot would exceed 64 times that snapshot. With the default
15 min / 60 min / 8 h schedule, an offline rebuild of the illisis history (all 12 surfaces;
the same versions as a live server that commits every window) stores 2.89 MB, 1.88 B per
chunk-hour including record headers and index (nauvis alone 1.40), with cold reads of any
historical version at p95 about 1.1-1.8 ms (`internal/v3/pyramid` TestIllisisHistory,
TestIllisisTuning). The plan's 5/15/60 min schedule stored 2.29 B per chunk-hour. WP2's z0
on the same map is 3.09 B per chunk-hour (5.3), so z0 and pyramid together come to 4.96.
Replayed live through WP2's file-backed terrain store (a commit before every batch of
1,024 updates, `Update` after each commit, a flush at the end), the whole map stored
3.09 B per chunk-hour for z0 and 1.82 for the pyramid, 4.91 in total
(`internal/v3/pyramid` TestWholeMapFileStores, with TSDB_TESTDATA set).

### 5.5 Mover windows on disk (WP4)

A `.mvw` file is a single zstd frame whose content is an MVW3 body (7.4). Closed windows
have kind 1, keyframes kind 2. Because the file is already a zstd frame of the wire body,
the server can send it unchanged with `Content-Encoding: zstd`. Windows with no rows are not
written. A day index lists only windows and keyframes that exist.

Live samples (the open windows) are kept in a log per window, `open/<start>.wal` (decimal
window start in ms): CRC-framed batches of `'M'` (meta) and `'S'` (sample) records,
documented in `internal/v3/movers/wal.go`, with meta numbers local to the file. A batch is
written to the logs of the windows it touches before the store accepts it, one write per
log: a crash between those writes can leave a batch that was never acknowledged in some of
its logs only (the sender retries it; the repeats are removed at commit). The rows are
also kept in memory while they fit the store's byte budget (the server's
`--mem-budget`/8, counted per row: about 150 bytes, more with carriages and rail pieces);
beyond it whole windows, oldest first, are evicted and read back from their logs by the
commit that closes them, so the budget never makes ingest refuse samples. A log is deleted
after the commit that closed its window wrote `meta.json`; on open, logs of windows below
the horizon are deleted (a crash between the two) and the others replayed (a torn tail is
truncated). Stores before these logs kept every open sample in one file,
`movers/wal/wal.log` (the same records), rewritten at every commit; opening such a
database moves its rows into the window logs and deletes it.

### 5.6 Overlays on disk (WP4, recommended)

An append-only log of `core.OverlayEvent` records plus a per-tile index of interval
positions, both rebuildable from the log. The format is WP4's choice; only the API output
(API.md "overlays") is a contract.

### 5.7 Terrain state and WAL (WP2)

`state.json` holds `{"horizon": ms, "committing": ms, "first": ms, "last": ms, "has_data":
bool}`: the committed horizon, the target of a commit that has not finished, and the
accepted update time range. It is rewritten (tmp + rename, fsynced) when a commit that has
tiles to write starts, with `committing` set to its target horizon, and again at the end of
every commit that moves the horizon, after the records of that commit are fsynced.
`horizon` is absent until the first commit; `committing` is absent when no commit is
pending. `meta.json` is written once and never changes.

`wal/<seq>.wal` (16-digit decimal seq) holds the updates of open windows. A segment is a
sequence of frames:

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | payload length u32 |
| 4 | 4 | CRC-32C of the payload |
| 8 | | payload: one zstd frame |

Decompressed, a payload is `uvarint n` followed by n updates of `varint T`, `varint cx`,
`varint cy`, `u8 flags` (bit 0 deleted) and, unless deleted, 1,024 RGB565 pixels (u16 LE).
A frame holds at most 1,024 updates (about 2.1 MB decompressed, under the 64 MiB decoder
limit); `Apply` splits a larger batch into several frames, written with one write, so a
batch is logged whole or not at all. Late updates are not logged. Frames are fsynced by a
group commit (default every second). A commit that moves the horizon starts a new segment
and deletes the segments whose updates are all older than the new horizon.

On open, frames are read one at a time and replayed in order; updates older than the
horizon are dropped. A torn tail (an incomplete header or frame, a zero or oversized
length, a checksum mismatch) ends its segment, which is truncated there, and the
truncation is logged. A frame whose checksum is valid but that does not decode is not a
torn write: open fails with an error naming the segment and offset, and nothing is
truncated.

If a crash interrupted a commit after some tiles were written but before `state.json`
recorded the new horizon, the replay brings those windows back; a tile skips every window
that ends at or before its last record's `t_end`, so nothing is written twice. Open then
finishes that commit (to `committing`) before it accepts any update, so an update for one
of those windows is late for every tile, as it was while the commit ran.

`open/<start>.spill` holds open windows moved out of memory when the open windows would
exceed the store's budget (the server's `--mem-budget`/8): whole windows, oldest first,
instead of refusing the batch. It is a sequence of frames in the WAL frame layout above,
one per tile and spill (a window spilled twice has two frames for a tile); decompressed:
`varint` tile x, `varint` tile y, `uvarint n`, then n chunks of `u8 block`, `varint T`,
`u8 flags` (bit 0 deleted) and, unless deleted, 1,024 RGB565 pixels (u16 LE). Updates
for a spilled window that arrive later are kept in memory; the commit merges them per
chunk (greatest T wins, on equal T the later frame, and the in-memory update over the
spilled one), and live reads of a tile read its frames. Spill files are a cache of the
WAL, which keeps every update of an open window: open deletes them and the replay spills
again as it fills the budget, so a crash during a spill or a commit loses nothing.

### 5.8 Sources (`sources.json`)

The live server keeps, per forwarder source (envelope `src`), what it needs to hold
commits back for a source that is still sending older data and to recognise retried
envelopes (API.md 5, "Horizon"). Written as tmp + rename about once a commit step;
missing or unreadable means no sources known.

```json
{"version": 1, "sources": [{"src": "...", "seq": 123, "recv_ms": 1791630000000,
  "seen_ms": 1791630005000, "maps": {"<map key>": 1791629999000},
  "map_seen_ms": {"<map key>": 1791630005000}}]}
```

`maps` is, per map the source fed, the newest message time applied (or the newest
`caught_up` time); `seq` and `recv_ms` are the last applied envelope's (absent without a
`seq`); `seen_ms` is the server's wall clock when the source was last heard from, and
`map_seen_ms` per map when it last had data or `caught_up` for it (optional; `seen_ms`
for a map without an entry). Entries not heard from for 7 days are dropped. Maps with data
but no source here are held from the server's start (API.md 5, "Horizon"); that hold is
not saved.

## 6. Version and compatibility rules

- `timeshadedb-v3.json` `version` and every `meta.json` `format_version` are 1. A reader
  rejects a higher version and never writes to it.
- Additions that old readers can ignore (new JSON fields, new files) keep the version. Any
  change to a byte layout in this document bumps it, and a migration (the copy-and-swap
  generation framework in `migration.go`) converts existing data.

## 7. Wire formats

Bodies below are what HTTP carries after removing `Content-Encoding`. The server compresses
with zstd when the client accepts it, else gzip, else identity (API.md), so viewers never
decompress in JavaScript. Reference codecs: `internal/v3/core/wire` (Go) and
`web/src/api/wire.ts` (TypeScript), tested against `web/fixtures/`.

### 7.1 TSV3 tile

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `TSV3` |
| 4 | 1 | format: 1 pal8, 2 pal16, 3 rgb565 |
| 5 | 1 | z, i8 |
| 6 | 1 | flags: bit 0 live (content of the open window, not a version) |
| 7 | 1 | reserved, 0 |
| 8 | 4 | x, i32 |
| 12 | 4 | y, i32 |
| 16 | 4 | version u32 (for live tiles: the committed version underneath) |
| 20 | 4 | palette version u32: smallest palette length that decodes the tile (highest index + 1); 0 for rgb565 |
| 24 | 2 | width, 512 |
| 26 | 2 | height, 512 |
| 28 | 4 | reserved, 0 |
| 32 | | 262,144 pixels, row-major: u8 index (pal8), u16 LE index (pal16) or u16 LE RGB565 (rgb565) |

z0 tiles are paletted: pal8 when every index is below 256, else pal16. Coarse tiles are
rgb565. The encoding depends only on the content and the palette order, so the bytes of an
immutable (datastore, tile, version) URL never change.

### 7.2 TSD3 tile delta (z0)

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `TSD3` |
| 4 | 1 | format: 1 pal8, 2 pal16, 3 rgb565 |
| 5 | 1 | z, always 0 |
| 6 | 2 | entry count u16 (0..256) |
| 8 | 4 | x, i32 |
| 12 | 4 | y, i32 |
| 16 | 4 | from version u32 |
| 20 | 4 | to version u32 |
| 24 | 4 | palette version u32 (as in TSV3) |
| 28 | 4 | reserved, 0 |
| 32 | | entries, block order strictly increasing |

Entry: `u16 block`, `u16 reserved (0)`, then the chunk's 1,024 pixels at version `to`
(row-major; u8 or u16 LE as the format says). The entries are exactly the chunks whose
content differs between the two versions (`TerrainStore.ChangedChunks`). Applying them to
the tile at `from` gives the tile at `to`.

### 7.3 TSP3 palette

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `TSP3` |
| 4 | 4 | entry count n u32 (the palette version) |
| 8 | 2n | u16 LE RGB565; entry 0 is 0 |

### 7.4 MVW3 mover window

Used for closed windows, keyframes and the live payload, on the wire and (zstd-framed) on
disk.

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `MVW3` |
| 4 | 1 | kind: 1 closed window, 2 keyframe, 3 live |
| 5 | 1 | format version, 1 |
| 6 | 2 | flags: bit 0 = the tick column holds data; bit 1 = the speed and rail columns (9, 10) follow |
| 8 | 8 | start i64 (window start, keyframe time, or live "now") |
| 16 | 4 | len u32 (window length; 0 for keyframes; trail length for live) |
| 20 | 4 | number of mover table entries |
| 24 | 4 | number of rows |
| 28 | 4 | mover table length in bytes |
| 32 | | mover table: UTF-8 JSON array of `MoverMeta` objects (`{"id","k","f","name","n"?,"o"?,"c"?}`) |
| | | then 8 columns (10 with header bit 1), each `u32 byte length` followed by its bytes |

Rows are sorted by (mover entry, t), and (entry, t) is unique. A journey whose meta changes
inside the window has one entry per meta, with the same `id`. Entries are sorted by
(`id`, first row time). Per entry, the running values (`t`, tick, x, y, orientation, speed) start
at (start, 0, 0, 0, 0, 0).

| # | Column | Per | Encoding |
|---|---|---|---|
| 1 | counts | entry | uvarint: the entry's number of rows |
| 2 | dt | row | varint: t - previous t (first row: t - start; negative in keyframes) |
| 3 | tick | row | varint: tick - previous tick; empty when header bit 0 is clear; rows with flag `no-tick` write 0 and keep the running tick |
| 4 | x | row | varint: x units - previous x units (1 unit = 1/16 tile); gone rows write 0 and keep the running value |
| 5 | y | row | same as x |
| 6 | orientation | row | varint: (o - previous o) as a signed 16-bit difference, o in u16 units (65,536 per turn); gone rows write 0 |
| 7 | flags | row | 1 byte (below) |
| 8 | carriages | row with bit 6 | uvarint n, then per carriage: varint (cx - x) units, varint (cy - y) units, uvarint orientation u16 |
| 9 | speed | row with bit 7 | varint: speed units - the entry's previous rail row's (8,192 units per tile a tick) |
| 10 | rail | row with bit 7 | uvarint (pieces << 1 \| rail start), then per piece: varint (px - previous px), varint (py - previous py) in whole tiles, the first from the row's tile (x units >> 4, y units >> 4), and a byte kind << 4 \| direction (RAIL-GEOMETRY.md 8) |

Row flags (`core.MoverFlags`; bit 6 exists only on the wire):

| Bit | Name | Meaning |
|---|---|---|
| 0 | stop | at rest at this sample |
| 1 | gone | left this surface or instance; the row has no position |
| 2 | handoff | on a gone: the journey resumed within 10 s (instance crossing, not an end) |
| 3 | end-switch | the train's leading end flipped; x, y is the new leading carriage |
| 4 | no-tick | tick unknown (imported data) |
| 5 | no-heading | orientation unknown; derive it from the motion |
| 6 | carriages | the carriage column has a block for this row |
| 7 | rail | a train's rail path sample (RAIL-GEOMETRY.md 8): the orientation is the leading carriage's own orientation (the rail tangent), and columns 9 and 10 have its speed, its rail pieces and whether a new rail path begins (PROTOCOL.md 3.1 `v`, `rp`, `ru`). Gone rows never have it |

**Train bodies in keyframes and live payloads.** A rail row lists only the pieces its leading
joint entered since the row before, so a viewer that starts from a keyframe or a live payload
would know a moving train's head but not where its body lies. In a keyframe (kind 2), and in a
live payload (kind 3) for each journey's first row, a rail row therefore carries its *body* in
front of its own pieces: the pieces that the journey's earlier rows of the same rail path
listed and that still lie under the train, rear first (`core.RailBody`: the viewer's chain of
RAIL-GEOMETRY.md 8.2 followed over the rows, a piece kept while its exit end lies within the
train length plus 4 tiles behind the leading joint; at most 1,024 pieces in all). This is the
shape a stop sample already has (PROTOCOL.md 3.1 `rp`): a viewer that holds the rows before
skips those pieces as resends. Rows that list the whole train themselves (with rail start, stop
samples and the sample after a stop) get no body, nor do rows whose path was lost (a piece that
neither continues the path nor is on it) until such a row. The body follows on from the newest
keyframe at or before the row, whose row begins the path again, so it is the same whether or
not the store restarted in between. Closed windows (kind 1) never carry bodies. Older data and
older stores give keyframes without bodies, which read as before.

Positions: a train's x, y is its leading carriage centre; carriages list every carriage from
the leading end. Orientation is Factorio's (0 north, 0.25 east); a train's points from the
second carriage to the first, except on rail rows (bit 7), where it is the leading carriage's
own orientation. Velocity is not stored except a rail row's speed: viewers derive it from
neighbouring rows (with ticks when present: `ticks / (60 * speed)` seconds). Windows without
rail rows keep the eight-column layout, so older readers read them unchanged.

Measured on a synthetic train track (1,000 samples, 250-350 ms apart): 8.1 B/sample raw,
2.1 B/sample after zstd (`wire.TestMoverWindowSizeOnTrainTrack`). The PLAN target is
6 B/sample stored on the imported data.
