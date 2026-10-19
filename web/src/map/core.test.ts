// Unit tests for the DOM-free parts of the map engine: camera math, the byte LRU, the
// scheduler, the data clock, colour lookup tables and version resolution.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

import { ancestorSubRect, boundsAtLevel, fitCamera, intersectRange, levelFor, rangeTiles, screenToWorld, tileRange, visibleWorld, worldToScreen, zoomAt } from "./camera.ts";
import { DataClock } from "./clock.ts";
import { ByteLru } from "./lru.ts";
import { expandRgba, lutFor, paletteLut, rgb565Lut, snapRect } from "./renderer.ts";
import { defaultMaxInFlight, Scheduler } from "./scheduler.ts";
import { TimelineVersions, timelineVersionAt, ViewVersions } from "./versions.ts";
import { HttpError } from "./sources.ts";
import { decodePalette, decodeTile, pixelColor, rgb565ToRgb } from "../api/wire.ts";
import type { CatalogDatastore, DataSource, TimelineResponse, ViewResponse } from "../api/types";

const fixtures = new URL("../../fixtures/", import.meta.url);
const bin = (n: string): Uint8Array => new Uint8Array(readFileSync(new URL(n, fixtures)));
const json = (n: string): any => JSON.parse(readFileSync(new URL(n, fixtures), "utf8"));

test("level selection follows device pixels per tile and the available levels", () => {
  assert.equal(levelFor(1, 1), 0);
  assert.equal(levelFor(4, 1), 0);
  assert.equal(levelFor(0.5, 1), -1);
  assert.equal(levelFor(0.5, 2), 0, "dpr 2 at half scale still shows z0");
  assert.equal(levelFor(1 / 16, 1), -4);
  assert.equal(levelFor(1 / 16, 1, [0, -1, -2]), -2, "clamped to the coarsest level that exists");
  assert.equal(levelFor(1e-9, 1), -10);
});

test("visible tile range at 1920x1080 and transforms round-trip", () => {
  const cam = { x: 0, y: 0, scale: 1 };
  const vp = { width: 1920, height: 1080, dpr: 1 };
  const w = visibleWorld(cam, vp);
  assert.deepEqual(w, [-960, -540, 960, 540]);
  const r = tileRange(w, 0);
  // x: -960..960 relative to origin -256 -> tiles -2..2; y: -540..540 -> -1..1.
  assert.deepEqual([r.x0, r.x1, r.y0, r.y1], [-2, 2, -1, 1]);
  assert.equal(rangeTiles(r).length, 15);
  const ring = tileRange(w, 0, 1);
  assert.deepEqual([ring.x0, ring.x1, ring.y0, ring.y1], [-3, 3, -2, 2]);
  // A rectangle ending exactly on a tile edge does not include the next tile.
  const edge = tileRange([-256, -256, 256, 256], 0);
  assert.deepEqual([edge.x0, edge.x1, edge.y0, edge.y1], [0, 0, 0, 0]);
  const [sx, sy] = worldToScreen(cam, vp, 100, -50);
  assert.deepEqual(screenToWorld(cam, vp, sx, sy), [100, -50]);
  // Bounds intersection.
  assert.equal(intersectRange(r, [5, 5, 6, 6]), null);
  assert.deepEqual(intersectRange(r, [0, 0, 1, 1]), { z: 0, x0: 0, y0: 0, x1: 1, y1: 1 });
  assert.deepEqual(boundsAtLevel([-4, -3, 3, 2], -1), [-2, -2, 1, 1]);
  assert.deepEqual(boundsAtLevel([-4, -3, 3, 2], -2), [-1, -1, 0, 0]);
});

test("zoom about a point keeps the point under the cursor", () => {
  const vp = { width: 800, height: 600, dpr: 1 };
  const cam = { x: 10, y: 20, scale: 1 };
  const before = screenToWorld(cam, vp, 100, 450);
  const next = zoomAt(cam, vp, 100, 450, 4);
  const after = screenToWorld(next, vp, 100, 450);
  assert.ok(Math.abs(before[0] - after[0]) < 1e-9 && Math.abs(before[1] - after[1]) < 1e-9);
  const fit = fitCamera([-256, -256, 768, 256], vp);
  assert.equal(fit.x, 256);
  assert.ok(fit.scale <= 800 / 1024 + 1e-12);
});

test("ancestor sub-rectangles", () => {
  assert.deepEqual(ancestorSubRect(0, 1, 0, -1), [256, 0, 256, 256]);
  assert.deepEqual(ancestorSubRect(0, -1, -1, -1), [256, 256, 256, 256]);
  assert.deepEqual(ancestorSubRect(0, 5, 2, -2), [128, 256, 128, 128]);
  assert.deepEqual(ancestorSubRect(-1, 3, 3, -3), [384, 384, 128, 128]);
});

