# Viewer terrain engine (WP7)

The map canvas of the v3 viewer: WebGL2 palette-index textures (Canvas2D fallback), a global
request scheduler, version resolution from `view`/`timeline`, never-black fallbacks, versioned
texture caching and tile deltas. Contracts: `docs/v3/API.md`, `docs/v3/FORMAT.md` 7,
`web/src/api/types.ts` (`DataSource`, perf hooks) and `web/src/map/layer.ts` (`MapLayer`).

## Files

| File | Role |
|---|---|
| `view.ts` | `MapView`: canvases, pan/zoom/pinch/keys, the frame loop, time (live, scrub, timelapse), catalog polling, live events, the `MapLayer` list |
| `terrain.ts` | `TerrainEngine` (DOM-free): level and tile choice, version lookup, scheduling, texture LRU, fallbacks, deltas, palette, perf events |
| `versions.ts` | `TimelineVersions` (one `timeline` per level and UTC day answers any time of that day) and `ViewVersions` (live server, `view?t=latest` plus `tile` events) |
| `scheduler.ts` | Priority scheduler: at most 16 requests in flight (6 on HTTP/1.1), lowest priority first, unwanted requests dropped or aborted |
| `sources.ts` | `ServerSource` (`/api/v3/`, fetch-based SSE with bearer token) and `StaticSource` (export file names, client-side overlay filter); both expose request URLs (`UrlSource`) |
| `boot.ts` | Start-up: source detection and the speculative first `view`/`timeline` request; adopts the requests `index.html` starts before the bundle loads |
| `loader.ts`, `tile_worker.ts` | `DirectLoader` (main-thread fetch) and `WorkerLoader` (pool of 2-4 workers; expands tiles to RGBA for Canvas2D) |
| `render_gl.ts` | WebGL2: R8UI (pal8) / R16UI (pal16, rgb565) textures, 256x256 RGBA palette texture, RGB565 expanded in the shader |
| `render_2d.ts` | Canvas2D: 512x512 RGBA canvas per tile through a 65,536-entry lookup table |
| `renderer.ts` | Renderer interface, lookup tables, pixel snapping |
| `camera.ts`, `lru.ts`, `clock.ts` | Camera math, byte-bounded LRU, data clock (`X-TSDB-Now`, `catalog.now`) |
| `testing/` | Node-only helpers (excluded from `tsc`): TSV3/TSD3/TSP3 encoders and synthetic data, a mock v3 server (live and static layouts, gzip, optional HTTP/2, holds, request log), an illisis-export loader, `serve.ts`, `bench.ts`, `browsercheck.ts` |

## How a frame works

1. Level: `round(log2(scale * dpr))`, clamped to `[min level of the datastore, 0]`.
2. Versions: live server and latest: `view` for the visible rectangle plus a one-tile ring
   (one request per level, then `tile` events); history, or any time on a static export: the
   day's `timeline` (cached; neighbouring days are fetched when idle, the next day during
   timelapse).
3. For each visible tile at its wanted version: draw its texture, else (in this order) an
   older version of the same tile, its nearest loaded ancestor (sub-rectangle, scaled up) or
   loaded descendants (one and two levels down, scaled down). Tiles with no data count as
   complete.
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
first full paint (on HTTP/1.1 it would hold one of the six connections).

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

- Any `DataSource` works; one that also implements `UrlSource` (both sources here do) gets its
  tile bodies fetched by URL. A Clusterio adapter (WP10) can use `new ServerSource({ base:
  "/api/timeshade/api/v3/" })` behind the plugin's auth proxy, or implement `DataSource`.
- Renderer: `renderer: "auto" | "webgl2" | "canvas2d"` (page: `?renderer=canvas2d`). Loader:
  default is direct fetches for WebGL2 and the worker pool for Canvas2D (page:
  `?loader=worker|direct`); see the measurements below for why.
- Page URL state: `map`, `surface`, `force`, `x`, `y`, `s` (CSS px per tile), `t` (absent =
  live), `layers`, plus `source=static|server`, `data=<base>`, `token=<read token>`.
- `window.__tsdbMap` (tests, benchmarks): `setCamera(x, y, s)`, `setTime(t)`, `goLive()`,
  `play(speed)`, `pause()`, `fit()`, `state()`, `view` (the `MapView`;
  `view.readTerrainPixels()` reads back the drawn terrain).
- Static exports (WP12): set `<meta name="tsdb-source" content="static">` in the exported
  `index.html`. Start-up then fetches `catalog.json` and today's timeline while the bundle
  loads, and never probes `/api/v3/`. Without it the viewer still works (it falls back after
  the `/api/v3/catalog` probe fails), one round trip slower.

## Tests

`npm test` (from `web/`) runs `src/**/*.test.ts` with Node's type stripping:

- `core.test.ts`: camera, LRU, scheduler, clock, lookup tables against the fixtures, timeline
  and view version resolution;
- `sources.test.ts`: both sources against the mock server (paths, errors, aborts, the data
  clock, static mapping, SSE parsing and live events);
- `terrain.test.ts`: the engine over real HTTP with a recording renderer: first paint order,
  never-black on zoom in and out, versioned refetch with deltas (texture pixels checked), static
  delta chains, aborts, scrub vs timelapse, palette growth, 404 tiles, live tiles, the texture
  budget, datastore switches, no spurious aborts under microtask planning;
