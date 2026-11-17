// SPDX-License-Identifier: MIT
//
// Virtualized tree list (rail). Fixed 28 px rows, a pool of DOM rows reused
// on scroll, children sorted by allocated size. Mirrors the treemap's zoom
// root; selection and hover are shared with the map through the controller.

import { effect } from "@preact/signals";
import { useEffect, useRef } from "preact/hooks";
import { DIR_FLAG } from "../protocol/types";
import { formatBytes, formatCompact, formatCount, formatPercent, type Units } from "../store/format";
import { KIND_BUCKET, KIND_DIR, KIND_FILE, KIND_SYNTH, type TreeStore } from "../store/tree";
import { fileCategory, hueCss } from "../viz/colors";
import { dirHue } from "../viz/layout";
import type { Controller } from "./controller";

const ROW = 28;
const INDENT = 14;
const CHEVRON = '<svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 3.5 10.5 8 6 12.5"/></svg>';

interface RowEls {
  el: HTMLDivElement;
  name: HTMLSpanElement;
  tw: HTMLButtonElement;
  label: HTMLSpanElement;
  bar: HTMLElement;
  more: HTMLButtonElement;
  size: HTMLSpanElement;
  pct: HTMLSpanElement;
  items: HTMLSpanElement;
  node: number;
  key: string;
}

/** branch hue of a node relative to the list/treemap zoom root */
function branchHue(st: TreeStore, node: number, root: number): number {
  if (st.kind[node] === KIND_FILE) return fileCategory(st.names[node]);
  if (st.kind[node] === KIND_SYNTH) return 4;
  let k = node;
  while (k >= 0 && st.parent[k] !== root) k = st.parent[k];
  if (k < 0) return 6;
  if (st.kind[k] !== KIND_DIR) return st.kind[k] === KIND_FILE ? fileCategory(st.names[k]) : 6;
  return dirHue(st, k);
}

export class ListView {
  private rows: RowEls[] = [];
  private flat = new Int32Array(1024);
  private depthOf = new Uint8Array(1024);
  private count = 0;
  private expanded = new Set<number>();
  private sortCache = new Map<number, number[]>();
  private sortVersion = -1;
  private hueCache = new Map<number, number>();
  private sizer: HTMLDivElement;
  private root = -1;
  private rebuildTimer = 0;
  private lastRebuild = 0;
  private ro: ResizeObserver;
  private disposers: (() => void)[] = [];
  private loadingFiles = new Set<number>();

  constructor(
    private host: HTMLDivElement,
    private ctl: Controller,
  ) {
    this.sizer = document.createElement("div");
    this.sizer.className = "list-sizer";
    host.appendChild(this.sizer);
    host.addEventListener("scroll", () => this.render(), { passive: true });
    host.addEventListener("keydown", this.onKey);
    host.addEventListener("pointerleave", () => {
      if (this.ctl.hovered.peek() >= 0) this.ctl.hovered.value = -1;
    });
    this.ro = new ResizeObserver(() => this.render());
    this.ro.observe(host);
    this.disposers.push(ctl.onStore(() => this.schedule()));
    this.disposers.push(
      ctl.onReset(() => {
        this.expanded.clear();
        this.loadingFiles.clear();
        host.scrollTop = 0;
      }),
    );
    this.disposers.push(
      effect(() => {
        const z = ctl.zoom.value;
        if (z !== this.root) {
          this.root = z;
          this.hueCache.clear();
          host.scrollTop = 0;
          this.rebuild();
        }
      }),
    );
    this.disposers.push(
      effect(() => {
        const sel = ctl.selected.value;
        ctl.units.value;
        this.reveal(sel);
        this.render(true);
      }),
    );
    this.disposers.push(
      effect(() => {
        ctl.hovered.value;
        this.render();
      }),
    );
  }

  destroy(): void {
    this.ro.disconnect();
    window.clearTimeout(this.rebuildTimer);
    for (const d of this.disposers) d();
    this.host.replaceChildren();
  }

  // ---- data -----------------------------------------------------------------

  private sorted = (n: number): number[] => {
    const st = this.ctl.store;
    if (this.sortVersion !== st.version) {
      this.sortCache.clear();
      this.hueCache.clear();
      this.sortVersion = st.version;
    }
    let s = this.sortCache.get(n);
    if (!s) {
      s = st.sortedChildren(n).filter((c) => st.size(c) > 0 || st.kind[c] === KIND_DIR);
      this.sortCache.set(n, s);
    }
    return s;
  };

