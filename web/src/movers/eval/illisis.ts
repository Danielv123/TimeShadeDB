// The illisis/minimap viewer's train placement, ported for the train evaluator only, as the
// baseline the v3 viewer is measured against. Not used for drawing.
//
// Ported from illisis-minimap web/view/movers.ts (place, heading, line, carriages) at 91a6461.
// Copyright (c) illisis-minimap contributors. Licensed under the Blue Oak Model License 1.0.0
// (https://blueoakcouncil.org/license/1.0.0); this notice must be kept with the code.

/** A raw illisis sample (with the captured velocity), or a gone. */
export interface RawRow {
  t: number;
  x: number;
  y: number;
  vx: number;
  vy: number;
  /** Orientation in turns. */
  h: number;
  gone: boolean;
}

export const CAP = 60_000;
export const BLEND = 500;
const PITCH = 7;

export interface IllisisCar {
  x: number;
  y: number;
  a: number;
}

export interface IllisisShown {
  x: number;
  y: number;
  path: [number, number][];
  cars: IllisisCar[];
}

/** Index of the last row with t' <= t among the first `count` rows, or -1. */
export function findRow(evs: RawRow[], count: number, t: number): number {
  let lo = 0;
  let hi = count - 1;
  if (hi < 0 || evs[0].t > t) {
    return -1;
  }
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (evs[mid].t <= t) {
      lo = mid;
    } else {
      hi = mid - 1;
    }
  }
  return lo;
}

/**
 * illisis `Tracks.at` + `place` for one mover at t, seeing only the first `count` rows
 * (rows that have arrived). Returns null when not shown (no row yet, or behind a gone).
 */
export function illisisAt(evs: RawRow[], count: number, t: number, n: number): IllisisShown | null {
  const i = findRow(evs, count, t);
  if (i < 0 || evs[i].gone) {
    return null;
  }
  const e = evs[i];
  const next = i + 1 < count ? evs[i + 1] : undefined;
  const run = (u: number): [number, number] => {
    const s = Math.min(u - e.t, CAP) / 1000;
    return [e.x + e.vx * s, e.y + e.vy * s];
  };
  let [x, y] = run(t);
  if (next && !next.gone && next.t - e.t <= CAP) {
    const [px, py] = run(next.t);
    const f = Math.max(0, 1 - (next.t - t) / Math.min(BLEND, next.t - e.t));
    x += f * (next.x - px);
    y += f * (next.y - py);
  }
  const [hx, hy] = heading(evs, i);
  const path = line(evs, i, x, y, hx, hy, n * PITCH);
  return { x, y, path, cars: carriages(path, n) };
}

function heading(evs: RawRow[], i: number): [number, number] {
  const e = evs[i];
  const prev = evs[i - 1];
  if (e.vx === 0 && e.vy === 0) {
    const a = e.h * 2 * Math.PI;
    return [Math.sin(a), -Math.cos(a)];
  }
  const [dx, dy] = e.vx !== 0 || e.vy !== 0 || !prev || prev.gone ? [e.vx, e.vy] : [e.x - prev.x, e.y - prev.y];
  const len = Math.hypot(dx, dy);
  return len === 0 ? [1, 0] : [dx / len, dy / len];
}

function line(evs: RawRow[], i: number, x: number, y: number, hx: number, hy: number, length: number): [number, number][] {
  const out: [number, number][] = [[x, y]];
  let left = length;
  for (let j = i; j >= 0 && !evs[j].gone; j--) {
    const [px, py] = out[out.length - 1];
    const d = Math.hypot(evs[j].x - px, evs[j].y - py);
    if (d > 0 && d >= left) {
      out.push([px + (evs[j].x - px) * left / d, py + (evs[j].y - py) * left / d]);
      return out;
    }
    if (d > 0) {
      out.push([evs[j].x, evs[j].y]);
      left -= d;
    }
  }
  const [px, py] = out[out.length - 1];
  const [qx, qy] = out.length > 1 ? out[out.length - 2] : [px + hx, py + hy];
  const d = Math.hypot(px - qx, py - qy);
  out.push([px + (px - qx) * left / d, py + (py - qy) * left / d]);
  return out;
}

function carriages(path: [number, number][], n: number): IllisisCar[] {
  const cars: IllisisCar[] = [];
  let i = 0;
  let walked = 0;
  for (let j = 1; j < path.length && i < n; j++) {
    const [ax, ay] = path[j - 1];
    const [bx, by] = path[j];
    const len = Math.hypot(bx - ax, by - ay);
    while (i < n && i * PITCH <= walked + len) {
      const f = len === 0 ? 0 : (i * PITCH - walked) / len;
      cars.push({ x: ax + f * (bx - ax), y: ay + f * (by - ay), a: Math.atan2(ay - by, ax - bx) });
      i += 1;
    }
    walked += len;
  }
  return cars;
}
