// SPDX-License-Identifier: MIT
// Viewer side of the diskspace.cc handshake (docs/protocol.md §2).
// Adapted from github.com/zyno-io/sp2p (MIT) (WebCrypto helpers, X25519 fallback pattern).

import { ProtocolError } from "./varint";

const enc = new TextEncoder();
export const LABEL = "diskspace.cc v1 ";
export const L = (x: string): Uint8Array => enc.encode(LABEL + x);

const subtle = (): SubtleCrypto => globalThis.crypto.subtle;
const bs = (u: Uint8Array): BufferSource => u as BufferSource;

export class HandshakeError extends Error {
  constructor(readonly kind: "key-mismatch" | "version" | "protocol", message: string) {
    super(message);
  }
}

// ---- X25519 (native WebCrypto, lazy @noble/curves fallback) ------------------

// PKCS#8 prefix for a raw 32-byte X25519 private key.
export const X25519_PKCS8_PREFIX = Uint8Array.from([0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6e, 0x04, 0x22, 0x04, 0x20]);

let nativeOk: boolean | undefined;
let forceFallback = false;

/** test hook: force the @noble/curves path */
export function _forceNobleFallback(v: boolean): void {
  forceFallback = v;
}

async function hasNativeX25519(): Promise<boolean> {
  if (forceFallback) return false;
  if (nativeOk === undefined) {
    try {
      await subtle().generateKey({ name: "X25519" }, true, ["deriveBits"]);
      nativeOk = true;
    } catch {
      nativeOk = false;
    }
  }
  return nativeOk;
}

async function noble() {
  return (await import("@noble/curves/ed25519.js")).x25519;
}

interface EphemeralKey {
  publicKey: Uint8Array; // 32 B
  /** returns the 32-byte shared secret; throws on all-zero output */
  dh(peerPub: Uint8Array): Promise<Uint8Array>;
}

function nonZero(b: Uint8Array): Uint8Array {
  let acc = 0;
  for (const x of b) acc |= x;
  if (acc === 0) throw new HandshakeError("protocol", "degenerate DH output");
  return b;
}

