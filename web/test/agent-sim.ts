// SPDX-License-Identifier: MIT
// Minimal agent-side handshake/cipher in TS, used only to exercise the viewer connection in Node.
import { b64url, L } from "../src/protocol/crypto";
import type { SocketLike } from "../src/protocol/connection";
import { encodeMsg } from "../src/protocol/messages";
import { MSG } from "../src/protocol/types";

const s = crypto.subtle;
const enc = new TextEncoder();
const cat = (...p: Uint8Array[]) => Uint8Array.from(p.flatMap((x) => Array.from(x)));

async function hkdf(ikm: Uint8Array, salt: Uint8Array, info: Uint8Array, n: number) {
  const k = await s.importKey("raw", ikm as BufferSource, "HKDF", false, ["deriveBits"]);
  return new Uint8Array(await s.deriveBits({ name: "HKDF", hash: "SHA-256", salt: salt as BufferSource, info: info as BufferSource }, k, n * 8));
}
async function hmac(key: Uint8Array, data: Uint8Array) {
  const k = await s.importKey("raw", key as BufferSource, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await s.sign("HMAC", k, data as BufferSource));
}
const iv = (seq: number) => {
  const b = new Uint8Array(12);
  new DataView(b.buffer).setUint32(8, seq);
  return b;
};

export class AgentSim {
  sealSeq = 0;
  openSeq = 0;
  a2v!: CryptoKey;
  v2a!: CryptoKey;
  code = "";
  constructor(readonly sid: Uint8Array, readonly K: Uint8Array) {}

  /** returns M2 or null if M1 is not authentic */
  async respond(m1: Uint8Array): Promise<Uint8Array | null> {
    const ka = await hkdf(this.K, L("auth"), this.sid, 32);
    const tv = await hmac(ka, cat(L("vh"), this.sid, m1.subarray(0, 50)));
    if (Buffer.compare(tv, m1.subarray(50)) !== 0) return null;
    const kp = (await s.generateKey({ name: "X25519" }, true, ["deriveBits"])) as CryptoKeyPair;
    const m2 = new Uint8Array(82);
    m2[0] = 0xd2;
    m2[1] = 1;
    m2.set(new Uint8Array(await s.exportKey("raw", kp.publicKey)), 2);
    crypto.getRandomValues(m2.subarray(34, 50));
    m2.set(await hmac(ka, cat(L("ah"), this.sid, m1, m2.subarray(0, 50))), 50);
    const pv = await s.importKey("raw", m1.subarray(2, 34) as BufferSource, { name: "X25519" }, false, []);
    const dh = new Uint8Array(await s.deriveBits({ name: "X25519", public: pv }, kp.privateKey, 256));
    const th = new Uint8Array(await s.digest("SHA-256", cat(L("th"), this.sid, m1, m2) as BufferSource));
    const kk = await hkdf(dh, this.K, cat(L("keys"), th), 64);
    const cb = await hkdf(dh, this.K, cat(L("code"), th), 4);
    const n = new DataView(cb.buffer).getUint32(0) % 1_000_000;
    this.code = `${String(Math.floor(n / 1000)).padStart(3, "0")} ${String(n % 1000).padStart(3, "0")}`;
    const imp = (r: Uint8Array) => s.importKey("raw", r as BufferSource, "AES-GCM", false, ["encrypt", "decrypt"]);
    this.a2v = await imp(kk.subarray(0, 32));
    this.v2a = await imp(kk.subarray(32));
    return m2;
  }

  async seal(plain: Uint8Array): Promise<Uint8Array> {
    const ct = await s.encrypt({ name: "AES-GCM", iv: iv(this.sealSeq++) as BufferSource, additionalData: L("a2v") as BufferSource }, this.a2v, plain as BufferSource);
    return new Uint8Array(ct);
  }
  async open(ct: Uint8Array): Promise<Uint8Array> {
    return new Uint8Array(await s.decrypt({ name: "AES-GCM", iv: iv(this.openSeq++) as BufferSource, additionalData: L("v2a") as BufferSource }, this.v2a, ct as BufferSource));
  }
  json(type: number, v?: unknown): Promise<Uint8Array> {
    return this.seal(cat(Uint8Array.of(0), encodeMsg(type, v)));
  }
}

export class FakeSocket implements SocketLike {
  binaryType = "blob";
  onopen: ((ev: unknown) => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onclose: ((ev: { code: number; reason?: string }) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;
  sent: Uint8Array[] = [];
  closedByClient: number | null = null;
  onSend: ((d: Uint8Array) => void) | null = null;
  send(d: Uint8Array) {
    this.sent.push(d);
    this.onSend?.(d);
  }
  close(code = 1005) {
    this.closedByClient = code;
  }
  open() {
    this.onopen?.({});
  }
  push(d: Uint8Array) {
    this.onmessage?.({ data: d.buffer.slice(d.byteOffset, d.byteOffset + d.byteLength) });
  }
  drop(code: number) {
    this.onclose?.({ code });
  }
}

export const sidB64 = (sid: Uint8Array) => b64url(sid);
export { MSG, enc };
