// The terrain engine (PLAN.md §4.7), DOM-free: per frame it picks the level and visible
// tiles, resolves the version each tile should show, schedules loads through the global
// scheduler, keeps textures in a GPU-byte LRU and returns the draw list. While a tile loads
// it draws an older version of the same tile, its nearest loaded ancestor (scaled up) or its
// loaded descendants (scaled down), so the screen never goes black.

import type { Catalog, CatalogDatastore, DataSource, LiveTileEvent, TimeMs } from "../api/types";
import type { Camera, Viewport } from "./layer";
import { decodePalette, decodeTile, decodeTileDelta } from "../api/wire.ts";
import { tileSpan } from "../api/grid.ts";
import { perfCount, perfEvent } from "../perf.ts";
import {
  ancestorSubRect,
  boundsAtLevel,
  intersectRange,
  levelFor,
  rangeContains,
  tileKey,
  tileRange,
  tileWorldRect,
  visibleWorld,
  worldToScreen,
  type TileRange,
} from "./camera.ts";
import { DirectLoader, type TileLoader } from "./loader.ts";
import { ByteLru } from "./lru.ts";
import type { DrawCommand, TileRenderer, TileTexture } from "./renderer.ts";
import { paletteLut } from "./renderer.ts";
import { defaultMaxInFlight, Scheduler } from "./scheduler.ts";
import { HttpError, isUrlSource, type UrlSource } from "./sources.ts";
import { TimelineVersions, ViewVersions, type TileVersion, type VersionResolver } from "./versions.ts";

/** Priority classes (lower loads first). */
export const PRIO_PALETTE = -1;
export const PRIO_VISIBLE = 0;
export const PRIO_RING = 1;
export const PRIO_PARENT = 2;
const PRIO_CLASS = 1e7;

/** Defaults. */
export const DEFAULT_TEXTURE_BUDGET = 160 * 1024 * 1024;
const MISSING_TTL_MS = 30_000;
const RETRY_BASE_MS = 1000;
const MAX_ANCESTOR_LEVELS = 8;
/** Server: longest version span a single delta may cover before a full tile is cheaper. */
const MAX_DELTA_SPAN = 256;
/** Static exports: longest chain of consecutive deltas. */
const MAX_DELTA_CHAIN = 4;

export interface TerrainOptions {
  source: DataSource;
  renderer: TileRenderer;
  loader?: TileLoader;
  maxInFlight?: number;
  textureBudget?: number;
  /** Ask the host for another frame (data arrived). */
  requestRedraw?(): void;
  now?(): number;
  /** Use tiledelta to move loaded textures forward in time (default true). */
  deltas?: boolean;
}

export interface FrameResult {
  z: number;
  draws: DrawCommand[];
  /** Visible tiles at the target level that are in the datastore bounds. */
  visible: number;
  /** Tiles showing something (own data, an older version, an ancestor or descendants) or known empty. */
  covered: number;
  /** Tiles showing their own data at the wanted version, or known empty. */
  full: number;
  /** Version answers still loading. */
  unresolved: number;
}

interface TexRec {
  tex: TileTexture;
  tile: string;
  ver: string;
  /** Numeric version for committed content, null for live content. */
  version: number | null;
  z: number;
}

type Want = { key: string; ver: string; version: number | null; live: boolean };

function wantFor(z: number, x: number, y: number, s: TileVersion): Want {
  const tk = tileKey(z, x, y);
  if (s.live && z === 0) {
    const ver = `L${s.ls ?? 0}.${s.v}`;
    return { key: `${tk}@${ver}`, ver, version: null, live: true };
  }
  return { key: `${tk}@${s.v}`, ver: String(s.v), version: s.v, live: false };
}

