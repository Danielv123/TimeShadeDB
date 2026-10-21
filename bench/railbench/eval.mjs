// Railbench evaluator: what the v3 viewer draws (web/src/movers, run headless) from the samples the v3 server stored,
// against the recorder's ground truth of the same game run, at every tick. See README.md for the metrics.
//
//   node bench/railbench/eval.mjs --run <name> [--work <dir>] [--modes history,live] [--batch-ms 1000]
//       [--extra-ms 150] [--live-estimator shared|journey] [--journeys N] [--out <file>]
//
// --live-estimator: the live model's delay estimator (web/src/movers/clock.ts) is shared by every train of a surface,
// as the viewer's is (default), or one per journey (each journey's live replay starts with no observations; the
// baseline of 2026-10-21 was measured that way).
//
// Diagnostics: RAILBENCH_WORST_CAR=<tiles> lists carriages further off than that in worst-*.json (default 5);
// RAILBENCH_DEBUG_DELAY=<tiles> prints the live delay every 10 s and writes debug-live.json with the head mode, rail
// run state and neighbouring samples of every live frame whose head is further off than that.
//
// Inputs under <work>/runs/<name>: write/script-output/railbench/gt-*.txt (ground truth), db/ (the v3 database `feed`
// filled), feed.json, run.json. The mover rows are read back from the database's closed windows with
// `measure movers` (bench/cmd/measure), so the evaluator sees exactly what the store keeps (1/16 tile positions, u16
// orientations, ticks, `car` rows).
import "../../web/src/movers/eval/ts-resolve.mjs";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { readGroundTruth } from "./gt.mjs";
import { RailIndex } from "./rails.mjs";
import { KINDS, K, STOPSTART_TICKS, prepareTrain, head, truthAt } from "./truth.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const { MoverView } = await import("../../web/src/movers/view.ts");
const { DelayEstimator } = await import("../../web/src/movers/clock.ts");
const { LiveLags } = await import("../../web/src/movers/feed.ts");
const { makeSample, headAt, newHead, railRuns, F_STOP, F_GONE, F_END_SWITCH, F_NO_HEADING, F_NO_TICK } = await import("../../web/src/movers/track.ts");
const debugLive = [];

const args = process.argv.slice(2);
const option = (name, fallback) => {
	const i = args.indexOf(name);
	return i >= 0 && args[i + 1] !== undefined ? args[i + 1] : fallback;
};
const work = path.resolve(option("--work", process.env.RAILBENCH_WORK ?? "G:/project_files/timeshade-v3-work/railbench"));
const name = option("--run");
const dir = path.join(work, "runs", name);
const modes = option("--modes", "history,live").split(",");
const batchMs = Number(option("--batch-ms", 1000));
const extraMs = Number(option("--extra-ms", 150));
const maxJourneys = Number(option("--journeys", 0));
const liveEstimator = option("--live-estimator", "shared");
// standing trains are scored every this many ticks (moving ones every tick), in a bucket of their own
const stoppedEvery = Number(option("--stopped-every", 10));
// seek and open modes: a session (a seek to a keyframe, an opening of the live map) every this much map time, each
// scored until the next
const sessionMs = Number(option("--session-ms", 60000));
const LIVE_TRAIL_MS = 60000; // core.LiveTrail: the live payload's trail
const LOOKAHEAD = 60000; // the feed's history lookahead (feed.ts LOOKAHEAD_MS)
const measureBin = option("--measure", path.join(work, "bin", "measure.exe"));

/** Histogram with fixed bins and an exact maximum. */
class Hist {
	constructor(width, max) {
		this.width = width;
		this.bins = new Float64Array(Math.ceil(max / width) + 1);
		this.n = 0;
		this.max = 0;
		this.sum = 0;
	}
	add(v) {
		this.bins[Math.min(this.bins.length - 1, Math.floor(v / this.width))]++;
		this.n++;
		this.sum += v;
		if (v > this.max) {
			this.max = v;
		}
	}
	q(p) {
		if (this.n === 0) {
			return null;
		}
		const target = p * this.n;
		let acc = 0;
		for (let i = 0; i < this.bins.length; i++) {
			acc += this.bins[i];
			if (acc >= target && this.bins[i] > 0) {
				return Math.min(this.max, (i + 1) * this.width);
			}
		}
		return this.max;
	}
	above(v) {
		let acc = 0;
		for (let i = Math.floor(v / this.width) + 1; i < this.bins.length; i++) {
			acc += this.bins[i];
		}
		return acc;
	}
	summary(digits = 3) {
		const r = (x) => (x === null ? null : Number(x.toFixed(digits)));
		return { n: this.n, p50: r(this.q(0.5)), p90: r(this.q(0.9)), p99: r(this.q(0.99)), max: r(this.max),
			mean: this.n ? r(this.sum / this.n) : null };
	}
}

