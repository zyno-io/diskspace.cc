// SPDX-License-Identifier: MIT

import type { ComponentChildren } from "preact";
import { useCallback, useEffect, useRef } from "preact/hooks";
import type { ConnState } from "../protocol/types";
import { SurveyGrid } from "../viz/survey";
import type { Controller } from "./controller";
import { HostChip } from "./HostChip";
import { Back } from "./icons";
import { prefersReducedMotion } from "./motion";
import { Picker } from "./Picker";
import { goToLauncher, Results } from "./Results";
import { Wordmark } from "./Wordmark";

export function App({ ctl, embedded }: { ctl: Controller; embedded: boolean }) {
  const phase = ctl.phase.value;
  if (phase === "scanning" || phase === "done" || phase === "stopped") return <Results ctl={ctl} embedded={embedded} />;
  return (
    <Plane ctl={ctl} embedded={embedded} breathe={phase === "connecting"}>
      {phase === "picker" ? <Picker ctl={ctl} /> : <StatusFor ctl={ctl} />}
    </Plane>
  );
}

/** survey-grid plane with the giant wordmark bleeding off the bottom-left */
export function Plane({ ctl, embedded, breathe, children }: { ctl: Controller | null; embedded: boolean; breathe: boolean; children: ComponentChildren }) {
  const ref = useRef<HTMLCanvasElement>(null);
  useEffect(() => {
    const g = new SurveyGrid(ref.current!, { reducedMotion: prefersReducedMotion() });
    return () => g.destroy();
  }, []);
  const stretch = useCallback(() => (breathe ? null : 100), [breathe]);
  return (
    <div class="plane">
      <canvas class="survey" ref={ref} aria-hidden="true" />
      <div class="plane-top">
        {embedded ? (
          <button class="btn btn-quiet back-launcher" onClick={goToLauncher}>
            <Back /> Launcher
          </button>
        ) : (
          <span />
        )}
        {ctl && <HostChip info={ctl.info.value} conn={ctl.conn.value} />}
      </div>
      <main class="plane-main">{children}</main>
      <div class="plane-foot" aria-hidden="true">
        <Wordmark size="hero" stretch={stretch} fit={112} />
      </div>
    </div>
  );
}

export function Status({ title, body, detail, children, live = false }: { title: string; body?: string; detail?: string; children?: ComponentChildren; live?: boolean }) {
  return (
    <section class="status" role={live ? "status" : "alert"}>
      <h1 class={live ? "dots" : undefined}>{title}</h1>
      {body && <p>{body}</p>}
      {children}
      {detail && <p class="detail">{detail}</p>}
    </section>
  );
}

function connectingCopy(c: ConnState, hasInfo: boolean): [string, string] {
  switch (c.s) {
    case "connecting":
      return ["Connecting to your computer", "Reaching the relay."];
    case "handshaking":
      return ["Connecting to your computer", "Checking this link's key and setting up end-to-end encryption."];
    case "reconnecting":
      return ["Reconnecting", `The connection dropped. Trying again${c.attempt > 1 ? ` (attempt ${c.attempt})` : ""}.`];
    case "connected":
      return ["Connected", hasInfo ? "" : "Waiting for diskspace to describe this computer."];
    default:
      return ["Connecting to your computer", ""];
  }
}

function StatusFor({ ctl }: { ctl: Controller }) {
  const c = ctl.conn.value;
  if (c.s === "superseded")
    return (
      <section class="card" aria-labelledby="sup-title">
        <div class="card-head">
          <h1 id="sup-title">This link was opened in another window.</h1>
          <p>Only one window can view a scan at a time. Taking over here disconnects the other one.</p>
        </div>
        <div class="card-foot">
          <button class="btn btn-primary" onClick={() => ctl.session.takeOver()}>
            Take over here
          </button>
        </div>
      </section>
    );
  if (c.s === "closed") {
    const idle = c.reason === "idle";
    return <Status title={idle ? "This session timed out." : "diskspace stopped on your computer."} body={idle ? "It ended after a long period without activity. Run diskspace again to get a new link." : "Run it again to get a new link."} />;
  }
  if (c.s === "error") return <ErrorStatus kind={c.kind} message={c.message} />;
  const [t, b] = connectingCopy(c, !!ctl.info.value);
  return <Status title={t} body={b} live />;
}

export function ErrorStatus({ kind, message }: { kind: string; message?: string }) {
  switch (kind) {
    case "key-mismatch":
      return <Status title="This link's key doesn't match." body="Open the newest link printed by diskspace." />;
    case "no-session":
      return <Status title="No running diskspace found for this link." body="It may have been closed. Run diskspace again and open the link it prints." />;
    case "missing-key":
      return <Status title="This link is missing its key." body="Open the full link printed by diskspace, including everything after the #. The key stays in your browser and is never sent to the relay." />;
    case "insecure":
      return <Status title="This link needs a secure connection." body="Browsers only allow end-to-end encryption on https:// pages or on this computer (127.0.0.1). Open the https link printed by diskspace, or use its local link." />;
    case "unsupported":
      return <Status title="This browser can't open the link." body="diskspace needs a current browser with WebCrypto and WebSockets. Try the latest Chrome, Firefox, Safari or Edge." detail={message} />;
    case "network":
      return (
        <Status title="Can't reach diskspace.cc." body="Check your internet connection, then reload this page." detail={message}>
          <button class="btn" onClick={() => location.reload()}>
            Reload
          </button>
        </Status>
      );
    default:
      return (
        <Status title="Something went wrong talking to your computer." body="Reload to reconnect. If it keeps happening, restart diskspace." detail={message}>
          <button class="btn" onClick={() => location.reload()}>
            Reload
          </button>
        </Status>
      );
  }
}
