/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #125: the New Run rail used to HOLD the wizard on an "Open run" screen so a
// launch's advisory warnings had somewhere to sit before the member left for
// the run. A launch now always navigates in the same tick, carrying the
// warnings as router state — this is where they land instead, in the rail's
// own advisory-block shape (new-run-rail.tsx's removed inline block). The
// durable record stays the run.create audit row's own clamp warnings.
//
// review defect 1: BrowserRouter restores `location.state` from
// `window.history.state.usr` on EVERY mount over the same history entry — a
// real reload, and Back/Forward — not just the one navigation that set it
// (react-router's own createBrowserHistory: getHistoryState() serializes
// `{ usr: location.state, key, idx }` into `history.state`, and it reads that
// same object back on init). Leaving it there made LAUNCH_WARNING_EPHEMERAL's
// "this note goes when you reload" false: a reload is a fresh mount over the
// SAME entry, `history.state.usr` is still populated, and the note (even a
// DISMISSED one, since dismiss only ever touched this component's own state)
// came right back. useLaunchWarnings reads it once into local state, then
// immediately REPLACES the entry with `state: null` — from then on the value
// lives only in this component's memory, so a reload genuinely shows nothing.
import * as React from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { X } from "lucide-react";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { RUN_DETAIL } from "../../wardyn/copy/run-cockpit";
import { Button } from "../../ui/button";

function useLaunchWarnings(): { warnings: string[]; dismiss: () => void } {
  const location = useLocation();
  const navigate = useNavigate();
  const [warnings, setWarnings] = React.useState<string[]>(
    () => (location.state as { launchWarnings?: string[] } | null)?.launchWarnings ?? [],
  );
  // Fires at most once per mount: the effect itself is what wipes the state
  // this same read depends on, so nothing here has to remember not to re-run
  // it — a second render with the (now null) state simply has nothing to clear.
  const cleared = React.useRef(false);
  React.useEffect(() => {
    if (cleared.current) return;
    cleared.current = true;
    if (!(location.state as { launchWarnings?: string[] } | null)?.launchWarnings) return;
    navigate(location.pathname + location.search, { replace: true, state: null });
    // eslint-disable-next-line react-hooks/exhaustive-deps -- read once, on mount, by design (see cleared.current above)
  }, []);
  return { warnings, dismiss: () => setWarnings([]) };
}

// Self-contained: reads and clears its own router state, so run-detail.tsx
// (at the file-size gate's ceiling) needs no state of its own for this —
// just the mount site.
export function LaunchWarningsNote() {
  const { warnings, dismiss } = useLaunchWarnings();
  if (warnings.length === 0) return null;
  return (
    <div
      data-testid="launch-warnings-note"
      className="mx-4 mt-2 shrink-0 rounded-md border border-warning/30 bg-warning-subtle px-2.5 py-2 text-xs text-warning"
    >
      <div className="flex items-start justify-between gap-2">
        <p className="font-medium text-foreground">{AGENTS.LAUNCH_WARNING_TITLE}</p>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="h-6 shrink-0 gap-1 px-1.5 text-muted-foreground"
          onClick={dismiss}
        >
          <X className="size-3.5" />
          {RUN_DETAIL.LAUNCH_WARNING_DISMISS}
        </Button>
      </div>
      <ul className="mt-1 list-disc space-y-0.5 pl-4">
        {warnings.map((w, i) => (
          <li key={i}>{w}</li>
        ))}
      </ul>
      <p className="mt-1.5 text-muted-foreground">{RUN_DETAIL.LAUNCH_WARNING_EPHEMERAL}</p>
    </div>
  );
}
