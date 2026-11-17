// SPDX-License-Identifier: MIT
//
// Desktop launcher (Wails window, ~480×680): choose admin or not, then hand
// out the secure link (QR, copy, open in browser / here) and mirror the
// agent's status. All state lives in Go (gui/app.go); this page renders the
// latest Snapshot, so it survives the round trip through "Open here".

import type { ComponentChildren } from "preact";
import { useCallback, useEffect, useMemo, useRef, useState } from "preact/hooks";
import { SurveyGrid } from "../viz/survey";
import { Check, Copy, Lock } from "../ui/icons";
import { prefersReducedMotion } from "../ui/motion";
import { Wordmark } from "../ui/Wordmark";
import { type Bridge, displayLink, type Snapshot } from "./bridge";
import { qrPath } from "./qr";

const OS_NAME: Record<string, string> = { darwin: "macOS", windows: "Windows" };

/** survey-grid plane with the wordmark bleeding off the bottom edge */
function Shell({ top, children, breathe, foot = true }: { top?: ComponentChildren; children: ComponentChildren; breathe: boolean; foot?: boolean }) {
  const ref = useRef<HTMLCanvasElement>(null);
  useEffect(() => {
    const g = new SurveyGrid(ref.current!, { reducedMotion: prefersReducedMotion(), alpha: 0.55 });
    return () => g.destroy();
  }, []);
  const stretch = useCallback(() => (breathe ? null : 100), [breathe]);
  return (
    <div class="lx">
      <canvas class="survey" ref={ref} aria-hidden="true" />
      <header class="lx-top">{top}</header>
      <main class="lx-main">{children}</main>
      {foot && (
        <div class="lx-foot" aria-hidden="true">
          <Wordmark size="hero" stretch={stretch} fit={118} />
        </div>
      )}
    </div>
  );
}

// ---- choice ------------------------------------------------------------------

function ChoiceScreen({ snap, bridge }: { snap: Snapshot; bridge: Bridge }) {
  const [remember, setRemember] = useState(snap.savedChoice !== "");
  const [busy, setBusy] = useState(false);
  const cancelled = snap.phase === "cancelled";
  const choose = async (admin: boolean) => {
    if (busy) return;
    setBusy(true);
    try {
      if (!remember && snap.savedChoice) await bridge.app.Forget();
      await bridge.app.Start(admin, remember);
    } finally {
      setBusy(false);
    }
  };
  const ask = snap.os === "windows" ? "Windows will ask for permission." : "You'll be asked for your password.";
  return (
    <Shell breathe={false}>
      <section class="lx-intro">
        <h1>See what's filling your disk.</h1>
        <p>Pick how much diskspace may look at.</p>
      </section>
      {cancelled && (
        <p class="lx-notice" role="status">
          {snap.os === "windows" ? "Windows didn't grant administrator access." : "Administrator access wasn't granted."} Pick again, or scan just your files.
        </p>
      )}
      <div class="card lx-choices" role="group" aria-label="What diskspace may look at">
        <button class="lx-choice" onClick={() => choose(true)} disabled={busy}>
          <span class="lx-choice-title">Scan everything</span>
          <span class="lx-tag lx-tag-admin">Administrator</span>
          <span class="lx-choice-desc">
            Includes other users and system folders. {ask}
          </span>
        </button>
        <div class="lx-sep" />
        <button class="lx-choice" onClick={() => choose(false)} disabled={busy}>
          <span class="lx-choice-title">Just my files</span>
          <span class="lx-tag">Standard</span>
          <span class="lx-choice-desc">Your home folder and anything you can open. No admin rights needed.</span>
        </button>
      </div>
      <label class="lx-remember">
        <input type="checkbox" checked={remember} onChange={(e) => setRemember((e.currentTarget as HTMLInputElement).checked)} />
        <span class="lx-box" aria-hidden="true">
          <Check />
        </span>
        Remember my choice
      </label>
    </Shell>
  );
}

// ---- starting / error ---------------------------------------------------------

function StartingScreen({ snap }: { snap: Snapshot }) {
  const admin = snap.admin && !snap.guiElevated;
  let title = "Starting diskspace";
  let body = "Setting up a private, end-to-end encrypted link.";
  if (admin && snap.os === "darwin") {
    title = "Waiting for your password";
    body = "Enter your password in the macOS dialog so diskspace can read every folder.";
  } else if (admin && snap.os === "windows") {
    title = "Waiting for Windows";
    body = "Approve the Windows prompt so diskspace can read every folder.";
  }
  return (
    <Shell breathe>
      <section class="status lx-status" role="status">
        <h1 class="dots">{title}</h1>
        <p>{body}</p>
      </section>
    </Shell>
  );
}

