// SPDX-License-Identifier: MIT
//
// "A newer diskspace is available" logic: version compare, the relay lookup,
// where to send the user, and per-version dismissal.
//
// The relay serves the viewer and answers GET /v1/latest. An agent-served local
// viewer (127.0.0.1, no relay) has no such endpoint, so it simply shows no banner.

import type { AgentInfo } from "../protocol/types";

/** numeric major.minor.patch; "dev" and anything unparseable is not a release */
export function parseVersion(v: string): [number, number, number] | null {
  const m = /^v?(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:[-+].*)?$/.exec(v.trim());
  if (!m) return null;
  return [Number(m[1]), Number(m[2] ?? 0), Number(m[3] ?? 0)];
}

/** true when `latest` is a higher release than `have`; prereleases of the same version are not older */
export function isNewer(have: string, latest: string): boolean {
  const h = parseVersion(have);
  const l = parseVersion(latest);
  if (!h || !l) return false;
  for (let i = 0; i < 3; i++) if (l[i] !== h[i]) return l[i] > h[i];
  return false;
}

/** newest stable version from the relay, or null (offline, local viewer, unknown) */
export async function fetchLatest(): Promise<string | null> {
  try {
    const r = await fetch("/v1/latest", { cache: "no-store", headers: { accept: "application/json" } });
    if (!r.ok || !(r.headers.get("content-type") ?? "").includes("json")) return null;
    const j = (await r.json()) as { version?: unknown };
    return typeof j.version === "string" && parseVersion(j.version) ? j.version.replace(/^v/, "") : null;
  } catch {
    return null;
  }
}

export const INSTALL_URL = "https://diskspace.cc/#install";

/** where "How to update" goes: the desktop app's download, or the install command */
export function updateLink(info: AgentInfo): string {
  if (info.launcher === "app") {
    if (info.os === "darwin") return "/dl/gui/mac";
    if (info.os === "windows") return "/dl/gui/windows";
  }
  return INSTALL_URL;
}

const KEY = "diskspace:update-dismissed";

export function isDismissed(version: string): boolean {
  try {
    return localStorage.getItem(KEY) === version;
  } catch {
    return false;
  }
}

export function dismiss(version: string): void {
  try {
    localStorage.setItem(KEY, version);
  } catch {
    /* storage blocked: the banner just returns next time */
  }
}
