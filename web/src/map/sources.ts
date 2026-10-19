// DataSource implementations (web/src/api/types.ts): the live server under /api/v3/ and a
// static export (docs/v3/API.md section 6). Both also expose request URLs so tile fetches can
// run in workers (UrlSource). The static source maps calls to the export's file names.
// Source abstraction modelled on illisis-minimap web/view/source.ts (Blue Oak Model License 1.0.0).

import type {
  Catalog,
  DataSource,
  DayIndex,
  ErrorResponse,
  IntervalKind,
  LiveHandlers,
  LiveHello,
  OverlayResponse,
  TileCoord,
  TimelineResponse,
  TimeMs,
  Version,
  ViewResponse,
} from "../api/types";
import { DataClock } from "./clock.ts";

export class HttpError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "HttpError";
    this.status = status;
    this.code = code;
  }
}

/** A source whose binary resources can be fetched by URL (from a worker). */
export interface UrlSource extends DataSource {
  tileUrl(ds: string, tile: TileCoord, version: Version | "live"): string;
  tileDeltaUrl(ds: string, x: number, y: number, from: Version, to: Version): string;
  paletteUrl(ds: string, version: number): string;
  /** Headers every request needs (authorization). */
  requestHeaders(): Record<string, string>;
  /** Longest version span one tiledelta request may cover (static exports: 1). */
  readonly deltaSpan: number;
}

export function isUrlSource(s: DataSource): s is UrlSource {
  return typeof (s as Partial<UrlSource>).tileUrl === "function";
}

export type FetchFn = (input: string, init?: RequestInit) => Promise<Response>;

export interface HttpSourceOptions {
  /** Base URL ending in "/" (e.g. "/api/v3/" or "https://bucket.example/map/"). */
  base: string;
  /** Read token (API.md 1.3), sent as a bearer header and as ?token= for live events. */
  readToken?: string;
  fetch?: FetchFn;
  clock?: DataClock;
  /** Static only: use the Cache API for immutable files (when available). */
  cacheApi?: boolean;
}

const enc = encodeURIComponent;

function joinBase(base: string): string {
  return base.endsWith("/") ? base : base + "/";
}

function abortError(): Error {
  const e = new Error("aborted");
  e.name = "AbortError";
  return e;
}

abstract class HttpSource {
  readonly base: string;
  readonly clock: DataClock;
  protected readonly readToken?: string;
  protected readonly fetchFn: FetchFn;

  constructor(opts: HttpSourceOptions) {
    this.base = joinBase(opts.base);
    this.readToken = opts.readToken;
    this.clock = opts.clock ?? new DataClock();
    const f = opts.fetch ?? ((input: string, init?: RequestInit) => fetch(input, init));
    this.fetchFn = f;
  }

  now(): TimeMs {
    return this.clock.now();
  }

  requestHeaders(): Record<string, string> {
    return this.readToken ? { Authorization: `Bearer ${this.readToken}` } : {};
  }

  url(path: string): string {
    return this.base + path;
  }

  protected async get(path: string, signal?: AbortSignal, init?: RequestInit): Promise<Response> {
    if (signal?.aborted) {
      throw abortError();
    }
    const res = await this.fetchFn(this.url(path), { ...init, headers: { ...this.requestHeaders(), ...(init?.headers as Record<string, string> | undefined) }, signal });
    return this.adopt(res, path);
  }

  /**
   * Takes a response fetched elsewhere for `path` (the early requests in index.html): keeps the
   * data clock and turns error statuses into HttpError, as for the source's own requests.
   */
  async adopt(res: Response, path: string): Promise<Response> {
    const now = res.headers.get("X-TSDB-Now");
    if (now !== null && now !== "") {
      this.clock.observe(Number(now));
    }
    if (!res.ok) {
      let code = "internal";
      let message = `${res.status} ${res.statusText} for ${path}`;
      try {
        const body = (await res.json()) as ErrorResponse;
        if (body && typeof body.code === "string") {
          code = body.code;
          message = `${body.error} (${res.status} ${path})`;
        }
      } catch {
        // Not a JSON error body.
      }
      throw new HttpError(res.status, code, message);
    }
    return res;
  }

  protected async getJson<T>(path: string, signal?: AbortSignal, init?: RequestInit): Promise<T> {
    const res = await this.get(path, signal, init);
    return (await res.json()) as T;
  }

  protected async getBin(path: string, signal?: AbortSignal, init?: RequestInit): Promise<ArrayBuffer> {
    const res = await this.get(path, signal, init);
    return res.arrayBuffer();
  }

