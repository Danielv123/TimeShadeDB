// A mock TimeShadeDB v3 server for tests and benchmarks (Node only). Serves a MockDataset as
// the live API under /api/v3/ and as a static export under /static/ (docs/v3/API.md 1-6),
// with gzip Content-Encoding, X-TSDB-Now, the documented cache headers and error bodies,
// optional latency, a request log and "holds" that keep matching requests open until
// released (to observe the viewer while tiles load). Optionally serves a directory of
// static files (the built viewer) at /.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { createSecureServer } from "node:http2";
import { readFile } from "node:fs/promises";
import { extname, join, normalize, resolve } from "node:path";
import { gzipSync } from "node:zlib";
import type { AddressInfo } from "node:net";
import {
  encodeDelta,
  encodePalette,
  encodeTile,
  timelineFor,
  tk,
  versionAt,
  viewFor,
  type MockDataset,
  type MockDatastore,
} from "./dataset.ts";
import { moverRoute, type MoverDb } from "./moverdb.ts";

export interface RequestLog {
  path: string;
  t: number;
  status: number;
  bytes: number;
}

export interface MockServerOptions {
  /** Delay before answering each data request, ms. */
  latencyMs?: number;
  /** gzip responses when the client accepts it (default true). */
  gzip?: boolean;
  /** Directory served at / for non-API paths (the built viewer). */
  staticDir?: string;
  /** Extra files: path -> body (for fixture passthrough, e.g. movers). */
  files?: Map<string, { body: Uint8Array; type: string }>;
  /** Answer every tiledelta request with 404 (an export without deltas). */
  noDeltas?: boolean;
  /** Serve HTTP/2 over TLS (HTTP/1.1 is still accepted) with this key and certificate (PEM). */
  tls?: { key: string | Buffer; cert: string | Buffer };
  /** Answer `movers/...` (day indexes, windows, keyframes) from a v3 database (moverdb.ts). */
  moverDb?: MoverDb;
}

interface Hold {
  re: RegExp;
  waiting: (() => void)[];
  released: boolean;
}

const MIME: Record<string, string> = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript",
  ".css": "text/css",
  ".png": "image/png",
  ".json": "application/json",
  ".svg": "image/svg+xml",
};

export class MockServer {
  readonly data: MockDataset;
  readonly log: RequestLog[] = [];
  private server: Server | null = null;
  private readonly opts: MockServerOptions;
  private readonly holds: Hold[] = [];
  private readonly cache = new Map<string, { body: Buffer; gz: Buffer | null }>();
  private readonly sse = new Set<{ res: ServerResponse; ds: Set<string> }>();
  port = 0;

  constructor(data: MockDataset, opts: MockServerOptions = {}) {
    this.data = data;
    this.opts = opts;
  }

  get url(): string {
    return `${this.opts.tls ? "https" : "http"}://127.0.0.1:${this.port}`;
  }

  async start(port = 0): Promise<this> {
    const handler = (req: IncomingMessage, res: ServerResponse): void => {
      void this.handle(req, res).catch(err => {
        if (!res.headersSent) {
          res.writeHead(500, { "Content-Type": "application/json" });
        }
        res.end(JSON.stringify({ error: String(err), code: "internal" }));
      });
    };
    // The HTTP/2 compatibility API matches the parts of IncomingMessage/ServerResponse used here.
    this.server = this.opts.tls
      ? (createSecureServer({ key: this.opts.tls.key, cert: this.opts.tls.cert, allowHTTP1: true }, handler as never) as unknown as Server)
      : createServer(handler);
    await new Promise<void>(r => this.server!.listen(port, "127.0.0.1", r));
    this.port = (this.server.address() as AddressInfo).port;
    return this;
  }

  async stop(): Promise<void> {
    for (const h of this.holds) {
      this.releaseHold(h);
    }
    for (const c of this.sse) {
      c.res.end();
    }
    this.sse.clear();
    if (this.server) {
      this.server.closeAllConnections?.();
      await new Promise<void>(r => this.server!.close(() => r()));
      this.server = null;
    }
  }

  /** Keeps requests whose path matches `re` open until the returned function is called. */
  hold(re: RegExp): () => void {
    const h: Hold = { re, waiting: [], released: false };
    this.holds.push(h);
    return () => this.releaseHold(h);
  }

  private releaseHold(h: Hold): void {
    h.released = true;
    const i = this.holds.indexOf(h);
    if (i >= 0) {
      this.holds.splice(i, 1);
    }
    for (const w of h.waiting.splice(0)) {
      w();
    }
  }

  /** Requests whose path matches. */
  requests(re: RegExp): RequestLog[] {
    return this.log.filter(l => re.test(l.path));
  }

  clearLog(): void {
    this.log.length = 0;
  }

  /** Sends a live event to SSE clients subscribed to `ds`. */
  pushEvent(event: string, data: Record<string, unknown>, ds?: string): void {
    for (const c of this.sse) {
      if (!ds || c.ds.has(ds)) {
        c.res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
      }
    }
  }

