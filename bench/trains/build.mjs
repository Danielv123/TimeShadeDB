// Bundles the evaluator with esbuild into dist/eval.mjs.
// - "illisis-movers" resolves to <ILLISIS_REPO>/web/view/movers.ts (env ILLISIS_REPO,
//   default ../../../illisis-minimap next to the TimeShadeDB checkout). The illisis
//   source is only read by the bundler; nothing from the test data is executed.
// - "v3-movers" resolves to web/src/movers/bench.ts when it exists (WP8), else a stub.
import { build } from "esbuild";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, "..", "..");
// Default: an illisis-minimap checkout next to the TimeShadeDB checkout, or next to the
// directory holding a worktree (TimeShadeDB-wt/<name>).
const illisis = resolve(process.env.ILLISIS_REPO ??
  [join(repo, "..", "illisis-minimap"), join(repo, "..", "..", "illisis-minimap")].find((p) => existsSync(p)) ??
  join(repo, "..", "illisis-minimap"));
const stubDir = join(here, "dist", "stubs");
mkdirSync(stubDir, { recursive: true });

const illisisMovers = join(illisis, "web", "view", "movers.ts");
const v3Movers = join(repo, "web", "src", "movers", "bench.ts");

function stub(name, body) {
  const p = join(stubDir, name);
  writeFileSync(p, body);
  return p;
}

const aliases = {
  "illisis-movers": existsSync(illisisMovers)
    ? illisisMovers
    : stub("illisis-movers.mjs", `export class Tracks { constructor() { throw new Error("illisis repo not found at ${illisis.replaceAll("\\", "/")}; set ILLISIS_REPO"); } }\n`),
  "v3-movers": existsSync(v3Movers) ? v3Movers : stub("v3-movers.mjs", "export const createTrainRenderer = undefined;\n"),
};

await build({
  entryPoints: [join(here, "src", "main.ts")],
  bundle: true,
  platform: "node",
  format: "esm",
  target: "node22",
  outfile: join(here, "dist", "eval.mjs"),
  logLevel: "warning",
  plugins: [{
    name: "aliases",
    setup(b) {
      b.onResolve({ filter: /^(illisis-movers|v3-movers)$/ }, (a) => ({ path: aliases[a.path] }));
    },
  }],
});
console.error(`built dist/eval.mjs (illisis: ${existsSync(illisisMovers) ? illisisMovers : "missing"}, v3 movers: ${existsSync(v3Movers) ? "yes" : "not yet"})`);