// ---------------------------------------------------------------------------------------------------------------
// Inputs

const run = JSON.parse(fs.readFileSync(path.join(dir, "run.json"), "utf8"));
const dbName = option("--db", "db"); // the database under the run that feed filled (railbench.mjs feed --db)
const tag = dbName === "db" ? "" : `-${dbName}`;
const feedMeta = JSON.parse(fs.readFileSync(path.join(dir, `feed${tag}.json`), "utf8"));
console.log(`run ${name}: ${run.save}, ${run.ticks} ticks, game ${run.game}`);
let t = performance.now();
const gt = readGroundTruth(path.join(dir, "write", "script-output", "railbench"));
console.log(`ground truth: ${gt.trains.length} train records, ${gt.rails.length} rails, ticks ${gt.tick0}-${gt.lastTick} `
	+ `(${((performance.now() - t) / 1000).toFixed(1)} s)`);
const rails = new RailIndex(gt.rails);
for (const tr of gt.trains) {
	prepareTrain(tr, rails, Number(option("--stopstart-ticks", STOPSTART_TICKS)));
}
const byId = new Map();
for (const tr of gt.trains) {
	if (!byId.has(tr.id)) {
		byId.set(tr.id, []);
	}
	byId.get(tr.id).push(tr);
}

// mover rows from the database, per surface directory
const surfDirs = fs.readdirSync(path.join(dir, dbName, "movers")).filter(s => s !== "wal"
	&& fs.existsSync(path.join(dir, dbName, "movers", s, "windows")));
const journeys = [];
for (const s of surfDirs) {
	const out = path.join(dir, `journeys${tag}-${s}.json`);
	execFileSync(measureBin, ["movers", "-db", path.join(dir, dbName), "-surface", s, "-out", out], { encoding: "utf8" });
	const ex = JSON.parse(fs.readFileSync(out, "utf8"));
	const meta = JSON.parse(fs.readFileSync(path.join(dir, dbName, "movers", s, "meta.json"), "utf8"));
	const surfaceName = meta.key.surface;
	const sidx = Number(Object.entries(gt.surfaces).find(([, n]) => n === surfaceName)?.[0]);
	for (const j of ex.journeys ?? []) {
		journeys.push({ ...j, surfaceName, sidx });
	}
}
journeys.sort((a, b) => (a.id < b.id ? -1 : 1));
console.log(`database: ${journeys.length} train journeys on ${surfDirs.join(", ")}`);

// The clock: every stored row keeps its tick; the map time of a tick is fitted from the rows (T = a + tick * ms/tick).
// feed posts recv_ms = base + game time + lag, so the anchor should give a constant offset.
const msPerTick = feedMeta.msPerTick ?? 1000 / 60; // the game speed may be below 1 (feed.json)
const offs = [];
for (const j of journeys) {
	for (const r of j.rows) {
		if (r[1] !== null) {
			offs.push(r[0] - r[1] * msPerTick);
		}
	}
}
offs.sort((a, b) => a - b);
const a0 = offs[Math.floor(offs.length / 2)];
const clockResid = offs.length ? [offs[0] - a0, offs[offs.length - 1] - a0] : [0, 0];
console.log(`clock: T = ${a0.toFixed(1)} + tick * ${msPerTick.toFixed(4)} ms; residual ${clockResid[0].toFixed(1)}..${clockResid[1].toFixed(1)} ms`);
const tickOf = (ms) => (ms - a0) / msPerTick;
const msOf = (tick) => a0 + tick * msPerTick;

