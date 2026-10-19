// The path between two consecutive samples (PLAN.md §4.8 step 2, refined). Rails are lines
// and arcs, and a sample pair often spans a long straight that ends just inside a curve:
// a cubic cannot be straight for 60 tiles and then turn, so with accurate end tangents it
// bulges off the straight. Where the end points and tangents allow it, the segment is
// therefore a straight line followed by a circular arc (or an arc followed by a line);
// otherwise it falls back to the cubic Hermite. Motion along the geometry is timed by a 1D
// cubic Hermite of arc length with the samples' speeds as end slopes.
//
// Pure math, no imports besides hermite.ts.

import { hermite } from "./hermite";

/** Kinds of segment geometry. */
export const SEG_LINE = 0;
export const SEG_LINE_ARC = 1;
export const SEG_ARC_LINE = 2;
export const SEG_HERMITE = 3;

/** Arcs tighter than this are not rail-like; such segments use the Hermite fallback. */
const MIN_RADIUS = 3;
/** Segments longer than this times their chord (plus a tile) are rejected as loops. */
const MAX_STRETCH = 1.5;
/** Tangents within this angle (radians) count as parallel. */
const PARALLEL = 1e-3;

const tmp = new Float64Array(4);

/** Geometry of "a line of length l0 from p along t, then an arc of turn theta". */
interface LineArc {
  l0: number;
  r: number;
  theta: number;
}

/** Solves line+arc from (p0, t0) to (p1, t1); null when it does not exist. */
function lineArc(p0x: number, p0y: number, t0x: number, t0y: number, p1x: number, p1y: number, t1x: number, t1y: number): LineArc | null {
  const theta = Math.atan2(t0x * t1y - t0y * t1x, t0x * t1x + t0y * t1y);
  const dx = p1x - p0x;
  const dy = p1y - p0y;
  const f = dx * t0x + dy * t0y;
  const h = t0x * dy - t0y * dx;
  if (Math.abs(theta) < PARALLEL || Math.abs(theta) > 0.75 * Math.PI || h * theta <= 0 || f <= 0) {
    return null;
  }
  const l0 = f - h / Math.tan(theta / 2);
  if (l0 < 0 || l0 > f) {
    return null;
  }
  const chord = Math.hypot(f - l0, h);
  const r = chord / (2 * Math.abs(Math.sin(theta / 2)));
  if (r < MIN_RADIUS) {
    return null;
  }
  return { l0, r, theta };
}

/** One segment between samples a (time ta) and b (time tb). Reused objects; call `build`. */
export class Segment {
  kind = SEG_LINE;
  /** Total arc length (tiles); for the Hermite fallback, an estimate from the chord. */
  length = 0;
  ta = 0;
  tb = 0;
  private h = 0;
  private v0 = 0;
  private v1 = 0;
  // endpoints and unit tangents
  private p0x = 0;
  private p0y = 0;
  private t0x = 1;
  private t0y = 0;
  private p1x = 0;
  private p1y = 0;
  private t1x = 1;
  private t1y = 0;
  // line+arc parameters, in the frame of the line's start (reversed for arc+line)
  private l0 = 0;
  private r = 0;
  private theta = 0;
  // Hermite fallback velocities (tiles/s)
  private hv0x = 0;
  private hv0y = 0;
  private hv1x = 0;
  private hv1y = 0;

