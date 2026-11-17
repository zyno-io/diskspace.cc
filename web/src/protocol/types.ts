// SPDX-License-Identifier: MIT
//
// Shared contract between the protocol layer (worker + RealSession) and the
// UI (store, treemap, list, picker). The UI depends ONLY on this file and on
// the Session interface; MockSession and RealSession both implement it.
// Wire details live in docs/protocol.md.

// ---- message type bytes ----------------------------------------------------
export const MSG = {
  // agent → viewer
  STREAM_INIT: 0x00,
  AGENT_INFO: 0x01,
  ROOTS: 0x02,
  BROWSE_RESULT: 0x03,
  PONG: 0x08,
  SCAN_STARTED: 0x10,
  NODES: 0x11,
  PROGRESS: 0x12,
  SCAN_DONE: 0x13,
  SCAN_ENDED: 0x14,
  DIR_FILES: 0x15,
  REPLAY_DONE: 0x16,
  BYE: 0x7e,
  ERROR: 0x7f,
  // viewer → agent
  CLIENT_HELLO: 0x81,
  GET_ROOTS: 0x82,
  BROWSE: 0x83,
  SCAN_START: 0x84,
  SCAN_CANCEL: 0x85,
  DIR_FILES_REQ: 0x86,
  CREDIT: 0x87,
  PING: 0x88,
} as const;

// ---- flags (mirror internal/proto/records.go) --------------------------------
export const DIR_FLAG = { READ_DENIED: 1, READ_ERROR: 2, VANISHED: 4, FOLDED_CHILDREN: 8, HAS_ENTRY_ERRORS: 16 } as const;
export const CHILD_FLAG = { OTHER_DEVICE: 1, REPARSE: 2, EXCLUDED: 4, DUP_DIR: 8, BIND_MOUNT: 16 } as const;
export const FILE_FLAG = { MULTILINK: 1, SYMLINK: 2, SPARSE: 4, CLOUD: 8, SPECIAL: 16, BAD_NAME: 32 } as const;

// ---- JSON payloads -----------------------------------------------------------
export interface Volume {
  path: string;
  label: string;
  fs: string;
  total: number;
  free: number;
  used: number;
  kind: "boot" | "internal" | "external" | "network";
  readOnly: boolean;
}

export interface AgentInfo {
  proto: number;
  version: string;
  os: "darwin" | "linux" | "windows" | string;
  arch: string;
  hostname: string;
  user: string;
  invokingUser: string;
  elevated: boolean;
  elevation: "none" | "sudo" | "uac" | "gui-admin" | "native";
  /** where the user started diskspace; absent from older agents (= cli) */
  launcher?: "cli" | "app";
  fda: "granted" | "denied" | null;
  root: string | null;
  cwd: { path: string } | null;
  home: string;
  autoScan: string | null;
  volumes: Volume[];
  scan: { id: number; state: "running" | "done" | "canceled" | "error" } | null;
}

export interface ScanOptions {
  crossMounts?: boolean;
  topFiles?: number;
  minFileBytes?: number;
}

/** Application protocol version; agent and viewer must match exactly. */
export const PROTO_VERSION = 1;

export interface ScanStarted {
  scanId: number;
  path: string;
  rootName: string;
  volume: { total: number; free: number; used: number } | null;
  opts: ScanOptions;
}

export interface Progress {
  scanId: number;
  dirs: number;
  files: number;
  alloc: number;
  apparent: number;
  errors: number;
  elapsedMs: number;
  currentPath?: string;
}

export interface ScanDone {
  scanId: number;
  totals: { alloc: number; apparent: number; files: number; dirs: number };
  denied: number;
  errors: number;
  vanished: number;
  skippedMounts: number;
  hardlinksDeduped: number;
  folded: number;
  ms: number;
}

export interface ScanEnded {
  scanId: number;
  reason: "canceled" | "error";
  message?: string;
}

export interface BrowseResult {
  reqId: number;
  path: string;
  parent: string | null;
  dirs: string[]; // names only
  error?: string;
}

export interface ByeMsg {
  reason: "superseded" | "shutdown" | "idle" | string;
}

export interface ErrorMsg {
  code: string;
  message: string;
}

// ---- decoded NODES records (worker → main) ---------------------------------
export interface FileRec {
  name: string;
  alloc: number;
  apparent: number;
  flags: number;
}

export interface ChildRec {
  name: string;
  flags: number;
}

export interface DirRec {
  k: 1;
  id: number;
  flags: number;
  ownAlloc: number;
  ownApparent: number;
  ownFiles: number;
  entryErrors: number;
  /** total child dirs announced; dirs (inline) is a prefix, the rest follow in DirMoreRec */
  totalDirs: number;
  firstChildId: number; // meaningful when totalDirs > 0; child i id = firstChildId + i
  dirs: ChildRec[];
  files: FileRec[];
}

export interface FoldRec {
  k: 2;
  targetId: number;
  alloc: number;
  apparent: number;
  files: number;
  dirs: number;
}

export interface FoldEndRec {
  k: 3;
  targetId: number;
}

/** Continuation of DirRec.dirs for an already-announced dir: child i has id firstChildId + i. */
export interface DirMoreRec {
  k: 4;
  id: number;
  firstChildId: number;
  dirs: ChildRec[];
}

export type NodeRecord = DirRec | FoldRec | FoldEndRec | DirMoreRec;

export interface NodeBatch {
  scanId: number;
  records: NodeRecord[];
  /** ciphertext bytes this batch represents; UI calls session.applied(bytes) after ingesting */
  wireBytes: number;
}

export interface DirFiles {
  reqId: number;
  scanId: number;
  dirId: number;
  flags: number;
  restCount: number;
  restAlloc: number;
  restApparent: number;
  files: FileRec[];
}

// ---- Session interface (UI ↔ protocol) --------------------------------------
export type ConnState =
  | { s: "connecting" }
  | { s: "handshaking" }
  | { s: "connected"; code: string } // 6-digit verification code, formatted "123 456"
  | { s: "reconnecting"; attempt: number }
  | { s: "superseded" } // another tab/device took over; offer takeOver()
  | { s: "closed"; reason: string }
  | { s: "error"; kind: "key-mismatch" | "no-session" | "unsupported" | "version" | "protocol" | "network"; message: string };

export interface SessionEvents {
  state(s: ConnState): void;
  agentInfo(info: AgentInfo): void;
  scanStarted(m: ScanStarted): void;
  nodes(b: NodeBatch): void;
  progress(p: Progress): void;
  scanDone(m: ScanDone): void;
  scanEnded(m: ScanEnded): void;
  replayDone(): void;
  bye(m: ByeMsg): void;
  error(m: ErrorMsg): void;
}

export interface Session {
  /** start connecting; events are delivered to the handlers */
  start(events: SessionEvents): void;
  scanStart(path: string, opts?: ScanOptions): void;
  scanCancel(scanId: number): void;
  browse(path: string): Promise<BrowseResult>;
  dirFiles(scanId: number, dirId: number, limit?: number): Promise<DirFiles>;
  /** flow control: report ciphertext bytes the UI has finished applying */
  applied(wireBytes: number): void;
  /** reconnect after being superseded */
  takeOver(): void;
  close(): void;
}