export class TerrainEngine {
  private readonly source: DataSource;
  readonly renderer: TileRenderer;
  private readonly loader: TileLoader;
  private readonly scheduler: Scheduler;
  private readonly textures: ByteLru<string, TexRec>;
  /** tile key -> loaded version keys. */
  private readonly byTile = new Map<string, Set<string>>();
  /** tile key -> most recently drawn version key (for stale drawing). */
  private readonly lastDrawn = new Map<string, string>();
  private readonly drawnOnce = new Set<string>();
  private readonly missing = new Map<string, number>();
  private readonly failed = new Map<string, { at: number; n: number }>();
  private readonly requestMeta = new Map<string, { z: number }>();
  private readonly redraw: () => void;
  private readonly now: () => number;
  private readonly useDeltas: boolean;

  private catalog: Catalog | null = null;
  private ds: CatalogDatastore | null = null;
  private resolver: VersionResolver | null = null;
  private timeline: TimelineVersions | null = null;
  private view: ViewVersions | null = null;
  private target: TimeMs | "latest" = "latest";
  private playing = false;
  private gen = 0;

  private palettePromise: Promise<void> | null = null;
  private paletteWanted = 0;

  private epoch = 0;
  private epochSig = "";
  private emittedFallback = -1;
  private emittedFull = -1;
  private lastFrame: FrameResult | null = null;

  constructor(opts: TerrainOptions) {
    this.source = opts.source;
    this.renderer = opts.renderer;
    this.loader = opts.loader ?? new DirectLoader();
    this.redraw = opts.requestRedraw ?? (() => {});
    this.now = opts.now ?? (() => (typeof performance !== "undefined" ? performance.now() : Date.now()));
    this.useDeltas = opts.deltas ?? true;
    this.scheduler = new Scheduler({
      maxInFlight: opts.maxInFlight ?? defaultMaxInFlight(),
      now: this.now,
      onStart: (key, prio) => {
        const meta = this.requestMeta.get(key);
        perfCount("tilesRequested");
        perfEvent("tile.request", { key, z: meta?.z ?? 0, prio: Math.floor(prio / PRIO_CLASS) });
      },
      onAbort: key => {
        perfCount("tilesAborted");
        perfEvent("tile.abort", { key });
      },
    });
    this.textures = new ByteLru<string, TexRec>({
      budget: opts.textureBudget ?? DEFAULT_TEXTURE_BUDGET,
      onEvict: (key, rec) => {
        this.renderer.deleteTexture(rec.tex);
        perfCount("textureBytes", -rec.tex.bytes);
        const set = this.byTile.get(rec.tile);
        set?.delete(rec.ver);
        if (set && set.size === 0) {
          this.byTile.delete(rec.tile);
        }
        this.drawnOnce.delete(key);
      },
    });
    this.renderer.onLost = () => {
      // Every texture is gone; drop the records so tiles load again.
      this.textures.clear();
      this.redraw();
    };
  }

  get datastore(): CatalogDatastore | null {
    return this.ds;
  }

  get maxInFlight(): number {
    return this.scheduler.maxInFlight;
  }

  set maxInFlight(n: number) {
    this.scheduler.maxInFlight = Math.max(1, n);
  }

  /** Stats for debugging and tests. */
  stats(): { textures: number; textureBytes: number; inFlight: number; queued: number } {
    return { textures: this.textures.size, textureBytes: this.textures.bytes, inFlight: this.scheduler.inFlight, queued: this.scheduler.pending };
  }

  /** Switches the datastore (or clears it with null). Drops every texture and request. */
  setDatastore(catalog: Catalog, ds: CatalogDatastore | null): void {
    if (ds && this.ds && ds.id === this.ds.id) {
      this.updateCatalog(catalog);
      return;
    }
    this.gen++;
    this.catalog = catalog;
    this.ds = ds;
    this.scheduler.clear();
    this.textures.clear();
    this.byTile.clear();
    this.lastDrawn.clear();
    this.missing.clear();
    this.failed.clear();
    this.renderer.resetPalette();
    this.loader.setLut(null);
    this.palettePromise = null;
    this.paletteWanted = 0;
    this.resolver?.dispose();
    this.resolver = null;
    this.timeline = null;
    this.view = null;
    this.bumpEpoch();
    if (ds) {
      this.makeResolver();
      // Fetch the palette in parallel with the first tiles.
      if (ds.palette_version > 0) {
        void this.ensurePalette(ds.palette_version).catch(() => {});
      }
    }
    this.redraw();
  }

