import test from "node:test";
import assert from "node:assert/strict";
import { loadIconAtlas } from "./view.ts";
import type { Catalog, DataSource } from "../api/types";

test("icons: the spritesheet and its metadata are fetched with the source's authorization", async () => {
  const seen: { url: string; auth?: string }[] = [];
  const g = globalThis as Record<string, unknown>;
  const saved = { fetch: g.fetch, createImageBitmap: g.createImageBitmap, location: g.location };
  g.location = { href: "http://host/api/timeshade/" };
  g.fetch = async (url: URL, init?: RequestInit) => {
    seen.push({ url: String(url), auth: (init?.headers as Record<string, string> | undefined)?.Authorization });
    if (String(url).endsWith(".json")) {
      return new Response(JSON.stringify({ item: [{ name: "iron-plate", icon: { x: 1, y: 2, size: 32 } }], "virtual-signal": [{ name: "signal-A", icon: { x: 3, y: 4, size: 32 } }] }));
    }
    return new Response(new Uint8Array([1, 2, 3]));
  };
  g.createImageBitmap = async () => ({ width: 64, height: 64 });
  try {
    const source = { base: "/api/timeshade/api/v3/", requestHeaders: () => ({ Authorization: "Bearer view" }) } as unknown as DataSource;
    const catalog = { icons: { png: "icons/sheet.png", json: "icons/meta.json" } } as Catalog;
    const atlas = await loadIconAtlas(source, catalog);
    assert.ok(atlas);
    assert.deepEqual(atlas.sprites.get("item/iron-plate"), { x: 1, y: 2, size: 32 });
    assert.deepEqual(atlas.sprites.get("virtual/signal-A"), { x: 3, y: 4, size: 32 });
    assert.deepEqual(seen.map(s => s.url).sort(), ["http://host/api/timeshade/api/v3/icons/meta.json", "http://host/api/timeshade/api/v3/icons/sheet.png"]);
    assert.ok(seen.every(s => s.auth === "Bearer view"));
  } finally {
    Object.assign(g, saved);
  }
});
