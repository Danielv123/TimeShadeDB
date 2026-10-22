// Mover tracks and their motion: samples per journey in time order, the
// head position at time t (cubic Hermite between samples, bounded extrapolation past the
// newest one, straight bridges across instance handoffs), and a train's body built from
// the head's spline history with carriages placed by arc length.
//
// Train samples of the rail path model (F_RAIL, docs/RAIL-GEOMETRY.md 8) carry the
// train's speed and the rail pieces since the sample before: their journey's rail path is
// known exactly (railpath.ts), so between two of them the leading joint runs along it by
// the timing model, and every carriage sits on it at its joint distances. Other samples
// (imported data, older captures, the stretch around a missed piece) use the rest of this
// file.
//
// Times are map-clock ms; positions are world tiles; velocities are tiles per second.
// MVW3 stores no velocity (FORMAT.md 7.4) except a rail sample's speed, so velocities are
// derived here from the neighbouring rows and, for direction, from the orientation.
//
// DOM-free; imported by the mover layer, the tests and the train evaluator.

import type { Carriage, MoverKind, MoverMeta } from "../api/types";
import { Polyline, hermite, orientationDelta, orientationVector } from "./hermite";
import { CONNECTION, JOINT, type Piece, RailChain, pieceLength, timingU } from "./railpath";
import { Segment } from "./segment";

// Row flags (FORMAT.md 7.4). Local copies so this file needs no runtime import of types.ts.
export const F_STOP = 1 << 0;
export const F_GONE = 1 << 1;
export const F_HANDOFF = 1 << 2;
export const F_END_SWITCH = 1 << 3;
export const F_NO_TICK = 1 << 4;
export const F_NO_HEADING = 1 << 5;
/** A rail path sample: o is the leading carriage's own orientation, v its speed, rp its new rail pieces. */
export const F_RAIL = 1 << 7;

/** A gone followed by a sample of the same journey within this is a handoff. */
export const HANDOFF_MS = 10_000;
/** Consecutive samples further apart than this are not interpolated (a data gap). */
export const MAX_SEGMENT_MS = 60_000;
/** Extrapolation past the data horizon (the newest time the feed is known complete). */
export const EXTRAPOLATE_MS = 1_000;
/**
 * Extrapolation inside the data horizon: the capture sends a sample when the mover leaves
 * its predicted line or when the heartbeat (600 ticks) expires, so silence up to a known
 * time means "still on the line" for at most about this long.
 */
export const HEARTBEAT_MS = 11_000;
/** Curvatures above 1 / this (tiles) are not believed for extrapolation (tighter than any rail curve). */
export const MIN_RADIUS = 10;
/** Joint distance / 2: a carriage's bogies sit this far from its centre (vanilla 4 / 2). */
export const BOGIE = 2;
/** Default distance between carriage centres: joint 4 + connection 3. */
export const PITCH = 7;
/** Body path sampling step in tiles. */
export const PATH_STEP = 0.5;

/** Velocity estimation choices; the evaluator compares them, the viewer uses the default. */
export interface MotionConfig {
  /**
   * Direction of a sample's velocity. "blend" (default): the circumcircle tangent through
   * the neighbouring samples averaged with the orientation corrected for its half-pitch lag;
   * "orient": the corrected orientation only; "fd": finite differences of positions only.
   * Speed always comes from the neighbouring rows (MVW3 stores no velocity).
   */
  tangent: "blend" | "orient" | "fd";
  /** Segment geometry: "rail" (line+arc where it fits, else Hermite) or "hermite". */
  geometry: "rail" | "hermite";
  /** Handoff bridges: "line" is a straight blend in time; "hermite" uses both sides' velocities. */
  bridge: "line" | "hermite";
  /**
   * Largest turn (radians) an extrapolation past the data horizon follows the newest
   * curvature for, then straight; 0 extrapolates straight. With the turn inside the horizon
   * too, 15 degrees measured best on the test data (live snaps: 22 straight, 15 at 15
   * degrees, 16 at 22.5, 18 at 30: past a curve's exit a long turn runs off the other side).
   */
  extrapTurn: number;
  /**
   * Whether the extrapolation inside the data horizon follows the curve too (default false:
   * only the run past the horizon does). The capture emits on about 2 degrees of turn, so
   * silence inside the horizon means a straight run. Turning there saved 7 of 22 live
   * snaps on the test data, but nearly doubled the carriages off the rails while moving at
   * lookahead 0 in the train evaluator (5.2% -> 9.9% on 10 journeys): trains turned on past a
   * curve's exit.
   */
  extrapTurnInside: boolean;
}

export const motionConfig: MotionConfig = { tangent: "blend", geometry: "rail", bridge: "line", extrapTurn: Math.PI / 12, extrapTurnInside: false };

/** One stored row of a journey. */
export interface Sample {
  t: number;
  tick: number | null;
  x: number;
  y: number;
  /** Orientation in turns, NaN when unknown. */
  o: number;
  flags: number;
  car: Carriage[] | null;
  meta: MoverMeta;
  /** With F_RAIL: speed (tiles a tick), the rail pieces entered since the sample before, and whether a new rail path begins. */
  v: number;
  rp: Piece[] | null;
  ru: boolean;
  /** Derived velocity (tiles/s), valid while `vv` equals the track version. */
  vx: number;
  vy: number;
  vv: number;
  /** The segment to the next sample, valid while `sv` equals the track version. */
  seg: Segment | null;
  sv: number;
}

export function makeSample(
  meta: MoverMeta, t: number, x: number, y: number, o: number, flags = 0, car: Carriage[] | null = null, tick: number | null = null,
  v = NaN, rp: Piece[] | null = null, ru = false,
): Sample {
  return { t, tick, x, y, o, flags, car, meta, v, rp, ru, vx: 0, vy: 0, vv: -1, seg: null, sv: -1 };
}

/** A journey's samples, sorted by t, unique per t. */
export class Track {
  readonly id: string;
  meta: MoverMeta;
  s: Sample[] = [];
  /** Bumped on every change; invalidates derived velocities. */
  version = 0;
  /** Number of samples with carriage geometry. */
  withCars = 0;
  /** The rail runs (railRuns), valid while their version equals the track's. */
  rr: RailRuns | null = null;

  constructor(id: string, meta: MoverMeta) {
    this.id = id;
    this.meta = meta;
  }

  get kind(): MoverKind {
    return this.meta.k;
  }

  /** Inserts in time order; a sample at an existing time replaces it (stores keep the last arrival). */
  insert(x: Sample): void {
    const s = this.s;
    let i = s.length;
    if (i > 0 && s[i - 1].t >= x.t) {
      i = this.find(x.t) + 1;
    }
    if (x.car) {
      this.withCars++;
    }
    if (i > 0 && s[i - 1].t === x.t) {
      if (s[i - 1].car) {
        this.withCars--;
      }
      s[i - 1] = x;
    } else if (i === s.length) {
      s.push(x);
    } else {
      s.splice(i, 0, x);
    }
    if (x.t >= s[s.length - 1].t) {
      this.meta = x.meta;
    }
    this.version++;
  }

