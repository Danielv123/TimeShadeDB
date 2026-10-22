// Collects the railbench outputs of runs into Markdown tables (the numbers of BASELINE.md / RESULT.md).
//
//   node bench/railbench/report.mjs --run bizz[:ups-bizz] --run l1n3[:ups-l1n3] ... [--work <dir>] [--out <file.md>]
//
// Per run it reads run.json, feed.json, disk.json, eval.json, eval-geo.json (eval --stopstart-ticks 0), the
// simulate-*-capture.json, -accel.json and -capture-slow30.json replays, geometry.json and, after the colon, the ups
// run's ups.json.
import fs from "node:fs";
import path from "node:path";

const args = process.argv.slice(2);
const runs = args.flatMap((a, i) => (a === "--run" ? [args[i + 1]] : []));
const option = (name, fallback) => {
	const i = args.indexOf(name);
	return i >= 0 && args[i + 1] !== undefined ? args[i + 1] : fallback;
};
const work = path.resolve(option("--work", process.env.RAILBENCH_WORK ?? "G:/project_files/timeshade-v3-work/railbench"));
const read = (p) => (fs.existsSync(p) ? JSON.parse(fs.readFileSync(p, "utf8")) : null);
const f = (v, d = 2) => (v === null || v === undefined || Number.isNaN(v) ? "-" : Number(v).toFixed(d));
const n0 = (v) => (v === null || v === undefined ? "-" : Math.round(v).toLocaleString("en-US"));
const KINDS = ["straight", "diagonal", "curve", "junction", "stopstart"];
const lines = [];
const P = (s = "") => lines.push(s);

const data = runs.map((spec) => {
	const [name, ups] = spec.split(":");
	const dir = path.join(work, "runs", name);
	const sims = fs.readdirSync(dir).filter(x => /^simulate-.*\.json$/.test(x));
	return {
		name, dir,
		run: read(path.join(dir, "run.json")), feed: read(path.join(dir, "feed.json")), disk: read(path.join(dir, "disk.json")),
		ev: read(path.join(dir, "eval.json")), evGeo: read(path.join(dir, "eval-geo.json")),
		sim: read(path.join(dir, sims.find(x => x.endsWith("-capture.json")) ?? "none")),
		simAccel: read(path.join(dir, sims.find(x => x.endsWith("-accel.json")) ?? "none")),
		simSlow: read(path.join(dir, sims.find(x => x.endsWith("-capture-slow30.json")) ?? "none")),
		geo: read(path.join(dir, "geometry.json")), ups: ups ? read(path.join(work, "runs", ups, "ups.json")) : null,
	};
});

P("### Runs");
P();
P("| Run | Save (saved by) | Ticks | Game speed | Trains moving | Moving train-hours | Train-km | Wall s (ms/tick, capture + recorder) |");
P("|---|---|---:|---:|---:|---:|---:|---:|");
for (const d of data) {
	const s = d.ev.samples;
	P(`| ${d.name} | ${d.run.save.slice(0, 40)} (${d.run.saveVersion}) | ${d.run.ticks} | ${d.feed?.speed ?? 1} | ${s.trains_moving} | `
		+ `${f(s.moving_train_hours, 3)} | ${f(s.train_km, 1)} | ${f(d.run.wallMs / 1000, 0)} (${f(d.run.msPerTick, 2)}) |`);
}
P();
P("### Samples stored (today's capture, read back from serve)");
P();
P("| Run | Samples | per train-hour | per train-km | `car` rows per train-hour | unmatched |");
P("|---|---:|---:|---:|---:|---:|");
for (const d of data) {
	const s = d.ev.samples;
	P(`| ${d.name} | ${n0(s.samples)} | ${n0(s.samples_per_train_hour)} | ${f(s.samples_per_train_km, 1)} | ${n0(s.car_rows_per_train_hour)} | ${s.unmatched} |`);
}
P();
for (const [label, key] of [["with stop/start (2 s around a stand)", "ev"], ["by track alone", "evGeo"]]) {
	P(`Samples per train-hour / per train-km by track kind, ${label} (share of moving time in brackets):`);
	P();
	P(`| Run | ${KINDS.filter(k => key === "ev" || k !== "stopstart").join(" | ")} |`);
	P(`|---|${KINDS.filter(k => key === "ev" || k !== "stopstart").map(() => "---:").join("|")}|`);
	for (const d of data) {
		const bk = d[key]?.samples.by_kind;
		if (!bk) {
			continue;
		}
		P(`| ${d.name} | ${KINDS.filter(k => key === "ev" || k !== "stopstart").map(k => `${n0(bk[k].per_train_hour)} / ${f(bk[k].per_train_km, 1)} (${f(100 * bk[k].share_of_time, 1)}%)`).join(" | ")} |`);
	}
	P();
}

