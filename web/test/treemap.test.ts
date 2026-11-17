// SPDX-License-Identifier: MIT
import { describe, expect, test } from "vitest";
import { generateTree } from "../src/protocol/mock";
import { TreeStore } from "../src/store/tree";
import { computeLayout, DEFAULT_LAYOUT, IT_DIR, IT_REST, squarify } from "../src/viz/layout";

function store(n = 8000): TreeStore {
  const g = generateTree(n, { root: "home", seed: 3 });
  const s = new TreeStore();
  s.reset({ scanId: 1, path: "/Users/alex", rootName: "alex", volume: null, opts: {} });
  s.ingest({ scanId: 1, records: g.records, wireBytes: 0 });
  return s;
}

describe("squarify", () => {
  test("tiles the rect exactly with areas in proportion", () => {
    const areas = [600, 300, 200, 100, 50, 30, 20].map((a) => (a / 1300) * 800 * 500);
    const n = areas.length;
    const x = new Float64Array(n), y = new Float64Array(n), w = new Float64Array(n), h = new Float64Array(n);
    squarify(areas, n, 0, 0, 800, 500, x, y, w, h);
    let sum = 0;
    for (let i = 0; i < n; i++) {
      expect(w[i] * h[i]).toBeCloseTo(areas[i], 3);
      expect(x[i]).toBeGreaterThanOrEqual(-1e-9);
      expect(y[i]).toBeGreaterThanOrEqual(-1e-9);
      expect(x[i] + w[i]).toBeLessThanOrEqual(800 + 1e-6);
      expect(y[i] + h[i]).toBeLessThanOrEqual(500 + 1e-6);
      sum += w[i] * h[i];
      for (let j = 0; j < i; j++) {
        const overlap = Math.max(0, Math.min(x[i] + w[i], x[j] + w[j]) - Math.max(x[i], x[j])) * Math.max(0, Math.min(y[i] + h[i], y[j] + h[j]) - Math.max(y[i], y[j]));
        expect(overlap).toBeLessThan(1e-6);
      }
    }
    expect(sum).toBeCloseTo(800 * 500, 3);
  });

  test("keeps aspect ratios reasonable", () => {
    const areas = Array.from({ length: 40 }, (_, i) => 1000 / (i + 1));
    const tot = areas.reduce((a, b) => a + b, 0);
    const sc = areas.map((a) => (a / tot) * 1200 * 800);
    const n = sc.length;
    const x = new Float64Array(n), y = new Float64Array(n), w = new Float64Array(n), h = new Float64Array(n);
    squarify(sc, n, 0, 0, 1200, 800, x, y, w, h);
    for (let i = 0; i < 10; i++) expect(Math.max(w[i] / h[i], h[i] / w[i])).toBeLessThan(4);
  });
});

describe("computeLayout", () => {
  const s = store();
  const L = computeLayout(s, s.root, 1200, 800);

  test("emits parcels in pre-order, nested inside their parents", () => {
    expect(L.count).toBeGreaterThan(100);
    const stack: number[] = [];
    for (let i = 0; i < L.count; i++) {
      expect(L.x[i]).toBeGreaterThanOrEqual(0);
      expect(L.y[i]).toBeGreaterThanOrEqual(0);
      expect(L.x[i] + L.w[i]).toBeLessThanOrEqual(1200);
      expect(L.y[i] + L.h[i]).toBeLessThanOrEqual(800);
      while (stack.length && L.depth[stack[stack.length - 1]] >= L.depth[i]) stack.pop();
      const p = stack[stack.length - 1];
      if (p !== undefined && L.depth[i] > 0) {
        expect(L.x[i]).toBeGreaterThanOrEqual(L.x[p]);
        expect(L.y[i]).toBeGreaterThanOrEqual(L.y[p] + L.head[p]);
        expect(L.x[i] + L.w[i]).toBeLessThanOrEqual(L.x[p] + L.w[p] + 1e-6);
        expect(L.y[i] + L.h[i]).toBeLessThanOrEqual(L.y[p] + L.h[p] + 1e-6);
      }
      if (L.type[i] === IT_DIR) stack.push(i);
    }
  });

  test("respects min parcel size and max depth", () => {
    for (let i = 0; i < L.count; i++) {
      if (L.type[i] !== IT_REST) {
        expect(L.w[i]).toBeGreaterThanOrEqual(DEFAULT_LAYOUT.minPx);
        expect(L.h[i]).toBeGreaterThanOrEqual(DEFAULT_LAYOUT.minPx);
      }
      expect(L.depth[i]).toBeLessThan(DEFAULT_LAYOUT.maxDepth);
    }
  });

  test("top-level areas are proportional to size", () => {
    const top: number[] = [];
    for (let i = 0; i < L.count; i++) if (L.depth[i] === 0 && L.type[i] !== IT_REST) top.push(i);
    const total = s.size(s.root);
    for (const i of top.slice(0, 5)) {
      const want = (s.size(L.node[i]) / total) * 1200 * 800;
      // snapping + lot lines cost a little area
      expect((L.w[i] + 1) * (L.h[i] + 1)).toBeGreaterThan(want * 0.9);
      expect((L.w[i] + 1) * (L.h[i] + 1)).toBeLessThan(want * 1.1 + 400);
    }
  });

  test("hit-testing returns the deepest parcel", () => {
    let deepest = 0;
    for (let i = 1; i < L.count; i++) if (L.depth[i] > L.depth[deepest] && L.type[i] !== IT_REST) deepest = i;
    const hit = L.hit(L.x[deepest] + L.w[deepest] / 2, L.y[deepest] + L.h[deepest] / 2);
    expect(hit).toBe(deepest);
  });

  test("layout of 100k-node tree stays bounded by pixels", () => {
    const big = store(60_000);
    const t0 = performance.now();
    const L2 = computeLayout(big, big.root, 1440, 900);
    const ms = performance.now() - t0;
    expect(L2.count).toBeLessThan((1440 * 900) / (DEFAULT_LAYOUT.minPx * DEFAULT_LAYOUT.minPx));
    expect(ms).toBeLessThan(2000);
  });
});