  /**
   * Builds the segment. (vax, vay) and (vbx, vby) are the samples' velocities (tiles/s);
   * (dax, day) and (dbx, dby) are unit directions to use where a velocity is zero (e.g.
   * from the orientation), or (0, 0) to use the chord.
   */
  build(
    ta: number, ax: number, ay: number, vax: number, vay: number, dax: number, day: number,
    tb: number, bx: number, by: number, vbx: number, vby: number, dbx: number, dby: number,
  ): this {
    this.ta = ta;
    this.tb = tb;
    this.h = Math.max(1e-3, (tb - ta) / 1000);
    this.p0x = ax;
    this.p0y = ay;
    this.p1x = bx;
    this.p1y = by;
    this.hv0x = vax;
    this.hv0y = vay;
    this.hv1x = vbx;
    this.hv1y = vby;
    const cx = bx - ax;
    const cy = by - ay;
    const chord = Math.hypot(cx, cy);
    const s0 = Math.hypot(vax, vay);
    const s1 = Math.hypot(vbx, vby);
    [this.t0x, this.t0y] = unit(vax, vay, s0, dax, day, cx, cy, chord);
    [this.t1x, this.t1y] = unit(vbx, vby, s1, dbx, dby, cx, cy, chord);
    this.l0 = 0;
    this.r = 0;
    this.theta = 0;
    if (chord < 1e-6) {
      this.kind = SEG_LINE;
      this.length = 0;
      this.setSpeeds(s0, s1);
      return this;
    }
    const along0 = (this.t0x * cx + this.t0y * cy) / chord;
    const along1 = (this.t1x * cx + this.t1y * cy) / chord;
    const lateral0 = Math.abs(this.t0x * cy - this.t0y * cx);
    const lateral1 = Math.abs(this.t1x * cy - this.t1y * cx);
    if (along0 > 0 && along1 > 0 && Math.abs(Math.atan2(this.t0x * this.t1y - this.t0y * this.t1x, this.t0x * this.t1x + this.t0y * this.t1y)) < 0.02 && lateral0 < 0.15 && lateral1 < 0.15) {
      // Parallel tangents along the chord: a straight.
      this.kind = SEG_LINE;
      this.length = chord;
      this.setSpeeds(s0, s1);
      return this;
    }
    const la = lineArc(ax, ay, this.t0x, this.t0y, bx, by, this.t1x, this.t1y);
    const al = lineArc(bx, by, -this.t1x, -this.t1y, ax, ay, -this.t0x, -this.t0y);
    // Rail-like segments are close to their chord; a much longer solution is a loop.
    const fits = (g: LineArc | null): g is LineArc => !!g && g.l0 + g.r * Math.abs(g.theta) <= MAX_STRETCH * chord + 1;
    const pick = fits(la) && fits(al) ? (la.r >= al.r ? la : al) : fits(la) ? la : fits(al) ? al : null;
    if (pick) {
      this.kind = pick === la ? SEG_LINE_ARC : SEG_ARC_LINE;
      this.l0 = pick.l0;
      this.r = pick.r;
      this.theta = pick.theta;
      this.length = pick.l0 + pick.r * Math.abs(pick.theta);
    } else {
      this.kind = SEG_HERMITE;
      this.length = hermiteLength(this, 8);
      if (this.length > MAX_STRETCH * chord + 1) {
        // Tangents that disagree with the chord make the cubic loop: go straight instead.
        this.kind = SEG_LINE;
        this.length = chord;
      }
    }
    this.setSpeeds(s0, s1);
    return this;
  }

  private setSpeeds(s0: number, s1: number): void {
    // Sample speeds come from chords; along a longer arc they scale by arc / chord.
    const chord = Math.hypot(this.p1x - this.p0x, this.p1y - this.p0y);
    if (chord > 1e-6 && this.length > chord) {
      const k = this.length / chord;
      s0 *= k;
      s1 *= k;
    }
    // Monotone timing: end slopes at most 3x the mean speed (Fritsch-Carlson).
    const mean = this.length / this.h;
    this.v0 = Math.min(s0, 3 * mean);
    this.v1 = Math.min(s1, 3 * mean);
  }

  /** Arc length travelled at time t (clamped to the segment), and its rate (tiles/s) in out[1]. */
  arcAt(t: number, out: Float64Array | number[]): void {
    const h = this.h;
    const u = Math.min(1, Math.max(0, (t - this.ta) / 1000 / h));
    const u2 = u * u;
    const u3 = u2 * u;
    const L = this.length;
    out[0] = (-2 * u3 + 3 * u2) * L + (u3 - 2 * u2 + u) * h * this.v0 + (u3 - u2) * h * this.v1;
    out[1] = ((-6 * u2 + 6 * u) * L) / h + (3 * u2 - 4 * u + 1) * this.v0 + (3 * u2 - 2 * u) * this.v1;
    out[0] = Math.min(L, Math.max(0, out[0]));
  }

