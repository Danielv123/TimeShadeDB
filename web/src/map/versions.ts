// Which version of each tile to show (docs/v3/API.md "view" and "timeline").
//
// - TimelineVersions: one timeline per (level, UTC day) answers every tile of that level for
//   any time of the day, so scrubbing and timelapse need no request per step. Used for
//   history on both sources and for "latest" on static exports.
// - ViewVersions: the live server's `view` route at t=latest, by 16x16-tile cells, kept
//   current by live `tile` events (or polling). Carries live content (`live`, `ls`).

import type { CatalogDatastore, DataSource, LiveTileEvent, TimelineResponse, TimelineTile, TimeMs, Version, ViewResponse } from "../api/types";
import { utcDay } from "../api/grid.ts";
import { boundsAtLevel, tileKey, type TileRange } from "./camera.ts";
import { HttpError } from "./sources.ts";

export interface TileVersion {
  v: Version;
  /** Live content (z0, server only): the open window applies on top of `v`. */
  live?: boolean;
  ls?: number;
}

/**
 * `undefined`: not known yet (loading). `null`: the tile has no data at the target
 * (nothing to draw). Otherwise the version to show.
 */
export type VersionAnswer = TileVersion | null | undefined;

export interface VersionResolver {
  /** Version of a tile at the current target. */
  get(z: number, x: number, y: number): VersionAnswer;
  /** Starts loading whatever is needed to answer for the tiles in `range`. */
  ensure(range: TileRange): void;
  /** Re-fetches mutable answers (catalog changed, stream reconnected). */
  refresh(): void;
  dispose(): void;
}

const RETRY_MS = 2000;

function perfNow(): number {
  return typeof performance !== "undefined" ? performance.now() : Date.now();
}
const MAX_TIMELINES = 48;

interface TimelineEntry {
  date: string;
  z: number;
  tiles: Map<string, TimelineTile> | null;
  final: boolean;
  loading: AbortController | null;
  failedAt: number;
  stale: boolean;
}