function ErrorScreen({ snap, bridge }: { snap: Snapshot; bridge: Bridge }) {
  return (
    <Shell breathe={false}>
      <section class="status lx-status" role="alert">
        <h1>diskspace stopped.</h1>
        <p>{snap.message || "Something went wrong starting the scanner."}</p>
        <div class="lx-row">
          <button class="btn btn-primary" onClick={() => bridge.app.Start(snap.admin, false)}>
            Try again
          </button>
          <button class="btn" onClick={() => bridge.app.Reset()}>
            Choose again
          </button>
        </div>
      </section>
    </Shell>
  );
}

// ---- running -----------------------------------------------------------------

function QR({ url }: { url: string }) {
  const q = useMemo(() => (url ? qrPath(url) : null), [url]);
  if (!q)
    return (
      <div class="lx-qr lx-qr-empty" aria-hidden="true">
        <span />
      </div>
    );
  return (
    <div class="lx-qr" role="img" aria-label="QR code of the secure link">
      <svg viewBox={`-1 -1 ${q.size + 2} ${q.size + 2}`} shape-rendering="crispEdges" aria-hidden="true">
        <path d={q.d} fill="currentColor" />
      </svg>
    </div>
  );
}

function useCopied(): [boolean, () => void] {
  const [on, setOn] = useState(false);
  const t = useRef(0);
  useEffect(() => () => clearTimeout(t.current), []);
  return [
    on,
    () => {
      setOn(true);
      clearTimeout(t.current);
      t.current = window.setTimeout(() => setOn(false), 1600);
    },
  ];
}

function useSlowLink(waiting: boolean): boolean {
  const [slow, setSlow] = useState(false);
  useEffect(() => {
    setSlow(false);
    if (!waiting) return;
    const id = window.setTimeout(() => setSlow(true), 12_000);
    return () => clearTimeout(id);
  }, [waiting]);
  return slow;
}

function Status({ snap }: { snap: Snapshot }) {
  const hasLink = !!(snap.url || snap.localUrl);
  const slow = useSlowLink(!hasLink);
  const code = snap.viewer === "connected" ? snap.code : "";
  let tone = "wait";
  let title = "Waiting for a browser";
  let body: ComponentChildren = "Scan the code with your phone, or open the link on this computer.";
  if (!hasLink) {
    title = "Getting a secure link";
    body = slow ? "Still waiting for diskspace.cc. Check your internet connection." : "Setting up end-to-end encryption.";
  } else if (snap.scan === "started") {
    tone = "busy";
    title = "Scanning";
    body = (
      <span class="lx-path" title={snap.scanPath}>
        {snap.scanPath}
      </span>
    );
  } else if (snap.scan === "done") {
    tone = "done";
    title = "Done";
    body = (
      <>
        Scanned <span class="lx-path lx-path-inline">{snap.scanPath}</span>
      </>
    );
  } else if (code) {
    tone = "live";
    title = "Connected";
    body = "Pick what to scan in your browser.";
  }
  return (
    <div class={`lx-state lx-${tone}`} role="status" aria-live="polite">
      <div class="lx-state-title">
        <i class="lx-dot" aria-hidden="true" />
        <span class={tone === "wait" || tone === "busy" ? "dots" : undefined}>{title}</span>
      </div>
      <div class="lx-state-body">{body}</div>
      {code && (
        <div class="lx-code has-hint hint-left hint-up" tabIndex={0}>
          <span class="lx-code-label">
            <Lock /> Verification code
          </span>
          <span class="lx-code-num">{code}</span>
          <span class="hint" role="tooltip">
            Your browser shows the same code. The connection is end-to-end encrypted.
            <span class="muted">If the codes differ, close that browser tab and make a new link.</span>
          </span>
        </div>
      )}
    </div>
  );
}

function ElevationChip({ snap }: { snap: Snapshot }) {
  const admin = snap.elevated || snap.guiElevated;
  const hint = admin
    ? "diskspace runs as administrator. Every folder on this computer can be read, including other users' folders."
    : `diskspace runs as ${snap.user} without administrator rights. Other users' folders and some system areas will show as unreadable.`;
  return (
    <span class={`chip has-hint lx-nodrag${admin ? " lx-chip-admin" : ""}`} tabIndex={0}>
      <i class="lx-chip-dot" aria-hidden="true" />
      <b>{admin ? "Admin" : "Standard user"}</b>
      <span class="hint" role="tooltip">
        {hint}
      </span>
    </span>
  );
}

function FDABanner({ bridge }: { bridge: Bridge }) {
  return (
    <aside class="lx-fda" aria-labelledby="fda-title">
      <div>
        <h2 id="fda-title">Full Disk Access is off</h2>
        <p>
          Mail, Messages, Safari and some app data will show as unreadable. Turn on Diskspace in Privacy &amp; Security › Full Disk Access, then quit and reopen Diskspace.
        </p>
      </div>
      <button class="btn" onClick={() => bridge.app.OpenFDASettings()}>
        Open settings
      </button>
    </aside>
  );
}

