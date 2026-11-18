// SPDX-License-Identifier: MIT
//
// Canvas2D treemap renderer. Imperative on purpose: layout lives in typed
// arrays, colours are precomputed strings, label fits are cached per layout,
// and the frame loop only runs while something is moving (tween or zoom).

import { DIR_FLAG } from "../protocol/types";
import { formatBytes, formatCount, formatPercent, plural, type Units } from "../store/format";
import { KIND_BUCKET, KIND_DIR, KIND_FILE, KIND_SYNTH, type TreeStore } from "../store/tree";
import { longPress } from "../ui/longpress";
import { hatchPatterns, PALETTE, SODIUM, FOAM, MAX_DEPTH, PETROL_DEEP, ROSE } from "./colors";
import { computeLayout, DEFAULT_LAYOUT, IT_DIR, IT_LEAF, IT_REST, Layout, type LayoutOptions } from "./layout";

const TWEEN_MS = 220;
const ZOOM_MS = 250;
/** relayout interval while streaming: ≥ 250 ms, ≈ 8× the measured layout cost, ≤ 2 s */
export const relayoutInterval = (layoutMs: number): number => Math.min(2000, Math.max(250, 8 * layoutMs));
/** skip the tween when a layout+draw cycle is this slow (it would only add frames to a struggling main thread) */
const TWEEN_COST_MS = 8;

const STRETCH = ["ultra-condensed", "extra-condensed", "condensed", "semi-condensed", "normal"] as const;
const WEIGHT = [620, 450] as const;
const FONT_PX = 12;
const fontKey = (w: number, s: number) => w * STRETCH.length + s;
const FONTS: string[] = [];
/** canvas labels use the page's data face (--font-data) */
function initFonts(): void {
  if (FONTS.length) return;
  const css = typeof document !== "undefined" ? getComputedStyle(document.documentElement).getPropertyValue("--font-data").trim() : "";
  const family = css && !css.startsWith("var(") ? css : `"Anybody Variable", "Anybody", sans-serif`;
  for (let w = 0; w < WEIGHT.length; w++) for (let s = 0; s < STRETCH.length; s++) FONTS.push(`${WEIGHT[w]} ${STRETCH[s]} ${FONT_PX}px ${family}`);
}
const DASH = [3, 3];
const NO_DASH: number[] = [];
/** queued-for-deletion outline */
const MARK_DASH = [5, 3];

const easeOut = (t: number) => 1 - (1 - t) * (1 - t) * (1 - t);
const easeInOut = (t: number) => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2);

export interface TreemapOptions {
  interactive: boolean;
  reducedMotion: boolean;
  units(): Units;
  onHover?(node: number): void;
  onSelect?(node: number): void;
  onZoom?(node: number): void;
  /** queued, not yet applied ingest items; > 0 means the main thread is busy */
  backlog?(): number;
  /** the scan is paused: no layout tweens, tooltips say so */
  paused?(): boolean;
  /** right-click or long-press on a parcel (client coordinates) */
  onContext?(node: number, x: number, y: number): void;
  /** nodes queued for deletion: veiled with a dashed outline */
  marked?(): ReadonlySet<number>;
  layout?: Partial<LayoutOptions>;
}

interface Scene {
  L: Layout;
  // per-item tween origin
  fx: Float32Array;
  fy: Float32Array;
  fw: Float32Array;
  fh: Float32Array;
  enter: Uint8Array;
  t0: number;
  // label cache
  label: (string | null)[];
  labelFont: Uint8Array;
  labelW: Float32Array;
  sub: (string | null)[];
  subW: Float32Array;
  order: Int32Array; // labelled items sorted by font
  nLabels: number;
}

function newScene(): Scene {
  return { L: new Layout(), fx: new Float32Array(0), fy: new Float32Array(0), fw: new Float32Array(0), fh: new Float32Array(0), enter: new Uint8Array(0), t0: 0, label: [], labelFont: new Uint8Array(0), labelW: new Float32Array(0), sub: [], subW: new Float32Array(0), order: new Int32Array(0), nLabels: 0 };
}

function sizeScene(s: Scene): void {
  const c = s.L.count;
  if (s.fx.length < c) {
    const n = Math.max(c, s.fx.length * 2, 1024);
    s.fx = new Float32Array(n);
    s.fy = new Float32Array(n);
    s.fw = new Float32Array(n);
    s.fh = new Float32Array(n);
    s.enter = new Uint8Array(n);
    s.labelFont = new Uint8Array(n);
    s.labelW = new Float32Array(n);
    s.subW = new Float32Array(n);
    s.order = new Int32Array(n);
  }
}

