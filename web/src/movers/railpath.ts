// Train paths on the rails (docs/RAIL-GEOMETRY.md sections 2, 3 and 8). The capture sends,
// with each train sample, the curved rail pieces and ramps the leading joint entered since the
// sample before (`rp`), and the train's speed (`v`). Rails are straight between those pieces,
// so the pieces of a journey, joined end to end by straight lines, are the exact path its
// leading joint ran along (a "run"). Each sample's leading joint lies on that path at a rail
// distance U; between two samples U follows the timing model (timingU) from their speeds; the
// leading carriage's centre is the midpoint of the joints at U and U - 4, and every carriage
// follows at fixed joint distances (section 3). So curves cost no samples: only the timing does.
//
// Pure math, no imports.

/** Piece kinds of `rp` entries (PROTOCOL.md 3.1). Elevated pieces have the ground ones' shapes. */
export const KIND_CURVE_A = 1;
export const KIND_CURVE_B = 2;
export const KIND_LEGACY_CURVE = 3;
export const KIND_RAMP = 4;

/** A curved rail piece or ramp: the entity's position (whole tiles), 16-way direction and kind. */
export interface Piece {
  x: number;
  y: number;
  dir: number;
  kind: number;
}

/** Rail distance between a carriage's joints (vanilla rolling stock, RAIL-GEOMETRY.md 3). */
export const JOINT = 4;
/** Rail distance between neighbouring carriages' facing joints (vanilla). */
export const CONNECTION = 3;

// Spec section 2: constants of the piece shapes.
const R = 13;
const CURVE_A_LENGTH = 5.132284556;
const CURVE_B_LENGTH = 5.077891568;
const CURVE_B_END_RADIUS = 9 * Math.SQRT2;
const LEGACY_GAUGE = 35 / 64;
const PROJECTION = 0.7071067811865;
const LEGACY_B = (Math.SQRT2 * LEGACY_GAUGE * PROJECTION) / 2;
const LEGACY_R = (3 - LEGACY_B) / (1 - Math.sin(Math.PI / 4));
const LEGACY_SX = LEGACY_R + 1;
const LEGACY_SY = LEGACY_R * Math.sin(Math.PI / 4) + 1 + LEGACY_B;
const LEGACY_STRAIGHT = 8 - LEGACY_SY;
const LEGACY_CURVE_LENGTH = LEGACY_STRAIGHT + LEGACY_GAUGE * PROJECTION + (2 * LEGACY_R * Math.PI) / 8;

// Rail ramp (spec 2.6): straight in plan over 16 tiles; its rail distance includes the height.
const RAMP_HEIGHT = 3;
const RAMP_STEPS = 1024;
function rampHeight(p: number): number {
  const q = Math.max(-0.5, Math.min(0.5, (p - 0.5) * (16 / 14)));
  return (RAMP_HEIGHT * (1 + Math.sin(q * Math.PI))) / 2;
}
const rampTable = (() => {
  const t = new Float64Array(RAMP_STEPS + 1);
  let h0 = rampHeight(0);
  for (let i = 1; i <= RAMP_STEPS; i++) {
    const h = rampHeight(i / RAMP_STEPS);
    t[i] = t[i - 1] + Math.hypot(16 / RAMP_STEPS, (h - h0) / PROJECTION);
    h0 = h;
  }
  return t;
})();
const RAMP_LENGTH = rampTable[RAMP_STEPS];

/** The fraction p of the ramp's plan length at rail distance s (inverse of the length table). */
function rampPlan(s: number): number {
  if (s <= 0) {
    return 0;
  }
  if (s >= RAMP_LENGTH) {
    return 1;
  }
  let lo = 0;
  let hi = RAMP_STEPS;
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1;
    if (rampTable[mid] <= s) {
      lo = mid;
    } else {
      hi = mid;
    }
  }
  return (lo + (s - rampTable[lo]) / (rampTable[hi] - rampTable[lo])) / RAMP_STEPS;
}

/** Rail distance from a piece's front end to its back end. */
export function pieceLength(kind: number): number {
  switch (kind) {
    case KIND_CURVE_A:
      return CURVE_A_LENGTH;
    case KIND_CURVE_B:
      return CURVE_B_LENGTH;
    case KIND_LEGACY_CURVE:
      return LEGACY_CURVE_LENGTH;
    case KIND_RAMP:
      return RAMP_LENGTH;
  }
  return 0;
}

