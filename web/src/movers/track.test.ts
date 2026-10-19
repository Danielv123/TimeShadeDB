// Motion tests (PLAN.md §4.8): Hermite and rail segments, head placement, extrapolation
// bounds, gones and handoffs, end switches, bodies and carriages. Run with `npm test`.
import "./eval/ts-resolve.mjs";
import assert from "node:assert/strict";
import { test } from "node:test";

const H = await import("./hermite.ts");
const G = await import("./segment.ts");
const T = await import("./track.ts");

type Meta = { id: string; k: "t" | "v" | "p"; f: string; name: string; n?: number; o?: string };
const meta: Meta = { id: "j1", k: "t", f: "player", name: "locomotive", n: 5, o: "LCCCC" };

/** A train running at speed v around a circle of radius R (clockwise on screen), sampled every `every` tiles. */
function circleTrack(R = 30, v = 20, every = 3, count = 40, t0 = 1000) {
  const tr = new T.Track("j1", meta);
  for (let i = 0; i < count; i++) {
    const s = i * every;
    const phi = s / R;
    // the orientation is the tangent half a pitch behind the head
    const lag = (s - 3.5) / R;
    tr.insert(T.makeSample(meta, t0 + (s / v) * 1000, R * Math.cos(phi), R * Math.sin(phi), H.vectorOrientation(-Math.sin(lag), Math.cos(lag))));
  }
  return tr;
}

/** A straight east-bound train at speed v, one sample per `dt` ms. */
function straightTrack(n: number, v = 10, dt = 500, t0 = 0, y = 0) {
  const tr = new T.Track("j1", meta);
  for (let i = 0; i < n; i++) {
    tr.insert(T.makeSample(meta, t0 + i * dt, (v * i * dt) / 1000, y, 0.25));
  }
  return tr;
}

test("orientation helpers", () => {
  assert.deepEqual(H.orientationVector(0).map(v => Math.round(v * 1e9) / 1e9 + 0), [0, -1]);
  assert.deepEqual(H.orientationVector(0.25).map(v => Math.round(v * 1e9) / 1e9 + 0), [1, 0]);
  assert.ok(Math.abs(H.vectorOrientation(0, 1) - 0.5) < 1e-12);
  assert.ok(Math.abs(H.orientationDelta(0.95, 0.05) - 0.1) < 1e-12);
  assert.ok(Math.abs(H.orientationDelta(0.05, 0.95) + 0.1) < 1e-12);
});

test("hermite passes through its end points with the given velocities", () => {
  const out = [0, 0, 0, 0];
  H.hermite(0, 0, 10, 0, 10, 5, 0, 10, 2, 0, out);
  assert.deepEqual(out, [0, 0, 10, 0]);
  H.hermite(0, 0, 10, 0, 10, 5, 0, 10, 2, 1, out);
  assert.deepEqual(out.map(v => Math.round(v * 1e9) / 1e9), [10, 5, 0, 10]);
});

test("polyline arc-length lookup", () => {
  const p = new H.Polyline();
  p.push(0, 0);
  p.push(3, 4);
  p.push(3, 4);
  p.push(3, 10);
  assert.equal(p.size, 3);
  assert.equal(p.length, 11);
  const o = [0, 0];
  p.at(2.5, o);
  assert.deepEqual(o, [1.5, 2]);
  p.at(8, o);
  assert.deepEqual(o, [3, 7]);
  p.at(99, o);
  assert.deepEqual(o, [3, 10]);
});

