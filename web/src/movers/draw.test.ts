// Drawing against a recording 2D context: batching, minimum sizes, culling, text.
import "./eval/ts-resolve.mjs";
import assert from "node:assert/strict";
import { test } from "node:test";

const D = await import("./draw.ts");

type Call = [string, ...unknown[]];

function recorder() {
  const calls: Call[] = [];
  const pen: any = {
    fillStyle: "",
    strokeStyle: "",
    lineWidth: 1,
    lineJoin: "miter",
    font: "",
    textBaseline: "alphabetic",
  };
  for (const m of ["save", "restore", "translate", "rotate", "beginPath", "moveTo", "lineTo", "closePath", "arc", "fill", "stroke", "fillText", "strokeText"]) {
    pen[m] = (...args: unknown[]) => calls.push([m, ...args]);
  }
  return { pen, calls, count: (m: string) => calls.filter(c => c[0] === m).length };
}

const proj = { cx: 0, cy: 0, scale: 4, width: 800, height: 600 };

function train(id: string, x: number, y: number, kinds = "LCCF"): any {
  const cars = [...kinds].map((k, i) => ({ x: x - i * 7, y, a: 0, loco: k === "L", kind: k }));
  return { id, kind: "t", meta: { id, k: "t", f: "player", name: "locomotive", n: kinds.length, o: kinds }, x, y, hx: 1, hy: 0, moving: true, cars, track: null };
}

test("each train's bodies are one fill and one stroke, without save/restore", () => {
  const r = recorder();
  const movers = Array.from({ length: 500 }, (_, i) => train(`t${i}`, (i % 25) * 8 - 100, Math.floor(i / 25) * 6 - 60));
  const n = D.drawTrains(r.pen, proj, movers);
  assert.equal(n, 500);
  assert.equal(r.count("fill"), 500, "one fill per train");
  assert.equal(r.count("stroke"), 1000, "bars + outlines per train");
  assert.equal(r.count("save"), 0, "no per-carriage save/restore");
});

test("artillery wagons are drawn on top, disc over barrel", () => {
  const r = recorder();
  D.drawTrains(r.pen, proj, [train("a", 0, 0, "LA")]);
  assert.equal(r.count("fill"), 3); // train + barrel + disc
  assert.ok(r.calls.some(c => c[0] === "arc"));
});

test("off-screen trains are culled", () => {
  const r = recorder();
  assert.equal(D.drawTrains(r.pen, proj, [train("far", 10_000, 0)]), 0);
  assert.equal(r.count("moveTo"), 0);
});

test("far zoom keeps a minimum carriage size", () => {
  const r = recorder();
  D.drawTrains(r.pen, { ...proj, scale: 0.01 }, [train("a", 0, 0, "C")]);
  const pts = r.calls.filter(c => c[0] === "moveTo" || c[0] === "lineTo").map(c => [c[1] as number, c[2] as number]);
  const xs = pts.map(p => p[0]);
  assert.ok(Math.max(...xs) - Math.min(...xs) >= 2, "at least ~3 px long");
});

test("players are named only when zoomed in; names go through fillText", () => {
  const p: any = { id: "p:alice", kind: "p", meta: { id: "p:alice", k: "p", f: "player", name: "<b>alice</b>" }, x: 1, y: 1, hx: 1, hy: 0, moving: false, cars: [], track: null };
  const near = recorder();
  D.drawPlayer(near.pen, { ...proj, scale: 2 }, p);
  assert.ok(near.calls.some(c => c[0] === "fillText" && c[1] === "<b>alice</b>"));
  const far = recorder();
  D.drawPlayer(far.pen, { ...proj, scale: 0.5 }, p);
  assert.equal(far.count("fillText"), 0);
});

test("vehicles: spidertron legs and car triangle", () => {
  const spider: any = { id: "v", kind: "v", meta: { id: "v", k: "v", f: "player", name: "spidertron", o: "S" }, x: 0, y: 0, hx: 1, hy: 0, moving: false, cars: [], track: null };
  const r = recorder();
  D.drawVehicle(r.pen, proj, spider);
  assert.ok(r.count("closePath") >= 16, "8 legs stroked and filled");
  const car = { ...spider, meta: { ...spider.meta, o: "C", name: "car" } };
  const r2 = recorder();
  D.drawVehicle(r2.pen, proj, car);
  assert.equal(r2.count("rotate"), 1);
  assert.equal(r2.count("fill"), 1);
});

test("hit test finds the nearest carriage", () => {
  const movers = [train("a", 0, 0), train("b", 0, 20)];
  const [sx, sy] = [(-7 - proj.cx) * proj.scale + proj.width / 2, (20 - proj.cy) * proj.scale + proj.height / 2];
  assert.equal(D.hitMover(proj, movers, sx + 2, sy)?.id, "b");
  assert.equal(D.hitMover(proj, movers, 5, 5), null);
});
