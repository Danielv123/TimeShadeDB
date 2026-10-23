// Reads train journeys exported from a v3 database by `measure movers` (the imported,
// re-timed and deduped rows), as an alternative to the illisis movement windows.

import { readFileSync } from "node:fs";

import { HANDOFF_MS } from "./illisis_data.ts";
import type { Journey, TrainSample } from "./types.ts";

// core.MoverFlags (internal/core/movers.go).
const F_STOP = 1 << 0;
const F_GONE = 1 << 1;
const F_HANDOFF = 1 << 2;
const F_END_SWITCH = 1 << 3;

type Row = [number, number | null, number, number, number | null, number, number, [number, number, number][] | null];

interface Export {
  source: string;
  surface: string;
  journeys: { id: string; force: string; name: string; metas: [number, string, string][]; rows: Row[] }[];
}

/** Journeys from a `measure movers` file. Velocity is 0: v3 does not store it. */
export function loadV3Journeys(path: string): Journey[] {
  const ex = JSON.parse(readFileSync(path, "utf8")) as Export;
  const out: Journey[] = [];
  for (const ej of ex.journeys) {
    const samples: TrainSample[] = ej.rows.map(([t, tick, x, y, h, flags, mi, car]) => {
      const [n, kinds, colour] = ej.metas[mi] ?? [0, "", ""];
      const s: TrainSample = { t, tick, x, y, vx: 0, vy: 0, h, gone: (flags & F_GONE) !== 0, n, kinds, colour };
      if (flags & F_STOP) s.stop = true;
      if (flags & F_END_SWITCH) s.es = true;
      if (car) s.car = car;
      return s;
    });
    const handoffs: [number, number][] = [];
    for (let i = 0; i < samples.length; i++) {
      if (!samples[i].gone) continue;
      let k = i + 1;
      while (k < samples.length && samples[k].gone) k++;
      const flagged = (ej.rows[i][5] & F_HANDOFF) !== 0;
      if (k < samples.length && (flagged || samples[k].t - samples[i].t <= HANDOFF_MS)) {
        handoffs.push([samples[i].t, samples[k].t]);
      }
    }
    if (samples.some((s) => !s.gone)) {
      out.push({ id: ej.id, force: ej.force, name: ej.name, samples, handoffs });
    }
  }
  out.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  return out;
}
