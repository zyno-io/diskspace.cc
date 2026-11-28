// SPDX-License-Identifier: MIT
//
// Motion 1, "survey grid": a ghost treemap of thin contour-colored lot lines
// that keeps subdividing itself (one cut every ~600 ms) and slowly re-forms by
// merging old cuts away. Normalized coordinates, so resizes are free.

interface Cell {
  x: number;
  y: number;
  w: number;
  h: number;
  cut: 0 | 1 | 2; // 0 leaf, 1 vertical line, 2 horizontal line
  at: number; // cut ratio
  a: Cell | null;
  b: Cell | null;
  born: number;
  dying: number;
}

const STEP_MS = 600;
const GROW_MS = 1100;
const FADE_MS = 1400;

export class SurveyGrid {
  private ctx: CanvasRenderingContext2D;
  private root: Cell;
  private W = 0;
  private H = 0;
  private dpr = 1;
  private raf = 0;
  private lastStep = 0;
  private ro: ResizeObserver;
  private running = false;
  private wanted = true;
  private seed = 0x9e3779b9;

  constructor(private canvas: HTMLCanvasElement, private opts: { reducedMotion: boolean; color?: string; alpha?: number }) {
    this.ctx = canvas.getContext("2d")!;
    this.root = this.cell(0, 0, 1, 1, 0);
    this.ro = new ResizeObserver(() => this.resize());
    this.ro.observe(canvas);
    this.resize();
    // pre-seed so the first frame already reads as a survey
    for (let i = 0; i < this.target() * 0.7; i++) this.split(-1e9);
    document.addEventListener("visibilitychange", this.onVis);
    if (opts.reducedMotion) this.draw(performance.now());
    else this.start();
  }

  destroy(): void {
    this.stop();
    this.ro.disconnect();
    document.removeEventListener("visibilitychange", this.onVis);
  }

  private onVis = (): void => {
    if (document.hidden) this.stop();
    else if (!this.opts.reducedMotion && this.wanted) this.start();
  };

  /** animate only while wanted (scanning, main thread keeping up); otherwise hold the last frame */
  setRunning(on: boolean): void {
    this.wanted = on;
    if (on && !this.opts.reducedMotion && !document.hidden) this.start();
    else if (!on) {
      const was = this.running;
      this.stop();
      if (was) this.draw(performance.now());
    }
  }

  private start(): void {
    if (this.running) return;
    this.running = true;
    this.raf = requestAnimationFrame(this.tick);
  }

  private stop(): void {
    this.running = false;
    cancelAnimationFrame(this.raf);
  }

  private rand(): number {
    // xorshift32: deterministic, allocation-free
    let x = this.seed;
    x ^= x << 13;
    x ^= x >>> 17;
    x ^= x << 5;
    this.seed = x >>> 0;
    return this.seed / 4294967296;
  }

  private cell(x: number, y: number, w: number, h: number, born: number): Cell {
    return { x, y, w, h, cut: 0, at: 0.5, a: null, b: null, born, dying: 0 };
  }

  private target(): number {
    const area = this.W * this.H;
    return Math.max(9, Math.min(64, Math.round(area / 24000)));
  }

  private resize(): void {
    const r = this.canvas.getBoundingClientRect();
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    this.W = Math.max(1, r.width);
    this.H = Math.max(1, r.height);
    this.dpr = dpr;
    this.canvas.width = Math.round(this.W * dpr);
    this.canvas.height = Math.round(this.H * dpr);
    if (!this.running) this.draw(performance.now());
  }

  private leaves(c: Cell, out: Cell[]): void {
    if (!c.a || !c.b) out.push(c);
    else {
      this.leaves(c.a, out);
      this.leaves(c.b, out);
    }
  }

  private mergeable(c: Cell, out: Cell[]): void {
    if (!c.a || !c.b) return;
    if (!c.a.a && !c.b.a && !c.dying) out.push(c);
    this.mergeable(c.a, out);
    this.mergeable(c.b, out);
  }

