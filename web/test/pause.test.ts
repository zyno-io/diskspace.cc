// SPDX-License-Identifier: MIT
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { AgentInfo, ScanStarted, Session, SessionEvents } from "../src/protocol/types";
import { Controller } from "../src/ui/controller";
import { isDismissed, isNewer, parseVersion, updateLink } from "../src/ui/update";

(globalThis as any).window = globalThis;

const INFO = (features?: AgentInfo["features"]): AgentInfo => ({
  proto: 1, version: "0.1.1", os: "darwin", arch: "arm64", hostname: "h", user: "u", invokingUser: "u", elevated: false, elevation: "none",
  fda: null, root: null, cwd: null, home: "/", autoScan: null, volumes: [], scan: null, features,
});
const STARTED = (scanId = 1): ScanStarted => ({ scanId, path: "/x", rootName: "x", volume: null, opts: {} });

class FakeSession implements Session {
  ev!: SessionEvents;
  pauses: [number, boolean][] = [];
  cancels: number[] = [];
  start(e: SessionEvents) { this.ev = e; }
  scanStart() {}
  scanCancel(id: number) { this.cancels.push(id); }
  scanPause(id: number, p: boolean) { this.pauses.push([id, p]); }
  deleteRequest() {}
  deleteCancel() {}
  browse() { return Promise.reject(new Error("n/a")); }
  dirFiles() { return Promise.reject(new Error("n/a")); }
  applied() {}
  takeOver() {}
  close() {}
}

function setup(features: AgentInfo["features"] | "none" = { pause: true }) {
  const s = new FakeSession();
  const env = { now: () => 0, request: (cb: (t: number | null) => void) => cb(null) };
  const ctl = new Controller(s, env);
  ctl.start();
  s.ev.state({ s: "connected", code: "123 456" });
  s.ev.agentInfo(INFO(features === "none" ? undefined : features));
  s.ev.scanStarted(STARTED());
  return { s, ctl };
}

describe("controller pause", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("pause is pending until the agent's SCAN_STATE; phase stays scanning", () => {
    const { s, ctl } = setup();
    ctl.pause();
    expect(s.pauses).toEqual([[1, true]]);
    expect(ctl.pausePending.value).toBe(true);
    expect(ctl.paused.value).toBe(false);
    s.ev.scanState({ scanId: 1, paused: true, elapsedMs: 5000 });
    expect(ctl.paused.value).toBe(true);
    expect(ctl.pausePending.value).toBe(false);
    expect(ctl.stateElapsedMs.value).toBe(5000);
    expect(ctl.phase.value).toBe("scanning");
    ctl.resume();
    expect(s.pauses.at(-1)).toEqual([1, false]);
    s.ev.scanState({ scanId: 1, paused: false, elapsedMs: 5100 });
    expect(ctl.paused.value).toBe(false);
  });

  it("does not request a pause from an agent without the feature", () => {
    const { s, ctl } = setup("none");
    expect(ctl.canPause.value).toBe(false);
    ctl.pause();
    expect(s.pauses).toEqual([]);
    expect(ctl.pausePending.value).toBe(false);
  });

  it("ignores duplicate requests while pending and a state that is already current", () => {
    const { s, ctl } = setup();
    ctl.pause();
    ctl.pause();
    expect(s.pauses.length).toBe(1);
    s.ev.scanState({ scanId: 1, paused: true, elapsedMs: 1 });
    ctl.pause(); // already paused
    expect(s.pauses.length).toBe(1);
  });

  it("clears a lost reply after a timeout", () => {
    const { ctl } = setup();
    ctl.pause();
    vi.advanceTimersByTime(5001);
    expect(ctl.pausePending.value).toBe(false);
    expect(ctl.paused.value).toBe(false);
  });

  it("a replayed scan (SCAN_STARTED then SCAN_STATE paused) shows paused; a new SCAN_STARTED resets it", () => {
    const { s, ctl } = setup();
    s.ev.scanState({ scanId: 1, paused: true, elapsedMs: 9 });
    expect(ctl.paused.value).toBe(true);
    s.ev.scanStarted(STARTED(1)); // reconnect replay restarts the same scan
    expect(ctl.paused.value).toBe(false);
    s.ev.scanState({ scanId: 1, paused: true, elapsedMs: 9 });
    expect(ctl.paused.value).toBe(true);
    s.ev.scanStarted(STARTED(2));
    expect(ctl.paused.value).toBe(false);
    expect(ctl.pausePending.value).toBe(false);
  });

  it("ignores SCAN_STATE for another scan", () => {
    const { s, ctl } = setup();
    s.ev.scanState({ scanId: 9, paused: true, elapsedMs: 1 });
    expect(ctl.paused.value).toBe(false);
  });

  it("stopping while paused cancels the scan; the end clears the pause", () => {
    const { s, ctl } = setup();
    s.ev.scanState({ scanId: 1, paused: true, elapsedMs: 1 });
    ctl.stop();
    expect(s.cancels).toEqual([1]);
    s.ev.scanEnded({ scanId: 1, reason: "canceled" });
    expect(ctl.paused.value).toBe(false);
    expect(ctl.phase.value).toBe("stopped");
  });
});

describe("update check", () => {
  it("compares versions; dev and same-version prereleases are never outdated", () => {
    expect(isNewer("0.1.1", "0.1.2")).toBe(true);
    expect(isNewer("v0.9.0", "0.10.0")).toBe(true);
    expect(isNewer("0.1.2", "0.1.2")).toBe(false);
    expect(isNewer("0.1.3", "0.1.2")).toBe(false);
    expect(isNewer("dev", "0.1.2")).toBe(false);
    expect(isNewer("0.1.2-rc1", "0.1.2")).toBe(false);
    expect(isNewer("0.1.0-demo", "0.1.2")).toBe(true);
    expect(isNewer("0.1.1", "garbage")).toBe(false);
    expect(parseVersion("dev")).toBeNull();
  });

  it("links the desktop app to its download and the CLI to the install card", () => {
    expect(updateLink({ ...INFO(), launcher: "app", os: "darwin" })).toBe("/dl/gui/mac");
    expect(updateLink({ ...INFO(), launcher: "app", os: "windows" })).toBe("/dl/gui/windows");
    expect(updateLink({ ...INFO(), launcher: "cli" })).toBe("https://diskspace.cc/#install");
    expect(updateLink(INFO())).toBe("https://diskspace.cc/#install");
  });

  it("survives blocked storage", () => {
    expect(isDismissed("0.1.2")).toBe(false); // no localStorage in node
  });
});

describe("controller stop", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("a SCAN_STATE that arrives after Stop cannot bring the running UI back", () => {
    const { s, ctl } = setup();
    ctl.stop();
    expect(ctl.stopping.value).toBe(true);
    s.ev.scanState({ scanId: 1, paused: false, elapsedMs: 9 });
    expect(ctl.paused.value).toBe(false);
    ctl.pause();
    expect(s.pauses).toEqual([]);
    s.ev.scanEnded({ scanId: 1, reason: "canceled" });
    expect(ctl.stopping.value).toBe(false);
    expect(ctl.phase.value).toBe("stopped");
  });

  it("stopping a paused scan drops the paused view and clears on a new scan", () => {
    const { s, ctl } = setup();
    s.ev.scanState({ scanId: 1, paused: true, elapsedMs: 1 });
    ctl.stop();
    expect(ctl.paused.value).toBe(false);
    s.ev.scanStarted(STARTED(2));
    expect(ctl.stopping.value).toBe(false);
  });
});
