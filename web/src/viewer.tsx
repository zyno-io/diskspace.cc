// SPDX-License-Identifier: MIT
//
// Viewer entry for /s/<sid>#<K>. The key K moves from the URL fragment into
// sessionStorage (keyed by sid) and the fragment is stripped, so it never
// lingers in history or gets copied along with the address bar.

import "@fontsource-variable/anybody/standard.css";
import "@fontsource-variable/instrument-sans/standard.css";
import "./ui/theme.css";
import { render } from "preact";
import { MockSession } from "./protocol/mock";
import { RealSession } from "./protocol/session";
import type { Session } from "./protocol/types";
import { App, ErrorStatus, Plane } from "./ui/App";
import { Controller } from "./ui/controller";
import { fetchLatest } from "./ui/update";

const params = new URLSearchParams(location.search);
const embedded = params.get("embedded") === "1";
const root = document.getElementById("app")!;

function b64urlDecode(s: string): Uint8Array | null {
  if (!/^[A-Za-z0-9_-]+$/.test(s)) return null;
  try {
    const bin = atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4));
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  } catch {
    return null;
  }
}

function readKey(sid: string): Uint8Array | null {
  const storeKey = `diskspace:k:${sid}`;
  const frag = location.hash.slice(1);
  if (frag) {
    // strip the fragment immediately, whatever happens next
    history.replaceState(history.state, "", location.pathname + location.search);
    const k = b64urlDecode(frag);
    if (k && k.length === 32) {
      try {
        sessionStorage.setItem(storeKey, frag);
      } catch {
        /* storage blocked: key lives only in memory for this page */
      }
      return k;
    }
    return null;
  }
  try {
    const saved = sessionStorage.getItem(storeKey);
    const k = saved ? b64urlDecode(saved) : null;
    return k && k.length === 32 ? k : null;
  } catch {
    return null;
  }
}

function boot(): void {
  const path = location.pathname;
  const demo = params.has("demo") || path === "/demo" || path === "/demo/";
  let session: Session;
  if (demo) {
    const approve = params.get("approve");
    session = new MockSession({ demoState: params.get("state"), autoScan: params.get("scan"), approve: approve === "no" ? "no" : approve ? Number(approve) : undefined });
  } else {
    const m = /^\/s\/([A-Za-z0-9_-]{8,64})\/?$/.exec(path);
    const sid = m?.[1] ?? "";
    const key = sid ? readKey(sid) : null;
    // WebCrypto exists only in secure contexts (https, or http on loopback).
    const kind = !sid || !key ? "missing-key" : !window.isSecureContext || !crypto?.subtle ? "insecure" : null;
    if (kind) {
      render(
        <Plane ctl={null} embedded={embedded} breathe={false}>
          <ErrorStatus kind={kind} />
        </Plane>,
        root,
      );
      return;
    }
    const wsUrl = (location.protocol === "https:" ? "wss" : "ws") + "://" + location.host + "/v1/view?sid=" + sid;
    session = new RealSession({ sid, key: key!, wsUrl });
  }
  const ctl = new Controller(session);
  render(<App ctl={ctl} embedded={embedded} />, root);
  ctl.start();
  // The demo takes ?latest=0.1.2 to preview the update banner.
  if (demo) ctl.latest.value = params.get("latest");
  else void fetchLatest().then((v) => (ctl.latest.value = v));
  addEventListener("pagehide", () => session.close());
}

boot();