  /** A refreshed catalog: updates the datastore summary (horizon, last, palette version). */
  updateCatalog(catalog: Catalog): void {
    this.catalog = catalog;
    if (!this.ds) {
      return;
    }
    const id = this.ds.id;
    for (const m of catalog.maps) {
      for (const s of m.surfaces) {
        for (const d of s.datastores) {
          if (d.id === id) {
            this.ds = d;
            this.timeline?.setDatastore(d);
            this.view?.setDatastore(d);
          }
        }
      }
    }
    this.redraw();
  }

  /**
   * Sets the displayed time. "latest" follows live data (live tiles on the server). While
   * `playing` (timelapse), a tile keeps its in-flight load instead of restarting it on every
   * step, and loaded textures move forward with deltas.
   */
  setTime(t: TimeMs | "latest", playing = false): void {
    const changed = t !== this.target;
    const modeChanged = (t === "latest") !== (this.target === "latest");
    this.target = t;
    this.playing = playing;
    if (modeChanged) {
      this.makeResolver();
    } else {
      this.timeline?.setTarget(t);
    }
    if (changed) {
      this.bumpEpoch();
      perfEvent("time.change", { t: t === "latest" ? -1 : t });
      this.redraw();
    }
  }

  get time(): TimeMs | "latest" {
    return this.target;
  }

  /** A live `tile` event from the server. */
  applyLiveTile(e: LiveTileEvent): void {
    if (this.view?.apply(e)) {
      this.redraw();
    }
  }

  /** Re-fetches mutable version answers (catalog changed or the live stream reconnected). */
  refreshVersions(): void {
    this.resolver?.refresh();
  }

  dispose(): void {
    this.gen++;
    this.scheduler.clear();
    this.resolver?.dispose();
    this.textures.clear();
    this.loader.dispose();
  }

  private makeResolver(): void {
    this.resolver?.dispose();
    this.resolver = null;
    this.timeline = null;
    this.view = null;
    const ds = this.ds;
    if (!ds) {
      return;
    }
    const onChange = (): void => this.redraw();
    if (this.target === "latest" && this.source.view && this.source.kind === "server") {
      this.view = new ViewVersions(this.source, ds, onChange, this.now);
      this.resolver = this.view;
    } else {
      this.timeline = new TimelineVersions(this.source, ds, onChange, this.now);
      this.timeline.setTarget(this.target);
      this.resolver = this.timeline;
    }
  }

  private bumpEpoch(): void {
    this.epoch++;
  }

