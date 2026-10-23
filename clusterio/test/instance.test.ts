import { mock, test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import loadInstancePlugin, { BATCH_MS, HEARTBEAT_MS, MAX_EVENT_BYTES, MAX_PENDING_BYTES } from "../instance.js";
import { EnvelopesEvent, EnvelopesRequest } from "../messages.js";
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

/** Advance the mocked clock by ms, letting the requests sent on the way settle. */
async function tick(ms: number) {
	mock.timers.tick(ms);
	for (let i = 0; i < 20; i++) {
		await Promise.resolve();
	}
}

test("envelopes are stamped at arrival, split by map and sent as one request a second", () => withTimers(async () => {
	const f = fakeInstanceContext(11);
	await loadInstancePlugin(f.context);
	await f.ipc({ v: 1, tick: 5, msgs: [{ type: "chunk", s: "nauvis", f: "player", x: 0, y: 0, d: "eJzt" }] });
	await tick(500);
	await f.ipc({ v: 1, tick: 35, msgs: [{ type: "tag", s: "nauvis", f: "player", n: 7 }] });
	assert.equal(f.sent.length, 0, "nothing before the second");
	await tick(500);
	assert.equal(f.sent.length, 1);
	assert.ok(f.sent[0] instanceof EnvelopesRequest);
	const envs: any[] = f.sent[0].envelopes;
	assert.deepEqual(envs.map((e: any) => [e.map, e.tick, e.recv_ms, e.msgs[0].type]), [
		["i-11", 5, 1_000_000, "chunk"],
		["i-11", 35, 1_000_500, "tag"],
	]);
	assert.ok(envs[1].seq > envs[0].seq && envs[0].seq > 1_000_000 * 1000, "seq increases from the clock");
	assert.equal(envs[0].src, undefined, "the controller sets src");
	await tick(BATCH_MS);
	assert.equal(f.sent.length, 1, "no request without envelopes");
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
	await tick(BATCH_MS);
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
		await tick(BATCH_MS);
		assert.equal(f.sent[0].envelopes[0].map, "gw-w1");
	}));

test("a caught_up envelope for each map without envelopes for HEARTBEAT_MS, busy maps or not; none before Lua sent anything",
	() => withTimers(async () => {
		const f = fakeInstanceContext(7, { "gridworld.tile": tile });
		await loadInstancePlugin(f.context);
		await tick(3 * HEARTBEAT_MS);
		assert.equal(f.sent.length, 0, "nothing before Lua sent anything");
		await f.ipc({ v: 1, tick: 40, msgs: [{ type: "chunk", s: "nauvis", f: "player", x: 0, y: 0, d: "eJzt" }] });
		await tick(BATCH_MS);
		assert.equal(f.sent.length, 1);
		await tick(HEARTBEAT_MS - 2 * BATCH_MS);
		assert.equal(f.sent.length, 1, "not before HEARTBEAT_MS");
		await tick(BATCH_MS);
		assert.equal(f.sent.length, 2);
		assert.deepEqual(f.sent[1].envelopes.map((e: any) => [e.map, e.tick, e.caught_up, e.ended, e.msgs.length]),
			[["gw-w1", 40, true, undefined, 0], ["i-7", 40, true, undefined, 0]]);
		await tick(HEARTBEAT_MS);
		assert.equal(f.sent.length, 3, "again while idle");
		// The world's map is busy (data every second): the instance's own map still gets its caught_up.
		const before = f.sent.length;
		for (let i = 0; i < 2 * HEARTBEAT_MS / BATCH_MS; i++) {
			await f.ipc({ v: 1, tick: 100 + i, msgs: [{ type: "chunk", s: tile.surface, f: "player", x: i, y: 0, d: "eJzt" }] });
			await tick(BATCH_MS);
		}
		const envs = f.sent.slice(before).flatMap((e: any) => e.envelopes);
		assert.equal(envs.filter((e: any) => e.map === "i-7" && e.caught_up).length, 2);
		assert.equal(envs.filter((e: any) => e.map === "gw-w1" && e.caught_up).length, 0);
		assert.equal(envs.filter((e: any) => e.map === "gw-w1" && !e.caught_up).length, 2 * HEARTBEAT_MS / BATCH_MS);
	}));

test("the exit sends what waits as events (the link routes no responses any more), then ended envelopes", () => withTimers(async () => {
	const f = fakeInstanceContext(5);
	await loadInstancePlugin(f.context);
	await f.ipc({ v: 1, tick: 1, msgs: [{ type: "adopt", j: "a", trace: "b" }] });
	await f.call("exit");
	assert.equal(f.sent.length, 1);
	assert.ok(f.sent[0] instanceof EnvelopesEvent);
	assert.deepEqual(f.sent[0].envelopes.map((e: any) => [e.map, e.caught_up, e.ended]), [["i-5", undefined, undefined], ["i-5", true, true]]);
	await tick(3 * BATCH_MS);
	assert.equal(f.sent.length, 1, "nothing after the exit");
}));

test("a failed request keeps its envelopes for the next try; a big backlog leaves in requests of about 1 MiB, in order", () => withTimers(async () => {
	let fail = true;
	const delivered: any[] = [];
	const g = fakeInstanceContext(6, {}, (message: any) => {
		if (fail) {
			throw new Error("No session");
		}
		delivered.push(message);
	});
	await loadInstancePlugin(g.context);
	const d = "x".repeat(3000);
	for (let i = 0; i < 1000; i++) {
		await g.ipc({ v: 1, tick: i, msgs: [{ type: "chunk", s: "nauvis", f: "player", x: i, y: 0, d }] });
	}
	await tick(BATCH_MS);
	await tick(BATCH_MS);
	assert.equal(g.sent.length, 2, "one try a second");
	assert.equal(delivered.length, 0);
	assert.ok(g.lines.some(l => l.message === "timeshade.forward send failed"));
	fail = false;
	await tick(BATCH_MS);
	assert.ok(delivered.length >= 3, `${delivered.length} requests`);
	for (const request of delivered) {
		assert.ok(request instanceof EnvelopesRequest);
		assert.ok(JSON.stringify(request.envelopes).length <= MAX_EVENT_BYTES + 4096);
	}
	assert.deepEqual(delivered.flatMap((e: any) => e.envelopes.map((x: any) => x.tick)), [...Array(1000).keys()], "in order");
}));

test("a controller restart: requests in the lost session fail late and go again; one request at a time", () => withTimers(async () => {
	// Clusterio keeps messages sent while the link is resuming, and fails them when the controller comes back with a new
	// session: the requests stay open for a while and then fail. (An event would be dropped without a word.)
	const open: { message: any; resolve: () => void; reject: (err: Error) => void }[] = [];
	const g = fakeInstanceContext(9, {}, (message: any) => new Promise<void>((resolve, reject) => {
		open.push({ message, resolve, reject });
	}));
	await loadInstancePlugin(g.context);
	await g.ipc({ v: 1, tick: 1, msgs: [{ type: "chunk", s: "nauvis", f: "player", x: 1, y: 0, d: "eJzt" }] });
	await tick(BATCH_MS);
	await g.call("controllerConnectionEvent", "drop");
	await g.ipc({ v: 1, tick: 2, msgs: [{ type: "chunk", s: "nauvis", f: "player", x: 2, y: 0, d: "eJzt" }] });
	await tick(5 * BATCH_MS);
	assert.equal(open.length, 1, "no second request while one is open");
	open.shift()!.reject(new Error("Session Lost"));
	await tick(0);
	await tick(BATCH_MS);
	assert.equal(open.length, 1);
	assert.deepEqual(open[0].message.envelopes.filter((e: any) => !e.caught_up).map((e: any) => e.tick), [1, 2], "the failed envelopes again, first");
	open.shift()!.resolve();
	await tick(0);
	await tick(BATCH_MS);
	// Delivered: nothing is sent again (only caught_up envelopes follow).
	await tick(5 * BATCH_MS);
	assert.ok(open.every(o => o.message.envelopes.every((e: any) => e.caught_up)), "only caught_up after the delivery");
}));

test("an exit while the controller is away keeps everything on disk for the next start", () => withTimers(async () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "tsp-"));
	const g = fakeInstanceContext(10);
	await loadInstancePlugin(g.context, { spoolDir: dir });
	await g.ipc({ v: 1, tick: 1, msgs: [{ type: "chunk", s: "nauvis", f: "player", x: 1, y: 0, d: "eJzt" }] });
	await g.call("controllerConnectionEvent", "drop");
	await g.call("exit");
	assert.equal(g.sent.length, 0, "no events into a dropped link");
	const h = fakeInstanceContext(10);
	await loadInstancePlugin(h.context, { spoolDir: dir });
	await tick(BATCH_MS);
	assert.deepEqual(h.sent.flatMap((e: any) => e.envelopes).map((e: any) => [e.tick, e.caught_up, e.ended]),
		[[1, undefined, undefined], [1, true, true]]);
	fs.rmSync(dir, { recursive: true, force: true });
}));

