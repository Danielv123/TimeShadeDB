// Drawing trains, vehicles and players on the 2D overlay canvas, as the in-game map draws
// them: carriages by kind (L locomotive with a point, F fluid capsule, A artillery disc and
// barrel, else a box; upper case faces the leading end), connection bars, minimum pixel
// sizes so far trains stay visible; a car's triangle, a spidertron's legs; players as dots
// with a heading line and, zoomed in, their name.
//
// Shapes and colours are ported from illisis-minimap web/view/train.ts, vehicle.ts and
// layers.ts at 91a6461. Copyright (c) illisis-minimap contributors. Licensed under the Blue
// Oak Model License 1.0.0 (https://blueoakcouncil.org/license/1.0.0); keep this notice.
// Changed here: a train's carriages go into one path (one fill, one stroke) computed in screen
// space, instead of a save/rotate/restore per carriage, so 500 trains stay cheap; game text
// is drawn only with fillText/strokeText.

import type { Shown } from "./view";

/** The slice of CanvasRenderingContext2D the drawing uses (tests pass a recorder). */
export interface Pen {
  fillStyle: string | CanvasGradient | CanvasPattern;
  strokeStyle: string | CanvasGradient | CanvasPattern;
  lineWidth: number;
  lineJoin: CanvasLineJoin;
  font: string;
  textBaseline: CanvasTextBaseline;
  save(): void;
  restore(): void;
  translate(x: number, y: number): void;
  rotate(a: number): void;
  beginPath(): void;
  moveTo(x: number, y: number): void;
  lineTo(x: number, y: number): void;
  closePath(): void;
  arc(x: number, y: number, r: number, a0: number, a1: number): void;
  fill(): void;
  stroke(): void;
  fillText(text: string, x: number, y: number): void;
  strokeText(text: string, x: number, y: number): void;
}

/** World-to-screen mapping of a frame (layer.ts Camera + Viewport). */
export interface Projection {
  /** World tile at the canvas centre. */
  cx: number;
  cy: number;
  /** CSS pixels per tile. */
  scale: number;
  width: number;
  height: number;
}

/** The chart's `vehicle_outer_color` and wagon connection colour. */
export const OUTLINE_COLOUR = "#ff1a1a";
const TRAIN_FILL = "#f9ce3e";
// Carriage outer size in tiles, measured from the in-game map.
const LENGTH = 4.9;
const WIDTH = 2.4;
const OUTLINE = 0.5;
const BAR = 0.8;
const BARREL = 1.4;
const SHOULDER = 2.5;
// Vehicles, in tiles.
const V_LENGTH = 4.78;
const V_WIDTH = 4.65;
const DISC = 2.2;
const BELLY = 3.8;
const HALF = 0.55;
const TIP = 5.2;
const REACH = TIP + OUTLINE;
const FAN = [-35, -10, 10, 35].map(d => (d * Math.PI) / 180);
const V_MIN = 8;
/** Players are named from this zoom (CSS px per tile). */
export const NAME_SCALE = 1;

function sx(p: Projection, x: number): number {
  return (x - p.cx) * p.scale + p.width / 2;
}

function sy(p: Projection, y: number): number {
  return (y - p.cy) * p.scale + p.height / 2;
}

/** Whether a mover at world (x, y) with radius r tiles can be on screen. */
export function onScreen(p: Projection, x: number, y: number, r: number): boolean {
  const X = sx(p, x);
  const Y = sy(p, y);
  const R = r * p.scale + 12;
  return X > -R && Y > -R && X < p.width + R && Y < p.height + R;
}

/**
 * Draws every train in `movers` (kind "t"): per train its connection bars in one stroke and
 * its carriage outlines in one path, filled and stroked once. One path per train, not one
 * for the whole frame: Chromium measured 4.4 ms for 2,500 carriages that way against 22 ms
 * for a single 2,500-polygon path. Artillery wagons, whose disc must cover the barrel, are
 * drawn afterwards one by one.
 */
