// The rail path model (railpath.ts) against the shared test vectors of docs/v3/RAIL-GEOMETRY.md
// (docs/v3/rail-geometry-vectors.json): piece shapes and ends, carriages as the game placed them
// on chains built from curved pieces and ramps alone, and the timing model. capture/test runs the
// Lua implementation against the same file.
import "./eval/ts-resolve.mjs";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const P = await import("./railpath.ts");

const here = path.dirname(fileURLToPath(import.meta.url));
const vectors = JSON.parse(fs.readFileSync(path.join(here, "..", "..", "..", "docs", "v3", "rail-geometry-vectors.json"), "utf8"));

/** The `rp` kind of a rail type, or 0 for a straight piece. */
function kindOf(type: string): number {
  switch (type.replace(/^elevated-/, "")) {
    case "curved-rail-a":
      return P.KIND_CURVE_A;
    case "curved-rail-b":
      return P.KIND_CURVE_B;
    case "legacy-curved-rail":
      return P.KIND_LEGACY_CURVE;
    case "rail-ramp":
      return P.KIND_RAMP;
  }
  return 0;
}

const turnDiff = (a: number, b: number): number => {
  const d = Math.abs(a - b) % 1;
  return Math.min(d, 1 - d);
};
const orientationOf = (x: number, y: number): number => ((Math.atan2(x, -y) / (2 * Math.PI)) % 1 + 1) % 1;

test("piece shapes and lengths", () => {
  const out = [0, 0];
  let n = 0;
  for (const p of vectors.pieces) {
    const k = kindOf(p.type);
    if (!k) {
      continue;
    }
    n++;
    assert.ok(Math.abs(P.pieceLength(k) - p.length) < 1e-6, `${p.type}/${p.dir} length`);
    const tol = k === P.KIND_RAMP ? 1e-3 : 1e-6;
    for (const [s, x, y] of p.points) {
      P.pieceOffset(k, p.dir, s, out);
      assert.ok(Math.hypot(p.x + out[0] - x, p.y + out[1] - y) < tol, `${p.type}/${p.dir} at ${s}`);
    }
  }
  assert.ok(n >= 40);
});

test("piece ends match the game's", () => {
  for (const p of vectors.pieces) {
    const k = kindOf(p.type);
    if (!k) {
      continue;
    }
    const e = P.pieceEnds(k, p.dir);
    assert.deepEqual([p.x + e[0], p.y + e[1], e[2]], p.front_end, `${p.type}/${p.dir} front end`);
    assert.deepEqual([p.x + e[3], p.y + e[4], e[5]], p.back_end, `${p.type}/${p.dir} back end`);
  }
});

/** The vector path's point at rail distance u (pieces from the rear, straights included). */
function vectorPoint(pathList: { type: string; x: number; y: number; dir: number; rev: boolean }[], u: number): [number, number, number, number] {
  const out = [0, 0];
  let rest = u;
  for (let i = 0; i < pathList.length; i++) {
    const r = pathList[i];
    const k = kindOf(r.type);
    const L = k ? P.pieceLength(k) : straightLength(r.type, r.dir);
    if (rest <= L || i === pathList.length - 1) {
      const q = Math.max(0, Math.min(L, rest));
      const s = r.rev ? L - q : q;
      const at = (ss: number): [number, number] => {
        if (k) {
          P.pieceOffset(k, r.dir, ss, out);
        } else {
          straightOffset(r.type, r.dir, ss, out);
        }
        return [r.x + out[0], r.y + out[1]];
      };
      const [x, y] = at(s);
      const [ax, ay] = at(Math.max(0, s - 1e-4));
      const [bx, by] = at(Math.min(L, s + 1e-4));
      const sg = r.rev ? -1 : 1;
      const l = Math.hypot(bx - ax, by - ay) || 1;
      return [x, y, (sg * (bx - ax)) / l, (sg * (by - ay)) / l];
    }
    rest -= L;
  }
  throw new Error("empty path");
}

function straightLength(type: string, dir: number): number {
  const t = type.replace(/^elevated-/, "");
  if (t === "half-diagonal-rail") {
    return 2 * Math.sqrt(5);
  }
  if (t === "legacy-straight-rail") {
    return (dir >> 1) & 1 ? Math.SQRT2 : 2;
  }
  return (dir >> 1) & 1 ? 2 * Math.SQRT2 : 2;
}

