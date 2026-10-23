// End-to-end pixel check of the built viewer in Chromium: renders the mock dataset at z0
// (palette path) and z -1 (RGB565 path) with the WebGL2 and Canvas2D renderers, from the
// live-server and static sources, screenshots the terrain canvas and compares every pixel
// with the expected colour (FORMAT.md 3 bit replication). Also checks that a MapLayer's
// draw2d output lands on the overlay at the right screen position.
//
//   npm --prefix web run build
//   node src/map/testing/browsercheck.ts --pw <playwright-core dir> --chrome <chrome.exe>
//        [--illisis <export root>]  (default: a synthetic dataset)
//
// Exits non-zero on any mismatch. Node only.

import { resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { inflateSync } from "node:zlib";
import { tk, versionAt, type MockDataset } from "./dataset.ts";
import { MockServer } from "./mockserver.ts";
import { loadDataset, parseArgs } from "./serve.ts";

/** Decodes an 8-bit RGB or RGBA, non-interlaced PNG (what Chromium screenshots are). */
export function decodePng(buf: Buffer): { width: number; height: number; rgba: Uint8Array } {
  let off = 8;
  let width = 0;
  let height = 0;
  let type = 0;
  const idat: Buffer[] = [];
  while (off < buf.length) {
    const len = buf.readUInt32BE(off);
    const kind = buf.toString("ascii", off + 4, off + 8);
    const data = buf.subarray(off + 8, off + 8 + len);
    if (kind === "IHDR") {
      width = data.readUInt32BE(0);
      height = data.readUInt32BE(4);
      if (data[8] !== 8 || data[12] !== 0) {
        throw new Error("unsupported PNG");
      }
      type = data[9];
    } else if (kind === "IDAT") {
      idat.push(data);
    }
    off += 12 + len;
  }
  const bpp = type === 6 ? 4 : type === 2 ? 3 : 0;
  if (!bpp) {
    throw new Error(`PNG colour type ${type}`);
  }
  const raw = inflateSync(Buffer.concat(idat));
  const stride = width * bpp;
  const out = new Uint8Array(width * height * 4);
  const prev = new Uint8Array(stride);
  const cur = new Uint8Array(stride);
  for (let y = 0; y < height; y++) {
    const f = raw[y * (stride + 1)];
    const line = raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1));
    for (let i = 0; i < stride; i++) {
      const a = i >= bpp ? cur[i - bpp] : 0;
      const b = prev[i];
      const c = i >= bpp ? prev[i - bpp] : 0;
      let v = line[i];
      if (f === 1) {
        v += a;
      } else if (f === 2) {
        v += b;
      } else if (f === 3) {
        v += (a + b) >> 1;
      } else if (f === 4) {
        const p = a + b - c;
        const pa = Math.abs(p - a);
        const pb = Math.abs(p - b);
        const pc = Math.abs(p - c);
        v += pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
      }
      cur[i] = v & 255;
    }
    for (let x = 0; x < width; x++) {
      out[(y * width + x) * 4] = cur[x * bpp];
      out[(y * width + x) * 4 + 1] = cur[x * bpp + 1];
      out[(y * width + x) * 4 + 2] = cur[x * bpp + 2];
      out[(y * width + x) * 4 + 3] = bpp === 4 ? cur[x * bpp + 3] : 255;
    }
    prev.set(cur);
  }
  return { width, height, rgba: out };
}

function expand(c: number): [number, number, number] {
  const r = (c >> 11) & 31;
  const g = (c >> 5) & 63;
  const b = c & 31;
  return [(r << 3) | (r >> 2), (g << 2) | (g >> 4), (b << 3) | (b >> 2)];
}

