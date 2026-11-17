// SPDX-License-Identifier: MIT
import { describe, expect, it } from "vitest";
import { encodeUvarint, ProtocolError, Reader } from "../src/protocol/varint";

describe("varint", () => {
  it("round-trips values up to 2^53-1", () => {
    for (const v of [0, 1, 127, 128, 16383, 16384, 2 ** 31 - 1, 2 ** 31, 2 ** 32 - 1, 2 ** 32, 2 ** 40 + 5, Number.MAX_SAFE_INTEGER]) {
      const r = new Reader(encodeUvarint(v));
      expect(r.uv()).toBe(v);
      expect(r.remaining).toBe(0);
    }
  });
  it("matches Go encoding", () => {
    expect(Array.from(encodeUvarint(300))).toEqual([0xac, 0x02]);
  });
  it("rejects truncation, overflow and too-long", () => {
    expect(() => new Reader(Uint8Array.of(0x80)).uv()).toThrow(ProtocolError);
    expect(() => new Reader(new Uint8Array(10).fill(0xff)).uv()).toThrow(ProtocolError);
    expect(() => new Reader(new Uint8Array(11).fill(0x80)).uv()).toThrow(ProtocolError);
  });
  it("count() rejects implausible values", () => {
    expect(() => new Reader(Uint8Array.of(100, 0, 0)).count(2)).toThrow(ProtocolError);
  });
});
