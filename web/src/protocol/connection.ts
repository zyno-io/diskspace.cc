// SPDX-License-Identifier: MIT
// Worker-side connection logic: WebSocket + handshake + decrypt + decompress + decode.
// Kept separate from worker.ts so it can be exercised in Node with a fake socket.
import { finish, HandshakeError, viewerHello } from "./crypto";
import type { ViewerState } from "./crypto";
import { decodeDirFiles, decodeNodes } from "./codec";
import { ViewerCipher } from "./frame";
import { decodeA2VJson, framePlain, msg } from "./messages";
import { MSG } from "./types";
import type { ConnState } from "./types";
import type { WorkerCommand, WorkerEvent } from "./worker-protocol";
import { StreamDecoder } from "./zstream";
import { ProtocolError } from "./varint";

export interface SocketLike {
  binaryType: string;
  onopen: ((ev: unknown) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onclose: ((ev: { code: number; reason?: string }) => void) | null;
  onerror: ((ev: unknown) => void) | null;
  send(data: Uint8Array): void;
  close(code?: number, reason?: string): void;
}

export interface ConnectionOptions {
  sid: string;
  key: Uint8Array;
  wsUrl: string;
  ua?: string;
  post: (e: WorkerEvent) => void;
  createSocket?: (url: string) => SocketLike;
  /** override timings (tests) */
  timing?: Partial<typeof DEFAULT_TIMING>;
}

const DEFAULT_TIMING = { handshakeMs: 10_000, pingMs: 20_000, idleMs: 60_000, backoffBaseMs: 500, backoffMaxMs: 15_000 };
const CREDIT_FLUSH = 1 << 20;

type Phase = "idle" | "await-m2" | "active";

export class ViewerConnection {
  private readonly o: ConnectionOptions;
  private readonly timing: typeof DEFAULT_TIMING;
  private ws: SocketLike | null = null;
  private gen = 0;
  private phase: Phase = "idle";
  private hello: ViewerState | null = null;
  private cipher: ViewerCipher | null = null;
  private zdec: StreamDecoder | null = null;
  private code = "";
  private recvChain: Promise<void> = Promise.resolve();
  private sendChain: Promise<void> = Promise.resolve();
  private sawInit = false;
  private confirmed = false;
  private attempt = 0;
  private terminal = false; // no automatic reconnect
  private superseded = false;
  private inflight = 0;
  private pendingCredit = 0;
  private timers: { hs?: ReturnType<typeof setTimeout>; ping?: ReturnType<typeof setInterval>; retry?: ReturnType<typeof setTimeout>; lastRx: number } = { lastRx: 0 };

  constructor(o: ConnectionOptions) {
    this.o = o;
    this.timing = { ...DEFAULT_TIMING, ...o.timing };
  }

  private state(s: ConnState): void {
    this.o.post({ t: "state", s });
  }

  connect(): void {
    this.clearTimers();
    const gen = ++this.gen;
    this.phase = "await-m2";
    this.cipher = null;
    this.zdec = null;
    this.hello = null;
    this.sawInit = false;
    this.confirmed = false;
    this.inflight = 0;
    this.pendingCredit = 0;
    this.recvChain = Promise.resolve();
    this.sendChain = Promise.resolve();
    this.state(this.attempt === 0 ? { s: "connecting" } : { s: "reconnecting", attempt: this.attempt });
    let ws: SocketLike;
    try {
      ws = this.o.createSocket ? this.o.createSocket(this.o.wsUrl) : (new WebSocket(this.o.wsUrl) as unknown as SocketLike);
    } catch (e) {
      this.dropped();
      return;
    }
    ws.binaryType = "arraybuffer";
    this.ws = ws;
    ws.onopen = () => {
      if (gen !== this.gen) return;
      this.state({ s: "handshaking" });
      this.timers.hs = setTimeout(() => {
        if (gen === this.gen && this.phase === "await-m2" ) ws.close(4000, "handshake timeout");
      }, this.timing.handshakeMs);
      viewerHello(this.o.sid, this.o.key).then(
        ({ m1, state }) => {
          if (gen !== this.gen) return;
          this.hello = state;
          ws.send(m1);
        },
        (e) => this.fail(gen, "protocol", (e as Error).message),
      );
    };
    ws.onmessage = (ev) => {
      if (gen !== this.gen) return;
      if (!(ev.data instanceof ArrayBuffer)) return this.fail(gen, "protocol", "unexpected non-binary message");
      const data = new Uint8Array(ev.data);
      this.timers.lastRx = Date.now();
      this.recvChain = this.recvChain.then(() => (gen === this.gen ? this.onFrame(gen, data) : undefined)).catch((e) => this.onFrameError(gen, e));
    };
    ws.onerror = () => {};
    ws.onclose = (ev) => {
      if (gen !== this.gen) return;
      // let queued frames (e.g. a final BYE) finish before reacting to the close
      this.recvChain = this.recvChain.then(() => (gen === this.gen ? this.onClose(ev.code) : undefined));
    };
  }

