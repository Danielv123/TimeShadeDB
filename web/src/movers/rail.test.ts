// Trains of the rail path model (F_RAIL, docs/v3/RAIL-GEOMETRY.md 8) in the viewer: the head
// between sparse samples follows the rail path by the timing model, carriages sit on it at their
// joint distances, extrapolation stays on it, a missed piece and an end switch start a new run,
// and a first sight after a resync links back. Run with `npm test`.
import "./eval/ts-resolve.mjs";
import assert from "node:assert/strict";
import { test } from "node:test";

const T = await import("./track.ts");
const P = await import("./railpath.ts");
const V = await import("./view.ts");

type Meta = { id: string; k: "t"; f: string; name: string; n: number; o: string };
const meta: Meta = { id: "j1", k: "t", f: "player", name: "locomotive", n: 3, o: "LCC" };

// A real stretch of the benchmark map (rail-geometry-vectors.json, bizz train 28): westwards along
// y = -125, then curved-rail-a (dir 12) and curved-rail-b (dir 12) turning north-west.
const A = { x: -192, y: -125, dir: 12, kind: P.KIND_CURVE_A };
const B = { x: -197, y: -123, dir: 12, kind: P.KIND_CURVE_B };
/** The true path: a chain anchored far back on the straight with both pieces. */
const truthChain = new P.RailChain(-150, -125, -1, 0, [A, B]);
const out = [0, 0, 0, 0];

/** Uniform acceleration from 0.2 tiles a tick at 0.004 a tick per tick: the leading joint's rail distance at tick t. */
const U0 = truthChain.project(-160, -125);
const uAt = (t: number) => U0 + 0.2 * t + 0.002 * t * t;
const vAt = (t: number) => 0.2 + 0.004 * t;
const msOf = (t: number) => 1_000_000 + (t * 1000) / 60;

/** A rail sample at tick t (the leading carriage's centre and own orientation from the truth). */
function sample(t: number, rp: { x: number; y: number; dir: number; kind: number }[] | null, ru = false, flags = 0) {
  truthChain.carriage(uAt(t), out);
  const o = ((Math.atan2(out[2], -out[3]) / (2 * Math.PI)) % 1 + 1) % 1;
  return T.makeSample(meta, msOf(t), out[0], out[1], o, T.F_RAIL | flags, null, 1000 + t, vAt(t), rp, ru);
}

test("rail samples: the head follows the rail path between sparse samples, the carriages too", () => {
  const tr = new T.Track("j1", meta);
  // three samples 60 ticks apart, the pieces sent with the sample after the leading joint entered them
  tr.insert(sample(0, null, true));
  tr.insert(sample(60, [A]));
  tr.insert(sample(120, [B]));
  const rr = T.railRuns(tr);
  assert.deepEqual(Array.from(rr.run), [0, 0, 0], "one run");
  assert.ok(T.railLinked(tr, 0) && T.railLinked(tr, 1));
  const h = T.newHead();
  let worst = 0;
  for (let t = 0; t <= 120; t++) {
    T.headAt(tr, msOf(t), Infinity, h);
    assert.ok(h.rail, `rail head at ${t}`);
    truthChain.carriage(uAt(t), out);
    worst = Math.max(worst, Math.hypot(h.x - out[0], h.y - out[1]));
  }
  // uniform acceleration is exact in the timing model; the rest is the sample rounding of the test
  assert.ok(worst < 1e-3, `head off the truth by ${worst}`);
  // carriages: every carriage's front joint 7 behind the one before, on the same path
  T.headAt(tr, msOf(100), Infinity, h);
  const cars = T.placeRailCarriages(h, "LCC", [7, 7], []);
  for (const [i, c] of cars.entries()) {
    truthChain.carriage(uAt(100) - 7 * i, out);
    assert.ok(Math.hypot(c.x - out[0], c.y - out[1]) < 1e-3, `carriage ${i}`);
  }
  // the view draws the same body
  const view = new V.MoverView();
  for (const s of tr.s) {
    view.add(s.meta, { ...s, seg: null, sv: -1, vv: -1 });
  }
  const shown = view.frame(msOf(100), Infinity, { bodies: true })[0];
  assert.equal(shown.cars.length, 3);
  for (const [i, c] of shown.cars.entries()) {
    assert.ok(Math.hypot(c.x - cars[i].x, c.y - cars[i].y) < 1e-6, `view carriage ${i}`);
  }
});

