// SPDX-License-Identifier: MIT

import type { AgentInfo, ConnState } from "../protocol/types";
import { Lock } from "./icons";

const METHOD: Record<string, string> = {
  sudo: "sudo",
  uac: "Windows administrator approval",
  "gui-admin": "the desktop app's administrator mode",
  native: "an administrator account",
};

export function elevationHint(info: AgentInfo): string {
  if (info.elevated) {
    const how = METHOD[info.elevation] ?? "administrator rights";
    const who = info.invokingUser && info.invokingUser !== info.user ? `, started by ${info.invokingUser}` : "";
    return `Running as ${info.user} via ${how}${who}. Every folder on this computer can be read.`;
  }
  return `Running as ${info.user} without administrator rights. Other users' folders and some system areas can't be read.`;
}

export function HostChip({ info, conn, compact = false }: { info: AgentInfo | null; conn: ConnState; compact?: boolean }) {
  const code = conn.s === "connected" ? conn.code : null;
  return (
    <div class="chips">
      {info && !compact && (
        <span class="chip has-hint" tabIndex={0}>
          <b>{info.hostname}</b>
          <span class="chip-sep" />
          <span>{info.elevated ? "Admin" : "Standard user"}</span>
          <span class="hint" role="tooltip">
            {elevationHint(info)}
          </span>
        </span>
      )}
      {info && info.os === "darwin" && info.fda === "denied" && !compact && (
        <span class="chip chip-warn has-hint" tabIndex={0}>
          Full Disk Access off
          <span class="hint" role="tooltip">
            Mail, Messages, Safari and some app data are protected by macOS and will show as unreadable.
            <span class="muted">{info.launcher === "app" ? <>To include them, allow Diskspace in System Settings › Privacy &amp; Security › Full Disk Access, then quit and reopen it.</> : <>To include them, allow your terminal app in System Settings › Privacy &amp; Security › Full Disk Access, then restart it and run diskspace again.</>}</span>
          </span>
        </span>
      )}
      {code && (
        <span class="chip has-hint" tabIndex={0}>
          <Lock />
          {compact && info ? <b>{info.hostname}</b> : <span>Verification code</span>}
          {compact ? <span class="chip-sep" /> : null}
          <span class="code">{code}</span>
          <span class="hint" role="tooltip">
            {compact && <>Verification code {code}. </>}Matches the code {info?.launcher === "app" ? "in the diskspace app" : "in your terminal"}. The connection is end-to-end encrypted.
            <span class="muted">If the codes differ, close this tab: something is between you and your computer.</span>
          </span>
        </span>
      )}
    </div>
  );
}
