// SPDX-License-Identifier: MIT
// JSON message encode/decode and plaintext framing.
import { MSG, PROTO_VERSION } from "./types";
import type { AgentInfo, BrowseResult, ByeMsg, ErrorMsg, Progress, ScanDone, ScanEnded, ScanOptions, ScanStarted, ScanState } from "./types";
import { ProtocolError } from "./varint";

const te = new TextEncoder();
const td = new TextDecoder("utf-8", { fatal: true });

export const MAX_V2A_BODY = 64 * 1024 - 64;

/** body = [type][JSON] */
export function encodeMsg(type: number, payload?: unknown): Uint8Array {
  const j = payload === undefined ? new Uint8Array(0) : te.encode(JSON.stringify(payload));
  const out = new Uint8Array(1 + j.length);
  out[0] = type;
  out.set(j, 1);
  return out;
}

/** v2a plaintext frame: codec 0 + body */
export function framePlain(body: Uint8Array): Uint8Array {
  const out = new Uint8Array(1 + body.length);
  out.set(body, 1);
  return out;
}

export const msg = {
  clientHello: (ua: string) => encodeMsg(MSG.CLIENT_HELLO, { proto: PROTO_VERSION, ua, caps: { zstdStream: true, scanState: true } }),
  getRoots: () => encodeMsg(MSG.GET_ROOTS),
  browse: (reqId: number, path: string) => encodeMsg(MSG.BROWSE, { reqId, path }),
  scanStart: (path: string, o: ScanOptions = {}) =>
    encodeMsg(MSG.SCAN_START, { path, crossMounts: o.crossMounts ?? false, ...(o.topFiles !== undefined ? { topFiles: o.topFiles } : {}), ...(o.minFileBytes !== undefined ? { minFileBytes: o.minFileBytes } : {}) }),
  scanCancel: (scanId: number) => encodeMsg(MSG.SCAN_CANCEL, { scanId }),
  dirFilesReq: (reqId: number, scanId: number, dirId: number, limit: number) =>
    encodeMsg(MSG.DIR_FILES_REQ, { reqId, scanId, dirId, limit: Math.max(1, Math.min(10000, Math.floor(limit))) }),
  scanPause: (scanId: number, paused: boolean) => encodeMsg(MSG.SCAN_PAUSE, { scanId, paused }),
  credit: (n: number) => encodeMsg(MSG.CREDIT, { n }),
  ping: () => encodeMsg(MSG.PING),
};

export type A2VJson =
  | { type: typeof MSG.AGENT_INFO; v: AgentInfo }
  | { type: typeof MSG.BROWSE_RESULT; v: BrowseResult }
  | { type: typeof MSG.SCAN_STARTED; v: ScanStarted }
  | { type: typeof MSG.PROGRESS; v: Progress }
  | { type: typeof MSG.SCAN_DONE; v: ScanDone }
  | { type: typeof MSG.SCAN_ENDED; v: ScanEnded }
  | { type: typeof MSG.SCAN_STATE; v: ScanState }
  | { type: typeof MSG.BYE; v: ByeMsg }
  | { type: typeof MSG.ERROR; v: ErrorMsg }
  | { type: typeof MSG.ROOTS; v: unknown }
  | { type: typeof MSG.PONG | typeof MSG.REPLAY_DONE | typeof MSG.STREAM_INIT; v: null };

const JSON_TYPES = new Set<number>([MSG.AGENT_INFO, MSG.ROOTS, MSG.BROWSE_RESULT, MSG.SCAN_STARTED, MSG.PROGRESS, MSG.SCAN_DONE, MSG.SCAN_ENDED, MSG.SCAN_STATE, MSG.BYE, MSG.ERROR]);
const BARE_TYPES = new Set<number>([MSG.PONG, MSG.REPLAY_DONE, MSG.STREAM_INIT]);

/** Decode a JSON-payload a2v body. NODES / DIR_FILES are handled by codec.ts. */
export function decodeA2VJson(body: Uint8Array): A2VJson {
  if (body.length === 0) throw new ProtocolError("empty message");
  const type = body[0];
  if (BARE_TYPES.has(type)) return { type, v: null } as A2VJson;
  if (!JSON_TYPES.has(type)) throw new ProtocolError(`unknown message type 0x${type.toString(16)}`);
  try {
    return { type, v: JSON.parse(td.decode(body.subarray(1))) } as A2VJson;
  } catch {
    throw new ProtocolError("malformed JSON payload");
  }
}
