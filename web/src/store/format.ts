// SPDX-License-Identifier: MIT

export type Units = "decimal" | "binary";

const DEC = ["B", "KB", "MB", "GB", "TB", "PB"];
const BIN = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

export function defaultUnits(os: string | undefined): Units {
  return os === "darwin" ? "decimal" : "binary";
}

/** "412.3 GB", "16.0 MB", "512 B" */
export function formatBytes(n: number, units: Units): string {
  const base = units === "decimal" ? 1000 : 1024;
  const names = units === "decimal" ? DEC : BIN;
  if (!(n > 0)) return "0 B";
  if (n < base) return `${Math.round(n)} B`;
  let i = 0;
  let v = n;
  while (v >= base && i < names.length - 1) {
    v /= base;
    i++;
  }
  // 999.95 would round to "1000.0"; bump the unit instead
  if (v >= base - 0.05 && i < names.length - 1) {
    v /= base;
    i++;
  }
  return `${v.toFixed(1)} ${names[i]}`;
}

const nf = new Intl.NumberFormat("en-US");
export function formatCount(n: number): string {
  return nf.format(Math.round(n));
}

/** 1.2M, 86k, 950 */
export function formatCompact(n: number): string {
  if (n < 1000) return String(Math.round(n));
  if (n < 10_000) return (n / 1000).toFixed(1).replace(/\.0$/, "") + "k";
  if (n < 1_000_000) return Math.round(n / 1000) + "k";
  return (n / 1_000_000).toFixed(1).replace(/\.0$/, "") + "M";
}

export function formatDuration(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, "0")}m`;
}

export function formatPercent(part: number, whole: number): string {
  if (!(whole > 0)) return "";
  const p = (part / whole) * 100;
  if (p >= 99.95) return "100%";
  if (p >= 10) return `${Math.round(p)}%`;
  if (p >= 0.1) return `${p.toFixed(1)}%`;
  return p > 0 ? "<0.1%" : "0%";
}

export function plural(n: number, one: string, many: string): string {
  return `${formatCount(n)} ${n === 1 ? one : many}`;
}
