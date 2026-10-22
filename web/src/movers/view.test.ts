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

test("an overrun wait ends when the true head turns away; the rest blends without a jump", () => {
  const v = new MoverView();
  for (let i = 0; i < 6; i++) {
    v.add(meta, T.makeSample(meta, i * 500, i * 20, 0, 0.25)); // 40 tiles/s east
  }
  let prev = v.frame(2500, 4100)[0];
  for (let t = 2600; t <= 4000; t += 100) {
    prev = v.frame(t, 4100)[0]; // extrapolated to x = 160 at t=4000
  }
  // It slowed to 10 tiles/s and turned north at x = 120, short of the drawn point.
  const rows: [number, number, number][] = [[3000, 105, 0], [3500, 110, 0], [4000, 115, 0], [4500, 120, 0]];
  for (let i = 1; i <= 12; i++) {
    rows.push([4500 + i * 500, 120, i * 5]);
  }
  for (const [t, x, y] of rows) {
    v.add(meta, T.makeSample(meta, t, x, y, y > 0 ? 0 : 0.25));
  }
  let settled = NaN;
  for (let t = 4100; t <= 10_000; t += 100) {
    const m = v.frame(t, 11_000)[0];
    const step = Math.hypot(m.x - prev.x, m.y - prev.y);
    assert.ok(step <= 1 + (CORRECTION_RATE * 100) / 1000 + 0.05, `no jump at ${t}: ${step}`);
    const h = T.headAt(v.tracks.get("j1")!, t, 11_000, T.newHead());
    if (Number.isNaN(settled) && Math.hypot(m.x - h.x, m.y - h.y) < 1e-9) {
      settled = t;
    }
    prev = m;
  }
  assert.ok(!Number.isNaN(settled), "follows the data again");
});

