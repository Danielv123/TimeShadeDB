// Overlay intervals, the per-tile feed and icon lookup.
import "../movers/eval/ts-resolve.mjs";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const { TileIntervals, openAt } = await import("./intervals.ts");
const { OverlayFeed, MAX_VIEW_TILES } = await import("./feed.ts");
const { iconIndex, recipeIcon } = await import("./icons.ts");

const fixture = JSON.parse(readFileSync(new URL("../../fixtures/overlays_0_0.json", import.meta.url), "utf8"));
const T0 = 1791630000000;

test("intervals open at a time; a tag's two versions meet at the edit", () => {
  const ti = new TileIntervals(fixture.version, fixture.intervals);
  const tags = new Set(["tag" as const]);
  const at = (t: number) => ti.at(t, tags, []).map(i => i.text);
  assert.deepEqual(at(T0 + 500), []);
  assert.deepEqual(at(T0 + 10_000), ["21M"]);
  assert.deepEqual(at(T0 + 30_000), ["12M"], "the edit's stamp belongs to the new version");
  assert.deepEqual(at(T0 + 9e9), ["12M"], "open versions stay open");
  const rails = ti.at(T0 + 10_000, new Set(["rail" as const]), []);
  assert.equal(rails.length, 1);
  assert.equal(rails[0].name, "straight-rail");
  assert.equal(openAt({ ...fixture.intervals[0], start: 10, end: 20 }, 20), false);
});

test("a closed copy replaces an open one, never the other way", () => {
  const open = { kind: "tag" as const, key: "1/1", instance: 1, start: 100, end: 0, x: 0, y: 0, text: "a" };
  const closed = { ...open, end: 200 };
  assert.equal(new TileIntervals(1, [open, closed]).at(300, new Set(["tag" as const]), []).length, 0);
  assert.equal(new TileIntervals(1, [closed, open]).at(300, new Set(["tag" as const]), []).length, 0);
});

test("the feed fetches each visible tile once, refreshes live tiles, caps the view", async () => {
  const calls: string[] = [];
  let clock = 0;
  const src: any = {
    async overlays(ds: string, x: number, y: number, range: any) {
      calls.push(`${x},${y},${range.kinds.join("+")}`);
      if (x !== 0 || y !== 0) {
        throw new Error("404");
      }
      return fixture;
    },
  };
  let changed = 0;
  const feed = new OverlayFeed(src, "ds", ["tag"], () => changed++, () => clock);
  const kinds = new Set(["tag" as const]);
  // tile (0,0) covers world [-256, 256)
  assert.deepEqual(feed.query([-300, -100, 100, 100], T0 + 10_000, false, kinds), []);
  await new Promise(r => setTimeout(r, 0));
  assert.deepEqual(calls.sort(), ["-1,0,tag", "0,0,tag"]);
  assert.equal(changed, 1);
  const tags = feed.query([-300, -100, 100, 100], T0 + 10_000, false, kinds)!;
  assert.deepEqual(tags.map(t => t.text), ["21M"]);
  assert.equal(calls.length, 2, "cached");
  clock += 61_000;
  feed.query([-300, -100, 100, 100], T0 + 10_000, true, kinds);
  assert.equal(calls.length, 4, "live views refresh after a minute");
  const huge = feed.query([-100_000, -100_000, 100_000, 100_000], T0, false, kinds);
  assert.equal(huge, null);
  assert.ok(MAX_VIEW_TILES >= 16);
});

test("icon index maps virtual signals and finds recipe icons", () => {
  const idx = iconIndex({
    item: [{ name: "iron-plate", icon: { x: 0, y: 0, size: 32 } }],
    "virtual-signal": [{ name: "signal-A", icon: { x: 32, y: 0, size: 32 } }],
    fluid: [{ name: "water", icon: { x: 64, y: 0, size: 32 } }, { name: "no-icon" }],
  });
  assert.ok(idx.has("virtual/signal-A"));
  assert.ok(!idx.has("fluid/no-icon"));
  assert.equal(recipeIcon(idx, "iron-plate"), "item/iron-plate");
  assert.equal(recipeIcon(idx, "water"), "fluid/water");
  assert.equal(recipeIcon(idx, "nothing"), undefined);
  assert.equal(recipeIcon(undefined, "water"), undefined);
});
