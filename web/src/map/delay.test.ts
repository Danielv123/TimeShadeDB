import assert from "node:assert/strict";
import { test } from "node:test";
import { defaultLiveDelay, LIVE_DELAY_SLEW, LIVE_DELAY_SLEW_DOWN, LiveDelay } from "./delay.ts";

test("live delay: the default matches the movers layer's initial delay", () => {
  assert.equal(defaultLiveDelay({ static: false, live_ms: 5000 }), 2000);
  assert.equal(defaultLiveDelay({ static: true, live_ms: 5000 }), 10_000);
  assert.equal(defaultLiveDelay({ static: true, live_ms: 0 }), 10_000);
});

test("live delay: target changes are slewed, a reset jumps", () => {
  const d = new LiveDelay();
  assert.equal(d.next(2000, 0), 2000);
  // A layer reporting 10 s attaches: the delay grows by at most LIVE_DELAY_SLEW per ms.
  assert.equal(d.next(10_000, 1000), 2000 + 1000 * LIVE_DELAY_SLEW);
  assert.equal(d.next(10_000, 100_000), 10_000);
  // Turned off again: it shrinks by at most LIVE_DELAY_SLEW_DOWN per ms.
  assert.equal(d.next(2000, 101_000), 10_000 - 1000 * LIVE_DELAY_SLEW_DOWN);
  assert.equal(d.next(2000, 101_000, true), 2000);
  // Displayed time (now - delay) never runs backwards.
  const e = new LiveDelay();
  let last = -Infinity;
  for (let t = 0; t <= 20_000; t += 16) {
    const shown = t - e.next(t < 5000 ? 2000 : 10_000, t);
    assert.ok(shown >= last);
    last = shown;
  }
});