/** The spec's 8-fold placement (2.1): bit 0 mirrors x, bit 1 turns a quarter clockwise, bit 2 a half turn. */
function sym(i8: number, x: number, y: number, out: number[]): void {
  if (i8 & 1) {
    x = -x;
  }
  if (i8 & 2) {
    const t = x;
    x = -y;
    y = t;
  }
  if (i8 & 4) {
    x = -x;
    y = -y;
  }
  out[0] = x;
  out[1] = y;
}

/** The same placement applied to a 16-way direction. */
function symDir(i8: number, d: number): number {
  if (i8 & 1) {
    d = (16 - d) % 16;
  }
  if (i8 & 2) {
    d = (d + 4) % 16;
  }
  if (i8 & 4) {
    d = (d + 8) % 16;
  }
  return d;
}

/** Offset from the entity position of the point at rail distance s from the front end, into out[0..1]. */
export function pieceOffset(kind: number, dir: number, s: number, out: number[]): void {
  const i8 = (dir >> 1) & 7;
  switch (kind) {
    case KIND_CURVE_A: {
      const a = s / R;
      sym(i8, R * (Math.cos(a) - 1), 2 - R * Math.sin(a), out);
      return;
    }
    case KIND_CURVE_B: {
      const a = (CURVE_A_LENGTH + s) / R;
      const r = R + (s / CURVE_B_LENGTH) * (CURVE_B_END_RADIUS - R);
      sym(i8, -11 + r * Math.cos(a), 7 - r * Math.sin(a), out);
      return;
    }
    case KIND_LEGACY_CURVE: {
      if (s < LEGACY_STRAIGHT) {
        sym(i8, 1, 4 - s, out);
      } else {
        const a = (s - LEGACY_STRAIGHT) / LEGACY_R;
        sym(i8, 2 - LEGACY_SX + LEGACY_R * Math.cos(a), LEGACY_SY - 4 - LEGACY_R * Math.sin(a), out);
      }
      return;
    }
    case KIND_RAMP: {
      let x = 0;
      let y = 8 - 16 * rampPlan(s);
      for (let q = 0; q < ((dir >> 2) & 3); q++) {
        const t = x;
        x = -y;
        y = t;
      }
      out[0] = x;
      out[1] = y;
      return;
    }
  }
  out[0] = 0;
  out[1] = 0;
}

/**
 * A piece's two ends as the game reports them (LuaEntity.get_rail_end): front end offset and
 * outward direction, then the back end's, as [fx, fy, fd, bx, by, bd]. Offsets are whole tiles.
 */
export function pieceEnds(kind: number, dir: number): number[] {
  const o = [0, 0];
  let c: number[];
  switch (kind) {
    case KIND_CURVE_A:
      c = [0, 2, 8, -1, -3, 15];
      break;
    case KIND_CURVE_B:
      c = [1, 2, 7, -2, -2, 14];
      break;
    case KIND_LEGACY_CURVE:
      c = [1, 4, 8, -2, -3, 14];
      break;
    case KIND_RAMP: {
      // turned a quarter clockwise per 4 of the direction
      let fx = 0;
      let fy = 8;
      for (let q = 0; q < ((dir >> 2) & 3); q++) {
        const t = fx;
        fx = -fy;
        fy = t;
      }
      const fd = (8 + (dir & 12)) % 16;
      return [fx, fy, fd, -fx, -fy, (fd + 8) % 16];
    }
    default:
      return [0, 0, 0, 0, 0, 8];
  }
  const i8 = (dir >> 1) & 7;
  sym(i8, c[0], c[1], o);
  const fx = o[0];
  const fy = o[1];
  sym(i8, c[3], c[4], o);
  return [fx + 0, fy + 0, symDir(i8, c[2]), o[0] + 0, o[1] + 0, symDir(i8, c[5])];
}

/**
 * Integer step of a 16-way rail direction: even directions are the 8 compass directions, odd
 * ones the half-diagonals (1:2 slopes, 26.565 degrees off an axis; spec 1).
 */
