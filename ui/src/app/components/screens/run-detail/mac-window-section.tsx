/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop tile's window on the Mac (board WIN-1, WIN-2's launch line): GET /runs/{id}'s mac_window, in the
// server's words. "open" has the button; "preparing" and "paused" keep it, disabled, with the server's reason, since
// they wait out; every other state is the refusal's sentence alone, and "not_run_owner" says nothing. The run page
// learns the window is up by reading the run again; until that read changes the state, the button stays busy.
import * as React from "react";
import type { RunDetail, RunMacWindow, RunMacWindowClose } from "../../../lib/types";
import { runs as runsApi } from "../../../lib/api/runs";
import { getErrorMessage } from "../../../lib/format";
import { isBackgroundRun } from "../../../lib/run-entry";
import { Button } from "../../ui/button";
import { UI_APPS_LANE } from "../../wardyn/copy";
import { MAC_WINDOW } from "../../wardyn/copy/mac-window";

const line = "mt-2 text-meta leading-relaxed text-muted-foreground";
const WITH_BUTTON = new Set<RunMacWindow["state"]>(["open", "preparing", "paused"]);

/** The window states the tile draws a button for. */
export const macWindowHasButton = (w: RunMacWindow | undefined): boolean => !!w && WITH_BUTTON.has(w.state);

/** The states with something to say: every one but a person who isn't the run's owner. */
export const macWindowShown = (w: RunMacWindow | undefined): w is RunMacWindow => !!w && w.state !== "not_run_owner";

/** The last close in words (W-08 to W-11, W2-12); a launch refusal's sentence follows it as the server wrote it. */
export function lastCloseText(c: RunMacWindowClose, runner: string): string {
  switch (c.reason) {
    case "person": return MAC_WINDOW.closedPerson;
    case "host_locked": return MAC_WINDOW.closedLocked(runner);
    case "run_ended": return MAC_WINDOW.closedRunEnded;
    case "not_opened": return c.message ? `${MAC_WINDOW.notOpened} ${c.message}` : MAC_WINDOW.notOpened;
    default: return MAC_WINDOW.closedFailed;
  }
}

/** The read an open was answered on: the button waits until a read of this run differs from it. */
interface Asked {
  runId: string;
  state: string;
  closedAt?: string;
}
/** How long the button waits for a read that shows the window, before it is offered again. */
export const ASKED_MS = 45_000;

/** The button and what it does, or the open window's lines; the refusal-only states are MacWindowRefusal's. */
export function MacWindowButton({ run }: { run: RunDetail }) {
  const w = run.mac_window;
  const [opening, setOpening] = React.useState(false);
  const [asked, setAsked] = React.useState<Asked | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  // The answer comes when the window is up; a later read shows it open, or a newer last close when it opened and
  // closed between two reads. Until one of those, for this run, the button stays busy, and never past ASKED_MS.
  React.useEffect(() => {
    if (!asked) return;
    const t = setTimeout(() => setAsked(null), ASKED_MS);
    return () => clearTimeout(t);
  }, [asked]);
  if (!w || (!macWindowHasButton(w) && w.state !== "window_open")) return null;
  const named = run.runner_name;
  const runner = named || MAC_WINDOW.thisMac;
  if (w.state === "window_open") {
    return (
      <div data-testid="mac-window">
        {w.message && <p className="text-meta leading-relaxed text-foreground">{w.message}</p>}
        <p className={line}>{MAC_WINDOW.handBack(runner)}</p>
        <p className={line}>{MAC_WINDOW.viewOnlyWhileOpen}</p>
      </div>
    );
  }
  const busy = opening || (asked !== null && asked.runId === run.id && asked.state === w.state && asked.closedAt === w.last_close?.at);
  const open = async () => {
    setError(null);
    setOpening(true);
    try {
      await runsApi.openMacWindow(run.id);
      setAsked({ runId: run.id, state: w.state, closedAt: w.last_close?.at });
    } catch (err) {
      setError(getErrorMessage(err));
    } finally {
      setOpening(false);
    }
  };
  return (
    <div data-testid="mac-window">
      <Button size="sm" disabled={busy || w.state !== "open"} onClick={() => void open()}>
        {busy ? UI_APPS_LANE.ctaBusy : named ? (
          <>
            <span className="hidden sm:inline">{MAC_WINDOW.open(named)}</span>
            <span className="sm:hidden">{MAC_WINDOW.openCompact}</span>
          </>
        ) : MAC_WINDOW.openCompact}
      </Button>
      {named && <p className={line}>{MAC_WINDOW.opensOn(named)}</p>}
      {w.state === "open" ? (
        <p className={line}>{busy ? MAC_WINDOW.waiting(runner) : MAC_WINDOW.explain(runner)}</p>
      ) : (
        w.message && <p className={line}>{w.message}</p>
      )}
      {w.state === "open" && isBackgroundRun(run) && <p className={line}>{MAC_WINDOW.background}</p>}
      {error && <p role="alert" className={line}>{error}</p>}
      {w.last_close && !busy && <p className={line}>{lastCloseText(w.last_close, runner)}</p>}
    </div>
  );
}

/** A state with no button: the refusal's sentence, then the last close, if any. */
export function MacWindowRefusal({ run }: { run: RunDetail }) {
  const w = run.mac_window;
  if (!macWindowShown(w) || macWindowHasButton(w) || w.state === "window_open") return null;
  const runner = run.runner_name || MAC_WINDOW.thisMac;
  return (
    <div data-testid="mac-window-refusal">
      {w.message && <p className={line}>{w.message}</p>}
      {w.last_close && <p className={line}>{lastCloseText(w.last_close, runner)}</p>}
    </div>
  );
}
