// DataSource tests against the mock server: paths, the data clock, errors, aborts, the static
// export mapping and live events.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { after, before, test } from "node:test";

import { decodePalette, decodeTile, decodeTileDelta } from "../api/wire.ts";
import { detectSource, HttpError, readEvents, ServerSource, StaticSource } from "./sources.ts";
import { syntheticDataset } from "./testing/dataset.ts";
import { MockServer } from "./testing/mockserver.ts";

const fixtures = new URL("../../fixtures/", import.meta.url);
const data = syntheticDataset({ bounds: [0, 0, 1, 0], versions: 3, coarse: 1 });
const ds = data.catalog.maps[0].surfaces[0].datastores[0];
const overlays = readFileSync(new URL("overlays_0_0.json", fixtures));
let server: MockServer;

before(async () => {
  server = await new MockServer(data, {
    files: new Map([[`overlays/${ds.id}/0/0.json`, { body: new Uint8Array(overlays), type: "application/json" }]]),
  }).start();
});

after(async () => {
  await server.stop();
});

test("server source: routes, decoding and the data clock", async () => {
  const s = new ServerSource({ base: `${server.url}/api/v3` });
  assert.equal(s.base, `${server.url}/api/v3/`);
  const cat = await s.catalog();
  assert.equal(cat.version, 1);
  assert.ok(Math.abs(s.now() - data.now) < 1000, "clock from X-TSDB-Now / catalog.now");
  const pal = decodePalette(await s.palette(ds.id, ds.palette_version));
  assert.equal(pal.length, ds.palette_version);
  const view = await s.view(ds.id, 0, [0, 0, 1, 0], "latest");
  assert.deepEqual(view.tiles.map(t => [t.x, t.v]), [[0, 3], [1, 3]]);
  const tl = await s.timeline(ds.id, 0, "2026-10-10");
  assert.equal(tl.tiles.length, 2);
  const tile = decodeTile(await s.tile(ds.id, { z: 0, x: 1, y: 0 }, 2));
  assert.deepEqual([tile.header.z, tile.header.x, tile.header.version], [0, 1, 2]);
  const coarse = decodeTile(await s.tile(ds.id, { z: -1, x: 0, y: 0 }, 1));
  assert.equal(coarse.header.format, 3);
  const d = decodeTileDelta(await s.tileDelta(ds.id, 0, 0, 1, 3));
  assert.deepEqual([d.header.from, d.header.to], [1, 3]);
  assert.equal(s.tileUrl(ds.id, { z: -2, x: -1, y: 3 }, 7), `${server.url}/api/v3/tile/${ds.id}/-2/-1/3/7.bin`);
  assert.equal(s.tileDeltaUrl(ds.id, 1, 2, 3, 4), `${server.url}/api/v3/tiledelta/${ds.id}/1/2/3-4.bin`);
  assert.ok(server.requests(/\/api\/v3\/view\/.*bbox=0,0,1,0&t=latest$/).length === 1);
});

