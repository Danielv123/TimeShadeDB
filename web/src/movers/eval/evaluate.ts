// Train rendering metrics (PLAN.md §5 "Trains"), shared by the CLI (train_eval.mjs) and the
// tests. Inputs are illisis-style journeys (research out/trains.jsonl) and a rail bitmask of
// the final chart (research scripts/rail_mask.py). Pure computation, no Node APIs.
//
// Metrics:
// - leave-one-out: hide sample b, rebuild it at t_b from its neighbours, error in tiles;
// - render: draw every journey at 10 Hz, distance of every carriage centre to the nearest
//   rail pixel (share > 1 tile, p99, count > 2 tiles outside handoffs), and frame-to-frame
//   head jumps larger than v*dt + 2 tiles outside handoffs;
// - both for the v3 viewer (track.ts/view.ts) and for the illisis baseline (illisis.ts).

import type { MoverMeta } from "../../api/types";
import { DelayEstimator } from "../clock";
import { hermite } from "../hermite";
import { F_GONE, F_HANDOFF, F_NO_TICK, F_STOP, HANDOFF_MS, MAX_SEGMENT_MS, Track, headAt, isEndSwitch, makeSample, newHead, type Sample } from "../track";
import { MoverView } from "../view";
import { BLEND, type RawRow, findRow, illisisAt } from "./illisis";

export interface Journey {
  id: string;
  surface: string;
  n: number;
  kinds: string;
  rows: RawRow[];
}

/** Parses one line of research out/trains.jsonl. */
export function parseJourney(line: string): Journey {
  const d = JSON.parse(line) as { surface: string; j: string; n: number[]; kinds: string[]; samples: (number | string)[][] };
  const rows: RawRow[] = d.samples.map(s =>
    s[1] === "gone"
      ? { t: s[0] as number, x: 0, y: 0, vx: 0, vy: 0, h: 0, gone: true }
      : { t: s[0] as number, x: s[1] as number, y: s[2] as number, vx: s[3] as number, vy: s[4] as number, h: s[5] as number, gone: false },
  );
  rows.sort((a, b) => a.t - b.t);
  // Same-time rows: keep the last (as the store does).
  const dedup: RawRow[] = [];
  for (const r of rows) {
    if (dedup.length > 0 && dedup[dedup.length - 1].t === r.t) {
      dedup[dedup.length - 1] = r;
    } else {
      dedup.push(r);
    }
  }
  return { id: d.j, surface: d.surface, n: Math.max(...d.n), kinds: d.kinds[0] ?? "", rows: dedup };
}

/** Drops samples stamped on a 5-minute window boundary by the illisis store clamp (t mod 300000 <= 2). */
export function dropClamped(j: Journey, windowMs = 300_000): Journey {
  return { ...j, rows: j.rows.filter(r => r.gone || r.t % windowMs > 2) };
}

const q16 = (v: number): number => Math.round(v * 16) / 16;
const qo = (o: number): number => (((Math.round(o * 65536) % 65536) + 65536) % 65536) / 65536;

/** The journey as the v3 store holds it: positions in 1/16 tile, u16 orientation, no velocity, no tick. */
export function toSamples(j: Journey): { meta: MoverMeta; samples: Sample[] } {
  const meta: MoverMeta = { id: j.id, k: "t", f: "player", name: "locomotive", n: j.n, o: j.kinds };
  const samples: Sample[] = [];
  for (let i = 0; i < j.rows.length; i++) {
    const r = j.rows[i];
    if (r.gone) {
      const next = j.rows[i + 1];
      const handoff = next && !next.gone && next.t - r.t <= HANDOFF_MS ? F_HANDOFF : 0;
      samples.push(makeSample(meta, r.t, 0, 0, 0, F_GONE | F_NO_TICK | handoff));
      continue;
    }
    const stop = r.vx === 0 && r.vy === 0 ? F_STOP : 0;
    samples.push(makeSample(meta, r.t, q16(r.x), q16(r.y), qo(r.h), F_NO_TICK | stop));
  }
  return { meta, samples };
}

/** Rail pixels of the final chart (rail_mask.py output). */
export class RailMask {
  private readonly areas = new Map<number, Uint8Array>();

