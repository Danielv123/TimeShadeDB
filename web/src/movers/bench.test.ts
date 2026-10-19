// bench.ts: the v3 movers as a renderer for the WP11 train evaluator (bench/trains).
import "./eval/ts-resolve.mjs";
import assert from "node:assert/strict";
import { test } from "node:test";

const { createTrainRenderer } = await import("./bench.ts");

const sample = (t: number, x: number, extra: Record<string, unknown> = {}) => ({
  t, tick: null, x, y: 0, vx: 10, vy: 0, h: 0.25, gone: false, n: 3, kinds: "LCC", colour: "", ...extra,
});

test("bench renderer: a straight journey is drawn on its line with its carriages", () => {
  const r = createTrainRenderer();
  assert.equal(r.name, "v3");
  const s = r.session({ id: "j1", force: "player", name: "train" });
  assert.equal(s.pose(0), undefined, "nothing before the first sample");
  for (let i = 0; i <= 10; i++) {
    s.feed(sample(i * 500, i * 5)); // 10 tiles/s east
  }
  for (let t = 1000; t <= 4000; t += 250) {
    const p = s.pose(t);
    assert.ok(p, `drawn at ${t}`);
    assert.ok(Math.abs(p.x - t / 100) < 0.1 && Math.abs(p.y) < 1e-6, `head at ${t}: ${p.x},${p.y}`);
    assert.equal(p.cars.length, 3);
    assert.deepEqual([p.cars[0].x, p.cars[0].y], [p.x, p.y], "the first carriage is the head");
    for (let i = 1; i < 3; i++) {
      const d = p.cars[i - 1].x - p.cars[i].x;
      assert.ok(d > 6 && d < 7.2 && Math.abs(p.cars[i].y) < 1e-6, `carriage ${i} trails by ${d}`);
    }
  }
  // positions go through the store's 1/16-tile quantisation
  s.feed(sample(5500, 55.03));
  const p = s.pose(5500)!;
  assert.equal(p.x, 55);
  // a gone ends the journey once no handoff can follow
  s.feed({ ...sample(6000, 0), gone: true });
  assert.equal(s.pose(30_000), undefined);
});
