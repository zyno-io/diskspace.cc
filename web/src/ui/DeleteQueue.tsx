// SPDX-License-Identifier: MIT
//
// Deletion queue: the top-bar pill, the review drawer (over the list on
// desktop, a bottom sheet on phones) and the request dialog that follows a
// request through host approval, progress and the result. The viewer never
// confirms on its own: every request is approved on the scanned computer.

import { useEffect, useLayoutEffect, useRef, useState } from "preact/hooks";
import type { DeleteStatus } from "../protocol/types";
import { formatBytes, plural } from "../store/format";
import type { Controller, DeleteRequestView } from "./controller";
import { itemCodeText, itemWarnings, requestCodeText, trashName } from "./deletion";
import { Close, Trash } from "./icons";

function queueTotal(ctl: Controller): number {
  const st = ctl.store;
  let b = 0;
  for (const n of ctl.queue.value) b += st.size(n);
  return b;
}

export function QueuePill({ ctl }: { ctl: Controller }) {
  const q = ctl.queue.value;
  ctl.version.value;
  if (q.length === 0) return null;
  const units = ctl.units.value;
  const bytes = queueTotal(ctl);
  const open = ctl.drawerOpen.value;
  return (
    <button
      class="btn dq-pill"
      aria-expanded={open}
      aria-controls="dq"
      aria-label={`Deletion queue: ${plural(q.length, "item", "items")}, ${formatBytes(bytes, units)}`}
      onClick={() => {
        ctl.menu.value = null;
        ctl.drawerOpen.value = !open;
      }}
    >
      <Trash />
      <span>{q.length}</span>
      <span class="dq-pill-sep" aria-hidden="true">
        ·
      </span>
      <span class="dq-pill-size">{formatBytes(bytes, units)}</span>
    </button>
  );
}

/** parent folder of a node, for the muted second line */
function parentPath(ctl: Controller, n: number): string {
  const p = ctl.store.parent[n];
  return p >= 0 ? ctl.store.fullPath(p) : "";
}

export function DeleteQueue({ ctl }: { ctl: Controller }) {
  const open = ctl.drawerOpen.value;
  const q = ctl.queue.value;
  ctl.version.value;
  ctl.phase.value;
  ctl.request.value;
  const info = ctl.info.value;
  const units = ctl.units.value;
  const [confirm, setConfirm] = useState(false);
  const ref = useRef<HTMLElement>(null);
  useEffect(() => {
    if (open) ref.current?.focus({ preventScroll: true });
    else setConfirm(false);
  }, [open]);
  useEffect(() => {
    if (q.length === 0) setConfirm(false);
  }, [q.length]);
  if (!open) return null;

  const st = ctl.store;
  const host = info?.hostname ?? "this computer";
  const trash = trashName(info?.os);
  const bytes = queueTotal(ctl);
  const why = q.length === 0 ? null : ctl.applyReason();
  const canTrash = ctl.canTrash();
  const close = () => (ctl.drawerOpen.value = false);
  const send = (mode: "trash" | "permanent") => {
    const err = ctl.applyQueue(mode);
    if (err) ctl.flash(err);
    else setConfirm(false);
  };

  return (
    <>
      <div class="dq-scrim" onClick={close} aria-hidden="true" />
      <aside
        id="dq"
        class="dq"
        ref={ref}
        tabIndex={-1}
        aria-labelledby="dq-title"
        onKeyDown={(e) => {
          if (e.key === "Escape") {
            e.stopPropagation();
            if (confirm) setConfirm(false);
            else close();
          }
        }}
      >
        <div class="rail-head dq-head">
          <div class="rail-title">
            <h2 id="dq-title">Deletion queue</h2>
            <span>
              {plural(q.length, "item", "items")} · {formatBytes(bytes, units)}
            </span>
          </div>
          {q.length > 0 && (
            <button class="btn btn-quiet dq-clear" onClick={() => ctl.clearQueue()}>
              Clear
            </button>
          )}
          <button class="btn btn-quiet dq-close" aria-label="Close deletion queue" onClick={close}>
            <Close />
          </button>
        </div>
        <p class="dq-note">Nothing is deleted until you approve it on {host}.</p>
        {q.length === 0 ? (
          <p class="dq-empty">Nothing queued. Right-click a folder or file, or select it and press Delete.</p>
        ) : (
          <ul class="dq-list">
            {q.map((n) => {
              const warn = itemWarnings(st, n, info?.os ?? "");
              const name = st.names[n];
              return (
                <li class="dq-row" key={n}>
                  <button
                    class="dq-item"
                    title={st.fullPath(n)}
                    onClick={() => {
                      ctl.selected.value = n;
                    }}
                  >
                    <span class="dq-name">{name}</span>
                    <span class="dq-path">{"‎" + parentPath(ctl, n)}</span>
                    {warn.map((w) => (
                      <span class="dq-warn" key={w}>
                        {w}
                      </span>
                    ))}
                  </button>
                  <span class="dq-size">{formatBytes(st.size(n), units)}</span>
                  <button class="dq-x" aria-label={`Remove ${name} from the queue`} onClick={() => ctl.removeFromQueue(n)}>
                    <Close />
                  </button>
                </li>
              );
            })}
          </ul>
        )}
        {confirm ? (
          <div class="dq-foot dq-confirm" role="group" aria-labelledby="dq-confirm-title">
            <p id="dq-confirm-title">
              <b>
                Delete {plural(q.length, "item", "items")} ({formatBytes(bytes, units)}) permanently?
              </b>{" "}
              They skip the {trash} and can't be restored. You'll approve this on {host}.
            </p>
            <div class="dq-buttons">
              <button class="btn btn-danger" disabled={!!why} onClick={() => send("permanent")}>
                Delete permanently
              </button>
              <button class="btn btn-quiet" onClick={() => setConfirm(false)}>
                Cancel
              </button>
            </div>
          </div>
        ) : (
          <div class="dq-foot">
            <div class="dq-buttons">
              {canTrash && (
                <button class="btn btn-primary" disabled={q.length === 0 || !!why} onClick={() => send("trash")}>
                  Move to {trash}
                </button>
              )}
              <button class="btn btn-danger-quiet" disabled={q.length === 0 || !!why} onClick={() => setConfirm(true)}>
                Delete permanently…
              </button>
            </div>
            {why && <p class="dq-hint">{why}</p>}
          </div>
        )}
      </aside>
    </>
  );
}

