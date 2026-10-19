// Unit tests of the evaluator on synthetic tracks with known answers.
import assert from "node:assert/strict";
import { test } from "node:test";

import { RailMask, illisisName } from "../src/illisis_data.ts";
import { Hist, defaultOptions, leaveOneOut, newRenderStats, renderJourney, summarize } from "../src/metrics.ts";
import { referenceRenderer } from "../src/renderers/reference.ts";
import type { Journey, TrainSample } from "../src/types.ts";

function sample(t: number, x: number, y: number, vx: number, vy: number, gone = false): TrainSample {
  return { t, tick: null, x, y, vx, vy, h: null, gone, n: 1, kinds: "L", colour: "" };
}

/** A train on a circle of radius R tiles at w rad/s, sampled every dtMs. */
function circle(R: number, w: number, dtMs: number, n: number): Journey {
  const samples: TrainSample[] = [];
  for (let i = 0; i < n; i++) {
    const a = (w * i * dtMs) / 1000;
    samples.push(sample(i * dtMs, R * Math.cos(a), R * Math.sin(a), -R * w * Math.sin(a), R * w * Math.cos(a)));
  }
  return { id: "c", force: "player", name: "locomotive", samples, handoffs: [] };
}

test("rail distance to unit squares", () => {
  const m = new RailMask();
  m.setTile(10, 10);
  assert.equal(m.distance(10.5, 10.5), 0);
  assert.equal(m.distance(12.5, 10.5), 1.5); // 1.5 tiles right of the square's edge at x=11
  assert.ok(Math.abs(m.distance(12, 12) - Math.SQRT2) < 1e-9);
  assert.equal(m.distance(100, 100, 8), 8); // capped
  m.setTile(-300, 700); // another area, negative coordinates
  assert.equal(m.distance(-299.5, 700.5), 0);
});

test("histogram quantiles", () => {
  const h = new Hist(0.01, 32);
  for (let i = 0; i < 90; i++) h.add(0);
  for (let i = 0; i < 10; i++) h.add(5);
  assert.equal(h.quantile(0.5), 0);
  assert.equal(h.quantile(0.9), 0);
  assert.ok(Math.abs(h.quantile(0.95) - 5.01) < 1e-9);
});

test("leave-one-out: Hermite beats linear on a curve", () => {
  const j = circle(30, 1.5, 300, 200); // 45 tiles/s on a radius-30 curve, like a curve entry
  const lin = { all: [] as number[], curves: [] as number[] };
  const her = { all: [] as number[], curves: [] as number[] };
  leaveOneOut(referenceRenderer("linear"), j, defaultOptions, lin);
  leaveOneOut(referenceRenderer("hermite"), j, defaultOptions, her);
  const sl = summarize(lin, newRenderStats());
  const sh = summarize(her, newRenderStats());
  // Linear misses the arc by about the sagitta of a 2-sample chord: R(1 - cos(w*dt)).
  const sagitta = 30 * (1 - Math.cos(1.5 * 0.3));
  assert.ok(Math.abs(sl["trains.loo_p50"] - sagitta) < 0.05, `linear p50 ${sl["trains.loo_p50"]} vs ${sagitta}`);
  assert.ok(sh["trains.loo_p90"] < sl["trains.loo_p90"] / 10, `hermite ${sh["trains.loo_p90"]} linear ${sl["trains.loo_p90"]}`);
  assert.equal(sl["trains.loo_curves_n"], sl["trains.loo_n"]);
});

test("render sampling: on-rail train has no off-rail frames or jumps", () => {
  const mask = new RailMask();
  for (let x = -5; x <= 205; x++) mask.setTile(x, 0);
  const s: TrainSample[] = [];
  for (let i = 0; i <= 100; i++) s.push(sample(i * 100, 2 * i + 0.5, 0.5, 20, 0)); // 20 tiles/s along y=0.5
  const j: Journey = { id: "r", force: "player", name: "l", samples: s, handoffs: [] };
  const st = newRenderStats();
  renderJourney(referenceRenderer("linear"), j, mask, { ...defaultOptions, hz: 10 }, st);
  const m = summarize({ all: [], curves: [] }, st);
  assert.equal(m["trains.frames"], 101);
  assert.equal(m["trains.offrail_share"], 0);
  assert.equal(m["trains.raw_offrail_share"], 0);
  assert.equal(m["trains.jumps"], 0);
  assert.equal(m["trains.offrail_excess_pp"], 0);
});

test("render sampling: a teleport is a jump, unless it is a handoff", () => {
  const mask = new RailMask();
  const s = [sample(0, 0, 0, 0, 0), sample(1000, 0, 0, 0, 0), sample(1100, 50, 0, 0, 0), sample(3000, 50, 0, 0, 0)];
  const j: Journey = { id: "t", force: "player", name: "l", samples: s, handoffs: [] };
  const st = newRenderStats();
  renderJourney(referenceRenderer("linear"), j, mask, { ...defaultOptions, hz: 10 }, st);
  assert.equal(st.jumps, 1);
  assert.equal(st.jumpsOutsideHandoff, 1);
  const st2 = newRenderStats();
  renderJourney(referenceRenderer("linear"), { ...j, handoffs: [[1000, 1100]] }, mask, { ...defaultOptions, hz: 10 }, st2);
  assert.equal(st2.jumps, 1);
  assert.equal(st2.jumpsOutsideHandoff, 0);
});

test("a gone hides the train", () => {
  const mask = new RailMask();
  const s = [sample(0, 0, 0, 1, 0), sample(500, 0.5, 0, 1, 0), sample(1000, 0, 0, 0, 0, true)];
  const st = newRenderStats();
  renderJourney(referenceRenderer("linear"), { id: "g", force: "p", name: "l", samples: s, handoffs: [] }, mask, defaultOptions, st);
  assert.equal(st.frames, 10); // t = 0..900 ms; at 1000 the gone applies
});

test("illisis names", () => {
  assert.equal(illisisName("nauvis"), "nauvis");
  assert.equal(illisisName("Nauvis.1"), "~4Eauvis~2E1");
});

test("rail mask takes every track colour (rail, elevated rail, rail ramp)", () => {
  const px = new Uint16Array(512 * 512);
  px[0] = 0x8c71; // rail
  px[1] = 0xbdd7; // elevated rail
  px[2] = 0xa534; // rail ramp
  px[3] = 0x51a3; // something else
  const m = new RailMask();
  m.addArea(0, 0, px);
  assert.equal(m.railTiles, 3);
  assert.ok(m.isRail(-256, -256) && m.isRail(-255, -256) && m.isRail(-254, -256));
  assert.ok(!m.isRail(-253, -256));
});
