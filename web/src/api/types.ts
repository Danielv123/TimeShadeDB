// TimeShadeDB v3 API contract for the viewer (WP0). Mirrors internal/v3/core/apitypes.go,
// internal/v3/core/movers.go and overlays.go field for field; docs/v3/API.md and
// docs/v3/FORMAT.md are the normative descriptions. Change Go and TypeScript together.

/** Unix milliseconds on the map clock. */
export type TimeMs = number;
/** A tile version: 0 is all uncharted; content of (ds, tile, version) never changes. */
export type Version = number;
/** [x1, y1, x2, y2] in world tiles. */
export type Quad = [number, number, number, number];

export interface DatastoreKey {
  map: string;
  surface: string;
  force: string;
}

export interface SurfaceKey {
  map: string;
  surface: string;
}

/** A tile at level z <= 0 (FORMAT.md 2). */
export interface TileCoord {
  z: number;
  x: number;
  y: number;
}

// ---------------------------------------------------------------------------------------
// Catalog (GET catalog)

export interface InstanceInfo {
  id: string;
  name: string;
  bounds?: Quad;
}

export interface MapInfo {
  key: string;
  title: string;
  tile_size?: number;
  instances: InstanceInfo[];
  edges?: Record<string, Quad[]>;
}

export interface CatalogDatastore {
  force: string;
  /** Datastore id, used in terrain and overlay routes. */
  id: string;
  window_ms: number;
  horizon: TimeMs;
  first: TimeMs;
  last: TimeMs;
  palette_version: number;
  /** z0 tile bounds [minX, minY, maxX, maxY], inclusive. */
  bounds: [number, number, number, number];
  /** Levels with tiles, from 0 down. */
  levels: number[];
}

export interface CatalogMovers {
  horizon: TimeMs;
  first: TimeMs;
  last: TimeMs;
  days: string[];
}

export interface CatalogSurface {
  name: string;
  /** Surface id, used in mover routes. */
  id: string;
  edges: Quad[];
  datastores: CatalogDatastore[];
  movers: CatalogMovers | null;
}

export interface CatalogMap extends MapInfo {
  surfaces: CatalogSurface[];
}

export interface CatalogIcons {
  png: string;
  json: string;
}

export interface Catalog {
  version: number;
  now: TimeMs;
  static: boolean;
  live_ms: number;
  maps: CatalogMap[];
  icons: CatalogIcons | null;
}

// ---------------------------------------------------------------------------------------
// Terrain

export interface TileState {
  v: Version;
  live?: boolean;
  ls?: number;
}

export interface ViewTile extends TileState {
  x: number;
  y: number;
}

/** GET view/{ds}/{z}?bbox=&t= (live server only). */
export interface ViewResponse {
  ds: string;
  z: number;
  t: TimeMs;
  horizon: TimeMs;
  tiles: ViewTile[];
}

export interface TimelineTile {
  x: number;
  y: number;
  /** Version effective at the start of the day. */
  v0: Version;
  /** [t, version] for versions that became effective during the day. */
  points: [TimeMs, Version][];
}

/** GET timeline/{ds}/{z}/{date}.json. */
export interface TimelineResponse {
  ds: string;
  z: number;
  date: string;
  start: TimeMs;
  final: boolean;
  tiles: TimelineTile[];
}

/** TSV3/TSD3 pixel formats. */
export const TileFormat = { Pal8: 1, Pal16: 2, RGB565: 3 } as const;
export type TileFormat = (typeof TileFormat)[keyof typeof TileFormat];

export interface TileHeader {
  format: TileFormat;
  z: number;
  x: number;
  y: number;
  version: Version;
  live: boolean;
  /** Smallest palette length that decodes the tile; 0 for rgb565. */
  paletteVersion: number;
}

/** A decoded TSV3 tile. `pixels` are palette indices (pal8/pal16) or RGB565 (rgb565), 512x512 row-major. */
export interface DecodedTile {
  header: TileHeader;
  pixels: Uint8Array | Uint16Array;
}