function b64urlToBytes(s: string): Uint8Array {
  s = s.replace(/-/g, "+").replace(/_/g, "/");
  s += "=".repeat((4 - (s.length % 4)) % 4);
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

export function b64url(b: Uint8Array): string {
  let s = "";
  for (const x of b) s += String.fromCharCode(x);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

async function newEphemeral(fixedPriv?: Uint8Array): Promise<EphemeralKey> {
  if (await hasNativeX25519()) {
    let priv: CryptoKey;
    let pub: Uint8Array;
    if (fixedPriv) {
      const pkcs8 = new Uint8Array(48);
      pkcs8.set(X25519_PKCS8_PREFIX);
      pkcs8.set(fixedPriv, 16);
      priv = await subtle().importKey("pkcs8", bs(pkcs8), { name: "X25519" }, true, ["deriveBits"]);
      const jwk = await subtle().exportKey("jwk", priv);
      pub = b64urlToBytes(jwk.x!);
    } else {
      const kp = (await subtle().generateKey({ name: "X25519" }, true, ["deriveBits"])) as CryptoKeyPair;
      priv = kp.privateKey;
      pub = new Uint8Array(await subtle().exportKey("raw", kp.publicKey));
    }
    return {
      publicKey: pub,
      async dh(peer) {
        const pk = await subtle().importKey("raw", bs(peer), { name: "X25519" }, false, []);
        return nonZero(new Uint8Array(await subtle().deriveBits({ name: "X25519", public: pk }, priv, 256)));
      },
    };
  }
  const x = await noble();
  const sk = fixedPriv ?? x.utils.randomSecretKey();
  return {
    publicKey: x.getPublicKey(sk),
    async dh(peer) {
      return nonZero(x.getSharedSecret(sk, peer));
    },
  };
}

// ---- HKDF / HMAC -------------------------------------------------------------

async function hkdf(ikm: Uint8Array, salt: Uint8Array, info: Uint8Array, bytes: number): Promise<Uint8Array> {
  const k = await subtle().importKey("raw", bs(ikm), "HKDF", false, ["deriveBits"]);
  return new Uint8Array(await subtle().deriveBits({ name: "HKDF", hash: "SHA-256", salt: bs(salt), info: bs(info) }, k, bytes * 8));
}

function concat(...p: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(p.reduce((n, x) => n + x.length, 0));
  let o = 0;
  for (const x of p) {
    out.set(x, o);
    o += x.length;
  }
  return out;
}

async function hmacKey(raw: Uint8Array, usage: "sign" | "verify"): Promise<CryptoKey> {
  return subtle().importKey("raw", bs(raw), { name: "HMAC", hash: "SHA-256" }, false, [usage]);
}

// ---- handshake ---------------------------------------------------------------

export interface ViewerState {
  sid: Uint8Array;
  k: Uint8Array;
  kAuth: Uint8Array;
  m1: Uint8Array;
  eph: EphemeralKey;
}

export interface Keys {
  a2v: CryptoKey;
  v2a: CryptoKey;
  /** "123 456" */
  code: string;
  th: Uint8Array;
}

export interface HelloOptions {
  /** TEST ONLY: fixed X25519 private scalar (32 B) */
  privateKey?: Uint8Array;
  /** TEST ONLY: fixed 16-byte nonce */
  nonce?: Uint8Array;
}

/** sid: base64url string (22 chars) or raw 16 bytes; K: raw 32 bytes. */
export async function viewerHello(sid: string | Uint8Array, K: Uint8Array, opts: HelloOptions = {}): Promise<{ m1: Uint8Array; state: ViewerState }> {
  const sidRaw = typeof sid === "string" ? b64urlToBytes(sid) : sid;
  if (sidRaw.length !== 16 || K.length !== 32) throw new HandshakeError("protocol", "bad link secret");
  const kAuth = await hkdf(K, L("auth"), sidRaw, 32);
  const eph = await newEphemeral(opts.privateKey);
  const m1 = new Uint8Array(82);
  m1[0] = 0xd1;
  m1[1] = 1;
  m1.set(eph.publicKey, 2);
  m1.set(opts.nonce ?? crypto.getRandomValues(new Uint8Array(16)), 34);
  const tv = await subtle().sign("HMAC", await hmacKey(kAuth, "sign"), bs(concat(L("vh"), sidRaw, m1.subarray(0, 50))));
  m1.set(new Uint8Array(tv), 50);
  return { m1, state: { sid: sidRaw, k: K, kAuth, m1, eph } };
}

/** Verify AGENT_HELLO and derive keys. Throws HandshakeError. */
export async function finish(state: ViewerState, m2: Uint8Array): Promise<Keys> {
  if (m2.length !== 82 || m2[0] !== 0xd2) throw new HandshakeError("protocol", "malformed AGENT_HELLO");
  if (m2[1] !== 1) throw new HandshakeError("version", "unsupported protocol version");
  const ok = await subtle().verify("HMAC", await hmacKey(state.kAuth, "verify"), bs(m2.subarray(50)), bs(concat(L("ah"), state.sid, state.m1, m2.subarray(0, 50))));
  if (!ok) throw new HandshakeError("key-mismatch", "Key mismatch — stale link or tampering");
  const dh = await state.eph.dh(m2.subarray(2, 34));
  const th = new Uint8Array(await subtle().digest("SHA-256", bs(concat(L("th"), state.sid, state.m1, m2))));
  const kk = await hkdf(dh, state.k, concat(L("keys"), th), 64);
  const cb = await hkdf(dh, state.k, concat(L("code"), th), 4);
  const n = new DataView(cb.buffer, cb.byteOffset, 4).getUint32(0) % 1_000_000;
  const code = `${String(Math.floor(n / 1000)).padStart(3, "0")} ${String(n % 1000).padStart(3, "0")}`;
  const imp = (raw: Uint8Array) => subtle().importKey("raw", bs(raw), { name: "AES-GCM" }, false, ["encrypt", "decrypt"]);
  return { a2v: await imp(kk.subarray(0, 32)), v2a: await imp(kk.subarray(32)), code, th };
}

export { ProtocolError };
