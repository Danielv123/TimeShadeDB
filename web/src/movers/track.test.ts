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
  T.headAt(tr, 4000, Infinity, h);
  assert.equal(h.mode, "extrap");
  assert.ok(Math.abs(h.x - 40) < 1e-6, `runs on: ${h.x}`);
  T.headAt(tr, 20000, Infinity, h);
  assert.ok(Math.abs(h.x - (20 + 10 * T.HEARTBEAT_MS / 1000)) < 1e-6, "at most the heartbeat");
  assert.equal(h.mode, "hold");
  // data known only up to t=2500: at most 1 s past it
  T.headAt(tr, 9000, 2500, h);
  assert.ok(Math.abs(h.x - (20 + 10 * 1.5)) < 1e-6, `capped: ${h.x}`);
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
