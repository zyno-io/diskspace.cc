// SPDX-License-Identifier: MIT
//
// Landing page. The hero is a live treemap fed by MockSession (looping), with
// the survey grid behind it and the stretching wordmark over a petrol scrim.

import "@fontsource-variable/anybody/standard.css";
import "@fontsource-variable/instrument-sans/standard.css";
import "./ui/theme.css";
import "./ui/landing.css";
import { render } from "preact";
import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import { MockSession } from "./protocol/mock";
import type { Progress } from "./protocol/types";
import { TreeStore } from "./store/tree";
import { SurveyGrid } from "./viz/survey";
import { TreemapView } from "./viz/treemap";
import { Check, Copy, Download } from "./ui/icons";
import { prefersReducedMotion } from "./ui/motion";
import { Wordmark } from "./ui/Wordmark";

const DEMO_DIRS = 20_000;

/** drives the hero map; returns a progress getter for the wordmark */
function useDemo(host: { current: HTMLDivElement | null }, grid: { current: HTMLCanvasElement | null }) {
  const progress = useRef(0);
  useEffect(() => {
    const reduced = prefersReducedMotion();
    const survey = new SurveyGrid(grid.current!, { reducedMotion: reduced, alpha: 0.75 });
    const store = new TreeStore();
    const view = new TreemapView(host.current!, store, { interactive: false, reducedMotion: reduced, units: () => "decimal", layout: { nestW: 56, nestH: 46 } });
    const session = new MockSession({ targetDirs: DEMO_DIRS, durationMs: 15_000, quick: true, seed: 11 });
    let timer = 0;
    let final = 1;
    const home = "/Users/alex";
    session.start({
      state: () => undefined,
      agentInfo: () => session.scanStart(home),
      scanStarted: (m) => {
        store.reset(m);
        view.reset(store.root);
        progress.current = 0;
      },
      nodes: (b) => {
        store.ingest(b);
        session.applied(b.wireBytes);
        view.invalidate();
      },
      progress: (p: Progress) => {
        progress.current = Math.min(1, p.alloc / final);
      },
      scanDone: (m) => {
        final = m.totals.alloc || 1;
        progress.current = 1;
        // hold the finished map, then survey again
        timer = window.setTimeout(() => {
          session.scanStart(home);
        }, reduced ? 600_000 : 6500);
      },
      scanEnded: () => undefined,
      replayDone: () => undefined,
      bye: () => undefined,
      error: () => undefined,
    });
    // first cycle has no known final size; estimate so the stretch is smooth
    final = 150e9;
    return () => {
      window.clearTimeout(timer);
      session.close();
      view.destroy();
      survey.destroy();
    };
  }, []);
  return progress;
}

type Tab = "unix" | "windows" | "desktop";

function detectTab(): Tab {
  const ua = navigator.userAgent;
  const plat = (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData?.platform ?? navigator.platform ?? "";
  if (/win/i.test(plat) || /Windows/.test(ua)) return "windows";
  return "unix";
}

const COMMANDS: Record<Exclude<Tab, "desktop">, { cmd: string; note: string }> = {
  unix: { cmd: "curl -fsSL https://diskspace.cc/run | sh", note: "Paste into Terminal. Add a path after sh -s -- to scan it straight away." },
  windows: { cmd: "irm https://diskspace.cc/run.ps1 | iex", note: "Open PowerShell and paste it there (Defender blocks it from Run or cmd). Nothing is installed; the binary is checked against published checksums." },
};

function InstallCard() {
  const [tab, setTab] = useState<Tab>(detectTab);
  const [copied, setCopied] = useState(false);
  const tabs: [Tab, string][] = [
    ["unix", "macOS & Linux"],
    ["windows", "Windows"],
    ["desktop", "Desktop app"],
  ];
  const copy = async (text: string) => {
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      const r = document.createRange();
      const el = document.getElementById("install-cmd");
      if (el) {
        r.selectNodeContents(el);
        getSelection()?.removeAllRanges();
        getSelection()?.addRange(r);
      }
      return;
    }
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1800);
  };
  const onKey = (e: KeyboardEvent) => {
    const i = tabs.findIndex(([t]) => t === tab);
    if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
      const n = tabs[(i + (e.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length][0];
      setTab(n);
      document.getElementById(`tab-${n}`)?.focus();
      e.preventDefault();
    }
  };
  return (
    <div class="install card">
      <div class="tabs" role="tablist" aria-label="How to run diskspace" onKeyDown={onKey}>
        {tabs.map(([t, label]) => (
          <button key={t} id={`tab-${t}`} role="tab" aria-selected={tab === t} aria-controls="install-panel" tabIndex={tab === t ? 0 : -1} onClick={() => setTab(t)}>
            {label}
          </button>
        ))}
      </div>
      <div id="install-panel" role="tabpanel" aria-labelledby={`tab-${tab}`} class="install-panel">
        {tab === "desktop" ? (
          <div class="dl-row">
            <a class="btn btn-primary" href="/dl/gui/mac">
              <Download /> Download for macOS
            </a>
            <a class="btn" href="/dl/gui/windows">
              <Download /> Download for Windows
            </a>
            <p class="install-note">The same scanner in a small window. macOS 11 or later, Windows 10 or later.</p>
          </div>
        ) : (
          <>
            <div class="cmd-row">
              <code id="install-cmd" class="cmd">
                {COMMANDS[tab].cmd}
              </code>
              <button class={`btn copy${copied ? " ok" : ""}`} onClick={() => copy(COMMANDS[tab].cmd)} aria-live="polite">
                {copied ? <Check /> : <Copy />}
                {copied ? "Copied" : "Copy"}
              </button>
            </div>
            <p class="install-note">{COMMANDS[tab].note}</p>
          </>
        )}
      </div>
    </div>
  );
}

