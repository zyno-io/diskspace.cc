// SPDX-License-Identifier: MIT
import { describe, expect, test } from "vitest";
import { generateTree } from "../src/protocol/mock";
import { TreeStore } from "../src/store/tree";
import type { NodeBatch, NodeRecord, ScanDone, ScanEnded } from "../src/protocol/types";
import { BUDGET_DEFAULT, BUDGET_HIDDEN, BUDGET_INPUT, BUDGET_MAX, BUDGET_MIN, IngestScheduler, type IngestEnv, type IngestHandlers } from "../src/ui/ingest";

/** manual clock + frame driver */
class FakeEnv implements IngestEnv {
  t = 0;
  cb: ((ts: number | null) => void) | null = null;
  now = () => this.t;
  request(cb: (ts: number | null) => void): void {
    this.cb = cb;
  }
  /** fire the pending callback as a frame at the current time */
  frame(hidden = false): boolean {
    const cb = this.cb;
    this.cb = null;
    if (!cb) return false;
    cb(hidden ? null : this.t);
    return true;
  }
}

const batch = (n: number, scanId = 1, wire = 100): NodeBatch => ({ scanId, wireBytes: wire, records: Array.from({ length: n }, () => ({ k: 3, targetId: 0 }) as unknown as NodeRecord) });

function harness(msPerRecord = 0.01) {
  const env = new FakeEnv();
  const log: string[] = [];
  const applied: number[] = [];
  const h: IngestHandlers = {
    applySlice(b, from, deadline) {
      let i = from;
      let c = 0;
      while (i < b.records.length) {
        i++;
        env.t += msPerRecord;
        if ((++c & 63) === 0 && i < b.records.length && env.t >= deadline) break;
      }
      log.push(`slice:${b.wireBytes}:${from}-${i}`);
      return i;
    },
    batchDone: (b) => applied.push(b.wireBytes),
    done: (m) => log.push(`done:${m.scanId}`),
    ended: (m) => log.push(`ended:${m.scanId}`),
    replayDone: () => log.push("replayDone"),
    tick: () => log.push("tick"),
  };
  const s = new IngestScheduler(h, env);
  return { env, s, log, applied };
}

describe("IngestScheduler", () => {
  test("respects the frame budget and resumes a partial batch", () => {
    const { env, s, log, applied } = harness(0.01); // 100 records/ms
    s.pushBatch(batch(2000, 1, 111)); // 20 ms of work
    expect(env.frame()).toBe(true);
    const first = log.filter((l) => l.startsWith("slice"));
    expect(first.length).toBe(1);
    const stop = Number(first[0].split("-")[1]);
    expect(stop).toBeGreaterThan(0);
    expect(stop).toBeLessThan(2000); // did not finish in one slice
    expect(env.t).toBeLessThanOrEqual(BUDGET_DEFAULT + 1); // within budget (+ one 64-record chunk)
    expect(applied).toEqual([]); // not credited until fully applied
    expect(s.backlog).toBe(1);
    let frames = 1;
    while (env.frame()) {
      frames++;
      env.t += 16;
      expect(frames).toBeLessThan(50);
    }
    expect(frames).toBeGreaterThan(2);
    expect(applied).toEqual([111]);
    expect(s.backlog).toBe(0);
    // slices tile the batch without gaps
    let at = 0;
    for (const l of log.filter((x) => x.startsWith("slice"))) {
      const [a, b] = l.split(":")[2].split("-").map(Number);
      expect(a).toBe(at);
      at = b;
    }
    expect(at).toBe(2000);
  });

  test("credits only after the whole batch; each batch once, in order", () => {
    const { env, s, applied } = harness(0.001);
    s.pushBatch(batch(100, 1, 1));
    s.pushBatch(batch(100, 1, 2));
    s.pushBatch(batch(100, 1, 3));
    env.frame();
    expect(applied).toEqual([1, 2, 3]);
  });

  test("control items stay ordered behind queued batches", () => {
    const { env, s, log } = harness(0.01);
    s.pushBatch(batch(1500, 1, 7));
    s.pushDone({ scanId: 1 } as ScanDone);
    s.pushEnded({ scanId: 1 } as ScanEnded);
    s.pushReplayDone();
    while (env.frame()) env.t += 16;
    const order = log.filter((l) => !l.startsWith("tick") && !l.startsWith("slice"));
    expect(order).toEqual(["done:1", "ended:1", "replayDone"]);
    const lastSlice = log.map((l, i) => (l.startsWith("slice") ? i : -1)).reduce((a, b) => Math.max(a, b));
    expect(log.indexOf("done:1")).toBeGreaterThan(lastSlice);
  });

  test("input shrinks the budget; slow frames shrink it, fast frames grow it back", () => {
    const { env, s } = harness(0.01);
    const big = batch(100_000);
    s.pushBatch(big);
    env.frame(); // first frame: no dt yet
    expect(s.lastBudget).toBe(BUDGET_DEFAULT);
    env.t += 16;
    s.noteInput();
    env.frame();
    expect(s.lastBudget).toBe(BUDGET_INPUT);
    env.t += 400; // input window over, and a very slow frame
    env.frame();
    expect(s.lastBudget).toBeCloseTo((BUDGET_DEFAULT + 0.5) * 0.7, 5); // grew to 6.5 on the previous frame
    for (let i = 0; i < 20; i++) {
      env.t += 40; // consistently slow frames floor at the minimum
      if (s.backlog === 0) s.pushBatch(big);
      env.frame();
    }
    expect(s.lastBudget).toBe(BUDGET_MIN);
    for (let i = 0; i < 40; i++) {
      env.t += 8; // slice (<= 8 ms) + 8 ms idle stays under the slow-frame limit
      if (s.backlog === 0) s.pushBatch(big);
      env.frame();
    }
    expect(s.lastBudget).toBe(BUDGET_MAX);
  });

  test("hidden page gets the long slice", () => {
    const { env, s } = harness(0.01);
    s.pushBatch(batch(100_000));
    env.frame(true);
    expect(s.lastBudget).toBe(BUDGET_HIDDEN);
  });

  test("discard drops matching queued batches but still reports them applied", () => {
    const { env, s, applied } = harness();
    s.pushBatch(batch(10, 1, 5));
    s.pushBatch(batch(10, 2, 6));
    s.discard((b) => b.scanId === 1);
    expect(applied).toEqual([5]);
    env.frame();
    expect(applied).toEqual([5, 6]);
  });
});

