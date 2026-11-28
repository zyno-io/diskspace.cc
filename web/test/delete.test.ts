// SPDX-License-Identifier: MIT
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { CHILD_FLAG, FILE_FLAG, type AgentInfo, type DeleteReq, type DirRec, type NodeRecord, type ScanStarted, type Session, type SessionEvents } from "../src/protocol/types";
import { KIND_DELETED, KIND_DIR, TreeStore } from "../src/store/tree";
import { Controller } from "../src/ui/controller";
import { isProtectedPath, itemKey, resolveKey } from "../src/ui/deletion";

(globalThis as any).window = globalThis;

// sessionStorage shim (node has none)
class MemStorage {
  m = new Map<string, string>();
  getItem(k: string) { return this.m.get(k) ?? null; }
  setItem(k: string, v: string) { this.m.set(k, String(v)); }
  removeItem(k: string) { this.m.delete(k); }
}

const started = (scanId = 1, path = "/Users/u"): ScanStarted => ({ scanId, path, rootName: path.split("/").pop()!, volume: null, opts: {} });
const dir = (id: number, p: Partial<DirRec> = {}): DirRec => ({ k: 1, id, flags: 0, ownAlloc: 0, ownApparent: 0, ownFiles: 0, entryErrors: 0, firstChildId: 0, dirs: [], files: [], ...p, totalDirs: p.totalDirs ?? p.dirs?.length ?? 0 });
const MB = 1048576;

/**
 * /Users/u (0)            own 1 MB, 1 file
 *   Projects (1)          own 2 MB
 *     app (3)             own 10 MB, 4 files, big.mov listed (6 MB)
 *     web (4)             own 5 MB
 *   Library (2)           own 1 MB (protected: standard folder in a home)
 *   ext (5)               other volume (flagged child, no record)
 */
function tree(): NodeRecord[] {
  return [
    dir(0, { ownAlloc: MB, ownApparent: MB, ownFiles: 1, firstChildId: 1, dirs: [{ name: "Projects", flags: 0 }, { name: "Library", flags: 0 }, { name: "Volumes", flags: 0 }, { name: "ext", flags: CHILD_FLAG.OTHER_DEVICE }] }),
    dir(1, { ownAlloc: 2 * MB, ownApparent: 2 * MB, ownFiles: 2, firstChildId: 5, dirs: [{ name: "app", flags: 0 }, { name: "web", flags: 0 }] }),
    dir(2, { ownAlloc: MB, ownApparent: MB, ownFiles: 1 }),
    dir(3, { ownAlloc: 0, ownApparent: 0 }),
    dir(5, { ownAlloc: 10 * MB, ownApparent: 10 * MB, ownFiles: 4, files: [{ name: "big.mov", alloc: 6 * MB, apparent: 6 * MB, flags: 0 }, { name: "cloud.pdf", alloc: MB, apparent: MB, flags: FILE_FLAG.CLOUD }] }),
    dir(6, { ownAlloc: 5 * MB, ownApparent: 5 * MB, ownFiles: 3 }),
  ];
}

const byName = (s: TreeStore, parent: number, name: string) => {
  const out: number[] = [];
  s.childrenOf(parent, out);
  return out.find((c) => s.names[c] === name) ?? -1;
};

function store(): TreeStore {
  const s = new TreeStore();
  s.reset(started());
  s.ingest({ scanId: 1, records: tree(), wireBytes: 0 });
  return s;
}