function KeyDiagram() {
  return (
    <>
      <KeyDiagramWide />
      <KeyDiagramTall />
    </>
  );
}

/** narrow-screen version of the same diagram, stacked vertically */
function KeyDiagramTall() {
  return (
    <svg class="keydiag tall" viewBox="0 0 340 330" role="img" aria-labelledby="kdt-t">
      <title id="kdt-t">The key after the # stays in your browser; only the session id reaches the relay.</title>
      <rect x="1" y="1" width="338" height="196" rx="12" class="kd-box" />
      <text x="16" y="27" class="kd-label">Your browser</text>
      <rect x="14" y="40" width="312" height="38" rx="8" class="kd-url" />
      <text x="26" y="64" class="kd-mono">
        <tspan class="kd-sent">diskspace.cc/s/q8Kf2wX</tspan>
        <tspan class="kd-key">#Hp3vR9…</tspan>
      </text>
      <path d="M238 78 V112" class="kd-key-line" />
      <rect x="180" y="114" width="146" height="64" rx="8" class="kd-keybox" />
      <text x="194" y="141" class="kd-small">Key stays here</text>
      <text x="194" y="161" class="kd-small kd-dim">decrypts results</text>
      <path d="M90 78 V238" class="kd-sent-line" />
      <path d="M84 230 90 238 96 230" class="kd-sent-line" />
      <text x="100" y="150" class="kd-small kd-dim">/s/q8Kf2wX</text>
      <rect x="1" y="240" width="338" height="88" rx="12" class="kd-box" />
      <text x="16" y="266" class="kd-label">Relay</text>
      <text x="16" y="290" class="kd-small kd-dim">sees the session id and encrypted bytes</text>
      <text x="16" y="312" class="kd-small">no names, no sizes</text>
    </svg>
  );
}

function KeyDiagramWide() {
  return (
    <svg class="keydiag wide" viewBox="0 0 640 210" role="img" aria-labelledby="kd-t kd-d">
      <title id="kd-t">Where the key goes</title>
      <desc id="kd-d">The link has two parts. The part before the # goes to the relay to find your session. The key after the # stays in your browser and is never sent.</desc>
      {/* browser */}
      <rect x="1" y="1" width="380" height="208" rx="12" class="kd-box" />
      <text x="20" y="30" class="kd-label">Your browser</text>
      <rect x="20" y="46" width="342" height="38" rx="8" class="kd-url" />
      <text x="34" y="70" class="kd-mono">
        <tspan class="kd-sent">diskspace.cc/s/q8Kf2wX</tspan>
        <tspan class="kd-key">#Hp3vR9…</tspan>
      </text>
      <rect x="236" y="120" width="126" height="64" rx="8" class="kd-keybox" />
      <text x="250" y="146" class="kd-small">Key stays here</text>
      <text x="250" y="166" class="kd-small kd-dim">decrypts results</text>
      <path d="M299 84 V118" class="kd-key-line" />
      {/* relay */}
      <rect x="470" y="46" width="168" height="138" rx="12" class="kd-box" />
      <text x="488" y="74" class="kd-label">Relay</text>
      <text x="488" y="100" class="kd-small kd-dim">sees the session id</text>
      <text x="488" y="120" class="kd-small kd-dim">and encrypted bytes</text>
      <text x="488" y="150" class="kd-small">no names, no sizes</text>
      <path d="M150 84 V150 H462" class="kd-sent-line" />
      <path d="M454 144 462 150 454 156" class="kd-sent-line" />
      <text x="170" y="142" class="kd-small kd-dim">
        /s/q8Kf2wX
      </text>
    </svg>
  );
}