test("rail samples: extrapolation runs on along the rail path; a stop sample holds", () => {
  const tr = new T.Track("j1", meta);
  tr.insert(sample(0, null, true));
  tr.insert(sample(60, [A, B]));
  const h = T.newHead();
  T.headAt(tr, msOf(70), msOf(70), h); // past the newest sample, inside the data horizon (live)
  assert.equal(h.mode, "extrap");
  assert.ok(h.rail);
  truthChain.carriage(uAt(60) + vAt(60) * 10, out);
  assert.ok(Math.hypot(h.x - out[0], h.y - out[1]) < 1e-3, "on along the path at the speed");
  const stopped = new T.Track("j1", meta);
  stopped.insert(sample(0, null, true));
  stopped.insert(sample(60, [A], false, T.F_STOP));
  T.headAt(stopped, msOf(90), msOf(90), h);
  assert.equal(h.mode, "hold");
});

test("rail samples: a missed piece starts a new run; an end switch never links", () => {
  const tr = new T.Track("j1", meta);
  tr.insert(sample(0, null, true));
  // B without A: it does not continue the straight
  tr.insert(sample(120, [B]));
  const rr = T.railRuns(tr);
  assert.notEqual(rr.run[0], rr.run[1], "two runs");
  const es = new T.Track("j1", meta);
  es.insert(sample(0, null, true));
  es.insert(sample(60, [A], true, T.F_END_SWITCH));
  assert.ok(!T.railLinked(es, 0), "no rail segment across an end switch");
});

test("rail samples: a new run whose pieces reach back links the sample before it", () => {
  const tr = new T.Track("j1", meta);
  // a resync sample on the straight, then a first sight in the curve listing the pieces under the train
  tr.insert(sample(0, null, true));
  tr.insert(sample(120, [A, B], true));
  const rr = T.railRuns(tr);
  assert.notEqual(rr.run[0], rr.run[1]);
  assert.ok(T.railLinked(tr, 0), "linked on the new run's chain");
  const h = T.newHead();
  T.headAt(tr, msOf(60), Infinity, h);
  truthChain.carriage(uAt(60), out);
  assert.ok(Math.hypot(h.x - out[0], h.y - out[1]) < 1e-3);
});

test("rail samples: a stop links to the start however long it stood; it stands, then accelerates", () => {
  const tr = new T.Track("j1", meta);
  truthChain.carriage(U0, out);
  const o = ((Math.atan2(out[2], -out[3]) / (2 * Math.PI)) % 1 + 1) % 1;
  tr.insert(T.makeSample(meta, msOf(0), out[0], out[1], o, T.F_RAIL | T.F_STOP, null, 1000, 0, null, true));
  // started at tick 10,000 (three minutes later) at 0.01 a tick per tick; read 20 ticks later
  truthChain.carriage(U0 + 0.005 * 400, out);
  tr.insert(T.makeSample(meta, msOf(10_020), out[0], out[1], o, T.F_RAIL, null, 1000 + 10_020, 0.2, null, false));
  assert.ok(T.railLinked(tr, 0));
  const h = T.newHead();
  T.headAt(tr, msOf(9000), Infinity, h);
  truthChain.carriage(U0, out);
  assert.ok(Math.hypot(h.x - out[0], h.y - out[1]) < 1e-6, "still standing");
  T.headAt(tr, msOf(10_010), Infinity, h);
  truthChain.carriage(U0 + 0.005 * 100, out);
  assert.ok(Math.hypot(h.x - out[0], h.y - out[1]) < 1e-3, "accelerating");
});
