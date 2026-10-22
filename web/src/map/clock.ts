// The data clock: map-clock milliseconds now, kept from catalog.now and X-TSDB-Now headers
// (docs/API.md 1) plus performance.now(), never Date.now() alone. Ported in spirit from
// illisis-minimap web/view/movers.ts `Clock` (Blue Oak Model License 1.0.0).

import type { TimeMs } from "../api/types";

function perfNow(): number {
  return typeof performance !== "undefined" ? performance.now() : Date.now();
}

/** A server time this far behind the running clock replaces it (server clock reset). */
export const RESET_MS = 120_000;

export class DataClock {
  private anchorMs: TimeMs | null = null;
  private anchorPerf = 0;
  private readonly perf: () => number;

  constructor(perf: () => number = perfNow) {
    this.perf = perf;
  }

  /** Whether any server time has been observed. */
  get synced(): boolean {
    return this.anchorMs !== null;
  }

  /**
   * Records a server "now". Observations behind the running clock are ignored (a cached
   * catalog or a slow response carries an older time), unless they are more than
   * `RESET_MS` behind, which means the server clock really moved back.
   */
  observe(now: TimeMs): void {
    if (!Number.isFinite(now)) {
      return;
    }
    if (this.anchorMs !== null) {
      const cur = this.now();
      if (now < cur && cur - now < RESET_MS) {
        return;
      }
    }
    this.anchorMs = now;
    this.anchorPerf = this.perf();
  }

  /** Map-clock ms now. Falls back to Date.now() before the first observation. */
  now(): TimeMs {
    if (this.anchorMs === null) {
      return Date.now();
    }
    return this.anchorMs + (this.perf() - this.anchorPerf);
  }
}