P("### Mover storage on disk (zstd)");
P();
P("Train rows only, re-encoded per closed window at the store's zstd level (`raildisk`); keyframes are hourly, so their cost per train-hour is one keyframe per hour of map time over the moving train-hours in an hour.");
P();
P("| Run | Window files (all movers) | Train rows | Train-row bytes | B/sample | B per train-hour | `car` share | Keyframe B/hour | Keyframe share of an hour | B per train-hour incl. keyframes |");
P("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|");
for (const d of data) {
	if (!d.disk) {
		continue;
	}
	const sum = (k) => d.disk.reduce((n, s) => n + s[k], 0);
	const trainBytes = sum("train_only_bytes");
	const noCar = sum("train_nocar_bytes");
	const rows = sum("train_rows");
	const kf = sum("keyframe_bytes");
	const h = d.ev.samples.moving_train_hours;
	const runHours = d.run.ticks / 216000;
	const perHourWindows = trainBytes / runHours;
	P(`| ${d.name} | ${n0(sum("window_bytes"))} | ${n0(rows)} | ${n0(trainBytes)} | ${f(trainBytes / rows, 2)} | ${n0(trainBytes / h)} | `
		+ `${f(100 * (trainBytes - noCar) / trainBytes, 1)}% | ${n0(kf)} | ${f(100 * kf / (perHourWindows + kf), 2)}% | `
		+ `${n0((trainBytes + kf * runHours) / h)} |`);
}
P();

P("### What the viewer draws against the game");
P();
for (const mode of ["history", "live"]) {
	P(`${mode === "history" ? "History playback (every row known)" : "Live (rows in 1 s batches + 150 ms, the viewer's adaptive delay)"}, moving train-ticks:`);
	P();
	P("| Run | Head p50 / p90 / p99 / max | Carriage position p50 / p99 / max | Carriage angle p50 / p99 / max (deg) | Spacing error p99 / max | Carriage-frames > 1 / > 2 tiles | Jumps (head / carriage) |");
	P("|---|---|---|---|---|---|---|");
	for (const d of data) {
		const m = d.ev.modes[mode];
		const h = m.head;
		P(`| ${d.name} | ${h.p50} / ${h.p90} / ${h.p99} / ${h.max} | ${m.car_position.p50} / ${m.car_position.p99} / ${m.car_position.max} | `
			+ `${m.car_angle_deg.p50} / ${m.car_angle_deg.p99} / ${m.car_angle_deg.max} | ${m.spacing.p99} / ${m.spacing.max} | `
			+ `${f(100 * m.car_over_1 / m.car_frames, 2)}% / ${f(100 * m.car_over_2 / m.car_frames, 2)}% | ${m.jumps} / ${m.car_jumps} |`);
	}
	P();
	for (const [label, key] of [["with stop/start", "ev"], ["by track alone", "evGeo"]]) {
		P(`Head error p99 / max by track kind (${mode}, ${label}):`);
		P();
		const ks = KINDS.filter(k => key === "ev" || k !== "stopstart");
		P(`| Run | ${ks.join(" | ")} |`);
		P(`|---|${ks.map(() => "---").join("|")}|`);
		for (const d of data) {
			const hb = d[key]?.modes[mode].head_by_kind;
			if (!hb) {
				continue;
			}
			P(`| ${d.name} | ${ks.map(k => `${hb[k].p99 ?? "-"} / ${hb[k].max ?? "-"}`).join(" | ")} |`);
		}
		P();
	}
}

P("### Sampling rules replayed on the recording (simulate.mjs)");
P();
for (const [label, key] of [["Reads as the capture schedules them", "sim"], ["Reads scheduled with the acceleration (--reads accel)", "simAccel"],
	["Reads at most 30 ticks apart (--slow 30)", "simSlow"]]) {
	P(`${label}; position error of the head at every moving tick against what the samples describe:`);
	P();
	P("| Run | Rule | Samples per train-hour | per train-km | vs today | p50 / p90 / p99 / max (tiles) | Across p99 / max |");
	P("|---|---|---:|---:|---:|---|---|");
	for (const d of data) {
		const s = d[key];
		if (!s) {
			continue;
		}
		for (const r of s.params.rules) {
			const x = s[r];
			const pa = x.position_at_time;
			P(`| ${d.name} | ${r} | ${n0(x.per_train_hour)} | ${f(x.per_train_km, 1)} | ${f(100 * (x.per_train_hour / s.current.per_train_hour - 1), 0)}% | `
				+ `${pa.p50} / ${pa.p90} / ${pa.p99} / ${pa.max} | ${x.cross_track ? `${x.cross_track.p99} / ${x.cross_track.max}` : "-"} |`);
		}
		if (s.matched?.matching) {
			const m = s.matched.matching;
			P(`| ${d.name} | (matching) | ${m.pairs} sample pairs, ${m.not_the_true_path} on another path, ${m.no_path} without one | | | ${f(m.facing_switches_per_km, 1)} facing switches per km | |`);
		}
	}
	P();
}