export function drawTrains(ctx: Pen, p: Projection, movers: Shown[]): number {
  const k = p.scale;
  const l = Math.max(LENGTH * k, 3);
  const w = Math.max(WIDTH * k, 2);
  const o = Math.max(OUTLINE * k, 1);
  const bar = Math.max(BAR * k, 1);
  const hl = (l - o) / 2;
  const hw = (w - o) / 2;
  let drawn = 0;
  const artillery: [number, number, number, number][] = [];
  ctx.fillStyle = TRAIN_FILL;
  ctx.strokeStyle = OUTLINE_COLOUR;
  ctx.lineJoin = "miter";
  for (const m of movers) {
    if (m.kind !== "t" || !onScreen(p, m.x, m.y, m.cars.length * 7 + 4)) {
      continue;
    }
    drawn++;
    if (m.cars.length > 1) {
      ctx.beginPath();
      let bars = false;
      for (let i = 1; i < m.cars.length; i++) {
        const ax = sx(p, m.cars[i - 1].x);
        const ay = sy(p, m.cars[i - 1].y);
        const bx = sx(p, m.cars[i].x);
        const by = sy(p, m.cars[i].y);
        const d = Math.hypot(bx - ax, by - ay) || 1;
        if (d > l) {
          const ux = (bx - ax) / d;
          const uy = (by - ay) / d;
          ctx.moveTo(ax + (ux * l) / 2, ay + (uy * l) / 2);
          ctx.lineTo(bx - (ux * l) / 2, by - (uy * l) / 2);
          bars = true;
        }
      }
      if (bars) {
        ctx.lineWidth = bar;
        ctx.stroke();
      }
    }
    ctx.beginPath();
    if (m.cars.length === 0) {
      // No body (far zoom): a locomotive-sized marker along the heading.
      outline(ctx, sx(p, m.x), sy(p, m.y), Math.atan2(m.hy, m.hx), "L", hl, hw, l);
    }
    for (const c of m.cars) {
      const kind = c.kind.toUpperCase();
      if (kind === "A") {
        artillery.push([sx(p, c.x), sy(p, c.y), c.a, c.kind === kind ? 1 : -1]);
        continue;
      }
      outline(ctx, sx(p, c.x), sy(p, c.y), c.a, c.kind, hl, hw, l);
    }
    ctx.fill();
    ctx.lineWidth = o;
    ctx.stroke();
  }
  for (const [x, y, a, f] of artillery) {
    const cosA = Math.cos(a);
    const sinA = Math.sin(a);
    const at = (lx: number, ly: number): [number, number] => [x + lx * cosA - ly * sinA, y + lx * sinA + ly * cosA];
    const cx = -f * (hl - hw);
    const hb = Math.max((w * BARREL) / WIDTH - o, 0) / 2;
    ctx.beginPath();
    ctx.moveTo(...at(cx, -hb));
    ctx.lineTo(...at(f * hl, -hb));
    ctx.lineTo(...at(f * hl, hb));
    ctx.lineTo(...at(cx, hb));
    ctx.closePath();
    finish(ctx, o);
    ctx.beginPath();
    const [dx, dy] = at(cx, 0);
    ctx.moveTo(dx + hw, dy);
    ctx.arc(dx, dy, hw, 0, 2 * Math.PI);
    finish(ctx, o);
  }
  return drawn;
}

/** Adds one carriage outline (screen centre, angle towards the leading end) to the current path. */
function outline(ctx: Pen, x: number, y: number, a: number, kindLetter: string, hl: number, hw: number, l: number): void {
  const cosA = Math.cos(a);
  const sinA = Math.sin(a);
  const X = (lx: number, ly: number): number => x + lx * cosA - ly * sinA;
  const Y = (lx: number, ly: number): number => y + lx * sinA + ly * cosA;
  const kind = kindLetter.toUpperCase();
  const f = kindLetter === kind ? 1 : -1;
  if (kind === "L") {
    const s = f * (-(l / 2) + (l * SHOULDER) / LENGTH);
    ctx.moveTo(X(-f * hl, -hw), Y(-f * hl, -hw));
    ctx.lineTo(X(s, -hw), Y(s, -hw));
    ctx.lineTo(X(f * hl, 0), Y(f * hl, 0));
    ctx.lineTo(X(s, hw), Y(s, hw));
    ctx.lineTo(X(-f * hl, hw), Y(-f * hl, hw));
    ctx.closePath();
  } else if (kind === "F") {
    const e = Math.max(hl - hw, 0);
    ctx.moveTo(X(-e, -hw), Y(-e, -hw));
    ctx.lineTo(X(e, -hw), Y(e, -hw));
    ctx.arc(X(e, 0), Y(e, 0), hw, a - Math.PI / 2, a + Math.PI / 2);
    ctx.lineTo(X(-e, hw), Y(-e, hw));
    ctx.arc(X(-e, 0), Y(-e, 0), hw, a + Math.PI / 2, a + (3 * Math.PI) / 2);
    ctx.closePath();
  } else {
    ctx.moveTo(X(-hl, -hw), Y(-hl, -hw));
    ctx.lineTo(X(hl, -hw), Y(hl, -hw));
    ctx.lineTo(X(hl, hw), Y(hl, hw));
    ctx.lineTo(X(-hl, hw), Y(-hl, hw));
    ctx.closePath();
  }
}

function finish(ctx: Pen, o: number): void {
  ctx.fillStyle = TRAIN_FILL;
  ctx.fill();
  ctx.strokeStyle = OUTLINE_COLOUR;
  ctx.lineJoin = "miter";
  ctx.lineWidth = o;
  ctx.stroke();
}