describe("TreeStore.applySlice", () => {
  test("slicing a batch is equivalent to ingest", () => {
    const g = generateTree(6000, { root: "home", seed: 11 });
    const started = { scanId: 1, path: "/x", rootName: "x", volume: null, opts: {} };
    const a = new TreeStore();
    a.reset(started);
    a.ingest({ scanId: 1, records: g.records, wireBytes: 0 });
    const b = new TreeStore();
    b.reset(started);
    const batchB: NodeBatch = { scanId: 1, records: g.records, wireBytes: 0 };
    let from = 0;
    let slices = 0;
    let clock = 0;
    while (from < g.records.length) {
      from = b.applySlice(batchB, from, clock + 0.5, () => (clock += 0.05));
      slices++;
    }
    expect(slices).toBeGreaterThan(3);
    expect(b.n).toBe(a.n);
    for (let i = 0; i < a.n; i++) {
      expect(b.totAlloc[i]).toBe(a.totAlloc[i]);
      expect(b.kind[i]).toBe(a.kind[i]);
      expect(b.unsettled[i]).toBe(a.unsettled[i]);
    }
    expect(b.rootSettled()).toBe(a.rootSettled());
    expect(b.orphanCount()).toBe(a.orphanCount());
  });

  test("stale scan id is skipped and reported finished", () => {
    const s = new TreeStore();
    s.reset({ scanId: 2, path: "/x", rootName: "x", volume: null, opts: {} });
    expect(s.applySlice({ scanId: 1, records: [{} as NodeRecord, {} as NodeRecord], wireBytes: 0 }, 0, 0)).toBe(2);
  });

  test("touched propagates to every ancestor and stamps new nodes", () => {
    const s = new TreeStore();
    s.reset({ scanId: 1, path: "/x", rootName: "x", volume: null, opts: {} });
    const dir = (id: number, p: Record<string, unknown>) => ({ k: 1, id, flags: 0, ownAlloc: 0, ownApparent: 0, ownFiles: 0, entryErrors: 0, firstChildId: 0, dirs: [], files: [], totalDirs: 0, ...p }) as unknown as NodeRecord;
    s.ingest({ scanId: 1, wireBytes: 0, records: [dir(0, { firstChildId: 1, totalDirs: 1, dirs: [{ name: "a", flags: 0 }] }), dir(1, { firstChildId: 2, totalDirs: 2, dirs: [{ name: "b", flags: 0 }, { name: "c", flags: 0 }] })] });
    const v = s.version;
    const root = s.root, a = s.dStart[root], b = s.dStart[a], c = b + 1;
    s.ingest({ scanId: 1, wireBytes: 0, records: [dir(2, { ownAlloc: 5000, ownApparent: 5000, ownFiles: 1 })] });
    expect(s.touched[b]).toBeGreaterThan(v); // changed
    expect(s.touched[a]).toBeGreaterThan(v); // ancestors
    expect(s.touched[root]).toBeGreaterThan(v);
    expect(s.touched[c]).toBeLessThanOrEqual(v); // sibling untouched
  });
});
