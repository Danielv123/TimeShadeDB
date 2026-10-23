import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { illisisCacheControl, serve, v3CacheControl } from "../server.mjs";
import { summarize } from "../stats.mjs";

test("percentiles match the Go harness (nearest rank)", () => {
  const s = summarize(Array.from({ length: 100 }, (_, i) => i + 1));
  assert.equal(s.p50, 50);
  assert.equal(s.p95, 95);
  assert.equal(s.max, 100);
  assert.deepEqual(summarize([]), { n: 0 });
});

test("cache rules", () => {
  assert.match(illisisCacheControl("gw/nauvis/player/0/0_0/123.c.gz"), /immutable/);
  assert.match(illisisCacheControl("gw/nauvis/player/0/0_0/index.json.gz"), /max-age=60/);
  assert.match(illisisCacheControl("gw/nauvis/live.json.gz"), /max-age=5/);
  assert.match(v3CacheControl("tile/a.b.c/0/1/2/3.bin"), /immutable/);
  assert.match(v3CacheControl("catalog.json"), /max-age=10/);
});

test("static server serves files, refuses traversal, avoids reserved ports", async () => {
  const dir = mkdtempSync(join(tmpdir(), "benchweb-"));
  mkdirSync(join(dir, "a"));
  writeFileSync(join(dir, "a", "x.c.gz"), "hello");
  const s = await serve({ root: dir, cacheControl: illisisCacheControl });
  try {
    assert.ok(![8880, 4443, 8080].includes(Number(new URL(s.url).port)));
    const r = await fetch(s.url + "/a/x.c.gz");
    assert.equal(r.status, 200);
    assert.equal(await r.text(), "hello");
    assert.match(r.headers.get("cache-control"), /immutable/);
    assert.equal((await fetch(s.url + "/a/missing")).status, 404);
    assert.notEqual((await fetch(s.url + "/..%2F..%2Fetc%2Fpasswd")).status, 200);
    assert.equal(s.stats.requests, 1);
  } finally {
    await s.close();
  }
});