  /** Index of the last sample with t' <= t, or -1. */
  find(t: number): number {
    const s = this.s;
    let lo = 0;
    let hi = s.length - 1;
    if (hi < 0 || s[0].t > t) {
      return -1;
    }
    if (s[hi].t <= t) {
      return hi;
    }
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (s[mid].t <= t) {
        lo = mid;
      } else {
        hi = mid - 1;
      }
    }
    return lo;
  }

  /**
   * Drops samples older than `before`, keeping the samples at or before it that cover
   * `reach` tiles of path behind it (a train's body history; parked heartbeats do not
   * count) and the newest sample with carriage geometry.
   */
  trim(before: number, reach: number): void {
    const s = this.s;
    const i = this.find(before);
    if (i < 0) {
      return;
    }
    let j = i;
    let walked = 0;
    while (j > 0 && walked < reach) {
      const p = s[j - 1];
      if (isGone(p)) {
        // A handoff: the body runs on across the bridge to the sample before the gone.
        if (j >= 2 && !isGone(s[j - 2]) && s[j].t - p.t <= HANDOFF_MS) {
          walked += Math.hypot(s[j].x - s[j - 2].x, s[j].y - s[j - 2].y);
          j -= 2;
          continue;
        }
        break;
      }
      walked += Math.hypot(s[j].x - p.x, s[j].y - p.y);
      j--;
    }
    // One more sample, so the oldest kept segment's velocities still see their neighbours
    // (trimming must not reshape the path the body lies on).
    if (j > 0 && !isGone(s[j - 1])) {
      j--;
    }
    let carIdx = -1;
    for (let k = j - 1; k >= 0 && this.withCars > 0; k--) {
      if (s[k].car) {
        carIdx = k;
        break;
      }
    }
    let newer = false;
    for (let k = j; k < s.length && !newer && this.withCars > 0; k++) {
      newer = s[k].car !== null;
    }
    // (kept apart from the rail path: the samples between it and the rest are gone, so it does not
    // begin the rest's run)
    const kept: Sample[] = carIdx >= 0 && !newer ? [{ ...s[carIdx], flags: s[carIdx].flags & ~F_RAIL, seg: null, sv: -1, vv: -1 }] : [];
    for (let k = 0; k < j; k++) {
      if (s[k].car && s[k] !== kept[0]) {
        this.withCars--;
      }
    }
    // Parked heartbeats: of a run of samples at one position before `before`, keep the first and last.
    const rest: Sample[] = [];
    for (let k = j; k < s.length; k++) {
      const x = s[k];
      const p = rest[rest.length - 1];
      const q = rest[rest.length - 2];
      if (
        x.t < before && p && q && !x.car && !p.car && x.flags === p.flags && p.flags === q.flags &&
        !isGone(x) && x.x === p.x && x.y === p.y && p.x === q.x && p.y === q.y
      ) {
        rest[rest.length - 1] = x;
      } else {
        rest.push(x);
      }
    }
    // The first kept rail sample begins a rail path of its own: the pieces of its run's chain
    // under the body behind it (reach), so the run without the dropped samples still holds the
    // train (railRuns).
    const f = rest[0];
    if (j > 0 && f && f === s[j] && (f.flags & F_RAIL) !== 0) {
      const rr = railRuns(this);
      if (rr.run[j] >= 0 && !Number.isNaN(rr.u[j])) {
        const pieces = rr.chains[rr.run[j]].piecesBetween(rr.u[j] - reach - 8, rr.u[j]);
        rest[0] = { ...f, rp: pieces.length > 0 ? pieces : null, ru: true, seg: null, sv: -1, vv: -1 };
      }
    }
    const next = kept.concat(rest);
    if (next.length !== s.length) {
      this.s = next;
      this.version++;
    }
  }
}

function isGone(x: Sample): boolean {
  return (x.flags & F_GONE) !== 0;
}

/** Distances between carriage centres for a train: measured from carriage geometry when known. */
export function pitches(tr: Track, upTo: number, out: number[]): number[] {
  const n = Math.max(1, tr.s[Math.max(0, upTo)]?.meta.n ?? tr.meta.n ?? 1);
  out.length = 0;
  let car: Carriage[] | null = null;
  for (let j = Math.min(upTo, tr.s.length - 1); j >= 0 && tr.withCars > 0; j--) {
    if (tr.s[j].car) {
      car = tr.s[j].car;
      break;
    }
  }
  for (let i = 0; i + 1 < n; i++) {
    let p = PITCH;
    if (car && i + 1 < car.length) {
      const d = Math.hypot(car[i + 1].x - car[i].x, car[i + 1].y - car[i].y);
      if (d >= 4 && d <= 14) {
        p = d;
      }
    }
    out.push(p);
  }
  return out;
}

// ---------------------------------------------------------------------------------------
// Rail path samples (F_RAIL)

/**
 * A track's rail runs: per sample the run index (-1: none) and the leading joint's rail
 * distance on its chain (NaN: unknown); `ub` is a sample's rail distance on the next
 * sample's chain when that one begins a new run whose pieces reach back to it (NaN else).
 */
export interface RailRuns {
  version: number;
  run: Int32Array;
  u: Float64Array;
  ub: Float64Array;
  chains: RailChain[];
  /** Rail distances between carriages from the `car` of a sample (by index), cached (railGaps). */
  pitches: Map<number, number[] | null>;
}

/** A sample's leading joint lies this far at most from its run's chain, or its rail distance is unknown. */
const RAIL_SNAP = 0.35;

function isRail(x: Sample): boolean {
  return (x.flags & (F_RAIL | F_GONE)) === F_RAIL && !Number.isNaN(x.v) && !Number.isNaN(x.o) && x.tick !== null;
}

/**
 * The rail runs of a train's track: consecutive rail samples whose pieces continue one rail
 * path. A run begins at a sample with `ru`, after a gone, a sample that is not a rail sample
 * or an end switch, and where a piece does not continue the path (a missed piece). Each
 * sample's leading joint (its centre plus half the joint distance along its orientation)
 * is projected onto the run's chain.
 */
export function railRuns(tr: Track): RailRuns {
  if (tr.rr && tr.rr.version === tr.version) {
    return tr.rr;
  }
  const s = tr.s;
  const n = s.length;
  const run = new Int32Array(n).fill(-1);
  const u = new Float64Array(n).fill(NaN);
  const ub = new Float64Array(n).fill(NaN);
  const chains: RailChain[] = [];
  let chain: RailChain | null = null;
  let members: number[] = [];
  // where the run's first sample can lie: on the chain it started with (a later pass of the
  // path near it does not count)
  let firstTo = Infinity;
  // ...and no earlier than the last piece it began with: a loop's earlier pass is not it either
  let firstFrom = -Infinity;
  // the sample before the run, when the run's pieces may reach back to it (see below)
  let back = -1;
  const close = (): void => {
    if (!chain) {
      return;
    }
    let from = firstFrom;
    // each sample no further ahead than the faster end speed (plus half) allows since the last
    // placed one, or than the run's first sample may lie
    let refU = firstTo;
    let ref = s[members[0]];
    for (const k of members) {
      const x = s[k];
      const to = refU + 1.5 * Math.max(ref.v, x.v) * (x.tick! - ref.tick!) + 4;
      // where the speeds put it (the mean of the two over the ticks between); the run's first
      // sample nearest the start of its range: its pieces are the run's first, and a later pass
      // of the same rails (a loop) comes from later samples' pieces
      const expect = k === members[0] ? (Number.isFinite(from) ? from : NaN)
        : Number.isFinite(refU) ? refU + ((ref.v + x.v) / 2) * (x.tick! - ref.tick!) : NaN;
      const [ux, uy] = orientationVector(x.o);
      const q = chain.project(x.x + (JOINT / 2) * ux, x.y + (JOINT / 2) * uy, from, to, expect);
      if (chain.lastDistance <= RAIL_SNAP) {
        u[k] = q;
        from = q;
        refU = q;
        ref = x;
      }
    }
    const first = members[0];
    if (back >= 0 && !Number.isNaN(u[first])) {
      // A new rail path (a first sight after a resync, a missed piece) whose pieces under the
      // train, or straight line without any, reach back to the sample before: that segment
      // runs on the new chain, behind the run's first sample.
      const p = s[back];
      const [px, py] = orientationVector(p.o);
      const q = chain.project(p.x + (JOINT / 2) * px, p.y + (JOINT / 2) * py, -Infinity, u[first]);
      if (chain.lastDistance <= RAIL_SNAP && q <= u[first] + 1e-6) {
        ub[back] = q;
      }
    }
  };
  for (let k = 0; k < n; k++) {
    const x = s[k];
    if (tr.kind !== "t" || !isRail(x)) {
      close();
      chain = null;
      continue;
    }
    const prev = k > 0 ? s[k - 1] : null;
    let start = chain === null || x.ru || !prev || !isRail(prev) || (x.flags & F_END_SWITCH) !== 0;
    if (!start && x.rp) {
      for (const p of x.rp) {
        if (!chain!.append(p)) {
          start = true;
          break;
        }
      }
    }
    if (start) {
      close();
      const [ux, uy] = orientationVector(x.o);
      chain = new RailChain(x.x + (JOINT / 2) * ux, x.y + (JOINT / 2) * uy, ux, uy, x.rp ?? []);
      // with `ru` the pieces were under the train: its leading joint is at most a train length
      // past the last; else they were entered since the sample before (a stop sample's also lie
      // under the train): at most as far past them as it went since, or a train length (a piece
      // that did not continue the run), or a heartbeat at its speed without that sample (the
      // first in view: a keyframe's, or the live payload's)
      const len = (JOINT + CONNECTION) * Math.max(1, x.meta.n ?? 1);
      const since = prev && isRail(prev) && x.tick! > prev.tick!
        ? Math.max(len, 1.5 * Math.max(prev.v, x.v) * (x.tick! - prev.tick!) + 4) : len + x.v * 600;
      // (without pieces the sample is the anchor: before the first piece appended later, u <= 0)
      firstTo = !x.rp || x.rp.length === 0 ? 0 : x.ru ? chain.end + len : chain.end + since;
      firstFrom = !x.rp || x.rp.length === 0 ? -Infinity : chain.end - pieceLength(x.rp[x.rp.length - 1].kind);
      chains.push(chain);
      members = [];
      back = prev && isRail(prev) && (x.flags & F_END_SWITCH) === 0 ? k - 1 : -1;
    }
    members.push(k);
    run[k] = chains.length - 1;
  }
  close();
  tr.rr = { version: tr.version, run, u, ub, chains, pitches: new Map() };
  return tr.rr;
}

