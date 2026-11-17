// SPDX-License-Identifier: MIT
// NODES / DIR_FILES binary decoding (docs/protocol.md §4).
import { CHILD_FLAG, FILE_FLAG, MSG } from "./types";
import type { ChildRec, DirFiles, DirRec, FileRec, NodeBatch, NodeRecord } from "./types";
import { ProtocolError, Reader } from "./varint";

export const MAX_NAME = 4096;
const utf8 = new TextDecoder("utf-8", { fatal: true });
const utf8Lossy = new TextDecoder("utf-8", { fatal: false });

/** Decode a name; `bad` is true when the bytes were not valid UTF-8 (replacement chars inserted). */
export function decodeName(b: Uint8Array): { s: string; bad: boolean } {
  let ascii = true;
  for (let i = 0; i < b.length; i++) {
    if (b[i] >= 0x80) {
      ascii = false;
      break;
    }
  }
  if (ascii) {
    if (b.length < 64) {
      let s = "";
      for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
      return { s, bad: false };
    }
    return { s: utf8Lossy.decode(b), bad: false };
  }
  try {
    return { s: utf8.decode(b), bad: false };
  } catch {
    return { s: utf8Lossy.decode(b), bad: true };
  }
}

function name(r: Reader): { s: string; bad: boolean } {
  const n = r.uv();
  if (n > MAX_NAME) throw new ProtocolError("name too long");
  return decodeName(r.bytes(n));
}

function file(r: Reader): FileRec {
  const n = name(r);
  const alloc = r.uv();
  const apparent = r.uv();
  let flags = r.uv();
  if (n.bad && !(flags & FILE_FLAG.BAD_NAME)) flags += FILE_FLAG.BAD_NAME;
  return { name: n.s, alloc, apparent, flags };
}

/** Decode a NODES body (starting with the 0x11 type byte). */
export function decodeNodes(body: Uint8Array, wireBytes = 0): NodeBatch {
  const r = new Reader(body);
  if (r.u8() !== MSG.NODES) throw new ProtocolError("not a NODES message");
  const scanId = r.uv();
  const count = r.count(2);
  const records: NodeRecord[] = [];
  for (let i = 0; i < count; i++) {
    const kind = r.u8();
    if (kind === 1) {
      const id = r.uv();
      const flags = r.uv();
      const ownAlloc = r.uv();
      const ownApparent = r.uv();
      const ownFiles = r.uv();
      const entryErrors = r.uv();
      const nd = r.count(2);
      let firstChildId = 0;
      const dirs: ChildRec[] = [];
      if (nd > 0) {
        firstChildId = r.uv();
        for (let j = 0; j < nd; j++) {
          const n = name(r);
          let f = r.uv();
          // BAD_NAME (32) is not defined for children on the wire; the decoder synthesizes it.
          if (n.bad && !(f & FILE_FLAG.BAD_NAME)) f += FILE_FLAG.BAD_NAME;
          dirs.push({ name: n.s, flags: f });
        }
      }
      const nf = r.count(4);
      const files: FileRec[] = [];
      for (let j = 0; j < nf; j++) files.push(file(r));
      const d: DirRec = { k: 1, id, flags, ownAlloc, ownApparent, ownFiles, entryErrors, firstChildId, dirs, files };
      records.push(d);
    } else if (kind === 2) {
      records.push({ k: 2, targetId: r.uv(), alloc: r.uv(), apparent: r.uv(), files: r.uv(), dirs: r.uv() });
    } else if (kind === 3) {
      records.push({ k: 3, targetId: r.uv() });
    } else {
      throw new ProtocolError("unknown record kind");
    }
  }
  if (r.remaining !== 0) throw new ProtocolError("trailing bytes in NODES");
  return { scanId, records, wireBytes };
}

/** Decode a DIR_FILES body (starting with the 0x15 type byte). */
export function decodeDirFiles(body: Uint8Array): DirFiles {
  const r = new Reader(body);
  if (r.u8() !== MSG.DIR_FILES) throw new ProtocolError("not a DIR_FILES message");
  const reqId = r.uv();
  const scanId = r.uv();
  const dirId = r.uv();
  const flags = r.uv();
  const restCount = r.uv();
  const restAlloc = r.uv();
  const restApparent = r.uv();
  const n = r.count(4);
  const files: FileRec[] = [];
  for (let i = 0; i < n; i++) files.push(file(r));
  if (r.remaining !== 0) throw new ProtocolError("trailing bytes in DIR_FILES");
  return { reqId, scanId, dirId, flags, restCount, restAlloc, restApparent, files };
}

// Re-export so tests can reference the child flag set.
export { CHILD_FLAG };
