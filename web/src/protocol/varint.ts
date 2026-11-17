// SPDX-License-Identifier: MIT
// Unsigned LEB128 using arithmetic (never bit operations above 31 bits), so
// values up to 2^53-1 round-trip exactly.

export class ProtocolError extends Error {}

export function writeUvarint(out: number[], v: number): void {
  if (!Number.isSafeInteger(v) || v < 0) throw new ProtocolError("varint out of range");
  while (v >= 0x80) {
    out.push((v % 0x80) | 0x80);
    v = Math.floor(v / 0x80);
  }
  out.push(v);
}

export function encodeUvarint(v: number): Uint8Array {
  const a: number[] = [];
  writeUvarint(a, v);
  return Uint8Array.from(a);
}

/** Bounds-checked cursor over a byte buffer. */
export class Reader {
  pos = 0;
  constructor(readonly b: Uint8Array, pos = 0) {
    this.pos = pos;
  }

  get remaining(): number {
    return this.b.length - this.pos;
  }

  u8(): number {
    if (this.pos >= this.b.length) throw new ProtocolError("truncated");
    return this.b[this.pos++];
  }

  /** unsigned LEB128, throws if it exceeds 2^53-1 or is truncated */
  uv(): number {
    let v = 0;
    let mul = 1;
    for (let i = 0; i < 10; i++) {
      const c = this.u8();
      v += (c & 0x7f) * mul;
      if (v > Number.MAX_SAFE_INTEGER) throw new ProtocolError("varint overflow");
      if (c < 0x80) return v;
      mul *= 128;
    }
    throw new ProtocolError("varint too long");
  }

  bytes(n: number): Uint8Array {
    if (n > this.remaining) throw new ProtocolError("truncated");
    const s = this.b.subarray(this.pos, this.pos + n);
    this.pos += n;
    return s;
  }

  /** a count of elements each at least `minElem` bytes: must be plausible vs remaining bytes */
  count(minElem: number): number {
    const n = this.uv();
    if (n > Math.floor(this.remaining / minElem)) throw new ProtocolError("implausible count");
    return n;
  }
}
