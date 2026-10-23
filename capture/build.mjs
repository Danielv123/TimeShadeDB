// Assembles the two capture hosts from the shared library (no dependencies, Node 22+):
//   <out>/timeshadedb_exporter_<version>/      the standalone mod, unpacked (Factorio loads mod directories)
//   <out>/timeshadedb_exporter_<version>.zip   the same, zipped for the mod portal or a mods folder
//   <out>/clusterio_module/                    the Clusterio save-patch module "timeshade" (a plugin's module/ directory)
// Usage: node capture/build.mjs [--out <dir>] [--test] [--no-zip] [--factorio-version 2.1]
//   --test adds capture/test/game/{hooks,synthetic}.lua to the mod (a remote interface the game tests use); never ship
//   that build.
//   --factorio-version sets info.json's factorio_version in the output (default: the source's, 2.0). Factorio 2.1 refuses
//   mods declaring 2.0, and the library runs unchanged on both.
import fs from "node:fs";
import path from "node:path";
import zlib from "node:zlib";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const args = process.argv.slice(2);
const flag = (name) => args.includes(name);
const option = (name, fallback) => {
	const i = args.indexOf(name);
	return i >= 0 && args[i + 1] ? args[i + 1] : fallback;
};
const out = path.resolve(option("--out", path.join(here, "dist")));
const test = flag("--test");

const lib = path.join(here, "lua");
const modSrc = path.join(here, "mod", "timeshadedb_exporter");
const moduleSrc = path.join(here, "clusterio_module");

const info = JSON.parse(fs.readFileSync(path.join(modSrc, "info.json"), "utf8"));
const moduleInfo = JSON.parse(fs.readFileSync(path.join(moduleSrc, "module.json"), "utf8"));
const libVersion = /capture\.VERSION = "([^"]+)"/.exec(fs.readFileSync(path.join(lib, "capture.lua"), "utf8"))?.[1];
if (info.version !== moduleInfo.version || info.version !== libVersion) {
	throw new Error(`version mismatch: mod ${info.version}, module ${moduleInfo.version}, capture.lua ${libVersion}`);
}

function copyTree(src, dst) {
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

function libFiles() {
	return fs.readdirSync(lib).filter(f => f.endsWith(".lua")).sort();
}

// The mod.
const modName = `${info.name}_${info.version}`;
const modDir = path.join(out, modName);
fs.rmSync(modDir, { recursive: true, force: true });
copyTree(modSrc, modDir);
const fv = option("--factorio-version", null);
if (fv) {
	fs.writeFileSync(path.join(modDir, "info.json"), JSON.stringify({ ...info, factorio_version: fv }, null, 2) + "\n");
}
fs.mkdirSync(path.join(modDir, "lua"), { recursive: true });
for (const f of libFiles()) {
	fs.copyFileSync(path.join(lib, f), path.join(modDir, "lua", f));
}
// Ported from illisis/minimap under the Blue Oak Model License 1.0.0, whose notice rule asks that every copy carries it.
const licence = path.join(here, "LICENSE-BlueOak-1.0.0.md");
fs.copyFileSync(licence, path.join(modDir, "LICENSE-BlueOak-1.0.0.md"));
if (test) {
	fs.mkdirSync(path.join(modDir, "test"), { recursive: true });
	for (const f of ["hooks.lua", "synthetic.lua"]) {
		fs.copyFileSync(path.join(here, "test", "game", f), path.join(modDir, "test", f));
	}
	fs.appendFileSync(path.join(modDir, "control.lua"), '\nrequire("test.hooks")(capture)\n');
}

// The Clusterio module: its own files plus the library, flat, so the save holds modules/timeshade/<file>.lua.
const moduleDir = path.join(out, "clusterio_module");
fs.rmSync(moduleDir, { recursive: true, force: true });
copyTree(moduleSrc, moduleDir);
fs.copyFileSync(licence, path.join(moduleDir, "LICENSE-BlueOak-1.0.0.md"));
for (const f of libFiles()) {
	if (fs.existsSync(path.join(moduleDir, f))) {
		throw new Error(`library file ${f} would overwrite a module file`);
	}
	fs.copyFileSync(path.join(lib, f), path.join(moduleDir, f));
}

// A zip of the mod: entries under <name>_<version>/, deflated, with a CRC-32 each.
function zipDir(dir, prefix, zipPath) {
	const files = [];
	(function walk(d, rel) {
		for (const entry of fs.readdirSync(d, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
			const p = path.join(d, entry.name);
			const r = rel ? `${rel}/${entry.name}` : entry.name;
			if (entry.isDirectory()) {
				walk(p, r);
			} else {
				files.push([p, `${prefix}/${r}`]);
			}
		}
	})(dir, "");
	const locals = [];
	const centrals = [];
	let offset = 0;
	// a fixed timestamp (1980-01-01) keeps the zip reproducible
	const dosTime = 0;
	const dosDate = (0 << 9) | (1 << 5) | 1;
	for (const [p, name] of files) {
		const data = fs.readFileSync(p);
		const comp = zlib.deflateRawSync(data, { level: 9 });
		const crc = zlib.crc32(data);
		const nameBuf = Buffer.from(name, "utf8");
		const local = Buffer.alloc(30);
		local.writeUInt32LE(0x04034b50, 0);
		local.writeUInt16LE(20, 4);
		local.writeUInt16LE(0x0800, 6);
		local.writeUInt16LE(8, 8);
		local.writeUInt16LE(dosTime, 10);
		local.writeUInt16LE(dosDate, 12);
		local.writeUInt32LE(crc, 14);
		local.writeUInt32LE(comp.length, 18);
		local.writeUInt32LE(data.length, 22);
		local.writeUInt16LE(nameBuf.length, 26);
		local.writeUInt16LE(0, 28);
		locals.push(local, nameBuf, comp);
		const central = Buffer.alloc(46);
		central.writeUInt32LE(0x02014b50, 0);
		central.writeUInt16LE(20, 4);
		central.writeUInt16LE(20, 6);
		central.writeUInt16LE(0x0800, 8);
		central.writeUInt16LE(8, 10);
		central.writeUInt16LE(dosTime, 12);
		central.writeUInt16LE(dosDate, 14);
		central.writeUInt32LE(crc, 16);
		central.writeUInt32LE(comp.length, 20);
		central.writeUInt32LE(data.length, 24);
		central.writeUInt16LE(nameBuf.length, 28);
		central.writeUInt32LE(offset, 42);
		centrals.push(central, nameBuf);
		offset += 30 + nameBuf.length + comp.length;
	}
	const centralSize = centrals.reduce((n, b) => n + b.length, 0);
	const end = Buffer.alloc(22);
	end.writeUInt32LE(0x06054b50, 0);
	end.writeUInt16LE(files.length, 8);
	end.writeUInt16LE(files.length, 10);
	end.writeUInt32LE(centralSize, 12);
	end.writeUInt32LE(offset, 16);
	fs.writeFileSync(zipPath, Buffer.concat([...locals, ...centrals, end]));
	return files.length;
}

if (!flag("--no-zip")) {
	const n = zipDir(modDir, modName, path.join(out, `${modName}.zip`));
	console.log(`${path.join(out, modName + ".zip")} (${n} files)`);
}
console.log(modDir + (test ? " (with test hooks)" : ""));
console.log(moduleDir);
