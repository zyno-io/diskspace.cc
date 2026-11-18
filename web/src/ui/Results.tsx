// SPDX-License-Identifier: MIT
//
// Scanning / results layout: slim top bar on the plane, full-bleed treemap,
// frosted list rail. Zoom changes are pushed to history so Back zooms out.

import { effect } from "@preact/signals";
import { useCallback, useEffect, useRef } from "preact/hooks";
import { DIR_FLAG } from "../protocol/types";
import { formatBytes, formatCompact, formatCount, formatDuration, plural } from "../store/format";
import { KIND_DIR } from "../store/tree";
import { SurveyGrid } from "../viz/survey";
import { TreemapView } from "../viz/treemap";
import type { Controller } from "./controller";
import { HostChip } from "./HostChip";
import { Back } from "./icons";
import { prefersReducedMotion } from "./motion";
import { TreeList } from "./TreeList";
import { Wordmark } from "./Wordmark";

interface ZoomState {
  dz: number;
  scan: number;
}

let pushed = 0;

export function goToLauncher(): void {
  history.go(-(pushed + 1));
}

export function zoomTo(ctl: Controller, node: number): void {
  if (node === ctl.zoom.peek()) return;
  ctl.zoom.value = node;
  history.pushState({ dz: node, scan: ctl.store.scanId } satisfies ZoomState, "");
  pushed++;
}

export function zoomOut(ctl: Controller): void {
  const z = ctl.zoom.peek();
  const st = ctl.store;
  if (z === st.root || z < 0) return;
  if (pushed > 0) history.back();
  else ctl.zoom.value = st.parent[z];
}

export function Results({ ctl, embedded }: { ctl: Controller; embedded: boolean }) {
  const mapRef = useRef<HTMLDivElement>(null);
  const surveyRef = useRef<HTMLCanvasElement>(null);

  useEffect(() => {
    const reduced = prefersReducedMotion();
    const survey = new SurveyGrid(surveyRef.current!, { reducedMotion: reduced, alpha: 0.5 });
    const view = new TreemapView(mapRef.current!, ctl.store, {
      interactive: true,
      reducedMotion: reduced,
      units: () => ctl.units.peek(),
      backlog: () => ctl.ingest.backlog,
      onHover: (n) => (ctl.hovered.value = n),
      onSelect: (n) => (ctl.selected.value = n),
      onZoom: (n) => {
        zoomTo(ctl, n);
        ctl.selected.value = n;
      },
    });
    const syncSurvey = () => survey.setRunning(ctl.phase.peek() === "scanning" && ctl.ingest.backlog === 0);
    syncSurvey();
    view.reset(ctl.zoom.peek() >= 0 ? ctl.zoom.peek() : ctl.store.root);
    const offs = [
      ctl.onStore(() => {
        view.invalidate();
        syncSurvey();
      }),
      effect(() => {
        ctl.phase.value;
        syncSurvey();
      }),
      ctl.onReset(() => {
        view.reset(ctl.store.root);
      }),
      effect(() => view.setRoot(ctl.zoom.value, true)),
      effect(() => view.setSelected(ctl.selected.value)),
      effect(() => view.setHovered(ctl.hovered.value)),
      effect(() => {
        ctl.units.value;
        view.refreshLabels();
      }),
    ];
    const onPop = (e: PopStateEvent) => {
      if (pushed > 0) pushed--;
      const s = e.state as ZoomState | null;
      const st = ctl.store;
      ctl.zoom.value = s && s.scan === st.scanId && s.dz < st.n ? s.dz : st.root;
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !(e.target instanceof HTMLInputElement)) zoomOut(ctl);
    };
    window.addEventListener("popstate", onPop);
    window.addEventListener("keydown", onKey);
    return () => {
      offs.forEach((f) => f());
      window.removeEventListener("popstate", onPop);
      window.removeEventListener("keydown", onKey);
      view.destroy();
      survey.destroy();
    };
  }, [ctl]);

  return (
    <div class="results">
      <TopBar ctl={ctl} embedded={embedded} />
      <div class="stage">
        <div class="map" role="region" aria-label="Treemap of disk usage. Click a folder to zoom in, Escape to zoom out.">
          <canvas class="survey" ref={surveyRef} aria-hidden="true" />
          <div class="tm-host" ref={mapRef} />
        </div>
        {/* absolute over the map on desktop; in flow between map and list on mobile */}
        <Meter ctl={ctl} />
        <TreeList ctl={ctl} />
      </div>
    </div>
  );
}

