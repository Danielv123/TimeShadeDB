// Test datasets for the mock server: TSV3/TSD3/TSP3 encoders (docs/v3/FORMAT.md 7) and a
// synthetic terrain history with coarse levels built by the FORMAT.md 3 average. Node-only
// helpers (excluded from tsc); used by *.test.ts and the browser benchmark harness.

import type { Catalog, CatalogDatastore, TimelineResponse, ViewResponse } from "../../api/types";
import { utcDay } from "../../api/grid.ts";

export const TILE = 512;
export const TP = TILE * TILE;

export function encodeTile(z: number, x: number, y: number, version: number, format: number, pixels: Uint8Array | Uint16Array, paletteVersion: number, live = false): Uint8Array {
  const bpp = format === 1 ? 1 : 2;
  const out = new Uint8Array(32 + TP * bpp);
  const v = new DataView(out.buffer);
  out.set([0x54, 0x53, 0x56, 0x33], 0); // TSV3
  v.setUint8(4, format);
  v.setInt8(5, z);
  v.setUint8(6, live ? 1 : 0);
  v.setInt32(8, x, true);
  v.setInt32(12, y, true);
  v.setUint32(16, version, true);
  v.setUint32(20, paletteVersion, true);
  v.setUint16(24, TILE, true);
  v.setUint16(26, TILE, true);
  if (bpp === 1) {
    out.set(pixels as Uint8Array, 32);
  } else {
    for (let i = 0; i < TP; i++) {
      v.setUint16(32 + 2 * i, pixels[i], true);
    }
  }
  return out;
}

export function encodePalette(colors: Uint16Array, n = colors.length): Uint8Array {
  const out = new Uint8Array(8 + 2 * n);
  const v = new DataView(out.buffer);
  out.set([0x54, 0x53, 0x50, 0x33], 0); // TSP3
  v.setUint32(4, n, true);
  for (let i = 0; i < n; i++) {
    v.setUint16(8 + 2 * i, colors[i], true);
  }
  return out;
}

export function blockPixelIndex(b: number, p: number): number {
  return ((b >> 4) * 32 + (p >> 5)) * TILE + (b & 15) * 32 + (p & 31);
}

/** TSD3 between two pixel arrays of a z0 tile (only changed chunks). */
export function encodeDelta(x: number, y: number, from: number, to: number, a: Uint8Array | Uint16Array, b: Uint8Array | Uint16Array, format: number, paletteVersion: number): Uint8Array {
  const bpp = format === 1 ? 1 : 2;
  const blocks: number[] = [];
  for (let blk = 0; blk < 256; blk++) {
    for (let p = 0; p < 1024; p++) {
      const i = blockPixelIndex(blk, p);
      if (a[i] !== b[i]) {
        blocks.push(blk);
        break;
      }
    }
  }
  const entry = 4 + 1024 * bpp;
  const out = new Uint8Array(32 + entry * blocks.length);
  const v = new DataView(out.buffer);
  out.set([0x54, 0x53, 0x44, 0x33], 0); // TSD3
  v.setUint8(4, format);
  v.setUint8(5, 0);
  v.setUint16(6, blocks.length, true);
  v.setInt32(8, x, true);
  v.setInt32(12, y, true);
  v.setUint32(16, from, true);
  v.setUint32(20, to, true);
  v.setUint32(24, paletteVersion, true);
  blocks.forEach((blk, k) => {
    const o = 32 + k * entry;
    v.setUint16(o, blk, true);
    for (let p = 0; p < 1024; p++) {
      const val = b[blockPixelIndex(blk, p)];
      if (bpp === 1) {
        out[o + 4 + p] = val;
      } else {
        v.setUint16(o + 4 + 2 * p, val, true);
      }
    }
  });
  return out;
}

export interface TileVersionData {
  v: number;
  /** Effective time. */
  t: number;
  format: number;
  pixels: Uint8Array | Uint16Array;
  paletteVersion: number;
}

export interface MockTile {
  z: number;
  x: number;
  y: number;
  versions: TileVersionData[];
}

export interface MockDatastore {
  ds: CatalogDatastore;
  palette: Uint16Array;
  tiles: Map<string, MockTile>;
  /** Live content (z0): tile key -> live body state. */
  live: Map<string, { ls: number; pixels: Uint8Array; paletteVersion: number }>;
}

export interface MockDataset {
  catalog: Catalog;
  stores: Map<string, MockDatastore>;
  /** Map-clock now for X-TSDB-Now. */
  now: number;
}

export function tk(z: number, x: number, y: number): string {
  return `${z}/${x}/${y}`;
}

/** Version of a tile effective at t (0 when none). */
export function versionAt(tile: MockTile, t: number): TileVersionData | null {
  let best: TileVersionData | null = null;
  for (const v of tile.versions) {
    if (v.t <= t) {
      best = v;
    }
  }
  return best;
}

