// The movers of one surface at a displayed time: tracks fed from MVW3 windows, keyframes,
// live payloads and live events, evaluated per frame into drawable movers (head, heading,
// a train's carriages). A frame never snaps: when new data changes where a drawn mover or
// any of its carriages would have been, the difference is blended out over CORRECTION_MS, or
// longer for a large difference so the blend never moves a mover faster than
// CORRECTION_RATE on top of its own motion.
//
// DOM-free; the mover layer draws what `frame` returns.

import type { LiveMoversEvent, MoverMeta, MoverWindow } from "../api/types";
import { Polyline, orientationVector } from "./hermite";
import {
  type Car,
  F_GONE,
  F_NO_HEADING,
  F_NO_TICK,
  HANDOFF_MS,
  PITCH,
  type Head,
  type Sample,
  Track,
  bodyPath,
  headAt,
  makeSample,
  newHead,
  pitches,
  placeCarriages,
} from "./track";

/** New data that moves a drawn mover is blended in over at least this long. */
export const CORRECTION_MS = 400;
/** A blend moves a mover by at most this (tiles/s) on top of its own motion... */
export const CORRECTION_RATE = 15;
/** ...unless that would take longer than this. */
export const MAX_CORRECTION_MS = 2000;
/** A correction larger than this is a teleport and is not blended... */
export const MAX_CORRECTION = 30;
/**
 * ...except an overrun: the drawn mover ran ahead along its own line (it extrapolated
 * through a stop or a slowdown that no sample announced). Up to this size it is not blended
 * back in time: the drawn mover and its carriages stand where they were drawn until the
 * true head, running along its own track, reaches that point, so it neither jumps back nor
 * slides backwards.
 */
export const MAX_OVERRUN = 150;
/**
 * An overrun lies within this (tiles) of the true head's line of travel, so the drawn mover
 * waits on the track the true head is about to use. While waiting, the true head turning
 * away by twice this (a curve or a junction before the drawn point) ends the wait.
 */
export const OVERRUN_LATERAL = 1;
/**
 * A wait ends after this long, or at once when the true head stands still; the rest then
 * blends out at CORRECTION_RATE (along the line, which is the track the overrun lies on).
 */
export const OVERRUN_WAIT_MS = 6000;
/** Samples older than this behind the displayed time are dropped (body history needs less). */
export const KEEP_MS = 120_000;

/** What a frame draws for one mover. Objects are reused between frames. */
export interface Shown {
  id: string;
  kind: "t" | "v" | "p";
  meta: MoverMeta;
  x: number;
  y: number;
  /** Heading unit vector (direction of travel, or the orientation at rest). */
  hx: number;
  hy: number;
  /** The drawn position still changes as time advances. */
  moving: boolean;
  /** A train's carriages from the leading end; empty for other kinds or without bodies. */
  cars: Car[];
  track: Track;
}

interface RenderState {
  head: Head;
  poly: Polyline;
  gaps: number[];
  /** The uncorrected body, reused while `rawKey` matches the head state. */
  raw: Car[];
  rawKey: number[];
  shown: Shown;
  version: number;
  drawn: boolean;
  /** What was drawn last frame: time, head, carriage positions (x, y pairs). */
  lastT: number;
  lastX: number;
  lastY: number;
  lastCars: number[];
  /** Correction offsets (head, then per carriage x, y pairs) fading from time ct. */
  cx: number;
  cy: number;
  cc: number[];
  ct: number;
  /** Duration of the current blend (ms). */
  cd: number;
  /**
   * Overrun wait (MAX_OVERRUN), while `cl` (the overrun length) is above 0: the frozen pose
   * (head fx, fy, heading fhx, fhy, carriages fcars) is drawn while the true head's progress
   * `cp` along its own direction of travel (measured frame to frame from cpx, cpy) is below
   * cl and the frozen point is still ahead of it.
   */
  cl: number;
  cp: number;
  cpx: number;
  cpy: number;
  fx: number;
  fy: number;
  fhx: number;
  fhy: number;
  fcars: Car[];
}

