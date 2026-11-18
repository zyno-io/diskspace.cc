// SPDX-License-Identifier: MIT
//
// Columnar (SoA) scan tree. One internal index per node; wire dir ids are
// mapped through `wire2idx`. Dirs announced by their parent get a contiguous
// index block, listed files another, the "other files" bucket a single slot.
// A DIR reserves all its child slots up front; DIRMORE names the rest in place.
// Every DIR/FOLD record adds its own bytes to all ancestors (O(depth)), so
// totals are always consistent with what has arrived so far.

import type { ChildRec, Deleted, DirFiles, DirMoreRec, DirRec, FileRec, FoldEndRec, FoldRec, NodeBatch, NodeRecord, ScanStarted } from "../protocol/types";
import { MAX_CHILD_DIRS } from "../protocol/codec";
import { DIR_FLAG, FILE_FLAG } from "../protocol/types";

export const KIND_NONE = 0; // dead slot (replaced file listing)
export const KIND_DIR = 1;
export const KIND_FILE = 2;
export const KIND_BUCKET = 3;
export const KIND_SYNTH = 4;
/** tombstone: a dir or file the agent deleted (unlike KIND_NONE, never a placeholder for apply()) */
export const KIND_DELETED = 5;

export const SYNTH_NAME = "Other volumes, snapshots & unreadable";

/** packed flags: dir flags in bits 0-7, child flags 8-15, file flags 16-23 */
export const CHILD_SHIFT = 8;
export const FILE_SHIFT = 16;
/** set once the dir's own DIR record has been applied */
export const F_RECORD = 1 << 24;
/** a dir the agent only partly deleted: its totals no longer match its children ("scan again") */
export const F_STALE = 1 << 25;

const INITIAL = 1 << 12;

export class TreeStore {
  /** number of allocated node slots */
  n = 0;
  private cap = 0;
  kind = new Uint8Array(0);
  flags = new Uint32Array(0);
  parent = new Int32Array(0);
  depth = new Uint16Array(0);
  wireId = new Int32Array(0);
  ownAlloc = new Float64Array(0);
  ownApparent = new Float64Array(0);
  ownFiles = new Float64Array(0);
  totAlloc = new Float64Array(0);
  totApparent = new Float64Array(0);
  totFiles = new Float64Array(0);
  totDirs = new Float64Array(0);
  listedAlloc = new Float64Array(0);
  listedApparent = new Float64Array(0);
  listedCount = new Float64Array(0);
  unsettled = new Int32Array(0);
  dStart = new Int32Array(0);
  dCount = new Int32Array(0);
  /** child dir slots already named (dCount - dFilled are placeholders) */
  dFilled = new Int32Array(0);
  fStart = new Int32Array(0);
  fCount = new Int32Array(0);
  bucket = new Int32Array(0);
  /**
   * Mutation stamp per node (store.version + 1 at the time of the change). Stamped
   * on insert and on every ancestor walk, so a dir's cached child ordering is
   * stale iff touched[dir] > the version it was sorted at.
   */
  touched = new Int32Array(0);
  names: string[] = [];

  private wire2idx = new Int32Array(0);
  private orphans = new Map<number, NodeRecord[]>();
  /** child slots announced so far (bounded by MAX_CHILD_DIRS) */
  private announced = 0;

  /** bumped on every mutation batch; renderers compare against it */
  version = 0;
  scanId = -1;
  root = -1;
  synth = -1;
  rootPath = "";
  /** dirs whose DIR record says READ_DENIED */
  deniedDirs = 0;
  /** DELETED items already applied (job + item), so a redelivered message is a no-op */
  private tombstones = new Set<string>();

  constructor() {
    this.grow(INITIAL);
  }

  // ---- lifecycle ------------------------------------------------------------