test("line+arc segment reproduces a straight followed by a quarter circle", () => {
  // Straight east from (0,0) for 20 tiles, then a clockwise (on screen) quarter circle of radius 10.
  const seg = new G.Segment().build(0, 0, 0, 10, 0, 0, 0, 3000, 30, 10, 0, 10, 0, 0);
  assert.equal(seg.kind, G.SEG_LINE_ARC);
  assert.ok(Math.abs(seg.length - (20 + (Math.PI / 2) * 10)) < 1e-9);
  const o = [0, 0, 0, 0];
  seg.pointAt(10, o);
  assert.ok(Math.hypot(o[0] - 10, o[1]) < 1e-9);
  seg.pointAt(20 + (Math.PI / 4) * 10, o);
  const c = [20, 10];
  assert.ok(Math.abs(Math.hypot(o[0] - c[0], o[1] - c[1]) - 10) < 1e-9, "on the circle");
  seg.pointAt(seg.length, o);
  assert.ok(Math.hypot(o[0] - 30, o[1] - 10) < 1e-9 && Math.abs(o[3] - 1) < 1e-9, "ends at b going south");
  // reversed: an arc then a line
  const rev = new G.Segment().build(0, 30, 10, 0, -10, 0, 0, 3000, 0, 0, -10, 0, 0, 0);
  assert.equal(rev.kind, G.SEG_ARC_LINE);
  rev.pointAt(rev.length - 10, o);
  assert.ok(Math.hypot(o[0] - 10, o[1]) < 1e-9);
});

/** Distance from (x, y) to the polyline through pts. */
function distToPath(x: number, y: number, pts: number[][]): number {
  let best = Infinity;
  for (let i = 0; i + 1 < pts.length; i++) {
    const [ax, ay] = pts[i];
    const [bx, by] = pts[i + 1];
    const l2 = (bx - ax) ** 2 + (by - ay) ** 2;
    const u = l2 > 0 ? Math.max(0, Math.min(1, ((x - ax) * (bx - ax) + (y - ay) * (by - ay)) / l2)) : 0;
    best = Math.min(best, Math.hypot(x - (ax + u * (bx - ax)), y - (ay + u * (by - ay))));
  }
  return best;
}

test("a train's long S gap follows line, arc, rail-direction straight, arc instead of the chord", () => {
  // Test data, nauvis (-312, 799) -> (-295, 751), 1.6 s at 36 tiles/s: a 1:2 diagonal straight
  // bends north onto a straight, then into the next curve. Track centre line from the chart.
  const rail = [[-312, 799], [-300.5, 775], [-298, 769], [-297.5, 760], [-296.5, 755], [-295.3, 751.4]];
  const t0 = [Math.sin(2 * Math.PI * 0.0738), -Math.cos(2 * Math.PI * 0.0738)];
  const t1 = [Math.sin(2 * Math.PI * 0.08), -Math.cos(2 * Math.PI * 0.08)];
  const build = (rail: boolean) => new G.Segment().build(0, -312, 799, t0[0] * 36, t0[1] * 36, 0, 0, 1600, -295.3, 751.4, t1[0] * 36, t1[1] * 36, 0, 0, rail, 0, 1 / 14);
  const seg = build(true);
  assert.equal(seg.kind, G.SEG_PATH);
  const cubic = build(false);
  assert.equal(cubic.kind, G.SEG_HERMITE, "other movers keep the cubic");
  const o = [0, 0, 0, 0];
  let worst = 0;
  let worstCubic = 0;
  for (let i = 0; i <= 40; i++) {
    seg.pointAt((seg.length * i) / 40, o);
    worst = Math.max(worst, distToPath(o[0], o[1], rail));
    cubic.pointAt((cubic.length * i) / 40, o);
    worstCubic = Math.max(worstCubic, distToPath(o[0], o[1], rail));
  }
  assert.ok(worst < 1.5, `rail path within ${worst.toFixed(2)} tiles of the track`);
  assert.ok(worstCubic > 2.5, `the cubic cuts the corner by ${worstCubic.toFixed(2)} tiles`);
  seg.pointAt(seg.length, o);
  assert.ok(Math.hypot(o[0] + 295.3, o[1] - 751.4) < 1e-6 && Math.abs(o[2] - t1[0]) < 1e-6, "ends at b along its tangent");
  // A short S stays a cubic (under 30 tiles of chord the samples are close enough).
  const short = new G.Segment().build(0, 0, 0, 10, -5, 0, 0, 1000, 20, 0, 10, -5, 0, 0, true);
  assert.notEqual(short.kind, G.SEG_PATH);
});

