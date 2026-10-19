// The layer list and the layers against a fake engine context and a recording canvas.
import "../movers/eval/ts-resolve.mjs";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const { layers, createLayers } = await import("./index.ts");

const dir = new URL("../../fixtures/", import.meta.url);
const bin = (name: string): ArrayBuffer => {
  const b = readFileSync(new URL(name, dir));
  return b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength) as ArrayBuffer;
};
const catalog = JSON.parse(readFileSync(new URL("catalog.json", dir), "utf8"));
const overlays = JSON.parse(readFileSync(new URL("overlays_0_0.json", dir), "utf8"));

function canvas() {
  const calls: string[] = [];
  return new Proxy({ calls } as any, {
    get(target, prop) {
      if (prop in target) {
        return target[prop];
      }
      return (...args: unknown[]) => {
        calls.push(String(prop));
        return undefined;
      };
    },
    set(target, prop, value) {
      target[prop] = value;
      return true;
    },
  });
}

function context(redraws: { n: number }) {
  const map = catalog.maps[0];
  const surface = map.surfaces[0];
  const now = 1791633610000;
  const source: any = {
    kind: "server",
    now: () => now,
    moverLive: async () => bin("movers_live.bin"),
    moverDay: async () => JSON.parse(readFileSync(new URL("movers_day.json", dir), "utf8")),
    moverWindow: async () => bin("movers_window.bin"),
    moverKeyframe: async () => bin("movers_keyframe.bin"),
    overlays: async () => overlays,
  };
  return {
    source,
    catalog,
    map,
    surface,
    datastore: surface.datastores[0],
    requestRedraw: () => redraws.n++,
    icons: async () => null,
  };
}

function frame(t: number, live: boolean, scale = 4): any {
  const camera = { x: -100, y: -150, scale };
  const viewport = { width: 800, height: 600, dpr: 1 };
  const hw = viewport.width / 2 / scale;
  const hh = viewport.height / 2 / scale;
  return {
    camera,
    viewport,
    z: 0,
    time: t,
    live,
    now: 0,
    dt: 16,
    visible: [camera.x - hw, camera.y - hh, camera.x + hw, camera.y + hh],
    worldToScreen: (x: number, y: number) => [(x - camera.x) * scale + 400, (y - camera.y) * scale + 300],
    screenToWorld: (x: number, y: number) => [(x - 400) / scale + camera.x, (y - 300) / scale + camera.y],
  };
}

test("layer ids are unique and the default set is drawn in order", () => {
  assert.equal(new Set(layers.map(l => l.id)).size, layers.length);
  const made = createLayers();
  assert.deepEqual(made.map(l => l.id), ["edges", "recipes", "tags", "movers"]);
  assert.deepEqual(createLayers(new Set(["rails", "movers"])).map(l => l.id), ["rails", "movers"]);
});

test("movers layer: history at the fixture time draws the train and spidertron", async () => {
  const redraws = { n: 0 };
  const [movers] = createLayers(new Set(["movers"]));
  movers.attach(context(redraws) as any);
  const t = 1791630003000;
  movers.setTime!(t, false);
  for (let i = 0; i < 10; i++) {
    await new Promise(r => setTimeout(r, 0));
  }
  const c = canvas();
  movers.draw2d!(c, frame(t, false, 1));
  assert.ok(c.calls.includes("fill"), "something was drawn");
  assert.ok(redraws.n > 0);
  assert.equal(typeof movers.delay?.(), "number");
  // Paused: the same time again draws the same frame and asks for no more.
  const n = redraws.n;
  movers.draw2d!(canvas(), frame(t, false, 1));
  movers.draw2d!(canvas(), frame(t, false, 1));
  assert.equal(redraws.n, n, "a paused history view stops animating");
  movers.draw2d!(canvas(), frame(t + 16, false, 1));
  assert.equal(redraws.n, n + 1, "playing on, the moving train asks for the next frame");
  movers.detach();
});

test("movers layer: live draws behind the data clock", async () => {
  const redraws = { n: 0 };
  const [movers] = createLayers(new Set(["movers"]));
  const ctx = context(redraws);
  movers.attach(ctx as any);
  movers.setTime!(ctx.source.now(), true);
  await new Promise(r => setTimeout(r, 0));
  const c = canvas();
  movers.draw2d!(c, frame(ctx.source.now(), true, 1));
  assert.ok(movers.delay!() >= 500);
  movers.detach();
});

test("tags layer draws the tag text through fillText; hidden when zoomed out", async () => {
  const redraws = { n: 0 };
  const [tags] = createLayers(new Set(["tags"]));
  tags.attach(context(redraws) as any);
  const t = 1791630010000;
  const c0 = canvas();
  tags.draw2d!(c0, frame(t, false));
  await new Promise(r => setTimeout(r, 0));
  const c = canvas();
  const texts: string[] = [];
  c.fillText = (s: string) => texts.push(s);
  tags.draw2d!(c, frame(t, false));
  assert.deepEqual(texts, ["iron-ore 21M"], "no atlas: the icon's name, then the text");
  const far = canvas();
  const farTexts: string[] = [];
  far.fillText = (s: string) => farTexts.push(s);
  tags.draw2d!(far, frame(t, false, 0.1));
  assert.deepEqual(farTexts, []);
  tags.detach();
});

test("edges and instances layers draw catalog geometry", () => {
  const [inst, edges] = createLayers(new Set(["edges", "instances"]));
  const ctx = context({ n: 0 });
  edges.attach(ctx as any);
  inst.attach(ctx as any);
  const c = canvas();
  edges.draw2d!(c, frame(0, false));
  inst.draw2d!(c, frame(0, false));
  assert.ok(c.calls.includes("lineTo"));
  assert.equal(c.calls.filter((x: string) => x === "strokeRect").length, 2);
});