  reset(m: ScanStarted): void {
    this.n = 0;
    this.names.length = 0;
    this.wire2idx.fill(-1);
    this.orphans.clear();
    this.announced = 0;
    this.scanId = m.scanId;
    this.rootPath = m.path;
    this.synth = -1;
    this.deniedDirs = 0;
    this.tombstones.clear();
    const r = this.alloc(1);
    this.initNode(r, KIND_DIR, -1, m.rootName || m.path, 0);
    this.unsettled[r] = 1;
    this.mapWire(0, r);
    this.root = r;
    this.version++;
  }

  /** release memory-heavy arrays (e.g. when leaving the results view) */
  clear(): void {
    this.n = 0;
    this.names.length = 0;
    this.root = -1;
    this.scanId = -1;
    this.orphans.clear();
    this.tombstones.clear();
    this.version++;
  }

  // ---- ingest ---------------------------------------------------------------

  ingest(b: NodeBatch): void {
    this.applySlice(b, 0, Infinity);
  }

  /**
   * Apply records [from, …) of a batch, stopping once performance.now() passes
   * `deadline` (checked every 64 records, so at least 64 are always applied).
   * Returns the index to resume from; === records.length when the batch is done.
   */
  applySlice(b: NodeBatch, from: number, deadline: number, now: () => number = () => performance.now()): number {
    const recs = b.records;
    if (b.scanId !== this.scanId || this.root < 0) return recs.length;
    let i = from;
    let c = 0;
    while (i < recs.length) {
      this.apply(recs[i++]);
      if ((++c & 63) === 0 && i < recs.length && now() >= deadline) break;
    }
    this.version++;
    return i;
  }

  private apply(r: NodeRecord): void {
    const wid = r.k === 1 || r.k === 4 ? r.id : r.targetId;
    const idx = wid < this.wire2idx.length ? this.wire2idx[wid] : -1;
    if (r.k === 4 && idx >= 0 && this.kind[idx] !== KIND_NONE) {
      // continuation of a dir whose DIR was applied; mismatches are dropped there
      this.applyDirMore(idx, r);
      return;
    }
    // unknown id, or a child slot announced by its parent's DIR but not yet named by a DIRMORE
    if (idx < 0 || this.kind[idx] === KIND_NONE) {
      const q = this.orphans.get(wid);
      if (q) q.push(r);
      else this.orphans.set(wid, [r]);
      return;
    }
    if (r.k === 1) this.applyDir(idx, r);
    else if (r.k === 2) this.applyFold(idx, r);
    else if (r.k === 3) this.applyFoldEnd(idx, r);
  }

  private applyDir(idx: number, r: DirRec): void {
    if (this.flags[idx] & F_RECORD) return; // duplicate (replay overlap)
    const total = r.totalDirs, nd = r.dirs.length;
    // Ids are sequential below the agent's per-scan cap; refuse records that would
    // make the store allocate more (hostile agent) or index the id map absurdly.
    if (nd > total || this.announced + total > MAX_CHILD_DIRS || (total > 0 && r.firstChildId + total > MAX_CHILD_DIRS + 1)) return;
    this.flags[idx] |= F_RECORD | (r.flags & 0xff);
    if (r.flags & DIR_FLAG.READ_DENIED) this.deniedDirs++;

    // own bytes → self + ancestors
    this.ownAlloc[idx] += r.ownAlloc;
    this.ownApparent[idx] += r.ownApparent;
    this.ownFiles[idx] += r.ownFiles;
    this.addUp(idx, r.ownAlloc, r.ownApparent, r.ownFiles, 0);

    const d = this.depth[idx] + 1;
    let pending = 0;
    if (total > 0) {
      // Reserve every child slot once (stable indices). Slots past the inline
      // chunk are KIND_NONE placeholders until a DIRMORE names them; each counts
      // as an unsettled child so the dir cannot settle early.
      this.announced += total;
      const s = this.alloc(total);
      this.dStart[idx] = s;
      this.dCount[idx] = total;
      this.dFilled[idx] = nd;
      for (let i = 0; i < total; i++) {
        this.initNode(s + i, KIND_NONE, idx, "", d);
        this.mapWire(r.firstChildId + i, s + i);
      }
      const real = this.fillChildren(idx, s, r.dirs, r.firstChildId, d);
      pending += real + (total - nd);
      if (real) this.addUp(idx, 0, 0, 0, real);
    }
    this.setFiles(idx, r.files, d);
    if (r.flags & DIR_FLAG.FOLDED_CHILDREN) pending++;

    this.unsettled[idx] += pending - 1;
    if (this.unsettled[idx] === 0) this.settle(idx);

    if (nd > 0) this.replayOrphans(r.firstChildId, nd);
  }