export function viewFor(store: MockDatastore, z: number, bbox: [number, number, number, number], t: number | "latest", now: number): ViewResponse {
  const tt = t === "latest" ? Number.POSITIVE_INFINITY : t;
  const tiles: ViewResponse["tiles"] = [];
  for (const tile of store.tiles.values()) {
    if (tile.z !== z || tile.x < bbox[0] || tile.x > bbox[2] || tile.y < bbox[1] || tile.y > bbox[3]) {
      continue;
    }
    const v = versionAt(tile, tt);
    const live = t === "latest" && z === 0 ? store.live.get(tk(z, tile.x, tile.y)) : undefined;
    if (!v && !live) {
      continue;
    }
    tiles.push(live ? { x: tile.x, y: tile.y, v: v?.v ?? 0, live: true, ls: live.ls } : { x: tile.x, y: tile.y, v: v!.v });
  }
  return { ds: store.ds.id, z, t: t === "latest" ? now : t, horizon: store.ds.horizon, tiles };
}

export function timelineFor(store: MockDatastore, z: number, date: string): TimelineResponse {
  const start = Date.parse(`${date}T00:00:00Z`);
  const end = start + 86400_000;
  const tiles: TimelineResponse["tiles"] = [];
  for (const tile of store.tiles.values()) {
    if (tile.z !== z) {
      continue;
    }
    let v0 = 0;
    const points: [number, number][] = [];
    for (const v of tile.versions) {
      if (v.t <= start) {
        v0 = v.v;
      } else if (v.t < end) {
        points.push([v.t, v.v]);
      }
    }
    if (v0 === 0 && points.length === 0) {
      continue;
    }
    tiles.push({ x: tile.x, y: tile.y, v0, points });
  }
  return { ds: store.ds.id, z, date, start, final: store.ds.horizon >= end, tiles };
}

// ---------------------------------------------------------------------------------------
// Synthetic data

/** A small deterministic PRNG (mulberry32). */
export function rng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** Averages four RGB565 quadrant tiles (TL, TR, BL, BR; null = uncharted) into a parent tile (FORMAT.md 3). */
export function averageRgb565(children: (Uint16Array | null)[]): Uint16Array {
  const out = new Uint16Array(TP);
  for (let q = 0; q < 4; q++) {
    const c = children[q];
    if (!c) {
      continue;
    }
    const ox = (q & 1) * 256;
    const oy = (q >> 1) * 256;
    for (let py = 0; py < 256; py++) {
      for (let px = 0; px < 256; px++) {
        const i = 2 * py * TILE + 2 * px;
        const a = c[i];
        const b = c[i + 1];
        const d = c[i + TILE];
        const e = c[i + TILE + 1];
        const r = (((a >> 11) & 31) + ((b >> 11) & 31) + ((d >> 11) & 31) + ((e >> 11) & 31) + 2) >> 2;
        const g = (((a >> 5) & 63) + ((b >> 5) & 63) + ((d >> 5) & 63) + ((e >> 5) & 63) + 2) >> 2;
        const bl = ((a & 31) + (b & 31) + (d & 31) + (e & 31) + 2) >> 2;
        out[(oy + py) * TILE + ox + px] = (r << 11) | (g << 5) | bl;
      }
    }
  }
  return out;
}

export interface SyntheticOptions {
  /** z0 tile bounds [x0, y0, x1, y1] inclusive. */
  bounds?: [number, number, number, number];
  /** Committed versions per z0 tile (1..n). */
  versions?: number;
  /** Coarse levels below 0 to build (e.g. 2 -> z -1 and -2). */
  coarse?: number;
  /** First version effective at t0 + window; then one per `step` ms. */
  t0?: number;
  step?: number;
  window?: number;
  /** Chunks changed per version per tile. */
  changes?: number;
  seed?: number;
  /** Palette size (<= 255 for pal8). */
  colors?: number;
  /** Extra palette entries appended for the last version (to test palette growth). */
  lateColors?: number;
  dsId?: string;
  map?: string;
}

function pixelsToRgb(px: Uint8Array, pal: Uint16Array): Uint16Array {
  const out = new Uint16Array(TP);
  for (let i = 0; i < TP; i++) {
    out[i] = pal[px[i]];
  }
  return out;
}

