// MapView: the map canvas in the page. Owns the terrain canvas (WebGL2, or Canvas2D as a
// fallback) and a 2D overlay canvas for layers, pan and zoom by hand (ported from
// illisis-minimap web/map_view.ts, Blue Oak Model License 1.0.0), the frame loop, the
// displayed time (live, scrub, timelapse), catalog refresh and live updates, and the
// MapLayer list (web/src/layers/index.ts).

import type { Catalog, CatalogDatastore, CatalogMap, CatalogSurface, DataSource, LiveTileEvent, TimeMs } from "../api/types";
import type { Camera, FrameState, IconAtlas, LayerContext, LayerFactory, LayerHit, MapLayer, Viewport } from "./layer";
import { perfEvent } from "../perf.ts";
import { clampScale, fitCamera, screenToWorld, visibleWorld, worldToScreen, zoomAt } from "./camera.ts";
import type { Seed } from "./boot.ts";
import { createLoader, DirectLoader, type TileLoader } from "./loader.ts";
import { Canvas2DRenderer } from "./render_2d.ts";
import { GLRenderer } from "./render_gl.ts";
import type { TileRenderer } from "./renderer.ts";
import { defaultMaxInFlight } from "./scheduler.ts";
import { TerrainEngine, type FrameResult } from "./terrain.ts";

export type RendererChoice = "auto" | "webgl2" | "canvas2d";

export interface Selection {
  map: string;
  surface: string;
  force: string;
}

export interface MapViewState {
  camera: Camera;
  z: number;
  /** Displayed time (map clock). In live mode: the data clock now. */
  time: TimeMs;
  live: boolean;
  playing: boolean;
  /** Timelapse speed in map milliseconds per real second. */
  speed: number;
  /** Selectable time range [first, now]. */
  range: [TimeMs, TimeMs];
  renderer: TileRenderer["kind"];
  loader: TileLoader["kind"];
  tiles: { visible: number; full: number; covered: number; inFlight: number; queued: number; textures: number; textureBytes: number };
  selection: Selection | null;
  datastore: CatalogDatastore | null;
}

export interface MapViewOptions {
  container: HTMLElement;
  source: DataSource;
  catalog: Catalog;
  renderer?: RendererChoice;
  loader?: TileLoader;
  /** Layer factories (default: none; the app passes web/src/layers/index.ts). */
  layers?: LayerFactory[];
  /** Enabled layer ids (default: each factory's defaultOn). */
  enabledLayers?: Iterable<string>;
  /** Movers are drawn this far behind live (ms). */
  liveDelayMs?: number;
  camera?: Camera;
  /** Called (at most ~10 times a second) when the camera, time or loading state changes. */
  onState?(s: MapViewState): void;
  /** Called with the layer object under the pointer, or null. */
  onHover?(hit: LayerHit | null, world: [number, number]): void;
  /** Called when the catalog was refreshed. */
  onCatalog?(c: Catalog): void;
  /** A version answer fetched during start-up (boot.ts), used for the first selection. */
  viewSeed?: Promise<Seed | null>;
}

const CATALOG_POLL_MS = 10_000;
/** Longest wait for the start-up version answer before planning without it. */
const SEED_WAIT_MS = 150;

function findSelection(c: Catalog, sel: Selection | null): { map: CatalogMap; surface: CatalogSurface; ds: CatalogDatastore | null } | null {
  if (!sel) {
    return null;
  }
  const map = c.maps.find(m => m.key === sel.map);
  const surface = map?.surfaces.find(s => s.name === sel.surface);
  if (!map || !surface) {
    return null;
  }
  return { map, surface, ds: surface.datastores.find(d => d.force === sel.force) ?? null };
}

/** The first map/surface/force with terrain, preferring nauvis and the player force. */
export function defaultSelection(c: Catalog): Selection | null {
  let best: Selection | null = null;
  let bestScore = -1;
  for (const m of c.maps) {
    for (const s of m.surfaces) {
      for (const d of s.datastores) {
        const score = (s.name === "nauvis" ? 2 : 0) + (d.force === "player" ? 1 : 0);
        if (score > bestScore) {
          bestScore = score;
          best = { map: m.key, surface: s.name, force: d.force };
        }
      }
    }
  }
  return best;
}