  /** name child slots [s, s+dirs.length); returns how many await their own record */
  private fillChildren(idx: number, s: number, dirs: ChildRec[], firstChildId: number, d: number): number {
    let real = 0;
    for (let i = 0; i < dirs.length; i++) {
      const c = dirs[i];
      const ci = s + i;
      this.initNode(ci, KIND_DIR, idx, c.name, d);
      this.flags[ci] = (c.flags & 0xff) << CHILD_SHIFT;
      if (c.flags & ~FILE_FLAG.BAD_NAME) {
        // flagged children (other device, excluded, …) never get a record;
        // BAD_NAME alone is only a name annotation (set by the decoder)
        this.flags[ci] |= F_RECORD;
      } else {
        this.unsettled[ci] = 1;
        real++;
      }
      this.mapWire(firstChildId + i, ci);
    }
    return real;
  }

  private replayOrphans(first: number, n: number): void {
    if (this.orphans.size === 0) return;
    for (let i = 0; i < n; i++) {
      const q = this.orphans.get(first + i);
      if (q) {
        this.orphans.delete(first + i);
        for (const o of q) this.apply(o);
      }
    }
  }

  /**
   * DIRMORE: name the next child slots of an already-recorded dir. A chunk that
   * does not start at the next unfilled slot, overflows the announced total, or
   * targets a dir without a DIR record is dropped (not buffered).
   */
  private applyDirMore(idx: number, r: DirMoreRec): void {
    const total = this.dCount[idx], filled = this.dFilled[idx], nd = r.dirs.length;
    if (!(this.flags[idx] & F_RECORD) || nd === 0 || filled + nd > total) return;
    const ds = this.dStart[idx];
    if (r.firstChildId !== this.wireId[ds] + filled) return;
    const real = this.fillChildren(idx, ds + filled, r.dirs, r.firstChildId, this.depth[idx] + 1);
    this.dFilled[idx] = filled + nd;
    // each filled slot stops being a placeholder: a real child keeps the pending
    // count (now its own), a flagged one resolves it
    this.unsettled[idx] -= nd - real;
    this.touched[idx] = this.version + 1;
    if (real) this.addUp(idx, 0, 0, 0, real);
    if (nd - real > 0 && this.unsettled[idx] === 0) this.settle(idx);
    this.replayOrphans(r.firstChildId, nd);
  }

  private applyFold(idx: number, r: FoldRec): void {
    this.ownAlloc[idx] += r.alloc;
    this.ownApparent[idx] += r.apparent;
    this.ownFiles[idx] += r.files;
    this.addUp(idx, r.alloc, r.apparent, r.files, r.dirs);
    this.ensureBucket(idx);
  }

  private applyFoldEnd(idx: number, _r: FoldEndRec): void {
    if (!(this.flags[idx] & DIR_FLAG.FOLDED_CHILDREN)) return;
    if (this.unsettled[idx] > 0 && --this.unsettled[idx] === 0) {
      this.touched[idx] = this.version + 1;
      this.settle(idx);
    }
  }

  /** replace the listed files of a dir with a full DIR_FILES listing */
  applyDirFiles(df: DirFiles): boolean {
    if (df.scanId !== this.scanId) return false;
    const idx = df.dirId < this.wire2idx.length ? this.wire2idx[df.dirId] : -1;
    if (idx < 0) return false;
    this.touched[idx] = this.version + 1;
    const fs = this.fStart[idx];
    for (let i = 0; i < this.fCount[idx]; i++) this.kind[fs + i] = KIND_NONE;
    this.listedAlloc[idx] = 0;
    this.listedApparent[idx] = 0;
    this.listedCount[idx] = 0;
    this.setFiles(idx, df.files, this.depth[idx] + 1);
    this.version++;
    return true;
  }