// ---------------------------------------------------------------------------------------------------------------
// Match journeys to ground-truth trains: by the journey id's train id, checked against the head position at each
// sample's tick (joins and splits keep the journey but change the train).

function matchRow(j, r) {
	const tick = r[1];
	if (tick === null || (r[5] & F_GONE)) {
		return null;
	}
	const idPart = Number(j.id.split(".").pop());
	const cands = [...(byId.get(idPart) ?? [])];
	const tryTrains = (list) => {
		let best = null;
		for (const tr of list) {
			if (tr.surface !== j.sidx) {
				continue;
			}
			const k = tr.at(tick);
			if (k < 0) {
				continue;
			}
			const h = head(tr, k);
			const d = Math.hypot(h.x - r[2], h.y - r[3]);
			if (d < 0.2 && (!best || d < best.d)) {
				best = { tr, k, d };
			}
		}
		return best;
	};
	return tryTrains(cands) ?? tryTrains(gt.trains);
}

// ---------------------------------------------------------------------------------------------------------------
// Sample statistics: samples and `car` rows per kind of track, against moving time and distance per kind.

const stats = {
	movingTicks: new Float64Array(KINDS.length), dist: new Float64Array(KINDS.length),
	samples: new Float64Array(KINDS.length), carRows: 0, rows: 0, gones: 0, unmatched: 0, stopRows: 0, endSwitch: 0,
	trainsMoving: 0, matchDist: new Hist(0.001, 1),
};
for (const tr of gt.trains) {
	let moved = false;
	for (let k = 0; k < tr.ticks.length; k++) {
		if (tr.speed[k] !== 0) {
			const kind = tr.kindOf(k);
			stats.movingTicks[kind]++;
			stats.dist[kind] += Math.abs(tr.speed[k]) / 1e5;
			moved = true;
		}
	}
	if (moved) {
		stats.trainsMoving++;
	}
}
const matched = new Map(); // journey id -> [{ row index, tr, k }]
for (const j of journeys) {
	const list = [];
	for (let i = 0; i < j.rows.length; i++) {
		const r = j.rows[i];
		stats.rows++;
		if (r[5] & F_GONE) {
			stats.gones++;
			continue;
		}
		if (r[7]) {
			stats.carRows++;
		}
		if (r[5] & F_STOP) {
			stats.stopRows++;
		}
		if (r[5] & F_END_SWITCH) {
			stats.endSwitch++;
		}
		const m = matchRow(j, r);
		if (!m) {
			stats.unmatched++;
			continue;
		}
		stats.matchDist.add(m.d);
		list.push({ i, tr: m.tr, k: m.k });
		const kind = m.tr.ticks[m.k] === r[1] && m.tr.speed[m.k] !== 0 ? m.tr.kindOf(m.k) : K.stopstart;
		stats.samples[kind]++;
	}
	matched.set(j.id, list);
}
const movingHours = stats.movingTicks.reduce((a, b) => a + b, 0) / 216000;
const km = stats.dist.reduce((a, b) => a + b, 0) / 1000;
const sampleTotal = stats.samples.reduce((a, b) => a + b, 0);
const sampleReport = {
	trains_moving: stats.trainsMoving, moving_train_hours: movingHours, train_km: km, rows: stats.rows, gones: stats.gones,
	samples: sampleTotal, unmatched: stats.unmatched, stop_rows: stats.stopRows, end_switch_rows: stats.endSwitch,
	car_rows: stats.carRows, samples_per_train_hour: sampleTotal / movingHours, samples_per_train_km: sampleTotal / km,
	car_rows_per_train_hour: stats.carRows / movingHours, match_distance: stats.matchDist.summary(4), by_kind: {},
};
for (const [i, k] of KINDS.entries()) {
	const h = stats.movingTicks[i] / 216000;
	const d = stats.dist[i] / 1000;
	sampleReport.by_kind[k] = { samples: stats.samples[i], moving_hours: h, km: d, share_of_time: h / movingHours,
		per_train_hour: h > 0 ? stats.samples[i] / h : null, per_train_km: d > 0 ? stats.samples[i] / d : null };
}
console.log(`samples: ${sampleTotal} (${stats.unmatched} unmatched) over ${movingHours.toFixed(2)} moving train-hours, ${km.toFixed(1)} train-km: `
	+ `${sampleReport.samples_per_train_hour.toFixed(0)}/train-hour, ${sampleReport.samples_per_train_km.toFixed(1)}/train-km; `
	+ `car rows ${stats.carRows} (${sampleReport.car_rows_per_train_hour.toFixed(0)}/train-hour)`);