  /** cut a large leaf, perpendicular to its longer pixel side */
  private split(now: number): void {
    const ls: Cell[] = [];
    this.leaves(this.root, ls);
    let total = 0;
    const wts = ls.map((c) => {
      const w = Math.pow(c.w * c.h, 1.35);
      total += w;
      return w;
    });
    let r = this.rand() * total;
    let c = ls[ls.length - 1];
    for (let i = 0; i < ls.length; i++) {
      r -= wts[i];
      if (r <= 0) {
        c = ls[i];
        break;
      }
    }
    const pw = c.w * this.W, ph = c.h * this.H;
    if (Math.max(pw, ph) < 60) return;
    c.cut = pw >= ph ? 1 : 2;
    c.at = 0.28 + this.rand() * 0.44;
    c.born = now;
    if (c.cut === 1) {
      c.a = this.cell(c.x, c.y, c.w * c.at, c.h, now);
      c.b = this.cell(c.x + c.w * c.at, c.y, c.w * (1 - c.at), c.h, now);
    } else {
      c.a = this.cell(c.x, c.y, c.w, c.h * c.at, now);
      c.b = this.cell(c.x, c.y + c.h * c.at, c.w, c.h * (1 - c.at), now);
    }
  }

  private merge(now: number): void {
    const ms: Cell[] = [];
    this.mergeable(this.root, ms);
    const old = ms.filter((c) => now - c.born > 4000);
    if (!old.length) return;
    const c = old[Math.floor(this.rand() * old.length)];
    c.dying = now;
  }

  private reap(c: Cell, now: number): void {
    if (!c.a || !c.b) return;
    if (c.dying && now - c.dying > FADE_MS) {
      c.a = c.b = null;
      c.cut = 0;
      c.dying = 0;
      return;
    }
    this.reap(c.a, now);
    this.reap(c.b, now);
  }

  private tick = (now: number): void => {
    if (!this.running) return;
    if (now - this.lastStep > STEP_MS) {
      this.lastStep = now;
      const ls: Cell[] = [];
      this.leaves(this.root, ls);
      // hover around the target: split below it, alternate merge/split above
      if (ls.length < this.target() || this.rand() < 0.5) this.split(now);
      else this.merge(now);
      this.reap(this.root, now);
    }
    this.draw(now);
    this.raf = requestAnimationFrame(this.tick);
  };

  private draw(now: number): void {
    const ctx = this.ctx;
    const dpr = this.dpr;
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.clearRect(0, 0, this.canvas.width, this.canvas.height);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.strokeStyle = this.opts.color ?? "#3E7470";
    ctx.lineWidth = 1;
    this.drawCell(this.root, now, this.opts.alpha ?? 0.7);
    ctx.globalAlpha = 1;
  }

  private drawCell(c: Cell, now: number, alpha: number): void {
    if (!c.a || !c.b) return;
    const ctx = this.ctx;
    const W = this.W, H = this.H;
    let grow = this.opts.reducedMotion ? 1 : Math.min(1, (now - c.born) / GROW_MS);
    grow = 1 - Math.pow(1 - grow, 3);
    let a = alpha;
    if (c.dying) a *= Math.max(0, 1 - (now - c.dying) / FADE_MS);
    if (a > 0.003) {
      ctx.globalAlpha = a;
      ctx.beginPath();
      if (c.cut === 1) {
        const x = Math.round((c.x + c.w * c.at) * W) + 0.5;
        const y0 = c.y * H, y1 = (c.y + c.h) * H, m = (y0 + y1) / 2, half = ((y1 - y0) / 2) * grow;
        ctx.moveTo(x, m - half);
        ctx.lineTo(x, m + half);
      } else {
        const y = Math.round((c.y + c.h * c.at) * H) + 0.5;
        const x0 = c.x * W, x1 = (c.x + c.w) * W, m = (x0 + x1) / 2, half = ((x1 - x0) / 2) * grow;
        ctx.moveTo(m - half, y);
        ctx.lineTo(m + half, y);
      }
      ctx.stroke();
    }
    this.drawCell(c.a, now, a);
    this.drawCell(c.b, now, a);
  }
}
