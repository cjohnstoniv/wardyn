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
import { cn } from "../../../ui/utils";
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
  /** Extra classes on the button, for the one caller whose footer lays it out differently (Review's phone bar). */
  className?: string;
}

interface StepShellBase {
  title: string;
  lede?: React.ReactNode;
  eyebrow?: string;
  progress?: { label: string; steps: FlowStep[] };
  /** None: no primary button and no Ctrl/Cmd+Enter (Shape from scratch, where a choice moves on). */
  primary?: StepShellPrimary;
  /** Local issues: the primary action then focuses the first one's control instead of running. */
  issues?: readonly StepIssue[];
  /** Beside the heading, at its right (Review's view toggle). The heading is then Make's 32 px. */
  headingAside?: React.ReactNode;
  /** Where the aside sits against the heading: Review centres it, the document editor sets it on the bottom line. */
  headingAsideAlign?: "center" | "end";
  /** Pins the footer to the bottom of the view while the step scrolls (Tools and access's bar). */
  stickyFooter?: boolean;
  /**
   * Below sm only: the footer becomes a sticky two-row bar, `footerExtras.end` (then the primary) on the grid and
   * `footerExtras.start` hidden, for the caller to put behind its own menu. From sm up nothing changes (Review).
   */
  phoneBar?: boolean;
  children?: React.ReactNode;
}

/** The step heading, which takes focus on arrival and is where a step puts focus when what it held is gone. */
export const STEP_HEADING_ID = "nr-step-heading";

/** The footer's left side is Back or the extras, never both: with extras Back is not drawn, so the type refuses the pair. */
export type StepShellProps = StepShellBase &
  (
    | { back?: { label: string; onBack: () => void }; footerExtras?: never }
    /** More footer actions: `start` on the left in place of Back, `end` just before the primary (Review's Save and Check again). */
    | { back?: never; footerExtras?: { start?: React.ReactNode; end?: React.ReactNode } }
  );

export function StepShell({ title, lede, eyebrow, progress, back, primary, issues, headingAside, headingAsideAlign = "center", footerExtras, stickyFooter, phoneBar, children }: StepShellProps) {
  const heading = React.useRef<HTMLHeadingElement>(null);
  const announce = useAnnounce();
  React.useEffect(() => {
    heading.current?.focus();
    announce(title);
  }, [title, announce]);

  // The controls held Continues marked invalid. Each mark is cleared once no issue points at its control.
  const flagged = React.useRef(new Set<string>());
  React.useEffect(() => {
    for (const id of flagged.current) {
      if (issues?.some((i) => i.focus === id)) continue;
      document.getElementById(id)?.removeAttribute("aria-invalid");
      flagged.current.delete(id);
    }
  });
  const activate = React.useRef(() => {});
  activate.current = () => {
    const first = issues?.[0];
    if (first) {
      const control = document.getElementById(first.focus);
      control?.setAttribute("aria-invalid", "true");
      flagged.current.add(first.focus);
      control?.focus();
      return;
    }
    if (primary && !primary.disabled) primary.onActivate();
  };
  const hasPrimary = primary !== undefined;
  // A layout effect, so Ctrl/Cmd+Enter works from the first paint the primary is on screen.
  React.useLayoutEffect(() => {
    if (!hasPrimary) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Enter" || !(e.ctrlKey || e.metaKey) || e.defaultPrevented || modalLayerOpen()) return;
      e.preventDefault();
      activate.current();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [hasPrimary]);

  const header = (
    <header className="flex max-w-[680px] flex-col gap-2.5">
      {eyebrow && <p className="mb-1 text-meta font-semibold uppercase tracking-[0.12em] text-primary">{eyebrow}</p>}
      <h1 id={STEP_HEADING_ID} ref={heading} tabIndex={-1} className={cn(headingAside ? "text-[32px]" : "text-[clamp(28px,4vw,42px)]", "leading-[1.08] font-normal tracking-[-0.04em] outline-none")}>{title}</h1>
      {lede && <p className="max-w-[620px] text-muted-foreground">{lede}</p>}
    </header>
  );
  const primaryButton = primary && (
    <Button type="button" className={primary.className} disabled={primary.disabled} aria-describedby={primary.describedBy} onClick={() => activate.current()}>
      {primary.label}
      {primary.arrow !== false && <span aria-hidden="true">→</span>}
    </Button>
  );

  return (
    <section className={cn("flex flex-col gap-6", phoneBar && "max-sm:pb-4")}>
      {progress && <div className="mb-[26px] max-[520px]:mb-2"><FlowProgress steps={progress.steps} label={progress.label} /></div>}
      {headingAside ? (
        <div className={cn("flex justify-between gap-6 max-[760px]:flex-col max-[760px]:items-start", headingAsideAlign === "end" ? "items-end" : "items-center")}>
          {header}
          <div className="shrink-0">{headingAside}</div>
        </div>
      ) : header}
      {children}
      <footer
        data-testid={phoneBar ? "step-footer" : undefined}
        className={cn(
          "mt-1 flex items-center justify-between gap-3 border-t border-border",
          stickyFooter ? "sticky bottom-0 z-10 bg-background py-3" : "pt-[18px]",
          phoneBar && "max-sm:sticky max-sm:bottom-0 max-sm:z-10 max-sm:grid max-sm:grid-cols-[minmax(0,1fr)_auto] max-sm:gap-2 max-sm:bg-card max-sm:py-3",
        )}
      >
        {/* With extras the footer keeps one shape, so the primary is never remounted as an extra comes and goes. */}
        {footerExtras ? <div className={cn("flex flex-wrap gap-2", phoneBar && "max-sm:hidden")}>{footerExtras.start}</div>
          : back ? <Button type="button" variant="ghost" onClick={back.onBack}>{back.label}</Button> : <span />}
        {footerExtras ? <div className={cn("flex flex-wrap gap-2", phoneBar && "max-sm:contents")}>{footerExtras.end}{primaryButton}</div> : primaryButton}
      </footer>
    </section>
  );
}