  /** Computes one frame: schedules loads and returns what to draw. */
  frame(cam: Camera, vp: Viewport): FrameResult {
    const ds = this.ds;
    const resolver = this.resolver;
    const draws: DrawCommand[] = [];
    if (!ds || !resolver) {
      const r = { z: 0, draws, visible: 0, covered: 0, full: 0, unresolved: 0 };
      this.lastFrame = r;
      return r;
    }
    const z = levelFor(cam.scale, vp.dpr, ds.levels);
    const world = visibleWorld(cam, vp);
    const bounds = boundsAtLevel(ds.bounds, z);
    const vis = intersectRange(tileRange(world, z), bounds);
    const ring = intersectRange(tileRange(world, z, 1), bounds);
    const levels = ds.levels.length > 0 ? ds.levels : [0];
    const minLevel = Math.min(...levels);
    const pz = z - 1;
    const parentLevel = levels.includes(pz) ? pz : null;
    const parent = parentLevel !== null ? intersectRange(tileRange(world, parentLevel), boundsAtLevel(ds.bounds, parentLevel)) : null;

    const sig = `${ds.id}|${z}|${vis ? `${vis.x0},${vis.y0},${vis.x1},${vis.y1}` : "-"}`;
    if (sig !== this.epochSig) {
      this.epochSig = sig;
      this.bumpEpoch();
    }

    if (ring) {
      resolver.ensure(ring);
    }
    if (parent) {
      resolver.ensure(parent);
    }
    if (this.playing && this.timeline && this.target !== "latest") {
      // Timelapse: have the next day's timeline ready before midnight.
      this.timeline.prefetch(z, this.target + 3 * 3600_000);
    }

    this.textures.unpinAll();
    const wanted = new Set<string>();
    const busyTiles = new Set<string>();
    if (this.playing) {
      // Timelapse: keep the in-flight load of each tile near the view (any version) instead
      // of restarting it at every step; the next step continues from it with a delta.
      for (const key of this.requestMeta.keys()) {
        if (this.scheduler.isRunning(key)) {
          const tk = key.slice(0, key.indexOf("@"));
          const [kz, kx, ky] = tk.split("/").map(Number);
          if ((kz === z && rangeContains(ring, kx, ky)) || (kz === parentLevel && rangeContains(parent, kx, ky))) {
            busyTiles.add(tk);
            wanted.add(key);
          }
        }
      }
    }

    const cx = cam.x;
    const cy = cam.y;
    const prioOf = (cls: number, tz: number, x: number, y: number): number => {
      const r = tileWorldRect(tz, x, y);
      const span = tileSpan(tz);
      const dx = ((r[0] + r[2]) / 2 - cx) / span;
      const dy = ((r[1] + r[3]) / 2 - cy) / span;
      return cls * PRIO_CLASS + Math.round((dx * dx + dy * dy) * 1000);
    };

    const ancestorDraws: DrawCommand[] = [];
    const childDraws: DrawCommand[] = [];
    const ownDraws: DrawCommand[] = [];
    let visible = 0;
    let covered = 0;
    let full = 0;
    let unresolved = 0;
    const tNow = this.now();

    const rectFor = (tz: number, x: number, y: number): [number, number, number, number] => {
      const r = tileWorldRect(tz, x, y);
      const [sx0, sy0] = worldToScreen(cam, vp, r[0], r[1]);
      const [sx1, sy1] = worldToScreen(cam, vp, r[2], r[3]);
      return [sx0, sy0, sx1 - sx0, sy1 - sy0];
    };

    if (vis) {
      for (let y = vis.y0; y <= vis.y1; y++) {
        for (let x = vis.x0; x <= vis.x1; x++) {
          visible++;
          const tk = tileKey(z, x, y);
          const ans = resolver.get(z, x, y);
          const [dx, dy, dw, dh] = rectFor(z, x, y);
          if (ans === null) {
            // No data at this time: nothing to draw, and nothing missing.
            covered++;
            full++;
            continue;
          }
          let want: Want | null = null;
          if (ans === undefined) {
            unresolved++;
          } else {
            want = wantFor(z, x, y, ans);
            if (this.isMissing(want.key, tNow)) {
              covered++;
              full++;
              continue;
            }
            const own = this.textures.get(want.key);
            if (own) {
              this.textures.pin(want.key);
              ownDraws.push({ tex: own.tex, sx: 0, sy: 0, sw: 512, sh: 512, dx, dy, dw, dh });
              this.markDrawn(want.key, tk, want.ver);
              covered++;
              full++;
              continue;
            }
            this.requestTile(want, z, x, y, prioOf(PRIO_VISIBLE, z, x, y), wanted, busyTiles, tNow);
          }
          // Fallbacks, best first: an older version of this tile, then ancestors, then children.
          const stale = this.staleFor(tk);
          if (stale) {
            this.textures.pin(`${tk}@${stale.ver}`);
            ownDraws.push({ tex: stale.tex, sx: 0, sy: 0, sw: 512, sh: 512, dx, dy, dw, dh });
            covered++;
            continue;
          }
          let shown = false;
          for (let az = z - 1, n = 0; az >= minLevel && n < MAX_ANCESTOR_LEVELS; az--, n++) {
            const f = 2 ** (z - az);
            const ax = Math.floor(x / f);
            const ay = Math.floor(y / f);
            const rec = this.bestTexture(az, ax, ay);
            if (rec) {
              const [sx, sy, sw, sh] = ancestorSubRect(z, x, y, az);
              ancestorDraws.push({ tex: rec.tex, sx, sy, sw, sh, dx, dy, dw, dh });
              this.textures.pin(`${tileKey(az, ax, ay)}@${rec.ver}`);
              shown = true;
              break;
            }
          }
          if (z < 0) {
            // Children one and two levels down (after zooming out).
            for (let dz = 1; dz <= 2 && z + dz <= 0; dz++) {
              const f = 2 ** dz;
              for (let j = 0; j < f; j++) {
                for (let i = 0; i < f; i++) {
                  const cxT = x * f + i;
                  const cyT = y * f + j;
                  const rec = this.bestTexture(z + dz, cxT, cyT);
                  if (rec) {
                    const [cdx, cdy, cdw, cdh] = rectFor(z + dz, cxT, cyT);
                    childDraws.push({ tex: rec.tex, sx: 0, sy: 0, sw: 512, sh: 512, dx: cdx, dy: cdy, dw: cdw, dh: cdh });
                    this.textures.pin(`${tileKey(z + dz, cxT, cyT)}@${rec.ver}`);
                    shown = true;
                  }
                }
              }
            }
          }
          if (shown) {
            covered++;
          }
        }
      }
    }

    // Prefetch: the one-tile ring at this level, then the parent level.
    if (ring) {
      for (let y = ring.y0; y <= ring.y1; y++) {
        for (let x = ring.x0; x <= ring.x1; x++) {
          if (rangeContains(vis, x, y)) {
            continue;
          }
          this.prefetch(z, x, y, prioOf(PRIO_RING, z, x, y), wanted, busyTiles, tNow);
        }
      }
    }
    if (parent && parentLevel !== null) {
      for (let y = parent.y0; y <= parent.y1; y++) {
        for (let x = parent.x0; x <= parent.x1; x++) {
          this.prefetch(parentLevel, x, y, prioOf(PRIO_PARENT, parentLevel, x, y), wanted, busyTiles, tNow);
        }
      }
    }

    this.scheduler.retain(wanted);
    for (const k of this.requestMeta.keys()) {
      if (!this.scheduler.has(k)) {
        this.requestMeta.delete(k);
      }
    }
    this.scheduler.pump();
    this.textures.evict();

    draws.push(...ancestorDraws, ...childDraws, ...ownDraws);
    const result: FrameResult = { z, draws, visible, covered, full, unresolved };
    this.lastFrame = result;
    perfCount("framesDrawn");
    if (covered === visible && this.emittedFallback !== this.epoch) {
      this.emittedFallback = this.epoch;
      perfEvent("view.fallback");
    }
    if (full === visible && this.emittedFull !== this.epoch) {
      this.emittedFull = this.epoch;
      if (this.emittedFallback !== this.epoch) {
        this.emittedFallback = this.epoch;
        perfEvent("view.fallback");
      }
      perfEvent("view.full", { tiles: visible });
    }
    return result;
  }

