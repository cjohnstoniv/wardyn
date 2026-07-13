/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { type ReactNode, useState } from "react";
import { ArrowLeft, ArrowRight, Rocket, X, Eye } from "lucide-react";
import { Button } from "../ui/button";
import { PhaseRail } from "./PhaseRail";
import { PipelineDiagram } from "./PipelineDiagram";
import { HostStatusBar } from "./HostStatusBar";
import { STEP_ORDER, STEPS, type StepId } from "../../data/steps";
import { hasConnectedModel, showFastPath, type SetupStatus, type ModelFamily } from "../../data/setupFixtures";
import { StatusChip } from "./StatusChip";

const OPTIONAL_STEPS = new Set<StepId>(["proxy", "scm", "artifacts", "workspaces", "credentials"]);

// Two-column funnel shell (brief §6.10 / §7.1): phased rail + content column with a single
// host-status strip, optional fast-path banner, dismissible intro, and a persistent footer.
export function SetupLayout({
  status,
  current,
  checking,
  onRecheck,
  onSelect,
  onFinishLater,
  onLaunch,
  children,
}: {
  status: SetupStatus;
  current: StepId;
  checking: boolean;
  onRecheck: () => void;
  onSelect: (step: StepId) => void;
  onFinishLater: () => void;
  onLaunch: () => void;
  children: ReactNode;
}) {
  const [showIntro, setShowIntro] = useState(false);
  const idx = STEP_ORDER.indexOf(current);
  const prev = idx > 0 ? STEP_ORDER[idx - 1] : null;
  const next = idx < STEP_ORDER.length - 1 ? STEP_ORDER[idx + 1] : null;
  const fastPath = showFastPath(status);
  const connectedModel: ModelFamily | undefined = status.models.find((m) => m.connected);
  const canLaunch =
    status.barriers.some((b) => b.state === "ready") && hasConnectedModel(status);

  return (
    <div className="mx-auto w-full max-w-[1200px] px-6 py-8">
      {/* Header */}
      <header className="mb-6 flex items-start justify-between gap-4">
        <div>
          <h1>Getting started</h1>
          <p className="mt-1 text-muted-foreground">
            Let agents work while you keep your keys.
          </p>
        </div>
        <Button variant="ghost" size="sm" onClick={() => setShowIntro((s) => !s)}>
          <Eye className="size-4" aria-hidden />
          {showIntro ? "Hide intro" : "Show intro"}
        </Button>
      </header>

      {/* Dismissible intro panel — same content as Welcome */}
      {showIntro && (
        <section className="mb-6 rounded-xl border bg-muted/40 p-4">
          <div className="mb-3 flex items-start justify-between gap-3">
            <p className="max-w-[720px] text-sm text-muted-foreground">
              Wardyn runs your coding agents behind a barrier, with{" "}
              <span className="text-foreground">
                no resident credentials by default and no privileged host access
              </span>
              . Every run gets its own identity; you gate the risky moments; everything is
              audited.
            </p>
            <button
              onClick={() => setShowIntro(false)}
              className="text-muted-foreground hover:text-foreground"
              aria-label="Close intro panel"
            >
              <X className="size-4" aria-hidden />
            </button>
          </div>
          <PipelineDiagram />
        </section>
      )}

      {/* Fast-path banner — only when genuinely ready AND a model is connected */}
      {fastPath && (
        <section className="mb-6 flex flex-wrap items-center justify-between gap-3 rounded-xl border border-primary/40 bg-primary/10 p-4">
          <div className="flex items-start gap-3">
            <Rocket className="mt-0.5 size-5 text-primary" aria-hidden />
            <div>
              <div className="text-sm text-foreground">
                You're ready — launch your first run now.
              </div>
              <div className="text-sm text-muted-foreground">
                A barrier is up and{" "}
                {connectedModel ? connectedModel.label.split(" /")[0] : "a model"} connected.
                That's enough for a first run — you can harden anytime.
              </div>
            </div>
          </div>
          <div className="flex gap-2">
            <Button onClick={onLaunch}>Launch your first run</Button>
            <Button variant="outline" onClick={() => onSelect("environment")}>
              Keep setting up
            </Button>
          </div>
        </section>
      )}

      {/* Two-column grid: rail + content. Rail collapses to icon-only at lg, expands at xl. */}
      <div className="grid grid-cols-1 gap-8 lg:grid-cols-[56px_minmax(0,1fr)] xl:grid-cols-[240px_minmax(0,1fr)]">
        <aside className="lg:sticky lg:top-6 lg:self-start">
          <PhaseRail status={status} current={current} onSelect={onSelect} />
        </aside>

        <div className="min-w-0">
          <HostStatusBar
            checking={checking}
            lastCheckedLabel={status.lastCheckedLabel}
            onRecheck={onRecheck}
            className="mb-6"
          />
          <div className="mb-2 text-xs uppercase tracking-wide text-muted-foreground">
            Step {idx + 1} of {STEP_ORDER.length}
          </div>
          <div className="mb-4 flex items-baseline gap-3">
            <h2>{STEPS[current].heading}</h2>
            {OPTIONAL_STEPS.has(current) && <StatusChip kind="optional" />}
          </div>
          {children}

          {/* Footer */}
          <footer className="mt-10 flex flex-wrap items-center justify-between gap-4 border-t pt-5">
            <button
              onClick={onFinishLater}
              className="text-left text-sm text-muted-foreground hover:text-foreground"
            >
              <span className="block text-foreground">Finish later</span>
              Come back anytime from Getting started.
            </button>
            <div className="flex gap-2">
              <Button
                variant="outline"
                disabled={!prev}
                onClick={() => prev && onSelect(prev)}
              >
                <ArrowLeft className="size-4" aria-hidden />
                Back
              </Button>
              {next ? (
                <Button onClick={() => onSelect(next)}>
                  Next: {STEPS[next].label}
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