function TopBar({ ctl, embedded }: { ctl: Controller; embedded: boolean }) {
  const st = ctl.store;
  ctl.version.value;
  const phase = ctl.phase.value;
  const z = ctl.zoom.value;
  const units = ctl.units.value;
  const scan = ctl.scan.value;
  const prog = ctl.progress.value;
  const info = ctl.info.value;
  const scanned = phase === "scanning" ? Math.max(prog?.alloc ?? 0, st.root >= 0 ? st.totAlloc[st.root] : 0) : st.root >= 0 ? st.totAlloc[st.root] : 0;
  const volUsed = scan?.volume?.used ?? 0;
  const frac = volUsed > 0 ? Math.min(1, scanned / volUsed) : 0;

  const stretch = useCallback(() => {
    const s = ctl.scan.peek();
    // Progress against the volume only means something when the whole
    // volume is being scanned; a folder scan breathes, then settles.
    const wholeVolume = !!s && (ctl.info.peek()?.volumes ?? []).some((v) => v.path === s.path);
    const used = wholeVolume ? (s?.volume?.used ?? 0) : 0;
    const p = ctl.progress.peek();
    const store = ctl.store;
    const bytes = Math.max(p?.alloc ?? 0, store.root >= 0 ? store.totAlloc[store.root] : 0);
    if (!used) return ctl.done.peek() ? 100 : null;
    return 50 + 100 * Math.min(1, bytes / used);
  }, [ctl]);

  const path = z >= 0 ? st.pathTo(z) : [];
  let crumbs: (number | null)[] = path;
  if (path.length > 5) crumbs = [path[0], null, ...path.slice(-3)];
  const zSize = z >= 0 ? st.size(z) + (z === st.root && st.synth >= 0 ? st.size(st.synth) : 0) : 0;

  return (
    <header class="topbar">
      {embedded && (
        <button class="btn btn-quiet" onClick={goToLauncher}>
          <Back /> Launcher
        </button>
      )}
      <span class="wm-slot">
        <Wordmark size="bar" stretch={stretch} label="diskspace.cc" fit={150} />
      </span>
      <nav class="crumbs" aria-label="Zoom path">
        {info && (
          <>
            <span class="host-crumb" title={`Computer: ${info.hostname}`}>
              {info.hostname}
            </span>
            <span class="sep" aria-hidden="true">
              ›
            </span>
          </>
        )}
        {crumbs.map((n, i) => (
          <>
            {i > 0 && (
              <span class="sep" aria-hidden="true">
                ›
              </span>
            )}
            {n === null ? (
              <button key="gap" onClick={() => zoomTo(ctl, path[path.length - 4])} title="Show more of the path">
                …
              </button>
            ) : (
              <button key={n} aria-current={n === z ? "page" : undefined} onClick={() => zoomTo(ctl, n)} title={st.fullPath(n)}>
                {st.names[n] || st.rootPath}
              </button>
            )}
          </>
        ))}
      </nav>
      <div class="total">
        <span class="total-num">{formatBytes(zSize, units)}</span>
        <span class="seg" role="group" aria-label="Units">
          <button aria-pressed={units === "decimal"} onClick={() => ctl.setUnits("decimal")} title="Decimal units, 1 GB = 1000³ bytes">
            GB
          </button>
          <button aria-pressed={units === "binary"} onClick={() => ctl.setUnits("binary")} title="Binary units, 1 GiB = 1024³ bytes">
            GiB
          </button>
        </span>
      </div>
      <div class="actions">
        {phase === "scanning" ? (
          <button class="btn" onClick={() => ctl.stop()}>
            Stop scan
          </button>
        ) : (
          <>
            <button class="btn" onClick={() => (ctl.picking.value = true)}>
              Change folder
            </button>
            <button class="btn btn-primary" onClick={() => ctl.rescan()}>
              Scan again
            </button>
          </>
        )}
      </div>
      <HostChip info={ctl.info.value} conn={ctl.conn.value} compact />
      <div class={`progress-line${phase !== "scanning" ? " done" : ""}`} aria-hidden="true">
        <i style={{ transform: `scaleX(${phase === "scanning" ? frac : 1})` }} />
      </div>
    </header>
  );
}