  private setFiles(idx: number, files: FileRec[], d: number): void {
    const nf = files.length;
    if (nf > 0) {
      const s = this.alloc(nf);
      this.fStart[idx] = s;
      this.fCount[idx] = nf;
      let la = 0, lp = 0;
      for (let i = 0; i < nf; i++) {
        const f = files[i];
        const fi = s + i;
        this.initNode(fi, KIND_FILE, idx, f.name, d);
        this.flags[fi] = (f.flags & 0xff) << FILE_SHIFT;
        this.ownAlloc[fi] = f.alloc;
        this.ownApparent[fi] = f.apparent;
        this.totAlloc[fi] = f.alloc;
        this.totApparent[fi] = f.apparent;
        this.ownFiles[fi] = 1;
        this.totFiles[fi] = 1;
        la += f.alloc;
        lp += f.apparent;
      }
      this.listedAlloc[idx] += la;
      this.listedApparent[idx] += lp;
      this.listedCount[idx] += nf;
    } else {
      this.fCount[idx] = 0;
    }
    this.ensureBucket(idx);
  }

  private ensureBucket(idx: number): void {
    if (this.bucket[idx] >= 0) return;
    if (this.ownFiles[idx] - this.listedCount[idx] <= 0 && this.ownAlloc[idx] - this.listedAlloc[idx] <= 0) return;
    const b = this.alloc(1);
    this.initNode(b, KIND_BUCKET, idx, "", this.depth[idx] + 1);
    this.bucket[idx] = b;
  }

  private addUp(idx: number, a: number, p: number, f: number, dirs: number): void {
    const stamp = this.version + 1;
    for (let i = idx; i >= 0; i = this.parent[i]) {
      this.touched[i] = stamp;
      this.totAlloc[i] += a;
      this.totApparent[i] += p;
      this.totFiles[i] += f;
      this.totDirs[i] += dirs;
    }
  }

  private settle(idx: number): void {
    for (let p = this.parent[idx]; p >= 0; p = this.parent[p]) {
      if (--this.unsettled[p] !== 0) break;
      this.touched[p] = this.version + 1; // row shows a "pending" mark until settled
    }
  }

  // ---- deletions ------------------------------------------------------------

  /**
   * Apply a DELETED message. Whole dirs and files become tombstones and their
   * totals leave every ancestor; a partly deleted dir loses the agent's freed
   * amounts (clamped) and is marked F_STALE. Idempotent per job and item.
   * Returns true if anything changed.
   */
  applyDeleted(m: Deleted): boolean {
    if (m.scanId !== this.scanId || this.root < 0) return false;
    let changed = false;
    for (const it of m.items) {
      const key = "dir" in it ? `${m.job}|d:${it.dir}` : `${m.job}|f:${it.parent}:${it.name}`;
      if (this.tombstones.has(key)) continue;
      this.tombstones.add(key);
      const ok =
        "dir" in it
          ? it.partial
            ? this.removePartial(it.dir, it.freedAlloc, it.freedApparent, it.files, it.dirs)
            : this.removeDir(it.dir)
          : this.removeFile(it.parent, it.name, it.freedAlloc, it.freedApparent);
      changed ||= ok;
    }
    if (changed) this.version++;
    return changed;
  }

  /** internal index of a wire dir id, or -1 */
  nodeOfWire(w: number): number {
    return w >= 0 && w < this.wire2idx.length ? this.wire2idx[w] : -1;
  }

  /** a live listed file of dir `p` with this name, or -1 */
  findFile(p: number, name: string): number {
    const fs = this.fStart[p], fc = this.fCount[p];
    for (let k = 0; k < fc; k++) if (this.kind[fs + k] === KIND_FILE && this.names[fs + k] === name) return fs + k;
    return -1;
  }

