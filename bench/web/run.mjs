// Client benchmark runner (PLAN.md section 5, Client rows). Drives a viewer in headless
// Chromium through window.__tsdbPerf (paint events) and window.__tsdbControl (view, time,
// timelapse), with network emulation (default 40 ms RTT, 50 Mbit/s), and writes a result
// file in the bench format.
//
//   node run.mjs --viewer illisis --data <illisis export> [--runs 5] [--out r.json]
//   node run.mjs --viewer v3 --url http://127.0.0.1:PORT/ [--ds <datastore id>] ...
//   node run.mjs --viewer v3 --static <v3 static export dir with index.html> ...
//
// Phases, per cold run (fresh browser context, empty cache): first full paint of the z0
// view at (x, y), then zoom out to the whole surface. Then in one warm context: warm
// first paints (reloads), a time scrub of -5 min steps, a 10 s timelapse at 1 h/s, and
// memory.
import { mkdirSync, writeFileSync, existsSync, readdirSync } from "node:fs";
import { cpus, platform, release, arch, homedir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright-core";

import { illisisCacheControl, serve, v3CacheControl } from "./server.mjs";
import { summarize } from "./stats.mjs";

const here = dirname(fileURLToPath(import.meta.url));
function arg(name, def) {
  const i = process.argv.indexOf("--" + name);
  return i >= 0 && i + 1 < process.argv.length ? process.argv[i + 1] : def;
}
const viewer = arg("viewer", "illisis");
const runs = Number(arg("runs", "5"));
const rtt = Number(arg("rtt", "40"));
const mbit = Number(arg("mbit", "50"));
const width = Number(arg("width", "1920"));
const height = Number(arg("height", "1080"));
const label = arg("label", viewer);
const scenario = arg("scenario", "testdata");
const out = arg("out");
const timeoutMs = Number(arg("timeout-ms", "60000"));
const scrubSteps = Number(arg("scrub-steps", "10"));
const view = { x: Number(arg("x", "0")), y: Number(arg("y", "0")), s: Number(arg("s", "1")) };

const notes = [];
const metrics = {};
const started = new Date();

// ---------------------------------------------------------------- server
let srv;
let pageUrl;
if (viewer === "illisis") {
  const data = arg("data");
  if (!data) throw new Error("--data <illisis export root> is required");
  const dist = join(here, "dist", "illisis");
  if (!existsSync(join(dist, "viewer.js"))) throw new Error("run `node build.mjs` first");
  srv = await serve({
    root: data,
    routes: { "/": join(dist, "index.html"), "/index.html": join(dist, "index.html"), "/__bench/viewer.js": join(dist, "viewer.js") },
    cacheControl: illisisCacheControl,
  });
  const q = new URLSearchParams({ map: arg("map", "gw-bc41437d~2E1"), surface: arg("surface", "nauvis"), x: view.x, y: view.y, s: view.s });
  pageUrl = `${srv.url}/?${q}`;
  notes.push("illisis viewer built from the illisis repo source (bench/web/illisis/entry.ts); served over HTTP/1.1 (production uses h2 behind a CDN)");
} else if (viewer === "v3") {
  const st = arg("static");
  if (st) {
    srv = await serve({ root: st, cacheControl: v3CacheControl });
    pageUrl = srv.url + "/";
  } else {
    pageUrl = arg("url");
    if (!pageUrl) throw new Error("--url or --static is required for --viewer v3");
  }
  const q = new URLSearchParams();
  for (const k of ["ds", "x", "y", "z", "t"]) if (arg(k)) q.set(k, arg(k));
  if ([...q].length) pageUrl += (pageUrl.includes("?") ? "&" : "?") + q;
} else {
  throw new Error(`unknown viewer ${viewer}`);
}

// ---------------------------------------------------------------- browser
function chromePath() {
  if (process.env.TSDB_BENCH_CHROME) return process.env.TSDB_BENCH_CHROME;
  const pw = join(homedir(), "AppData", "Local", "ms-playwright");
  if (existsSync(pw)) {
    for (const d of readdirSync(pw).filter((d) => /^chromium-\d+$/.test(d)).sort().reverse()) {
      for (const sub of ["chrome-win64", "chrome-win"]) {
        const p = join(pw, d, sub, "chrome.exe");
        if (existsSync(p)) return p;
      }
    }
  }
  return undefined;
}
async function launch() {
  const exe = chromePath();
  try {
    return await chromium.launch(exe ? { executablePath: exe, headless: true } : { channel: "chrome", headless: true });
  } catch (e) {
    if (exe) return chromium.launch({ channel: "chrome", headless: true });
    throw e;
  }
}
const browser = await launch();
metrics["web.browser_version"] = Number(browser.version().split(".")[0]) || 0;
notes.push(`browser ${browser.version()}, ${width}x${height}, ${rtt} ms RTT, ${mbit} Mbit/s (CDP network emulation)`);

async function openPage(context) {
  const page = await context.newPage();
  const cdp = await context.newCDPSession(page);
  await cdp.send("Network.enable");
  await cdp.send("Network.emulateNetworkConditions", {
    offline: false, latency: rtt, downloadThroughput: (mbit * 1e6) / 8, uploadThroughput: (mbit * 1e6) / 8,
  });
  await cdp.send("Performance.enable");
  const net = { bytes: 0, requests: 0, fromCache: 0 };
  cdp.on("Network.loadingFinished", (e) => {
    net.bytes += e.encodedDataLength;
    net.requests++;
  });
  cdp.on("Network.requestServedFromCache", () => net.fromCache++);
  page.on("pageerror", (e) => notes.push(`page error: ${String(e).slice(0, 200)}`));
  return { page, cdp, net };
}

async function waitFor(page, fn, arg, what) {
  try {
    await page.waitForFunction(fn, arg, { timeout: timeoutMs, polling: 16 });
    return true;
  } catch {
    notes.push(`timeout (${timeoutMs} ms) waiting for ${what}`);
    return false;
  }
}

const hasControl = (page) => page.evaluate(() => !!window.__tsdbControl);

/** Waits for the next view.full (and view.fallback) after `since`; returns ms after since. */
async function nextPaint(page, since, what) {
  const ok = await waitFor(page, (s) => (window.__tsdbPerf?.events ?? []).some((e) => e.name === "view.full" && e.t >= s), since, what);
  if (!ok) return {};
  return page.evaluate((s) => {
    const ev = window.__tsdbPerf.events;
    const full = ev.find((e) => e.name === "view.full" && e.t >= s);
    const fb = ev.find((e) => e.name === "view.fallback" && e.t >= s);
    return { full: full.t - s, fallback: fb ? fb.t - s : undefined };
  }, since);
}

/** The whole-surface view: the bounding box of the surface's z0 areas (illisis catalog). */
async function wholeSurfaceView(page) {
  return page.evaluate(([w, h]) => {
    const c = window.__tsdbControl;
    if (c.surfaceBounds) {
      const [x0, y0, x1, y1] = c.surfaceBounds();
      return { x: (x0 + x1) / 2, y: (y0 + y1) / 2, s: Math.max((x1 - x0) / w, (y1 - y0) / h) * 1.05 };
    }
    const cat = c.catalog();
    const sel = c.selection();
    const areas = cat.maps[sel.map].surfaces[sel.surface].forces[sel.force].areas[0];
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (const a of areas) {
      const [ax, ay] = a.split("_").map(Number);
      x0 = Math.min(x0, ax * 512 - 256); y0 = Math.min(y0, ay * 512 - 256);
      x1 = Math.max(x1, ax * 512 + 256); y1 = Math.max(y1, ay * 512 + 256);
    }
    return { x: (x0 + x1) / 2, y: (y0 + y1) / 2, s: Math.max((x1 - x0) / w, (y1 - y0) / h) * 1.05 };
  }, [width, height]);
}

/**
 * Checks that the v3 viewer shows what was asked for (--ds, --x, --y, --z), so its
 * numbers compare with the illisis run (gridworld nauvis, z0, centred on 0, 0). A
 * mismatch is an error; what the viewer cannot report is noted once.
 */
async function checkV3View(page, run) {
  const want = { ds: arg("ds"), x: arg("x"), y: arg("y"), z: arg("z") };
  if (!want.ds && want.x === undefined && want.z === undefined) return;
  const got = await page.evaluate(() => {
    const c = window.__tsdbControl;
    if (!c) return null;
    return { view: c.view ? c.view() : null, ds: c.datastore ? c.datastore() : null };
  });
  if (!got) {
    if (run === 0) notes.push("viewer has no window.__tsdbControl: the shown datastore and view were not verified");
    return;
  }
  if (want.ds) {
    if (got.ds == null) {
      if (run === 0) notes.push("viewer has no __tsdbControl.datastore(): the shown datastore was not verified");
    } else if (got.ds !== want.ds) {
      throw new Error(`viewer shows datastore ${got.ds}, not ${want.ds}`);
    }
  }
  if (got.view) {
    const v = got.view;
    if (want.x !== undefined && Math.abs(v.x - Number(want.x)) > 1) throw new Error(`viewer centre x ${v.x}, not ${want.x}`);
    if (want.y !== undefined && Math.abs(v.y - Number(want.y)) > 1) throw new Error(`viewer centre y ${v.y}, not ${want.y}`);
    if (want.z !== undefined) {
      const s = 2 ** -Number(want.z);
      if (Math.abs(v.s - s) > s * 0.01) throw new Error(`viewer scale ${v.s} world tiles per pixel, not ${s} (z ${want.z})`);
    }
    if (run === 0 && (v.width !== width || v.height !== height)) notes.push(`viewer canvas ${v.width}x${v.height}, page ${width}x${height}`);
  } else if (run === 0) {
    notes.push("viewer has no __tsdbControl.view(): the shown view was not verified");
  }
}

// ---------------------------------------------------------------- cold runs
const coldPaint = [], coldBytes = [], coldReqs = [], zoomFull = [], zoomFallback = [];
for (let i = 0; i < runs; i++) {
  const context = await browser.newContext({ viewport: { width, height }, deviceScaleFactor: 1 });
  const { page, net } = await openPage(context);
  await page.goto(pageUrl, { waitUntil: "commit" });
  if (await waitFor(page, () => window.__tsdbPerf?.firstFullPaint != null, undefined, `first full paint (cold run ${i + 1})`)) {
    if (viewer === "illisis") {
      const sel = await page.evaluate(() => window.__tsdbControl.selection());
      if (sel.map !== arg("map", "gw-bc41437d~2E1") || sel.surface !== arg("surface", "nauvis")) {
        throw new Error(`viewer shows ${sel.map}/${sel.surface}, not the requested surface`);
      }
    } else {
      await checkV3View(page, i);
    }
    coldPaint.push(await page.evaluate(() => window.__tsdbPerf.firstFullPaint));
    coldBytes.push(net.bytes);
    coldReqs.push(net.requests);
  }
  if (await hasControl(page)) {
    const v = await wholeSurfaceView(page);
    const since = await page.evaluate((v) => {
      const t = performance.now();
      window.__tsdbControl.setView(v.x, v.y, v.s);
      return t;
    }, v);
    const p = await nextPaint(page, since, `zoom-out full paint (cold run ${i + 1})`);
    if (p.full !== undefined) {
      zoomFull.push(p.full);
      zoomFallback.push(p.fallback ?? p.full);
    }
  } else if (i === 0) {
    notes.push("viewer has no window.__tsdbControl: zoom, scrub, timelapse phases skipped");
  }
  await context.close();
}
function put(prefix, xs) {
  const s = summarize(xs);
  for (const [k, v] of Object.entries(s)) metrics[`${prefix}.${k}`] = v;
}
put("web.first_full_paint_cold_ms", coldPaint);
put("web.cold_bytes", coldBytes);
put("web.cold_requests", coldReqs);
put("web.zoomout_full_ms", zoomFull);
put("web.zoomout_fallback_ms", zoomFallback);
if (viewer === "illisis") {
  notes.push("illisis has no fallback drawing, so zoomout_fallback_ms equals zoomout_full_ms (black until each area's chain lands)");
}

// ---------------------------------------------------------------- warm context
{
  const context = await browser.newContext({ viewport: { width, height }, deviceScaleFactor: 1 });
  const { page, cdp, net } = await openPage(context);
  await page.goto(pageUrl, { waitUntil: "commit" });
  await waitFor(page, () => window.__tsdbPerf?.firstFullPaint != null, undefined, "first paint (warm-up)");
  const warm = [];
  for (let i = 0; i < runs; i++) {
    await page.reload({ waitUntil: "commit" });
    if (await waitFor(page, () => window.__tsdbPerf?.firstFullPaint != null, undefined, `first full paint (warm run ${i + 1})`)) {
      warm.push(await page.evaluate(() => window.__tsdbPerf.firstFullPaint));
    }
  }
  put("web.first_full_paint_warm_ms", warm);

  if (await hasControl(page)) {
    // Scrub: from 1 h before the end, steps of -5 min at the z0 view.
    const [first, end] = await page.evaluate(() => window.__tsdbControl.range());
    let t = end - 3600_000;
    let since = await page.evaluate((t) => { const s = performance.now(); window.__tsdbControl.seek(t); return s; }, t);
    await nextPaint(page, since, "scrub start");
    const scrub = [], scrubBytes = [];
    for (let i = 0; i < scrubSteps && t - 300_000 > first; i++) {
      t -= 300_000;
      const b0 = net.bytes;
      since = await page.evaluate((t) => { const s = performance.now(); window.__tsdbControl.seek(t); return s; }, t);
      const p = await nextPaint(page, since, `scrub step ${i + 1}`);
      if (p.full !== undefined) {
        scrub.push(p.full);
        scrubBytes.push(net.bytes - b0);
      }
    }
    put("web.scrub_ms", scrub);
    put("web.scrub_bytes", scrubBytes);

    // Timelapse at 1 h/s for 10 s, from 24 h before the end (or the start).
    const t0 = Math.max(first, end - 24 * 3600_000);
    since = await page.evaluate((t) => { const s = performance.now(); window.__tsdbControl.seek(t); return s; }, t0);
    await nextPaint(page, since, "timelapse start");
    const before = await page.evaluate(() => ({ frames: window.__tsdbPerf.counters.framesDrawn, long: window.__tsdbPerf.counters.longTasks, t: performance.now() }));
    await page.evaluate(() => window.__tsdbControl.play(3600));
    await page.waitForTimeout(10_000);
    const after = await page.evaluate(() => {
      window.__tsdbControl.play(0);
      return { frames: window.__tsdbPerf.counters.framesDrawn, long: window.__tsdbPerf.counters.longTasks, t: performance.now() };
    });
    const secs = (after.t - before.t) / 1000;
    metrics["web.timelapse_fps"] = (after.frames - before.frames) / secs;
    metrics["web.timelapse_longtasks_per_10s"] = ((after.long - before.long) * 10) / secs;

    // Memory: JS heap plus the viewer's own texture/canvas estimate.
    const pm = await cdp.send("Performance.getMetrics");
    const heap = pm.metrics.find((m) => m.name === "JSHeapUsedSize")?.value ?? NaN;
    const tex = await page.evaluate(() => window.__tsdbPerf.counters.textureBytes ?? 0);
    metrics["web.js_heap_mib"] = heap / 2 ** 20;
    metrics["web.texture_mib"] = tex / 2 ** 20;
    metrics["web.memory_mib"] = (heap + tex) / 2 ** 20;
    if (viewer === "illisis") {
      notes.push("illisis texture_mib estimates its decoded areas: 1 MiB RGBA + 1 MiB OffscreenCanvas each (not in the JS heap)");
    }
  }
  await context.close();
}

await browser.close();
if (srv) await srv.close();

const result = {
  tool: "webbench",
  label,
  target: viewer,
  scenario,
  started: started.toISOString(),
  seconds: (Date.now() - started.getTime()) / 1000,
  machine: { os: `${platform()} ${release()}`, arch: arch(), cpu: cpus()[0]?.model ?? "", cpus: cpus().length, go: `node ${process.version}` },
  params: { viewer, runs, rtt, mbit, width, height, view, url: pageUrl.replace(/127\.0\.0\.1:\d+/, "127.0.0.1:PORT") },
  metrics,
  notes,
};
for (const k of Object.keys(metrics).sort()) console.log(`  ${k.padEnd(44)} ${Number.isInteger(metrics[k]) ? metrics[k] : metrics[k]?.toFixed?.(2)}`);
for (const n of notes) console.log(`  note: ${n}`);
if (out) {
  mkdirSync(dirname(resolve(out)), { recursive: true });
  writeFileSync(out, JSON.stringify(result, (_k, v) => (typeof v === "number" && !Number.isFinite(v) ? null : v), 2) + "\n");
}
