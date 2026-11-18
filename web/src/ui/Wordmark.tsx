// SPDX-License-Identifier: MIT
//
// Motion 3, "wordmark stretch": the width axis follows scan progress
// (50 → 150) or breathes 70 ↔ 110 when progress is unknown.

import { useEffect, useRef } from "preact/hooks";
import { prefersReducedMotion } from "./motion";

export interface WordmarkProps {
  /** returns target wdth (50–150), or null to breathe */
  stretch?: () => number | null;
  size: "hero" | "bar";
  as?: "h1" | "div" | "a";
  href?: string;
  label?: string;
  /** shrink the font so the mark, stretched to this wdth, fits its parent */
  fit?: number;
  /** freeze the current width (a paused scan) */
  hold?: boolean;
}

export function Wordmark({ stretch, size, as = "div", href, label, fit, hold }: WordmarkProps) {
  const ref = useRef<HTMLElement>(null);
  useEffect(() => {
    const el = ref.current;
    const parent = el?.parentElement;
    if (!fit || !el || !parent) return;
    const measure = () => {
      const keep = el.style.fontStretch;
      el.style.fontSize = "";
      el.style.fontStretch = `${fit}%`;
      const cs = getComputedStyle(parent);
      const avail = parent.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
      const w = el.scrollWidth;
      if (w > avail && avail > 0) el.style.fontSize = `${(parseFloat(getComputedStyle(el).fontSize) * avail) / w}px`;
      el.style.fontStretch = keep;
    };
    const ro = new ResizeObserver(measure);
    ro.observe(parent);
    document.fonts?.ready.then(measure).catch(() => undefined);
    return () => ro.disconnect();
  }, [fit]);
  useEffect(() => {
    const el = ref.current;
    if (!el || !stretch || hold) return;
    const reduced = prefersReducedMotion();
    const shown = parseFloat(el.style.fontStretch);
    let cur = Number.isFinite(shown) ? shown : (stretch() ?? 100);
    let raf = 0;
    let poll: ReturnType<typeof setTimeout> | undefined;
    let last = -1;
    const tick = (now: number) => {
      raf = 0;
      const target = stretch();
      let v: number;
      if (reduced) v = target ?? 100;
      else if (target === null) v = 90 + 20 * Math.sin(now / 1500);
      else {
        cur += (target - cur) * 0.06;
        v = cur;
      }
      cur = v;
      const r = Math.round(v * 10) / 10;
      if (r !== last) {
        el.style.fontStretch = `${r}%`;
        last = r;
      }
      if (target !== null && Math.abs(target - v) < 0.05) {
        // converged: stop the frame loop, watch for a new target at a low rate
        cur = target;
        poll = setTimeout(function watch() {
          const t = stretch();
          if (t !== null && Math.abs(t - cur) < 0.05) poll = setTimeout(watch, 250);
          else raf = requestAnimationFrame(tick);
        }, 250);
        return;
      }
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => {
      cancelAnimationFrame(raf);
      clearTimeout(poll);
    };
  }, [stretch, hold]);
  const cls = `wm wm-${size}`;
  const inner = (
    <>
      diskspace<span class="cc">.cc</span>
    </>
  );
  if (as === "a")
    return (
      <a ref={ref as never} class={cls} href={href} aria-label={label ?? "diskspace.cc"}>
        {inner}
      </a>
    );
  if (as === "h1")
    return (
      <h1 ref={ref as never} class={cls}>
        {inner}
      </h1>
    );
  return (
    <div ref={ref as never} class={cls} aria-label={label}>
      {inner}
    </div>
  );
}
