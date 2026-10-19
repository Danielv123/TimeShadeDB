// MoverView (frames, never-snap corrections, fixtures) and live timing (delay, clock).
import "./eval/ts-resolve.mjs";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const { MoverView, CORRECTION_MS, CORRECTION_RATE, MAX_CORRECTION_MS } = await import("./view.ts");
const T = await import("./track.ts");
const { DelayEstimator, DataClock, MIN_DELAY_MS, MAX_DELAY_MS, delayConfig } = await import("./clock.ts");
const { decodeMoverWindow } = await import("../api/wire.ts");

const fixture = (name: string) => decodeMoverWindow(new Uint8Array(readFileSync(new URL(`../../fixtures/${name}`, import.meta.url))));
const meta = { id: "j1", k: "t" as const, f: "player", name: "locomotive", n: 3, o: "LCC" };

test("fixture window: train, spidertron and player are drawn; the train has its carriages", () => {
  const v = new MoverView();
  v.addWindow(fixture("movers_window.bin"));
  assert.equal(v.size, 3);
  const w = fixture("movers_window.bin");
  const t = w.rows.filter(r => r.mover === 0)[3].t + 50;
  const shown = v.frame(t, Infinity);
  const train = shown.find(m => m.kind === "t");
  assert.ok(train, "train shown");
  assert.equal(train.cars.length, 3);
  for (let i = 1; i < 3; i++) {
    const d = Math.hypot(train.cars[i].x - train.cars[i - 1].x, train.cars[i].y - train.cars[i - 1].y);
    assert.ok(d > 6 && d < 7.2, `spacing ${d}`);
  }
  assert.ok(shown.some(m => m.kind === "v"));
});

test("fixture train crosses its handoff without disappearing or jumping", () => {
  const v = new MoverView();
  const w = fixture("movers_window.bin");
  v.addWindow(w);
  const rows = w.rows.filter(r => r.mover === 0 || w.movers[r.mover].id === w.movers[0].id);
  const gone = rows.find(r => (r.flags & T.F_GONE) !== 0)!;
  let prev: { x: number; y: number } | null = null;
  for (let t = gone.t - 1500; t <= gone.t + 1500; t += 20) {
    const m = v.frame(t, Infinity).find(s => s.id === w.movers[0].id);
    assert.ok(m, `visible at ${t - gone.t}`);
    if (prev) {
      assert.ok(Math.hypot(m.x - prev.x, m.y - prev.y) < 3, `step at ${t - gone.t}`);
    }
    prev = { x: m.x, y: m.y };
  }
});

test("keyframe and live payloads add rows; duplicates are ignored", () => {
  const v = new MoverView();
  let seen = 0;
  v.addWindow(fixture("movers_keyframe.bin"), () => seen++);
  assert.equal(seen, 3);
  v.addWindow(fixture("movers_keyframe.bin"), () => seen++);
  assert.equal(seen, 3, "same rows again are not new");
  v.addWindow(fixture("movers_live.bin"), () => seen++);
  assert.ok(seen > 3);
});

test("live events add rows with carriages", () => {
  const v = new MoverView();
  v.addLive({
    sfc: "s",
    now: 2000,
    movers: [meta],
    rows: [
      [0, 1000, 60, 0, 0, 0.25, 0, null],
      [0, 1500, 90, 5, 0, 0.25, 1, [[5, 0, 0.25], [-2, 0, 0.25], [-9, 0, 0.25]]],
    ],
  });
  const tr = v.tracks.get("j1")!;
  assert.equal(tr.s.length, 2);
  assert.equal(tr.s[1].car?.length, 3);
  assert.equal(tr.s[1].tick, 90);
});