describe("TreeStore deletions", () => {
  it("removing a dir subtracts its totals (and itself) from every ancestor", () => {
    const s = store();
    const rootBefore = { a: s.totAlloc[0], p: s.totApparent[0], f: s.totFiles[0], d: s.totDirs[0] };
    const projects = byName(s, 0, "Projects");
    const app = byName(s, projects, "app");
    const appSize = s.totAlloc[app];
    expect(appSize).toBe(10 * MB);
    expect(s.applyDeleted({ scanId: 1, job: 1, items: [{ dir: 5, freedAlloc: 1, freedApparent: 1, files: 1, dirs: 0 }] })).toBe(true);
    expect(s.kind[app]).toBe(KIND_DELETED);
    expect(s.size(app)).toBe(0);
    expect(s.totAlloc[projects]).toBe(7 * MB);
    expect(s.totAlloc[0]).toBe(rootBefore.a - appSize);
    expect(s.totApparent[0]).toBe(rootBefore.p - 10 * MB);
    expect(s.totFiles[0]).toBe(rootBefore.f - 4);
    expect(s.totDirs[0]).toBe(rootBefore.d - 1);
    expect(byName(s, projects, "app")).toBe(-1);
    expect(s.sortedChildren(projects).filter((c) => s.kind[c] === KIND_DIR).map((c) => s.names[c])).toEqual(["web"]);
  });

  it("removing a dir with subdirs removes them from the dir count too", () => {
    const s = store();
    const d0 = s.totDirs[0];
    const projects = byName(s, 0, "Projects");
    expect(s.totDirs[projects]).toBe(2);
    s.applyDeleted({ scanId: 1, job: 1, items: [{ dir: 1, freedAlloc: 0, freedApparent: 0, files: 0, dirs: 0 }] });
    expect(s.totDirs[0]).toBe(d0 - 3);
    expect(s.totAlloc[0]).toBe(MB + MB); // root own + Library
    expect(s.isRemoved(byName(s, projects, "app"))).toBe(true);
  });

  it("tombstones are idempotent: a redelivered message or a descendant of a deleted dir is a no-op", () => {
    const s = store();
    const m = { scanId: 1, job: 7, items: [{ dir: 1, freedAlloc: 0, freedApparent: 0, files: 0, dirs: 0 }] };
    expect(s.applyDeleted(m)).toBe(true);
    const after = s.totAlloc[0];
    const v = s.version;
    expect(s.applyDeleted(m)).toBe(false);
    expect(s.applyDeleted({ ...m, job: 8 })).toBe(false); // already a tombstone
    expect(s.applyDeleted({ scanId: 1, job: 9, items: [{ dir: 5, freedAlloc: 0, freedApparent: 0, files: 0, dirs: 0 }] })).toBe(false); // under it
    expect(s.applyDeleted({ scanId: 1, job: 9, items: [{ parent: 5, name: "big.mov", freedAlloc: 0, freedApparent: 0 }] })).toBe(false);
    expect(s.applyDeleted({ scanId: 2, job: 10, items: [{ dir: 2, freedAlloc: 0, freedApparent: 0, files: 0, dirs: 0 }] })).toBe(false); // other scan
    expect(s.applyDeleted({ scanId: 1, job: 11, items: [{ dir: 0, freedAlloc: 0, freedApparent: 0, files: 0, dirs: 0 }] })).toBe(false); // never the root
    expect(s.totAlloc[0]).toBe(after);
    expect(s.version).toBe(v);
  });

  it("a partial delete subtracts the freed amount, clamped, and marks the dir stale", () => {
    const s = store();
    const projects = byName(s, 0, "Projects");
    const app = byName(s, projects, "app");
    const root0 = s.totAlloc[0];
    s.applyDeleted({ scanId: 1, job: 1, items: [{ dir: 5, partial: true, freedAlloc: 4 * MB, freedApparent: 4 * MB, files: 2, dirs: 0 }] });
    expect(s.kind[app]).toBe(KIND_DIR);
    expect(s.isStale(app)).toBe(true);
    expect(s.totAlloc[app]).toBe(6 * MB);
    expect(s.totFiles[app]).toBe(2);
    expect(s.totAlloc[0]).toBe(root0 - 4 * MB);
    // more than is left: clamped at zero, never negative
    s.applyDeleted({ scanId: 1, job: 2, items: [{ dir: 5, partial: true, freedAlloc: 999 * MB, freedApparent: 999 * MB, files: 99, dirs: 9 }] });
    expect(s.totAlloc[app]).toBe(0);
    expect(s.totFiles[app]).toBe(0);
    expect(s.totDirs[app]).toBe(0);
    expect(s.totAlloc[0]).toBe(root0 - 10 * MB);
  });

  it("removing a listed file adjusts its parent's own and listed bytes; the bucket stays", () => {
    const s = store();
    const app = s.nodeOfWire(5);
    const bucketBefore = s.size(s.bucket[app]);
    const file = s.findFile(app, "big.mov");
    expect(file).toBeGreaterThan(0);
    s.applyDeleted({ scanId: 1, job: 1, items: [{ parent: 5, name: "big.mov", freedAlloc: 6 * MB, freedApparent: 6 * MB }] });
    expect(s.kind[file]).toBe(KIND_DELETED);
    expect(s.ownAlloc[app]).toBe(4 * MB);
    expect(s.listedAlloc[app]).toBe(MB);
    expect(s.ownFiles[app]).toBe(3);
    expect(s.listedCount[app]).toBe(1);
    expect(s.totAlloc[app]).toBe(4 * MB);
    expect(s.size(s.bucket[app])).toBe(bucketBefore);
    expect(s.findFile(app, "big.mov")).toBe(-1);
  });

  it("removing an unlisted file shrinks the bucket by the agent's amount, clamped", () => {
    const s = store();
    const web = s.nodeOfWire(6);
    s.applyDeleted({ scanId: 1, job: 1, items: [{ parent: 6, name: "hidden.bin", freedAlloc: 2 * MB, freedApparent: 2 * MB }] });
    expect(s.totAlloc[web]).toBe(3 * MB);
    expect(s.files(s.bucket[web])).toBe(2);
    s.applyDeleted({ scanId: 1, job: 2, items: [{ parent: 6, name: "huge.bin", freedAlloc: 99 * MB, freedApparent: 99 * MB }] });
    expect(s.totAlloc[web]).toBe(0);
    expect(s.ownAlloc[web]).toBe(0);
  });

  it("totals never go negative when a partial delete freed more than a later full delete expects", () => {
    const s = store();
    // Projects (wire 1) partly deleted for more than its subtree, then a child goes too
    s.applyDeleted({ scanId: 1, job: 1, items: [{ dir: 1, partial: true, freedAlloc: 16 * MB, freedApparent: 16 * MB, files: 8, dirs: 1 }] });
    s.applyDeleted({ scanId: 1, job: 2, items: [{ dir: 5, freedAlloc: 0, freedApparent: 0, files: 0, dirs: 0 }] });
    for (let i = 0; i < s.n; i++) {
      expect(s.totAlloc[i]).toBeGreaterThanOrEqual(0);
      expect(s.totApparent[i]).toBeGreaterThanOrEqual(0);
      expect(s.totFiles[i]).toBeGreaterThanOrEqual(0);
      expect(s.totDirs[i]).toBeGreaterThanOrEqual(0);
    }
  });

  it("survivor: nearest live ancestor of a deleted node", () => {
    const s = store();
    const projects = byName(s, 0, "Projects");
    const app = byName(s, projects, "app");
    const file = s.findFile(app, "big.mov");
    expect(s.survivor(file)).toBe(file);
    s.applyDeleted({ scanId: 1, job: 1, items: [{ dir: 1, freedAlloc: 0, freedApparent: 0, files: 0, dirs: 0 }] });
    expect(s.survivor(file)).toBe(0);
    expect(s.survivor(app)).toBe(0);
    expect(s.survivor(projects)).toBe(0);
  });
});

