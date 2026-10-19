import { mock, test } from "node:test";
import assert from "node:assert/strict";
import loadInstancePlugin, { BATCH_MS, MAX_EVENT_BYTES } from "../instance.js";
import { fakeInstanceContext } from "./fakes.js";

const tile = { world: "w1", x: 1, y: -2, size: 512, surface: "a\"b\\c'd\ne" };

/** Run fn with setInterval and Date mocked, from time `now`. */
async function withTimers(fn: () => Promise<void>, now = 1_000_000) {
	mock.timers.enable({ apis: ["setInterval", "Date"], now });
	try {
		await fn();
	} finally {
		mock.timers.reset();
	}
}

test("envelopes are stamped at arrival, split by map and sent as one event a second", () => withTimers(async () => {
	const f = fakeInstanceContext(11);
	await loadInstancePlugin(f.context);
	await f.ipc({ v: 1, tick: 5, msgs: [{ type: "chunk", s: "nauvis", f: "player", x: 0, y: 0, d: "eJzt" }] });
	mock.timers.tick(500);
	await f.ipc({ v: 1, tick: 35, msgs: [{ type: "tag", s: "nauvis", f: "player", n: 7 }] });
	assert.equal(f.sent.length, 0, "nothing before the second");
	mock.timers.tick(500);
	assert.equal(f.sent.length, 1);
	const envs = f.sent[0].envelopes;
	assert.deepEqual(envs.map((e: any) => [e.map, e.tick, e.recv_ms, e.msgs[0].type]), [
		["i-11", 5, 1_000_000, "chunk"],
		["i-11", 35, 1_000_500, "tag"],
	]);
	assert.ok(envs[1].seq > envs[0].seq && envs[0].seq > 1_000_000 * 1000, "seq increases from the clock");
	assert.equal(envs[0].src, undefined, "the controller sets src");
	mock.timers.tick(BATCH_MS);
	assert.equal(f.sent.length, 1, "no event without envelopes");
}));

test("invalid lines are dropped with a rate-limited warning; trace lines are logged, never forwarded", () => withTimers(async () => {
	const f = fakeInstanceContext(2);
	await loadInstancePlugin(f.context);
	await f.ipc("not an envelope");
	await f.ipc({ v: 1, tick: -1, msgs: [] });
	await f.ipc({ v: 1, tick: 1, msgs: [
		{ type: "trace", op: "timeshade.integration", phase: "universal_edges", level: "info", world: "w", instance_id: 99, message: "spoof" },
		{ type: "resynced", trace: "ab.1", counts: { chunks: 3 } },
		7,
	] });
	mock.timers.tick(BATCH_MS);
	const warns = f.lines.filter(l => l.message === "timeshade.ipc invalid");
	assert.deepEqual(warns.map(l => l.meta.error), ["not an object", "bad tick", "bad message"]);
	const trace = f.lines.find(l => l.message === "timeshade.integration universal_edges")!;
	assert.deepEqual([trace.level, trace.meta], ["info", { world: "w", op: "timeshade.integration", phase: "universal_edges" }]);
	assert.ok(f.lines.some(l => l.message === "timeshade.resync done" && l.meta.trace === "ab.1"));
	assert.deepEqual(f.sent[0].envelopes.map((e: any) => e.msgs.map((m: any) => m.type)), [["resynced"]]);
}));

