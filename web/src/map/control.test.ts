import test from "node:test";
import assert from "node:assert/strict";
import { createControl, type ControlTarget } from "./control.ts";
import type { Camera } from "./layer";
import type { CatalogDatastore } from "../api/types";

function fakeView(): ControlTarget & { calls: string[]; cam: Camera; t: number } {
  const ds: CatalogDatastore = {
    force: "player",
    id: "gw-x~2E1.nauvis.player",
    window_ms: 60000,
    horizon: 2000,
    first: 1000,
    last: 3000,
    palette_version: 1,
    bounds: [-1, 0, 2, 1],
    levels: [0, -1],
  };
  const f = {
    calls: [] as string[],
    cam: { x: 0, y: 0, scale: 1 } as Camera,
    t: 1500,
    datastore: ds,
    isLive: true,
    isPlaying: false,
    setCamera(c: Camera) {
      f.cam = c;
    },
    setTime(t: number) {
      f.calls.push(`setTime ${t}`);
      f.t = t;
    },
    goLive() {
      f.calls.push("goLive");
    },
    play(speed?: number) {
      f.calls.push(`play ${speed}`);
    },
    pause() {
      f.calls.push("pause");
    },
    range: (): [number, number] => [1000, 3000],
    displayTime: () => f.t,
    state: () => ({ camera: f.cam }),
  };
  return f;
}

test("control adapts TsdbControl calls to MapView", () => {
  const v = fakeView();
  const seeks: (number | undefined)[] = [];
  const c = createControl(v, { size: () => ({ width: 800, height: 600 }), onSeek: t => seeks.push(t) });
  assert.equal(c.ready(), true);
  assert.equal(c.datastore(), "gw-x~2E1.nauvis.player");
  c.setView(100, -50, 16);
  assert.deepEqual(v.cam, { x: 100, y: -50, scale: 1 / 16 });
  assert.deepEqual(c.view(), { x: 100, y: -50, s: 16, width: 800, height: 600 });
  c.seek(2000);
  c.seek();
  c.play(3600);
  c.play(0);
  assert.deepEqual(v.calls, ["setTime 2000", "goLive", "play 3600000", "pause"]);
  assert.deepEqual(seeks, [2000, undefined]);
  assert.equal(c.time(), 2000);
  assert.deepEqual(c.range(), [1000, 3000]);
  // z0 tiles -1..2 x 0..1, 512 world tiles each, grid origin -256.
  assert.deepEqual(c.surfaceBounds(), [-768, -256, 1280, 768]);
});
