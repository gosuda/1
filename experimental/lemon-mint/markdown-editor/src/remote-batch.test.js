'use strict';

import { test } from "node:test";
import assert from "node:assert/strict";
import { RemoteBatcher, MAX_WAIT } from "./remote-batch.js";

// A fake clock: applying costs `cost` ms and rendering `render` ms, timers
// run when the clock passes them.
function clock() {
  const c = {
    t: 0,
    timers: [],
    setTimer(f, ms) {
      const timer = { at: c.t + ms, f };
      c.timers.push(timer);
      return timer;
    },
    clearTimer(timer) {
      c.timers = c.timers.filter((x) => x !== timer);
    },
    // advance moves the clock to t + ms, running due timers in order.
    advance(ms) {
      const end = c.t + ms;
      for (;;) {
        c.timers.sort((a, b) => a.at - b.at);
        const next = c.timers[0];
        if (!next || next.at > end) break;
        c.timers.shift();
        c.t = Math.max(c.t, next.at);
        next.f();
      }
      c.t = Math.max(c.t, end);
    },
  };
  return c;
}

function batcher(c, { cost = 0, render = 0 } = {}) {
  const batches = [];
  const b = new RemoteBatcher(
    (updates) => {
      batches.push({ at: c.t, updates });
      c.t += typeof cost === "function" ? cost(updates) : cost;
    },
    {
      now: () => c.t,
      setTimer: c.setTimer,
      clearTimer: c.clearTimer,
      afterRender: (done) =>
        c.setTimer(() => {
          c.t += render;
          done(render);
        }, 0),
    },
  );
  return { b, batches };
}

test("an idle page applies a remote update at once", () => {
  const c = clock(),
    { b, batches } = batcher(c);
  b.push("a");
  assert.deepEqual(batches, [{ at: 0, updates: ["a"] }]);
  c.advance(0);
  assert.equal(b.state, "idle");
  b.push("b");
  assert.equal(batches.length, 2);
});

test("batches wait as long as the last one took to apply and render", () => {
  const c = clock(),
    { b, batches } = batcher(c, { cost: 30, render: 20 });
  b.push("u0");
  // Updates keep arriving every 5 ms for two seconds.
  for (let i = 1; i < 400; i++) {
    c.advance(5);
    b.push("u" + i);
  }
  c.advance(2000);
  // Nothing is lost, order is kept, and every batch after the first holds
  // what arrived while the page applied, rendered and paused.
  assert.deepEqual(
    batches.flatMap((x) => x.updates),
    Array.from({ length: 400 }, (_, i) => "u" + i),
  );
  assert.ok(batches.length < 400 / 5, `${batches.length} batches`);
  // Between the end of one batch's rendering and the start of the next,
  // the page waits at least that batch's cost (30 + 20 ms).
  for (let i = 1; i < batches.length; i++)
    assert.ok(
      batches[i].at - batches[i - 1].at >= 30 + 20 + 50,
      `batch ${i} at ${batches[i].at} after ${batches[i - 1].at}`,
    );
  assert.equal(b.lastCost, 50);
  assert.equal(b.state, "idle");
});

test("the wait follows the measured cost and stops at one second", () => {
  const c = clock();
  // A batch costs 400 ms per update, rendering nothing.
  const { b, batches } = batcher(c, { cost: (u) => u.length * 400 });
  b.push("first");
  for (let i = 0; i < 10; i++) b.push("x" + i);
  // first cost 400 ms; the ten others wait 400 ms, then cost 4000 ms.
  c.advance(400);
  assert.equal(batches.length, 2);
  assert.equal(batches[1].at, 800);
  assert.equal(batches[1].updates.length, 10);
  // The second batch took 4000 ms: the clock is at 4800.
  assert.equal(c.t, 4800);
  b.push("late");
  c.advance(MAX_WAIT - 1);
  assert.equal(batches.length, 2, "waits at most a second, not 4 s");
  c.advance(1);
  assert.equal(batches.length, 3);
  assert.equal(batches[2].at, 800 + 4000 + MAX_WAIT);
});

test("a batch too cheap to measure lets the next apply at once", () => {
  const c = clock(),
    { b, batches } = batcher(c, { cost: 0.2 });
  b.push("a");
  c.advance(0);
  b.push("b");
  assert.equal(batches.length, 2);
});

test("flush applies the queue now; destroy drops it", () => {
  const c = clock(),
    { b, batches } = batcher(c, { cost: 100 });
  b.push("a");
  b.push("b");
  b.push("c");
  assert.equal(batches.length, 1);
  b.flush();
  assert.deepEqual(batches[1].updates, ["b", "c"]);
  // The stale render and timer of the first batch do nothing; the flushed
  // batch's pause (100 ms) ends with nothing queued.
  c.advance(150);
  assert.equal(batches.length, 2);
  assert.equal(b.state, "idle");
  b.push("d");
  assert.equal(batches.length, 3);
  b.push("e");
  b.destroy();
  c.advance(5000);
  assert.equal(batches.length, 3);
  b.push("f");
  assert.equal(batches.length, 3);
});
