// SPDX-License-Identifier: MIT
// nest() finds the "too small to draw" cut with one O(k) pass; it must produce
// exactly the layout of the original full-sort implementation.
import { describe, expect, test } from "vitest";
import { generateTree } from "../src/protocol/mock";
import { TreeStore, KIND_DIR } from "../src/store/tree";
import { computeLayout, DEFAULT_LAYOUT, IT_DIR, IT_LEAF, IT_REST, Layout, squarify, type LayoutOptions } from "../src/viz/layout";

function refLayout(store: TreeStore, root: number, W: number, H: number, o: LayoutOptions): Layout {
  const out = new Layout();
  out.root = root;
  const REST = -2;
  const walk = (parent: number, x: number, y: number, w: number, h: number, depth: number) => {
    const kids: number[] = [];
    if (store.childrenOf(parent, kids) === 0) return;
    const ids: number[] = [], sz: number[] = [];
    let total = 0;
    for (const c of kids) {
      const s = store.size(c);
      if (s > 0) {
        ids.push(c);
        sz.push(s);
        total += s;
      }
    }
    if (total <= 0) return;
    const band = sz.map((v) => Math.floor(Math.log2(v) * 8));
    const order = ids.map((_, i) => i).sort((a, b) => band[b] - band[a] || ids[a] - ids[b]);
    const scale = (w * h) / total;
    const minArea = o.minPx * o.minPx * 1.6;
    let keep = 0, restSum = 0;
    for (let i = 0; i < order.length; i++) {
      if (sz[order[i]] * scale < minArea) {
        for (let j = i; j < order.length; j++) restSum += sz[order[j]];
        break;
      }
      keep++;
    }
    const m = keep + (restSum > 0 ? 1 : 0);
    const areas = new Float64Array(m), nodeOf = new Int32Array(m);
    let k = 0, placed = restSum <= 0;
    for (let i = 0; i < keep; i++) {
      const a = sz[order[i]] * scale;
      if (!placed && restSum * scale >= a) {
        areas[k] = restSum * scale;
        nodeOf[k++] = REST;
        placed = true;
      }
      areas[k] = a;
      nodeOf[k++] = ids[order[i]];
    }
    if (!placed) {
      areas[k] = restSum * scale;
      nodeOf[k++] = REST;
    }
    const rx = new Float64Array(m), ry = new Float64Array(m), rw = new Float64Array(m), rh = new Float64Array(m);
    squarify(areas, m, x, y, w, h, rx, ry, rw, rh);
    for (let i = 0; i < m; i++) {
      const x0 = Math.round(rx[i]), y0 = Math.round(ry[i]);
      const cw = Math.round(rx[i] + rw[i]) - x0 - o.gap, ch = Math.round(ry[i] + rh[i]) - y0 - o.gap;
      const node = nodeOf[i];
      if (node === REST) {
        if (cw >= 2 && ch >= 2) out.push(parent, x0, y0, cw, ch, IT_REST, depth, 0, 0);
        continue;
      }
      const kind = store.kind[node];
      if (cw < o.minPx || ch < o.minPx) continue;
      const canNest = kind === KIND_DIR && depth + 1 < o.maxDepth && cw >= o.nestW && ch >= o.nestH && store.hasChildren(node);
      out.push(node, x0, y0, cw, ch, canNest ? IT_DIR : IT_LEAF, depth, 0, kind);
      if (canNest) walk(node, x0 + 1, y0 + o.header, cw - 2, ch - o.header - 1, depth + 1);
    }
  };
  walk(root, 0, 0, W, H, 0);
  return out;
}

describe("nest() cut-point selection", () => {
  for (const [seed, n, W, H] of [[3, 8000, 1200, 800], [5, 30000, 900, 500], [9, 3000, 300, 200], [21, 60000, 1600, 900]] as const) {
    test(`matches the full-sort layout (seed ${seed}, ${n} nodes, ${W}x${H})`, () => {
      const g = generateTree(n, { root: "home", seed });
      const s = new TreeStore();
      s.reset({ scanId: 1, path: "/x", rootName: "x", volume: null, opts: {} });
      s.ingest({ scanId: 1, records: g.records, wireBytes: 0 });
      const a = computeLayout(s, s.root, W, H, DEFAULT_LAYOUT);
      const b = refLayout(s, s.root, W, H, DEFAULT_LAYOUT);
      expect(a.count).toBe(b.count);
      expect(a.count).toBeGreaterThan(20);
      for (let i = 0; i < a.count; i++) {
        expect([a.node[i], a.x[i], a.y[i], a.w[i], a.h[i], a.type[i], a.depth[i]]).toEqual([b.node[i], b.x[i], b.y[i], b.w[i], b.h[i], b.type[i], b.depth[i]]);
      }
    });
  }
});
