// SPDX-License-Identifier: MIT
//
// Hand-written types for the Wails bindings the desktop app injects
// (window.go.main.App.* and window.runtime.EventsOn). They mirror gui/app.go;
// there is no Wails codegen in this repo.

export type Phase = "idle" | "starting" | "running" | "cancelled" | "error";

/** gui/app.go Snapshot */
export interface Snapshot {
  seq: number;
  phase: Phase;
  admin: boolean;
  os: string;
  version: string;
  user: string;
  guiElevated: boolean;
  elevated: boolean;
  fda: "granted" | "denied" | "";
  savedChoice: "" | "admin" | "user";
  url: string;
  localUrl: string;
  viewer: "" | "connected" | "disconnected" | "superseded";
  code: string;
  scan: "" | "started" | "done" | "canceled";
  scanPath: string;
  notice: string;
  message: string;
  expanded: boolean;
}

/** window.go.main.App — every call returns a promise; Go errors reject with a string */
export interface AppBindings {
  State(): Promise<Snapshot>;
  Start(admin: boolean, remember: boolean): Promise<void>;
  Restart(admin: boolean): Promise<void>;
  Reset(): Promise<void>;
  Forget(): Promise<void>;
  Rotate(): Promise<void>;
  OpenInBrowser(url: string): Promise<void>;
  OpenHere(): Promise<string>;
  LauncherShown(): Promise<void>;
  OpenFDASettings(): Promise<void>;
  Copy(text: string): Promise<void>;
  Quit(): Promise<void>;
}

export interface WailsRuntime {
  EventsOn(name: string, cb: (...data: unknown[]) => void): () => void;
}

declare global {
  interface Window {
    go?: { main?: { App?: AppBindings } };
    runtime?: WailsRuntime;
  }
}

export interface Bridge {
  app: AppBindings;
  runtime: WailsRuntime;
}

/** the desktop bridge, or null when this page is opened in a normal browser */
export async function getBridge(waitMs = 400): Promise<Bridge | null> {
  const t0 = performance.now();
  for (;;) {
    const app = window.go?.main?.App;
    if (app && window.runtime?.EventsOn) return { app, runtime: window.runtime };
    if (performance.now() - t0 >= waitMs) return null;
    await new Promise((r) => setTimeout(r, 25));
  }
}

/** a short, display-only form of the link: host + session id, key hidden */
export function displayLink(url: string): { base: string; hasKey: boolean } {
  try {
    const u = new URL(url);
    return { base: u.host + u.pathname, hasKey: u.hash.length > 1 };
  } catch {
    return { base: url, hasKey: false };
  }
}

export type StatusTone = "wait" | "live" | "busy" | "done";

/** the one-line status under the QR code */
export function statusOf(s: Snapshot): { tone: StatusTone; text: string } {
  if (!s.url && !s.localUrl) return { tone: "wait", text: "Getting a secure link" };
  if (s.scan === "started") return { tone: "busy", text: s.scanPath ? `Scanning ${s.scanPath}` : "Scanning" };
  if (s.viewer === "connected") {
    if (s.scan === "done") return { tone: "done", text: "Done" };
    return { tone: "live", text: "Connected" };
  }
  if (s.scan === "done") return { tone: "done", text: "Done" };
  return { tone: "wait", text: "Waiting for a browser" };
}
