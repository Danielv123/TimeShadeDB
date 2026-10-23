// Train rendering evaluator (PLAN.md §4.8, §5 "Trains"): leave-one-out interpolation error and
// rendered carriage distance to rails, v3 viewer against the illisis baseline.
//
// usage (from web/): node src/movers/eval/train_eval.mjs <trains.jsonl> <rail_mask.bin>
//          [--surface nauvis] [--limit N] [--no-clamp-fix] [--skip-loo] [--skip-render]
//          [--modes history,live] [--extrap-turn radians] [--extrap-turn-inside] [--json out.json]
// Inputs are research outputs (out/trains.jsonl from movers_stats.py --dump-trains, and
// rail_mask.py); they are read as data only.
import "./ts-resolve.mjs";
import { readFileSync, writeFileSync } from "node:fs";

const { parseJourney, dropClamped, leaveOneOut, renderEval, RailMask, quantile } = await import("./evaluate.ts");
const { motionConfig } = await import("../track.ts");

const args = process.argv.slice(2);
const flag = name => args.includes(name);
const opt = (name, def) => (args.includes(name) ? args[args.indexOf(name) + 1] : def);
const pos = args.filter((a, i) => !a.startsWith("--") && !(i > 0 && args[i - 1].startsWith("--") && !["--no-clamp-fix", "--skip-loo", "--skip-render", "--extrap-turn-inside"].includes(args[i - 1])));
const [trainsPath, maskPath] = pos;
if (!trainsPath || !maskPath) {
  console.error("usage: train_eval.mjs <trains.jsonl> <rail_mask.bin> [--surface nauvis] [--limit N]");
  process.exit(2);
}
const surface = opt("--surface", "nauvis");
if (opt("--extrap-turn") !== undefined) {
  motionConfig.extrapTurn = Number(opt("--extrap-turn"));
}
if (flag("--extrap-turn-inside")) {
  motionConfig.extrapTurnInside = true;
}
const limit = Number(opt("--limit", "0"));
let journeys = readFileSync(trainsPath, "utf8").trim().split("\n").map(parseJourney).filter(j => j.surface === surface);
if (limit > 0) {
  journeys = journeys.slice(0, limit);
}
if (!flag("--no-clamp-fix")) {
  journeys = journeys.map(j => dropClamped(j));
}
const rows = journeys.reduce((a, j) => a + j.rows.length, 0);
console.log(`surface ${surface}: ${journeys.length} journeys, ${rows} rows, clamp fix ${flag("--no-clamp-fix") ? "off" : "on"}`);
const report = { surface, journeys: journeys.length, rows, clampFix: !flag("--no-clamp-fix") };
const fmt = a => `n=${a.length} p50=${quantile(a, 0.5).toFixed(2)} p90=${quantile(a, 0.9).toFixed(2)} p99=${quantile(a, 0.99).toFixed(2)}`;

if (!flag("--skip-loo")) {
  const saved = { ...motionConfig };
  const modes = {
    "v3 (default)": () => Object.assign(motionConfig, { tangent: "blend", geometry: "rail" }),
    "v3 orient, rail": () => Object.assign(motionConfig, { tangent: "orient", geometry: "rail" }),
    "v3 fd, rail": () => Object.assign(motionConfig, { tangent: "fd", geometry: "rail" }),
    "v3 blend, hermite": () => Object.assign(motionConfig, { tangent: "blend", geometry: "hermite" }),
    "v3 fd, hermite": () => Object.assign(motionConfig, { tangent: "fd", geometry: "hermite" }),
  };

  const t = performance.now();
  const loo = leaveOneOut(journeys, modes, () => Object.assign(motionConfig, saved));
  console.log(`\nleave-one-out (${((performance.now() - t) / 1000).toFixed(1)} s), error in tiles`);
  report.loo = {};
  for (const k of Object.keys(loo.all)) {
    const all = loo.all[k].filter(Number.isFinite);
    const cur = loo.curves[k].filter(Number.isFinite);
    console.log(`  ${k.padEnd(20)} all: ${fmt(all)} | curves: ${fmt(cur)}`);
    report.loo[k] = { all: [0.5, 0.9, 0.99].map(q => quantile(all, q)), curves: [0.5, 0.9, 0.99].map(q => quantile(cur, q)), n: all.length };
  }
}

if (!flag("--skip-render")) {
  const mask = new RailMask(new Uint8Array(readFileSync(maskPath)));
  report.render = {};
  for (const mode of opt("--modes", "history,live").split(",")) {
    const t = performance.now();
    const r = renderEval(journeys, mask, { mode });
    console.log(`\nrender ${mode} (${((performance.now() - t) / 1000).toFixed(1)} s); raw sample heads > 1 tile from rail: ${(100 * r.rawHead.above(1)).toFixed(2)}%, > 2 tiles ${(100 * r.rawHead.above(2)).toFixed(2)}%, p99 ${r.rawHead.quantile(0.99).toFixed(2)}`);
    report.render[mode] = { rawAbove1: r.rawHead.above(1), rawAbove2: r.rawHead.above(2), rawP99: r.rawHead.quantile(0.99) };
    for (const [name, st] of [["v3", r.v3], ["illisis", r.illisis]]) {
      const line = {
        frames: st.frames, movingFrames: st.movingFrames,
        carsAbove1: st.cars.above(1), carsP99: st.cars.quantile(0.99),
        movingAbove1: st.movingCars.above(1), movingP99: st.movingCars.quantile(0.99),
        validAbove1: st.validCars.above(1), validP99: st.validCars.quantile(0.99), validOver2: st.validOver2, validShare: st.validCars.total / Math.max(1, st.cars.total),
        over2: st.over2, movingOver2: st.movingOver2, headJumps: st.headJumps, carJumps: st.carJumps, handoffFrames: st.handoffFrames,
      };
      report.render[mode][name] = line;
      console.log(`  ${name.padEnd(8)} frames ${st.frames} (moving ${st.movingFrames}); cars >1 tile ${(100 * line.carsAbove1).toFixed(2)}% p99 ${line.carsP99.toFixed(2)}; moving cars >1 tile ${(100 * line.movingAbove1).toFixed(2)}% p99 ${line.movingP99.toFixed(2)}; >2 tiles outside handoffs ${st.over2} (moving ${st.movingOver2}); head jumps ${st.headJumps}, carriage jumps ${st.carJumps}`);
      console.log(`  ${"".padEnd(8)} where the mask covers the track (${(100 * line.validShare).toFixed(1)}% of carriage-frames): >1 tile ${(100 * line.validAbove1).toFixed(2)}%, p99 ${line.validP99.toFixed(2)}, >2 tiles outside handoffs ${st.validOver2}`);
    }
  }
}
if (opt("--json")) {
  writeFileSync(opt("--json"), JSON.stringify(report, null, 2));
}