  paletteUrl(ds: string, version: number): string {
    return this.url(`palette/${enc(ds)}/${version}.bin`);
  }

  tileDeltaUrl(ds: string, x: number, y: number, from: Version, to: Version): string {
    return this.url(`tiledelta/${enc(ds)}/${x}/${y}/${from}-${to}.bin`);
  }

  timeline(ds: string, z: number, date: string, signal?: AbortSignal): Promise<TimelineResponse> {
    return this.getJson(`timeline/${enc(ds)}/${z}/${date}.json`, signal, { cache: "no-cache" });
  }

  moverDay(sfc: string, date: string, signal?: AbortSignal): Promise<DayIndex> {
    return this.getJson(`movers/${enc(sfc)}/day/${date}.json`, signal, { cache: "no-cache" });
  }

  moverWindow(sfc: string, start: TimeMs, signal?: AbortSignal): Promise<ArrayBuffer> {
    return this.getBin(`movers/${enc(sfc)}/window/${start}.bin`, signal);
  }

  moverKeyframe(sfc: string, t: TimeMs, signal?: AbortSignal): Promise<ArrayBuffer> {
    return this.getBin(`movers/${enc(sfc)}/keyframe/${t}.bin`, signal);
  }

  moverLive(sfc: string, signal?: AbortSignal): Promise<ArrayBuffer> {
    return this.getBin(`movers/${enc(sfc)}/live.bin`, signal, { cache: "no-cache" });
  }
}

/** The live TimeShadeDB server (`/api/v3/`). */
export class ServerSource extends HttpSource implements UrlSource {
  readonly kind = "server" as const;
  readonly deltaSpan = Number.POSITIVE_INFINITY;

  async catalog(signal?: AbortSignal): Promise<Catalog> {
    const c = await this.getJson<Catalog>("catalog", signal, { cache: "no-cache" });
    if (!this.clock.synced && Number.isFinite(c.now)) {
      this.clock.observe(c.now);
    }
    return c;
  }

  palette(ds: string, version: number, signal?: AbortSignal): Promise<ArrayBuffer> {
    return this.getBin(`palette/${enc(ds)}/${version}.bin`, signal);
  }

  /** The path of a `view` request (relative to the base). */
  viewPath(ds: string, z: number, bbox: [number, number, number, number], t: TimeMs | "latest"): string {
    const tq = t === "latest" ? "latest" : String(Math.floor(t));
    return `view/${enc(ds)}/${z}?bbox=${bbox.join(",")}&t=${tq}`;
  }

  view(ds: string, z: number, bbox: [number, number, number, number], t: TimeMs | "latest", signal?: AbortSignal): Promise<ViewResponse> {
    const init: RequestInit | undefined = t === "latest" ? { cache: "no-cache" } : undefined;
    return this.getJson(this.viewPath(ds, z, bbox, t), signal, init);
  }

  tileUrl(ds: string, tile: TileCoord, version: Version | "live"): string {
    return this.url(`tile/${enc(ds)}/${tile.z}/${tile.x}/${tile.y}/${version}.bin`);
  }

  tile(ds: string, tile: TileCoord, version: Version | "live", signal?: AbortSignal): Promise<ArrayBuffer> {
    return this.getBin(`tile/${enc(ds)}/${tile.z}/${tile.x}/${tile.y}/${version}.bin`, signal, version === "live" ? { cache: "no-store" } : undefined);
  }

  tileDelta(ds: string, x: number, y: number, from: Version, to: Version, signal?: AbortSignal): Promise<ArrayBuffer> {
    return this.getBin(`tiledelta/${enc(ds)}/${x}/${y}/${from}-${to}.bin`, signal);
  }

  overlays(ds: string, x: number, y: number, range?: { t0?: TimeMs; t1?: TimeMs; kinds?: IntervalKind[] }, signal?: AbortSignal): Promise<OverlayResponse> {
    const q: string[] = [];
    if (range?.t0 !== undefined) {
      q.push(`t0=${Math.floor(range.t0)}`);
    }
    if (range?.t1 !== undefined) {
      q.push(`t1=${Math.floor(range.t1)}`);
    }
    if (range?.kinds && range.kinds.length > 0) {
      q.push(`kinds=${range.kinds.join(",")}`);
    }
    return this.getJson(`overlays/${enc(ds)}/${x}/${y}.json${q.length ? "?" + q.join("&") : ""}`, signal, { cache: "no-cache" });
  }