- `boot.test.ts`: the `index.html` early-start script computes exactly what `boot.ts` would
  (600 random URLs and viewports), and `boot()` adopts it.

Browser checks (need a Chromium and `npm i --no-save --prefix <dir> playwright-core`; build first):

```sh
node src/map/testing/browsercheck.ts --pw <dir>/node_modules/playwright-core --chrome <chrome.exe> [--illisis <export root>]
node src/map/testing/bench.ts --pw <dir>/node_modules/playwright-core --chrome <chrome.exe> --illisis <export root> [--h2] [--static] [--shift-to-now] [--runs 10]
node src/map/testing/serve.ts --illisis <export root>   # browse the viewer against mock data
```

## Measurements (2026-10-19)

Windows 11, 32 cores, NVIDIA RTX 5090 (ANGLE D3D11), Chromium 1223 headless, Node 26.7. Data:
the illisis test export, nauvis, newest 4 snapshots per area as versions (35 z0 tiles, levels
0 to -4, palette 149), served by the mock server with gzip. 1920x1080, dpr 1, nauvis centre at
z0 (15 visible tiles). CDP network emulation 40 ms latency and 50 Mbit/s. "Cold" = no HTTP
cache, new browser context; "warm" = reload with cache. 15 runs each, data shifted to "now".

| Case | Cold first full paint p50 / p95 | Warm p50 | Target (PLAN §5) |
|---|---|---|---|
| live server, HTTP/1.1 | 377 / 393 ms | 214 ms | p50 ≤ 400, p95 ≤ 900: met |
| live server, HTTP/2 | 296 / 328 ms | 249 ms | met |
| static export, HTTP/1.1 | 383 / 395 ms | 223 ms | met |
| static export, HTTP/2 | 290 / 316 ms | 250 ms | met (see the stall note) |

Before the start-up work (same harness, HTTP/1.1, worker loader): 600 ms p50. Steps: speculative
first `view` with the catalog (518), direct loader (472), `index.html` early start (412),
microtask planning, delayed event stream and lazy logo (377-393).

Other rows (HTTP/1.1 unless noted):

| Metric | Result | Target |
|---|---|---|
| Zoom out to the whole surface: fallback painted | 2-4 ms (descendants drawn at once) | ≤ 100 ms: met |
| Zoom out: full (first time, coarse tiles cold) | 164-226 ms | ≤ 500 ms p95: met |
| Time scrub step on the imported data (12 steps across version changes) | p50 2 ms, p95 69-71 ms; ~1 KB per changed tile (tiledelta) instead of 262 KB | p95 ≤ 150 ms, bytes ≤ changed tiles: met |
| Scrub worst case: synthetic, all 15 visible tiles change every step | HTTP/1.1 p95 188 ms (three request waves of six), HTTP/2 p95 87 ms; 42 KB per step | HTTP/1.1 misses 150 ms |
| Timelapse at 1 h/s, 10 s | 125 fps (headless frame rate), frame work p95 0.2-0.5 ms, 0 long tasks; synthetic dense history: 118 fps (HTTP/1.1, 714 deltas) and 83 fps (HTTP/2, 1,255 deltas) | ≥ 55 fps, ≤ 2 long tasks: met |
| Memory after the scenario | JS heap 4-9 MiB, textures 16-18 MiB (48 MiB synthetic) | ≤ 300 MB: met |
| Canvas2D fallback, cold first paint | 424-440 ms p50 (HTTP/1.1, before the last start-up changes) | |
| Renderer output | bit-exact against the expected RGB565 expansion: 0 of 897,280 pixels differ in 8 cases (WebGL2/Canvas2D x server/static x z0/z-1), real and synthetic data | |

Loader choice: with WebGL2, direct fetches were faster than the worker pool (cold p50 472 vs
518 ms on HTTP/1.1, 372 vs 397 ms on HTTP/2): the browser decompresses off the main thread
anyway, the main thread only uploads an index texture, and four worker scripts cost requests
during start-up. The pool is kept for Canvas2D, where it expands pixels to RGBA.

Known limits and observations:

- Under emulated 50 Mbit throttling on HTTP/2 about 1 in 30 cold loads stalled ~4.0 s: the
  server log shows the tile responses written at once, and the browser received them 4 s
  later. It did not happen in 90 HTTP/2 loads without bandwidth throttling (with or without
  latency), nor on HTTP/1.1, so it looks like an artefact of Chrome's throttling with the Node
  `http2` mock, not of the viewer. Once, early on, an HTTP/1.1 run timed out (30 s) waiting for
  first paint; it did not recur in more than 200 later loads, and start-up now retries a
  catalog request that fails with a network error.
- The numbers are against the mock server, not WP6's Go server and an imported database (not
  available yet). Response compression and HTTP/2 on the real server decide the transfer part.
- Screenshots differ from the drawing buffer by up to 13 per channel (compositor colour
  handling); the pixel check compares the drawing buffer.
- Tile fetches by URL rely on the HTTP cache (immutable responses); the Cache API is only used
  by `StaticSource.palette/tile` calls made through the `DataSource` interface.