export class TreemapView {
  readonly host: HTMLElement;
  private canvas: HTMLCanvasElement;
  private overlay: HTMLCanvasElement;
  private ctx: CanvasRenderingContext2D;
  private octx: CanvasRenderingContext2D;
  private tip: HTMLDivElement | null = null;
  private tipName!: HTMLDivElement;
  private tipSize!: HTMLDivElement;
  private tipMeta!: HTMLDivElement;

  private cur = newScene();
  private old = newScene(); // previous layout (tween source / zoom-from scene)
  private zoom: { from: Scene; to: Scene; t0: number; rect: [number, number, number, number] | null; inward: boolean } | null = null;
  private opts: LayoutOptions;

  private W = 0;
  private H = 0;
  private dpr = 1;
  private raf = 0;
  private layoutTimer = 0;
  private lastLayout = 0;
  private lastLayoutMs = 0;
  private lastDrawMs = 0;
  private patterns: CanvasPattern[] = [];
  private measureCache = new Map<string, number>();
  private slot = new Int32Array(0);
  private slotGen = new Int32Array(0);
  private gen = 1;
  private ro: ResizeObserver;
  private fontsReady = false;

  root = -1;
  hovered = -1;
  selected = -1;
  private hoverItem = -1;
  private disposed = false;
  private offPress: (() => void) | null = null;

  constructor(host: HTMLElement, readonly store: TreeStore, readonly o: TreemapOptions) {
    initFonts();
    this.host = host;
    this.opts = { ...DEFAULT_LAYOUT, ...o.layout };
    this.canvas = document.createElement("canvas");
    this.canvas.className = "tm-canvas";
    this.overlay = document.createElement("canvas");
    this.overlay.className = "tm-overlay";
    this.canvas.setAttribute("aria-hidden", "true");
    host.appendChild(this.canvas);
    host.appendChild(this.overlay);
    this.ctx = this.canvas.getContext("2d", { alpha: true })!;
    this.octx = this.overlay.getContext("2d")!;
    if (o.interactive) {
      this.tip = document.createElement("div");
      this.tip.className = "tm-tip";
      this.tip.setAttribute("role", "status");
      this.tipName = this.tip.appendChild(document.createElement("div"));
      this.tipName.className = "tm-tip-name";
      this.tipSize = this.tip.appendChild(document.createElement("div"));
      this.tipSize.className = "tm-tip-size";
      this.tipMeta = this.tip.appendChild(document.createElement("div"));
      this.tipMeta.className = "tm-tip-meta";
      host.appendChild(this.tip);
      this.overlay.addEventListener("pointermove", this.onMove);
      this.overlay.addEventListener("pointerleave", this.onLeave);
      this.overlay.addEventListener("click", this.onClick);
      if (o.onContext) {
        this.overlay.addEventListener("contextmenu", this.onContextMenu);
        this.offPress = longPress(this.overlay, (x, y) => this.contextAt(x, y));
      }
    } else {
      this.overlay.style.pointerEvents = "none";
    }
    this.ro = new ResizeObserver(() => this.resize());
    this.ro.observe(host);
    this.resize();
    const fonts = (document as Document & { fonts?: FontFaceSet }).fonts;
    if (fonts) {
      Promise.all([fonts.load(FONTS[fontKey(0, 4)]), fonts.load(FONTS[fontKey(1, 0)])])
        .catch(() => undefined)
        .then(() => {
          this.fontsReady = true;
          this.measureCache.clear();
          if (!this.disposed) this.relayout(false);
        });
    } else this.fontsReady = true;
  }

  destroy(): void {
    this.disposed = true;
    this.offPress?.();
    this.ro.disconnect();
    cancelAnimationFrame(this.raf);
    clearTimeout(this.layoutTimer);
    this.host.replaceChildren();
  }

  // ---- public API -----------------------------------------------------------

  /** store contents changed (throttled relayout with tween) */
  invalidate(): void {
    if (this.layoutTimer || this.disposed) return;
    const due = Math.max(0, this.lastLayout + relayoutInterval(this.lastLayoutMs) - performance.now());
    this.layoutTimer = window.setTimeout(() => {
      this.layoutTimer = 0;
      this.relayout(true);
    }, due);
  }

  /** new scan: drop all cached geometry */
  reset(root: number): void {
    this.root = root;
    this.hovered = this.selected = -1;
    this.cur.L.count = 0;
    this.old.L.count = 0;
    this.zoom = null;
    this.gen++;
    this.relayout(false);
  }