  private async handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const u = new URL(req.url ?? "/", "http://x");
    const path = u.pathname;
    let rel: string | null = null;
    let mode: "server" | "static" = "server";
    if (path.startsWith("/api/v3/")) {
      rel = path.slice("/api/v3/".length);
    } else if (path.startsWith("/static/")) {
      rel = path.slice("/static/".length);
      mode = "static";
    }
    if (rel === null) {
      await this.serveFile(path, req, res);
      return;
    }
    for (const h of this.holds) {
      if (h.re.test(path)) {
        await new Promise<void>(r => (h.released ? r() : h.waiting.push(r)));
      }
    }
    if (this.opts.latencyMs) {
      await new Promise(r => setTimeout(r, this.opts.latencyMs));
    }
    if (req.destroyed) {
      this.log.push({ path: path + u.search, t: performance.now(), status: 499, bytes: 0 });
      return;
    }
    const r = this.route(rel, u.searchParams, mode, req, res);
    if (r === "sse") {
      return;
    }
    const { status, body, type, cache, key } = r;
    const headers: Record<string, string> = {
      "Content-Type": type,
      "Cache-Control": status >= 400 ? "no-store" : cache,
      "X-TSDB-Now": String(Math.round(this.data.now)),
      Vary: "Accept-Encoding",
    };
    let out: Buffer = Buffer.from(body);
    if ((this.opts.gzip ?? true) && /\bgzip\b/.test(String(req.headers["accept-encoding"] ?? "")) && body.byteLength > 256) {
      let c = key ? this.cache.get(key) : undefined;
      if (!c || !c.gz) {
        c = { body: out, gz: gzipSync(out, { level: 6 }) };
        if (key) {
          this.cache.set(key, c);
        }
      }
      out = c.gz!;
      headers["Content-Encoding"] = "gzip";
    }
    headers["Content-Length"] = String(out.byteLength);
    res.writeHead(status, headers);
    res.end(out);
    this.log.push({ path: path + u.search, t: performance.now(), status, bytes: out.byteLength });
  }

  private route(
    rel: string,
    q: URLSearchParams,
    mode: "server" | "static",
    req: IncomingMessage,
    res: ServerResponse,
  ): { status: number; body: Uint8Array; type: string; cache: string; key?: string } | "sse" {
    const json = (status: number, v: unknown, cache = "no-store"): { status: number; body: Uint8Array; type: string; cache: string } => ({
      status,
      body: new TextEncoder().encode(JSON.stringify(v)),
      type: "application/json",
      cache,
    });
    const err = (status: number, code: string, msg: string) => json(status, { error: msg, code });
    const bin = (body: Uint8Array, cache: string, key?: string) => ({ status: 200, body, type: "application/octet-stream", cache, key });
    const imm = "public, max-age=31536000, immutable";
    const parts = rel.split("/").map(decodeURIComponent);
    const extra = this.opts.files?.get(rel);
    if (extra) {
      return { status: 200, body: extra.body, type: extra.type, cache: "public, max-age=10" };
    }
    if ((mode === "server" && (rel === "catalog" || rel === "catalog.json")) || (mode === "static" && rel === "catalog.json")) {
      return json(200, { ...this.data.catalog, now: Math.round(this.data.now), static: mode === "static" }, "public, max-age=10");
    }
    const store = (id: string): MockDatastore | undefined => this.data.stores.get(id);
    switch (parts[0]) {
      case "movers": {
        const m = this.opts.moverDb ? moverRoute(this.opts.moverDb, parts) : null;
        return m ? { status: 200, ...m } : err(404, "not_found", "no mover data");
      }
      case "palette": {
        const s = store(parts[1]);
        const n = Number(parts[2]?.replace(/\.bin$/, ""));
        if (!s) {
          return err(404, "not_found", "unknown datastore");
        }
        if (!Number.isInteger(n) || n < 1 || n > s.palette.length) {
          return err(404, "not_found", "no such palette version");
        }
        return bin(encodePalette(s.palette, n), imm, `pal/${parts[1]}/${n}`);
      }
      case "view": {
        if (mode === "static") {
          return err(404, "not_found", "static exports have no view");
        }
        const s = store(parts[1]);
        if (!s) {
          return err(404, "not_found", "unknown datastore");
        }
        const z = Number(parts[2]);
        const bbox = (q.get("bbox") ?? "").split(",").map(Number) as [number, number, number, number];
        if (bbox.length !== 4 || bbox.some(n => !Number.isInteger(n))) {
          return err(400, "bad_request", "bbox");
        }
        if ((bbox[2] - bbox[0] + 1) * (bbox[3] - bbox[1] + 1) > 4096) {
          return err(400, "bad_request", "bbox too large");
        }
        const tq = q.get("t") ?? "latest";
        const t = tq === "latest" ? "latest" : Number(tq);
        const cache = t !== "latest" && t < s.ds.horizon ? imm : "public, max-age=2";
        return json(200, viewFor(s, z, bbox, t, this.data.now), cache);
      }
      case "timeline": {
        const s = store(parts[1]);
        if (!s) {
          return err(404, "not_found", "unknown datastore");
        }
        const z = Number(parts[2]);
        const date = (parts[3] ?? "").replace(/\.json$/, "");
        const tl = timelineFor(s, z, date);
        return json(200, tl, tl.final ? imm : "public, max-age=10");
      }
      case "tile": {
        const s = store(parts[1]);
        if (!s) {
          return err(404, "not_found", "unknown datastore");
        }
        const [z, x, y] = [Number(parts[2]), Number(parts[3]), Number(parts[4])];
        const verS = (parts[5] ?? "").replace(/\.bin$/, "");
        const tile = s.tiles.get(tk(z, x, y));
        if (verS === "live") {
          if (mode === "static" || z !== 0) {
            return err(404, "not_found", "no live tile");
          }
          const live = s.live.get(tk(z, x, y));
          const base = tile ? versionAt(tile, Number.POSITIVE_INFINITY) : null;
          if (live) {
            return bin(encodeTile(z, x, y, base?.v ?? 0, 1, live.pixels, live.paletteVersion, true), "no-store");
          }
          if (!base) {
            return err(404, "not_found", "no tile");
          }
          return bin(encodeTile(z, x, y, base.v, base.format, base.pixels, base.paletteVersion, true), "no-store");
        }
        const v = Number(verS);
        const ver = tile?.versions.find(d => d.v === v);
        if (!tile || !ver) {
          return err(404, "not_found", "no such version");
        }
        return bin(encodeTile(z, x, y, v, ver.format, ver.pixels, ver.paletteVersion), imm, `tile/${parts[1]}/${z}/${x}/${y}/${v}`);
      }
      case "tiledelta": {
        const s = store(parts[1]);
        if (!s) {
          return err(404, "not_found", "unknown datastore");
        }
        const [x, y] = [Number(parts[2]), Number(parts[3])];
        const m = /^(\d+)-(\d+)\.bin$/.exec(parts[4] ?? "");
        const tile = s.tiles.get(tk(0, x, y));
        if (!m || !tile) {
          return err(404, "not_found", "no tile");
        }
        const [from, to] = [Number(m[1]), Number(m[2])];
        if (this.opts.noDeltas) {
          return err(404, "not_found", "no deltas here");
        }
        if (mode === "static" && to !== from + 1) {
          return err(404, "not_found", "static exports only have consecutive deltas");
        }
        const a = tile.versions.find(d => d.v === from);
        const b = tile.versions.find(d => d.v === to);
        if (!a || !b || from >= to) {
          return err(404, "not_found", "no such version");
        }
        const fmt = a.format === 2 || b.format === 2 ? 2 : 1;
        return bin(encodeDelta(x, y, from, to, a.pixels, b.pixels, fmt, b.paletteVersion), imm, `delta/${parts[1]}/${x}/${y}/${from}-${to}`);
      }
      case "live": {
        if (mode === "static") {
          return err(404, "not_found", "static exports have no live events");
        }
        const ds = new Set(q.getAll("ds"));
        res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-store", "X-TSDB-Now": String(Math.round(this.data.now)) });
        const client = { res, ds };
        this.sse.add(client);
        const horizon: Record<string, number> = {};
        for (const id of ds) {
          const st = store(id);
          if (st) {
            horizon[id] = st.ds.horizon;
          }
        }
        res.write(`event: hello\ndata: ${JSON.stringify({ now: Math.round(this.data.now), horizon })}\n\n`);
        req.on("close", () => this.sse.delete(client));
        return "sse";
      }
    }
    return err(404, "not_found", `no route ${rel}`);
  }

  private async serveFile(path: string, req: IncomingMessage, res: ServerResponse): Promise<void> {
    const dir = this.opts.staticDir;
    if (!dir) {
      res.writeHead(404, { "Content-Type": "text/plain" });
      res.end("not found");
      return;
    }
    const root = resolve(dir);
    let p = normalize(join(root, decodeURIComponent(path)));
    if (!p.startsWith(root)) {
      res.writeHead(403);
      res.end();
      return;
    }
    if (path.endsWith("/")) {
      p = join(p, "index.html");
    }
    try {
      let body: Buffer = await readFile(p);
      const type = MIME[extname(p)] ?? "application/octet-stream";
      const headers: Record<string, string> = { "Content-Type": type, "Cache-Control": path.includes("/assets/") ? "public, max-age=31536000, immutable" : "no-cache", Vary: "Accept-Encoding" };
      // Compress text assets as a CDN or the Go server would.
      if (/^(text\/|application\/json)/.test(type) && /\bgzip\b/.test(String(req.headers["accept-encoding"] ?? ""))) {
        const key = `file:${p}`;
        let c = this.cache.get(key);
        if (!c) {
          c = { body, gz: gzipSync(body, { level: 9 }) };
          this.cache.set(key, c);
        }
        body = c.gz!;
        headers["Content-Encoding"] = "gzip";
      }
      headers["Content-Length"] = String(body.byteLength);
      res.writeHead(200, headers);
      res.end(body);
      this.log.push({ path, t: performance.now(), status: 200, bytes: body.byteLength });
    } catch {
      res.writeHead(404, { "Content-Type": "text/plain" });
      res.end("not found");
    }
  }
}
