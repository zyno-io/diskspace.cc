// SPDX-License-Identifier: MIT
// postMessage contract between RealSession (main thread) and worker.ts.
import type { AgentInfo, BrowseResult, ByeMsg, ConnState, Deleted, DeleteReq, DeleteStatus, DirFiles, ErrorMsg, NodeBatch, Progress, ScanDone, ScanEnded, ScanOptions, ScanStarted, ScanState } from "./types";

export type WorkerCommand =
  | { t: "init"; sid: string; key: Uint8Array; wsUrl: string }
  | { t: "scanStart"; path: string; opts?: ScanOptions }
  | { t: "scanCancel"; scanId: number }
  | { t: "scanPause"; scanId: number; paused: boolean }
  | { t: "deleteReq"; req: DeleteReq }
  | { t: "deleteCancel"; job: number }
  | { t: "browse"; reqId: number; path: string }
  | { t: "dirFiles"; reqId: number; scanId: number; dirId: number; limit: number }
  | { t: "applied"; n: number; gen?: number }
  | { t: "takeOver" }
  | { t: "close" };

export type WorkerEvent =
  | { t: "state"; s: ConnState }
  | { t: "agentInfo"; v: AgentInfo }
  | { t: "scanStarted"; v: ScanStarted }
  | { t: "nodes"; v: NodeBatch }
  | { t: "progress"; v: Progress }
  | { t: "scanDone"; v: ScanDone }
  | { t: "scanEnded"; v: ScanEnded }
  | { t: "scanState"; v: ScanState }
  | { t: "dirFiles"; v: DirFiles }
  | { t: "browseResult"; v: BrowseResult }
  | { t: "replayDone" }
  | { t: "deleteStatus"; v: DeleteStatus }
  | { t: "deleted"; v: Deleted }
  | { t: "bye"; v: ByeMsg }
  | { t: "error"; v: ErrorMsg };
