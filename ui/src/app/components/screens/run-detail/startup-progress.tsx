/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's startup view (#1419): the sign-in door's step list, shown in
// the terminal hero while a run is PENDING or STARTING. All the derivation
// lives in run-status-detail.ts's runStartupView; this owns the two pieces of
// client memory it needs (a clock, and whether a build was seen) and draws it.
import * as React from "react";
import type { AgentRun, SetupStatus } from "../../../lib/types";
import { ProgressSteps } from "../../wardyn/progress-steps";
import { setup as setupApi } from "../../../lib/api/setup";
import { runStartupView, startOverdueMs, STATUS_REASON_BUILDING, type StartupLastStep } from "../run-status-detail";

// Fast enough that the 60 s hint and the overdue bounds land within a second
// of their deadline, without waiting for the next 4 s run poll.
const CLOCK_TICK_MS = 1000;

export function StartupProgress({
  run,
  lastStep,
  children,
}: {
  run: AgentRun;
  lastStep: StartupLastStep;
  // Drawn inside the same dark-scoped column, under the list (state 9's note).
  children?: React.ReactNode;
}) {
  const [now, setNow] = React.useState(() => Date.now());
  React.useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), CLOCK_TICK_MS);
    return () => clearInterval(t);
  }, []);

  // Build ticks to done rather than vanishing once the server blanks
  // STARTING + Building. Reset per run; a reload drops it, and that draw
  // honestly has no Build row (the page cannot know a build happened).
  const sawBuilding = React.useRef({ runId: run.id, saw: false });
  if (sawBuilding.current.runId !== run.id) sawBuilding.current = { runId: run.id, saw: false };
  const sawNow = sawBuilding.current.saw;
  const buildingNow = run.state === "PENDING" && run.status_reason === STATUS_REASON_BUILDING;
  React.useEffect(() => {
    if (buildingNow) sawBuilding.current.saw = true;
  }, [buildingNow, run.id]);

  // The deployment's real start deadlines, once, while a start is on screen: a run waiting for room
  // is not overdue at the default 4.5 minutes when the deployment lets it wait longer.
  const [sandboxStart, setSandboxStart] = React.useState<SetupStatus["runner"]["sandbox_start"]>();
  const starting = run.state === "STARTING";
  React.useEffect(() => {
    if (!starting) return;
    let live = true;
    setupApi
      .getSetupStatus()
      .then((s) => live && setSandboxStart(s.runner?.sandbox_start))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [starting]);

  const view = runStartupView(run, now, {
    lastStep,
    sawBuilding: sawNow || buildingNow,
    startOverdueMs: startOverdueMs(sandboxStart),
  });
  if (!view) return null;
  return (
    // The hero frame is dark in both themes (terminal-notice.tsx); `dark`
    // scopes the tokens so the list stays readable in the light theme.
    <div className="dark flex min-h-0 flex-1 flex-col items-center justify-center p-6">
      <div className="w-full max-w-sm space-y-2.5">
        <ProgressSteps rows={view.rows} testId="run-startup-progress" variant="run" />
        {view.hint && (
          <p role="status" className="text-xs leading-relaxed text-muted-foreground">
            {view.hint}
          </p>
        )}
        {view.alert && (
          <p
            role="alert"
            className="rounded-lg border border-danger bg-danger-subtle px-3 py-2 font-mono text-xs leading-relaxed text-danger [overflow-wrap:anywhere]"
          >
            {view.alert}
          </p>
        )}
        {children}
      </div>
    </div>
  );
}
