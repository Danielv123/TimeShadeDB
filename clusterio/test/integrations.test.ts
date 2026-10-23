import { test } from "node:test";
import assert from "node:assert/strict";
import * as path from "node:path";
import { binaryName, binaryPackage, findBinary } from "../binary.js";
import { edgeLines, mapInfos, mapOf, tileOf, ueApi } from "../integrations.js";
import { fakeConfig } from "./fakes.js";

const tile = (x: number, y: number) => ({ world: "w", x, y, size: 512, surface: "nauvis" });

test("tileOf: gridworld's tile, or null without gridworld (No field named), unset, or malformed", () => {
	assert.equal(tileOf(fakeConfig({})), null);
	assert.equal(tileOf(fakeConfig({ "gridworld.tile": null })), null);
	assert.equal(tileOf(fakeConfig({ "gridworld.tile": { world: "w" } })), null);
	assert.deepEqual(tileOf(fakeConfig({ "gridworld.tile": tile(1, -2) })), tile(1, -2));
});

test("mapOf and mapInfos: one map per instance, one per world with tile bounds; edges on their maps", () => {
	assert.equal(mapOf({ id: 5, tile: null }, "nauvis"), "i-5");
	assert.equal(mapOf({ id: 5, tile: tile(0, 0) }, "nauvis"), "gw-w");
	assert.equal(mapOf({ id: 5, tile: tile(0, 0) }, "gleba"), "i-5");
	assert.equal(mapOf({ id: 5, tile: tile(0, 0) }, "gleba", new Set(["gleba"])), "gw-w");
	const edges = new Map([["gw-w|nauvis", [[256, -256, 256, 256]] as [number, number, number, number][]], ["nope|x", []]]);
	const maps = mapInfos([
		{ id: 2, name: "east", tile: tile(1, 0) },
		{ id: 1, name: "origin", tile: tile(0, 0) },
		{ id: 9, name: "lobby", tile: null },
	], edges);
	assert.deepEqual(maps, [
		{
			key: "gw-w", title: "Gridworld w", tile_size: 512,
			instances: [
				{ id: "1", name: "origin", bounds: [-256, -256, 256, 256] },
				{ id: "2", name: "east", bounds: [256, -256, 768, 256] },
			],
			edges: { nauvis: [[256, -256, 256, 256]] },
		},
		{ key: "i-1", title: "origin", instances: [{ id: "1", name: "origin" }] },
		{ key: "i-2", title: "east", instances: [{ id: "2", name: "east" }] },
		{ key: "i-9", title: "lobby", instances: [{ id: "9", name: "lobby" }] },
	]);
});

test("edgeLines: each side along its facing, endpoints ordered, coincident sides once, deleted edges skipped", () => {
	const side = (instanceId: number, origin: [number, number], facing: number) => ({ instanceId, surface: "nauvis", origin, facing });
	const lines = edgeLines([
		// gridworld's shared border: both sides on the same line, drawn from opposite ends
		{ deleted: false, length: 512, a: side(1, [256, -256], 4), b: side(2, [256, 256], 12) },
		{ deleted: true, length: 10, a: side(1, [0, 0], 0), b: side(2, [0, 0], 0) },
		{ deleted: false, length: 10, a: side(3, [0, 0], 8), b: side(3, [5, 5], 3) },
	], (id, surface) => (id === 3 ? `i-3` : "gw-w"));
	assert.deepEqual([...lines], [
		["gw-w|nauvis", [[256, -256, 256, 256]]],
		["i-3|nauvis", [[-10, 0, 0, 0]]],
	]);
});

test("ueApi: undefined without universal_edges or when its module does not load", async () => {
	assert.equal(await ueApi([]), undefined);
	assert.equal(await ueApi([{ name: "universal_edges", packagePath: path.join("nowhere", "package.json") }]), undefined);
});

test("findBinary: config, then TIMESHADEDB_BINARY, then the platform package, then bin/; a missing configured file is an error", () => {
	const files = new Set<string>();
	const exists = (f: string) => files.has(f);
	const pluginDir = path.resolve("plugin");
	const pkgDir = path.resolve("node_modules", "@timeshadedb", "bin-linux-x64");
	const resolve = (request: string) => {
		if (request !== "@timeshadedb/bin-linux-x64/package.json") {
			throw new Error("not found");
		}
		return path.join(pkgDir, "package.json");
	};
	const base = { pluginDir, platform: "linux", arch: "x64", exists, resolve, env: {} };
	assert.equal(binaryPackage("linux", "x64"), "@timeshadedb/bin-linux-x64");
	assert.equal(binaryName("win32"), "timeshadedb.exe");
	assert.match(String(findBinary(base)), /no timeshadedb binary for linux-x64/);
	const local = path.join(pluginDir, "bin", "timeshadedb");
	files.add(local);
	assert.deepEqual(findBinary(base), { path: local, source: "local" });
	const pkg = path.join(pkgDir, "bin", "timeshadedb");
	files.add(pkg);
	assert.deepEqual(findBinary(base), { path: pkg, source: "package" });
	const envBin = path.resolve("env-bin");
	assert.match(String(findBinary({ ...base, env: { TIMESHADEDB_BINARY: envBin } })), /TIMESHADEDB_BINARY .* does not exist/);
	files.add(envBin);
	assert.deepEqual(findBinary({ ...base, env: { TIMESHADEDB_BINARY: envBin } }), { path: envBin, source: "env" });
	const conf = path.resolve("conf-bin");
	assert.match(String(findBinary({ ...base, configured: conf })), /timeshade.binary_path .* does not exist/);
	files.add(conf);
	assert.deepEqual(findBinary({ ...base, configured: conf, env: { TIMESHADEDB_BINARY: envBin } }), { path: conf, source: "config" });
	// An uninstalled platform package falls through to bin/.
	assert.deepEqual(findBinary({ ...base, platform: "darwin", arch: "arm64" }), {
		path: local, source: "local",
	});
});