  /**
   * Live events (API.md 5) over Server-Sent Events, parsed from a fetch stream so the
   * bearer header works. Reconnects with backoff; `hello` is delivered again after each
   * reconnect, so callers resync there. Every subscription shares one event stream for
   * the union of their topics (a stream holds a connection for good, one of six per host
   * on HTTP/1.1): a change of the subscriber set reopens it after SUBSCRIBE_SETTLE_MS, and
   * the new stream's `hello` reaches every subscriber, which resyncs as after a reconnect.
   * A subscriber whose topics the open stream already covers is told its state instead:
   * the last `hello` again while it is up, `error` while it is down.
   * Tile events go to the subscribers of their datastore, mover events to those of their
   * surface, everything else to all. A handler that throws is isolated: the error is
   * logged and reported to that subscriber alone (`error`, then the last `hello` again so
   * it resyncs), and the stream stays open for the others.
   */
  subscribe(topics: { ds: string[]; sfc: string[] }, handlers: LiveHandlers): () => void {
    const sub: LiveSub = { ds: new Set(topics.ds), sfc: new Set(topics.sfc), handlers, synced: false };
    this.subs.add(sub);
    this.restartLive();
    return () => {
      if (this.subs.delete(sub)) {
        this.restartLive();
      }
    };
  }

  private readonly subs = new Set<LiveSub>();
  private liveStop: (() => void) | null = null;
  private liveKey = "";
  private liveTimer: ReturnType<typeof setTimeout> | null = null;
  /** State of the open stream: connecting (no hello yet), up (since `lastHello`) or down (`lastErr`, in backoff). */
  private liveState: "connecting" | "up" | "down" = "connecting";
  private lastHello: LiveHello | null = null;
  private lastErr: unknown = null;

  /** Reopens the shared stream for the current subscribers (debounced). */
  private restartLive(): void {
    if (this.liveTimer) {
      clearTimeout(this.liveTimer);
    }
    this.liveTimer = setTimeout(() => {
      this.liveTimer = null;
      const ds = new Set<string>();
      const sfc = new Set<string>();
      for (const s of this.subs) {
        s.ds.forEach(d => ds.add(d));
        s.sfc.forEach(x => sfc.add(x));
      }
      const q = [...[...ds].sort().map(d => `ds=${enc(d)}`), ...[...sfc].sort().map(x => `sfc=${enc(x)}`)];
      const key = q.join("&");
      if (key === this.liveKey && this.liveStop) {
        // Same stream: tell new subscribers where it stands.
        for (const s of [...this.subs]) {
          if (!s.synced && this.liveState === "up" && this.lastHello) {
            this.replayHello(s, this.lastHello);
          } else if (!s.synced && this.liveState === "down") {
            this.fail(s, this.lastErr);
          }
        }
        return;
      }
      this.liveStop?.();
      this.liveStop = null;
      this.liveKey = key;
      this.liveState = "connecting";
      this.lastHello = null;
      for (const s of this.subs) {
        s.synced = false;
      }
      if (q.length > 0) {
        this.liveStop = this.openLive(`live?${key}`);
      }
    }, this.subs.size > 0 ? SUBSCRIBE_SETTLE_MS : 0);
  }

  /** Reports a stream error (or a handler failure) to one subscriber. */
  private fail(s: LiveSub, err: unknown): void {
    s.synced = true;
    try {
      s.handlers.error?.(err);
    } catch (e) {
      console.error("live error handler", e);
    }
  }

  /** Delivers a past hello to one subscriber (without observing its stale `now`). */
  private replayHello(s: LiveSub, hello: LiveHello): void {
    s.synced = true;
    try {
      s.handlers.hello?.(hello);
    } catch (e) {
      console.error("live hello handler", e);
      this.fail(s, e);
    }
  }

  /** Delivers one event to one subscriber, isolating a handler that throws. */
  private deliver(s: LiveSub, event: string, msg: Record<string, unknown>): void {
    if (event === "hello") {
      s.synced = true;
    }
    try {
      dispatchLive(event, msg, s.handlers, s);
    } catch (err) {
      console.error(`live ${event} handler`, err);
      this.fail(s, err);
      if (event !== "hello" && this.lastHello) {
        // It missed this event: resync it as after a reconnect.
        this.replayHello(s, this.lastHello);
      }
    }
  }

