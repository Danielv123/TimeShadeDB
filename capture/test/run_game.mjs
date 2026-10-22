// Headless Factorio tests of the capture (docs/PROTOCOL.md section 6). Needs a Factorio 2.x install; nothing else.
//
//   node run_game.mjs [--mode test|clusterio|bench|all] [--factorio <factorio.exe>] [--keep] [--ticks N]
//   test options: [--write-fixture] (rewrite fixtures/game_envelopes.ndjson from this run)
//   bench options: [--runs N] [--trains N] [--charted N] [--chart-reads N] [--profile] [--micro]
//                  [--real-charts <illisis export dir>] [--compare <another capture/ dir>]
//
// test       the standalone mod (test build) and the driver mod run scenario.lua; the envelopes it wrote to
//            script-output/timeshade are validated line by line, chart bytes are recomputed, train samples are checked
//            against the true positions the driver logged.
// clusterio  the Clusterio module, loaded as a patched save loads it (with a stub Clusterio API), runs the same
//            scenario; its IPC lines on stdout are validated the same way.
// bench      the UPS benchmark: the same save with and without the capture mod, Factorio's --benchmark-verbose
//            timings compared (scriptUpdate + luaGarbageIncremental per tick).
//
// The Factorio executable comes from --factorio, $FACTORIO_EXE, or the default Steam path. Everything runs in a
// fresh temporary directory (its own write-data, mods and saves); --keep leaves it for inspection.
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import zlib from "node:zlib";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const captureDir = path.join(here, "..");
const args = process.argv.slice(2);
const option = (name, fallback) => {
	const i = args.indexOf(name);
	return i >= 0 && args[i + 1] ? args[i + 1] : fallback;
};
const flag = (name) => args.includes(name);
const modeArg = flag("--bench") ? "bench" : option("--mode", "all");
const keep = flag("--keep");
if (option("--real-charts", null) && option("--host", "mod") === "clusterio") {
	// the real charts are only wired into the standalone mod's test interface: the harness would bench generated ones
	console.log("ERROR: --real-charts only works with the standalone mod, not with --host clusterio");
	process.exit(2);
}
const factorio = option("--factorio", process.env.FACTORIO_EXE
	?? "C:/Program Files (x86)/Steam/steamapps/common/Factorio/bin/x64/factorio.exe");

if (!fs.existsSync(factorio)) {
	console.log(`SKIP: no Factorio at ${factorio} (set --factorio or FACTORIO_EXE)`);
	process.exit(2);
}

const root = fs.mkdtempSync(path.join(os.tmpdir(), "timeshade-capture-game-"));
const failures = [];
const notes = [];
function check(ok, message) {
	if (!ok) {
		failures.push(message);
		console.log(`  FAIL ${message}`);
	}
	return ok;
}
function note(message) {
	notes.push(message);
	console.log(`  ${message}`);
}

