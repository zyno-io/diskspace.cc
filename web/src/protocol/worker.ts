// SPDX-License-Identifier: MIT
// Web Worker entry: owns the WebSocket and all protocol work off the main thread.
import { ViewerConnection } from "./connection";
import type { WorkerCommand, WorkerEvent } from "./worker-protocol";

const post = (e: WorkerEvent) => (self as unknown as Worker).postMessage(e);
let conn: ViewerConnection | null = null;

self.onmessage = (ev: MessageEvent<WorkerCommand>) => {
  const c = ev.data;
  if (c.t === "init") {
    if (conn) return;
    conn = new ViewerConnection({ sid: c.sid, key: c.key, wsUrl: c.wsUrl, ua: self.navigator?.userAgent, post });
    conn.connect();
    return;
  }
  conn?.handle(c);
  if (c.t === "close") conn = null;
};
