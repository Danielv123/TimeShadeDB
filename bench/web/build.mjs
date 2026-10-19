// Builds the illisis viewer for the client benchmark from the illisis repo source
// (env ILLISIS_REPO, default an illisis-minimap checkout next to this repo or its
// worktree directory) into dist/illisis/viewer.js. The viewer bundle shipped inside the
// test data is never used.
import { build } from "esbuild";
import { copyFileSync, existsSync, mkdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, "..", "..");
const illisis = resolve(process.env.ILLISIS_REPO ??
  [join(repo, "..", "illisis-minimap"), join(repo, "..", "..", "illisis-minimap")].find((p) => existsSync(p)) ??
  join(repo, "..", "illisis-minimap"));
if (!existsSync(join(illisis, "web", "map_view.ts"))) {
  console.error(`illisis repo not found at ${illisis}; set ILLISIS_REPO`);
  process.exit(1);
}
const out = join(here, "dist", "illisis");
mkdirSync(out, { recursive: true });
await build({
  entryPoints: [join(here, "illisis", "entry.ts")],
  bundle: true,
  format: "iife",
  target: "chrome120",
  outfile: join(out, "viewer.js"),
  logLevel: "warning",
  plugins: [{
    name: "illisis",
    setup(b) {
      b.onResolve({ filter: /^illisis\// }, (a) => ({ path: join(illisis, a.path.slice("illisis/".length)) }));
      // illisis imports .ts files with .js extensions (TypeScript NodeNext style).
      b.onResolve({ filter: /\.js$/ }, (a) => {
        if (!a.importer.startsWith(illisis)) return undefined;
        const ts = join(a.resolveDir, a.path.replace(/\.js$/, ".ts"));
        return existsSync(ts) ? { path: ts } : undefined;
      });
    },
  }],
});
copyFileSync(join(here, "illisis", "index.html"), join(out, "index.html"));
console.error(`built ${out} from ${illisis}`);