test("byte LRU evicts oldest unpinned entries first", () => {
  const evicted: string[] = [];
  const lru = new ByteLru<string, number>({ budget: 300, onEvict: k => evicted.push(k) });
  lru.set("a", 1, 100);
  lru.set("b", 2, 100);
  lru.set("c", 3, 100);
  lru.get("a");
  lru.pin("b");
  lru.set("d", 4, 100);
  assert.deepEqual(evicted, ["c"], "b is pinned, a was used recently");
  assert.equal(lru.bytes, 300);
  lru.unpinAll();
  lru.set("e", 5, 150);
  assert.deepEqual(evicted, ["c", "b", "a"]);
  assert.ok(lru.rekey("e", "e2") && lru.has("e2") && !lru.has("e"));
  lru.resize("e2", 10);
  assert.equal(lru.bytes, 110);
  lru.clear();
  assert.equal(lru.bytes, 0);
});

test("scheduler: concurrency limit, priority order, retain aborts unwanted", async () => {
  const started: string[] = [];
  const aborted: string[] = [];
  const s = new Scheduler({ maxInFlight: 2, onStart: k => started.push(k), onAbort: k => aborted.push(k) });
  const resolvers = new Map<string, () => void>();
  const signals = new Map<string, AbortSignal>();
  const job = (k: string) => (signal: AbortSignal) =>
    new Promise<void>(r => {
      resolvers.set(k, r);
      signals.set(k, signal);
    });
  for (const [k, p] of [["far", 50], ["near", 1], ["mid", 10], ["ring", 1e7]] as [string, number][]) {
    s.want(k, p, job(k));
  }
  s.pump();
  assert.deepEqual(started, ["near", "mid"]);
  assert.equal(s.inFlight, 2);
  // Priority updates of queued jobs take effect.
  s.want("ring", 0, job("ring"));
  resolvers.get("near")!();
  await new Promise(r => setTimeout(r, 0));
  assert.deepEqual(started, ["near", "mid", "ring"]);
  // "mid" leaves the view: aborted; "far" stays queued.
  s.retain(new Set(["far", "ring"]));
  assert.deepEqual(aborted, ["mid"]);
  assert.equal(signals.get("mid")!.aborted, true);
  s.pump();
  assert.deepEqual(started, ["near", "mid", "ring", "far"]);
  assert.equal(defaultMaxInFlight("http/1.1"), 6);
  assert.equal(defaultMaxInFlight("h2"), 16);
});

test("data clock follows server time and ignores stale observations", () => {
  let p = 1000;
  const c = new DataClock(() => p);
  c.observe(5_000_000);
  p += 250;
  assert.equal(c.now(), 5_000_250);
  c.observe(5_000_100); // older (cached catalog): ignored
  assert.equal(c.now(), 5_000_250);
  c.observe(5_001_000);
  assert.equal(c.now(), 5_001_000);
  c.observe(1_000); // far behind: the server clock moved back
  assert.equal(c.now(), 1_000);
});

test("RGB565 lookup tables match the reference expansion and the fixtures", () => {
  const lut = rgb565Lut();
  for (const c of [0x0001, 0xffff, 0xf8c3, 0x1234, 0x07e0]) {
    const [r, g, b] = rgb565ToRgb(c);
    assert.equal(lut[c] >>> 24, 0xff);
    assert.equal(lut[c] & 0xff, r);
    assert.equal((lut[c] >> 8) & 0xff, g);
    assert.equal((lut[c] >> 16) & 0xff, b);
  }
  assert.equal(lut[0] >>> 0, 0xff000000, "uncharted is opaque black");
  const pal = decodePalette(bin("palette.bin"));
  const expected = json("expected.json");
  for (const name of ["tile_z0_0_0_v2.bin", "tile_zm1_0_0_v2.bin", "tile_z0_0_0_live.bin"]) {
    const t = decodeTile(bin(name));
    const rgba = expandRgba(t.pixels, lutFor(t.header.format, paletteLut(pal)));
    for (const [px, py, want] of expected[name].probes as number[][]) {
      const i = py * 512 + px;
      const c = pixelColor(t.pixels, t.header.format, i, pal);
      assert.equal(c, want);
      assert.equal(rgba[i] >>> 0, lut[want] >>> 0, `${name} ${px},${py}`);
    }
  }
  assert.deepEqual(snapRect(0.4, 10.6, 511.2, 512, 1), [0, 11, 512, 512]);
  assert.deepEqual(snapRect(0.25, 0, 100.25, 1, 2), [1, 0, 200, 2]);
});