/** World rectangle of a datastore's tiles. */
export function datastoreWorld(ds: CatalogDatastore): [number, number, number, number] {
  return [-256 + ds.bounds[0] * 512, -256 + ds.bounds[1] * 512, -256 + (ds.bounds[2] + 1) * 512, -256 + (ds.bounds[3] + 1) * 512];
}

export class MapView {
  readonly source: DataSource;
  readonly engine: TerrainEngine;
  readonly renderer: TileRenderer;
  readonly loader: TileLoader;
  catalog: Catalog;
  camera: Camera;
  private readonly container: HTMLElement;
  private terrainCanvas: HTMLCanvasElement;
  private readonly overlay: HTMLCanvasElement;
  private readonly overlayCtx: CanvasRenderingContext2D;
  private readonly opts: MapViewOptions;
  private vp: Viewport = { width: 1, height: 1, dpr: 1 };
  private sel: Selection | null = null;
  private factories: LayerFactory[];
  private enabled: Set<string>;
  private active: MapLayer[] = [];
  private live = true;
  private time: TimeMs = 0;
  private playing = false;
  private speed = 3600_000;
  private liveDelay: number;
  private raf = 0;
  private lastFrameAt = 0;
  private lastStateAt = 0;
  private stateDirty = true;
  private schedulePending = false;
  private seedGate: Promise<void> | null = null;
  private stateTimer: ReturnType<typeof setTimeout> | null = null;
  private lastLayerTime = NaN;
  private lastResult: FrameResult | null = null;
  private unsubscribe: (() => void) | null = null;
  private pendingSubscribe = false;
  private subscribeTimer: ReturnType<typeof setTimeout> | null = null;
  private catalogTimer: ReturnType<typeof setInterval> | null = null;
  private pollTimer: ReturnType<typeof setInterval> | null = null;
  private resizeObs: ResizeObserver | null = null;
  private destroyed = false;
  private iconsPromise: Promise<IconAtlas | null> | null = null;
  private readonly pointers = new Map<number, { x: number; y: number }>();

  constructor(opts: MapViewOptions) {
    this.opts = opts;
    this.source = opts.source;
    this.catalog = opts.catalog;
    this.container = opts.container;
    this.liveDelay = opts.liveDelayMs ?? 2000;
    this.factories = opts.layers ?? [];
    this.enabled = new Set(opts.enabledLayers ?? this.factories.filter(f => f.defaultOn).map(f => f.id));
    this.camera = opts.camera ?? { x: 0, y: 0, scale: 1 };

    this.terrainCanvas = document.createElement("canvas");
    this.terrainCanvas.className = "tsdbTerrain";
    this.overlay = document.createElement("canvas");
    this.overlay.className = "tsdbOverlay";
    this.overlay.tabIndex = 0;
    this.overlay.setAttribute("aria-label", "Map");
    this.container.append(this.terrainCanvas, this.overlay);
    this.overlayCtx = this.overlay.getContext("2d")!;

    this.renderer = this.makeRenderer(opts.renderer ?? "auto");
    // WebGL2 only uploads index textures, and the browser decompresses responses off the main
    // thread anyway, so fetching on the main thread was measured faster than a worker pool
    // (README "Measurements"). Canvas2D expands every pixel to RGBA: that runs in workers.
    this.loader = opts.loader ?? (this.renderer.kind === "canvas2d" ? createLoader() : new DirectLoader());
    this.engine = new TerrainEngine({
      source: this.source,
      renderer: this.renderer,
      loader: this.loader,
      maxInFlight: defaultMaxInFlight(protocolOf(this.source)),
      requestRedraw: () => {
        this.schedule();
        this.invalidate();
      },
    });

    this.measure();
    this.resizeObs = new ResizeObserver(() => {
      this.measure();
      this.invalidate();
    });
    this.resizeObs.observe(this.container);
    this.bindInput();
    this.startPolling();
    this.invalidate();
  }

  private makeRenderer(choice: RendererChoice): TileRenderer {
    if (choice !== "canvas2d") {
      try {
        return new GLRenderer(this.terrainCanvas);
      } catch (err) {
        if (choice === "webgl2") {
          throw err;
        }
        console.warn("WebGL2 renderer failed, using Canvas2D", err);
        // getContext("webgl2") may have succeeded before init failed (shader error, context
        // lost at start-up); that canvas can no longer give a 2D context, so use a new one.
        const fresh = document.createElement("canvas");
        fresh.className = this.terrainCanvas.className;
        this.terrainCanvas.replaceWith(fresh);
        this.terrainCanvas = fresh;
      }
    }
    return new Canvas2DRenderer(this.terrainCanvas);
  }