  /** The result of the latest frame() call. */
  get last(): FrameResult | null {
    return this.lastFrame;
  }

  /** Renders a frame result with the renderer. */
  render(vp: Viewport, result: FrameResult): void {
    this.renderer.render(vp, result.draws);
  }

  private markDrawn(key: string, tk: string, ver: string): void {
    this.lastDrawn.set(tk, ver);
    if (!this.drawnOnce.has(key)) {
      this.drawnOnce.add(key);
      perfEvent("tile.drawn", { key });
    }
  }

  /** The most recently drawn loaded version of a tile, else any loaded version. */
  private staleFor(tk: string): TexRec | null {
    const set = this.byTile.get(tk);
    if (!set || set.size === 0) {
      return null;
    }
    const last = this.lastDrawn.get(tk);
    if (last && set.has(last)) {
      return this.textures.peek(`${tk}@${last}`) ?? null;
    }
    let best: TexRec | null = null;
    for (const ver of set) {
      const rec = this.textures.peek(`${tk}@${ver}`);
      if (rec && (!best || (rec.version ?? Infinity) > (best.version ?? Infinity))) {
        best = rec;
      }
    }
    return best;
  }

  /** The texture for tile (z, x, y) at its wanted version if loaded, else any loaded version. */
  private bestTexture(z: number, x: number, y: number): TexRec | null {
    const tk = tileKey(z, x, y);
    const set = this.byTile.get(tk);
    if (!set || set.size === 0) {
      return null;
    }
    const ans = this.resolver?.get(z, x, y);
    if (ans) {
      const w = wantFor(z, x, y, ans);
      const rec = this.textures.peek(w.key);
      if (rec) {
        return rec;
      }
    }
    return this.staleFor(tk);
  }

