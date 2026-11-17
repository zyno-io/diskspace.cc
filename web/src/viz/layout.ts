// SPDX-License-Identifier: MIT
//
// Squarified, nested treemap layout for one zoom root. Pure (no DOM) so it can
// be unit-tested. Output is a flat pre-order list of parcels in typed arrays:
// a parent always precedes its children, so painting in order nests correctly.

import { KIND_BUCKET, KIND_DIR, KIND_FILE, KIND_SYNTH, type TreeStore } from "../store/tree";
import { fileCategory, CAT_OTHER, HUES } from "./colors";

export const IT_LEAF = 0; // filled parcel (file, collapsed dir, bucket)
export const IT_DIR = 1; // open dir: header strip + nested children
export const IT_REST = 2; // aggregate of children too small to draw

export interface LayoutOptions {
  header: number; // dir header strip height (px)
  minPx: number; // smallest parcel side drawn
  maxDepth: number; // levels below the zoom root
  gap: number; // lot-line width between siblings
  /** min parcel size that gets a header + nested children */
  nestW: number;
  nestH: number;
}

export const DEFAULT_LAYOUT: LayoutOptions = { header: 16, minPx: 6, maxDepth: 12, gap: 1, nestW: 44, nestH: 38 };

export class Layout {
  count = 0;
  cap = 0;
  root = -1;
  width = 0;
  height = 0;
  node = new Int32Array(0);
  x = new Float32Array(0);
  y = new Float32Array(0);
  w = new Float32Array(0);
  h = new Float32Array(0);
  type = new Uint8Array(0);
  depth = new Uint8Array(0);
  hue = new Uint8Array(0);
  /** header strip height for IT_DIR (0 = none) */
  head = new Uint8Array(0);
  /** store kind of the node (dir/file/bucket/synth) */
  kind = new Uint8Array(0);

  constructor(cap = 1024) {
    this.ensure(cap);
  }

  ensure(c: number): void {
    if (c <= this.cap) return;
    let n = Math.max(1024, this.cap);
    while (n < c) n *= 2;
    const g = <T extends Int32Array | Float32Array | Uint8Array>(a: T): T => {
      const b = new (a.constructor as { new (n: number): T })(n);
      b.set(a.subarray(0, this.count));
      return b;
    };
    this.node = g(this.node);
    this.x = g(this.x);
    this.y = g(this.y);
    this.w = g(this.w);
    this.h = g(this.h);
    this.type = g(this.type);
    this.depth = g(this.depth);
    this.hue = g(this.hue);
    this.head = g(this.head);
    this.kind = g(this.kind);
    this.cap = n;
  }

  push(node: number, x: number, y: number, w: number, h: number, type: number, depth: number, hue: number, kind: number): number {
    if (this.count >= this.cap) this.ensure(this.count + 1);
    const i = this.count++;
    this.node[i] = node;
    this.x[i] = x;
    this.y[i] = y;
    this.w[i] = w;
    this.h[i] = h;
    this.type[i] = type;
    this.depth[i] = depth;
    this.hue[i] = hue;
    this.head[i] = 0;
    this.kind[i] = kind;
    return i;
  }

  /** topmost (deepest) parcel under a point, or -1 */
  hit(px: number, py: number): number {
    for (let i = this.count - 1; i >= 0; i--) {
      const x = this.x[i], y = this.y[i];
      if (px >= x && py >= y && px < x + this.w[i] && py < y + this.h[i]) return i;
    }
    return -1;
  }

  find(node: number): number {
    for (let i = 0; i < this.count; i++) if (this.node[i] === node && this.type[i] !== IT_REST) return i;
    return -1;
  }
}

/** squarify `n` areas (desc) into the rect; writes rects into ox/oy/ow/oh */
export function squarify(areas: ArrayLike<number>, n: number, x: number, y: number, w: number, h: number, ox: Float64Array, oy: Float64Array, ow: Float64Array, oh: Float64Array): void {
  let i = 0;
  while (i < n) {
    if (w <= 0 || h <= 0) {
      for (; i < n; i++) ox[i] = oy[i] = ow[i] = oh[i] = 0;
      return;
    }
    const short = w < h ? w : h;
    const s2 = short * short;
    let sum = 0, mn = Infinity, mx = 0, best = Infinity, j = i;
    while (j < n) {
      const a = areas[j];
      const s = sum + a;
      const m1 = a < mn ? a : mn;
      const m2 = a > mx ? a : mx;
      const ss = s * s;
      const worst = Math.max((s2 * m2) / ss, ss / (s2 * m1));
      if (j > i && worst > best) break;
      best = worst;
      sum = s;
      mn = m1;
      mx = m2;
      j++;
    }
    if (j === n) {
      // last row fills the remaining rect exactly (absorbs float drift)
      sum = 0;
      for (let k = i; k < n; k++) sum += areas[k];
    }
    if (w >= h) {
      const cw = j === n ? w : sum / h;
      let cy = y;
      for (let k = i; k < j; k++) {
        const ch = sum > 0 ? (areas[k] / sum) * h : 0;
        ox[k] = x;
        oy[k] = cy;
        ow[k] = cw;
        oh[k] = ch;
        cy += ch;
      }
      x += cw;
      w -= cw;
    } else {
      const rh = j === n ? h : sum / w;
      let cx = x;
      for (let k = i; k < j; k++) {
        const cw = sum > 0 ? (areas[k] / sum) * w : 0;
        ox[k] = cx;
        oy[k] = y;
        ow[k] = cw;
        oh[k] = rh;
        cx += cw;
      }
      y += rh;
      h -= rh;
    }
    i = j;
  }
}

