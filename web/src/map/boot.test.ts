// Start-up: the early requests in index.html must match what boot.ts would request, and boot()
// must use them (one catalog and one view request) or fall back to a static export.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { after, before, test } from "node:test";
import { runInNewContext } from "node:vm";

import { boot, seedRequest, type EarlyRequests } from "./boot.ts";
import { ServerSource } from "./sources.ts";
import { syntheticDataset } from "./testing/dataset.ts";
import { MockServer } from "./testing/mockserver.ts";

const html = readFileSync(new URL("../../index.html", import.meta.url), "utf8");
const inline = /<script id="tsdb-early">([\s\S]*?)<\/script>/.exec(html)?.[1];

/** Runs the index.html early-start script for a URL and viewport; returns its requests. */
function runEarly(search: string, vp: { width: number; height: number; dpr: number }, fetchImpl?: (url: string, init: RequestInit) => Promise<Response>) {
  const urls: string[] = [];
  const win: Record<string, unknown> = {
    innerWidth: vp.width,
    innerHeight: vp.height,
    devicePixelRatio: vp.dpr,
  };
  const sandbox = {
    window: win,
    location: { search },
    URLSearchParams,
    TextEncoder,
    fetch: (url: string, init: RequestInit) => {
      urls.push(url);
      return fetchImpl ? fetchImpl(url, init) : Promise.resolve(new Response("{}"));
    },
  };
  runInNewContext(inline!, sandbox);
  return { early: win.__tsdbEarly as EarlyRequests | undefined, urls };
}

test("index.html early view request equals boot.ts seedRequest", () => {
  assert.ok(inline, "index.html has the tsdb-early script");
  const names = ["gw-bc41437d.1", "Platform 1", "nauvis", "é~x", "a.b.c"];
  let checked = 0;
  for (let i = 0; i < 400; i++) {
    const r = (n: number) => Math.floor(Math.abs(Math.sin(i * 12.9898 + n) * 43758.5453) % 1 * 1e6) / 1e6;
    const vp = { width: 300 + Math.floor(r(1) * 3500), height: 200 + Math.floor(r(2) * 2000), dpr: [1, 1.25, 1.5, 2, 3][Math.floor(r(3) * 5)] };
    const scale = 2 ** (r(4) * 14 - 11);
    const p = new URLSearchParams({
      map: names[i % names.length],
      surface: names[(i + 1) % names.length],
      x: String((r(5) - 0.5) * 2e5),
      y: String((r(6) - 0.5) * 2e5),
      s: String(scale),
    });
    if (i % 3 === 0) {
      p.set("force", names[(i + 2) % names.length]);
    }
    const { early, urls } = runEarly(`?${p}`, vp);
    const req = seedRequest(p, vp);
    const src = new ServerSource({ base: "/api/v3/" });
    if (!req) {
      assert.equal(early?.view, null, `no seed for ${p}`);
      continue;
    }
    assert.equal(early?.view?.path, src.viewPath(req.ds, req.z, req.bbox, "latest"), `${p} ${JSON.stringify(vp)}`);
    assert.deepEqual(urls, ["/api/v3/catalog", `/api/v3/${early!.view!.path}`]);
    checked++;
  }
  assert.ok(checked > 300);
  // No camera, history time or a static source: catalog only, or nothing.
  assert.equal(runEarly("?map=a&surface=b", { width: 800, height: 600, dpr: 1 }).early?.view, null);
  assert.equal(runEarly("?map=a&surface=b&x=1&y=2&s=1&t=5", { width: 800, height: 600, dpr: 1 }).early?.view, null);
  assert.equal(runEarly("?source=static", { width: 800, height: 600, dpr: 1 }).early, undefined);
  assert.equal(runEarly("?data=https://x.example/api", { width: 800, height: 600, dpr: 1 }).early?.base, "https://x.example/api/");
});

const data = syntheticDataset({ bounds: [-2, -1, 2, 1], versions: 2, coarse: 1 });
const ds = data.catalog.maps[0].surfaces[0].datastores[0];
let server: MockServer;

before(async () => {
  server = await new MockServer(data).start();
});

after(async () => {
  await server.stop();
});

test("boot uses the early requests: one catalog and one view request", async () => {
  const vp = { width: 1920, height: 1080, dpr: 1 };
  const search = `?data=${encodeURIComponent(server.url + "/api/v3/")}&map=${encodeURIComponent(data.catalog.maps[0].key)}&surface=nauvis&x=0&y=0&s=1`;
  server.clearLog();
  const { early } = runEarly(search, vp, (url, init) => fetch(url, init));
  const r = await boot(search, vp, { early });
  const seed = await r.seed;
  assert.equal(r.source.kind, "server");
  assert.equal(r.catalog.maps.length, 1);
  assert.ok(seed && seed.res.ds === ds.id && seed.res.tiles.length === 15);
  assert.deepEqual(seed!.bbox, [-3, -2, 3, 2]);
  assert.equal(server.requests(/\/catalog$/).length, 1);
  assert.equal(server.requests(/\/view\//).length, 1);
  assert.ok(Math.abs(r.source.now() - data.now) < 1000, "data clock from the early catalog response");
});

test("boot without early requests, and the static fallback", async () => {
  const vp = { width: 800, height: 600, dpr: 1 };
  const a = await boot(`?data=${encodeURIComponent(server.url + "/api/v3/")}&map=gw-test.1&surface=nauvis&x=0&y=0&s=1`, vp);
  assert.equal(a.source.kind, "server");
  assert.ok(await a.seed);
  const b = await boot(`?data=${encodeURIComponent(server.url + "/static/")}`, vp);
  assert.equal(b.source.kind, "static");
  assert.equal(await b.seed, null);
});