export interface FrameOptions {
  /** Build train bodies (carriages). Off at far zoom, where trains are drawn as markers. */
  bodies?: boolean;
  /** Only movers whose head is inside [x0, y0, x1, y1] (expanded by the train length). */
  bounds?: [number, number, number, number];
}

const scratchHead = newHead();
const scratchPoly = new Polyline();
const scratchGaps: number[] = [];
const scratchCars: Car[] = [];

export class MoverView {
  readonly tracks = new Map<string, Track>();
  private readonly states = new Map<string, RenderState>();
  private readonly out: Shown[] = [];
  /** Time of the last frame, to tell a continuous frame from a seek. */
  private lastFrameT = NaN;
  /**
   * Set by `frame`: the earliest sample time after the frame's time over all tracks
   * (Infinity when none). A frame at or after it can differ even where nothing is moving
   * now: in live mode samples arrive before the delayed display time reaches them (a parked
   * train about to start, a mover about to appear).
   */
  nextT = Infinity;

  get size(): number {
    return this.tracks.size;
  }

  clear(): void {
    this.tracks.clear();
    this.states.clear();
    this.lastFrameT = NaN;
  }

  /**
   * Adds the rows of a decoded MVW3 window, keyframe or live payload. `onRow` sees each new
   * row's time and, when the row extends a moving mover's track, the previous sample's time
   * (else NaN): the segment between them became known with this row (readiness, clock.ts).
   */
  addWindow(w: MoverWindow, onRow?: (t: number, prevT: number) => void): void {
    for (const r of w.rows) {
      const meta = w.movers[r.mover];
      const o = (r.flags & F_NO_HEADING) !== 0 ? NaN : r.o;
      const tick = (r.flags & F_NO_TICK) !== 0 ? null : r.tick;
      if (this.add(meta, makeSample(meta, r.t, r.x, r.y, o, r.flags, r.carriages, tick)) && onRow) {
        onRow(r.t, this.prevT);
      }
    }
  }

  /** Adds the rows of a live `movers` event. */
  addLive(e: LiveMoversEvent, onRow?: (t: number, prevT: number) => void): void {
    for (const [entry, t, tick, x, y, o, flags, car] of e.rows) {
      const meta = e.movers[entry];
      if (!meta) {
        continue;
      }
      const carriages = car ? car.map(([cx, cy, co]) => ({ x: cx, y: cy, o: co })) : null;
      const s = makeSample(meta, t, x, y, (flags & F_NO_HEADING) !== 0 ? NaN : o, flags, carriages, tick);
      if (this.add(meta, s) && onRow) {
        onRow(t, this.prevT);
      }
    }
  }

  /**
   * Set by `add`: when the added sample became the newest of its track and the one before it
   * is a different position of the same run (no gone in between), that sample's time; else NaN.
   */
  prevT = NaN;

  /** Adds one sample; returns false when the track already had this row. */
  add(meta: MoverMeta, s: Sample): boolean {
    this.prevT = NaN;
    let tr = this.tracks.get(meta.id);
    if (!tr) {
      tr = new Track(meta.id, meta);
      this.tracks.set(meta.id, tr);
    }
    const i = tr.find(s.t);
    if (i >= 0) {
      const old = tr.s[i];
      if (old.t === s.t && old.x === s.x && old.y === s.y && old.flags === s.flags && (old.car !== null) === (s.car !== null)) {
        return false;
      }
    }
    tr.insert(s);
    const n = tr.s.length;
    if (n >= 2 && tr.s[n - 1] === s) {
      const p = tr.s[n - 2];
      if (((p.flags | s.flags) & F_GONE) === 0 && (p.x !== s.x || p.y !== s.y)) {
        this.prevT = p.t;
      }
    }
    return true;
  }