  /**
   * Position and unit tangent at arc length s into out[0..3]. For the Hermite fallback, s
   * maps linearly to the curve parameter.
   */
  pointAt(s: number, out: Float64Array | number[]): void {
    const L = this.length;
    if (this.kind === SEG_LINE) {
      const f = L > 0 ? Math.min(1, Math.max(0, s / L)) : 0;
      out[0] = this.p0x + (this.p1x - this.p0x) * f;
      out[1] = this.p0y + (this.p1y - this.p0y) * f;
      if (L > 0) {
        out[2] = (this.p1x - this.p0x) / L;
        out[3] = (this.p1y - this.p0y) / L;
      } else {
        out[2] = this.t0x;
        out[3] = this.t0y;
      }
      return;
    }
    if (this.kind === SEG_HERMITE) {
      const u = L > 0 ? Math.min(1, Math.max(0, s / L)) : 0;
      hermite(this.p0x, this.p0y, this.hv0x, this.hv0y, this.p1x, this.p1y, this.hv1x, this.hv1y, this.h, u, tmp);
      const sp = Math.hypot(tmp[2], tmp[3]);
      out[0] = tmp[0];
      out[1] = tmp[1];
      out[2] = sp > 1e-9 ? tmp[2] / sp : this.t0x;
      out[3] = sp > 1e-9 ? tmp[3] / sp : this.t0y;
      return;
    }
    // Line then arc, from (px, py) along (tx, ty); for arc+line the frame starts at p1 going back.
    const rev = this.kind === SEG_ARC_LINE;
    const q = rev ? L - s : s;
    const px = rev ? this.p1x : this.p0x;
    const py = rev ? this.p1y : this.p0y;
    const tx = rev ? -this.t1x : this.t0x;
    const ty = rev ? -this.t1y : this.t0y;
    let x: number;
    let y: number;
    let ux: number;
    let uy: number;
    if (q <= this.l0) {
      x = px + tx * q;
      y = py + ty * q;
      ux = tx;
      uy = ty;
    } else {
      const sg = this.theta > 0 ? 1 : -1;
      const phi = Math.min(Math.abs(this.theta), (q - this.l0) / this.r) * sg;
      // centre: line end + r * left normal (towards the turn)
      const ex = px + tx * this.l0;
      const ey = py + ty * this.l0;
      const nx = -ty * sg;
      const ny = tx * sg;
      const cx = ex + nx * this.r;
      const cy = ey + ny * this.r;
      const c = Math.cos(phi);
      const sn = Math.sin(phi);
      const rx = ex - cx;
      const ry = ey - cy;
      x = cx + rx * c - ry * sn;
      y = cy + rx * sn + ry * c;
      ux = tx * c - ty * sn;
      uy = tx * sn + ty * c;
    }
    out[0] = x;
    out[1] = y;
    out[2] = rev ? -ux : ux;
    out[3] = rev ? -uy : uy;
  }

  /** Position (out[0..1]) and velocity (out[2..3], tiles/s) at time t. */
  at(t: number, out: Float64Array | number[]): void {
    this.arcAt(t, tmp);
    const rate = tmp[1];
    this.pointAt(tmp[0], out);
    out[2] *= rate;
    out[3] *= rate;
  }
}

function unit(vx: number, vy: number, s: number, dx: number, dy: number, cx: number, cy: number, chord: number): [number, number] {
  if (s > 1e-9) {
    return [vx / s, vy / s];
  }
  const d = Math.hypot(dx, dy);
  if (d > 1e-9) {
    // a direction without a sign (orientation of a stopped train): point it along the chord
    const sg = dx * cx + dy * cy < 0 ? -1 : 1;
    return [(sg * dx) / d, (sg * dy) / d];
  }
  return chord > 0 ? [cx / chord, cy / chord] : [1, 0];
}

function hermiteLength(seg: Segment, n: number): number {
  // Hermite fallback length estimate from n chords (pointAt maps s to u via `length`, so
  // evaluate with a provisional length of 1).
  seg.length = 1;
  let len = 0;
  let px = NaN;
  let py = NaN;
  for (let i = 0; i <= n; i++) {
    seg.pointAt(i / n, tmp);
    if (i > 0) {
      len += Math.hypot(tmp[0] - px, tmp[1] - py);
    }
    px = tmp[0];
    py = tmp[1];
  }
  return len;
}
