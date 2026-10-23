// A small static file server for the client benchmark: the bench page and viewer bundle,
// plus a data directory (the illisis test data, or a v3 static export) served with the
// Cache-Control rules of the system being measured. Files are only read as bytes.
// Listens on 127.0.0.1 with a random free port (never the reserved 8880/4443/8080).
import { createReadStream, statSync } from "node:fs";
import { createServer } from "node:http";
import { extname, join, normalize, resolve, sep } from "node:path";

const RESERVED = new Set([8880, 4443, 8080]);

const TYPES = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".mjs": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".json": "application/json",
  ".png": "image/png",
  ".gz": "application/gzip",
  ".bin": "application/octet-stream",
};

/** illisis shared/keys.ts cacheControl(), for keys of its S3 tree. */
export function illisisCacheControl(key, asOf = Date.now()) {
  const day = /\/m\/(\d{4}-\d\d-\d\d)\.json\.gz$/.exec(key)?.[1];
  const today = new Date(asOf).toISOString().slice(0, 10);
  if (/\.[smck]\.gz$/.test(key) || (day !== undefined && day < today) || /^(viewer-[^/]+\.js|icons\/.+)$/.test(key)) {
    return "public, max-age=31536000, immutable";
  }
  if (key.endsWith("/live.json.gz")) {
    return "public, max-age=5";
  }
  return key === "index.html" ? "public, max-age=300" : "public, max-age=60";
}

/** API.md section 6 static export rules for a v3 export. */
export function v3CacheControl(key) {
  if (/^(tile|tiledelta|palette)\//.test(key) || /^movers\/[^/]+\/(window|keyframe)\//.test(key) || key.startsWith("assets/")) {
    return "public, max-age=31536000, immutable";
  }
  if (key.endsWith("/live.bin")) {
    return "public, max-age=5";
  }
  if (key.startsWith("overlays/")) {
    return "public, max-age=60";
  }
  return key === "index.html" ? "public, max-age=300" : "public, max-age=10";
}

/**
 * Starts the server. `routes` maps exact URL paths to files (the bench page and bundle);
 * everything else is looked up under `root`.
 */
export function serve({ root, routes = {}, cacheControl = () => "no-cache", encodings = {} }) {
  const base = resolve(root);
  const stats = { requests: 0, bytes: 0 };
  const server = createServer((req, res) => {
    if (req.method !== "GET" && req.method !== "HEAD") {
      res.writeHead(405).end();
      return;
    }
    const url = new URL(req.url, "http://x");
    let path = decodeURIComponent(url.pathname);
    let file = routes[path];
    let key = path.replace(/^\/+/, "");
    if (!file) {
      if (key === "") {
        key = "index.html";
      }
      file = normalize(join(base, key));
      if (file !== base && !file.startsWith(base + sep)) {
        res.writeHead(403).end();
        return;
      }
    }
    let st;
    try {
      st = statSync(file);
    } catch {
      res.writeHead(404, { "Cache-Control": "no-store" }).end();
      return;
    }
    if (!st.isFile()) {
      res.writeHead(404, { "Cache-Control": "no-store" }).end();
      return;
    }
    const headers = {
      "Content-Type": TYPES[extname(file)] ?? "application/octet-stream",
      "Content-Length": st.size,
      "Cache-Control": routes[path] ? "no-cache" : cacheControl(key),
      "Last-Modified": st.mtime.toUTCString(),
    };
    const enc = encodings[extname(file)];
    if (enc) {
      headers["Content-Encoding"] = enc;
    }
    stats.requests++;
    stats.bytes += st.size;
    res.writeHead(200, headers);
    if (req.method === "HEAD") {
      res.end();
      return;
    }
    createReadStream(file).pipe(res);
  });
  return new Promise((ok, fail) => {
    const tryListen = (attempt) => {
      server.once("error", fail);
      server.listen(0, "127.0.0.1", () => {
        const port = server.address().port;
        if (RESERVED.has(port) && attempt < 10) {
          server.close(() => tryListen(attempt + 1));
          return;
        }
        ok({ url: `http://127.0.0.1:${port}`, stats, close: () => new Promise((r) => { server.close(r); server.closeAllConnections(); }) });
      });
    };
    tryListen(0);
  });
}