describe("deletion keys and protected folders", () => {
  it("keys round-trip through the tree", () => {
    const s = store();
    const app = s.nodeOfWire(5);
    const file = s.findFile(app, "big.mov");
    expect(itemKey(s, app)).toBe("d:5");
    expect(itemKey(s, file)).toBe("f:5:big.mov");
    expect(resolveKey(s, "d:5")).toBe(app);
    expect(resolveKey(s, "f:5:big.mov")).toBe(file);
    expect(resolveKey(s, "f:5:nope")).toBe(-1);
    expect(resolveKey(s, "d:999")).toBe(-1);
    expect(resolveKey(s, "x:1")).toBe(-1);
  });

  it("protects roots, homes, system and standard home folders and their ancestors", () => {
    const mac = { os: "darwin", home: "/Users/u", volumes: [{ path: "/", label: "", fs: "", total: 0, free: 0, used: 0, kind: "boot" as const, readOnly: false }, { path: "/Volumes/Ext", label: "", fs: "", total: 0, free: 0, used: 0, kind: "external" as const, readOnly: false }] };
    for (const p of ["/", "/Users", "/Users/u", "/Users/other", "/Users/u/Library", "/users/u/documents", "/Users/u/.Trash", "/System", "/Volumes", "/Volumes/Ext", "/private"]) expect(isProtectedPath(p, mac), p).toBe(true);
    for (const p of ["/Users/u/Projects", "/Users/u/Library/Caches", "/private/var/folders", "/Volumes/Ext/stuff", "/Users/u/Documents/old"]) expect(isProtectedPath(p, mac), p).toBe(false);
    const win = { os: "windows", home: "C:\\Users\\u", volumes: [{ path: "C:\\", label: "", fs: "", total: 0, free: 0, used: 0, kind: "boot" as const, readOnly: false }] };
    for (const p of ["C:\\", "C:\\Users", "C:\\Users\\u", "C:\\Users\\u\\AppData", "C:\\Windows", "C:\\Program Files (x86)", "C:\\ProgramData", "D:\\$Recycle.Bin"]) expect(isProtectedPath(p, win), p).toBe(true);
    for (const p of ["C:\\Users\\u\\AppData\\Local\\Temp", "C:\\Users\\u\\Projects", "C:\\games"]) expect(isProtectedPath(p, win), p).toBe(false);
    const linux = { os: "linux", home: "/home/deploy", volumes: [{ path: "/", label: "", fs: "", total: 0, free: 0, used: 0, kind: "internal" as const, readOnly: false }, { path: "/mnt/backup", label: "", fs: "", total: 0, free: 0, used: 0, kind: "internal" as const, readOnly: false }] };
    for (const p of ["/", "/boot", "/tmp", "/srv", "/snap", "/var", "/var/tmp", "/root", "/home", "/home/deploy", "/home/other", "/home/deploy/.config", "/home/deploy/.local", "/home/deploy/.local/share", "/home/deploy/.local/share/Trash", "/home/deploy/Downloads", "/root/.local", "/mnt", "/mnt/backup", "/usr/lib", "/usr/bin"]) expect(isProtectedPath(p, linux), p).toBe(true);
    // case-sensitive on Linux, and a trailing slash on home is ignored
    expect(isProtectedPath("/home/deploy/downloads", linux)).toBe(false);
    expect(isProtectedPath("/srv/app/.config", { ...linux, home: "/srv/app/" })).toBe(true);
    for (const p of ["/var/lib", "/var/log", "/var/lib/docker", "/home/deploy/app", "/home/deploy/.cache", "/home/deploy/.npm", "/home/deploy/Downloads/old", "/opt/acme-agent", "/root/.cache", "/data/uploads", "/srv/www", "/usr/lib/node_modules", "/usr/local"]) expect(isProtectedPath(p, linux), p).toBe(false);
  });
});