  private onFrameError(gen: number, e: unknown): void {
    if (e instanceof HandshakeError) {
      const kind = e.kind === "key-mismatch" ? "key-mismatch" : e.kind === "version" ? "unsupported" : "protocol";
      this.fail(gen, kind, e.message);
    } else {
      this.fail(gen, "protocol", (e as Error)?.message ?? "protocol error");
    }
  }

  /** terminal failure: report, close, never auto-reconnect */
  private fail(gen: number, kind: "key-mismatch" | "no-session" | "unsupported" | "protocol" | "network", message: string): void {
    if (gen !== this.gen || this.terminal) return;
    this.terminal = true;
    this.clearTimers();
    this.state({ s: "error", kind, message });
    this.killSocket(4402);
  }

  private killSocket(code = 1000): void {
    const ws = this.ws;
    this.ws = null;
    this.phase = "idle";
    if (ws) {
      ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
      try {
        ws.close(code);
      } catch {
        /* ignore */
      }
    }
  }

  private async onFrame(gen: number, data: Uint8Array): Promise<void> {
    if (this.phase === "await-m2") {
      if (!this.hello) throw new ProtocolError("hello not sent");
      clearTimeout(this.timers.hs);
      const keys = await finish(this.hello, data);
      if (gen !== this.gen) return;
      this.hello = null;
      this.cipher = new ViewerCipher(keys);
      this.zdec = new StreamDecoder();
      this.code = keys.code;
      this.phase = "active";
      this.sendBody(msg.clientHello((this.o.ua ?? "").slice(0, 200)));
      this.timers.ping = setInterval(() => {
        if (Date.now() - this.timers.lastRx > this.timing.idleMs) {
          this.ws?.close(4000, "idle");
          return;
        }
        this.sendBody(msg.ping());
      }, this.timing.pingMs);
      return;
    }
    if (this.phase !== "active" || !this.cipher || !this.zdec) return;
    const pt = await this.cipher.open(data);
    if (gen !== this.gen) return;
    if (pt.length < 1) throw new ProtocolError("empty frame");
    let body: Uint8Array;
    if (pt[0] === 0) body = pt.subarray(1);
    else if (pt[0] === 1) body = this.zdec.push(pt.subarray(1));
    else throw new ProtocolError("unknown codec");
    if (body.length === 0) throw new ProtocolError("empty message");
    this.dispatch(body, data.length, pt[0]);
  }

  private dispatch(body: Uint8Array, wire: number, codec: number): void {
    const type = body[0];
    if (!this.sawInit) {
      if (type !== MSG.STREAM_INIT || codec !== 1) throw new ProtocolError("expected STREAM_INIT");
      this.sawInit = true;
      return;
    }
    if (type === MSG.STREAM_INIT) throw new ProtocolError("duplicate STREAM_INIT");
    if (!this.confirmed && type !== MSG.AGENT_INFO) throw new ProtocolError("expected AGENT_INFO");
    switch (type) {
      case MSG.NODES: {
        const b = decodeNodes(body, wire);
        this.inflight += wire;
        this.o.post({ t: "nodes", v: b });
        return;
      }
      case MSG.DIR_FILES:
        this.o.post({ t: "dirFiles", v: decodeDirFiles(body) });
        return;
      default:
    }
    const m = decodeA2VJson(body);
    switch (m.type) {
      case MSG.AGENT_INFO:
        if (!this.confirmed) {
          this.confirmed = true;
          this.attempt = 0;
          this.state({ s: "connected", code: this.code });
        }
        this.o.post({ t: "agentInfo", v: m.v });
        break;
      case MSG.BROWSE_RESULT:
        this.o.post({ t: "browseResult", v: m.v });
        break;
      case MSG.SCAN_STARTED:
        this.o.post({ t: "scanStarted", v: m.v });
        break;
      case MSG.PROGRESS:
        this.o.post({ t: "progress", v: m.v });
        break;
      case MSG.SCAN_DONE:
        this.o.post({ t: "scanDone", v: m.v });
        break;
      case MSG.SCAN_ENDED:
        this.o.post({ t: "scanEnded", v: m.v });
        break;
      case MSG.REPLAY_DONE:
        this.o.post({ t: "replayDone" });
        break;
      case MSG.ERROR:
        this.o.post({ t: "error", v: m.v });
        break;
      case MSG.BYE:
        this.o.post({ t: "bye", v: m.v });
        if (m.v.reason === "superseded") {
          this.superseded = true;
          this.state({ s: "superseded" });
        } else {
          this.terminal = true;
          this.state({ s: "closed", reason: m.v.reason });
        }
        this.clearTimers();
        this.killSocket(1000);
        break;
      default: // ROOTS, PONG: ignored
    }
  }