function RunningScreen({ snap, bridge }: { snap: Snapshot; bridge: Bridge }) {
  const [copied, flash] = useCopied();
  const [err, setErr] = useState("");
  const link = snap.url || snap.localUrl;
  const shown = displayLink(link);
  const admin = snap.elevated || snap.guiElevated;
  const canToggle = !snap.guiElevated && (snap.os === "darwin" || snap.os === "windows");
  const fda = snap.os === "darwin" && snap.fda === "denied";
  const run = (p: Promise<unknown>) => p.then(() => setErr("")).catch((e) => setErr(String(e)));
  const openHere = async () => {
    try {
      location.href = await bridge.app.OpenHere();
    } catch (e) {
      setErr(String(e));
    }
  };
  return (
    <Shell
      breathe={!link || snap.scan === "started"}
      foot={!fda}
      top={
        <>
          <ElevationChip snap={snap} />
          {canToggle && (
            <button class="btn btn-quiet lx-nodrag lx-restart" onClick={() => run(bridge.app.Restart(!admin))}>
              {admin ? "Restart without admin" : "Restart with admin"}
            </button>
          )}
        </>
      }
    >
      <section class="card lx-link" aria-label="Secure link">
        <div class="lx-link-top">
          <QR url={link} />
          <Status snap={snap} />
        </div>
        <div class="lx-url">
          <span class="lx-url-text" title={link ? "The part after # is the key. It never leaves your devices." : undefined}>
            {link ? (
              <>
                {shown.base}
                {shown.hasKey && <span class="lx-key">#key</span>}
              </>
            ) : (
              <span class="lx-url-wait">Link appears here</span>
            )}
          </span>
          <button
            class={`btn lx-copy${copied ? " on" : ""}`}
            disabled={!link}
            onClick={() => run(bridge.app.Copy(link).then(flash))}
            aria-label={copied ? "Copied" : "Copy link"}
          >
            {copied ? <Check /> : <Copy />}
            <span aria-hidden="true">{copied ? "Copied" : "Copy"}</span>
          </button>
        </div>
        <div class="lx-actions">
          <button class="btn btn-primary lx-grow" disabled={!snap.url} onClick={() => run(bridge.app.OpenInBrowser(snap.url))}>
            Open in browser
          </button>
          <button class="btn" disabled={!snap.localUrl} onClick={openHere}>
            Open here
          </button>
          <button class="btn btn-quiet" disabled={!link} onClick={() => run(bridge.app.Rotate())} title="Make a new link. The current one stops working.">
            New link
          </button>
        </div>
        <p class="lx-fine">
          {admin ? "Anyone with this link can see the names of every file on this computer." : "Anyone with this link can see the names of your files."} Keep it to yourself.
        </p>
      </section>
      {(err || snap.notice) && (
        <p class="lx-warn" role="alert">
          {err || snap.notice}
        </p>
      )}
      {fda && <FDABanner bridge={bridge} />}
    </Shell>
  );
}

// ---- entry -------------------------------------------------------------------

export function Launcher({ bridge, initial }: { bridge: Bridge; initial: Snapshot }) {
  const [snap, setSnap] = useState(initial);
  const seq = useRef(initial.seq);
  useEffect(() => {
    const take = (s: Snapshot) => {
      if (s.seq < seq.current) return;
      seq.current = s.seq;
      setSnap(s);
    };
    const off = bridge.runtime.EventsOn("agent", (d) => take(d as Snapshot));
    // coming back from the in-app viewer (bfcache or reload): resync + shrink
    const onShow = () => {
      bridge.app.LauncherShown();
      bridge.app.State().then(take);
    };
    window.addEventListener("pageshow", onShow);
    bridge.app.LauncherShown();
    return () => {
      off();
      window.removeEventListener("pageshow", onShow);
    };
  }, [bridge]);
  switch (snap.phase) {
    case "starting":
      return <StartingScreen snap={snap} />;
    case "running":
      return <RunningScreen snap={snap} bridge={bridge} />;
    case "error":
      return <ErrorScreen snap={snap} bridge={bridge} />;
    default:
      return <ChoiceScreen snap={snap} bridge={bridge} />;
  }
}

/** shown when /launcher is opened in a normal browser */
export function NotDesktop() {
  return (
    <Shell breathe={false}>
      <section class="status lx-status">
        <h1>This page is the desktop app's launcher.</h1>
        <p>
          It only works inside Diskspace for {OS_NAME.darwin} and {OS_NAME.windows}. <a href="/">Go to diskspace.cc</a>
        </p>
      </section>
    </Shell>
  );
}
