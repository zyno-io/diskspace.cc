// SPDX-License-Identifier: MIT
//
// Wires a Session to the TreeStore and to the signals the Preact chrome reads.
// Heavy consumers (treemap, list) subscribe to `onStore` directly; the
// `version` signal is throttled so chrome re-renders at most ~4×/s mid-scan.

import { batch, computed, signal } from "@preact/signals";
import type { AgentInfo, ByeMsg, ConnState, NodeBatch, Progress, ScanDone, ScanEnded, ScanStarted, ScanState, Session } from "../protocol/types";
import { defaultUnits, type Units } from "../store/format";
import { TreeStore } from "../store/tree";
import { IngestScheduler, trackInput, type IngestEnv } from "./ingest";

export type Phase = "connecting" | "picker" | "scanning" | "done" | "stopped" | "superseded" | "closed" | "error";

export class Controller {
  readonly store = new TreeStore();
  readonly conn = signal<ConnState>({ s: "connecting" });
  readonly info = signal<AgentInfo | null>(null);
  readonly scan = signal<ScanStarted | null>(null);
  readonly progress = signal<Progress | null>(null);
  readonly done = signal<ScanDone | null>(null);
  readonly ended = signal<ScanEnded | null>(null);
  readonly bye = signal<ByeMsg | null>(null);
  readonly units = signal<Units>("decimal");
  readonly version = signal(0);
  readonly zoom = signal(-1);
  readonly selected = signal(-1);
  readonly hovered = signal(-1);
  /** user chose "Change folder" while results are still around */
  readonly picking = signal(false);
  readonly protocolError = signal<string | null>(null);
  /** the running scan is paused (phase stays "scanning") */
  readonly paused = signal(false);
  /** a pause/resume request is in flight (until the agent's SCAN_STATE arrives) */
  readonly pausePending = signal(false);
  /** elapsed ms excluding paused time, from the last SCAN_STATE */
  readonly stateElapsedMs = signal(0);
  /** newer stable version than the agent's, once known and not dismissed */
  readonly latest = signal<string | null>(null);
  /** Stop was pressed; Pause/Resume are hidden and SCAN_STATE ignored until the end arrives */
  readonly stopping = signal(false);
  /** the agent can pause (older agents cannot) */
  readonly canPause = computed(() => this.info.value?.features?.pause === true);

  readonly phase = computed<Phase>(() => {
    const c = this.conn.value;
    if (c.s === "superseded") return "superseded";
    if (c.s === "error") return "error";
    if (c.s === "closed") return "closed";
    if (this.scan.value && !this.picking.value) {
      if (this.done.value) return "done";
      if (this.ended.value) return "stopped";
      return "scanning";
    }
    if (this.info.value && (c.s === "connected" || c.s === "reconnecting")) return "picker";
    return "connecting";
  });

  private listeners = new Set<() => void>();
  private resetListeners = new Set<() => void>();
  private vTimer = 0;
  private vLast = 0;
  private unitsTouched = false;
  private pendingTimer = 0;

  /** paced main-thread ingest; `backlog` > 0 tells renderers to back off */
  readonly ingest: IngestScheduler;
  private untrack: (() => void) | null = null;

  constructor(
    readonly session: Session,
    env?: IngestEnv,
  ) {
    this.ingest = new IngestScheduler(
      {
        applySlice: (b, from, deadline) => this.store.applySlice(b, from, deadline),
        batchDone: (b) => this.session.applied(b.wireBytes, b.gen),
        done: (m) => this.onDone(m),
        ended: (m) => this.onEnded(m),
        replayDone: () => this.flush(),
        tick: () => this.changed(false),
      },
      env,
    );
    if (!env && typeof window !== "undefined") this.untrack = trackInput(this.ingest);
  }

  /** stop listening for input (view teardown) */
  dispose(): void {
    window.clearTimeout(this.pendingTimer);
    this.untrack?.();
    this.untrack = null;
  }

  start(): void {
    this.session.start({
      state: (s) => {
        this.conn.value = s;
      },
      agentInfo: (i) => {
        batch(() => {
          this.info.value = i;
          if (!this.unitsTouched) this.units.value = defaultUnits(i.os);
        });
      },
      scanStarted: (m) => this.onScanStarted(m),
      nodes: (b) => this.onNodes(b),
      progress: (p) => {
        if (p.scanId === this.store.scanId) this.progress.value = p;
      },
      scanDone: (m) => this.ingest.pushDone(m),
      scanEnded: (m) => this.ingest.pushEnded(m),
      scanState: (m) => this.onScanState(m),
      replayDone: () => this.ingest.pushReplayDone(),
      bye: (m) => {
        this.bye.value = m;
        if (m.reason === "superseded") this.conn.value = { s: "superseded" };
        else this.conn.value = { s: "closed", reason: m.reason };
      },
      error: (m) => {
        this.conn.value = { s: "error", kind: "protocol", message: m.message || m.code };
      },
    });
  }

  setUnits(u: Units): void {
    this.unitsTouched = true;
    this.units.value = u;
  }

