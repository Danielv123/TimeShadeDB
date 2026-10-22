// Fetching tile bodies off the main thread: a pool of 2-4 workers
// fetches (the browser decompresses through Content-Encoding) and transfers the ArrayBuffer
// back. For the Canvas2D renderer the worker also decodes the tile and expands its pixels to
// RGBA through the palette LUT; otherwise the header is validated on the main thread when the
// body is decoded (terrain.ts). DirectLoader fetches on the calling thread (tests, WebGL2 by
// default, or when workers are unavailable or fail to start).

import { decodeTile } from "../api/wire.ts";
import { expandRgba, lutFor } from "./renderer.ts";
import { HttpError } from "./sources.ts";

export interface FetchOptions {
  cache?: RequestCache;
  /** Expand a TSV3 body to RGBA (Canvas2D) if the loader has a palette LUT covering it. */
  expand?: boolean;
}

export interface FetchResult {
  buffer: ArrayBuffer;
  /** Fetch time in ms (request start to body read). */
  ms: number;
  rgba?: Uint8ClampedArray;
}

export interface TileLoader {
  readonly kind: "worker" | "direct";
  fetch(url: string, headers: Record<string, string>, signal: AbortSignal, opts?: FetchOptions): Promise<FetchResult>;
  /** Palette LUT for RGBA expansion (Canvas2D); null to disable. */
  setLut(lut: Uint32Array | null): void;
  dispose(): void;
}

function abortError(): Error {
  const e = new Error("aborted");
  e.name = "AbortError";
  return e;
}

function nowMs(): number {
  return typeof performance !== "undefined" ? performance.now() : Date.now();
}

async function errorFor(res: Response, url: string): Promise<HttpError> {
  let code = "internal";
  let message = `${res.status} ${res.statusText} for ${url}`;
  try {
    const body = (await res.json()) as { error?: string; code?: string };
    if (body && typeof body.code === "string") {
      code = body.code;
      message = `${body.error ?? code} (${res.status})`;
    }
  } catch {
    // not JSON
  }
  return new HttpError(res.status, code, message);
}

/** Expands a TSV3 body to RGBA if `lut` covers its palette version. */
export function expandBody(buffer: ArrayBuffer, lut: Uint32Array | null): Uint8ClampedArray | undefined {
  try {
    const t = decodeTile(buffer);
    if (t.header.format !== 3 && (!lut || lut.length < t.header.paletteVersion)) {
      return undefined;
    }
    const out = expandRgba(t.pixels, lutFor(t.header.format, lut));
    return new Uint8ClampedArray(out.buffer);
  } catch {
    return undefined;
  }
}

export class DirectLoader implements TileLoader {
  readonly kind = "direct" as const;
  private lut: Uint32Array | null = null;
  private readonly fetchFn: (url: string, init: RequestInit) => Promise<Response>;

  constructor(fetchFn?: (url: string, init: RequestInit) => Promise<Response>) {
    this.fetchFn = fetchFn ?? ((url, init) => fetch(url, init));
  }

  async fetch(url: string, headers: Record<string, string>, signal: AbortSignal, opts?: FetchOptions): Promise<FetchResult> {
    if (signal.aborted) {
      throw abortError();
    }
    const t0 = nowMs();
    const res = await this.fetchFn(url, { headers, signal, cache: opts?.cache });
    if (!res.ok) {
      throw await errorFor(res, url);
    }
    const buffer = await res.arrayBuffer();
    const ms = nowMs() - t0;
    return { buffer, ms, rgba: opts?.expand ? expandBody(buffer, this.lut) : undefined };
  }

  setLut(lut: Uint32Array | null): void {
    this.lut = lut;
  }

  dispose(): void {}
}

interface Pending {
  resolve(r: FetchResult): void;
  reject(e: unknown): void;
  worker: number;
  signal: AbortSignal;
  onAbort: () => void;
}

/** Messages to the worker (tile_worker.ts). */
export type WorkerRequest =
  | { type: "fetch"; id: number; url: string; headers: Record<string, string>; cache?: RequestCache; expand?: boolean }
  | { type: "abort"; id: number }
  | { type: "lut"; lut: Uint32Array | null };

/** Messages from the worker. */
export type WorkerResponse =
  | { id: number; ok: true; buffer: ArrayBuffer; ms: number; rgba?: ArrayBuffer }
  | { id: number; ok: false; status?: number; code?: string; name?: string; message: string };

/** `url` resolved against the page, when there is a page; unchanged otherwise. */
export function absoluteUrl(url: string): string {
  if (typeof location === "undefined" || !location.href) {
    return url;
  }
  try {
    return new URL(url, location.href).href;
  } catch {
    return url;
  }
}