test("a long gap from one straight round a curve onto another keeps both straights", () => {
  // East along y = 0, a 13-tile quarter curve round to south, then south: samples 40 tiles
  // before the curve and 30 after it.
  const seg = new G.Segment().build(0, -40, 0, 20, 0, 0, 0, 5000, 13, 43, 0, 20, 0, 0, true);
  assert.equal(seg.kind, G.SEG_PATH);
  assert.ok(Math.abs(seg.length - (40 + 30 + (Math.PI / 2) * 13)) < 1e-6, `length ${seg.length}`);
  const o = [0, 0, 0, 0];
  seg.pointAt(39, o);
  assert.ok(Math.abs(o[1]) < 1e-9, "straight before the curve");
  seg.pointAt(seg.length - 29, o);
  assert.ok(Math.abs(o[0] - 13) < 1e-9, "straight after it");
  // Not a rail direction at one end: line+arc as before.
  const other = new G.Segment().build(0, -40, 0, 20, 0, 0, 0, 5000, 13, 43, 4, 20, 0, 0, true);
  assert.notEqual(other.kind, G.SEG_PATH);
});

test("segment timing is monotone and hits both ends", () => {
  const seg = new G.Segment().build(0, 0, 0, 0, 0, 1, 0, 2000, 10, 0, 20, 0, 0, 0);
  const o = [0, 0];
  let prev = -1;
  for (let t = 0; t <= 2000; t += 50) {
    seg.arcAt(t, o);
    assert.ok(o[0] >= prev - 1e-12, `monotone at ${t}`);
    prev = o[0];
  }
  seg.arcAt(0, o);
  assert.equal(o[0], 0);
  seg.arcAt(2000, o);
  assert.ok(Math.abs(o[0] - seg.length) < 1e-9);
});

test("segments that would loop fall back to a straight line", () => {
  // tangents pointing away from each other along a short chord
  const seg = new G.Segment().build(0, 0, 0, -30, 0, 0, 0, 1000, 2, 0, -30, 0, 0, 0);
  assert.ok(seg.length <= 1.5 * 2 + 1, `length ${seg.length}`);
});

test("the fallback curve neither overshoots its end nor races (braking train, test data)", () => {
  // From the illisis test data (journey 1 on nauvis): 31 tiles/s into a segment that covers
  // 17.9 tiles in 2.7 s and ends at 9.5 tiles/s, with a start tangent 0.56 degrees off the
  // chord. v x dt tangents made the curve run past its end and back, and walking it by curve
  // parameter drew the head at 57 tiles/s.
  const seg = new G.Segment().build(0, 268.6875, -205, -31.274, 0.307, 0, 0, 2709, 250.8125, -205, -9.515, 0, 0, 0);
  assert.equal(seg.kind, G.SEG_HERMITE);
  assert.ok(seg.length < 17.875 + 0.05, `length ${seg.length}`);
  const o = [0, 0, 0, 0];
  let px = 268.6875;
  for (let t = 50; t <= 2709; t += 50) {
    seg.at(t, o);
    assert.ok(o[0] <= px + 1e-9, `never backwards at ${t}`);
    assert.ok(px - o[0] <= 33 * 0.05 + 1e-6, `at most the start speed at ${t}: ${(px - o[0]) / 0.05} tiles/s`);
    assert.ok(Math.abs(o[1] + 205) < 0.1, `stays on the line at ${t}: y=${o[1]}`);
    px = o[0];
  }
  seg.at(2709, o);
  assert.ok(Math.hypot(o[0] - 250.8125, o[1] + 205) < 1e-6, "ends at b");
});

test("a braking train's extrapolation decelerates and stops", () => {
  // Braking from 20 to 10 tiles/s, then no more samples (a lost stop sample).
  const tr = new T.Track("j1", meta);
  tr.insert(T.makeSample(meta, 0, 0, 0, 0.25));
  tr.insert(T.makeSample(meta, 500, 10, 0, 0.25)); // 20 tiles/s
  tr.insert(T.makeSample(meta, 1000, 17.5, 0, 0.25)); // 15
  tr.insert(T.makeSample(meta, 1500, 22.5, 0, 0.25)); // 10: braking at 10 tiles/s^2, 7.5 tiles/s at the sample
  const h = T.newHead();
  // Data known complete up to 10 s past the last sample (silence inside the heartbeat).
  const known = 1500 + 10_000;
  T.headAt(tr, 1600, known, h);
  assert.equal(h.mode, "extrap");
  assert.ok(h.x > 22.5 && h.x < 22.5 + 0.1 * 10, `slower than the last chord: ${h.x}`);
  T.headAt(tr, 10_000, known, h);
  assert.equal(h.mode, "hold");
  // v0 = 7.5, a = -10: stops after 0.75 s and 2.81 tiles (the old rule ran on 110 tiles)
  assert.ok(Math.abs(h.x - (22.5 + (7.5 * 7.5) / 20)) < 1e-6, `stopped at ${h.x}`);
  // a train at constant speed still runs on
  const st = straightTrack(5, 10, 500);
  T.headAt(st, 3000, 2000 + 10_000, h);
  assert.ok(Math.abs(h.x - 30) < 1e-6);
});

