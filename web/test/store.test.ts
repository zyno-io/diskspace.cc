// SPDX-License-Identifier: MIT
import { describe, expect, test } from "vitest";
import { generateTree } from "../src/protocol/mock";
import { CHILD_FLAG, DIR_FLAG, type DirRec, type NodeRecord, type ScanStarted } from "../src/protocol/types";
import { KIND_BUCKET, KIND_DIR, KIND_FILE, KIND_SYNTH, TreeStore } from "../src/store/tree";
import { formatBytes, formatPercent } from "../src/store/format";

const started = (scanId = 1): ScanStarted => ({ scanId, path: "/Users/alex", rootName: "alex", volume: null, opts: {} });
const dir = (id: number, p: Partial<DirRec> = {}): DirRec => ({ k: 1, id, flags: 0, ownAlloc: 0, ownApparent: 0, ownFiles: 0, entryErrors: 0, firstChildId: 0, dirs: [], files: [], ...p, totalDirs: p.totalDirs ?? p.dirs?.length ?? 0 });
const feed = (s: TreeStore, records: NodeRecord[], scanId = 1) => s.ingest({ scanId, records, wireBytes: 0 });
const byName = (s: TreeStore, parent: number, name: string) => {
  const out: number[] = [];
  s.childrenOf(parent, out);
  return out.find((c) => s.names[c] === name) ?? -1;
};

