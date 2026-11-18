// SPDX-License-Identifier: MIT
//
// Long-press for touch and pen: fires after `ms` without moving more than
// 8 px, then swallows the click that the lift would otherwise produce.

export function longPress(el: HTMLElement, fire: (x: number, y: number, target: EventTarget | null) => void, ms = 500): () => void {
  let timer = 0;
  let x0 = 0, y0 = 0;
  let suppress = false;
  const cancel = () => {
    window.clearTimeout(timer);
    timer = 0;
  };
  const down = (e: PointerEvent) => {
    if (e.pointerType === "mouse" || !e.isPrimary) return;
    cancel();
    suppress = false;
    x0 = e.clientX;
    y0 = e.clientY;
    const target = e.target;
    timer = window.setTimeout(() => {
      timer = 0;
      suppress = true;
      fire(x0, y0, target);
    }, ms);
  };
  const move = (e: PointerEvent) => {
    if (timer && Math.hypot(e.clientX - x0, e.clientY - y0) > 8) cancel();
  };
  const click = (e: MouseEvent) => {
    if (!suppress) return;
    suppress = false;
    e.preventDefault();
    e.stopImmediatePropagation();
  };
  el.addEventListener("pointerdown", down);
  el.addEventListener("pointermove", move);
  el.addEventListener("pointerup", cancel);
  el.addEventListener("pointercancel", cancel);
  el.addEventListener("click", click, true);
  return () => {
    cancel();
    el.removeEventListener("pointerdown", down);
    el.removeEventListener("pointermove", move);
    el.removeEventListener("pointerup", cancel);
    el.removeEventListener("pointercancel", cancel);
    el.removeEventListener("click", click, true);
  };
}
