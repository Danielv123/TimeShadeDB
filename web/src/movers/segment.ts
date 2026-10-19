// The path between two consecutive samples (PLAN.md §4.8 step 2, refined). Rails are lines
// and arcs, and a sample pair often spans a long straight that ends just inside a curve:
// a cubic cannot be straight for 60 tiles and then turn, so with accurate end tangents it
// bulges off the straight. Where the end points and tangents allow it, the segment is
// therefore a straight line followed by a circular arc (or an arc followed by a line);
// otherwise it falls back to a cubic Hermite curve. Motion along the geometry is timed by a
// 1D cubic Hermite of arc length with the samples' speeds as end slopes.
//
// The fallback curve is geometric: its end tangents have the length that makes it a circular
// arc when the turn is symmetric (the chord's length when straight), not v x dt
// (timing is the 1D Hermite's job), and it is walked by arc length through a small length
// table. With v x dt tangents a braking train's curve overshoots its end point and comes
// back, and walking it by curve parameter races where the parameter speed is high: both
// were measured as frame-to-frame jumps on the test data.
//
// Pure math, no imports besides hermite.ts.

import { hermite } from "./hermite";

/** Kinds of segment geometry. */
export const SEG_LINE = 0;
export const SEG_LINE_ARC = 1;
export const SEG_ARC_LINE = 2;
export const SEG_HERMITE = 3;
/**
 * Trains only, over gaps of RAIL_MIN_CHORD or more: rail-shaped pieces (lines and arcs of
 * a rail-like radius). Line, arc, line when both ends lie in rail directions (a curve
 * between two straights); line, arc, line, arc (or arc, line, arc, line) through a middle
 * straight in a rail direction when the end tangents make an S, where neither line+arc
 * nor arc+line fits and a cubic stays close to the chord, cutting the corners by tiles.
 */
export const SEG_PATH = 4;

/** Arcs tighter than this are not rail-like; such segments use the Hermite fallback. */
const MIN_RADIUS = 3;
/** Segments longer than this times their chord (plus a tile) are rejected as loops. */
const MAX_STRETCH = 1.5;
/** Tangents within this angle (radians) count as parallel. */
const PARALLEL = 1e-3;
/** Sub-chords of the fallback curve's arc-length table. */
const TABLE = 16;
/** Highest timing rate along a segment, as a multiple of the fastest of its end speeds and its mean speed. */
const SPEED_CAP = 1.25;

/** Arc radius (tiles) of the two-curve rail path (SEG_PATH): about a Factorio 2.0 rail curve. */
const RAIL_RADIUS = 13;
/** Radii tried after RAIL_RADIUS when the two-curve path does not fit with it. */
const RAIL_RADII = [RAIL_RADIUS, 9, 20, 30];
/** Shortest chord (tiles) a two-curve rail path is tried for. */
const RAIL_MIN_CHORD = 30;
/** Smallest angle (radians) between each end tangent and the chord for the two-curve rail path. */
const RAIL_MIN_SKEW = (4 * Math.PI) / 180;
/** Smallest turn (radians) of the line, arc, line rail path. */
const RAIL_MIN_TURN = (10 * Math.PI) / 180;
/** A heading this close (radians) to one of the 16 rail directions lies on a straight. */
const RAIL_DIR_TOLERANCE = (2 * Math.PI) / 180;
/** A two-curve rail path is at most this times its chord (plus a tile). */
const RAIL_STRETCH = 1.2;
/** The 16 rail directions (radians): 4 per quadrant, the 1:2 slopes included. */
const RAIL_DIRS: number[] = [];
for (let q = 0; q < 4; q++) {
  for (const o of [0, Math.atan(0.5), Math.PI / 4, Math.atan(2)]) {
    RAIL_DIRS.push((q * Math.PI) / 2 + o);
  }
}

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

function wrapAngle(a: number): number {
  return Math.atan2(Math.sin(a), Math.cos(a));
}

/** Whether heading th (radians) is within RAIL_DIR_TOLERANCE of a rail direction. */
function onRailDir(th: number): boolean {
  for (const d of RAIL_DIRS) {
    if (Math.abs(wrapAngle(th - d)) <= RAIL_DIR_TOLERANCE) {
      return true;
    }
  }
  return false;
}

