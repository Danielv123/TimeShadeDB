// Page start-up: finds the data source and, when the URL already names the datastore and the
// camera (a shared link, or a reload), sends the first version request in parallel with the
// catalog instead of after it, which saves one round trip before the first tiles:
//
// - live server, latest: `view` for the visible tiles plus a one-tile ring;
// - history (`t=` in the URL) or a static export: the `timeline` of that UTC day (today for
//   latest on a static export).
//
// index.html starts the same requests before the bundle has even loaded
// (window.__tsdbEarly); boot() adopts them when they match what it would request.

import type { Catalog, TimelineResponse, ViewResponse } from "../api/types";
import { datastoreId, utcDay } from "../api/grid.ts";
import { levelFor, tileRange, visibleWorld } from "./camera.ts";
import { HttpError, ServerSource, StaticSource, type FetchFn, type UrlSource } from "./sources.ts";

export type Seed = { kind: "view"; res: ViewResponse; bbox: [number, number, number, number] } | { kind: "timeline"; res: TimelineResponse };

export type SeedRequest =
  | { kind: "view"; ds: string; z: number; bbox: [number, number, number, number] }
  | { kind: "timeline"; ds: string; z: number; date: string };

export interface BootResult {
  source: UrlSource;
  catalog: Catalog;
  /** The speculative first version answer, or null (no camera in the URL, or it failed). */
  seed: Promise<Seed | null>;
}

/**
 * The first version request a URL implies, or null. `isStatic` selects timelines for latest;
 * `now` (Date.now()) names today's timeline.
 */
export function seedRequest(params: URLSearchParams, viewport: { width: number; height: number; dpr: number }, isStatic: boolean, now: number): SeedRequest | null {
  const map = params.get("map");
  const surface = params.get("surface");
  const force = params.get("force") ?? "player";
  const x = Number(params.get("x"));
  const y = Number(params.get("y"));
  const s = Number(params.get("s"));
  const tRaw = params.get("t");
  const t = tRaw !== null && tRaw !== "" ? Number(tRaw) : null;
  if (!map || !surface || params.get("x") === null || !Number.isFinite(x) || !Number.isFinite(y) || !(s > 0) || (t !== null && !(Number.isFinite(t) && Math.abs(t) <= 8.64e15))) {
    return null;
  }
  const ds = datastoreId({ map, surface, force });
  const z = levelFor(s, viewport.dpr);
  if (t !== null || isStatic) {
    return { kind: "timeline", ds, z, date: utcDay(t ?? now) };
  }
  const r = tileRange(visibleWorld({ x, y, scale: s }, viewport), z, 1);
  if ((r.x1 - r.x0 + 1) * (r.y1 - r.y0 + 1) > 4096) {
    return null;
  }
  return { kind: "view", ds, z, bbox: [r.x0, r.y0, r.x1, r.y1] };
}

/** Path of a seed request relative to the source base (the same on both sources). */
export function seedPath(req: SeedRequest): string {
  const ds = encodeURIComponent(req.ds);
  return req.kind === "view" ? `view/${ds}/${req.z}?bbox=${req.bbox.join(",")}&t=latest` : `timeline/${ds}/${req.z}/${req.date}.json`;
}

/** Requests index.html started before the bundle loaded (window.__tsdbEarly). */
export interface EarlyRequests {
  kind: "server" | "static";
  base: string;
  catalog: Promise<Response>;
  seed: { path: string; res: Promise<Response> } | null;
}

/** Takes window.__tsdbEarly (once). */
export function takeEarly(): EarlyRequests | null {
  const g = globalThis as { __tsdbEarly?: EarlyRequests };
  const e = g.__tsdbEarly ?? null;
  g.__tsdbEarly = undefined;
  return e;
}

/** The source kind the page asks for: ?source=, else <meta name="tsdb-source">, else null (detect). */
export function requestedSource(params: URLSearchParams, meta: string | null | undefined): "server" | "static" | null {
  const k = params.get("source") ?? meta ?? null;
  return k === "static" || k === "server" ? k : null;
}

export interface BootOptions {
  fetch?: FetchFn;
  early?: EarlyRequests | null;
  /** Content of <meta name="tsdb-source"> (set by static exports). */
  metaSource?: string | null;
  /** Date.now() override (tests). */
  now?: number;
}

async function start(src: UrlSource & (ServerSource | StaticSource), params: URLSearchParams, viewport: { width: number; height: number; dpr: number }, opts: BootOptions): Promise<BootResult> {
  const isStatic = src.kind === "static";
  const early = opts.early && opts.early.kind === src.kind && opts.early.base === src.base ? opts.early : null;
  const req = seedRequest(params, viewport, isStatic, opts.now ?? Date.now());
  let seed: Promise<Seed | null> = Promise.resolve(null);
  if (req) {
    const path = seedPath(req);
    const fromEarly = early?.seed && early.seed.path === path ? early.seed.res.then(r => src.adopt(r, path)).then(r => r.json() as Promise<unknown>) : null;
    if (req.kind === "view") {
      const p = (fromEarly as Promise<ViewResponse> | null) ?? (src as ServerSource).view(req.ds, req.z, req.bbox, "latest");
      seed = p.then(
        res => ({ kind: "view" as const, res, bbox: req.bbox }),
        () => null,
      );
    } else {
      const p = (fromEarly as Promise<TimelineResponse> | null) ?? src.timeline(req.ds, req.z, req.date);
      seed = p.then(
        res => ({ kind: "timeline" as const, res }),
        () => null,
      );
    }
  }
  // A network error (not an HTTP status) is retried once.
  const catalog: Catalog = early
    ? await early.catalog.then(r => src.adopt(r, isStatic ? "catalog.json" : "catalog"), () => null).then(r => (r ? (r.json() as Promise<Catalog>) : src.catalog()))
    : await src.catalog().catch(err => {
        if (err instanceof HttpError) {
          throw err;
        }
        return src.catalog();
      });
  if (!catalog || !Array.isArray(catalog.maps)) {
    throw new Error("not a TimeShadeDB catalog");
  }
  if (Number.isFinite(catalog.now)) {
    src.clock.observe(catalog.now);
  }
  return { source: src, catalog, seed };
}

/**
 * Finds the data source: `?source=static|server&data=<base>`, else the page's
 * <meta name="tsdb-source">, else the live server at /api/v3/ and then a static export next to
 * the page (catalog.json).
 */
export async function boot(search: string, viewport: { width: number; height: number; dpr: number }, opts: BootOptions = {}): Promise<BootResult> {
  const params = new URLSearchParams(search);
  const kind = requestedSource(params, opts.metaSource);
  const token = params.get("token") ?? undefined;
  const data = params.get("data");
  const makeStatic = (): StaticSource => new StaticSource({ base: data ?? "./", readToken: token, fetch: opts.fetch, cacheApi: true });
  if (kind === "static") {
    return start(makeStatic(), params, viewport, opts);
  }
  const server = new ServerSource({ base: data ?? "/api/v3/", readToken: token, fetch: opts.fetch });
  try {
    return await start(server, params, viewport, opts);
  } catch (err) {
    if (kind === "server") {
      throw err;
    }
  }
  // Not a live server: a static export next to the page (or at ?data=).
  return start(makeStatic(), params, viewport, { ...opts, early: null });
}
