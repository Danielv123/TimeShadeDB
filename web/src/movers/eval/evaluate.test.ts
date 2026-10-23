// The train evaluator on a synthetic journey: a train circling a rail loop, sampled the way
// illisis' dead-reckoning capture samples it (sparse on the straightest parts).
import "./ts-resolve.mjs";
import assert from "node:assert/strict";
import { test } from "node:test";

const E = await import("./evaluate.ts");

const R = 60;
const V = 25; // tiles per second

/** A rail mask with one area (0,0) whose pixels within 0.75 tile of the circle are rails. */
function circleMask(): Uint8Array {
  const out = new Uint8Array(8 + 8 + 32768);
  out.set([82, 77, 83, 75]); // "RMSK"
  const v = new DataView(out.buffer);
  v.setUint32(4, 1, true);
  v.setInt32(8, 0, true);
  v.setInt32(12, 0, true);
  for (let py = 0; py < 512; py++) {
    for (let px = 0; px < 512; px++) {
      const x = px - 256 + 0.5;
      const y = py - 256 + 0.5;
      if (Math.abs(Math.hypot(x, y) - R) < 0.75) {
        const i = py * 512 + px;
        out[16 + (i >> 3)] |= 1 << (i & 7);
      }
    }
  }
  return out;
}

/** A journey around the loop: a sample every `every` ms, with the captured velocity and the lagging orientation. */
function circleJourney(every: number, seconds: number): any {
  const rows = [];
  for (let t = 0; t <= seconds * 1000; t += every) {
    const s = (V * t) / 1000;
    const phi = s / R;
    const lag = (s - 3.5) / R;
    const tx = -Math.sin(lag);
    const ty = Math.cos(lag);
    let h = Math.atan2(tx, -ty) / (2 * Math.PI);
    h = h < 0 ? h + 1 : h;
    rows.push({ t: 1_000_000 + t, x: R * Math.cos(phi), y: R * Math.sin(phi), vx: -V * Math.sin(phi), vy: V * Math.cos(phi), h, gone: false });
  }
  return { id: "j", surface: "nauvis", n: 4, kinds: "LCCC", rows };
}

test("rail mask distance", () => {
  const mask = new E.RailMask(circleMask());
  assert.equal(mask.distance(R, 0), 0);
  const d = mask.distance(R - 3, 0);
  assert.ok(d > 1.5 && d < 3, `distance ${d}`);
  assert.equal(mask.distance(1000, 1000), 4, "capped outside the mask");
});

test("leave-one-out: v3 rebuilds hidden samples on a curve far better than the illisis viewer", () => {
  const j = circleJourney(700, 60);
  const r = E.leaveOneOut([j], { v3: () => {} }, () => {});
  const v3 = E.quantile(r.all.v3, 0.9);
  const il = E.quantile(r.all.illisis, 0.9);
  assert.ok(v3 < 0.2, `v3 p90 ${v3}`);
  assert.ok(il > 2 * v3, `illisis p90 ${il}`);
});

test("render: v3 carriages stay on the rail; the illisis baseline cuts the curve", () => {
  const j = circleJourney(1500, 40);
  const r = E.renderEval([j], new E.RailMask(circleMask()), { mode: "history" });
  assert.ok(r.v3.frames > 300);
  assert.ok(r.v3.cars.above(1) < 0.01, `v3 > 1 tile: ${r.v3.cars.above(1)}`);
  assert.ok(r.illisis.cars.above(1) > r.v3.cars.above(1), `illisis ${r.illisis.cars.above(1)}`);
  assert.equal(r.v3.headJumps, 0);
  const live = E.renderEval([j], new E.RailMask(circleMask()), { mode: "live" });
  assert.equal(live.v3.headJumps, 0);
  assert.ok(live.v3.cars.above(1) < 0.02, `live v3 > 1 tile: ${live.v3.cars.above(1)}`);
});

test("clamp fix drops boundary-stamped rows only", () => {
  const j = { id: "j", surface: "s", n: 1, kinds: "L", rows: [600_000, 600_001, 600_003, 900_002].map(t => ({ t, x: 0, y: 0, vx: 0, vy: 0, h: 0, gone: false })) };
  assert.deepEqual(E.dropClamped(j).rows.map(r => r.t), [600_003]);
});
