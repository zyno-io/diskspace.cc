// SPDX-License-Identifier: MIT
//
// Desktop app launcher entry (the Wails window's start page). Opened in a
// normal browser it only explains what it is.

import "@fontsource-variable/anybody/standard.css";
import "./ui/theme.css";
import "./launcher/launcher.css";
import { render } from "preact";
import { getBridge } from "./launcher/bridge";
import { Launcher, NotDesktop } from "./launcher/Launcher";

const root = document.getElementById("app")!;

async function boot(): Promise<void> {
  const bridge = await getBridge();
  if (!bridge) {
    document.documentElement.classList.add("lx-web");
    render(<NotDesktop />, root);
    return;
  }
  document.documentElement.classList.add("lx-desktop");
  let snap = await bridge.app.State();
  // A remembered choice (or an app that already runs as administrator) skips
  // the choice screen, but only on a fresh launch, not when coming back.
  if (snap.phase === "idle" && (snap.guiElevated || snap.savedChoice)) {
    try {
      await bridge.app.Start(snap.guiElevated || snap.savedChoice === "admin", false);
      snap = await bridge.app.State();
    } catch {
      /* already starting: the "agent" event will catch us up */
    }
  }
  render(<Launcher bridge={bridge} initial={snap} />, root);
}

void boot();
