// SPDX-License-Identifier: MIT
import { describe, expect, it } from "vitest";
import { Decompress } from "fzstd";
import { decompressFrame, parseFrameHeader, StreamDecoder } from "../src/protocol/zstream";
import { ProtocolError } from "../src/protocol/varint";
import { hex, synthBody, unhex, vec } from "./vectors";

const z = vec<any>("zstream.json");

describe("zstream vectors (Go StreamCompressor)", () => {
  it("decodes 500 messages incl. 1 B and ~1 MiB bodies", () => {
    expect(z.stream.length).toBe(500);
    const lens: number[] = z.stream.map((f: any) => f.len);
    expect(Math.min(...lens)).toBe(1);
    expect(Math.max(...lens)).toBe(1 << 20);
    const dec = new StreamDecoder();
    const init = dec.push(unhex(z.init).subarray(1));
    expect(init.length).toBe(33);
    expect(init[0]).toBe(0);
    let compressed = 0;
    z.stream.forEach((f: any, i: number) => {
      const frame = unhex(f.frame);
      const want = synthBody(i, f.len);
      if (f.codec === 0) {
        expect(f.len).toBeLessThan(512);
        expect(hex(frame.subarray(1))).toBe(hex(want));
      } else {
        compressed++;
        const got = dec.push(frame.subarray(1));
        expect(got.length).toBe(want.length);
        expect(Buffer.compare(got, want)).toBe(0);
      }
    });
    expect(compressed).toBeGreaterThan(400);
  });

  it("independent-frame mode decodes", () => {
    z.independent.forEach((f: any, i: number) => {
      if (f.codec === 0) return;
      const got = decompressFrame(unhex(f.frame).subarray(1));
      expect(Buffer.compare(got, synthBody(i, f.len))).toBe(0);
    });
  });
});

describe("why STREAM_INIT exists", () => {
  it("fzstd emits nothing while a frame start is still buffered (< 18 bytes)", () => {
    // fzstd buffers the beginning of a frame until >= 18 bytes are available, so a tiny first
    // compressed message would produce no output (and a stream could stall). STREAM_INIT is
    // always >= 18 bytes (random, incompressible), priming the decoder before real messages.
    const init = unhex(z.init).subarray(1);
    expect(init.length).toBeGreaterThanOrEqual(18);
    let calls = 0;
    new Decompress(() => void calls++).push(init.subarray(0, 17), false);
    expect(calls).toBe(0); // raw fzstd: nothing emitted for a < 18 byte frame start
    expect(() => new StreamDecoder().push(init.subarray(0, 12))).toThrow(ProtocolError);
    expect(new StreamDecoder().push(init).length).toBe(33);
  });
});

describe("bounds", () => {
  it("rejects windows above 8 MiB", () => {
    const hdr = Uint8Array.of(0x28, 0xb5, 0x2f, 0xfd, 0x00, 14 << 3, 0x01, 0x00, 0x00);
    expect(() => parseFrameHeader(hdr, 8 << 20)).toThrow(ProtocolError);
    expect(() => new StreamDecoder().push(hdr)).toThrow(ProtocolError);
  });
  it("accepts the Go stream header (4 MiB window)", () => {
    const h = parseFrameHeader(unhex(z.init).subarray(1), 8 << 20);
    expect(h.window).toBeLessThanOrEqual(8 << 20);
  });
  it("rejects bad magic and dictionaries", () => {
    expect(() => new StreamDecoder().push(new Uint8Array(40))).toThrow(ProtocolError);
    expect(() => parseFrameHeader(Uint8Array.of(0x28, 0xb5, 0x2f, 0xfd, 0x01, 0x58, 0x05, 0, 0, 0), 8 << 20)).toThrow(ProtocolError);
  });
  it("a failed decoder stays failed", () => {
    const d = new StreamDecoder();
    expect(() => d.push(new Uint8Array(40))).toThrow();
    expect(() => d.push(unhex(z.init).subarray(1))).toThrow();
  });
  it("independent mode rejects trailing garbage and truncation", () => {
    const f = z.independent.find((f: any) => f.codec === 1);
    const frame = unhex(f.frame).subarray(1);
    expect(() => decompressFrame(Uint8Array.from([...frame, 0]))).toThrow(ProtocolError);
    expect(() => decompressFrame(frame.subarray(0, frame.length - 1))).toThrow(ProtocolError);
  });
});