test("insert keeps time order and replaces equal times", () => {
  const tr = new T.Track("j1", meta);
  for (const t of [300, 100, 200, 100]) {
    tr.insert(T.makeSample(meta, t, t, 0, 0.25));
  }
  assert.deepEqual(tr.s.map(s => s.t), [100, 200, 300]);
  assert.equal(tr.find(99), -1);
  assert.equal(tr.find(250), 1);
  assert.equal(tr.find(1000), 2);
});

test("the head passes through samples and follows a circle between them", () => {
  const tr = circleTrack();
  const h = T.newHead();
  for (let i = 2; i < 36; i++) {
    const s = tr.s[i];
    T.headAt(tr, s.t, Infinity, h);
    assert.ok(Math.hypot(h.x - s.x, h.y - s.y) < 1e-9, "at the sample");
    const mid = (s.t + tr.s[i + 1].t) / 2;
    T.headAt(tr, mid, Infinity, h);
    assert.equal(h.mode, "interp");
    assert.ok(Math.abs(Math.hypot(h.x, h.y) - 30) < 0.02, `on the circle: r=${Math.hypot(h.x, h.y)}`);
  }
});

test("extrapolation runs on inside the data horizon and stops 1 s past it", () => {
  const tr = straightTrack(5, 10, 500); // last sample at t=2000, x=20
  const h = T.newHead();
  const known = 2000 + T.HEARTBEAT_MS; // silent up to the heartbeat: still on its line
  T.headAt(tr, 4000, known, h);
  assert.equal(h.mode, "extrap");
  assert.ok(Math.abs(h.x - 40) < 1e-6, `runs on: ${h.x}`);
  T.headAt(tr, 20000, known, h);
  assert.ok(Math.abs(h.x - (20 + 10 * T.HEARTBEAT_MS / 1000)) < 1e-6, "at most the heartbeat");
  assert.equal(h.mode, "hold");
  // data known only up to t=2500: at most 1 s past it
  T.headAt(tr, 9000, 2500, h);
  assert.ok(Math.abs(h.x - (20 + 10 * 1.5)) < 1e-6, `capped: ${h.x}`);
  // the heartbeat is known missed (or the data ends: history, every row known): the data
  // stopped, so the mover stays at its last known position
  T.headAt(tr, 4000, 2000 + T.HEARTBEAT_MS + T.EXTRAPOLATE_MS + 1, h);
  assert.equal(h.mode, "hold");
  assert.ok(Math.abs(h.x - 20) < 1e-9, `held at the last sample: ${h.x}`);
  T.headAt(tr, 4000, Infinity, h);
  assert.ok(Math.abs(h.x - 20) < 1e-9);
});