  private hue(n: number): number {
    let h = this.hueCache.get(n);
    if (h === undefined) {
      h = branchHue(this.ctl.store, n, this.root);
      this.hueCache.set(n, h);
    }
    return h;
  }

  private schedule(): void {
    if (this.rebuildTimer) return;
    const due = Math.max(0, this.lastRebuild + 300 - performance.now());
    this.rebuildTimer = window.setTimeout(() => {
      this.rebuildTimer = 0;
      this.rebuild();
    }, due);
  }

  private push(n: number, d: number): void {
    if (this.count >= this.flat.length) {
      const f = new Int32Array(this.flat.length * 2);
      f.set(this.flat);
      this.flat = f;
      const g = new Uint8Array(this.depthOf.length * 2);
      g.set(this.depthOf);
      this.depthOf = g;
    }
    this.flat[this.count] = n;
    this.depthOf[this.count++] = d;
  }

  private walk(n: number, d: number): void {
    for (const c of this.sorted(n)) {
      this.push(c, d);
      if (this.expanded.has(c)) this.walk(c, d + 1);
    }
  }

  rebuild(): void {
    this.lastRebuild = performance.now();
    this.count = 0;
    if (this.root >= 0 && this.ctl.store.root >= 0) this.walk(this.root, 0);
    this.sizer.style.height = `${this.count * ROW}px`;
    this.render(true);
  }

  private reveal(node: number): void {
    const st = this.ctl.store;
    if (node < 0 || this.root < 0 || !st.isAncestor(this.root, node) || node === this.root) return;
    let changed = false;
    for (let k = st.parent[node]; k >= 0 && k !== this.root; k = st.parent[k]) {
      if (!this.expanded.has(k)) {
        this.expanded.add(k);
        changed = true;
      }
    }
    if (changed) this.rebuild();
    const i = this.indexOf(node);
    if (i >= 0) {
      const top = i * ROW, h = this.host.clientHeight;
      if (top < this.host.scrollTop) this.host.scrollTop = top - ROW;
      else if (top + ROW > this.host.scrollTop + h) this.host.scrollTop = top + 2 * ROW - h;
    }
  }

  private indexOf(node: number): number {
    for (let i = 0; i < this.count; i++) if (this.flat[i] === node) return i;
    return -1;
  }

  toggle(node: number, open?: boolean): void {
    const isOpen = this.expanded.has(node);
    const want = open ?? !isOpen;
    if (want === isOpen) return;
    if (want) this.expanded.add(node);
    else this.expanded.delete(node);
    this.rebuild();
  }

  // ---- DOM ------------------------------------------------------------------

  private makeRow(i: number): RowEls {
    const el = document.createElement("div");
    el.className = "row";
    el.hidden = true;
    el.id = `lr${i}`;
    el.setAttribute("role", "treeitem");
    const name = document.createElement("span");
    name.className = "name";
    const tw = document.createElement("button");
    tw.className = "tw";
    tw.tabIndex = -1;
    tw.innerHTML = CHEVRON;
    tw.setAttribute("aria-label", "Expand");
    const label = document.createElement("span");
    label.className = "label";
    const more = document.createElement("button");
    more.className = "more";
    more.tabIndex = -1;
    more.textContent = "Show all";
    more.hidden = true;
    const bar = document.createElement("i");
    bar.className = "bar";
    name.append(tw, label, more, bar);
    const size = document.createElement("span");
    size.className = "num size";
    const pct = document.createElement("span");
    pct.className = "num pct";
    const items = document.createElement("span");
    items.className = "num items";
    el.append(name, size, pct, items);
    const r: RowEls = { el, name, tw, label, bar, more, size, pct, items, node: -1, key: "" };
    tw.addEventListener("click", (e) => {
      e.stopPropagation();
      if (r.node >= 0) this.toggle(r.node);
    });
    more.addEventListener("click", (e) => {
      e.stopPropagation();
      const p = this.ctl.store.parent[r.node];
      if (p < 0 || this.loadingFiles.has(p)) return;
      this.loadingFiles.add(p);
      more.textContent = "Loading…";
      this.ctl
        .showAllFiles(p)
        .catch(() => undefined)
        .finally(() => {
          this.loadingFiles.delete(p);
          r.key = "";
          this.render(true);
        });
    });
    el.addEventListener("click", () => {
      if (r.node < 0) return;
      this.ctl.selected.value = r.node;
      this.host.focus({ preventScroll: true });
    });
    el.addEventListener("dblclick", () => {
      if (r.node >= 0) this.zoomInto(r.node);
    });
    el.addEventListener("pointerenter", () => {
      if (r.node >= 0) this.ctl.hovered.value = r.node;
    });
    this.host.appendChild(el);
    return r;
  }

