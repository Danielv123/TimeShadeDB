import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import * as path from "node:path";
import loadControllerPlugin from "../controller.js";
import { syncIcons } from "../icons.js";
import { fakeControllerContext, tempDir } from "./fakes.js";

const fake = path.join(import.meta.dirname, "fake_sidecar.js");

/** Export files as Clusterio's upload route stores them; returns the manifest's assets. */
async function exportFiles(staticDir: string, hash: string) {
	await mkdir(staticDir, { recursive: true });
	const assets = { spritesheet: `spritesheet.${hash}.png`, metadata: `metadata.${hash}.json`, locale: `locale.${hash}.json` };
	await writeFile(path.join(staticDir, assets.spritesheet), `PNG ${hash}`);
	await writeFile(path.join(staticDir, assets.metadata), JSON.stringify({ item: [{ name: hash, icon: { x: 0, y: 0, size: 32 } }] }));
	await writeFile(path.join(staticDir, assets.locale), "{}");
	return assets;
}

const iconsJson = async (dir: string) => JSON.parse(await readFile(path.join(dir, "icons.json"), "utf8"));

test("syncIcons copies the spritesheet and metadata, names them in icons.json and removes the replaced files", async () => {
	const root = await tempDir("icons");
	const staticDir = path.join(root, "static");
	const icons = path.join(root, "db", "icons");
	const a = await exportFiles(staticDir, "aaaa");
	const b = await exportFiles(staticDir, "bbbb");

	assert.equal(await syncIcons(staticDir, icons, undefined), "none");
	assert.equal(await syncIcons(staticDir, icons, { locale: a.locale }), "none");
	await assert.rejects(readdir(icons), { code: "ENOENT" }, "nothing is written without an export");

	assert.equal(await syncIcons(staticDir, icons, a), "copied");
	assert.deepEqual(await iconsJson(icons), { png: `icons/${a.spritesheet}`, json: `icons/${a.metadata}` });
	assert.deepEqual((await readdir(icons)).sort(), ["icons.json", a.metadata, a.spritesheet].sort());
	assert.equal(await readFile(path.join(icons, a.spritesheet), "utf8"), "PNG aaaa");
	assert.equal(await syncIcons(staticDir, icons, a), "unchanged");

	assert.equal(await syncIcons(staticDir, icons, b), "copied");
	assert.deepEqual(await iconsJson(icons), { png: `icons/${b.spritesheet}`, json: `icons/${b.metadata}` });
	assert.deepEqual((await readdir(icons)).sort(), ["icons.json", b.metadata, b.spritesheet].sort());

	// A missing export file or a name the map server would not serve fails and leaves the icons as they were.
	await assert.rejects(syncIcons(staticDir, icons, { spritesheet: "spritesheet.cccc.png", metadata: b.metadata }), { code: "ENOENT" });
	await assert.rejects(syncIcons(staticDir, icons, { spritesheet: "../x.png", metadata: b.metadata }), /bad export file name/);
	assert.deepEqual(await iconsJson(icons), { png: `icons/${b.spritesheet}`, json: `icons/${b.metadata}` });
});

test("the controller copies the default mod pack's export icons at start and when the pack or the default changes", async () => {
	const staticDir = path.join(await tempDir("static"), "static");
	const a = await exportFiles(staticDir, "aaaa");
	const b = await exportFiles(staticDir, "bbbb");
	const c = await fakeControllerContext({ "controller.default_mod_pack_id": 1 });
	c.modPacks.set(1, { id: 1, exportManifest: { assets: a } });
	c.modPacks.set(2, { id: 2 });
	c.modPacks.set(3, { id: 3, exportManifest: { assets: b } });
	const plugin = await loadControllerPlugin(c.context, {
		command: process.execPath, commandArgs: [fake], sidecar: { minBackoffMs: 50 }, staticDir,
	});
	const icons = path.join(c.dir, "timeshade", "icons");
	try {
		await plugin.updateIcons();
		assert.equal((await iconsJson(icons)).png, `icons/${a.spritesheet}`);
		assert.ok(c.lines.some(l => l.message === "timeshade.icons copied" && l.meta.mod_pack === 1));

		// Another pack's export is not the default's.
		c.modPacks.set(3, { id: 3, exportManifest: { assets: { ...b } } });
		await c.call("modPacksUpdated", [c.modPacks.get(3)]);
		assert.equal((await iconsJson(icons)).png, `icons/${a.spritesheet}`);

		// A new export of the default pack replaces the icons.
		c.modPacks.set(1, { id: 1, exportManifest: { assets: b } });
		await c.call("modPacksUpdated", [c.modPacks.get(1)]);
		assert.equal((await iconsJson(icons)).png, `icons/${b.spritesheet}`);
		assert.ok(!(await readdir(icons)).includes(a.spritesheet));

		// A default without an export leaves them, quietly.
		c.controller.config.set("controller.default_mod_pack_id", 2);
		await c.call("controllerConfigFieldChanged", "controller.default_mod_pack_id", 2, 1);
		assert.equal((await iconsJson(icons)).png, `icons/${b.spritesheet}`);
		assert.ok(c.lines.some(l => l.message === "timeshade.icons none" && l.level === "verbose"));

		c.controller.config.set("controller.default_mod_pack_id", 1);
		c.modPacks.set(1, { id: 1, exportManifest: { assets: a } });
		await c.call("controllerConfigFieldChanged", "controller.default_mod_pack_id", 1, 2);
		assert.equal((await iconsJson(icons)).png, `icons/${a.spritesheet}`);

		// A new database directory gets them too.
		const other = path.join(c.dir, "elsewhere");
		c.controller.config.set("timeshade.database_directory", other);
		await c.call("controllerConfigFieldChanged", "timeshade.database_directory", other, null);
		assert.equal((await iconsJson(path.join(other, "icons"))).png, `icons/${a.spritesheet}`);
		assert.deepEqual(c.lines.filter(l => l.level === "warn" || l.level === "error"), []);
	} finally {
		await c.call("shutdown");
		await c.web.close();
	}
});

test("without a default mod pack or an export the controller writes no icons and warns about nothing", async () => {
	const c = await fakeControllerContext();
	const plugin = await loadControllerPlugin(c.context, {
		command: process.execPath, commandArgs: [fake], sidecar: { minBackoffMs: 50 }, staticDir: path.join(c.dir, "static"),
	});
	try {
		await plugin.updateIcons();
		c.controller.config.set("controller.default_mod_pack_id", 5);
		await c.call("controllerConfigFieldChanged", "controller.default_mod_pack_id", 5, null);
		c.modPacks.set(5, { id: 5 });
		await c.call("modPacksUpdated", [c.modPacks.get(5)]);
		await assert.rejects(readdir(path.join(c.dir, "timeshade", "icons")), { code: "ENOENT" });
		assert.deepEqual(c.lines.filter(l => l.message.startsWith("timeshade.icons") && l.level !== "verbose"), []);
		assert.deepEqual(c.lines.filter(l => l.level === "warn"), []);
	} finally {
		await c.call("shutdown");
		await c.web.close();
	}
});
