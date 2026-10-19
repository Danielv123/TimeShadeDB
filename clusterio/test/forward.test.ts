import { test } from "node:test";
import assert from "node:assert/strict";
import { envelopeError, Splitter } from "../forward.js";

const tile = { world: "w1", x: 1, y: 0, size: 512, surface: "nauvis" };
const seq = () => {
	let n = 0;
	return () => ++n;
};

test("envelopeError accepts Lua's envelopes and names what is wrong with others", () => {
	assert.equal(envelopeError({ v: 1, tick: 0, msgs: [] }), undefined);
	assert.equal(envelopeError({ v: 1, tick: 5, speed: 2, msgs: [] }), undefined);
	assert.equal(envelopeError(null), "not an object");
	assert.equal(envelopeError([1]), "not an object");
	assert.equal(envelopeError({ tick: 0, msgs: [] }), "bad v");
	assert.equal(envelopeError({ v: 1, tick: -1, msgs: [] }), "bad tick");
	assert.equal(envelopeError({ v: 1, tick: 1.5, msgs: [] }), "bad tick");
	assert.equal(envelopeError({ v: 1, tick: 0, speed: 0, msgs: [] }), "bad speed");
	assert.equal(envelopeError({ v: 1, tick: 0 }), "no msgs");
});

test("an instance that is no tile sends everything on its own map, traces taken out, bad messages counted", () => {
	const s = new Splitter(7);
	const out = s.split({ v: 1, tick: 9, speed: 2, msgs: [
		{ type: "chunk", s: "nauvis", f: "player", x: 0, y: 0, d: "eJzt" },
		{ type: "trace", op: "timeshade.integration", phase: "gridworld" },
		"junk", { s: "nauvis" },
		{ type: "move", s: "platform-1", m: [] },
		{ type: "adopt", j: "1.2.3", trace: "t" },
	] }, 1234, seq());
	assert.equal(out.invalid, 2);
	assert.deepEqual(out.traces.map(t => t.phase), ["gridworld"]);
	assert.equal(out.envelopes.length, 1);
	const [e] = out.envelopes;
	assert.deepEqual({ ...e, msgs: e.msgs.map((m: any) => m.type) }, {
		v: 1, tick: 9, speed: 2, map: "i-7", recv_ms: 1234, seq: 1, msgs: ["chunk", "move", "adopt"],
	});
	assert.deepEqual(s.maps(), ["i-7"]);
});

test("a tile's tiled surfaces go to its world's map, the rest to its own; resync markers go to both, in order", () => {
	const s = new Splitter(3);
	s.setTile(tile);
	const next = seq();
	const out = s.split({ v: 1, tick: 1, msgs: [
		{ type: "surfaces", list: [{ name: "nauvis", tile: true }, { name: "vulcanus", tile: true }, { name: "platform", tile: false }] },
		{ type: "chunk", s: "nauvis", f: "player", x: 0, y: 0 },
		{ type: "chunk", s: "vulcanus", f: "player", x: 0, y: 0 },
		{ type: "move", s: "platform", m: [] },
		{ type: "adopt", j: "2.5.9", trace: "x" },
		{ type: "resynced", trace: "r", counts: {} },
	] }, 50, next);
	assert.deepEqual(out.envelopes.map(e => [e.map, e.seq, e.msgs.map((m: any) => `${m.type}:${m.s ?? ""}`)]), [
		["gw-w1", 1, ["surfaces:", "chunk:nauvis", "chunk:vulcanus", "adopt:", "resynced:"]],
		["i-3", 2, ["surfaces:", "move:platform", "adopt:", "resynced:"]],
	]);
	assert.equal(out.resynced.length, 1);
	// A surface listed alone (made mid-game) and not tiled leaves the world's map; tile.surface always stays on it.
	const later = s.split({ v: 1, tick: 2, msgs: [
		{ type: "surfaces", list: [{ name: "vulcanus", tile: false }, { name: "nauvis", tile: false }], created: true },
		{ type: "chunk", s: "vulcanus", f: "player", x: 1, y: 1 },
		{ type: "chunk", s: "nauvis", f: "player", x: 1, y: 1 },
	] }, 60, next);
	assert.deepEqual(later.envelopes.map(e => [e.map, e.msgs.map((m: any) => `${m.type}:${m.s ?? ""}`)]), [
		["gw-w1", ["surfaces:", "chunk:nauvis"]],
		["i-3", ["surfaces:", "chunk:vulcanus"]],
	]);
});

test("a new world forgets the tiled surfaces of the old one; no tile sends everything home", () => {
	const s = new Splitter(4);
	s.setTile(tile);
	s.split({ v: 1, tick: 1, msgs: [{ type: "surfaces", list: [{ name: "gleba", tile: true }] }] }, 1, seq());
	assert.equal(s.mapOfSurface("gleba"), "gw-w1");
	s.setTile({ ...tile, x: 2 });
	assert.equal(s.mapOfSurface("gleba"), "gw-w1", "same world: kept");
	s.setTile({ ...tile, world: "w2" });
	assert.equal(s.mapOfSurface("gleba"), "i-4");
	assert.equal(s.mapOfSurface("nauvis"), "gw-w2");
	s.setTile(null);
	assert.equal(s.mapOfSurface("nauvis"), "i-4");
	assert.deepEqual(s.maps(), ["i-4"]);
});
