/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #125: the New Run rail used to HOLD the wizard on an "Open run" screen so a
// launch's advisory warnings had somewhere to sit before the member left for
// the run. A launch now always navigates in the same tick, carrying the
// warnings as router state — this is where they land instead, in the rail's
// own advisory-block shape (new-run-rail.tsx's removed inline block). Router
// state is gone the moment this page reloads, so LAUNCH_WARNING_EPHEMERAL says
// so rather than letting the note vanish with no explanation; the durable
// record stays the run.create audit row's own clamp warnings.
import { X } from "lucide-react";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { RUN_DETAIL } from "../../wardyn/copy/run-cockpit";
import { Button } from "../../ui/button";

export function LaunchWarningsNote({ warnings, onDismiss }: { warnings: string[]; onDismiss: () => void }) {
  return (
    <div
      data-testid="launch-warnings-note"
      className="mb-2 shrink-0 rounded-md border border-warning/30 bg-warning-subtle px-2.5 py-2 text-xs text-warning"
    >
      <div className="flex items-start justify-between gap-2">
        <p className="font-medium text-foreground">{AGENTS.LAUNCH_WARNING_TITLE}</p>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="h-6 shrink-0 gap-1 px-1.5 text-muted-foreground"
          onClick={onDismiss}
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