/** Whether s[k] to s[k+1] is a rail segment: both on one rail run with known rail distances, no end switch between. */
export function railLinked(tr: Track, k: number): boolean {
  const a = tr.s[k];
  const b = tr.s[k + 1];
  if (!a || !b || !isRail(a) || !isRail(b) || (b.flags & F_END_SWITCH) !== 0) {
    return false;
  }
  const rr = railRuns(tr);
  if (rr.run[k + 1] < 0 || Number.isNaN(railFrom(rr, k)) || Number.isNaN(rr.u[k + 1])) {
    return false;
  }
  // A parked train is not sampled until it moves: a stop sample links to the start however long it stood.
  return b.t - a.t <= MAX_SEGMENT_MS || (a.flags & F_STOP) !== 0;
}

/** The rail distance of sample k on the chain of sample k+1's run (NaN: not on it). */
function railFrom(rr: RailRuns, k: number): number {
  return rr.run[k] === rr.run[k + 1] ? rr.u[k] : rr.ub[k];
}

/** Game ticks a second around sample k, from neighbouring samples' ticks (60 when unknown). */
function ticksPerSecond(tr: Track, k: number): number {
  const s = tr.s;
  for (const [i, j] of [[k - 1, k], [k, k + 1], [k - 2, k - 1], [k + 1, k + 2]]) {
    const a = s[i];
    const b = s[j];
    if (a && b && a.tick !== null && b.tick !== null && b.tick > a.tick && b.t > a.t && !isGone(a) && !isGone(b)) {
      return ((b.tick - a.tick) * 1000) / (b.t - a.t);
    }
  }
  return 60;
}

const rt = [0, 0];
const rc = [0, 0, 0, 0];

/** The head between rail samples k and k+1 (railLinked) at time t. */
function railInterpolate(tr: Track, k: number, t: number, out: Head): void {
  const a = tr.s[k];
  const b = tr.s[k + 1];
  const rr = railRuns(tr);
  const T = b.tick! - a.tick!;
  const f = b.t > a.t ? Math.min(1, Math.max(0, (t - a.t) / (b.t - a.t))) : 1;
  const ua = railFrom(rr, k);
  timingU(rr.u[k + 1] - ua, T, a.v, b.v, f * T, rt);
  const chain = rr.chains[rr.run[k + 1]];
  const ru = ua + rt[0];
  chain.carriage(ru, rc);
  const tps = b.t > a.t ? (T * 1000) / (b.t - a.t) : 60;
  out.visible = true;
  out.x = rc[0];
  out.y = rc[1];
  out.vx = rc[2] * rt[1] * tps;
  out.vy = rc[3] * rt[1] * tps;
  out.k = k;
  out.to = k + 1;
  out.u = f;
  out.rail = chain;
  out.ru = ru;
  out.moving = rt[1] > 0 || (f > 0 && f < 1 && rr.u[k + 1] !== ua);
  out.mode = "interp";
}

/**
 * The head past rail sample k at time t: on along the rail path at its speed (braking to a
 * stop when it was braking), within the same caps as extrapolate(); held at the sample when
 * it stands. Ahead of the newest piece the path is the straight it ended on.
 */
function railExtrapolate(tr: Track, k: number, t: number, horizon: number, end: number, out: Head): void {
  const a = tr.s[k];
  const rr = railRuns(tr);
  const chain = rr.chains[rr.run[k]];
  const tps = ticksPerSecond(tr, k);
  let cap = Math.min(HEARTBEAT_MS, Math.max(0, horizon - a.t) + EXTRAPOLATE_MS, end - a.t);
  if (end === Infinity && horizon - a.t > HEARTBEAT_MS + EXTRAPOLATE_MS) {
    cap = 0; // the heartbeat is known missed: the data stopped (as extrapolate())
  }
  const v0 = (a.flags & F_STOP) !== 0 ? 0 : a.v * tps;
  let acc = 0;
  const p = k > 0 ? tr.s[k - 1] : null;
  if (p && isRail(p) && p.t < a.t && p.v > a.v) {
    acc = Math.max(-MAX_BRAKING, ((a.v - p.v) * tps * 1000) / (a.t - p.t));
    if (v0 > 0) {
      cap = Math.min(cap, (1000 * v0) / -acc);
    }
  }
  const dt = Math.max(0, Math.min(t - a.t, cap)) / 1000;
  const dist = Math.max(0, v0 * dt + 0.5 * acc * dt * dt);
  const rate = Math.max(0, v0 + acc * dt);
  const ru = rr.u[k] + dist;
  chain.carriage(ru, rc);
  const moving = v0 > 0 && rate > 0 && t - a.t < cap;
  out.visible = true;
  out.x = rc[0];
  out.y = rc[1];
  out.vx = moving ? rc[2] * rate : 0;
  out.vy = moving ? rc[3] * rate : 0;
  out.k = k;
  out.to = -1;
  out.u = 0;
  out.ed = dist;
  out.eth = Math.atan2(rc[3], rc[2]);
  out.ek = 0;
  out.el = 0;
  out.rail = chain;
  out.ru = ru;
  out.mode = moving ? "extrap" : "hold";
  out.moving = moving;
}

/** Whether sample k is a rail sample with a known place on its run's chain. */
function railKnown(tr: Track, k: number): boolean {
  const x = tr.s[k];
  if (!x || !isRail(x)) {
    return false;
  }
  const rr = railRuns(tr);
  return rr.run[k] >= 0 && !Number.isNaN(rr.u[k]);
}

/**
 * The rail distances between neighbouring carriages' front joints for a train drawn at sample
 * k on a rail path, into gaps. From the newest carriage geometry (`car`) of the same run: each
 * carriage's front joint (its centre plus 2 tiles along its axis, towards the leading end)
 * projected onto the run's chain, so a pitch measured on a curve or a ramp is the rail
 * distance and not the chord. Otherwise the centre distances (pitches()), taken as the vanilla
 * joint + connection distance when they lie between 5.5 and 7.6 (a chord on a curve or a ramp
 * is up to 10% short).
 */
