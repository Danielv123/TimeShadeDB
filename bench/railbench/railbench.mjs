// Real-map train benchmark (bench/railbench/README.md).
//
//   node bench/railbench/railbench.mjs survey <save.zip>... [--ticks 18000] [--work <dir>]
//   node bench/railbench/railbench.mjs record <save.zip> --name <run> [--ticks 36000] [--refuel] [--work <dir>]
//   node bench/railbench/railbench.mjs feed --name <run> [--work <dir>]
//   node bench/railbench/railbench.mjs ups <save.zip> --name <run> [--pairs 3] [--ticks 3600] [--affinity FF00]
//
// Saves are loaded from where they are given (copies; railbench never writes to a save). Every run has its own
// sandbox under <work>/runs/<name>. See README.md for what each step writes.
import fs from "node:fs";
import path from "node:path";
import zlib from "node:zlib";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { copyTree, gameVersion, runFactorio, sandbox, setEnabled } from "./factorio.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(here, "..", "..");
const args = process.argv.slice(2);
const cmd = args[0];
const option = (name, fallback) => {
	const i = args.indexOf(name);
	return i >= 0 && args[i + 1] !== undefined ? args[i + 1] : fallback;
};
const flag = (name) => args.includes(name);
const positional = () => {
	const out = [];
	for (let i = 1; i < args.length; i++) {
		if (args[i].startsWith("--")) {
			if (!["--refuel", "--keep", "--no-capture"].includes(args[i])) {
				i++;
			}
			continue;
		}
		out.push(args[i]);
	}
	return out;
};
const work = path.resolve(option("--work", process.env.RAILBENCH_WORK ?? "G:/project_files/timeshade-v3-work/railbench"));

/** The save's version from level-init.dat (zip entry), and whether it needs the Space Age mods (2.x saves). */
function saveInfo(save) {
	const buf = fs.readFileSync(save);
	// minimal zip reader: central directory entries
	const eocd = buf.lastIndexOf(Buffer.from([0x50, 0x4b, 0x05, 0x06]));
	const count = buf.readUInt16LE(eocd + 10);
	let off = buf.readUInt32LE(eocd + 16);
	for (let i = 0; i < count; i++) {
		const method = buf.readUInt16LE(off + 10);
		const csize = buf.readUInt32LE(off + 20);
		const nlen = buf.readUInt16LE(off + 28);
		const xlen = buf.readUInt16LE(off + 30);
		const clen = buf.readUInt16LE(off + 32);
		const local = buf.readUInt32LE(off + 42);
		const name = buf.toString("utf8", off + 46, off + 46 + nlen);
		if (name.endsWith("level-init.dat")) {
			const lnlen = buf.readUInt16LE(local + 26);
			const lxlen = buf.readUInt16LE(local + 28);
			const data = buf.subarray(local + 30 + lnlen + lxlen, local + 30 + lnlen + lxlen + csize);
			const raw = method === 8 ? zlib.inflateRawSync(data) : data;
			const v = [raw.readUInt16LE(0), raw.readUInt16LE(2), raw.readUInt16LE(4)];
			return { version: v.join("."), spaceAge: v[0] >= 2 };
		}
		off += 46 + nlen + xlen + clen;
	}
	throw new Error(`${save}: no level-init.dat`);
}

const recorderMod = (dst) => copyTree(path.join(here, "mods", "railbench_recorder"), dst);

function slug(save) {
	return path.basename(save, ".zip").replace(/[^A-Za-z0-9_-]+/g, "_").slice(0, 40);
}

