// Loads overlay intervals per visible z0 tile through DataSource.overlays, with an LRU of
// tiles. Live views refresh their tiles every REFRESH_MS (the route is short-cached).

import type { DataSource, IntervalKind, Interval } from "../api/types";
import { tileAt } from "../api/grid";
import { TileIntervals } from "./intervals";

export const REFRESH_MS = 60_000;
/** Tiles kept in memory. */
export const MAX_TILES = 256;
/** At most this many tiles are requested for one view (zoomed far out, overlays are not drawn). */
export const MAX_VIEW_TILES = 64;

interface Entry {
  data: TileIntervals | null;
  fetched: number;
  pending: boolean;
}

export class OverlayFeed {
  private readonly tiles = new Map<string, Entry>();
  private abort = new AbortController();
  private readonly source: DataSource;
  private readonly ds: string;
  private readonly kinds: IntervalKind[];
  private readonly changed: () => void;
  private readonly clock: () => number;

  constructor(source: DataSource, ds: string, kinds: IntervalKind[], changed: () => void, clock: () => number = () => performance.now()) {
    this.source = source;
    this.ds = ds;
    this.kinds = kinds;
    this.changed = changed;
    this.clock = clock;
  }

  stop(): void {
    this.abort.abort();
    this.abort = new AbortController();
    this.tiles.clear();
  }

  get size(): number {
    return this.tiles.size;
  }

  /**
   * The intervals open at t in the visible world rectangle, fetching missing tiles (and,
   * when `live`, refreshing stale ones). Returns null when the view spans too many tiles.
   */
  query(visible: [number, number, number, number], t: number, live: boolean, kinds: ReadonlySet<IntervalKind>): Interval[] | null {
    const a = tileAt(visible[0], visible[1], 0);
    const b = tileAt(visible[2], visible[3], 0);
    if ((b.x - a.x + 1) * (b.y - a.y + 1) > MAX_VIEW_TILES) {
      return null;
    }
    const out: Interval[] = [];
    const now = this.clock();
    for (let y = a.y; y <= b.y; y++) {
      for (let x = a.x; x <= b.x; x++) {
        const key = `${x},${y}`;
        let e = this.tiles.get(key);
        if (e) {
          // LRU: move to the end
          this.tiles.delete(key);
          this.tiles.set(key, e);
        }
        if (!e || (live && !e.pending && now - e.fetched > REFRESH_MS)) {
          e = e ?? { data: null, fetched: now, pending: false };
          this.tiles.set(key, e);
          this.load(x, y, e);
        }
        e.data?.at(t, kinds, out);
      }
    }
    while (this.tiles.size > MAX_TILES) {
      const first = this.tiles.keys().next().value as string;
      this.tiles.delete(first);
    }
    return out;
  }

  private load(x: number, y: number, e: Entry): void {
    e.pending = true;
    e.fetched = this.clock();
    this.source
      .overlays(this.ds, x, y, { kinds: this.kinds }, this.abort.signal)
      .then(r => {
        if (!e.data || e.data.version !== r.version) {
          e.data = new TileIntervals(r.version, r.intervals);
          this.changed();
        }
      })
      .catch(() => {
        // Missing tile (404) or network error: nothing to draw; retried on the next refresh.
        e.data ??= new TileIntervals(0, []);
      })
      .finally(() => {
        e.pending = false;
      });
  }
}
