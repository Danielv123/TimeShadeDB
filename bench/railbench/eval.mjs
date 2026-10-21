// Railbench evaluator: what the v3 viewer draws (web/src/movers, run headless) from the samples the v3 server stored,
// against the recorder's ground truth of the same game run, at every tick. See README.md for the metrics.
//
//   node bench/railbench/eval.mjs --run <name> [--work <dir>] [--modes history,live] [--batch-ms 1000]
//       [--extra-ms 150] [--journeys N] [--out <file>]
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
const { makeSample, F_STOP, F_GONE, F_END_SWITCH, F_NO_HEADING, F_NO_TICK } = await import("../../web/src/movers/track.ts");

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
		worst: [], worstCar: [],
	};
}

function sessionFor(j) {
	const view = new MoverView();
	const metas = j.metas.map(([n, kinds, colour]) => ({ id: j.id, k: "t", f: j.force, name: j.name, n, o: kinds, c: colour || undefined }));
	const samples = j.rows.map((r) => {
		const [tt, tick, x, y, h, flags, mi, car] = r;
		const meta = metas[mi];
		const c = car ? car.map(([cx, cy, co]) => ({ x: cx, y: cy, o: co })) : null;
		return { meta, s: makeSample(meta, tt, x, y, h === null ? NaN : h, flags, c, tick) };
	});
	return { view, samples };
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
		if (ce > 5 && st.worstCar.length < 2000) {
			st.worstCar.push({ tick: Number(truth.tick.toFixed(2)), train: tr.id, car: i, e: Number(ce.toFixed(2)), kind: KINDS[kind] });
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
		st.worst.push({ tick: Number(truth.tick.toFixed(2)), train: tr.id, e: Number(e.toFixed(2)), kind: KINDS[kind] });
	}
	const flat = new Array(2 * cars.length);
	for (let i = 0; i < cars.length; i++) {
		flat[2 * i] = cars[i].x;
		flat[2 * i + 1] = cars[i].y;
	}
	return { x: hx, y: hy, cars: flat };
}

const report = { run: name, save: run.save, game: run.game, ticks: run.ticks, clock: { a0, residual_ms: clockResid },
	samples: sampleReport, modes: {} };
let list = journeys.filter(j => (matched.get(j.id) ?? []).length > 0);
if (maxJourneys > 0) {
	list = list.slice(0, maxJourneys);
}
for (const mode of modes) {
	const st = newModeStats();
	t = performance.now();
	for (const j of list) {
		const m = matched.get(j.id);
		const { view, samples } = sessionFor(j);
		const firstT = samples.find(x => !(x.s.flags & F_GONE))?.s.t;
		const lastT = samples[samples.length - 1].s.t;
		// the ground-truth train for a time: the one matched at the latest sample at or before it
		const mt = m.map(x => j.rows[x.i][0]);
		const trainAt = (ms) => {
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
		let prev = null;
		if (mode === "history") {
			for (const x of samples) {
				view.add(x.s.meta, x.s);
			}
			const tick0 = Math.ceil(tickOf(firstT));
			const tick1 = Math.floor(tickOf(lastT));
			for (let tick = tick0; tick <= tick1; tick++) {
				const ms = msOf(tick);
				const tr = trainAt(ms);
				if (!truthAt(tr, tick, truth) || !truth.moving) {
					prev = null;
					continue;
				}
				truth.tick = tick;
				const shown = view.frame(ms, Infinity, { bodies: true })[0];
				const kind = tr.kindOf(truth.k);
				const p = score(st, shown, tr, kind, prev);
				prev = p ? { ...p, dtTicks: 1 } : null;
			}
		} else {
			const est = new DelayEstimator(2000);
			let fed = 0;
			const arrival = (ms) => Math.ceil(ms / batchMs) * batchMs + extraMs;
			let perf = 0;
			const step = msPerTick;
			for (let now = firstT + 1000; now <= lastT + 20000; now += step, perf += step) {
				while (fed < samples.length && arrival(samples[fed].s.t) <= now) {
					const x = samples[fed];
					if (view.add(x.s.meta, x.s) && !Number.isNaN(view.prevT)) {
						est.observeReady(now - view.prevT);
					}
					est.observe(now - x.s.t);
					fed++;
				}
				const tv = now - est.delay(perf);
				const horizon = est.horizon(now);
				if (Math.round(perf) % 10000 === 0) {
					view.trim(tv);
				}
				if (tv < firstT || tv > lastT) {
					prev = null;
					continue;
				}
				const tau = tickOf(tv);
				const tr = trainAt(tv);
				if (!truthAt(tr, tau, truth) || !truth.moving) {
					prev = null;
					continue;
				}
				truth.tick = tau;
				const shown = view.frame(tv, horizon, { bodies: true })[0];
				const kind = tr.kindOf(truth.k);
				const p = score(st, shown, tr, kind, prev);
				prev = p ? { ...p, dtTicks: step / msPerTick } : null;
			}
		}
	}
	const secs = (performance.now() - t) / 1000;
	const r = {
		frames: st.frames, not_drawn: st.notDrawn, head: st.headAll.summary(), head_by_kind: {},
		head_over_1: st.headOver1, head_over_2: st.headOver2,
		car_position: st.car.summary(), car_angle_deg: st.carAngle.summary(2), spacing: st.spacing.summary(),
		car_frames: st.carFrames, car_over_1: st.over1, car_over_2: st.over2, jumps: st.jumps, car_jumps: st.carJumps,
		car_by_kind: {},
	};
	for (const [i, k] of KINDS.entries()) {
		r.head_by_kind[k] = st.head[i].summary();
		r.car_by_kind[k] = st.carByKind[i].summary();
	}
	report.modes[mode] = r;
	fs.writeFileSync(path.join(dir, `worst${tag}-${mode}.json`), JSON.stringify({ head: st.worst, carriages: st.worstCar }));
	console.log(`\n${mode} (${secs.toFixed(1)} s): ${st.frames} train-frames, ${st.notDrawn} not drawn`);
	console.log(`  head error   ${fmt(r.head)}; > 1 tile ${st.headOver1}, > 2 tiles ${st.headOver2}`);
	for (const k of KINDS) {
		if (r.head_by_kind[k].n) {
			console.log(`    ${k.padEnd(9)} ${fmt(r.head_by_kind[k])}`);
		}
	}
	console.log(`  carriages   ${fmt(r.car_position)}; angle ${fmt(r.car_angle_deg, " deg")}; spacing ${fmt(r.spacing)}`);
	console.log(`  carriage-frames ${st.carFrames}: > 1 tile ${st.over1} (${(100 * st.over1 / st.carFrames).toFixed(3)}%), `
		+ `> 2 tiles ${st.over2} (${(100 * st.over2 / st.carFrames).toFixed(3)}%); jumps ${st.jumps}, carriage jumps ${st.carJumps}`);
}

function fmt(s, unit = "") {
	return `p50 ${s.p50}${unit} p90 ${s.p90} p99 ${s.p99} max ${s.max}`;
}

const outPath = option("--out", path.join(dir, `eval${tag}.json`));
fs.writeFileSync(outPath, JSON.stringify(report, null, 2));
console.log(`\n-> ${outPath}`);