describe("TreeStore", () => {
  test("aggregates own bytes up the tree and tracks settling", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [dir(0, { ownAlloc: 4096, ownApparent: 100, ownFiles: 1, firstChildId: 1, dirs: [{ name: "a", flags: 0 }, { name: "b", flags: 0 }] })]);
    expect(s.rootSettled()).toBe(false);
    expect(s.dirs(0)).toBe(2);
    const a = byName(s, 0, "a");
    feed(s, [dir(1, { ownAlloc: 10_000, ownApparent: 9000, ownFiles: 3, firstChildId: 3, dirs: [{ name: "deep", flags: 0 }] })]);
    feed(s, [dir(3, { ownAlloc: 1_000_000, ownApparent: 999_000, ownFiles: 10 })]);
    expect(s.size(a)).toBe(1_010_000);
    expect(s.size(0)).toBe(1_014_096);
    expect(s.files(0)).toBe(14);
    expect(s.isSettled(a)).toBe(true);
    expect(s.rootSettled()).toBe(false);
    feed(s, [dir(2, { ownAlloc: 5, ownApparent: 5 })]);
    expect(s.rootSettled()).toBe(true);
    expect(s.orphanCount()).toBe(0);
  });

  test("buckets = own − Σ listed, files are leaves", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [dir(0, { ownAlloc: 10 * 1048576, ownApparent: 10 * 1048576, ownFiles: 5, files: [{ name: "big.mov", alloc: 6 * 1048576, apparent: 6 * 1048576, flags: 0 }] })]);
    const kids: number[] = [];
    s.childrenOf(0, kids);
    const file = kids.find((k) => s.kind[k] === KIND_FILE)!;
    const bucket = kids.find((k) => s.kind[k] === KIND_BUCKET)!;
    expect(s.size(file)).toBe(6 * 1048576);
    expect(s.size(bucket)).toBe(4 * 1048576);
    expect(s.files(bucket)).toBe(4);
    expect(s.hiddenFiles(0)).toBe(4);
  });

  const more = (id: number, firstChildId: number, names: string[], flags = 0): NodeRecord => ({ k: 4, id, firstChildId, dirs: names.map((name) => ({ name, flags })) });
  const kidsOf = (s: TreeStore, n: number) => {
    const out: number[] = [];
    s.childrenOf(n, out);
    return out;
  };

  test("a child's DIR and DIRMORE arriving before the DIRMORE that names it are buffered", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [
      dir(0, { totalDirs: 2, firstChildId: 1, dirs: [{ name: "a", flags: 0 }] }),
      dir(2, { totalDirs: 2, firstChildId: 3, dirs: [{ name: "c0", flags: 0 }], ownAlloc: 1 }),
      more(2, 4, ["c1"]),
      more(0, 2, ["b"]),
      dir(1, { ownAlloc: 2 }),
      dir(3, { ownAlloc: 3 }),
      dir(4, { ownAlloc: 3 }),
    ]);
    expect(s.orphanCount()).toBe(0);
    expect(kidsOf(s, byName(s, 0, "b")).map((k) => s.names[k])).toEqual(["c0", "c1"]);
    expect(s.size(0)).toBe(9);
    expect(s.rootSettled()).toBe(true);
  });

  test("DIRMORE fills pre-reserved slots in place (stable indices, placeholders hidden)", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [dir(0, { totalDirs: 4, firstChildId: 1, dirs: [{ name: "a", flags: 0 }, { name: "x", flags: CHILD_FLAG.EXCLUDED }] })]);
    const a = byName(s, 0, "a");
    expect(kidsOf(s, 0).length).toBe(2); // placeholders not listed
    feed(s, [dir(1, { ownAlloc: 100, firstChildId: 5, dirs: [{ name: "deep", flags: 0 }] })]);
    const n = s.n;
    feed(s, [more(0, 3, ["b", "c"])]);
    expect(s.n).toBe(n); // no new slots, nothing moved
    expect(byName(s, 0, "a")).toBe(a);
    expect(kidsOf(s, 0).map((k) => s.names[k])).toEqual(["a", "x", "b", "c"]);
    for (const k of kidsOf(s, 0)) expect(s.parent[k]).toBe(0);
    expect(s.dirs(0)).toBe(4); // a, b, c + deep
    feed(s, [dir(5, { ownAlloc: 9 })]);
    expect(s.size(a)).toBe(109);
    expect(s.size(0)).toBe(109);
  });

  test("placeholders keep a dir unsettled until filled; flagged fills settle", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [dir(0, { totalDirs: 3, firstChildId: 1, dirs: [{ name: "x", flags: CHILD_FLAG.EXCLUDED }] })]);
    expect(s.rootSettled()).toBe(false);
    feed(s, [more(0, 2, ["y"], CHILD_FLAG.REPARSE)]);
    expect(s.rootSettled()).toBe(false);
    feed(s, [more(0, 3, ["a"])]);
    expect(s.rootSettled()).toBe(false);
    feed(s, [dir(3, { ownAlloc: 7 })]);
    expect(s.rootSettled()).toBe(true);
    expect(s.size(0)).toBe(7);
  });

  test("mismatched DIRMORE for a recorded dir is dropped; one for an unknown id is buffered", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [more(42, 1, ["nodir"])]); // unknown id: buffered until named
    feed(s, [dir(0, { totalDirs: 3, firstChildId: 1, dirs: [{ name: "a", flags: 0 }] })]);
    feed(s, [more(0, 9, ["gap"])]); // wrong firstChildId
    feed(s, [more(0, 1, ["dup"])]); // overlaps filled slot
    feed(s, [more(0, 2, ["b", "c", "d"])]); // overflows total
    feed(s, [more(1, 2, ["childOfUnrecorded"])]); // dir 1 has no DIR yet
    expect(s.orphanCount()).toBe(1); // only id 42
    expect(kidsOf(s, 0).map((k) => s.names[k])).toEqual(["a"]);
    feed(s, [more(0, 2, ["b", "c"])]);
    expect(kidsOf(s, 0).map((k) => s.names[k])).toEqual(["a", "b", "c"]);
  });

  test("a child's DIR arriving before its DIRMORE names it is held and replayed", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [dir(0, { totalDirs: 3, firstChildId: 1, dirs: [{ name: "a", flags: 0 }] })]);
    feed(s, [dir(3, { ownAlloc: 50 }), { k: 2, targetId: 3, alloc: 5, apparent: 5, files: 1, dirs: 0 }]);
    expect(s.orphanCount()).toBe(1);
    expect(s.size(0)).toBe(0);
    feed(s, [more(0, 2, ["b", "c"])]);
    expect(s.orphanCount()).toBe(0);
    expect(s.size(0)).toBe(55);
    feed(s, [dir(1), dir(2)]);
    expect(s.rootSettled()).toBe(true);
  });

  test("BAD_NAME alone does not make a child flagged", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [dir(0, { totalDirs: 2, firstChildId: 1, dirs: [{ name: "bad\ufffd", flags: 32 }, { name: "bad2", flags: 32 | CHILD_FLAG.EXCLUDED }] })]);
    feed(s, [dir(1, { ownAlloc: 10, firstChildId: 3, dirs: [{ name: "sub", flags: 0 }] })]);
    feed(s, [dir(3, { ownAlloc: 5 })]);
    expect(s.orphanCount()).toBe(0);
    expect(s.size(0)).toBe(15);
    expect(s.rootSettled()).toBe(true);
  });

  test("hostile: 12k tiny DIRMOREs alternating between two dirs stay linear; arena bounded by announced totals", () => {
    const s = new TreeStore();
    s.reset(started());
    const N = 12_000;
    feed(s, [dir(0, { totalDirs: 2, firstChildId: 1, dirs: [{ name: "p", flags: 0 }, { name: "q", flags: 0 }] })]);
    const first = 3;
    feed(s, [dir(1, { totalDirs: N, firstChildId: first, dirs: [] }), dir(2, { totalDirs: N, firstChildId: first + N, dirs: [] })]);
    const n = s.n;
    const t0 = performance.now();
    const recs: NodeRecord[] = [];
    for (let i = 0; i < N; i++) {
      recs.push(more(1, first + i, [`a${i}`]));
      recs.push(more(2, first + N + i, [`b${i}`]));
    }
    feed(s, recs);
    expect(performance.now() - t0).toBeLessThan(2000);
    expect(s.n).toBe(n);
    expect(kidsOf(s, 1).length).toBe(N);
    expect(kidsOf(s, 2).length).toBe(N);
  });

  test("absurd child totals are refused", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [dir(0, { totalDirs: 3_000_000, firstChildId: 1, dirs: [] })]); // over per-scan cap
    expect(s.n).toBe(1);
    feed(s, [dir(0, { totalDirs: 2, firstChildId: 2 ** 40, dirs: [] })]); // absurd wire id
    expect(s.n).toBe(1);
    feed(s, [dir(0, { totalDirs: 1, firstChildId: 1, dirs: [{ name: "a", flags: 0 }, { name: "b", flags: 0 }] })]); // inline > total
    expect(s.n).toBe(1);
  });

  test("buffers orphan records until the parent announces them", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [dir(5, { ownAlloc: 700 }), { k: 2, targetId: 5, alloc: 300, apparent: 300, files: 2, dirs: 1 }]);
    expect(s.orphanCount()).toBe(1);
    feed(s, [dir(0, { firstChildId: 5, dirs: [{ name: "late", flags: 0 }] })]);
    expect(s.orphanCount()).toBe(0);
    expect(s.size(0)).toBe(1000);
    expect(s.rootSettled()).toBe(true);
  });

  test("FOLDED_CHILDREN keeps a dir unsettled until FOLDEND", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [dir(0, { flags: DIR_FLAG.FOLDED_CHILDREN, ownAlloc: 1 })]);
    expect(s.rootSettled()).toBe(false);
    feed(s, [{ k: 2, targetId: 0, alloc: 99, apparent: 90, files: 9, dirs: 3 }]);
    expect(s.size(0)).toBe(100);
    expect(s.dirs(0)).toBe(3);
    feed(s, [{ k: 3, targetId: 0 }]);
    expect(s.rootSettled()).toBe(true);
  });

  test("flagged children are settled, zero-size and never block the root", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [dir(0, { firstChildId: 1, dirs: [{ name: "Volumes", flags: CHILD_FLAG.OTHER_DEVICE }] })]);
    expect(s.rootSettled()).toBe(true);
    const v = byName(s, 0, "Volumes");
    expect(s.kind[v]).toBe(KIND_DIR);
    expect(s.childFlags(v)).toBe(CHILD_FLAG.OTHER_DEVICE);
  });

  test("ignores other scans, duplicates, and resets on scanStarted", () => {
    const s = new TreeStore();
    s.reset(started(1));
    feed(s, [dir(0, { ownAlloc: 10 })]);
    feed(s, [dir(0, { ownAlloc: 10 })]); // replay overlap
    feed(s, [dir(0, { ownAlloc: 999 })], 2); // stale scan id
    expect(s.size(0)).toBe(10);
    s.reset(started(2));
    expect(s.size(0)).toBe(0);
    expect(s.n).toBe(1);
  });

  test("dirFiles replaces listed files", () => {
    const s = new TreeStore();
    s.reset(started());
    feed(s, [dir(0, { ownAlloc: 100, ownFiles: 4, files: [{ name: "x", alloc: 40, apparent: 40, flags: 0 }] })]);
    s.applyDirFiles({ reqId: 1, scanId: 1, dirId: 0, flags: 0, restCount: 1, restAlloc: 10, restApparent: 10, files: [
      { name: "x", alloc: 40, apparent: 40, flags: 0 },
      { name: "y", alloc: 30, apparent: 30, flags: 0 },
      { name: "z", alloc: 20, apparent: 20, flags: 0 },
    ] });
    const kids: number[] = [];
    s.childrenOf(0, kids);
    expect(kids.filter((k) => s.kind[k] === KIND_FILE).map((k) => s.names[k])).toEqual(["x", "y", "z"]);
    const bucket = kids.find((k) => s.kind[k] === KIND_BUCKET)!;
    expect(s.size(bucket)).toBe(10);
    expect(s.files(bucket)).toBe(1);
  });

  test("synthetic remainder is a child of the root, not part of its total", () => {
    const s = new TreeStore();
    s.reset({ ...started(), path: "/", rootName: "/" });
    feed(s, [dir(0, { ownAlloc: 1000 })]);
    s.setSynthetic(500);
    const kids: number[] = [];
    s.childrenOf(0, kids);
    expect(kids.some((k) => s.kind[k] === KIND_SYNTH && s.size(k) === 500)).toBe(true);
    expect(s.size(0)).toBe(1000);
  });

  test("mock stream respects protocol invariants and settles exactly", () => {
    const g = generateTree(6000, { root: "root" });
    const s = new TreeStore();
    s.reset({ ...started(), path: "/", rootName: "/" });
    const seen = new Set<number>([0]);
    let more = 0;
    for (const r of g.records) {
      if (r.k === 4) {
        more++;
        r.dirs.forEach((_, i) => seen.add(r.firstChildId + i));
      } else if (r.k === 1) {
        expect(seen.has(r.id)).toBe(true); // announced before its own record
        r.dirs.forEach((_, i) => seen.add(r.firstChildId + i));
        expect(r.files.length).toBeLessThanOrEqual(8);
        for (const f of r.files) expect(f.alloc).toBeGreaterThanOrEqual(1048576);
      }
    }
    expect(more).toBeGreaterThan(0);
    feed(s, g.records);
    expect(s.orphanCount()).toBe(0);
    expect(s.rootSettled()).toBe(true);
    expect(s.size(0)).toBe(g.totals.alloc);
    expect(s.files(0)).toBe(g.totals.files);
    expect(s.deniedDirs).toBe(g.denied);
  });

  test("ingests a 200k-dir batch quickly (loose bound)", () => {
    // synthetic wide/deep tree: each dir announces up to 4 children
    const N = 200_000;
    const records: DirRec[] = [];
    let next = 1;
    for (let id = 0; id < N; id++) {
      const k = next < N ? Math.min(4, N - next) : 0;
      const dirs = Array.from({ length: k }, (_, i) => ({ name: `d${next + i}`, flags: 0 }));
      records.push(dir(id, { ownAlloc: 4096 + (id % 97) * 8192, ownApparent: 4000, ownFiles: id % 7, firstChildId: k ? next : 0, dirs, files: id % 50 === 0 ? [{ name: "big.bin", alloc: 2 << 20, apparent: 2 << 20, flags: 0 }] : [] }));
      next += k;
    }
    const s = new TreeStore();
    s.reset(started());
    const t0 = performance.now();
    for (let i = 0; i < records.length; i += 2000) feed(s, records.slice(i, i + 2000));
    const ms = performance.now() - t0;
    expect(s.rootSettled()).toBe(true);
    expect(s.dirs(0)).toBe(N - 1);
    // target is 1M records < 5 s; allow plenty of headroom for loaded CI machines
    expect(ms).toBeLessThan(5000);
  });
});

describe("format", () => {
  test("bytes and percents", () => {
    expect(formatBytes(412_300_000_000, "decimal")).toBe("412.3 GB");
    expect(formatBytes(1024 ** 3, "binary")).toBe("1.0 GiB");
    expect(formatBytes(999_960, "decimal")).toBe("1.0 MB");
    expect(formatBytes(512, "decimal")).toBe("512 B");
    expect(formatPercent(1, 3)).toBe("33%");
    expect(formatPercent(1, 2000)).toBe("<0.1%");
  });
});
