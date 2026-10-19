// Loads one surface's movers into a MoverView (API.md 3, 5): in live mode the live payload
// plus SSE `movers` events (or, for static exports, live.bin polled every live_ms); for a
// historical time the day index, the newest keyframe at or before it and the windows from
// there on. A generation counter drops results of a seek that was superseded, and arrival
// lags of live rows feed the DelayEstimator. Day indexes are cached across seeks (open
// days and failed fetches only for a while) and are fetched with their own abort signal,
// so a superseded seek cannot leave a day cached as missing.

import type { DataSource, DayIndex, LiveHandlers, TimeMs } from "../api/types";
import { utcDay } from "../api/grid";
import { decodeMoverWindow } from "../api/wire";
import type { DelayEstimator } from "./clock";
import { KEEP_MS, type MoverView } from "./view";

/** Window length of closed mover windows (FORMAT.md / core.MoverWindowLen). */
export const WINDOW_MS = 300_000;
/** History mode loads at least this far past the displayed time, so the next sample is known. */
export const LOOKAHEAD_MS = 60_000;
/**
 * During playback the feed loads ahead by this much real time at the current speed (at
 * most MAX_AHEAD_MS of data), so fast playback does not outrun the network.
 */
export const PREFETCH_REAL_MS = 2_000;
export const MAX_AHEAD_MS = 1_800_000;
/**
 * A forward jump of less than this past the loaded range extends it (keeping what is
 * loaded) instead of seeking again: a seek loads from the hourly keyframe, so extending
 * across less than the keyframe interval never fetches more.
 */