export function railGaps(tr: Track, k: number, gaps: number[]): number[] {
  pitches(tr, k, gaps);
  const rr = railRuns(tr);
  const run = rr.run[k];
  let j = k;
  while (j >= 0 && rr.run[j] === run && !tr.s[j].car) {
    j--;
  }
  let measured: number[] | null = null;
  if (run >= 0 && j >= 0 && rr.run[j] === run && !Number.isNaN(rr.u[j])) {
    if (!rr.pitches.has(j)) {
      rr.pitches.set(j, carGaps(tr.s[j], rr.chains[run], rr.u[j]));
    }
    measured = rr.pitches.get(j) ?? null;
  }
  for (let i = 0; i < gaps.length; i++) {
    if (measured && i < measured.length) {
      gaps[i] = measured[i];
    } else if (gaps[i] >= 5.5 && gaps[i] <= 7.6) {
      gaps[i] = JOINT + CONNECTION;
    }
  }
  return gaps;
}

/** carGaps: the rail distances between the front joints of a sample's carriages on chain (u0: the leading joint's), or null. */
function carGaps(x: Sample, chain: RailChain, u0: number): number[] | null {
  const car = x.car!;
  const out: number[] = [];
  let prev = u0;
  for (let i = 1; i < car.length; i++) {
    const c = car[i];
    let [ux, uy] = orientationVector(c.o);
    // the axis pointed towards the carriage ahead
    if (ux * (car[i - 1].x - c.x) + uy * (car[i - 1].y - c.y) < 0) {
      ux = -ux;
      uy = -uy;
    }
    const q = chain.project(c.x + (JOINT / 2) * ux, c.y + (JOINT / 2) * uy, prev - 20, prev - 3);
    if (chain.lastDistance > RAIL_SNAP) {
      return out.length > 0 ? out : null;
    }
    out.push(prev - q);
    prev = q;
  }
  return out;
}

/**
 * Carriages of a head on a rail path (head.rail): carriage i's front joint lies the sum of
 * the gaps before it (railGaps) behind the leading one, its centre midway between its joints
 * and its angle along their chord (RAIL-GEOMETRY.md 3).
 */
export function placeRailCarriages(head: Head, kinds: string, gaps: number[], out: Car[]): Car[] {
  const chain = head.rail!;
  out.length = 0;
  let u = head.ru;
  for (let i = 0; i <= gaps.length; i++) {
    if (i > 0) {
      u -= gaps[i - 1];
    }
    chain.carriage(u, rc);
    const kind = kinds[i] ?? "C";
    out.push({ x: rc[0], y: rc[1], a: Math.atan2(rc[3], rc[2]), loco: kind === "L" || kind === "l", kind });
  }
  return out;
}

/** Trains do not move faster than this (tiles/s); a faster step is a switch of the sampled carriage. */
export const MAX_TRAIN_SPEED = 100;

/**
 * Whether b (= s[k+1]) is an end switch after a (= s[k]): flagged, or (imported data has
 * no flag) the head jumped about the train's length while the orientation flipped or the
 * step was faster than any train.
 */
export function isEndSwitch(tr: Track, k: number): boolean {
  const a = tr.s[k];
  const b = tr.s[k + 1];
  if ((b.flags & F_END_SWITCH) !== 0) {
    return true;
  }
  if (tr.kind !== "t" || isGone(a) || isGone(b) || (b.flags & F_RAIL) !== 0) {
    // a rail sample's capture flags every end switch
    return false;
  }
  const n = b.meta.n ?? 0;
  if (n < 2) {
    return false;
  }
  const d = Math.hypot(b.x - a.x, b.y - a.y);
  if (Math.abs(d - (n - 1) * PITCH) >= 4) {
    return false;
  }
  const flipped = !Number.isNaN(a.o) && !Number.isNaN(b.o) && Math.abs(orientationDelta(a.o, b.o)) >= 0.4;
  if (flipped || d > MAX_TRAIN_SPEED * Math.max(0.001, (b.t - a.t) / 1000)) {
    return true;
  }
  // The new head sits where the body was: behind the old head (a reversal on a curve flips
  // the orientation by less than half a turn).
  if (!Number.isNaN(a.o)) {
    const [ux, uy] = orientationVector(a.o);
    return (b.x - a.x) * ux + (b.y - a.y) * uy < -0.5 * d;
  }
  return false;
}

/** Whether the end switch into s[k+1] reversed the direction of travel (else the new head is ahead). */
function switchReverses(tr: Track, k: number): boolean {
  const a = tr.s[k];
  const b = tr.s[k + 1];
  if (Number.isNaN(a.o)) {
    return true;
  }
  const [ux, uy] = orientationVector(a.o);
  return (b.x - a.x) * ux + (b.y - a.y) * uy < 0;
}

/** Whether s[k] and s[k+1] are a stationary pair (the mover stayed put across a long gap). */
function stationary(tr: Track, k: number): boolean {
  const a = tr.s[k];
  const b = tr.s[k + 1];
  return !!a && !!b && !isGone(a) && !isGone(b) && Math.hypot(b.x - a.x, b.y - a.y) < 2;
}

/** Whether s[k] and s[k+1] form an interpolated segment. */
export function linked(tr: Track, k: number): boolean {
  const a = tr.s[k];
  const b = tr.s[k + 1];
  if (!a || !b || isGone(a) || isGone(b) || b.t - a.t > MAX_SEGMENT_MS) {
    return false;
  }
  if ((a.flags & b.flags & F_RAIL) !== 0 && b.ru && a.meta.n !== b.meta.n) {
    // The train changed (a join or a split; its capture reads it at once): where the rail path
    // does not link the two leading joints, the leading carriage changed. Hold, then jump.
    return false;
  }
  return !isEndSwitch(tr, k);
}

/**
 * For a gone at index g: the index of the sample that resumes the journey within
 * HANDOFF_MS, or -1. `horizon` is the time up to which the data is complete; while it is
 * before the end of the handoff window, an unresolved gone is "pending" (-2).
 */
export function resumeOf(tr: Track, g: number, horizon: number): number {
  const gone = tr.s[g];
  const next = tr.s[g + 1];
  if (next && !isGone(next) && next.t - gone.t <= HANDOFF_MS) {
    return g + 1;
  }
  if (!next && ((gone.flags & F_HANDOFF) !== 0 || horizon < gone.t + HANDOFF_MS)) {
    return -2;
  }
  return -1;
}

/** Unit tangent at p of the circle through a, p, b (the chord direction when collinear), pointing from a to b. */
function circleTangent(a: Sample, p: Sample, b: Sample): [number, number] {
  const ax = a.x - p.x;
  const ay = a.y - p.y;
  const bx = b.x - p.x;
  const by = b.y - p.y;
  const d = 2 * (ax * by - ay * bx);
  const la = ax * ax + ay * ay;
  const lb = bx * bx + by * by;
  let tx: number;
  let ty: number;
  if (Math.abs(d) < 1e-9 * Math.max(1, la + lb) || la === 0 || lb === 0) {
    tx = b.x - a.x;
    ty = b.y - a.y;
  } else {
    // The tangent is perpendicular to the radius: circumcentre relative to p, turned 90 degrees.
    const ox = (by * la - ay * lb) / d;
    const oy = (ax * lb - bx * la) / d;
    tx = -oy;
    ty = ox;
    if (tx * (b.x - a.x) + ty * (b.y - a.y) < 0) {
      tx = -tx;
      ty = -ty;
    }
  }
  const l = Math.hypot(tx, ty) || 1;
  return [tx / l, ty / l];
}

/**
 * The orientation of a train sample corrected to the head's tangent. The captured
 * orientation is the chord from the second carriage's centre to the first, which on a
 * curve is the tangent half a pitch behind the head; turn it by the curvature (the
 * orientation change per tile between the linked neighbours) times half a pitch.
 */