  constructor(bytes: Uint8Array) {
    const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    if (String.fromCharCode(...bytes.subarray(0, 4)) !== "RMSK") {
      throw new Error("not a rail mask");
    }
    const n = v.getUint32(4, true);
    let off = 8;
    for (let i = 0; i < n; i++) {
      const ax = v.getInt32(off, true);
      const ay = v.getInt32(off + 4, true);
      this.areas.set(key(ax, ay), bytes.subarray(off + 8, off + 8 + 32768));
      off += 8 + 32768;
    }
  }

  /** Whether world tile (ix, iy) is a rail pixel. */
  rail(ix: number, iy: number): boolean {
    const gx = ix + 256;
    const gy = iy + 256;
    const ax = Math.floor(gx / 512);
    const ay = Math.floor(gy / 512);
    const a = this.areas.get(key(ax, ay));
    if (!a) {
      return false;
    }
    const i = (gy - ay * 512) * 512 + (gx - ax * 512);
    return (a[i >> 3] & (1 << (i & 7))) !== 0;
  }

  /** Distance from point (x, y) to the nearest rail pixel square, capped at maxR. */
  distance(x: number, y: number, maxR = 4): number {
    const ix = Math.floor(x);
    const iy = Math.floor(y);
    if (this.rail(ix, iy)) {
      return 0;
    }
    let best = maxR;
    for (let r = 1; r <= Math.ceil(maxR); r++) {
      if (r - 1 >= best) {
        break;
      }
      for (let dy = -r; dy <= r; dy++) {
        for (let dx = -r; dx <= r; dx++) {
          if (Math.max(Math.abs(dx), Math.abs(dy)) !== r) {
            continue;
          }
          const px = ix + dx;
          const py = iy + dy;
          const ex = x < px ? px - x : x > px + 1 ? x - px - 1 : 0;
          const ey = y < py ? py - y : y > py + 1 ? y - py - 1 : 0;
          const d = Math.hypot(ex, ey);
          if (d < best && this.rail(px, py)) {
            best = d;
          }
        }
      }
    }
    return best;
  }
}

function key(ax: number, ay: number): number {
  return (ax + 4096) * 8192 + (ay + 4096);
}

/** A weighted histogram of distances (0.01 tile bins). */
export class Hist {
  readonly bins: Float64Array;
  readonly width: number;
  total = 0;
  constructor(width = 0.01, max = 20) {
    this.width = width;
    this.bins = new Float64Array(Math.ceil(max / width) + 1);
  }

  add(v: number, w = 1): void {
    this.bins[Math.min(this.bins.length - 1, Math.floor(v / this.width))] += w;
    this.total += w;
  }

  quantile(q: number): number {
    let acc = 0;
    const target = q * this.total;
    for (let i = 0; i < this.bins.length; i++) {
      acc += this.bins[i];
      if (acc >= target && this.bins[i] > 0) {
        return (i + 1) * this.width;
      }
    }
    return NaN;
  }

  /** Share of weight strictly above v (bin resolution). */
  above(v: number): number {
    let acc = 0;
    for (let i = Math.floor(v / this.width) + 1; i < this.bins.length; i++) {
      acc += this.bins[i];
    }
    return this.total > 0 ? acc / this.total : NaN;
  }
}

/** Quantile of an array (sorts a copy). */
export function quantile(a: ArrayLike<number>, q: number): number {
  if (a.length === 0) {
    return NaN;
  }
  const s = Float64Array.from(a).sort();
  return s[Math.min(s.length - 1, Math.floor(q * (s.length - 1)))];
}

export interface LooResult {
  /** method -> errors (tiles) for all triples and for triples on curves. */
  all: Record<string, number[]>;
  curves: Record<string, number[]>;
}

/**
 * Leave-one-out over consecutive non-gone triples (a, b, c) with a moving and c.t - a.t <= 3 s,
 * as research scripts/train_interp_eval.mjs: illisis viewer, linear, Hermite with the captured
 * velocity (reference, not available in v3) and the v3 viewer on quantized store rows.
 */