/** Version of a timeline tile at t: the newest point at or before t, else v0. */
export function timelineVersionAt(tile: TimelineTile, t: TimeMs): Version {
  const pts = tile.points;
  let lo = 0;
  let hi = pts.length - 1;
  let found = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (pts[mid][0] <= t) {
      found = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return found >= 0 ? pts[found][1] : tile.v0;
}

export class TimelineVersions implements VersionResolver {
  private readonly entries = new Map<string, TimelineEntry>();
  private target: TimeMs | "latest" = "latest";
  private ds: CatalogDatastore;
  private disposed = false;
  private readonly source: DataSource;
  private readonly onChange: () => void;
  private readonly now: () => number;

  constructor(source: DataSource, ds: CatalogDatastore, onChange: () => void, now: () => number = perfNow) {
    this.source = source;
    this.ds = ds;
    this.onChange = onChange;
    this.now = now;
  }

  /** Updates the datastore summary (after a catalog refresh). */
  setDatastore(ds: CatalogDatastore): void {
    const moved = ds.horizon !== this.ds.horizon || ds.last !== this.ds.last;
    this.ds = ds;
    if (moved) {
      this.refresh();
    }
  }

  setTarget(t: TimeMs | "latest"): void {
    this.target = t;
  }

  /** The time answers are computed for (latest resolves to the newest committed state). */
  effectiveTime(): TimeMs {
    const top = Math.max(this.ds.horizon, this.ds.last);
    return this.target === "latest" ? top : this.target;
  }

  /** The UTC day whose timeline answers time t. */
  dayFor(t: TimeMs): string | null {
    if (t < this.ds.first) {
      return null;
    }
    const top = Math.max(this.ds.horizon, this.ds.last);
    return utcDay(Math.min(t, top));
  }

  get(z: number, x: number, y: number): VersionAnswer {
    const t = this.effectiveTime();
    const day = this.dayFor(t);
    if (day === null) {
      return null;
    }
    const e = this.entries.get(`${z}/${day}`);
    if (!e || !e.tiles) {
      return undefined;
    }
    const tile = e.tiles.get(tileKey(z, x, y));
    if (!tile) {
      return null;
    }
    const v = timelineVersionAt(tile, t);
    return v === 0 ? null : { v };
  }

  ensure(range: TileRange): void {
    const day = this.dayFor(this.effectiveTime());
    if (day !== null) {
      this.load(range.z, day);
    }
  }

  /** Loads the timeline of another day ahead of time (timelapse crossing midnight). */
  prefetch(z: number, t: TimeMs): void {
    const day = this.dayFor(t);
    if (day !== null) {
      this.load(z, day);
    }
  }

  /** Whether the timeline for (z, day of t) is loaded. */
  loaded(z: number, t: TimeMs): boolean {
    const day = this.dayFor(t);
    return day === null || !!this.entries.get(`${z}/${day}`)?.tiles;
  }

  refresh(): void {
    for (const e of this.entries.values()) {
      if (!e.final) {
        e.stale = true;
        this.fetch(e);
      }
    }
  }

  dispose(): void {
    this.disposed = true;
    for (const e of this.entries.values()) {
      e.loading?.abort();
    }
    this.entries.clear();
  }

  private load(z: number, day: string): void {
    const key = `${z}/${day}`;
    let e = this.entries.get(key);
    if (!e) {
      e = { date: day, z, tiles: null, final: false, loading: null, failedAt: -Infinity, stale: false };
      this.entries.set(key, e);
      this.trim();
    } else {
      // Mark recently used.
      this.entries.delete(key);
      this.entries.set(key, e);
    }
    if ((!e.tiles || e.stale) && !e.loading && this.now() - e.failedAt >= RETRY_MS) {
      this.fetch(e);
    }
  }

  private fetch(e: TimelineEntry): void {
    if (e.loading || this.disposed) {
      return;
    }
    const ctrl = new AbortController();
    e.loading = ctrl;
    this.source.timeline(this.ds.id, e.z, e.date, ctrl.signal).then(
      (res: TimelineResponse) => {
        e.loading = null;
        if (this.disposed) {
          return;
        }
        const tiles = new Map<string, TimelineTile>();
        for (const t of res.tiles) {
          tiles.set(tileKey(e.z, t.x, t.y), t);
        }
        e.tiles = tiles;
        e.final = res.final;
        e.stale = false;
        this.onChange();
      },
      (err: unknown) => {
        e.loading = null;
        if (this.disposed || (err as Error)?.name === "AbortError") {
          return;
        }
        if (err instanceof HttpError && err.status === 404) {
          // No timeline for that day: nothing at that level.
          e.tiles = new Map();
          e.final = false;
          e.stale = false;
          this.onChange();
          return;
        }
        e.failedAt = this.now();
        // Try again on a later ensure().
        setTimeout(() => this.onChange(), RETRY_MS);
      },
    );
  }

  private trim(): void {
    while (this.entries.size > MAX_TIMELINES) {
      const [k, e] = this.entries.entries().next().value as [string, TimelineEntry];
      e.loading?.abort();
      this.entries.delete(k);
    }
  }
}

/** Most tiles one `view` request may cover (API.md: at most 4,096). */
export const VIEW_MAX_TILES = 4096;

/**
 * Live-server versions at t=latest. One `view` request covers the whole wanted rectangle
 * (clipped to the datastore bounds) whenever it holds tiles with no answer yet, so a first
 * paint costs one request per level. Answers are kept per tile and updated by live events;
 * refresh() marks them stale, and stale tiles are re-requested while still answering from
 * the old data.
 */
export class ViewVersions implements VersionResolver {
  private readonly states = new Map<string, TileVersion>();
  /** tile key -> refresh generation of the answer that covered it. */
  private readonly known = new Map<string, number>();
  /** tile key -> generation of an in-flight request covering it. */
  private readonly pending = new Map<string, number>();
  private readonly loading = new Set<AbortController>();
  private refreshGen = 0;
  /** Live events: tile key -> event sequence, so an older `view` answer never overwrites a newer event. */
  private readonly touched = new Map<string, number>();
  private eventSeq = 0;
  private failedAt = -Infinity;
  private disposed = false;
  private readonly source: DataSource;
  private ds: CatalogDatastore;
  private readonly onChange: () => void;
  private readonly now: () => number;

  constructor(source: DataSource, ds: CatalogDatastore, onChange: () => void, now: () => number = perfNow) {
    if (!source.view) {
      throw new Error("ViewVersions needs a source with view()");
    }
    this.source = source;
    this.ds = ds;
    this.onChange = onChange;
    this.now = now;
  }

  setDatastore(ds: CatalogDatastore): void {
    this.ds = ds;
  }

  get(z: number, x: number, y: number): VersionAnswer {
    const key = tileKey(z, x, y);
    const s = this.states.get(key);
    if (s) {
      return s.v === 0 && !s.live ? null : s;
    }
    if (this.known.has(key)) {
      return null;
    }
    // Outside the datastore bounds nothing exists.
    const b = boundsAtLevel(this.ds.bounds, z);
    return x < b[0] || x > b[2] || y < b[1] || y > b[3] ? null : undefined;
  }

  ensure(range: TileRange): void {
    if (this.now() - this.failedAt < RETRY_MS) {
      return;
    }
    const b = boundsAtLevel(this.ds.bounds, range.z);
    const x0 = Math.max(range.x0, b[0]);
    const y0 = Math.max(range.y0, b[1]);
    const x1 = Math.min(range.x1, b[2]);
    const y1 = Math.min(range.y1, b[3]);
    if (x0 > x1 || y0 > y1) {
      return;
    }
    // Rows with tiles that need an answer.
    let need = false;
    for (let y = y0; y <= y1 && !need; y++) {
      for (let x = x0; x <= x1; x++) {
        const key = tileKey(range.z, x, y);
        const k = this.known.get(key);
        const p = this.pending.get(key);
        if ((k === undefined || k < this.refreshGen) && (p === undefined || p < this.refreshGen)) {
          need = true;
          break;
        }
      }
    }
    if (!need) {
      return;
    }
    const w = x1 - x0 + 1;
    const rows = Math.max(1, Math.floor(VIEW_MAX_TILES / w));
    for (let ry = y0; ry <= y1; ry += rows) {
      this.fetch(range.z, [x0, ry, Math.min(x1, x0 + Math.min(w, VIEW_MAX_TILES) - 1), Math.min(y1, ry + rows - 1)]);
    }
  }

  /**
   * Seeds answers from a `view` response for `bbox` fetched elsewhere (the speculative first
   * request in boot.ts). Tiles already answered are left alone.
   */
  seed(res: ViewResponse, bbox: [number, number, number, number]): void {
    if (res.ds !== this.ds.id) {
      return;
    }
    const z = res.z;
    const listed = new Map(res.tiles.map(t => [tileKey(z, t.x, t.y), t] as const));
    for (let y = bbox[1]; y <= bbox[3]; y++) {
      for (let x = bbox[0]; x <= bbox[2]; x++) {
        const k = tileKey(z, x, y);
        if (this.known.has(k) || this.touched.has(k)) {
          continue;
        }
        const t = listed.get(k);
        if (t) {
          this.states.set(k, { v: t.v, live: t.live || undefined, ls: t.live ? t.ls : undefined });
        }
        this.known.set(k, this.refreshGen);
      }
    }
    this.onChange();
  }

  /** Applies a live `tile` event. */
  apply(e: LiveTileEvent): boolean {
    if (e.ds !== this.ds.id) {
      return false;
    }
    const key = tileKey(e.z, e.x, e.y);
    const old = this.states.get(key);
    const next: TileVersion = { v: e.v, live: e.live || undefined, ls: e.live ? e.ls : undefined };
    if (old && old.v === next.v && !!old.live === !!next.live && old.ls === next.ls) {
      return false;
    }
    this.states.set(key, next);
    this.touched.set(key, ++this.eventSeq);
    this.onChange();
    return true;
  }

  refresh(): void {
    this.refreshGen++;
    this.onChange();
  }

  dispose(): void {
    this.disposed = true;
    for (const c of this.loading) {
      c.abort();
    }
    this.loading.clear();
    this.states.clear();
    this.known.clear();
    this.pending.clear();
    this.touched.clear();
  }

  private fetch(z: number, bbox: [number, number, number, number]): void {
    const gen = this.refreshGen;
    const seq = this.eventSeq;
    const [x0, y0, x1, y1] = bbox;
    for (let y = y0; y <= y1; y++) {
      for (let x = x0; x <= x1; x++) {
        this.pending.set(tileKey(z, x, y), gen);
      }
    }
    const ctrl = new AbortController();
    this.loading.add(ctrl);
    const clear = (): void => {
      for (let y = y0; y <= y1; y++) {
        for (let x = x0; x <= x1; x++) {
          const k = tileKey(z, x, y);
          if (this.pending.get(k) === gen) {
            this.pending.delete(k);
          }
        }
      }
    };
    this.source.view!(this.ds.id, z, bbox, "latest", ctrl.signal).then(
      res => {
        this.loading.delete(ctrl);
        if (this.disposed) {
          return;
        }
        clear();
        // Tiles missing from the answer have no data.
        for (let y = y0; y <= y1; y++) {
          for (let x = x0; x <= x1; x++) {
            const k = tileKey(z, x, y);
            if ((this.known.get(k) ?? -1) <= gen && (this.touched.get(k) ?? 0) <= seq) {
              this.states.delete(k);
              this.known.set(k, gen);
            }
          }
        }
        for (const t of res.tiles) {
          const k = tileKey(z, t.x, t.y);
          if ((this.touched.get(k) ?? 0) <= seq) {
            this.states.set(k, { v: t.v, live: t.live || undefined, ls: t.live ? t.ls : undefined });
          }
        }
        this.onChange();
      },
      (err: unknown) => {
        this.loading.delete(ctrl);
        clear();
        if (this.disposed || (err as Error)?.name === "AbortError") {
          return;
        }
        this.failedAt = this.now();
        setTimeout(() => this.onChange(), RETRY_MS);
      },
    );
  }
}