  setRoot(node: number, animate = true): void {
    if (node === this.root) return;
    const prevRoot = this.root;
    this.root = node;
    if (!animate || this.o.reducedMotion || prevRoot < 0 || this.cur.L.count === 0) {
      this.relayout(false);
      return;
    }
    // freeze the current scene as the zoom origin
    const from = this.cur;
    this.cur = this.old;
    this.old = from;
    this.finishTween(from);
    const inward = this.store.isAncestor(prevRoot, node);
    let rect: [number, number, number, number] | null = null;
    if (inward) {
      // where the new root sits in the old scene (or its visible ancestor)
      const it = this.itemFor(from.L, node);
      if (it >= 0) rect = [from.L.x[it], from.L.y[it], from.L.w[it], from.L.h[it]];
    }
    this.layoutInto(this.cur, false);
    if (!inward) {
      const it = this.itemFor(this.cur.L, prevRoot);
      if (it >= 0) rect = [this.cur.L.x[it], this.cur.L.y[it], this.cur.L.w[it], this.cur.L.h[it]];
    }
    this.zoom = { from, to: this.cur, t0: performance.now(), rect, inward };
    this.drawOverlay();
    this.requestDraw();
  }

  setSelected(node: number): void {
    this.selected = node;
    this.drawOverlay();
  }

  setHovered(node: number): void {
    if (this.hovered === node) return;
    this.hovered = node;
    this.drawOverlay();
  }

  /** marks changed: repaint without relayout */
  redraw(): void {
    this.requestDraw();
  }

  /** units changed: relabel without moving anything */
  refreshLabels(): void {
    this.labelScene(this.cur);
    this.requestDraw();
  }

  // ---- layout ---------------------------------------------------------------

  private resize(): void {
    const r = this.host.getBoundingClientRect();
    const W = Math.max(1, Math.floor(r.width));
    const H = Math.max(1, Math.floor(r.height));
    const dpr = Math.min(window.devicePixelRatio || 1, 3);
    if (W === this.W && H === this.H && dpr === this.dpr) return;
    this.W = W;
    this.H = H;
    if (dpr !== this.dpr || !this.patterns.length) {
      this.dpr = dpr;
      this.patterns = hatchPatterns(this.ctx, dpr);
    }
    for (const c of [this.canvas, this.overlay]) {
      c.width = Math.round(W * dpr);
      c.height = Math.round(H * dpr);
      c.style.width = `${W}px`;
      c.style.height = `${H}px`;
    }
    this.zoom = null;
    this.relayout(false);
  }

  private relayout(tween: boolean): void {
    if (this.disposed) return;
    if (this.zoom) {
      // wait for the zoom to finish; it relayouts at the end
      this.invalidate();
      return;
    }
    const t0 = performance.now();
    this.lastLayout = t0;
    const busy = (this.o.backlog?.() ?? 0) > 0 || (this.o.paused?.() ?? false) || this.lastLayoutMs + this.lastDrawMs > TWEEN_COST_MS;
    const animate = tween && !busy && !this.o.reducedMotion && this.cur.L.count > 0;
    const prev = this.cur;
    this.cur = this.old;
    this.old = prev;
    if (animate) this.captureDisplayed(prev);
    this.layoutInto(this.cur, animate);
    this.lastLayoutMs = performance.now() - t0;
    this.requestDraw();
  }

  /** bake the prev scene's on-screen rects into its from-arrays (tween source) */
  private captureDisplayed(s: Scene): void {
    const t = this.tweenT(s, performance.now());
    const L = s.L;
    for (let i = 0; i < L.count; i++) {
      s.fx[i] = s.fx[i] + (L.x[i] - s.fx[i]) * t;
      s.fy[i] = s.fy[i] + (L.y[i] - s.fy[i]) * t;
      s.fw[i] = s.fw[i] + (L.w[i] - s.fw[i]) * t;
      s.fh[i] = s.fh[i] + (L.h[i] - s.fh[i]) * t;
    }
  }

  private finishTween(s: Scene): void {
    const L = s.L;
    for (let i = 0; i < L.count; i++) {
      s.fx[i] = L.x[i];
      s.fy[i] = L.y[i];
      s.fw[i] = L.w[i];
      s.fh[i] = L.h[i];
      s.enter[i] = 0;
    }
    s.t0 = 0;
  }