export function leaveOneOut(journeys: Journey[], v3Modes: Record<string, () => void>, restore: () => void): LooResult {
  const all: Record<string, number[]> = { illisis: [], linear: [], "hermite-captured-v": [] };
  const curves: Record<string, number[]> = { illisis: [], linear: [], "hermite-captured-v": [] };
  for (const m of Object.keys(v3Modes)) {
    all[m] = [];
    curves[m] = [];
  }
  const tmp = new Float64Array(4);
  const head = newHead();
  for (const j of journeys) {
    const { meta, samples } = toSamples(j);
    const r = j.rows;
    for (let i = 1; i + 1 < r.length; i++) {
      const a = r[i - 1];
      const b = r[i];
      const c = r[i + 1];
      if (a.gone || b.gone || c.gone) {
        continue;
      }
      const T = c.t - a.t;
      if (T <= 0 || T > 3000 || !(a.vx || a.vy) || b.t === a.t) {
        continue;
      }
      const s = (b.t - a.t) / 1000;
      const S = T / 1000;
      const u = (b.t - a.t) / T;
      // illisis viewer
      let vx = a.x + a.vx * s;
      let vy = a.y + a.vy * s;
      const px = a.x + a.vx * S;
      const py = a.y + a.vy * S;
      const f = Math.max(0, 1 - (c.t - b.t) / Math.min(BLEND, T));
      vx += f * (c.x - px);
      vy += f * (c.y - py);
      const lx = a.x + (c.x - a.x) * u;
      const ly = a.y + (c.y - a.y) * u;
      hermite(a.x, a.y, a.vx, a.vy, c.x, c.y, c.vx, c.vy, S, u, tmp);
      const curve = Math.abs(a.vx * c.vy - a.vy * c.vx) / (Math.hypot(a.vx, a.vy) * Math.hypot(c.vx, c.vy) || 1) > 0.05;
      const push = (k: string, x: number, y: number): void => {
        const e = Math.hypot(x - b.x, y - b.y);
        all[k].push(e);
        if (curve) {
          curves[k].push(e);
        }
      };
      push("illisis", vx, vy);
      push("linear", lx, ly);
      push("hermite-captured-v", tmp[0], tmp[1]);
      // v3: a local track of the rows around b without b (the neighbours give the tangents).
      for (const [name, setup] of Object.entries(v3Modes)) {
        setup();
        const tr = new Track(meta.id, meta);
        for (const k of [i - 2, i - 1, i + 1, i + 2]) {
          if (k >= 0 && k < samples.length) {
            tr.insert({ ...samples[k], vv: -1 });
          }
        }
        headAt(tr, b.t, Infinity, head);
        push(name, head.visible ? head.x : NaN, head.visible ? head.y : NaN);
      }
      restore();
    }
  }
  return { all, curves };
}

export interface RenderStats {
  frames: number;
  movingFrames: number;
  cars: Hist;
  movingCars: Hist;
  /** Carriage-frames more than 2 tiles from a rail outside 1 s around handoffs and end switches. */
  over2: number;
  /** The same, counting only frames where the head moved. */
  movingOver2: number;
  headJumps: number;
  carJumps: number;
  /** Frames inside the 1 s windows around handoffs (excluded from over2 and jumps). */
  handoffFrames: number;
  /**
   * Carriages of frames whose two bracketing raw samples are both within 1 tile of a rail
   * pixel: where the mask covers the track (raw samples themselves are off the mask in
   * places), so distances there measure the drawing, not mask holes.
   */
  validCars: Hist;
  validOver2: number;
}

function newStats(): RenderStats {
  return { frames: 0, movingFrames: 0, cars: new Hist(), movingCars: new Hist(), over2: 0, movingOver2: 0, headJumps: 0, carJumps: 0, handoffFrames: 0, validCars: new Hist(), validOver2: 0 };
}

export interface RenderOptions {
  /** "history": every row known; "live": rows arrive in 1 s batches (+150 ms), drawn behind live. */
  mode: "history" | "live";
  /** Frame step in ms (100 = 10 Hz). */
  step?: number;
  /** Fixed delay for the illisis baseline in live mode (its Clusterio page uses 2 s). */
  illisisDelay?: number;
}

/** When a row reaches the viewer in the live model: the next 1 s batch boundary plus 150 ms. */
export function arrival(t: number): number {
  return Math.ceil(t / 1000) * 1000 + 150;
}

