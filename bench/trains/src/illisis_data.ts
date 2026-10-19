// Reads train journeys and the final z0 chart from an illisis/minimap static export (the
// test data). Formats: illisis store/movers.ts (movement windows) and shared/codec.ts
// (MMS1 snapshots, MMC1 change files), as bench/internal/benchlib/illisis_stream.go also decodes them. The data
// is untrusted: it is only parsed (JSON, gzip, fixed binary layouts), never executed.

import { readdirSync, readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import { gunzipSync } from "node:zlib";

import type { Journey, TrainSample } from "./types.ts";

/** A gone followed by a sample of the same journey within this long is a crossing. */
export const HANDOFF_MS = 10_000;

/** illisis name(): bytes outside a safe set are "~XX". */
export function illisisName(s: string): string {
  return Array.from(new TextEncoder().encode(s), (b) => {
    const c = String.fromCharCode(b);
    return /[a-z0-9_-]/.test(c) ? c : "~" + b.toString(16).toUpperCase().padStart(2, "0");
  }).join("");
}

function readGzJson(path: string): any {
  return JSON.parse(gunzipSync(readFileSync(path)).toString("utf8"));
}

/** Train journeys of one map surface, from its movement windows (.m.gz). */
export function loadTrainJourneys(root: string, mapDir: string, surfaceDir: string): Journey[] {
  const dir = join(root, mapDir, surfaceDir, "m");
  const files = readdirSync(dir).filter((f) => f.endsWith(".m.gz")).sort();
  const byId = new Map<string, Journey>();
  for (const f of files) {
    const w = readGzJson(join(dir, f));
    for (const row of w.s as number[][]) {
      const m = w.movers[row[0]] as (string | number)[];
      if (m[1] !== "t") {
        continue;
      }
      const id = m[0] as string;
      let j = byId.get(id);
      if (!j) {
        j = { id, force: String(m[2]), name: String(m[3]), samples: [], handoffs: [] };
        byId.set(id, j);
      }
      const t = (w.t as number) + row[1];
      const n = Number(m[4] ?? 0);
      const kinds = String(m[5] ?? "");
      const colour = String(m[6] ?? "");
      if (row.length === 2) {
        j.samples.push({ t, tick: null, x: 0, y: 0, vx: 0, vy: 0, h: null, gone: true, n, kinds, colour });
      } else {
        const [, , x, y, vx, vy, h] = row;
        j.samples.push({ t, tick: null, x, y, vx, vy, h: h === undefined ? null : h, gone: false, n, kinds, colour });
      }
    }
  }
  const out: Journey[] = [];
  for (const j of byId.values()) {
    // Stable sort: rows of one window are in arrival order; equal times keep it.
    j.samples.sort((a, b) => a.t - b.t);
    for (let i = 0; i + 1 < j.samples.length; i++) {
      const a = j.samples[i];
      if (!a.gone) {
        continue;
      }
      const next = j.samples.slice(i + 1).find((s) => !s.gone);
      if (next && next.t - a.t <= HANDOFF_MS) {
        j.handoffs.push([a.t, next.t]);
      }
    }
    if (j.samples.some((s) => !s.gone)) {
      out.push(j);
    }
  }
  out.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  return out;
}

/** RGB565 of the in-game map's rail colour (140, 142, 140). */
export const RAIL_RGB565 = 0x8c71;

const SIZE = 512;
const ORIGIN = 256;

/** Area pixel index of block b's pixel p (illisis codec.ts at()). */
function at(block: number, pos: number): number {
  return (((block >> 4) * 32 + (pos >> 5)) * SIZE) + ((block & 15) * 32) + (pos & 31);
}

/** Applies one MMC1 change file to px, up to time `to`. */
function applyChange(px: Uint16Array, t0: number, b: Uint8Array, to: number): void {
  const v = new DataView(b.buffer, b.byteOffset, b.byteLength);
  if (b.length < 4 || String.fromCharCode(b[0], b[1], b[2], b[3]) !== "MMC1") {
    throw new Error("not MMC1");
  }
  let o = 4;
  while (o < b.length) {
    const dt = v.getUint32(o, true);
    const block = v.getUint8(o + 4);
    const n = v.getUint16(o + 5, true);
    o += 7;
    if (t0 + dt > to) {
      return;
    }
    if (n === 0) {
      const by = (block >> 4) * 32;
      const bx = (block & 15) * 32;
      for (let p = 0; p < 1024; p++) {
        px[(by + (p >> 5)) * SIZE + bx + (p & 31)] = v.getUint16(o + 2 * p, true);
      }
      o += 2048;
    } else {
      for (let k = 0; k < n; k++) {
        px[at(block, v.getUint16(o + 4 * k, true))] = v.getUint16(o + 4 * k + 2, true);
      }
      o += 4 * n;
    }
  }
}

/** The z0 pixels of one area at time `to` (latest snapshot <= to, then change files). */
export function replayArea(areaDir: string, to = Infinity): Uint16Array {
  const index = readGzJson(join(areaDir, "index.json.gz")) as { snaps: number[]; changes: [number, number][] };
  const snaps = index.snaps.filter((s) => s <= to);
  const snap = snaps.length ? snaps[snaps.length - 1] : undefined;
  let px = new Uint16Array(SIZE * SIZE);
  if (snap !== undefined) {
    const b = gunzipSync(readFileSync(join(areaDir, `${snap}.s.gz`)));
    if (b.length !== 4 + 2 * SIZE * SIZE || b.toString("latin1", 0, 4) !== "MMS1") {
      throw new Error("not MMS1");
    }
    px = new Uint16Array(SIZE * SIZE);
    const v = new DataView(b.buffer, b.byteOffset, b.byteLength);
    for (let i = 0; i < SIZE * SIZE; i++) {
      px[i] = v.getUint16(4 + 2 * i, true);
    }
  }
  for (const [first] of index.changes) {
    if (first > to || (snap !== undefined && first <= snap)) {
      continue;
    }
    applyChange(px, first, new Uint8Array(gunzipSync(readFileSync(join(areaDir, `${first}.c.gz`)))), to);
  }
  return px;
}

/** A set of rail tiles with point-to-rail distance queries. */
export class RailMask {
  private areas = new Map<string, Uint8Array>();
  railTiles = 0;

  /** Marks every pixel of colour `rgb565` in the area's pixels. */
  addArea(ax: number, ay: number, px: Uint16Array, rgb565 = RAIL_RGB565): void {
    const m = new Uint8Array(SIZE * SIZE);
    for (let i = 0; i < px.length; i++) {
      if (px[i] === rgb565) {
        m[i] = 1;
        this.railTiles++;
      }
    }
    this.areas.set(`${ax}_${ay}`, m);
  }

  /** Marks one tile (tests). */
  setTile(tx: number, ty: number): void {
    const ax = Math.floor((tx + ORIGIN) / SIZE);
    const ay = Math.floor((ty + ORIGIN) / SIZE);
    let m = this.areas.get(`${ax}_${ay}`);
    if (!m) {
      m = new Uint8Array(SIZE * SIZE);
      this.areas.set(`${ax}_${ay}`, m);
    }
    m[(ty + ORIGIN - ay * SIZE) * SIZE + (tx + ORIGIN - ax * SIZE)] = 1;
    this.railTiles++;
  }

  isRail(tx: number, ty: number): boolean {
    const ax = Math.floor((tx + ORIGIN) / SIZE);
    const ay = Math.floor((ty + ORIGIN) / SIZE);
    const m = this.areas.get(`${ax}_${ay}`);
    return m !== undefined && m[(ty + ORIGIN - ay * SIZE) * SIZE + (tx + ORIGIN - ax * SIZE)] === 1;
  }

  /**
   * Euclidean distance from point (x, y) to the nearest rail tile (a unit square), 0
   * inside one; searched up to `maxR` tiles, beyond which maxR is returned.
   */
  distance(x: number, y: number, maxR = 32): number {
    const tx = Math.floor(x);
    const ty = Math.floor(y);
    if (this.isRail(tx, ty)) {
      return 0;
    }
    let best = Infinity;
    for (let r = 1; r <= maxR; r++) {
      // The nearest point of a tile in ring r is at least r-1 tiles away.
      if (best <= r - 1) {
        break;
      }
      for (let dy = -r; dy <= r; dy++) {
        const edge = dy === -r || dy === r;
        for (let dx = -r; dx <= r; dx += edge ? 1 : 2 * r) {
          const qx = tx + dx;
          const qy = ty + dy;
          if (!this.isRail(qx, qy)) {
            continue;
          }
          const ex = Math.max(qx - x, 0, x - (qx + 1));
          const ey = Math.max(qy - y, 0, y - (qy + 1));
          const d = Math.hypot(ex, ey);
          if (d < best) {
            best = d;
          }
        }
      }
    }
    return Math.min(best, maxR);
  }
}

/** The rail mask of a surface from the final z0 chart of every area. */
export function loadRailMask(root: string, mapDir: string, surfaceDir: string, forceDir = "player", to = Infinity): RailMask {
  const base = join(root, mapDir, surfaceDir, forceDir, "0");
  const mask = new RailMask();
  for (const d of readdirSync(base)) {
    const m = /^(-?\d+)_(-?\d+)$/.exec(d);
    if (!m || !existsSync(join(base, d, "index.json.gz"))) {
      continue;
    }
    mask.addArea(Number(m[1]), Number(m[2]), replayArea(join(base, d), to));
  }
  return mask;
}

/** Raw samples of a journey that are not gones. */
export function moving(samples: TrainSample[]): TrainSample[] {
  return samples.filter((s) => !s.gone);
}