  // -------------------------------------------------------------------------------------
  // Selection, layers and catalog

  get selection(): Selection | null {
    return this.sel;
  }

  get datastore(): CatalogDatastore | null {
    return this.engine.datastore;
  }

  /** Shows a map/surface/force. Returns false when it does not exist in the catalog. */
  select(sel: Selection | null, opts: { fit?: boolean } = {}): boolean {
    const found = findSelection(this.catalog, sel);
    if (sel && !found) {
      return false;
    }
    const same = this.sel && sel && this.sel.map === sel.map && this.sel.surface === sel.surface && this.sel.force === sel.force;
    this.sel = sel;
    this.engine.setDatastore(this.catalog, found?.ds ?? null);
    const seed = this.opts.viewSeed;
    if (seed && found?.ds) {
      // Only the first selection can use the start-up answer. Frames wait for it (at most
      // SEED_WAIT_MS): planning before it arrives would request the same versions again.
      this.opts.viewSeed = undefined;
      const wait = Promise.race([seed, new Promise<null>(r => setTimeout(() => r(null), SEED_WAIT_MS))]);
      const gate = wait.then(sd => {
        if (sd && !this.destroyed) {
          if (sd.kind === "view") {
            this.engine.seedView(sd.res, sd.bbox);
          } else {
            this.engine.seedTimeline(sd.res);
          }
        }
      });
      this.seedGate = gate;
      void gate.finally(() => {
        if (this.seedGate === gate) {
          this.seedGate = null;
          this.schedule();
          this.invalidate();
        }
      });
    }
    if (!same) {
      if (opts.fit && found?.ds) {
        this.fit();
      }
      this.attachLayers();
      this.subscribeSoon();
    }
    this.markState();
    this.schedule();
    this.invalidate();
    return true;
  }

  /** Zooms to fit the selected datastore. */
  fit(): void {
    const ds = this.engine.datastore;
    if (ds) {
      this.camera = fitCamera(datastoreWorld(ds), this.vp);
      this.markState();
      this.invalidate();
    }
  }

  setLayers(enabled: Iterable<string>, factories?: LayerFactory[]): void {
    if (factories) {
      this.factories = factories;
    }
    this.enabled = new Set(enabled);
    this.attachLayers();
    this.invalidate();
  }

  private attachLayers(): void {
    for (const l of this.active) {
      try {
        l.detach();
      } catch (err) {
        console.error(`layer ${l.id} detach`, err);
      }
    }
    this.active = [];
    const found = findSelection(this.catalog, this.sel);
    if (!found) {
      return;
    }
    const ctx: LayerContext = {
      source: this.source,
      catalog: this.catalog,
      map: found.map,
      surface: found.surface,
      datastore: found.ds,
      requestRedraw: () => this.invalidate(),
      icons: () => this.icons(),
    };
    this.active = this.factories
      .filter(f => this.enabled.has(f.id))
      .map(f => f.create())
      .sort((a, b) => a.order - b.order);
    this.lastLayerTime = NaN;
    for (const l of this.active) {
      try {
        void Promise.resolve(l.attach(ctx)).catch(err => console.error(`layer ${l.id} attach`, err));
      } catch (err) {
        console.error(`layer ${l.id} attach`, err);
      }
    }
  }

  private icons(): Promise<IconAtlas | null> {
    if (!this.iconsPromise) {
      this.iconsPromise = loadIconAtlas(this.source, this.catalog);
    }
    return this.iconsPromise;
  }

  /** Re-reads the catalog (polling, live `catalog` events). */
  async refreshCatalog(): Promise<void> {
    try {
      const c = await this.source.catalog();
      if (this.destroyed) {
        return;
      }
      this.catalog = c;
      this.engine.updateCatalog(c);
      this.opts.onCatalog?.(c);
      this.markState();
      this.invalidate();
    } catch {
      // Keep the old catalog; the next poll tries again.
    }
  }