// ---- controller ---------------------------------------------------------------------

const INFO = (del: boolean | "none" = true): AgentInfo => ({
  proto: 1, version: "0.2.0", os: "darwin", arch: "arm64", hostname: "mac", user: "u", invokingUser: "u", elevated: false, elevation: "none",
  fda: null, root: null, cwd: null, home: "/Users/u", autoScan: null, volumes: [], scan: null,
  features: del === "none" ? { pause: true } : { pause: true, delete: { available: del, confirm: "terminal", trash: true, maxItems: 500 } },
});

class FakeSession implements Session {
  ev!: SessionEvents;
  reqs: DeleteReq[] = [];
  cancels: number[] = [];
  constructor(readonly id = "sid1") {}
  start(e: SessionEvents) { this.ev = e; }
  scanStart() {}
  scanCancel() {}
  scanPause() {}
  deleteRequest(r: DeleteReq) { this.reqs.push(r); }
  deleteCancel(job: number) { this.cancels.push(job); }
  browse() { return Promise.reject(new Error("n/a")); }
  dirFiles: Session["dirFiles"] = () => Promise.reject(new Error("n/a"));
  applied() {}
  takeOver() {}
  close() {}
}

const DONE = (scanId = 1) => ({ scanId, totals: { alloc: 0, apparent: 0, files: 0, dirs: 0 }, denied: 0, errors: 0, vanished: 0, skippedMounts: 0, hardlinksDeduped: 0, folded: 0, ms: 1 });

