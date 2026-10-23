// window.__tsdbControl (TsdbControl in web/src/api/types.ts): the control surface the
// Playwright benchmark (bench/web) drives, as a thin adapter over MapView. DOM-free so it runs under
// node --test.

import type { CatalogDatastore, TimeMs, TsdbControl } from "../api/types";
import { tileSpan, GRID_ORIGIN } from "../api/grid.ts";
import type { Camera } from "./layer";

/** The part of MapView the adapter uses. */
export interface ControlTarget {
  readonly datastore: CatalogDatastore | null;
  readonly isLive: boolean;
  readonly isPlaying: boolean;
  setCamera(cam: Camera): void;
  setTime(t: TimeMs): void;
  goLive(): void;
  play(speed?: number): void;
  pause(): void;
  range(): [TimeMs, TimeMs];
  displayTime(): TimeMs;
  state(): { camera: Camera };
}

export interface ControlOptions {
  /** CSS size of the map element. */
  size(): { width: number; height: number };
  /** Called by seek, to record the time.change perf event. */
  onSeek?(t: TimeMs | undefined): void;
}

export function createControl(v: ControlTarget, opts: ControlOptions): TsdbControl {
  return {
    ready: () => v.datastore !== null,
    // TsdbControl scales are world tiles per CSS pixel; the camera's are CSS pixels per tile.
    setView: (x, y, s) => v.setCamera({ x, y, scale: 1 / s }),
    view: () => {
      const cam = v.state().camera;
      const { width, height } = opts.size();
      return { x: cam.x, y: cam.y, s: 1 / cam.scale, width, height };
    },
    datastore: () => v.datastore?.id ?? "",
    seek: t => {
      if (t === undefined) {
        v.goLive();
      } else {
        v.setTime(t);
      }
      opts.onSeek?.(t);
    },
    // TsdbControl speeds are data ms per browser ms; MapView's are map ms per real second.
    play: speed => {
      if (speed <= 0) {
        v.pause();
      } else {
        v.play(speed * 1000);
      }
    },
    time: () => v.displayTime(),
    range: () => v.range(),
    surfaceBounds: () => {
      const ds = v.datastore;
      if (!ds) {
        return [0, 0, 0, 0];
      }
      const span = tileSpan(0);
      const [x0, y0, x1, y1] = ds.bounds;
      return [GRID_ORIGIN + x0 * span, GRID_ORIGIN + y0 * span, GRID_ORIGIN + (x1 + 1) * span, GRID_ORIGIN + (y1 + 1) * span];
    },
  };
}