  private startPolling(): void {
    const every = this.source.kind === "static" ? Math.max(2000, this.catalog.live_ms || 5000) : CATALOG_POLL_MS;
    this.catalogTimer = setInterval(() => void this.refreshCatalog(), every);
  }

  /**
   * Subscribes to live events once the first view is fully painted (or after 3 s): on
   * HTTP/1.1 the open event stream holds one of the browser's six connections, which the
   * first tiles need more.
   */
  private subscribeSoon(): void {
    this.unsubscribe?.();
    this.unsubscribe = null;
    if (this.subscribeTimer) {
      clearTimeout(this.subscribeTimer);
    }
    this.pendingSubscribe = true;
    this.subscribeTimer = setTimeout(() => {
      this.subscribeTimer = null;
      if (this.pendingSubscribe) {
        this.pendingSubscribe = false;
        this.subscribeLive();
      }
    }, 3000);
  }

  private subscribeLive(): void {
    this.unsubscribe?.();
    this.unsubscribe = null;
    if (this.pollTimer) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
    const ds = this.engine.datastore;
    if (!ds || this.source.kind !== "server") {
      return;
    }
    if (this.source.subscribe) {
      // Every hello resyncs the versions: tile events between the first `view` answers and
      // the subscription (which opens only after the first full paint), or during a
      // reconnect, were missed. It costs one `view` request per level in view; answers stay
      // usable meanwhile.
      const every = Math.max(2000, this.catalog.live_ms || 5000);
      this.unsubscribe = this.source.subscribe(
        { ds: [ds.id], sfc: [] },
        {
          hello: () => {
            if (this.pollTimer) {
              clearInterval(this.pollTimer);
              this.pollTimer = null;
            }
            this.engine.refreshVersions();
          },
          tile: (e: LiveTileEvent) => this.engine.applyLiveTile(e),
          horizon: () => void this.refreshCatalog(),
          catalog: () => void this.refreshCatalog(),
          // While the stream is down (or a proxy buffers it), poll the versions instead.
          error: () => {
            if (!this.pollTimer && !this.destroyed) {
              this.pollTimer = setInterval(() => this.engine.refreshVersions(), every);
            }
          },
        },
      );
    } else {
      this.pollTimer = setInterval(() => this.engine.refreshVersions(), Math.max(2000, this.catalog.live_ms || 5000));
    }
  }

  // -------------------------------------------------------------------------------------
  // Time

  /** The selectable time range [first, now]. */
  range(): [TimeMs, TimeMs] {
    const now = this.source.now();
    const ds = this.engine.datastore;
    const found = findSelection(this.catalog, this.sel);
    let first = ds?.first ?? now;
    if (found?.surface.movers && found.surface.movers.first > 0) {
      first = Math.min(first, found.surface.movers.first);
    }
    return [Math.min(first, now), now];
  }

  /** The displayed time. */
  displayTime(): TimeMs {
    return this.live ? this.source.now() - this.liveDelayNow() : this.time;
  }

  /**
   * How far live mode trails the data clock: the largest MapLayer.delay() of the enabled
   * layers (the movers layer's adaptive delay, PLAN §4.8 step 1), or `liveDelayMs` when no
   * layer reports one.
   */
  liveDelayNow(): number {
    let d = -1;
    for (const l of this.active) {
      const v = l.delay?.();
      if (v !== undefined && Number.isFinite(v) && v > d) {
        d = v;
      }
    }
    return d >= 0 ? d : this.liveDelay;
  }

  get isLive(): boolean {
    return this.live;
  }

  get isPlaying(): boolean {
    return this.playing;
  }

  /** Shows the map at time t (stops following live). */
  setTime(t: TimeMs): void {
    const [lo, hi] = this.range();
    this.time = Math.min(hi, Math.max(lo, t));
    this.live = false;
    this.engine.setTime(this.time, this.playing);
    this.markState();
    this.invalidate();
  }

  goLive(): void {
    this.live = true;
    this.playing = false;
    this.engine.setTime("latest", false);
    this.markState();
    this.invalidate();
  }

  /** Starts a timelapse at `speed` map ms per real second. */
  play(speed?: number): void {
    if (speed !== undefined) {
      this.speed = speed;
    }
    if (this.live) {
      this.time = this.range()[0];
      this.live = false;
    }
    this.playing = true;
    this.engine.setTime(this.time, true);
    this.lastFrameAt = 0;
    this.markState();
    this.invalidate();
  }