test("extrapolation past the data horizon follows the curve it is in, for at most motionConfig.extrapTurn, and the body follows it", () => {
  // Radius 30, 20 tiles/s, a sample every 3 tiles; the last at arc 33 (phi = 1.1) and t = 2650,
  // which is also the data horizon: everything after it is past the horizon.
  const R = 30;
  const tr = circleTrack(R, 20, 3, 12);
  const lastPhi = 33 / R;
  const horizon = 2650;
  const h = T.newHead();
  // 0.3 s on: 6 more tiles of the circle
  T.headAt(tr, 2950, horizon, h);
  assert.equal(h.mode, "extrap");
  assert.ok(Math.abs(Math.hypot(h.x, h.y) - R) < 0.3, `stays on the circle: r=${Math.hypot(h.x, h.y)}`);
  assert.ok(Math.atan2(h.y, h.x) > lastPhi + 0.15, "moved on along it");
  // the turn stops after motionConfig.extrapTurn: straight on from there (19 tiles on)
  T.headAt(tr, 3600, horizon, h);
  assert.equal(h.mode, "extrap");
  const dir = Math.atan2(h.vy, h.vx);
  const start = Math.atan2(Math.cos(lastPhi), -Math.sin(lastPhi));
  const turned = Math.abs(Math.atan2(Math.sin(dir - start), Math.cos(dir - start)));
  assert.ok(Math.abs(turned - T.motionConfig.extrapTurn) < 0.1, `turned ${turned}`);
  // the body lies on the arc behind the head, not on its chord
  T.headAt(tr, 2950, horizon, h);
  const poly = T.bodyPath(tr, h, 2950, 20, new H.Polyline());
  for (let i = 0; i < poly.size; i++) {
    assert.ok(Math.abs(Math.hypot(poly.xs[i], poly.ys[i]) - R) < 0.35, `body point ${i} r=${Math.hypot(poly.xs[i], poly.ys[i])}`);
  }
  // on a straight it stays straight
  const st = straightTrack(5, 10, 500);
  T.headAt(st, 3000, 2000, h);
  assert.ok(Math.abs(h.y) < 1e-9 && Math.abs(h.x - 30) < 1e-6);
});

test("inside the data horizon the extrapolation runs straight; the curve starts at the horizon", () => {
  const R = 30;
  const tr = circleTrack(R, 20, 3, 12);
  const h = T.newHead();
  T.headAt(tr, 2660, 3000, h);
  const start = Math.atan2(h.vy, h.vx);
  // silence confirmed up to t = 3000 (the capture would have sent a turn): straight to there
  T.headAt(tr, 2950, 3000, h);
  assert.ok(Math.abs(Math.atan2(h.vy, h.vx) - start) < 1e-9, "no turn inside the horizon");
  const lead = T.newHead();
  T.headAt(tr, 3000, 3000, lead);
  // past it, the curve starts from the straight's end, and the body follows both
  T.headAt(tr, 3300, 3000, h);
  const turned = Math.atan2(Math.sin(Math.atan2(h.vy, h.vx) - start), Math.cos(Math.atan2(h.vy, h.vx) - start));
  assert.ok(Math.abs(turned) > 0.1, `turns past the horizon: ${turned}`);
  const poly = T.bodyPath(tr, h, 3300, 20, new H.Polyline());
  let nearLead = Infinity;
  for (let i = 0; i < poly.size; i++) {
    nearLead = Math.min(nearLead, Math.hypot(poly.xs[i] - lead.x, poly.ys[i] - lead.y));
  }
  assert.ok(nearLead < 0.3, `the body passes the straight's end: ${nearLead}`);
  // the evaluator's switch turns inside the horizon too
  T.motionConfig.extrapTurnInside = true;
  try {
    T.headAt(tr, 2950, 3000, h);
    assert.ok(Math.abs(Math.hypot(h.x, h.y) - R) < 0.3, "on the circle");
  } finally {
    T.motionConfig.extrapTurnInside = false;
  }
});

test("never towards a gone; hidden after an end, held while a handoff may follow", () => {
  const tr = straightTrack(5, 10, 500);
  tr.insert(T.makeSample(meta, 2300, 0, 0, 0, T.F_GONE));
  const h = T.newHead();
  T.headAt(tr, 2200, Infinity, h);
  assert.ok(h.visible && Math.abs(h.x - 22) < 1e-6);
  T.headAt(tr, 2400, Infinity, h);
  assert.equal(h.visible, false, "the journey ended");
  // live: the handoff window is still open, keep it on screen
  T.headAt(tr, 2400, 3000, h);
  assert.equal(h.visible, true);
});

