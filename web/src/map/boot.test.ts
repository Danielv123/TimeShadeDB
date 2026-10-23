// Start-up: the early requests in index.html must match what boot.ts would request, and boot()
// must use them (one catalog and one version request) or fall back to a static export.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { after, before, test } from "node:test";
import { runInNewContext } from "node:vm";

import { datastoreId } from "../api/grid.ts";
import { boot, requestedSource, seedPath, seedRequest, urlDatastore, urlScale, type EarlyRequests } from "./boot.ts";
import { syntheticDataset } from "./testing/dataset.ts";
import { MockServer } from "./testing/mockserver.ts";

const html = readFileSync(new URL("../../index.html", import.meta.url), "utf8");
const inline = /<script id="tsdb-early">([\s\S]*?)<\/script>/.exec(html)?.[1];

/** Runs the index.html early-start script for a URL and viewport; returns its requests. */
function runEarly(
  search: string,
  vp: { width: number; height: number; dpr: number },
  opts: { meta?: string; fetch?: (url: string, init: RequestInit) => Promise<Response> } = {},
) {
  const urls: string[] = [];
  const win: Record<string, unknown> = { innerWidth: vp.width, innerHeight: vp.height, devicePixelRatio: vp.dpr };
  const sandbox = {
    window: win,
    location: { search },
    document: { querySelector: () => (opts.meta === undefined ? null : { getAttribute: () => opts.meta }) },
    URLSearchParams,
    TextEncoder,
    fetch: (url: string, init: RequestInit) => {
      urls.push(url);
      return opts.fetch ? opts.fetch(url, init) : Promise.resolve(new Response("{}"));
    },
  };
  runInNewContext(inline!, sandbox);
  return { early: win.__tsdbEarly as EarlyRequests | undefined, urls };
}

test("index.html early requests equal boot.ts seedRequest/seedPath", () => {
  assert.ok(inline, "index.html has the tsdb-early script");
  const names = ["gw-bc41437d.1", "Platform 1", "nauvis", "é~x", "a.b.c"];
  let checked = 0;
  for (let i = 0; i < 600; i++) {
    const r = (n: number) => Math.floor((Math.abs(Math.sin(i * 12.9898 + n) * 43758.5453) % 1) * 1e6) / 1e6;
    const vp = { width: 300 + Math.floor(r(1) * 3500), height: 200 + Math.floor(r(2) * 2000), dpr: [1, 1.25, 1.5, 2, 3][Math.floor(r(3) * 5)] };
    const p = new URLSearchParams({
      map: names[i % names.length],
      surface: names[(i + 1) % names.length],
      x: String((r(5) - 0.5) * 2e5),
      y: String((r(6) - 0.5) * 2e5),
      s: String(2 ** (r(4) * 14 - 11)),
    });
    if (i % 3 === 0) {
      p.set("force", names[(i + 2) % names.length]);
    }
    if (i % 4 === 1) {
      p.set("t", String(1791630000000 + Math.floor(r(7) * 1e9)));
    }
    // The benchmark suite's parameters: ds instead of map/surface/force, z instead of s (or both).
    if (i % 7 === 3) {
      p.set("ds", datastoreId({ map: p.get("map")!, surface: p.get("surface")!, force: p.get("force") ?? "player" }));
      if (i % 2 === 0) {
        p.delete("map");
        p.delete("surface");
        p.delete("force");
      }
    }
    if (i % 6 === 4) {
      p.set("z", String(i % 12 === 4 ? Math.round(r(4) * 10 - 10) : r(4) * 12 - 10));
      if (i % 12 === 4) {
        p.delete("s");
      }
    }
    const kind = i % 5 === 2 ? "static" : "server";
    const viaMeta = kind === "static" && i % 2 === 0;
    if (kind === "static" && !viaMeta) {
      p.set("source", "static");
    }
    const now = Date.now();
    const { early, urls } = runEarly(`?${p}`, vp, { meta: viaMeta ? "static" : "" });
    const isStatic = requestedSource(p, viaMeta ? "static" : "") === "static";
    assert.equal(isStatic, kind === "static");
    assert.equal(early?.kind, kind);
    const req = seedRequest(p, vp, isStatic, now);
    const base = kind === "static" ? "./" : "/api/v3/";
    assert.equal(urls[0], base + (kind === "static" ? "catalog.json" : "catalog"));
    if (!req) {
      assert.equal(early?.seed, null, `no seed for ${p}`);
      continue;
    }
    assert.equal(early?.seed?.path, seedPath(req), `${p} ${JSON.stringify(vp)}`);
    assert.equal(urls[1], base + seedPath(req));
    checked++;
  }
  assert.ok(checked > 450, `checked ${checked}`);
  assert.equal(runEarly("?map=a&surface=b", { width: 800, height: 600, dpr: 1 }).early?.seed, null);
  assert.equal(runEarly("?map=a&surface=b&x=1&y=2&s=1&t=1e300", { width: 800, height: 600, dpr: 1 }).early?.seed, null);
  assert.equal(runEarly("?data=https://x.example/api", { width: 800, height: 600, dpr: 1 }).early?.base, "https://x.example/api/");
  assert.equal(seedRequest(new URLSearchParams("map=a&surface=b&x=1&y=2&s=1&t=1e300"), { width: 800, height: 600, dpr: 1 }, false, 0), null);
  // A ds with characters a path needs escaped is sent escaped by both.
  const odd = runEarly("?ds=a%20b.c.d&x=0&y=0&z=0", { width: 800, height: 600, dpr: 1 }).early?.seed?.path;
  assert.equal(odd, seedPath(seedRequest(new URLSearchParams("ds=a%20b.c.d&x=0&y=0&z=0"), { width: 800, height: 600, dpr: 1 }, false, 0)!));
  assert.ok(odd?.startsWith("view/a%20b.c.d/0?"));
});