  pause(): void {
    this.playing = false;
    if (!this.live) {
      this.engine.setTime(this.time, false);
    }
    this.markState();
    this.invalidate();
  }

  setSpeed(speed: number): void {
    this.speed = speed;
    this.markState();
  }

  // -------------------------------------------------------------------------------------
  // Camera and input

  setCamera(cam: Camera): void {
    this.camera = { x: cam.x, y: cam.y, scale: clampScale(cam.scale) };
    this.markState();
    this.invalidate();
  }

  private measure(): void {
    const r = this.container.getBoundingClientRect();
    this.vp = { width: Math.max(1, r.width), height: Math.max(1, r.height), dpr: window.devicePixelRatio || 1 };
  }

  private bindInput(): void {
    const el = this.overlay;
    el.addEventListener("pointerdown", e => {
      if (e.button !== 0 && e.pointerType === "mouse") {
        return;
      }
      el.setPointerCapture(e.pointerId);
      this.pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    });
    el.addEventListener("pointermove", e => {
      const p = this.pointers.get(e.pointerId);
      const rect = el.getBoundingClientRect();
      if (!p) {
        this.hover(e.clientX - rect.left, e.clientY - rect.top);
        return;
      }
      if (this.pointers.size === 1) {
        this.setCamera({ ...this.camera, x: this.camera.x - (e.clientX - p.x) / this.camera.scale, y: this.camera.y - (e.clientY - p.y) / this.camera.scale });
      } else if (this.pointers.size === 2) {
        const [a, b] = [...this.pointers.entries()];
        const other = a[0] === e.pointerId ? b[1] : a[1];
        const before = Math.hypot(p.x - other.x, p.y - other.y);
        const after = Math.hypot(e.clientX - other.x, e.clientY - other.y);
        if (before > 0) {
          const mx = (e.clientX + other.x) / 2 - rect.left;
          const my = (e.clientY + other.y) / 2 - rect.top;
          this.setCamera(zoomAt(this.camera, this.vp, mx, my, this.camera.scale * (after / before)));
        }
      }
      this.pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    });
    const end = (e: PointerEvent): void => {
      this.pointers.delete(e.pointerId);
    };
    el.addEventListener("pointerup", end);
    el.addEventListener("pointercancel", end);
    el.addEventListener(
      "wheel",
      e => {
        e.preventDefault();
        const rect = el.getBoundingClientRect();
        const lines = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? rect.height : 1;
        const dy = e.deltaY * lines;
        this.setCamera(zoomAt(this.camera, this.vp, e.clientX - rect.left, e.clientY - rect.top, this.camera.scale * 2 ** (-dy / 500)));
      },
      { passive: false },
    );
    el.addEventListener("dblclick", e => {
      const rect = el.getBoundingClientRect();
      this.setCamera(zoomAt(this.camera, this.vp, e.clientX - rect.left, e.clientY - rect.top, this.camera.scale * (e.shiftKey ? 0.5 : 2)));
    });
    el.addEventListener("keydown", e => {
      const step = 100 / this.camera.scale;
      const c = this.camera;
      switch (e.key) {
        case "+":
        case "=":
          this.setCamera(zoomAt(c, this.vp, this.vp.width / 2, this.vp.height / 2, c.scale * 2));
          break;
        case "-":
          this.setCamera(zoomAt(c, this.vp, this.vp.width / 2, this.vp.height / 2, c.scale / 2));
          break;
        case "ArrowLeft":
          this.setCamera({ ...c, x: c.x - step });
          break;
        case "ArrowRight":
          this.setCamera({ ...c, x: c.x + step });
          break;
        case "ArrowUp":
          this.setCamera({ ...c, y: c.y - step });
          break;
        case "ArrowDown":
          this.setCamera({ ...c, y: c.y + step });
          break;
        default:
          return;
      }
      e.preventDefault();
    });
    el.addEventListener("pointerleave", () => this.opts.onHover?.(null, [0, 0]));
  }

