// Train evaluator CLI. Reads train journeys and the rail mask from the illisis test data,
// runs a renderer through the PLAN.md section 5 train metrics, and writes a result file
// in the bench result format (bench/internal/benchlib/result.go).
//
//   node dist/eval.mjs --data <illisis export> [--map gw-bc41437d~2E1] [--surface nauvis]
//       [--renderer illisis|linear|hermite|v3] [--lookahead-ms 61000] [--hz 10]
//       [--label illisis] [--scenario testdata] [--out result.json] [--journeys N]
//       [--journeys-json <file from `measure movers`>]  (train rows from a v3 database, such
//       as the imported test data, instead of the illisis windows; the rail mask still
//       comes from --data)

import { writeFileSync, mkdirSync } from "node:fs";
import { cpus, platform, release, arch } from "node:os";
import { basename, dirname } from "node:path";

import { illisisName, loadRailMask, loadTrainJourneys } from "./illisis_data.ts";
import { loadV3Journeys } from "./v3_data.ts";
import { defaultOptions, leaveOneOut, newRenderStats, renderJourney, summarize, type LooResult } from "./metrics.ts";
import { renderer } from "./renderers/index.ts";

function arg(name: string, def?: string): string | undefined {
  const i = process.argv.indexOf("--" + name);
  return i >= 0 && i + 1 < process.argv.length ? process.argv[i + 1] : def;
}

const data = arg("data");
if (!data) {
  console.error("usage: eval --data <illisis export root> [--map M] [--surface S] [--renderer R] [--out F]");
  process.exit(2);
}
const mapDir = arg("map", "gw-bc41437d~2E1")!;
const surfaceDir = illisisName(arg("surface", "nauvis")!);
const rname = arg("renderer", "illisis")!;
const label = arg("label", rname)!;
const scenario = arg("scenario", "testdata")!;
const out = arg("out");
const maxJourneys = Number(arg("journeys", "0"));
const journeysJson = arg("journeys-json");
const opts = {
  ...defaultOptions,
  hz: Number(arg("hz", String(defaultOptions.hz))),
  lookaheadMs: Number(arg("lookahead-ms", String(defaultOptions.lookaheadMs))),
};

const started = new Date();
const t0 = performance.now();
let journeys = journeysJson ? loadV3Journeys(journeysJson) : loadTrainJourneys(data, mapDir, surfaceDir);
if (maxJourneys > 0) {
  journeys = journeys.slice(0, maxJourneys);
}
const tLoad = performance.now();
const mask = loadRailMask(data, mapDir, surfaceDir);
const tMask = performance.now();
console.error(`loaded ${journeys.length} journeys in ${((tLoad - t0) / 1000).toFixed(1)} s, rail mask (${mask.railTiles} tiles) in ${((tMask - tLoad) / 1000).toFixed(1)} s`);

const r = renderer(rname);
const loo: LooResult = { all: [], curves: [] };
const st = newRenderStats();
let samples = 0;
let handoffs = 0;
for (const [k, j] of journeys.entries()) {
  samples += j.samples.length;
  handoffs += j.handoffs.length;
  leaveOneOut(r, j, opts, loo);
  renderJourney(r, j, mask, opts, st);
  if ((k + 1) % 5 === 0) {
    console.error(`  ${k + 1}/${journeys.length} journeys, ${st.frames} frames`);
  }
}
const metrics = summarize(loo, st);
metrics["trains.journeys"] = journeys.length;
metrics["trains.samples"] = samples;
metrics["trains.handoffs"] = handoffs;
metrics["trains.rail_tiles"] = mask.railTiles;
const seconds = (performance.now() - t0) / 1000;

const result = {
  tool: "trains",
  label,
  target: rname === "v3" ? "v3" : rname === "illisis" ? "illisis" : "reference",
  scenario,
  started: started.toISOString(),
  seconds,
  machine: { os: `${platform()} ${release()}`, arch: arch(), cpu: cpus()[0]?.model ?? "", cpus: cpus().length, go: `node ${process.version}` },
  // The data path is local; record it relative to TSDB_TESTDATA when it is that, else its last element.
  params: { data: data === process.env.TSDB_TESTDATA ? "$TSDB_TESTDATA" : basename(data), map: mapDir, surface: surfaceDir, renderer: rname,
    journeys: journeysJson ? "v3 database export (" + basename(journeysJson) + ")" : "illisis windows", ...opts },
  metrics,
  notes: [
    journeysJson
      ? "train samples: the v3 database's stored rows (imported by WP5: re-timed, deduped, handoffs flagged), exported by `measure movers`"
      : "train samples: the illisis movement windows as exported (with the illisis store's clamp re-timing)",
    "rail mask: final z0 chart pixels of colour (140,142,140); off-rail = carriage centre more than 1 tile from a rail tile",
    "trains.offrail_* count every drawn carriage (5 per train for illisis, 1 for the head-only references); trains.head_offrail_* count the head only and compare across renderers",
    `lookahead ${opts.lookaheadMs} ms (61000 = history playback, where the next sample is always known)`,
  ],
};
for (const k of Object.keys(metrics).sort()) {
  console.log(`  ${k.padEnd(44)} ${Number.isInteger(metrics[k]) ? metrics[k] : metrics[k].toFixed(4)}`);
}
if (out) {
  mkdirSync(dirname(out), { recursive: true });
  writeFileSync(out, JSON.stringify(result, (_k, v) => (typeof v === "number" && !Number.isFinite(v) ? null : v), 2) + "\n");
  console.error(`wrote ${out} (${seconds.toFixed(1)} s)`);
}
