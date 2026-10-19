// Bench entry for the illisis/minimap public viewer, built from the illisis repo source
// (web/map_view.ts, web/view/*), never from the viewer bundle in the test data. It mounts
// the viewer like web/public.ts does, without the controls, exposes it for the runner,
// and records paint events into window.__tsdbPerf (the v3 perf hook format,
// docs/v3/README.md), so one runner measures both viewers.
//
// "Full" means: every area the frame shows (viewer.shown) has a decoded entry for the
// time shown and no load is in flight; illisis draws areas only from their own data, so
// the first full frame is also the first frame without black areas (no fallback).

import { mount } from "illisis/web/map_view.ts";
import { Clock } from "illisis/web/view/movers.ts";
import { bucketSource } from "illisis/web/view/source.ts";

type PerfEvent = { name: string; t: number; detail?: Record<string, unknown> };
const perf = {
  version: 1,
  firstFullPaint: null as number | null,
  lastFullPaint: null as number | null,
  lastFallbackPaint: null as number | null,
  counters: { tilesRequested: 0, tilesLoaded: 0, tilesAborted: 0, tilesFromCache: 0, bytesFetched: 0, textureBytes: 0, framesDrawn: 0, longTasks: 0 },
  events: [] as PerfEvent[],
  reset() {
    perf.events.length = 0;
    perf.lastFullPaint = null;
    perf.lastFallbackPaint = null;
    perf.counters.framesDrawn = 0;
    perf.counters.longTasks = 0;
  },
};
(window as any).__tsdbPerf = perf;
function event(name: string, detail?: Record<string, unknown>): void {
  const t = performance.now();
  perf.events.push({ name, t, detail });
  if (perf.events.length > 5000) {
    perf.events.shift();
  }
  if (name === "view.full") {
    perf.firstFullPaint ??= t;
    perf.lastFullPaint = t;
  }
  if (name === "view.fallback") {
    perf.lastFallbackPaint = t;
  }
}
try {
  new PerformanceObserver((list) => {
    for (const e of list.getEntries()) {
      perf.counters.longTasks++;
      event("longtask", { ms: e.duration });
    }
  }).observe({ type: "longtask", buffered: true });
} catch {
  // no long task support
}

const params = new URLSearchParams(location.search);
const want = {
  map: params.get("map") ?? undefined,
  surface: params.get("surface") ?? undefined,
  force: params.get("force") ?? undefined,
};
const canvas = document.getElementById("canvas") as HTMLCanvasElement;
const clock = new Clock();
const { viewer } = mount(canvas, bucketSource((t: number) => clock.see(t)), Number(params.get("delay") ?? 16000), clock);
if (params.has("x")) {
  viewer.move({ x: Number(params.get("x")), y: Number(params.get("y") ?? 0), s: Number(params.get("s") ?? 1) });
}
// Select the wanted surface in the same task as the first catalog (before any frame).
viewer.subscribe(() => {
  const sel = viewer.sel;
  if (viewer.catalog && sel && want.map && (sel.map !== want.map || sel.surface !== want.surface)) {
    const s = viewer.catalog.maps[want.map]?.surfaces[want.surface ?? ""];
    if (s) {
      viewer.select({ map: want.map, surface: want.surface!, force: want.force ?? Object.keys(s.forces)[0] });
    } else {
      event("bench.error", { message: `no surface ${want.map}/${want.surface} in the catalog` });
    }
  }
});

// Paint state, checked after every frame the viewer draws.
let wasFull = false;
let lastKey = "";
const v: any = viewer;
const origDraw = v.draw;
v.draw = (x: unknown) => {
  origDraw(x);
  perf.counters.framesDrawn++;
  const shown: { key: string }[] = v.shown;
  const live = v.at === undefined;
  const t = v.time();
  const full = v.catalog !== undefined && shown.length > 0 && shown.every(({ key }) => {
    const e = v.areas.peek(key);
    return e !== undefined && !v.loads.has(key) && e.live === live && (live || e.t === t) && !e.stale;
  });
  const key = shown.map((s) => s.key).join(",") + "|" + (live ? "live" : String(v.at));
  if (full && (!wasFull || key !== lastKey)) {
    event("view.fallback", {});
    event("view.full", { tiles: shown.length });
  }
  wasFull = full;
  lastKey = key;
  perf.counters.textureBytes = v.areas.size * 512 * 512 * 4 * 2; // RGBA entry + its OffscreenCanvas
};

// The runner's control surface (same shape as the v3 window.__tsdbControl proposal).
(window as any).__tsdbControl = {
  ready: () => viewer.catalog !== undefined && viewer.sel !== undefined,
  catalog: () => viewer.catalog,
  selection: () => viewer.sel,
  /** Center (tiles) and scale (tiles per CSS pixel). */
  setView(x: number, y: number, s: number) {
    viewer.move({ x, y, s });
  },
  view: () => ({ ...viewer.view, width: viewer.width, height: viewer.height }),
  /** Show time t (ms), or live with undefined. */
  seek(t?: number) {
    event("time.change", { t: t ?? -1 });
    viewer.seek(t);
  },
  /** Timelapse speed in data ms per browser ms (3600 = 1 h/s); 0 pauses. */
  play(speed: number) {
    viewer.play(speed);
  },
  time: () => viewer.time(),
  range: () => viewer.range(),
  areasHeld: () => v.areas.size,
};