  private openLive(path: string): () => void {
    let stopped = false;
    let ctrl: AbortController | null = null;
    let backoff = 500;
    const down = (err: unknown): void => {
      this.liveState = "down";
      this.lastErr = err;
      for (const s of [...this.subs]) {
        this.fail(s, err);
      }
    };
    const dispatch = (event: string, data: string): void => {
      let msg: Record<string, unknown>;
      try {
        msg = JSON.parse(data) as Record<string, unknown>;
      } catch {
        return;
      }
      if (typeof msg.now === "number") {
        this.clock.observe(msg.now);
      }
      if (event === "hello") {
        this.liveState = "up";
        this.lastHello = msg as unknown as LiveHello;
      }
      for (const s of [...this.subs]) {
        this.deliver(s, event, msg);
      }
    };
    const loop = async (): Promise<void> => {
      while (!stopped) {
        ctrl = new AbortController();
        try {
          const res = await this.get(path, ctrl.signal, { cache: "no-store", headers: { Accept: "text/event-stream" } });
          if (!res.body) {
            throw new Error("live: no body");
          }
          backoff = 500;
          await readEvents(res.body, (event, data) => {
            if (!stopped) {
              dispatch(event, data);
            }
          });
          if (!stopped) {
            down(new Error("live: stream ended"));
          }
        } catch (err) {
          if (stopped) {
            return;
          }
          down(err);
        }
        if (stopped) {
          return;
        }
        await new Promise(r => setTimeout(r, backoff));
        backoff = Math.min(15000, backoff * 2);
      }
    };
    void loop();
    return () => {
      stopped = true;
      ctrl?.abort();
    };
  }
}

/** One subscription to the shared event stream; `synced` once it was told the stream's state. */
interface LiveSub {
  ds: Set<string>;
  sfc: Set<string>;
  handlers: LiveHandlers;
  synced: boolean;
}

/** Subscriptions within this long of each other share the first stream opening. */
export const SUBSCRIBE_SETTLE_MS = 50;

/** Calls the handler for one parsed event, filtered by the subscriber's topics. */
function dispatchLive(event: string, msg: Record<string, unknown>, h: LiveHandlers, topics?: { ds: Set<string>; sfc: Set<string> }): void {
  switch (event) {
    case "hello":
      h.hello?.(msg as never);
      break;
    case "tile":
      if (!topics || topics.ds.has(msg.ds as string)) {
        h.tile?.(msg as never);
      }
      break;
    case "horizon":
      h.horizon?.(msg as never);
      break;
    case "movers":
      if (!topics || topics.sfc.has(msg.sfc as string)) {
        h.movers?.(msg as never);
      }
      break;
    case "catalog":
      h.catalog?.(msg as never);
      break;
  }
}

/** Parses a text/event-stream body, calling `on(event, data)` per dispatched event. */
export async function readEvents(body: ReadableStream<Uint8Array>, on: (event: string, data: string) => void): Promise<void> {
  const reader = body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  let event = "message";
  let data: string[] = [];
  for (;;) {
    const { value, done } = await reader.read();
    if (done) {
      return;
    }
    buf += dec.decode(value, { stream: true });
    let nl: number;
    while ((nl = buf.search(/\r\n|\n|\r/)) >= 0) {
      const line = buf.slice(0, nl);
      buf = buf.slice(buf[nl] === "\r" && buf[nl + 1] === "\n" ? nl + 2 : nl + 1);
      if (line === "") {
        if (data.length > 0) {
          on(event, data.join("\n"));
        }
        event = "message";
        data = [];
      } else if (line.startsWith(":")) {
        // comment / ping
      } else {
        const i = line.indexOf(":");
        const field = i < 0 ? line : line.slice(0, i);
        let val = i < 0 ? "" : line.slice(i + 1);
        if (val.startsWith(" ")) {
          val = val.slice(1);
        }
        if (field === "event") {
          event = val;
        } else if (field === "data") {
          data.push(val);
        }
      }
    }
  }
}

const CACHE_NAME = "tsdb-static-v1";

/**
 * A static export (API.md 6): the same relative paths, with `catalog.json`, no `view`, no live
 * tiles and no live events. Immutable files can be kept in the Cache API across visits.
 */
export class StaticSource extends HttpSource implements UrlSource {
  readonly kind = "static" as const;
  readonly deltaSpan = 1;
  private readonly cacheApi: boolean;

  constructor(opts: HttpSourceOptions) {
    super(opts);
    this.cacheApi = !!opts.cacheApi && typeof caches !== "undefined";
  }