for (const k of KINDS) {
	const b = sampleReport.by_kind[k];
	console.log(`  ${k.padEnd(9)} ${(100 * b.share_of_time).toFixed(1).padStart(5)}% of moving time: ${String(b.samples).padStart(7)} samples, `
		+ `${b.per_train_hour?.toFixed(0) ?? "-"}/train-hour, ${b.per_train_km?.toFixed(1) ?? "-"}/train-km`);
}

// ---------------------------------------------------------------------------------------------------------------
// Rendering against the truth

const q16 = (v) => Math.round(v * 16) / 16;

function newModeStats() {
	return {
		head: KINDS.map(() => new Hist(0.001, 200)), headAll: new Hist(0.001, 200),
		car: new Hist(0.001, 200), carAngle: new Hist(0.01, 180), spacing: new Hist(0.001, 100),
		carByKind: KINDS.map(() => new Hist(0.001, 200)),
		frames: 0, carFrames: 0, over1: 0, over2: 0, headOver1: 0, headOver2: 0, jumps: 0, carJumps: 0, notDrawn: 0,
		// the live model's display delay (now - drawn time), once a second of each live replay
		delay: new Hist(10, 60000),
		worst: [], worstCar: [],
	};
}

/** Fresh samples of journey j's rows [from, to) (a view keeps state in its samples, so each view gets its own). */
function samplesOf(j, from = 0, to = j.rows.length) {
	if (!j.metaList) {
		j.metaList = j.metas.map(([n, kinds, colour]) => ({ id: j.id, k: "t", f: j.force, name: j.name, n, o: kinds, c: colour || undefined }));
	}
	const out = [];
	for (let i = from; i < to; i++) {
		const [tt, tick, x, y, h, flags, mi, car, v, rp, ru] = j.rows[i];
		const meta = j.metaList[mi];
		const c = car ? car.map(([cx, cy, co]) => ({ x: cx, y: cy, o: co })) : null;
		// rail path rows (F_RAIL) also carry the speed, the rail pieces and the rail start
		const pieces = rp ? rp.map(([px, py, dir, kind]) => ({ x: px, y: py, dir, kind })) : null;
		out.push({ meta, s: makeSample(meta, tt, x, y, h === null ? NaN : h, flags, c, tick, v ?? NaN, pieces, !!ru) });
	}
	return out;
}

const truth = { c: new Float64Array(3 * 10000), n: 0, speed: 0, k: 0, moving: false };