  private ensureSlots(): void {
    const n = this.store.n;
    if (this.slot.length < n) {
      let c = Math.max(1024, this.slot.length);
      while (c < n) c *= 2;
      this.slot = new Int32Array(c);
      this.slotGen = new Int32Array(c);
      this.gen++;
    }
  }

  private layoutInto(s: Scene, animate: boolean): void {
    const prev = this.old;
    computeLayout(this.store, this.root, this.W, this.H, this.opts, s.L);
    sizeScene(s);
    const L = s.L;
    if (animate) {
      // index the previous scene by node id
      this.ensureSlots();
      const g = ++this.gen;
      const P = prev.L;
      for (let j = 0; j < P.count; j++) {
        if (P.type[j] === IT_REST) continue;
        const nd = P.node[j];
        this.slotGen[nd] = g;
        this.slot[nd] = j;
      }
      for (let i = 0; i < L.count; i++) {
        const nd = L.node[i];
        if (L.type[i] !== IT_REST && nd < this.slotGen.length && this.slotGen[nd] === g) {
          const j = this.slot[nd];
          s.fx[i] = prev.fx[j];
          s.fy[i] = prev.fy[j];
          s.fw[i] = prev.fw[j];
          s.fh[i] = prev.fh[j];
          s.enter[i] = 0;
        } else {
          s.fx[i] = L.x[i];
          s.fy[i] = L.y[i];
          s.fw[i] = L.w[i];
          s.fh[i] = L.h[i];
          s.enter[i] = 1;
        }
      }
      s.t0 = performance.now();
    } else {
      this.finishTween(s);
    }
    this.labelScene(s);
    // keep hover target pointing at the same node
    this.hoverItem = -1;
    this.drawOverlay();
  }

  private itemFor(L: Layout, node: number): number {
    for (let k = node; k >= 0; k = this.store.parent[k]) {
      const it = L.find(k);
      if (it >= 0) return it;
      if (k === L.root) break;
    }
    return -1;
  }

  // ---- labels ---------------------------------------------------------------

  private measure(text: string, f: number): number {
    const key = f + "\u0000" + text;
    let w = this.measureCache.get(key);
    if (w === undefined) {
      if (this.measureCache.size > 60000) this.measureCache.clear();
      this.ctx.font = FONTS[f];
      setStretch(this.ctx, f);
      w = this.ctx.measureText(text).width;
      this.measureCache.set(key, w);
    }
    return w;
  }

  /** widest stretch that fits; truncate at ultra-condensed; null if hopeless */
  private fit(text: string, maxW: number, weight: number): [string, number, number] | null {
    if (maxW < 18) return null;
    for (let s = STRETCH.length - 1; s >= 0; s--) {
      const f = fontKey(weight, s);
      const w = this.measure(text, f);
      if (w <= maxW) return [text, f, w];
    }
    const f = fontKey(weight, 0);
    let lo = 1, hi = text.length - 1, best = "";
    while (lo <= hi) {
      const mid = (lo + hi) >> 1;
      const t = text.slice(0, mid) + "…";
      if (this.measure(t, f) <= maxW) {
        best = t;
        lo = mid + 1;
      } else hi = mid - 1;
    }
    if (best.length < 5) return null;
    return [best, f, this.measure(best, f)];
  }

  private nodeLabel(node: number, kind: number): string {
    const st = this.store;
    if (kind === KIND_BUCKET) {
      const n = st.files(node);
      return n > 0 ? `${formatCount(n)} other ${n === 1 ? "file" : "files"}` : "Other files";
    }
    return st.names[node] || st.rootPath;
  }

  private labelScene(s: Scene): void {
    const L = s.L;
    const units = this.o.units();
    s.label.length = L.count;
    s.sub.length = L.count;
    let n = 0;
    for (let i = 0; i < L.count; i++) {
      s.label[i] = null;
      s.sub[i] = null;
      if (!this.fontsReady) continue;
      const w = L.w[i], h = L.h[i], t = L.type[i];
      if (t === IT_REST) continue;
      const node = L.node[i];
      if (t === IT_DIR) {
        if (L.head[i] < 14 || w < 30) continue;
        const r = this.fit(this.nodeLabel(node, L.kind[i]), w - 10, 0);
        if (!r) continue;
        s.label[i] = r[0];
        s.labelFont[i] = r[1];
        s.labelW[i] = r[2] + 10;
        const sz = formatBytes(this.store.size(node), units);
        const sw = this.measure(sz, fontKey(1, 2));
        if (r[2] + 10 + sw + 8 <= w && r[0] === this.nodeLabel(node, L.kind[i])) {
          s.sub[i] = sz;
          s.subW[i] = sw;
        }
        s.order[n++] = i;
      } else {
        if (w < 30 || h < 18) continue;
        const r = this.fit(this.nodeLabel(node, L.kind[i]), w - 10, 1);
        if (!r) continue;
        s.label[i] = r[0];
        s.labelFont[i] = r[1];
        s.labelW[i] = r[2] + 10;
        if (h >= 34) {
          const sz = formatBytes(this.store.size(node), units);
          const sw = this.measure(sz, fontKey(1, 2));
          if (sw + 10 <= w) {
            s.sub[i] = sz;
            s.subW[i] = sw;
          }
        }
        s.order[n++] = i;
      }
    }
    s.nLabels = n;
    // group by font so ctx.font changes ≤ 10× per frame
    const ord = s.order.subarray(0, n);
    ord.sort((a, b) => s.labelFont[a] - s.labelFont[b]);
  }

