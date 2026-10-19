// Loads one surface's movers into a MoverView (API.md 3, 5): in live mode the live payload
// plus SSE `movers` events (or, for static exports, live.bin polled every live_ms); for a
// historical time the day index, the newest keyframe at or before it and the windows from
// there on. A generation counter drops results of a seek that was superseded, and arrival
// lags of live rows feed the DelayEstimator.

import type { DataSource, DayIndex, LiveHandlers, TimeMs } from "../api/types";
import { utcDay } from "../api/grid";
import { decodeMoverWindow } from "../api/wire";
import type { DelayEstimator } from "./clock";
import type { MoverView } from "./view";

/** Window length of closed mover windows (FORMAT.md / core.MoverWindowLen). */
export const WINDOW_MS = 300_000;
/** History mode loads this far past the displayed time, so the next sample is known. */
export const LOOKAHEAD_MS = 60_000;
/** Above this playback speed (data ms per real ms) movers are not loaded (1 h/s). */
export const MAX_SPEED = 3600;
/** A time unchanged for this long (real ms) counts as stopped, whatever the last speed. */
export const QUIET_MS = 250;

export interface FeedEvents {
  /** New data arrived; redraw. */
  changed(): void;
  /** A load failed (shown as a hint; the feed keeps going). */
  error?(err: unknown): void;
}

/**
 * Feeds `view` for surface `sfc`. Call `setTime` with the displayed time on every time
 * change; the feed decides what to fetch.
 */
export class MoverFeed {
  private gen = 0;
  private live = false;
  private unsubscribe: (() => void) | null = null;
  private poll: ReturnType<typeof setInterval> | null = null;
  private abort = new AbortController();
  /** Loaded closed-data range [from, to) in history mode; NaN when nothing is loaded. */
  private from = NaN;
  private to = NaN;
  private loading: Promise<void> | null = null;
  private readonly days = new Map<string, Promise<DayIndex | null>>();
  /** Newest row time seen from live sources (rows newer than this count for lag stats). */
  private newest = -Infinity;
  private lastT = NaN;
  private lastPerf = NaN;
  /** Data ms per real ms of the last time changes (1 when following live). */
  speed = 1;
  private readonly source: DataSource;
  private readonly sfc: string;
  private readonly view: MoverView;
  private readonly delay: DelayEstimator;
  private readonly events: FeedEvents;
  private readonly liveMs: number;
  private readonly perf: () => number;
  private readonly closed: () => number;

  /**
   * `closed` returns the surface's mover horizon (catalog movers.horizon): windows ending
   * at or before it are closed and listed in day indexes; newer rows come from live.bin.
   */
  constructor(
    source: DataSource, sfc: string, view: MoverView, delay: DelayEstimator, events: FeedEvents,
    closed: () => number, liveMs = 5000, perf: () => number = () => performance.now(),
  ) {
    this.closed = closed;
    this.source = source;
    this.sfc = sfc;
    this.view = view;
    this.delay = delay;
    this.events = events;
    this.liveMs = liveMs;
    this.perf = perf;
  }

  /**
   * Whether movers should be loaded and drawn: not while time runs faster than MAX_SPEED
   * (timelapse); a time that stopped changing (the end of a seek) is active again.
   */
  get active(): boolean {
    return this.speed <= MAX_SPEED || this.perf() - this.lastPerf > QUIET_MS;
  }

  /** Called every frame: loads for the last time set if that was skipped while inactive. */
  ensure(): void {
    if (!this.live && !Number.isNaN(this.lastT) && this.active) {
      this.history(this.lastT);
    }
  }

  /**
   * The time up to which loaded data is complete: the live horizon from arrival lags, or
   * the end of the loaded history range.
   */
  horizon(): number {
    if (this.live) {
      return this.delay.horizon(this.source.now());
    }
    return Number.isNaN(this.to) ? -Infinity : this.to;
  }

  setTime(t: TimeMs, live: boolean): void {
    const now = this.perf();
    if (live) {
      this.speed = 1;
    } else if (!Number.isNaN(this.lastT) && now > this.lastPerf && now - this.lastPerf <= QUIET_MS) {
      const s = Math.abs(t - this.lastT) / (now - this.lastPerf);
      this.speed = 0.7 * this.speed + 0.3 * s;
    } else {
      this.speed = 0;
    }
    this.lastT = t;
    this.lastPerf = now;
    if (live !== this.live) {
      this.reset();
      this.live = live;
      if (live) {
        this.startLive();
        return;
      }
    }
    if (live) {
      this.view.trim(t);
      return;
    }
    if (!this.active) {
      return;
    }
    this.history(t);
  }

  stop(): void {
    this.reset();
  }

  private reset(): void {
    this.gen++;
    this.abort.abort();
    this.abort = new AbortController();
    this.unsubscribe?.();
    this.unsubscribe = null;
    if (this.poll !== null) {
      clearInterval(this.poll);
      this.poll = null;
    }
    this.from = NaN;
    this.to = NaN;
    this.loading = null;
    this.newest = -Infinity;
    this.view.clear();
  }

  private startLive(): void {
    const gen = this.gen;
    void this.fetchLive(gen, false);
    if (this.source.subscribe) {
      const handlers: LiveHandlers = {
        hello: () => void this.fetchLive(gen, true),
        movers: e => {
          if (gen !== this.gen || e.sfc !== this.sfc) {
            return;
          }
          const arrived = Math.max(e.now, this.source.now());
          this.view.addLive(e, t => this.see(t, arrived, true));
          this.events.changed();
        },
        error: err => this.events.error?.(err),
      };
      this.unsubscribe = this.source.subscribe({ ds: [], sfc: [this.sfc] }, handlers);
    } else {
      this.poll = setInterval(() => void this.fetchLive(gen, true), this.liveMs);
    }
  }