  /** the node or one of its ancestors was deleted */
  isRemoved(i: number): boolean {
    for (let k = i; k >= 0; k = this.parent[k]) if (this.kind[k] === KIND_DELETED) return true;
    return false;
  }

  /** nearest node on i's root path that is not deleted (i itself when it is live) */
  survivor(i: number): number {
    if (i < 0 || i >= this.n) return this.root;
    let s = i;
    for (let k = i; k >= 0; k = this.parent[k]) if (this.kind[k] === KIND_DELETED) s = this.parent[k];
    return s < 0 ? this.root : s;
  }

  isStale(i: number): boolean {
    return (this.flags[i] & F_STALE) !== 0;
  }

  /** a live scanned dir by wire id (not the root's own deletion target check) */
  private liveDir(wire: number): number {
    const idx = this.nodeOfWire(wire);
    if (idx < 0 || this.kind[idx] !== KIND_DIR || this.isRemoved(idx)) return -1;
    return idx;
  }

  private removeDir(wire: number): boolean {
    const idx = this.liveDir(wire);
    if (idx < 0 || idx === this.root) return false;
    const p = this.parent[idx];
    this.addUp(p, -this.totAlloc[idx], -this.totApparent[idx], -this.totFiles[idx], -(this.totDirs[idx] + 1));
    if (this.unsettled[idx] > 0) {
      // a stopped scan can leave it unsettled; it no longer holds its parent back
      this.unsettled[idx] = 0;
      this.settle(idx);
    }
    this.kind[idx] = KIND_DELETED;
    this.touched[idx] = this.version + 1;
    return true;
  }

  private removePartial(wire: number, alloc: number, apparent: number, files: number, dirs: number): boolean {
    const idx = this.liveDir(wire);
    if (idx < 0) return false;
    const a = clamp(alloc, this.totAlloc[idx]);
    const p = clamp(apparent, this.totApparent[idx]);
    const f = clamp(files, this.totFiles[idx]);
    const d = clamp(dirs, this.totDirs[idx]);
    this.addUp(idx, -a, -p, -f, -d);
    this.flags[idx] |= F_STALE;
    return true;
  }

  private removeFile(parentWire: number, name: string, alloc: number, apparent: number): boolean {
    const p = this.liveDir(parentWire);
    if (p < 0) return false;
    const fi = this.findFile(p, name);
    let a: number, ap: number;
    if (fi >= 0) {
      // a listed file: its own recorded size leaves the parent (keeps the bucket unchanged)
      a = this.ownAlloc[fi];
      ap = this.ownApparent[fi];
      this.listedAlloc[p] -= a;
      this.listedApparent[p] -= ap;
      this.listedCount[p] -= 1;
      this.kind[fi] = KIND_DELETED;
      this.touched[fi] = this.version + 1;
    } else {
      // not listed here (e.g. replay without the "show all" listing): shrink the bucket
      a = clamp(alloc, this.ownAlloc[p] - this.listedAlloc[p]);
      ap = clamp(apparent, this.ownApparent[p] - this.listedApparent[p]);
      if (this.ownFiles[p] - this.listedCount[p] < 1) return false;
    }
    this.ownAlloc[p] -= a;
    this.ownApparent[p] -= ap;
    this.ownFiles[p] -= 1;
    this.addUp(p, -a, -ap, -1, 0);
    return true;
  }

  // ---- synthetic remainder ------------------------------------------------

  /** "Other volumes, snapshots & unreadable" (volume-root scans only) */
  setSynthetic(bytes: number): void {
    if (this.root < 0) return;
    if (bytes <= 0) {
      if (this.synth >= 0) {
        this.totAlloc[this.synth] = this.ownAlloc[this.synth] = 0;
        this.touched[this.root] = this.touched[this.synth] = this.version + 1;
      }
      return;
    }
    if (this.synth < 0) {
      this.synth = this.alloc(1);
      this.initNode(this.synth, KIND_SYNTH, this.root, SYNTH_NAME, 1);
      this.flags[this.synth] = F_RECORD;
    }
    this.ownAlloc[this.synth] = this.totAlloc[this.synth] = bytes;
    this.ownApparent[this.synth] = this.totApparent[this.synth] = bytes;
    this.touched[this.root] = this.touched[this.synth] = this.version + 1;
    this.version++;
  }