const DIR_STEPS = [
  [0, -1], [1, -2], [1, -1], [2, -1], [1, 0], [2, 1], [1, 1], [1, 2],
  [0, 1], [-1, 2], [-1, 1], [-2, 1], [-1, 0], [-2, -1], [-1, -1], [-1, -2],
];

/** Unit vector of a 16-way rail direction. */
export function dirVector(d: number): [number, number] {
  const [x, y] = DIR_STEPS[((d % 16) + 16) % 16];
  const l = Math.hypot(x, y);
  return [x / l, y / l];
}

/**
 * Whether a rail end at (sx, sy) facing sd (outward) continues the end at (ex, ey) facing ed:
 * they face each other and the first lies straight ahead of the second (equal, or joined by
 * straight rails).
 */
export function connects(ex: number, ey: number, ed: number, sx: number, sy: number, sd: number): boolean {
  if (sd !== (ed + 8) % 16) {
    return false;
  }
  const [dx, dy] = DIR_STEPS[ed];
  const px = sx - ex;
  const py = sy - ey;
  return Math.abs(px * dy - py * dx) < 1e-6 && px * dx + py * dy > -1e-6;
}

/** One element of a chain: a straight line or a piece run one way. */
interface Elem {
  /** Rail distance where the element starts (-Infinity for the lead-in) and its length (Infinity for the lead lines). */
  u: number;
  len: number;
  // a line: the point at rail distance uo and the unit direction of travel
  uo: number;
  x: number;
  y: number;
  ux: number;
  uy: number;
  /** A piece, run from its back end to its front end when rev; null for a line. */
  piece: Piece | null;
  rev: boolean;
  /** A piece's points every PROJ_STEP or so of rail distance, for projection. */
  pts: Float64Array | null;
}

const PROJ_STEP = 0.25;
/** Points this much (tiles) further than the nearest still count as on the chain when they lie nearer the expected place. */
const PROJ_TIE = 0.1;
/** Rail distance a projection may reach outside the range it is given. */
const PROJ_SLACK = 8;
const scratch = [0, 0];
const pt = [0, 0, 0, 0];

function line(u: number, len: number, uo: number, x: number, y: number, ux: number, uy: number): Elem {
  return { u, len, uo, x, y, ux, uy, piece: null, rev: false, pts: null };
}

/**
 * The rail path of one run, parametrised by rail distance u: a lead-in line, pieces joined by
 * straight lines, and a lead-out line. u = 0 is the entry of the first piece; the lead-in runs
 * back from there without end, the lead-out on from the last piece. A chain without pieces is
 * one line through its anchor (u = 0 there) until its first piece arrives, which then becomes
 * u = 0: rail distances of a run are only compared after the chain is built.
 */
export class RailChain {
  private readonly el: Elem[] = [];
  /** The pieces in travel order. */
  readonly pieces: Piece[] = [];
  // the exit end (where the lead-out starts) and its outward direction; -1 while there is no piece
  private exX = 0;
  private exY = 0;
  private exD = -1;
  /** Whether a piece given to the constructor did not connect (the chain ends before it). */
  broken = false;

  /**
   * A chain from the pieces of a run start, rear to leading end, or with none the line through
   * (ax, ay) along (aux, auy): the leading joint and direction of travel of the run's first
   * sample. The first piece runs the way that connects to the second; a lone piece the way
   * whose exit faces the direction of travel.
   */
  constructor(ax: number, ay: number, aux: number, auy: number, pieces: Piece[]) {
    const l = Math.hypot(aux, auy) || 1;
    this.el.push(line(-Infinity, Infinity, 0, ax, ay, aux / l, auy / l));
    if (pieces.length === 0) {
      return;
    }
    const p0 = pieces[0];
    const e = pieceEnds(p0.kind, p0.dir);
    let rev: boolean;
    if (pieces.length > 1) {
      const p1 = pieces[1];
      const e1 = pieceEnds(p1.kind, p1.dir);
      const fits = (bx: number, by: number, bd: number): boolean =>
        connects(p0.x + bx, p0.y + by, bd, p1.x + e1[0], p1.y + e1[1], e1[2]) ||
        connects(p0.x + bx, p0.y + by, bd, p1.x + e1[3], p1.y + e1[4], e1[5]);
      // run back to front when only the front end connects on
      rev = !fits(e[3], e[4], e[5]) && fits(e[0], e[1], e[2]);
    } else {
      const [bx, by] = dirVector(e[5]);
      const [fx, fy] = dirVector(e[2]);
      rev = fx * aux + fy * auy > bx * aux + by * auy;
    }
    this.start(p0, rev);
    for (let i = 1; i < pieces.length; i++) {
      if (!this.append(pieces[i])) {
        this.broken = true;
        break;
      }
    }
  }

