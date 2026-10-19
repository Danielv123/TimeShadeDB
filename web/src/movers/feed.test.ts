// MoverFeed against a fake DataSource serving web/fixtures.
import "./eval/ts-resolve.mjs";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const { MoverFeed } = await import("./feed.ts");
const { MoverView } = await import("./view.ts");
const { DelayEstimator } = await import("./clock.ts");

const dir = new URL("../../fixtures/", import.meta.url);
const bin = (name: string): ArrayBuffer => {
  const b = readFileSync(new URL(name, dir));
  return b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength) as ArrayBuffer;
};
const json = (name: string): any => JSON.parse(readFileSync(new URL(name, dir), "utf8"));
const catalog = json("catalog.json");
const SFC = catalog.maps[0].surfaces[0].id as string;
const HORIZON = catalog.maps[0].surfaces[0].movers.horizon as number;
const T0 = 1791630000000;

const tick = () => new Promise(r => setTimeout(r, 0));

function fakeSource(opts: { subscribe?: boolean; gate?: Promise<void> } = {}) {
  const calls: string[] = [];
  let handlers: any = null;
  const src: any = {
    kind: opts.subscribe ? "server" : "static",
    now: () => T0 + 3_600_000 + 10_000,
    async moverDay(sfc: string, date: string) {
      calls.push(`day ${date}`);
      if (date !== "2026-10-10") {
        throw new Error("404");
      }
      return json("movers_day.json");
    },
    async moverWindow(sfc: string, start: number) {
      calls.push(`window ${start - T0}`);
      await opts.gate;
      return bin("movers_window.bin");
    },
    async moverKeyframe(sfc: string, t: number) {
      calls.push(`keyframe ${t - T0}`);
      return bin("movers_keyframe.bin");
    },
    async moverLive() {
      calls.push("live");
      return bin("movers_live.bin");
    },
  };
  if (opts.subscribe) {
    src.subscribe = (topics: any, h: any) => {
      calls.push(`subscribe ${topics.sfc.join(",")}`);
      handlers = h;
      return () => calls.push("unsubscribe");
    };
  }
  return { src, calls, emit: () => handlers };
}

test("history: day index, then the windows around the time, then frames draw the train", async () => {
  const { src, calls } = fakeSource();
  const view = new MoverView();
  let changed = 0;
  const feed = new MoverFeed(src, SFC, view, new DelayEstimator(), { changed: () => changed++ }, () => HORIZON);
  feed.setTime(T0 + 3000, false);
  for (let i = 0; i < 10; i++) {
    await tick();
  }
  assert.ok(calls.includes("day 2026-10-10"));
  assert.ok(calls.includes("window 0"), calls.join("; "));
  assert.ok(!calls.includes("live"), "the closed range needs no live payload");
  assert.ok(changed > 0);
  assert.equal(view.size, 3);
  assert.ok(feed.horizon() >= T0 + 3000 + 60_000, "loaded through the lookahead");
  assert.ok(view.frame(T0 + 3000, feed.horizon()).length >= 2);
  feed.stop();
});

test("history: a superseded seek's results are dropped", async () => {
  let open!: () => void;
  const gate = new Promise<void>(r => (open = r));
  const { src } = fakeSource({ gate });
  const view = new MoverView();
  let perf = 0;
  const feed = new MoverFeed(src, SFC, view, new DelayEstimator(), { changed() {} }, () => HORIZON, 5000, () => perf);
  feed.setTime(T0 + 3000, false);
  await tick();
  perf += 100;
  feed.setTime(T0 - 86_400_000 * 3, false); // a day with nothing, before the closed horizon
  assert.equal(feed.active, false, "a jump this fast looks like timelapse");
  perf += 300;
  assert.equal(feed.active, true, "time stopped changing");
  feed.ensure();
  open();
  for (let i = 0; i < 10; i++) {
    await tick();
  }
  assert.equal(view.size, 0, "rows of the first seek were not added");
  feed.stop();
});

test("history: keyframe first when the time is after one", async () => {
  const { src, calls } = fakeSource();
  const view = new MoverView();
  const feed = new MoverFeed(src, SFC, view, new DelayEstimator(), { changed() {} }, () => HORIZON);
  feed.setTime(T0 + 3_600_000 + 1000, false);
  for (let i = 0; i < 10; i++) {
    await tick();
  }
  assert.ok(calls.includes("keyframe 3600000"), calls.join("; "));
  assert.ok(calls.includes("live"), "past the closed horizon the live payload fills in");
  feed.stop();
});

test("live with SSE: live payload, subscription, events counted for the delay", async () => {
  const { src, calls, emit } = fakeSource({ subscribe: true });
  const view = new MoverView();
  const est = new DelayEstimator(2000);
  const feed = new MoverFeed(src, SFC, view, est, { changed() {} }, () => HORIZON);
  feed.setTime(src.now() - 2000, true);
  for (let i = 0; i < 5; i++) {
    await tick();
  }
  assert.deepEqual(calls.slice(0, 2).sort(), ["live", `subscribe ${SFC}`].sort());
  assert.equal(est.samples, 0, "the initial payload does not count");
  const meta = { id: "p:bob", k: "p", f: "player", name: "bob" };
  emit().movers({ sfc: SFC, now: src.now(), movers: [meta], rows: [[0, src.now() - 700, null, 1, 2, 0.5, 16, null]] });
  emit().movers({ sfc: "other", now: src.now(), movers: [meta], rows: [[0, src.now() - 100, null, 1, 2, 0.5, 16, null]] });
  assert.equal(est.samples, 1);
  assert.ok(Math.abs(est.quantile(0.5) - 700) < 1e-9);
  assert.ok(view.tracks.has("p:bob"));
  emit().hello({ now: src.now(), horizon: {} });
  await tick();
  assert.equal(calls.filter(c => c === "live").length, 2, "resync after reconnect");
  feed.stop();
  assert.ok(calls.includes("unsubscribe"));
});

test("live without SSE (static export): polls live.bin", async () => {
  const { src, calls } = fakeSource();
  const view = new MoverView();
  const feed = new MoverFeed(src, SFC, view, new DelayEstimator(), { changed() {} }, () => HORIZON, 10);
  feed.setTime(src.now(), true);
  await new Promise(r => setTimeout(r, 60));
  feed.stop();
  const n = calls.filter(c => c === "live").length;
  assert.ok(n >= 3, `polled ${n} times`);
  await new Promise(r => setTimeout(r, 30));
  assert.equal(calls.filter(c => c === "live").length, n, "stopped");
});

test("very fast playback skips mover loading", async () => {
  const { src, calls } = fakeSource();
  let perf = 0;
  const feed = new MoverFeed(src, SFC, new MoverView(), new DelayEstimator(), { changed() {} }, () => HORIZON, 5000, () => perf);
  feed.setTime(T0, false);
  for (let i = 1; i <= 10; i++) {
    perf += 16;
    feed.setTime(T0 + i * 3_600_000, false); // an hour per frame
  }
  assert.equal(feed.active, false);
  feed.stop();
  void calls;
});