/** Builds a synthetic dataset: blocky terrain, `versions` per z0 tile, coarse levels averaged. */
export function syntheticDataset(o: SyntheticOptions = {}): MockDataset {
  const bounds = o.bounds ?? [0, 0, 1, 1];
  const nver = o.versions ?? 3;
  const coarse = o.coarse ?? 1;
  const window = o.window ?? 60_000;
  const t0 = o.t0 ?? Date.parse("2026-10-10T10:00:00Z");
  const step = o.step ?? window;
  const changes = o.changes ?? 4;
  const r = rng(o.seed ?? 1);
  const ncol = o.colors ?? 40;
  const late = o.lateColors ?? 0;
  const palette = new Uint16Array(ncol + late);
  palette[0] = 0;
  for (let i = 1; i < palette.length; i++) {
    palette[i] = (Math.floor(r() * 65535) + 1) & 0xffff || 1;
  }
  const map = o.map ?? "gw-test.1";
  const dsId = o.dsId ?? "gw-test~2E1.nauvis.player";
  const tiles = new Map<string, MockTile>();
  const tEff = (v: number): number => t0 + window + (v - 1) * step;
  for (let y = bounds[1]; y <= bounds[3]; y++) {
    for (let x = bounds[0]; x <= bounds[2]; x++) {
      const tile: MockTile = { z: 0, x, y, versions: [] };
      // Version 1: every chunk a flat colour with a stripe.
      let px = new Uint8Array(TP);
      for (let b = 0; b < 256; b++) {
        const c = 1 + Math.floor(r() * (ncol - 1));
        const stripe = 1 + Math.floor(r() * (ncol - 1));
        for (let p = 0; p < 1024; p++) {
          px[blockPixelIndex(b, p)] = (p >> 5) % 8 === 0 ? stripe : c;
        }
      }
      tile.versions.push({ v: 1, t: tEff(1), format: 1, pixels: px, paletteVersion: maxIndex(px) + 1 });
      for (let v = 2; v <= nver; v++) {
        px = px.slice();
        const lastVersion = v === nver;
        for (let k = 0; k < changes; k++) {
          const b = Math.floor(r() * 256);
          const c = lastVersion && late > 0 ? ncol + Math.floor(r() * late) : 1 + Math.floor(r() * (ncol - 1));
          for (let p = 0; p < 1024; p += 1 + Math.floor(r() * 3)) {
            px[blockPixelIndex(b, p)] = c;
          }
        }
        tile.versions.push({ v, t: tEff(v), format: 1, pixels: px, paletteVersion: maxIndex(px) + 1 });
      }
      tiles.set(tk(0, x, y), tile);
    }
  }
  // Coarse levels: one version per distinct child change time, the 2x2 average.
  const levels = [0];
  for (let z = -1; z >= -coarse; z--) {
    levels.push(z);
    const f = 2 ** -z;
    const cb: [number, number, number, number] = [Math.floor(bounds[0] / f), Math.floor(bounds[1] / f), Math.floor(bounds[2] / f), Math.floor(bounds[3] / f)];
    for (let y = cb[1]; y <= cb[3]; y++) {
      for (let x = cb[0]; x <= cb[2]; x++) {
        const kids = [tk(z + 1, 2 * x, 2 * y), tk(z + 1, 2 * x + 1, 2 * y), tk(z + 1, 2 * x, 2 * y + 1), tk(z + 1, 2 * x + 1, 2 * y + 1)].map(k => tiles.get(k) ?? null);
        const times = [...new Set(kids.flatMap(k => (k ? k.versions.map(v => v.t) : [])))].sort((a, b) => a - b);
        if (times.length === 0) {
          continue;
        }
        const tile: MockTile = { z, x, y, versions: [] };
        times.forEach((t, i) => {
          const rgb = kids.map(k => {
            const v = k ? versionAt(k, t) : null;
            if (!v) {
              return null;
            }
            return v.format === 3 ? (v.pixels as Uint16Array) : pixelsToRgb(v.pixels as Uint8Array, palette);
          });
          tile.versions.push({ v: i + 1, t, format: 3, pixels: averageRgb565(rgb), paletteVersion: 0 });
        });
        tiles.set(tk(z, x, y), tile);
      }
    }
  }
  const last = tEff(nver);
  const ds: CatalogDatastore = {
    force: "player",
    id: dsId,
    window_ms: window,
    horizon: last,
    first: t0 + 1000,
    last: last - 1000,
    palette_version: palette.length,
    bounds,
    levels,
  };
  const now = last + 5000;
  const catalog: Catalog = {
    version: 1,
    now,
    static: false,
    live_ms: 5000,
    maps: [
      {
        key: map,
        title: "Test map",
        tile_size: 512,
        instances: [],
        surfaces: [{ name: "nauvis", id: dsId.split(".").slice(0, 2).join("."), edges: [], datastores: [ds], movers: null }],
      },
    ],
    icons: null,
  };
  return { catalog, stores: new Map([[dsId, { ds, palette, tiles, live: new Map() }]]), now };
}

function maxIndex(px: Uint8Array | Uint16Array): number {
  let m = 0;
  for (let i = 0; i < px.length; i++) {
    if (px[i] > m) {
      m = px[i];
    }
  }
  return m;
}

export { utcDay };
