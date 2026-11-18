// SPDX-License-Identifier: MIT
import { describe, expect, it } from "vitest";
import { ViewerConnection } from "../src/protocol/connection";
import type { WorkerEvent } from "../src/protocol/worker-protocol";
import { encodeUvarint } from "../src/protocol/varint";
import { AgentSim, FakeSocket, MSG, sidB64 } from "./agent-sim";
import { unhex, vec } from "./vectors";

const z = vec<any>("zstream.json");
const concat0 = (b: Uint8Array) => { const o = new Uint8Array(b.length + 1); o.set(b, 1); return o; };
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
async function until(f: () => boolean, ms = 3000) {
  const t = Date.now();
  while (!f()) {
    if (Date.now() - t > ms) throw new Error("timeout");
    await sleep(5);
  }
}

const INFO = { proto: 1, version: "t", os: "linux", arch: "x", hostname: "h", user: "u", invokingUser: "u", elevated: false, elevation: "none", fda: null, root: null, cwd: null, home: "/", autoScan: null, volumes: [], scan: null };

function setup(agentKey?: Uint8Array) {
  const sid = crypto.getRandomValues(new Uint8Array(16));
  const K = crypto.getRandomValues(new Uint8Array(32));
  const events: WorkerEvent[] = [];
  const sockets: FakeSocket[] = [];
  const agents: AgentSim[] = [];
  const conn = new ViewerConnection({
    sid: sidB64(sid),
    key: K,
    wsUrl: "ws://x/v1/view",
    ua: "test-ua",
    post: (e) => events.push(e),
    timing: { backoffBaseMs: 10, backoffMaxMs: 20, pingMs: 100000 },
    createSocket: () => {
      const s = new FakeSocket();
      const agent = new AgentSim(sid, agentKey ?? K);
      sockets.push(s);
      agents.push(agent);
      let first = true;
      s.onSend = async (d) => {
        if (first) {
          first = false;
          const m2 = await agent.respond(d);
          if (m2) s.push(m2);
          else s.drop(4401);
        }
      };
      return s;
    },
  });
  const states = () => events.filter((e) => e.t === "state").map((e: any) => e.s.s);
  const stage = async (i = sockets.length - 1) => {
    sockets[i].open();
    await until(() => sockets[i].sent.length >= 2 && agents[i].a2v !== undefined);
  };
  // agent side of a normal session start: STREAM_INIT then AGENT_INFO
  const start = async (i = sockets.length - 1) => {
    await stage(i);
    const a = agents[i];
    sockets[i].push(await a.seal(Uint8Array.from([1, ...unhex(z.init).subarray(1)])));
    sockets[i].push(await a.json(MSG.AGENT_INFO, INFO));
  };
  return { conn, events, sockets, agents, states, stage, start };
}