function score(st, shown, tr, kind, prev) {
	st.frames++;
	if (!shown) {
		st.notDrawn++;
		return null;
	}
	const hx = shown.x;
	const hy = shown.y;
	const e = Math.hypot(hx - truth.c[0], hy - truth.c[1]);
	st.head[kind].add(e);
	st.headAll.add(e);
	if (e > 1) {
		st.headOver1++;
	}
	if (e > 2) {
		st.headOver2++;
	}
	const cars = shown.cars;
	const n = Math.min(cars.length, truth.n);
	let px = 0;
	let py = 0;
	let tpx = 0;
	let tpy = 0;
	let carJump = false;
	for (let i = 0; i < n; i++) {
		const c = cars[i];
		const tx = truth.c[3 * i];
		const ty = truth.c[3 * i + 1];
		const to = truth.c[3 * i + 2];
		const ce = Math.hypot(c.x - tx, c.y - ty);
		if (ce > Number(process.env.RAILBENCH_WORST_CAR ?? 5) && st.worstCar.length < 2000) {
			st.worstCar.push({ tick: Number(truth.tick.toFixed(2)), train: tr.id, j: truth.j, car: i, e: Number(ce.toFixed(2)), kind: KINDS[kind] });
		}
		st.car.add(ce);
		st.carByKind[kind].add(ce);
		st.carFrames++;
		if (ce > 1) {
			st.over1++;
		}
		if (ce > 2) {
			st.over2++;
		}
		// drawn angle (radians, screen coordinates) as an orientation in turns; compared as an axis (a carriage that
		// faces backwards has the opposite orientation in the game)
		const od = Math.atan2(Math.cos(c.a), -Math.sin(c.a)) / (2 * Math.PI);
		let d = Math.abs(od - to) % 0.5;
		d = Math.min(d, 0.5 - d);
		st.carAngle.add(d * 360);
		if (i > 0) {
			const drawn = Math.hypot(c.x - px, c.y - py);
			const tru = Math.hypot(tx - tpx, ty - tpy);
			st.spacing.add(Math.abs(drawn - tru));
		}
		if (prev && prev.cars && i < prev.cars.length / 2) {
			const lim = truth.speed + 2;
			if (Math.hypot(c.x - prev.cars[2 * i], c.y - prev.cars[2 * i + 1]) > lim) {
				carJump = true;
			}
		}
		px = c.x;
		py = c.y;
		tpx = tx;
		tpy = ty;
	}
	if (prev) {
		// a jump: the drawn head moved more than the true speed over one frame plus 2 tiles (as the WP11 evaluator,
		// with the true speed instead of the captured one)
		if (Math.hypot(hx - prev.x, hy - prev.y) > truth.speed * prev.dtTicks + 2) {
			st.jumps++;
		}
		if (carJump) {
			st.carJumps++;
		}
	}
	if (e > 2 && st.worst.length < 2000) {
		st.worst.push({ tick: Number(truth.tick.toFixed(2)), train: tr.id, j: truth.j, e: Number(e.toFixed(2)), kind: KINDS[kind] });
	}
	const flat = new Array(2 * cars.length);
	for (let i = 0; i < cars.length; i++) {
		flat[2 * i] = cars[i].x;
		flat[2 * i + 1] = cars[i].y;
	}
	return { x: hx, y: hy, cars: flat };
}

const report = { run: name, save: run.save, game: run.game, ticks: run.ticks, clock: { a0, residual_ms: clockResid },
	samples: sampleReport, live_estimator: liveEstimator, stopped_every_ticks: stoppedEvery, session_ms: sessionMs, modes: {} };
let list = journeys.filter(j => (matched.get(j.id) ?? []).length > 0);
if (maxJourneys > 0) {
	list = list.slice(0, maxJourneys);
}
/** The ground-truth train for a time of journey j: the one matched at the latest sample at or before it. */
function trainFinder(j) {
	const m = matched.get(j.id);
	const mt = m.map(x => j.rows[x.i][0]);
	return (ms) => {
		let lo = 0;
		let hi = m.length - 1;
		if (ms < mt[0]) {
			return m[0].tr;
		}
		while (lo < hi) {
			const mid = (lo + hi + 1) >> 1;
			if (mt[mid] <= ms) {
				lo = mid;
			} else {
				hi = mid - 1;
			}
		}
		return m[lo].tr;
	};
}

const lastMs = msOf(gt.lastTick);
/**
 * Journey j's span: its first row and its end (its gone, else the end of the recording: a parked train stays drawn from
 * its last row), and its truth finder.
 */
function info(j) {
	if (!j.info) {
		const last = j.rows[j.rows.length - 1];
		j.info = { firstT: j.rows.find(r => !(r[5] & F_GONE))?.[0], endT: last[5] & F_GONE ? last[0] : Math.max(last[0], lastMs),
			trainAt: trainFinder(j) };
	}
	return j.info;
}

/** Index of journey j's last row at or before t (-1: none). */
function rowAt(j, t) {
	let lo = 0;
	let hi = j.rows.length - 1;
	if (hi < 0 || j.rows[0][0] > t) {
		return -1;
	}
	while (lo < hi) {
		const mid = (lo + hi + 1) >> 1;
		if (j.rows[mid][0] <= t) {
			lo = mid;
		} else {
			hi = mid - 1;
		}
	}
	return lo;
}

/**
 * Scores the train drawn for journey state q at map time ms (fractional tick tau) against its truth: a moving frame in
 * st, a standing one (when `standing`: every --stopped-every ticks) in st.stop. shown() draws it. q.prev and q.prevS
 * keep the last scored frames for jumps.
 */
