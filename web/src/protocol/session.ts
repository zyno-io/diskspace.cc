// SPDX-License-Identifier: MIT
// STUB — replaced by the protocol implementation. Keep this exact export signature.
import type { BrowseResult, DirFiles, ScanOptions, Session, SessionEvents } from "./types";

export interface RealSessionOptions {
  /** base64url session id from /s/<sid> */
  sid: string;
  /** 32-byte fragment secret K */
  key: Uint8Array;
  /** e.g. wss://diskspace.cc/v1/view?sid=<sid> or ws://127.0.0.1:port/v1/view?sid=<sid> */
  wsUrl: string;
}

export class RealSession implements Session {
  constructor(readonly opts: RealSessionOptions) {}
  start(events: SessionEvents): void {
    events.state({ s: "error", kind: "unsupported", message: "protocol not implemented yet" });
  }
  scanStart(_path: string, _opts?: ScanOptions): void {}
  scanCancel(_scanId: number): void {}
  browse(_path: string): Promise<BrowseResult> { return Promise.reject(new Error("not implemented")); }
  dirFiles(_scanId: number, _dirId: number, _limit?: number): Promise<DirFiles> { return Promise.reject(new Error("not implemented")); }
  applied(_wireBytes: number): void {}
  takeOver(): void {}
  close(): void {}
}