async function survey() {
	const ticks = Number(option("--ticks", 18000));
	const results = [];
	for (const save of positional()) {
		let info;
		try {
			info = saveInfo(save);
		} catch (e) {
			// saves older than 0.17 have no level-init.dat; the game itself reports whether it can load them
			info = { version: "unknown", spaceAge: false };
		}
		const name = "survey-" + slug(save);
		const dir = path.join(work, "runs", name);
		const box = sandbox(dir, { mods: { railbench_recorder: recorderMod }, spaceAge: info.spaceAge,
			configs: { railbench_recorder: { mode: "survey", ticks, refuel: flag("--refuel") } } });
		console.log(`== ${path.basename(save)} (saved by ${info.version}, ${info.spaceAge ? "Space Age mods" : "base only"})`);
		const res = await runFactorio(box, ["--benchmark", path.resolve(save), "--benchmark-ticks", String(ticks + 2),
			"--benchmark-runs", "1"], "survey");
		const m = /RBSURVEY (.*)/.exec(res.stdout);
		const perf = /Performed (\d+) updates in ([\d.]+) ms/.exec(res.stdout);
		if (!m) {
			const errs = res.stdout.split(/\r?\n/).filter(l => /Error|error|Failed|failed/.test(l)).slice(0, 8).join("\n");
			console.log(`  no survey (exit ${res.code}):\n${errs}`);
			results.push({ save: path.basename(save), version: info.version, error: errs || `exit ${res.code}` });
			continue;
		}
		const s = JSON.parse(m[1]);
		s.save = path.basename(save);
		s.version = info.version;
		s.ms_per_tick = perf ? Number(perf[2]) / Number(perf[1]) : null;
		results.push(s);
		console.log(`  trains ${s.trains} (automatic ${s.automatic}), moved > 100 tiles in ${ticks} ticks: ${s.moved} `
			+ `(automatic ${s.moved_auto}), ${s.km.toFixed(1)} train-km, no fuel ${s.no_fuel}, `
			+ `${s.ms_per_tick?.toFixed(2)} ms/tick`);
		console.log(`  carriages ${JSON.stringify(s.carriages)} states ${JSON.stringify(s.states)}`);
		console.log(`  rails ${JSON.stringify(s.rails)}`);
		fs.rmSync(path.join(dir, "write", "temp"), { recursive: true, force: true });
	}
	const outPath = path.join(work, `survey-${Date.now()}.json`);
	fs.writeFileSync(outPath, JSON.stringify(results, null, 2));
	console.log(`-> ${outPath}`);
}

/**
 * The capture's envelopes of a recorded run through the real v3 ingest: starts `timeshadedb serve --clock data` on
 * a free port with its own database under the run, posts every envelope of write/script-output/timeshade in name order
 * as `timeshadedb tail` does (gzip NDJSON with src, map, recv_ms, seq), except that recv_ms is the game time of the
 * envelope's tick plus a fixed lag (the benchmark ran faster than real time), then posts one trace envelope 7 minutes
 * later, which moves the data clock past every mover window, and an ended envelope (the game stopped, so the server
 * does not hold the map for the source), waits for the commit and stops the server.
 */
