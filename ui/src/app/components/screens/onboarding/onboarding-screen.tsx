/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// OnboardingScreen — the first-boot WELCOME (redesign). The old 7-page tour is
// collapsed into ONE glanceable intro (B1): a hero, the single 5-node
// how-it-works strip (shared with the funnel shell's intro panel), live readiness chips
// off the real SetupStatus, and two exits — Get set up / Skip. Rendered inside
// the AppShell as the "Getting started" nav content; "Get set up" advances to the
// setup funnel, "Skip" drops the operator into the console.
import * as React from "react";
import { ArrowRight, BrickWall, KeyRound, Shield } from "lucide-react";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { strongestAvailable } from "../../wardyn/default-confinement";
import { api } from "../../../lib/api";
import { lsGet, lsSet } from "../../../lib/storage";
import type { SetupStatus } from "../../../lib/types";
import { HowItWorksStrip, IntroBlurb, deriveReadiness } from "./intro";
import { SetupScreen } from "../setup/setup-screen";

// ---------------------------------------------------------------------------
// "Have they seen the welcome" flag — localStorage, private-mode tolerant.
// Distinct from wardyn-setup-dismissed so the welcome and the setup funnel track
// separately (skipping the welcome must not dismiss the funnel).
// ---------------------------------------------------------------------------
const ONBOARDING_KEY = "wardyn-onboarding-seen";
export function onboardingSeen(): boolean {
  return lsGet(ONBOARDING_KEY) === "1";
}
export function markOnboardingSeen(): void {
  lsSet(ONBOARDING_KEY, "1");
}

// The "Getting started" flow: the welcome hero first (until seen), then the setup
// funnel. No double stepper — the welcome has no stepper; the funnel has one.
export function GettingStarted({ onDone }: { onDone: () => void }) {
  const [seen, setSeen] = React.useState(onboardingSeen());
  if (!seen) {
    return (
      <OnboardingScreen
        onGetStarted={() => {
          markOnboardingSeen();
          setSeen(true);
        }}
        onSkip={() => {
          // Skipping the intro must NOT dismiss the funnel — it still lives in the
          // sidebar (and auto-opens) until the operator finishes or launches.
          markOnboardingSeen();
          onDone();
        }}
      />
    );
  }
  return <SetupScreen onDone={onDone} />;
}

function ReadinessRow({ status, loading }: { status: SetupStatus | null; loading: boolean }) {
  const readiness = status ? deriveReadiness(status) : null;
  const chip = (
    icon: React.ElementType,
    ready: boolean,
    readyText: string,
  ): React.ReactNode => {
    const Icon = icon;
    if (loading || !readiness)
      return (
        <Chip tone="neutral">
          <Icon className="size-3" /> Checking…
        </Chip>
      );
    return (
      <Chip tone={ready ? "success" : "warning"} dot={ready}>
        <Icon className="size-3" /> {ready ? readyText : "Needs setup"}
      </Chip>
    );
  };
  // Strongest AVAILABLE barrier label for the "Fence ready" text, from the real
  // confinement-class list.
  const strongest = status ? strongestAvailable(status.runner.confinement_classes) : undefined;
  const strongestLabel = strongest ? CC_META[strongest].label : CC_META.CC1.label;
  // Barrier + Model only — no Composer chip (zero composer UI surfaces on the
  // hero; deriveReadiness().composerReady is left untouched, just unused here).
  return (
    <div className="mt-8 w-full rounded-xl border border-border bg-muted/40 p-4 text-left">
      <div className="mb-3 text-sm text-muted-foreground">This host right now:</div>
      <div className="flex flex-wrap gap-2">
        {chip(BrickWall, !!readiness?.barrierReady, `Barrier: ${strongestLabel} ready`)}
        {chip(KeyRound, !!readiness?.llmReady, readiness?.llmLabel ? `Model: ${readiness.llmLabel}` : "Model: ready")}
      </div>
    </div>
  );
}

export function OnboardingScreen({
  onGetStarted,
  onSkip,
}: {
  onGetStarted: () => void;
  onSkip: () => void;
}) {
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [loading, setLoading] = React.useState(true);

  React.useEffect(() => {
    let active = true;
    api
      .getSetupStatus()
      .then((s) => active && setStatus(s))
      .catch(() => {
        /* leave readiness unknown — never block the welcome on a failed probe */
      })
      .finally(() => active && setLoading(false));
    return () => {
      active = false;
    };
  }, []);

  const readiness = status ? deriveReadiness(status) : null;
  const ready = !!readiness?.ready;

  return (
    <div className="mx-auto w-full max-w-[780px] px-6 py-12">
      <span className="inline-flex size-11 items-center justify-center rounded-xl bg-primary/15 text-primary">
        <Shield className="size-6" aria-hidden />
      </span>
      <h1 className="mt-5 text-[2rem] font-semibold leading-tight tracking-tight text-foreground">
        Let agents work. Keep your keys.
      </h1>
      <p className="mt-3 max-w-[640px] text-base leading-relaxed text-muted-foreground">
        <IntroBlurb />
      </p>

      <div className="mt-7 w-full text-left">
        <HowItWorksStrip />
      </div>

      <ReadinessRow status={status} loading={loading} />

      <div className="mt-6 flex items-center gap-2.5">
        <Button onClick={onGetStarted}>
          {ready ? "Finish setup" : "Get set up — about 2 minutes"} <ArrowRight className="size-4" />
        </Button>
        <Button variant="outline" onClick={onSkip}>
          Skip for now
        </Button>
      </div>

      <p className="mt-6 max-w-[560px] text-xs text-muted-foreground">
        Shown once — everything lives on under “Getting started” in the sidebar.
      </p>
    </div>
  );
}