test("an overrun ahead of a stopped train slides back along its line at CORRECTION_RATE", () => {
  const v = new MoverView();
  for (let i = 0; i < 6; i++) {
    v.add(meta, T.makeSample(meta, i * 500, i * 20, 0, 0.25)); // 40 tiles/s east
  }
  let prev = v.frame(2500, 4100)[0];
  for (let t = 2600; t <= 4000; t += 100) {
    prev = v.frame(t, 4100)[0];
  }
  // It stopped at x = 110: a 50-tile overrun (over MAX_CORRECTION, so no ordinary blend).
  v.add(meta, T.makeSample(meta, 3000, 110, 0, 0.25, T.F_STOP));
  v.add(meta, T.makeSample(meta, 6000, 110, 0, 0.25, T.F_STOP));
  for (let t = 4100; t <= 9000; t += 100) {
    const m = v.frame(t, 9000)[0];
    assert.ok(Math.abs(m.y) < 1e-9, "on its line");
    assert.ok(prev.x - m.x <= (CORRECTION_RATE * 100) / 1000 + 1e-6 && m.x <= prev.x + 1e-9, `slides back at ${t}: ${prev.x} -> ${m.x}`);
    prev = m;
  }
  assert.ok(Math.abs(prev.x - 110) < 1e-9, `settles: ${prev.x}`);
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
  const prior = new DelayEstimator(2000);
  prior.observe(1000);
  assert.ok(Math.abs(prior.target() - 1500) < 1e-9, "older data: p99 lag + 0.5 s");
  prior.railPrior();
  assert.equal(prior.current, delayConfig.readyPriorMs, "rail path rows: the prior at once");
  assert.equal(prior.target(), delayConfig.readyPriorMs, "no readiness observed yet: the prior");
  prior.railPrior(12 / 1000); // a game at speed 0.2: 60 ticks are 5 s
  assert.equal(prior.target(), delayConfig.readyCapMs, "a slow game's prior: up to the cap");
  const d = new DelayEstimator(2000);
  assert.equal(d.current, 2000);
  d.observeReady(100);
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

test("readiness: the wait after a rail path stop sample is not a moving train's", () => {
  for (const rail of [true, false]) {
    const v = new MoverView();
    const f = rail ? T.F_RAIL : 0;
    const add = (t: number, x: number, flags: number) =>
      v.add(meta, T.makeSample(meta, t, x, 0, 0.25, flags, null, t / 50, rail ? 0.1 : NaN));
    add(0, 0, f);
    add(1000, 5, f | T.F_STOP);
    assert.equal(v.prevT, 0, "a moving sample before the stop");
    add(31_000, 5.1, f);
    // a rail path holds the stopped train until its start; older data keeps counting it
    assert.equal(Number.isNaN(v.prevT), rail, `rail ${rail}: prevT ${v.prevT}`);
  }
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

test("a parked train is placed from its stop sample alone (a keyframe, live.bin) when it lists the pieces under it", async () => {
  // the real-game trains of the shared rail vectors (docs/rail-geometry-vectors.json)
  const P = await import("./railpath.ts");
  const vectors = JSON.parse(readFileSync(new URL("../../../docs/rail-geometry-vectors.json", import.meta.url), "utf8"));
  const kindOf = (type: string): number =>
    ({ "curved-rail-a": P.KIND_CURVE_A, "curved-rail-b": P.KIND_CURVE_B, "legacy-curved-rail": P.KIND_LEGACY_CURVE, "rail-ramp": P.KIND_RAMP })[
      type.replace(/^elevated-/, "")
    ] ?? 0;
  let curved = 0;
  let worstOld = 0;
  for (const t of vectors.trains) {
    const pieces = t.path.filter((r: { type: string }) => kindOf(r.type)).map((r: { type: string; x: number; y: number; dir: number }) =>
      ({ x: r.x, y: r.y, dir: r.dir, kind: kindOf(r.type) }));
    if (pieces.length === 0) {
      continue;
    }
    curved++;
    // the leading carriage's own orientation turned to the direction of travel: along its axis from its centre towards
    // the leading joint (the joint is the rail point a train length ahead of the rear)
    const [cx, cy, co] = t.game_carriages[0];
    const [nx, ny] = t.game_carriages[1];
    const a = 2 * Math.PI * co;
    const ahead = Math.sin(a) * (cx - nx) - Math.cos(a) * (cy - ny) > 0;
    const o = (co + (ahead ? 0 : 0.5)) % 1;
    const m = { id: t.source, k: "t" as const, f: "player", name: "locomotive", n: t.game_carriages.length, o: "L" + "C".repeat(t.game_carriages.length - 1) };
    const draw = (rp: typeof pieces) => {
      const v = new MoverView();
      v.add(m, T.makeSample(m, 1000, cx, cy, o, T.F_RAIL | T.F_STOP, null, 600, 0, rp));
      const shown = v.frame(61_000, Infinity, { bodies: true })[0];
      assert.equal(shown.cars.length, t.game_carriages.length);
      let worst = 0;
      for (const [i, [x, y]] of t.game_carriages.entries()) {
        worst = Math.max(worst, Math.hypot(shown.cars[i].x - x, shown.cars[i].y - y));
      }
      return worst;
    };
    const e = draw(pieces);
    // within 0.05 of the game, except where the leading carriage stands on a ramp's slope: its joint is taken 2 tiles
    // ahead of its centre in plan, about 0.2 short there (RAIL-GEOMETRY.md 8.2)
    assert.ok(e < (t.category === "rail-ramp" ? 0.25 : 0.05), `${t.source}: a carriage ${e.toFixed(3)} tiles off`);
    // only the last piece (as a stop sample listing just the pieces entered since the sample before may): off the rails
    worstOld = Math.max(worstOld, draw(pieces.slice(-1)));
  }
  assert.ok(curved >= 20, `${curved} trains on curved pieces or ramps`);
  assert.ok(worstOld > 2, `without the pieces under it a parked train is drawn up to ${worstOld.toFixed(1)} tiles off`);
});

test("a moving train's first row in view (a keyframe's, live.bin's) places its body when the server put the pieces under it in front", async () => {
  // the server's body (core.RailBody, FORMAT.md 7.4): the same real-game trains as above, now moving and seen first at
  // a row that is neither a run start nor a stop
  const P = await import("./railpath.ts");
  const vectors = JSON.parse(readFileSync(new URL("../../../docs/rail-geometry-vectors.json", import.meta.url), "utf8"));
  const kindOf = (type: string): number =>
    ({ "curved-rail-a": P.KIND_CURVE_A, "curved-rail-b": P.KIND_CURVE_B, "legacy-curved-rail": P.KIND_LEGACY_CURVE, "rail-ramp": P.KIND_RAMP })[
      type.replace(/^elevated-/, "")
    ] ?? 0;
  let curved = 0;
  let worstOld = 0;
  for (const t of vectors.trains) {
    const pieces = t.path.filter((r: { type: string }) => kindOf(r.type)).map((r: { type: string; x: number; y: number; dir: number }) =>
      ({ x: r.x, y: r.y, dir: r.dir, kind: kindOf(r.type) }));
    if (pieces.length < 2) {
      continue;
    }
    curved++;
    const [cx, cy, co] = t.game_carriages[0];
    const [nx, ny] = t.game_carriages[1];
    const a = 2 * Math.PI * co;
    const ahead = Math.sin(a) * (cx - nx) - Math.cos(a) * (cy - ny) > 0;
    const o = (co + (ahead ? 0 : 0.5)) % 1;
    const m = { id: t.source, k: "t" as const, f: "player", name: "locomotive", n: t.game_carriages.length, o: "L" + "C".repeat(t.game_carriages.length - 1) };
    const draw = (rp: typeof pieces) => {
      const v = new MoverView();
      v.add(m, T.makeSample(m, 1000, cx, cy, o, T.F_RAIL, null, 600, 0.5, rp));
      const shown = v.frame(1000, Infinity, { bodies: true })[0];
      assert.equal(shown.cars.length, t.game_carriages.length);
      let worst = 0;
      for (const [i, [x, y]] of t.game_carriages.entries()) {
        worst = Math.max(worst, Math.hypot(shown.cars[i].x - x, shown.cars[i].y - y));
      }
      return worst;
    };
    const e = draw(pieces);
    assert.ok(e < (t.category === "rail-ramp" ? 0.25 : 0.05), `${t.source}: a carriage ${e.toFixed(3)} tiles off`);
    // only the piece the row itself lists (entered since the row before): the body behind it is not known
    worstOld = Math.max(worstOld, draw(pieces.slice(-1)));
  }
  assert.ok(curved >= 15, `${curved} trains on two or more curved pieces or ramps`);
  assert.ok(worstOld > 2, `without its body a moving train is drawn up to ${worstOld.toFixed(1)} tiles off`);
});

test("a changed train (a split at rest: a new rail path, fewer carriages) holds at its stop sample, then jumps", () => {
  const v = new MoverView();
  const m3 = { ...meta, id: "split", n: 3, o: "LCC" };
  const m2 = { ...meta, id: "split", n: 2, o: "CC", name: "cargo-wagon" };
  v.add(m3, T.makeSample(m3, 0, 0, 0, 0.25, T.F_RAIL, null, 0, 0.3));
  v.add(m3, T.makeSample(m3, 1000, 18, 0, 0.25, T.F_RAIL | T.F_STOP, null, 60, 0));
  // 40 s later its locomotive is taken off: the rest's leading carriage stands 7 tiles back
  const car = [{ x: 11, y: 0, o: 0.25 }, { x: 4, y: 0, o: 0.25 }];
  v.add(m2, T.makeSample(m2, 41_000, 11, 0, 0.25, T.F_RAIL | T.F_STOP, car, 2460, 0, null, true));
  for (const t of [2000, 20_000, 40_900]) {
    const shown = v.frame(t, Infinity)[0];
    assert.ok(shown && Math.abs(shown.x - 18) < 1e-6, `at ${t}: held at the stop (${shown?.x})`);
  }
  const after = v.frame(41_000, Infinity)[0];
  assert.ok(Math.abs(after.x - 11) < 1e-6, `then where the rest stands (${after.x})`);
});
