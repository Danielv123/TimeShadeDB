// The train metrics of PLAN.md section 5:
// - leave-one-out (LOO): hide a sample, let the renderer draw the head at its time from
//   the samples around it, measure the distance to the hidden sample;
// - rail distance of every drawn carriage centre, sampled at `hz` over all train time,
//   against the share of raw samples that are off the rail;
// - frame-to-frame jumps of the head larger than v*dt + 2 tiles.
// Handoff windows (a gone followed by the same journey within 10 s) +-1 s are excluded
// from the "outside handoff" counts.

import type { RailMask } from "./illisis_data.ts";
import type { Journey, TrainRenderer, TrainSample } from "./types.ts";

export interface EvalOptions {
  /** Render sampling rate (frames per second of data time). */
  hz: number;
  /**
   * How far ahead of the drawn time samples are known. History playback knows every
   * sample within the 60 s the illisis viewer looks ahead (61 s); a live page drawn D ms
   * behind with arrival lag L knows D - L ms ahead.
   */
  lookaheadMs: number;
  /** Samples on each side fed for a LOO reconstruction. */
  looContext: number;
  /** Only triples whose outer samples are at most this far apart (ms). */
  looMaxGapMs: number;
  /** Rail distance threshold for "off the rail" (tiles). */
  offTiles: number;
  /** Margin around handoffs excluded from the outside-handoff counts (ms). */
  handoffMarginMs: number;
  /** Called for every jump outside a handoff window (diagnostics, --jumps-out). */
  onJump?: (j: Journey, info: JumpInfo) => void;
}

/** A frame-to-frame jump of the head larger than v*dt + 2 tiles. */
export interface JumpInfo {
  t: number;
  from: { x: number; y: number; t: number };
  to: { x: number; y: number };
  disp: number;
  v: number;
  /** Index of the last sample at or before t. */
  at: number;
}

export const defaultOptions: EvalOptions = {
  hz: 10,
  lookaheadMs: 61_000,
  looContext: 3,
  looMaxGapMs: 3000,
  offTiles: 1,
  handoffMarginMs: 1000,
};

/** A histogram of distances with fixed bins, for percentiles over millions of values. */
export class Hist {
  bins: Float64Array;
  n = 0;
  over = 0;
  width: number;
  max: number;
  constructor(width: number, max: number) {
    this.width = width;
    this.max = max;
    this.bins = new Float64Array(Math.ceil(max / width) + 1);
  }
  zeros = 0;
  add(v: number, count = 1): void {
    this.n += count;
    if (v === 0) {
      this.zeros += count;
    }
    if (v >= this.max) {
      this.over += count;
      this.bins[this.bins.length - 1] += count;
      return;
    }
    this.bins[Math.floor(v / this.width)] += count;
  }
  /** Upper edge of the bin holding the nearest-rank percentile p (0..1). */
  quantile(p: number): number {
    if (this.n === 0) {
      return NaN;
    }
    const rank = Math.max(1, Math.ceil(p * this.n));
    if (rank <= this.zeros) {
      return 0;
    }
    let c = 0;
    for (let i = 0; i < this.bins.length; i++) {
      c += this.bins[i];
      if (c >= rank) {
        return Math.min(this.max, (i + 1) * this.width);
      }
    }
    return this.max;
  }
}

function quantile(sorted: Float64Array | number[], p: number): number {
  if (sorted.length === 0) {
    return NaN;
  }
  const rank = Math.max(1, Math.ceil(p * sorted.length)) - 1;
  return sorted[Math.min(rank, sorted.length - 1)];
}

export interface LooResult {
  all: number[];
  curves: number[];
}

/**
 * Leave-one-out over every triple (a, b, c) of consecutive samples without a gone
 * between them, where a is moving and c.t - a.t <= looMaxGapMs.
 */
export function leaveOneOut(r: TrainRenderer, j: Journey, o: EvalOptions, out: LooResult): void {
  const s = j.samples;
  for (let i = 1; i + 1 < s.length; i++) {
    const a = s[i - 1];
    const b = s[i];
    const c = s[i + 1];
    if (a.gone || b.gone || c.gone || b.t <= a.t || c.t <= b.t || c.t - a.t > o.looMaxGapMs) {
      continue;
    }
    if (a.vx === 0 && a.vy === 0 && (a.x === c.x && a.y === c.y)) {
      continue; // parked: nothing to reconstruct
    }
    const sess = r.session(j);
    // Context: up to looContext samples on each side, not across a gone.
    let lo = i - 1;
    while (lo - 1 >= 0 && i - (lo - 1) <= o.looContext && !s[lo - 1].gone) {
      lo--;
    }
    let hi = i + 1;
    while (hi + 1 < s.length && hi + 1 - i <= o.looContext && !s[hi + 1].gone) {
      hi++;
    }
    for (let k = lo; k <= hi; k++) {
      if (k !== i) {
        sess.feed(s[k]);
      }
    }
    const p = sess.pose(b.t);
    if (!p) {
      continue;
    }
    const e = Math.hypot(p.x - b.x, p.y - b.y);
    out.all.push(e);
    const va = Math.hypot(a.vx, a.vy);
    const vc = Math.hypot(c.vx, c.vy);
    let curve = false;
    if (va > 0 && vc > 0) {
      curve = Math.abs(a.vx * c.vy - a.vy * c.vx) / (va * vc) > 0.05; // heading change > ~3 degrees
    } else {
      // No velocity (v3 data): use the chord directions a->b and b->c.
      const ux = b.x - a.x, uy = b.y - a.y, wx = c.x - b.x, wy = c.y - b.y;
      const lu = Math.hypot(ux, uy), lw = Math.hypot(wx, wy);
      curve = lu > 0 && lw > 0 && Math.abs(ux * wy - uy * wx) / (lu * lw) > 0.05;
    }
    if (curve) {
      out.curves.push(e);
    }
  }
}

