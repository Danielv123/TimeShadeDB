import { test } from "node:test";
import assert from "node:assert/strict";
import * as path from "node:path";
import loadControllerPlugin from "../controller.js";
import { EnvelopesEvent, EnvelopesRequest, ViewTokenRequest } from "../messages.js";
import { fakeControllerContext, fakeUser, until } from "./fakes.js";

const fake = path.join(import.meta.dirname, "fake_sidecar.js");

async function start(fields: Record<string, unknown> = {}) {
	const c = await fakeControllerContext(fields);
	c.addInstance(1, { "gridworld.tile": { world: "w", x: 0, y: 0, size: 512, surface: "nauvis" } });
	c.addInstance(2);
	const plugin = await loadControllerPlugin(c.context, {
		command: process.execPath, commandArgs: [fake], sidecar: { minBackoffMs: 50 }, mapsEveryMs: 100,
	});
	const url = await plugin.sidecar.ready(10_000);
	const received = async () => (await (await fetch(`${plugin.sidecar.url}/_received`)).json()) as { lines: string[]; args: string[] };
	return { c, plugin, url, received };
}

test("the sidecar runs serve on a random loopback port with random tokens, the database under the controller's", async () => {
	const { c, plugin, received } = await start();
	try {
		const { args } = await received();
		const arg = (name: string) => args[args.indexOf(name) + 1];
		assert.equal(args[0], "serve");
		assert.equal(arg("--listen"), "127.0.0.1:0");
		assert.equal(arg("--db"), path.resolve(c.dir, "timeshade"));
		assert.equal(arg("--mem-budget"), String(64 << 20));
		assert.match(arg("--ingest-token"), /^[0-9a-f]{48}$/);
		assert.match(arg("--read-token"), /^[0-9a-f]{48}$/);
		assert.notEqual(arg("--ingest-token"), arg("--read-token"));
		assert.ok(args.includes("--exit-on-stdin-close"));
	} finally {
		await c.call("shutdown");
		await c.web.close();
	}
	assert.equal(plugin.sidecar.state, "stopped");
});

test("envelopes are forwarded with src and instance from the link address; maps are published", async () => {
	const { c, plugin, received } = await start();
	try {
		// A request while the instance runs, an event (the same handling) at its exit.
		await c.deliver(EnvelopesRequest, new EnvelopesRequest([
			{ v: 1, tick: 10, msgs: [{ type: "chunk", s: "nauvis", f: "player", x: 0, y: 0 }], map: "gw-w", recv_ms: 1000, seq: 5 },
		]), { type: "instance", id: 1 });
		await c.deliver(EnvelopesEvent, new EnvelopesEvent([
			// src and instance in the payload are overwritten
			{ v: 1, tick: 11, msgs: [], map: "i-1", recv_ms: 1001, seq: 6, src: "spoof", instance: 99 } as any,
		]), { type: "instance", id: 1 });
		await until(async () => (await received()).lines.filter(l => JSON.parse(l).src === "clusterio:1").length === 2, 5000, "forwarded lines");
		const lines = (await received()).lines.map(l => JSON.parse(l));
		const fwd = lines.filter(l => l.src === "clusterio:1");
		assert.deepEqual(fwd.map(l => [l.map, l.instance, l.tick, l.seq]), [["gw-w", 1, 10, 5], ["i-1", 1, 11, 6]]);
		const maps = lines.filter(l => l.src === "clusterio:controller");
		// All of them twice when the maps timer (100 ms here) fired before the sidecar was ready, once more at ready.
		assert.deepEqual([...new Set(maps.map(l => l.map))].sort(), ["gw-w", "i-1", "i-2"]);
		const gw = maps.find(l => l.map === "gw-w");
		assert.deepEqual(gw.msgs, [{ type: "map", info: {
			key: "gw-w", title: "Gridworld w", tile_size: 512, instances: [{ id: "1", name: "inst1", bounds: [-256, -256, 256, 256] }],
		} }]);
		assert.ok(c.lines.some(l => l.message === "timeshade.integration gridworld"));

		// A renamed instance is published again; unchanged maps are not.
		const before = (await received()).lines.length;
		c.instances.get(2)!.config.set("instance.name", "renamed");
		await c.call("instanceConfigFieldChanged", c.instances.get(2), "instance.name", "renamed", "inst2");
		await until(async () => (await received()).lines.length > before, 5000, "the new map message");
		const added = (await received()).lines.slice(before).map(l => JSON.parse(l));
		assert.deepEqual(added.map(l => [l.map, l.msgs[0].info.title]), [["i-2", "renamed"]]);
	} finally {
		await c.call("shutdown");
		await c.web.close();
	}
});