test("new data never makes a drawn train jump: head and carriages blend", () => {
  const v = new MoverView();
  for (let i = 0; i < 6; i++) {
    v.add(meta, T.makeSample(meta, i * 500, i * 5, 0, 0.25));
  }
  // drawn up to t=3400 by extrapolation (horizon covers it)
  let last = v.frame(3000, 3500)[0];
  const lastCars = () => last.cars.map(c => [c.x, c.y]);
  let prevCars = lastCars();
  let prev = { x: last.x, y: last.y };
  for (let t = 3020; t <= 3400; t += 20) {
    last = v.frame(t, 3500)[0];
    prev = { x: last.x, y: last.y };
    prevCars = lastCars();
  }
  // the train actually turned: a sample south-east of the extrapolated line arrives
  v.add(meta, T.makeSample(meta, 3000, 28, 3, 0.3));
  v.add(meta, T.makeSample(meta, 3500, 31, 6, 0.4));
  for (let t = 3420; t <= 3420 + CORRECTION_MS + 100; t += 20) {
    const m = v.frame(t, 4000)[0];
    assert.ok(Math.hypot(m.x - prev.x, m.y - prev.y) < 1, `head step at ${t}: ${Math.hypot(m.x - prev.x, m.y - prev.y)}`);
    const cars = m.cars.map(c => [c.x, c.y]);
    for (let i = 0; i < cars.length; i++) {
      assert.ok(Math.hypot(cars[i][0] - prevCars[i][0], cars[i][1] - prevCars[i][1]) < 1.2, `carriage ${i} step at ${t}`);
    }
    prev = { x: m.x, y: m.y };
    prevCars = cars;
  }
  // after the blend the drawing follows the new curve exactly
  const h = T.headAt(v.tracks.get("j1")!, 3420 + CORRECTION_MS + 100, 4000, T.newHead());
  assert.ok(Math.hypot(prev.x - h.x, prev.y - h.y) < 1e-9);
});

test("a large correction blends no faster than CORRECTION_RATE", () => {
  const v = new MoverView();
  for (let i = 0; i < 6; i++) {
    v.add(meta, T.makeSample(meta, i * 500, i * 5, 0, 0.25)); // 10 tiles/s east
  }
  let px = v.frame(2500, 2600)[0].x;
  for (let t = 2600; t <= 4000; t += 100) {
    px = v.frame(t, 4100)[0].x; // extrapolated to x = 40 at t=4000
  }
  // the train actually stopped at x = 28: a 12-tile correction
  v.add(meta, T.makeSample(meta, 2800, 28, 0, 0.25, T.F_STOP));
  v.add(meta, T.makeSample(meta, 4500, 28, 0, 0.25, T.F_STOP));
  const expected = Math.min(MAX_CORRECTION_MS, (1000 * 12) / CORRECTION_RATE);
  let steps = 0;
  for (let t = 4100; t <= 4000 + expected + 200; t += 100) {
    const m = v.frame(t, 5000)[0];
    assert.ok(Math.abs(m.x - px) <= (CORRECTION_RATE * 100) / 1000 + 1e-6, `step at ${t}: ${m.x - px}`);
    if (m.x !== px) {
      steps++;
    }
    px = m.x;
  }
  assert.ok(Math.abs(px - 28) < 1e-9, `settles on the new data: ${px}`);
  assert.ok(steps >= 7, `blended over ${steps} frames`);
});

test("an overrun waits on the track for the true position instead of snapping or sliding back", () => {
  const v = new MoverView();
  for (let i = 0; i < 6; i++) {
    v.add(meta, T.makeSample(meta, i * 500, i * 20, 0, 0.25)); // 40 tiles/s east
  }
  let px = 0;
  for (let t = 2500; t <= 4000; t += 100) {
    px = v.frame(t, 4100)[0].x; // extrapolated to x = 160 at t=4000
  }
  assert.ok(px > 150, `ran on: ${px}`);
  // It actually slowed down hard: 10 tiles/s from x = 105 (a 45-tile overrun, over MAX_CORRECTION).
  for (let i = 1; i <= 20; i++) {
    v.add(meta, T.makeSample(meta, 2500 + i * 500, 100 + i * 5, 0, 0.25));
  }
  let caught = NaN;
  for (let t = 4100; t <= 14_000; t += 100) {
    const m = v.frame(t, 15_000)[0];
    assert.ok(m.x >= px - 1e-9, `never backwards at ${t}: ${m.x} < ${px}`);
    assert.ok(m.x - px <= 10 * 0.1 + 1e-6, `no faster than the train at ${t}`);
    assert.ok(Math.abs(m.y) < 1e-9, "stays on its line");
    const h = T.headAt(v.tracks.get("j1")!, t, 15_000, T.newHead());
    if (Number.isNaN(caught) && Math.abs(m.x - h.x) < 1e-9) {
      caught = t;
    }
    px = m.x;
  }
  assert.ok(!Number.isNaN(caught), "follows the data again once caught up");
});