test("a handoff keeps the history and bridges the gap without a jump", () => {
  const tr = straightTrack(5, 10, 500); // to x=20 at 2000
  tr.insert(T.makeSample(meta, 2100, 0, 0, 0, T.F_GONE));
  for (let i = 0; i < 5; i++) {
    tr.insert(T.makeSample(meta, 2700 + i * 500, 31 + i * 5, 0, 0.25));
  }
  const h = T.newHead();
  let prev = NaN;
  for (let t = 1500; t <= 4000; t += 50) {
    T.headAt(tr, t, Infinity, h);
    assert.ok(h.visible, `visible at ${t}`);
    if (!Number.isNaN(prev)) {
      assert.ok(Math.abs(h.x - prev) < 2.5, `no jump at ${t}: ${prev} -> ${h.x}`);
    }
    prev = h.x;
  }
  // The body behind the resumed head reaches back over the samples before the gone.
  T.headAt(tr, 2800, Infinity, h);
  const poly = T.bodyPath(tr, h, 2800, 30, new H.Polyline());
  assert.ok(poly.xs.some(x => x <= 20 && x >= 0), "history kept across the handoff");
});

test("carriages sit on a circular track, spaced by arc length", () => {
  const tr = circleTrack();
  const h = T.newHead();
  const t = (tr.s[30].t + tr.s[31].t) / 2;
  T.headAt(tr, t, Infinity, h);
  const poly = T.bodyPath(tr, h, t, 40, new H.Polyline());
  const cars = T.placeCarriages(poly, h, "LCCCC", T.pitches(tr, h.k, []), []);
  assert.equal(cars.length, 5);
  for (const c of cars) {
    // bogies on the circle, centre their midpoint: inside by at most 4^2/(8R) ~ 0.07
    const r = Math.hypot(c.x, c.y);
    assert.ok(r <= 30.001 && r > 29.85, `carriage radius ${r}`);
  }
  for (let i = 1; i < cars.length; i++) {
    const d = Math.hypot(cars[i].x - cars[i - 1].x, cars[i].y - cars[i - 1].y);
    assert.ok(Math.abs(d - 7) < 0.15, `spacing ${d}`);
  }
  assert.equal(cars[0].loco, true);
  assert.equal(cars[1].loco, false);
});

test("captured carriage geometry places a stopped train's body", () => {
  const tr = new T.Track("j1", meta);
  // a stopped train on a bend: geometry from the capture, no motion history
  const pts = [[0, 0], [-7, 0.5], [-13.8, 2], [-20.4, 4], [-26.8, 6.6]];
  // each carriage faces along the chord between its neighbours
  const car = pts.map(([x, y], i) => {
    const [ax, ay] = pts[Math.max(0, i - 1)];
    const [bx, by] = pts[Math.min(pts.length - 1, i + 1)];
    return { x, y, o: H.vectorOrientation(ax - bx, ay - by) };
  });
  tr.insert(T.makeSample(meta, 1000, 0, 0, 0.25, T.F_STOP, car));
  const h = T.newHead();
  T.headAt(tr, 5000, Infinity, h);
  const gaps = T.pitches(tr, h.k, []);
  assert.ok(Math.abs(gaps[1] - Math.hypot(6.8, 1.5)) < 1e-9, "pitch measured from the geometry");
  const poly = T.bodyPath(tr, h, 5000, gaps.reduce((a, b) => a + b, 0) + 4, new H.Polyline());
  const cars = T.placeCarriages(poly, h, "LCCCC", gaps, []);
  for (let i = 0; i < car.length; i++) {
    assert.ok(Math.hypot(cars[i].x - car[i].x, cars[i].y - car[i].y) < 0.35, `carriage ${i} at the captured place`);
  }
});

test("without history or geometry the body runs straight against the orientation", () => {
  const tr = new T.Track("j1", meta);
  tr.insert(T.makeSample(meta, 1000, 10, 10, 0, T.F_STOP)); // facing north
  const h = T.newHead();
  T.headAt(tr, 2000, Infinity, h);
  const poly = T.bodyPath(tr, h, 2000, 32, new H.Polyline());
  const cars = T.placeCarriages(poly, h, "LCCCC", T.pitches(tr, h.k, []), []);
  assert.ok(Math.abs(cars[4].x - 10) < 1e-9 && Math.abs(cars[4].y - 38) < 1e-6, `tail south of the head: ${cars[4].x},${cars[4].y}`);
});

