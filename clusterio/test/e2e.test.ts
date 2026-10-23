// End to end with the real timeshadedb binary (skipped without one: npm run build:binary): two gridworld instances
// and a third plain one, each running the instance plugin, feed the controller plugin, which forwards to its sidecar.
// A train crosses from one tile to the other and must come out as one journey with a handoff. Time is replayed: the
// sidecar runs with --clock data and the plugins stamp a simulated clock two hours in the past, so windows close in
// seconds.
import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import jwt from "jsonwebtoken";
import { binaryName } from "../binary.js";
import loadControllerPlugin from "../controller.js";
import loadInstancePlugin from "../instance.js";
import { EnvelopesEvent, EnvelopesRequest } from "../messages.js";
import { fakeControllerContext, fakeInstanceContext, fakeUser, sleep, until } from "./fakes.js";

const pluginDir = path.resolve(import.meta.dirname, "..", "..", "..");
const binary = process.env.TIMESHADEDB_BINARY ?? path.join(pluginDir, "bin", binaryName());
const fixture = path.resolve(pluginDir, "..", "capture", "test", "fixtures", "game_envelopes.ndjson");
const skip = fs.existsSync(binary) ? false : `no timeshadedb binary at ${binary} (npm run build:binary)`;

/** MVW3 (docs/v3/FORMAT.md 7.4), the parts this test reads. */
function decodeMVW3(buf: Buffer) {
	assert.equal(buf.toString("latin1", 0, 4), "MVW3");
	const kind = buf[4];
	const start = Number(buf.readBigInt64LE(8));
	const entries = buf.readUInt32LE(20);
	const rows = buf.readUInt32LE(24);
	const tableLen = buf.readUInt32LE(28);
	const table = JSON.parse(buf.toString("utf8", 32, 32 + tableLen)) as { id: string; k: string; name: string }[];
	assert.equal(table.length, entries);
	let off = 32 + tableLen;
	const cols: Buffer[] = [];
	for (let i = 0; i < 8; i++) {
		const n = buf.readUInt32LE(off);
		cols.push(buf.subarray(off + 4, off + 4 + n));
		off += 4 + n;
	}
	const reader = (b: Buffer) => {
		let p = 0;
		const uvarint = () => {
			let v = 0n;
			let shift = 0n;
			for (;;) {
				const byte = b[p++];
				v |= BigInt(byte & 0x7f) << shift;
				if (byte < 0x80) {
					return v;
				}
				shift += 7n;
			}
		};
		const varint = () => {
			const u = uvarint();
			return Number((u >> 1n) ^ -(u & 1n));
		};
		return { uvarint: () => Number(uvarint()), varint };
	};
	const counts = reader(cols[0]);
	const dt = reader(cols[1]);
	const xs = reader(cols[3]);
	const ys = reader(cols[4]);
	const flags = cols[6];
	const out: { id: string; t: number; x: number; y: number; flags: number }[] = [];
	let r = 0;
	for (const entry of table) {
		const n = counts.uvarint();
		let t = start;
		let x = 0;
		let y = 0;
		for (let i = 0; i < n; i++, r++) {
			t += dt.varint();
			x += xs.varint();
			y += ys.varint();
			out.push({ id: entry.id, t, x: x / 16, y: y / 16, flags: flags[r] });
		}
	}
	assert.equal(r, rows);
	return { kind, start, table, rows: out };
}

