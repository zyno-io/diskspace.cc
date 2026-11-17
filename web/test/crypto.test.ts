// SPDX-License-Identifier: MIT
import { afterEach, describe, expect, it } from "vitest";
import { _forceNobleFallback, finish, HandshakeError, viewerHello } from "../src/protocol/crypto";
import { ViewerCipher } from "../src/protocol/frame";
import { hex, unhex, vec } from "./vectors";

const hs = vec<any[]>("handshake.json");
const fr = vec<any>("frames.json");

afterEach(() => _forceNobleFallback(false));

for (const fallback of [false, true]) {
  describe(`handshake vectors (${fallback ? "noble fallback" : "native X25519"})`, () => {
    for (const v of hs) {
      it(`${v.name}: m1 and key schedule match Go`, async () => {
        _forceNobleFallback(fallback);
        const rnd = unhex(v.viewerRnd);
        const { m1, state } = await viewerHello(v.sidB64, unhex(v.k), { privateKey: rnd.subarray(0, 32), nonce: rnd.subarray(32, 48) });
        expect(hex(m1)).toBe(v.m1);
        const keys = await finish(state, unhex(v.m2));
        expect(hex(keys.th)).toBe(v.th);
        expect(keys.code).toBe(v.code);
      });
    }

    it("derived keys decrypt/encrypt the Go frame vectors", async () => {
      _forceNobleFallback(fallback);
      const v = hs.find((h) => h.name === fr.handshake)!;
      const rnd = unhex(v.viewerRnd);
      const { state } = await viewerHello(v.sidB64, unhex(v.k), { privateKey: rnd.subarray(0, 32), nonce: rnd.subarray(32, 48) });
      const cipher = new ViewerCipher(await finish(state, unhex(v.m2)));
      for (const f of fr.frames.filter((f: any) => f.dir === "a2v")) {
        expect(hex(await cipher.open(unhex(f.ciphertext)))).toBe(f.plaintext);
      }
      for (const f of fr.frames.filter((f: any) => f.dir === "v2a")) {
        expect(hex(await cipher.seal(unhex(f.plaintext)))).toBe(f.ciphertext);
      }
    });
  });
}

describe("handshake tamper", () => {
  const v = hs[0];
  const start = async () => {
    const rnd = unhex(v.viewerRnd);
    return viewerHello(v.sidB64, unhex(v.k), { privateKey: rnd.subarray(0, 32), nonce: rnd.subarray(32, 48) });
  };

  it("every bit flip in M2 is rejected", async () => {
    const m2 = unhex(v.m2);
    const { state } = await start(); // finish() does not mutate state
    for (let i = 0; i < m2.length * 8; i++) {
      const b = m2.slice();
      b[i >> 3] ^= 1 << (i & 7);
      await expect(finish(state, b)).rejects.toBeInstanceOf(HandshakeError);
    }
  });

  it("wrong K -> key-mismatch", async () => {
    const k = unhex(v.k);
    k[0] ^= 1;
    const rnd = unhex(v.viewerRnd);
    const { state } = await viewerHello(v.sidB64, k, { privateKey: rnd.subarray(0, 32), nonce: rnd.subarray(32, 48) });
    await expect(finish(state, unhex(v.m2))).rejects.toMatchObject({ kind: "key-mismatch" });
  });

  it("version 2 / bad length / bad type", async () => {
    const { state } = await start();
    const b = unhex(v.m2);
    b[1] = 2;
    await expect(finish(state, b)).rejects.toMatchObject({ kind: "version" });
    await expect(finish(state, b.subarray(0, 81))).rejects.toMatchObject({ kind: "protocol" });
    const c = unhex(v.m2);
    c[0] = 0xd1;
    await expect(finish(state, c)).rejects.toMatchObject({ kind: "protocol" });
  });

  it("fresh random handshake produces a well-formed M1", async () => {
    const { m1 } = await viewerHello(v.sidB64, unhex(v.k));
    expect(m1.length).toBe(82);
    expect([m1[0], m1[1]]).toEqual([0xd1, 1]);
  });
});

describe("frames", () => {
  async function cipher() {
    const v = hs.find((h) => h.name === fr.handshake)!;
    const rnd = unhex(v.viewerRnd);
    const { state } = await viewerHello(v.sidB64, unhex(v.k), { privateKey: rnd.subarray(0, 32), nonce: rnd.subarray(32, 48) });
    return new ViewerCipher(await finish(state, unhex(v.m2)));
  }
  const a2v = fr.frames.filter((f: any) => f.dir === "a2v");

  it("rejects reorder, duplicate, drop, reflect", async () => {
    let c = await cipher();
    await expect(c.open(unhex(a2v[1].ciphertext))).rejects.toThrow(); // reorder
    c = await cipher();
    await c.open(unhex(a2v[0].ciphertext));
    await expect(c.open(unhex(a2v[0].ciphertext))).rejects.toThrow(); // dup
    c = await cipher();
    await c.open(unhex(a2v[0].ciphertext));
    await expect(c.open(unhex(a2v[2].ciphertext))).rejects.toThrow(); // drop
    c = await cipher();
    const own = await c.seal(new Uint8Array([0, 1]));
    await expect(c.open(own)).rejects.toThrow(); // reflect
  });

  it("concurrent seals keep sequence order", async () => {
    const c = await cipher();
    const v2a = fr.frames.filter((f: any) => f.dir === "v2a");
    const out = await Promise.all(v2a.map((f: any) => c.seal(unhex(f.plaintext))));
    out.forEach((ct, i) => expect(hex(ct)).toBe(v2a[i].ciphertext));
  });
});
