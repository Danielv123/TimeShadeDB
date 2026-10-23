# Viewer terrain engine

The map canvas of the viewer: WebGL2 palette-index textures (Canvas2D fallback), a global
request scheduler, version resolution from `view`/`timeline`, never-black fallbacks, versioned
texture caching and tile deltas. Contracts: [docs/API.md](../../../docs/API.md),
[docs/FORMAT.md](../../../docs/FORMAT.md) 7, `web/src/api/types.ts` (`DataSource`, perf
hooks) and `layer.ts` (`MapLayer`).

## Files

| File | Role |
|---|---|
| `view.ts` | `MapView`: canvases, pan/zoom/pinch/keys, the frame loop, time (live, scrub, timelapse), catalog polling, live events, the `MapLayer` list |
| `terrain.ts` | `TerrainEngine` (DOM-free): level and tile choice, version lookup, scheduling, texture LRU, fallbacks, deltas, palette, perf events |
| `versions.ts` | `TimelineVersions` (one `timeline` per level and UTC day answers any time of that day) and `ViewVersions` (live server, `view?t=latest` plus `tile` events) |
| `scheduler.ts` | Priority scheduler: at most 16 requests in flight (6 on HTTP/1.1), lowest priority first, unwanted requests dropped or aborted |
| `sources.ts` | `ServerSource` (`/api/v3/`, fetch-based SSE with bearer token) and `StaticSource` (export file names, client-side overlay filter); both expose request URLs (`UrlSource`) |
| `boot.ts` | Start-up: source detection and the speculative first `view`/`timeline` request; adopts the requests `index.html` starts before the bundle loads |
| `loader.ts`, `tile_worker.ts` | `DirectLoader` (main-thread fetch) and `WorkerLoader` (pool of 2-4 workers that fetch and transfer bodies; for Canvas2D they also decode tiles and expand them to RGBA). A worker that fails is not used again; with none left, fetches run on the main thread |
| `render_gl.ts` | WebGL2: R8UI (pal8) / R16UI (pal16, rgb565) textures, 256x256 RGBA palette texture, RGB565 expanded in the shader |
| `render_2d.ts` | Canvas2D: 512x512 RGBA canvas per tile through a 65,536-entry lookup table |
| `renderer.ts` | Renderer interface, lookup tables, pixel snapping |
| `control.ts` | `window.__tsdbControl`, adapted from `MapView` |
| `camera.ts`, `lru.ts`, `clock.ts`, `delay.ts` | Camera math, byte-bounded LRU, data clock (`X-TSDB-Now`, `catalog.now`), the slewed live delay |
| `testing/` | Node-only helpers (excluded from `tsc`): TSV3/TSD3/TSP3 encoders and synthetic data, a mock server (live and static layouts, gzip, optional HTTP/2, holds, request log), an illisis-export loader, `serve.ts`, `bench.ts`, `browsercheck.ts` |

## How a frame works

1. Level: `round(log2(scale * dpr))`, limited to `[-10, 0]` and snapped to the nearest level
   in the datastore's `levels` (the finer one on a tie), so gaps in `levels` never select a
   level without tiles.
2. Versions: live server and latest: `view` for the visible rectangle plus a one-tile ring
   (one request per level for the tiles without an answer, then `tile` events; every `hello`
   on the event stream re-requests them, since events before the subscription or during a
   reconnect are missed); history, or any time on a static export: the day's `timeline`
   (cached; neighbouring days are fetched when idle, the next day during timelapse). For
   latest, or any time after the newest data, a loaded timeline of a later day also answers
   (its `v0` is the version at the newest data), so the timeline of today that start-up
   fetches on a static export is used even when the newest data is days old.
3. For each visible tile at its wanted version: draw its texture, else (in this order) an
   older version of the same tile, its nearest loaded ancestor (sub-rectangle, scaled up) or
   loaded descendants (one and two levels down, scaled down). Tiles with no data count as
   complete. A wanted version that answers 404 counts as complete for 30 s, and the older
   data is still drawn.
4. Requests: visible tiles (by distance from the centre), then the ring, then the parent level
   (only for tiles with nothing loaded at all, and only after the visible level is resolved).
   Requests for tiles that left the view are aborted. A z0 tile whose older committed version
   is loaded moves forward with `tiledelta` (`texSubImage2D` on 32x32 chunks); static exports
   chain consecutive deltas (up to 4).
5. While playing a timelapse a tile keeps its in-flight load (no restart on every step) and
   continues from it with a delta; scrubbing replaces it.
6. Textures live in a GPU-byte LRU (default 160 MiB); what the frame draws is pinned.