test("a seek does not blend", () => {
  const v = new MoverView();
  for (let i = 0; i < 6; i++) {
    v.add(meta, T.makeSample(meta, i * 500, i * 5, 0, 0.25));
  }
  v.frame(1000, Infinity);
  v.add(meta, T.makeSample(meta, 2600, 13, 2, 0.25));
  const m = v.frame(500, Infinity)[0];
  const h = T.headAt(v.tracks.get("j1")!, 500, Infinity, T.newHead());
  assert.equal(m.x, h.x);
});

test("bounds filter and trim", () => {
  const v = new MoverView();
  for (let i = 0; i < 6; i++) {
    v.add(meta, T.makeSample(meta, i * 500, 1000 + i * 5, 0, 0.25));
  }
  assert.equal(v.frame(1000, Infinity, { bounds: [-100, -100, 100, 100] }).length, 0);
  assert.equal(v.frame(1000, Infinity, { bounds: [900, -100, 1100, 100] }).length, 1);
  v.add({ ...meta, id: "j2" }, T.makeSample({ ...meta, id: "j2" }, 100, 0, 0, 0, T.F_GONE));
  v.trim(1_000_000);
  assert.equal(v.tracks.has("j2"), false, "ended journeys are dropped");
  assert.equal(v.tracks.has("j1"), true, "a parked journey stays");
});

test("delay estimator: p99 lag + 0.5 s, clamped, changing smoothly", () => {
  const d = new DelayEstimator(2000);
  assert.equal(d.current, 2000);
  for (let i = 0; i < 1000; i++) {
    d.observe(150 + (i % 100) * 10); // 150..1140 ms
  }
  assert.ok(Math.abs(d.target() - (1140 + 500)) <= 10, `target ${d.target()}`);
  d.delay(0);
  const after100 = d.delay(100); // shrinks by at most 5 ms per 100 ms (time runs at most 1.05x)
  assert.ok(Math.abs(after100 - 1995) < 1e-9, `slewed ${after100}`);
  const later = d.delay(100_000);
  assert.ok(Math.abs(later - d.target()) < 1e-9);
  assert.ok(Math.abs(d.horizon(10_000) - (10_000 - 1140)) <= 10);
  // grows by at most 20 ms per 100 ms (time slows to 0.8x)
  for (let i = 0; i < 1000; i++) {
    d.observe(3000);
  }
  const grown = d.delay(100_100);
  assert.ok(Math.abs(grown - (later + 20)) < 1e-9, `grew ${grown - later}`);
  const tiny = new DelayEstimator(0);
  assert.equal(tiny.current, MIN_DELAY_MS);
  const huge = new DelayEstimator(1e9);
  assert.equal(huge.current, MAX_DELAY_MS);
});

test("delay estimator: hysteresis, and readiness of moving movers raises the delay up to its cap", () => {
  const d = new DelayEstimator(2000);
  for (let i = 0; i < 1000; i++) {
    d.observe(150 + (i % 100) * 10);
  }
  const base = d.target();
  // a small drop of the estimate does not move the target
  for (let i = 0; i < 1024; i++) {
    d.observe(150 + (i % 100) * 9); // p99 about 1040: 100 ms lower
  }
  assert.equal(d.target(), base, "within the hysteresis");
  // samples of moving trains arrive up to 2 s after the sample before them
  for (let i = 0; i < 1000; i++) {
    d.observeReady(500 + (i % 100) * 15); // 500..1985 ms, p90 about 1835
  }
  const ready = d.readyQuantile(delayConfig.readyQuantile);
  assert.ok(Math.abs(d.target() - (ready + delayConfig.readyMarginMs)) < 1e-9, `readiness target ${d.target()}`);
  // capped
  for (let i = 0; i < 1024; i++) {
    d.observeReady(20_000);
  }
  assert.equal(d.target(), delayConfig.readyCapMs);
});

test("data clock never goes back", () => {
  let p = 0;
  const c = new DataClock(() => p);
  c.see(1000);
  p = 500;
  assert.equal(c.now(), 1500);
  c.see(1200); // older than now: ignored
  assert.equal(c.now(), 1500);
  c.see(5000);
  assert.equal(c.now(), 5000);
});
