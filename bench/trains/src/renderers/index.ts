// Renderer registry. "v3" is the v3 viewer's movers module: build.mjs resolves the
// "v3-movers" import to web/src/movers/bench.ts when that file exists, which must export
// createTrainRenderer(): TrainRenderer (see bench/README.md); otherwise to a stub.

import { createTrainRenderer } from "v3-movers";

import type { TrainRenderer } from "../types.ts";
import { illisisRenderer } from "./illisis.ts";
import { referenceRenderer } from "./reference.ts";

export function renderer(name: string): TrainRenderer {
  switch (name) {
    case "illisis":
      return illisisRenderer();
    case "linear":
    case "hermite":
      return referenceRenderer(name);
    case "v3":
      if (!createTrainRenderer) {
        throw new Error("renderer v3: web/src/movers/bench.ts does not exist");
      }
      return createTrainRenderer();
  }
  throw new Error(`unknown renderer ${name} (illisis, linear, hermite, v3)`);
}