  private isMissing(key: string, tNow: number): boolean {
    const at = this.missing.get(key);
    if (at === undefined) {
      return false;
    }
    if (tNow - at > MISSING_TTL_MS) {
      this.missing.delete(key);
      return false;
    }
    return true;
  }

  private prefetch(z: number, x: number, y: number, prio: number, wanted: Set<string>, busy: Set<string>, tNow: number): void {
    const ans = this.resolver?.get(z, x, y);
    if (!ans) {
      return;
    }
    const want = wantFor(z, x, y, ans);
    if (this.textures.has(want.key) || this.isMissing(want.key, tNow)) {
      return;
    }
    this.requestTile(want, z, x, y, prio, wanted, busy, tNow);
  }

  private requestTile(want: Want, z: number, x: number, y: number, prio: number, wanted: Set<string>, busy: Set<string>, tNow: number): void {
    const tk = tileKey(z, x, y);
    if (busy.has(tk) && !this.scheduler.has(want.key)) {
      // Timelapse: wait for the tile's in-flight load to finish first.
      return;
    }
    const f = this.failed.get(want.key);
    if (f && tNow - f.at < Math.min(30_000, RETRY_BASE_MS * 2 ** f.n)) {
      return;
    }
    wanted.add(want.key);
    if (!this.scheduler.has(want.key)) {
      this.requestMeta.set(want.key, { z });
    }
    this.scheduler.want(want.key, prio, signal => this.load(want, z, x, y, signal));
  }

  /** Loads a tile version: a delta on top of an older loaded version when possible, else the full tile. */
  private async load(want: Want, z: number, x: number, y: number, signal: AbortSignal): Promise<void> {
    const gen = this.gen;
    const ds = this.ds!;
    const tk = tileKey(z, x, y);
    try {
      if (this.useDeltas && z === 0 && want.version !== null) {
        const base = this.deltaBase(tk, want.version);
        if (base && (await this.loadDelta(ds.id, base, want, x, y, signal, gen))) {
          return;
        }
      }
      await this.loadFull(ds.id, want, z, x, y, signal, gen);
      this.failed.delete(want.key);
    } catch (err) {
      if ((err as Error)?.name === "AbortError" || gen !== this.gen) {
        return;
      }
      if (err instanceof HttpError && err.status === 404) {
        this.missing.set(want.key, this.now());
      } else {
        const f = this.failed.get(want.key);
        this.failed.set(want.key, { at: this.now(), n: (f?.n ?? 0) + 1 });
        setTimeout(() => this.redraw(), RETRY_BASE_MS);
      }
      this.redraw();
    }
  }

  /** The newest loaded committed version of a tile below `version`, if a delta can bridge it. */
  private deltaBase(tk: string, version: number): TexRec | null {
    const set = this.byTile.get(tk);
    if (!set) {
      return null;
    }
    const span = isUrlSource(this.source) ? this.source.deltaSpan : Number.POSITIVE_INFINITY;
    const limit = Number.isFinite(span) && span <= 1 ? MAX_DELTA_CHAIN : MAX_DELTA_SPAN;
    let best: TexRec | null = null;
    for (const ver of set) {
      const rec = this.textures.peek(`${tk}@${ver}`);
      if (rec && rec.version !== null && rec.version > 0 && rec.version < version && version - rec.version <= limit) {
        if (!best || rec.version! > best.version!) {
          best = rec;
        }
      }
    }
    return best;
  }