  /** Drops history far behind `t` and journeys that ended before it. */
  trim(t: number): void {
    for (const [id, tr] of this.tracks) {
      const last = tr.s[tr.s.length - 1];
      if (!last || ((last.flags & F_GONE) !== 0 && last.t < t - HANDOFF_MS - KEEP_MS)) {
        this.tracks.delete(id);
        this.states.delete(id);
        continue;
      }
      tr.trim(t - KEEP_MS, (tr.meta.n ?? 1) * PITCH + 40);
    }
  }

  /**
   * Every mover shown at `t`. `horizon` is the time up to which the feed is complete
   * (DelayEstimator.horizon in live mode, the loaded range's end in history).
   */
  frame(t: number, horizon: number, opts: FrameOptions = {}): Shown[] {
    const out = this.out;
    out.length = 0;
    const continuous = Number.isFinite(this.lastFrameT) && t >= this.lastFrameT && t - this.lastFrameT < 2000;
    this.lastFrameT = t;
    const b = opts.bounds;
    const bodies = opts.bodies !== false;
    this.nextT = Infinity;
    for (const [id, tr] of this.tracks) {
      const last = tr.s[tr.s.length - 1];
      if (last && last.t > t) {
        const nx = tr.s[tr.find(t) + 1];
        if (nx && nx.t < this.nextT) {
          this.nextT = nx.t;
        }
      }
      let st = this.states.get(id);
      if (!st) {
        st = newState(tr);
        this.states.set(id, st);
      }
      const head = headAt(tr, t, horizon, st.head);
      if (!head.visible) {
        st.drawn = false;
        continue;
      }
      const meta = tr.s[head.k]?.meta ?? tr.meta;
      const train = tr.kind === "t" && bodies;
      if (continuous && st.drawn && st.version !== tr.version) {
        this.correct(tr, st, horizon, train);
      } else if (!continuous) {
        st.cx = 0;
        st.cy = 0;
        st.cc.length = 0;
        st.cl = 0;
      }
      st.version = tr.version;
      const holding = st.cl > 0 && this.hold(tr, st, t, head, meta, train);
      const w = holding ? 0 : weight(st, t);
      const x = holding ? st.fx : head.x + st.cx * w;
      const y = holding ? st.fy : head.y + st.cy * w;
      st.lastT = t;
      st.lastX = x;
      st.lastY = y;
      st.drawn = true;
      if (b) {
        const pad = tr.kind === "t" ? (meta.n ?? 1) * PITCH + 8 : 8;
        if (x < b[0] - pad || x > b[2] + pad || y < b[1] - pad || y > b[3] + pad) {
          st.lastCars.length = 0;
          continue;
        }
      }
      const sh = st.shown;
      sh.meta = meta;
      sh.kind = meta.k;
      sh.x = x;
      sh.y = y;
      sh.moving = head.moving || w > 0 || holding;
      const sp = Math.hypot(head.vx, head.vy);
      const o = tr.s[head.k].o;
      if (holding) {
        sh.hx = st.fhx;
        sh.hy = st.fhy;
      } else if (sp > 1e-3) {
        sh.hx = head.vx / sp;
        sh.hy = head.vy / sp;
      } else if (!Number.isNaN(o)) {
        [sh.hx, sh.hy] = orientationVector(o);
      } else {
        sh.hx = 1;
        sh.hy = 0;
      }
      if (!train) {
        sh.cars.length = 0;
        st.lastCars.length = 0;
      } else if (holding && st.fcars.length > 0) {
        copyCars(st.fcars, sh.cars);
        st.lastCars.length = 0;
        for (const c of sh.cars) {
          st.lastCars.push(c.x, c.y);
        }
      } else {
        rawBody(tr, st, head, t, meta);
        copyCars(st.raw, sh.cars);
        st.lastCars.length = 0;
        for (let i = 0; i < sh.cars.length; i++) {
          const c = sh.cars[i];
          if (i === 0) {
            c.x = x;
            c.y = y;
          } else if (w > 0 && 2 * i + 1 < st.cc.length) {
            c.x += st.cc[2 * i] * w;
            c.y += st.cc[2 * i + 1] * w;
          }
          st.lastCars.push(c.x, c.y);
        }
      }
      out.push(sh);
    }
    return out;
  }