async function feed() {
	const name = option("--name");
	const dir = path.join(work, "runs", name);
	const capDir = path.join(dir, "write", "script-output", "timeshade");
	// --db names the database directory under the run (default db), so one run can be fed more than one way
	const dbName = option("--db", "db");
	const db = path.join(dir, dbName);
	const asSpeed = option("--speed", null);
	fs.rmSync(db, { recursive: true, force: true });
	const bin = option("--bin", path.join(work, "bin", "timeshadedb.exe"));
	const token = "railbench";
	const { spawn } = await import("node:child_process");
	const budget = option("--mem-budget", null);
	const srv = spawn(bin, ["serve", "--db", db, "--listen", "127.0.0.1:0", "--clock", "data", "--ingest-token", token,
		"--no-viewer", "--quiet", "--exit-on-stdin-close", ...(budget ? ["--mem-budget", String(Number(budget))] : [])],
	{ stdio: ["pipe", "pipe", "pipe"] });
	const logPath = path.join(dir, "serve.log");
	const log = fs.createWriteStream(logPath);
	srv.stderr.on("data", (d) => log.write(d));
	const addr = await new Promise((resolve, reject) => {
		let buf = "";
		srv.stdout.on("data", (d) => {
			log.write(d);
			buf += d.toString();
			const m = /listening on (\S+)/.exec(buf);
			if (m) {
				resolve(m[1]);
			}
		});
		srv.on("exit", (c) => reject(new Error(`serve exited ${c}, see ${logPath}`)));
	});
	console.log(`serve on ${addr}, db ${db}`);
	const url = `http://${addr}/api/v3/ingest`;
	const files = fs.readdirSync(capDir).filter(f => f.endsWith(".ndjson")).sort();
	const lagMs = Number(option("--lag-ms", 250));
	// the run's game time starts at an epoch chosen so that a UTC hour boundary falls inside the run: the hourly keyframe
	// is part of what the store keeps per hour
	let tick0 = null;
	let prevTick = null;
	let gameMs = 0;
	let base = null;
	let seq = 0;
	let posted = 0;
	let lastRecv = 0;
	let batch = [];
	let batchBytes = 0;
	const meta = { files: files.length, lagMs };
	async function post(lines) {
		const body = zlib.gzipSync(Buffer.from(lines.join("\n") + "\n"));
		for (let attempt = 0; ; attempt++) {
			const res = await fetch(url, { method: "POST", body, headers: { "Content-Type": "application/x-ndjson",
				"Content-Encoding": "gzip", Authorization: `Bearer ${token}` } });
			if (res.ok) {
				await res.arrayBuffer();
				return;
			}
			const text = await res.text();
			if (res.status >= 500 || res.status === 429) {
				if (attempt > 300) {
					throw new Error(`ingest ${res.status}: ${text}`);
				}
				await new Promise(r => setTimeout(r, Math.min(2000, 100 * (attempt + 1))));
				continue;
			}
			throw new Error(`ingest ${res.status}: ${text.slice(0, 300)}`);
		}
	}
	const runTicks = Number(JSON.parse(fs.readFileSync(path.join(dir, "run.json"), "utf8")).ticks ?? 36000);
	for (const f of files) {
		const text = fs.readFileSync(path.join(capDir, f), "utf8");
		for (let line of text.split("\n")) {
			if (line.length < 2 || line[0] !== "{") {
				continue;
			}
			const tick = Number(/"tick":(\d+)/.exec(line)?.[1]);
			// game time runs at the envelope's game speed (a save may run slower than 1, as the l1n3 save does at 0.2):
			// the tailer would see the envelopes arrive at that pace, and serve converts sample ticks with it
			// --speed replays the run as if the game had run at that speed (the capture only writes game.speed into the
			// envelope; serve and the viewer use it to put ticks on the clock)
			if (asSpeed !== null) {
				line = line.replace(/"speed":[\d.eE+-]+,/, "").replace('{"v":1,', `{"v":1,"speed":${Number(asSpeed)},`);
			}
			const speed = Number(/"speed":([\d.eE+-]+)/.exec(line)?.[1] ?? 1);
			if (tick0 === null) {
				tick0 = tick;
				prevTick = tick;
				const hour = Date.UTC(2026, 9, 1, 12, 0, 0);
				base = hour - Math.round((runTicks / 2) * 1000 / (60 * speed));
				meta.tick0 = tick0;
				meta.base = base;
				meta.speed = speed;
			}
			if (speed !== meta.speed) {
				meta.speedChanged = true;
			}
			gameMs += Math.max(0, tick - prevTick) * 1000 / (60 * speed);
			prevTick = Math.max(prevTick, tick);
			const recv = base + Math.round(gameMs) + lagMs;
			lastRecv = Math.max(lastRecv, recv);
			seq++;
			const env = `{"src":"standalone:railbench","map":"railbench","recv_ms":${recv},"seq":${seq},` + line.slice(1);
			batch.push(env);
			batchBytes += env.length;
			// one post per second of game time or 1 MiB, as the tailer posts about once a second
			if (batchBytes > (1 << 20) || recv - (batch.recv0 ?? recv) >= 1000) {
				await post(batch);
				posted += batch.length;
				batch = [];
				batchBytes = 0;
			}
			batch.recv0 ??= recv;
		}
	}
	if (batch.length) {
		await post(batch);
		posted += batch.length;
	}
	const endRecv = lastRecv + 7 * 60 * 1000;
	await post([`{"src":"standalone:railbench","map":"railbench","recv_ms":${endRecv},"seq":${seq + 1},"v":1,"tick":${0},`
		+ `"msgs":[{"type":"trace","op":"railbench.end","phase":"end"}]}`,
	`{"src":"standalone:railbench","map":"railbench","recv_ms":${endRecv},"seq":${seq + 2},"v":1,"tick":${0},`
		+ `"caught_up":true,"ended":true,"msgs":[]}`]);
	// wait until the closed windows cover the last envelope: meta.json's horizon passes it
	const want = lastRecv;
	const t0 = Date.now();
	let horizon = 0;
	while (Date.now() - t0 < 60000) {
		horizon = 0;
		const mdir = path.join(db, "movers");
		for (const s of fs.existsSync(mdir) ? fs.readdirSync(mdir) : []) {
			const mp = path.join(mdir, s, "meta.json");
			if (fs.existsSync(mp)) {
				horizon = Math.max(horizon, JSON.parse(fs.readFileSync(mp, "utf8")).horizon ?? 0);
			}
		}
		if (horizon > want) {
			break;
		}
		await new Promise(r => setTimeout(r, 500));
	}
	srv.stdin.end();
	await new Promise(r => srv.on("exit", r));
	meta.envelopes = posted;
	// milliseconds of map time per tick (eval.mjs puts ticks on the map clock with it)
	meta.msPerTick = meta.speedChanged ? null : 1000 / (60 * meta.speed);
	meta.lastRecv = lastRecv;
	meta.horizon = horizon;
	fs.writeFileSync(path.join(dir, dbName === "db" ? "feed.json" : `feed-${dbName}.json`), JSON.stringify(meta, null, 2));
	console.log(`posted ${posted} envelopes from ${files.length} files; mover horizon ${horizon} (last envelope ${lastRecv})`
		+ `${horizon > want ? "" : " NOT CLOSED"}`);
}