function setup(opts: { del?: boolean | "none"; done?: boolean; session?: FakeSession } = {}) {
  const s = opts.session ?? new FakeSession();
  const env = { now: () => 0, request: (cb: (t: number | null) => void) => cb(null) };
  const ctl = new Controller(s, env);
  ctl.start();
  s.ev.state({ s: "connected", code: "123 456" });
  s.ev.agentInfo(INFO(opts.del ?? true));
  s.ev.scanStarted(started());
  s.ev.nodes({ scanId: 1, records: tree(), wireBytes: 0 });
  if (opts.done !== false) s.ev.scanDone(DONE());
  const st = ctl.store;
  const n = { projects: st.nodeOfWire(1), library: st.nodeOfWire(2), volumes: st.nodeOfWire(3), ext: st.nodeOfWire(4), app: st.nodeOfWire(5), web: st.nodeOfWire(6) };
  return { s, ctl, st, n, file: st.findFile(n.app, "big.mov") };
}

describe("controller deletion queue", () => {
  let mem: MemStorage;
  beforeEach(() => {
    mem = new MemStorage();
    (globalThis as any).sessionStorage = mem;
  });
  afterEach(() => {
    delete (globalThis as any).sessionStorage;
  });

  it("disabled reasons", () => {
    const { ctl, st, n } = setup();
    expect(ctl.queueReason(st.root)).toBe("This is the scanned folder");
    expect(ctl.queueReason(n.library)).toMatch(/^Protected folder/);
    expect(ctl.queueReason(n.ext)).toBe("On another volume");
    expect(ctl.queueReason(st.bucket[n.app])).toMatch(/^Grouped files/);
    expect(ctl.queueReason(n.projects)).toBeNull();
    const old = setup({ del: "none" });
    expect(old.ctl.queueReason(old.n.projects)).toBe("Update diskspace on mac");
    expect(old.ctl.toggleQueue(old.n.projects)).toBe("Update diskspace on mac");
    expect(old.ctl.queue.value).toEqual([]);
    const off = setup({ del: false });
    expect(off.ctl.queueReason(off.n.projects)).not.toBeNull();
  });

  it("an item under a queued folder is included in it; queueing a parent absorbs queued children", () => {
    const { ctl, n, file } = setup();
    expect(ctl.addToQueue(n.app)).toBeNull();
    expect(ctl.addToQueue(file)).toBe("Included in queued app");
    expect(ctl.addToQueue(n.projects)).toBeNull();
    expect(ctl.queue.value).toEqual([n.projects]);
    expect(ctl.queueReason(n.web)).toBe("Included in queued Projects");
    expect(ctl.toggleQueue(n.projects)).toBeNull();
    expect(ctl.queue.value).toEqual([]);
    expect(ctl.queueReason(n.web)).toBeNull();
  });

  it("queueing is allowed while scanning; applying waits for the scan to end", () => {
    const { s, ctl, n, file } = setup({ done: false });
    expect(ctl.addToQueue(n.web)).toBeNull();
    expect(ctl.addToQueue(file)).toBeNull();
    expect(ctl.applyReason()).toBe("Finish or stop the scan first");
    expect(ctl.applyQueue("trash")).toBe("Finish or stop the scan first");
    expect(ctl.trashNow(n.web)).toBe("Finish or stop the scan first");
    expect(s.reqs).toEqual([]);
    s.ev.scanDone(DONE());
    expect(ctl.applyReason()).toBeNull();
    expect(ctl.applyQueue("permanent")).toBeNull();
    expect(s.reqs).toHaveLength(1);
    expect(s.reqs[0]).toMatchObject({ scanId: 1, mode: "permanent", items: [{ dir: 6 }, { parent: 5, name: "big.mov" }] });
    // one request at a time
    expect(ctl.applyReason()).toBe("Another request is in progress");
    expect(ctl.trashNow(n.projects)).toBe("Another request is in progress");
  });

  it("a stopped scan can be applied too", () => {
    const { s, ctl, n } = setup({ done: false });
    s.ev.scanEnded({ scanId: 1, reason: "canceled" });
    expect(ctl.trashNow(n.web)).toBeNull();
    expect(s.reqs[0].items).toEqual([{ dir: 6 }]);
  });

  it("follows the request through approval, cancel before the job is known, and the result", () => {
    const { s, ctl, n } = setup();
    ctl.addToQueue(n.web);
    ctl.applyQueue("trash");
    const reqId = s.reqs[0].reqId;
    ctl.cancelRequest(); // no job id yet: sent with the first status
    expect(s.cancels).toEqual([]);
    s.ev.deleteStatus({ job: 42, reqId, state: "preparing", total: 1, done: 0, freedAlloc: 0 });
    expect(s.cancels).toEqual([42]);
    s.ev.deleteStatus({ job: 42, reqId, state: "refused", code: "canceled", total: 1, done: 0, freedAlloc: 0 });
    expect(ctl.request.value?.status?.state).toBe("refused");
    expect(ctl.queue.value).toEqual([n.web]); // nothing deleted: still queued
    ctl.closeRequest();
    expect(ctl.request.value).toBeNull();
  });

  it("DELETED prunes the queue and moves zoom and selection to the nearest survivor", () => {
    const { s, ctl, st, n } = setup();
    ctl.addToQueue(n.projects);
    ctl.addToQueue(n.volumes);
    ctl.zoom.value = n.app;
    ctl.selected.value = st.findFile(n.app, "big.mov");
    ctl.applyQueue("trash");
    const reqId = s.reqs[0].reqId;
    s.ev.deleteStatus({ job: 3, reqId, state: "running", total: 2, done: 1, freedAlloc: 1 });
    expect(ctl.request.value?.job).toBe(3);
    s.ev.deleted({ scanId: 1, job: 3, items: [{ dir: 1, freedAlloc: 1, freedApparent: 1, files: 1, dirs: 2 }] });
    s.ev.deleteStatus({ job: 3, reqId, state: "done", total: 2, done: 2, freedAlloc: 1, results: [{ i: 0, ok: true, freedAlloc: 1, files: 1, dirs: 2 }, { i: 1, ok: false, code: "denied", freedAlloc: 0, files: 0, dirs: 0 }] });
    expect(ctl.zoom.value).toBe(st.root);
    expect(ctl.selected.value).toBe(st.root);
    expect(ctl.queue.value).toEqual([n.volumes]);
    expect(JSON.parse(mem.getItem("diskspace:dq:sid1:1")!)).toEqual(["d:3"]);
  });

  it("persists the queue per sid:scanId and re-resolves it after a replay", () => {
    const { s, ctl, n } = setup();
    ctl.addToQueue(n.web);
    ctl.addToQueue(ctl.store.findFile(n.app, "big.mov"));
    expect(JSON.parse(mem.getItem("diskspace:dq:sid1:1")!)).toEqual(["d:6", "f:5:big.mov"]);
    // reconnect: the same scan is replayed into a reset store
    s.ev.scanStarted(started());
    expect(ctl.queue.value).toEqual([]);
    s.ev.nodes({ scanId: 1, records: tree(), wireBytes: 0 });
    s.ev.scanDone(DONE());
    s.ev.replayDone();
    const st = ctl.store;
    expect(ctl.queue.value.map((q) => st.names[q])).toEqual(["web", "big.mov"]);

    // a reload: a new controller, same session id, finds it in storage
    const s2 = new FakeSession("sid1");
    const again = setup({ session: s2 });
    expect(again.ctl.queue.value.map((q) => again.st.names[q])).toEqual(["web", "big.mov"]);
    // another link (sid) or another scan does not
    const other = setup({ session: new FakeSession("sid2") });
    expect(other.ctl.queue.value).toEqual([]);
  });

  it("drops persisted entries that no longer resolve or are now disallowed", () => {
    mem.setItem("diskspace:dq:sid1:1", JSON.stringify(["d:999", "d:2", "f:5:gone.txt", "d:5", "f:5:big.mov", 7]));
    const { ctl, n } = setup();
    expect(ctl.queue.value).toEqual([n.app]); // Library is protected; big.mov is inside app
    expect(JSON.parse(mem.getItem("diskspace:dq:sid1:1")!)).toEqual(["d:5"]);
  });

  it("adopts a running job announced in AGENT_INFO after a reconnect", () => {
    const { s, ctl } = setup();
    s.ev.agentInfo({ ...INFO(), deleting: { job: 9, state: "running", done: 1, total: 4 } });
    expect(ctl.request.value?.job).toBe(9);
    expect(ctl.applyReason()).not.toBeNull();
    s.ev.deleteStatus({ job: 9, reqId: 1, state: "done", total: 4, done: 4, freedAlloc: 10 });
    expect(ctl.request.value?.status?.state).toBe("done");
  });

  it("a running request whose job ended while disconnected can be closed after reconnecting", () => {
    const { s, ctl, n } = setup();
    ctl.trashNow(n.web);
    const reqId = s.reqs[0].reqId;
    s.ev.deleteStatus({ job: 4, reqId, state: "running", total: 1, done: 0, freedAlloc: 0 });
    s.ev.state({ s: "reconnecting", attempt: 1 });
    expect(ctl.request.value?.localError).toBeUndefined(); // approved: it keeps running on the host
    s.ev.state({ s: "connected", code: "9" });
    s.ev.agentInfo({ ...INFO(), deleting: null });
    expect(ctl.request.value?.localError).toBe("ended_offline");
    ctl.closeRequest();
    expect(ctl.request.value).toBeNull();
    expect(ctl.applyReason()).toBeNull();
  });

  it("a job's own status clears AGENT_INFO.deleting once it ends", () => {
    const { s, ctl } = setup();
    s.ev.agentInfo({ ...INFO(), deleting: { job: 9, state: "running", done: 1, total: 2 } });
    s.ev.deleteStatus({ job: 9, reqId: 1, state: "done", total: 2, done: 2, freedAlloc: 1 });
    ctl.closeRequest();
    expect(ctl.info.value?.deleting).toBeNull();
    expect(ctl.applyReason()).toBeNull();
  });

  it("no request while the connection is not up", () => {
    const { s, ctl, n } = setup();
    s.ev.state({ s: "handshaking" });
    expect(ctl.applyReason()).toBe("Not connected");
    expect(ctl.trashNow(n.web)).toBe("Not connected");
    expect(s.reqs).toEqual([]);
  });

  it("Show all files keeps a queued file queued (its slot moves)", async () => {
    const { s, ctl, st, n, file } = setup();
    ctl.addToQueue(file);
    s.dirFiles = () =>
      Promise.resolve({ reqId: 1, scanId: 1, dirId: 5, flags: 0, restCount: 0, restAlloc: 0, restApparent: 0, files: [
        { name: "big.mov", alloc: 6 * MB, apparent: 6 * MB, flags: 0 },
        { name: "cloud.pdf", alloc: MB, apparent: MB, flags: FILE_FLAG.CLOUD },
        { name: "c.bin", alloc: MB, apparent: MB, flags: 0 },
      ] });
    await ctl.showAllFiles(n.app);
    const now = st.findFile(n.app, "big.mov");
    expect(now).not.toBe(file);
    expect(ctl.queue.value).toEqual([now]);
    ctl.applyQueue("trash");
    expect(s.reqs[0].items).toEqual([{ parent: 5, name: "big.mov" }]);
  });

  it("uses the volume's usage from SCAN_STARTED for the synthetic remainder", () => {
    const s = new FakeSession();
    const env = { now: () => 0, request: (cb: (t: number | null) => void) => cb(null) };
    const ctl = new Controller(s, env);
    ctl.start();
    s.ev.state({ s: "connected", code: "1" });
    s.ev.agentInfo({ ...INFO(), volumes: [{ path: "/Users/u", label: "", fs: "", total: 100 * MB, free: 0, used: 999 * MB, kind: "internal", readOnly: false }] });
    s.ev.scanStarted({ ...started(), volume: { total: 100 * MB, free: 50 * MB, used: 50 * MB } });
    s.ev.nodes({ scanId: 1, records: tree(), wireBytes: 0 });
    s.ev.scanDone(DONE());
    const st = ctl.store;
    expect(st.size(st.synth)).toBe(50 * MB - st.totAlloc[st.root]);
  });
});