function Meter({ ctl }: { ctl: Controller }) {
  const st = ctl.store;
  ctl.version.value;
  const phase = ctl.phase.value;
  const prog = ctl.progress.value;
  const done = ctl.done.value;
  const info = ctl.info.value;
  const units = ctl.units.value;
  const err = ctl.protocolError.value;
  if (phase === "scanning") {
    const files = Math.max(prog?.files ?? 0, st.root >= 0 ? st.totFiles[st.root] : 0);
    const dirs = Math.max(prog?.dirs ?? 0, st.root >= 0 ? st.totDirs[st.root] + 1 : 0);
    return (
      <div class="meter" role="status" aria-live="off">
        <div class="meter-main">
          <b>{formatCompact(files)}</b> files · <b>{formatCompact(dirs)}</b> folders · {formatDuration(prog?.elapsedMs ?? 0)}
        </div>
        {prog?.currentPath && <div class="meter-path">{"‎" + prog.currentPath}</div>}
      </div>
    );
  }
  const total = st.root >= 0 ? st.totAlloc[st.root] : 0;
  const files = done?.totals.files ?? (st.root >= 0 ? st.totFiles[st.root] : 0);
  // denied = dirs we were refused; count from SCAN_DONE (authoritative) or the store
  const denied = done ? done.denied : st.deniedDirs;
  const reasons: string[] = [];
  if (info && !info.elevated) reasons.push("not running as administrator");
  if (info?.os === "darwin" && info.fda === "denied") reasons.push("Full Disk Access off");
  const how = info?.os === "windows" ? "Run diskspace again and choose Restart as administrator when it asks." : "Run diskspace again and answer y when it offers to restart as administrator.";
  return (
    <div class="meter" role="status">
      <div class="meter-main">
        {phase === "stopped" ? "Scan stopped: " : ""}
        <b>{formatBytes(total, units)}</b> in {plural(files, "file", "files")}
        {denied > 0 && (
          <>
            {" · "}
            <span class="meter-warn has-hint hint-up hint-left" tabIndex={0}>
              {formatCount(denied)} {denied === 1 ? "folder" : "folders"} couldn't be read{reasons.length ? ` (${reasons.join(", ")})` : ""}
              <span class="hint" role="tooltip">
                {how}
                {info?.os === "darwin" && info.fda === "denied" && <span class="muted">For Mail, Messages and Safari data, also allow {info.launcher === "app" ? "Diskspace" : "your terminal app"} under System Settings › Privacy &amp; Security › Full Disk Access.</span>}
              </span>
            </span>
          </>
        )}
      </div>
      {phase === "stopped" && <div class="meter-path">Partial results. Scan again to finish.</div>}
      {err && <div class="meter-path meter-warn">Some data arrived out of order ({err}). Scan again if totals look wrong.</div>}
    </div>
  );
}

export function isZoomable(ctl: Controller, n: number): boolean {
  const st = ctl.store;
  return st.kind[n] === KIND_DIR && st.hasChildren(n) && !(st.flags[n] & DIR_FLAG.READ_DENIED);
}