test("at start: the config (tile and rails) then a resync, in one /sc, the JSON escaped into a Lua string", () => withTimers(async () => {
	const plain = fakeInstanceContext(1);
	await loadInstancePlugin(plain.context);
	await plain.call("start");
	assert.match(plain.rcon[0], /^\/sc ts_rt\.config\("\{\\"rails\\":false,\\"forces\\":\[\]\}"\) ts_rt\.resync\("[0-9a-f]{8}\.\d+"\)$/);

	const f = fakeInstanceContext(3, { "gridworld.tile": tile, "timeshade.capture_rails": true, "timeshade.extra_forces": " player, enemy,," });
	await loadInstancePlugin(f.context);
	await f.call("start");
	const m = /^\/sc ts_rt\.config\("(.*)"\) ts_rt\.resync\("(.*)"\)$/.exec(f.rcon[0])!;
	assert.ok(!/(^|[^\\])(\\\\)*"/.test(m[1]), "no unescaped quote inside the literal");
	const unescaped = m[1].replace(/\\(.)/g, (_m, c) => (c === "n" ? "\n" : c));
	assert.deepEqual(JSON.parse(unescaped), { rails: true, forces: ["player", "enemy"], tile });
	assert.ok(f.lines.some(l => l.message === "timeshade.resync start" && l.meta.reason === "start" && l.meta.trace === m[2]));
}));

test("a gridworld.tile change, a rails change and a new controller session reconfigure a running instance; nothing else",
	() => withTimers(async () => {
		const f = fakeInstanceContext(4, { "gridworld.tile": null });
		await loadInstancePlugin(f.context);
		await f.call("instanceConfigFieldChanged", "factorio.settings", {}, {});
		f.instance.config.set("gridworld.tile", tile);
		await f.call("instanceConfigFieldChanged", "gridworld.tile", tile, null);
		await f.call("instanceConfigFieldChanged", "timeshade.capture_rails", true, false);
		await f.call("instanceConfigFieldChanged", "timeshade.extra_forces", "player", "");
		await f.call("controllerConnectionEvent", "resume");
		await f.call("controllerConnectionEvent", "drop");
		await f.call("controllerConnectionEvent", "connect");
		assert.deepEqual(f.lines.filter(l => l.message === "timeshade.resync start").map(l => l.meta.reason),
			["tile", "config", "config", "reconnect"]);
		f.instance.status = "stopping";
		await f.call("instanceConfigFieldChanged", "gridworld.tile", null, null);
		await f.call("controllerConnectionEvent", "connect");
		assert.equal(f.rcon.length, 4, "not while the instance isn't running");
		// After the tile change the instance's tile surface goes to the world's map.
		await f.ipc({ v: 1, tick: 1, msgs: [{ type: "chunk", s: tile.surface, f: "player", x: 0, y: 0 }] });
		mock.timers.tick(BATCH_MS);
		assert.equal(f.sent[0].envelopes[0].map, "gw-w1");
	}));

test("the exit sends what waits; a big backlog leaves in events of about 1 MiB; a failed send is retried", () => withTimers(async () => {
	const f = fakeInstanceContext(5);
	await loadInstancePlugin(f.context);
	await f.ipc({ v: 1, tick: 1, msgs: [{ type: "adopt", j: "a", trace: "b" }] });
	await f.call("exit");
	assert.equal(f.sent.length, 1);

	const g = fakeInstanceContext(6);
	let fail = true;
	const sendTo = g.instance.sendTo;
	g.instance.sendTo = (dst: string, event: any) => {
		if (fail) {
			throw new Error("not connected");
		}
		sendTo(dst, event);
	};
	await loadInstancePlugin(g.context);
	const d = "x".repeat(3000);
	for (let i = 0; i < 1000; i++) {
		await g.ipc({ v: 1, tick: i, msgs: [{ type: "chunk", s: "nauvis", f: "player", x: i, y: 0, d }] });
	}
	mock.timers.tick(BATCH_MS);
	assert.equal(g.sent.length, 0);
	assert.ok(g.lines.some(l => l.message === "timeshade.forward send failed"));
	fail = false;
	mock.timers.tick(BATCH_MS);
	assert.ok(g.sent.length >= 3, `${g.sent.length} events`);
	for (const event of g.sent) {
		assert.ok(JSON.stringify(event.envelopes).length <= MAX_EVENT_BYTES + 4096);
	}
	assert.deepEqual(g.sent.flatMap((e: any) => e.envelopes.map((x: any) => x.tick)), [...Array(1000).keys()], "in order");
}));