/**
 * The standalone capture mod as shipped (not the test build), built into dir from the capture directory `capture`
 * (default this checkout's). Returns its directory.
 */
function buildCapture(dir, capture = path.join(repo, "capture")) {
	const mm = gameVersion().split(".").slice(0, 2).join(".");
	execFileSync(process.execPath, [path.join(capture, "build.mjs"), "--out", dir, "--no-zip",
		"--factorio-version", mm], { encoding: "utf8" });
	const info = JSON.parse(fs.readFileSync(path.join(capture, "mod", "timeshadedb_exporter", "info.json"), "utf8"));
	const modDir = path.join(dir, `${info.name}_${info.version}`);
	// Benchmark mode has no server, but a save made in multiplayer keeps game.is_multiplayer() true, and the mod writes
	// its files only for the server (for_player = 0), so nothing would be written. The harness's copy writes them as
	// in a single-player game; nothing else changes.
	const control = path.join(modDir, "control.lua");
	const src = fs.readFileSync(control, "utf8");
	const patched = src.replace("game.is_multiplayer() and 0 or nil", "nil");
	if (patched === src) {
		throw new Error("buildCapture: the standalone mod's write_file call changed; update the benchmark-mode patch");
	}
	fs.writeFileSync(control, patched);
	return modDir;
}

/**
 * One recorded run: the save with the shipped capture mod and the recorder in the same game, in benchmark mode for
 * --ticks ticks (plus 240 so the capture's 2 s file buffer covers the recorded range). Writes <run>/run.json; the
 * ground truth is in <run>/write/script-output/railbench, the capture's envelopes in .../timeshade.
 */
