// End-to-end check of serve-v3 with the embedded viewer in headless Chromium (WP6
// acceptance): open the viewer on a running server, wait until the terrain view is fully
// painted and the mover layer draws movers, and report what the browser fetched.
//
//   node internal/v3/api/e2e/viewer_e2e.mjs --url http://127.0.0.1:PORT [--map gw-bc41437d.1]
//     [--surface nauvis] [--x 0] [--y 0] [--s 1] [--t <ms>] [--shot out.png] [--timeout-ms 30000]
//
// playwright-core is resolved from bench/web/node_modules (`npm ci` there); the browser is
// the Playwright Chromium under %LOCALAPPDATA%/ms-playwright, $TSDB_BENCH_CHROME, or an
// installed Chrome. Prints one JSON object; exits 1 when a check fails.
import { createRequire } from "node:module";
import { existsSync, readdirSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const require = createRequire(resolve(here, "../../../../bench/web/package.json"));
const { chromium } = require("playwright-core");

function arg(name, def) {
  const i = process.argv.indexOf("--" + name);
  return i >= 0 && i + 1 < process.argv.length ? process.argv[i + 1] : def;
}
const base = arg("url");
if (!base) throw new Error("--url is required");
const timeoutMs = Number(arg("timeout-ms", "30000"));
const q = new URLSearchParams({ map: arg("map", "gw-bc41437d.1"), surface: arg("surface", "nauvis"), x: arg("x", "0"), y: arg("y", "0"), s: arg("s", "1") });
if (arg("t")) q.set("t", arg("t"));
const pageUrl = `${base.replace(/\/$/, "")}/?${q}`;

function chromePath() {
  if (process.env.TSDB_BENCH_CHROME) return process.env.TSDB_BENCH_CHROME;
  const pw = join(homedir(), "AppData", "Local", "ms-playwright");
  if (existsSync(pw)) {
    for (const d of readdirSync(pw).filter((d) => /^chromium-\d+$/.test(d)).sort().reverse()) {
      for (const sub of ["chrome-win64", "chrome-win", "chrome-linux"]) {
        const p = join(pw, d, sub, process.platform === "win32" ? "chrome.exe" : "chrome");
        if (existsSync(p)) return p;
      }
    }
  }
  return undefined;
}

const exe = chromePath();
const browser = await chromium.launch(exe ? { executablePath: exe, headless: true } : { channel: "chrome", headless: true });
const context = await browser.newContext({ viewport: { width: 1920, height: 1080 } });
const page = await context.newPage();
const errors = [];
const statuses = {};
const routes = {};
let apiBytes = 0;
page.on("pageerror", (e) => errors.push(`pageerror: ${String(e).slice(0, 300)}`));
page.on("console", (m) => {
  if (m.type() === "error") errors.push(`console: ${m.text().slice(0, 300)}`);
});
page.on("response", async (res) => {
  const u = new URL(res.url());
  if (!u.pathname.startsWith("/api/v3/")) return;
  const route = u.pathname.slice(8).split("/")[0];
  const key = `${route} ${res.status()}`;
  statuses[key] = (statuses[key] ?? 0) + 1;
  routes[route] = (routes[route] ?? 0) + 1;
  const len = Number(res.headers()["content-length"] ?? 0);
  apiBytes += len;
});

const t0 = Date.now();
await page.goto(pageUrl, { waitUntil: "load" });
const result = { url: pageUrl, browser: browser.version(), checks: {} };
async function waitFn(fn, arg, what) {
  try {
    await page.waitForFunction(fn, arg, { timeout: timeoutMs, polling: 50 });
    return true;
  } catch {
    errors.push(`timeout waiting for ${what}`);
    return false;
  }
}
result.checks.fullPaint = await waitFn(() => !!window.__tsdbPerf?.firstFullPaint, null, "view.full");
result.checks.moversDrawn = await waitFn(
  () => (window.__tsdbPerf?.events ?? []).some((e) => e.name === "movers.frame" && e.detail?.movers > 0),
  null,
  "movers.frame with movers",
);
const perf = await page.evaluate(() => {
  const p = window.__tsdbPerf;
  const ev = p?.events ?? [];
  const count = (n) => ev.filter((e) => e.name === n).length;
  const movers = ev.filter((e) => e.name === "movers.frame").map((e) => e.detail?.movers ?? 0);
  const full = ev.find((e) => e.name === "view.full");
  return {
    firstFullPaintMs: p?.firstFullPaint ?? null,
    viewFullTiles: full?.detail?.tiles ?? null,
    tileRequests: count("tile.request"),
    tilesLoaded: count("tile.loaded"),
    tilesDrawn: count("tile.drawn"),
    moverFrames: movers.length,
    maxMovers: movers.length ? Math.max(...movers) : 0,
    datastore: window.__tsdbControl?.datastore?.() ?? null,
    time: window.__tsdbControl?.time?.() ?? null,
  };
});
const shot = arg("shot");
if (shot) await page.screenshot({ path: shot });
result.wallMs = Date.now() - t0;
result.perf = perf;
result.api = { statuses, routes, bytes: apiBytes };
const serverErrors = Object.entries(statuses).filter(([k]) => /\s5\d\d$/.test(k));
result.checks.noServerErrors = serverErrors.length === 0;
result.checks.tilesLoaded = perf.tilesLoaded > 0;
result.checks.noPageErrors = !errors.some((e) => e.startsWith("pageerror"));
result.errors = errors;
result.ok = Object.values(result.checks).every(Boolean);
await browser.close();
console.log(JSON.stringify(result, null, 2));
process.exit(result.ok ? 0 : 1);
