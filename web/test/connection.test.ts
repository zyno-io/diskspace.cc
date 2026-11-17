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
    expect(JSON.parse(new TextDecoder().decode(hello.subarray(2)))).toEqual({ proto: 1, ua: "test-ua", caps: { zstdStream: true } });
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

  it("maps close codes", async () => {
    for (const [code, kind] of [[4401, "key-mismatch"], [4404, "no-session"], [4400, "unsupported"], [4402, "protocol"]] as const) {
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