function Landing() {
  const host = useRef<HTMLDivElement>(null);
  const grid = useRef<HTMLCanvasElement>(null);
  const progress = useDemo(host, grid);
  const stretch = useCallback(() => 50 + 100 * progress.current, [progress]);
  return (
    <>
      <section class="hero">
        <canvas class="hero-grid" ref={grid} aria-hidden="true" />
        <div class="hero-map" ref={host} aria-hidden="true" />
        <div class="hero-scrim" aria-hidden="true" />
        <p class="hero-by">
          Made by <a href="https://zyno.io">Zyno Consulting</a>
        </p>
        <div class="hero-copy">
          <Wordmark as="h1" size="hero" stretch={stretch} fit={150} />
          <p class="pitch">See what's filling your disk, from any browser.</p>
          <p class="sub">End-to-end encrypted; the relay never sees your file names.</p>
          <InstallCard />
          <p class="demo">
            <a href="/demo">Try the demo</a> with a made-up Mac, no install needed.
          </p>
        </div>
      </section>

      <main class="below">
        <section aria-labelledby="how">
          <h2 id="how">How it works</h2>
          <ol class="steps">
            <li>
              <h3>Run the command</h3>
              <p>It downloads a small binary for your system, checks it against published checksums and starts it. Nothing is installed, and it exits when you close it.</p>
            </li>
            <li>
              <h3>Open the link it prints</h3>
              <p>The link carries a one-time key after the #. Your browser uses it to open an encrypted channel to your computer. Both sides show the same six-digit code so you can check nobody is in between.</p>
            </li>
            <li>
              <h3>Pick a folder</h3>
              <p>Choose the current folder, your home or a whole disk. The map fills in as the scan runs, so you can start digging before it finishes.</p>
            </li>
          </ol>
        </section>

        <section aria-labelledby="relay">
          <h2 id="relay">What the relay can and can't see</h2>
          <p class="lede">The relay at diskspace.cc only passes messages between your browser and your computer. Everything about your files is encrypted before it leaves your computer, with a key the relay never receives.</p>
          <KeyDiagram />
          <div class="sees">
            <div>
              <h3>The relay can see</h3>
              <ul>
                <li>That a session exists, and when it starts and ends</li>
                <li>How much encrypted data passes through</li>
                <li>The IP addresses of your browser and your computer</li>
              </ul>
            </div>
            <div>
              <h3>The relay can't see</h3>
              <ul>
                <li>File or folder names, paths or sizes</li>
                <li>Anything about your disk or what's on it</li>
                <li>The key in your link: browsers never send the part after #</li>
              </ul>
            </div>
          </div>
          <p class="fine">The relay also serves this page's code. If you'd rather not rely on that, run diskspace with --local and your computer serves the page itself.</p>
        </section>

        <section aria-labelledby="desktop">
          <h2 id="desktop">Desktop app</h2>
          <p>Prefer a window to a terminal? The desktop app runs the same scanner and can show results right inside it.</p>
          <div class="dl-row">
            <a class="btn btn-primary" href="/dl/gui/mac">
              <Download /> macOS
            </a>
            <a class="btn" href="/dl/gui/windows">
              <Download /> Windows
            </a>
          </div>
        </section>
      </main>
      <footer class="foot">
        <span class="wm foot-wm">
          diskspace<span class="cc">.cc</span>
        </span>
        <span>
          MIT licensed. Source on <a href="https://github.com/zyno-io/diskspace.cc">GitHub</a>.
        </span>
        <span class="foot-made">
          Made by <a href="https://zyno.io">Zyno Consulting</a>
        </span>
      </footer>
    </>
  );
}

render(<Landing />, document.getElementById("app")!);