test("server source: errors carry the API code; aborts reject with AbortError", async () => {
  const s = new ServerSource({ base: `${server.url}/api/v3/` });
  await assert.rejects(s.tile(ds.id, { z: 0, x: 0, y: 0 }, 99), (e: unknown) => e instanceof HttpError && e.status === 404 && e.code === "not_found");
  await assert.rejects(s.view(ds.id, 0, [0, 0, 100, 100], "latest"), (e: unknown) => e instanceof HttpError && e.code === "bad_request");
  const release = server.hold(/\/tile\//);
  const ctrl = new AbortController();
  const p = s.tile(ds.id, { z: 0, x: 0, y: 0 }, 1, ctrl.signal);
  setTimeout(() => ctrl.abort(), 20);
  await assert.rejects(p, (e: unknown) => (e as Error).name === "AbortError");
  release();
  const pre = new AbortController();
  pre.abort();
  await assert.rejects(s.catalog(pre.signal), (e: unknown) => (e as Error).name === "AbortError");
});

test("server source: overlays query string and read token header", async () => {
  const seen: { url: string; auth: string | null }[] = [];
  const s = new ServerSource({
    base: `${server.url}/api/v3/`,
    readToken: "secret",
    fetch: (url, init) => {
      seen.push({ url, auth: new Headers(init?.headers).get("Authorization") });
      return fetch(url, init);
    },
  });
  await s.overlays(ds.id, 0, 0, { t0: 10, t1: 20, kinds: ["tag", "rail"] }).catch(() => {});
  assert.equal(seen[0].url, `${server.url}/api/v3/overlays/${ds.id}/0/0.json?t0=10&t1=20&kinds=tag,rail`);
  assert.equal(seen[0].auth, "Bearer secret");
  assert.deepEqual(s.requestHeaders(), { Authorization: "Bearer secret" });
});

test("static source: export file names, no view, consecutive deltas, client-side overlay filter", async () => {
  const s = new StaticSource({ base: `${server.url}/static/` });
  assert.equal(s.view, undefined);
  assert.equal(s.subscribe, undefined);
  const cat = await s.catalog();
  assert.equal(cat.static, true);
  assert.ok(server.requests(/\/static\/catalog\.json$/).length >= 1);
  assert.ok(Math.abs(s.now() - data.now) < 1000);
  const t = decodeTile(await s.tile(ds.id, { z: 0, x: 0, y: 0 }, 1));
  assert.equal(t.header.version, 1);
  await assert.rejects(s.tile(ds.id, { z: 0, x: 0, y: 0 }, "live"), (e: unknown) => e instanceof HttpError && e.status === 404);
  assert.throws(() => s.tileUrl(ds.id, { z: 0, x: 0, y: 0 }, "live"));
  assert.equal(s.deltaSpan, 1);
  decodeTileDelta(await s.tileDelta(ds.id, 0, 0, 1, 2));
  await assert.rejects(s.tileDelta(ds.id, 0, 0, 1, 3), (e: unknown) => e instanceof HttpError && e.status === 404);
  const all = await s.overlays(ds.id, 0, 0);
  assert.equal(all.intervals.length, 4);
  const rails = await s.overlays(ds.id, 0, 0, { kinds: ["rail"] });
  assert.deepEqual(rails.intervals.map(i => i.kind), ["rail"]);
  const none = await s.overlays(ds.id, 0, 0, { t0: 0, t1: 1 });
  assert.equal(none.intervals.length, 0);
  assert.ok(server.requests(/\/static\/overlays\/.*\.json$/).every(r => !r.path.includes("?")), "static overlays have no query string");
});

test("detectSource: explicit and automatic", async () => {
  const a = await detectSource(`?source=static&data=${encodeURIComponent(server.url + "/static/")}`);
  assert.equal(a.source.kind, "static");
  const b = await detectSource(`?data=${encodeURIComponent(server.url + "/api/v3/")}`);
  assert.equal(b.source.kind, "server");
  // The static root answers catalog.json but not catalog: falls through to static.
  const c = await detectSource(`?data=${encodeURIComponent(server.url + "/static/")}`);
  assert.equal(c.source.kind, "static");
});

test("event stream parser handles split chunks, comments and multi-line data", async () => {
  const chunks = [": ping\n\nevent: ti", "le\ndata: {\"a\":", "1}\n\nevent: hello\r\ndata: x\r\ndata: y\r\n\r\n", "data: z\n\n"];
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      for (const s of chunks) {
        c.enqueue(new TextEncoder().encode(s));
      }
      c.close();
    },
  });
  const got: [string, string][] = [];
  await readEvents(body, (e, d) => got.push([e, d]));
  assert.deepEqual(got, [["tile", "{\"a\":1}"], ["hello", "x\ny"], ["message", "z"]]);
});

test("server source: live events over the fetch-based SSE client", async () => {
  const s = new ServerSource({ base: `${server.url}/api/v3/` });
  const got: string[] = [];
  let resolveHello!: () => void;
  const hello = new Promise<void>(r => (resolveHello = r));
  let resolveTile!: () => void;
  const tile = new Promise<void>(r => (resolveTile = r));
  const stop = s.subscribe({ ds: [ds.id], sfc: [] }, {
    hello: e => {
      got.push(`hello ${e.horizon[ds.id]}`);
      resolveHello();
    },
    tile: e => {
      got.push(`tile ${e.x},${e.y} v${e.v}`);
      resolveTile();
    },
  });
  await hello;
  server.pushEvent("tile", { ds: ds.id, z: 0, x: 1, y: 0, v: 4, now: data.now + 1 }, ds.id);
  await tile;
  stop();
  assert.deepEqual(got, [`hello ${ds.horizon}`, "tile 1,0 v4"]);
});