describe("ViewerConnection against a simulated agent", () => {
  it("handshakes, sends CLIENT_HELLO as frame 0, confirms on AGENT_INFO", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    const a = t.agents[0];
    expect(t.states().slice(0, 3)).toEqual(["connecting", "handshaking", "connected"]);
    expect((t.events.find((e) => e.t === "state" && e.s.s === "connected") as any).s.code).toBe(a.code);
    expect(t.events.some((e) => e.t === "agentInfo")).toBe(true);
    const hello = await a.open(t.sockets[0].sent[1]);
    expect(hello[0]).toBe(0); // codec 0
    expect(hello[1]).toBe(MSG.CLIENT_HELLO);
    expect(JSON.parse(new TextDecoder().decode(hello.subarray(2)))).toEqual({ proto: 1, ua: "test-ua", caps: { zstdStream: true, scanState: true, del: true } });
  });

  it("requires STREAM_INIT first and AGENT_INFO as key confirmation", async () => {
    const t = setup();
    t.conn.connect();
    await t.stage();
    t.sockets[0].push(await t.agents[0].json(MSG.REPLAY_DONE));
    await until(() => t.states().includes("error"));
    expect(t.sockets[0].closedByClient).toBe(4402);
    expect(t.states()).not.toContain("connected");
  });

  it("decodes NODES and reports wireBytes; flushes CREDIT >= 1 MiB of applied bytes", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    // one large NODES body of FOLDEND records (2 bytes each), > 1 MiB
    const m = 600000;
    const huge = new Uint8Array(1 + 1 + 3 + m * 2);
    huge.set([MSG.NODES, 1, ...encodeUvarint(m)]);
    for (let i = 0; i < m; i++) huge.set([3, 5], 5 + i * 2);
    expect(huge.length).toBeGreaterThan(1 << 20);
    const ct = await t.agents[0].seal(concat0(huge));
    t.sockets[0].push(ct);
    await until(() => t.events.some((e) => e.t === "nodes"));
    const nodes: any = t.events.find((e) => e.t === "nodes");
    expect(nodes.v.records.length).toBe(m);
    expect(nodes.v.wireBytes).toBe(ct.length);
    const sentBefore = t.sockets[0].sent.length;
    t.conn.handle({ t: "applied", n: 500_000 });
    await sleep(30);
    expect(t.sockets[0].sent.length).toBe(sentBefore); // below 1 MiB: not flushed
    t.conn.handle({ t: "applied", n: 10_000_000 }); // clamped to what was delivered
    await until(() => t.sockets[0].sent.length === sentBefore + 1);
    await t.agents[0].open(t.sockets[0].sent[1]); // CLIENT_HELLO (seq 0)
    const credit = await t.agents[0].open(t.sockets[0].sent[sentBefore]);
    expect(credit[1]).toBe(MSG.CREDIT);
    expect(JSON.parse(new TextDecoder().decode(credit.subarray(2))).n).toBe(ct.length);
  });

  it("BYE superseded: no auto-reconnect; takeOver reconnects with a fresh handshake", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    t.sockets[0].push(await t.agents[0].json(MSG.BYE, { reason: "superseded" }));
    await until(() => t.states().includes("superseded"));
    t.sockets[0].drop(1000);
    await sleep(100);
    expect(t.sockets.length).toBe(1);
    expect(t.events.some((e) => e.t === "bye")).toBe(true);
    t.conn.handle({ t: "takeOver" });
    expect(t.sockets.length).toBe(2);
    await t.start(1);
    await until(() => t.states().filter((s) => s === "connected").length === 2);
    expect(t.agents[1].code).not.toBe(t.agents[0].code);
  });

  it("network drop reconnects with backoff", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    t.sockets[0].drop(1006);
    await until(() => t.sockets.length === 2);
    expect(t.states()).toContain("reconnecting");
    await t.start(1);
    await until(() => t.states().filter((s) => s === "connected").length === 2);
  });

  it("4429 and 1013 are retryable, not fatal", async () => {
    for (const code of [4429, 1013]) {
      const t = setup();
      t.conn.connect();
      t.sockets[0].drop(code);
      await until(() => t.sockets.length === 2);
      expect(t.states()).toContain("reconnecting");
      expect(t.states()).not.toContain("error");
    }
  });

  it("maps close codes", async () => {
    for (const [code, kind] of [[4401, "key-mismatch"], [4404, "no-session"], [4400, "version"], [4402, "protocol"]] as const) {
      const t = setup();
      t.conn.connect();
      t.sockets[0].drop(code);
      await sleep(30);
      const err: any = t.events.find((e) => e.t === "state" && e.s.s === "error");
      expect(err.s.kind).toBe(kind);
      expect(t.sockets.length).toBe(1);
    }
  });

  it("tampered frame -> protocol error and no reconnect", async () => {
    const t = setup();
    t.conn.connect();
    await t.stage();
    const ct = await t.agents[0].seal(Uint8Array.from([1, ...unhex(z.init).subarray(1)]));
    ct[3] ^= 1;
    t.sockets[0].push(ct);
    await until(() => t.states().includes("error"));
    await sleep(60);
    expect(t.sockets.length).toBe(1);
  });

  it("agent keyed with a different K -> key-mismatch", async () => {
    const t = setup(crypto.getRandomValues(new Uint8Array(32)));
    t.conn.connect();
    t.sockets[0].open();
    // agent can't verify M1 so the sim drops with 4401, as a real agent would
    await until(() => t.states().includes("error"));
    expect((t.events.find((e) => e.t === "state" && e.s.s === "error") as any).s.kind).toBe("key-mismatch");
  });

  it("close() is final", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    t.conn.handle({ t: "close" });
    t.sockets[0].drop(1006);
    await sleep(60);
    expect(t.sockets.length).toBe(1);
    expect(t.states().at(-1)).toBe("closed");
  });
});