/** hue for a child of the zoom root (dirs cycle branch hues) */
function itemHue(store: TreeStore, node: number, inherited: number, dirOrdinal: number): number {
  const k = store.kind[node];
  if (k === KIND_FILE) return fileCategory(store.names[node]);
  if (k === KIND_SYNTH) return 4; // quartz
  if (inherited >= 0) return inherited;
  if (k === KIND_BUCKET) return CAT_OTHER;
  void dirOrdinal;
  return dirHue(store, node);
}

/**
 * Branch hue for a dir directly under the zoom root: its position in the
 * parent's announced child list. Unlike size rank this never changes while a
 * scan streams in, so branches keep their colour from first paint to done.
 */
export function dirHue(store: TreeStore, node: number): number {
  const p = store.parent[node];
  return p >= 0 ? (node - store.dStart[p]) % HUES.length : 0;
}

export function computeLayout(store: TreeStore, root: number, W: number, H: number, opts: LayoutOptions = DEFAULT_LAYOUT, out = new Layout()): Layout {
  out.count = 0;
  out.root = root;
  out.width = W;
  out.height = H;
  if (root < 0 || W < 1 || H < 1) return out;
  const kids: number[] = [];
  nest(store, root, 0, 0, W, H, 0, -1, opts, out, kids);
  return out;
}

const REST = -2;

/** children of `parent` with size > 0 in treemap order (also used for hue ordinals) */
export function bandOrder(store: TreeStore, parent: number): number[] {
  const kids: number[] = [];
  store.childrenOf(parent, kids);
  const live = kids.filter((c) => store.size(c) > 0);
  const band = new Map<number, number>();
  for (const c of live) band.set(c, Math.floor(Math.log2(store.size(c)) * 8));
  return live.sort((a, b) => band.get(b)! - band.get(a)! || a - b);
}

function nest(store: TreeStore, parent: number, x: number, y: number, w: number, h: number, depth: number, hue: number, o: LayoutOptions, out: Layout, scratch: number[]): void {
  const n0 = store.childrenOf(parent, scratch);
  if (n0 === 0) return;
  // sizes, drop empties, sort desc
  const ids: number[] = [];
  const sz: number[] = [];
  let total = 0;
  for (let i = 0; i < n0; i++) {
    const c = scratch[i];
    const s = store.size(c);
    if (s > 0) {
      ids.push(c);
      sz.push(s);
      total += s;
    }
  }
  if (total <= 0) return;
  // sort by size, quantised to ~9% bands with id as tie-break: siblings of
  // similar size keep their order while a scan streams in, so parcels don't
  // swap places on every relayout
  const band = sz.map((v) => Math.floor(Math.log2(v) * 8));
  const order = ids.map((_, i) => i).sort((a, b) => band[b] - band[a] || ids[a] - ids[b]);
  const area = w * h;
  const scale = area / total;
  const minArea = o.minPx * o.minPx * 1.6;

  // keep children until they get too small; the tail becomes one REST parcel
  let keep = 0;
  let restSum = 0;
  for (let i = 0; i < order.length; i++) {
    const a = sz[order[i]] * scale;
    if (a < minArea) {
      for (let j = i; j < order.length; j++) restSum += sz[order[j]];
      break;
    }
    keep++;
  }
  const m = keep + (restSum > 0 ? 1 : 0);
  const areas = new Float64Array(m);
  const nodeOf = new Int32Array(m);
  let k = 0;
  let restPlaced = restSum <= 0;
  for (let i = 0; i < keep; i++) {
    const a = sz[order[i]] * scale;
    if (!restPlaced && restSum * scale >= a) {
      areas[k] = restSum * scale;
      nodeOf[k++] = REST;
      restPlaced = true;
    }
    areas[k] = a;
    nodeOf[k++] = ids[order[i]];
  }
  if (!restPlaced) {
    areas[k] = restSum * scale;
    nodeOf[k++] = REST;
  }

  const rx = new Float64Array(m), ry = new Float64Array(m), rw = new Float64Array(m), rh = new Float64Array(m);
  squarify(areas, m, x, y, w, h, rx, ry, rw, rh);

  let dirOrdinal = 0;
  for (let i = 0; i < m; i++) {
    // snap to whole px; leave a lot line on the right/bottom edge
    const x0 = Math.round(rx[i]), y0 = Math.round(ry[i]);
    const x1 = Math.round(rx[i] + rw[i]), y1 = Math.round(ry[i] + rh[i]);
    const cw = x1 - x0 - o.gap, ch = y1 - y0 - o.gap;
    const node = nodeOf[i];
    if (node === REST) {
      if (cw >= 2 && ch >= 2) out.push(parent, x0, y0, cw, ch, IT_REST, depth, hue < 0 ? CAT_OTHER : hue, 0);
      continue;
    }
    const kind = store.kind[node];
    const ord = kind === KIND_DIR ? dirOrdinal++ : 0;
    if (cw < o.minPx || ch < o.minPx) continue;
    const hh = itemHue(store, node, hue, ord);
    const canNest = kind === KIND_DIR && depth + 1 < o.maxDepth && cw >= o.nestW && ch >= o.nestH && store.hasChildren(node);
    const it = out.push(node, x0, y0, cw, ch, canNest ? IT_DIR : IT_LEAF, depth, hh, kind);
    if (canNest) {
      out.head[it] = o.header;
      nest(store, node, x0 + 1, y0 + o.header, cw - 2, ch - o.header - 1, depth + 1, hh, o, out, scratch);
    }
  }
}