test("a long controller outage: envelopes past MAX_PENDING_BYTES wait on disk, survive a host restart and go in order", () => withTimers(async () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "tsp-"));
	const g = fakeInstanceContext(8);
	g.instance.sendTo = () => {
		throw new Error("not connected");
	};
	await loadInstancePlugin(g.context, { spoolDir: dir });
	const d = "x".repeat(200_000);
	const n = Math.ceil(MAX_PENDING_BYTES / 200_000) + 40;
	for (let i = 0; i < n; i++) {
		await g.ipc({ v: 1, tick: i, msgs: [{ type: "chunk", s: "nauvis", f: "player", x: i, y: 0, d }] });
	}
	await tick(BATCH_MS);
	assert.ok(!g.lines.some(l => l.message === "timeshade.forward dropped"), "nothing dropped");
	await g.call("exit"); // the host stops while the controller is still away
	// The next start sends everything in order, then goes on.
	const h = fakeInstanceContext(8);
	await loadInstancePlugin(h.context, { spoolDir: dir });
	await h.ipc({ v: 1, tick: n, msgs: [{ type: "chunk", s: "nauvis", f: "player", x: n, y: 0, d: "eJzt" }] });
	for (let i = 0; i < 20; i++) {
		await tick(BATCH_MS);
	}
	const ticks = h.sent.flatMap((e: any) => e.envelopes.filter((x: any) => x.msgs.length > 0).map((x: any) => x.tick));
	assert.deepEqual(ticks, [...Array(n + 1).keys()]);
	const ended = h.sent.flatMap((e: any) => e.envelopes).filter((x: any) => x.ended);
	assert.equal(ended.length, 1, "the first run's ended envelope, after its data");
	assert.deepEqual(fs.readdirSync(dir), [], "sent segments are deleted");
	fs.rmSync(dir, { recursive: true, force: true });
}));