  // ---- drawing --------------------------------------------------------------

  private requestDraw(): void {
    if (!this.raf && !this.disposed) this.raf = requestAnimationFrame(this.frame);
  }

  private tweenT(s: Scene, now: number): number {
    if (!s.t0) return 1;
    const t = (now - s.t0) / TWEEN_MS;
    return t >= 1 ? 1 : easeOut(t < 0 ? 0 : t);
  }

  private frame = (now: number): void => {
    this.raf = 0;
    const d0 = performance.now();
    try {
      this.frameBody(now);
    } finally {
      this.lastDrawMs = performance.now() - d0;
    }
  };

  private frameBody(now: number): void {
    
    const ctx = this.ctx;
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.clearRect(0, 0, this.canvas.width, this.canvas.height);
    let again = false;
    const z = this.zoom;
    if (z) {
      const raw = Math.min(1, (now - z.t0) / ZOOM_MS);
      const t = easeInOut(raw);
      const r = z.rect;
      if (r) {
        const W = this.W, H = this.H;
        // transform mapping rect r → full viewport
        const sx = W / Math.max(1, r[2]), sy = H / Math.max(1, r[3]);
        const tx = -r[0] * sx, ty = -r[1] * sy;
        if (z.inward) {
          // old scene blows up around r; new scene grows from r to full
          this.drawScene(z.from, 1, lerp(1, sx, t), lerp(1, sy, t), lerp(0, tx, t), lerp(0, ty, t), 1);
          this.drawScene(z.to, 1, lerp(1 / sx, 1, t), lerp(1 / sy, 1, t), lerp(r[0], 0, t), lerp(r[1], 0, t), t);
        } else {
          // new (outer) scene shrinks from blown-up to identity; old collapses into r
          this.drawScene(z.to, 1, lerp(sx, 1, t), lerp(sy, 1, t), lerp(tx, 0, t), lerp(ty, 0, t), 1);
          this.drawScene(z.from, 1, lerp(1, 1 / sx, t), lerp(1, 1 / sy, t), lerp(0, r[0], t), lerp(0, r[1], t), 1 - t);
        }
      } else {
        this.drawScene(z.from, 1, 1, 1, 0, 0, 1 - t);
        this.drawScene(z.to, 1, 1, 1, 0, 0, t);
      }
      if (raw < 1) again = true;
      else {
        this.zoom = null;
        this.finishTween(this.old);
        this.drawOverlay();
        this.invalidate();
      }
    } else {
      const t = this.tweenT(this.cur, now);
      this.drawScene(this.cur, t, 1, 1, 0, 0, 1);
      if (t < 1) again = true;
      else this.cur.t0 = 0;
    }
    if (again) this.raf = requestAnimationFrame(this.frame);
  }

