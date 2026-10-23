// The illisis/minimap viewer's own train drawing (web/view/movers.ts: Tracks, place,
// line, carriages), bundled from the illisis repo by build.mjs (alias "illisis-movers").
// Nothing is copied: the evaluator drives the original class through its public API.

import { Tracks } from "illisis-movers";

import type { JourneyMeta, Pose, TrainRenderer, TrainSample, TrainSession } from "../types.ts";

export function illisisRenderer(): TrainRenderer {
  return {
    name: "illisis",
    session(meta: JourneyMeta): TrainSession {
      const tracks = new Tracks();
      return {
        feed(s: TrainSample): void {
          const mover = [meta.id, "t", meta.force, meta.name, s.n, s.kinds, s.colour];
          const row = s.gone ? [0, s.t] : [0, s.t, s.x, s.y, s.vx, s.vy, s.h ?? 0];
          tracks.window({ t: 0, movers: [mover], s: [row] });
        },
        pose(t: number): Pose | undefined {
          const shown = tracks.at(t)[0];
          if (!shown) {
            return undefined;
          }
          return { x: shown.x, y: shown.y, cars: shown.cars.map((c: { x: number; y: number }) => ({ x: c.x, y: c.y })) };
        },
      };
    },
  };
}
