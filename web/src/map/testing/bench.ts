// Browser benchmark of the viewer against the mock server (PLAN.md §5 client rows), driven
// through Chrome DevTools Protocol with network emulation (40 ms RTT, 50 Mbit/s):
// first full paint (cold and warm), zoom out to the whole surface, time scrub steps,
// timelapse frame rate, long tasks and memory. Reads window.__tsdbPerf and window.__tsdbMap.
//
//   npm --prefix web run build
//   npm i --no-save --prefix <dir> playwright-core
//   node src/map/testing/bench.ts --pw <dir>/node_modules/playwright-core --chrome <chrome.exe>
//        --illisis <export root> [--map gw-bc41437d~2E1 --surface nauvis] [--runs 10]
//        [--renderer auto|canvas2d] [--loader worker|direct] [--rtt 40] [--mbit 50]
//        [--h2] (HTTP/2 over TLS, self-signed via openssl) [--static] (static export paths)
//        [--diag] (print one cold load's request timeline to stderr)
//
// Prints a JSON report. Node only. WP11's Playwright suite is the authoritative harness;
// this one exists so WP7 could measure before WP6 landed.

import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { MockServer } from "./mockserver.ts";
import { loadDataset, parseArgs } from "./serve.ts";

interface PerfEv {
  name: string;
  t: number;
  detail?: Record<string, unknown>;
}

function pct(xs: number[], p: number): number {
  if (xs.length === 0) {
    return NaN;
  }
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.min(s.length - 1, Math.max(0, Math.ceil((p / 100) * s.length) - 1))];
}

