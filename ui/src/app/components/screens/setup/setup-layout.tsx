/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SetupLayout — the Getting-started funnel SHELL. Purely
// presentational/composable: it renders the phase rail an orchestrator hands
// it, a single host-status strip, the step heading + optional badge, the
// step body (children), and the footer nav — it computes NO readiness itself
// (fastPath/canLaunch/connectedModelLabel all come from the caller). Reads the
// step data layer (./steps.ts) and the StatusChip API.
import { type ReactNode, useState } from "react";
import { ArrowLeft, ArrowRight, Eye, Rocket, X } from "lucide-react";
import { Button } from "../../ui/button";
import { HostStatusBar } from "./host-status-bar";
import { OPTIONAL_STEPS, STEP_HEADING, STEP_LABEL, STEP_ORDER, type SetupStepId } from "./steps";
import { StatusChip } from "../../wardyn/status-chip";
import { BTN } from "../../wardyn/copy";
import { HowItWorksStrip, IntroBlurb } from "../onboarding/intro";

export function SetupLayout({
  current,
  rail,
  checking,
  lastCheckedLabel,
  onRecheck,
  onSelect,
  onFinishLater,
  onLaunch,
  canLaunch,
  fastPath,
  connectedModelLabel,
  onKeepSettingUp,
  children,
}: {
  current: SetupStepId;
  rail: ReactNode;
  checking: boolean;
  lastCheckedLabel: string;
  onRecheck: () => void;
  onSelect: (step: SetupStepId) => void;
  onFinishLater: () => void;
  onLaunch: () => void;
  canLaunch: boolean;
  fastPath: boolean;
  /**
   * Full readiness fragment from deriveReadiness().llmLabel, e.g. "Claude
   * connected" / "Anthropic key added" — rendered as-is in the banner sentence.
   */
  connectedModelLabel?: string;
  /** Called alongside the environment jump when "Keep setting up" is clicked —
   * lets the orchestrator dismiss the banner (live-screen fastPathHidden behavior). */
  onKeepSettingUp?: () => void;
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
          <p className="mt-1 text-muted-foreground">Let agents work while you keep your keys.</p>
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
          <p className="mt-3 text-sm text-muted-foreground">
            Not sure it works?{" "}
            <a href="/demos" className="font-medium text-primary underline underline-offset-2">
              Run a demo sandbox first
            </a>{" "}
            — no repo or key needed.
          </p>
        </section>
      )}

      {/* Fast-path banner — only when the orchestrator says we're genuinely ready
          AND a model is connected. */}
      {fastPath && (
        <section className="mb-6 flex flex-wrap items-center justify-between gap-3 rounded-xl border border-primary/40 bg-primary/10 p-4">
          <div className="flex items-start gap-3">
            <Rocket className="mt-0.5 size-5 text-primary" aria-hidden />
            <div>
              <div className="text-sm text-foreground">You're ready — launch your first run now.</div>
              <div className="text-sm text-muted-foreground">
                A barrier is up and {connectedModelLabel ?? "a model is connected"}. That's enough
                for a first run — you can harden anytime.
              </div>
            </div>
          </div>
          <div className="flex gap-2">
            <Button onClick={onLaunch} disabled={!canLaunch}>
              Launch your first run
            </Button>
            <Button
              variant="outline"
              onClick={() => {
                // Just dismiss the nudge — the operator said "keep setting up",
                // so keep them on the step they were configuring, don't jump
                // them back to Environment.
                onKeepSettingUp?.();
              }}
            >
              Keep setting up
            </Button>
          </div>
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

          {/* Footer */}
          <footer className="mt-10 flex flex-wrap items-center justify-between gap-4 border-t pt-5">
            <button
              onClick={onFinishLater}
              className="text-left text-sm text-muted-foreground hover:text-foreground"
            >
              <span className="block text-foreground">{BTN.finishLater}</span>
              {BTN.finishLaterHint}
            </button>
            <div className="flex gap-2">
              <Button variant="outline" disabled={!prev} onClick={() => prev && onSelect(prev)}>
                <ArrowLeft className="size-4" aria-hidden />
                Back
              </Button>
              {next ? (
                <Button onClick={() => onSelect(next)}>
                  Next: {STEP_LABEL[next]}
                  <ArrowRight className="size-4" aria-hidden />
                </Button>
              ) : (
                <Button onClick={onLaunch} disabled={!canLaunch}>
                  Launch your first run
                  <Rocket className="size-4" aria-hidden />
                </Button>
              )}
            </div>
          </footer>
        </div>
      </div>
    </div>
  );
}
