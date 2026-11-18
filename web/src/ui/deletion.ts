// SPDX-License-Identifier: MIT
//
// Deletion queue helpers: which nodes may be queued (and why not), how queue
// entries are keyed for sessionStorage and DELETE_REQ, protected folders, and
// the copy for agent result codes. The agent re-checks everything; this only
// keeps the viewer from offering what the agent would refuse.

import { CHILD_FLAG, DIR_FLAG, FILE_FLAG, type AgentInfo, type DeleteItem } from "../protocol/types";
import { KIND_BUCKET, KIND_DIR, KIND_FILE, KIND_SYNTH, type TreeStore } from "../store/tree";

/** permanent deletes above these need a second confirmation on the host */
export const LARGE_BYTES = 100e9;
export const LARGE_FILES = 100_000;
export const DEFAULT_MAX_ITEMS = 500;

// ---- keys ------------------------------------------------------------------------

/** "d:<wire>" for a dir, "f:<parentWire>:<name>" for a listed file; null for anything else */
export function itemKey(st: TreeStore, n: number): string | null {
  const k = st.kind[n];
  if (k === KIND_DIR) {
    const w = st.wire(n);
    return w >= 0 ? `d:${w}` : null;
  }
  if (k === KIND_FILE) {
    const p = st.parent[n];
    const w = p >= 0 ? st.wire(p) : -1;
    return w >= 0 ? `f:${w}:${st.names[n]}` : null;
  }
  return null;
}

/** node for a key in the current tree, or -1 (unknown or deleted) */
export function resolveKey(st: TreeStore, key: string): number {
  let n = -1;
  if (key.startsWith("d:")) {
    const w = Number(key.slice(2));
    if (Number.isInteger(w)) n = st.nodeOfWire(w);
    if (n >= 0 && st.kind[n] !== KIND_DIR) n = -1;
  } else if (key.startsWith("f:")) {
    const c = key.indexOf(":", 2);
    const w = c > 2 ? Number(key.slice(2, c)) : NaN;
    const p = Number.isInteger(w) ? st.nodeOfWire(w) : -1;
    if (p >= 0 && st.kind[p] === KIND_DIR) n = st.findFile(p, key.slice(c + 1));
  }
  return n >= 0 && !st.isRemoved(n) ? n : -1;
}

/** the DELETE_REQ item for a node (dirs by wire id, files by parent wire id + name) */
export function wireItem(st: TreeStore, n: number): DeleteItem | null {
  if (st.kind[n] === KIND_DIR) {
    const w = st.wire(n);
    return w > 0 ? { dir: w } : null;
  }
  if (st.kind[n] === KIND_FILE) {
    const p = st.parent[n];
    const w = p >= 0 ? st.wire(p) : -1;
    return w >= 0 ? { parent: w, name: st.names[n] } : null;
  }
  return null;
}

// ---- protected folders ---------------------------------------------------------------

const UNIX_SYSTEM = ["/", "/Users", "/home", "/root", "/var/root", "/System", "/Library", "/Applications", "/usr", "/bin", "/sbin", "/etc", "/var", "/private", "/opt", "/dev", "/cores", "/Volumes"];
const UNIX_HOME_DIRS = ["Library", "Documents", "Desktop", "Downloads", "Pictures", "Movies", "Music", "Public", "Applications", ".Trash"];
const WIN_HOME_DIRS = ["AppData", "Documents", "Desktop", "Downloads", "Pictures", "Videos", "Music", "OneDrive"];
const WIN_SYSTEM = /^[a-z]:\\(windows|program files[^\\]*|programdata|\$recycle\.bin|system volume information|users)$/;