// ---- request dialog -------------------------------------------------------------

function approveHow(confirm: string | undefined): string {
  switch (confirm) {
    case "app":
      return "Confirm in the diskspace app.";
    case "console":
      return "Type y in the diskspace console window.";
    default:
      return "Type y in the terminal where diskspace is running.";
  }
}

export function DeleteDialog({ ctl }: { ctl: Controller }) {
  const r = ctl.request.value;
  const ref = useRef<HTMLDialogElement>(null);
  // before paint, so an emptied dialog is never shown open
  useLayoutEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (r && !d.open) d.showModal();
    else if (!r && d.open) d.close();
  }, [!!r]);
  return (
    <dialog
      ref={ref}
      class="card dq-dialog"
      aria-labelledby="dqd-title"
      onCancel={(e) => {
        // Escape: dismiss a finished request; an open one needs an explicit cancel
        e.preventDefault();
        ctl.closeRequest();
      }}
    >
      {r && <DialogBody ctl={ctl} r={r} />}
    </dialog>
  );
}

function DialogBody({ ctl, r }: { ctl: Controller; r: DeleteRequestView }) {
  const info = ctl.info.value;
  const units = ctl.units.value;
  const host = info?.hostname ?? "your computer";
  const trash = trashName(info?.os);
  const s = r.status;
  const what = r.names.length ? `${r.names.length === 1 ? `“${r.names[0]}”` : plural(r.count, "item", "items")} (${formatBytes(r.bytes, units)})` : plural(r.count, "item", "items");
  const doing = r.mode === "trash" ? `Move ${what} to the ${trash}` : `Delete ${what} permanently`;
  const close = () => ctl.closeRequest();

  if (r.localError) {
    return <Frame title="Request canceled" body={requestCodeText(r.localError, undefined, host, trash)} foot={<button class="btn btn-primary" onClick={close}>Close</button>} />;
  }

  const state = s?.state ?? "preparing";
  if (state === "preparing" || state === "awaiting") {
    const awaiting = state === "awaiting";
    const code = ctl.conn.value.s === "connected" ? ctl.conn.value.code : null;
    return (
      <Frame
        title={awaiting ? `Approve on ${host}` : `Checking with ${host}`}
        live
        body={
          <>
            <p class="dqd-lead">{awaiting ? approveHow(info?.features?.delete?.confirm) : "diskspace is measuring what you asked for before it asks you to approve."}</p>
            <p class="dqd-what">{doing}</p>
            {awaiting && code && (
              <p class="dqd-muted">
                Check that it shows the code <span class="dqd-code">{code}</span>. If you didn't ask for this, decline it there.
              </p>
            )}
            {!awaiting && <Waiting />}
          </>
        }
        foot={
          <button class="btn" disabled={r.cancelWanted} onClick={() => ctl.cancelRequest()}>
            {r.cancelWanted ? "Canceling…" : "Cancel request"}
          </button>
        }
      />
    );
  }

  if (state === "running") {
    const total = Math.max(1, s!.total);
    const done = Math.min(s!.done, total);
    return (
      <Frame
        title={r.names.length === 0 ? `Deleting on ${host}` : r.mode === "trash" ? `Moving to the ${trash}` : "Deleting permanently"}
        live
        body={
          <>
            <div class="dqd-bar" role="progressbar" aria-valuemin={0} aria-valuemax={total} aria-valuenow={done} aria-label="Deletion progress">
              <i style={{ transform: `scaleX(${done / total})` }} />
            </div>
            <p class="dqd-count">
              {done} of {total}
              {s!.freedAlloc > 0 && <span class="dqd-muted"> · {formatBytes(s!.freedAlloc, units)}</span>}
            </p>
            {s!.current && <p class="dqd-current">{"‎" + s!.current}</p>}
          </>
        }
        foot={
          <button class="btn" disabled={r.cancelWanted} onClick={() => ctl.cancelRequest()}>
            {r.cancelWanted ? "Stopping…" : "Stop after this item"}
          </button>
        }
      />
    );
  }

  return <Outcome ctl={ctl} r={r} s={s!} host={host} trash={trash} />;
}