  // ---- queries --------------------------------------------------------------

  isSettled(i: number): boolean {
    return this.unsettled[i] === 0;
  }
  rootSettled(): boolean {
    return this.root >= 0 && this.unsettled[this.root] === 0;
  }
  orphanCount(): number {
    return this.orphans.size;
  }

  /** allocated size used for layout + sorting */
  size(i: number): number {
    const k = this.kind[i];
    if (k === KIND_BUCKET) {
      const p = this.parent[i];
      const v = this.ownAlloc[p] - this.listedAlloc[p];
      return v > 0 ? v : 0;
    }
    if (k === KIND_NONE || k === KIND_DELETED) return 0;
    return this.totAlloc[i];
  }
  apparent(i: number): number {
    if (this.kind[i] === KIND_BUCKET) {
      const p = this.parent[i];
      const v = this.ownApparent[p] - this.listedApparent[p];
      return v > 0 ? v : 0;
    }
    if (this.kind[i] === KIND_DELETED) return 0;
    return this.totApparent[i];
  }
  files(i: number): number {
    if (this.kind[i] === KIND_BUCKET) {
      const p = this.parent[i];
      return Math.max(0, this.ownFiles[p] - this.listedCount[p]);
    }
    return this.totFiles[i];
  }
  dirs(i: number): number {
    return this.totDirs[i];
  }
  name(i: number): string {
    return this.names[i] ?? "";
  }
  isDir(i: number): boolean {
    return this.kind[i] === KIND_DIR;
  }
  dirFlags(i: number): number {
    return this.flags[i] & 0xff;
  }
  childFlags(i: number): number {
    return (this.flags[i] >>> CHILD_SHIFT) & 0xff;
  }
  fileFlags(i: number): number {
    return (this.flags[i] >>> FILE_SHIFT) & 0xff;
  }
  hasRecord(i: number): boolean {
    return (this.flags[i] & F_RECORD) !== 0;
  }
  /** files hidden in the bucket that "Show all files" could reveal */
  hiddenFiles(i: number): number {
    return Math.max(0, this.ownFiles[i] - this.listedCount[i]);
  }

  /** fill `out` with live children of `i` (dirs, files, bucket, synth); returns count */
  childrenOf(i: number, out: number[]): number {
    out.length = 0;
    const ds = this.dStart[i], dc = this.dCount[i];
    for (let k = 0; k < dc; k++) if (this.kind[ds + k] === KIND_DIR) out.push(ds + k);
    const fs = this.fStart[i], fc = this.fCount[i];
    for (let k = 0; k < fc; k++) if (this.kind[fs + k] === KIND_FILE) out.push(fs + k);
    // a bucket with no files is just the dir's own blocks; not worth a row
    if (this.bucket[i] >= 0 && this.files(this.bucket[i]) > 0) out.push(this.bucket[i]);
    if (i === this.root && this.synth >= 0 && this.totAlloc[this.synth] > 0) out.push(this.synth);
    return out.length;
  }

  hasChildren(i: number): boolean {
    return this.dFilled[i] > 0 || this.fCount[i] > 0 || (this.bucket[i] >= 0 && this.files(this.bucket[i]) > 0) || (i === this.root && this.synth >= 0);
  }

  /** children sorted by allocated size desc (fresh array) */
  sortedChildren(i: number): number[] {
    const out: number[] = [];
    this.childrenOf(i, out);
    const sz = new Float64Array(this.n > 0 ? out.length : 0);
    for (let k = 0; k < out.length; k++) sz[k] = this.size(out[k]);
    const order = out.map((_, k) => k);
    order.sort((a, b) => sz[b] - sz[a] || a - b);
    return order.map((k) => out[k]);
  }

