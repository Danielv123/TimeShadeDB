// Serves the built viewer (web/dist) with a mock v3 API, for manual testing and benchmarks:
//
//   node src/map/testing/serve.ts [--port 0] [--latency ms] [--no-gzip]
//        [--illisis <export root> --map gw-bc41437d~2E1 --surface nauvis --snapshots 4]
//        [--synthetic WxH --versions n --step ms --changes chunks --coarse levels]
//        [--shift-to-now] (move all times so the newest data is 30 s old)
//        [--movers-db <v3 db>] (movers, instances and edges from `timeshadedb import-minimap --db`;
//        history only, and not with --shift-to-now)
//
// Prints "listening on http://127.0.0.1:<port>" once ready. The live API is under
// /api/v3/, the same data as a static export under /static/ (open
// /?source=static&data=/static/ to view it). Node only; the port defaults to a free one.

import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { shiftDataset, syntheticDataset, type MockDataset } from "./dataset.ts";
import { loadIllisisDataset } from "./illisis.ts";
import { MockServer } from "./mockserver.ts";
import { attachMoverDb } from "./moverdb.ts";

export async function loadDataset(args: Map<string, string>): Promise<MockDataset> {
  const data = await loadRaw(args);
  if (args.has("shift-to-now")) {
    // Make the newest data 30 s old, as on a live server or a current export.
    const st = [...data.stores.values()][0];
    shiftDataset(data, Date.now() - 30_000 - st.ds.last);
  }
  return data;
}

async function loadRaw(args: Map<string, string>): Promise<MockDataset> {
  const root = args.get("illisis");
  if (root) {
    return loadIllisisDataset(root, {
      map: args.get("map") ?? "gw-bc41437d~2E1",
      surface: args.get("surface") ?? "nauvis",
      snapshots: Number(args.get("snapshots") ?? 4),
    });
  }
  const [w, h] = (args.get("synthetic") ?? "8x6").split("x").map(Number);
  return syntheticDataset({
    bounds: [-Math.floor(w / 2), -Math.floor(h / 2), Math.ceil(w / 2) - 1, Math.ceil(h / 2) - 1],
    versions: Number(args.get("versions") ?? 6),
    step: Number(args.get("step") ?? 60_000),
    coarse: Number(args.get("coarse") ?? 3),
    changes: Number(args.get("changes") ?? 6),
  });
}

export function parseArgs(argv: string[]): Map<string, string> {
  const args = new Map<string, string>();
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith("--")) {
      const next = argv[i + 1];
      if (next === undefined || next.startsWith("--")) {
        args.set(a.slice(2), "true");
      } else {
        args.set(a.slice(2), next);
        i++;
      }
    }
  }
  return args;
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2));
  const data = await loadDataset(args);
  const moverDb = args.get("movers-db") ? { dir: args.get("movers-db")! } : undefined;
  if (moverDb) {
    attachMoverDb(data, moverDb);
  }
  const dist = resolve(fileURLToPath(new URL("../../../dist/", import.meta.url)));
  const server = await new MockServer(data, {
    staticDir: dist,
    latencyMs: Number(args.get("latency") ?? 0),
    gzip: args.get("no-gzip") !== "true",
    moverDb,
  }).start(Number(args.get("port") ?? 0));
  console.log(`listening on ${server.url}`);
  const stop = (): void => {
    void server.stop().then(() => process.exit(0));
  };
  process.on("SIGINT", stop);
  process.on("SIGTERM", stop);
}

if (process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url))) {
  void main();
}