function Outcome({ ctl, r, s, host, trash }: { ctl: Controller; r: DeleteRequestView; s: DeleteStatus; host: string; trash: string }) {
  const units = ctl.units.value;
  const close = () => ctl.closeRequest();
  const failed = (s.results ?? []).filter((x) => !x.ok);
  const okCount = s.results ? s.results.length - failed.length : s.state === "done" ? s.done : 0;
  const freed = formatBytes(s.freedAlloc, units);
  const moved = r.mode === "trash" ? `Moved ${freed} to the ${trash}. Empty the ${trash} to free the space.` : `Deleted ${freed} permanently.`;
  const foot = (
    <button class="btn btn-primary" onClick={close} autoFocus>
      {s.state === "done" && failed.length === 0 ? "Done" : "Close"}
    </button>
  );

  if (s.state === "done" && failed.length === 0) {
    return <Frame title={r.mode === "trash" ? `Moved to the ${trash}` : "Deleted"} body={<p class="dqd-lead">{moved}</p>} foot={foot} />;
  }
  if (s.state === "refused") {
    return <Frame title="Not deleted" body={<p class="dqd-lead">{requestCodeText(s.code, s.message, host, trash)}</p>} foot={foot} />;
  }
  return (
    <Frame
      title={okCount > 0 ? "Some items weren't deleted" : s.state === "failed" ? "Couldn't delete" : "Nothing was deleted"}
      body={
        <>
          {okCount > 0 && <p class="dqd-lead">{moved}</p>}
          {s.state === "failed" && <p class="dqd-lead">{requestCodeText(s.code, s.message, host, trash)}</p>}
          {failed.length > 0 && (
            <ul class="dqd-fails">
              {failed.map((x) => (
                <li key={x.i}>
                  <span class="dqd-fname">{r.names[x.i] ?? `Item ${x.i + 1}`}</span>
                  <span class="dqd-why">{itemCodeText(x.code, x.message)}</span>
                </li>
              ))}
            </ul>
          )}
          {failed.length > 0 && r.names.length > 0 && <p class="dqd-muted">Items that weren't deleted stay in the queue.</p>}
        </>
      }
      foot={foot}
    />
  );
}

function Waiting() {
  return (
    <div class="dqd-bar dqd-indeterminate" aria-hidden="true">
      <i />
    </div>
  );
}

function Frame({ title, body, foot, live = false }: { title: string; body: preact.ComponentChildren; foot: preact.ComponentChildren; live?: boolean }) {
  return (
    <>
      <div class="card-head">
        <h2 id="dqd-title">{title}</h2>
      </div>
      <div class="card-pad dqd-body" role={live ? "status" : undefined} aria-live={live ? "polite" : undefined}>
        {typeof body === "string" ? <p class="dqd-lead">{body}</p> : body}
      </div>
      <div class="card-foot">{foot}</div>
    </>
  );
}