export class WorkerLoader implements TileLoader {
  readonly kind = "worker" as const;
  private readonly workers: Worker[];
  private readonly load: number[];
  /** Workers that failed (script blocked by CSP, wrong asset base, crash): never picked again. */
  private readonly dead: boolean[];
  private readonly pending = new Map<number, Pending>();
  private nextId = 1;
  private readonly fallback: DirectLoader;

  constructor(make: () => Worker, size: number, fallback: DirectLoader = new DirectLoader()) {
    this.workers = [];
    this.load = [];
    this.dead = [];
    this.fallback = fallback;
    for (let i = 0; i < size; i++) {
      const w = make();
      const idx = i;
      w.onmessage = (e: MessageEvent<WorkerResponse>) => this.onMessage(idx, e.data);
      w.onerror = (ev: Event) => {
        ev.preventDefault?.();
        // A worker that fails is not used again; its pending fetches are rejected and the
        // callers retry (on another worker, or on the main thread once all have failed).
        this.dead[idx] = true;
        w.terminate();
        for (const [id, p] of this.pending) {
          if (p.worker === idx) {
            this.finish(id);
            p.reject(new Error("tile worker failed"));
          }
        }
      };
      this.workers.push(w);
      this.load.push(0);
      this.dead.push(false);
    }
  }

  /** Workers still usable. */
  get alive(): number {
    return this.dead.filter(d => !d).length;
  }

  fetch(url: string, headers: Record<string, string>, signal: AbortSignal, opts?: FetchOptions): Promise<FetchResult> {
    if (signal.aborted) {
      return Promise.reject(abortError());
    }
    let wi = -1;
    for (let i = 0; i < this.load.length; i++) {
      if (!this.dead[i] && (wi < 0 || this.load[i] < this.load[wi])) {
        wi = i;
      }
    }
    if (wi < 0) {
      return this.fallback.fetch(url, headers, signal, opts);
    }
    const id = this.nextId++;
    return new Promise<FetchResult>((resolve, reject) => {
      const onAbort = (): void => {
        if (this.pending.has(id)) {
          this.finish(id);
          this.workers[wi].postMessage({ type: "abort", id } satisfies WorkerRequest);
          reject(abortError());
        }
      };
      signal.addEventListener("abort", onAbort, { once: true });
      this.pending.set(id, { resolve, reject, worker: wi, signal, onAbort });
      this.load[wi]++;
      // A worker resolves relative URLs against its own script (assets/), not the page, so a
      // relative source base ("./", a static export) is made absolute here.
      const abs = absoluteUrl(url);
      this.workers[wi].postMessage({ type: "fetch", id, url: abs, headers, cache: opts?.cache, expand: opts?.expand } satisfies WorkerRequest);
    });
  }

  setLut(lut: Uint32Array | null): void {
    this.fallback.setLut(lut);
    this.workers.forEach((w, i) => {
      if (!this.dead[i]) {
        w.postMessage({ type: "lut", lut } satisfies WorkerRequest);
      }
    });
  }

  private finish(id: number): Pending | undefined {
    const p = this.pending.get(id);
    if (p) {
      this.pending.delete(id);
      this.load[p.worker]--;
      p.signal.removeEventListener("abort", p.onAbort);
    }
    return p;
  }

  private onMessage(_worker: number, msg: WorkerResponse): void {
    const p = this.finish(msg.id);
    if (!p) {
      return;
    }
    if (msg.ok) {
      p.resolve({ buffer: msg.buffer, ms: msg.ms, rgba: msg.rgba ? new Uint8ClampedArray(msg.rgba) : undefined });
    } else if (msg.name === "AbortError") {
      p.reject(abortError());
    } else if (msg.status !== undefined) {
      p.reject(new HttpError(msg.status, msg.code ?? "internal", msg.message));
    } else {
      p.reject(new Error(msg.message));
    }
  }

  dispose(): void {
    for (const [id, p] of this.pending) {
      this.finish(id);
      p.reject(abortError());
    }
    for (const w of this.workers) {
      w.terminate();
    }
  }
}

/** Creates the default loader: a worker pool when module workers work, else direct fetches. */
export function createLoader(size?: number): TileLoader {
  if (typeof Worker === "undefined") {
    return new DirectLoader();
  }
  const n = size ?? Math.min(4, Math.max(2, Math.floor((typeof navigator !== "undefined" ? navigator.hardwareConcurrency : 4) / 2)));
  try {
    return new WorkerLoader(() => new Worker(new URL("./tile_worker.ts", import.meta.url), { type: "module", name: "tsdb-tiles" }), n);
  } catch {
    return new DirectLoader();
  }
}
