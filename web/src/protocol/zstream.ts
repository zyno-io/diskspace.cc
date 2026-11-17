// SPDX-License-Identifier: MIT
// Adapted from github.com/zyno-io/sp2p (MIT) web/src/bounded-zstd.ts (header validation).
//
// a2v compression (codec 1). Two modes, selected by CLIENT_HELLO caps:
//  * stream:      all codec-1 frames of a connection are consecutive chunks of ONE
//                 zstd frame (shared window); each push yields exactly one message.
//                 The first codec-1 message is STREAM_INIT, which exists because
//                 fzstd buffers the start of a frame until >= 18 bytes are available
//                 and would otherwise emit nothing for a tiny first message.
//  * independent: each codec-1 frame is a complete zstd frame.
import { Decompress } from "fzstd";
import { ProtocolError } from "./varint";

export const MAX_BODY = 1 << 20;
export const MAX_WINDOW = 8 << 20;

const bad = (m = "invalid or oversized zstd data"): never => {
  throw new ProtocolError(m);
};

interface Header {
  headerEnd: number;
  single: boolean;
  window: number;
  contentSize: number | null;
}

/** Parse + validate a zstd frame header (no skippable frames, no dictionaries). */
export function parseFrameHeader(input: Uint8Array, limit: number): Header {
  if (input.length < 6 || input[0] !== 0x28 || input[1] !== 0xb5 || input[2] !== 0x2f || input[3] !== 0xfd) bad();
  const flags = input[4];
  if (flags & 0x18) bad(); // reserved/unused bits
  const single = !!(flags & 0x20);
  let pos = 5;
  let window = 0;
  if (!single) {
    const d = input[pos++];
    const base = 2 ** (10 + (d >>> 3));
    window = base + (base / 8) * (d & 7);
    if (window > limit) bad();
  }
  const dictBytes = [0, 1, 2, 4][flags & 3];
  if (pos + dictBytes > input.length) bad();
  for (let i = 0; i < dictBytes; i++) if (input[pos++] !== 0) bad();
  const sizeFlag = flags >>> 6;
  const sizeBytes = sizeFlag ? 2 ** sizeFlag : single ? 1 : 0;
  if (pos + sizeBytes > input.length) bad();
  let size = 0;
  for (let i = 0; i < sizeBytes; i++) size += input[pos++] * 2 ** (8 * i);
  if (sizeFlag === 1) size += 256;
  if (!Number.isSafeInteger(size) || size > limit) bad();
  if (single) window = size;
  if (window > limit) bad();
  return { headerEnd: pos, single, window, contentSize: sizeBytes ? size : null };
}

/** Streaming decoder: one instance per connection. */
export class StreamDecoder {
  private dec: Decompress | null = null;
  private chunks: Uint8Array[] = [];
  private total = 0;
  private dead = false;

  /** Decompress one codec-1 payload; returns the message body (1 B .. 1 MiB). */
  push(payload: Uint8Array): Uint8Array {
    if (this.dead) bad("decoder failed");
    try {
      if (!this.dec) {
        parseFrameHeader(payload, MAX_WINDOW); // validate before fzstd allocates a window
        this.dec = new Decompress((chunk, final) => {
          if (final) bad("stream ended");
          this.total += chunk.length;
          if (this.total > MAX_BODY) bad();
          this.chunks.push(chunk.slice());
        });
      }
      this.chunks = [];
      this.total = 0;
      this.dec.push(payload, false);
      if (this.total < 1 || this.total > MAX_BODY) bad("bad decompressed size");
      if (this.chunks.length === 1) return this.chunks[0];
      const out = new Uint8Array(this.total);
      let o = 0;
      for (const c of this.chunks) {
        out.set(c, o);
        o += c.length;
      }
      return out;
    } catch (e) {
      this.dead = true;
      throw e instanceof ProtocolError ? e : new ProtocolError("zstd: " + (e as Error)?.message);
    }
  }
}

/** Independent-frame mode: validate the whole envelope, then decompress. */
export function decompressFrame(input: Uint8Array, limit = MAX_BODY): Uint8Array {
  const h = parseFrameHeader(input, Math.max(limit, MAX_WINDOW));
  if (h.contentSize !== null && h.contentSize > limit) bad();
  let pos = h.headerEnd;
  let last = false;
  while (!last) {
    if (pos + 3 > input.length) bad();
    const header = input[pos] + input[pos + 1] * 256 + input[pos + 2] * 65536;
    pos += 3;
    last = !!(header & 1);
    const kind = (header >>> 1) & 3;
    const blockSize = header >>> 3;
    if (kind === 3 || blockSize > 128 * 1024) bad();
    pos += kind === 1 ? 1 : blockSize;
    if (pos > input.length) bad();
  }
  if (input[4] & 4) pos += 4;
  if (pos !== input.length) bad();
  const cap = h.contentSize ?? limit;
  const output = new Uint8Array(cap);
  let length = 0;
  const d = new Decompress((chunk) => {
    if (chunk.length > output.length - length) bad();
    output.set(chunk, length);
    length += chunk.length;
  });
  try {
    d.push(input, true);
  } catch (e) {
    throw e instanceof ProtocolError ? e : new ProtocolError("zstd: " + (e as Error)?.message);
  }
  if (h.contentSize !== null && length !== h.contentSize) bad();
  if (length < 1) bad();
  return length === output.length ? output : output.slice(0, length);
}
