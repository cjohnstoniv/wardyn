/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The frame every Guided step, the document editor and Review share: progress,
// eyebrow, heading, lede, Back on the left, the one primary action on the right,
// and Ctrl/Cmd+Enter for that action. Strings come from the caller's copy module.
// The page column (width and top padding) is the route's; see new-run-route.tsx.
// Geometry follows Make (snapshot index.css): rail 50 px above the header, 32 px at
// <=520 px (:124, :528); teal eyebrow (:91); h1 clamp(28px, 4vw, 42px) at regular
// weight (:69), its own size so --text-display elsewhere stays put; footer rule as .sticky-actions (:383).

import * as React from "react";
import { modalLayerOpen } from "../../../../lib/modal-layer-open";
import { Button } from "../../../ui/button";
import { FlowProgress, type FlowStep } from "../../../wardyn/flow-progress";
import { NEW_RUN_STEPS, type NewRunStep, type StepIssue } from "../lenses/step-issues";

/** The progress states with `current` on screen. */
export const flowSteps = (labels: Record<NewRunStep, string>, current: NewRunStep): FlowStep[] => {
  const at = NEW_RUN_STEPS.indexOf(current);
  return NEW_RUN_STEPS.map((id, i) => ({ id, label: labels[id], state: i < at ? "done" : i === at ? "current" : "upcoming" }));
};

/** The route's polite announcer; a shell outside the route announces nothing. */
export const AnnounceContext = React.createContext<(message: string) => void>(() => {});
export const useAnnounce = () => React.useContext(AnnounceContext);

export interface StepShellPrimary {
  label: string;
  onActivate: () => void;
  disabled?: boolean;
  /** The id of the element that says why the action is held (Review: the blocker list). */
  describedBy?: string;
  /** Make's trailing arrow, shown to sight only. On unless set false (Review's Launch run has none). */
  arrow?: boolean;
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
  /** Beside the heading, at its right (Review's view toggle). */
  headingAside?: React.ReactNode;
  /** More footer actions: `start` on the left in place of Back, `end` just before the primary (Review's Save and Check again). */
  footerExtras?: { start?: React.ReactNode; end?: React.ReactNode };
  children?: React.ReactNode;
}

export function StepShell({ title, lede, eyebrow, progress, back, primary, issues, headingAside, footerExtras, children }: StepShellProps) {
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

  const header = (
    <header className="flex max-w-[680px] flex-col gap-2.5">
      {eyebrow && <p className="mb-1 text-meta font-semibold uppercase tracking-[0.12em] text-primary">{eyebrow}</p>}
      <h1 ref={heading} tabIndex={-1} className="text-[clamp(28px,4vw,42px)] leading-[1.08] font-normal tracking-[-0.04em] outline-none">{title}</h1>
      {lede && <p className="max-w-[620px] text-muted-foreground">{lede}</p>}
    </header>
  );
  const primaryButton = (
    <Button type="button" disabled={primary.disabled} aria-describedby={primary.describedBy} onClick={() => activate.current()}>
      {primary.label}
      {primary.arrow !== false && <span aria-hidden="true">→</span>}
    </Button>
  );

  return (
    <section className="flex flex-col gap-6">
      {progress && <div className="mb-[26px] max-[520px]:mb-2"><FlowProgress steps={progress.steps} label={progress.label} /></div>}
      {headingAside ? (
        <div className="flex flex-wrap items-center justify-between gap-6">
          {header}
          <div className="shrink-0">{headingAside}</div>
        </div>
      ) : header}
      {children}
      <footer className="mt-1 flex items-center justify-between gap-3 border-t border-border pt-[18px]">
        {/* With extras the footer keeps one shape, so the primary is never remounted as an extra comes and goes. */}
        {footerExtras ? <div className="flex flex-wrap gap-2">{footerExtras.start}</div>
          : back ? <Button type="button" variant="ghost" onClick={back.onBack}>{back.label}</Button> : <span />}
        {footerExtras ? <div className="flex flex-wrap gap-2">{footerExtras.end}{primaryButton}</div> : primaryButton}
      </footer>
    </section>
  );
}
