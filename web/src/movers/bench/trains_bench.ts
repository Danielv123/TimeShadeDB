// Browser benchmark for the movers layer (WP8 acceptance "500 trains at 60 fps"): N trains
// of five carriages circling loops of different radii, sampled every ~0.4 s, evaluated
// (MoverView.frame) and drawn (drawMovers) on a 1920x1080 2D canvas every animation frame,
// all on screen. Results go to window.__trainsBench for the runner (bench/run.mjs).
//
// Bundle: esbuild src/movers/bench/trains_bench.ts --bundle --format=esm (see run.mjs).

import { drawMovers } from "../draw";
import { MoverView } from "../view";
import { makeSample } from "../track";
import { vectorOrientation } from "../hermite";

interface BenchResult {
  trains: number;
  frames: number;
  seconds: number;
  fps: number;
  /** JS time per frame for evaluating and drawing the movers (ms). */
  p50: number;
  p95: number;
  max: number;
  /** Median ms of the two parts: evaluating movers, and drawing them. */
  frameP50: number;
  drawP50: number;
  carriages: number;
}

declare global {
  interface Window {
    __trainsBench?: BenchResult | { error: string };
  }
}

function build(view: MoverView, trains: number, seconds: number): void {
  for (let i = 0; i < trains; i++) {
    const meta = { id: `t${i}`, k: "t" as const, f: "player", name: "locomotive", n: 5, o: "LCCCF" };
    const R = 20 + (i % 13) * 6;
    const cx = ((i % 25) - 12) * 70;
    const cy = (Math.floor(i / 25) - 10) * 50;
    const v = 15 + (i % 7) * 4;
    const phase = (i * 0.37) % (2 * Math.PI);
    for (let t = -10_000; t <= seconds * 1000 + 5000; t += 400) {
      const s = (v * t) / 1000;
      const phi = phase + s / R;
      const lag = phi - 3.5 / R;
      view.add(meta, makeSample(meta, t, cx + R * Math.cos(phi), cy + R * Math.sin(phi), vectorOrientation(-Math.sin(lag), Math.cos(lag))));
    }
  }
}

export async function run(trains = 500, seconds = 5): Promise<BenchResult> {
  const canvas = document.createElement("canvas");
  canvas.width = 1920;
  canvas.height = 1080;
  document.body.appendChild(canvas);
  const ctx = canvas.getContext("2d")!;
  const view = new MoverView();
  build(view, trains, seconds);
  const proj = { cx: 0, cy: 0, scale: 1.0, width: 1920, height: 1080 };
  const costs: number[] = [];
  const evals: number[] = [];
  const draws: number[] = [];
  let carriages = 0;
  const start = performance.now();
  let frames = 0;
  await new Promise<void>(resolve => {
    const step = (now: number) => {
      const t = now - start;
      const t0 = performance.now();
      ctx.fillStyle = "#101010";
      ctx.fillRect(0, 0, 1920, 1080);
      const shown = view.frame(t, Infinity, { bodies: true });
      const t1 = performance.now();
      drawMovers(ctx, proj, shown);
      const t2 = performance.now();
      costs.push(t2 - t0);
      evals.push(t1 - t0);
      draws.push(t2 - t1);
      carriages = shown.reduce((a, m) => a + m.cars.length, 0);
      frames++;
      if (t < seconds * 1000) {
        requestAnimationFrame(step);
      } else {
        resolve();
      }
    };
    requestAnimationFrame(step);
  });
  const elapsed = (performance.now() - start) / 1000;
  costs.sort((a, b) => a - b);
  const q = (p: number) => costs[Math.min(costs.length - 1, Math.floor(p * (costs.length - 1)))];
  const med = (a: number[]) => a.sort((x, y) => x - y)[Math.floor(a.length / 2)];
  return { trains, frames, seconds: elapsed, fps: frames / elapsed, p50: q(0.5), p95: q(0.95), max: costs[costs.length - 1], frameP50: med(evals), drawP50: med(draws), carriages };
}

const params = new URLSearchParams(location.search);
run(Number(params.get("trains") ?? 500), Number(params.get("seconds") ?? 5)).then(
  r => (window.__trainsBench = r),
  e => (window.__trainsBench = { error: String(e) }),
);