test("a body whose known path ends in a curve continues along it", () => {
  // A journey that starts on a circle of radius 30: 4 samples, 9 tiles of path; the rest of
  // the 30-tile body lies on the same circle instead of on its tangent.
  const tr = circleTrack(30, 20, 3, 4);
  const h = T.newHead();
  T.headAt(tr, tr.s[3].t, Infinity, h);
  const poly = T.bodyPath(tr, h, tr.s[3].t, 30, new H.Polyline());
  assert.ok(poly.length >= 30);
  const n = poly.size;
  const r = Math.hypot(poly.xs[n - 1], poly.ys[n - 1]);
  assert.ok(Math.abs(r - 30) < 1.5, `tail ${r.toFixed(2)} from the centre, on the circle`);
});

test("an end switch re-anchors the head without moving the carriages", () => {
  const n3: Meta = { ...meta, n: 3, o: "LCl" };
  const tr = new T.Track("j1", n3);
  for (let i = 0; i < 6; i++) {
    tr.insert(T.makeSample(n3, i * 500, i * 5, 0, 0.25));
  }
  tr.insert(T.makeSample(n3, 3000, 25, 0, 0.25, T.F_STOP));
  // reverses: the new leading carriage is the old tail, 14 tiles back
  tr.insert(T.makeSample({ ...n3, o: "LcL" }, 6000, 11, 0, 0.75, T.F_END_SWITCH | T.F_STOP));
  tr.insert(T.makeSample({ ...n3, o: "LcL" }, 6500, 9, 0, 0.75));
  const body = (t: number) => {
    const h = T.newHead();
    T.headAt(tr, t, Infinity, h);
    const poly = T.bodyPath(tr, h, t, 20, new H.Polyline());
    return T.placeCarriages(poly, h, "LCL", T.pitches(tr, h.k, []), []);
  };
  const before = body(5999).map(c => [c.x, c.y]).sort((a, b) => a[0] - b[0]);
  const after = body(6000).map(c => [c.x, c.y]).sort((a, b) => a[0] - b[0]);
  for (let i = 0; i < 3; i++) {
    assert.ok(Math.hypot(before[i][0] - after[i][0], before[i][1] - after[i][1]) < 0.3, `carriage ${i} stays: ${before[i]} vs ${after[i]}`);
  }
});

test("a reversal that the capture did not flag is detected from the geometry", () => {
  const tr = new T.Track("j1", meta);
  tr.insert(T.makeSample(meta, 0, 0, 0, 0.1, T.F_STOP));
  // 25 tiles behind the old head (n=5: about 28), orientation changed by 0.35 turn on a curve
  tr.insert(T.makeSample(meta, 50_000, -2.5, 25, 0.45));
  assert.equal(T.isEndSwitch(tr, 0), true);
  assert.equal(T.linked(tr, 0), false);
});

test("a long stationary gap keeps the body history", () => {
  const tr = straightTrack(8, 10, 500); // to x=35 at 3500
  tr.insert(T.makeSample(meta, 3600, 35, 0, 0.25, T.F_STOP));
  tr.insert(T.makeSample(meta, 200_000, 35.5, 0, 0.25, T.F_STOP));
  const h = T.newHead();
  T.headAt(tr, 200_500, Infinity, h);
  const poly = T.bodyPath(tr, h, 200_500, 30, new H.Polyline());
  const o = [0, 0];
  poly.at(28, o);
  assert.ok(Math.abs(o[1]) < 1e-9 && o[0] < 10, `body along the old track: ${o}`);
});

test("trim keeps the body's reach and drops parked heartbeats", () => {
  const tr = straightTrack(20, 10, 500); // x 0..95, t 0..9500
  for (let i = 1; i <= 50; i++) {
    tr.insert(T.makeSample(meta, 9500 + i * 10_000, 95, 0, 0.25, T.F_STOP));
  }
  const before = tr.s.length;
  tr.trim(400_000, 40);
  assert.ok(tr.s.length < before - 40, `trimmed ${before} -> ${tr.s.length}`);
  const first = tr.s[0];
  assert.ok(first.x <= 95 - 40, `history covers 40 tiles: first x=${first.x}`);
});