function scoreAt(st, q, ms, tau, dtTicks, standing, shown) {
	const tr = q.trainAt(ms);
	// (after the recording's last tick a train only looks standing)
	if (tau > gt.lastTick || (tr.gone !== null && tau >= tr.gone) || !truthAt(tr, tau, truth)) {
		q.prev = q.prevS = null;
		return;
	}
	truth.tick = tau;
	truth.j = q.id;
	if (truth.moving) {
		q.prevS = null;
		const p = score(st, shown(), tr, tr.kindOf(truth.k), q.prev);
		q.prev = p ? { ...p, dtTicks } : null;
		return;
	}
	q.prev = null;
	if (standing) {
		const p = score(st.stop, shown(), tr, K.stopstart, q.prevS);
		q.prevS = p ? { ...p, dtTicks: stoppedEvery } : null;
	}
}

/** History playback of journey j drawn from `samples` (every row known), scored at every tick in [fromT, toT). */
function playback(st, j, samples, fromT, toT) {
	const view = new MoverView();
	for (const x of samples) {
		view.add(x.s.meta, x.s);
	}
	const q = { id: j.id, trainAt: info(j).trainAt, prev: null, prevS: null, ms: 0 };
	const shown = () => view.frame(q.ms, Infinity, { bodies: true })[0];
	for (let tick = Math.ceil(tickOf(fromT)); msOf(tick) < toT; tick++) {
		q.ms = msOf(tick);
		scoreAt(st, q, q.ms, tick, 1, tick % stoppedEvery === 0, shown);
	}
}

/**
 * The live model with one view and one delay estimator for all journeys of a surface, as the viewer's MoverFeed: rows
 * arrive in batches and are added with the feed's own lag bookkeeping (feed.ts LiveLags), every train is drawn at the
 * delayed display time and scored against its truth. With `open` ({ at, until }) the viewer opens the live map at map
 * time `at`: it gets the live payload then (the rows that arrived within the last core.LiveTrail and each live
 * journey's last row; their lags do not count), then the rows arriving later, and is scored until `until`.
 */