export interface RenderStats {
  frames: number;
  carFrames: number;
  carOff: number;
  carOver2: number;
  carOver2OutsideHandoff: number;
  dist: Hist;
  jumps: number;
  jumpsOutsideHandoff: number;
  rawSamples: number;
  rawOff: number;
  maxJump: number;
  /** The same counts restricted to moving trains (head moved > 0.05 tiles since the last frame; raw samples faster than 0.5 tiles/s). */
  movingCarFrames: number;
  movingCarOff: number;
  movingDist: Hist;
  rawMoving: number;
  rawMovingOff: number;
  /** Head-only rail distance per frame, comparable across renderers that draw different numbers of carriages. */
  headDist: Hist;
  headOff: number;
  headOver2OutsideHandoff: number;
}

export function newRenderStats(): RenderStats {
  return {
    frames: 0, carFrames: 0, carOff: 0, carOver2: 0, carOver2OutsideHandoff: 0, dist: new Hist(0.01, 32),
    jumps: 0, jumpsOutsideHandoff: 0, rawSamples: 0, rawOff: 0, maxJump: 0,
    movingCarFrames: 0, movingCarOff: 0, movingDist: new Hist(0.01, 32), rawMoving: 0, rawMovingOff: 0,
    headDist: new Hist(0.01, 32), headOff: 0, headOver2OutsideHandoff: 0,
  };
}

function inHandoff(j: Journey, t: number, margin: number): boolean {
  for (const [g, n] of j.handoffs) {
    if (t >= g - margin && t <= n + margin) {
      return true;
    }
  }
  return false;
}

/**
 * Train speed bound for data without velocity: a vanilla locomotive tops out at 72 tiles/s
 * (259.2 km/h); faster implied chord speeds are teleports, not motion.
 */
export const MAX_TRAIN_SPEED = 120;

/**
 * Speed (tiles/s) of a sample, from its velocity or, without one, from the chords to its
 * neighbours, capped at MAX_TRAIN_SPEED.
 */
function speedAt(s: TrainSample[], i: number): number {
  const v = Math.hypot(s[i].vx, s[i].vy);
  if (v > 0) {
    return v;
  }
  let best = 0;
  for (const k of [i - 1, i + 1]) {
    if (k >= 0 && k < s.length && !s[k].gone && !s[i].gone && s[k].t !== s[i].t) {
      best = Math.max(best, Math.hypot(s[k].x - s[i].x, s[k].y - s[i].y) / (Math.abs(s[k].t - s[i].t) / 1000));
    }
  }
  return Math.min(best, MAX_TRAIN_SPEED);
}