  /** Fetches live.bin; `count` says whether new rows count for the lag statistics. */
  private async fetchLive(gen: number, count: boolean): Promise<void> {
    try {
      const body = await this.source.moverLive(this.sfc, this.abort.signal);
      if (gen !== this.gen) {
        return;
      }
      const w = decodeMoverWindow(body);
      const arrived = Math.max(w.start, this.source.now());
      this.view.addWindow(w, t => this.see(t, arrived, count));
      this.events.changed();
    } catch (err) {
      if (gen === this.gen && !isAbort(err)) {
        this.events.error?.(err);
      }
    }
  }

  private see(t: number, arrived: number, count: boolean): void {
    if (t > this.newest) {
      if (count && Number.isFinite(this.newest)) {
        this.delay.observe(arrived - t);
      }
      this.newest = t;
    }
  }

  private history(t: TimeMs): void {
    const covered = !Number.isNaN(this.from) && t >= this.from && t < this.to;
    if (covered) {
      this.view.trim(t);
      if (t + LOOKAHEAD_MS > this.to && !this.loading) {
        // Playing forward: append the next window.
        this.loading = this.extend(this.gen, this.to, t + LOOKAHEAD_MS).finally(() => {
          this.loading = null;
        });
      }
      return;
    }
    if (this.loading && !Number.isNaN(this.from) && t >= this.from && t < this.to + WINDOW_MS) {
      return;
    }
    // A seek: start over from the newest keyframe at or before t.
    this.reset();
    const gen = this.gen;
    this.loading = this.seek(gen, t).finally(() => {
      if (gen === this.gen) {
        this.loading = null;
      }
    });
  }

  private day(date: string): Promise<DayIndex | null> {
    let p = this.days.get(date);
    if (!p) {
      p = this.source.moverDay(this.sfc, date, this.abort.signal).catch(() => null);
      this.days.set(date, p);
      // Open days change; forget them after a while.
      void p.then(d => {
        if (!d || !d.final) {
          setTimeout(() => this.days.delete(date), 60_000);
        }
      });
    }
    return p;
  }

  private async seek(gen: number, t: TimeMs): Promise<void> {
    try {
      const today = await this.day(utcDay(t));
      let kf = today ? newestAtOrBefore(today.keyframes, t) : NaN;
      if (Number.isNaN(kf)) {
        const yesterday = await this.day(utcDay(t - 86_400_000));
        kf = yesterday ? newestAtOrBefore(yesterday.keyframes, t) : NaN;
      }
      if (gen !== this.gen) {
        return;
      }
      const start = Number.isNaN(kf) ? t - WINDOW_MS : kf;
      this.from = start;
      this.to = start;
      if (!Number.isNaN(kf)) {
        const body = await this.source.moverKeyframe(this.sfc, kf, this.abort.signal);
        if (gen !== this.gen) {
          return;
        }
        this.view.addWindow(decodeMoverWindow(body));
      }
      await this.extend(gen, start, t + LOOKAHEAD_MS);
    } catch (err) {
      if (gen === this.gen && !isAbort(err)) {
        this.events.error?.(err);
      }
    }
  }

  /**
   * Loads the closed windows overlapping [from, to) (fetched in parallel, added in order),
   * plus the live payload when the range reaches past the closed horizon, and advances `to`.
   */
  private async extend(gen: number, from: number, to: number): Promise<void> {
    const first = Math.floor(from / WINDOW_MS) * WINDOW_MS;
    const horizon = this.closed();
    const listed = new Set<number>();
    for (const d of new Set(rangeDays(first, Math.min(to, horizon)))) {
      const idx = await this.day(d);
      if (gen !== this.gen) {
        return;
      }
      for (const w of idx?.windows ?? []) {
        listed.add(w.start);
      }
    }
    const starts: number[] = [];
    for (let s = first; s < to && s + WINDOW_MS <= horizon; s += WINDOW_MS) {
      if (listed.has(s)) {
        starts.push(s);
      }
    }
    try {
      const bodies = await Promise.all(starts.map(s => this.source.moverWindow(this.sfc, s, this.abort.signal)));
      if (gen !== this.gen) {
        return;
      }
      for (const b of bodies) {
        this.view.addWindow(decodeMoverWindow(b));
      }
      if (to > horizon) {
        const live = decodeMoverWindow(await this.source.moverLive(this.sfc, this.abort.signal));
        if (gen !== this.gen) {
          return;
        }
        this.view.addWindow(live);
        this.to = Math.max(this.to, Math.min(to, live.start));
      } else {
        this.to = Math.max(this.to, to);
      }
      this.events.changed();
    } catch (err) {
      if (gen === this.gen && !isAbort(err)) {
        this.events.error?.(err);
      }
    }
  }
}

function rangeDays(from: number, to: number): string[] {
  const out: string[] = [];
  for (let d = Math.floor(from / 86_400_000) * 86_400_000; d < Math.max(to, from + 1); d += 86_400_000) {
    out.push(utcDay(d));
  }
  return out;
}

function newestAtOrBefore(ts: number[], t: number): number {
  let best = NaN;
  for (const k of ts) {
    if (k <= t && !(k <= best)) {
      best = k;
    }
  }
  return best;
}

function isAbort(err: unknown): boolean {
  return err instanceof Error && err.name === "AbortError";
}