function liveShared(st, js, open = null) {
	const view = new MoverView();
	const est = new DelayEstimator(2000); // as the viewer's (layer.ts)
	const lags = new LiveLags(view, est);
	const arrival = (ms) => Math.ceil(ms / batchMs) * batchMs + extraMs;
	const all = [];
	const payload = [];
	const per = new Map();
	let t0 = Infinity;
	let t1 = -Infinity;
	for (const j of js) {
		const q = info(j);
		if (open && (q.endT <= open.at || q.firstT >= open.until)) {
			continue;
		}
		per.set(j.id, { id: j.id, firstT: q.firstT, endT: q.endT, trainAt: q.trainAt, prev: null, prevS: null });
		t0 = Math.min(t0, q.firstT);
		t1 = Math.max(t1, q.endT);
		if (!open) {
			all.push(...samplesOf(j));
			continue;
		}
		// the rows that arrived before the live map opens: the trail, and the last row of a live (not gone) journey
		let k = j.rows.length;
		while (k > 0 && arrival(j.rows[k - 1][0]) > open.at) {
			k--;
		}
		let from = k;
		while (from > 0 && j.rows[from - 1][0] > open.at - LIVE_TRAIL_MS) {
			from--;
		}
		if (from === k && k > 0 && !(j.rows[k - 1][5] & F_GONE)) {
			from = k - 1;
		}
		payload.push(...samplesOf(j, from, k));
		let to = k;
		while (to < j.rows.length && j.rows[to][0] <= open.until + 20000) {
			to++;
		}
		all.push(...samplesOf(j, k, to));
	}
	all.sort((a, b) => arrival(a.s.t) - arrival(b.s.t) || a.s.t - b.s.t);
	payload.sort((a, b) => a.s.t - b.s.t);
	let now = t0 + 1000;
	let end = t1 + 20000;
	if (open) {
		now = open.at;
		end = open.until;
		for (const x of payload) {
			if (view.add(x.s.meta, x.s)) {
				lags.see(x.s.t, now, false, view.prevT);
			}
		}
	}
	let fed = 0;
	let perf = 0;
	const step = msPerTick;
	let tv = 0;
	let horizon = 0;
	let frame = null;
	const shownOf = (id) => () => {
		if (!frame) {
			frame = new Map(view.frame(tv, horizon, { bodies: true }).map(x => [x.id, x]));
		}
		return frame.get(id);
	};
	for (let n = 0; now < end; now += step, perf += step, n++) {
		while (fed < all.length && arrival(all[fed].s.t) <= now) {
			const x = all[fed];
			if (view.add(x.s.meta, x.s)) {
				lags.see(x.s.t, now, true, view.prevT);
			}
			fed++;
		}
		tv = now - est.delay(perf);
		horizon = est.horizon(now);
		if (Math.floor(perf / 1000) !== Math.floor((perf - step) / 1000)) {
			st.delay.add(now - tv);
		}
		if (Math.round(perf) % 10000 === 0) {
			view.trim(tv);
			if (process.env.RAILBENCH_DEBUG_DELAY) {
				console.log(`  delay ${est.current.toFixed(0)} ms, readiness p50 ${est.readyQuantile(0.5).toFixed(0)} p95 ${est.readyQuantile(0.95).toFixed(0)} p99 ${est.readyQuantile(0.99).toFixed(0)}, lag p99 ${est.quantile(0.99).toFixed(0)}`);
			}
		}
		// every frame draws every train (the viewer's frame), once something is scored
		frame = null;
		const tau = tickOf(tv);
		for (const [id, q] of per) {
			if (tv < q.firstT || tv >= q.endT) {
				q.prev = q.prevS = null;
				continue;
			}
			const shown = shownOf(id);
			scoreAt(st, q, tv, tau, step / msPerTick, n % stoppedEvery === 0, shown);
			if (process.env.RAILBENCH_DEBUG_DELAY && q.prev) {
				const sh = shown();
				if (sh && Math.hypot(sh.x - truth.c[0], sh.y - truth.c[1]) > Number(process.env.RAILBENCH_DEBUG_DELAY)) {
					const track = view.tracks.get(id);
					const h = headAt(track, tv, horizon, newHead());
					const nx = track.s[h.k + 1];
					debugLive.push({ train: q.trainAt(tv).id, tick: Math.round(tau), e: +Math.hypot(sh.x - truth.c[0], sh.y - truth.c[1]).toFixed(2),
						mode: h.mode, rail: !!h.rail, sinceSample: Math.round(tv - track.s[h.k].t), next: nx ? Math.round(nx.t - tv) : null,
						delay: Math.round(now - tv), drawnVsHead: +Math.hypot(sh.x - h.x, sh.y - h.y).toFixed(2),
						k: h.k, n: track.s.length, rr: (() => { const r = railRuns(track); return [r.run[h.k], r.run[h.k + 1], r.u[h.k], r.u[h.k + 1], r.ub[h.k]]; })(),
						a: { f: track.s[h.k].flags, v: track.s[h.k].v, rp: track.s[h.k].rp?.length, ru: track.s[h.k].ru, tick: track.s[h.k].tick },
						b: nx ? { f: nx.flags, v: nx.v, rp: nx.rp?.length, ru: nx.ru, tick: nx.tick } : null });
				}
			}
		}
	}
}

/** Sessions of the seek and open modes: one every sessionMs from the first row on, each until the next. */
function sessions(js) {
	let t0 = Infinity;
	let t1 = -Infinity;
	for (const j of js) {
		t0 = Math.min(t0, info(j).firstT);
		t1 = Math.max(t1, info(j).endT);
	}
	const out = [];
	for (let at = t0 + sessionMs; at < t1; at += sessionMs) {
		out.push({ at, until: Math.min(t1, at + sessionMs) });
	}
	return out;
}

const bySurface = new Map();
for (const j of list) {
	bySurface.set(j.surfaceName, [...(bySurface.get(j.surfaceName) ?? []), j]);
}

