// window.__tsdbPerf: the viewer's perf hooks (docs/README.md "Viewer hooks"). The map
// engine and the layers record into it; the client benchmark (bench/web) reads it.
// Recording is cheap (a ring buffer push), so it stays on in production builds.

import type { PerfCounters, PerfEvent, PerfEventName, TsdbPerf } from "./api/types";

const MAX_EVENTS = 5000;

function zeroCounters(): PerfCounters {
  return {
    tilesRequested: 0,
    tilesLoaded: 0,
    tilesAborted: 0,
    tilesFromCache: 0,
    bytesFetched: 0,
    textureBytes: 0,
    framesDrawn: 0,
    longTasks: 0,
  };
}

function now(): number {
  return typeof performance !== "undefined" ? performance.now() : Date.now();
}

let fallback: TsdbPerf | undefined;

/** Returns window.__tsdbPerf, creating it (and a long-task observer) on first use. */
export function perf(): TsdbPerf {
  const host = typeof window !== "undefined" ? window : undefined;
  const existing = host ? host.__tsdbPerf : fallback;
  if (existing) {
    return existing;
  }
  const p: TsdbPerf = {
    version: 1,
    firstFullPaint: null,
    lastFullPaint: null,
    lastFallbackPaint: null,
    counters: zeroCounters(),
    events: [],
    reset() {
      p.events.length = 0;
      p.counters = zeroCounters();
      p.lastFullPaint = null;
      p.lastFallbackPaint = null;
    },
  };
  if (host) {
    host.__tsdbPerf = p;
    observeLongTasks();
  } else {
    fallback = p;
  }
  return p;
}

/** Records an event. view.full and view.fallback also set the paint timestamps. */
export function perfEvent(name: PerfEventName, detail?: PerfEvent["detail"]): void {
  const p = perf();
  const t = now();
  if (p.events.length >= MAX_EVENTS) {
    p.events.splice(0, p.events.length - MAX_EVENTS + 1);
  }
  p.events.push(detail ? { name, t, detail } : { name, t });
  switch (name) {
    case "view.full":
      p.lastFullPaint = t;
      if (p.firstFullPaint === null) {
        p.firstFullPaint = t;
      }
      break;
    case "view.fallback":
      p.lastFallbackPaint = t;
      break;
  }
}

/** Adds n to a counter (textureBytes may go down). */
export function perfCount(counter: keyof PerfCounters, n = 1): void {
  perf().counters[counter] += n;
}

function observeLongTasks(): void {
  if (typeof PerformanceObserver === "undefined") {
    return;
  }
  try {
    const obs = new PerformanceObserver(list => {
      for (const entry of list.getEntries()) {
        perfCount("longTasks");
        perfEvent("longtask", { ms: Math.round(entry.duration) });
      }
    });
    obs.observe({ type: "longtask", buffered: true });
  } catch {
    // Long-task timing is not supported in this browser.
  }
}