export interface DeltaHeader {
  format: TileFormat;
  x: number;
  y: number;
  from: Version;
  to: Version;
  paletteVersion: number;
  count: number;
}

/** A decoded TSD3 delta: per changed chunk its block (0..255) and 1024 pixels. */
export interface DecodedDelta {
  header: DeltaHeader;
  entries: { block: number; pixels: Uint8Array | Uint16Array }[];
}

// ---------------------------------------------------------------------------------------
// Movers

export type MoverKind = "t" | "v" | "p";

export interface MoverMeta {
  id: string;
  k: MoverKind;
  f: string;
  name: string;
  n?: number;
  o?: string;
  c?: string;
}

/** Row flags (FORMAT.md 7.4). */
export const MoverFlag = {
  Stop: 1 << 0,
  Gone: 1 << 1,
  Handoff: 1 << 2,
  EndSwitch: 1 << 3,
  NoTick: 1 << 4,
  NoHeading: 1 << 5,
} as const;

export interface Carriage {
  x: number;
  y: number;
  o: number;
}

export interface MoverRow {
  /** Index into MoverWindow.movers. */
  mover: number;
  t: TimeMs;
  /** Game tick; 0 when flags has NoTick. */
  tick: number;
  x: number;
  y: number;
  /** Orientation 0..1 (0 north, 0.25 east). */
  o: number;
  flags: number;
  carriages: Carriage[] | null;
}

export const WindowKind = { Closed: 1, Keyframe: 2, Live: 3 } as const;
export type WindowKind = (typeof WindowKind)[keyof typeof WindowKind];

/** A decoded MVW3 body. Rows are sorted by (mover, t). */
export interface MoverWindow {
  kind: WindowKind;
  start: TimeMs;
  len: number;
  movers: MoverMeta[];
  rows: MoverRow[];
}

export interface WindowRef {
  start: TimeMs;
  first: TimeMs;
  last: TimeMs;
  rows: number;
}

/** GET movers/{sfc}/day/{date}.json. */
export interface DayIndex {
  date: string;
  windows: WindowRef[];
  keyframes: TimeMs[];
  final: boolean;
}

// ---------------------------------------------------------------------------------------
// Overlays

export type IntervalKind = "tag" | "recipe" | "rail";

export interface Interval {
  kind: IntervalKind;
  key: string;
  instance: number;
  start: TimeMs;
  /** 0 = still open. */
  end: TimeMs;
  x: number;
  y: number;
  icon?: string;
  text?: string;
  recipe?: string;
  quality?: string;
  name?: string;
  dir?: number;
}

/** GET overlays/{ds}/{x}/{y}.json. */
export interface OverlayResponse {
  ds: string;
  x: number;
  y: number;
  version: number;
  intervals: Interval[];
}

// ---------------------------------------------------------------------------------------
// Live events (GET live, server only)

export interface LiveHello {
  now: TimeMs;
  horizon: Record<string, TimeMs>;
}

export interface LiveTileEvent {
  ds: string;
  z: number;
  x: number;
  y: number;
  v: Version;
  live?: boolean;
  ls?: number;
  now: TimeMs;
}

export interface LiveHorizonEvent {
  ds?: string;
  sfc?: string;
  horizon: TimeMs;
  now: TimeMs;
}

/** [entry, t, tick | null, x, y, o, flags, carriages | null]; entry indexes `movers`. */
export type LiveMoverRow = [number, TimeMs, number | null, number, number, number, number, [number, number, number][] | null];

export interface LiveMoversEvent {
  sfc: string;
  now: TimeMs;
  movers: MoverMeta[];
  rows: LiveMoverRow[];
}

export interface LiveHandlers {
  hello?(e: LiveHello): void;
  tile?(e: LiveTileEvent): void;
  horizon?(e: LiveHorizonEvent): void;
  movers?(e: LiveMoversEvent): void;
  catalog?(e: { now: TimeMs }): void;
  /** The stream dropped; the source reconnects by itself and calls hello again. */
  error?(err: unknown): void;
}