  /** Makes p (run back to front when rev) the first piece: the lead-in ends at its entry. */
  private start(p: Piece, rev: boolean): void {
    const e = pieceEnds(p.kind, p.dir);
    const [ox, oy] = dirVector(rev ? e[5] : e[2]);
    this.el.length = 0;
    this.pieces.length = 0;
    // the entry end faces back along the lead-in, so the travel direction is its opposite
    this.el.push(line(-Infinity, Infinity, 0, p.x + (rev ? e[3] : e[0]), p.y + (rev ? e[4] : e[1]), -ox, -oy));
    this.place(p, rev, 0);
  }

  /** The rail distance where the lead-out starts (the end of the last piece; 0 without one). */
  get end(): number {
    const last = this.el[this.el.length - 1];
    if (last.piece) {
      return last.u + last.len;
    }
    return Number.isFinite(last.u) ? last.u : 0;
  }

  /**
   * Appends a piece the leading joint entered: true when it continues the chain (the straight
   * rails between become a line), false when it does not (a piece was missed: the run ends).
   * A piece that does not continue the chain but is already in it is a resend and is ignored
   * (a resync sample lists the pieces under the train, and the next sample the pieces since
   * the sample before the resync); a piece met again on a loop continues the chain.
   */
  append(p: Piece): boolean {
    if (this.appendNew(p)) {
      return true;
    }
    for (let i = this.pieces.length - 1; i >= 0; i--) {
      const q = this.pieces[i];
      if (q.x === p.x && q.y === p.y && q.dir === p.dir && q.kind === p.kind) {
        return true;
      }
    }
    return false;
  }

  private appendNew(p: Piece): boolean {
    const e = pieceEnds(p.kind, p.dir);
    if (this.exD < 0) {
      // the anchor line: the piece's entry must lie on it, ahead, facing back
      const a = this.el[0];
      const onLine = (bx: number, by: number, bd: number): boolean => {
        const px = p.x + bx - a.x;
        const py = p.y + by - a.y;
        const [ox, oy] = dirVector(bd);
        return Math.abs(px * a.uy - py * a.ux) < 0.3 && ox * a.ux + oy * a.uy < -0.9 && px * a.ux + py * a.uy > -8;
      };
      if (onLine(e[0], e[1], e[2])) {
        this.start(p, false);
      } else if (onLine(e[3], e[4], e[5])) {
        this.start(p, true);
      } else {
        return false;
      }
      return true;
    }
    let rev: boolean;
    if (connects(this.exX, this.exY, this.exD, p.x + e[0], p.y + e[1], e[2])) {
      rev = false;
    } else if (connects(this.exX, this.exY, this.exD, p.x + e[3], p.y + e[4], e[5])) {
      rev = true;
    } else {
      return false;
    }
    this.el.pop(); // the lead-out
    let u = this.end;
    const gap = Math.hypot(p.x + (rev ? e[3] : e[0]) - this.exX, p.y + (rev ? e[4] : e[1]) - this.exY);
    if (gap > 1e-9) {
      const [ux, uy] = dirVector(this.exD);
      this.el.push(line(u, gap, u, this.exX, this.exY, ux, uy));
      u += gap;
    }
    this.place(p, rev, u);
    return true;
  }

  /** Adds piece p (run back to front when rev) at rail distance u, then the lead-out after it. */
  private place(p: Piece, rev: boolean, u: number): void {
    const len = pieceLength(p.kind);
    const n = Math.max(2, Math.ceil(len / PROJ_STEP));
    const pts = new Float64Array(2 * (n + 1));
    for (let i = 0; i <= n; i++) {
      const s = (len * i) / n;
      pieceOffset(p.kind, p.dir, rev ? len - s : s, scratch);
      pts[2 * i] = p.x + scratch[0];
      pts[2 * i + 1] = p.y + scratch[1];
    }
    this.el.push({ u, len, uo: u, x: 0, y: 0, ux: 0, uy: 0, piece: p, rev, pts });
    this.pieces.push(p);
    const e = pieceEnds(p.kind, p.dir);
    this.exX = p.x + (rev ? e[0] : e[3]);
    this.exY = p.y + (rev ? e[1] : e[4]);
    this.exD = rev ? e[2] : e[5];
    const [ux, uy] = dirVector(this.exD);
    this.el.push(line(u + len, Infinity, u + len, this.exX, this.exY, ux, uy));
  }

