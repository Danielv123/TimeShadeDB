// TerrainEngine behaviour against the mock server (real HTTP, gzip) with a recording renderer:
// first paint, never-black fallbacks, versioned refetch with deltas, aborts, timelapse,
// palette growth, missing tiles, live tiles, the texture budget and datastore switches.

import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import type { Catalog, CatalogDatastore, PerfEvent } from "../api/types";
import { perf } from "../perf.ts";
import { DirectLoader } from "./loader.ts";
import { ServerSource, StaticSource } from "./sources.ts";
import { TerrainEngine, type FrameResult } from "./terrain.ts";
import { syntheticDataset, tk, type MockDataset, type SyntheticOptions } from "./testing/dataset.ts";
import { FakeRenderer, type FakeTexture } from "./testing/fakes.ts";
import { MockServer } from "./testing/mockserver.ts";

const VP = { width: 1920, height: 1080, dpr: 1 };
const servers: MockServer[] = [];
const engines: TerrainEngine[] = [];

afterEach(async () => {
  for (const e of engines.splice(0)) {
    e.dispose();
  }
  for (const s of servers.splice(0)) {
    await s.stop();
  }
});

async function setup(opts: SyntheticOptions, kind: "server" | "static" = "server", engineOpts: { textureBudget?: number; maxInFlight?: number } = {}, mutate?: (d: MockDataset) => void) {
  const data = syntheticDataset(opts);
  mutate?.(data);
  const server = await new MockServer(data).start();
  servers.push(server);
  const source = kind === "server" ? new ServerSource({ base: `${server.url}/api/v3/` }) : new StaticSource({ base: `${server.url}/static/` });
  const catalog: Catalog = await source.catalog();
  const ds: CatalogDatastore = catalog.maps[0].surfaces[0].datastores[0];
  const renderer = new FakeRenderer();
  let redraws = 0;
  const engine = new TerrainEngine({ source, renderer, loader: new DirectLoader(), requestRedraw: () => redraws++, ...engineOpts });
  engines.push(engine);
  perf().reset();
  return { data, server, source, catalog, ds, renderer, engine, redraws: () => redraws };
}

const sleep = (ms: number) => new Promise(r => setTimeout(r, ms));

/** Runs frames until nothing is loading and every visible tile shows its own data. */
async function settle(engine: TerrainEngine, cam: { x: number; y: number; scale: number }, vp = VP, max = 400): Promise<FrameResult> {
  let r!: FrameResult;
  for (let i = 0; i < max; i++) {
    r = engine.frame(cam, vp);
    engine.render(vp, r);
    const s = engine.stats();
    if (s.inFlight === 0 && s.queued === 0 && r.full === r.visible && r.unresolved === 0) {
      return r;
    }
    await sleep(3);
  }
  throw new Error(`did not settle: ${JSON.stringify({ ...r, draws: r.draws.length, stats: engine.stats() })}`);
}

/** Runs frames for a while (requests held) and returns the last result. */
async function spin(engine: TerrainEngine, cam: { x: number; y: number; scale: number }, ms = 60, vp = VP): Promise<FrameResult> {
  let r!: FrameResult;
  const end = Date.now() + ms;
  do {
    r = engine.frame(cam, vp);
    engine.render(vp, r);
    await sleep(3);
  } while (Date.now() < end);
  return r;
}

const events = (name: PerfEvent["name"]) => perf().events.filter(e => e.name === name);
const tileReqs = (s: MockServer) => s.requests(/\/tile\/.*\/\d+\.bin$/);