  /**
   * New data arrived for a drawn mover: what it puts at the last drawn time, minus what was
   * drawn there (old corrections included), becomes the correction, fading from then.
   */
  private correct(tr: Track, st: RenderState, horizon: number, train: boolean): void {
    const before = headAt(tr, st.lastT, horizon, scratchHead);
    st.ct = st.lastT;
    st.cc.length = 0;
    st.cl = 0;
    if (!before.visible) {
      st.cx = 0;
      st.cy = 0;
      return;
    }
    st.cx = st.lastX - before.x;
    st.cy = st.lastY - before.y;
    const len = Math.hypot(st.cx, st.cy);
    let max = len;
    // An overrun: the drawn head is ahead of the true one, on its line of travel.
    let hx = before.vx;
    let hy = before.vy;
    if (Math.hypot(hx, hy) < 1e-3) {
      hx = st.shown.hx;
      hy = st.shown.hy;
    }
    const hl = Math.hypot(hx, hy);
    const ahead = hl > 0 ? (st.cx * hx + st.cy * hy) / hl : 0;
    const lateral = hl > 0 ? Math.abs(st.cx * hy - st.cy * hx) / hl : len;
    if (ahead >= 1 && len <= MAX_OVERRUN && lateral <= OVERRUN_LATERAL) {
      st.cl = ahead;
      st.cp = 0;
      st.cpx = before.x;
      st.cpy = before.y;
      st.fx = st.lastX;
      st.fy = st.lastY;
      st.fhx = st.shown.hx;
      st.fhy = st.shown.hy;
      st.fcars.length = 0;
      if (train && st.lastCars.length > 0 && 2 * st.shown.cars.length === st.lastCars.length) {
        copyCars(st.shown.cars, st.fcars);
      }
      st.cx = 0;
      st.cy = 0;
      return;
    }
    if (train && st.lastCars.length > 0) {
      const meta = tr.s[before.k]?.meta ?? tr.meta;
      buildBody(tr, before, st.lastT, meta, scratchPoly, scratchGaps, scratchCars);
      if (2 * scratchCars.length === st.lastCars.length) {
        for (let i = 0; i < scratchCars.length; i++) {
          const dx = st.lastCars[2 * i] - scratchCars[i].x;
          const dy = st.lastCars[2 * i + 1] - scratchCars[i].y;
          st.cc.push(dx, dy);
          max = Math.max(max, Math.hypot(dx, dy));
        }
      }
    }
    // An overrun off the line (it ran on past a curve the true head took, or a junction):
    // no wait, but no teleport either; it blends out at CORRECTION_RATE, however long that takes.
    const offLine = ahead >= 1 && max <= MAX_OVERRUN;
    if (max <= 0.02 || (max >= MAX_CORRECTION && !offLine)) {
      st.cx = 0;
      st.cy = 0;
      st.cc.length = 0;
    }
    const cd = Math.max(CORRECTION_MS, (1000 * max) / CORRECTION_RATE);
    st.cd = offLine ? cd : Math.min(MAX_CORRECTION_MS, cd);
  }

