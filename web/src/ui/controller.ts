// SPDX-License-Identifier: MIT
//
// Wires a Session to the TreeStore and to the signals the Preact chrome reads.
// Heavy consumers (treemap, list) subscribe to `onStore` directly; the
// `version` signal is throttled so chrome re-renders at most ~4×/s mid-scan.

import { batch, computed, signal } from "@preact/signals";
import { MAX_V2A_BODY } from "../protocol/messages";
import type { AgentInfo, ByeMsg, ConnState, Deleted, DeleteItem, DeleteMode, DeleteStatus, NodeBatch, Progress, ScanDone, ScanEnded, ScanStarted, ScanState, Session } from "../protocol/types";
import { defaultUnits, type Units } from "../store/format";
import { KIND_NONE, TreeStore } from "../store/tree";
import { DEFAULT_MAX_ITEMS, MAX_FOLDERS, featureReason, itemKey, nodeReason, queuedAncestor, resolveKey, wireItem } from "./deletion";
import { IngestScheduler, trackInput, type IngestEnv } from "./ingest";

export type Phase = "connecting" | "picker" | "scanning" | "done" | "stopped" | "superseded" | "closed" | "error";

/** a delete request this viewer sent (or a running job it adopted after reconnecting) */
export interface DeleteRequestView {
  reqId: number;
  mode: DeleteMode;
  /** what was asked for, as shown at request time (empty for an adopted job) */
  names: string[];
  bytes: number;
  count: number;
  job: number | null;
  status: DeleteStatus | null;
  /** Cancel was pressed (sent once the agent names the job) */
  cancelWanted: boolean;
  /** ended without an agent answer (e.g. "disconnected") */
  localError?: string;
}

export interface MenuState {
  node: number;
  x: number;
  y: number;
  /** opened from the keyboard: focus the first item */
  keyboard: boolean;
}