export const FORWARD_MS = 3_600_000;
/** Open (non-final) day indexes are fetched again after this long (ms). */
export const DAY_TTL_MS = 60_000;
/** A day index that failed to load (missing day, network error) is retried after this long. */
export const DAY_RETRY_MS = 10_000;
/** Above this playback speed (data ms per real ms) movers are not loaded (1 h/s). */
export const MAX_SPEED = 3600;
/** History is trimmed at most once per this much data time. */
export const TRIM_EVERY_MS = 10_000;
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
  /** The time of the seek in flight (NaN when none), so redraws do not restart it. */
  private seekT = NaN;
  private readonly days = new Map<string, DayEntry>();
  /** Aborts day index fetches; only stop() uses it (seeks share the cached indexes). */
  private dayAbort = new AbortController();
  /** perf() of the last live.bin fetch made for history (throttled to liveMs). */
  private lastLiveFetch = -Infinity;
  /** Newest row time seen from live sources (rows newer than this count for lag stats). */
  private newest = -Infinity;
  private lastT = NaN;
  private lastPerf = NaN;
  private lastTrim = NaN;
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
      this.trim(t);
      return;
    }
    if (!this.active) {
      return;
    }
    this.history(t);
  }

  stop(): void {
    this.reset();
    this.dayAbort.abort();
    this.dayAbort = new AbortController();
    this.days.clear();
  }

  private trim(t: number): void {
    if (Number.isNaN(this.lastTrim) || Math.abs(t - this.lastTrim) >= TRIM_EVERY_MS) {
      this.lastTrim = t;
      this.view.trim(t);
      if (!this.live && !Number.isNaN(this.from)) {
        // History before t - KEEP_MS is gone now: a seek back past it must load again.
        this.from = Math.max(this.from, t - KEEP_MS);
      }
    }
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
    this.seekT = NaN;
    this.newest = -Infinity;
    this.lastLiveFetch = -Infinity;
    this.view.clear();
  }

  private startLive(): void {
    const gen = this.gen;
    void this.fetchLive(gen, false);
    if (this.source.subscribe) {
      const handlers: LiveHandlers = {
        // A resync after a reconnect: its rows piled up during the outage, so their
        // arrival lags say nothing about the steady-state delay (like the first payload).
        hello: () => void this.fetchLive(gen, false),
        movers: e => {
          if (gen !== this.gen || e.sfc !== this.sfc) {
            return;
          }
          const arrived = Math.max(e.now, this.source.now());
          this.view.addLive(e, (t, prevT) => this.see(t, arrived, true, prevT));
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
      this.view.addWindow(w, (t, prevT) => this.see(t, arrived, count, prevT));
      this.events.changed();
    } catch (err) {
      if (gen === this.gen && !isAbort(err)) {
        this.events.error?.(err);
      }
    }
  }

  private see(t: number, arrived: number, count: boolean, prevT: number): void {
    if (count && Number.isFinite(this.newest) && !Number.isNaN(prevT)) {
      this.delay.observeReady(arrived - prevT);
    }
    if (t > this.newest) {
      if (count && Number.isFinite(this.newest)) {
        this.delay.observe(arrived - t);
      }
      this.newest = t;
    }
  }

  /** How far past the displayed time history is loaded, at the current playback speed. */
  private lookahead(): number {
    return Math.min(MAX_AHEAD_MS, Math.max(LOOKAHEAD_MS, this.speed * PREFETCH_REAL_MS));
  }

  private history(t: TimeMs): void {
    if (!Number.isNaN(this.from) && t >= this.from && t < this.to + FORWARD_MS) {
      // Inside the loaded range, or ahead of it (playback, a short forward jump): keep
      // what is loaded and extend it contiguously from its end.
      this.trim(t);
      const target = t + this.lookahead();
      if (target > this.to && !this.loading && this.liveDue()) {
        const gen = this.gen;
        this.loading = this.extend(gen, this.to, target).finally(() => {
          if (gen === this.gen) {
            this.loading = null;
          }
        });
      }
      return;
    }
    if (this.loading && t === this.seekT) {
      // The same seek again (ensure() runs every frame) while its day index loads.
      return;
    }
    // A seek: start over from the newest keyframe at or before t.
    this.reset();
    const gen = this.gen;
    this.seekT = t;
    this.loading = this.seek(gen, t, t + this.lookahead()).finally(() => {
      if (gen === this.gen) {
        this.loading = null;
        this.seekT = NaN;
      }
    });
  }

  /**
   * Whether the loaded range may be extended now: once it reaches past the closed horizon
   * every extension refetches live.bin, so that happens at most every liveMs.
   */
  private liveDue(): boolean {
    return this.to <= this.closed() || this.perf() - this.lastLiveFetch >= this.liveMs;
  }

  /**
   * The day index for a UTC date, cached: final days for good, open days for DAY_TTL_MS,
   * failures for DAY_RETRY_MS. An aborted fetch (stop) is never cached.
   */
  private day(date: string): Promise<DayIndex | null> {
    const cached = this.days.get(date);
    if (cached && cached.expires > this.perf()) {
      return cached.p;
    }
    const entry: DayEntry = { p: Promise.resolve(null), expires: Infinity };
    entry.p = this.source.moverDay(this.sfc, date, this.dayAbort.signal).then(
      d => {
        if (!d.final) {
          entry.expires = this.perf() + DAY_TTL_MS;
        }
        return d;
      },
      err => {
        if (isAbort(err)) {
          if (this.days.get(date) === entry) {
            this.days.delete(date);
          }
        } else {
          entry.expires = this.perf() + DAY_RETRY_MS;
        }
        return null;
      },
    );
    this.days.set(date, entry);
    return entry.p;
  }

  private async seek(gen: number, t: TimeMs, target: number): Promise<void> {
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
      await this.extend(gen, start, target);
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
    let end = first;
    for (; end < to && end + WINDOW_MS <= horizon; end += WINDOW_MS) {
      if (listed.has(end)) {
        starts.push(end);
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
        this.lastLiveFetch = this.perf();
        const live = decodeMoverWindow(await this.source.moverLive(this.sfc, this.abort.signal));
        if (gen !== this.gen) {
          return;
        }
        this.view.addWindow(live);
        this.to = Math.max(this.to, Math.min(to, live.start));
      } else {
        // Whole windows were loaded (an unlisted window has no rows), so the range is
        // complete to the end of the last one, and the next extension starts after it
        // instead of fetching that window again.
        this.to = Math.max(this.to, end);
      }
      this.events.changed();
    } catch (err) {
      if (gen === this.gen && !isAbort(err)) {
        this.events.error?.(err);
      }
    }
  }
}

interface DayEntry {
  p: Promise<DayIndex | null>;
  /** perf() time after which the index is fetched again (Infinity: final, or in flight). */
  expires: number;
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
