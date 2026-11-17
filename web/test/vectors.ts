// SPDX-License-Identifier: MIT
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const dir = resolve(dirname(fileURLToPath(import.meta.url)), "../../testdata/vectors");

export function vec<T = any>(name: string): T {
  return JSON.parse(readFileSync(resolve(dir, name), "utf8")) as T;
}

export function unhex(s: string): Uint8Array {
  return Uint8Array.from(Buffer.from(s, "hex"));
}

export function hex(b: Uint8Array): string {
  return Buffer.from(b).toString("hex");
}

/** mirrors synthBody in internal/proto/proto_test.go */
export function synthBody(i: number, n: number): Uint8Array {
  const b = new Uint8Array(n);
  b[0] = 0x11;
  let st = (Math.imul(i, 7919) + 1) >>> 0;
  const tab = new Uint8Array(97);
  for (let j = 0; j < 97; j++) {
    st = (Math.imul(st, 1664525) + 1013904223) >>> 0;
    tab[j] = st >>> 24;
  }
  for (let j = 1; j < n; j++) b[j] = (tab[j % 97] + (j >> 12)) & 0xff;
  return b;
}