  private hover(sx: number, sy: number): void {
    if (!this.opts.onHover) {
      return;
    }
    const frame = this.frameState(performance.now(), 0, this.lastResult?.z ?? 0);
    let hit: LayerHit | null = null;
    for (let i = this.active.length - 1; i >= 0 && !hit; i--) {
      hit = this.active[i].hitTest?.(sx, sy, frame) ?? null;
    }
    this.opts.onHover(hit, screenToWorld(this.camera, this.vp, sx, sy));
  }

  // -------------------------------------------------------------------------------------
  // Frames

  /**
   * Plans loads right away (in a microtask) when data arrives, so the next requests do not
   * wait for an animation frame; drawing still happens in the frame.
   */
  private schedule(): void {
    if (this.schedulePending || this.destroyed || this.seedGate) {
      return;
    }
    this.schedulePending = true;
    queueMicrotask(() => {
      this.schedulePending = false;
      if (!this.destroyed && !this.seedGate) {
        this.engine.frame(this.camera, this.vp, { scheduleOnly: true });
      }
    });
  }

  /** Requests a frame. */
  invalidate(): void {
    if (this.raf === 0 && !this.destroyed) {
      this.raf = requestAnimationFrame(t => this.frame(t));
    }
  }

  private markState(): void {
    this.stateDirty = true;
  }

  private frameState(now: number, dt: number, z: number): FrameState {
    const cam = this.camera;
    const vp = this.vp;
    return {
      camera: cam,
      viewport: vp,
      z,
      time: this.displayTime(),
      live: this.live,
      now,
      dt,
      visible: visibleWorld(cam, vp),
      worldToScreen: (x, y) => worldToScreen(cam, vp, x, y),
      screenToWorld: (x, y) => screenToWorld(cam, vp, x, y),
    };
  }

  private frame(now: number): void {
    this.raf = 0;
    if (this.destroyed || this.seedGate) {
      // Waiting for the start-up version answer; the gate requests a frame when it opens.
      return;
    }
    const t0 = performance.now();
    const dt = this.lastFrameAt ? Math.min(250, now - this.lastFrameAt) : 0;
    this.lastFrameAt = now;
    if (this.playing && !this.live) {
      const [, hi] = this.range();
      this.time += (dt / 1000) * this.speed;
      if (this.time >= hi) {
        this.goLive();
      } else {
        this.engine.setTime(this.time, true);
      }
      this.markState();
    }
    const result = this.engine.frame(this.camera, this.vp);
    const prev = this.lastResult;
    if (!prev || prev.full !== result.full || prev.visible !== result.visible || prev.z !== result.z) {
      // Loading progress is state too (the HUD would otherwise keep showing "loading").
      this.markState();
    }
    this.lastResult = result;
    this.engine.render(this.vp, result);
    if (this.pendingSubscribe && result.full === result.visible && result.visible > 0) {
      this.pendingSubscribe = false;
      if (this.subscribeTimer) {
        clearTimeout(this.subscribeTimer);
        this.subscribeTimer = null;
      }
      this.subscribeLive();
    }

    // Layers.
    const frame = this.frameState(now, dt, result.z);
    const ctx = this.overlayCtx;
    const w = Math.round(this.vp.width * this.vp.dpr);
    const h = Math.round(this.vp.height * this.vp.dpr);
    if (this.overlay.width !== w || this.overlay.height !== h) {
      this.overlay.width = w;
      this.overlay.height = h;
    }
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.clearRect(0, 0, w, h);
    if (this.active.length > 0) {
      if (frame.time !== this.lastLayerTime) {
        this.lastLayerTime = frame.time;
        for (const l of this.active) {
          l.setTime?.(frame.time, this.live);
        }
      }
      const gl = this.renderer instanceof GLRenderer ? this.renderer.context : null;
      for (const l of this.active) {
        try {
          if (gl && l.drawGL) {
            l.drawGL(gl, frame);
          }
          if (l.draw2d) {
            ctx.setTransform(this.vp.dpr, 0, 0, this.vp.dpr, 0, 0);
            l.draw2d(ctx, frame);
          }
        } catch (err) {
          console.error(`layer ${l.id} draw`, err);
        }
      }
    }
    if (this.playing) {
      perfEvent("timelapse.frame", { t: Math.round(this.time), ms: Math.round((performance.now() - t0) * 100) / 100 });
      this.invalidate();
    }
    if (this.opts.onState && (this.stateDirty || now - this.lastStateAt > 500) && now - this.lastStateAt > 100) {
      this.stateDirty = false;
      this.lastStateAt = now;
      this.opts.onState(this.state());
    } else if (this.stateDirty && !this.stateTimer) {
      // Deliver the pending state change on a later frame.
      this.stateTimer = setTimeout(() => {
        this.stateTimer = null;
        this.invalidate();
      }, 110);
    }
  }