  /**
   * One frame of an overrun wait: true while the frozen pose is still drawn. The wait ends
   * when the true head has run the overrun's length along its own track, has passed the
   * frozen point or turned away from it, stands still, or after OVERRUN_WAIT_MS. What is
   * then left between the frozen pose and the true one blends out at CORRECTION_RATE,
   * unless it is no more than the true head ran this frame (that is no jump).
   */
  private hold(tr: Track, st: RenderState, t: number, head: Head, meta: MoverMeta, train: boolean): boolean {
    const dx = head.x - st.cpx;
    const dy = head.y - st.cpy;
    const step = Math.hypot(dx, dy);
    const sp = Math.hypot(head.vx, head.vy);
    if (sp > 1e-3) {
      st.cp += (dx * head.vx + dy * head.vy) / sp;
    }
    st.cpx = head.x;
    st.cpy = head.y;
    const rx = st.fx - head.x;
    const ry = st.fy - head.y;
    const rest = Math.hypot(rx, ry);
    const ahead = sp > 1e-3 ? (rx * head.vx + ry * head.vy) / sp : 0;
    const lateral = sp > 1e-3 ? Math.abs(rx * head.vy - ry * head.vx) / sp : rest;
    if (head.moving && st.cp < st.cl && ahead > 0 && lateral <= 2 * OVERRUN_LATERAL && t - st.ct <= OVERRUN_WAIT_MS) {
      return true;
    }
    st.cl = 0;
    st.cc.length = 0;
    if (rest <= Math.max(0.5, step)) {
      st.cx = 0;
      st.cy = 0;
      return false;
    }
    st.cx = rx;
    st.cy = ry;
    st.ct = t;
    let max = rest;
    if (train && st.fcars.length > 0) {
      rawBody(tr, st, head, t, meta);
      if (st.raw.length === st.fcars.length) {
        for (let i = 0; i < st.raw.length; i++) {
          const ox = st.fcars[i].x - st.raw[i].x;
          const oy = st.fcars[i].y - st.raw[i].y;
          st.cc.push(ox, oy);
          max = Math.max(max, Math.hypot(ox, oy));
        }
      }
    }
    // Along the line, so no MAX_CORRECTION_MS cap: it never moves faster than CORRECTION_RATE.
    st.cd = Math.max(CORRECTION_MS, (1000 * max) / CORRECTION_RATE);
    return false;
  }
}

/** The uncorrected body for this head state into st.raw (cached while the state holds). */
function rawBody(tr: Track, st: RenderState, head: Head, t: number, meta: MoverMeta): void {
  if (!sameBody(st.rawKey, tr.version, head)) {
    buildBody(tr, head, t, meta, st.poly, st.gaps, st.raw);
    st.rawKey = [tr.version, head.k, head.x, head.y, head.u];
  }
}

function buildBody(tr: Track, head: Head, t: number, meta: MoverMeta, poly: Polyline, gaps: number[], out: Car[]): void {
  pitches(tr, head.k, gaps);
  let length = 4;
  for (const g of gaps) {
    length += g;
  }
  bodyPath(tr, head, t, length, poly);
  placeCarriages(poly, head, meta.o ?? "", gaps, out);
}

function copyCars(from: Car[], to: Car[]): void {
  to.length = from.length;
  for (let i = 0; i < from.length; i++) {
    const f = from[i];
    const c = to[i];
    if (c) {
      c.x = f.x;
      c.y = f.y;
      c.a = f.a;
      c.loco = f.loco;
      c.kind = f.kind;
    } else {
      to[i] = { ...f };
    }
  }
}

/** Whether the raw body built for `key` is valid for this head state. */
function sameBody(key: number[], version: number, head: Head): boolean {
  return key.length === 5 && key[0] === version && key[1] === head.k && key[2] === head.x && key[3] === head.y && key[4] === head.u;
}

function weight(st: RenderState, t: number): number {
  if (st.cx === 0 && st.cy === 0 && st.cc.length === 0) {
    return 0;
  }
  return Math.max(0, 1 - (t - st.ct) / st.cd);
}

function newState(tr: Track): RenderState {
  return {
    head: newHead(),
    poly: new Polyline(),
    gaps: [],
    raw: [],
    rawKey: [],
    shown: { id: tr.id, kind: tr.kind, meta: tr.meta, x: 0, y: 0, hx: 1, hy: 0, moving: false, cars: [], track: tr },
    version: -1,
    drawn: false,
    lastT: NaN,
    lastX: 0,
    lastY: 0,
    lastCars: [],
    cx: 0,
    cy: 0,
    cc: [],
    ct: 0,
    cd: CORRECTION_MS,
    cl: 0,
    cp: 0,
    cpx: 0,
    cpy: 0,
    fx: 0,
    fy: 0,
    fhx: 1,
    fhy: 0,
    fcars: [],
  };
}