export interface ErrorResponse {
  error: string;
  code: "bad_request" | "bad_tile" | "not_found" | "unauthorized" | "too_large" | "overloaded" | "internal";
}

// ---------------------------------------------------------------------------------------
// Data sources (implemented by WP7 in web/src/map/; WP8 reads movers and overlays through it)

/**
 * Where the viewer reads data from: the live server (`/api/v3/`) or a static export
 * (API.md 6). Binary methods return the raw body (already decompressed by the browser
 * through Content-Encoding) so workers can decode it with web/src/api/wire.ts.
 * Every method rejects with an AbortError when its signal aborts.
 */
export interface DataSource {
  readonly kind: "server" | "static";
  /** The data clock: map-clock ms now, kept from catalog.now and X-TSDB-Now headers. */
  now(): TimeMs;
  catalog(signal?: AbortSignal): Promise<Catalog>;
  palette(ds: string, version: number, signal?: AbortSignal): Promise<ArrayBuffer>;
  /** Live server only; static sources leave it undefined and callers use timeline(). */
  view?(ds: string, z: number, bbox: [number, number, number, number], t: TimeMs | "latest", signal?: AbortSignal): Promise<ViewResponse>;
  timeline(ds: string, z: number, date: string, signal?: AbortSignal): Promise<TimelineResponse>;
  /** "live" only on the live server, z = 0. */
  tile(ds: string, tile: TileCoord, version: Version | "live", signal?: AbortSignal): Promise<ArrayBuffer>;
  tileDelta(ds: string, x: number, y: number, from: Version, to: Version, signal?: AbortSignal): Promise<ArrayBuffer>;
  moverDay(sfc: string, date: string, signal?: AbortSignal): Promise<DayIndex>;
  moverWindow(sfc: string, start: TimeMs, signal?: AbortSignal): Promise<ArrayBuffer>;
  moverKeyframe(sfc: string, t: TimeMs, signal?: AbortSignal): Promise<ArrayBuffer>;
  moverLive(sfc: string, signal?: AbortSignal): Promise<ArrayBuffer>;
  overlays(ds: string, x: number, y: number, range?: { t0?: TimeMs; t1?: TimeMs; kinds?: IntervalKind[] }, signal?: AbortSignal): Promise<OverlayResponse>;
  /** Live server only. Returns an unsubscribe function. */
  subscribe?(topics: { ds: string[]; sfc: string[] }, handlers: LiveHandlers): () => void;
}

// ---------------------------------------------------------------------------------------
// Perf hooks (window.__tsdbPerf), read by the WP11 Playwright suite. See docs/v3/README.md.

export type PerfEventName =
  | "tile.request"
  | "tile.loaded"
  | "tile.drawn"
  | "tile.abort"
  | "view.fallback"
  | "view.full"
  | "time.change"
  | "timelapse.frame"
  | "movers.frame"
  | "longtask";

export interface PerfEvent {
  name: PerfEventName;
  /** performance.now() ms. */
  t: number;
  detail?: Record<string, number | string | boolean>;
}

export interface PerfCounters {
  tilesRequested: number;
  tilesLoaded: number;
  tilesAborted: number;
  tilesFromCache: number;
  bytesFetched: number;
  /** Current GPU texture bytes held by the texture LRU. */
  textureBytes: number;
  framesDrawn: number;
  longTasks: number;
}

export interface TsdbPerf {
  version: 1;
  /** performance.now() of the first view.full, or null. */
  firstFullPaint: number | null;
  /** performance.now() of the latest view.full, or null. */
  lastFullPaint: number | null;
  /** performance.now() of the latest view.fallback, or null. */
  lastFallbackPaint: number | null;
  counters: PerfCounters;
  /** Ring buffer of the most recent events (at most 5000, oldest first). */
  events: PerfEvent[];
  /** Clears events and counters (not firstFullPaint), for measuring one interaction. */
  reset(): void;
}

declare global {
  interface Window {
    __tsdbPerf?: TsdbPerf;
  }
}