  /** The pieces that reach into rail distances [u0, u1], in order. */
  piecesBetween(u0: number, u1: number): Piece[] {
    const out: Piece[] = [];
    for (const e of this.el) {
      if (e.piece && e.u <= u1 && e.u + e.len >= u0) {
        out.push(e.piece);
      }
    }
    return out;
  }

  /** Index of the element holding rail distance u. */
  private find(u: number): number {
    const el = this.el;
    let lo = 0;
    let hi = el.length - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (el[mid].u <= u) {
        lo = mid;
      } else {
        hi = mid - 1;
      }
    }
    return lo;
  }

  /** The point at rail distance u and the unit tangent in the direction of travel: out = [x, y, tx, ty]. */
  point(u: number, out: number[] | Float64Array): void {
    const e = this.el[this.find(u)];
    if (!e.piece) {
      const d = u - e.uo;
      out[0] = e.x + e.ux * d;
      out[1] = e.y + e.uy * d;
      out[2] = e.ux;
      out[3] = e.uy;
      return;
    }
    const p = e.piece;
    const len = e.len;
    const q = Math.max(0, Math.min(len, u - e.u));
    const s = e.rev ? len - q : q;
    pieceOffset(p.kind, p.dir, s, scratch);
    const x = p.x + scratch[0];
    const y = p.y + scratch[1];
    const h = 1e-4;
    pieceOffset(p.kind, p.dir, Math.min(len, s + h), scratch);
    let tx = scratch[0];
    let ty = scratch[1];
    pieceOffset(p.kind, p.dir, Math.max(0, s - h), scratch);
    tx -= scratch[0];
    ty -= scratch[1];
    const l = Math.hypot(tx, ty) || 1;
    const sg = e.rev ? -1 : 1;
    out[0] = x;
    out[1] = y;
    out[2] = (sg * tx) / l;
    out[3] = (sg * ty) / l;
  }

  /**
   * The rail distance of the chain's point nearest (x, y) with a rail distance between `from`
   * and `to` (each widened by PROJ_SLACK): the samples of a run lie in order along it, and a
   * train that comes back past a place must not be put on its later pass. lastDistance is the
   * distance. With `expect`, a point within PROJ_TIE of the nearest distance that lies closer
   * to the expected rail distance wins (a straight passed twice, or a lead line on the same
   * line as an earlier stretch).
   */
  project(x: number, y: number, from = -Infinity, to = Infinity, expect = NaN): number {
    const el = this.el;
    let best = Infinity;
    let bu = 0;
    const better = (d: number, u: number): boolean =>
      Number.isNaN(expect) ? d < best : d < best - PROJ_TIE || (d <= best + PROJ_TIE && Math.abs(u - expect) < Math.abs(bu - expect));
    const lo = from - PROJ_SLACK;
    const hi = to + PROJ_SLACK;
    for (let i = Number.isFinite(lo) ? this.find(lo) : 0; i < el.length && el[i].u <= hi; i++) {
      const e = el[i];
      if (!e.piece) {
        let d = e.uo + (x - e.x) * e.ux + (y - e.y) * e.uy;
        d = Math.max(e.u, lo, Math.min(i + 1 < el.length ? el[i + 1].u : Infinity, hi, d));
        const px = e.x + e.ux * (d - e.uo);
        const py = e.y + e.uy * (d - e.uo);
        const dist = Math.hypot(x - px, y - py);
        if (better(dist, d)) {
          best = Math.min(best, dist);
          bu = d;
        }
        continue;
      }
      const pts = e.pts!;
      const n = pts.length / 2 - 1;
      let bi = 0;
      let bd = Infinity;
      for (let k = 0; k <= n; k++) {
        const d = (pts[2 * k] - x) ** 2 + (pts[2 * k + 1] - y) ** 2;
        if (d < bd) {
          bd = d;
          bi = k;
        }
      }
      if (Math.sqrt(bd) > best + PROJ_STEP + PROJ_TIE) {
        continue;
      }
      let a = (e.len * Math.max(0, bi - 1)) / n;
      let b = (e.len * Math.min(n, bi + 1)) / n;
      const f = (q: number): number => {
        this.point(e.u + q, pt);
        return (pt[0] - x) ** 2 + (pt[1] - y) ** 2;
      };
      for (let it = 0; it < 24; it++) {
        const m1 = a + (b - a) * 0.381966;
        const m2 = a + (b - a) * 0.618034;
        if (f(m1) < f(m2)) {
          b = m2;
        } else {
          a = m1;
        }
      }
      const q = Math.max(lo - e.u, Math.min(hi - e.u, (a + b) / 2));
      const d = Math.sqrt(f(q));
      if (better(d, e.u + q)) {
        best = Math.min(best, d);
        bu = e.u + q;
      }
    }
    this.lastDistance = Number.isNaN(expect) ? best : this.distanceAt(x, y, bu);
    return bu;
  }

  /** Distance from the point to the chain at the last project(). */
  lastDistance = 0;

  private distanceAt(x: number, y: number, u: number): number {
    this.point(u, pt);
    return Math.hypot(pt[0] - x, pt[1] - y);
  }

  /**
   * The carriage whose front joint is at rail distance u (RAIL-GEOMETRY.md 3): its centre, the
   * midpoint of the joints at u and u - JOINT, and the unit chord from the rear joint to the
   * front one: out = [x, y, cx, cy].
   */
  carriage(u: number, out: number[] | Float64Array): void {
    this.point(u, pt);
    const fx = pt[0];
    const fy = pt[1];
    this.point(u - JOINT, pt);
    const dx = fx - pt[0];
    const dy = fy - pt[1];
    const l = Math.hypot(dx, dy) || 1;
    out[0] = (fx + pt[0]) / 2;
    out[1] = (fy + pt[1]) / 2;
    out[2] = dx / l;
    out[3] = dy / l;
  }
}

