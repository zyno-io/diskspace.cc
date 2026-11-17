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
}

export function Wordmark({ stretch, size, as = "div", href, label, fit }: WordmarkProps) {
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
    if (!el || !stretch) return;
    const reduced = prefersReducedMotion();
    let cur = stretch() ?? 100;
    let raf = 0;
    let last = -1;
    const tick = (now: number) => {
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
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [stretch]);
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
