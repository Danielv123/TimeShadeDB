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

function fakeSource(opts: { subscribe?: boolean; gate?: Promise<void>; dayDelay?: number } = {}) {
  const calls: string[] = [];
  let handlers: any = null;
  const src: any = {
    kind: opts.subscribe ? "server" : "static",
    now: () => T0 + 3_600_000 + 10_000,
    moverDay(sfc: string, date: string, signal?: AbortSignal) {
      calls.push(`day ${date}`);
      // Answers after dayDelay ms and honours the abort signal like fetch does.
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => (date === "2026-10-10" ? resolve(json("movers_day.json")) : reject(new Error("404"))), opts.dayDelay ?? 0);
        signal?.addEventListener("abort", () => {
          clearTimeout(timer);
          const err = new Error("aborted");
          err.name = "AbortError";
          reject(err);
        });
      });
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
  assert.equal(est.samples, 1, "rows of the resync payload piled up in the outage and do not count");
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

const settle = async (n = 20) => {
  for (let i = 0; i < n; i++) {
    await new Promise(r => setTimeout(r, 2));
  }
};

// Waits for cond instead of a fixed time, so a delayed fake answer is not raced by slow or fast timers.
const until = async (cond: () => boolean, ms = 2000) => {
  for (const end = Date.now() + ms; !cond() && Date.now() < end;) {
    await new Promise(r => setTimeout(r, 2));
  }
};

test("history: a seek that supersedes one with its day index in flight still loads", async () => {
  const { src, calls } = fakeSource({ dayDelay: 50 });
  const view = new MoverView();
  let perf = 0;
  const feed = new MoverFeed(src, SFC, view, new DelayEstimator(), { changed() {} }, () => HORIZON, 5000, () => perf);
  feed.setTime(T0 + 3000, false); // the day index is in flight
  perf += 1000; // a slow scrub step, not timelapse
  feed.setTime(T0 + 200_000, false); // a second seek in the same day
  for (let i = 0; i < 5; i++) {
    feed.ensure(); // every frame while the day index loads
    await tick();
  }
  await until(() => view.size === 3);
  await settle();
  assert.equal(calls.filter(c => c === "day 2026-10-10").length, 1, "the in-flight index is shared");
  assert.equal(calls.filter(c => c === "window 0").length, 1, "frames during the seek do not restart it");
  assert.ok(calls.includes("window 0"), calls.join("; "));
  assert.equal(view.size, 3);
  feed.stop();
});

test("history: stop aborts day index fetches without caching them as missing", async () => {
  const { src, calls } = fakeSource({ dayDelay: 20 });
  const view = new MoverView();
  const feed = new MoverFeed(src, SFC, view, new DelayEstimator(), { changed() {} }, () => HORIZON);
  feed.setTime(T0 + 3000, false);
  feed.stop();
  feed.setTime(T0 + 3000, false);
  await until(() => view.size === 3);
  await settle();
  assert.equal(calls.filter(c => c === "day 2026-10-10").length, 2, calls.join("; "));
  assert.equal(view.size, 3);
  feed.stop();
});

test("history: after a trim, seeking back before the kept history loads again", async () => {
  const { src, calls } = fakeSource();
  const view = new MoverView();
  let perf = 0;
  const feed = new MoverFeed(src, SFC, view, new DelayEstimator(), { changed() {} }, () => HORIZON, 5000, () => perf);
  feed.setTime(T0 + 3000, false);
  await settle();
  const fresh = view.frame(T0 + 3000, feed.horizon()).length;
  assert.ok(fresh >= 2);
  perf += 1000;
  feed.setTime(T0 + 200_000, false); // plays on and trims history before T0 + 80 s
  await settle();
  const n = calls.length;
  perf += 1000;
  feed.setTime(T0 + 3000, false); // back before the kept history
  await settle();
  assert.ok(calls.slice(n).includes("window 0"), `refetched: ${calls.slice(n).join("; ")}`);
  assert.equal(view.frame(T0 + 3000, feed.horizon()).length, fresh);
  feed.stop();
});

test("history: fast playback extends the loaded range instead of seeking again", async () => {
  const { src, calls } = fakeSource();
  const view = new MoverView();
  let perf = 0;
  const feed = new MoverFeed(src, SFC, view, new DelayEstimator(), { changed() {} }, () => HORIZON, 5000, () => perf);
  let t = T0 + 3000;
  feed.setTime(t, false);
  await settle();
  for (let i = 0; i < 6; i++) {
    perf += 100;
    t += 300_000; // 3000x, below MAX_SPEED
    feed.setTime(t, false);
    assert.equal(feed.active, true);
    await settle(3);
    feed.ensure();
    await settle(3);
  }
  assert.equal(calls.filter(c => c === "window 0").length, 1, calls.join("; "));
  assert.equal(calls.filter(c => c.startsWith("keyframe")).length, 0, "no seek again (a seek past T0 + 1 h would load its keyframe)");
  assert.ok(feed.horizon() > t, "loaded ahead of the displayed time");
  feed.stop();
});

test("live: the resync payload after a reconnect does not count for the delay", async () => {
  const { src, emit } = fakeSource({ subscribe: true });
  const live = src.moverLive;
  let first = true;
  src.moverLive = async () => {
    if (first) {
      first = false;
      throw new Error("offline"); // the first payload fails; the resync brings every row
    }
    return live();
  };
  const view = new MoverView();
  const est = new DelayEstimator(2000);
  const feed = new MoverFeed(src, SFC, view, est, { changed() {}, error() {} }, () => HORIZON);
  feed.setTime(src.now() - 2000, true);
  await tick();
  emit().hello({ now: src.now(), horizon: {} });
  await settle(5);
  assert.ok(view.size > 0, "the resync loaded the movers");
  assert.equal(est.samples, 0);
  assert.equal((est as any).readyN, 0, "nor for readiness");
  feed.stop();
});

test("live: while the event stream is down the data horizon stays where it broke", async () => {
  const { src, emit } = fakeSource({ subscribe: true });
  let now = src.now();
  src.now = () => now;
  const view = new MoverView();
  const est = new DelayEstimator(2000);
  const feed = new MoverFeed(src, SFC, view, est, { changed() {}, error() {} }, () => HORIZON);
  feed.setTime(now - 2000, true);
  await tick();
  const h0 = feed.horizon();
  assert.equal(h0, est.horizon(now));
  emit().error(new Error("dropped"));
  now += 30_000;
  assert.equal(feed.horizon(), h0, "frozen while down");
  emit().hello({ now, horizon: {} });
  assert.equal(feed.horizon(), est.horizon(now), "moves again after the reconnect");
  feed.stop();
});

test("live without SSE: a failed poll freezes the horizon until a poll succeeds", async () => {
  const { src } = fakeSource();
  let now = src.now();
  src.now = () => now;
  const live = src.moverLive;
  let fail = false;
  src.moverLive = async () => {
    if (fail) {
      throw new Error("offline");
    }
    return live();
  };
  const est = new DelayEstimator(2000);
  const feed = new MoverFeed(src, SFC, new MoverView(), est, { changed() {}, error() {} }, () => HORIZON, 20);
  feed.setTime(now, true);
  await settle(3);
  fail = true;
  await new Promise(r => setTimeout(r, 60));
  const frozen = feed.horizon();
  now += 30_000;
  assert.equal(feed.horizon(), frozen);
  fail = false;
  await new Promise(r => setTimeout(r, 60));
  assert.equal(feed.horizon(), est.horizon(now));
  feed.stop();
});