test("URL parameters: ds and z (the bench suite) next to map/surface/force and s", () => {
  const q = (s: string) => new URLSearchParams(s);
  assert.equal(urlDatastore(q("ds=gw-bc41437d~2E1.nauvis.player")), "gw-bc41437d~2E1.nauvis.player");
  assert.equal(urlDatastore(q("map=gw-bc41437d.1&surface=nauvis")), "gw-bc41437d~2E1.nauvis.player");
  assert.equal(urlDatastore(q("map=m&surface=s&force=enemy")), "m.s.enemy");
  assert.equal(urlDatastore(q("ds=m.s.f&map=x&surface=y")), "m.s.f", "ds wins");
  assert.equal(urlDatastore(q("ds=bad&map=x&surface=y")), "x.y.player", "a malformed ds is ignored");
  assert.equal(urlDatastore(q("ds=a..b")), null);
  assert.equal(urlDatastore(q("map=x")), null);
  assert.equal(urlScale(q("s=0.25")), 0.25);
  assert.equal(urlScale(q("z=0")), 1);
  assert.equal(urlScale(q("z=-4")), 1 / 16);
  assert.equal(urlScale(q("s=2&z=-4")), 2, "s wins");
  assert.equal(urlScale(q("s=0&z=-1")), 0.5, "an invalid s falls back to z");
  assert.equal(urlScale(q("z=abc")), null);
  assert.equal(urlScale(q("")), null);
  const vp = { width: 1920, height: 1080, dpr: 1 };
  const a = seedRequest(q("ds=m.s.player&x=0&y=0&z=0"), vp, false, 0);
  const b = seedRequest(q("map=m&surface=s&x=0&y=0&s=1"), vp, false, 0);
  assert.deepEqual(a, b);
  assert.equal(seedRequest(q("ds=m.s.player&x=0&y=0&z=-4"), vp, false, 0)?.z, -4);
});

const data = syntheticDataset({ bounds: [-2, -1, 2, 1], versions: 2, coarse: 1 });
const ds = data.catalog.maps[0].surfaces[0].datastores[0];
const mapKey = encodeURIComponent(data.catalog.maps[0].key);
let server: MockServer;

before(async () => {
  server = await new MockServer(data).start();
});

after(async () => {
  await server.stop();
});

test("boot adopts the early server requests: one catalog and one view request", async () => {
  const vp = { width: 1920, height: 1080, dpr: 1 };
  const search = `?data=${encodeURIComponent(server.url + "/api/v3/")}&map=${mapKey}&surface=nauvis&x=0&y=0&s=1`;
  server.clearLog();
  const { early } = runEarly(search, vp, { fetch: (url, init) => fetch(url, init) });
  const r = await boot(search, vp, { early });
  const seed = await r.seed;
  assert.equal(r.source.kind, "server");
  assert.ok(seed && seed.kind === "view" && seed.res.ds === ds.id && seed.res.tiles.length === 15);
  assert.deepEqual(seed.kind === "view" && seed.bbox, [-3, -2, 3, 2]);
  assert.equal(server.requests(/\/catalog$/).length, 1);
  assert.equal(server.requests(/\/view\//).length, 1);
  assert.ok(Math.abs(r.source.now() - data.now) < 1000, "data clock from the early catalog response");
  // The early script prefetches the visible tiles (all 15 here).
  for (let i = 0; i < 200 && server.requests(/\/tile\//).length < 15; i++) {
    await new Promise(res => setTimeout(res, 10));
  }
  const tiles = server.requests(/\/tile\//);
  assert.equal(tiles.length, 15, "visible tiles prefetched");
  assert.ok(tiles.some(l => /\/tile\/[^/]+\/0\/0\/0\/\d+\.bin$/.test(l.path)), "the centre tile is among them");
});

test("boot adopts early static requests: catalog.json and today's timeline", async () => {
  const vp = { width: 1280, height: 720, dpr: 1 };
  const search = `?data=${encodeURIComponent(server.url + "/static/")}&map=${mapKey}&surface=nauvis&x=0&y=0&s=1`;
  server.clearLog();
  const { early } = runEarly(search, vp, { meta: "static", fetch: (url, init) => fetch(url, init) });
  const r = await boot(search, vp, { early, metaSource: "static" });
  const seed = await r.seed;
  assert.equal(r.source.kind, "static");
  assert.ok(seed && seed.kind === "timeline" && seed.res.z === 0);
  assert.equal(server.requests(/\/static\/catalog\.json$/).length, 1);
  assert.equal(server.requests(/\/static\/timeline\//).length, 1);
  assert.equal(server.requests(/\/api\/v3\//).length, 0, "no probing of the live API");
});

test("history links seed the day's timeline; boot without early requests; static fallback", async () => {
  const vp = { width: 800, height: 600, dpr: 1 };
  const t = ds.last;
  const h = await boot(`?data=${encodeURIComponent(server.url + "/api/v3/")}&map=${mapKey}&surface=nauvis&x=0&y=0&s=1&t=${t}`, vp);
  const hs = await h.seed;
  assert.ok(hs && hs.kind === "timeline" && hs.res.tiles.length === 15);
  const a = await boot(`?data=${encodeURIComponent(server.url + "/api/v3/")}&map=${mapKey}&surface=nauvis&x=0&y=0&s=1`, vp);
  assert.equal(a.source.kind, "server");
  assert.equal((await a.seed)?.kind, "view");
  // The static root answers catalog.json but not catalog: falls through to static.
  const b = await boot(`?data=${encodeURIComponent(server.url + "/static/")}`, vp);
  assert.equal(b.source.kind, "static");
  assert.equal(await b.seed, null);
});