/** Times around handoffs (gone followed by the same journey within 10 s) and end switches, +-1 s. */
export function exclusions(j: Journey, tr: Track): [number, number][] {
  const out: [number, number][] = [];
  const r = j.rows;
  for (let i = 0; i < r.length; i++) {
    if (r[i].gone) {
      let k = i + 1;
      if (k < r.length && !r[k].gone && r[k].t - r[i].t <= HANDOFF_MS) {
        const before = i > 0 ? r[i - 1].t : r[i].t;
        out.push([before - 1000, r[k].t + 1000]);
      }
    }
  }
  for (let k = 0; k + 1 < tr.s.length; k++) {
    const a = tr.s[k];
    const b = tr.s[k + 1];
    if (isEndSwitch(tr, k)) {
      out.push([a.t - 1000, b.t + 1000]);
    } else if ((a.flags & F_GONE) === 0 && (b.flags & F_GONE) === 0 && b.t - a.t > MAX_SEGMENT_MS && Math.hypot(b.x - a.x, b.y - a.y) > 2) {
      // a data gap the mover crossed: its reappearance is a jump no viewer can avoid
      out.push([b.t - 1000, b.t + 1000]);
    }
  }
  out.sort((a, b) => a[0] - b[0]);
  return out;
}

/**
 * Reference speed at t for the jump metric: the larger captured speed of the rows around it,
 * or 1.5 x the average speed between them when that is larger (a train that accelerates and
 * brakes between two samples moves faster than either sample says).
 */
export function refSpeed(rows: RawRow[], t: number): number {
  const i = findRow(rows, rows.length, t);
  let v = 0;
  for (const k of [i, i + 1]) {
    if (k >= 0 && k < rows.length && !rows[k].gone) {
      v = Math.max(v, Math.hypot(rows[k].vx, rows[k].vy));
    }
  }
  if (i >= 0 && i + 1 < rows.length && !rows[i].gone && !rows[i + 1].gone && rows[i + 1].t > rows[i].t) {
    const a = rows[i];
    const b = rows[i + 1];
    v = Math.max(v, (1.5 * Math.hypot(b.x - a.x, b.y - a.y)) / ((b.t - a.t) / 1000));
  }
  return v;
}

/** Spans [t_i, t_i+1) where a journey is parked: equal positions, zero captured speed. Drawing is static there. */
function staticSpans(rows: RawRow[]): [number, number][] {
  const out: [number, number][] = [];
  for (let i = 0; i + 1 < rows.length; i++) {
    const a = rows[i];
    const b = rows[i + 1];
    if (!a.gone && !b.gone && a.x === b.x && a.y === b.y && a.vx === 0 && a.vy === 0 && b.vx === 0 && b.vy === 0 && b.t - a.t > 2000) {
      // Keep 1 s at both ends drawn normally (a body can still settle after the stop).
      out.push([a.t + 1000, b.t - 1000]);
    }
  }
  return out;
}

class Spans {
  private i = 0;
  private readonly spans: [number, number][];
  constructor(spans: [number, number][]) {
    this.spans = spans;
  }
  /** Index of the span holding t (t non-decreasing between calls), or -1. */
  at(t: number): number {
    while (this.i < this.spans.length && this.spans[this.i][1] <= t) {
      this.i++;
    }
    return this.i < this.spans.length && this.spans[this.i][0] <= t ? this.i : -1;
  }
}

interface Prev {
  x: number;
  y: number;
  cars: number[];
  d: number[];
  span: number;
  valid: boolean;
}

/**
 * Draws every journey at 10 Hz from its first to its last row with the v3 viewer and with
 * the illisis baseline and measures carriage distance to rails and jumps.
 */