  /**
   * Renders the terrain now and reads it back (RGBA, top row first, device pixels), or null
   * when the renderer cannot. For tests: it bypasses the compositor's colour handling.
   */
  readTerrainPixels(): { width: number; height: number; rgba: Uint8Array } | null {
    if (!this.renderer.readPixels) {
      return null;
    }
    const result = this.engine.frame(this.camera, this.vp);
    this.engine.render(this.vp, result);
    return this.renderer.readPixels();
  }

  state(): MapViewState {
    const r = this.lastResult;
    const s = this.engine.stats();
    return {
      camera: { ...this.camera },
      z: r?.z ?? 0,
      time: this.displayTime(),
      live: this.live,
      playing: this.playing,
      speed: this.speed,
      range: this.range(),
      renderer: this.renderer.kind,
      loader: this.loader.kind,
      tiles: { visible: r?.visible ?? 0, full: r?.full ?? 0, covered: r?.covered ?? 0, inFlight: s.inFlight, queued: s.queued, textures: s.textures, textureBytes: s.textureBytes },
      selection: this.sel,
      datastore: this.engine.datastore,
    };
  }

  destroy(): void {
    this.destroyed = true;
    if (this.raf) {
      cancelAnimationFrame(this.raf);
    }
    for (const l of this.active) {
      l.detach();
    }
    this.active = [];
    this.unsubscribe?.();
    if (this.subscribeTimer) {
      clearTimeout(this.subscribeTimer);
    }
    if (this.catalogTimer) {
      clearInterval(this.catalogTimer);
    }
    if (this.pollTimer) {
      clearInterval(this.pollTimer);
    }
    this.resizeObs?.disconnect();
    this.engine.dispose();
    this.renderer.dispose();
    this.terrainCanvas.remove();
    this.overlay.remove();
  }
}

/** The network protocol the page or the source is served with ("h2", "http/1.1", ...). */
function protocolOf(source: DataSource): string | undefined {
  if (typeof performance === "undefined" || !performance.getEntriesByType) {
    return undefined;
  }
  const base = (source as { base?: string }).base;
  if (base) {
    const abs = new URL(base, location.href).href;
    const hit = (performance.getEntriesByType("resource") as PerformanceResourceTiming[]).reverse().find(e => e.name.startsWith(abs) && e.nextHopProtocol);
    if (hit) {
      return hit.nextHopProtocol;
    }
  }
  const nav = performance.getEntriesByType("navigation")[0] as PerformanceNavigationTiming | undefined;
  return nav?.nextHopProtocol || undefined;
}

/** Loads the icon spritesheet named by the catalog (paths relative to the source base). */
async function loadIconAtlas(source: DataSource, catalog: Catalog): Promise<IconAtlas | null> {
  if (!catalog.icons) {
    return null;
  }
  const base = new URL((source as { base?: string }).base ?? "./", location.href);
  try {
    const [meta, img] = await Promise.all([
      fetch(new URL(catalog.icons.json, base)).then(r => (r.ok ? (r.json() as Promise<Record<string, { name: string; icon?: { x: number; y: number; size: number } }[]>>) : null)),
      (async () => {
        const im = new Image();
        im.src = new URL(catalog.icons!.png, base).href;
        await im.decode();
        return im;
      })(),
    ]);
    if (!meta) {
      return null;
    }
    // Sprite keys as illisis-minimap web/view/overlays.ts iconIndex: "<type>/<name>", with
    // the export's "virtual-signal" type called "virtual".
    const sprites = new Map<string, { x: number; y: number; size: number }>();
    for (const [type, entries] of Object.entries(meta)) {
      const t = type === "virtual-signal" ? "virtual" : type;
      for (const e of entries) {
        if (e.icon) {
          sprites.set(`${t}/${e.name}`, e.icon);
        }
      }
    }
    return { image: img, sprites };
  } catch {
    return null;
  }
}