  private zoomInto(node: number): void {
    const st = this.ctl.store;
    if (st.kind[node] === KIND_DIR && st.hasChildren(node) && !(st.flags[node] & DIR_FLAG.READ_DENIED)) this.ctl.zoom.value = node;
  }

  render(force = false): void {
    const st = this.ctl.store;
    const units: Units = this.ctl.units.peek();
    const sel = this.ctl.selected.peek();
    const hov = this.ctl.hovered.peek();
    const h = this.host.clientHeight || 600;
    const first = Math.max(0, Math.floor(this.host.scrollTop / ROW) - 4);
    const need = Math.ceil(h / ROW) + 9;
    while (this.rows.length < need) this.rows.push(this.makeRow(this.rows.length));
    let activeId = "";
    for (let k = 0; k < this.rows.length; k++) {
      const r = this.rows[k];
      const i = first + k;
      if (i >= this.count || k >= need) {
        if (r.node !== -1) {
          r.el.hidden = true;
          r.node = -1;
          r.key = "";
        }
        continue;
      }
      const n = this.flat[i];
      const d = this.depthOf[i];
      if (n === sel) activeId = r.el.id;
      const key = `${i}:${n}:${st.version}:${units}:${this.expanded.has(n) ? 1 : 0}:${n === sel ? 1 : 0}:${this.loadingFiles.has(st.parent[n]) ? 1 : 0}`;
      if (force || r.key !== key) {
        r.key = key;
        r.node = n;
        r.el.hidden = false;
        r.el.style.transform = `translateY(${i * ROW}px)`;
        this.fill(r, n, d, units, n === sel);
      }
      r.el.classList.toggle("hov", n === hov);
    }
    if (activeId) this.host.setAttribute("aria-activedescendant", activeId);
    else this.host.removeAttribute("aria-activedescendant");
  }

  private fill(r: RowEls, n: number, d: number, units: Units, selected: boolean): void {
    const st = this.ctl.store;
    const kind = st.kind[n];
    const isDir = kind === KIND_DIR;
    const denied = isDir && (st.flags[n] & DIR_FLAG.READ_DENIED) !== 0;
    const other = isDir && st.childFlags(n) !== 0;
    const expandable = isDir && st.hasChildren(n) && !denied;
    const open = expandable && this.expanded.has(n);
    const size = st.size(n);
    const p = st.parent[n];
    const pSize = p >= 0 ? (p === st.root && st.synth >= 0 ? st.size(p) + st.size(st.synth) : st.size(p)) : 0;
    r.el.className = `row${isDir ? " dir" : ""}${open ? " open" : ""}${selected ? " sel" : ""}${kind === KIND_BUCKET || denied || other ? " muted" : ""}`;
    r.el.setAttribute("aria-level", String(d + 1));
    r.el.setAttribute("aria-selected", selected ? "true" : "false");
    if (expandable) r.el.setAttribute("aria-expanded", open ? "true" : "false");
    else r.el.removeAttribute("aria-expanded");
    r.name.style.paddingLeft = `${d * INDENT}px`;
    r.tw.className = expandable ? "tw" : "tw leaf";
    r.tw.setAttribute("aria-label", open ? "Collapse" : "Expand");

    let label: string;
    if (kind === KIND_BUCKET) {
      const f = st.files(n);
      label = f > 0 ? `${formatCount(f)} other ${f === 1 ? "file" : "files"}` : "Other files";
    } else label = st.names[n] || st.rootPath;
    if (denied) label += " (no access)";
    else if (other) label += st.childFlags(n) & 1 ? " (other volume)" : " (skipped)";
    r.label.textContent = label;
    r.label.title = kind === KIND_SYNTH ? "Used on this volume but outside the scan: other volumes, local snapshots, purgeable space" : label;

    const showMore = kind === KIND_BUCKET && st.hiddenFiles(p) > 0 && st.hasRecord(p);
    r.more.hidden = !showMore;
    if (showMore) r.more.textContent = this.loadingFiles.has(p) ? "Loading…" : "Show all";

    const frac = pSize > 0 ? size / pSize : 0;
    const hue = this.hue(n);
    r.bar.style.left = `${d * INDENT + 20}px`;
    r.bar.style.width = `calc((100% - ${d * INDENT + 20}px) * ${frac.toFixed(4)})`;
    r.bar.style.background = hueCss(hue, Math.max(0, st.depth[n] - st.depth[this.root] - 1));

    r.size.textContent = size > 0 || !isDir ? formatBytes(size, units) : st.hasRecord(n) ? formatBytes(0, units) : "…";
    r.size.className = `num size${isDir && !st.isSettled(n) ? " pending" : ""}`;
    const app = st.apparent(n);
    r.size.title = `${formatBytes(size, units)} on disk, ${formatBytes(app, units)} apparent`;
    r.pct.textContent = pSize > 0 ? formatPercent(size, pSize) : "";
    if (isDir && !denied) {
      const items = st.files(n) + st.dirs(n);
      r.items.textContent = items > 0 ? formatCompact(items) : "";
      r.items.title = `${formatCount(st.files(n))} files, ${formatCount(st.dirs(n))} folders`;
    } else if (kind === KIND_BUCKET) {
      r.items.textContent = formatCompact(st.files(n));
      r.items.title = "";
    } else {
      r.items.textContent = "";
      r.items.title = "";
    }
  }