/**
 * The pieces of a two-curve rail path from (p0, heading th0) to (p1, heading th1), into
 * `out` (4 pieces x [length, curvature]); returns the total length, or -1 when none fits.
 * startLine: line, arc, line, arc (else arc, line, arc, line). Each arc has radius r and
 * turns between an end heading and a middle straight in a rail direction; the two line
 * lengths are solved from the end point. Of the fitting middle directions the shortest
 * path wins.
 */
function railPath(p0x: number, p0y: number, th0: number, p1x: number, p1y: number, th1: number, r: number, startLine: boolean, maxLen: number, out: Float64Array): number {
  let best = -1;
  const cx = p1x - p0x;
  const cy = p1y - p0y;
  for (const m of RAIL_DIRS) {
    const ta = wrapAngle(m - th0);
    const tb = wrapAngle(th1 - m);
    if (ta * tb >= 0 || Math.abs(ta) < 0.03 || Math.abs(tb) < 0.03 || Math.abs(ta) > Math.PI / 2 || Math.abs(tb) > Math.PI / 2) {
      continue;
    }
    const ka = (ta > 0 ? 1 : -1) / r;
    const kb = (tb > 0 ? 1 : -1) / r;
    // Displacements of the two arcs.
    const d1x = (Math.sin(m) - Math.sin(th0)) / ka;
    const d1y = (Math.cos(th0) - Math.cos(m)) / ka;
    const d2x = (Math.sin(th1) - Math.sin(m)) / kb;
    const d2y = (Math.cos(m) - Math.cos(th1)) / kb;
    const rx = cx - d1x - d2x;
    const ry = cy - d1y - d2y;
    // Lines: along th0 then m (startLine), or along m then th1.
    const ux = Math.cos(startLine ? th0 : m);
    const uy = Math.sin(startLine ? th0 : m);
    const wx = Math.cos(startLine ? m : th1);
    const wy = Math.sin(startLine ? m : th1);
    const det = ux * wy - uy * wx;
    if (Math.abs(det) < 0.05) {
      continue;
    }
    const l1 = (rx * wy - ry * wx) / det;
    const l2 = (ux * ry - uy * rx) / det;
    if (l1 < 0 || l2 < 0) {
      continue;
    }
    const total = l1 + l2 + r * (Math.abs(ta) + Math.abs(tb));
    if (total > maxLen || (best >= 0 && total >= best)) {
      continue;
    }
    best = total;
    if (startLine) {
      out.set([l1, 0, r * Math.abs(ta), ka, l2, 0, r * Math.abs(tb), kb]);
    } else {
      out.set([r * Math.abs(ta), ka, l1, 0, r * Math.abs(tb), kb, l2, 0]);
    }
  }
  return best;
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
  /** Weight of the cubic timing against constant speed (1: pure cubic). */
  private lam = 1;
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
  // Hermite fallback: end tangents as velocities over h (so hermite() sees chord-length
  // tangents), and the cumulative arc length at u = i / TABLE.
  private hv0x = 0;
  private hv0y = 0;
  private hv1x = 0;
  private hv1y = 0;
  private readonly cum = new Float64Array(TABLE + 1);
  // Two-curve rail path: 4 pieces x [length, curvature (0: line)], and each piece's start
  // point and heading.
  private readonly pieces = new Float64Array(8);
  private readonly pstart = new Float64Array(12);

  /**
   * Builds the segment. (vax, vay) and (vbx, vby) are the samples' velocities (tiles/s);
   * (dax, day) and (dbx, dby) are unit directions to use where a velocity is zero (e.g.
   * from the orientation), or (0, 0) to use the chord. `rail` (trains) allows the
   * two-curve rail path; k0 and k1 are the curvatures (1/tiles, magnitudes) the mover was
   * turning at just before a and just after b (NaN: unknown), which choose whether that
   * path starts or ends on a straight.
   */
  build(
    ta: number, ax: number, ay: number, vax: number, vay: number, dax: number, day: number,
    tb: number, bx: number, by: number, vbx: number, vby: number, dbx: number, dby: number,
    rail = false, k0 = NaN, k1 = NaN,
  ): this {
    this.ta = ta;
    this.tb = tb;
    this.h = Math.max(1e-3, (tb - ta) / 1000);
    this.p0x = ax;
    this.p0y = ay;
    this.p1x = bx;
    this.p1y = by;
    const cx = bx - ax;
    const cy = by - ay;
    const chord = Math.hypot(cx, cy);
    let s0 = Math.hypot(vax, vay);
    let s1 = Math.hypot(vbx, vby);
    [this.t0x, this.t0y] = unit(vax, vay, s0, dax, day, cx, cy, chord);
    [this.t1x, this.t1y] = unit(vbx, vby, s1, dbx, dby, cx, cy, chord);
    if (chord >= 1e-6) {
      // A tangent against the chord means the mover reversed at that end (or a sample
      // glitched backwards): a curve honouring it hooks past the end and comes back,
      // racing ahead of the train. Use the chord direction there, at rest.
      if (this.t0x * cx + this.t0y * cy < 0) {
        [this.t0x, this.t0y, s0] = [cx / chord, cy / chord, 0];
      }
      if (this.t1x * cx + this.t1y * cy < 0) {
        [this.t1x, this.t1y, s1] = [cx / chord, cy / chord, 0];
      }
    }
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
    if (rail && chord >= RAIL_MIN_CHORD && this.buildLineArcLine(chord)) {
      this.kind = SEG_PATH;
    } else if (pick) {
      this.kind = pick === la ? SEG_LINE_ARC : SEG_ARC_LINE;
      this.l0 = pick.l0;
      this.r = pick.r;
      this.theta = pick.theta;
      this.length = pick.l0 + pick.r * Math.abs(pick.theta);
    } else if (rail && chord >= RAIL_MIN_CHORD && this.isS(cx, cy) && this.buildRailPath(chord, k0, k1)) {
      this.kind = SEG_PATH;
    } else {
      this.kind = SEG_HERMITE;
      // Tangent length of the cubic closest to a circular arc turning by the angle between
      // the end tangents: 2 c tan(theta/4) / sin(theta/2), the chord c when straight.
      const turn = Math.min(0.85 * Math.PI, Math.abs(Math.atan2(this.t0x * this.t1y - this.t0y * this.t1x, this.t0x * this.t1x + this.t0y * this.t1y)));
      const m = turn > 1e-6 ? (2 * chord * Math.tan(turn / 4)) / Math.sin(turn / 2) : chord;
      this.hv0x = (this.t0x * m) / this.h;
      this.hv0y = (this.t0y * m) / this.h;
      this.hv1x = (this.t1x * m) / this.h;
      this.hv1y = (this.t1y * m) / this.h;
      this.length = this.buildTable();
      if (this.length > MAX_STRETCH * chord + 1) {
        // Tangents that disagree with the chord make the cubic loop: go straight instead.
        this.kind = SEG_LINE;
        this.length = chord;
      }
    }
    this.setSpeeds(s0, s1);
    return this;
  }

  /**
   * Whether the end tangents make an S: both turned to the same side of the chord, by at
   * least RAIL_MIN_SKEW. Line+arc and arc+line cannot turn both ways, and the cubic stays
   * within about a tile of the chord.
   */
  private isS(cx: number, cy: number): boolean {
    const c = Math.atan2(cy, cx);
    const a0 = wrapAngle(Math.atan2(this.t0y, this.t0x) - c);
    const a1 = wrapAngle(Math.atan2(this.t1y, this.t1x) - c);
    return a0 * a1 > 0 && Math.abs(a0) >= RAIL_MIN_SKEW && Math.abs(a1) >= RAIL_MIN_SKEW;
  }

  /**
   * Line, arc of RAIL_RADIUS, line (SEG_PATH with an empty fourth piece) when both end
   * tangents lie in rail directions and turn by at least RAIL_MIN_TURN: a long gap from one
   * straight round a curve onto another. Line+arc would stretch the curve over the whole
   * gap instead of keeping the straights straight.
   */
  private buildLineArcLine(chord: number): boolean {
    const th0 = Math.atan2(this.t0y, this.t0x);
    const th1 = Math.atan2(this.t1y, this.t1x);
    const turn = wrapAngle(th1 - th0);
    if (Math.abs(turn) < RAIL_MIN_TURN || Math.abs(turn) > Math.PI / 2 || !onRailDir(th0) || !onRailDir(th1)) {
      return false;
    }
    const k = (turn > 0 ? 1 : -1) / RAIL_RADIUS;
    const dx = (Math.sin(th1) - Math.sin(th0)) / k;
    const dy = (Math.cos(th0) - Math.cos(th1)) / k;
    const rx = this.p1x - this.p0x - dx;
    const ry = this.p1y - this.p0y - dy;
    const det = Math.cos(th0) * Math.sin(th1) - Math.sin(th0) * Math.cos(th1);
    const l1 = (rx * Math.sin(th1) - ry * Math.cos(th1)) / det;
    const l2 = (Math.cos(th0) * ry - Math.sin(th0) * rx) / det;
    const len = l1 + l2 + RAIL_RADIUS * Math.abs(turn);
    if (!(l1 >= 0 && l2 >= 0) || len > MAX_STRETCH * chord + 1) {
      return false;
    }
    this.pieces.set([l1, 0, RAIL_RADIUS * Math.abs(turn), k, l2, 0, 0, 0]);
    this.setPieces(th0, len);
    return true;
  }

  /** Each piece's start point and heading from p0 at heading th0; sets the length. */
  private setPieces(th0: number, len: number): void {
    this.length = len;
    let x = this.p0x;
    let y = this.p0y;
    let th = th0;
    for (let i = 0; i < 4; i++) {
      const l = this.pieces[2 * i];
      const k = this.pieces[2 * i + 1];
      this.pstart[3 * i] = x;
      this.pstart[3 * i + 1] = y;
      this.pstart[3 * i + 2] = th;
      if (k === 0) {
        x += Math.cos(th) * l;
        y += Math.sin(th) * l;
      } else {
        const th2 = th + k * l;
        x += (Math.sin(th2) - Math.sin(th)) / k;
        y += (Math.cos(th) - Math.cos(th2)) / k;
        th = th2;
      }
    }
  }

  /** Fits the two-curve rail path (SEG_PATH): sets the pieces and the length. */
  private buildRailPath(chord: number, k0: number, k1: number): boolean {
    const th0 = Math.atan2(this.t0y, this.t0x);
    const th1 = Math.atan2(this.t1y, this.t1x);
    // Start on the straight unless the mover turned harder before a than after b.
    const first = !(k0 > k1);
    const maxLen = RAIL_STRETCH * chord + 1;
    for (const r of RAIL_RADII) {
      for (const startLine of [first, !first]) {
        const len = railPath(this.p0x, this.p0y, th0, this.p1x, this.p1y, th1, r, startLine, maxLen, this.pieces);
        if (len <= 0) {
          continue;
        }
        this.setPieces(th0, len);
        return true;
      }
    }
    return false;
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
    // Bounded speed: end speeds far below the mean (a stop row, or a neighbour that is
    // inconsistent with this chord) make the cubic race in the middle, up to 1.5x the mean
    // and beyond, faster than the train ever went. Blend towards constant speed so the
    // peak stays within SPEED_CAP x the fastest of the end speeds and the mean.
    this.lam = 1;
    const peak = this.peakSpeed();
    const cap = SPEED_CAP * Math.max(this.v0, this.v1, mean);
    if (peak > cap && peak > mean) {
      this.lam = Math.max(0, (cap - mean) / (peak - mean));
    }
  }

  /** The cubic timing's highest rate (tiles/s) over the segment. */
  private peakSpeed(): number {
    const L = this.length;
    const h = this.h;
    // rate(u) = a u^2 + b u + c
    const a = (-6 * L) / h + 3 * this.v0 + 3 * this.v1;
    const b = (6 * L) / h - 4 * this.v0 - 2 * this.v1;
    const c = this.v0;
    let best = Math.max(c, a + b + c);
    if (a < 0) {
      const u = -b / (2 * a);
      if (u > 0 && u < 1) {
        best = Math.max(best, a * u * u + b * u + c);
      }
    }
    return best;
  }

  /**
   * Signed curvature (1/tiles, positive turning counterclockwise) at the segment's end: the
   * arc's for line+arc, 0 for a line or arc+line (the turn is behind it), and the mean turn
   * per tile for the Hermite fallback.
   */
  endCurvature(): number {
    if (this.kind === SEG_LINE_ARC) {
      return this.theta > 0 ? 1 / this.r : -1 / this.r;
    }
    if (this.kind === SEG_PATH) {
      return this.pieces[7];
    }
    if (this.kind === SEG_HERMITE && this.length > 1e-6) {
      return Math.atan2(this.t0x * this.t1y - this.t0y * this.t1x, this.t0x * this.t1x + this.t0y * this.t1y) / this.length;
    }
    return 0;
  }

  /** Arc length travelled at time t (clamped to the segment), and its rate (tiles/s) in out[1]. */
  arcAt(t: number, out: Float64Array | number[]): void {
    const h = this.h;
    const u = Math.min(1, Math.max(0, (t - this.ta) / 1000 / h));
    const u2 = u * u;
    const u3 = u2 * u;
    const L = this.length;
    const lam = this.lam;
    out[0] = lam * ((-2 * u3 + 3 * u2) * L + (u3 - 2 * u2 + u) * h * this.v0 + (u3 - u2) * h * this.v1) + (1 - lam) * L * u;
    out[1] = lam * (((-6 * u2 + 6 * u) * L) / h + (3 * u2 - 4 * u + 1) * this.v0 + (3 * u2 - 2 * u) * this.v1) + ((1 - lam) * L) / h;
    out[0] = Math.min(L, Math.max(0, out[0]));
  }

  /**
   * Position and unit tangent at arc length s into out[0..3]. For the Hermite fallback, s
   * maps to the curve parameter through the arc-length table (paramAt).
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
    if (this.kind === SEG_PATH) {
      let q = Math.min(L, Math.max(0, s));
      let i = 0;
      while (i < 3 && q > this.pieces[2 * i]) {
        q -= this.pieces[2 * i];
        i++;
      }
      q = Math.min(q, this.pieces[2 * i]);
      const k = this.pieces[2 * i + 1];
      const x0 = this.pstart[3 * i];
      const y0 = this.pstart[3 * i + 1];
      const th = this.pstart[3 * i + 2];
      if (k === 0) {
        out[0] = x0 + Math.cos(th) * q;
        out[1] = y0 + Math.sin(th) * q;
        out[2] = Math.cos(th);
        out[3] = Math.sin(th);
      } else {
        const th2 = th + k * q;
        out[0] = x0 + (Math.sin(th2) - Math.sin(th)) / k;
        out[1] = y0 + (Math.cos(th) - Math.cos(th2)) / k;
        out[2] = Math.cos(th2);
        out[3] = Math.sin(th2);
      }
      return;
    }
    if (this.kind === SEG_HERMITE) {
      const u = this.paramAt(s);
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

  /** Fills the fallback curve's arc-length table; returns its length. */
  private buildTable(): number {
    const cum = this.cum;
    cum[0] = 0;
    let px = this.p0x;
    let py = this.p0y;
    for (let i = 1; i <= TABLE; i++) {
      hermite(this.p0x, this.p0y, this.hv0x, this.hv0y, this.p1x, this.p1y, this.hv1x, this.hv1y, this.h, i / TABLE, tmp);
      cum[i] = cum[i - 1] + Math.hypot(tmp[0] - px, tmp[1] - py);
      px = tmp[0];
      py = tmp[1];
    }
    return cum[TABLE];
  }

  /** The fallback curve's parameter u at arc length s (linear within a table step). */
  private paramAt(s: number): number {
    const cum = this.cum;
    if (!(s > 0)) {
      return 0;
    }
    if (s >= cum[TABLE]) {
      return 1;
    }
    let lo = 0;
    let hi = TABLE;
    while (hi - lo > 1) {
      const mid = (lo + hi) >> 1;
      if (cum[mid] <= s) {
        lo = mid;
      } else {
        hi = mid;
      }
    }
    const span = cum[hi] - cum[lo];
    return (lo + (span > 0 ? (s - cum[lo]) / span : 0)) / TABLE;
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
