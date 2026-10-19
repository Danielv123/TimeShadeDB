// Live timing for movers (PLAN.md §4.8 step 1): the arrival-lag estimate, the adaptive
// display delay and the data horizon. Times are map-clock ms from DataSource.now(),
// which follows the server's clock (catalog.now, X-TSDB-Now), never Date.now().

/** Lower and upper bounds of the adaptive delay. */
export const MIN_DELAY_MS = 500;
export const MAX_DELAY_MS = 30_000;
/** Added to the p99 arrival lag. */
export const DELAY_MARGIN_MS = 500;
/** The delay changes by at most this fraction of elapsed time, so displayed time never jumps. */
export const DELAY_SLEW = 0.2;

/**
 * Collects arrival lags (data-clock time at arrival minus the row's time) of live rows and
 * turns them into a display delay: p99 lag + 0.5 s, clamped, changing smoothly.
 */
export class DelayEstimator {
  private readonly ring: Float64Array;
  private n = 0;
  private next = 0;
  private sorted: Float64Array | null = null;
  private current_: number;
  private lastUpdate: number | null = null;

  /** `initial` is used until lags are observed (e.g. 2 s for the server, 2 x live_ms for static exports). */
  constructor(initial = 2000, size = 1024) {
    this.ring = new Float64Array(size);
    this.current_ = clamp(initial);
  }

  /** Records the lag of one newly arrived row. Negative lags (clock skew) count as 0. */
  observe(lagMs: number): void {
    if (!Number.isFinite(lagMs)) {
      return;
    }
    this.ring[this.next] = Math.max(0, lagMs);
    this.next = (this.next + 1) % this.ring.length;
    this.n = Math.min(this.n + 1, this.ring.length);
    this.sorted = null;
  }

  get samples(): number {
    return this.n;
  }

  /** The current (smoothed) delay, without advancing it. */
  get current(): number {
    return this.current_;
  }

  /** The q-quantile of the observed lags, or NaN without observations. */
  quantile(q: number): number {
    if (this.n === 0) {
      return NaN;
    }
    if (!this.sorted) {
      this.sorted = this.ring.slice(0, this.n).sort();
    }
    return this.sorted[Math.min(this.n - 1, Math.floor(q * (this.n - 1) + 0.5))];
  }

  /** The delay the estimate asks for (p99 lag + margin, clamped), without smoothing. */
  target(): number {
    const p = this.quantile(0.99);
    return Number.isNaN(p) ? this.current_ : clamp(p + DELAY_MARGIN_MS);
  }

  /**
   * The display delay at wall time `perfNow` (performance.now()): moves towards target()
   * at most DELAY_SLEW ms per elapsed ms.
   */
  delay(perfNow: number): number {
    if (this.lastUpdate !== null) {
      const step = Math.max(0, perfNow - this.lastUpdate) * DELAY_SLEW;
      const want = this.target();
      this.current_ += Math.max(-step, Math.min(step, want - this.current_));
    }
    this.lastUpdate = perfNow;
    return this.current_;
  }

  /**
   * The data horizon at data-clock time `now`: every row older than this has (p99)
   * arrived. Movers extrapolate freely before it and at most 1 s past it.
   */
  horizon(now: number): number {
    const p = this.quantile(0.99);
    return now - (Number.isNaN(p) ? this.current_ : p);
  }
}

function clamp(d: number): number {
  return Math.max(MIN_DELAY_MS, Math.min(MAX_DELAY_MS, d));
}

/**
 * A monotone data clock (port of illisis web/view/movers.ts Clock, Blue Oak 1.0.0): the
 * newest server time seen plus elapsed performance time. Used when a DataSource offers no
 * clock of its own (tests, the evaluator).
 */
export class DataClock {
  private ref = -Infinity;
  private seen = 0;
  private readonly perf: () => number;

  constructor(perf: () => number = () => performance.now()) {
    this.perf = perf;
  }

  see(t: number): void {
    if (t > this.now()) {
      this.ref = t;
      this.seen = this.perf();
    }
  }

  now(): number {
    return this.ref + (this.perf() - this.seen);
  }
}