  async catalog(signal?: AbortSignal): Promise<Catalog> {
    const c = await this.getJson<Catalog>("catalog.json", signal, { cache: "no-cache" });
    if (Number.isFinite(c.now)) {
      this.clock.observe(c.now);
    }
    return c;
  }

  private async immutable(path: string, signal?: AbortSignal): Promise<ArrayBuffer> {
    if (!this.cacheApi) {
      return this.getBin(path, signal);
    }
    const url = this.url(path);
    try {
      const cache = await caches.open(CACHE_NAME);
      const hit = await cache.match(url);
      if (hit) {
        return hit.arrayBuffer();
      }
      const res = await this.get(path, signal);
      const body = await res.arrayBuffer();
      void cache.put(url, new Response(body.slice(0), { headers: { "Content-Type": "application/octet-stream" } })).catch(() => {});
      return body;
    } catch (err) {
      if ((err as Error).name === "AbortError" || err instanceof HttpError) {
        throw err;
      }
      return this.getBin(path, signal);
    }
  }

  palette(ds: string, version: number, signal?: AbortSignal): Promise<ArrayBuffer> {
    return this.immutable(`palette/${enc(ds)}/${version}.bin`, signal);
  }

  tileUrl(ds: string, tile: TileCoord, version: Version | "live"): string {
    if (version === "live") {
      throw new HttpError(404, "not_found", "static exports have no live tiles");
    }
    return this.url(`tile/${enc(ds)}/${tile.z}/${tile.x}/${tile.y}/${version}.bin`);
  }

  async tile(ds: string, tile: TileCoord, version: Version | "live", signal?: AbortSignal): Promise<ArrayBuffer> {
    if (version === "live") {
      throw new HttpError(404, "not_found", "static exports have no live tiles");
    }
    return this.immutable(`tile/${enc(ds)}/${tile.z}/${tile.x}/${tile.y}/${version}.bin`, signal);
  }

  tileDelta(ds: string, x: number, y: number, from: Version, to: Version, signal?: AbortSignal): Promise<ArrayBuffer> {
    return this.immutable(`tiledelta/${enc(ds)}/${x}/${y}/${from}-${to}.bin`, signal);
  }

  async overlays(ds: string, x: number, y: number, range?: { t0?: TimeMs; t1?: TimeMs; kinds?: IntervalKind[] }, signal?: AbortSignal): Promise<OverlayResponse> {
    const all = await this.getJson<OverlayResponse>(`overlays/${enc(ds)}/${x}/${y}.json`, signal, { cache: "no-cache" });
    if (!range) {
      return all;
    }
    const t0 = range.t0 ?? Number.NEGATIVE_INFINITY;
    const t1 = range.t1 ?? Number.POSITIVE_INFINITY;
    const kinds = range.kinds && range.kinds.length > 0 ? new Set(range.kinds) : null;
    return {
      ...all,
      intervals: all.intervals.filter(i => i.start < t1 && (i.end === 0 || i.end > t0) && (!kinds || kinds.has(i.kind))),
    };
  }
}

/**
 * Picks a source for the page: `?source=static|server&data=<base>` when given, else the live
 * server at /api/v3/, else a static export next to the page (catalog.json).
 */
export async function detectSource(search: string, opts: { fetch?: FetchFn; readToken?: string } = {}): Promise<{ source: UrlSource; catalog: Catalog }> {
  const params = new URLSearchParams(search);
  const kind = params.get("source");
  const data = params.get("data");
  const token = params.get("token") ?? opts.readToken;
  const make = (k: string, base: string): UrlSource =>
    k === "static" ? new StaticSource({ base, readToken: token ?? undefined, fetch: opts.fetch, cacheApi: true }) : new ServerSource({ base, readToken: token ?? undefined, fetch: opts.fetch });
  if (kind === "static" || kind === "server") {
    const s = make(kind, data ?? (kind === "static" ? "./" : "/api/v3/"));
    return { source: s, catalog: await s.catalog() };
  }
  const candidates: [string, string][] = data ? [["server", data], ["static", data]] : [["server", "/api/v3/"], ["static", "./"]];
  let lastErr: unknown;
  for (const [k, base] of candidates) {
    const s = make(k, base);
    try {
      const catalog = await s.catalog();
      if (catalog && Array.isArray(catalog.maps)) {
        return { source: s, catalog };
      }
    } catch (err) {
      lastErr = err;
    }
  }
  throw lastErr instanceof Error ? lastErr : new Error("no TimeShadeDB data source found");
}