const TERMINAL = new Set(["done", "refused", "failed"]);
const te = new TextEncoder();

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

  // ---- deletion queue ----------------------------------------------------------
  /** queued nodes in the order they were added */
  readonly queue = signal<number[]>([]);
  readonly queueSet = computed(() => new Set(this.queue.value));
  readonly drawerOpen = signal(false);
  readonly menu = signal<MenuState | null>(null);
  readonly request = signal<DeleteRequestView | null>(null);
  /** short-lived status line ("Path copied", why something can't be queued) */
  readonly notice = signal<string | null>(null);
  /** why the deletion feature can't be used at all (older agent, no confirmation channel), or null */
  readonly deleteUnavailable = computed(() => featureReason(this.info.value));
  /** persisted keys waiting for the tree (resolved after REPLAY_DONE or the scan's end) */
  private pendingKeys: string[] = [];
  private nextReqId = (Date.now() % 1_000_000_000) + 1;
  private noticeTimer = 0;

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
        replayDone: () => {
          this.resolvePending();
          this.flush();
        },
        deleted: (m) => this.onDeleted(m),
        tick: () => this.changed(false),
      },
      env,
    );
    if (!env && typeof window !== "undefined") this.untrack = trackInput(this.ingest);
  }

  /** stop listening for input (view teardown) */
  dispose(): void {
    window.clearTimeout(this.pendingTimer);
    window.clearTimeout(this.noticeTimer);
    this.untrack?.();
    this.untrack = null;
  }

  start(): void {
    this.session.start({
      state: (s) => {
        this.conn.value = s;
        // the agent cancels a request that is still unapproved when its connection drops
        const r = this.request.peek();
        if (r && !this.requestFinished(r) && s.s !== "connected" && s.s !== "handshaking" && s.s !== "connecting") {
          const unapproved = !r.status || r.status.state === "preparing" || r.status.state === "awaiting";
          if (unapproved) this.request.value = { ...r, localError: "disconnected" };
        }
      },
      agentInfo: (i) => {
        batch(() => {
          this.info.value = i;
          if (!this.unitsTouched) this.units.value = defaultUnits(i.os);
          this.adoptJob(i);
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
      deleteStatus: (m) => this.onDeleteStatus(m),
      deleted: (m) => this.ingest.pushDeleted(m),
      bye: (m) => {
        this.bye.value = m;
        if (m.reason === "superseded") this.conn.value = { s: "superseded" };
        else this.conn.value = { s: "closed", reason: m.reason };
      },
      error: (m) => {
        // a scan asked for while a deletion runs is refused; the session stays up
        if (m.code === "busy") {
          this.flash("Wait for the deletion to finish before scanning");
          return;
        }
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
    const d = this.info.peek()?.deleting;
    if (d && !TERMINAL.has(d.state)) {
      this.flash("Wait for the deletion to finish before scanning");
      return;
    }
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
    // the listing replaces the dir's file slots: carry queued files over by key
    const queued = this.queue.peek().filter((q) => st.parent[q] === node && !st.isDir(q));
    const keys = queued.map((q) => itemKey(st, q));
    const menu = this.menu.peek();
    if (!st.applyDirFiles(df)) return;
    batch(() => {
      if (queued.length) {
        const gone = new Set(queued);
        const next = this.queue.peek().filter((q) => !gone.has(q));
        for (const k of keys) {
          const n = k ? resolveKey(st, k) : -1;
          if (n >= 0 && !next.includes(n)) next.push(n);
        }
        this.queue.value = next;
        this.saveQueue();
      }
      const dead = (n: number) => n >= 0 && st.kind[n] === KIND_NONE;
      if (dead(this.selected.peek())) this.selected.value = node;
      if (dead(this.hovered.peek())) this.hovered.value = -1;
      if (menu && dead(menu.node)) this.menu.value = null;
    });
    this.changed(true);
  }

  // ---- deletion queue ------------------------------------------------------------

  /** why `n` can't be added to the queue right now, or null (also null when already queued) */
  queueReason(n: number): string | null {
    const st = this.store;
    const f = this.deleteUnavailable.peek();
    if (f) return f;
    const r = nodeReason(st, n, this.info.peek());
    if (r) return r;
    const q = this.queueSet.peek();
    if (q.has(n)) return null;
    const a = queuedAncestor(st, n, q);
    if (a >= 0) return `Included in queued ${st.names[a] || st.rootPath}`;
    const max = this.info.peek()?.features?.delete?.maxItems ?? DEFAULT_MAX_ITEMS;
    if (this.queue.peek().length >= max) return `The queue is full (${max} items)`;
    return null;
  }

  isQueued(n: number): boolean {
    return this.queueSet.peek().has(n);
  }

  /** add or remove; returns the reason when `n` can't be added */
  toggleQueue(n: number): string | null {
    if (this.isQueued(n)) {
      this.removeFromQueue(n);
      return null;
    }
    return this.addToQueue(n);
  }

  addToQueue(n: number): string | null {
    const why = this.queueReason(n);
    if (why) return why;
    if (this.isQueued(n)) return null;
    const st = this.store;
    // a queued folder covers everything below it
    const next = this.queue.peek().filter((q) => !st.isAncestor(n, q));
    next.push(n);
    this.queue.value = next;
    this.saveQueue();
    return null;
  }

  removeFromQueue(n: number): void {
    const q = this.queue.peek();
    if (!q.includes(n)) return;
    this.queue.value = q.filter((x) => x !== n);
    this.saveQueue();
    this.closeDrawerIfEmpty();
  }

  clearQueue(): void {
    this.queue.value = [];
    this.saveQueue();
    this.closeDrawerIfEmpty();
  }

  /** an empty drawer has nothing to show once no request is pending (its pill is gone too) */
  private closeDrawerIfEmpty(): void {
    if (this.queue.peek().length === 0 && !this.request.peek()) this.drawerOpen.value = false;
  }

  /** why a delete request can't be sent now (scan state, other jobs), or null */
  applyReason(): string | null {
    const f = this.deleteUnavailable.peek();
    if (f) return f;
    const ph = this.phase.peek();
    if (ph === "scanning") return "Finish or stop the scan first";
    if (ph !== "done" && ph !== "stopped" || this.conn.peek().s !== "connected") return "Not connected";
    const r = this.request.peek();
    if (r && !this.requestFinished(r)) return "Another request is in progress";
    const d = this.info.peek()?.deleting;
    if (d && !TERMINAL.has(d.state)) return "A deletion is running on this computer";
    return null;
  }

  /** the agent can move to the Trash (else only permanent deletion is offered) */
  canTrash(): boolean {
    return this.info.peek()?.features?.delete?.trash !== false;
  }

  /** send the whole queue */
  applyQueue(mode: DeleteMode): string | null {
    return this.sendDelete(this.queue.peek(), mode);
  }

  /** "Move to Trash now": one item, skipping the queue */
  trashNow(n: number): string | null {
    const why = this.deleteUnavailable.peek() ?? nodeReason(this.store, n, this.info.peek());
    if (why) return why;
    return this.sendDelete([n], "trash");
  }

  private sendDelete(nodes: number[], mode: DeleteMode): string | null {
    const why = this.applyReason();
    if (why) return why;
    if (mode === "trash" && !this.canTrash()) return "The Trash isn't available on this computer";
    const st = this.store;
    const items: DeleteItem[] = [];
    const names: string[] = [];
    let bytes = 0;
    const folders = new Set<number>();
    for (const n of nodes) {
      const it = wireItem(st, n);
      if (!it || st.isRemoved(n)) continue;
      folders.add(st.parent[n]);
      items.push(it);
      names.push(st.names[n]);
      bytes += st.size(n);
    }
    if (items.length === 0) return "Nothing to delete";
    // the host's prompt names the folder of every item, so it refuses wider spreads
    if (folders.size > MAX_FOLDERS) return `These items are in ${folders.size} folders. Send items from at most ${MAX_FOLDERS} folders at a time.`;
    const req = { reqId: this.nextReqId++, scanId: st.scanId, mode, items };
    if (te.encode(JSON.stringify(req)).length + 1 > MAX_V2A_BODY) return "Too many items to send at once. Remove some and try again.";
    this.request.value = { reqId: req.reqId, mode, names, bytes, count: items.length, job: null, status: null, cancelWanted: false };
    this.menu.value = null;
    this.session.deleteRequest(req);
    return null;
  }

  requestFinished(r: DeleteRequestView): boolean {
    return !!r.localError || (!!r.status && TERMINAL.has(r.status.state));
  }

  /** withdraw before approval, or stop after the current item once running */
  cancelRequest(): void {
    const r = this.request.peek();
    if (!r || this.requestFinished(r) || r.cancelWanted) return;
    this.request.value = { ...r, cancelWanted: true };
    // before the agent names the job, the cancel goes out with its first status
    if (r.job !== null) this.session.deleteCancel(r.job);
  }

  closeRequest(): void {
    const r = this.request.peek();
    if (r && !this.requestFinished(r)) return;
    this.request.value = null;
    this.closeDrawerIfEmpty();
  }

  private onDeleteStatus(m: DeleteStatus): void {
    // AGENT_INFO.deleting only arrives on connect: keep it current from the job's own status
    const info = this.info.peek();
    if (info?.deleting && info.deleting.job === m.job) {
      this.info.value = { ...info, deleting: TERMINAL.has(m.state) ? null : { job: m.job, state: m.state, done: m.done, total: m.total } };
    }
    const r = this.request.peek();
    const mine = !!r && (r.job === m.job || (r.job === null && r.reqId === m.reqId));
    if (!r || !mine) {
      // a job from before a reload or reconnect: follow it while it runs
      if ((r && !this.requestFinished(r)) || TERMINAL.has(m.state)) return;
      this.request.value = { reqId: m.reqId, mode: "trash", names: [], bytes: 0, count: m.total, job: m.job, status: m, cancelWanted: false };
      return;
    }
    if (this.requestFinished(r) && !r.localError) return;
    this.request.value = { ...r, job: m.job, status: m, localError: undefined };
    if (r.job === null && r.cancelWanted && !TERMINAL.has(m.state)) this.session.deleteCancel(m.job);
  }

  /** AGENT_INFO.deleting after a reconnect: show the running job */
  private adoptJob(i: AgentInfo): void {
    const d = i.deleting && !TERMINAL.has(i.deleting.state) ? i.deleting : null;
    const r = this.request.peek();
    // our request is not the agent's running job: it ended (or never arrived) while we were away
    if (r && !this.requestFinished(r) && r.job !== null && (!d || d.job !== r.job)) {
      this.request.value = { ...r, localError: r.status && r.status.state === "running" ? "ended_offline" : "disconnected" };
    }
    if (!d) return;
    if (r && (r.job === d.job || !this.requestFinished(r))) return;
    this.request.value = { reqId: 0, mode: "trash", names: [], bytes: 0, count: d.total, job: d.job, status: { job: d.job, reqId: 0, state: d.state, total: d.total, done: d.done, freedAlloc: 0 }, cancelWanted: false };
  }

  private onDeleted(m: Deleted): void {
    const st = this.store;
    if (!st.applyDeleted(m)) return;
    batch(() => {
      const z = this.zoom.peek();
      if (z >= 0 && st.isRemoved(z)) this.zoom.value = st.survivor(z);
      const sel = this.selected.peek();
      if (sel >= 0 && st.isRemoved(sel)) this.selected.value = st.survivor(sel);
      const hov = this.hovered.peek();
      if (hov >= 0 && st.isRemoved(hov)) this.hovered.value = -1;
      const menu = this.menu.peek();
      if (menu && st.isRemoved(menu.node)) this.menu.value = null;
      // deleted items leave the queue; failed ones stay
      const q = this.queue.peek();
      const live = q.filter((n) => !st.isRemoved(n));
      if (live.length !== q.length) {
        this.queue.value = live;
        this.saveQueue();
        this.closeDrawerIfEmpty();
      }
    });
    this.changed(true);
  }

  /** brief status line */
  flash(text: string): void {
    this.notice.value = text;
    window.clearTimeout(this.noticeTimer);
    this.noticeTimer = window.setTimeout(() => (this.notice.value = null), 2600);
  }

  // ---- queue persistence (sessionStorage, keyed by sid and scan) -------------------

  private queueKeys(): string[] {
    const st = this.store;
    const out: string[] = [];
    for (const n of this.queue.peek()) {
      const k = itemKey(st, n);
      if (k) out.push(k);
    }
    return out;
  }

  private storageKey(scanId: number): string {
    return `diskspace:dq:${this.session.id ?? "local"}:${scanId}`;
  }

  private loadQueue(scanId: number): string[] {
    try {
      const raw = sessionStorage.getItem(this.storageKey(scanId));
      const v = raw ? (JSON.parse(raw) as unknown) : null;
      return Array.isArray(v) ? v.filter((k): k is string => typeof k === "string") : [];
    } catch {
      return [];
    }
  }

  private saveQueue(): void {
    const st = this.store;
    if (st.scanId < 0) return;
    const keys = [...new Set([...this.queueKeys(), ...this.pendingKeys])];
    try {
      const k = this.storageKey(st.scanId);
      if (keys.length) sessionStorage.setItem(k, JSON.stringify(keys));
      else sessionStorage.removeItem(k);
    } catch {
      /* storage blocked: the queue lives in memory only */
    }
  }

  /** map persisted keys back to nodes once the tree is complete; unresolved ones are dropped */
  private resolvePending(): void {
    if (this.pendingKeys.length === 0) return;
    const keys = this.pendingKeys;
    this.pendingKeys = [];
    const st = this.store;
    const info = this.info.peek();
    const q = [...this.queue.peek()];
    const set = new Set(q);
    for (const k of keys) {
      const n = resolveKey(st, k);
      if (n < 0 || set.has(n) || nodeReason(st, n, info)) continue;
      q.push(n);
      set.add(n);
    }
    // keep only the outermost of nested entries
    this.queue.value = q.filter((n) => queuedAncestor(st, n, set) < 0);
    this.saveQueue();
  }

  // ---- internals --------------------------------------------------------------

  private onScanStarted(m: ScanStarted): void {
    // Anything still queued is obsolete: a new scan, or a reconnect replaying this
    // scan from the start (same scanId) into the reset store.
    this.ingest.discardAll();
    // same scan replayed: keep what was queued and re-resolve it once the tree is back
    const keep = this.store.scanId === m.scanId ? [...this.queueKeys(), ...this.pendingKeys] : [];
    this.store.reset(m);
    this.pendingKeys = [...new Set([...keep, ...this.loadQueue(m.scanId)])];
    batch(() => {
      this.queue.value = [];
      this.menu.value = null;
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
    this.resolvePending();
    this.flush();
    batch(() => {
      this.clearPause();
      this.ended.value = m;
    });
  }

  private onDone(m: ScanDone): void {
    const st = this.store;
    if (m.scanId !== st.scanId) return;
    // synthetic remainder only for whole-volume scans, from the volume's usage at
    // SCAN_STARTED: a replay after deletions computes the same remainder
    const s = this.scan.value;
    const whole = !!s && !!this.info.value?.volumes.some((v) => samePath(v.path, s.path));
    const used = s?.volume?.used ?? 0;
    if (whole && used > 0) {
      const rest = used - st.totAlloc[st.root];
      st.setSynthetic(rest > used * 0.01 ? rest : 0);
    }
    this.resolvePending();
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