  private onClose(code: number): void {
    this.ws = null;
    this.phase = "idle";
    this.clearTimers();
    if (this.terminal || this.superseded) return;
    const fatal = (kind: "key-mismatch" | "no-session" | "unsupported" | "protocol", message: string) => {
      this.terminal = true;
      this.state({ s: "error", kind, message });
    };
    switch (code) {
      case 4401:
        return fatal("key-mismatch", "Key mismatch — stale link or tampering");
      case 4400:
        return fatal("unsupported", "Agent speaks an unsupported protocol version");
      case 4404:
        return fatal("no-session", "Session not found — the agent may have exited");
      case 4402:
        return fatal("protocol", "Protocol error");
      case 4408:
        this.terminal = true;
        return this.state({ s: "closed", reason: "expired" });
      case 1000:
        this.terminal = true;
        return this.state({ s: "closed", reason: "agent closed the connection" });
      default:
        if (code >= 4400 && code < 4500) return fatal("protocol", `Relay refused the connection (${code})`);
        this.dropped();
    }
  }

  /** network-level loss: schedule reconnect with exponential backoff */
  private dropped(): void {
    if (this.terminal || this.superseded) return;
    this.attempt++;
    const d = Math.min(this.timing.backoffMaxMs, this.timing.backoffBaseMs * 2 ** (this.attempt - 1));
    this.state({ s: "reconnecting", attempt: this.attempt });
    this.timers.retry = setTimeout(() => {
      if (!this.terminal && !this.superseded) this.connect();
    }, d * (0.75 + Math.random() * 0.5));
  }

  private clearTimers(): void {
    clearTimeout(this.timers.hs);
    clearInterval(this.timers.ping);
    clearTimeout(this.timers.retry);
  }

  /** encrypt + send in order */
  private sendBody(body: Uint8Array): void {
    const gen = this.gen;
    const cipher = this.cipher;
    if (!cipher || this.phase !== "active") return;
    const plain = framePlain(body);
    this.sendChain = this.sendChain
      .then(async () => {
        const ct = await cipher.seal(plain);
        if (gen === this.gen) this.ws?.send(ct);
      })
      .catch((e) => this.fail(gen, "protocol", (e as Error).message));
  }

  handle(c: WorkerCommand): void {
    switch (c.t) {
      case "init":
        break;
      case "scanStart":
        this.sendBody(msg.scanStart(c.path, c.opts));
        break;
      case "scanCancel":
        this.sendBody(msg.scanCancel(c.scanId));
        break;
      case "browse":
        this.sendBody(msg.browse(c.reqId, c.path));
        break;
      case "dirFiles":
        this.sendBody(msg.dirFilesReq(c.reqId, c.scanId, c.dirId, c.limit));
        break;
      case "applied": {
        // only credit bytes this connection delivered (clamp against stale reports after a reconnect)
        const n = Math.min(Math.max(0, Math.floor(c.n)), this.inflight);
        this.inflight -= n;
        this.pendingCredit += n;
        if (this.pendingCredit >= CREDIT_FLUSH) {
          this.sendBody(msg.credit(this.pendingCredit));
          this.pendingCredit = 0;
        }
        break;
      }
      case "takeOver":
        if (this.superseded) {
          this.superseded = false;
          this.terminal = false;
          this.attempt = 0;
          this.connect();
        }
        break;
      case "close":
        this.terminal = true;
        this.clearTimers();
        this.gen++;
        this.killSocket(1000);
        this.state({ s: "closed", reason: "closed" });
        break;
    }
  }
}