function headOrientation(tr: Track, k: number, hasPrev: boolean, hasNext: boolean): number {
  const s = tr.s;
  const p = s[k];
  if (tr.kind !== "t" || (p.meta.n ?? 0) < 2) {
    return p.o;
  }
  const a = hasPrev ? s[k - 1] : p;
  const b = hasNext ? s[k + 1] : p;
  if (a === b || Number.isNaN(a.o) || Number.isNaN(b.o)) {
    return p.o;
  }
  const dO = orientationDelta(a.o, b.o);
  const ds = (hasPrev ? Math.hypot(p.x - a.x, p.y - a.y) : 0) + (hasNext ? Math.hypot(b.x - p.x, b.y - p.y) : 0);
  return ds > 0.5 && Math.abs(dO) < 0.25 ? p.o + (dO / ds) * (PITCH / 2) : p.o;
}

/**
 * Derived velocity of sample k (tiles/s), cached per track version. Speed: the quadratic
 * through the neighbouring rows (weighted chord speeds). Direction: see MotionConfig.
 * Zero at stops and gones.
 */
export function velocity(tr: Track, k: number): Sample {
  const s = tr.s;
  const p = s[k];
  if (p.vv === tr.version) {
    return p;
  }
  p.vv = tr.version;
  p.vx = 0;
  p.vy = 0;
  if (isGone(p) || (p.flags & F_STOP) !== 0) {
    return p;
  }
  if (isRail(p)) {
    // the captured speed along the leading carriage's own orientation (the rail tangent)
    const [ux, uy] = orientationVector(p.o);
    const sp = p.v * ticksPerSecond(tr, k);
    p.vx = ux * sp;
    p.vy = uy * sp;
    return p;
  }
  const hasPrev = k > 0 && linked(tr, k - 1);
  const hasNext = k + 1 < s.length && linked(tr, k);
  let fx = 0;
  let fy = 0;
  let speed = 0;
  const h0 = hasPrev ? (p.t - s[k - 1].t) / 1000 : 0;
  const h1 = hasNext ? (s[k + 1].t - p.t) / 1000 : 0;
  const d0x = h0 > 0 ? (p.x - s[k - 1].x) / h0 : 0;
  const d0y = h0 > 0 ? (p.y - s[k - 1].y) / h0 : 0;
  const d1x = h1 > 0 ? (s[k + 1].x - p.x) / h1 : 0;
  const d1y = h1 > 0 ? (s[k + 1].y - p.y) / h1 : 0;
  if (h0 > 0 && h1 > 0) {
    fx = (h1 * d0x + h0 * d1x) / (h0 + h1);
    fy = (h1 * d0y + h0 * d1y) / (h0 + h1);
    speed = (h1 * Math.hypot(d0x, d0y) + h0 * Math.hypot(d1x, d1y)) / (h0 + h1);
  } else if (h0 > 0) {
    fx = d0x;
    fy = d0y;
    speed = Math.hypot(d0x, d0y);
  } else if (h1 > 0) {
    fx = d1x;
    fy = d1y;
    speed = Math.hypot(d1x, d1y);
  }
  if (speed <= 0) {
    return p;
  }
  const mode = motionConfig.tangent;
  if (mode === "fd" || Number.isNaN(p.o)) {
    if (mode === "blend" && h0 > 0 && h1 > 0) {
      const [cx, cy] = circleTangent(s[k - 1], p, s[k + 1]);
      p.vx = cx * speed;
      p.vy = cy * speed;
    } else {
      p.vx = fx;
      p.vy = fy;
    }
    return p;
  }
  let [ux, uy] = orientationVector(headOrientation(tr, k, hasPrev, hasNext));
  if (ux * fx + uy * fy < 0) {
    ux = -ux;
    uy = -uy;
  }
  if (mode === "blend" && h0 > 0 && h1 > 0) {
    const [cx, cy] = circleTangent(s[k - 1], p, s[k + 1]);
    const sg = cx * ux + cy * uy < 0 ? -1 : 1;
    ux += sg * cx;
    uy += sg * cy;
    const l = Math.hypot(ux, uy) || 1;
    ux /= l;
    uy /= l;
  }
  p.vx = ux * speed;
  p.vy = uy * speed;
  return p;
}

/** The segment from sample k to k+1 (which must be linked), cached per track version. */
export function segment(tr: Track, k: number): Segment {
  const a = tr.s[k];
  if (a.seg && a.sv === tr.version) {
    return a.seg;
  }
  const b = tr.s[k + 1];
  velocity(tr, k);
  velocity(tr, k + 1);
  const [dax, day] = Number.isNaN(a.o) ? [0, 0] : orientationVector(a.o);
  const [dbx, dby] = Number.isNaN(b.o) ? [0, 0] : orientationVector(b.o);
  a.seg ??= new Segment();
  const train = tr.kind === "t";
  a.seg.build(a.t, a.x, a.y, a.vx, a.vy, dax, day, b.t, b.x, b.y, b.vx, b.vy, dbx, dby,
    train, train ? turnRate(tr, k - 1) : NaN, train ? turnRate(tr, k + 1) : NaN);
  a.sv = tr.version;
  return a.seg;
}

/**
 * How fast a mover turned between samples k and k+1 (radians per tile, magnitude): the
 * angle between their velocities over the distance. NaN when they are not linked, either
 * one stands still, or they are under half a tile apart. Reads velocities only, never
 * segments (segment() asks for its neighbours' turn rates).
 */
function turnRate(tr: Track, k: number): number {
  if (k < 0 || k + 1 >= tr.s.length || !linked(tr, k)) {
    return NaN;
  }
  const p = velocity(tr, k);
  const a = velocity(tr, k + 1);
  const ds = Math.hypot(a.x - p.x, a.y - p.y);
  if (ds < 0.5 || (p.vx === 0 && p.vy === 0) || (a.vx === 0 && a.vy === 0)) {
    return NaN;
  }
  return Math.abs(Math.atan2(p.vx * a.vy - p.vy * a.vx, p.vx * a.vx + p.vy * a.vy)) / ds;
}

/** The head of a mover at time t. */
export interface Head {
  visible: boolean;
  x: number;
  y: number;
  vx: number;
  vy: number;
  /** Index of the sample at or before t (the segment start; for bridges, the sample before the gone). */
  k: number;
  /** "interp" between linked samples, "extrap"/"hold" past the last one, "bridge" across a handoff. */
  mode: "interp" | "extrap" | "hold" | "bridge" | "hidden";
  /** Hermite parameter in the segment (interp, bridge). */
  u: number;
  /** For bridges: the index of the sample the bridge ends at. */
  to: number;
  /** The drawn position still changes as t advances. */
  moving: boolean;
  /**
   * Extrapolation path ("extrap"/"hold" past sample k): distance run from it (tiles), start
   * heading (radians, atan2(vy, vx)), curvature (1/tiles, 0 = straight) and the straight
   * lead before the curve starts (tiles: the run up to the data horizon). The body follows
   * this path back to the sample.
   */
  ed: number;
  eth: number;
  ek: number;
  el: number;
  /** On a rail path: its chain and the leading joint's rail distance on it (else null). */
  rail: RailChain | null;
  ru: number;
}

export function newHead(): Head {
  return { visible: false, x: 0, y: 0, vx: 0, vy: 0, k: -1, mode: "hidden", u: 0, to: -1, moving: false, ed: 0, eth: 0, ek: 0, el: 0, rail: null, ru: 0 };
}

/**
 * Point and heading at distance `d` along an extrapolation path from (x0, y0) at heading
 * `th`: straight for `lead` tiles, then an arc of curvature `k`, straight on once it turned
 * motionConfig.extrapTurn. out = [x, y, heading].
 */