P("### Rail geometry against the game (geometry.mjs)");
P();
P("| Run | Rail type | Model residual p50 / p99 / max | Bezier through ends p99 / max | Carriage centre off rail p50 / max | Carriage from own front joint p99 / max | game distance per speed p50 (s; plan) |");
P("|---|---|---|---|---|---|---|");
for (const d of data) {
	const g = d.geo;
	if (!g) {
		continue;
	}
	for (const [t, m] of Object.entries(g.model_residual)) {
		const b = g.bezier_residual[t];
		const c = g.centre_offset[t];
		const o = g.carriage_from_own_joint?.[t];
		const sr = g.s_per_speed[t];
		const pr = g.plan_per_speed[t];
		P(`| ${d.name} | ${t} | ${m.p50} / ${m.p99} / ${m.max} | ${b ? `${b.p99} / ${b.max}` : "-"} | ${c ? `${c.p50} / ${c.max}` : "-"} | `
			+ `${o ? `${o.p99} / ${o.max}` : "-"} | ${sr ? `${sr.p50}; ${pr.p50}` : "-"} |`);
	}
}
P();
P("| Run | Carriages placed from the leading front joint + rail path: position p50 / p99 / max | angle p99 / max (deg) | front joint spacing \\|dU - 7\\| p99 / max | h vs tangent on curves p50 / max (deg) | leading carriage's own vs tangent on curves p50 / max |");
P("|---|---|---|---|---|---|");
for (const d of data) {
	const g = d.geo;
	if (!g) {
		continue;
	}
	const pl = g.placement;
	P(`| ${d.name} | ${pl.position.p50} / ${pl.position.p99} / ${pl.position.max} (${pl.carriages} carriages, ${pl.failed} of ${pl.trains} lines not chained) | `
		+ `${pl.angle_deg.p99} / ${pl.angle_deg.max} | ${g.pitch_error?.p99} / ${g.pitch_error?.max} | ${g.heading.curve.h_vs_tangent_deg.p50} / `
		+ `${g.heading.curve.h_vs_tangent_deg.max} | ${g.heading.curve.own_vs_tangent_deg.p50} / ${g.heading.curve.own_vs_tangent_deg.max} |`);
}
P();
P("| Run | Curve type | Best-fit circle radius | residual p99 / max |");
P("|---|---|---:|---|");
for (const d of data) {
	for (const [t, c] of Object.entries(d.geo?.circle_fit ?? {})) {
		P(`| ${d.name} | ${t} | ${c.radius} | ${c.residual.p99} / ${c.residual.max} |`);
	}
}
P();

P("### Capture UPS cost (recorder off)");
P();
P("| Run | Pairs | Affinity | Capture Lua ms/tick per pair (after the load resync) | Mean | Whole tick delta mean | Baseline Lua / whole tick | Load resync window Lua delta |");
P("|---|---:|---|---|---:|---:|---|---|");
for (const d of data) {
	const u = d.ups;
	if (!u) {
		continue;
	}
	const mean = (k) => u.lines.reduce((n, l) => n + l[k], 0) / u.lines.length;
	P(`| ${d.name} | ${u.pairs} | ${u.affinity ?? "none"} | ${u.lines.map(l => f(l.luaDelta, 3)).join(", ")} | ${f(mean("luaDelta"), 3)} | `
		+ `${f(mean("wholeDelta"), 3)} | ${f(mean("luaWithout"), 3)} / ${f(mean("wholeWithout"), 2)} | ${u.lines.map(l => f(l.luaDeltaFirst, 3)).join(", ")} (ticks 0-${u.from}) |`);
}
P();

const text = lines.join("\n") + "\n";
const outPath = option("--out", null);
if (outPath) {
	fs.writeFileSync(outPath, text);
	console.log(`-> ${outPath}`);
} else {
	process.stdout.write(text);
}
