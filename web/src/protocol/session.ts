// SPDX-License-Identifier: MIT
import type { BrowseResult, DirFiles, ScanOptions, Session, SessionEvents } from "./types";
import type { WorkerCommand, WorkerEvent } from "./worker-protocol";

export interface RealSessionOptions {
  /** base64url session id from /s/<sid> */
  sid: string;
  /** 32-byte fragment secret K */
  key: Uint8Array;
  /** e.g. wss://diskspace.cc/v1/view?sid=<sid> or ws://127.0.0.1:port/v1/view?sid=<sid> */
  wsUrl: string;
}

interface Pending<T> {
  resolve(v: T): void;
  reject(e: Error): void;
  timer: ReturnType<typeof setTimeout>;
}

const REQUEST_TIMEOUT_MS = 30_000;

export class RealSession implements Session {
  private worker: Worker | null = null;
  private events: SessionEvents | null = null;
  private nextReq = 1;
  private browses = new Map<number, Pending<BrowseResult>>();
  private files = new Map<number, Pending<DirFiles>>();

  constructor(readonly opts: RealSessionOptions) {}

  start(events: SessionEvents): void {
    if (this.worker) return;
    this.events = events;
    const w = new Worker(new URL("./worker.ts", import.meta.url), { type: "module" });
    this.worker = w;
    w.onmessage = (ev: MessageEvent<WorkerEvent>) => this.onEvent(ev.data);
    w.onerror = () => events.state({ s: "error", kind: "protocol", message: "protocol worker crashed" });
    // K goes to the worker once; never logged.
    this.send({ t: "init", sid: this.opts.sid, key: this.opts.key.slice(), wsUrl: this.opts.wsUrl });
  }

  private send(c: WorkerCommand): void {
    this.worker?.postMessage(c);
  }

  private onEvent(e: WorkerEvent): void {
    const ev = this.events;
    if (!ev) return;
    switch (e.t) {
      case "state":
        if (e.s.s === "reconnecting" || e.s.s === "closed" || e.s.s === "error" || e.s.s === "superseded") this.rejectAll("connection lost");
        ev.state(e.s);
        break;
      case "agentInfo":
        ev.agentInfo(e.v);
        break;
      case "scanStarted":
        ev.scanStarted(e.v);
        break;
      case "nodes":
        ev.nodes(e.v);
        break;
      case "progress":
        ev.progress(e.v);
        break;
      case "scanDone":
        ev.scanDone(e.v);
        break;
      case "scanEnded":
        ev.scanEnded(e.v);
        break;
      case "replayDone":
        ev.replayDone();
        break;
      case "bye":
        ev.bye(e.v);
        break;
      case "error":
        ev.error(e.v);
        break;
      case "browseResult": {
        const p = this.browses.get(e.v.reqId);
        if (p) {
          this.browses.delete(e.v.reqId);
          clearTimeout(p.timer);
          p.resolve(e.v);
        }
        break;
      }
      case "dirFiles": {
        const p = this.files.get(e.v.reqId);
        if (p) {
          this.files.delete(e.v.reqId);
          clearTimeout(p.timer);
          p.resolve(e.v);
        }
        break;
      }
    }
  }

  private rejectAll(why: string): void {
    for (const m of [this.browses, this.files] as Map<number, Pending<unknown>>[]) {
      for (const p of m.values()) {
        clearTimeout(p.timer);
        p.reject(new Error(why));
      }
      m.clear();
    }
  }

  private request<T>(map: Map<number, Pending<T>>, cmd: (reqId: number) => WorkerCommand): Promise<T> {
    if (!this.worker) return Promise.reject(new Error("session not started"));
    const reqId = this.nextReq++;
    return new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        map.delete(reqId);
        reject(new Error("request timed out"));
      }, REQUEST_TIMEOUT_MS);
      map.set(reqId, { resolve, reject, timer });
      this.send(cmd(reqId));
    });
  }

  scanStart(path: string, opts?: ScanOptions): void {
    this.send({ t: "scanStart", path, opts });
  }
  scanCancel(scanId: number): void {
    this.send({ t: "scanCancel", scanId });
  }
  browse(path: string): Promise<BrowseResult> {
    return this.request(this.browses, (reqId) => ({ t: "browse", reqId, path }));
  }
  dirFiles(scanId: number, dirId: number, limit = 500): Promise<DirFiles> {
    return this.request(this.files, (reqId) => ({ t: "dirFiles", reqId, scanId, dirId, limit }));
  }
  applied(wireBytes: number, gen?: number): void {
    this.send({ t: "applied", n: wireBytes, gen });
  }
  takeOver(): void {
    this.send({ t: "takeOver" });
  }
  close(): void {
    this.send({ t: "close" });
    this.rejectAll("session closed");
    this.worker?.terminate();
    this.worker = null;
  }
}