  private drawScene(s: Scene, t: number, sx: number, sy: number, tx: number, ty: number, alpha: number): void {
    if (alpha <= 0.001) return;
    const ctx = this.ctx;
    const L = s.L;
    const dpr = this.dpr;
    ctx.setTransform(dpr * sx, 0, 0, dpr * sy, dpr * tx, dpr * ty);
    ctx.globalAlpha = alpha;
    const W = this.W, H = this.H;
    // visible bounds in layout coords (cull)
    const vx0 = -tx / sx, vy0 = -ty / sy, vx1 = (W - tx) / sx, vy1 = (H - ty) / sy;
    const P = PALETTE;
    const st = this.store;
    const tweening = t < 1;
    let curAlpha = alpha;
    let dashed = false;
    const fx = s.fx, fy = s.fy, fw = s.fw, fh = s.fh, lx = L.x, ly = L.y, lw = L.w, lh = L.h;
    for (let i = 0; i < L.count; i++) {
      let x = lx[i], y = ly[i], w = lw[i], h = lh[i];
      if (tweening) {
        x = fx[i] + (x - fx[i]) * t;
        y = fy[i] + (y - fy[i]) * t;
        w = fw[i] + (w - fw[i]) * t;
        h = fh[i] + (h - fh[i]) * t;
        const a = s.enter[i] ? alpha * t : alpha;
        if (a !== curAlpha) {
          ctx.globalAlpha = a;
          curAlpha = a;
        }
      }
      if (x > vx1 || y > vy1 || x + w < vx0 || y + h < vy0 || w < 0.5 || h < 0.5) continue;
      const hue = L.hue[i];
      const d = L.depth[i] > MAX_DEPTH ? MAX_DEPTH : L.depth[i];
      const type = L.type[i];
      const kind = L.kind[i];
      if (type === IT_REST) {
        ctx.fillStyle = P.rest[hue][d];
        ctx.fillRect(x, y, w, h);
        continue;
      }
      const node = L.node[i];
      const denied = kind === KIND_DIR && (st.flags[node] & DIR_FLAG.READ_DENIED) !== 0;
      if (denied) {
        if (!dashed) {
          ctx.setLineDash(DASH);
          dashed = true;
        }
        ctx.strokeStyle = P.hatch[hue];
        ctx.lineWidth = 1;
        ctx.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1);
        continue;
      }
      if (type === IT_DIR) {
        ctx.fillStyle = P.body[hue][d];
        ctx.fillRect(x, y, w, h);
        ctx.fillStyle = P.header[hue][d];
        ctx.fillRect(x, y, w, L.head[i] - 1);
      } else if (kind === KIND_BUCKET || kind === KIND_SYNTH) {
        ctx.fillStyle = P.bucketBase[hue][d];
        ctx.fillRect(x, y, w, h);
        ctx.fillStyle = this.patterns[hue];
        ctx.fillRect(x, y, w, h);
      } else {
        ctx.fillStyle = P.leaf[hue][d];
        ctx.fillRect(x, y, w, h);
      }
    }
    if (dashed) ctx.setLineDash(NO_DASH);