  /** subscribe to store mutations (called per batch, unthrottled) */
  onStore(fn: () => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }
  onReset(fn: () => void): () => void {
    this.resetListeners.add(fn);
    return () => this.resetListeners.delete(fn);
  }

  scanPath(path: string): void {
    this.picking.value = false;
    this.session.scanStart(path, { crossMounts: false });
  }

  stop(): void {
    const s = this.scan.value;
    if (s && !this.done.value && !this.ended.value) {
      batch(() => {
        this.stopping.value = true;
        this.paused.value = false;
        this.pausePending.value = false;
      });
      window.clearTimeout(this.pendingTimer);
      this.session.scanCancel(s.scanId);
    }
  }

  /** ask the agent to pause; the UI changes when its SCAN_STATE arrives */
  pause(): void {
    this.requestPause(true);
  }
  resume(): void {
    this.requestPause(false);
  }
  private requestPause(paused: boolean): void {
    const s = this.scan.value;
    if (!s || this.stopping.value || !this.canPause.value || this.done.value || this.ended.value || this.pausePending.value || this.paused.value === paused) return;
    this.pausePending.value = true;
    window.clearTimeout(this.pendingTimer);
    // never leave the button stuck if the reply is lost
    this.pendingTimer = window.setTimeout(() => (this.pausePending.value = false), 5000);
    this.session.scanPause(s.scanId, paused);
  }

  private clearPause(): void {
    window.clearTimeout(this.pendingTimer);
    this.pendingTimer = 0;
    this.paused.value = false;
    this.pausePending.value = false;
    this.stopping.value = false;
  }

  private onScanState(m: ScanState): void {
    if (m.scanId !== this.store.scanId || this.done.value || this.ended.value || this.stopping.value) return;
    window.clearTimeout(this.pendingTimer);
    this.pendingTimer = 0;
    batch(() => {
      this.paused.value = m.paused;
      this.pausePending.value = false;
      this.stateElapsedMs.value = m.elapsedMs;
    });
  }

  rescan(): void {
    const s = this.scan.value;
    if (s) this.scanPath(s.path);
  }

  async showAllFiles(node: number): Promise<void> {
    const st = this.store;
    const wire = st.wire(node);
    if (wire < 0) return;
    const df = await this.session.dirFiles(st.scanId, wire, 10_000);
    if (st.applyDirFiles(df)) this.changed(true);
  }

  // ---- internals --------------------------------------------------------------

  private onScanStarted(m: ScanStarted): void {
    // Anything still queued is obsolete: a new scan, or a reconnect replaying this
    // scan from the start (same scanId) into the reset store.
    this.ingest.discardAll();
    this.store.reset(m);
    batch(() => {
      this.scan.value = m;
      this.progress.value = null;
      this.done.value = null;
      this.ended.value = null;
      this.picking.value = false;
      this.protocolError.value = null;
      this.clearPause();
      this.zoom.value = this.store.root;
      this.selected.value = -1;
      this.hovered.value = -1;
      this.version.value = this.store.version;
    });
    for (const fn of this.resetListeners) fn();
  }

  private onNodes(b: NodeBatch): void {
    // stale batches are queued too: they are acknowledged in order, cheaply
    this.ingest.pushBatch(b);
  }

  private onEnded(m: ScanEnded): void {
    if (m.scanId !== this.store.scanId) return;
    this.flush();
    batch(() => {
      this.clearPause();
      this.ended.value = m;
    });
  }

  private onDone(m: ScanDone): void {
    const st = this.store;
    if (m.scanId !== st.scanId) return;
    // synthetic remainder only for whole-volume scans
    const s = this.scan.value;
    const vol = this.info.value?.volumes.find((v) => s && samePath(v.path, s.path));
    if (vol && s) {
      const rest = vol.used - st.totAlloc[st.root];
      st.setSynthetic(rest > vol.used * 0.01 ? rest : 0);
    }
    if (st.orphanCount() > 0 || !st.rootSettled()) {
      this.protocolError.value = `scan finished with ${st.orphanCount()} unattached folders`;
      console.warn("diskspace: SCAN_DONE before tree settled", { orphans: st.orphanCount() });
    }
    this.changed(true);
    batch(() => {
      this.clearPause();
      this.done.value = m;
    });
  }

  private flush(): void {
    this.changed(true);
  }

  private changed(now: boolean): void {
    for (const fn of this.listeners) fn();
    const t = performance.now();
    if (now || t - this.vLast > 250) {
      window.clearTimeout(this.vTimer);
      this.vTimer = 0;
      this.vLast = t;
      this.version.value = this.store.version;
    } else if (!this.vTimer) {
      this.vTimer = window.setTimeout(() => {
        this.vTimer = 0;
        this.vLast = performance.now();
        this.version.value = this.store.version;
      }, 250 - (t - this.vLast));
    }
  }
}

export function samePath(a: string, b: string): boolean {
  const n = (p: string) => (p.length > 1 ? p.replace(/[\\/]+$/, "") : p).toLowerCase();
  return n(a) === n(b);
}