export function extrapPoint(x0: number, y0: number, th: number, k: number, d: number, out: Float64Array, lead = 0): Float64Array {
  if (k === 0 || d <= lead) {
    out[0] = x0 + Math.cos(th) * d;
    out[1] = y0 + Math.sin(th) * d;
    out[2] = th;
    return out;
  }
  if (lead > 0) {
    x0 += Math.cos(th) * lead;
    y0 += Math.sin(th) * lead;
    d -= lead;
  }
  const arc = Math.min(d, motionConfig.extrapTurn / Math.abs(k));
  const th1 = th + k * arc;
  let x = x0 + (Math.sin(th1) - Math.sin(th)) / k;
  let y = y0 + (Math.cos(th) - Math.cos(th1)) / k;
  if (d > arc) {
    x += Math.cos(th1) * (d - arc);
    y += Math.sin(th1) * (d - arc);
  }
  out[0] = x;
  out[1] = y;
  out[2] = th1;
  return out;
}

/**
 * Curvature (1/tiles) at the newest sample k: the end curvature of the segment from k-1
 * (line+arc: the arc's; a line, or an arc followed by a line: 0); 0 when straight or unknown.
 * Not the mean turn per tile since k-1: for the common straight-curve-straight pair the
 * train is on a straight again, and a small mean curvature over a long chord would turn the
 * extrapolation off it gradually for tens of tiles.
 */
function curvature(tr: Track, k: number): number {
  if (k < 1 || !linked(tr, k - 1)) {
    return 0;
  }
  const p = velocity(tr, k - 1);
  const a = velocity(tr, k);
  const ds = Math.hypot(a.x - p.x, a.y - p.y);
  if (ds < 0.5 || (p.vx === 0 && p.vy === 0) || (a.vx === 0 && a.vy === 0)) {
    return 0;
  }
  const turn = Math.atan2(p.vx * a.vy - p.vy * a.vx, p.vx * a.vx + p.vy * a.vy);
  // Below about 1.5 degrees between samples it is noise on a straight.
  if (Math.abs(turn) < 0.025) {
    return 0;
  }
  return Math.max(-1 / MIN_RADIUS, Math.min(1 / MIN_RADIUS, segment(tr, k - 1).endCurvature()));
}

const tmp = new Float64Array(4);

/** Strongest braking the extrapolation assumes (tiles/s^2). */
export const MAX_BRAKING = 40;

/**
 * Braking of a train at its newest sample k (tiles/s^2, <= 0) and its speed at k, from the
 * chord speeds of the two segments before it; [0, NaN] when it is not braking or that is
 * not known. A chord speed is the mean over its segment, so the speed at k is the last
 * chord speed plus half a segment of deceleration.
 */
function braking(tr: Track, k: number): [number, number] {
  const s = tr.s;
  if (tr.kind !== "t" || k < 2 || !linked(tr, k - 2) || !linked(tr, k - 1)) {
    return [0, NaN];
  }
  const h0 = (s[k - 1].t - s[k - 2].t) / 1000;
  const h1 = (s[k].t - s[k - 1].t) / 1000;
  if (h0 <= 0 || h1 <= 0) {
    return [0, NaN];
  }
  const c0 = Math.hypot(s[k - 1].x - s[k - 2].x, s[k - 1].y - s[k - 2].y) / h0;
  const c1 = Math.hypot(s[k].x - s[k - 1].x, s[k].y - s[k - 1].y) / h1;
  const a = Math.max(-MAX_BRAKING, (c1 - c0) / ((h0 + h1) / 2));
  // Ignore speed noise: brake only on a clear drop (10% and 1 tile/s).
  if (!(a < 0) || c0 - c1 < Math.max(1, 0.1 * c0)) {
    return [0, NaN];
  }
  return [a, Math.max(0, c1 + (a * h1) / 2)];
}

const ep = new Float64Array(3);

function extrapolate(tr: Track, k: number, t: number, horizon: number, end: number, out: Head): void {
  const a = velocity(tr, k);
  // Inside the data horizon silence is confirmed up to the heartbeat; past it, at most EXTRAPOLATE_MS.
  let cap = Math.min(HEARTBEAT_MS, Math.max(0, horizon - a.t) + EXTRAPOLATE_MS, end - a.t);
  if (end === Infinity && horizon - a.t > HEARTBEAT_MS + EXTRAPOLATE_MS) {
    // The heartbeat is known missed (no sample within it, and none after): the data stopped
    // (an instance went down without a gone, or the journey's data ends here), so the last
    // sample is the last known position. Running on would leave the mover a heartbeat's
    // run away from its track until the next sample, minutes later.
    cap = 0;
  }
  const sp = Math.hypot(a.vx, a.vy);
  const [acc, v0] = sp > 0 ? braking(tr, k) : [0, NaN];
  if (acc < 0) {
    // A braking train runs on decelerating and stops, instead of running on at full speed
    // (a missing stop sample then costs a few tiles, not the heartbeat x the speed).
    cap = Math.min(cap, (1000 * v0) / -acc);
  }
  // Distance run by time tt, and the speed then.
  const run = (tt: number): [number, number] => {
    const dt = Math.max(0, Math.min(tt - a.t, cap)) / 1000;
    return acc < 0 ? [v0 * dt + 0.5 * acc * dt * dt, v0 + acc * dt] : [sp * dt, sp];
  };
  const [dist, rate] = run(t);
  const th = sp > 0 ? Math.atan2(a.vy, a.vx) : 0;
  const kappa = sp > 0 && dist > 0 && motionConfig.extrapTurn > 0 ? curvature(tr, k) : 0;
  // Inside the data horizon silence means the mover kept to its line (the capture emits on
  // a turn of about 2 degrees): only the run past the horizon follows the curve.
  const lead = kappa !== 0 && !motionConfig.extrapTurnInside ? run(Math.min(t, horizon))[0] : 0;
  extrapPoint(a.x, a.y, th, kappa, dist, ep, lead);
  out.visible = true;
  out.k = k;
  out.to = -1;
  out.u = 0;
  out.x = ep[0];
  out.y = ep[1];
  out.ed = dist;
  out.eth = th;
  out.ek = kappa;
  out.el = lead;
  const moving = sp > 0 && rate > 0 && t - a.t < cap;
  out.vx = moving ? Math.cos(ep[2]) * rate : 0;
  out.vy = moving ? Math.sin(ep[2]) * rate : 0;
  out.mode = moving ? "extrap" : "hold";
  out.moving = moving;
}

function interpolate(tr: Track, k: number, j: number, t: number, out: Head, straight: boolean): void {
  const a = velocity(tr, k);
  const b = velocity(tr, j);
  const h = (b.t - a.t) / 1000;
  const u = h > 0 ? Math.min(1, Math.max(0, (t - a.t) / 1000 / h)) : 1;
  if (straight) {
    out.x = a.x + (b.x - a.x) * u;
    out.y = a.y + (b.y - a.y) * u;
    out.vx = h > 0 ? (b.x - a.x) / h : 0;
    out.vy = h > 0 ? (b.y - a.y) / h : 0;
  } else if (motionConfig.geometry === "rail" && j === k + 1) {
    segment(tr, k).at(t, tmp);
    out.x = tmp[0];
    out.y = tmp[1];
    out.vx = tmp[2];
    out.vy = tmp[3];
  } else {
    hermite(a.x, a.y, a.vx, a.vy, b.x, b.y, b.vx, b.vy, h, u, tmp);
    out.x = tmp[0];
    out.y = tmp[1];
    out.vx = tmp[2];
    out.vy = tmp[3];
  }
  out.visible = true;
  out.k = k;
  out.to = j;
  out.u = u;
  out.moving = a.x !== b.x || a.y !== b.y || out.vx !== 0 || out.vy !== 0;
}

/**
 * The head at time t. `horizon` is the newest time up to which every sample has arrived
 * (live: data clock minus the arrival lag; history: +Infinity).
 */
