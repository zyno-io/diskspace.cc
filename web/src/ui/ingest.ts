// SPDX-License-Identifier: MIT
//
// Main-thread ingest scheduler. Decoded NODES batches (and the control events
// that must stay ordered behind them) are queued and applied in time slices
// sized to the frame budget, so a fast scan cannot starve rendering or input.
// A batch is reported applied (flow-control credit) only once fully applied.

import type { NodeBatch, ScanDone, ScanEnded } from "../protocol/types";

export type IngestItem =
  | { k: "batch"; b: NodeBatch; from: number }
  | { k: "done"; m: ScanDone }
  | { k: "ended"; m: ScanEnded }
  | { k: "replayDone" };

export interface IngestHandlers {
  /** apply records [from, …) until `deadline`; return the resume index (=== length when finished) */
  applySlice(b: NodeBatch, from: number, deadline: number): number;
  /** a batch is fully applied (or discarded): report it to the session */
  batchDone(b: NodeBatch): void;
  done(m: ScanDone): void;
  ended(m: ScanEnded): void;
  replayDone(): void;
  /** end of a slice that applied or handled something (renderers invalidate here) */
  tick(): void;
}

export interface IngestEnv {
  now(): number;
  /** call back once, soon: with the rAF timestamp when visible, null when the page is hidden */
  request(cb: (frameTs: number | null) => void): void;
}

export const BUDGET_DEFAULT = 6;
export const BUDGET_INPUT = 3;
export const BUDGET_MIN = 2;
export const BUDGET_MAX = 8;
export const BUDGET_HIDDEN = 25;
export const INPUT_WINDOW_MS = 300;
export const SLOW_FRAME_MS = 24;

export class IngestScheduler {
  private q: IngestItem[] = [];
  private head = 0;
  private pending = false;
  private adaptive = BUDGET_DEFAULT;
  private lastInput = -Infinity;
  private lastFrame: number | null = null;
  /** budget of the most recent slice (tests, diagnostics) */
  lastBudget = BUDGET_DEFAULT;

  constructor(
    private h: IngestHandlers,
    private env: IngestEnv = browserEnv(),
  ) {}

  /** queued items not yet fully handled; renderers use this to back off */
  get backlog(): number {
    return this.q.length - this.head;
  }

  noteInput(): void {
    this.lastInput = this.env.now();
  }

  pushBatch(b: NodeBatch): void {
    this.q.push({ k: "batch", b, from: 0 });
    this.kick();
  }
  pushDone(m: ScanDone): void {
    this.q.push({ k: "done", m });
    this.kick();
  }
  pushEnded(m: ScanEnded): void {
    this.q.push({ k: "ended", m });
    this.kick();
  }
  pushReplayDone(): void {
    this.q.push({ k: "replayDone" });
    this.kick();
  }

  /** drop everything queued (scan restarted or replayed); batches are still reported applied */
  discardAll(): void {
    for (let i = this.head; i < this.q.length; i++) {
      const it = this.q[i];
      if (it.k === "batch") this.h.batchDone(it.b);
    }
    this.q = [];
    this.head = 0;
  }

  /** drop queued batches matching `drop` (a new scan started); each is still reported applied */
  discard(drop: (b: NodeBatch) => boolean): void {
    const keep: IngestItem[] = [];
    for (let i = this.head; i < this.q.length; i++) {
      const it = this.q[i];
      if (it.k === "batch" && drop(it.b)) this.h.batchDone(it.b);
      else keep.push(it);
    }
    this.q = keep;
    this.head = 0;
  }

  private kick(): void {
    if (this.pending) return;
    this.pending = true;
    this.env.request(this.run);
  }

  private budget(hidden: boolean): number {
    if (hidden) return BUDGET_HIDDEN;
    return this.env.now() - this.lastInput < INPUT_WINDOW_MS ? Math.min(this.adaptive, BUDGET_INPUT) : this.adaptive;
  }

  private run = (frameTs: number | null): void => {
    this.pending = false;
    const hidden = frameTs === null;
    if (!hidden) {
      if (this.lastFrame !== null) {
        const dt = frameTs - this.lastFrame;
        this.adaptive = dt > SLOW_FRAME_MS ? Math.max(BUDGET_MIN, this.adaptive * 0.7) : Math.min(BUDGET_MAX, this.adaptive + 0.5);
      }
      this.lastFrame = frameTs;
    } else this.lastFrame = null;
    const budget = (this.lastBudget = this.budget(hidden));
    const deadline = this.env.now() + budget;
    let worked = false;
    while (this.head < this.q.length) {
      const it = this.q[this.head];
      if (it.k === "batch") {
        const n = it.b.records.length;
        if (it.from < n) it.from = this.h.applySlice(it.b, it.from, deadline);
        worked = true;
        if (it.from < n) break; // out of time mid-batch; resume next slice
        this.head++;
        this.h.batchDone(it.b);
      } else {
        this.head++;
        worked = true;
        if (it.k === "done") this.h.done(it.m);
        else if (it.k === "ended") this.h.ended(it.m);
        else this.h.replayDone();
      }
      if (this.env.now() >= deadline) break;
    }
    if (this.head >= this.q.length) {
      this.q = [];
      this.head = 0;
      this.lastFrame = null;
    } else if (this.head > 512 && this.head * 2 > this.q.length) {
      this.q = this.q.slice(this.head);
      this.head = 0;
    }
    if (worked) this.h.tick();
    if (this.backlog > 0) this.kick();
  };
}

/** rAF while visible, a MessageChannel loop while hidden (rAF is paused there) */
export function browserEnv(): IngestEnv {
  let chan: MessageChannel | null = null;
  let chanCb: (() => void) | null = null;
  return {
    now: () => performance.now(),
    request(cb) {
      let fired = false;
      const go = (ts: number | null) => {
        if (fired) return;
        fired = true;
        cb(ts);
      };
      const hiddenRun = () => {
        if (typeof MessageChannel === "undefined") {
          setTimeout(() => go(null), 0);
          return;
        }
        if (!chan) {
          chan = new MessageChannel();
          chan.port1.onmessage = () => {
            const f = chanCb;
            chanCb = null;
            f?.();
          };
        }
        chanCb = () => go(null);
        chan.port2.postMessage(0);
      };
      if (typeof document !== "undefined" && document.hidden) return hiddenRun();
      if (typeof requestAnimationFrame === "undefined") return hiddenRun();
      requestAnimationFrame((ts) => go(ts));
      // rAF stops if the tab is hidden after we asked
      setTimeout(() => {
        if (!fired && typeof document !== "undefined" && document.hidden) hiddenRun();
      }, 100);
    },
  };
}

/** feed user input times to the scheduler so it yields more to the main thread */
export function trackInput(s: IngestScheduler): () => void {
  const f = () => s.noteInput();
  const evs = ["pointerdown", "pointermove", "wheel", "keydown", "touchstart"] as const;
  for (const e of evs) window.addEventListener(e, f, { passive: true, capture: true });
  return () => {
    for (const e of evs) window.removeEventListener(e, f, { capture: true });
  };
}