    // labels, grouped by font
    if (sx < 0.6 || sy < 0.6) {
      ctx.globalAlpha = 1;
      return;
    }
    let font = -1;
    ctx.textBaseline = "alphabetic";
    for (let k = 0; k < s.nLabels; k++) {
      const i = s.order[k];
      const text = s.label[i];
      if (!text) continue;
      let x = lx[i], y = ly[i], w = lw[i], h = lh[i];
      if (tweening) {
        x = fx[i] + (x - fx[i]) * t;
        y = fy[i] + (y - fy[i]) * t;
        w = fw[i] + (w - fw[i]) * t;
        h = fh[i] + (h - fh[i]) * t;
        // hide while flying or still too small; follow the parcel otherwise
        if (Math.abs(x - lx[i]) + Math.abs(y - ly[i]) > 40 || w < s.labelW[i] - 0.5 || h < 15) continue;
        const a = s.enter[i] ? alpha * t : alpha;
        if (a !== curAlpha) {
          ctx.globalAlpha = a;
          curAlpha = a;
        }
      }
      if (x > vx1 || y > vy1 || x + w < vx0 || y + h < vy0) continue;
      const f = s.labelFont[i];
      if (f !== font) {
        ctx.font = FONTS[f];
        setStretch(ctx, f);
        font = f;
      }
      const kind = L.kind[i];
      const node = L.node[i];
      const darkFill = kind === KIND_BUCKET || kind === KIND_SYNTH || (kind === KIND_DIR && (st.flags[node] & DIR_FLAG.READ_DENIED) !== 0);
      ctx.fillStyle = darkFill ? P.inkLight : P.ink;
      if (L.type[i] === IT_DIR) ctx.fillText(text, x + 5, y + 11.5);
      else ctx.fillText(text, x + 5, y + 14);
    }
    // secondary size text (one font)
    ctx.font = FONTS[fontKey(1, 2)];
    setStretch(ctx, fontKey(1, 2));
    for (let k = 0; k < s.nLabels; k++) {
      const i = s.order[k];
      const sub = s.sub[i];
      if (!sub) continue;
      let x = lx[i], y = ly[i], w = lw[i], h = lh[i];
      if (tweening) {
        x = fx[i] + (x - fx[i]) * t;
        y = fy[i] + (y - fy[i]) * t;
        w = fw[i] + (w - fw[i]) * t;
        h = fh[i] + (h - fh[i]) * t;
        if (Math.abs(x - lx[i]) + Math.abs(y - ly[i]) > 40 || h < (L.type[i] === IT_DIR ? 15 : 34) || w < (L.type[i] === IT_DIR ? s.labelW[i] + s.subW[i] + 2 : s.subW[i] + 10)) continue;
      }
      if (x > vx1 || y > vy1 || x + w < vx0 || y + h < vy0) continue;
      const kind = L.kind[i];
      const node = L.node[i];
      const darkFill = kind === KIND_BUCKET || kind === KIND_SYNTH || (kind === KIND_DIR && (st.flags[node] & DIR_FLAG.READ_DENIED) !== 0);
      ctx.fillStyle = darkFill ? P.inkLightSoft : P.inkSoft;
      if (L.type[i] === IT_DIR) ctx.fillText(sub, x + s.labelW[i] + 2, y + 11.5);
      else ctx.fillText(sub, x + 5, y + 28);
    }
    this.drawMarks(s, t, alpha);
    ctx.globalAlpha = 1;
  }

  /** second pass: veil + dashed rose outline over parcels queued for deletion */
  private drawMarks(s: Scene, t: number, alpha: number): void {
    const marks = this.o.marked?.();
    if (!marks || marks.size === 0) return;
    const ctx = this.ctx;
    const L = s.L;
    const tweening = t < 1;
    ctx.setLineDash(MARK_DASH);
    ctx.lineWidth = 1.5;
    ctx.strokeStyle = ROSE;
    ctx.fillStyle = PETROL_DEEP;
    for (let i = 0; i < L.count; i++) {
      if (L.type[i] === IT_REST || !marks.has(L.node[i])) continue;
      let x = L.x[i], y = L.y[i], w = L.w[i], h = L.h[i];
      if (tweening) {
        x = s.fx[i] + (x - s.fx[i]) * t;
        y = s.fy[i] + (y - s.fy[i]) * t;
        w = s.fw[i] + (w - s.fw[i]) * t;
        h = s.fh[i] + (h - s.fh[i]) * t;
      }
      if (w < 1 || h < 1) continue;
      ctx.globalAlpha = alpha * 0.62;
      ctx.fillRect(x, y, w, h);
      ctx.globalAlpha = alpha;
      ctx.strokeRect(x + 0.75, y + 0.75, Math.max(0, w - 1.5), Math.max(0, h - 1.5));
    }
    ctx.setLineDash(NO_DASH);
  }

  private drawOverlay(): void {
    const c = this.octx;
    c.setTransform(1, 0, 0, 1, 0, 0);
    c.clearRect(0, 0, this.overlay.width, this.overlay.height);
    if (this.zoom) return;
    const dpr = this.dpr;
    c.setTransform(dpr, 0, 0, dpr, 0, 0);
    const L = this.cur.L;
    const outline = (node: number, color: string, width: number, fillAlpha: number) => {
      if (node < 0) return;
      let it = this.hoverItem >= 0 && node === this.hovered && L.node[this.hoverItem] === node ? this.hoverItem : this.itemFor(L, node);
      if (it < 0) return;
      const x = L.x[it], y = L.y[it], w = L.w[it], h = L.h[it];
      if (fillAlpha > 0) {
        c.globalAlpha = fillAlpha;
        c.fillStyle = FOAM;
        c.fillRect(x, y, w, h);
        c.globalAlpha = 1;
      }
      c.strokeStyle = color;
      c.lineWidth = width;
      c.strokeRect(x + width / 2, y + width / 2, Math.max(0, w - width), Math.max(0, h - width));
    };
    if (this.hovered !== this.selected) outline(this.hovered, SODIUM, 2, 0.16);
    outline(this.selected, SODIUM, 2.5, 0);
  }

  // ---- pointer --------------------------------------------------------------

  private pos(e: PointerEvent | MouseEvent): [number, number] {
    const r = this.overlay.getBoundingClientRect();
    return [e.clientX - r.left, e.clientY - r.top];
  }

  private onMove = (e: PointerEvent): void => {
    if (this.zoom) return;
    const [x, y] = this.pos(e);
    const it = this.cur.L.hit(x, y);
    const node = it >= 0 ? this.cur.L.node[it] : -1;
    if (it !== this.hoverItem) {
      this.hoverItem = it;
      this.hovered = node;
      this.drawOverlay();
      this.o.onHover?.(node);
      if (it >= 0) this.fillTip(it);
    }
    this.placeTip(x, y, it >= 0);
  };

  private onLeave = (): void => {
    this.hoverItem = -1;
    this.hovered = -1;
    this.drawOverlay();
    this.o.onHover?.(-1);
    this.placeTip(0, 0, false);
  };

  private onContextMenu = (e: MouseEvent): void => {
    e.preventDefault();
    this.contextAt(e.clientX, e.clientY);
  };

  private contextAt(cx: number, cy: number): void {
    if (this.zoom) return;
    const r = this.overlay.getBoundingClientRect();
    const it = this.cur.L.hit(cx - r.left, cy - r.top);
    if (it < 0 || this.cur.L.type[it] === IT_REST) return;
    this.placeTip(0, 0, false);
    this.o.onContext?.(this.cur.L.node[it], cx, cy);
  }

  private onClick = (e: MouseEvent): void => {
    if (this.zoom) return;
    const [x, y] = this.pos(e);
    const it = this.cur.L.hit(x, y);
    if (it < 0) return;
    const L = this.cur.L;
    const node = L.node[it];
    if (L.type[it] === IT_REST) {
      if (node !== this.root) this.o.onZoom?.(node);
      return;
    }
    const kind = L.kind[it];
    if (kind === KIND_DIR && this.store.hasChildren(node) && !(this.store.flags[node] & DIR_FLAG.READ_DENIED)) this.o.onZoom?.(node);
    else this.o.onSelect?.(node);
  };

  private fillTip(it: number): void {
    if (!this.tip) return;
    const L = this.cur.L;
    const st = this.store;
    const units = this.o.units();
    const node = L.node[it];
    const type = L.type[it];
    if (type === IT_REST) {
      this.tipName.textContent = "Smaller items";
      this.tipSize.textContent = "Too small to draw here";
      this.tipMeta.textContent = "Click to zoom in";
      return;
    }
    const kind = L.kind[it];
    const size = st.size(node);
    const parent = st.parent[node];
    const pSize = parent >= 0 ? st.size(parent) : 0;
    this.tipName.textContent = this.nodeLabel(node, kind);
    const app = st.apparent(node);
    let sizeLine = formatBytes(size, units);
    if (Math.abs(app - size) > size * 0.02 && app > 0) sizeLine += ` on disk, ${formatBytes(app, units)} apparent`;
    this.tipSize.textContent = sizeLine;
    const parts: string[] = [];
    if (pSize > 0) parts.push(`${formatPercent(size, pSize)} of ${st.names[parent] || "parent"}`);
    if (kind === KIND_DIR) {
      if (st.flags[node] & DIR_FLAG.READ_DENIED) parts.push("Couldn't be read");
      else {
        const f = st.files(node), d = st.dirs(node);
        parts.push(d > 0 ? `${plural(f, "file", "files")}, ${plural(d, "folder", "folders")}` : plural(f, "file", "files"));
        if (!st.isSettled(node)) parts.push(this.o.paused?.() ? "scan paused" : "still scanning");
      }
    } else if (kind === KIND_SYNTH) {
      parts.push("Used on this volume but outside the scan: other volumes, local snapshots, purgeable space");
    } else if (kind === KIND_FILE) {
      parts.push(st.fullPath(parent));
    }
    if (st.isStale(node)) parts.push("Partly deleted · scan again");
    if (this.o.marked?.().has(node)) parts.push("In the deletion queue");
    this.tipMeta.textContent = parts.join("\n");
  }

  private placeTip(x: number, y: number, show: boolean): void {
    const tip = this.tip;
    if (!tip) return;
    if (!show) {
      tip.classList.remove("on");
      return;
    }
    tip.classList.add("on");
    const tw = tip.offsetWidth, th = tip.offsetHeight;
    let left = x + 16, top = y + 18;
    if (left + tw > this.W - 8) left = x - tw - 12;
    if (top + th > this.H - 8) top = y - th - 12;
    tip.style.transform = `translate(${Math.max(8, left)}px, ${Math.max(8, top)}px)`;
  }

  /** stats for perf checks */
  get itemCount(): number {
    return this.cur.L.count;
  }
}

function lerp(a: number, b: number, t: number): number {
  return a + (b - a) * t;
}

function setStretch(ctx: CanvasRenderingContext2D, f: number): void {
  const c = ctx as CanvasRenderingContext2D & { fontStretch?: string };
  if ("fontStretch" in c) c.fontStretch = STRETCH[f % STRETCH.length];
}