/** A mode's statistics as reported. */
function summarize(st) {
	const r = {
		frames: st.frames, not_drawn: st.notDrawn, head: st.headAll.summary(), head_by_kind: {},
		head_over_1: st.headOver1, head_over_2: st.headOver2,
		car_position: st.car.summary(), car_angle_deg: st.carAngle.summary(2), spacing: st.spacing.summary(),
		car_frames: st.carFrames, car_over_1: st.over1, car_over_2: st.over2, jumps: st.jumps, car_jumps: st.carJumps,
		car_by_kind: {}, display_delay_ms: st.delay.n ? st.delay.summary(0) : null,
	};
	for (const [i, k] of KINDS.entries()) {
		r.head_by_kind[k] = st.head[i].summary();
		r.car_by_kind[k] = st.carByKind[i].summary();
	}
	return r;
}

function print(label, r) {
	console.log(`  ${label}: ${r.frames} train-frames, ${r.not_drawn} not drawn`);
	if (!r.head.n) {
		return;
	}
	console.log(`    head error   ${fmt(r.head)}; > 1 tile ${r.head_over_1}, > 2 tiles ${r.head_over_2}`);
	for (const k of KINDS) {
		if (r.head_by_kind[k].n && label === "moving") {
			console.log(`      ${k.padEnd(9)} ${fmt(r.head_by_kind[k])}`);
		}
	}
	console.log(`    carriages   ${fmt(r.car_position)}; angle ${fmt(r.car_angle_deg, " deg")}; spacing ${fmt(r.spacing)}`);
	console.log(`    carriage-frames ${r.car_frames}: > 1 tile ${r.car_over_1} (${(100 * r.car_over_1 / r.car_frames).toFixed(3)}%), `
		+ `> 2 tiles ${r.car_over_2} (${(100 * r.car_over_2 / r.car_frames).toFixed(3)}%); jumps ${r.jumps}, carriage jumps ${r.car_jumps}`);
}

for (const mode of modes) {
	const st = newModeStats();
	st.stop = newModeStats();
	t = performance.now();
	if (mode === "history") {
		for (const j of list) {
			playback(st, j, samplesOf(j), info(j).firstT, info(j).endT);
		}
	} else if (mode === "seek") {
		// a seek to a keyframe at the session's start: each journey's last row at or before it (its keyframe row, unless
		// a gone), then the rows after it up to the history lookahead past the session
		for (const s of sessions(list)) {
			for (const j of list) {
				const q = info(j);
				if (q.endT <= s.at || q.firstT >= s.until) {
					continue;
				}
				let from = rowAt(j, s.at);
				if (from >= 0 && j.rows[from][5] & F_GONE) {
					from++;
				}
				from = Math.max(0, from);
				let to = from;
				while (to < j.rows.length && j.rows[to][0] <= s.until + LOOKAHEAD) {
					to++;
				}
				playback(st, j, samplesOf(j, from, to), Math.max(s.at, q.firstT), Math.min(s.until, q.endT));
			}
		}
	} else if (mode === "live" || mode === "open") {
		for (const js of bySurface.values()) {
			if (mode === "open") {
				for (const s of sessions(js)) {
					liveShared(st, js, s);
				}
			} else if (liveEstimator === "shared") {
				liveShared(st, js);
			} else {
				for (const j of js) {
					liveShared(st, [j]);
				}
			}
		}
	} else {
		throw new Error(`unknown mode ${mode}`);
	}
	const secs = (performance.now() - t) / 1000;
	const r = summarize(st);
	r.stopped = summarize(st.stop);
	report.modes[mode] = r;
	fs.writeFileSync(path.join(dir, `worst${tag}-${mode}.json`), JSON.stringify({ head: st.worst, carriages: st.worstCar,
		stopped: { head: st.stop.worst, carriages: st.stop.worstCar } }));
	console.log(`\n${mode} (${secs.toFixed(1)} s)`);
	print("moving", r);
	print("standing", r.stopped);
	if (r.display_delay_ms) {
		console.log(`  display delay ms ${fmt(r.display_delay_ms)}`);
	}
}

function fmt(s, unit = "") {
	return `p50 ${s.p50}${unit} p90 ${s.p90} p99 ${s.p99} max ${s.max}`;
}

if (debugLive.length) {
	fs.writeFileSync(path.join(dir, `debug-live${tag}.json`), JSON.stringify(debugLive));
}
const outPath = option("--out", path.join(dir, `eval${tag}.json`));
fs.writeFileSync(outPath, JSON.stringify(report, null, 2));
console.log(`\n-> ${outPath}`);