  private async fetchBody(kind: "tile" | "delta" | "palette", url: (s: UrlSource) => string, direct: () => Promise<ArrayBuffer>, signal: AbortSignal, live = false): Promise<{ buffer: ArrayBuffer; ms: number }> {
    const t0 = this.now();
    if (isUrlSource(this.source)) {
      const r = await this.loader.fetch(url(this.source), this.source.requestHeaders(), signal, { cache: live ? "no-store" : undefined });
      return { buffer: r.buffer, ms: r.ms };
    }
    void kind;
    const buffer = await direct();
    return { buffer, ms: this.now() - t0 };
  }

  private async loadDelta(dsId: string, base: TexRec, want: Want, x: number, y: number, signal: AbortSignal, gen: number): Promise<boolean> {
    const from = base.version!;
    const to = want.version!;
    const span = isUrlSource(this.source) ? this.source.deltaSpan : Number.POSITIVE_INFINITY;
    const steps: [number, number][] = [];
    if (span >= to - from) {
      steps.push([from, to]);
    } else {
      for (let v = from; v < to; v += span) {
        steps.push([v, Math.min(to, v + span)]);
      }
    }
    let cur = base;
    let curKey = `${base.tile}@${base.ver}`;
    for (const [a, b] of steps) {
      const t0 = this.now();
      const { buffer, ms } = await this.fetchBody(
        "delta",
        s => s.tileDeltaUrl(dsId, x, y, a, b),
        () => this.source.tileDelta(dsId, x, y, a, b, signal),
        signal,
      );
      if (signal.aborted || gen !== this.gen) {
        return true;
      }
      const d = decodeTileDelta(buffer);
      if (d.header.x !== x || d.header.y !== y || d.header.from !== a || d.header.to !== b) {
        throw new Error(`delta ${a}-${b} for tile ${x},${y} has header ${d.header.x},${d.header.y} ${d.header.from}-${d.header.to}`);
      }
      perfCount("tilesLoaded");
      perfCount("bytesFetched", buffer.byteLength);
      perfEvent("tile.loaded", { key: `${base.tile}@${a}-${b}`, bytes: buffer.byteLength, ms: Math.round(ms || this.now() - t0) });
      await this.ensurePalette(d.header.paletteVersion, signal);
      if (signal.aborted || gen !== this.gen) {
        return true;
      }
      // The base may have been evicted or patched by another load meanwhile.
      const still = this.textures.peek(curKey);
      if (still !== cur) {
        return false;
      }
      if (!this.renderer.applyDelta(cur.tex, d)) {
        return false;
      }
      const nextVer = String(b);
      const nextKey = `${base.tile}@${nextVer}`;
      if (this.textures.has(nextKey)) {
        // Someone loaded the target meanwhile; drop the patched copy.
        this.textures.delete(curKey);
        this.redraw();
        return true;
      }
      this.textures.rekey(curKey, nextKey);
      const set = this.byTile.get(base.tile)!;
      set.delete(cur.ver);
      set.add(nextVer);
      if (this.lastDrawn.get(base.tile) === cur.ver) {
        this.lastDrawn.set(base.tile, nextVer);
      }
      this.drawnOnce.delete(curKey);
      cur.ver = nextVer;
      cur.version = b;
      curKey = nextKey;
      this.textures.get(curKey);
    }
    this.redraw();
    return true;
  }