/**
 * Where a train's leading joint is between two samples (RAIL-GEOMETRY.md 8): the rail
 * distance at tau ticks after the first sample, and its rate (tiles a tick), into out[0..1].
 * The samples are T ticks apart, L rail distance apart, with speeds va and vb (tiles a tick).
 *
 * - From rest (va = 0, vb > 0) when uniform acceleration from rest to vb covers L in less
 *   than T: it stands until then (a train is found moving only at the next scan).
 * - To rest (vb = 0, va > 0) when uniform braking from va covers L in less than T: it stops
 *   early and stands (a stop is read at the next read).
 * - Otherwise a cubic Hermite in time with the speeds as end slopes, held within [0, L].
 */
export function timingU(L: number, T: number, va: number, vb: number, tau: number, out: number[] | Float64Array): void {
  if (!(T > 0)) {
    out[0] = L;
    out[1] = 0;
    return;
  }
  const t = Math.max(0, Math.min(T, tau));
  if (L > 0) {
    if (va <= 0 && vb > 0 && (2 * L) / vb < T) {
      const tm = (2 * L) / vb;
      const s = t - (T - tm);
      out[0] = s <= 0 ? 0 : (vb * s * s) / (2 * tm);
      out[1] = s <= 0 ? 0 : (vb * s) / tm;
      return;
    }
    if (vb <= 0 && va > 0 && (2 * L) / va < T) {
      const tm = (2 * L) / va;
      const s = Math.min(t, tm);
      out[0] = va * s - (va * s * s) / (2 * tm);
      out[1] = va - (va * s) / tm;
      return;
    }
  }
  const f = t / T;
  const f2 = f * f;
  const f3 = f2 * f;
  const u = (f3 - 2 * f2 + f) * T * va + (-2 * f3 + 3 * f2) * L + (f3 - f2) * T * vb;
  const rate = (3 * f2 - 4 * f + 1) * va + ((-6 * f2 + 6 * f) * L) / T + (3 * f2 - 2 * f) * vb;
  const lo = Math.min(0, L);
  const hi = Math.max(0, L);
  out[0] = Math.max(lo, Math.min(hi, u));
  out[1] = u < lo || u > hi ? 0 : rate;
}
