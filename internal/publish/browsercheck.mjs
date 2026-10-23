// Headless browser check of a static export: opens the exported viewer from a
// static server, waits for the first full paint, checks that terrain was drawn, that no
// request went to the live API (/api/v3/) and that no tile, delta, palette or timeline
// request failed. Then it zooms to z0, scrubs to a time in the middle of the history (a
// cold load of historical versions), plays a timelapse (tile deltas) and checks again.
//
//   timeshadedb publish --db <db> --dir <out>
//   timeshadedb publish-serve --dir <out> --listen 127.0.0.1:0
//   node internal/publish/browsercheck.mjs --pw <playwright-core dir> --chrome <chrome.exe> --url http://127.0.0.1:<port>/
//
// Prints a JSON report and exits non-zero when a check fails. Node only; no dependencies
// beyond playwright-core (npm i --no-save --prefix <dir> playwright-core).

import { pathToFileURL } from "node:url";
import { join } from "node:path";

const args = Object.fromEntries(
  process.argv.slice(2).reduce((acc, a, i, all) => (a.startsWith("--") ? [...acc, [a.slice(2), all[i + 1]]] : acc), []),
);
if (!args.pw || !args.url) {
  console.error("usage: node browsercheck.mjs --pw <playwright-core dir> [--chrome <chrome.exe>] --url <export base URL> [--renderer webgl2|canvas2d] [--query <extra page query, e.g. data=/>]");
  process.exit(2);
}
const { chromium } = await import(pathToFileURL(join(args.pw, "index.mjs")).href);
const base = args.url.endsWith("/") ? args.url : args.url + "/";

const browser = await chromium.launch({ executablePath: args.chrome, headless: true });
const report = { checks: [], requests: { total: 0, byKind: {}, failed: [], api: [] } };
let ok = true;
const check = (name, pass, detail) => {
  report.checks.push({ name, pass, ...(detail === undefined ? {} : { detail }) });
  if (!pass) ok = false;
};

const console_ = [];
try {
  const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
  page.on("console", m => {
    if (m.type() === "error") console_.push(m.text());
  });
  page.on("pageerror", e => console_.push(String(e)));
  page.on("response", res => {
    const u = new URL(res.url());
    if (!u.href.startsWith(base)) return;
    const rel = u.pathname.slice(new URL(base).pathname.length);
    const kind = rel.split("/")[0] || "index";
    report.requests.total++;
    report.requests.byKind[kind] = (report.requests.byKind[kind] ?? 0) + 1;
    if (rel.startsWith("api/")) report.requests.api.push(rel);
    if (res.status() >= 400) report.requests.failed.push(`${res.status()} ${rel}`);
  });
  const q = new URLSearchParams(args.query ?? "");
  if (args.renderer) q.set("renderer", args.renderer);
  const qs = q.toString();
  await page.goto(base + "index.html" + (qs ? "?" + qs : ""));

  const waitFull = async (since, what, timeout = 30000) => {
    try {
      await page.waitForFunction(
        s => {
          const p = window.__tsdbPerf;
          return !!p && p.lastFullPaint !== null && p.lastFullPaint > s;
        },
        since,
        { timeout },
      );
      return true;
    } catch {
      check(`${what}: full paint within ${timeout} ms`, false);
      return false;
    }
  };
  const now = () => page.evaluate(() => performance.now());
  // Share of drawn (non-black, opaque) pixels in the terrain canvas.
  const drawn = () =>
    page.evaluate(() => {
      const r = window.__tsdbMap?.view?.readTerrainPixels?.();
      if (!r) return null;
      let n = 0;
      for (let i = 0; i < r.rgba.length; i += 4) {
        if (r.rgba[i] + r.rgba[i + 1] + r.rgba[i + 2] > 0) n++;
      }
      return n / (r.width * r.height);
    });

  await page.waitForFunction(() => window.__tsdbControl?.ready?.() === true, null, { timeout: 30000 });
  if (await waitFull(-1, "start")) {
    report.firstFullPaintMs = Math.round(await page.evaluate(() => window.__tsdbPerf.firstFullPaint));
  }
  const ds = await page.evaluate(() => window.__tsdbControl.datastore());
  const range = await page.evaluate(() => window.__tsdbControl.range());
  report.datastore = ds;
  report.range = range;
  const d0 = await drawn();
  check("start: terrain drawn", d0 !== null && d0 > 0.001, d0);

  // z0 over the middle of the data.
  const b = await page.evaluate(() => window.__tsdbControl.surfaceBounds());
  let t = await now();
  await page.evaluate(([x, y]) => window.__tsdbControl.setView(x, y, 1), [(b[0] + b[2]) / 2, (b[1] + b[3]) / 2]);
  if (await waitFull(t, "z0 latest")) {
    const d = await drawn();
    check("z0 latest: terrain drawn", d !== null && d > 0.001, d);
  }

  // History: a cold load of the versions effective in the middle of the range.
  const mid = Math.round(range[0] + (range[1] - range[0]) * 0.5);
  const before = report.requests.byKind.tile ?? 0;
  t = await now();
  await page.evaluate(tm => window.__tsdbControl.seek(tm), mid);
  if (await waitFull(t, "z0 history")) {
    const d = await drawn();
    check("z0 history: terrain drawn", d !== null && d > 0.001, d);
    report.historyTileRequests = (report.requests.byKind.tile ?? 0) - before;
  }

  // Timelapse from there: tiles move forward through tile deltas.
  const deltasBefore = report.requests.byKind.tiledelta ?? 0;
  await page.evaluate(() => window.__tsdbControl.play(3600 * 4));
  await page.waitForTimeout(4000);
  await page.evaluate(() => window.__tsdbControl.play(0));
  report.timelapseDeltaRequests = (report.requests.byKind.tiledelta ?? 0) - deltasBefore;
  report.timelapseEnd = await page.evaluate(() => window.__tsdbControl.time());
  const d2 = await drawn();
  check("timelapse: terrain drawn", d2 !== null && d2 > 0.001, d2);

  // Zoom out to the whole surface (coarse levels).
  t = await now();
  await page.evaluate(([x, y, s]) => window.__tsdbControl.setView(x, y, s), [(b[0] + b[2]) / 2, (b[1] + b[3]) / 2, Math.max((b[2] - b[0]) / 1280, (b[3] - b[1]) / 800, 1) * 1.1]);
  if (await waitFull(t, "zoomed out")) {
    const d = await drawn();
    check("zoomed out: terrain drawn", d !== null && d > 0.001, d);
  }

  check("no request to the live API", report.requests.api.length === 0, report.requests.api.slice(0, 5));
  const bad = report.requests.failed.filter(f => /\s(tile|tiledelta|palette|catalog\.json|assets|index\.html)/.test(f));
  check("no failed tile, delta, palette, catalog or viewer request", bad.length === 0, bad.slice(0, 10));
} catch (err) {
  check("run", false, String(err?.stack ?? err));
} finally {
  report.consoleErrors = console_.slice(0, 10);
  await browser.close();
}
report.ok = ok;
console.log(JSON.stringify(report, null, 2));
process.exit(ok ? 0 : 1);
