// Curve math for mover rendering (PLAN.md §4.8): cubic Hermite segments in time, the
// orientation helpers, and polyline arc-length lookup. Pure functions, no imports, so
// node --test and workers can load it directly.

/** Factorio orientation (turns, 0 north, 0.25 east) to a unit vector in world tiles (+y south). */
export function orientationVector(o: number): [number, number] {
  const a = o * 2 * Math.PI;
  return [Math.sin(a), -Math.cos(a)];
}

/** Unit vector to Factorio orientation in [0, 1). */
export function vectorOrientation(x: number, y: number): number {
  const o = Math.atan2(x, -y) / (2 * Math.PI);
  return o < 0 ? o + 1 : o;
}

/** Signed orientation difference b - a in turns, wrapped to [-0.5, 0.5). */
export function orientationDelta(a: number, b: number): number {
  let d = (b - a) % 1;
  if (d < -0.5) {
    d += 1;
  } else if (d >= 0.5) {
    d -= 1;
  }
  return d;
}

/**
 * A cubic Hermite segment from point a at time ta to point b at time tb. Velocities are
 * in tiles per second; `h` is the segment length in seconds. Writes the position at
 * u in [0, 1] into out[0..1] and the velocity (tiles/s) into out[2..3].
 */
export function hermite(
  ax: number, ay: number, avx: number, avy: number,
  bx: number, by: number, bvx: number, bvy: number,
  h: number, u: number, out: Float64Array | number[],
): void {
  const u2 = u * u;
  const u3 = u2 * u;
  const h00 = 2 * u3 - 3 * u2 + 1;
  const h10 = u3 - 2 * u2 + u;
  const h01 = -2 * u3 + 3 * u2;
  const h11 = u3 - u2;
  out[0] = h00 * ax + h10 * h * avx + h01 * bx + h11 * h * bvx;
  out[1] = h00 * ay + h10 * h * avy + h01 * by + h11 * h * bvy;
  // d/du, then divided by h for d/dt.
  const d00 = 6 * u2 - 6 * u;
  const d10 = 3 * u2 - 4 * u + 1;
  const d01 = -6 * u2 + 6 * u;
  const d11 = 3 * u2 - 2 * u;
  if (h > 0) {
    out[2] = (d00 * ax + d01 * bx) / h + d10 * avx + d11 * bvx;
    out[3] = (d00 * ay + d01 * by) / h + d10 * avy + d11 * bvy;
  } else {
    out[2] = 0;
    out[3] = 0;
  }
}

/**
 * A polyline with cumulative arc lengths, for placing carriages by distance along it.
 * Points are appended in order (from the head backwards); `at` interpolates linearly.
 */
export class Polyline {
  xs: number[] = [];
  ys: number[] = [];
  /** Cumulative length at each point. */
  ls: number[] = [];

  get length(): number {
    const n = this.ls.length;
    return n === 0 ? 0 : this.ls[n - 1];
  }

  get size(): number {
    return this.xs.length;
  }

  clear(): void {
    this.xs.length = 0;
    this.ys.length = 0;
    this.ls.length = 0;
  }

  /** Appends a point; points closer than `eps` to the last one are skipped. */
  push(x: number, y: number, eps = 1e-6): void {
    const n = this.xs.length;
    if (n === 0) {
      this.xs.push(x);
      this.ys.push(y);
      this.ls.push(0);
      return;
    }
    const d = Math.hypot(x - this.xs[n - 1], y - this.ys[n - 1]);
    if (d <= eps) {
      return;
    }
    this.xs.push(x);
    this.ys.push(y);
    this.ls.push(this.ls[n - 1] + d);
  }

  lastX(): number {
    return this.xs[this.xs.length - 1];
  }

  lastY(): number {
    return this.ys[this.ys.length - 1];
  }

  /**
   * The point at arc length s (clamped to [0, length]) into out[0..1]. `hint` is a point
   * index to start searching from (monotone lookups pass the previous result); the
   * return value is the segment index used.
   */
  at(s: number, out: Float64Array | number[], hint = 0): number {
    const n = this.xs.length;
    if (n === 0) {
      out[0] = NaN;
      out[1] = NaN;
      return 0;
    }
    if (n === 1 || s <= 0) {
      out[0] = this.xs[0];
      out[1] = this.ys[0];
      return 0;
    }
    let i = Math.max(0, Math.min(hint, n - 2));
    while (i > 0 && this.ls[i] > s) {
      i--;
    }
    while (i < n - 2 && this.ls[i + 1] < s) {
      i++;
    }
    const l0 = this.ls[i];
    const l1 = this.ls[i + 1];
    const f = l1 > l0 ? Math.min(1, Math.max(0, (s - l0) / (l1 - l0))) : 0;
    out[0] = this.xs[i] + (this.xs[i + 1] - this.xs[i]) * f;
    out[1] = this.ys[i] + (this.ys[i + 1] - this.ys[i]) * f;
    return i;
  }
}