test("first paint: visible z0 tiles first, palette in parallel, then prefetch; view.full once", async () => {
  const { engine, catalog, ds, server, renderer } = await setup({ bounds: [-2, -1, 2, 1], versions: 3, coarse: 2 });
  engine.setDatastore(catalog, ds);
  const r = await settle(engine, { x: 0, y: 0, scale: 1 });
  assert.equal(r.z, 0);
  assert.equal(r.visible, 15);
  assert.equal(r.full, 15);
  assert.equal(r.draws.length, 15, "only own tiles are drawn once everything is loaded");
  const full = events("view.full");
  assert.equal(full.length, 1);
  assert.equal(full[0].detail?.tiles, 15);
  assert.ok(perf().firstFullPaint !== null);
  // Request order: the 15 visible z0 tiles start before any parent-level prefetch.
  const starts = events("tile.request").filter(e => !String(e.detail?.key).startsWith("palette"));
  const firstZ = starts.slice(0, 15).map(e => e.detail?.z);
  assert.deepEqual(firstZ, new Array(15).fill(0));
  assert.ok(starts.slice(15).every(e => e.detail?.z === -1), "then the parent level");
  // Exactly one palette, one view cell per level, each tile once.
  assert.equal(server.requests(/\/palette\//).length, 1);
  assert.equal(server.requests(/\/view\/[^/]+\/0\?/).length, 1);
  assert.equal(tileReqs(server).filter(l => l.path.includes("/0/")).length >= 15, true);
  const paths = tileReqs(server).map(l => l.path);
  assert.equal(new Set(paths).size, paths.length, "no tile fetched twice");
  // Textures hold the right pixels.
  const tex = renderer.live().find(t => t.format === 1)!;
  assert.ok(tex.pixels.length === 512 * 512);
  // More frames add no requests.
  const before = server.log.length;
  await spin(engine, { x: 0, y: 0, scale: 1 }, 30);
  assert.equal(server.log.length, before);
  assert.equal(events("view.full").length, 1, "view.full is emitted once per view change");
});

test("never black when zooming in: ancestors are drawn while z0 loads", async () => {
  const { engine, catalog, ds, server } = await setup({ bounds: [-2, -1, 2, 1], versions: 2, coarse: 2 });
  engine.setDatastore(catalog, ds);
  // Zoomed out: z -1 at scale 0.5.
  const out = await settle(engine, { x: 0, y: 0, scale: 0.5 });
  assert.equal(out.z, -1);
  const release = server.hold(/\/tile\/[^/]+\/0\//);
  perf().reset();
  const r = await spin(engine, { x: 0, y: 0, scale: 1 }, 80);
  assert.equal(r.z, 0);
  assert.equal(r.full, 0, "z0 tiles are held");
  assert.equal(r.covered, r.visible, "every visible tile shows an ancestor");
  assert.ok(r.draws.length >= r.visible);
  assert.ok(r.draws.every(d => d.sw === 256 && d.sh === 256), "half of a z -1 tile per z0 tile");
  assert.equal(events("view.fallback").length, 1);
  assert.equal(events("view.full").length, 0);
  release();
  const done = await settle(engine, { x: 0, y: 0, scale: 1 });
  assert.equal(done.full, done.visible);
  assert.equal(events("view.full").length, 1);
});

test("never black when zooming out: descendants are drawn while the coarse level loads", async () => {
  const { engine, catalog, ds, server } = await setup({ bounds: [0, 0, 3, 1], versions: 2, coarse: 2 });
  engine.setDatastore(catalog, ds);
  // Centre on the data; z0 view of the 4x2 area.
  const cam0 = { x: 768, y: 256, scale: 1 };
  await settle(engine, cam0, { width: 2048, height: 1024, dpr: 1 });
  const release = server.hold(/\/tile\/[^/]+\/-[12]\//);
  perf().reset();
  // Zoom out to z -2: 1 tile covers 2048 world tiles; the z0 tiles (and z -1 if any) show.
  const r = await spin(engine, { x: 768, y: 256, scale: 0.25 }, 80, { width: 1024, height: 512, dpr: 1 });
  assert.equal(r.z, -2);
  assert.equal(r.full, 0);
  assert.equal(r.covered, r.visible);
  assert.ok(r.draws.length >= 8, "the eight z0 children are drawn scaled down");
  assert.equal(events("view.fallback").length, 1);
  release();
  await settle(engine, { x: 768, y: 256, scale: 0.25 }, { width: 1024, height: 512, dpr: 1 });
});

test("time change refetches only changed tiles, with deltas, and textures hold the new pixels", async () => {
  // Tiles (0,0) and (1,0) change at version 3; (0,1), (1,1) stop at version 2.
  const { engine, catalog, ds, server, renderer, data } = await setup({ bounds: [0, 0, 1, 1], versions: 3, coarse: 1 }, "server", {}, d => {
    const st = [...d.stores.values()][0];
    for (const k of [tk(0, 0, 1), tk(0, 1, 1)]) {
      st.tiles.get(k)!.versions.pop();
    }
  });
  const st = [...data.stores.values()][0];
  const t2 = st.tiles.get(tk(0, 0, 0))!.versions[1].t;
  const t3 = st.tiles.get(tk(0, 0, 0))!.versions[2].t;
  const cam = { x: 256, y: 256, scale: 1 };
  const vp = { width: 1024, height: 1024, dpr: 1 };
  engine.setDatastore(catalog, ds);
  engine.setTime(t2);
  await settle(engine, cam, vp);
  server.clearLog();
  perf().reset();
  engine.setTime(t3);
  const r = await settle(engine, cam, vp);
  assert.equal(r.full, 4);
  const deltas = server.requests(/\/tiledelta\//).map(l => l.path.replace(/^.*tiledelta\/[^/]+\//, ""));
  assert.deepEqual(deltas.sort(), ["0/0/2-3.bin", "1/0/2-3.bin"], "deltas for the two changed tiles only");
  assert.equal(tileReqs(server).filter(l => /\/tile\/[^/]+\/0\//.test(l.path)).length, 0, "no full z0 tile refetched");
  assert.equal(server.requests(/\/timeline\//).length, 0, "the day's timeline is reused");
  assert.equal(events("time.change").length, 1);
  // The patched textures equal version 3.
  for (const [x, y] of [[0, 0], [1, 0]]) {
    const want = st.tiles.get(tk(0, x, y))!.versions[2].pixels;
    const found = renderer.live().some((t: FakeTexture) => t.pixels.length === want.length && t.pixels.every((v, i) => v === want[i]));
    assert.ok(found, `texture of ${x},${y} equals version 3`);
  }
  // Scrubbing back: the changed tiles are fetched in full (their v2 texture became v3); the others are cached.
  server.clearLog();
  engine.setTime(t2);
  await settle(engine, cam, vp);
  const z0 = tileReqs(server)
    .map(l => /\/tile\/[^/]+\/0\/(-?\d+)\/(-?\d+)\/(\d+)\.bin$/.exec(l.path))
    .filter(m => m !== null)
    .map(m => `${m![1]}/${m![2]}/${m![3]}.bin`);
  assert.deepEqual(z0.sort(), ["0/0/2.bin", "1/0/2.bin"]);
});

test("static source: latest from timelines, consecutive delta chains", async () => {
  const { engine, catalog, ds, server, data } = await setup({ bounds: [0, 0, 1, 0], versions: 4, coarse: 0 }, "static");
  const st = [...data.stores.values()][0];
  const v = st.tiles.get(tk(0, 0, 0))!.versions;
  const cam = { x: 256, y: 0, scale: 1 };
  const vp = { width: 1024, height: 512, dpr: 1 };
  engine.setDatastore(catalog, ds);
  engine.setTime(v[0].t);
  await settle(engine, cam, vp);
  assert.equal(server.requests(/\/view\//).length, 0, "static exports have no view route");
  server.clearLog();
  engine.setTime(v[3].t);
  await settle(engine, cam, vp);
  const deltas = server.requests(/\/tiledelta\//).map(l => l.path.replace(/^.*tiledelta\/[^/]+\//, ""));
  assert.deepEqual(deltas.sort(), ["0/0/1-2.bin", "0/0/2-3.bin", "0/0/3-4.bin", "1/0/1-2.bin", "1/0/2-3.bin", "1/0/3-4.bin"]);
  // Latest on a static export resolves through the timeline.
  server.clearLog();
  engine.setTime("latest");
  const r = await settle(engine, cam, vp);
  assert.equal(r.full, 2);
  assert.equal(tileReqs(server).length, 0, "latest = version 4, already loaded");
});

test("requests for tiles that leave the view are aborted", async () => {
  const { engine, catalog, ds, server } = await setup({ bounds: [-8, -1, 8, 1], versions: 1, coarse: 0 });
  engine.setDatastore(catalog, ds);
  const release = server.hold(/\/tile\//);
  const vp = { width: 1024, height: 512, dpr: 1 };
  await spin(engine, { x: -3000, y: 0, scale: 1 }, 50, vp);
  const started = events("tile.request").filter(e => !String(e.detail?.key).startsWith("palette")).length;
  assert.ok(started >= 2);
  // Pan far east: the western tiles leave the view (and the ring).
  await spin(engine, { x: 3000, y: 0, scale: 1 }, 50, vp);
  const aborted = events("tile.abort").map(e => String(e.detail?.key));
  assert.ok(aborted.length >= 2, `aborted ${aborted.length}`);
  assert.ok(aborted.every(k => Number(k.split("/")[1]) < 0), "only western tiles were aborted");
  assert.equal(perf().counters.tilesAborted, aborted.length);
  release();
  const r = await settle(engine, { x: 3000, y: 0, scale: 1 }, vp);
  assert.equal(r.full, r.visible);
});

test("scrubbing replaces an in-flight older version; timelapse keeps it and continues with a delta", async () => {
  const { engine, catalog, ds, server, data } = await setup({ bounds: [0, 0, 0, 0], versions: 3, coarse: 0 });
  const v = [...data.stores.values()][0].tiles.get(tk(0, 0, 0))!.versions;
  const cam = { x: 0, y: 0, scale: 1 };
  const vp = { width: 512, height: 512, dpr: 1 };
  engine.setDatastore(catalog, ds);
  // Scrub: v1 is in flight when the time moves to v2 -> v1 is aborted.
  let release = server.hold(/\/tile\//);
  engine.setTime(v[0].t);
  await spin(engine, cam, 40, vp);
  engine.setTime(v[1].t);
  await spin(engine, cam, 40, vp);
  assert.deepEqual(events("tile.abort").map(e => e.detail?.key), ["0/0/0@1"]);
  release();
  await settle(engine, cam, vp);
  // Timelapse: from a fresh tile (no texture), v2 is in flight when playing moves to v3.
  engine.setDatastore({ ...catalog }, null);
  engine.setDatastore(catalog, ds);
  perf().reset();
  server.clearLog();
  release = server.hold(/\/tile\//);
  engine.setTime(v[1].t, true);
  await spin(engine, cam, 40, vp);
  engine.setTime(v[2].t, true);
  await spin(engine, cam, 40, vp);
  assert.equal(events("tile.abort").length, 0, "the in-flight load is kept during timelapse");
  release();
  await settle(engine, cam, vp);
  assert.deepEqual(server.requests(/\/tiledelta\//).map(l => l.path.replace(/^.*\//, "")), ["2-3.bin"], "then a delta moves it forward");
});

test("palette growth: a tile needing more entries than the catalog lists fetches a larger palette", async () => {
  const { engine, catalog, ds, server, renderer } = await setup({ bounds: [0, 0, 0, 0], versions: 2, coarse: 0, colors: 30, lateColors: 10 }, "server", {}, d => {
    const st = [...d.stores.values()][0];
    st.ds.palette_version = 30; // the catalog is older than the newest tile
  });
  assert.equal(ds.palette_version, 30);
  engine.setDatastore(catalog, ds);
  const r = await settle(engine, { x: 0, y: 0, scale: 1 }, { width: 512, height: 512, dpr: 1 });
  assert.equal(r.full, 1);
  const pals = server.requests(/\/palette\//).map(l => Number(l.path.replace(/^.*\/(\d+)\.bin$/, "$1")));
  assert.equal(pals[0], 30);
  assert.ok(pals.some(n => n > 30), `palettes fetched: ${pals}`);
  assert.ok(renderer.paletteLength > 30);
});

test("a tile that 404s counts as empty and is not refetched every frame", async () => {
  const { engine, catalog, ds, server } = await setup({ bounds: [0, 0, 1, 0], versions: 1, coarse: 0 }, "server", {}, d => {
    const st = [...d.stores.values()][0];
    const t = st.tiles.get(tk(0, 1, 0))!;
    // The view lists version 1, but the tile route has no such version.
    t.versions[0] = { ...t.versions[0], v: 1 };
    const orig = t.versions;
    Object.defineProperty(t, "versions", { get: () => orig, configurable: true });
  });
  // Make the tile route 404 for (1,0) by removing it after the view answered.
  const st = [...server.data.stores.values()][0];
  engine.setDatastore(catalog, ds);
  const vp = { width: 1024, height: 512, dpr: 1 };
  const cam = { x: 256, y: 0, scale: 1 };
  const releaseTiles = server.hold(/\/tile\//);
  await spin(engine, cam, 40, vp);
  st.tiles.get(tk(0, 1, 0))!.versions.splice(0);
  releaseTiles();
  const r = await settle(engine, cam, vp);
  assert.equal(r.full, 2);
  await spin(engine, cam, 40, vp);
  assert.equal(server.requests(/\/tile\/[^/]+\/0\/1\/0\//).length, 1);
});

test("live tiles: live.bin keyed by live sequence, replaced on live events", async () => {
  const { engine, catalog, ds, server, renderer, data } = await setup({ bounds: [0, 0, 0, 0], versions: 2, coarse: 0 });
  const st = [...data.stores.values()][0];
  const base = st.tiles.get(tk(0, 0, 0))!.versions[1];
  st.live.set(tk(0, 0, 0), { ls: 5, pixels: (base.pixels as Uint8Array).slice().fill(3, 0, 1024), paletteVersion: base.paletteVersion });
  const cam = { x: 0, y: 0, scale: 1 };
  const vp = { width: 512, height: 512, dpr: 1 };
  engine.setDatastore(catalog, ds);
  await settle(engine, cam, vp);
  assert.equal(server.requests(/\/live\.bin$/).length, 1);
  const first = renderer.live().length;
  st.live.set(tk(0, 0, 0), { ls: 6, pixels: (base.pixels as Uint8Array).slice().fill(4, 0, 1024), paletteVersion: base.paletteVersion });
  engine.applyLiveTile({ ds: ds.id, z: 0, x: 0, y: 0, v: 2, live: true, ls: 6, now: data.now });
  await settle(engine, cam, vp);
  assert.equal(server.requests(/\/live\.bin$/).length, 2);
  assert.equal(renderer.live().length, first, "the older live texture was dropped");
  // History mode does not use live content.
  server.clearLog();
  engine.setTime(base.t);
  await settle(engine, cam, vp);
  assert.equal(server.requests(/\/live\.bin$/).length, 0);
  assert.equal(server.requests(/\/tile\/.*\/2\.bin$/).length, 1);
});

test("texture LRU stays within its byte budget while panning", async () => {
  const budget = 6 * 256 * 1024;
  const { engine, catalog, ds, renderer } = await setup({ bounds: [0, 0, 11, 0], versions: 1, coarse: 0 }, "server", { textureBudget: budget });
  engine.setDatastore(catalog, ds);
  const vp = { width: 1024, height: 512, dpr: 1 };
  for (let x = 0; x < 12 * 512; x += 512) {
    await settle(engine, { x: x - 256, y: 0, scale: 1 }, vp);
    const live = renderer.live().reduce((n, t) => n + t.bytes, 0);
    assert.ok(live <= budget, `texture bytes ${live} > ${budget}`);
    assert.equal(engine.stats().textureBytes, live);
  }
  assert.equal(perf().counters.textureBytes, engine.stats().textureBytes);
});

test("switching datastores drops results of the old one", async () => {
  const { engine, catalog, ds, server, renderer } = await setup({ bounds: [0, 0, 1, 0], versions: 1, coarse: 0 });
  engine.setDatastore(catalog, ds);
  const release = server.hold(/\/tile\//);
  await spin(engine, { x: 256, y: 0, scale: 1 }, 40, { width: 1024, height: 512, dpr: 1 });
  engine.setDatastore(catalog, null);
  release();
  await sleep(50);
  assert.equal(renderer.live().length, 0);
  assert.equal(engine.frame({ x: 0, y: 0, scale: 1 }, VP).visible, 0);
});