// ---------------------------------------------------------------------------------------
// Version resolution

function fakeSource(over: Partial<DataSource>): DataSource {
  const no = () => Promise.reject(new Error("not used"));
  return {
    kind: "server",
    now: () => 0,
    catalog: no,
    palette: no,
    timeline: no,
    tile: no,
    tileDelta: no,
    moverDay: no,
    moverWindow: no,
    moverKeyframe: no,
    moverLive: no,
    overlays: no,
    ...over,
  } as DataSource;
}

const cat = json("catalog.json");
const fixtureDs: CatalogDatastore = cat.maps[0].surfaces[0].datastores[0];
const tick = () => new Promise(r => setTimeout(r, 0));

test("timeline versions: v0, points, days and missing tiles", async () => {
  const timeline: TimelineResponse = json("timeline_z0.json");
  const calls: string[] = [];
  const src = fakeSource({
    timeline: async (ds, z, date) => {
      calls.push(`${ds}/${z}/${date}`);
      if (date !== timeline.date) {
        throw new HttpError(404, "not_found", "no day");
      }
      return timeline;
    },
  });
  let changes = 0;
  const tv = new TimelineVersions(src, fixtureDs, () => changes++);
  const range = { z: 0, x0: 0, y0: 0, x1: 1, y1: 0 };
  tv.setTarget(1791630059999);
  assert.equal(tv.get(0, 0, 0), undefined, "unknown until loaded");
  tv.ensure(range);
  tv.ensure(range);
  await tick();
  assert.equal(calls.length, 1, "one request per (level, day)");
  assert.equal(changes, 1);
  assert.equal(tv.get(0, 0, 0), null, "before version 1");
  tv.setTarget(1791630060000);
  assert.deepEqual(tv.get(0, 0, 0), { v: 1 });
  assert.deepEqual(tv.get(0, 1, 0), { v: 1 });
  tv.setTarget(1791630125000);
  assert.deepEqual(tv.get(0, 0, 0), { v: 2 });
  assert.deepEqual(tv.get(0, 1, 0), { v: 1 });
  assert.equal(tv.get(0, 5, 5), null, "tiles absent from the timeline have no data");
  tv.setTarget("latest");
  assert.deepEqual(tv.get(0, 0, 0), { v: 2 }, "latest = newest committed state");
  tv.setTarget(fixtureDs.first - 1);
  assert.equal(tv.get(0, 0, 0), null, "before the datastore's first update nothing is fetched");
  // A time far after the last data uses the last day's timeline.
  tv.setTarget(fixtureDs.horizon + 10 * 86400_000);
  assert.deepEqual(tv.get(0, 0, 0), { v: 2 });
  assert.equal(calls.length, 1);
  assert.equal(timelineVersionAt({ x: 0, y: 0, v0: 7, points: [[10, 8], [20, 9]] }, 15), 8);
  assert.equal(timelineVersionAt({ x: 0, y: 0, v0: 7, points: [[10, 8], [20, 9]] }, 5), 7);
  tv.dispose();
});

test("view versions: cells, missing tiles and live events", async () => {
  const view: ViewResponse = json("view_z0.json");
  const calls: string[] = [];
  const src = fakeSource({
    view: async (ds, z, bbox, t) => {
      calls.push(`${z} ${bbox.join(",")} ${t}`);
      return view;
    },
  });
  const vv = new ViewVersions(src, fixtureDs, () => {});
  assert.equal(vv.get(0, 0, 0), undefined);
  vv.ensure({ z: 0, x0: -1, y0: -1, x1: 2, y1: 1 });
  await tick();
  // Cells are clipped to the datastore bounds [0,0,1,0]; tiles outside get no request.
  assert.deepEqual(calls, ["0 0,0,1,0 latest"]);
  assert.deepEqual(vv.get(0, 0, 0), { v: 2, live: true, ls: 68 });
  assert.deepEqual(vv.get(0, 1, 0), { v: 1, live: undefined, ls: undefined });
  assert.equal(vv.get(0, 0, 1), null);
  assert.equal(vv.get(0, -1, -1), null, "cell outside the bounds counts as loaded and empty");
  assert.ok(vv.apply({ ds: fixtureDs.id, z: 0, x: 1, y: 0, v: 3, now: 0 }));
  assert.deepEqual(vv.get(0, 1, 0), { v: 3, live: undefined, ls: undefined });
  assert.ok(!vv.apply({ ds: fixtureDs.id, z: 0, x: 1, y: 0, v: 3, now: 0 }), "same state: no change");
  assert.ok(!vv.apply({ ds: "other", z: 0, x: 1, y: 0, v: 9, now: 0 }));
  vv.dispose();
});