test("trim keeps a parked train's body across the handoff it arrived by (test data, journey 0)", () => {
  // A train comes round a curve, crosses an instance edge (gone + resume 84 ms later) and
  // parks just past it, sending heartbeats. Trimming used to stop at the gone and drop the
  // curve, so the parked body turned straight off the rails.
  const tr = circleTrack(30, 20, 3, 20); // quarter-ish circle, ends near (cos 1.9, sin 1.9) * 30
  const last = tr.s[tr.s.length - 1];
  tr.insert(T.makeSample(meta, last.t + 300, 0, 0, 0, T.F_GONE | T.F_HANDOFF));
  const park = { x: last.x - 2, y: last.y + 1 };
  for (let i = 0; i < 40; i++) {
    tr.insert(T.makeSample(meta, last.t + 384 + i * 10_000, park.x, park.y, last.o, T.F_STOP));
  }
  const t = last.t + 384 + 39 * 10_000 + 5000;
  const cars = (): { x: number; y: number }[] => {
    const h = T.headAt(tr, t, Infinity, T.newHead());
    const gaps = T.pitches(tr, h.k, []);
    const poly = T.bodyPath(tr, h, t, gaps.reduce((a, b) => a + b, 0) + 4, new H.Polyline());
    return T.placeCarriages(poly, h, "LCCCC", gaps, []);
  };
  const before = cars();
  tr.trim(t - 120_000, 5 * 7 + 40);
  assert.ok(tr.s.length < 40, `trimmed to ${tr.s.length}`);
  const after = cars();
  for (let i = 0; i < before.length; i++) {
    assert.ok(Math.hypot(after[i].x - before[i].x, after[i].y - before[i].y) < 1e-9, `carriage ${i} unchanged`);
  }
});

test("held movers are not moving, so a paused view stops asking for frames", () => {
  // Parked across a data gap longer than a segment (not linked): held, static.
  const parked = new T.Track("j1", meta);
  parked.insert(T.makeSample(meta, 0, 10, 0, 0.25, T.F_STOP));
  parked.insert(T.makeSample(meta, 600_000, 10, 0, 0.25, T.F_STOP));
  const h = T.newHead();
  T.headAt(parked, 100_000, Infinity, h);
  assert.equal(h.visible, true);
  assert.equal(h.moving, false, "a parked train across a gap is static");
  // Running towards a gone without a resume: runs on until the cap, then holds.
  const gone = straightTrack(4, 10, 500); // x 0..15, t 0..1500
  gone.insert(T.makeSample(meta, 30_000, 15, 0, 0.25, T.F_GONE));
  T.headAt(gone, 1600, Infinity, h);
  assert.equal(h.moving, true, "still running on");
  const x = h.x;
  T.headAt(gone, 20_000, Infinity, h);
  assert.equal(h.moving, false, "past the extrapolation cap the hold is static");
  T.headAt(gone, 20_016, Infinity, h);
  assert.ok(h.x >= x && h.moving === false);
  // Interpolating between two different samples is moving.
  T.headAt(straightTrack(4, 10, 500), 700, Infinity, h);
  assert.equal(h.moving, true);
});

test("a known data gap: hidden when the mover is elsewhere afterwards, held when parked", () => {
  const head = T.newHead();
  // Moved: last seen at x=40 at t=2000, next seen 500 tiles away 120 s later.
  const moved = straightTrack(5, 10, 500);
  moved.insert(T.makeSample(meta, 122_000, 540, 0, 0.25));
  T.headAt(moved, 2000 + 5000, Infinity, head);
  assert.ok(head.visible, "held within a heartbeat of the last sample");
  T.headAt(moved, 2000 + T.HEARTBEAT_MS + 1000, Infinity, head);
  assert.ok(!head.visible, "hidden once the gap is past the heartbeat");
  T.headAt(moved, 122_000, Infinity, head);
  assert.ok(head.visible && Math.abs(head.x - 540) < 1e-6, "shown again at the next sample");
  // Parked: the same place after the gap.
  const parked = straightTrack(5, 10, 500);
  parked.insert(T.makeSample(meta, 122_000, 20.5, 0, 0.25));
  T.headAt(parked, 60_000, Infinity, head);
  assert.ok(head.visible, "a parked train stays drawn across the gap");
});
