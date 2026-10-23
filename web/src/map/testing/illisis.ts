// Builds a MockDataset from an illisis-minimap S3 export (read as data only: snapshots
// `<t>.s.gz` are MMS1 = 4-byte magic + 512x512 RGB565 LE; levels z1..z4 there are v3 levels
// -1..-4 with the same -256 tile origin). The newest `snapshots` snapshots of each area become
// that tile's versions. Used by the browser benchmark for realistic tile content and sizes.
// Node only.

import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import { gunzipSync } from "node:zlib";
import type { Catalog, CatalogDatastore } from "../../api/types";
import { escapeName } from "../../api/grid.ts";
import { tk, type MockDataset, type MockTile, TP } from "./dataset.ts";

export interface IllisisOptions {
  /** Export map directory name, e.g. "gw-bc41437d~2E1". */
  map: string;
  surface: string;
  force?: string;
  /** Newest snapshots per area to keep as versions (default 4). */
  snapshots?: number;
  /** Deepest illisis level to load (default 4). */
  maxLevel?: number;
}

function unescapeIllisis(s: string): string {
  const bytes: number[] = [];
  for (let i = 0; i < s.length; i++) {
    if (s[i] === "~" && /^[0-9A-Fa-f]{2}$/.test(s.slice(i + 1, i + 3))) {
      bytes.push(parseInt(s.slice(i + 1, i + 3), 16));
      i += 2;
    } else {
      bytes.push(s.charCodeAt(i));
    }
  }
  return new TextDecoder().decode(new Uint8Array(bytes));
}

async function readSnapshot(path: string): Promise<Uint16Array> {
  const raw = gunzipSync(await readFile(path));
  if (raw.length !== 4 + TP * 2) {
    throw new Error(`${path}: snapshot of ${raw.length} bytes`);
  }
  const out = new Uint16Array(TP);
  for (let i = 0; i < TP; i++) {
    out[i] = raw[4 + 2 * i] | (raw[5 + 2 * i] << 8);
  }
  return out;
}

export async function loadIllisisDataset(root: string, o: IllisisOptions): Promise<MockDataset> {
  const force = o.force ?? "player";
  const keep = o.snapshots ?? 4;
  const maxLevel = o.maxLevel ?? 4;
  const rawMap = unescapeIllisis(o.map);
  const dsId = `${escapeName(rawMap)}.${escapeName(o.surface)}.${escapeName(force)}`;
  const base = join(root, o.map, o.surface, force);
  const tiles = new Map<string, MockTile>();
  const colorIndex = new Map<number, number>([[0, 0]]);
  const palette: number[] = [0];
  let first = Infinity;
  let last = 0;
  const bounds: [number, number, number, number] = [Infinity, Infinity, -Infinity, -Infinity];
  const levels: number[] = [];
  for (let lvl = 0; lvl <= maxLevel; lvl++) {
    let areas: string[];
    try {
      areas = await readdir(join(base, String(lvl)));
    } catch {
      continue;
    }
    const z = lvl === 0 ? 0 : -lvl;
    levels.push(z);
    for (const area of areas) {
      const m = /^(-?\d+)_(-?\d+)$/.exec(area);
      if (!m) {
        continue;
      }
      const x = Number(m[1]);
      const y = Number(m[2]);
      const files = (await readdir(join(base, String(lvl), area))).filter(f => f.endsWith(".s.gz"));
      const times = files.map(f => Number(f.slice(0, -5))).filter(Number.isFinite).sort((a, b) => a - b).slice(-keep);
      if (times.length === 0) {
        continue;
      }
      const tile: MockTile = { z, x, y, versions: [] };
      let v = 0;
      for (const t of times) {
        const rgb = await readSnapshot(join(base, String(lvl), area, `${t}.s.gz`));
        if (z === 0) {
          const px = new Uint16Array(TP);
          let max = 0;
          for (let i = 0; i < TP; i++) {
            const c = rgb[i];
            let idx = colorIndex.get(c);
            if (idx === undefined) {
              idx = palette.length;
              palette.push(c);
              colorIndex.set(c, idx);
            }
            px[i] = idx;
            if (idx > max) {
              max = idx;
            }
          }
          const narrow = max < 256;
          tile.versions.push({ v: ++v, t, format: narrow ? 1 : 2, pixels: narrow ? Uint8Array.from(px) : px, paletteVersion: max + 1 });
          bounds[0] = Math.min(bounds[0], x);
          bounds[1] = Math.min(bounds[1], y);
          bounds[2] = Math.max(bounds[2], x);
          bounds[3] = Math.max(bounds[3], y);
          first = Math.min(first, t);
          last = Math.max(last, t);
        } else {
          tile.versions.push({ v: ++v, t, format: 3, pixels: rgb, paletteVersion: 0 });
        }
      }
      tiles.set(tk(z, x, y), tile);
    }
  }
  if (!Number.isFinite(first)) {
    throw new Error(`no level-0 snapshots under ${base}`);
  }
  const ds: CatalogDatastore = {
    force,
    id: dsId,
    window_ms: 60_000,
    horizon: last,
    first,
    last,
    palette_version: palette.length,
    bounds,
    levels,
  };
  const now = last + 5_000;
  const catalog: Catalog = {
    version: 1,
    now,
    static: false,
    live_ms: 5000,
    maps: [
      {
        key: rawMap,
        title: rawMap,
        tile_size: 512,
        instances: [],
        surfaces: [{ name: o.surface, id: `${escapeName(rawMap)}.${escapeName(o.surface)}`, edges: [], datastores: [ds], movers: null }],
      },
    ],
    icons: null,
  };
  return { catalog, stores: new Map([[dsId, { ds, palette: Uint16Array.from(palette), tiles, live: new Map() }]]), now };
}