/** RAIL-GEOMETRY.md 2.2, 2.3, 2.7: the straight pieces (for walking the vectors' paths only). */
function straightOffset(type: string, dir: number, s: number, out: number[]): void {
  const t = type.replace(/^elevated-/, "");
  let x: number;
  let y: number;
  if (t === "half-diagonal-rail") {
    const f = 1 - s / Math.sqrt(5);
    [x, y] = [f, 2 * f];
    if ((dir >> 1) & 1) {
      x = -x;
    }
    if ((dir >> 2) & 1) {
      [x, y] = [-y, x];
    }
  } else if (t === "legacy-straight-rail") {
    const u = (d: number): [number, number] => [Math.sin((d * Math.PI) / 8), -Math.cos((d * Math.PI) / 8)];
    if (!((dir >> 1) & 1)) {
      const [ux, uy] = u(dir);
      [x, y] = [(1 - s) * ux, (1 - s) * uy];
    } else {
      const [ax, ay] = u((dir + 14) % 16);
      const [bx, by] = u((dir + 4) % 16);
      [x, y] = [ax + s * bx, ay + s * by];
    }
  } else {
    if ((dir >> 1) & 1) {
      [x, y] = [-1 + s / Math.SQRT2, 1 - s / Math.SQRT2];
    } else {
      [x, y] = [0, s - 1];
    }
    if ((dir >> 2) & 1) {
      [x, y] = [-y, x];
    }
  }
  out[0] = x;
  out[1] = y;
}

test("carriages on a chain of curved pieces and ramps, as the game placed them", () => {
  const out = [0, 0, 0, 0];
  for (const t of vectors.trains) {
    const pieces = t.path.filter((r: { type: string }) => kindOf(r.type)).map((r: { type: string; x: number; y: number; dir: number }) => ({ x: r.x, y: r.y, dir: r.dir, kind: kindOf(r.type) }));
    const [fx, fy, tx, ty] = vectorPoint(t.path, t.u0);
    const chain = new P.RailChain(fx, fy, tx, ty, pieces);
    assert.ok(!chain.broken, `${t.source}: chain broken`);
    const u0 = chain.project(fx, fy);
    assert.ok(chain.lastDistance < 1e-3, `${t.source}: leading joint ${chain.lastDistance} off the chain`);
    for (const [i, [x, y, o]] of t.game_carriages.entries()) {
      chain.carriage(u0 - i * (P.JOINT + P.CONNECTION), out);
      const e = Math.hypot(out[0] - x, out[1] - y);
      assert.ok(e < vectors.tolerance.train_position_vs_game, `${t.source} carriage ${i}: ${e.toFixed(4)} off`);
      const a = orientationOf(out[2], out[3]);
      const d = Math.min(turnDiff(a, o), turnDiff(a, o + 0.5)) * 360;
      assert.ok(d < vectors.tolerance.train_orientation_vs_game_deg, `${t.source} carriage ${i}: ${d.toFixed(3)} degrees`);
    }
  }
});

test("chains: append, resends and missed pieces", () => {
  // curved-rail-a dir 0 at the origin runs from (0, 2) north to (-1, -3); a train coming from
  // the south along x = 0 enters it at its front end
  const chain = new P.RailChain(0, 20, 0, -1, []);
  assert.ok(chain.append({ x: 0, y: 0, dir: 0, kind: P.KIND_CURVE_A }));
  assert.ok(chain.append({ x: 0, y: 0, dir: 0, kind: P.KIND_CURVE_A }), "a resend is accepted");
  const out = [0, 0, 0, 0];
  chain.point(-18, out);
  assert.ok(Math.hypot(out[0], out[1] - 20) < 1e-9, "the lead-in runs back along the anchor line");
  chain.point(P.pieceLength(P.KIND_CURVE_A), out);
  assert.ok(Math.hypot(out[0] + 1, out[1] + 3) < 1e-9, "the piece's back end");
  // a piece that does not continue the chain (a skipped piece)
  assert.ok(!chain.append({ x: 40, y: 40, dir: 0, kind: P.KIND_CURVE_A }));
  // the projection finds rail distances along it
  const u = chain.project(0, 10);
  assert.ok(Math.abs(u + 8) < 1e-9, `u ${u}`);
});

test("timing model", () => {
  const out = [0, 0];
  // constant speed: linear
  P.timingU(30, 60, 0.5, 0.5, 30, out);
  assert.ok(Math.abs(out[0] - 15) < 1e-9 && Math.abs(out[1] - 0.5) < 1e-9);
  // uniform acceleration from rest is exact
  const a = 0.01;
  P.timingU((a * 40 * 40) / 2, 40, 0, a * 40, 20, out);
  assert.ok(Math.abs(out[0] - (a * 20 * 20) / 2) < 1e-9);
  // a start found late: it stands, then accelerates uniformly to the start sample's speed
  P.timingU(2, 100, 0, 0.2, 50, out);
  assert.equal(out[0], 0);
  P.timingU(2, 100, 0, 0.2, 90, out);
  assert.ok(Math.abs(out[0] - 0.5) < 1e-9, `${out[0]}`);
  // a stop read late: uniform braking, then it stands
  P.timingU(3, 100, 0.2, 0, 30, out);
  assert.ok(Math.abs(out[0] - 3) < 1e-9 && out[1] === 0);
  for (const [L, T, va, vb, tau, u, rate] of vectors.timing ?? []) {
    P.timingU(L, T, va, vb, tau, out);
    assert.ok(Math.abs(out[0] - u) < 1e-9 && Math.abs(out[1] - rate) < 1e-9, `timing ${[L, T, va, vb, tau]}`);
  }
});