  /** root → i path of node indices */
  pathTo(i: number): number[] {
    const p: number[] = [];
    for (let k = i; k >= 0; k = this.parent[k]) p.push(k);
    return p.reverse();
  }

  fullPath(i: number): string {
    const parts = this.pathTo(i).slice(1).map((k) => this.names[k]);
    const base = this.rootPath.replace(/[\\/]+$/, "");
    const sep = this.rootPath.includes("\\") ? "\\" : "/";
    return parts.length ? base + sep + parts.join(sep) : this.rootPath;
  }

  isAncestor(a: number, b: number): boolean {
    for (let k = b; k >= 0; k = this.parent[k]) if (k === a) return true;
    return false;
  }

  /** wire dir id for DIR_FILES requests */
  wire(i: number): number {
    return this.wireId[i];
  }

  // ---- storage --------------------------------------------------------------

  private initNode(i: number, kind: number, parent: number, name: string, depth: number): void {
    this.kind[i] = kind;
    this.flags[i] = 0;
    this.parent[i] = parent;
    this.depth[i] = depth;
    this.wireId[i] = -1;
    this.ownAlloc[i] = this.ownApparent[i] = this.ownFiles[i] = 0;
    this.totAlloc[i] = this.totApparent[i] = this.totFiles[i] = this.totDirs[i] = 0;
    this.listedAlloc[i] = this.listedApparent[i] = this.listedCount[i] = 0;
    this.unsettled[i] = 0;
    this.dStart[i] = this.dCount[i] = this.dFilled[i] = this.fStart[i] = this.fCount[i] = 0;
    this.bucket[i] = -1;
    this.touched[i] = this.version + 1;
    if (parent >= 0) this.touched[parent] = this.version + 1;
    this.names[i] = name;
  }

  private mapWire(w: number, idx: number): void {
    if (w >= this.wire2idx.length) {
      let c = this.wire2idx.length || INITIAL;
      while (c <= w) c *= 2;
      const nw = new Int32Array(c).fill(-1);
      nw.set(this.wire2idx);
      this.wire2idx = nw;
    }
    this.wire2idx[w] = idx;
    this.wireId[idx] = w;
  }

  private alloc(k: number): number {
    const s = this.n;
    if (s + k > this.cap) {
      let c = this.cap;
      while (c < s + k) c = Math.ceil(c * 1.5);
      this.grow(c);
    }
    this.n += k;
    return s;
  }

  private grow(c: number): void {
    const g = <T extends Uint8Array | Uint16Array | Uint32Array | Int32Array | Float64Array>(a: T): T => {
      const b = new (a.constructor as { new (n: number): T })(c);
      b.set(a);
      return b;
    };
    this.kind = g(this.kind);
    this.flags = g(this.flags);
    this.parent = g(this.parent);
    this.depth = g(this.depth);
    this.wireId = g(this.wireId);
    this.ownAlloc = g(this.ownAlloc);
    this.ownApparent = g(this.ownApparent);
    this.ownFiles = g(this.ownFiles);
    this.totAlloc = g(this.totAlloc);
    this.totApparent = g(this.totApparent);
    this.totFiles = g(this.totFiles);
    this.totDirs = g(this.totDirs);
    this.listedAlloc = g(this.listedAlloc);
    this.listedApparent = g(this.listedApparent);
    this.listedCount = g(this.listedCount);
    this.unsettled = g(this.unsettled);
    this.dStart = g(this.dStart);
    this.dCount = g(this.dCount);
    this.dFilled = g(this.dFilled);
    this.fStart = g(this.fStart);
    this.fCount = g(this.fCount);
    this.bucket = g(this.bucket);
    this.touched = g(this.touched);
    this.cap = c;
  }
}

/** 0 ≤ v ≤ max (max < 0 counts as 0) */
function clamp(v: number, max: number): number {
  return v > 0 ? Math.min(v, Math.max(0, max)) : 0;
}