/** Renders a journey at o.hz and accumulates rail distances and jumps into st. */
export function renderJourney(r: TrainRenderer, j: Journey, mask: RailMask, o: EvalOptions, st: RenderStats): void {
  const s = j.samples;
  for (let i = 0; i < s.length; i++) {
    const x = s[i];
    if (!x.gone) {
      st.rawSamples++;
      const off = mask.distance(x.x, x.y, 4) > o.offTiles;
      if (off) {
        st.rawOff++;
      }
      if (speedAt(s, i) > 0.5) {
        st.rawMoving++;
        if (off) {
          st.rawMovingOff++;
        }
      }
    }
  }
  const first = s.find((x) => !x.gone);
  if (!first) {
    return;
  }
  const last = s[s.length - 1];
  const step = 1000 / o.hz;
  const sess = r.session(j);
  let fed = 0;
  let prev: { x: number; y: number; t: number } | undefined;
  let prevKey = "";
  let prevDists: number[] = [];
  let prevHead = 0;
  // Index of the last sample at or before the frame, for the speed bound.
  let at = 0;
  for (let t = first.t; t <= last.t; t += step) {
    while (fed < s.length && s[fed].t <= t + o.lookaheadMs) {
      sess.feed(s[fed]);
      fed++;
    }
    while (at + 1 < s.length && s[at + 1].t <= t) {
      at++;
    }
    const p = sess.pose(t);
    if (!p) {
      prev = undefined;
      continue;
    }
    st.frames++;
    const handoff = inHandoff(j, t, o.handoffMarginMs);
    // Parked trains repeat the same pose: reuse the distances.
    const key = p.cars.length + ":" + p.x + "," + p.y + ":" + (p.cars.length ? p.cars[p.cars.length - 1].x + "," + p.cars[p.cars.length - 1].y : "");
    let dists: number[];
    let head: number;
    if (key === prevKey) {
      dists = prevDists;
      head = prevHead;
    } else {
      dists = p.cars.map((c) => mask.distance(c.x, c.y));
      head = mask.distance(p.x, p.y);
    }
    st.headDist.add(head);
    if (head > o.offTiles) {
      st.headOff++;
    }
    if (head > 2 && !handoff) {
      st.headOver2OutsideHandoff++;
    }
    const movingFrame = prev !== undefined && Math.hypot(p.x - prev.x, p.y - prev.y) > 0.05;
    for (const d of dists) {
      if (movingFrame) {
        st.movingCarFrames++;
        st.movingDist.add(d);
        if (d > o.offTiles) {
          st.movingCarOff++;
        }
      }
      st.carFrames++;
      st.dist.add(d);
      if (d > o.offTiles) {
        st.carOff++;
      }
      if (d > 2) {
        st.carOver2++;
        if (!handoff) {
          st.carOver2OutsideHandoff++;
        }
      }
    }
    prevKey = key;
    prevDists = dists;
    prevHead = head;
    if (prev) {
      const dt = (t - prev.t) / 1000;
      const disp = Math.hypot(p.x - prev.x, p.y - prev.y);
      const lo = Math.max(0, at - 1);
      const hi = Math.min(s.length - 1, at + 1);
      let v = 0;
      for (let k = lo; k <= hi; k++) {
        if (!s[k].gone) {
          v = Math.max(v, speedAt(s, k));
        }
      }
      st.maxJump = Math.max(st.maxJump, disp - v * dt);
      if (disp > v * dt + 2) {
        st.jumps++;
        if (!handoff) {
          st.jumpsOutsideHandoff++;
          o.onJump?.(j, { t, from: prev, to: { x: p.x, y: p.y }, disp, v, at });
        }
      }
    }
    prev = { x: p.x, y: p.y, t };
  }
}

/** Summary metrics (keys of bench/targets.json) from the accumulated results. */
export function summarize(loo: LooResult, st: RenderStats): Record<string, number> {
  const all = Float64Array.from(loo.all).sort();
  const curves = Float64Array.from(loo.curves).sort();
  const m: Record<string, number> = {
    "trains.loo_n": all.length,
    "trains.loo_p50": quantile(all, 0.5),
    "trains.loo_p90": quantile(all, 0.9),
    "trains.loo_p99": quantile(all, 0.99),
    "trains.loo_curves_n": curves.length,
    "trains.loo_curves_p50": quantile(curves, 0.5),
    "trains.loo_curves_p90": quantile(curves, 0.9),
    "trains.loo_curves_p99": quantile(curves, 0.99),
    "trains.frames": st.frames,
    "trains.car_frames": st.carFrames,
    "trains.raw_samples": st.rawSamples,
    "trains.raw_offrail_share": st.rawSamples ? st.rawOff / st.rawSamples : NaN,
    "trains.offrail_share": st.carFrames ? st.carOff / st.carFrames : NaN,
    "trains.offrail_p50_tiles": st.dist.quantile(0.5),
    "trains.offrail_p99_tiles": st.dist.quantile(0.99),
    "trains.offrail_over2": st.carOver2,
    "trains.offrail_over2_outside_handoff": st.carOver2OutsideHandoff,
    "trains.jumps": st.jumps,
    "trains.jumps_outside_handoff": st.jumpsOutsideHandoff,
    "trains.max_excess_jump_tiles": st.maxJump,
  };
  m["trains.offrail_excess_pp"] = (m["trains.offrail_share"] - m["trains.raw_offrail_share"]) * 100;
  m["trains.moving_car_frames"] = st.movingCarFrames;
  m["trains.moving_offrail_share"] = st.movingCarFrames ? st.movingCarOff / st.movingCarFrames : NaN;
  m["trains.moving_offrail_p99_tiles"] = st.movingDist.quantile(0.99);
  m["trains.raw_moving_offrail_share"] = st.rawMoving ? st.rawMovingOff / st.rawMoving : NaN;
  m["trains.moving_offrail_excess_pp"] = (m["trains.moving_offrail_share"] - m["trains.raw_moving_offrail_share"]) * 100;
  m["trains.head_offrail_share"] = st.frames ? st.headOff / st.frames : NaN;
  m["trains.head_offrail_p99_tiles"] = st.headDist.quantile(0.99);
  m["trains.head_offrail_over2_outside_handoff"] = st.headOver2OutsideHandoff;
  return m;
}
