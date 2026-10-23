// The MapLayer contract. The map engine (web/src/map/) owns the canvas, the
// camera, terrain drawing and time; everything drawn above terrain (trains, vehicles,
// players, tags, recipes, UE edges, rails, instance bounds) is a MapLayer
// (web/src/movers/, web/src/overlays/), listed in web/src/layers/index.ts.
//
// Coordinates: world tiles (Factorio map tiles), +x east, +y south, surface-local. Screen
// coordinates are CSS pixels from the canvas's top-left corner.

import type { Catalog, CatalogDatastore, CatalogMap, CatalogSurface, DataSource, TimeMs } from "../api/types";

/** The view: which world point is at the canvas centre and how large a tile is. */
export interface Camera {
  /** World tile coordinates at the centre of the canvas. */
  x: number;
  y: number;
  /** CSS pixels per world tile (1 = one pixel per tile, z0 at native size). */
  scale: number;
}

export interface Viewport {
  /** Canvas size in CSS pixels. */
  width: number;
  height: number;
  /** devicePixelRatio; the backing store is width*dpr x height*dpr. */
  dpr: number;
}

/** Everything a layer needs to draw one frame. Valid only during the draw call. */
export interface FrameState {
  camera: Camera;
  viewport: Viewport;
  /** Terrain level being drawn (z <= 0). */
  z: number;
  /** Displayed map time (behind live by the mover delay in live mode). */
  time: TimeMs;
  /** True when following live data. */
  live: boolean;
  /** performance.now() at frame start, and ms since the previous frame. */
  now: number;
  dt: number;
  /** Visible world rectangle [x0, y0, x1, y1] in tiles. */
  visible: [number, number, number, number];
  worldToScreen(x: number, y: number): [number, number];
  screenToWorld(x: number, y: number): [number, number];
}

/** Services the map engine gives a layer when it attaches. */
export interface LayerContext {
  source: DataSource;
  catalog: Catalog;
  map: CatalogMap;
  surface: CatalogSurface;
  /** The terrain datastore being shown (force), or null if the surface has none. */
  datastore: CatalogDatastore | null;
  /** Ask for another frame (e.g. after data arrived or while animating). */
  requestRedraw(): void;
  /** Icon spritesheet (catalog.icons), loaded lazily by the engine; null when absent. */
  icons(): Promise<IconAtlas | null>;
}

export interface IconAtlas {
  image: ImageBitmap | HTMLImageElement;
  /** "<type>/<name>" -> sprite rectangle in the image. */
  sprites: Map<string, { x: number; y: number; size: number }>;
}

export interface LayerHit {
  layer: string;
  /** Short text for a tooltip. */
  label: string;
  /** Identifies the object (journey id, overlay key). */
  key: string;
}

export interface MapLayer {
  /** Stable id, also used in URLs and settings ("movers", "tags", ...). */
  readonly id: string;
  /** Draw order: higher draws later (on top). Terrain is drawn before every layer. */
  readonly order: number;
  /** Called when the layer is enabled or the map/surface/datastore changes. */
  attach(ctx: LayerContext): void | Promise<void>;
  /** Called before attach to another surface and when disabled; drop data and timers. */
  detach(): void;
  /** Displayed time changed (scrub, play, live). Called before the next draw. */
  setTime?(time: TimeMs, live: boolean): void;
  /** Draw on the 2D overlay canvas. The context is already scaled by dpr. */
  draw2d?(ctx: CanvasRenderingContext2D, frame: FrameState): void;
  /** Draw on the WebGL2 map canvas, after terrain (for instanced drawing of many movers). */
  drawGL?(gl: WebGL2RenderingContext, frame: FrameState): void;
  /** Object under a screen point, for tooltips and selection. */
  hitTest?(x: number, y: number, frame: FrameState): LayerHit | null;
  /**
   * How many ms the displayed live time should trail the data clock for this layer (the
   * movers layer's adaptive delay, web/src/movers/clock.ts). The engine uses the largest value
   * over the enabled layers for FrameState.time in live mode. Optional; layers draw
   * correctly either way (the movers layer also clamps its own time).
   */
  delay?(): number;
}

/** An entry in the layer list (web/src/layers/index.ts). */
export interface LayerFactory {
  id: string;
  label: string;
  defaultOn: boolean;
  create(): MapLayer;
}