/** A car or tank (triangle along the heading) or a spidertron (kinds "S": disc with 8 legs). */
export function drawVehicle(ctx: Pen, p: Projection, m: Shown): void {
  const k = p.scale;
  const o = Math.max(OUTLINE * k, 1);
  ctx.save();
  ctx.translate(sx(p, m.x), sy(p, m.y));
  ctx.strokeStyle = OUTLINE_COLOUR;
  ctx.lineWidth = o;
  ctx.lineJoin = "miter";
  if (m.meta.o === "S") {
    const q = Math.max(k, V_MIN / (2 * REACH));
    const legs = (draw: () => void): void => {
      for (const side of [1, -1]) {
        for (const a of FAN) {
          const c = side * Math.cos(a);
          const n = Math.sin(a);
          const at = (x: number, y: number): [number, number] => [x * c - y * n, x * n + y * c];
          ctx.beginPath();
          ctx.moveTo(...at(DISC * q, 0));
          ctx.lineTo(...at(BELLY * q, HALF * q));
          ctx.lineTo(...at(TIP * q, 0));
          ctx.lineTo(...at(BELLY * q, -HALF * q));
          ctx.closePath();
          draw();
        }
      }
      ctx.beginPath();
      ctx.arc(0, 0, DISC * q, 0, 2 * Math.PI);
      draw();
    };
    // One outline around the silhouette: stroke every part at twice the width, then fill over it.
    ctx.lineJoin = "round";
    ctx.lineWidth = 2 * o;
    legs(() => ctx.stroke());
    ctx.fillStyle = "#ffffff";
    legs(() => ctx.fill());
  } else {
    const w = Math.max(V_WIDTH * k, V_MIN);
    const l = Math.max(V_LENGTH * k, V_MIN);
    const leg = Math.hypot(w / 2, l);
    const r = (w * l) / (w + 2 * leg);
    const f = (r - o / 2) / r;
    const top = -(l / 2);
    const bottom = l / 2;
    const iy = bottom - r;
    const ty = iy + (top - iy) * f;
    const by = iy + (bottom - iy) * f;
    const hw = (w / 2) * f;
    ctx.rotate(Math.atan2(m.hx, -m.hy));
    ctx.fillStyle = "#e5e5e5";
    ctx.beginPath();
    ctx.moveTo(0, ty);
    ctx.lineTo(hw, by);
    ctx.lineTo(-hw, by);
    ctx.closePath();
    ctx.fill();
    ctx.stroke();
  }
  ctx.restore();
}

/** A player: a white dot with a heading line; named at NAME_SCALE and closer. */
export function drawPlayer(ctx: Pen, p: Projection, m: Shown): void {
  const x = sx(p, m.x);
  const y = sy(p, m.y);
  ctx.strokeStyle = "#ffffff";
  ctx.fillStyle = "#ffffff";
  ctx.lineWidth = 2;
  ctx.beginPath();
  ctx.arc(x, y, 4, 0, 2 * Math.PI);
  ctx.fill();
  ctx.beginPath();
  ctx.moveTo(x, y);
  ctx.lineTo(x + 9 * m.hx, y + 9 * m.hy);
  ctx.stroke();
  if (p.scale >= NAME_SCALE && m.meta.name) {
    ctx.font = "12px sans-serif";
    ctx.textBaseline = "middle";
    ctx.strokeStyle = "#000000";
    ctx.lineWidth = 3;
    ctx.strokeText(m.meta.name, x + 8, y);
    ctx.fillText(m.meta.name, x + 8, y);
  }
}

/** Draws a frame's movers: trains batched, then vehicles, then players on top. Returns how many were drawn. */
export function drawMovers(ctx: Pen, p: Projection, movers: Shown[]): number {
  let n = drawTrains(ctx, p, movers);
  for (const m of movers) {
    if (m.kind === "v" && onScreen(p, m.x, m.y, 6)) {
      drawVehicle(ctx, p, m);
      n++;
    }
  }
  for (const m of movers) {
    if (m.kind === "p" && onScreen(p, m.x, m.y, 2)) {
      drawPlayer(ctx, p, m);
      n++;
    }
  }
  return n;
}

/** The mover nearest to screen point (x, y) within `radius` px, or null. */
export function hitMover(p: Projection, movers: Shown[], x: number, y: number, radius = 8): Shown | null {
  let best: Shown | null = null;
  let bd = radius;
  for (const m of movers) {
    const pts = m.cars.length > 0 ? m.cars : [m];
    for (const c of pts) {
      const d = Math.hypot(sx(p, c.x) - x, sy(p, c.y) - y);
      if (d < bd) {
        bd = d;
        best = m;
      }
    }
  }
  return best;
}
