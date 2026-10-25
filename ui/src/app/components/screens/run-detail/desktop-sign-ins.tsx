/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Sign-ins in a Linux VM's desktop (#2972, board R11-2972-DesktopSignIns): the pause button, the paused
// note and the note that says where a sign-in lives and when it is gone. The run page says nothing about
// sign-ins for a run whose guest reported no sign-in store.
import * as React from "react";
import { desktopSignIns, type DesktopSignInsView } from "../../../lib/api/desktop-sign-ins";
import { getErrorMessage } from "../../../lib/format";
import { Button } from "../../ui/button";
import { DESKTOP } from "../../wardyn/copy/desktop";

// Where a sign-in lives and when it is gone, by store; a desktop that keeps none says so instead.
const SIGN_INS_NOTE: Partial<Record<DesktopSignInsView["store"], string>> = { memory: DESKTOP.signInsNote, run_disk: DESKTOP.signInsNoteRunDisk };

export interface DesktopSignIns {
  view: DesktopSignInsView | null;
  busy: boolean;
  error: string | null;
  toggle: () => void;
}

export function useDesktopSignIns(runId: string, enabled: boolean): DesktopSignIns {
  const [view, setView] = React.useState<DesktopSignInsView | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (!enabled) return;
    const ctl = new AbortController();
    desktopSignIns.get(runId, ctl.signal).then(setView, () => {
      // Unreadable: claim nothing, as for a run with no store.
    });
    return () => ctl.abort();
  }, [runId, enabled]);
  const toggle = () => {
    if (!view || busy) return;
    setBusy(true);
    setError(null);
    (view.paused ? desktopSignIns.resume(runId) : desktopSignIns.pause(runId))
      .then(setView, (err: unknown) => setError(getErrorMessage(err)))
      .finally(() => setBusy(false));
  };
  return { view, busy, error, toggle };
}

/** The outline button beside Open desktop: Pause, or Resume while paused. None when the store keeps none
 *  or the admin set Not allowed (the server refuses the pause there too). */
export function DesktopSignInButton({ s }: { s: DesktopSignIns }) {
  const v = s.view;
  if (!v || (v.store !== "memory" && v.store !== "run_disk") || (!v.paused && !v.can_pause)) return null;
  return (
    <Button size="sm" variant="outline" disabled={s.busy} onClick={s.toggle}>
      {v.paused ? DESKTOP.resume : DESKTOP.pauseToSignIn}
    </Button>
  );
}

/** State 2's warning note, above the buttons. */
export function DesktopPausedNote({ s }: { s: DesktopSignIns }) {
  if (!s.view?.paused) return null;
  return (
    <p className="mt-2 flex items-start gap-2 rounded-md border border-warning/40 bg-warning-subtle px-3 py-2 text-meta leading-relaxed text-warning">
      <span aria-hidden="true" className="mt-1.5 size-1.5 shrink-0 rounded-full bg-warning" />
      {DESKTOP.pausedNote}
    </p>
  );
}

/** The block's last line: where a sign-in lives and when it is gone, or that this desktop keeps none. */
export function DesktopSignInsNote({ s }: { s: DesktopSignIns }) {
  const v = s.view;
  if (!v || v.store === "") return null;
  return (
    <>
      {s.error && (
        <p role="alert" className="mt-2 text-meta leading-relaxed text-danger">
          {s.error}
        </p>
      )}
      <p className="mt-2 text-meta leading-relaxed text-foreground">{SIGN_INS_NOTE[v.store] ?? DESKTOP.signInsOff(v.agent)}</p>
    </>
  );
}
