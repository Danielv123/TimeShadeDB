// The reference rail model (railgeom.mjs) against the shared test vectors (docs/rail-geometry-vectors.json): the
// pieces' points, the ends the game reported, and carriages as the game placed them. The capture (Lua), viewer (TS) and
// server (Go) implementations of docs/RAIL-GEOMETRY.md load the same file.
//
//   node --test bench/railbench/railgeom.test.mjs
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { railLength, railPoint, orientationOf, placeTrain } from "./railgeom.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const vectors = JSON.parse(fs.readFileSync(path.join(here, "..", "..", "docs", "rail-geometry-vectors.json"), "utf8"));

const turnDiff = (a, b) => {
	const d = Math.abs(a - b) % 1;
	return Math.min(d, 1 - d);
};

test("vectors cover every rail type", () => {
	const types = new Set(vectors.pieces.map(p => p.type.replace(/^elevated-/, "")));
	for (const t of ["straight-rail", "half-diagonal-rail", "curved-rail-a", "curved-rail-b", "rail-ramp",
		"legacy-straight-rail", "legacy-curved-rail"]) {
		assert.ok(types.has(t), t);
	}
});

test("piece points", () => {
	for (const p of vectors.pieces) {
		const tol = /rail-ramp/.test(p.type) ? 1e-3 : vectors.tolerance.piece_points;
		assert.ok(Math.abs(railLength(p.type, p.dir) - p.length) < 1e-9, `${p.type}/${p.dir} length`);
		assert.ok(Math.abs(p.length - p.game_length) < 1e-3, `${p.type}/${p.dir} length against the game's rail_length`);
		for (const [s, x, y, o] of p.points) {
			const q = railPoint(p, s);
			assert.ok(Math.hypot(q.x - x, q.y - y) < tol, `${p.type}/${p.dir} at s ${s}: (${q.x}, ${q.y}) != (${x}, ${y})`);
			assert.ok(turnDiff(orientationOf(q.tx, q.ty), o) < 1e-4, `${p.type}/${p.dir} tangent at s ${s}`);
		}
	}
});

test("piece ends match the game's rail ends", () => {
	for (const p of vectors.pieces) {
		const a = railPoint(p, 0);
		const b = railPoint(p, p.length);
		const tol = /legacy-curved-rail/.test(p.type) ? vectors.tolerance.piece_ends_vs_game_legacy_curved_rail
			: vectors.tolerance.piece_ends_vs_game;
		assert.ok(Math.hypot(a.x - p.front_end[0], a.y - p.front_end[1]) < tol, `${p.type}/${p.dir} front end`);
		assert.ok(Math.hypot(b.x - p.back_end[0], b.y - p.back_end[1]) < tol, `${p.type}/${p.dir} back end`);
	}
});

test("carriages as the game placed them", () => {
	assert.ok(vectors.trains.length > 0);
	for (const t of vectors.trains) {
		const placed = placeTrain(t.path, t.u0, t.game_carriages.length, vectors.constants.JOINT_DISTANCE,
			vectors.constants.CONNECTION_DISTANCE);
		for (const [i, [x, y, o]] of t.game_carriages.entries()) {
			const e = Math.hypot(placed[i].x - x, placed[i].y - y);
			assert.ok(e < vectors.tolerance.train_position_vs_game, `${t.source} carriage ${i}: ${e.toFixed(4)} tiles off`);
			// compared as axes: a carriage facing the train's rear has the opposite orientation
			const d = Math.min(turnDiff(placed[i].o, o), turnDiff(placed[i].o, o + 0.5)) * 360;
			assert.ok(d < vectors.tolerance.train_orientation_vs_game_deg, `${t.source} carriage ${i}: ${d.toFixed(3)} degrees off`);
		}
	}
});