  // ---- keyboard ---------------------------------------------------------------

  private onKey = (e: KeyboardEvent): void => {
    if (this.count === 0) return;
    const st = this.ctl.store;
    const sel = this.ctl.selected.peek();
    let i = this.indexOf(sel);
    const go = (j: number) => {
      j = Math.max(0, Math.min(this.count - 1, j));
      this.ctl.selected.value = this.flat[j];
    };
    switch (e.key) {
      case "ArrowDown":
        go(i < 0 ? 0 : i + 1);
        break;
      case "ArrowUp":
        go(i < 0 ? 0 : i - 1);
        break;
      case "PageDown":
        go((i < 0 ? 0 : i) + Math.floor(this.host.clientHeight / ROW));
        break;
      case "PageUp":
        go((i < 0 ? 0 : i) - Math.floor(this.host.clientHeight / ROW));
        break;
      case "Home":
        go(0);
        break;
      case "End":
        go(this.count - 1);
        break;
      case "ArrowRight":
        if (i < 0) return go(0);
        if (st.kind[sel] === KIND_DIR && st.hasChildren(sel)) {
          if (!this.expanded.has(sel)) this.toggle(sel, true);
          else if (i + 1 < this.count && this.depthOf[i + 1] > this.depthOf[i]) go(i + 1);
        }
        break;
      case "ArrowLeft":
        if (i < 0) return go(0);
        if (this.expanded.has(sel)) this.toggle(sel, false);
        else {
          const p = st.parent[sel];
          if (p >= 0 && p !== this.root) this.ctl.selected.value = p;
        }
        break;
      case "Enter":
        if (sel >= 0) {
          if (st.kind[sel] === KIND_DIR) this.zoomInto(sel);
          else if (st.parent[sel] >= 0 && st.parent[sel] !== this.root) this.zoomInto(st.parent[sel]);
        }
        break;
      default:
        return;
    }
    e.preventDefault();
  };
}

export function TreeList({ ctl }: { ctl: Controller }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const v = new ListView(ref.current!, ctl);
    return () => v.destroy();
  }, [ctl]);
  const st = ctl.store;
  ctl.version.value;
  const z = ctl.zoom.value;
  const units = ctl.units.value;
  const zName = z >= 0 ? st.names[z] || st.rootPath : "";
  const zSize = z >= 0 ? st.size(z) + (z === st.root && st.synth >= 0 ? st.size(st.synth) : 0) : 0;
  return (
    <aside class="rail" aria-label="Folder list">
      <div class="rail-head">
        <div class="rail-title">
          <h2 title={z >= 0 ? st.fullPath(z) : ""}>{zName}</h2>
          <span>{formatBytes(zSize, units)}</span>
        </div>
      </div>
      <div class="cols" aria-hidden="true">
        <span>Name</span>
        <span>Size</span>
        <span>%</span>
        <span>Items</span>
      </div>
      <div class="list" ref={ref} role="tree" tabIndex={0} aria-label={`Contents of ${zName}`} />
      <Legend />
    </aside>
  );
}

function Legend() {
  const cats: [string, number][] = [
    ["Video", 5],
    ["Images", 2],
    ["Audio", 3],
    ["Archives", 0],
    ["Code", 1],
    ["Documents", 4],
    ["Apps", 7],
  ];
  return (
    <div class="legend" aria-label="File colours">
      {cats.map(([n, h]) => (
        <span key={n}>
          <i style={{ background: hueCss(h, 0) }} />
          {n}
        </span>
      ))}
      <span>
        <i class="hatch" />
        Other files
      </span>
      <span>
        <i class="dash" />
        No access
      </span>
    </div>
  );
}