test("after the shutdown hook closed the queue, a request fails (the instance keeps it) and an event is dropped", async () => {
	const { c, plugin } = await start();
	const env = (seq: number) => ({ v: 1, tick: seq, msgs: [], map: "i-1", recv_ms: seq, seq });
	try {
		// Clusterio runs the hook first and closes the links after it, so messages can still arrive.
		await c.call("shutdown");
		assert.ok(plugin.queue.isClosed);
		await assert.rejects(
			c.deliver(EnvelopesRequest, new EnvelopesRequest([env(7)]), { type: "instance", id: 1 }),
			/shutting down/,
		);
		await c.deliver(EnvelopesEvent, new EnvelopesEvent([env(8)]), { type: "instance", id: 1 });
	} finally {
		await c.web.close();
	}
});

test("view tokens: given to user connections over the link, accepted by the proxy, which reaches the sidecar", async () => {
	const { c, plugin } = await start();
	try {
		c.users.set("alice", fakeUser("alice", ["timeshade.view"]));
		c.controls.set(7, { user: c.users.get("alice")! });
		const res = await c.deliver(ViewTokenRequest, new ViewTokenRequest(), { type: "control", id: 7 });
		assert.equal(res.running, true);
		assert.ok(res.expiresMs > Date.now());
		const ok = await fetch(`${c.web.url}/api/timeshade/api/v3/catalog?token=${encodeURIComponent(res.token)}`);
		assert.equal(ok.status, 200);
		assert.deepEqual(await ok.json(), { path: "/api/v3/catalog", query: "" }, "the fake sidecar saw its read token");
		assert.equal((await fetch(`${c.web.url}/api/timeshade/api/v3/catalog`)).status, 401);
		await assert.rejects(c.deliver(ViewTokenRequest, new ViewTokenRequest(), { type: "control", id: 8 }), /user connections/);
	} finally {
		await c.call("shutdown");
		await c.web.close();
	}
	assert.equal(plugin.sidecar.state, "stopped");
});

test("a crashed sidecar is restarted and the proxy and forwarding follow it to its new port", async () => {
	const { c, plugin, url, received } = await start();
	try {
		await fetch(`${url}/_crash`, { method: "POST" }).catch(() => undefined);
		await until(() => plugin.sidecar.state !== "running", 5000, "the crash");
		// Envelopes that arrive while it is down wait in the queue.
		await c.deliver(EnvelopesEvent, new EnvelopesEvent([
			{ v: 1, tick: 1, msgs: [], map: "i-2", recv_ms: 5, seq: 1 },
		]), { type: "instance", id: 2 });
		const next = await plugin.sidecar.ready(10_000);
		assert.notEqual(next, url);
		await until(async () => (await received()).lines.some(l => JSON.parse(l).src === "clusterio:2"), 5000, "the queued line");
		assert.ok(c.lines.some(l => l.message === "timeshade.sidecar exited"));
	} finally {
		await c.call("shutdown");
		await c.web.close();
	}
});

test("a config change restarts the sidecar with the new database; a missing binary is logged, not thrown", async () => {
	const { c, plugin, url, received } = await start();
	try {
		const other = path.join(c.dir, "elsewhere");
		c.controller.config.set("timeshade.database_directory", other);
		await c.call("controllerConfigFieldChanged", "timeshade.database_directory", other, null);
		const next = await plugin.sidecar.ready(10_000);
		assert.notEqual(next, url);
		const { args } = await received();
		assert.equal(args[args.indexOf("--db") + 1], other);
	} finally {
		await c.call("shutdown");
		await c.web.close();
	}

	const d = await fakeControllerContext({ "timeshade.binary_path": path.join(d0(), "missing.exe") });
	const p = await loadControllerPlugin(d.context);
	try {
		assert.equal(p.sidecar.state, "stopped");
		assert.ok(d.lines.some(l => l.message === "timeshade.sidecar no_binary" && l.level === "error"));
		const res = await fetch(`${d.web.url}/api/timeshade/assets/x.js`);
		assert.equal(res.status, 503);
	} finally {
		await d.call("shutdown");
		await d.web.close();
	}
});

function d0() {
	return path.join(import.meta.dirname, "nowhere");
}
