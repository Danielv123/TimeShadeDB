// The movers layer (MapLayer, web/src/map/layer.ts): trains, vehicles and players of the
// shown surface at the displayed time. In live mode the drawn time trails the data clock
// by the adaptive delay (DelayEstimator), so the next sample is usually known and movers
// interpolate instead of extrapolating.

import type { LayerContext, LayerHit, MapLayer, FrameState } from "../map/layer";
import type { TimeMs } from "../api/types";
import { perfEvent } from "../perf";
import { DelayEstimator } from "./clock";
import { type Projection, drawMovers, hitMover } from "./draw";
import { MoverFeed } from "./feed";
import { MoverView, type Shown } from "./view";

/** Below this zoom (CSS px per tile) trains are drawn as markers without carriages. */
export const BODY_SCALE = 0.05;

export class MoverLayer implements MapLayer {
  readonly id = "movers";
  readonly order = 50;
  readonly view = new MoverView();
  private ctx: LayerContext | null = null;
  private feed: MoverFeed | null = null;
  private delayEst: DelayEstimator | null = null;
  private shown: Shown[] = [];
  /** frame.time of the last draw, to tell a paused history view from playback. */
  private lastTime = NaN;
  /** Timer for the next frame in live mode while nothing moves (wakeAt). */
  private wake: ReturnType<typeof setTimeout> | null = null;

  attach(ctx: LayerContext): void {
    this.detach();
    this.ctx = ctx;
    if (!ctx.surface.movers) {
      return;
    }
    const liveMs = ctx.catalog.live_ms || 5000;
    // Static exports are polled every live_ms, so start from a delay that covers one poll.
    this.delayEst = new DelayEstimator(ctx.catalog.static ? 2 * liveMs : 2000);
    this.feed = new MoverFeed(
      ctx.source,
      ctx.surface.id,
      this.view,
      this.delayEst,
      { changed: () => ctx.requestRedraw() },
      () => ctx.surface.movers?.horizon ?? -Infinity,
      liveMs,
    );
  }

  detach(): void {
    if (this.wake) {
      clearTimeout(this.wake);
      this.wake = null;
    }
    this.feed?.stop();
    this.feed = null;
    this.delayEst = null;
    this.view.clear();
    this.shown = [];
    this.ctx = null;
  }

  setTime(time: TimeMs, live: boolean): void {
    this.feed?.setTime(time, live);
  }

  /** How far (ms) the live display should trail the data clock (MapLayer.delay). */
  delay(): number {
    return this.delayEst?.current ?? 0;
  }

  draw2d(ctx2d: CanvasRenderingContext2D, frame: FrameState): void {
    const feed = this.feed;
    const ctx = this.ctx;
    this.shown = [];
    if (!feed || !ctx || !this.delayEst) {
      return;
    }
    feed.ensure();
    if (!feed.active) {
      // Timelapse faster than 1 h/s: movers are not drawn; check again next frame.
      ctx.requestRedraw();
      return;
    }
    const start = performance.now();
    let t = frame.time;
    if (frame.live) {
      // Whatever the engine's own delay, never draw closer to live than ours.
      t = Math.min(t, ctx.source.now() - this.delayEst.delay(frame.now));
    }
    const shown = this.view.frame(t, feed.horizon(), { bodies: frame.camera.scale >= BODY_SCALE, bounds: frame.visible });
    const n = drawMovers(ctx2d, projection(frame), shown);
    this.shown = shown;
    perfEvent("movers.frame", { movers: n, ms: Math.round((performance.now() - start) * 100) / 100 });
    // Keep animating only while time runs: a paused history view draws the same frame
    // again, so it needs no more frames (the engine asks for one on the next time change).
    // In live mode the delayed display time also has to reach samples that have already
    // arrived (MoverView.nextT): without a frame then, a parked train would only start
    // moving when the next sample arrives.
    const running = frame.live || frame.time !== this.lastTime;
    this.lastTime = frame.time;
    if (running && shown.some(m => m.moving)) {
      ctx.requestRedraw();
    } else if (frame.live && Number.isFinite(this.view.nextT)) {
      this.wakeAt(Math.max(0, this.view.nextT - t));
    }
  }

  /** Requests a frame in `ms` (display time runs at about real time in live mode). */
  private wakeAt(ms: number): void {
    if (this.wake) {
      clearTimeout(this.wake);
    }
    this.wake = setTimeout(() => {
      this.wake = null;
      this.ctx?.requestRedraw();
    }, Math.min(5000, ms + 5));
  }

  hitTest(x: number, y: number, frame: FrameState): LayerHit | null {
    const m = hitMover(projection(frame), this.shown, x, y);
    if (!m) {
      return null;
    }
    const label = m.kind === "t" ? `${m.meta.name} (${m.meta.n ?? m.cars.length} carriages)` : m.meta.name;
    return { layer: this.id, label, key: m.id };
  }
}

export function projection(frame: FrameState): Projection {
  return { cx: frame.camera.x, cy: frame.camera.y, scale: frame.camera.scale, width: frame.viewport.width, height: frame.viewport.height };
}
