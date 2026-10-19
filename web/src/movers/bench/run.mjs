// Runs trains_bench.ts in Chromium and prints the result (WP8 "500 trains at 60 fps").
//
// usage (from web/): node src/movers/bench/run.mjs [--trains 500] [--seconds 5] [--headed]
// Needs `playwright-core` resolvable (NODE_PATH or a local install; it is not a dependency
// of this package; WP11's bench/web owns Playwright) and a Chromium: PLAYWRIGHT_CHROMIUM or
// the Playwright browser cache.
import { build } from "esbuild";
import { mkdtempSync, writeFileSync, readdirSync, existsSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir, homedir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const args = process.argv.slice(2);
const opt = (n, d) => (args.includes(n) ? args[args.indexOf(n) + 1] : d);
const trains = Number(opt("--trains", 500));
const seconds = Number(opt("--seconds", 5));

const require = createRequire(join(process.cwd(), "noop.js"));
let chromium;
try {
  ({ chromium } = require("playwright-core"));
} catch {
  console.error("playwright-core not found (set NODE_PATH to a node_modules that has it)");
  process.exit(2);
}

function findChrome() {
  if (process.env.PLAYWRIGHT_CHROMIUM) {
    return process.env.PLAYWRIGHT_CHROMIUM;
  }
  const cache = join(homedir(), "AppData", "Local", "ms-playwright");
  for (const d of existsSync(cache) ? readdirSync(cache).sort().reverse() : []) {
    for (const exe of [join(cache, d, "chrome-win64", "chrome.exe"), join(cache, d, "chrome-linux", "chrome")]) {
      if (d.startsWith("chromium-") && existsSync(exe)) {
        return exe;
      }
    }
  }
  return undefined;
}

const dir = mkdtempSync(join(tmpdir(), "trains-bench-"));
await build({ entryPoints: [new URL("./trains_bench.ts", import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, "$1")], bundle: true, format: "iife", outfile: join(dir, "bench.js"), target: "es2022" });
writeFileSync(join(dir, "index.html"), '<!doctype html><meta charset="utf-8"><body style="margin:0;background:#000"><script src="bench.js"></script>');
const browser = await chromium.launch({ executablePath: findChrome(), headless: !args.includes("--headed"), args: ["--disable-gpu-vsync", "--disable-frame-rate-limit"].filter(() => args.includes("--unlimited")) });
try {
  const page = await browser.newPage({ viewport: { width: 1920, height: 1080 } });
  await page.goto(`${pathToFileURL(join(dir, "index.html")).href}?trains=${trains}&seconds=${seconds}`);
  await page.waitForFunction(() => window.__trainsBench, null, { timeout: (seconds + 60) * 1000 });
  const r = await page.evaluate(() => window.__trainsBench);
  console.log(JSON.stringify({ ...r, chrome: browser.version() }));
} finally {
  await browser.close();
}