async function record() {
	const save = positional()[0];
	const name = option("--name", "record-" + slug(save));
	const ticks = Number(option("--ticks", 36000));
	const info = saveInfo(save);
	const dir = path.join(work, "runs", name);
	fs.rmSync(path.join(dir, "write", "script-output"), { recursive: true, force: true });
	const capDir = buildCapture(path.join(dir, "build"));
	const mods = { railbench_recorder: recorderMod };
	if (!flag("--no-capture")) {
		mods.timeshadedb_exporter = (dst) => copyTree(capDir, dst);
	}
	const box = sandbox(dir, { mods, spaceAge: info.spaceAge,
		configs: { railbench_recorder: { mode: "record", ticks, refuel: flag("--refuel") } } });
	console.log(`== record ${path.basename(save)} -> ${dir}: ${ticks} ticks${flag("--refuel") ? ", refuel on" : ""}`);
	const res = await runFactorio(box, ["--benchmark", path.resolve(save), "--benchmark-ticks", String(ticks + 240),
		"--benchmark-runs", "1"], "record", { keepStdout: true });
	const perf = /Performed (\d+) updates in ([\d.]+) ms/.exec(res.stdout);
	const done = /RBRECORD done at tick (\d+)/.exec(res.stdout);
	const errs = res.stdout.split(/\r?\n/).filter(l => /Error/.test(l)).slice(0, 8);
	const out = path.join(box.write, "script-output");
	const sizes = {};
	for (const sub of ["railbench", "timeshade"]) {
		const d = path.join(out, sub);
		sizes[sub] = fs.existsSync(d) ? fs.readdirSync(d).reduce((n, f) => n + fs.statSync(path.join(d, f)).size, 0) : 0;
	}
	const meta = { save: path.basename(save), saveVersion: info.version, game: gameVersion(), ticks, refuel: flag("--refuel"),
		capture: !flag("--no-capture"), exit: res.code, wallMs: res.ms, msPerTick: perf ? Number(perf[2]) / Number(perf[1]) : null,
		recordedTo: done ? Number(done[1]) : null, bytes: sizes, errors: errs };
	fs.writeFileSync(path.join(dir, "run.json"), JSON.stringify(meta, null, 2));
	fs.rmSync(path.join(box.write, "temp"), { recursive: true, force: true });
	console.log(JSON.stringify(meta, null, 2));
}

/** Rows of a --benchmark-verbose output (tick, wholeUpdate, scriptUpdate, luaGarbageIncremental in ns). */
function parseVerbose(file) {
	const rows = [];
	let idx = null;
	for (const l of fs.readFileSync(file, "latin1").split(/\r?\n/)) {
		if (l.startsWith("tick,timestamp,wholeUpdate")) {
			const cols = l.split(",");
			idx = { whole: cols.indexOf("wholeUpdate"), script: cols.indexOf("scriptUpdate"), gc: cols.indexOf("luaGarbageIncremental") };
		} else if (idx && /^t\d+,/.test(l)) {
			const v = l.split(",");
			rows.push({ tick: Number(v[0].slice(1)), whole: Number(v[idx.whole]), script: Number(v[idx.script]), gc: Number(v[idx.gc]) });
		}
	}
	return rows;
}

/**
 * Capture UPS cost on a real save: the same save with and without the shipped capture mod (recorder off), interleaved
 * pairs, Factorio's --benchmark-verbose timings. Steady state is measured from --from ticks (default: after the load
 * resync, which the capture runs on every load) to the end; the resync window is reported apart. --compare <capture
 * dir> also runs another version of capture/ (an older checkout's) in each pair, in its own sandbox.
 */
