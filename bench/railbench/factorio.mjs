// Headless Factorio sandboxes for the railbench harness: every run gets its own write-data directory, mods directory
// and mod list, so the owner's real Factorio config, mods and saves are never touched. Loads run in --benchmark mode
// (as fast as the machine allows, no network port).
import { spawn, execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";

export const factorioExe = () => process.env.FACTORIO_EXE
	?? "C:/Program Files (x86)/Steam/steamapps/common/Factorio/bin/x64/factorio.exe";

let versionCache = null;
/** "2.1.19" */
export function gameVersion() {
	if (!versionCache) {
		const out = execFileSync(factorioExe(), ["--version"], { encoding: "utf8" });
		versionCache = /Version: (\S+)/.exec(out)?.[1] ?? "unknown";
	}
	return versionCache;
}

export function copyTree(src, dst) {
	fs.mkdirSync(dst, { recursive: true });
	for (const entry of fs.readdirSync(src, { withFileTypes: true })) {
		const s = path.join(src, entry.name);
		const d = path.join(dst, entry.name);
		if (entry.isDirectory()) {
			copyTree(s, d);
		} else {
			fs.copyFileSync(s, d);
		}
	}
}

function luaValue(v) {
	if (typeof v === "string") {
		return JSON.stringify(v);
	}
	if (typeof v === "number" || typeof v === "boolean") {
		return String(v);
	}
	if (Array.isArray(v)) {
		return "{ " + v.map(luaValue).join(", ") + " }";
	}
	return "{ " + Object.entries(v).map(([k, x]) => `[${JSON.stringify(k)}] = ${luaValue(x)}`).join(", ") + " }";
}

/**
 * A sandbox in dir: config.ini (read-data from the install, write-data in dir/write), dir/mods with the given mods
 * (name -> function(dst) that fills the mod directory) and a mod list. spaceAge enables the Space Age mods that ship
 * with the game (needed by 2.0 saves made with them; 1.1 saves load with base only). configs: name -> object written as
 * the mod's config.lua.
 */
export function sandbox(dir, { mods = {}, spaceAge = false, configs = {} } = {}) {
	const write = path.join(dir, "write");
	const modsDir = path.join(dir, "mods");
	fs.mkdirSync(write, { recursive: true });
	fs.rmSync(modsDir, { recursive: true, force: true });
	fs.mkdirSync(modsDir, { recursive: true });
	const exeDir = path.dirname(factorioExe());
	fs.writeFileSync(path.join(dir, "config.ini"),
		`[path]\nread-data=${path.resolve(exeDir, "..", "..", "data").replace(/\\/g, "/")}\n`
		+ `write-data=${write.replace(/\\/g, "/")}\n`);
	const mm = gameVersion().split(".").slice(0, 2).join(".");
	const list = [
		{ name: "base", enabled: true },
		{ name: "elevated-rails", enabled: spaceAge },
		{ name: "quality", enabled: spaceAge },
		{ name: "space-age", enabled: spaceAge },
	];
	for (const [name, fill] of Object.entries(mods)) {
		const dst = path.join(modsDir, name);
		fill(dst);
		const infoPath = path.join(dst, "info.json");
		const info = JSON.parse(fs.readFileSync(infoPath, "utf8"));
		fs.writeFileSync(infoPath, JSON.stringify({ ...info, factorio_version: mm }, null, 2));
		if (configs[name]) {
			fs.writeFileSync(path.join(dst, "config.lua"), `return ${luaValue(configs[name])}\n`);
		}
		list.push({ name, enabled: true });
	}
	fs.writeFileSync(path.join(modsDir, "mod-list.json"), JSON.stringify({ mods: list }, null, "\t"));
	return { dir, write, modsDir, config: path.join(dir, "config.ini"), list };
}

/** Enable or disable mods of a sandbox by name. */
export function setEnabled(box, want) {
	box.list = box.list.map(m => (m.name in want ? { ...m, enabled: want[m.name] } : m));
	fs.writeFileSync(path.join(box.modsDir, "mod-list.json"), JSON.stringify({ mods: box.list }, null, "\t"));
}

/**
 * Runs factorio with the sandbox's config and mods. stdout goes to dir/<label>.stdout.txt; resolves with
 * { code, ms, stdout } where stdout is the text unless keepStdout is false (large benchmark-verbose output can be
 * parsed from the file instead).
 */
export function runFactorio(box, extra, label, { keepStdout = true, onLine = null, affinity = null } = {}) {
	return new Promise((resolve, reject) => {
		const t0 = Date.now();
		const outPath = path.join(box.dir, `${label}.stdout.txt`);
		const out = fs.createWriteStream(outPath);
		const args = ["--config", box.config, "--mod-directory", box.modsDir, "--disable-audio", ...extra];
		let child;
		if (affinity && process.platform === "win32") {
			// cmd start /affinity pins Factorio to the given cores (hex mask), as capture/README.md describes
			child = spawn("cmd", ["/c", "start", "", "/affinity", affinity, "/wait", "/b", factorioExe(), ...args],
				{ stdio: ["ignore", "pipe", "pipe"] });
		} else {
			child = spawn(factorioExe(), args, { stdio: ["ignore", "pipe", "pipe"] });
		}
		const chunks = [];
		let partial = "";
		child.stdout.on("data", (d) => {
			out.write(d);
			if (keepStdout) {
				chunks.push(d);
			}
			if (onLine) {
				const text = partial + d.toString("latin1");
				const lines = text.split(/\r?\n/);
				partial = lines.pop();
				for (const l of lines) {
					onLine(l);
				}
			}
		});
		child.stderr.on("data", (d) => out.write(d));
		child.on("error", reject);
		child.on("close", (code) => {
			out.end();
			resolve({ code, ms: Date.now() - t0, stdout: keepStdout ? Buffer.concat(chunks).toString("latin1") : null, outPath });
		});
	});
}
