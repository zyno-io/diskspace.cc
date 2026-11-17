// SPDX-License-Identifier: MIT
//
// Scan-target picker: the one card on the plane (it is the interaction
// container). Order: current folder, home, volumes, other folder… (browse).

import { useEffect, useRef, useState } from "preact/hooks";
import type { BrowseResult, Volume } from "../protocol/types";
import { formatBytes, type Units } from "../store/format";
import type { Controller } from "./controller";
import { Back, Folder } from "./icons";

const VOL_HUE: Record<Volume["kind"], string> = { boot: "var(--ochre)", internal: "var(--sandstone)", external: "var(--glacier)", network: "var(--lichen)" };
const VOL_KIND: Record<Volume["kind"], string> = { boot: "Startup disk", internal: "Internal", external: "External", network: "Network" };

export function Picker({ ctl }: { ctl: Controller }) {
  const info = ctl.info.value!;
  const units = ctl.units.value;
  const [browsing, setBrowsing] = useState<string | null>(null);
  const hasResults = !!ctl.scan.value;
  if (browsing !== null) return <Browse ctl={ctl} start={browsing} onCancel={() => setBrowsing(null)} />;
  return (
    <section class="card" aria-labelledby="pick-title">
      <div class="card-head">
        <h1 id="pick-title">Choose what to scan</h1>
      </div>
      <div class="card-body">
        {info.cwd && <Opt label="Current folder" path={info.cwd.path} onClick={() => ctl.scanPath(info.cwd!.path)} />}
        <Opt label="Home" path={info.home} onClick={() => ctl.scanPath(info.home)} />
        {info.volumes.length > 0 && <div class="opt-sep" role="separator" />}
        {info.volumes.map((v) => (
          <VolumeOpt key={v.path} v={v} units={units} onClick={() => ctl.scanPath(v.path)} />
        ))}
        <div class="opt-sep" role="separator" />
        <button class="opt opt-more" onClick={() => setBrowsing(info.cwd?.path ?? info.home)}>
          <span class="opt-label">Other folder…</span>
        </button>
      </div>
      {hasResults && (
        <div class="card-foot">
          <button class="btn btn-quiet" onClick={() => (ctl.picking.value = false)}>
            <Back /> Back to results
          </button>
        </div>
      )}
    </section>
  );
}

function Opt({ label, path, onClick }: { label: string; path: string; onClick(): void }) {
  return (
    <button class="opt" onClick={onClick}>
      <span class="opt-label">{label}</span>
      <span class="opt-path" title={path}>
        {path}
      </span>
    </button>
  );
}

function VolumeOpt({ v, units, onClick }: { v: Volume; units: Units; onClick(): void }) {
  const frac = v.total > 0 ? Math.min(1, v.used / v.total) : 0;
  return (
    <button class="opt" onClick={onClick} aria-label={`${v.label}, ${formatBytes(v.used, units)} of ${formatBytes(v.total, units)} used`}>
      <span class="opt-label">{v.label || v.path}</span>
      <span class="opt-meta">
        {formatBytes(v.used, units)} of {formatBytes(v.total, units)}
      </span>
      <span class="opt-path">
        {VOL_KIND[v.kind]}
        {v.path !== "/" ? `, ${v.path}` : ""}
        {v.readOnly ? ", read-only" : ""}
      </span>
      <span class="opt-bar" aria-hidden="true">
        <i style={{ width: `${(frac * 100).toFixed(1)}%`, background: VOL_HUE[v.kind] }} />
      </span>
    </button>
  );
}

function splitPath(p: string): { label: string; path: string }[] {
  const win = /^[A-Za-z]:[\\/]/.test(p) || p.includes("\\");
  const sep = win ? "\\" : "/";
  const parts = p.split(/[\\/]+/).filter(Boolean);
  const out: { label: string; path: string }[] = [];
  if (!win) out.push({ label: "/", path: "/" });
  let acc = "";
  for (const part of parts) {
    acc = win ? (acc ? acc + sep + part : part + sep) : acc + "/" + part;
    out.push({ label: part, path: acc });
  }
  return out;
}

function Browse({ ctl, start, onCancel }: { ctl: Controller; start: string; onCancel(): void }) {
  const [path, setPath] = useState(start);
  const [res, setRes] = useState<BrowseResult | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const listRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    let live = true;
    setLoading(true);
    setErr(null);
    ctl.session
      .browse(path)
      .then((r) => {
        if (!live) return;
        setRes(r);
        setErr(r.error ?? null);
        setLoading(false);
        listRef.current?.scrollTo(0, 0);
      })
      .catch((e: unknown) => {
        if (!live) return;
        setErr(e instanceof Error ? e.message : String(e));
        setLoading(false);
      });
    return () => {
      live = false;
    };
  }, [path]);
  const cur = res?.path ?? path;
  const crumbs = splitPath(cur);
  const join = (name: string) => (cur.endsWith("/") || cur.endsWith("\\") ? cur + name : cur + (cur.includes("\\") ? "\\" : "/") + name);
  return (
    <section class="card" aria-labelledby="browse-title">
      <div class="card-head">
        <h1 id="browse-title">Other folder</h1>
      </div>
      <nav class="crumbs-browse" aria-label="Folder path">
        {crumbs.map((c, i) => (
          <>
            {i > 0 && !(i === 1 && crumbs[0].label === "/") && <span class="sep">{cur.includes("\\") ? "\\" : "/"}</span>}
            <button key={c.path} onClick={() => setPath(c.path)} aria-current={i === crumbs.length - 1 ? "location" : undefined}>
              {c.label}
            </button>
          </>
        ))}
      </nav>
      <div class="dirlist" ref={listRef} aria-busy={loading}>
        {err && <div class="empty">Can't open this folder: {err}</div>}
        {!err && res && res.dirs.length === 0 && !loading && <div class="empty">No folders inside.</div>}
        {!err &&
          res?.dirs.map((d) => (
            <button key={d} onClick={() => setPath(join(d))}>
              <Folder />
              <span>{d}</span>
            </button>
          ))}
      </div>
      <div class="card-foot">
        <button class="btn btn-quiet" onClick={onCancel}>
          <Back /> Back
        </button>
        <span class="browse-path" title={cur}>
          {cur}
        </span>
        <button class="btn btn-primary" disabled={!!err} onClick={() => ctl.scanPath(cur)}>
          Scan this folder
        </button>
      </div>
    </section>
  );
}