  private async loadFull(dsId: string, want: Want, z: number, x: number, y: number, signal: AbortSignal, gen: number): Promise<void> {
    const version = want.live ? "live" : want.version!;
    const { buffer, ms } = await this.fetchBody(
      "tile",
      s => s.tileUrl(dsId, { z, x, y }, version),
      () => this.source.tile(dsId, { z, x, y }, version, signal),
      signal,
      want.live,
    );
    if (signal.aborted || gen !== this.gen) {
      return;
    }
    const tile = decodeTile(buffer);
    const h = tile.header;
    if (h.z !== z || h.x !== x || h.y !== y) {
      throw new Error(`tile ${z}/${x}/${y} has header ${h.z}/${h.x}/${h.y}`);
    }
    perfCount("tilesLoaded");
    perfCount("bytesFetched", buffer.byteLength);
    perfEvent("tile.loaded", { key: want.key, bytes: buffer.byteLength, ms: Math.round(ms) });
    if (h.format !== 3) {
      await this.ensurePalette(h.paletteVersion, signal);
    }
    if (signal.aborted || gen !== this.gen) {
      return;
    }
    this.addTexture(want, z, x, y, this.renderer.createTexture({ format: h.format, pixels: tile.pixels, paletteVersion: h.paletteVersion }));
  }

  private addTexture(want: Want, z: number, x: number, y: number, tex: TileTexture): void {
    const tk = tileKey(z, x, y);
    const ver = want.ver;
    if (want.live) {
      // Only the newest live content of a tile is worth keeping.
      const set = this.byTile.get(tk);
      if (set) {
        for (const v of [...set]) {
          if (v.startsWith("L") && v !== ver) {
            this.textures.delete(`${tk}@${v}`);
          }
        }
      }
    }
    this.textures.set(want.key, { tex, tile: tk, ver, version: want.version, z }, tex.bytes);
    perfCount("textureBytes", tex.bytes);
    let set = this.byTile.get(tk);
    if (!set) {
      set = new Set();
      this.byTile.set(tk, set);
    }
    set.add(ver);
    this.redraw();
  }

  /** Makes sure the renderer holds at least `n` palette entries. */
  ensurePalette(n: number, signal?: AbortSignal): Promise<void> {
    if (this.renderer.paletteLength >= n || n <= 0) {
      return Promise.resolve();
    }
    this.paletteWanted = Math.max(this.paletteWanted, n);
    if (!this.palettePromise) {
      const gen = this.gen;
      const run = async (): Promise<void> => {
        while (gen === this.gen && this.renderer.paletteLength < this.paletteWanted) {
          const ds = this.ds!;
          // The catalog's palette version always exists (static exports keep the latest).
          const need = this.paletteWanted;
          const ver = ds.palette_version >= need ? ds.palette_version : need;
          const ctrl = new AbortController();
          const { buffer } = await this.fetchBody(
            "palette",
            s => s.paletteUrl(ds.id, ver),
            () => this.source.palette(ds.id, ver, ctrl.signal),
            ctrl.signal,
          );
          if (gen !== this.gen) {
            return;
          }
          const colors = decodePalette(buffer);
          perfCount("bytesFetched", buffer.byteLength);
          if (colors.length > this.renderer.paletteLength) {
            this.renderer.setPalette(colors);
            this.loader.setLut(this.renderer.kind === "canvas2d" ? paletteLut(colors) : null);
          }
          if (colors.length < need) {
            throw new Error(`palette ${ver} has ${colors.length} entries, need ${need}`);
          }
        }
      };
      perfEvent("tile.request", { key: `palette@${n}`, z: 0, prio: PRIO_PALETTE });
      this.palettePromise = run().finally(() => {
        this.palettePromise = null;
        this.redraw();
      });
    }
    const p = this.palettePromise;
    if (!signal) {
      return p;
    }
    return new Promise<void>((resolve, reject) => {
      const onAbort = (): void => {
        const e = new Error("aborted");
        e.name = "AbortError";
        reject(e);
      };
      if (signal.aborted) {
        onAbort();
        return;
      }
      signal.addEventListener("abort", onAbort, { once: true });
      p.then(
        () => {
          signal.removeEventListener("abort", onAbort);
          // A larger palette may still be needed (palette grew while we waited).
          resolve(this.ensurePalette(n, signal));
        },
        err => {
          signal.removeEventListener("abort", onAbort);
          reject(err);
        },
      );
    });
  }
}

export type { TileRange };
