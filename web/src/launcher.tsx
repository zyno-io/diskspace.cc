// SPDX-License-Identifier: MIT
//
// GUI launcher placeholder (the real launcher is built later). Renders the
// wordmark on the plane so the Wails shell has something branded to show.

import "@fontsource-variable/anybody/standard.css";
import "./ui/theme.css";
import { render } from "preact";

function Launcher() {
  return (
    <div class="plane">
      <div />
      <main class="plane-main">
        <p class="status">Starting diskspace…</p>
      </main>
      <div class="plane-foot">
        <div class="wm wm-hero">
          diskspace<span class="cc">.cc</span>
        </div>
      </div>
    </div>
  );
}

render(<Launcher />, document.getElementById("app")!);
