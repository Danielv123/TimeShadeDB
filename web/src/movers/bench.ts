// The v3 movers module as a train renderer for the WP11 train evaluator (bench/trains):
// bench/trains/build.mjs resolves its "v3-movers" import to this file and calls
// createTrainRenderer(). The types are bench/trains/src/types.ts, repeated here
// structurally so the viewer does not import from bench/.
//
// Samples go through what the v3 store keeps (FORMAT.md 7.4): positions in 1/16 tile,
// u16 orientation, no velocity; a sample without a tick is flagged F_NO_TICK. Poses are what
// the movers layer draws (MoverView.frame, carriages included), with the data horizon at the
// newest sample fed, which is a lower bound of what the evaluator has fed.

import type { Carriage, MoverMeta } from "../api/types";
import { F_END_SWITCH, F_GONE, F_NO_HEADING, F_NO_TICK, F_STOP, makeSample } from "./track";
import { MoverView } from "./view";

export interface BenchTrainSample {
  t: number;
  tick: number | null;
  x: number;
  y: number;
  vx: number;
  vy: number;
  h: number | null;
  gone: boolean;
  n: number;
  kinds: string;
  colour: string;
  stop?: boolean;
  es?: boolean;
  car?: [number, number, number][];
}

export interface BenchJourneyMeta {
  id: string;
  force: string;
  name: string;
}

export interface BenchPose {
  x: number;
  y: number;
  cars: { x: number; y: number }[];
}

export interface BenchTrainSession {
  feed(s: BenchTrainSample): void;
  pose(t: number): BenchPose | undefined;
}

export interface BenchTrainRenderer {
  name: string;
  session(meta: BenchJourneyMeta): BenchTrainSession;
}

const q16 = (v: number): number => Math.round(v * 16) / 16;
const qo = (o: number): number => (((Math.round(o * 65536) % 65536) + 65536) % 65536) / 65536;

class Session implements BenchTrainSession {
  private readonly view = new MoverView();
  private readonly metas = new Map<string, MoverMeta>();
  private newest = -Infinity;

  constructor(private readonly j: BenchJourneyMeta) {}

  private meta(n: number, kinds: string, colour: string): MoverMeta {
    const key = `${n}|${kinds}|${colour}`;
    let m = this.metas.get(key);
    if (!m) {
      m = { id: this.j.id, k: "t", f: this.j.force, name: this.j.name, n, o: kinds, c: colour || undefined } as MoverMeta;
      this.metas.set(key, m);
    }
    return m;
  }

  feed(s: BenchTrainSample): void {
    const meta = this.meta(s.n, s.kinds, s.colour);
    let flags = s.tick === null ? F_NO_TICK : 0;
    if (s.gone) {
      this.view.add(meta, makeSample(meta, s.t, 0, 0, 0, flags | F_GONE, null, s.tick));
    } else {
      if (s.stop) {
        flags |= F_STOP;
      }
      if (s.es) {
        flags |= F_END_SWITCH;
      }
      if (s.h === null) {
        flags |= F_NO_HEADING;
      }
      const car: Carriage[] | null = s.car ? s.car.map(([x, y, o]) => ({ x: q16(x), y: q16(y), o: qo(o) })) : null;
      this.view.add(meta, makeSample(meta, s.t, q16(s.x), q16(s.y), s.h === null ? NaN : qo(s.h), flags, car, s.tick));
    }
    this.newest = Math.max(this.newest, s.t);
  }

  pose(t: number): BenchPose | undefined {
    const m = this.view.frame(t, Math.max(this.newest, t), { bodies: true })[0];
    if (!m) {
      return undefined;
    }
    return { x: m.x, y: m.y, cars: m.cars.length > 0 ? m.cars.map(c => ({ x: c.x, y: c.y })) : [{ x: m.x, y: m.y }] };
  }
}

export function createTrainRenderer(): BenchTrainRenderer {
  return { name: "v3", session: meta => new Session(meta) };
}