export function headAt(tr: Track, t: number, horizon: number, out: Head): Head {
  const s = tr.s;
  out.visible = false;
  out.mode = "hidden";
  out.moving = false;
  out.ed = 0;
  out.ek = 0;
  out.el = 0;
  out.rail = null;
  let k = tr.find(t);
  if (k < 0) {
    return out;
  }
  if (isGone(s[k])) {
    const r = resumeOf(tr, k, horizon);
    const before = k - 1;
    if (before < 0 || isGone(s[before])) {
      return out;
    }
    if (r >= 0) {
      bridge(tr, before, r, t, out);
      return out;
    }
    if (r === -2) {
      // Possibly an instance crossing whose resume has not arrived: hold at the last position.
      extrapolate(tr, before, t, Math.min(horizon, s[k].t), s[k].t, out);
      out.mode = "hold";
      out.vx = 0;
      out.vy = 0;
    }
    return out;
  }
  const next = s[k + 1];
  if (!next) {
    if (railKnown(tr, k)) {
      railExtrapolate(tr, k, t, horizon, Infinity, out);
    } else {
      extrapolate(tr, k, t, horizon, Infinity, out);
    }
    return out;
  }
  if (isGone(next)) {
    const r = resumeOf(tr, k + 1, horizon);
    if (r >= 0) {
      bridge(tr, k, r, t, out);
      return out;
    }
    // Never towards a gone: run on (bounded) until it.
    if (railKnown(tr, k)) {
      railExtrapolate(tr, k, t, Math.min(horizon, next.t), next.t, out);
    } else {
      extrapolate(tr, k, t, Math.min(horizon, next.t), next.t, out);
    }
    return out;
  }
  if (railLinked(tr, k)) {
    railInterpolate(tr, k, t, out);
    return out;
  }
  if (!linked(tr, k)) {
    const parked = (s[k].flags & (F_STOP | F_RAIL)) === (F_STOP | F_RAIL);
    if (t - s[k].t > HEARTBEAT_MS && !isEndSwitch(tr, k) && !stationary(tr, k) && !parked) {
      // A data gap after which the mover is somewhere else (an instance down or a save
      // reloaded without a gone): where it was meanwhile is unknown, and the capture would
      // have sent a sample within a heartbeat had it still been here, so it is not drawn
      // until the next sample instead of standing still and then jumping there. (A rail path
      // capture sends nothing for a parked train: it stood at its stop sample until then.)
      return out;
    }
    // A data gap or an end switch: hold until the next sample (it is known, so running on
    // along the old velocity would only have to jump back).
    extrapolate(tr, k, t, -Infinity, s[k].t, out);
    return out;
  }
  interpolate(tr, k, k + 1, t, out, false);
  out.mode = "interp";
  return out;
}

/**
 * Across a handoff (a = the last sample before the gone, b = the resume): a straight blend
 * in time from a to b (or a Hermite with both sides' velocities), keeping the history.
 * Measured on the test data this beats holding until the gone and then blending: the
 * samples before a crossing are often far ahead of the gone row.
 */
function bridge(tr: Track, a: number, b: number, t: number, out: Head): void {
  const s = tr.s;
  // A mover that stood still long before the gone (a parked train that then crossed or
  // switched ends): hold at a and blend only over the handoff window before the gone.
  // Blending over the whole time from a put the head most of the way to b the moment the
  // resume became known: a jump of up to a train's length.
  const start = s[a + 1].t - HANDOFF_MS;
  if (s[a].t < start) {
    const A = s[a];
    const B = s[b];
    const h = (B.t - start) / 1000;
    const u = h > 0 ? Math.min(1, Math.max(0, (t - start) / 1000 / h)) : 1;
    out.x = A.x + (B.x - A.x) * u;
    out.y = A.y + (B.y - A.y) * u;
    const moving = u > 0 && u < 1 && h > 0;
    out.vx = moving ? (B.x - A.x) / h : 0;
    out.vy = moving ? (B.y - A.y) / h : 0;
    out.visible = true;
    out.k = a;
    out.to = b;
    out.u = u;
    out.moving = moving;
    out.mode = "bridge";
    return;
  }
  interpolate(tr, a, b, t, out, motionConfig.bridge === "line");
  out.mode = "bridge";
}

function pushHermite(tr: Track, k: number, j: number, u0: number, poly: Polyline, step: number, t = NaN): void {
  if (motionConfig.geometry === "rail" && j === k + 1) {
    // Sample the segment geometry by arc length, from the head's position back to its start.
    const seg = segment(tr, k);
    let s0 = seg.length;
    if (!Number.isNaN(t)) {
      seg.arcAt(t, tmp);
      s0 = tmp[0];
    }
    const n = Math.max(1, Math.ceil(s0 / step));
    for (let i = 1; i <= n; i++) {
      seg.pointAt(s0 * (1 - i / n), tmp);
      poly.push(tmp[0], tmp[1]);
    }
    return;
  }
  const a = velocity(tr, k);
  const b = velocity(tr, j);
  const h = (b.t - a.t) / 1000;
  const chord = Math.hypot(b.x - a.x, b.y - a.y) * u0;
  const n = Math.max(1, Math.ceil(chord / step));
  for (let i = 1; i <= n; i++) {
    const u = u0 * (1 - i / n);
    hermite(a.x, a.y, a.vx, a.vy, b.x, b.y, b.vx, b.vy, h, u, tmp);
    poly.push(tmp[0], tmp[1]);
  }
}

/** Appends the path through a carriage chain (centres and bogies), starting at carriage 0. */
function pushCarriages(car: Carriage[], poly: Polyline): void {
  for (let i = 0; i < car.length; i++) {
    const c = car[i];
    const [ux, uy] = orientationVector(c.o);
    let fx = c.x + ux * BOGIE;
    let fy = c.y + uy * BOGIE;
    let rx = c.x - ux * BOGIE;
    let ry = c.y - uy * BOGIE;
    // The front bogie is the one towards the leading end (the previous carriage).
    const px = i > 0 ? car[i - 1].x : poly.lastX();
    const py = i > 0 ? car[i - 1].y : poly.lastY();
    const nx = i + 1 < car.length ? car[i + 1].x : NaN;
    const ny = i + 1 < car.length ? car[i + 1].y : NaN;
    let swap: boolean;
    if (i > 0) {
      swap = Math.hypot(rx - px, ry - py) < Math.hypot(fx - px, fy - py);
    } else if (!Number.isNaN(nx)) {
      swap = Math.hypot(fx - nx, fy - ny) < Math.hypot(rx - nx, ry - ny);
    } else {
      swap = false;
    }
    if (swap) {
      [fx, rx] = [rx, fx];
      [fy, ry] = [ry, fy];
    }
    if (i > 0) {
      poly.push(fx, fy);
      poly.push(c.x, c.y);
    }
    poly.push(rx, ry);
  }
}

/**
 * The path from the head back `length` tiles along the head's spline history. Where the
 * history ends it continues along the newest carriage geometry behind it, else straight
 * (along the last step, or against the orientation when there is no step).
 */
