/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's four panels (#1922): which one is on screen, how to move between
// them, and where focus lands when you do. The panels are steps you may take in
// any order — a nav of buttons with aria-current, never a tablist — and every
// one stays mounted, so a selection made on one is still there on the next.
import * as React from "react";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import { Chip } from "../../wardyn/primitives";
import { NEW_RUN_FLOW as F } from "../../wardyn/copy/new-run-flow";
import type { LaunchIssue, NewRunPanelId } from "./new-run-launch-gates";

export const PANELS: readonly NewRunPanelId[] = ["run", "workspace", "access", "policy"];

export const PANEL_LABEL: Record<NewRunPanelId, string> = {
  run: F.RUN,
  workspace: F.WORKSPACE,
  access: F.ACCESS,
  policy: F.POLICY,
};

const headingId = (id: NewRunPanelId) => `nr-panel-${id}`;

// An issue names its control by id. Where that id is a wrapper (the mode cards,
// the barrier picker, a structured section), focus goes to the choice already
// made inside it, else its first control, else the wrapper itself.
function focusTarget(id: string): void {
  const el = document.getElementById(id);
  if (!el) return;
  const inner = el.matches("button, input, textarea, select")
    ? null
    : el.querySelector<HTMLElement>('[aria-pressed="true"], [aria-checked="true"]') ??
      el.querySelector<HTMLElement>("button, input, textarea, select");
  (inner ?? el).focus();
}

export function useNewRunPanels() {
  // `seq` re-runs the focus effect when a link names the panel already on screen.
  const [at, setAt] = React.useState<{ panel: NewRunPanelId; seq: number }>({ panel: "run", seq: 0 });
  const focusId = React.useRef<string | null>(null);
  const go = React.useCallback((panel: NewRunPanelId, focus?: string) => {
    focusId.current = focus ?? headingId(panel);
    setAt((prev) => ({ panel, seq: prev.seq + 1 }));
  }, []);
  // After the panel is shown: a hidden control cannot take focus.
  React.useEffect(() => {
    if (focusId.current) focusTarget(focusId.current);
    focusId.current = null;
  }, [at]);
  const reveal = React.useCallback((issue: Pick<LaunchIssue, "panel" | "focus">) => go(issue.panel, issue.focus), [go]);
  return { panel: at.panel, go, reveal };
}

export function PanelNav({
  active,
  counts,
  onSelect,
}: {
  active: NewRunPanelId;
  /** How many issues each panel owns; a count is words, never colour alone. */
  counts: Record<NewRunPanelId, number>;
  onSelect: (panel: NewRunPanelId) => void;
}) {
  return (
    <nav aria-label={F.NAV} className="flex flex-wrap gap-1.5">
      {PANELS.map((id) => (
        <button
          key={id}
          type="button"
          aria-current={active === id ? "step" : undefined}
          onClick={() => onSelect(id)}
          className={cn(
            "flex items-center gap-1.5 rounded-md border px-3 py-1.5 text-sm outline-none transition-colors focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring",
            active === id
              ? "border-border-strong bg-accent font-medium text-foreground"
              : "border-border text-muted-foreground hover:text-foreground",
          )}
        >
          {PANEL_LABEL[id]}
          {/* The space is for the button's name ("Run 1 issue"); between flex
              items it draws nothing. */}
          {counts[id] > 0 && (
            <>
              {" "}
              <Chip tone="warning">{F.ISSUE_COUNT(counts[id])}</Chip>
            </>
          )}
        </button>
      ))}
    </nav>
  );
}

// One panel: a card with its heading, its controls, and the way to the panels
// either side. Hidden rather than unmounted while another panel is on screen.
export function Panel({
  id,
  active,
  onSelect,
  children,
}: {
  id: NewRunPanelId;
  active: NewRunPanelId;
  onSelect: (panel: NewRunPanelId) => void;
  children: React.ReactNode;
}) {
  const at = PANELS.indexOf(id);
  const prev = PANELS[at - 1];
  const next = PANELS[at + 1];
  return (
    <section aria-labelledby={headingId(id)} hidden={active !== id} className="rounded-xl border border-border bg-card p-4">
      {/* Continue, Back and the nav land here, so the panel is announced and
          the next Tab enters its controls. */}
      <h2 id={headingId(id)} tabIndex={-1} className="mb-3 text-foreground outline-offset-4">
        {PANEL_LABEL[id]}
      </h2>
      {children}
      {/* Neither of these launches: Launch is the rail's button alone, and the
          last panel offers only the way back. */}
      <div className="mt-4 flex gap-2">
        {prev && (
          <Button type="button" variant="ghost" onClick={() => onSelect(prev)}>
            {F.BACK}
          </Button>
        )}
        {next && (
          <Button type="button" variant="outline" onClick={() => onSelect(next)}>
            {F.CONTINUE(PANEL_LABEL[next])}
          </Button>
        )}
      </div>
    </section>
  );
}
