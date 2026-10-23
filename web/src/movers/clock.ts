// Live timing for movers: the arrival-lag estimate, the adaptive
// display delay and the data horizon. Times are map-clock ms from DataSource.now(),
// which follows the server's clock (catalog.now, X-TSDB-Now), never Date.now().

/** Lower and upper bounds of the adaptive delay. */
export const MIN_DELAY_MS = 500;
export const MAX_DELAY_MS = 30_000;
/** Added to the p99 arrival lag. */
export const DELAY_MARGIN_MS = 500;
/**
 * The delay grows by at most this fraction of elapsed time (displayed time slows to 0.8x)
 * and shrinks by at most DELAY_SLEW_DOWN (it speeds up to 1.05x), so displayed time never
 * jumps and movers never visibly race.
 */
export const DELAY_SLEW = 0.2;
export const DELAY_SLEW_DOWN = 0.05;
/** The target only drops when the estimate is this much below it (no wobble around the p99). */
export const DELAY_HYSTERESIS_MS = 250;

/**
 * Readiness: for a moving mover, the time from a sample until the next one has arrived
 * (when its segment can be interpolated instead of extrapolated). The delay also covers
 * this quantile of it, up to READY_CAP_MS, because extrapolating a mover whose next sample
 * is not known yet is where drawn trains leave the rails and then have to be corrected.
 * Measured on the illisis test data (live model): see web/src/movers/README.md. With rows of
 * the rail path model, until the first readiness is observed the delay is at least the
 * prior of railPrior: that capture samples a moving train at least every 120 ticks at game
 * speed 1 (60 below speed 0.5; capture/README.md) and posts about once a second, so the next
 * sample of a train is that late at worst; with only arrival lags the viewer ran trains off
 * the rails for their first seconds.
 */
export const delayConfig = { readyQuantile: 0.95, readyCapMs: 6000, readyMarginMs: 250, readyPriorMs: 3500 };

/**
 * The readiness prior holds until this many readiness lags were observed: the first ones come
 * from samples a few ticks apart (a first sight right after a load's resync) and say little.
 */
const PRIOR_UNTIL = 32;

/** The rail path capture's longest wait between samples of a moving train (ticks at game speed 1). */
export const RAIL_LIVE_GAP_TICKS = 120;

/**
 * Collects arrival lags (data-clock time at arrival minus the row's time) of live rows and
 * turns them into a display delay: the larger of p99 lag + 0.5 s and the readiness quantile
 * (capped), clamped, changing smoothly.
 */
export class DelayEstimator {
  private readonly ring: Float64Array;
  private n = 0;
  private next = 0;
  private sorted: Float64Array | null = null;
  private readonly ready: Float64Array;
  private readyN = 0;
  private readyNext = 0;
  private readySorted: Float64Array | null = null;
  private current_: number;
  private goal: number;
  private lastUpdate: number | null = null;

  /** The readiness prior once rows of the rail path model were seen (railPrior), else 0. */
  private prior = 0;

  /** `initial` is used until lags are observed (e.g. 2 s for the server, 2 x live_ms for static exports). */
  constructor(initial = 2000, size = 1024) {
    this.ring = new Float64Array(size);
    this.ready = new Float64Array(size);
    this.current_ = clamp(initial);
    this.goal = this.current_;
  }

  /**
   * Records a readiness lag: data-clock time at the arrival of a moving mover's sample minus
   * the time of its previous sample.
   */
  observeReady(lagMs: number): void {
    if (!Number.isFinite(lagMs)) {
      return;
    }
    this.ready[this.readyNext] = Math.max(0, lagMs);
    this.readyNext = (this.readyNext + 1) % this.ready.length;
    this.readyN = Math.min(this.readyN + 1, this.ready.length);
    this.readySorted = null;
  }

  /**
   * Rows of the rail path model (F_RAIL) arrived, from a game running `ticksPerMs` ticks a
   * millisecond (NaN: unknown, taken as 60 a second): until PRIOR_UNTIL readiness lags are
   * observed the delay is at least the capture's longest wait between samples of a moving train in real time
   * plus delayConfig.readyPriorMs - 2 s for posting (3.5 s at game speed 1, the cap at 0.2),
   * at once (displayed time steps back, before any mover has been interpolated).
   */
  railPrior(ticksPerMs = NaN): void {
    const speed = Number.isFinite(ticksPerMs) && ticksPerMs > 0 ? (ticksPerMs * 1000) / 60 : 1;
    const gapMs = (RAIL_LIVE_GAP_TICKS * Math.max(0.5, Math.min(1, speed)) * 1000) / (60 * speed);
    const prior = Math.min(delayConfig.readyCapMs, gapMs + delayConfig.readyPriorMs - 2000);
    if (prior <= this.prior) {
      return;
    }
    this.prior = prior;
    if (this.readyN < PRIOR_UNTIL && this.current_ < prior) {
      this.current_ = clamp(prior);
      this.goal = this.current_;
    }
  }

  /** The q-quantile of the readiness lags, or NaN without observations. */
  readyQuantile(q: number): number {
    if (this.readyN === 0) {
      return NaN;
    }
    if (!this.readySorted) {
      this.readySorted = this.ready.slice(0, this.readyN).sort();
    }
    return this.readySorted[Math.min(this.readyN - 1, Math.floor(q * (this.readyN - 1) + 0.5))];
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

  /**
   * The delay the estimate asks for, without smoothing: max(p99 lag + margin, readiness
   * quantile + margin capped at READY_CAP), clamped. NaN-free: without observations, the
   * current delay.
   */
  estimate(): number {
    const p = this.quantile(0.99);
    if (Number.isNaN(p)) {
      return this.current_;
    }
    let d = p + DELAY_MARGIN_MS;
    const r = this.readyQuantile(delayConfig.readyQuantile);
    if (!Number.isNaN(r)) {
      d = Math.max(d, Math.min(delayConfig.readyCapMs, r + delayConfig.readyMarginMs));
    }
    if (this.prior > 0 && this.readyN < PRIOR_UNTIL) {
      d = Math.max(d, this.prior);
    }
    return clamp(d);
  }

  /** The delay aimed at: estimate(), except that it drops only by more than the hysteresis. */
  target(): number {
    const e = this.estimate();
    if (e > this.goal || e < this.goal - DELAY_HYSTERESIS_MS) {
      this.goal = e;
    }
    return this.goal;
  }

  /**
   * The display delay at wall time `perfNow` (performance.now()): moves towards target()
   * at most DELAY_SLEW (growing) or DELAY_SLEW_DOWN (shrinking) ms per elapsed ms.
   */
  delay(perfNow: number): number {
    if (this.lastUpdate !== null) {
      const dt = Math.max(0, perfNow - this.lastUpdate);
      const want = this.target();
      this.current_ += Math.max(-dt * DELAY_SLEW_DOWN, Math.min(dt * DELAY_SLEW, want - this.current_));
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