Requests are planned in a microtask as soon as data arrives (not on the next animation
frame); drawing happens in `requestAnimationFrame`. The live event stream opens after the
first full paint (on HTTP/1.1 it would hold one of the six connections), and so do the
layers (`MapLayer.attach`): their mover and overlay requests would take connections the
first tiles need. `ServerSource` shares one event stream between all subscriptions (the map's
datastore and the movers layer's surface), reopened when the set of topics changes. A
subscriber whose topics the open stream already covers gets the last `hello` again while it
is up, or `error` while it is down. A handler that throws is logged and reported to its own
subscriber only (`error`, then the last `hello` so it resyncs); the stream stays open for the
others. In live mode the displayed time trails the data clock by the largest
`MapLayer.delay()`, or `liveDelayMs` before a layer reports one (default: the movers layer's
starting delay, 2 s, or 2 x `live_ms` for static exports). Changes of that target are slewed
(`delay.ts`: the displayed time runs at 0.8x to 1.05x), so attaching or turning off a layer
does not make it jump.

## Using it

```ts
import { boot, takeEarly } from "./map/boot";
import { MapView, defaultSelection } from "./map/view";
import { layers } from "./layers/index";

const { source, catalog, seed } = await boot(location.search, { width: innerWidth, height: innerHeight, dpr: devicePixelRatio }, { early: takeEarly() });
const view = new MapView({ container, source, catalog, layers, viewSeed: seed, onState: s => render(s) });
view.select(defaultSelection(catalog), { fit: true });
view.setTime(t); view.goLive(); view.play(3_600_000); view.setLayers(["movers"]);
```

- Any `DataSource` works. One that also implements `UrlSource` (both sources here do) gets
  its tile bodies fetched by URL.
- Renderer: `renderer: "auto" | "webgl2" | "canvas2d"` (page: `?renderer=canvas2d`). In
  `auto`, a WebGL2 renderer that fails to initialise (shader or link error, context lost at
  start-up) is replaced by Canvas2D on a new canvas element; if that fails too, the app shows
  an error notice.
- Loader: direct fetches for WebGL2 and the worker pool for Canvas2D (page:
  `?loader=worker|direct`). With WebGL2 direct fetches start faster: the browser decompresses
  off the main thread anyway, the main thread only uploads an index texture, and the worker
  scripts cost requests during start-up. With Canvas2D the workers also expand pixels to
  RGBA.
- Page URL state: the datastore as `ds` (a catalog datastore id, e.g.
  `gw-bc41437d~2E1.nauvis.player`, as the bench suite passes it) or as `map`, `surface`,
  `force`; the camera as `x`, `y` and `s` (CSS px per tile) or `z` (a level: 2^z px per
  tile); `t` (absent = live); `layers`; plus `source=static|server`, `data=<base>`,
  `token=<read token>`. `ds` wins over `map`/`surface`/`force` and `s` over `z`
  (`boot.ts` `urlDatastore`, `urlScale`; the `index.html` early start applies the same
  rule). The URL is written back as `map`/`surface`/`force`/`s`.
- `window.__tsdbMap` (tests, benchmarks): `setCamera(x, y, s)`, `setTime(t)`, `goLive()`,
  `play(speed)`, `pause()`, `fit()`, `state()`, `view` (the `MapView`;
  `view.readTerrainPixels()` reads back the drawn terrain).
- `window.__tsdbControl`: the `TsdbControl` contract (`web/src/api/types.ts`) that the client
  benchmark (`bench/web`) drives, adapted from `MapView` in `control.ts`. Its scale is world
  tiles per CSS pixel (the inverse of the camera's) and its timelapse speed is data ms per browser
  ms; `seek` records a `time.change` perf event.
- Perf counters (`window.__tsdbPerf.counters`): `textureBytes` is set to the bytes the texture
  LRU holds on every frame (absolute, so it is right again after `reset()`);
  `tilesFromCache` counts visible tiles shown from an already loaded texture, once per view
  change (pan, zoom or time step), without a request. HTTP cache hits are not detected (they
  count as `tilesRequested`/`tilesLoaded`).
- Static exports ([internal/publish](../../../internal/publish/README.md)): set
  `<meta name="tsdb-source" content="static">` in the exported `index.html`. Start-up then fetches `catalog.json` and today's timeline while the bundle
  loads, and never probes `/api/v3/`. Without it the viewer still works (it falls back after
  the `/api/v3/catalog` probe fails), one round trip slower.

## Tests

`npm test` (from `web/`) runs `src/**/*.test.ts` with Node's type stripping:

- `core.test.ts`: camera (including level snapping), LRU, scheduler, clock, lookup tables
  against the fixtures, timeline and view version resolution (later-day timelines answering
  latest, unanswered-only and split `view` requests), the worker loader's fallback;
- `sources.test.ts`: both sources against the mock server (paths, errors, aborts, the data
  clock, static mapping, SSE parsing and live events);
- `terrain.test.ts`: the engine over real HTTP with a recording renderer: first paint order,
  never-black on zoom in and out, versioned refetch with deltas (texture pixels checked), static
  delta chains, aborts, scrub vs timelapse, palette growth, 404 tiles, live tiles, the texture
  budget, datastore switches, no spurious aborts under microtask planning, today's timeline
  answering latest on a static export with old data, a 404 version still drawing the older
  one, the perf counters, one palette fetch across a datastore switch;
- `control.test.ts` and `delay.test.ts`: the `TsdbControl` adapter and the live delay slew;
- `boot.test.ts`: the `index.html` early-start script computes exactly what `boot.ts` would
  (600 random URLs and viewports), and `boot()` adopts it.

Browser checks (need a Chromium and `npm i --no-save --prefix <dir> playwright-core`; build first):

```sh
node src/map/testing/browsercheck.ts --pw <dir>/node_modules/playwright-core --chrome <chrome.exe> [--illisis <export root>]
node src/map/testing/bench.ts --pw <dir>/node_modules/playwright-core --chrome <chrome.exe> --illisis <export root> [--h2] [--static] [--shift-to-now] [--runs 10]
node src/map/testing/serve.ts --illisis <export root>   # browse the viewer against mock data
```

## Known limits

- Screenshots differ from the drawing buffer by up to 13 per channel (compositor colour
  handling). The pixel check compares the drawing buffer.
- Tile fetches by URL rely on the HTTP cache (immutable responses). The Cache API is only used
  by `StaticSource.palette/tile` calls made through the `DataSource` interface.