function version() {
	const out = execFileSync(factorio, ["--version"], { encoding: "utf8" });
	return /Version: (\S+)/.exec(out)?.[1] ?? "unknown";
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

/** A Factorio sandbox: its own config (write-data), mods directory and mod list. */
function sandbox(name, mods, config) {
	const dir = path.join(root, name);
	const write = path.join(dir, "write");
	const modsDir = path.join(dir, "mods");
	fs.mkdirSync(write, { recursive: true });
	fs.mkdirSync(modsDir, { recursive: true });
	const exeDir = path.dirname(factorio);
	fs.writeFileSync(path.join(dir, "config.ini"),
		`[path]\nread-data=${path.resolve(exeDir, "..", "..", "data").replace(/\\/g, "/")}\n`
		+ `write-data=${write.replace(/\\/g, "/")}\n`);
	const list = [
		{ name: "base", enabled: true },
		{ name: "elevated-rails", enabled: false },
		{ name: "quality", enabled: false },
		{ name: "space-age", enabled: false },
	];
	for (const [modName, src] of Object.entries(mods)) {
		const dst = path.join(modsDir, modName);
		src(dst);
		const infoPath = path.join(dst, "info.json");
		const info = JSON.parse(fs.readFileSync(infoPath, "utf8"));
		fs.writeFileSync(infoPath, JSON.stringify({ ...info, factorio_version: majorMinor() }, null, 2));
		fs.writeFileSync(path.join(dst, "config.lua"), `return ${luaTable(config)}\n`);
		list.push({ name: modName, enabled: true });
	}
	fs.writeFileSync(path.join(modsDir, "mod-list.json"), JSON.stringify({ mods: list }, null, "\t"));
	return { dir, write, modsDir, config: path.join(dir, "config.ini"), list };
}

/** Enable or disable mods: setEnabled(box, name, enabled) or setEnabled(box, { name: enabled, ... }). */
function setEnabled(box, modName, enabled) {
	const want = typeof modName === "string" ? { [modName]: enabled } : modName;
	const list = box.list.map(m => m.name in want ? { ...m, enabled: want[m.name] } : m);
	fs.writeFileSync(path.join(box.modsDir, "mod-list.json"), JSON.stringify({ mods: list }, null, "\t"));
}

function luaTable(v) {
	if (typeof v === "string") {
		return JSON.stringify(v);
	}
	if (typeof v === "number" || typeof v === "boolean") {
		return String(v);
	}
	return "{ " + Object.entries(v).map(([k, x]) => `${k} = ${luaTable(x)}`).join(", ") + " }";
}

function run(box, extra, label) {
	const t0 = Date.now();
	const res = spawnSync(factorio, ["--config", box.config, "--mod-directory", box.modsDir, "--disable-audio", ...extra],
		{ encoding: "latin1", maxBuffer: 1 << 30 });
	const ms = Date.now() - t0;
	fs.writeFileSync(path.join(box.dir, `${label}.stdout.txt`), res.stdout ?? "", "latin1");
	if (res.status !== 0 || /Error/.test(res.stdout?.split("\n").filter(l => !/Checksum|Loading/.test(l)).join("\n") ?? "")) {
		const errs = (res.stdout ?? "").split("\n").filter(l => /Error|error|TSTEST/.test(l)).slice(0, 20).join("\n");
		check(res.status === 0 && !/ Error /.test(res.stdout ?? ""), `${label}: factorio exited ${res.status}\n${errs}`);
	}
	return { stdout: res.stdout ?? "", ms };
}

let gameVersion = null;
/** major.minor of the game, for info.json's factorio_version (2.1 refuses mods that declare 2.0). */
function majorMinor() {
	return gameVersion.split(".").slice(0, 2).join(".");
}

function buildCapture(out) {
	execFileSync(process.execPath, [path.join(captureDir, "build.mjs"), "--out", out, "--test", "--no-zip",
		"--factorio-version", majorMinor()], { encoding: "utf8" });
	const info = JSON.parse(fs.readFileSync(path.join(captureDir, "mod", "timeshadedb_exporter", "info.json"), "utf8"));
	return { modDir: path.join(out, `${info.name}_${info.version}`), moduleDir: path.join(out, "clusterio_module"),
		version: info.version };
}

// Mirrors capture/test/game/synthetic.lua.
function syntheticChart(x, y, p) {
	const M = 2147483647;
	const v = (((x * 31 + y * 17 + p * 7) % 509) + 509) % 509;
	let seed = v * 7919 % (M - 1) + 1;
	const base = (v * 97 % 2048) * 32 + 7;
	const palette = [];
	for (let i = 1; i <= 6; i++) {
		palette.push((base + (i - 1) * 0x0841) % 65536);
	}
	const buf = Buffer.alloc(2048);
	let n = 0;
	while (n < 1024) {
		seed = seed * 16807 % M;
		const run = 1 + seed % 12;
		seed = seed * 16807 % M;
		const col = palette[seed % 6];
		for (let k = 0; k < run && n < 1024; k++) {
			buf.writeUInt16LE(col, 2 * n);
			n++;
		}
	}
	return buf;
}

/** Validate envelope lines; returns { envelopes, msgs } with each message's flush tick in msg.__tick. */
function validateLines(lines, label) {
	const envelopes = [];
	const msgs = [];
	let bad = 0;
	let maxLen = 0;
	for (const line of lines) {
		maxLen = Math.max(maxLen, Buffer.byteLength(line));
		let env;
		try {
			env = JSON.parse(line);
		} catch (err) {
			bad++;
			check(false, `${label}: not JSON: ${line.slice(0, 120)}`);
			continue;
		}
		const ok = env.v === 1 && Number.isInteger(env.tick) && Array.isArray(env.msgs) && env.msgs.length > 0
			&& (env.speed === undefined || typeof env.speed === "number");
		if (!check(ok, `${label}: bad envelope ${line.slice(0, 160)}`)) {
			bad++;
			continue;
		}
		envelopes.push(env);
		for (const m of env.msgs) {
			m.__tick = env.tick;
			msgs.push(m);
		}
	}
	return { envelopes, msgs, bad, maxLen };
}

function decodeChart(d) {
	return zlib.inflateSync(Buffer.from(d, "base64"));
}

/** Train lead per the capture's rule (front while speed > 0, back while < 0, the last end while stopped). */
function truthLeads(truthText) {
	const byTrain = {};
	for (const line of truthText.split("\n")) {
		if (!line) {
			continue;
		}
		const [tick, name, speed, fx, fy, bx, by] = line.split("\t");
		const tr = (byTrain[name] ??= { ticks: new Map(), fwd: true });
		const v = Number(speed);
		if (v !== 0) {
			tr.fwd = v > 0;
		}
		tr.ticks.set(Number(tick), { speed: v, fwd: tr.fwd, x: tr.fwd ? Number(fx) : Number(bx),
			y: tr.fwd ? Number(fy) : Number(by), fx: Number(fx), fy: Number(fy), bx: Number(bx), by: Number(by) });
	}
	return byTrain;
}

/**
 * What the viewer draws from a train's samples (web/src/movers/track.ts, history mode): the leading carriage's centre
 * at every tick from the first sample to the last, as a function of the tick (null where it draws nothing).
 */
async function viewerHeads(list) {
	await import("../../web/src/movers/eval/ts-resolve.mjs");
	const T = await import("../../web/src/movers/track.ts");
	const tr = new T.Track(list[0].j, { id: list[0].j, k: "t", f: list[0].f, name: list[0].name, n: list[0].n, o: list[0].o });
	for (const e of list) {
		// each sample's own carriages (a changed train has others)
		const meta = { id: e.j, k: "t", f: e.f, name: e.name, n: e.n, o: e.o };
		const flags = (e.stop ? T.F_STOP : 0) | (e.es ? T.F_END_SWITCH : 0) | (e.v !== undefined ? T.F_RAIL : 0);
		const car = e.car ? e.car.map(([x, y, o]) => ({ x, y, o })) : null;
		const rp = e.rp ? e.rp.map(([x, y, dir, kind]) => ({ x, y, dir, kind })) : null;
		tr.insert(T.makeSample(meta, (e.__tick * 1000) / 60, e.x, e.y, e.h ?? NaN, flags, car, e.__tick, e.v ?? NaN, rp, !!e.ru));
	}
	const head = T.newHead();
	return (tick) => {
		T.headAt(tr, (tick * 1000) / 60, Infinity, head);
		return head.visible ? { x: head.x, y: head.y, rail: head.rail !== null } : null;
	};
}

function entries(msgs, pred) {
	const out = [];
	for (const m of msgs) {
		if (m.type === "move") {
			for (const e of m.m) {
				if (pred(e)) {
					out.push({ ...e, __tick: e.tick ?? m.__tick, __s: m.s });
				}
			}
		}
	}
	return out;
}

/** The checks shared by the standalone and the Clusterio runs. */
async function checkScenario(label, msgs, results, truthText, { journeyPrefix }) {
	const spec = { x1: 0, y1: 0, x2: 20, y2: 20, flicker: { "3,3": 1, "4,4": 300 } };
	// chunks: every chart decodes to the bytes the seam produced at the flush tick
	const chunks = msgs.filter(m => m.type === "chunk");
	let badChart = 0;
	for (const c of chunks) {
		const period = spec.flicker[`${c.x},${c.y}`];
		const phase = period ? Math.floor(c.__tick / period) : 0;
		const raw = decodeChart(c.d);
		if (raw.length !== 2048 || !raw.equals(syntheticChart(c.x, c.y, phase))) {
			badChart++;
		}
	}
	check(chunks.length >= 400, `${label}: ${chunks.length} chunk messages (want at least the 400 charted chunks)`);
	check(badChart === 0, `${label}: ${badChart} chunk charts differ from the seam's bytes`);
	note(`${label}: ${chunks.length} chunk messages, every chart inflates to 2,048 bytes equal to the seam's`);
	// debounce: (3, 3) changes every tick for ~2000 ticks, (4, 4) every 300
	const hot = chunks.filter(c => c.x === 3 && c.y === 3);
	const warm = chunks.filter(c => c.x === 4 && c.y === 4);
	const bound = Math.floor(2000 / 600) + 1 + 1 + 1; // reads at most every 600 ticks, plus the boot and final resyncs
	check(hot.length <= bound, `${label}: chunk (3,3) flickering every tick sent ${hot.length} times (bound ${bound})`);
	note(`${label}: debounce: chunk (3,3) changed every tick for 2000 ticks, sent ${hot.length} times; `
		+ `(4,4) every 300 ticks, sent ${warm.length} times`);
	// resync "final": the 400 chunks, movers with car, resynced last
	const done = msgs.filter(m => m.type === "resynced");
	const final = done.find(m => m.trace === "final");
	check(final && final.counts.chunks === 400, `${label}: resynced "final" with 400 chunks: ${JSON.stringify(final)}`);
	check(final && final.counts.movers >= 2, `${label}: resync sent the movers: ${JSON.stringify(final?.counts)}`);
	// trains
	const truth = truthLeads(truthText);
	const aJ = entries(msgs, e => results.a_id && e.j && e.j.endsWith(`.${results.a_id}`) && e.k === "t");
	// B's journey keeps the id minted for its first train through the split
	const bId = results.b_id0 ?? results.b_gone_id;
	const bJ = entries(msgs, e => bId && e.j && e.j.endsWith(`.${bId}`));
	check(aJ.length > 0 && aJ[0].j.startsWith(journeyPrefix), `${label}: train A journey ${aJ[0]?.j} (prefix ${journeyPrefix})`);
	for (const [name, list] of [["A", aJ], ["B", bJ.filter(e => !e.gone)]]) {
		const tr = truth[name.toLowerCase()];
		if (!check(list.length > 0 && tr, `${label}: train ${name} has samples and truth`)) {
			continue;
		}
		let wrongPos = 0;
		let withTick = 0;
		for (const e of list) {
			const p = tr.ticks.get(e.__tick);
			if (e.tick !== undefined) {
				withTick++;
			}
			// a resync sample carries the flush tick; its position is the train's then
			if (!p || Math.hypot(p.x - e.x, p.y - e.y) > 0.01) {
				wrongPos++;
			}
		}
		check(wrongPos === 0, `${label}: train ${name}: ${wrongPos} of ${list.length} samples are not at the leading `
			+ `carriage's position at their tick`);
		// every sample is a rail path sample (speed, pieces) and what the viewer draws from them stays on the train at
		// every tick it moves (up to an end switch's sample the viewer still draws the old leading end)
		check(list.every(e => typeof e.v === "number"), `${label}: train ${name}: every sample carries the speed`);
		const pieces = list.reduce((n, e) => n + (e.rp ? e.rp.length : 0), 0);
		const moving = list.filter(e => !e.gone);
		const drawn = await viewerHeads(moving);
		let worst = 0;
		let worstTick = 0;
		let onRails = 0;
		let ticks = 0;
		const first = moving[0].__tick;
		const last = moving[moving.length - 1].__tick;
		for (let t = first; t <= last; t++) {
			const p = tr.ticks.get(t);
			// before an end switch's sample arrives the viewer cannot know the other end leads: it holds the old one
			const i = moving.findIndex(e => e.__tick > t);
			if (!p || p.speed === 0 || (i >= 0 && moving[i].es) || moving.some(e => e.es && e.__tick === t)) {
				continue;
			}
			const h = drawn(t);
			if (!h) {
				continue;
			}
			ticks++;
			onRails += h.rail ? 1 : 0;
			const d = Math.hypot(h.x - p.x, h.y - p.y);
			if (d > worst) {
				worst = d;
				worstTick = t;
			}
		}
		check(worst <= 0.5, `${label}: train ${name}: the viewer draws the head ${worst.toFixed(3)} tiles from the `
			+ `leading carriage at tick ${worstTick}`);
		const cars = list.filter(e => e.car);
		note(`${label}: train ${name}: ${list.length} samples (${withTick} with their own read tick, ${cars.length} with `
			+ `car, ${pieces} rail pieces); the viewer's head within ${worst.toFixed(3)} tiles of the leading carriage over `
			+ `${ticks} moving ticks (${onRails} on the rail path) from ${first} to ${last}`);
		for (const e of cars) {
			check(Array.isArray(e.car) && e.car.length === e.n && Math.abs(e.car[0][0] - e.x) < 0.01
				&& Math.abs(e.car[0][1] - e.y) < 0.01, `${label}: train ${name}: car starts at the leading carriage`);
		}
	}
	if (aJ.length > 0) {
		check(aJ[0].car && aJ[0].car.length === 3, `${label}: train A's first sample carries car`);
		const stops = aJ.filter(e => e.stop);
		check(stops.length >= 2 && stops.every(e => e.v === 0), `${label}: train A's stops (${stops.length}) have speed 0`);
		const es = aJ.filter(e => e.es);
		check(es.length === 1, `${label}: train A switched ends once (${es.length} es samples)`);
		if (es.length) {
			const p = truth.a.ticks.get(es[0].__tick);
			check(es[0].ru && p && Math.abs(es[0].x - p.bx) < 0.01, `${label}: after the switch the samples follow the back `
				+ `stock and begin a new rail path`);
			check(es[0].o === "ccl" && aJ[0].o === "LCC", `${label}: kinds from the leading end (${aJ[0].o}, then ${es[0].o})`);
		}
		check(aJ.every(e => e.n === 3 && e.name === (e.o === "LCC" ? "locomotive" : "cargo-wagon") && e.f === "player"
			&& typeof e.h === "number" && e.c), `${label}: train A sample fields (name of the leading carriage)`);
	}
	// B stood on a curve: its stop sample lists every curved piece under it and alone places the train there (a keyframe
	// or the live payload hold only that row); then its locomotive was taken off, and the wagon's next sample, read at
	// once, begins a new rail path with its carriages
	const bs = results.b_stop;
	if (check(bs && bs.under.length > 0, `${label}: train B stood on ${bs?.under.length} curved pieces before the split`)) {
		const KIND = { "curved-rail-a": 1, "curved-rail-b": 2, "elevated-curved-rail-a": 1, "elevated-curved-rail-b": 2, "rail-ramp": 4 };
		const stop = bJ.filter(e => e.stop && !e.ru && !e.gone && e.__tick <= bs.tick).pop();
		const listed = new Set((stop?.rp ?? []).map(p => p.join(",")));
		const missing = bs.under.filter(([x, y, dir, type]) => !listed.has([x, y, dir, KIND[type]].join(",")));
		check(stop && missing.length === 0, `${label}: train B's stop sample lists the ${bs.under.length} pieces under it `
			+ `(${missing.length} missing of ${stop?.rp?.length ?? 0} listed)`);
		if (stop) {
			await import("../../web/src/movers/eval/ts-resolve.mjs");
			const { MoverView } = await import("../../web/src/movers/view.ts");
			const T = await import("../../web/src/movers/track.ts");
			const view = new MoverView();
			const meta = { id: stop.j, k: "t", f: stop.f, name: stop.name, n: stop.n, o: stop.o };
			view.add(meta, T.makeSample(meta, (stop.__tick * 1000) / 60, stop.x, stop.y, stop.h, T.F_RAIL | T.F_STOP, null,
				stop.__tick, stop.v, stop.rp.map(([x, y, dir, kind]) => ({ x, y, dir, kind }))));
			const cars = view.frame((bs.tick * 1000) / 60, Infinity, { bodies: true })[0]?.cars ?? [];
			const off = cars.length === 2 ? Math.max(Math.hypot(cars[0].x - bs.front[0], cars[0].y - bs.front[1]),
				Math.hypot(cars[1].x - bs.back[0], cars[1].y - bs.back[1])) : Infinity;
			check(off < 0.3, `${label}: train B drawn from its stop sample alone: carriages within ${off.toFixed(3)} tiles`);
			note(`${label}: train B stood on ${bs.under.length} curved pieces; its stop sample at tick ${stop.__tick} lists `
				+ `${stop.rp.length} pieces and alone places both carriages within ${off.toFixed(3)} tiles; split at tick ${bs.tick}`);
		}
		const change = bJ.find(e => !e.gone && e.__tick > bs.tick);
		check(change && change.ru && change.car && change.car.length === bs.rest_n && change.n === bs.rest_n
			&& change.__tick <= bs.tick + 2, `${label}: after the split train B's next sample (tick ${change?.__tick}, split at `
			+ `${bs.tick}) begins a new rail path with car of ${change?.car?.length} carriages`);
	}
	const gone = bJ.filter(e => e.gone);
	check(gone.length === 1, `${label}: train B destroyed: one gone (${gone.length})`);
	// vehicles
	const car = entries(msgs, e => e.k === "v" && e.j.endsWith(`.v${results.car_unit}`));
	check(car.length >= 1 && car.every(e => e.o === "C" && typeof e.h === "number"), `${label}: car samples with heading `
		+ `(${car.length})`);
	// overlays
	const recipes = msgs.filter(m => m.type === "recipe");
	check(recipes.some(r => r.u === results.asm2 && r.r === "copper-cable"), `${label}: recipe of the built assembler`);
	check(recipes.some(r => r.r === undefined), `${label}: a recipe closed (assembler destroyed)`);
	const rails = msgs.filter(m => m.type === "rail");
	const built = rails.filter(r => r.y === -101 && r.name === "straight-rail" && r.dir === 4);
	check(built.length === 3 && rails.some(r => r.u === results.removed_rail && r.name === undefined),
		`${label}: rail layer: 3 built, 1 removed (${built.length} built at y = -101 of ${rails.length} rail messages)`);
	note(`${label}: rail layer: ${rails.length} rail messages (the resync sends the rails of each charted chunk)`);
	// the 2.0 API checks the driver made
	const c = results.checks;
	check(c.loop_closed, `${label}: the loop built through get_rail_extensions closed`);
	note(`${label}: rail planner used ${JSON.stringify(c.loop_rails)}; train B ran over ${JSON.stringify(results.b_rails)}`);
	note(`${label}: train API: ${JSON.stringify(c.train)}`);
	// train C under a schedule: LuaTrain.path is a LuaRailPath while it drives; the capture follows it like any train
	const p160 = c.path_auto_160 ?? {}, p400 = c.path_auto_400 ?? {};
	check(c.schedule_set === "LuaTrain.schedule", `${label}: schedule set through LuaTrain.schedule (${c.schedule_set})`);
	check(p160.path === "LuaRailPath" && p160.size > 0 && p160.total > 0,
		`${label}: a scheduled train's path is a LuaRailPath: ${JSON.stringify(p160)}`);
	note(`${label}: LuaTrain.path in automatic mode at tick 160 ${JSON.stringify(p160)}, at 400 ${JSON.stringify(p400)}`);
	const cJ = entries(msgs, e => results.c_id && e.j && e.j.endsWith(`.${results.c_id}`) && e.k === "t"
		&& e.x !== undefined);
	check(cJ.length >= 2 && cJ.every(e => Math.abs(e.y - 201) < 0.5) && cJ.some(e => e.x > 150),
		`${label}: the scheduled train's samples follow its line at y = 201 (${cJ.length} samples, last x `
		+ `${cJ.at(-1)?.x}; it ended at x ${results.c_x})`);
	note(`${label}: rail types: ${Object.keys(c.rail_types).sort().join(", ")}`);
	if (results.lua_kb) {
		note(`${label}: Lua memory of the capture's state at the end: ${results.lua_kb.toFixed(0)} KiB (stats ${JSON.stringify(results.stats)})`);
	}
}

const TEST_TICKS = Number(option("--ticks", 2200));

async function runTest() {
	console.log("== test: standalone mod");
	const build = buildCapture(path.join(root, "build"));
	const box = sandbox("test", {
		[`timeshadedb_exporter`]: (dst) => copyTree(build.modDir, dst),
		timeshade_capture_testdriver: (dst) => {
			copyTree(path.join(here, "game", "testdriver"), dst);
			for (const f of ["scenario.lua", "synthetic.lua"]) {
				fs.copyFileSync(path.join(here, "game", f), path.join(dst, f));
			}
		},
	}, { mode: "test", read_cost: 2150 });
	// the capture mod writes the config so the driver mod is not needed in it: drop the stray file
	fs.rmSync(path.join(box.modsDir, "timeshadedb_exporter", "config.lua"));
	const save = path.join(box.dir, "test.zip");
	run(box, ["--create", save], "create");
	const res = run(box, ["--benchmark", save, "--benchmark-ticks", String(TEST_TICKS)], "benchmark");
	const outDir = path.join(box.write, "script-output");
	const files = fs.existsSync(path.join(outDir, "timeshade"))
		? fs.readdirSync(path.join(outDir, "timeshade")).filter(f => f.endsWith(".ndjson")).sort() : [];
	check(files.length > 0, "test: capture files written to script-output/timeshade");
	check(files.every(f => /^\d{10}-\d{10}\.ndjson$/.test(f)), `test: file names <seq>-<tick>.ndjson: ${files.join(", ")}`);
	const lines = files.flatMap(f => fs.readFileSync(path.join(outDir, "timeshade", f), "utf8").split("\n")
		.filter(l => l.length));
	const { envelopes, msgs, bad, maxLen } = validateLines(lines, "test");
	note(`test: ${files.length} files, ${lines.length} lines, ${envelopes.length} valid envelopes, ${bad} bad, longest line `
		+ `${maxLen} bytes, ${msgs.length} messages`);
	// the standalone mod's files take lines up to 200,000 bytes (core.limits in its control.lua)
	check(maxLen < 200000, `test: every line under 200,000 bytes (longest ${maxLen})`);
	const resultsPath = path.join(outDir, "timeshade-test", "results.json");
	if (!check(fs.existsSync(resultsPath), "test: the driver wrote results.json")) {
		return;
	}
	const results = JSON.parse(fs.readFileSync(resultsPath, "utf8"));
	const truth = fs.readFileSync(path.join(outDir, "timeshade-test", "truth.tsv"), "utf8");
	await checkScenario("test", msgs, results, truth, { journeyPrefix: "0." });
	const cost = /TSTEST read_cost 1000 reads: .*?([\d.]+) ?ms/.exec(res.stdout);
	const costBytes = /TSTEST read_cost_bytes (\d+)/.exec(res.stdout);
	if (check(cost, "test: read_cost logged")) {
		note(`test: building a 2,048-byte chart and helpers.encode_string on it costs ${Number(cost[1]).toFixed(1)} µs `
			+ `per read (1,000 reads; ${costBytes ? Math.round(costBytes[1] / 1000) : "?"} bytes encoded per chart)`);
	}
	const types = new Set(msgs.map(m => m.type));
	note(`test: message types seen: ${[...types].sort().join(", ")}`);
	if (flag("--write-fixture")) {
		writeFixture(lines, results);
	}
}

/**
 * --write-fixture: rewrite fixtures/game_envelopes.ndjson (the Go decode test's input) from this run: the first line,
 * then each line that shows something no earlier pick did (a message type, a sample flag, a vehicle, a gone, the
 * scheduled train), in game order.
 */
function writeFixture(lines, results) {
	const seen = new Set();
	const picked = [];
	lines.forEach((line, i) => {
		const features = new Set();
		for (const m of JSON.parse(line).msgs) {
			features.add(`type:${m.type}`);
			if (m.type === "chunk" && m.d === undefined) {
				features.add("deleted");
			}
			for (const e of m.type === "move" ? m.m : []) {
				for (const f of ["car", "es", "stop", "tick", "gone"]) {
					if (e[f] !== undefined) {
						features.add(`${e.k ?? ""}:${f}`);
					}
				}
				features.add(`kind:${e.k ?? "gone"}`);
				if (results.c_id && e.j?.endsWith(`.${results.c_id}`)) {
					features.add("scheduled train");
				}
			}
		}
		if (i === 0 || [...features].some(f => !seen.has(f))) {
			picked.push(line);
			features.forEach(f => seen.add(f));
		}
	});
	const out = path.join(here, "fixtures", "game_envelopes.ndjson");
	fs.writeFileSync(out, picked.map(l => l + "\n").join(""));
	note(`test: wrote ${picked.length} of ${lines.length} lines to ${out} (${[...seen].sort().join(", ")})`);
}

async function runClusterio() {
	console.log("== clusterio: save-patch module with a stub Clusterio API");
	const build = buildCapture(path.join(root, "build-clusterio"));
	const box = sandbox("clusterio", {
		timeshade_clusterio_harness: (dst) => {
			copyTree(path.join(here, "game", "clusterio_harness"), dst);
			copyTree(build.moduleDir, path.join(dst, "modules", "timeshade"));
			for (const f of ["scenario.lua", "synthetic.lua"]) {
				fs.copyFileSync(path.join(here, "game", f), path.join(dst, f));
			}
		},
	}, { mode: "test" });
	const save = path.join(box.dir, "clusterio.zip");
	run(box, ["--create", save], "create");
	const res = run(box, ["--benchmark", save, "--benchmark-ticks", String(TEST_TICKS)], "benchmark");
	const FRAME = "\f$ipc:timeshade:out?j";
	const raw = res.stdout.split(/\r?\n/).filter(l => l.startsWith(FRAME));
	const lines = raw.map(l => l.slice(FRAME.length));
	const { envelopes, msgs, bad, maxLen } = validateLines(lines, "clusterio");
	note(`clusterio: ${raw.length} IPC lines on stdout, ${envelopes.length} valid envelopes, ${bad} bad, longest payload `
		+ `${maxLen} bytes`);
	check(raw.length > 0, "clusterio: IPC lines printed");
	check(maxLen < 65536, `clusterio: every line under 64 KiB (longest ${maxLen})`);
	const outDir = path.join(box.write, "script-output");
	const resultsPath = path.join(outDir, "timeshade-test", "results.json");
	if (!check(fs.existsSync(resultsPath), "clusterio: the driver wrote results.json")) {
		return;
	}
	check(!fs.existsSync(path.join(outDir, "timeshade")), "clusterio: the module writes no files");
	const results = JSON.parse(fs.readFileSync(resultsPath, "utf8"));
	const truth = fs.readFileSync(path.join(outDir, "timeshade-test", "truth.tsv"), "utf8");
	await checkScenario("clusterio", msgs, results, truth, { journeyPrefix: "4242." });
	const start = msgs.find(m => m.type === "resynced" && m.trace === "start-1");
	check(start, "clusterio: the start resync finished");
}

/**
 * `max` distinct 2,048-byte charts from an illisis/minimap export (--real-charts <dir>): the whole-block records
 * (a chunk's full chart) of the level-0 change files (MMC1: "MMC1", then per record u32 dt, u8 block, u16 n, and
 * n = 0 means 1,024 RGB565 LE pixels follow, else n x (u16 pos, u16 colour)). The export is only read as data. Returns
 * a Lua module source: a list of helpers.encode_string-format strings (zlib, base64) for helpers.decode_string.
 */
function realCharts(dir, max = 509, scan = 20000) {
	const seen = new Set();
	const out = [];
	const walk = (d, depth) => {
		for (const entry of fs.readdirSync(d, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
			if (out.length >= scan) {
				return;
			}
			const p = path.join(d, entry.name);
			if (entry.isDirectory() && depth < 6) {
				walk(p, depth + 1);
			} else if (entry.isFile() && entry.name.endsWith(".c.gz") && /[\\/]0[\\/][-\d]+_[-\d]+$/.test(d)) {
				// untrusted data: cap the inflated size (a level-0 change file is far smaller) and skip what exceeds it
				let b;
				try {
					b = zlib.gunzipSync(fs.readFileSync(p), { maxOutputLength: 64 * 1024 * 1024 });
				} catch {
					continue;
				}
				if (b.subarray(0, 4).toString("latin1") !== "MMC1") {
					continue;
				}
				for (let o = 4; o + 7 <= b.length && out.length < scan;) {
					const n = b.readUInt16LE(o + 5);
					o += 7;
					if (n === 0) {
						const chart = b.subarray(o, o + 2048);
						o += 2048;
						const key = chart.toString("base64");
						if (chart.length === 2048 && !seen.has(key) && chart.some(v => v !== 0)) {
							seen.add(key);
							out.push(zlib.deflateSync(chart).toString("base64"));
						}
					} else {
						o += 4 * n;
					}
				}
			}
		}
	};
	walk(dir, 0);
	// every k-th of the first `scan` found, so the sample spreads over the export's maps, surfaces and areas
	const step = Math.max(1, Math.floor(out.length / max));
	const pick = out.filter((_, i) => i % step === 0).slice(0, max);
	return "return {\n" + pick.map(s => `\t"${s}",\n`).join("") + "}\n";
}

/** Rows of every run in a --benchmark-verbose output (one header per run). */
function parseVerbose(stdout) {
	const lines = stdout.split(/\r?\n/);
	const rows = [];
	let idx = null;
	for (const l of lines) {
		if (l.startsWith("tick,timestamp,wholeUpdate")) {
			const cols = l.split(",");
			idx = (n) => cols.indexOf(n);
		} else if (idx && /^t\d+,/.test(l)) {
			const v = l.split(",");
			rows.push({ tick: Number(v[0].slice(1)), whole: Number(v[idx("wholeUpdate")]),
				script: Number(v[idx("scriptUpdate")]), gc: Number(v[idx("luaGarbageIncremental")]) });
		}
	}
	return rows.length ? rows : null;
}

function stats(values) {
	const s = [...values].sort((a, b) => a - b);
	const mean = s.reduce((a, b) => a + b, 0) / s.length;
	const q = (p) => s[Math.min(s.length - 1, Math.floor(p * s.length))];
	return { mean, p50: q(0.5), p99: q(0.99), max: s[s.length - 1] };
}

function runBench() {
	const trains = Number(option("--trains", 100));
	const ticks = Number(option("--bench-ticks", 4800));
	const warm = 600;
	// steady state from warm to resyncTick, then a resync over the generated chunks (about 1,800) to the end
	const resyncTick = ticks - 1200;
	console.log(`== bench: ${trains} moving trains, 50,176 virtual charted chunks, ${ticks} ticks (first ${warm} skipped), `
		+ `a resync at ${resyncTick}`);
	const build = buildCapture(path.join(root, "build-bench"));
	const cfg = { mode: "bench", trains, chunks: 50176, charted: Number(option("--charted", 20)), report_tick: ticks - 10,
		resync_tick: resyncTick, profile: flag("--profile") ? resyncTick - 1 : false, profile_start: warm, micro: flag("--micro") ? 700 : false,
		chart_reads: Number(option("--chart-reads", 1)), real_charts: Boolean(option("--real-charts", null)) };
	const realLua = cfg.real_charts ? realCharts(option("--real-charts")) : null;
	// --host clusterio benches the Clusterio module in the harness (one Lua state, as in a patched save, lines printed to
	// stdout) against the same harness with the module not loaded; the default benches the standalone mod
	const host = option("--host", "mod");
	const driver = (dst) => {
		copyTree(path.join(here, "game", "testdriver"), dst);
		for (const f of ["scenario.lua", "synthetic.lua"]) {
			fs.copyFileSync(path.join(here, "game", f), path.join(dst, f));
		}
	};
	const harness = (moduleDir) => (dst) => {
		copyTree(path.join(here, "game", "clusterio_harness"), dst);
		copyTree(moduleDir, path.join(dst, "modules", "timeshade"));
		for (const f of ["scenario.lua", "synthetic.lua"]) {
			fs.copyFileSync(path.join(here, "game", f), path.join(dst, f));
		}
	};
	/** The sandboxes for one capture build: returns [[label, box, before()], ...] for the runs without and with it. */
	const boxes = (name, modDir, moduleDir, withoutToo) => {
		if (host === "clusterio") {
			const out = [];
			const withBox = sandbox(name, { timeshade_clusterio_harness: harness(moduleDir) }, cfg);
			if (withoutToo) {
				const nullBox = sandbox(`${name}-null`, { timeshade_clusterio_harness: harness(moduleDir) },
					{ ...cfg, no_capture: true });
				out.push(["without", nullBox, () => {}]);
			}
			out.push(["with", withBox, () => {}]);
			return out;
		}
		const box = sandbox(name, {
			timeshadedb_exporter: (dst) => {
				copyTree(modDir, dst);
				if (realLua) {
					fs.writeFileSync(path.join(dst, "test", "real_charts.lua"), realLua);
				}
			},
			timeshade_capture_testdriver: driver,
			// the baseline's stand-in for the capture's test interface: the driver's remote calls cost the same in both
			timeshade_capture_null: (dst) => copyTree(path.join(here, "game", "nullcapture"), dst),
		}, cfg);
		fs.rmSync(path.join(box.modsDir, "timeshadedb_exporter", "config.lua"));
		fs.rmSync(path.join(box.modsDir, "timeshade_capture_null", "config.lua"));
		setEnabled(box, { timeshade_capture_null: false }); // both define the test interface
		const out = [];
		if (withoutToo) {
			out.push(["without", box, () => setEnabled(box, { timeshadedb_exporter: false, timeshade_capture_null: true })]);
		}
		out.push(["with", box, () => setEnabled(box, { timeshadedb_exporter: true, timeshade_capture_null: false })]);
		return out;
	};
	const variants = boxes("bench", build.modDir, build.moduleDir, true);
	const box = variants.find(v => v[0] === "with")[1];
	const save = path.join(box.dir, "bench.zip");
	// --compare <capture dir>: another version of the capture (an older checkout's capture/), built the same way and run
	// on the same map in its own sandbox, interleaved with the others, so both versions see the same machine load. Each
	// version gets its own save of the map (the same seed), made with it, since an older capture refuses a newer one's
	// storage schema.
	const compareDir = option("--compare", null);
	const seed = compareDir ? ["--map-gen-seed", "20261021"] : [];
	run(box, ["--create", save, ...seed], "create");
	if (compareDir) {
		const altOut = path.join(root, "build-alt");
		execFileSync(process.execPath, [path.join(compareDir, "build.mjs"), "--out", altOut, "--test", "--no-zip",
			"--factorio-version", majorMinor()], { encoding: "utf8" });
		const altInfo = JSON.parse(fs.readFileSync(path.join(compareDir, "mod", "timeshadedb_exporter", "info.json"), "utf8"));
		const [alt] = boxes("bench-alt", path.join(altOut, `${altInfo.name}_${altInfo.version}`),
			path.join(altOut, "clusterio_module"), false);
		alt[1].save = path.join(alt[1].dir, "bench.zip");
		run(alt[1], ["--create", alt[1].save, ...seed], "create-alt");
		variants.push(["alt", alt[1], alt[2]]);
		note(`bench: comparing with the capture in ${compareDir} ("alt")`);
	}
	note(`bench: host ${host === "clusterio" ? "Clusterio module (harness, stdout)" : "standalone mod (files)"}`);
	const runs = Number(option("--runs", 3));
	const result = {};
	// interleaved (without, with[, alt], without, with[, alt], ...) so a burst of load on the machine hits each alike
	const per = { without: [], with: [], alt: [] };
	const pairs = [];
	for (let r = 0; r < runs; r++) {
		for (const [label, vbox, before] of variants) {
			before();
			const res = run(vbox, ["--benchmark", vbox.save ?? save, "--benchmark-ticks", String(ticks), "--benchmark-verbose", "all"],
				`bench-${label}-${r}`);
			if (label === "with" && r === 0 && host === "clusterio") {
				const ipc = res.stdout.split(/\r?\n/).filter(l => l.startsWith("\f$ipc:timeshade:out?j"));
				const bytes = ipc.reduce((n, l) => n + l.length + 1, 0);
				note(`bench: ${ipc.length} IPC lines, ${(bytes / (ticks / 60)).toFixed(0)} bytes per game second printed`);
			}
			if (label === "with" && r === 0) {
				for (const m of res.stdout.matchAll(/TSTEST (?:prof|micro) (.*)/g)) {
					note(`bench profile (ticks ${warm}-${resyncTick - 1}): ${m[1].trim()}`);
				}
			}
			const rows = parseVerbose(res.stdout);
			if (!check(rows && rows.length > warm, `bench ${label}: verbose timings parsed`)) {
				return;
			}
			const kept = rows.filter(x => x.tick >= warm);
			per[label].push(...kept);
			pairs[r] = { ...pairs[r], [label]: kept.filter(x => x.tick < resyncTick) };
		}
	}
	// each interleaved pair alone: a baseline that moves between pairs means other jobs shared the machine
	const meanOf = (rows, f) => rows.reduce((n, x) => n + f(x), 0) / rows.length;
	const baselines = [];
	for (const [r, p] of pairs.entries()) {
		const lua = (x) => (x.script + x.gc) / 1e6;
		const base = meanOf(p.without, x => x.whole / 1e6);
		baselines.push(base);
		note(`bench pair ${r}: capture Lua cost ${(meanOf(p.with, lua) - meanOf(p.without, lua)).toFixed(4)} ms/tick `
			+ (p.alt ? `(alt ${(meanOf(p.alt, lua) - meanOf(p.without, lua)).toFixed(4)}) ` : "")
			+ `(baseline Lua ${meanOf(p.without, lua).toFixed(4)} ms, whole tick ${base.toFixed(4)} ms)`);
	}
	if (compareDir) {
		const steadyOf = (label) => per[label].filter(x => x.tick < resyncTick);
		const luaMean = (label) => meanOf(steadyOf(label), x => (x.script + x.gc) / 1e6);
		const wholeMean = (label) => meanOf(steadyOf(label), x => x.whole / 1e6);
		note(`bench compare: capture Lua cost ${(luaMean("with") - luaMean("without")).toFixed(4)} ms/tick, alt `
			+ `${(luaMean("alt") - luaMean("without")).toFixed(4)}; whole tick ${(wholeMean("with") - wholeMean("without"))
				.toFixed(4)}, alt ${(wholeMean("alt") - wholeMean("without")).toFixed(4)}`);
	}
	if (Math.max(...baselines) > 1.2 * Math.min(...baselines)) {
		note(`WARN bench: the baseline's whole tick moved from ${Math.min(...baselines).toFixed(3)} to `
			+ `${Math.max(...baselines).toFixed(3)} ms between pairs: the machine was busy, the costs are inflated`);
	}
	for (const label of ["without", "with"]) {
		const steady = per[label].filter(x => x.tick < resyncTick);
		const resync = per[label].filter(x => x.tick >= resyncTick && x.tick < resyncTick + 400);
		result[label] = {
			lua: stats(steady.map(x => (x.script + x.gc) / 1e6)),
			script: stats(steady.map(x => x.script / 1e6)),
			gc: stats(steady.map(x => x.gc / 1e6)),
			whole: stats(steady.map(x => x.whole / 1e6)),
			resyncLua: stats(resync.map(x => (x.script + x.gc) / 1e6)),
		};
	}
	const benchJson = path.join(box.write, "script-output", "timeshade-test", "bench.json");
	const fmt = (s) => `mean ${s.mean.toFixed(4)} ms, p50 ${s.p50.toFixed(4)}, p99 ${s.p99.toFixed(4)}, max ${s.max.toFixed(3)}`;
	note(`bench without capture: Lua ${fmt(result.without.lua)}; whole tick ${fmt(result.without.whole)}`);
	note(`bench with capture:    Lua ${fmt(result.with.lua)}; whole tick ${fmt(result.with.whole)}`);
	note(`bench: scriptUpdate mean ${result.without.script.mean.toFixed(4)} -> ${result.with.script.mean.toFixed(4)} ms, `
		+ `luaGarbageIncremental mean ${result.without.gc.mean.toFixed(4)} -> ${result.with.gc.mean.toFixed(4)} ms`);
	const delta = result.with.lua.mean - result.without.lua.mean;
	const deltaP50 = result.with.lua.p50 - result.without.lua.p50;
	note(`bench: capture costs ${deltaP50.toFixed(4)} ms/tick of Lua time by the medians`);
	const deltaWhole = result.with.whole.mean - result.without.whole.mean;
	note(`bench: capture costs ${delta.toFixed(4)} ms/tick of Lua time (scriptUpdate + luaGarbageIncremental, mean), `
		+ `${deltaWhole.toFixed(4)} ms/tick of the whole tick; budget 0.3 ms`);
	if (fs.existsSync(benchJson)) {
		note(`bench: capture stats at the end ${fs.readFileSync(benchJson, "utf8")}`);
	}
	if (host !== "clusterio") {
		const files = fs.readdirSync(path.join(box.write, "script-output", "timeshade")).sort();
		const bytes = files.reduce((n, f) => n + fs.statSync(path.join(box.write, "script-output", "timeshade", f)).size, 0);
		note(`bench: ${files.length} files, ${(bytes / runs / (ticks / 60)).toFixed(0)} bytes per game second written`);
	}
	const deltaResync = result.with.resyncLua.mean - result.without.resyncLua.mean;
	note(`bench: during a resync (400 ticks from tick ${resyncTick}): Lua ${fmt(result.with.resyncLua)} with, `
		+ `${fmt(result.without.resyncLua)} without; capture costs ${deltaResync.toFixed(4)} ms/tick`);
	// The budget (PLAN D9) is for the steady state; both measures of it must hold. A resync is a transient (start and
	// reconnect, about 3.5 min per 50,000 chunks): reported against the budget, not failed.
	const BUDGET = 0.3;
	check(delta <= BUDGET, `bench: capture Lua cost ${delta.toFixed(4)} ms/tick within the ${BUDGET} ms budget`);
	check(deltaWhole <= BUDGET, `bench: capture whole-tick cost ${deltaWhole.toFixed(4)} ms/tick within the ${BUDGET} ms `
		+ `budget`);
	if (deltaResync > BUDGET) {
		note(`WARN bench: a resync costs ${deltaResync.toFixed(4)} ms/tick while it runs, over the ${BUDGET} ms steady-state `
			+ `budget`);
	}
	const spikes = result.with.lua.max - result.without.lua.p50;
	if (spikes > 1) {
		note(`WARN bench: the slowest tick with the capture takes ${result.with.lua.max.toFixed(3)} ms of Lua time `
			+ `(p99 ${result.with.lua.p99.toFixed(3)} ms): the standalone mod's buffered file write (every 120 ticks, about `
			+ `1.3 ms on Windows), the mover scan (every 30 ticks), garbage collection and table growth`);
	}
}

gameVersion = version();
console.log(`Factorio ${gameVersion} at ${factorio}; sandbox ${root}`);
try {
	if (modeArg === "test" || modeArg === "all") {
		await runTest();
	}
	if (modeArg === "clusterio" || modeArg === "all") {
		await runClusterio();
	}
	if (modeArg === "bench") {
		runBench();
	}
} finally {
	if (!keep) {
		fs.rmSync(root, { recursive: true, force: true });
	}
}
console.log(failures.length === 0 ? `PASS (${notes.length} notes)` : `FAIL: ${failures.length} checks failed`);
process.exit(failures.length === 0 ? 0 : 1);