export function bodyPath(tr: Track, head: Head, t: number, length: number, poly: Polyline, step = PATH_STEP, depth = 0): Polyline {
  poly.clear();
  poly.push(head.x, head.y);
  const s = tr.s;
  let j = head.k;
  if (j < 0) {
    return poly;
  }
  if (head.mode === "interp") {
    pushHermite(tr, head.k, head.to, head.u, poly, step, t);
  } else if (head.mode === "bridge") {
    poly.push(s[j].x, s[j].y);
  } else {
    if (head.ek !== 0 && head.ed > head.el + step) {
      // Back along the extrapolated arc to the sample.
      const n = Math.ceil(head.ed / step);
      for (let i = 1; i < n; i++) {
        extrapPoint(s[j].x, s[j].y, head.eth, head.ek, head.ed * (1 - i / n), ep, head.el);
        poly.push(ep[0], ep[1]);
      }
    }
    poly.push(s[j].x, s[j].y);
  }
  while (poly.length < length) {
    const cur = s[j];
    if (cur.car && cur.car.length > 0 && Math.hypot(cur.car[0].x - cur.x, cur.car[0].y - cur.y) < 1.5) {
      pushCarriages(cur.car, poly);
      break;
    }
    if (j === 0) {
      break;
    }
    const prev = s[j - 1];
    if (isGone(prev)) {
      // A handoff: bridge straight to the sample before the gone and keep the history.
      if (j >= 2 && !isGone(s[j - 2]) && cur.t - prev.t <= HANDOFF_MS) {
        poly.push(s[j - 2].x, s[j - 2].y);
        j -= 2;
        continue;
      }
      break;
    }
    if (isEndSwitch(tr, j - 1) && !switchReverses(tr, j - 1)) {
      // The sampled carriage moved to the front of a train running on: the body runs back
      // over the old head's positions.
      poly.push(prev.x, prev.y);
      j--;
      continue;
    }
    if (isEndSwitch(tr, j - 1)) {
      if (depth === 0) {
        // The body behind the new leading end is the old body, reversed: from the old body's
        // point nearest the new head back to the old head (then straight on, below).
        const old = newHead();
        extrapolate(tr, j - 1, prev.t, prev.t, prev.t, old);
        const back = bodyPath(tr, old, prev.t, length + 8, new Polyline(), step, 1);
        let m = 0;
        let best = Infinity;
        for (let i = 0; i < back.size; i++) {
          const d = Math.hypot(back.xs[i] - cur.x, back.ys[i] - cur.y);
          if (d < best) {
            best = d;
            m = i;
          }
        }
        for (let i = m; i >= 0; i--) {
          poly.push(back.xs[i], back.ys[i]);
        }
      }
      break;
    }
    if (!linked(tr, j - 1)) {
      if (stationary(tr, j - 1)) {
        // Parked across a long gap: the history before the stop still holds the body.
        poly.push(prev.x, prev.y);
        j--;
        continue;
      }
      break;
    }
    pushHermite(tr, j - 1, j, 1, poly, step);
    j--;
  }
  if (poly.length < length) {
    let dx = 0;
    let dy = 0;
    const n = poly.size;
    for (let i = n - 1; i > 0 && dx === 0 && dy === 0; i--) {
      const ex = poly.xs[n - 1] - poly.xs[i - 1];
      const ey = poly.ys[n - 1] - poly.ys[i - 1];
      if (Math.hypot(ex, ey) > 0.05) {
        dx = ex;
        dy = ey;
      }
    }
    if (dx === 0 && dy === 0) {
      const o = s[Math.max(0, j)].o;
      if (head.vx !== 0 || head.vy !== 0) {
        dx = -head.vx;
        dy = -head.vy;
      } else if (!Number.isNaN(o)) {
        const [ux, uy] = orientationVector(o);
        dx = -ux;
        dy = -uy;
      } else {
        dx = -1;
      }
    }
    const d = Math.hypot(dx, dy);
    const rest = length - poly.length + step;
    const k = tailCurvature(poly);
    if (k === 0) {
      poly.push(poly.lastX() + (dx / d) * rest, poly.lastY() + (dy / d) * rest);
    } else {
      // The known path ends on a curve (a journey that starts in one): rails go on along
      // it, so the body follows the same curvature for up to TAIL_TURN, then straight.
      const th = Math.atan2(dy, dx);
      const x0 = poly.lastX();
      const y0 = poly.lastY();
      const arc = Math.min(rest, TAIL_TURN / Math.abs(k));
      const n = Math.max(1, Math.ceil(arc / step));
      for (let i = 1; i <= n; i++) {
        const a = (arc * i) / n;
        const th1 = th + k * a;
        poly.push(x0 + (Math.sin(th1) - Math.sin(th)) / k, y0 + (Math.cos(th) - Math.cos(th1)) / k);
      }
      if (rest > arc) {
        const th1 = th + k * arc;
        poly.push(poly.lastX() + Math.cos(th1) * (rest - arc), poly.lastY() + Math.sin(th1) * (rest - arc));
      }
    }
  }
  return poly;
}

/** Path length (tiles) over which tailCurvature measures the turn. */
const TAIL_SPAN = 6;
/** Largest turn (radians) a body path continues along the curvature its known path ended on. */
export const TAIL_TURN = Math.PI / 2;

/**
 * Signed curvature (1/tiles, in the polyline's direction of travel) of the last TAIL_SPAN
 * tiles of `poly`: the turn between its two halves over half the span. 0 when the path is
 * shorter, straight (under about 3 degrees) or turns tighter than any rail (MIN_RADIUS).
 */
function tailCurvature(poly: Polyline): number {
  const n = poly.size;
  const len = poly.length;
  if (n < 3 || len < TAIL_SPAN) {
    return 0;
  }
  let a = n - 1;
  while (a > 0 && len - poly.ls[a] < TAIL_SPAN / 2) {
    a--;
  }
  let b = a;
  while (b > 0 && len - poly.ls[b] < TAIL_SPAN) {
    b--;
  }
  const x1 = poly.xs[n - 1] - poly.xs[a];
  const y1 = poly.ys[n - 1] - poly.ys[a];
  const x0 = poly.xs[a] - poly.xs[b];
  const y0 = poly.ys[a] - poly.ys[b];
  const half = poly.ls[a] - poly.ls[b];
  if (half <= 0 || poly.ls[n - 1] - poly.ls[a] <= 0) {
    return 0;
  }
  const turn = Math.atan2(x0 * y1 - y0 * x1, x0 * x1 + y0 * y1);
  if (Math.abs(turn) < 0.05) {
    return 0;
  }
  const k = turn / ((half + poly.ls[n - 1] - poly.ls[a]) / 2);
  return Math.abs(k) > 1 / MIN_RADIUS ? 0 : k;
}

/** One carriage to draw: centre, angle (radians, towards the leading end), kind letter. */
export interface Car {
  x: number;
  y: number;
  a: number;
  loco: boolean;
  kind: string;
}

const pa = new Float64Array(2);
const pb = new Float64Array(2);

/**
 * Places carriages along `poly` (from the head back): carriage i's bogies sit BOGIE either
 * side of its arc position, its centre is their midpoint and its angle points from the rear
 * bogie to the front one, as Factorio places rolling stock on rails.
 */
export function placeCarriages(poly: Polyline, head: Head, kinds: string, gaps: number[], out: Car[]): Car[] {
  const n = gaps.length + 1;
  out.length = 0;
  let s = 0;
  let hint = 0;
  for (let i = 0; i < n; i++) {
    if (i > 0) {
      s += gaps[i - 1];
    }
    let cx: number;
    let cy: number;
    let fx: number;
    let fy: number;
    let rx: number;
    let ry: number;
    if (i === 0) {
      cx = head.x;
      cy = head.y;
      hint = poly.at(BOGIE, pb, hint);
      rx = pb[0];
      ry = pb[1];
      const sp = Math.hypot(head.vx, head.vy);
      if (sp > 1e-6) {
        fx = cx + (head.vx / sp) * BOGIE;
        fy = cy + (head.vy / sp) * BOGIE;
      } else {
        fx = 2 * cx - rx;
        fy = 2 * cy - ry;
      }
    } else {
      hint = poly.at(s - BOGIE, pa, hint);
      fx = pa[0];
      fy = pa[1];
      hint = poly.at(s + BOGIE, pb, hint);
      rx = pb[0];
      ry = pb[1];
      cx = (fx + rx) / 2;
      cy = (fy + ry) / 2;
    }
    const kind = kinds[i] ?? "C";
    out.push({ x: cx, y: cy, a: Math.atan2(fy - ry, fx - rx), loco: kind === "L" || kind === "l", kind });
  }
  return out;
}