function norm(p: string, win: boolean, fold: boolean): string {
  let s = win ? p.replace(/\//g, "\\") : p;
  const sep = win ? "\\" : "/";
  while (s.length > 1 && s.endsWith(sep) && !(win && /^[a-zA-Z]:\\$/.test(s))) s = s.slice(0, -1);
  if (win && /^[a-zA-Z]:$/.test(s)) s += "\\";
  return fold ? s.toLowerCase() : s;
}

/**
 * Folders deleted only by their contents (owner decision 5): the filesystem
 * root, homes and the homes root, system roots, volume roots, the standard
 * folders directly inside a home, and any ancestor of those.
 */
export function isProtectedPath(path: string, info: Pick<AgentInfo, "os" | "home" | "volumes">): boolean {
  const win = info.os === "windows";
  const fold = win || info.os === "darwin";
  const p = norm(path, win, fold);
  const sep = win ? "\\" : "/";
  const fixed: string[] = [];
  for (const v of info.volumes ?? []) fixed.push(norm(v.path, win, fold));
  if (info.home) fixed.push(norm(info.home, win, fold));
  if (win) {
    if (/^[a-z]:\\$/.test(p) || /^\\\\[^\\]+\\[^\\]+$/.test(p)) return true; // drive or share root
    if (WIN_SYSTEM.test(p)) return true;
    const m = /^[a-z]:\\users\\[^\\]+(?:\\([^\\]+))?$/.exec(p);
    if (m && (!m[1] || WIN_HOME_DIRS.some((d) => d.toLowerCase() === m[1]))) return true;
  } else {
    if (UNIX_SYSTEM.some((s) => norm(s, false, fold) === p)) return true;
    const m = /^(?:\/users|\/home)\/[^/]+(?:\/([^/]+))?$/i.exec(p);
    if (m && (fold || /^\/(Users|home)\//.test(p)) && (!m[1] || UNIX_HOME_DIRS.some((d) => (fold ? d.toLowerCase() : d) === m[1]))) return true;
    if (info.home) {
      const h = norm(info.home, false, fold);
      const rest = p.startsWith(h + "/") ? p.slice(h.length + 1) : null;
      if (rest && !rest.includes("/") && UNIX_HOME_DIRS.some((d) => (fold ? d.toLowerCase() : d) === rest)) return true;
    }
  }
  // exactly one of the fixed paths, or an ancestor of one
  return fixed.some((f) => f === p || f.startsWith(p.endsWith(sep) ? p : p + sep));
}

// ---- eligibility ---------------------------------------------------------------

/** why the agent's deletion feature can't be used at all, or null */
export function featureReason(info: AgentInfo | null): string | null {
  if (!info) return "Not connected";
  const f = info.features?.delete;
  if (!f) return `Update diskspace on ${info.hostname}`;
  if (!f.available) return f.reason === "no_confirm" ? `diskspace on ${info.hostname} has no terminal to ask you in` : `Deleting isn't available on ${info.hostname}`;
  return null;
}

/** why this node can't be deleted (independent of the queue), or null */
export function nodeReason(st: TreeStore, n: number, info: AgentInfo | null): string | null {
  if (n < 0 || n >= st.n || st.isRemoved(n)) return "Already deleted";
  const k = st.kind[n];
  if (n === st.root) return "This is the scanned folder";
  if (k === KIND_BUCKET) return "Grouped files: show all to pick one";
  if (k === KIND_SYNTH) return "Space outside this scan";
  if (k === KIND_FILE) {
    if (st.fileFlags(n) & FILE_FLAG.BAD_NAME) return "Its name can't be sent safely";
    return null;
  }
  if (k !== KIND_DIR) return "Can't be deleted";
  const cf = st.childFlags(n);
  if (cf & (CHILD_FLAG.OTHER_DEVICE | CHILD_FLAG.BIND_MOUNT | CHILD_FLAG.REPARSE)) return "On another volume";
  if (cf) return "Skipped by the scan";
  const df = st.dirFlags(n);
  if (df & DIR_FLAG.READ_DENIED) return "No access";
  if (!st.hasRecord(n)) return "Not scanned yet";
  if (df & DIR_FLAG.VANISHED) return "Already gone";
  if (info && isProtectedPath(st.fullPath(n), info)) return "Protected folder (its contents can be deleted)";
  return null;
}

/** queued ancestor of n, or -1 */
export function queuedAncestor(st: TreeStore, n: number, queued: ReadonlySet<number>): number {
  for (let k = st.parent[n]; k >= 0; k = st.parent[k]) if (queued.has(k)) return k;
  return -1;
}

// ---- warnings ------------------------------------------------------------------

/** short warnings shown with a queued item */
export function itemWarnings(st: TreeStore, n: number, os: string): string[] {
  const out: string[] = [];
  if (st.kind[n] === KIND_FILE && st.fileFlags(n) & FILE_FLAG.CLOUD) out.push(`${os === "windows" ? "OneDrive" : "iCloud"} file: deleting removes it on all your devices`);
  if (st.kind[n] === KIND_DIR && hasOtherVolume(st, n)) out.push("Contains another volume, which is skipped");
  if (st.size(n) > LARGE_BYTES || st.files(n) > LARGE_FILES) out.push("Large: deleting permanently needs a second confirmation");
  return out;
}

/** a dir somewhere below n is a mount point or another volume (bounded walk) */
function hasOtherVolume(st: TreeStore, n: number, budget = 200_000): boolean {
  const stack = [n];
  let seen = 0;
  while (stack.length && seen < budget) {
    const d = stack.pop()!;
    const ds = st.dStart[d], dc = st.dFilled[d];
    for (let i = 0; i < dc; i++) {
      const c = ds + i;
      seen++;
      if (st.kind[c] !== KIND_DIR) continue;
      if (st.childFlags(c) & (CHILD_FLAG.OTHER_DEVICE | CHILD_FLAG.BIND_MOUNT)) return true;
      if (st.dFilled[c] > 0) stack.push(c);
    }
  }
  return false;
}

// ---- result copy ----------------------------------------------------------------

export function trashName(os: string | undefined): string {
  return os === "windows" ? "Recycle Bin" : "Trash";
}

/** why one item was not deleted */
export function itemCodeText(code: string | undefined, message: string | undefined): string {
  switch (code) {
    case "changed":
      return "Changed since the scan. Scan again.";
    case "denied":
    case "permission":
      return "Permission denied";
    case "mount":
      return "Mount point skipped";
    case "protected":
      return "Protected folder";
    case "not_found":
    case "vanished":
      return "Already gone";
    case "partial":
      return "Partly deleted: some contents were skipped. Scan again.";
    case "canceled":
      return "Not deleted: stopped before this item";
    case "busy":
    case "in_use":
      return "In use";
    default:
      return message || code || "Not deleted";
  }
}

/** why a whole request was refused or failed */
export function requestCodeText(code: string | undefined, message: string | undefined, host: string, trash: string): string {
  switch (code) {
    case "declined":
      return `It was declined on ${host}. Nothing was deleted.`;
    case "timeout":
      return `No answer on ${host} within 2 minutes. Nothing was deleted.`;
    case "canceled":
      return "You canceled the request. Nothing was deleted.";
    case "cooldown":
      return "A request was just declined. Wait a little before asking again.";
    case "disabled":
      return "Deleting is off until diskspace restarts, after 3 declined requests.";
    case "no_confirm":
      return `diskspace on ${host} has no terminal to ask you in.`;
    case "scanning":
      return "Finish or stop the scan first.";
    case "busy":
      return "Another deletion is running.";
    case "trash_unavailable":
      return `The ${trash} isn't available here. You can delete permanently instead.`;
    case "disconnected":
      return "The connection dropped before it was approved. Nothing was deleted.";
    default:
      return message || (code ? `It didn't go through (${code}).` : "It didn't go through.");
  }
}