test("two gridworld instances: a train crossing between them is one journey with a handoff; a real capture ingests cleanly", { skip, timeout: 120_000 }, async () => {
	const window = 300_000;
	// Two hours ago, 30 s into a 5-minute mover window.
	const t0 = Math.floor((Date.now() - 2 * 3600_000) / window) * window + 30_000;
	let sim = t0;
	const now = () => sim;

	const c = await fakeControllerContext();
	const tile = (x: number) => ({ world: "w", x, y: 0, size: 512, surface: "nauvis" });
	c.addInstance(101, { "gridworld.tile": tile(0) });
	c.addInstance(102, { "gridworld.tile": tile(1) });
	c.addInstance(103);
	c.users.set("alice", fakeUser("alice", ["timeshade.view"]));
	const plugin = await loadControllerPlugin(c.context, {
		command: binary,
		extraArgs: ["--clock", "data", "--mover-grace", "15s", "--terrain-grace", "10s", "--commit-every", "200ms", "--no-viewer"],
		now,
	});
	const instances = new Map<number, ReturnType<typeof fakeInstanceContext>>();
	for (const id of [101, 102, 103]) {
		const f = fakeInstanceContext(id, { "gridworld.tile": id === 103 ? null : tile(id - 101) },
			(message: EnvelopesRequest | EnvelopesEvent) => c.deliver(message.constructor, message, { type: "instance", id }));
		await loadInstancePlugin(f.context, { now, batchMs: 20 });
		instances.set(id, f);
	}
	const token = jwt.sign({ user: "alice", iat: Math.floor(Date.now() / 1000) }, c.controller.authSecret, { audience: "user" });
	const api = async (rel: string) => {
		const res = await fetch(`${c.web.url}/api/timeshade/api/v3/${rel}`, { headers: { Authorization: `Bearer ${token}`, "Accept-Encoding": "gzip" } });
		// fetch decodes the gzip body itself.
		return { status: res.status, encoding: res.headers.get("content-encoding"), body: Buffer.from(await res.arrayBuffer()) };
	};
	try {
		await plugin.sidecar.ready(30_000);
		// Each instance's game clock: 60 ticks a second from its own base.
		const tickOf = (id: number, t: number) => Math.round((t - t0) * 60 / 1000) + id * 1000;
		const send = async (id: number, t: number, msgs: unknown[]) => {
			sim = t;
			await instances.get(id)!.ipc({ v: 1, tick: tickOf(id, t), msgs });
		};
		const j = "101.6000.5";
		const meta = { k: "t", f: "player", name: "locomotive", n: 2, o: "LC", c: "ff0000" };
		const sample = (id: number, t: number, x: number, extra = {}) => ({ j, ...meta, x, y: 0.5, h: 0.25, tick: tickOf(id, t), ...extra });
		// Instance 101 sees the train from x 200 to 250, then it crosses (gone); 102 adopts it and sees it on.
		for (let i = 0; i <= 10; i++) {
			const t = t0 + (i * 1000);
			await send(101, t, [{ type: "move", s: "nauvis", m: [sample(101, t, 200 + (5 * i), i === 0 ? { car: [[200, 0.5, 0.25], [207, 0.5, 0.25]] } : {})] }]);
		}
		await send(101, t0 + 11_000, [{ type: "move", s: "nauvis", m: [{ j, gone: true, tick: tickOf(101, t0 + 11_000) }] }]);
		await sleep(100);
		await send(102, t0 + 11_500, [{ type: "adopt", j, trace: "ue.1" }]);
		for (let i = 0; i <= 8; i++) {
			const t = t0 + 12_000 + (i * 1000);
			await send(102, t, [{ type: "move", s: "nauvis", m: [sample(102, t, 260 + (5 * i))] }]);
		}
		// A second train whose adopt reaches the controller before the old instance's gone (each instance sends about
		// once a second, a Universal Edges crossing takes less): the gone is from before the adopt, so it is kept, and
		// the train stays one journey with a handoff.
		const j2 = "101.6000.9";
		const s2 = (id: number, t: number, x: number) => ({ ...sample(id, t, x), j: j2, y: 40.5 });
		for (let i = 0; i <= 10; i++) {
			const t = t0 + 30_000 + (i * 1000);
			await send(101, t, [{ type: "move", s: "nauvis", m: [s2(101, t, 200 + (5 * i))] }]);
		}
		await send(102, t0 + 41_000, [{ type: "adopt", j: j2, trace: "ue.2" }]);
		await sleep(100);
		// printed at 40.5 s, so its envelope says so; it reaches the plugin after the adopt
		await instances.get(101)!.ipc({ v: 1, tick: tickOf(101, t0 + 40_500), msgs: [
			{ type: "move", s: "nauvis", m: [{ j: j2, gone: true, tick: tickOf(101, t0 + 40_500) }] },
		] });
		for (let i = 0; i <= 8; i++) {
			const t = t0 + 42_000 + (i * 1000);
			await send(102, t, [{ type: "move", s: "nauvis", m: [s2(102, t, 260 + (5 * i))] }]);
		}
		// A real capture (the Clusterio module in a headless game) on the third instance, at the same time.
		const lines = fs.readFileSync(fixture, "utf8").split("\n").filter(Boolean);
		for (const line of lines) {
			const env = JSON.parse(line);
			sim = t0 + Math.round(env.tick * 1000 / 60);
			await instances.get(103)!.ipc(env);
		}
		await sleep(200);
		// Move the clock past the window's close (start + 5 min + the 15 s grace) with a heartbeat of another mover.
		for (const dt of [200_000, 400_000]) {
			await send(101, t0 + dt, [{ type: "move", s: "nauvis", m: [{ j: "p:bob", k: "p", f: "player", name: "bob", x: 1, y: 1, tick: tickOf(101, t0 + dt) }] }]);
		}
		const day = new Date(t0).toISOString().slice(0, 10);
		const windowStart = t0 - 30_000;
		let index: any;
		await until(async () => {
			const r = await api(`movers/gw-w.nauvis/day/${day}.json`);
			index = r.status === 200 ? JSON.parse(r.body.toString()) : undefined;
			return Boolean(index?.windows?.some((w: any) => w.start === windowStart));
		}, 30_000, "the mover window to close");
		const win = await api(`movers/gw-w.nauvis/window/${windowStart}.bin`);
		assert.equal(win.status, 200);
		assert.equal(win.encoding, "gzip", "the sidecar's compression passes through the proxy");
		const mvw = decodeMVW3(win.body);
		assert.equal(mvw.kind, 1);
		const train = mvw.rows.filter(r => r.id === j);
		assert.deepEqual([...new Set(mvw.table.filter(e => e.k === "t").map(e => e.id))].sort(), [j, j2], "one journey per train");
		assert.equal(train.length, 11 + 1 + 9, "every sample of both instances and the gone");
		const gone = train.filter(r => r.flags & 2);
		assert.equal(gone.length, 1);
		assert.ok(gone[0].flags & 4, "the gone is a handoff (resumed within 10 s)");
		assert.equal(gone[0].t, t0 + 11_000);
		assert.ok(train.some(r => r.x < 256) && train.some(r => r.x > 256), "positions from both tiles");
		assert.equal(train.at(-1)!.x, 300);
		const train2 = mvw.rows.filter(r => r.id === j2);
		assert.equal(train2.length, 11 + 1 + 9, "both instances' samples and the late gone");
		const gone2 = train2.filter(r => r.flags & 2);
		assert.deepEqual(gone2.map(r => [r.t, Boolean(r.flags & 4)]), [[t0 + 40_500, true]], "the gone, a handoff");

		const catalog = JSON.parse((await api("catalog")).body.toString());
		const gw = catalog.maps.find((m: any) => m.key === "gw-w");
		assert.deepEqual(gw.instances, [
			{ id: "101", name: "inst101", bounds: [-256, -256, 256, 256] },
			{ id: "102", name: "inst102", bounds: [256, -256, 768, 256] },
		]);
		assert.equal(gw.tile_size, 512);
		assert.ok(gw.surfaces.some((s: any) => s.name === "nauvis" || s.surface === "nauvis"), JSON.stringify(gw.surfaces));
		const own = catalog.maps.find((m: any) => m.key === "i-103");
		assert.ok(own, "the plain instance's map");
		await until(async () => {
			const cat = JSON.parse((await api("catalog")).body.toString());
			const m = cat.maps.find((x: any) => x.key === "i-103");
			return JSON.stringify(m?.surfaces ?? []).includes("ts-lab");
		}, 20_000, "the captured surface in the catalog");
		assert.equal(plugin.queue.stats.badLines, 0);
		assert.equal(plugin.queue.stats.badMessages, 0);
		assert.equal(plugin.queue.stats.droppedLines, 0);

		// Auth against the real sidecar: no token, a bad one, and one without the permission.
		assert.equal((await fetch(`${c.web.url}/api/timeshade/api/v3/catalog`)).status, 401);
		c.users.set("bob", fakeUser("bob", []));
		const bob = jwt.sign({ user: "bob" }, c.controller.authSecret, { audience: "user" });
		assert.equal((await fetch(`${c.web.url}/api/timeshade/api/v3/catalog`, { headers: { Authorization: `Bearer ${bob}` } })).status, 403);
		// The sidecar itself refuses reads without its read token.
		assert.equal((await fetch(`${plugin.sidecar.url}/api/v3/catalog`)).status, 401);
	} finally {
		await c.call("shutdown");
		await c.web.close();
		for (const f of instances.values()) {
			await f.call("exit");
		}
	}
	assert.equal(plugin.sidecar.state, "stopped");
});