/** Expected RGB565 at world tile (wx, wy) for level z (latest versions); 0 = uncharted. */
function expectedColor(data: MockDataset, z: number, wx: number, wy: number): number {
  const st = [...data.stores.values()][0];
  const span = 512 * 2 ** -z;
  const tx = Math.floor((wx + 256) / span);
  const ty = Math.floor((wy + 256) / span);
  const tile = st.tiles.get(tk(z, tx, ty));
  const v = tile ? versionAt(tile, Number.POSITIVE_INFINITY) : null;
  if (!v) {
    return 0;
  }
  const f = 2 ** -z;
  const px = Math.floor((wx + 256 - tx * span) / f);
  const py = Math.floor((wy + 256 - ty * span) / f);
  const val = v.pixels[py * 512 + px];
  return v.format === 3 ? val : st.palette[val];
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2));
  const { chromium } = (await import(pathToFileURL(resolve(args.get("pw")!, "index.mjs")).href)) as typeof import("playwright-core");
  const data = await loadDataset(args);
  const st = [...data.stores.values()][0];
  const dist = resolve(fileURLToPath(new URL("../../../dist/", import.meta.url)));
  const server = await new MockServer(data, { staticDir: dist }).start(0);
  const browser = await chromium.launch({ executablePath: args.get("chrome"), headless: true, args: ["--use-angle=d3d11", "--ignore-gpu-blocklist", "--force-color-profile=srgb"] });
  const failures: string[] = [];
  const results: Record<string, unknown>[] = [];
  const map = data.catalog.maps[0];
  try {
    for (const source of ["server", "static"] as const) {
      for (const renderer of ["webgl2", "canvas2d"] as const) {
        for (const z of [0, -1]) {
          const page = await browser.newPage({ viewport: { width: 1280, height: 800 }, deviceScaleFactor: 1 });
          const q = new URLSearchParams({ map: map.key, surface: map.surfaces[0].name, force: st.ds.force, renderer, layers: "" });
          if (source === "static") {
            q.set("source", "static");
            q.set("data", "/static/");
          }
          await page.goto(`${server.url}/?${q}`);
          // Let the initial fit finish first, so its view.full is not mistaken for ours.
          await page.waitForFunction(() => (window as unknown as { __tsdbPerf?: { firstFullPaint: number | null } }).__tsdbPerf?.firstFullPaint != null && !!(window as unknown as { __tsdbMap?: unknown }).__tsdbMap, undefined, { timeout: 15000 });
          const size = await page.evaluate(() => {
            const c = document.querySelector(".tsdbTerrain") as HTMLCanvasElement;
            return { w: c.clientWidth, h: c.clientHeight };
          });
          // Put the top-left corner of the datastore's first tile (aligned to level z) at screen (0, 0).
          const span = 512 * 2 ** -z;
          const scale = 2 ** z;
          const wx0 = -256 + Math.floor((st.ds.bounds[0] * 512) / span) * span;
          const wy0 = -256 + Math.floor((st.ds.bounds[1] * 512) / span) * span;
          const cam = { x: wx0 + size.w / 2 / scale, y: wy0 + size.h / 2 / scale, scale };
          await page.evaluate(c => (window as unknown as { __tsdbMap: { setCamera(x: number, y: number, s: number): void } }).__tsdbMap.setCamera(c.x, c.y, c.scale), cam);
          // view.full is only emitted when the visible tile set changes, so wait on the state
          // after two frames instead.
          await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
          await page.waitForFunction(
            (zz: number) => {
              const st = (window as unknown as { __tsdbMap: { state(): { z: number; tiles: { full: number; visible: number } } } }).__tsdbMap.state();
              return st.z === zz && st.tiles.full === st.tiles.visible;
            },
            z,
            { timeout: 15000 },
          );
          const state = await page.evaluate(() => (window as unknown as { __tsdbMap: { state(): { z: number; renderer: string } } }).__tsdbMap.state());
          // Hide the HUD and notices that float over the map.
          await page.addStyleTag({ content: ".hud, .notice { display: none !important; }" });
          await page.waitForTimeout(100);
          const screenshot = decodePng(await page.locator(".tsdbTerrain").screenshot());
          // Exact renderer output, read back from the drawing buffer (the screenshot goes through
          // the compositor's colour handling and is only compared with a tolerance below).
          const read = await page.evaluate(() => {
            const r = (window as unknown as { __tsdbMap: { view: { readTerrainPixels(): { width: number; height: number; rgba: Uint8Array } | null } } }).__tsdbMap.view.readTerrainPixels();
            if (!r) {
              return null;
            }
            let bin = "";
            for (let i = 0; i < r.rgba.length; i += 0x8000) {
              bin += String.fromCharCode(...r.rgba.subarray(i, i + 0x8000));
            }
            return { width: r.width, height: r.height, b64: btoa(bin) };
          });
          if (!read) {
            throw new Error("renderer has no readPixels");
          }
          const png = { width: read.width, height: read.height, rgba: new Uint8Array(Buffer.from(read.b64, "base64")) };
          let shotMax = 0;
          for (let i = 0; i < Math.min(png.rgba.length, screenshot.rgba.length); i++) {
            if ((i & 3) !== 3) {
              shotMax = Math.max(shotMax, Math.abs(png.rgba[i] - screenshot.rgba[i]));
            }
          }
          let bad = 0;
          let charted = 0;
          let firstBad = "";
          let maxDiff = 0;
          for (let sy = 0; sy < png.height; sy++) {
            for (let sx = 0; sx < png.width; sx++) {
              // Pixel centre in world tiles.
              const wx = wx0 + (sx + 0.5) / scale;
              const wy = wy0 + (sy + 0.5) / scale;
              const want = expand(expectedColor(data, z, Math.floor(wx), Math.floor(wy)));
              const i = (sy * png.width + sx) * 4;
              const got = [png.rgba[i], png.rgba[i + 1], png.rgba[i + 2]];
              if (want[0] | want[1] | want[2]) {
                charted++;
              }
              const d = Math.max(Math.abs(got[0] - want[0]), Math.abs(got[1] - want[1]), Math.abs(got[2] - want[2]));
              if (d > maxDiff) {
                maxDiff = d;
              }
              if (d !== 0) {
                if (!firstBad) {
                  firstBad = `screen ${sx},${sy} world ${Math.floor(wx)},${Math.floor(wy)} got ${got} want ${want}`;
                }
                bad++;
              }
            }
          }
          const name = `${source}/${renderer}/z${z}`;
          if (state.z !== z || state.renderer !== renderer) {
            failures.push(`${name}: drew z${state.z} with ${state.renderer}`);
          }
          if (bad > 0 || charted === 0) {
            failures.push(`${name}: ${bad} of ${png.width * png.height} pixels differ (charted ${charted}); first: ${firstBad}`);
          }
          results.push({ case: name, pixels: png.width * png.height, charted, mismatched: bad, maxChannelDiff: maxDiff, screenshotMaxDiff: shotMax, drawn: `z${state.z} ${state.renderer}` });
          if (screenshot.width !== png.width || screenshot.height !== png.height || shotMax > 16) {
            failures.push(`${name}: screenshot differs from the drawing buffer by up to ${shotMax}`);
          }
          await page.close();
        }
      }
    }
    // A MapLayer's draw2d output on the overlay canvas.
    const page = await browser.newPage({ viewport: { width: 1000, height: 700 }, deviceScaleFactor: 1 });
    await page.goto(`${server.url}/?map=${encodeURIComponent(map.key)}&surface=${map.surfaces[0].name}&x=0&y=0&s=1&layers=`);
    await page.waitForFunction(() => (window as unknown as { __tsdbPerf?: { firstFullPaint: number | null } }).__tsdbPerf?.firstFullPaint != null, undefined, { timeout: 15000 });
    const probe = await page.evaluate(() => {
      const m = (window as unknown as { __tsdbMap: { view: { setLayers(ids: string[], f: unknown[]): void } } }).__tsdbMap;
      let attached = 0;
      let frames = 0;
      m.view.setLayers(["probe"], [
        {
          id: "probe",
          label: "Probe",
          defaultOn: true,
          create: () => ({
            id: "probe",
            order: 1,
            attach: () => {
              attached++;
            },
            detach: () => {},
            draw2d: (ctx: CanvasRenderingContext2D, f: { worldToScreen(x: number, y: number): [number, number] }) => {
              frames++;
              const [x, y] = f.worldToScreen(100, 50);
              ctx.fillStyle = "#ff00ff";
              ctx.fillRect(x - 4, y - 4, 8, 8);
            },
          }),
        },
      ]);
      const c = document.querySelector(".tsdbOverlay") as HTMLCanvasElement;
      return new Promise<{ attached: number; frames: number; w: number; h: number }>(r => setTimeout(() => r({ attached, frames, w: c.clientWidth, h: c.clientHeight }), 300));
    });
    const shot = decodePng(await page.locator(".mapHost").screenshot());
    // World (100, 50) at camera (0, 0), scale 1: screen centre + (100, 50).
    const sx = Math.round(probe.w / 2 + 100);
    const sy = Math.round(probe.h / 2 + 50);
    const i = (sy * shot.width + sx) * 4;
    const magenta = shot.rgba[i] === 255 && shot.rgba[i + 1] === 0 && shot.rgba[i + 2] === 255;
    results.push({ case: "layer draw2d", attached: probe.attached, frames: probe.frames, magentaAtWorldPoint: magenta });
    if (!magenta || probe.attached !== 1 || probe.frames < 1) {
      failures.push(`layer probe: ${JSON.stringify(probe)} pixel ${shot.rgba.slice(i, i + 4)}`);
    }
    await page.close();
  } finally {
    await browser.close();
    await server.stop();
  }
  console.log(JSON.stringify({ results, failures }, null, 2));
  if (failures.length > 0) {
    process.exit(1);
  }
}

void main().catch(err => {
  console.error(err);
  process.exit(1);
});
