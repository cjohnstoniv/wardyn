/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SetupLayout — the Getting-started funnel SHELL. Purely
// presentational/composable: it renders the phase rail an orchestrator hands
// it, a single host-status strip, the step heading + optional badge, the
// step body (children), and the footer nav — it computes NO readiness itself
// (that comes from the caller). Reads the step data layer (./steps.ts) and
// the StatusChip API. #11: the last step's own launch CTA lives in its body
// (step-bodies.tsx's LaunchStep) — this shell's footer does not duplicate it.
import { type ReactNode, useState } from "react";
import { ArrowLeft, ArrowRight, Eye, X } from "lucide-react";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import { HostStatusBar } from "./host-status-bar";
import {
  OPTIONAL_STEPS,
  STEP_HEADING,
  STEP_LABEL,
  STEP_ORDER,
  type SetupStepId,
} from "./steps";
import { StatusChip } from "../../wardyn/status-chip";
import { HowItWorksStrip, IntroBlurb } from "../onboarding/intro";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";

export function SetupLayout({
  current,
  rail,
  checking,
  lastCheckedLabel,
  onRecheck,
  onSelect,
  onFinish,
  nextGate,
  backOverride,
  operator,
  children,
}: {
  current: SetupStepId;
  rail: ReactNode;
  checking: boolean;
  lastCheckedLabel: string;
  onRecheck: () => void;
  onSelect: (step: SetupStepId) => void;
  // Complete setup and enter the app: dismisses the first-run gate (unlocks nav)
  // WITHOUT requiring a launched run. The end-of-flow completion action — there
  // is no early "skip" escape any more (the gate keeps the operator in setup).
  onFinish: () => void;
  // The current step's Next-gate, if it has one (only Corporate network
  // today — steps.ts's corpNetworkGate, mapped by setup-screen.tsx). Generic
  // on purpose: this shared layout has no idea which step or why.
  //  - blocked: Next is disabled (title = reason).
  //  - head/reason: the state, rendered as a bold headline over its sentence,
  //    with the gate dot — present on some ENABLED states too (no_runner, a
  //    custom-endpoint pass), where it is a neutral standing note.
  //  - action: a fix-it button rendered IN PLACE of the disabled Next (the
  //    gate row is the one launch point, and it names what it will do).
  //  - nextLabel/onNext: a step whose forward walk has an INTERNAL stop can
  //    rename and repoint the Next button (Corporate network's Host proxy tab
  //    forwards to its Egress redirection tab, not the exit). The label
  //    applies whenever the Next button renders — including disabled — so a
  //    probe-in-flight footer already names where a pass will go.
  nextGate?: {
    blocked: boolean;
    head?: string;
    reason?: string;
    tone?: "warning" | "neutral";
    action?: { label: string; onClick: () => void };
    nextLabel?: string;
    onNext?: () => void;
  };
  // When set, Back is enabled and calls this instead of stepping to the
  // previous step — the mirror of nextGate.onNext (Egress redirection's Back
  // returns to Host proxy).
  backOverride?: () => void;
  // W13-S1-8: nextGate.action is the one control on the step with no operator
  // check — it stands in for the inline Test/Test-all buttons it replaces
  // (corp-network-egress.tsx), which are already `disabled={!operator}`. This
  // shell has no idea of the role otherwise, so the caller passes it once.
  operator: boolean;
  children: ReactNode;
}) {
  const [showIntro, setShowIntro] = useState(false);
  const idx = STEP_ORDER.indexOf(current);
  const prev = idx > 0 ? STEP_ORDER[idx - 1] : null;
  const next = idx < STEP_ORDER.length - 1 ? STEP_ORDER[idx + 1] : null;

  return (
    <div className="mx-auto w-full max-w-[1200px] px-6 py-8">
      {/* Header */}
      <header className="mb-6 flex items-start justify-between gap-4">
        <div>
          <h1>Getting started</h1>
          <p className="mt-1 text-muted-foreground">Governed sandboxes for anything you run — keep your keys.</p>
        </div>
        <Button variant="ghost" size="sm" onClick={() => setShowIntro((s) => !s)}>
          <Eye className="size-4" aria-hidden />
          {showIntro ? "Hide intro" : "Show intro"}
        </Button>
      </header>

      {/* Dismissible intro panel — same blurb as Welcome, reusing IntroBlurb /
          HowItWorksStrip from onboarding/intro.tsx instead of duplicating them. */}
      {showIntro && (
        <section className="mb-6 rounded-xl border bg-muted/40 p-4">
          <div className="mb-3 flex items-start justify-between gap-3">
            <p className="max-w-[720px] text-sm text-muted-foreground">
              <IntroBlurb />
            </p>
            <button
              onClick={() => setShowIntro(false)}
              className="text-muted-foreground hover:text-foreground"
              aria-label="Close intro panel"
            >
              <X className="size-4" aria-hidden />
            </button>
          </div>
          <HowItWorksStrip />
        </section>
      )}

      {/* Two-column grid: rail + content. Rail collapses to icon-only at lg, expands at xl. */}
      <div className="grid grid-cols-1 gap-8 lg:grid-cols-[56px_minmax(0,1fr)] xl:grid-cols-[240px_minmax(0,1fr)]">
        <aside className="lg:sticky lg:top-6 lg:self-start">{rail}</aside>

        <div className="min-w-0">
          <HostStatusBar
            checking={checking}
            lastCheckedLabel={lastCheckedLabel}
            onRecheck={onRecheck}
            className="mb-6"
          />
          <div className="mb-2 text-xs uppercase tracking-wide text-muted-foreground">
            Step {idx + 1} of {STEP_ORDER.length}
          </div>
          <div className="mb-4 flex items-baseline gap-3">
            <h2>{STEP_HEADING[current]}</h2>
            {OPTIONAL_STEPS.has(current) && <StatusChip status="optional" />}
          </div>
          {children}

          {/* Footer — forward-only. No early "skip"/"finish later" escape: the
              first-run gate keeps the operator in setup until the flow's end, where
              "Finish setup" completes it (barrier is the only requirement; launching
              a run is offered but optional). */}
          <footer className="mt-10 flex flex-wrap items-center justify-end gap-2 border-t pt-5">
            <Button
              variant="outline"
              disabled={!prev && !backOverride}
              onClick={() => (backOverride ? backOverride() : prev && onSelect(prev))}
            >
              <ArrowLeft className="size-4" aria-hidden />
              Back
            </Button>
            {next ? (
              <>
                {nextGate && (nextGate.head || nextGate.reason) && (
                  <div className="min-w-0 max-w-md space-y-0.5">
                    {nextGate.head && (
                      <p
                        className={cn(
                          "flex items-start gap-1.5 text-[0.8125rem] font-medium leading-snug",
                          nextGate.tone === "neutral" ? "text-foreground" : "text-warning",
                        )}
                      >
                        <span
                          className={cn(
                            "mt-1.5 size-1.5 shrink-0 rounded-full",
                            nextGate.tone === "neutral" ? "bg-border-strong" : "bg-warning",
                          )}
                        />
                        {nextGate.head}
                      </p>
                    )}
                    {nextGate.reason && (
                      <p className={cn("text-xs leading-snug text-muted-foreground", nextGate.head && "pl-3.5")}>
                        {nextGate.reason}
                      </p>
                    )}
                  </div>
                )}
                {nextGate?.blocked && nextGate.action ? (
                  <Button
                    onClick={nextGate.action.onClick}
                    disabled={!operator}
                    title={!operator ? OPERATOR_ONLY_REASON : undefined}
                  >
                    {nextGate.action.label}
                  </Button>
                ) : (
                  <Button
                    onClick={() => (nextGate?.onNext ? nextGate.onNext() : onSelect(next))}
                    disabled={!!nextGate?.blocked}
                    title={nextGate?.blocked ? nextGate.reason : undefined}
                  >
                    {nextGate?.nextLabel ?? `Next: ${STEP_LABEL[next]}`}
                    <ArrowRight className="size-4" aria-hidden />
                  </Button>
                )}
              </>
            ) : (
              <Button onClick={onFinish}>
                Finish setup
                <ArrowRight className="size-4" aria-hidden />
              </Button>
            )}
          </footer>
        </div>
      </div>
    </div>
  );
}
