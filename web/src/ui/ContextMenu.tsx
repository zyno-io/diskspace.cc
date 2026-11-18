// SPDX-License-Identifier: MIT
//
// Context menu for a treemap parcel or list row: queue for deletion, move to
// the Trash right away, copy the path. Disabled items say why underneath.

import { useEffect, useLayoutEffect, useRef, useState } from "preact/hooks";
import { formatBytes } from "../store/format";
import { KIND_BUCKET, KIND_SYNTH } from "../store/tree";
import type { Controller } from "./controller";
import { nodeReason, trashName } from "./deletion";

interface Item {
  id: string;
  label: string;
  reason: string | null;
  danger?: boolean;
  run(): void;
}

export function ContextMenu({ ctl }: { ctl: Controller }) {
  const m = ctl.menu.value;
  // re-render when what the reasons depend on changes
  ctl.queue.value;
  ctl.version.value;
  ctl.phase.value;
  ctl.request.value;
  ctl.info.value;
  const ref = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState<{ left: number; top: number } | null>(null);
  const opener = useRef<Element | null>(null);

  const close = (refocus: boolean) => {
    ctl.menu.value = null;
    const o = opener.current as HTMLElement | null;
    if (refocus && o && document.contains(o)) o.focus({ preventScroll: true });
  };

  // place inside the viewport, flipping left/up near the edges
  useLayoutEffect(() => {
    if (!m || !ref.current) return setPos(null);
    const el = ref.current;
    const w = el.offsetWidth, h = el.offsetHeight;
    const vw = window.innerWidth, vh = window.innerHeight;
    let left = m.x + 2, top = m.y + 2;
    if (left + w > vw - 8) left = Math.max(8, m.x - w - 2);
    if (top + h > vh - 8) top = Math.max(8, m.y - h - 2);
    setPos({ left, top });
  }, [m?.node, m?.x, m?.y]);

  useEffect(() => {
    if (!m) return;
    opener.current = document.activeElement;
    const el = ref.current;
    const first = el?.querySelector<HTMLElement>('[role="menuitem"]');
    if (m.keyboard) first?.focus({ preventScroll: true });
    else el?.focus({ preventScroll: true });
    const outside = (e: Event) => {
      if (el && !el.contains(e.target as Node)) close(false);
    };
    const dismiss = () => close(false);
    document.addEventListener("pointerdown", outside, true);
    document.addEventListener("contextmenu", outside, true);
    window.addEventListener("resize", dismiss);
    window.addEventListener("blur", dismiss);
    document.addEventListener("wheel", dismiss, { passive: true, capture: true });
    return () => {
      document.removeEventListener("pointerdown", outside, true);
      document.removeEventListener("contextmenu", outside, true);
      window.removeEventListener("resize", dismiss);
      window.removeEventListener("blur", dismiss);
      document.removeEventListener("wheel", dismiss, { capture: true });
    };
  }, [m?.node, m?.x, m?.y]);

  if (!m) return null;
  const st = ctl.store;
  const n = m.node;
  if (n < 0 || n >= st.n) return null;
  const info = ctl.info.value;
  const trash = trashName(info?.os);
  const kind = st.kind[n];
  const queued = ctl.isQueued(n);
  const feature = ctl.deleteUnavailable.peek();
  const own = nodeReason(st, n, info);
  const items: Item[] = [
    queued
      ? { id: "dq", label: "Remove from queue", reason: null, run: () => ctl.removeFromQueue(n) }
      : {
          id: "dq",
          label: "Add to deletion queue",
          reason: ctl.queueReason(n),
          run: () => {
            const why = ctl.addToQueue(n);
            if (why) ctl.flash(why);
          },
        },
    {
      id: "now",
      label: `Move to ${trash} now…`,
      danger: true,
      reason: feature ?? own ?? ctl.applyReason() ?? (ctl.canTrash() ? null : `The ${trash} isn't available on ${info?.hostname ?? "this computer"}`),
      run: () => {
        const why = ctl.trashNow(n);
        if (why) ctl.flash(why);
      },
    },
    {
      id: "copy",
      label: "Copy path",
      reason: kind === KIND_BUCKET || kind === KIND_SYNTH ? "No single path" : null,
      run: () => {
        const path = st.fullPath(n);
        navigator.clipboard?.writeText(path).then(
          () => ctl.flash("Path copied"),
          () => ctl.flash("Couldn't copy the path"),
        );
      },
    },
  ];

  const activate = (it: Item) => {
    if (it.reason) return;
    close(true);
    it.run();
  };

  const onKey = (e: KeyboardEvent) => {
    const btns = Array.from(ref.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? []);
    const i = btns.indexOf(document.activeElement as HTMLElement);
    const go = (j: number) => btns[(j + btns.length) % btns.length]?.focus();
    switch (e.key) {
      case "ArrowDown":
        go(i + 1);
        break;
      case "ArrowUp":
        go(i < 0 ? btns.length - 1 : i - 1);
        break;
      case "Home":
        go(0);
        break;
      case "End":
        go(btns.length - 1);
        break;
      case "Escape":
        close(true);
        break;
      case "Tab":
        close(true);
        break;
      default:
        return;
    }
    e.preventDefault();
    e.stopPropagation();
  };

  const name = st.names[n] || st.rootPath;
  return (
    <div
      ref={ref}
      class="ctx-menu"
      role="menu"
      tabIndex={-1}
      aria-label={`Actions for ${name}`}
      style={pos ? { left: `${pos.left}px`, top: `${pos.top}px` } : { left: `${m.x}px`, top: `${m.y}px`, visibility: "hidden" }}
      onKeyDown={onKey}
      onContextMenu={(e) => e.preventDefault()}
    >
      <div class="ctx-head" aria-hidden="true">
        <span class="ctx-name">{kind === KIND_BUCKET ? "Grouped files" : name}</span>
        <span class="ctx-size">{formatBytes(st.size(n), ctl.units.value)}</span>
      </div>
      {items.map((it) => (
        <div class="ctx-entry" key={it.id}>
          <button
            role="menuitem"
            class={`ctx-item${it.danger ? " danger" : ""}`}
            aria-disabled={it.reason ? "true" : undefined}
            aria-describedby={it.reason ? `ctx-why-${it.id}` : undefined}
            tabIndex={-1}
            onClick={() => activate(it)}
          >
            {it.label}
          </button>
          {it.reason && (
            <div class="ctx-reason" id={`ctx-why-${it.id}`}>
              {it.reason}
            </div>
          )}
        </div>
      ))}
    </div>
  );
}
