/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The frame every Guided step and Review share: progress, eyebrow, heading,
// lede, Back on the left, the one primary action on the right, and
// Ctrl/Cmd+Enter for that action. Strings come from the caller's copy module.

import * as React from "react";
import { modalLayerOpen } from "../../../../lib/modal-layer-open";
import { Button } from "../../../ui/button";
import { NEW_RUN_STEPS, type NewRunStep, type StepIssue } from "../lenses/step-issues";

export interface FlowStep {
  id: string;
  label: string;
  state: "done" | "current" | "upcoming";
}

/** The progress states with `current` on screen; Review passes no step and shows every step done. */
export const flowSteps = (labels: Record<NewRunStep, string>, current?: NewRunStep): FlowStep[] => {
  const at = current ? NEW_RUN_STEPS.indexOf(current) : NEW_RUN_STEPS.length;
  return NEW_RUN_STEPS.map((id, i) => ({ id, label: labels[id], state: i < at ? "done" : i === at ? "current" : "upcoming" }));
};

// Seam: stands in for the shared FlowProgress primitive (same props) until it
// lands in the pattern layer; swap the import then.
function FlowProgress({ steps, label }: { steps: FlowStep[]; label: string }) {
  return (
    <nav aria-label={label}>
      <ol className="flex gap-2 text-sm text-muted-foreground">
        {steps.map((s) => (
          <li key={s.id} aria-current={s.state === "current" ? "step" : undefined} className={s.state === "current" ? "text-foreground" : undefined}>
            {s.label}
          </li>
        ))}
      </ol>
    </nav>
  );
}

/** The route's polite announcer; a shell outside the route announces nothing. */
export const AnnounceContext = React.createContext<(message: string) => void>(() => {});
export const useAnnounce = () => React.useContext(AnnounceContext);

export interface StepShellPrimary {
  label: string;
  onActivate: () => void;
  disabled?: boolean;
  /** The id of the element that says why the action is held (Review: the blocker list). */
  describedBy?: string;
}

export interface StepShellProps {
  title: string;
  lede?: React.ReactNode;
  eyebrow?: string;
  progress?: { label: string; steps: FlowStep[] };
  back?: { label: string; onBack: () => void };
  primary: StepShellPrimary;
  /** Local issues: the primary action then focuses the first one's control instead of running. */
  issues?: readonly StepIssue[];
  children?: React.ReactNode;
}

export function StepShell({ title, lede, eyebrow, progress, back, primary, issues, children }: StepShellProps) {
  const heading = React.useRef<HTMLHeadingElement>(null);
  const announce = useAnnounce();
  React.useEffect(() => {
    heading.current?.focus();
    announce(title);
  }, [title, announce]);

  const activate = React.useRef(() => {});
  activate.current = () => {
    const first = issues?.[0];
    if (first) {
      const control = document.getElementById(first.focus);
      control?.setAttribute("aria-invalid", "true");
      control?.focus();
      return;
    }
    if (!primary.disabled) primary.onActivate();
  };
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Enter" || !(e.ctrlKey || e.metaKey) || e.defaultPrevented || modalLayerOpen()) return;
      e.preventDefault();
      activate.current();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  return (
    <section className="flex flex-col gap-6">
      {progress && <FlowProgress steps={progress.steps} label={progress.label} />}
      <header className="flex flex-col gap-2">
        {eyebrow && <p className="label-eyebrow">{eyebrow}</p>}
        <h1 ref={heading} tabIndex={-1} className="font-semibold tracking-tight outline-none">{title}</h1>
        {lede && <p className="text-muted-foreground">{lede}</p>}
      </header>
      {children}
      <footer className="flex items-center justify-between gap-3">
        {back ? <Button type="button" variant="ghost" onClick={back.onBack}>{back.label}</Button> : <span />}
        <Button type="button" disabled={primary.disabled} aria-describedby={primary.describedBy} onClick={() => activate.current()}>
          {primary.label}
        </Button>
      </footer>
    </section>
  );
}