test("server source: subscriptions share one event stream and get only their events", async () => {
  const s = new ServerSource({ base: `${server.url}/api/v3/` });
  server.clearLog();
  const tiles: string[] = [];
  const movers: string[] = [];
  let hellos = 0;
  const wait = (f: () => boolean) =>
    new Promise<void>((resolve, reject) => {
      const t0 = Date.now();
      const tick = () => (f() ? resolve() : Date.now() - t0 > 3000 ? reject(new Error("timeout")) : setTimeout(tick, 10));
      tick();
    });
  const stopA = s.subscribe({ ds: [ds.id], sfc: [] }, { hello: () => hellos++, tile: e => tiles.push(`a ${e.x}`), movers: () => movers.push("a") });
  let stopB = s.subscribe({ ds: [], sfc: ["m.nauvis"] }, { hello: () => hellos++, tile: e => tiles.push(`b ${e.x}`), movers: e => movers.push(`b ${e.sfc}`) });
  try {
    await wait(() => hellos === 2);
    const opened = server.requests(/\/live\?/);
    assert.equal(opened.length, 1, "one stream for both");
    assert.match(opened[0].path, /ds=.*&sfc=m\.nauvis/);
    server.pushEvent("tile", { ds: ds.id, z: 0, x: 1, y: 0, v: 4, now: data.now + 1 });
    server.pushEvent("tile", { ds: "other.ds.player", z: 0, x: 2, y: 0, v: 1, now: data.now + 1 });
    server.pushEvent("movers", { sfc: "m.nauvis", now: data.now + 1, movers: [], rows: [] });
    await wait(() => tiles.length === 1 && movers.length === 1);
    assert.deepEqual(tiles, ["a 1"]);
    assert.deepEqual(movers, ["b m.nauvis"]);
    // Dropping a subscriber reopens the stream for the rest.
    stopB();
    stopB = () => {};
    await wait(() => server.requests(/\/live\?/).length === 2 && hellos === 3);
    assert.doesNotMatch(server.requests(/\/live\?/)[1].path, /sfc=/);
  } finally {
    stopB();
    stopA();
  }
});

const waitFor = (f: () => boolean, ms = 3000) =>
  new Promise<void>((resolve, reject) => {
    const t0 = Date.now();
    const tick = () => (f() ? resolve() : Date.now() - t0 > ms ? reject(new Error("timeout")) : setTimeout(tick, 10));
    tick();
  });

test("server source: a throwing handler does not take the shared stream down for others", async () => {
  const s = new ServerSource({ base: `${server.url}/api/v3/` });
  server.clearLog();
  const quiet = console.error;
  console.error = () => {};
  const a = { hello: 0, error: 0, tiles: 0 };
  const b = { hello: 0, error: 0 };
  const stopA = s.subscribe({ ds: [ds.id], sfc: [] }, { hello: () => a.hello++, error: () => a.error++, tile: () => a.tiles++ });
  const stopB = s.subscribe({ ds: [], sfc: ["m.nauvis"] }, {
    hello: () => b.hello++,
    error: () => b.error++,
    movers: () => {
      throw new Error("malformed");
    },
  });
  try {
    await waitFor(() => a.hello === 1 && b.hello === 1);
    server.pushEvent("movers", { sfc: "m.nauvis", now: data.now + 1, movers: [], rows: [] });
    server.pushEvent("tile", { ds: ds.id, z: 0, x: 1, y: 0, v: 4, now: data.now + 1 });
    await waitFor(() => a.tiles === 1 && b.hello === 2);
    // B was told and resynced; A saw nothing of it; the stream stayed open.
    assert.equal(b.error, 1);
    assert.equal(a.error, 0);
    assert.equal(a.hello, 1);
    assert.equal(server.requests(/\/live\?/).length, 1);
  } finally {
    console.error = quiet;
    stopB();
    stopA();
  }
});

test("server source: a subscriber the open stream already covers is told its state", async () => {
  const s = new ServerSource({ base: `${server.url}/api/v3/` });
  server.clearLog();
  const a = { hello: 0, error: 0 };
  const c = { hello: 0, error: 0 };
  const d = { hello: 0, error: 0 };
  const stopA = s.subscribe({ ds: [ds.id], sfc: [] }, { hello: () => a.hello++, error: () => a.error++ });
  let stopC = () => {};
  let stopD = () => {};
  let release = () => {};
  try {
    await waitFor(() => a.hello === 1);
    // Up: the last hello again, no new stream.
    stopC = s.subscribe({ ds: [ds.id], sfc: [] }, { hello: () => c.hello++, error: () => c.error++ });
    await waitFor(() => c.hello === 1);
    assert.equal(server.requests(/\/live\?/).length, 1);
    assert.equal(a.hello, 1);
    // Down (the reconnect is held): an error, no hello.
    release = server.hold(/\/live\?/);
    server.dropStreams();
    await waitFor(() => a.error === 1 && c.error === 1);
    stopD = s.subscribe({ ds: [ds.id], sfc: [] }, { hello: () => d.hello++, error: () => d.error++ });
    await waitFor(() => d.error === 1);
    assert.equal(d.hello, 0);
    // Back up: everyone gets the new hello.
    release();
    await waitFor(() => a.hello === 2 && c.hello === 2 && d.hello === 1, 5000);
  } finally {
    release();
    stopD();
    stopC();
    stopA();
  }
});
