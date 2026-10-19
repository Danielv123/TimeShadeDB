// How far the live display trails the data clock, slewed: the target (the largest delay an
// enabled layer reports, or the map's default) jumps when a layer attaches or is turned
// off, but the displayed time must not jump back or forward by seconds. DOM-free.

/** A growing delay slows displayed time by at most this (0.2 = it runs at 0.8x)... */
export const LIVE_DELAY_SLEW = 0.2;
/** ...and a shrinking one speeds it up by at most this (1.05x), as the movers delay does. */
export const LIVE_DELAY_SLEW_DOWN = 0.05;

/** The live delay of a catalog's map before any layer reports one: what the movers layer starts from. */
export function defaultLiveDelay(catalog: { static?: boolean; live_ms?: number }): number {
  // Static exports are polled every live_ms, so the movers layer starts from 2 x live_ms.
  return catalog.static ? 2 * (catalog.live_ms || 5000) : 2000;
}

export class LiveDelay {
  private cur = NaN;
  private at = NaN;

  /** The delay at wall time `perfNow` moving towards `target`; `reset` jumps to it (not live, or a seek to live). */
  next(target: number, perfNow: number, reset = false): number {
    if (reset || Number.isNaN(this.cur)) {
      this.cur = target;
    } else {
      const dt = Math.max(0, perfNow - this.at);
      this.cur += Math.max(-dt * LIVE_DELAY_SLEW_DOWN, Math.min(dt * LIVE_DELAY_SLEW, target - this.cur));
    }
    this.at = perfNow;
    return this.cur;
  }
}