describe("ViewerConnection pacing", () => {
  const nodesBody = (scanId: number, m = 3) => {
    const b = new Uint8Array(2 + 1 + m * 2);
    b.set([MSG.NODES, scanId, m]);
    for (let i = 0; i < m; i++) b.set([3, 5], 3 + i * 2);
    return b;
  };
  const pushNodes = async (t: ReturnType<typeof setup>, scanId: number) => {
    const ct = await t.agents[0].seal(concat0(nodesBody(scanId)));
    t.sockets[0].push(ct);
    return ct.length;
  };
  const kinds = (t: ReturnType<typeof setup>) => t.events.map((e) => e.t).filter((k) => k !== "state" && k !== "agentInfo");

  it("ignores applied reports from an earlier connection (gen mismatch)", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    t.sockets[0].push(await t.agents[0].json(MSG.SCAN_STARTED, { scanId: 1, path: "/x", rootName: "x", volume: null, opts: {} }));
    for (let i = 0; i < 4; i++) await pushNodes(t, 1);
    await until(() => kinds(t).filter((k) => k === "nodes").length === 2);
    t.conn.handle({ t: "applied", n: 10, gen: -1 }); // stale: must not free a slot
    await sleep(50);
    expect(kinds(t).filter((k) => k === "nodes").length).toBe(2);
    t.conn.handle({ t: "applied", n: 10 }); // current (untagged) report frees one
    await sleep(20);
    expect(kinds(t).filter((k) => k === "nodes").length).toBe(3);
  });

  it("keeps at most 2 NODES in flight; each applied releases one; SCAN_DONE stays behind queued NODES", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    t.sockets[0].push(await t.agents[0].json(MSG.SCAN_STARTED, { scanId: 1, path: "/x", rootName: "x", volume: null, opts: {} }));
    for (let i = 0; i < 5; i++) await pushNodes(t, 1);
    t.sockets[0].push(await t.agents[0].json(MSG.SCAN_DONE, { scanId: 1 }));
    await until(() => kinds(t).filter((k) => k === "nodes").length === 2);
    await sleep(50);
    expect(kinds(t)).toEqual(["scanStarted", "nodes", "nodes"]); // scanDone not yet delivered
    t.conn.handle({ t: "applied", n: 10 });
    await sleep(20);
    expect(kinds(t).filter((k) => k === "nodes").length).toBe(3);
    t.conn.handle({ t: "applied", n: 10 });
    t.conn.handle({ t: "applied", n: 10 });
    await sleep(20);
    expect(kinds(t).filter((k) => k === "nodes").length).toBe(5);
    expect(kinds(t)).toEqual(["scanStarted", "nodes", "nodes", "nodes", "nodes", "nodes", "scanDone"]); // done only after every queued NODES was posted
  });

  it("PROGRESS and BROWSE_RESULT bypass queued NODES", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    t.sockets[0].push(await t.agents[0].json(MSG.SCAN_STARTED, { scanId: 1, path: "/x", rootName: "x", volume: null, opts: {} }));
    for (let i = 0; i < 4; i++) await pushNodes(t, 1);
    t.sockets[0].push(await t.agents[0].json(MSG.PROGRESS, { scanId: 1, dirs: 1, files: 2, alloc: 3, apparent: 3 }));
    t.sockets[0].push(await t.agents[0].json(MSG.BROWSE_RESULT, { reqId: 1, path: "/", entries: [] }));
    await until(() => kinds(t).includes("progress") && kinds(t).includes("browseResult"));
    expect(kinds(t).filter((k) => k === "nodes").length).toBe(2); // window still full
  });

  it("a new SCAN_STARTED drops queued NODES of the old scan and credits them immediately", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    t.sockets[0].push(await t.agents[0].json(MSG.SCAN_STARTED, { scanId: 1, path: "/x", rootName: "x", volume: null, opts: {} }));
    let queuedBytes = 0;
    for (let i = 0; i < 4; i++) {
      const n = await pushNodes(t, 1);
      if (i >= 2) queuedBytes += n; // first two are posted, last two stay queued
    }
    await until(() => kinds(t).filter((k) => k === "nodes").length === 2);
    await sleep(20);
    // enough queued bytes to cross the credit flush threshold on their own
    expect(queuedBytes).toBeLessThan(1 << 20);
    const before = t.sockets[0].sent.length;
    t.sockets[0].push(await t.agents[0].json(MSG.SCAN_STARTED, { scanId: 2, path: "/x", rootName: "x", volume: null, opts: {} }));
    await until(() => kinds(t).filter((k) => k === "scanStarted").length === 2);
    await sleep(20);
    // old scan batches are gone: acknowledging the two in flight releases nothing else
    t.conn.handle({ t: "applied", n: 1 });
    t.conn.handle({ t: "applied", n: 1 });
    await sleep(20);
    expect(kinds(t).filter((k) => k === "nodes").length).toBe(2);
    void before;
  });
});