export function renderEval(journeys: Journey[], mask: RailMask, opts: RenderOptions): { v3: RenderStats; illisis: RenderStats; rawHead: Hist } {
  const step = opts.step ?? 100;
  const v3 = newStats();
  const il = newStats();
  const rawHead = new Hist();
  for (const j of journeys) {
    const rawD = new Float64Array(j.rows.length).fill(Infinity);
    j.rows.forEach((r, i) => {
      if (!r.gone) {
        rawD[i] = mask.distance(r.x, r.y);
        rawHead.add(rawD[i]);
      }
    });
    const { meta, samples } = toSamples(j);
    const full = new Track(meta.id, meta);
    for (const s of samples) {
      full.insert({ ...s });
    }
    const excl = new Spans(exclusions(j, full));
    const exclI = new Spans(exclusions(j, full));
    const stV = new Spans(staticSpans(j.rows));
    const stI = new Spans(staticSpans(j.rows));
    const view = new MoverView();
    const est = new DelayEstimator(2000);
    let fed = 0;
    if (opts.mode === "history") {
      for (const s of samples) {
        view.add(meta, { ...s });
      }
      fed = samples.length;
    }
    const rows = j.rows;
    const t0 = rows[0].t;
    const t1 = rows[rows.length - 1].t;
    let pv: Prev | null = null;
    let pi: Prev | null = null;
    let ilCount = opts.mode === "history" ? rows.length : 0;
    let perf = 0;
    const live = opts.mode === "live";
    const startNow = t0 + (live ? 1000 : 0);
    const endNow = t1 + (live ? 20_000 : 0);
    for (let now = startNow; now <= endNow; now += step, perf += step) {
      let tv = now;
      let ti = now;
      let horizon = Infinity;
      if (live) {
        while (fed < samples.length && arrival(samples[fed].t) <= now) {
          view.add(meta, { ...samples[fed] });
          est.observe(now - samples[fed].t);
          fed++;
        }
        while (ilCount < rows.length && arrival(rows[ilCount].t) <= now) {
          ilCount++;
        }
        tv = now - est.delay(perf);
        horizon = est.horizon(now);
        ti = now - (opts.illisisDelay ?? 2000);
        if (perf % 60_000 === 0) {
          view.trim(tv);
        }
      }
      // v3
      if (tv >= t0 && tv <= t1) {
        const span = stV.at(tv);
        if (span >= 0 && pv && pv.span === span) {
          repeat(v3, pv);
        } else {
          const m = view.frame(tv, horizon, { bodies: true })[0];
          if (m) {
            const xs: number[] = [];
            const d: number[] = [];
            for (const c of m.cars) {
              xs.push(c.x, c.y);
              d.push(mask.distance(c.x, c.y));
            }
            pv = account(v3, m.x, m.y, xs, d, pv, excl.at(tv) >= 0, refSpeed(rows, tv), step, span, validAt(rows, rawD, tv));
          } else {
            pv = null;
          }
        }
      }
      // illisis
      if (ti >= t0 && ti <= t1) {
        const span = stI.at(ti);
        if (span >= 0 && pi && pi.span === span) {
          repeat(il, pi);
        } else {
          const s = illisisAt(rows, ilCount, ti, j.n);
          if (s) {
            const xs: number[] = [];
            const d: number[] = [];
            for (const c of s.cars) {
              xs.push(c.x, c.y);
              d.push(mask.distance(c.x, c.y));
            }
            pi = account(il, s.x, s.y, xs, d, pi, exclI.at(ti) >= 0, refSpeed(rows, ti), step, span, validAt(rows, rawD, ti));
          } else {
            pi = null;
          }
        }
      }
    }
  }
  return { v3, illisis: il, rawHead };
}

function repeat(st: RenderStats, p: Prev): void {
  st.frames++;
  for (const dist of p.d) {
    st.cars.add(dist);
    if (dist > 2) {
      st.over2++;
    }
    if (p.valid) {
      st.validCars.add(dist);
      if (dist > 2) {
        st.validOver2++;
      }
    }
  }
}

/** Whether the raw rows around t are both within 1 tile of a rail pixel. */
function validAt(rows: RawRow[], rawD: Float64Array, t: number): boolean {
  const i = findRow(rows, rows.length, t);
  return i >= 0 && i + 1 < rows.length && rawD[i] <= 1 && rawD[i + 1] <= 1;
}

function account(
  st: RenderStats, x: number, y: number, xs: number[], d: number[],
  prev: Prev | null, excluded: boolean, v: number, step: number, span: number, valid: boolean,
): Prev {
  st.frames++;
  const moving = prev !== null && (prev.x !== x || prev.y !== y);
  if (moving) {
    st.movingFrames++;
  }
  if (excluded) {
    st.handoffFrames++;
  }
  for (const dist of d) {
    st.cars.add(dist);
    if (moving) {
      st.movingCars.add(dist);
    }
    if (dist > 2 && !excluded) {
      st.over2++;
      if (moving) {
        st.movingOver2++;
      }
    }
    if (valid) {
      st.validCars.add(dist);
      if (dist > 2 && !excluded) {
        st.validOver2++;
      }
    }
  }
  if (prev && !excluded) {
    const lim = v * (step / 1000) + 2;
    if (Math.hypot(x - prev.x, y - prev.y) > lim) {
      st.headJumps++;
    }
    for (let i = 0; i + 1 < Math.min(xs.length, prev.cars.length); i += 2) {
      if (Math.hypot(xs[i] - prev.cars[i], xs[i + 1] - prev.cars[i + 1]) > lim) {
        st.carJumps++;
        break;
      }
    }
  }
  return { x, y, cars: xs, d, span, valid };
}