async function ups() {
	const save = positional()[0];
	const name = option("--name", "ups-" + slug(save));
	const ticks = Number(option("--ticks", 7200));
	const from = Number(option("--from", 600));
	const pairs = Number(option("--pairs", 3));
	const affinity = option("--affinity", null);
	const info = saveInfo(save);
	const dir = path.join(work, "runs", name);
	const capDir = buildCapture(path.join(dir, "build"));
	const box = sandbox(dir, { mods: { timeshadedb_exporter: (dst) => copyTree(capDir, dst) }, spaceAge: info.spaceAge });
	const compare = option("--compare", null);
	let altBox = null;
	if (compare) {
		const altDir = buildCapture(path.join(dir, "build-alt"), path.resolve(compare));
		altBox = sandbox(path.join(dir, "alt"), { mods: { timeshadedb_exporter: (dst) => copyTree(altDir, dst) }, spaceAge: info.spaceAge });
		setEnabled(altBox, { timeshadedb_exporter: true });
	}
	const per = { with: [], without: [], alt: [] };
	const lines = [];
	for (let r = 0; r < pairs; r++) {
		for (const label of altBox ? ["without", "with", "alt"] : ["without", "with"]) {
			const b = label === "alt" ? altBox : box;
			if (label !== "alt") {
				setEnabled(box, { timeshadedb_exporter: label === "with" });
			}
			const res = await runFactorio(b, ["--benchmark", path.resolve(save), "--benchmark-ticks", String(ticks),
				"--benchmark-runs", "1", "--benchmark-verbose", "all"], `ups-${label}-${r}`, { keepStdout: false, affinity });
			const rows = parseVerbose(res.outPath);
			const t0 = rows.length ? rows[0].tick : 0;
			per[label].push(rows.map(x => ({ ...x, rel: x.tick - t0 })));
			fs.rmSync(path.join(b.write, "script-output"), { recursive: true, force: true });
			fs.rmSync(path.join(b.write, "temp"), { recursive: true, force: true });
		}
		const mean = (rows, f, lo, hi) => {
			const k = rows.filter(x => x.rel >= lo && x.rel < hi);
			return k.reduce((n, x) => n + f(x), 0) / Math.max(1, k.length);
		};
		const lua = (x) => (x.script + x.gc) / 1e6;
		const whole = (x) => x.whole / 1e6;
		const w = per.with[r];
		const wo = per.without[r];
		const line = { pair: r, luaWith: mean(w, lua, from, ticks), luaWithout: mean(wo, lua, from, ticks),
			wholeWith: mean(w, whole, from, ticks), wholeWithout: mean(wo, whole, from, ticks),
			luaWithFirst: mean(w, lua, 0, from), luaWithoutFirst: mean(wo, lua, 0, from) };
		line.luaDelta = line.luaWith - line.luaWithout;
		line.wholeDelta = line.wholeWith - line.wholeWithout;
		line.luaDeltaFirst = line.luaWithFirst - line.luaWithoutFirst;
		if (altBox) {
			const a = per.alt[r];
			line.luaAlt = mean(a, lua, from, ticks);
			line.luaDeltaAlt = line.luaAlt - line.luaWithout;
			line.wholeDeltaAlt = mean(a, whole, from, ticks) - line.wholeWithout;
		}
		lines.push(line);
		console.log(`pair ${r}: capture Lua ${line.luaDelta.toFixed(4)} ms/tick (ticks ${from}-${ticks}; baseline Lua `
			+ `${line.luaWithout.toFixed(4)}, whole tick ${line.wholeWithout.toFixed(3)} -> ${line.wholeWith.toFixed(3)}); `
			+ `first ${from} ticks (load resync) ${line.luaDeltaFirst.toFixed(4)}`
			+ `${altBox ? `; alt capture Lua ${line.luaDeltaAlt.toFixed(4)} ms/tick` : ""}`);
	}
	const res = { save: path.basename(save), ticks, from, pairs, affinity, game: gameVersion(), lines };
	fs.writeFileSync(path.join(dir, "ups.json"), JSON.stringify(res, null, 2));
	const avg = (k) => lines.reduce((n, l) => n + l[k], 0) / lines.length;
	console.log(`mean capture Lua cost ${avg("luaDelta").toFixed(4)} ms/tick, whole tick ${avg("wholeDelta").toFixed(4)} ms/tick`
		+ `${altBox ? `; alt ${avg("luaDeltaAlt").toFixed(4)} ms/tick, whole tick ${avg("wholeDeltaAlt").toFixed(4)}` : ""}`);
}

const commands = { survey, feed, record, ups };
if (!commands[cmd]) {
	console.error("usage: railbench.mjs survey|record|feed|ups ... (see the header)");
	process.exit(2);
}
console.log(`Factorio ${gameVersion()}, work ${work}`);
await commands[cmd]();
