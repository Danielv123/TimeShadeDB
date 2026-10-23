// Reference renderers that draw only the head: linear interpolation in time (a chord)
// and cubic Hermite with tangents v*dt (PLAN.md section 4.8 step 2). They reproduce the
// research numbers (research/illisis-viewer.md 2.3) and sanity-check the evaluator; the
// head is held at the newest known sample (no extrapolation).

import type { JourneyMeta, Pose, TrainRenderer, TrainSample, TrainSession } from "../types.ts";

type Mode = "linear" | "hermite";

function velocity(evs: TrainSample[], i: number): [number, number] {
  const e = evs[i];
  if (e.vx !== 0 || e.vy !== 0) {
    return [e.vx, e.vy];
  }
  // No captured velocity: central difference of the neighbours (Catmull-Rom).
  const a = evs[i - 1] && !evs[i - 1].gone ? evs[i - 1] : e;
  const b = evs[i + 1] && !evs[i + 1].gone ? evs[i + 1] : e;
  const dt = (b.t - a.t) / 1000;
  return dt > 0 ? [(b.x - a.x) / dt, (b.y - a.y) / dt] : [0, 0];
}

class Session implements TrainSession {
  private evs: TrainSample[] = [];
  private i = 0;
  private mode: Mode;
  constructor(mode: Mode) {
    this.mode = mode;
  }
  feed(s: TrainSample): void {
    const last = this.evs[this.evs.length - 1];
    if (last && last.t === s.t) {
      this.evs[this.evs.length - 1] = s; // same time: the later one wins
    } else {
      this.evs.push(s);
    }
  }
  pose(t: number): Pose | undefined {
    const evs = this.evs;
    if (this.i >= evs.length) {
      this.i = Math.max(0, evs.length - 1);
    }
    while (this.i > 0 && evs[this.i].t > t) {
      this.i--;
    }
    while (this.i + 1 < evs.length && evs[this.i + 1].t <= t) {
      this.i++;
    }
    const a = evs[this.i];
    if (!a || a.t > t || a.gone) {
      return undefined;
    }
    const c = evs[this.i + 1];
    let x = a.x;
    let y = a.y;
    if (c && !c.gone && c.t > a.t) {
      const T = (c.t - a.t) / 1000;
      const u = (t - a.t) / (c.t - a.t);
      if (this.mode === "linear") {
        x = a.x + (c.x - a.x) * u;
        y = a.y + (c.y - a.y) * u;
      } else {
        const [avx, avy] = velocity(evs, this.i);
        const [cvx, cvy] = velocity(evs, this.i + 1);
        const h00 = 2 * u ** 3 - 3 * u ** 2 + 1, h10 = u ** 3 - 2 * u ** 2 + u;
        const h01 = -2 * u ** 3 + 3 * u ** 2, h11 = u ** 3 - u ** 2;
        x = h00 * a.x + h10 * T * avx + h01 * c.x + h11 * T * cvx;
        y = h00 * a.y + h10 * T * avy + h01 * c.y + h11 * T * cvy;
      }
    }
    return { x, y, cars: [{ x, y }] };
  }
}

export function referenceRenderer(mode: Mode): TrainRenderer {
  return { name: mode, session: (_meta: JourneyMeta) => new Session(mode) };
}
