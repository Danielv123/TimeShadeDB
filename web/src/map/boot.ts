// Page start-up: finds the data source and, when the URL already names the datastore and the
// camera (a shared link, or a reload), sends the first `view` request in parallel with the
// catalog instead of after it, which saves one round trip before the first tiles.

import type { Catalog, ViewResponse } from "../api/types";
import { datastoreId } from "../api/grid.ts";
import { levelFor, tileRange, visibleWorld } from "./camera.ts";
import { detectSource, HttpError, ServerSource, type FetchFn, type UrlSource } from "./sources.ts";

export interface ViewSeed {
  res: ViewResponse;
  bbox: [number, number, number, number];
}

export interface BootResult {
  source: UrlSource;
  catalog: Catalog;
  /** The speculative first `view` answer, or null (no camera in the URL, static source, error). */
  seed: Promise<ViewSeed | null>;
}

/** The `view` request a URL implies: datastore, level and the visible tiles plus a one-tile ring. */
export function seedRequest(
  params: URLSearchParams,
  viewport: { width: number; height: number; dpr: number },
): { ds: string; z: number; bbox: [number, number, number, number] } | null {
  const map = params.get("map");
  const surface = params.get("surface");
  const force = params.get("force") ?? "player";
  const x = Number(params.get("x"));
  const y = Number(params.get("y"));
  const s = Number(params.get("s"));
  if (!map || !surface || params.get("t") || !Number.isFinite(x) || !Number.isFinite(y) || !(s > 0) || params.get("x") === null) {
    return null;
  }
  const z = levelFor(s, viewport.dpr);
  const r = tileRange(visibleWorld({ x, y, scale: s }, viewport), z, 1);
  if ((r.x1 - r.x0 + 1) * (r.y1 - r.y0 + 1) > 4096) {
    return null;
  }
  return { ds: datastoreId({ map, surface, force }), z, bbox: [r.x0, r.y0, r.x1, r.y1] };
}

/** Requests index.html started before the bundle loaded (window.__tsdbEarly). */
export interface EarlyRequests {
  base: string;
  catalog: Promise<Response>;
  view: { path: string; res: Promise<Response> } | null;
}

/** Takes window.__tsdbEarly (once). */
export function takeEarly(): EarlyRequests | null {
  const g = globalThis as { __tsdbEarly?: EarlyRequests };
  const e = g.__tsdbEarly ?? null;
  g.__tsdbEarly = undefined;
  return e;
}

export async function boot(
  search: string,
  viewport: { width: number; height: number; dpr: number },
  opts: { fetch?: FetchFn; early?: EarlyRequests | null } = {},
): Promise<BootResult> {
  const params = new URLSearchParams(search);
  const kind = params.get("source");
  if (kind === "static") {
    const r = await detectSource(search, opts);
    return { ...r, seed: Promise.resolve(null) };
  }
  const token = params.get("token") ?? undefined;
  const server = new ServerSource({ base: params.get("data") ?? "/api/v3/", readToken: token, fetch: opts.fetch });
  const early = opts.early && opts.early.base === server.base ? opts.early : null;
  const req = seedRequest(params, viewport);
  let seed: Promise<ViewSeed | null> = Promise.resolve(null);
  if (req) {
    const path = server.viewPath(req.ds, req.z, req.bbox, "latest");
    const res: Promise<ViewResponse> =
      early?.view && early.view.path === path
        ? early.view.res.then(r => server.adopt(r, path)).then(r => r.json() as Promise<ViewResponse>)
        : server.view(req.ds, req.z, req.bbox, "latest");
    seed = res.then(
      r => ({ res: r, bbox: req.bbox }),
      () => null,
    );
  }
  try {
    // A network error (not an HTTP status) on the early request is retried once.
    const catalog: Catalog = early
      ? await early.catalog
          .then(r => server.adopt(r, "catalog"), () => null)
          .then(r => (r ? (r.json() as Promise<Catalog>) : server.catalog()))
      : await server.catalog().catch(err => {
          if (err instanceof HttpError) {
            throw err;
          }
          return server.catalog();
        });
    if (!server.clock.synced && Number.isFinite(catalog?.now)) {
      server.clock.observe(catalog.now);
    }
    if (catalog && Array.isArray(catalog.maps)) {
      return { source: server, catalog, seed };
    }
  } catch (err) {
    if (kind === "server") {
      throw err;
    }
  }
  // Not a live server: a static export next to the page (or at ?data=).
  const r = await detectSource(`?source=static${params.get("data") ? `&data=${encodeURIComponent(params.get("data")!)}` : ""}`, opts);
  return { ...r, seed: Promise.resolve(null) };
}
