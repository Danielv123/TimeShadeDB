// Overlay intervals (tags, recipes, rails; API.md 4) per z0 tile, and which are open at a
// time. DOM-free.

import type { Interval, IntervalKind } from "../api/types";

/** Whether an interval is open at t: started at or before it and not yet ended (end 0 = open). */
export function openAt(iv: Interval, t: number): boolean {
  return iv.start <= t && (iv.end === 0 || iv.end > t);
}

/**
 * One tile's intervals, sorted by start, with a cursor-free query. Versions of one key
 * (a tag edited over time) are separate intervals that meet at a stamp; a closed copy
 * replaces an open one with the same key and start (a file fetched before the close).
 */
export class TileIntervals {
  readonly version: number;
  private readonly items: Interval[];

  constructor(version: number, intervals: Interval[]) {
    this.version = version;
    const byKey = new Map<string, Interval>();
    for (const iv of intervals) {
      const k = `${iv.kind}|${iv.key}|${iv.start}`;
      const old = byKey.get(k);
      if (!old || (old.end === 0 && iv.end !== 0)) {
        byKey.set(k, iv);
      }
    }
    this.items = [...byKey.values()].sort((a, b) => a.start - b.start);
  }

  get size(): number {
    return this.items.length;
  }

  /** Intervals of `kinds` open at t, into out. */
  at(t: number, kinds: ReadonlySet<IntervalKind>, out: Interval[]): Interval[] {
    for (const iv of this.items) {
      if (iv.start > t) {
        break;
      }
      if (kinds.has(iv.kind) && openAt(iv, t)) {
        out.push(iv);
      }
    }
    return out;
  }
}
