// SPDX-License-Identifier: MIT
import { describe, expect, it } from "vitest";
import { decodeDirFiles, decodeName, decodeNodes } from "../src/protocol/codec";
import { FILE_FLAG } from "../src/protocol/types";
import { ProtocolError } from "../src/protocol/varint";
import { unhex, vec } from "./vectors";

const v = vec<any>("nodes.json");
const td = new TextDecoder("utf-8");
const nameOf = (h: string) => td.decode(unhex(h)); // lossy, same as decoder

describe("NODES decode (Go vectors)", () => {
  for (const c of v.nodes) {
    it(c.name, () => {
      const b = decodeNodes(unhex(c.body), 123);
      expect(b.scanId).toBe(c.scanId);
      expect(b.wireBytes).toBe(123);
      const exp: any[] = c.records ?? [];
      expect(b.records.length).toBe(exp.length);
      exp.forEach((e: any, i: number) => {
        const r: any = b.records[i];
        expect(r.k).toBe(e.k);
        if (e.k === 1) {
          expect([r.id, r.flags, r.ownAlloc, r.ownApparent, r.ownFiles, r.entryErrors, r.totalDirs, r.firstChildId]).toEqual(
            [e.id ?? 0, e.flags ?? 0, e.ownAlloc ?? 0, e.ownApparent ?? 0, e.ownFiles ?? 0, e.entryErrors ?? 0, e.totalDirs ?? 0, e.firstChildId ?? 0],
          );
          expect(r.dirs.map((d: any) => d.name)).toEqual((e.dirs ?? []).map((d: any) => nameOf(d.hex)));
          expect(r.files.map((f: any) => [f.name, f.alloc, f.apparent])).toEqual((e.files ?? []).map((f: any) => [nameOf(f.hex), f.alloc, f.apparent]));
          // BAD_NAME exactly when the name bytes are invalid UTF-8
          (e.files ?? []).forEach((f: any, j: number) => {
            const invalid = !isValidUtf8(unhex(f.hex));
            expect(!!(r.files[j].flags & FILE_FLAG.BAD_NAME)).toBe(invalid || !!(f.flags & FILE_FLAG.BAD_NAME));
          });
        } else if (e.k === 2) {
          expect([r.targetId, r.alloc, r.apparent, r.files, r.dirs]).toEqual([e.targetId, e.alloc ?? 0, e.apparent ?? 0, e.filesN ?? 0, e.dirsN ?? 0]);
        } else if (e.k === 4) {
          expect([r.id, r.firstChildId]).toEqual([e.id ?? 0, e.firstChildId ?? 0]);
          expect(r.dirs.map((d: any) => [d.name, d.flags & 0xff])).toEqual((e.dirs ?? []).map((d: any) => [nameOf(d.hex), d.flags]));
        } else {
          expect(r.targetId).toBe(e.targetId);
        }
      });
    });
  }

  it("every truncation is rejected", () => {
    const body = unhex(v.nodes.find((c: any) => c.name === "mixed").body);
    for (let i = 0; i < body.length; i++) expect(() => decodeNodes(body.subarray(0, i))).toThrow(ProtocolError);
    expect(() => decodeNodes(Uint8Array.from([...body, 0]))).toThrow(ProtocolError);
  });

  it("rejects oversized names and bogus counts", () => {
    expect(() => decodeNodes(Uint8Array.of(0x11, 1, 0xff, 0xff, 0xff, 0xff, 0x0f))).toThrow(ProtocolError);
    const long = [0x11, 1, 1, 1, 0, 0, 0, 0, 0, 0, 1, 0, 1, 0x81, 0x20, ...new Array(4097).fill(65), 0, 0];
    expect(() => decodeNodes(Uint8Array.from(long))).toThrow("name too long");
  });
});

describe("DIR_FILES decode", () => {
  it("matches Go vector", () => {
    const d = v.dirFiles;
    const r = decodeDirFiles(unhex(d.body));
    expect([r.reqId, r.scanId, r.dirId, r.flags, r.restCount, r.restAlloc, r.restApparent]).toEqual([d.reqId, d.scanId, d.dirId, d.flags, d.restCount, d.restAlloc, d.restApparent]);
    expect(r.files.map((f) => f.name)).toEqual(d.files.map((f: any) => nameOf(f.hex)));
    expect(r.files[1].flags & FILE_FLAG.BAD_NAME).toBeTruthy();
    expect(r.files[2].name).toBe("日本語");
    expect(r.files[2].alloc).toBe(2 ** 40);
  });
  it("truncations rejected", () => {
    const b = unhex(v.dirFiles.body);
    for (let i = 0; i < b.length; i++) expect(() => decodeDirFiles(b.subarray(0, i))).toThrow(ProtocolError);
  });
});

describe("DIR child totals", () => {
  it("rejects nInline > nTotal, absurd nTotal and empty DIRMORE", () => {
    // 0x11 scan=1 count=1 DIR id flags oa oap of ee nTotal=0 nInline=1 ...
    expect(() => decodeNodes(Uint8Array.of(0x11, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0x61, 0, 0))).toThrow("inline children exceed total");
    expect(() => decodeNodes(Uint8Array.of(0x11, 1, 1, 1, 0, 0, 0, 0, 0, 0, 129, 146, 244, 1, 0, 0, 0))).toThrow(ProtocolError); // nTotal=4000001 > cap
    expect(() => decodeNodes(Uint8Array.of(0x11, 1, 1, 4, 0, 1, 0))).toThrow(ProtocolError);
  });
});

describe("decodeName", () => {
  it("flags invalid UTF-8 but not literal U+FFFD", () => {
    expect(decodeName(Uint8Array.of(0x61, 0xff)).bad).toBe(true);
    expect(decodeName(new TextEncoder().encode("a�b")).bad).toBe(false);
    expect(decodeName(new TextEncoder().encode("héllo")).s).toBe("héllo");
    expect(decodeName(new TextEncoder().encode("x".repeat(200))).s.length).toBe(200);
  });
});

function isValidUtf8(b: Uint8Array): boolean {
  try {
    new TextDecoder("utf-8", { fatal: true }).decode(b);
    return true;
  } catch {
    return false;
  }
}