function summary(xs: number[]): { n: number; p50: number; p95: number; min: number; max: number } {
  const r = (v: number) => Math.round(v * 10) / 10;
  return { n: xs.length, p50: r(pct(xs, 50)), p95: r(pct(xs, 95)), min: r(Math.min(...xs)), max: r(Math.max(...xs)) };
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2));
  const pwPath = args.get("pw");
  const chrome = args.get("chrome");
  if (!pwPath || !chrome) {
    throw new Error("--pw <playwright-core dir> and --chrome <executable> are required");
  }
  const { chromium } = (await import(pathToFileURL(resolve(pwPath, "index.mjs")).href)) as typeof import("playwright-core");
  const runs = Number(args.get("runs") ?? 10);
  const rtt = Number(args.get("rtt") ?? 40);
  const mbit = Number(args.get("mbit") ?? 50);
  const renderer = args.get("renderer") ?? "auto";
  const loader = args.get("loader") ?? "auto";
  const data = await loadDataset(args);
  const st = [...data.stores.values()][0];
  const mapKey = data.catalog.maps[0].key;
  const surface = data.catalog.maps[0].surfaces[0].name;
  const dist = resolve(fileURLToPath(new URL("../../../dist/", import.meta.url)));
  let tls: { key: Buffer; cert: Buffer } | undefined;
  if (args.has("h2")) {
    // A throwaway self-signed certificate for HTTP/2 (browsers only speak h2 over TLS).
    const dir = mkdtempSync(join(tmpdir(), "tsdb-bench-"));
    const r = spawnSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", join(dir, "key.pem"), "-out", join(dir, "cert.pem"), "-days", "1", "-subj", "/CN=127.0.0.1"], { encoding: "utf8" });
    if (r.status !== 0) {
      throw new Error(`openssl failed: ${r.stderr}`);
    }
    tls = { key: readFileSync(join(dir, "key.pem")), cert: readFileSync(join(dir, "cert.pem")) };
  }
  const server = await new MockServer(data, { staticDir: dist, tls }).start(0);
  const browser = await chromium.launch({
    executablePath: chrome,
    headless: true,
    args: ["--use-angle=d3d11", "--enable-gpu-rasterization", "--ignore-gpu-blocklist", ...(tls ? ["--ignore-certificate-errors"] : [])],
  });
  const q = new URLSearchParams({ map: mapKey, surface, force: st.ds.force, x: "0", y: "0", s: "1", renderer, loader, layers: "" });
  if (args.has("static")) {
    // The static export layout under /static/ (timelines instead of view, no live tiles).
    q.set("source", "static");
    q.set("data", "/static/");
  }
  const url = `${server.url}/?${q}`;
  const report: Record<string, unknown> = { url, source: args.has("static") ? "static" : "server", protocol: tls ? "h2" : "http/1.1", rtt, mbit, runs, renderer, loader, dataset: { ds: st.ds.id, tiles: st.tiles.size, palette: st.palette.length } };

  const throttle = async (cdp: import("playwright-core").CDPSession, cold: boolean): Promise<void> => {
    await cdp.send("Network.enable");
    await cdp.send("Network.setCacheDisabled", { cacheDisabled: cold });
    // --mbit 0 disables the bandwidth limit (-1 in CDP).
    const bps = mbit > 0 ? (mbit * 1e6) / 8 : -1;
    await cdp.send("Network.emulateNetworkConditions", { offline: false, latency: rtt, downloadThroughput: bps, uploadThroughput: bps });
  };
  const waitFull = (page: import("playwright-core").Page, since: number, timeout = 30000) =>
    page.waitForFunction((s: number) => (window as unknown as { __tsdbPerf: { events: PerfEv[] } }).__tsdbPerf.events.some(e => e.name === "view.full" && e.t >= s), since, { timeout });
  const events = (page: import("playwright-core").Page): Promise<PerfEv[]> => page.evaluate(() => (window as unknown as { __tsdbPerf: { events: PerfEv[] } }).__tsdbPerf.events);
  const nowIn = (page: import("playwright-core").Page): Promise<number> => page.evaluate(() => performance.now());

  // 1. First full paint, cold (no HTTP cache) and warm (reload with cache).
  const cold: number[] = [];
  const warm: number[] = [];
  const coldBytes: number[] = [];
  let glInfo = "";
  for (let i = 0; i < runs; i++) {
    const ctx = await browser.newContext({ viewport: { width: 1920, height: 1080 }, deviceScaleFactor: 1 });
    const page = await ctx.newPage();
    const cdp = await ctx.newCDPSession(page);
    await throttle(cdp, true);
    const problems: string[] = [];
    page.on("requestfailed", r => problems.push(`requestfailed ${r.failure()?.errorText} ${r.url().replace(server.url, "")}`));
    page.on("console", m => {
      if (m.type() === "error" || m.type() === "warning") {
        problems.push(`console.${m.type()} ${m.text()}`);
      }
    });
    page.on("pageerror", e => problems.push(`pageerror ${e.message}`));
    server.clearLog();
    await page.goto(url);
    try {
      await page.waitForFunction(() => (window as unknown as { __tsdbPerf?: { firstFullPaint: number | null } }).__tsdbPerf?.firstFullPaint != null, undefined, { timeout: 15000 });
    } catch (err) {
      const dump = await page.evaluate(() => {
        const w = window as unknown as { __tsdbMap?: { state(): unknown }; __tsdbPerf?: { events: PerfEv[] } };
        return { state: w.__tsdbMap?.state(), events: w.__tsdbPerf?.events.slice(-40) };
      });
      console.error("first paint timeout", JSON.stringify(dump, null, 1), server.log.map(l => `${l.status} ${l.path}`).join("\n"));
      throw err;
    }
    cold.push(await page.evaluate(() => (window as unknown as { __tsdbPerf: { firstFullPaint: number } }).__tsdbPerf.firstFullPaint));
    if (cold[cold.length - 1] > 1000 || problems.length > 0) {
      const ev = await page.evaluate(() => (window as unknown as { __tsdbPerf: { events: PerfEv[] } }).__tsdbPerf.events.map(e => `${Math.round(e.t)} ${e.name} ${JSON.stringify(e.detail ?? {})}`));
      console.error(`slow or noisy cold run ${i}: ${Math.round(cold[cold.length - 1])} ms`, JSON.stringify({ problems, server: server.log.map(l => `${Math.round(l.t)} ${l.status} ${l.bytes} ${l.path}`), events: ev }, null, 1));
    }
    // Bytes on the wire (after Content-Encoding) as the server sent them, up to first full paint.
    coldBytes.push(server.log.reduce((n, l) => n + l.bytes, 0));
    if (i === 0) {
      report.first_paint_requests = server.log.map(l => `${l.status} ${l.bytes} ${l.path.replace(/^\/api\/v3\//, "").slice(0, 80)}`);
    }
    if (i === 0 && args.has("diag")) {
      const diag = await page.evaluate(() => {
        const nav = performance.getEntriesByType("navigation")[0] as PerformanceNavigationTiming;
        const res = (performance.getEntriesByType("resource") as PerformanceResourceTiming[]).map(e => ({
          name: e.name.replace(location.origin, "").slice(0, 90),
          start: Math.round(e.startTime),
          ttfb: Math.round(e.responseStart),
          end: Math.round(e.responseEnd),
          proto: e.nextHopProtocol,
          bytes: e.transferSize,
        }));
        const ev = (window as unknown as { __tsdbPerf: { events: PerfEv[] } }).__tsdbPerf.events.map(e => `${Math.round(e.t)} ${e.name} ${JSON.stringify(e.detail ?? {})}`);
        return { nav: { responseEnd: Math.round(nav.responseEnd), domInteractive: Math.round(nav.domInteractive), domContentLoaded: Math.round(nav.domContentLoadedEventEnd) }, res, ev };
      });
      console.error(JSON.stringify(diag, null, 1));
    }
    if (i === 0) {
      glInfo = await page.evaluate(() => {
        const st = (window as unknown as { __tsdbMap: { state(): { renderer: string; loader: string; tiles: unknown } } }).__tsdbMap.state();
        const c = document.createElement("canvas").getContext("webgl2");
        const ext = c?.getExtension("WEBGL_debug_renderer_info");
        return `${st.renderer}/${st.loader} ${ext ? c!.getParameter(ext.UNMASKED_RENDERER_WEBGL) : "no webgl2"} ${JSON.stringify(st.tiles)}`;
      });
    }
    await cdp.send("Network.setCacheDisabled", { cacheDisabled: false });
    await page.reload();
    await page.waitForFunction(() => (window as unknown as { __tsdbPerf?: { firstFullPaint: number | null } }).__tsdbPerf?.firstFullPaint != null, undefined, { timeout: 30000 });
    // Warm: navigate again with the HTTP cache primed by the reload above.
    await page.reload();
    await page.waitForFunction(() => (window as unknown as { __tsdbPerf?: { firstFullPaint: number | null } }).__tsdbPerf?.firstFullPaint != null, undefined, { timeout: 30000 });
    warm.push(await page.evaluate(() => (window as unknown as { __tsdbPerf: { firstFullPaint: number } }).__tsdbPerf.firstFullPaint));
    await ctx.close();
  }
  report.renderer_info = glInfo;
  report.first_full_paint_cold_ms = summary(cold);
  report.first_full_paint_warm_ms = summary(warm);
  report.first_paint_cold_bytes = summary(coldBytes);

  // 2-5 in one page (cold cache).
  const ctx = await browser.newContext({ viewport: { width: 1920, height: 1080 }, deviceScaleFactor: 1 });
  const page = await ctx.newPage();
  const cdp = await ctx.newCDPSession(page);
  await throttle(cdp, false);
  await page.goto(url);
  await waitFull(page, 0);
  await page.waitForTimeout(1500); // let prefetch settle

  // 2. Zoom out to the whole surface, and back in.
  const zoom: { fallback: number; full: number }[] = [];
  for (let i = 0; i < 3; i++) {
    for (const action of ["fit", "in"] as const) {
      const t0 = await nowIn(page);
      await page.evaluate(a => {
        const m = (window as unknown as { __tsdbMap: { fit(): void; setCamera(x: number, y: number, s: number): void } }).__tsdbMap;
        if (a === "fit") {
          m.fit();
        } else {
          m.setCamera(0, 0, 1);
        }
      }, action);
      await waitFull(page, t0);
      const ev = (await events(page)).filter(e => e.t >= t0);
      const fb = ev.find(e => e.name === "view.fallback");
      const fu = ev.find(e => e.name === "view.full");
      if (action === "fit") {
        zoom.push({ fallback: (fb?.t ?? NaN) - t0, full: (fu?.t ?? NaN) - t0 });
      }
      await page.waitForTimeout(300);
    }
  }
  report.zoom_out_fallback_ms = summary(zoom.map(z => z.fallback));
  report.zoom_out_full_ms = summary(zoom.map(z => z.full));
  report.zoom_out_note = "first zoom-out is cold (coarse tiles not yet loaded except the prefetched parent level); later ones hit the texture cache";
  report.zoom_out_first = zoom[0];

  // 3. Time scrub: step through the z0 version times (each step changes at least one tile).
  const times = [...new Set([...st.tiles.values()].filter(t => t.z === 0).flatMap(t => t.versions.map(v => v.t)))].sort((a, b) => a - b);
  const steps = times.slice(-12);
  const scrub: { ms: number; bytes: number; changed: number; changedFullBytes: number }[] = [];
  await page.evaluate(t => (window as unknown as { __tsdbMap: { setTime(t: number): void } }).__tsdbMap.setTime(t), steps[0]);
  await page.waitForTimeout(2000);
  for (let i = 1; i < steps.length; i++) {
    const before = steps[i - 1];
    const t = steps[i];
    // Tiles visible at x=0,y=0,s=1 (1920x1080): z0 x -2..2, y -1..1 (clipped to bounds).
    let changed = 0;
    let changedFull = 0;
    for (const tile of st.tiles.values()) {
      if (tile.z !== 0 || tile.x < -3 || tile.x > 3 || tile.y < -2 || tile.y > 2) {
        continue;
      }
      const va = tile.versions.filter(v => v.t <= before).pop();
      const vb = tile.versions.filter(v => v.t <= t).pop();
      if (va?.v !== vb?.v) {
        changed++;
        changedFull += 32 + 262144;
      }
    }
    server.clearLog();
    const t0 = await nowIn(page);
    await page.evaluate(tt => (window as unknown as { __tsdbMap: { setTime(t: number): void } }).__tsdbMap.setTime(tt), t);
    await waitFull(page, t0);
    const ev = (await events(page)).filter(e => e.t >= t0);
    const fu = ev.find(e => e.name === "view.full");
    await page.waitForTimeout(400);
    const bytes = server.log.reduce((n, l) => n + l.bytes, 0);
    scrub.push({ ms: (fu?.t ?? NaN) - t0, bytes, changed, changedFullBytes: changedFull });
  }
  report.scrub_step_ms = summary(scrub.map(s => s.ms));
  report.scrub_steps = scrub;

  // 4. Timelapse at 1 h/s for 10 s (starting 10 h before the end, or at the start).
  await page.evaluate(t => (window as unknown as { __tsdbMap: { setTime(t: number): void } }).__tsdbMap.setTime(t), Math.max(st.ds.first, st.ds.last - 10 * 3600_000));
  await page.waitForTimeout(1500);
  const tl0 = await nowIn(page);
  await page.evaluate(() => (window as unknown as { __tsdbMap: { play(s: number): void } }).__tsdbMap.play(3_600_000));
  await page.waitForTimeout(10_000);
  await page.evaluate(() => (window as unknown as { __tsdbMap: { pause(): void } }).__tsdbMap.pause());
  const tl1 = await nowIn(page);
  const tev = (await events(page)).filter(e => e.t >= tl0 && e.t <= tl1);
  const frames = tev.filter(e => e.name === "timelapse.frame");
  const longTasks = tev.filter(e => e.name === "longtask");
  const frameMs = frames.map(f => Number(f.detail?.ms ?? 0));
  const loaded = tev.filter(e => e.name === "tile.loaded");
  report.timelapse = {
    tiles_loaded: loaded.length,
    deltas_loaded: loaded.filter(e => /@\d+-\d+$/.test(String(e.detail?.key))).length,
    seconds: Math.round((tl1 - tl0) / 100) / 10,
    fps: Math.round((frames.length / ((tl1 - tl0) / 1000)) * 10) / 10,
    frame_work_ms: summary(frameMs),
    long_tasks_over_50ms: longTasks.length,
  };

  // 5. Memory.
  report.memory = await page.evaluate(() => {
    const p = (window as unknown as { __tsdbPerf: { counters: { textureBytes: number } } }).__tsdbPerf;
    const mem = (performance as unknown as { memory?: { usedJSHeapSize: number } }).memory;
    return { jsHeapMiB: mem ? Math.round(mem.usedJSHeapSize / 1048576) : null, textureMiB: Math.round(p.counters.textureBytes / 1048576) };
  });
  await ctx.close();
  await browser.close();
  await server.stop();
  console.log(JSON.stringify(report, null, 2));
}

void main().catch(err => {
  console.error(err);
  process.exit(1);
});
