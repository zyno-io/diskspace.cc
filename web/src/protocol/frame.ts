// SPDX-License-Identifier: MIT
import type { Keys } from "./crypto";
import { L } from "./crypto";
import { ProtocolError } from "./varint";

const MAX_SEQ = 2 ** 32;

/**
 * Viewer-side frame cipher: seals v2a, opens a2v. Sequence numbers are
 * implicit and advance synchronously at call time, so concurrent calls keep
 * their order. A failed open is fatal for the connection.
 */
export class ViewerCipher {
  private sealSeq = 0;
  private openSeq = 0;
  private readonly sealAad = L("v2a");
  private readonly openAad = L("a2v");

  constructor(private readonly keys: Pick<Keys, "a2v" | "v2a">) {}

  private static iv(seq: number): Uint8Array {
    const iv = new Uint8Array(12);
    const dv = new DataView(iv.buffer);
    dv.setUint32(4, Math.floor(seq / 2 ** 32));
    dv.setUint32(8, seq >>> 0);
    return iv;
  }

  async seal(plaintext: Uint8Array): Promise<Uint8Array> {
    const seq = this.sealSeq;
    if (seq >= MAX_SEQ) throw new ProtocolError("sequence exhausted");
    this.sealSeq++;
    const ct = await crypto.subtle.encrypt({ name: "AES-GCM", iv: ViewerCipher.iv(seq) as BufferSource, additionalData: this.sealAad as BufferSource }, this.keys.v2a, plaintext as BufferSource);
    return new Uint8Array(ct);
  }

  async open(ct: Uint8Array): Promise<Uint8Array> {
    const seq = this.openSeq;
    if (seq >= MAX_SEQ) throw new ProtocolError("sequence exhausted");
    this.openSeq++;
    try {
      const pt = await crypto.subtle.decrypt({ name: "AES-GCM", iv: ViewerCipher.iv(seq) as BufferSource, additionalData: this.openAad as BufferSource }, this.keys.a2v, ct as BufferSource);
      return new Uint8Array(pt);
    } catch {
      throw new ProtocolError("frame authentication failed");
    }
  }
}