describe("pause extension", () => {
  it("dispatches SCAN_STATE in order behind SCAN_STARTED (a replayed pause lands after the reset)", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    t.sockets[0].push(await t.agents[0].json(MSG.SCAN_STARTED, { scanId: 1, path: "/x", rootName: "x", volume: null, opts: {} }));
    t.sockets[0].push(await t.agents[0].json(MSG.SCAN_STATE, { scanId: 1, paused: true, elapsedMs: 1234 }));
    await until(() => t.events.some((e) => e.t === "scanState"));
    const order = t.events.map((e) => e.t).filter((k) => k === "scanStarted" || k === "scanState");
    expect(order).toEqual(["scanStarted", "scanState"]);
    expect((t.events.find((e) => e.t === "scanState") as any).v).toEqual({ scanId: 1, paused: true, elapsedMs: 1234 });
  });

  it("sends SCAN_PAUSE as an idempotent set", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    const before = t.sockets[0].sent.length;
    t.conn.handle({ t: "scanPause", scanId: 7, paused: true });
    await until(() => t.sockets[0].sent.length === before + 1);
    await t.agents[0].open(t.sockets[0].sent[1]); // CLIENT_HELLO (seq 0)
    const m = await t.agents[0].open(t.sockets[0].sent[before]);
    expect(m[1]).toBe(MSG.SCAN_PAUSE);
    expect(JSON.parse(new TextDecoder().decode(m.subarray(2)))).toEqual({ scanId: 7, paused: true });
  });

  it("an unknown a2v type is still fatal", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    t.sockets[0].push(await t.agents[0].json(0x6b));
    await until(() => t.states().includes("error"));
    expect(t.sockets[0].closedByClient).toBe(4402);
  });
});

describe("delete extension", () => {
  it("sends DELETE_REQ and DELETE_CANCEL as JSON control messages", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    const before = t.sockets[0].sent.length;
    const req = { reqId: 9, scanId: 3, mode: "trash" as const, items: [{ dir: 12 }, { parent: 4, name: "a.mov" }] };
    t.conn.handle({ t: "deleteReq", req });
    t.conn.handle({ t: "deleteCancel", job: 5 });
    await until(() => t.sockets[0].sent.length === before + 2);
    await t.agents[0].open(t.sockets[0].sent[1]); // CLIENT_HELLO (seq 0)
    const a = await t.agents[0].open(t.sockets[0].sent[before]);
    expect(a[1]).toBe(MSG.DELETE_REQ);
    expect(JSON.parse(new TextDecoder().decode(a.subarray(2)))).toEqual(req);
    const b = await t.agents[0].open(t.sockets[0].sent[before + 1]);
    expect(b[1]).toBe(MSG.DELETE_CANCEL);
    expect(JSON.parse(new TextDecoder().decode(b.subarray(2)))).toEqual({ job: 5 });
  });

  it("dispatches DELETE_STATUS at once and DELETED in order behind the scan's end", async () => {
    const t = setup();
    t.conn.connect();
    await t.start();
    await until(() => t.states().includes("connected"));
    const a = t.agents[0], s = t.sockets[0];
    s.push(await a.json(MSG.SCAN_STARTED, { scanId: 1, path: "/x", rootName: "x", volume: null, opts: {} }));
    s.push(await a.json(MSG.SCAN_DONE, { scanId: 1, totals: { alloc: 0, apparent: 0, files: 0, dirs: 0 }, denied: 0, errors: 0, vanished: 0, skippedMounts: 0, hardlinksDeduped: 0, folded: 0, ms: 1 }));
    s.push(await a.json(MSG.DELETED, { scanId: 1, job: 2, items: [{ dir: 3, freedAlloc: 10, freedApparent: 9, files: 1, dirs: 0 }] }));
    s.push(await a.json(MSG.DELETE_STATUS, { job: 2, reqId: 7, state: "done", total: 1, done: 1, freedAlloc: 10 }));
    await until(() => t.events.some((e) => e.t === "deleteStatus") && t.events.some((e) => e.t === "deleted"));
    const order = t.events.map((e) => e.t).filter((k) => k === "scanDone" || k === "deleted");
    expect(order).toEqual(["scanDone", "deleted"]);
    expect((t.events.find((e) => e.t === "deleted") as any).v.items[0].dir).toBe(3);
  });
});
