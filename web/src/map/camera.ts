// Camera math: which level to draw, which tiles are visible, world <-> screen transforms and
// zoom about a point. DOM-free so it runs under node --test.

import type { Camera, Viewport } from "./layer";
import { GRID_ORIGIN, MIN_Z, TILE_SIZE, tileSpan } from "../api/grid.ts";

/** CSS pixels per world tile limits. */
export const MAX_SCALE = 32;
export const MIN_SCALE = 1 / 2048;

export interface TileRange {
  z: number;
  x0: number;
  y0: number;
  /** Inclusive. */
  x1: number;
  y1: number;
}

export function clampScale(scale: number): number {
  return Math.min(MAX_SCALE, Math.max(MIN_SCALE, scale));
}

/**
 * The level for a camera: the coarsest level whose pixels are still at least one device
 * pixel, rounded to the nearest level (z0 when zoomed in), then snapped to the nearest level
 * that exists (`levels` may have gaps).
 */
export function levelFor(scale: number, dpr: number, levels?: readonly number[]): number {
  const devicePerTile = scale * dpr;
  let z = Math.round(Math.log2(devicePerTile));
  z = Math.min(0, Math.max(MIN_Z, z));
  if (levels && levels.length > 0 && !levels.includes(z)) {
    // The nearest level that exists; on a tie the finer one.
    let best = levels[0];
    for (const l of levels) {
      const d = Math.abs(l - z);
      const bd = Math.abs(best - z);
      if (d < bd || (d === bd && l > best)) {
        best = l;
      }
    }
    z = best;
  }
  return z;
}

/** Visible world rectangle [x0, y0, x1, y1] in tiles. */
export function visibleWorld(cam: Camera, vp: Viewport): [number, number, number, number] {
  const hw = vp.width / 2 / cam.scale;
  const hh = vp.height / 2 / cam.scale;
  return [cam.x - hw, cam.y - hh, cam.x + hw, cam.y + hh];
}

/** Tiles at level z overlapping a world rectangle, grown by `ring` tiles on every side. */
export function tileRange(world: [number, number, number, number], z: number, ring = 0): TileRange {
  const span = tileSpan(z);
  const [wx0, wy0, wx1, wy1] = world;
  return {
    z,
    x0: Math.floor((wx0 - GRID_ORIGIN) / span) - ring,
    y0: Math.floor((wy0 - GRID_ORIGIN) / span) - ring,
    // A rectangle ending exactly on a tile edge does not include the next tile.
    x1: Math.ceil((wx1 - GRID_ORIGIN) / span) - 1 + ring,
    y1: Math.ceil((wy1 - GRID_ORIGIN) / span) - 1 + ring,
  };
}

/** z0 bounds [minX, minY, maxX, maxY] (inclusive) scaled to level z. */
export function boundsAtLevel(bounds: readonly [number, number, number, number], z: number): [number, number, number, number] {
  const f = 2 ** -z;
  return [Math.floor(bounds[0] / f), Math.floor(bounds[1] / f), Math.floor(bounds[2] / f), Math.floor(bounds[3] / f)];
}

export function intersectRange(r: TileRange, b: readonly [number, number, number, number] | null): TileRange | null {
  const out = b ? { z: r.z, x0: Math.max(r.x0, b[0]), y0: Math.max(r.y0, b[1]), x1: Math.min(r.x1, b[2]), y1: Math.min(r.y1, b[3]) } : { ...r };
  return out.x0 > out.x1 || out.y0 > out.y1 ? null : out;
}

export function rangeTiles(r: TileRange | null): { z: number; x: number; y: number }[] {
  const out: { z: number; x: number; y: number }[] = [];
  if (!r) {
    return out;
  }
  for (let y = r.y0; y <= r.y1; y++) {
    for (let x = r.x0; x <= r.x1; x++) {
      out.push({ z: r.z, x, y });
    }
  }
  return out;
}

export function rangeContains(r: TileRange | null, x: number, y: number): boolean {
  return !!r && x >= r.x0 && x <= r.x1 && y >= r.y0 && y <= r.y1;
}

export function worldToScreen(cam: Camera, vp: Viewport, x: number, y: number): [number, number] {
  return [(x - cam.x) * cam.scale + vp.width / 2, (y - cam.y) * cam.scale + vp.height / 2];
}

export function screenToWorld(cam: Camera, vp: Viewport, sx: number, sy: number): [number, number] {
  return [(sx - vp.width / 2) / cam.scale + cam.x, (sy - vp.height / 2) / cam.scale + cam.y];
}

/** Zoom to `scale` keeping the world point under screen point (sx, sy) fixed. */
export function zoomAt(cam: Camera, vp: Viewport, sx: number, sy: number, scale: number): Camera {
  const next = clampScale(scale);
  const [wx, wy] = screenToWorld(cam, vp, sx, sy);
  return {
    scale: next,
    x: wx - (sx - vp.width / 2) / next,
    y: wy - (sy - vp.height / 2) / next,
  };
}

/** A camera that fits a world rectangle into the viewport. */
export function fitCamera(world: [number, number, number, number], vp: Viewport): Camera {
  const w = Math.max(1, world[2] - world[0]);
  const h = Math.max(1, world[3] - world[1]);
  const scale = clampScale(Math.min(vp.width / w, vp.height / h));
  return { x: (world[0] + world[2]) / 2, y: (world[1] + world[3]) / 2, scale };
}

/** World rectangle covered by a tile. */
export function tileWorldRect(z: number, x: number, y: number): [number, number, number, number] {
  const span = tileSpan(z);
  const ox = GRID_ORIGIN + x * span;
  const oy = GRID_ORIGIN + y * span;
  return [ox, oy, ox + span, oy + span];
}

/**
 * The part of ancestor tile (az, ax, ay) that covers tile (z, x, y), in the ancestor's
 * pixels [sx, sy, sw, sh].
 */
export function ancestorSubRect(z: number, x: number, y: number, az: number): [number, number, number, number] {
  const levels = z - az;
  const f = 2 ** levels;
  const size = TILE_SIZE / f;
  const ix = x - Math.floor(x / f) * f;
  const iy = y - Math.floor(y / f) * f;
  return [ix * size, iy * size, size, size];
}

/** Key of a tile, "z/x/y". */
export function tileKey(z: number, x: number, y: number): string {
  return `${z}/${x}/${y}`;
}
