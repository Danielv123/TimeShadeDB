// Before/after tables of railbench runs (RESULT.md): samples, storage on disk, and what the viewer draws in history and
// live mode against the game, from eval.mjs and raildisk outputs.
//
//   node bench/railbench/compare.mjs --row "<label>=<run dir>[,eval=<file>][,disk=<file>]" ... [--out <file.md>]
//
// <run dir> is a railbench run (<work>/runs/<name>); eval defaults to eval.json, disk to disk.json in it.
import fs from "node:fs";
import path from "node:path";

const args = process.argv.slice(2);
const rows = [];
for (let i = 0; i < args.length; i++) {
	if (args[i] === "--row") {
		const [head, ...opts] = args[++i].split(",");
		const [label, dir] = head.split("=");
		const o = Object.fromEntries(opts.map(s => s.split("=")));
		const ev = JSON.parse(fs.readFileSync(path.join(dir, o.eval ?? "eval.json"), "utf8"));
		const diskPath = path.join(dir, o.disk ?? "disk.json");
		const disk = fs.existsSync(diskPath) ? JSON.parse(fs.readFileSync(diskPath, "utf8")) : null;
		rows.push({ label, ev, disk });
	}
}
const outPath = args.includes("--out") ? args[args.indexOf("--out") + 1] : null;

const f = (v, d = 3) => (v === null || v === undefined || Number.isNaN(v) ? "-" : Number(v).toFixed(d).replace(/\.?0+$/, (m) => (m.startsWith(".") ? "" : m)));
const n0 = (v) => (v === null || v === undefined ? "-" : Math.round(v).toLocaleString("en-US"));
const pct = (a, b) => (b ? `${((100 * a) / b).toFixed(2)}%` : "-");
const lines = [];
const table = (head, body) => {
	lines.push(`| ${head.join(" | ")} |`, `|${head.map((_, i) => (i === 0 ? "---" : "---:")).join("|")}|`);
	for (const r of body) {
		lines.push(`| ${r.join(" | ")} |`);
	}
	lines.push("");
};

lines.push("### Samples", "");
table(["Run", "Samples", "per train-hour", "per train-km", "curve per train-hour / per km", "straight per km", "junction per km",
	"stop/start per km", "`car` rows per train-hour", "unmatched"], rows.map(({ label, ev }) => {
	const s = ev.samples;
	const k = s.by_kind;
	return [label, n0(s.samples), n0(s.samples_per_train_hour), f(s.samples_per_train_km, 1),
		`${n0(k.curve.per_train_hour)} / ${f(k.curve.per_train_km, 1)}`, f(k.straight.per_train_km, 1), f(k.junction.per_train_km, 1),
		f(k.stopstart.per_train_km, 1), n0(s.car_rows_per_train_hour), n0(s.unmatched)];
}));

lines.push("### Mover storage (zstd, train rows)", "");
table(["Run", "Train rows", "Train-row bytes", "B/row", "B per train-hour", "`car` share", "rail pieces", "rail pieces share"],
	rows.map(({ label, ev, disk }) => {
		if (!disk) {
			return [label, "-", "-", "-", "-", "-", "-", "-"];
		}
		const sum = (k) => disk.reduce((a, d) => a + (d[k] ?? 0), 0);
		const bytes = sum("train_only_bytes");
		const trainRows = sum("train_rows");
		const noCar = sum("train_nocar_bytes");
		const noRail = disk.some(d => d.train_norail_bytes !== undefined) ? sum("train_norail_bytes") : null;
		return [label, n0(trainRows), n0(bytes), f(bytes / trainRows, 2), n0(bytes / ev.samples.moving_train_hours), pct(bytes - noCar, bytes),
			noRail === null ? "-" : n0(sum("rail_pieces")), noRail === null ? "-" : pct(bytes - noRail, bytes)];
	}));

for (const mode of ["history", "live"]) {
	lines.push(`### ${mode === "history" ? "History playback" : "Live model"} against the game (moving train-ticks)`, "");
	table(["Run", "Head p50 / p90 / p99 / max", "Carriage p50 / p99 / max", "Angle p99 / max (deg)", "Spacing p99 / max",
		"Carriage-frames > 1 / > 2 tiles", "Jumps (head / carriage)", ...(mode === "live" ? ["Display delay p50 / p90 / max (s)"] : [])],
	rows.filter(r => r.ev.modes[mode]).map(({ label, ev }) => {
		const m = ev.modes[mode];
		const d = m.display_delay_ms;
		return [label, `${f(m.head.p50)} / ${f(m.head.p90)} / ${f(m.head.p99)} / ${f(m.head.max)}`,
			`${f(m.car_position.p50)} / ${f(m.car_position.p99)} / ${f(m.car_position.max)}`, `${f(m.car_angle_deg.p99, 2)} / ${f(m.car_angle_deg.max, 2)}`,
			`${f(m.spacing.p99)} / ${f(m.spacing.max)}`, `${pct(m.car_over_1, m.car_frames)} / ${pct(m.car_over_2, m.car_frames)}`,
			`${m.jumps} / ${m.car_jumps}`,
			...(mode === "live" ? [d ? `${f(d.p50 / 1000, 2)} / ${f(d.p90 / 1000, 2)} / ${f(d.max / 1000, 2)}` : "-"] : [])];
	}));
	lines.push(`Head error p99 / max by track kind (${mode}, with stop/start):`, "");
	table(["Run", "straight", "diagonal", "curve", "junction", "stopstart"], rows.filter(r => r.ev.modes[mode]).map(({ label, ev }) => {
		const k = ev.modes[mode].head_by_kind;
		return [label, ...["straight", "diagonal", "curve", "junction", "stopstart"].map(x => (k[x]?.n ? `${f(k[x].p99)} / ${f(k[x].max)}` : "-"))];
	}));
}

const text = lines.join("\n");
if (outPath) {
	fs.writeFileSync(outPath, text + "\n");
	console.log(`-> ${outPath}`);
} else {
	console.log(text);
}
