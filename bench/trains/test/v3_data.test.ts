// The loader of `measure movers` exports (bench/cmd/measure/movers.go).
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { loadV3Journeys } from "../src/v3_data.ts";

test("v3 export: samples, flags, carriages and handoffs", () => {
  const dir = mkdtempSync(join(tmpdir(), "v3data-"));
  const file = join(dir, "j.json");
  const GONE = 2, HANDOFF = 4, STOP = 1, ES = 8;
  writeFileSync(file, JSON.stringify({
    source: "v3", surface: "m.nauvis",
    journeys: [
      {
        id: "b", force: "player", name: "locomotive", metas: [[2, "LC", "ff0000"]],
        rows: [
          [1000, 60, 1, 2, 0.25, 0, 0, null],
          [2000, 120, 3, 2, 0.25, STOP, 0, [[3, 2, 0.25], [-4, 2, 0.25]]],
          [3000, null, 0, 0, null, GONE | HANDOFF, 0, null],
          [30000, null, 9, 9, null, ES, 0, null], // flagged handoff, though 27 s later
          [40000, null, 0, 0, null, GONE, 0, null], // an end: nothing follows
        ],
      },
      { id: "a", force: "player", name: "locomotive", metas: [[1, "L", ""]], rows: [[5, null, 0, 0, null, GONE, 0, null]] },
    ],
  }));
  const js = loadV3Journeys(file);
  assert.equal(js.length, 1, "a journey with only a gone is dropped");
  const j = js[0];
  assert.equal(j.id, "b");
  assert.equal(j.samples.length, 5);
  assert.deepEqual(j.samples[0], { t: 1000, tick: 60, x: 1, y: 2, vx: 0, vy: 0, h: 0.25, gone: false, n: 2, kinds: "LC", colour: "ff0000" });
  assert.equal(j.samples[1].stop, true);
  assert.equal(j.samples[1].car?.length, 2);
  assert.equal(j.samples[2].gone, true);
  assert.equal(j.samples[3].es, true);
  assert.equal(j.samples[3].h, null);
  assert.deepEqual(j.handoffs, [[3000, 30000]]);
});
